// position_mode_test.go — unit + gated integration coverage for the
// NETTING/HEDGING posture toggle (position_mode.go; Phase-19 Task
// 19.3.15; spec §13.6e).
//
// Ungated legs cover the Mode read contract ("" → NETTING fail-safe),
// every SetMode guard (invalid value, retail hedging ban, open-position
// block, idempotent same-mode) and store-error propagation. The gated
// leg round-trips PgPositionModeStore + the audited write path:
//
//	EXC_PG_TEST=1 go test ./internal/risk/ -run 'TestPgPositionMode' -v
package risk

import (
	"context"
	"fmt"
	"testing"
)

// ---------------------------------------------------------------------------
// Fake store
// ---------------------------------------------------------------------------

type pmStoreFake struct {
	mode    string
	modeErr error
	cat     string
	catErr  error
	open    int64
	openErr error
	setErr  error
	sets    []pmSetCall
}

type pmSetCall struct {
	accountID    int64
	mode, before string
	userID       int64
}

func (f *pmStoreFake) PositionMode(context.Context, int64) (string, error) {
	return f.mode, f.modeErr
}
func (f *pmStoreFake) ClientCategory(context.Context, int64) (string, error) {
	return f.cat, f.catErr
}
func (f *pmStoreFake) OpenPositionCount(context.Context, int64) (int64, error) {
	return f.open, f.openErr
}
func (f *pmStoreFake) SetPositionMode(_ context.Context, accountID int64,
	mode, before string, userID int64) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.sets = append(f.sets, pmSetCall{accountID, mode, before, userID})
	f.mode = mode
	return nil
}

// ---------------------------------------------------------------------------
// Mode reads
// ---------------------------------------------------------------------------

func TestPositionModeReads(t *testing.T) {
	ctx := context.Background()
	// "" reads NETTING — the retail-safe default for pre-233 rows.
	svc := NewPositionModeService(&pmStoreFake{mode: ""})
	if m, err := svc.Mode(ctx, 7); err != nil || m != ModeNetting {
		t.Fatalf("empty mode: %q %v, want NETTING", m, err)
	}
	svc = NewPositionModeService(&pmStoreFake{mode: ModeHedging})
	if m, err := svc.Mode(ctx, 7); err != nil || m != ModeHedging {
		t.Fatalf("hedging: %q %v", m, err)
	}
	// PositionModeSource delegation — same contract.
	if m, err := svc.PositionMode(ctx, 7); err != nil || m != ModeHedging {
		t.Fatalf("PositionMode source: %q %v", m, err)
	}
	// Store error propagates fail-closed (never silently NETTING).
	svc = NewPositionModeService(&pmStoreFake{modeErr: fmt.Errorf("pg down")})
	if _, err := svc.Mode(ctx, 7); err == nil {
		t.Fatal("store error must propagate")
	} else {
		requireCode(t, err, CodeRiskLimitsInternal)
	}
}

// ---------------------------------------------------------------------------
// SetMode guards
// ---------------------------------------------------------------------------

func TestPositionModeSetModeGuards(t *testing.T) {
	ctx := context.Background()
	mk := func(f *pmStoreFake) *PositionModeService {
		return NewPositionModeService(f)
	}

	// Invalid value → INVALID_REQUEST before any store read.
	f := &pmStoreFake{mode: ModeNetting, cat: CategoryProfessional}
	svc := mk(f)
	for _, bad := range []string{"", "FLAT", "net", "HEDGE"} {
		if _, err := svc.SetMode(ctx, 7, bad, 1); err == nil {
			t.Fatalf("mode %q must reject", bad)
		} else {
			requireCode(t, err, CodeLeverageInvalid)
		}
	}
	if len(f.sets) != 0 {
		t.Fatal("invalid request must not write")
	}

	// Same mode → idempotent success, no write.
	if m, err := svc.SetMode(ctx, 7, "netting", 1); err != nil || m != ModeNetting {
		t.Fatalf("same-mode: %q %v", m, err)
	}
	if len(f.sets) != 0 {
		t.Fatal("idempotent same-mode must not write")
	}

	// RETAIL → HEDGING rejected (ESMA netting mandate), and so is an
	// uncategorized account (fail-closed retail treatment).
	for _, cat := range []string{CategoryRetail, ""} {
		f := &pmStoreFake{mode: ModeNetting, cat: cat}
		svc = mk(f)
		if _, err := svc.SetMode(ctx, 7, ModeHedging, 1); err == nil {
			t.Fatalf("cat %q → HEDGING must reject", cat)
		} else {
			requireCode(t, err, "PRODUCT_NOT_PERMITTED")
		}
		if len(f.sets) != 0 {
			t.Fatal("retail hedge must not write")
		}
	}

	// Professional with open positions → blocked (bookkeeping can't mix).
	f = &pmStoreFake{mode: ModeNetting, cat: CategoryProfessional, open: 2}
	svc = mk(f)
	if _, err := svc.SetMode(ctx, 7, ModeHedging, 1); err == nil {
		t.Fatal("open positions must block the toggle")
	} else {
		requireCode(t, err, CodePositionModeBlocked)
	}
	if len(f.sets) != 0 {
		t.Fatal("blocked toggle must not write")
	}

	// Professional, zero positions → write lands with the before image.
	f = &pmStoreFake{mode: ModeNetting, cat: CategoryProfessional, open: 0}
	svc = mk(f)
	if m, err := svc.SetMode(ctx, 7, ModeHedging, 9009); err != nil || m != ModeHedging {
		t.Fatalf("toggle: %q %v", m, err)
	}
	if len(f.sets) != 1 || f.sets[0].mode != ModeHedging ||
		f.sets[0].before != ModeNetting || f.sets[0].userID != 9009 {
		t.Fatalf("set call: %+v", f.sets)
	}
	// Eligible counterparty can hedge too.
	f = &pmStoreFake{mode: ModeNetting, cat: CategoryEligible}
	svc = mk(f)
	if _, err := svc.SetMode(ctx, 7, ModeHedging, 1); err != nil {
		t.Fatalf("ECP hedge: %v", err)
	}
	// Retail can always move HEDGING → NETTING (the retail-safe direction).
	f = &pmStoreFake{mode: ModeHedging, cat: CategoryRetail}
	svc = mk(f)
	if m, err := svc.SetMode(ctx, 7, ModeNetting, 1); err != nil || m != ModeNetting {
		t.Fatalf("hedging→netting must pass for retail: %q %v", m, err)
	}
}

func TestPositionModeStoreErrors(t *testing.T) {
	ctx := context.Background()
	// Mode read failure aborts the toggle.
	f := &pmStoreFake{modeErr: fmt.Errorf("pg down")}
	svc := NewPositionModeService(f)
	if _, err := svc.SetMode(ctx, 7, ModeHedging, 1); err == nil {
		t.Fatal("mode read error must abort")
	}
	// Category read failure aborts.
	f = &pmStoreFake{mode: ModeNetting, catErr: fmt.Errorf("pg down")}
	svc = NewPositionModeService(f)
	if _, err := svc.SetMode(ctx, 7, ModeHedging, 1); err == nil {
		t.Fatal("category read error must abort")
	}
	// Open-position count failure aborts (category passes first).
	f = &pmStoreFake{mode: ModeNetting, cat: CategoryProfessional,
		openErr: fmt.Errorf("pg down")}
	svc = NewPositionModeService(f)
	if _, err := svc.SetMode(ctx, 7, ModeHedging, 1); err == nil {
		t.Fatal("open-count error must abort")
	}
	// Write failure surfaces.
	f = &pmStoreFake{mode: ModeNetting, cat: CategoryProfessional,
		setErr: fmt.Errorf("pg down")}
	svc = NewPositionModeService(f)
	if _, err := svc.SetMode(ctx, 7, ModeHedging, 1); err == nil {
		t.Fatal("write error must surface")
	}
}

// ---------------------------------------------------------------------------
// Gated PostgreSQL — PgPositionModeStore round-trip + audit
// ---------------------------------------------------------------------------

func TestPgPositionModeStore(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	st := NewPgPositionModeStore(pool)

	acct := liqSeedAccount(t, pool, "PROFESSIONAL")

	// Defaults: mig 233 column default reads NETTING; category round-trips.
	if m, err := st.PositionMode(ctx, acct); err != nil || m != ModeNetting {
		t.Fatalf("mode: %q %v", m, err)
	}
	if c, err := st.ClientCategory(ctx, acct); err != nil || c != CategoryProfessional {
		t.Fatalf("category: %q %v", c, err)
	}
	// Missing rows read fail-safe rather than erroring.
	if m, err := st.PositionMode(ctx, 4242424242); err != nil || m != ModeNetting {
		t.Fatalf("missing account mode: %q %v", m, err)
	}
	if c, err := st.ClientCategory(ctx, 4242424242); err != nil || c != CategoryRetail {
		t.Fatalf("missing account category: %q %v", c, err)
	}

	// Audited write: mode flips and the admin_audit_log row commits.
	if err := st.SetPositionMode(ctx, acct, ModeHedging, ModeNetting, 4242); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	if m, err := st.PositionMode(ctx, acct); err != nil || m != ModeHedging {
		t.Fatalf("mode after write: %q %v", m, err)
	}
	var before, after string
	if err := pool.QueryRow(ctx, `
		SELECT before_state->>'position_mode', after_state->>'position_mode'
		FROM admin_audit_log
		WHERE action='account.position_mode.update' AND target_id=$1
		ORDER BY id DESC LIMIT 1`, acct).Scan(&before, &after); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if before != ModeNetting || after != ModeHedging {
		t.Fatalf("audit images: %s → %s", before, after)
	}
}

func TestPgPositionModeServiceGuards(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	svc := NewPositionModeService(NewPgPositionModeStore(pool))

	retail := liqSeedAccount(t, pool, "RETAIL")
	pro := liqSeedAccount(t, pool, "PROFESSIONAL")
	inst := liqSeedInstrument(t, pool)

	// Retail hedging ban survives the real store.
	if _, err := svc.SetMode(ctx, retail, ModeHedging, 1); err == nil {
		t.Fatal("retail hedge must reject")
	} else {
		requireCode(t, err, "PRODUCT_NOT_PERMITTED")
	}

	// Professional toggles freely with a flat book.
	if m, err := svc.SetMode(ctx, pro, ModeHedging, 1); err != nil || m != ModeHedging {
		t.Fatalf("pro hedge: %q %v", m, err)
	}
	// An open position (qty <> 0) blocks the way back.
	liqSeedPosition(t, pool, pro, inst, "LONG", "100", "1.10",
		liqStr("1.10"), nil, "0", "0")
	if _, err := svc.SetMode(ctx, pro, ModeNetting, 1); err == nil {
		t.Fatal("open position must block the toggle")
	} else {
		requireCode(t, err, CodePositionModeBlocked)
	}
	// Flatten the row (qty 0) → toggle unblocks.
	if _, err := pool.Exec(ctx,
		`UPDATE positions SET quantity=0 WHERE account_id=$1`, pro); err != nil {
		t.Fatal(err)
	}
	if m, err := svc.SetMode(ctx, pro, ModeNetting, 1); err != nil || m != ModeNetting {
		t.Fatalf("flattened book must toggle: %q %v", m, err)
	}
}
