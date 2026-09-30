// Tests for Task 20.3.13 — daily account snapshots + per-account hash
// chain (spec §16.7, §24 #345/#362).
//
// Canonical-form tests are pure unit tests; build/verify coverage is
// EXC_PG_TEST=1 gated against the dev Postgres (migration 093 applied).
package analytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
)

func TestCanonicalPositionsJSON(t *testing.T) {
	if got := CanonicalPositionsJSON(nil); got != "[]" {
		t.Fatalf("empty = %q", got)
	}
	pos := []SnapshotPosition{
		{PositionID: 9, InstrumentID: 2, Symbol: "USD/JPY", Side: "SHORT",
			Quantity: "500.00000000", EntryPrice: "149.12340000",
			UnrealizedPnL: "1.00000000", RealizedPnL: "0.00000000",
			MarginUsed: "10.00000000"},
		{PositionID: 4, InstrumentID: 1, Symbol: "EUR/USD", Side: "LONG",
			Quantity: "1000.00000000", EntryPrice: "1.08501000",
			MarkPrice: "1.08502000", LiquidationPrice: "1.00000000",
			UnrealizedPnL: "0.50000000", RealizedPnL: "-1.00000000",
			MarginUsed: "36.16666667"},
	}
	got := CanonicalPositionsJSON(pos)
	// Sorted by position_id, compact, struct-order keys.
	if !strings.HasPrefix(got, `[{"position_id":4,`) {
		t.Fatalf("not sorted/prefixed canonically: %s", got)
	}
	if strings.Contains(got, " ") {
		t.Fatalf("canonical JSON must be compact: %s", got)
	}
	if !strings.Contains(got, `"symbol":"EUR/USD"`) {
		t.Fatalf("missing symbol: %s", got)
	}
	// Deterministic: same input order-shuffled → same output.
	rev := []SnapshotPosition{pos[1], pos[0]}
	if CanonicalPositionsJSON(rev) != got {
		t.Fatal("canonical form must be input-order independent")
	}
	// Re-canonicalize through decode — the verifier path.
	dec, err := decanonicalizePositions(got)
	if err != nil || dec != got {
		t.Fatalf("decanonicalize = %q err=%v", dec, err)
	}
}

func TestSnapshotHashDeterministic(t *testing.T) {
	day := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 9, 27, 23, 59, 0, 123456, time.UTC)
	preimage := fmt.Sprintf("BS1|%d|%s|%s|%s|%s|%s|%d|%s",
		42, "USD", "100.50000000", "0.00000000", "[]",
		"2026-09-27", created.UnixMicro(), audit.GenesisPrevHash)
	want := sha256.Sum256([]byte(preimage))
	got := SnapshotHash(42, "USD", "100.50000000", "0.00000000",
		"[]", day, created, audit.GenesisPrevHash)
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("hash = %s, want %s", got, hex.EncodeToString(want[:]))
	}
	// prev_hash changes the digest — chain link is in the preimage.
	other := SnapshotHash(42, "USD", "100.50000000", "0.00000000",
		"[]", day, created, strings.Repeat("ab", 32))
	if other == got {
		t.Fatal("prev_hash must be inside the preimage")
	}
}

// ---------------------------------------------------------------------------
// PG-gated: two accounts × two days, chain links, tamper detection,
// tip-rebuild idempotence, gap refusal, history read, positions_json.
// ---------------------------------------------------------------------------

func snapshotTestAccounts(t *testing.T, pool *pgxpool.Pool) (a1, a2, posAcct int64, posID int64) {
	t.Helper()
	ctx := context.Background()
	tag := fmt.Sprint(time.Now().UnixNano())
	seq := 0
	mkAcct := func() int64 {
		seq++
		var uid int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`,
			fmt.Sprintf("snap-%s-%d@example.invalid", tag, seq)).Scan(&uid); err != nil {
			t.Fatalf("user: %v", err)
		}
		var aid int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
			uid).Scan(&aid); err != nil {
			t.Fatalf("account: %v", err)
		}
		t.Cleanup(func() {
			pool.Exec(context.Background(), `DELETE FROM balance_snapshots WHERE account_id=$1`, aid)
			pool.Exec(context.Background(), `DELETE FROM positions WHERE account_id=$1`, aid)
			pool.Exec(context.Background(), `DELETE FROM balances WHERE account_id=$1`, aid)
			pool.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, aid)
			pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, uid)
		})
		return aid
	}
	a1, a2 = mkAcct(), mkAcct()

	// a1: USD (nonzero) + EUR (zero-locked row) balances; one open position.
	if _, err := pool.Exec(ctx, `
		INSERT INTO balances (account_id, currency, available, locked) VALUES
		($1,'USD',1000.5,25.25), ($1,'EUR',0,0)`, a1); err != nil {
		t.Fatalf("a1 balances: %v", err)
	}
	var instID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol='EUR/USD'`).Scan(&instID); err != nil {
		t.Fatalf("instrument: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity,
		    entry_price, mark_price, liquidation_price, unrealized_pnl,
		    realized_pnl, margin_used)
		VALUES ($1,$2,'LONG',1000,1.08501,1.08502,1.0,0.5,-1,36.16666667)
		RETURNING id`, a1, instID).Scan(&posID); err != nil {
		t.Fatalf("position: %v", err)
	}
	posAcct = a1
	// a2: a single zero/zero GBP row — funded but empty account.
	if _, err := pool.Exec(ctx, `
		INSERT INTO balances (account_id, currency, available, locked)
		VALUES ($1,'GBP',0,0)`, a2); err != nil {
		t.Fatalf("a2 balances: %v", err)
	}
	return a1, a2, posAcct, posID
}

// resetSnapshotTable gives the chain tests a deterministic starting
// state on the shared dev DB — the table only exists since migration
// 093 and only these tests write it (the builder covers EVERY funded
// account, so a foreign fixture's stale tip would refuse the backfill).
func resetSnapshotTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`SELECT 1 FROM balance_snapshots LIMIT 1`); err != nil {
		t.Skip("migration 093 (balance_snapshots) not applied")
	}
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM balance_snapshots`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM balance_snapshots`)
	})
}

func TestPgSnapshotBuildVerifyChain(t *testing.T) {
	pool := pgTestPoolT(t)
	resetSnapshotTable(t, pool)
	a1, a2, _, posID := snapshotTestAccounts(t, pool)
	ctx := context.Background()

	clock := time.Date(2026, 9, 27, 23, 59, 0, 0, time.UTC)
	b := NewSnapshotBuilder(pool).WithClock(func() time.Time { return clock })

	d1 := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

	res, err := b.BuildDay(ctx, d1)
	if err != nil {
		t.Fatalf("build d1: %v", err)
	}
	if res.Accounts < 2 || res.Rows < 3 {
		t.Fatalf("d1 result = %+v", res)
	}

	// Second day: pinned clock differs → hash preimages differ.
	b.WithClock(func() time.Time { return clock.Add(24 * time.Hour) })
	res2, err := b.BuildDay(ctx, d2)
	if err != nil {
		t.Fatalf("build d2: %v", err)
	}
	if res2.Rows != res.Rows {
		t.Fatalf("d2 rows=%d vs d1 %d", res2.Rows, res.Rows)
	}

	// ---- per-account chain verification (both accounts) ----
	for _, acct := range []int64{a1, a2} {
		rep, err := VerifyAccountChain(ctx, pool, acct)
		if err != nil {
			t.Fatalf("verify acct %d: %v", acct, err)
		}
		if !rep.OK {
			t.Fatalf("acct %d violations: %+v", acct, rep.Violations)
		}
		wantRows := map[int64]int{a1: 4, a2: 2}[acct] // a1: 2 ccy × 2d; a2: 1 × 2
		if rep.RowsChecked != wantRows {
			t.Fatalf("acct %d rows = %d, want %d", acct, rep.RowsChecked, wantRows)
		}
	}

	// ---- genesis + explicit link check on a1 ----
	type row struct{ hash, prev string }
	var links []row
	lr, err := pool.Query(ctx, `
		SELECT hash, prev_hash FROM balance_snapshots
		WHERE account_id=$1 ORDER BY snapshot_date, id`, a1)
	if err != nil {
		t.Fatalf("links read: %v", err)
	}
	for lr.Next() {
		var r row
		lr.Scan(&r.hash, &r.prev)
		links = append(links, r)
	}
	lr.Close()
	if links[0].prev != audit.GenesisPrevHash {
		t.Fatalf("genesis prev = %s", links[0].prev)
	}
	for i := 1; i < len(links); i++ {
		if links[i].prev != links[i-1].hash {
			t.Fatalf("link %d broken: %s != %s", i, links[i].prev, links[i-1].hash)
		}
	}

	// ---- positions_json embedded + canonical ----
	// jsonb stores reordered keys with whitespace — assert on the
	// re-canonicalized form (the byte-stable preimage the hash covers).
	var posJSON string
	if err := pool.QueryRow(ctx, `
		SELECT positions_json::text FROM balance_snapshots
		WHERE account_id=$1 AND currency='USD' AND snapshot_date=$2`,
		a1, d1).Scan(&posJSON); err != nil {
		t.Fatalf("positions read: %v", err)
	}
	canon, err := decanonicalizePositions(posJSON)
	if err != nil {
		t.Fatalf("positions_json not canonical schema: %v — %s", err, posJSON)
	}
	if !strings.Contains(canon, fmt.Sprintf(`"position_id":%d`, posID)) {
		t.Fatalf("positions_json missing position %d: %s", posID, canon)
	}
	if !strings.Contains(canon, `"symbol":"EUR/USD"`) ||
		!strings.Contains(canon, `"quantity":"1000.00000000"`) {
		t.Fatalf("positions_json shape: %s", canon)
	}

	// ---- tip rebuild is idempotent; gap backfill is refused ----
	b.WithClock(func() time.Time { return clock.Add(48 * time.Hour) })
	if _, err := b.BuildDay(ctx, d2); err != nil {
		t.Fatalf("tip rebuild must succeed: %v", err)
	}
	// Still verifies after the rewrite.
	if rep, _ := VerifyAccountChain(ctx, pool, a1); !rep.OK {
		t.Fatalf("post-rebuild violations: %+v", rep.Violations)
	}
	// A third day exists now? No — d2 was rebuilt. Try a GAP date (d1-1d).
	if _, err := b.BuildDay(ctx, d1.AddDate(0, 0, -1)); err == nil {
		t.Fatal("gap backfill must be refused (would fork the chain)")
	}

	// ---- tamper detection: rewrite one available figure ----
	if _, err := pool.Exec(ctx, `
		UPDATE balance_snapshots SET available = available + 1
		WHERE account_id=$1 AND currency='USD' AND snapshot_date=$2`,
		a1, d1); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	rep, err := VerifyAccountChain(ctx, pool, a1)
	if err != nil {
		t.Fatalf("verify after tamper: %v", err)
	}
	if rep.OK || len(rep.Violations) == 0 {
		t.Fatal("tampered row must fail verification")
	}
	if rep.Violations[0].Field != "hash" {
		t.Fatalf("violation field = %s, want hash", rep.Violations[0].Field)
	}
}

func TestPgSnapshotHistory(t *testing.T) {
	pool := pgTestPoolT(t)
	resetSnapshotTable(t, pool)
	a1, _, _, _ := snapshotTestAccounts(t, pool)
	ctx := context.Background()
	b := NewSnapshotBuilder(pool)
	d1 := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	if _, err := b.BuildDay(ctx, d1); err != nil {
		t.Fatalf("build d1: %v", err)
	}
	if _, err := b.BuildDay(ctx, d2); err != nil {
		t.Fatalf("build d2: %v", err)
	}

	st := NewSnapshotStore(pool)
	// All history: newest-first, both dates retained.
	rows, total, err := st.History(ctx, a1, nil, nil, 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if total != 4 || len(rows) != 4 {
		t.Fatalf("history rows=%d total=%d, want 4/4", len(rows), total)
	}
	if rows[0].SnapshotDate.Format("2006-01-02") != "2026-09-28" {
		t.Fatalf("newest first violated: %v", rows[0].SnapshotDate)
	}
	// ?date= narrows to one day.
	one, total, err := st.History(ctx, a1, &d1, nil, 50)
	if err != nil || total != 2 || len(one) != 2 {
		t.Fatalf("date filter rows=%d total=%d", len(one), total)
	}
	for _, r := range one {
		if r.SnapshotDate.Format("2006-01-02") != "2026-09-27" {
			t.Fatalf("date filter leak: %v", r.SnapshotDate)
		}
		if r.PrevHash == "" || r.Hash == "" {
			t.Fatalf("row missing chain fields: %+v", r)
		}
	}
	// Keyset continuation: page 1 (limit 2) then page 2 disjoint.
	p1, _, _ := st.History(ctx, a1, nil, nil, 2)
	if len(p1) != 2 {
		t.Fatalf("page1 = %d rows", len(p1))
	}
	cur := &SnapshotCursor{SnapshotDate: p1[1].SnapshotDate, ID: p1[1].ID}
	p2, _, _ := st.History(ctx, a1, nil, cur, 2)
	if len(p2) != 2 {
		t.Fatalf("page2 = %d rows", len(p2))
	}
	for _, r := range p2 {
		for _, r1 := range p1 {
			if r.ID == r1.ID {
				t.Fatalf("pages overlap on id %d", r.ID)
			}
		}
	}
	// Other accounts' rows never leak.
	a2rows, _, err := st.History(ctx, 999999999, nil, nil, 50)
	if err != nil || len(a2rows) != 0 {
		t.Fatalf("foreign account rows=%d err=%v", len(a2rows), err)
	}
	// Total field math is handler-side; store returns raw figures.
	if !one[0].Available.IsZero() && !one[0].Available.IsPositive() {
		t.Fatalf("available = %s", one[0].Available)
	}
}
