// Phase-11 Task 11.3.9 integration tests — migration 198 round-trip,
// fee-schedule admin lifecycle + audit chain, resolution/charge against
// real PostgreSQL + ledger, free-tier usage, and currency-conversion
// persistence. Same DSN/Redis gating as integration_test.go:
// EXC_PG_TEST=1, EXC_TEST_DSN, EXC_REDIS_TEST_*.
package funding

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// itFeeSchema layers the Task 11.3.9 prerequisites onto the shared
// fixture: migration 009 (audit_hash_chain — admin.Log's tamper-evident
// anchor) and 198 (funding_fee_tiers + funding_fee_free_usage +
// funding_currency_conversions). itSchema already supplies accounts,
// funding_transactions and bank_method_enum (007).
func itFeeSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	itSchema(t, ctx, pool)
	execSQLFile(t, ctx, pool, "009_create_audit_hash_chain.up.sql")
	execSQLFile(t, ctx, pool, "198_funding_fee_schedule.up.sql")
}

func grantRole(role string) RoleResolver {
	return func(context.Context, int64) (string, error) { return role, nil }
}

// TestITMigration198RoundTrip — 198 up then down must execute cleanly
// and remove every object it created.
func TestITMigration198RoundTrip(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itSchema(t, ctx, pool) // accounts + funding_transactions + bank_method_enum

	execSQLFile(t, ctx, pool, "198_funding_fee_schedule.up.sql")
	execSQLFile(t, ctx, pool, "198_funding_fee_schedule.down.sql")

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_name IN ('funding_fee_tiers','funding_fee_free_usage',
		                     'funding_currency_conversions')`).Scan(&n); err != nil {
		t.Fatalf("post-down probe: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d migration-198 tables survived the down migration", n)
	}
}

// TestITFeeScheduleAdminLifecycle — create → get → update (successor
// version) → version chain → retire, with the role gate, resolution and
// the in-transaction audit/hash-chain anchor all verified against real
// PostgreSQL.
func TestITFeeScheduleAdminLifecycle(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itFeeSchema(t, ctx, pool)
	store := NewPgFeeScheduleStore(pool)
	actor := FeeAdminActor{UserID: 500, ClientIP: "127.0.0.1"}

	svc, err := NewFeeScheduleService(store, grantRole("Finance Ops"))
	if err != nil {
		t.Fatalf("svc: %v", err)
	}

	// Role gate: Support Agent + a nil resolver both fail closed.
	deny, _ := NewFeeScheduleService(store, grantRole("Support Agent"))
	if _, err := deny.Create(ctx, actor, FeeTierCreate{}); codeOf(err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("deny role: %v", err)
	}
	nores, _ := NewFeeScheduleService(store, nil)
	if _, err := nores.List(ctx, actor, FeeTierFilter{}); codeOf(err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("nil resolver: %v", err)
	}

	// Create version 1.
	eff := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	maxF := "100"
	t1, err := svc.Create(ctx, actor, FeeTierCreate{
		Rail: "swift", Currency: "usd", Direction: "withdrawal",
		AccountTier: "T2",
		FlatFee:     "5", PercentageBps: "10", MinFee: "1", MaxFee: &maxF,
		EffectiveDate: &eff,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if t1.Version != 1 || t1.Rail != "SWIFT" || t1.Currency != "USD" ||
		t1.Direction != "WITHDRAWAL" || t1.AccountTier != "T2" ||
		t1.CreatedBy != 500 {
		t.Fatalf("v1 %+v", t1)
	}

	// Duplicate (group, effective_date) → coded conflict.
	_, err = svc.Create(ctx, actor, FeeTierCreate{
		Rail: "swift", Currency: "USD", Direction: "withdrawal",
		AccountTier: "T2", FlatFee: "5", PercentageBps: "10", MinFee: "1",
		EffectiveDate: &eff,
	})
	if codeOf(err) != "FEE_INVALID_INPUT" {
		t.Fatalf("dup create: %v", err)
	}

	// Update → successor version linked to v1.
	t2, err := svc.Update(ctx, actor, t1.ID, FeeTierUpdate{
		FlatFee: "6", PercentageBps: "10", MinFee: "1", MaxFee: &maxF,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if t2.Version != 2 || t2.SupersedesID == nil || *t2.SupersedesID != t1.ID {
		t.Fatalf("v2 %+v", t2)
	}

	// Version chain: both rows, newest first.
	chain, err := svc.Versions(ctx, actor, t1.ID)
	if err != nil || len(chain) != 2 || chain[0].ID != t2.ID {
		t.Fatalf("chain %+v err=%v", chain, err)
	}

	// Resolution prefers the newest effective version (v2 → flat 6).
	res, err := store.FeeTierAt(ctx, "SWIFT", "USD", "WITHDRAWAL", "T2",
		time.Now().UTC())
	if err != nil || res == nil || res.ID != t2.ID {
		t.Fatalf("resolve %+v err=%v", res, err)
	}

	// Future-dated row is NOT yet resolvable.
	future := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	f1, err := svc.Create(ctx, actor, FeeTierCreate{
		Rail: "swift", Currency: "USD", Direction: "withdrawal",
		AccountTier: "T1", FlatFee: "9", PercentageBps: "0", MinFee: "0",
		EffectiveDate: &future,
	})
	if err != nil {
		t.Fatalf("future create: %v", err)
	}
	r2, err := store.FeeTierAt(ctx, "SWIFT", "USD", "WITHDRAWAL", "T1",
		time.Now().UTC())
	if err != nil {
		t.Fatalf("future resolve: %v", err)
	}
	if r2 != nil {
		t.Fatalf("future-dated row %d resolved before effective_date", f1.ID)
	}

	// Retire v2 → resolution falls back to v1 (still live).
	if _, err := svc.Retire(ctx, actor, t2.ID); err != nil {
		t.Fatalf("retire: %v", err)
	}
	res, err = store.FeeTierAt(ctx, "SWIFT", "USD", "WITHDRAWAL", "T2",
		time.Now().UTC())
	if err != nil || res == nil || res.ID != t1.ID {
		t.Fatalf("post-retire resolve %+v err=%v", res, err)
	}
	// Repeat retire → coded conflict; unknown id → FEE_TIER_NOT_FOUND.
	if _, err := svc.Retire(ctx, actor, t2.ID); codeOf(err) != "FEE_INVALID_INPUT" {
		t.Fatalf("re-retire: %v", err)
	}
	if _, err := svc.Retire(ctx, actor, 999999); codeOf(err) != "FEE_TIER_NOT_FOUND" {
		t.Fatalf("unknown retire: %v", err)
	}

	// Audit: every mutation bound an admin_audit_log row + hash-chain link.
	// Committed mutations (create v1, update v2, future-dated create,
	// retire) → 4 audit rows; the rolled-back duplicate leaves none.
	var auditN, chainN int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE target_type='funding_fee_tier'`).Scan(&auditN); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if auditN != 4 {
		t.Fatalf("audit rows=%d, want 4 (dup-create must roll back)", auditN)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_hash_chain
		WHERE table_name='admin_audit_log'`).Scan(&chainN); err != nil {
		t.Fatalf("chain count: %v", err)
	}
	if chainN != auditN {
		t.Fatalf("audit=%d chain=%d — rows must anchor 1:1", auditN, chainN)
	}
}

// TestITFeeEstimateCharge — the engine resolves the seeded schedule and
// Charge posts the real DR liability / CR fee-revenue journal with the
// wallet effect and idempotent replay. Requires Redis (itLedger skips).
func TestITFeeEstimateCharge(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itFeeSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "10000")

	store := NewPgFeeScheduleStore(pool)
	adm, err := NewFeeScheduleService(store, grantRole("Finance Ops"))
	if err != nil {
		t.Fatalf("adm: %v", err)
	}
	eff := time.Now().UTC().Add(-time.Hour)
	if _, err := adm.Create(ctx, FeeAdminActor{UserID: 500}, FeeTierCreate{
		Rail: "swift", Currency: "usd", Direction: "withdrawal",
		AccountTier: "T2", // accounts fixture: kyc_tier T2
		FlatFee:     "5", PercentageBps: "10", MinFee: "1",
		EffectiveDate: &eff,
	}); err != nil {
		t.Fatalf("tier: %v", err)
	}

	svc, err := NewFeeService(store, NewPgStore(pool), ledgerPosterAdapter{led})
	if err != nil {
		t.Fatalf("fee svc: %v", err)
	}

	// Estimate: 5 + 10000×10/10000 = 15, net 9985, no usage consumed.
	est, err := svc.Estimate(ctx, FeeEstimateRequest{
		AccountID: 1, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(10_000)})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.Fee.String() != "15" || est.NetAmount.String() != "9985" ||
		est.AccountTier != "T2" {
		t.Fatalf("estimate %+v", est)
	}

	// A funding transaction the fee attaches to.
	var txID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO funding_transactions (account_id, currency, type, amount)
		VALUES (1, 'USD', 'WITHDRAWAL', 10000) RETURNING id`).Scan(&txID); err != nil {
		t.Fatalf("funding tx: %v", err)
	}

	a, err := svc.Charge(ctx, FeeCharge{
		AccountID: 1, UserID: 100, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(10_000),
		FundingTxID: txID, Idempotency: "wd-1",
	})
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if a.Fee.String() != "15" || a.JournalID == 0 {
		t.Fatalf("assessment %+v", a)
	}
	avail, _ := balRow(t, ctx, pool, 1, "USD")
	if !avail.Equal(decimal.MustFromString("9985")) {
		t.Fatalf("wallet effect: avail=%s, want 9985", avail)
	}
	// GL: DR customer_liability / CR funding_fee_revenue.
	var rev string
	if err := pool.QueryRow(ctx, `
		SELECT credit_amount::text FROM ledger_lines
		WHERE journal_entry_id = $1 AND account_code LIKE '4200_FUNDING_FEE_REVENUE%'`,
		a.JournalID).Scan(&rev); err != nil {
		t.Fatalf("revenue line: %v", err)
	}
	if rev != "15.00000000" {
		t.Fatalf("revenue line %s, want 15", rev)
	}

	// Replay: same FundingTxID + Idempotency → the prior journal, no
	// double charge.
	a2, err := svc.Charge(ctx, FeeCharge{
		AccountID: 1, UserID: 100, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(10_000),
		FundingTxID: txID, Idempotency: "wd-1",
	})
	if err != nil || a2.JournalID != a.JournalID {
		t.Fatalf("replay %+v err=%v", a2, err)
	}
	avail, _ = balRow(t, ctx, pool, 1, "USD")
	if !avail.Equal(decimal.MustFromString("9985")) {
		t.Fatalf("replay double-charged: avail=%s", avail)
	}

	// Fee ≥ amount → 422 FUNDING_FEE_EXCEEDS_AMOUNT (flat 5 + 10bps on
	// a 1.00 movement computes 5.001 — the fee must reject, never
	// silently produce a negative net).
	_, err = svc.Estimate(ctx, FeeEstimateRequest{
		AccountID: 1, Rail: "SWIFT", Currency: "USD",
		Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(1)})
	if codeOf(err) != "FUNDING_FEE_EXCEEDS_AMOUNT" {
		t.Fatalf("exceeds: %v", err)
	}

	// No schedule → FEE_TIER_NOT_FOUND, never "free".
	_, err = svc.Estimate(ctx, FeeEstimateRequest{
		AccountID: 1, Rail: "SEPA", Currency: "EUR",
		Direction: "DEPOSIT", Amount: decimal.NewFromInt(10)})
	if codeOf(err) != "FEE_TIER_NOT_FOUND" {
		t.Fatalf("no tier: %v", err)
	}
}

// TestITFreeTierUsage — free_tier_monthly_count grants N free movements
// per UTC month; consumed slots post no journal and are counted in
// funding_fee_free_usage.
func TestITFreeTierUsage(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itFeeSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "1000")

	store := NewPgFeeScheduleStore(pool)
	adm, _ := NewFeeScheduleService(store, grantRole("Finance Ops"))
	eff := time.Now().UTC().Add(-time.Hour)
	free2 := 2
	if _, err := adm.Create(ctx, FeeAdminActor{UserID: 500}, FeeTierCreate{
		Rail: "ach", Currency: "usd", Direction: "withdrawal",
		AccountTier: "T2", FlatFee: "1", PercentageBps: "0", MinFee: "0",
		FreeTierMonthlyCount: &free2, EffectiveDate: &eff,
	}); err != nil {
		t.Fatalf("tier: %v", err)
	}
	svc, err := NewFeeService(store, NewPgStore(pool), ledgerPosterAdapter{led})
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	charge := func(txID int64) *FeeAssessment {
		t.Helper()
		a, err := svc.Charge(ctx, FeeCharge{
			AccountID: 1, UserID: 100, Rail: "ACH", Currency: "USD",
			Direction: "WITHDRAWAL", Amount: decimal.NewFromInt(100),
			FundingTxID: txID,
		})
		if err != nil {
			t.Fatalf("charge tx=%d: %v", txID, err)
		}
		return a
	}
	newTx := func() int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO funding_transactions (account_id, currency, type, amount)
			VALUES (1, 'USD', 'WITHDRAWAL', 100) RETURNING id`).Scan(&id); err != nil {
			t.Fatalf("tx: %v", err)
		}
		return id
	}

	a1 := charge(newTx())
	a2 := charge(newTx())
	if !a1.FreeApplied || !a2.FreeApplied || a1.Fee.String() != "0" {
		t.Fatalf("free slots: %+v %+v", a1, a2)
	}
	a3 := charge(newTx())
	if a3.FreeApplied || a3.Fee.String() != "1" {
		t.Fatalf("3rd charge should bill: %+v", a3)
	}

	var used int
	if err := pool.QueryRow(ctx, `
		SELECT used_count FROM funding_fee_free_usage
		WHERE account_id = 1 AND direction = 'WITHDRAWAL'`).Scan(&used); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if used != 2 {
		t.Fatalf("used_count=%d, want 2", used)
	}
	// Wallet: only the billed fee left the balance (1000 − 1).
	avail, _ := balRow(t, ctx, pool, 1, "USD")
	if !avail.Equal(decimal.MustFromString("999")) {
		t.Fatalf("avail=%s, want 999", avail)
	}
}

// ---------------------------------------------------------------------------
// Conversion persistence — a static rate source exercises Quote→insert→
// history against real PostgreSQL.
// ---------------------------------------------------------------------------

type staticRateSource struct{ rate string }

func (s staticRateSource) MidRate(_ context.Context, from, to string) (MidRate, error) {
	return MidRate{
		Rate:    decimal.MustFromString(s.rate),
		Source:  "test_src",
		ValidAt: time.Now().UTC(),
	}, nil
}

func TestITConversionPersistence(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itFeeSchema(t, ctx, pool)
	store := NewPgConversionStore(pool)

	svc, err := NewConversionService(staticRateSource{rate: "1.08"},
		store, NewPgStore(pool))
	if err != nil {
		t.Fatalf("svc: %v", err)
	}

	// EUR → USD at mid 1.08, 50bps spread: applied = 1.08×0.995 = 1.0746;
	// 100 EUR → 107.46 USD.
	res, err := svc.Quote(ctx, ConversionRequest{
		AccountID: 1, FromCurrency: "EUR", Amount: decimal.NewFromInt(100)})
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if !res.Converted || res.AmountTo.String() != "107.46" ||
		res.RateSource != "test_src" || res.Record == nil {
		t.Fatalf("quote %+v", res)
	}

	// The record round-trips through the read path.
	hist, err := svc.History(ctx, 1, 10)
	if err != nil || len(hist) != 1 {
		t.Fatalf("history %+v err=%v", hist, err)
	}
	rec := hist[0]
	if rec.FromCurrency != "EUR" || rec.ToCurrency != "USD" ||
		rec.AmountFrom.String() != "100" || rec.AmountTo.String() != "107.46" ||
		rec.MidRate.String() != "1.08" || rec.RateApplied.String() != "1.0746" {
		t.Fatalf("rec %+v", rec)
	}

	// Same-currency → converted=false, no record.
	res2, err := svc.Quote(ctx, ConversionRequest{
		AccountID: 1, FromCurrency: "USD", ToCurrency: "USD",
		Amount: decimal.NewFromInt(5)})
	if err != nil || res2.Converted {
		t.Fatalf("same-ccy %+v err=%v", res2, err)
	}
	hist2, _ := svc.History(ctx, 1, 10)
	if len(hist2) != 1 {
		t.Fatalf("same-ccy wrote a record: %+v", hist2)
	}

	// Nil source → fail closed PRICE_ORACLE_UNAVAILABLE.
	svc2, _ := NewConversionService(nil, store, NewPgStore(pool))
	_, err = svc2.Quote(ctx, ConversionRequest{
		AccountID: 1, FromCurrency: "EUR", ToCurrency: "GBP",
		Amount: decimal.NewFromInt(10)})
	if codeOf(err) != "PRICE_ORACLE_UNAVAILABLE" {
		t.Fatalf("nil src: %v", err)
	}
}
