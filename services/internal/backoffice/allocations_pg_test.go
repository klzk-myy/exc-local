// allocations_pg_test.go — PostgreSQL-gated integration coverage for
// Tasks 24.3.10/.15 (EXC_PG_TEST=1 / EXC_TEST_DSN, same boTestPool
// convention as integration_test.go). Migration 055 is applied verbatim
// over minimal stub parents — the real trades table is a daily-
// partitioned parent whose id carries no UNIQUE constraint, so a
// declarative FK is structurally impossible there; the migration
// documents the same logical-ref discipline as settlement_instructions
// (migration 019).
package backoffice

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// recordingLedger satisfies LedgerTxPoster — every journal is validated
// by the real per-currency double-entry rule and captured for assertion
// (production binds settlement.LedgerService, which additionally
// resolves+writes the §5.3 journal tables).
type recordingLedger struct{ journals []ledger.Journal }

func (r *recordingLedger) PostJournal(_ context.Context, _ pgx.Tx, j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	r.journals = append(r.journals, j)
	return ledger.PostResult{JournalID: int64(len(r.journals))}, nil
}

// allocBoSchema — stub parents (accounts/instruments/trades +
// settlement_instructions/nostro_accounts/funding_ops_alerts/
// chart_of_accounts minimal columns) then 055 verbatim.
func allocBoSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	execAll(t, ctx, pool, `
		CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY, parent_account_id BIGINT);
		CREATE TABLE instruments (
		    id BIGSERIAL PRIMARY KEY, symbol VARCHAR(20),
		    base_currency VARCHAR(3), quote_currency VARCHAR(3),
		    settlement_cycle INT DEFAULT 1);
		CREATE TABLE trades (
		    id BIGSERIAL PRIMARY KEY, instrument_id BIGINT,
		    buyer_account_id BIGINT, seller_account_id BIGINT,
		    price DECIMAL(20,8), quantity DECIMAL(28,8), side CHAR(1),
		    status VARCHAR(24) DEFAULT 'COMPLETED',
		    settlement_date DATE, created_at TIMESTAMPTZ DEFAULT now());
		CREATE TABLE nostro_accounts (
		    id BIGSERIAL PRIMARY KEY, currency VARCHAR(3),
		    status VARCHAR(16) DEFAULT 'ACTIVE');
		CREATE TABLE settlement_instructions (
		    id BIGSERIAL PRIMARY KEY, trade_id BIGINT, account_id BIGINT,
		    currency VARCHAR(3), amount DECIMAL(28,8), direction VARCHAR(8),
		    settlement_date DATE, nostro_account_id BIGINT,
		    status VARCHAR(16) DEFAULT 'PENDING');
		CREATE TABLE funding_ops_alerts (
		    id BIGSERIAL PRIMARY KEY, code VARCHAR(48), severity VARCHAR(4),
		    account_id BIGINT, currency VARCHAR(3), amount DECIMAL(28,8),
		    summary TEXT, detail JSONB, created_at TIMESTAMPTZ DEFAULT now());
		CREATE TABLE chart_of_accounts (
		    account_code VARCHAR(48) PRIMARY KEY, account_name VARCHAR(128),
		    account_type VARCHAR(12), currency VARCHAR(3));`,
		"055_trade_allocations.up.sql")
}

func pgTrade(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	instrID, buyer, seller int64, qty, price string, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO trades (instrument_id, buyer_account_id, seller_account_id,
		    price, quantity, side, status, settlement_date)
		VALUES ($1,$2,$3,$4::numeric,$5::numeric,'1',$6, now()::date + 1)
		RETURNING id`,
		instrID, buyer, seller, price, qty, status).Scan(&id); err != nil {
		t.Fatalf("seed trade: %v", err)
	}
	return id
}

// TestPgAllocations_Integration exercises the full Task 24.3.10/.15
// loop on real PostgreSQL: registration → fill attach → weighted
// allocation → per-fund confirm (child instructions + 2090 rebook) →
// lock → dual-control correction → T+0 escalation — plus the DB-layer
// invariants (conservation trigger, capacity-mix trigger, append-only
// audit, fill uniqueness).
func TestPgAllocations_Integration(t *testing.T) {
	ctx, pool := boTestPool(t)
	allocBoSchema(t, ctx, pool)

	// accounts 1..3 (manager + two funds) and 99 (foreign capacity probe).
	for i := 0; i < 4; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO accounts DEFAULT VALUES`); err != nil {
			t.Fatal(err)
		}
	}
	var instrID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments (symbol, base_currency, quote_currency, settlement_cycle)
		VALUES ('EURUSD','EUR','USD',1) RETURNING id`).Scan(&instrID); err != nil {
		t.Fatal(err)
	}
	// nostro for EUR receives.
	if _, err := pool.Exec(ctx, `INSERT INTO nostro_accounts (currency) VALUES ('EUR')`); err != nil {
		t.Fatal(err)
	}
	manager, fundA, fundB := int64(1), int64(2), int64(3)
	t1 := pgTrade(t, ctx, pool, instrID, manager, 99, "7", "1.10", "COMPLETED")
	t2 := pgTrade(t, ctx, pool, instrID, manager, 99, "3", "1.30", "COMPLETED")

	rec := &recordingLedger{}
	st, err := NewPgAllocStore(pool, rec)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(EngineDeps{Store: st})
	if err != nil {
		t.Fatal(err)
	}

	// Register group + eligibility (client capacity).
	g, err := eng.CreateGroup(ctx, manager, CreateGroupInput{
		GroupRef: "PG-G1", ManagerAccountID: manager, InstrumentID: instrID,
		Side: '1', Capacity: CapacityClient, Method: MethodProRata,
		Eligible: []EligibleAccount{
			{AccountID: fundA, Capacity: CapacityClient},
			{AccountID: fundB, Capacity: CapacityClient},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Capacity-mix trigger fires at the DB layer (account 4 exists, so the
	// refusal is the trigger's, not the FK's).
	if _, err := pool.Exec(ctx, `
		INSERT INTO average_price_group_accounts
		 (group_id, beneficiary_account_id, capacity)
		VALUES ($1, 4, 'PROPRIETARY')`, g.ID); err == nil {
		t.Fatal("capacity-mix trigger must reject PROPRIETARY on a CLIENT group")
	}

	if _, err := eng.AttachFills(ctx, g.ID, []int64{t1, t2}); err != nil {
		t.Fatal(err)
	}
	d, err := eng.Detail(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Group.TotalQty.String() != "10" || d.Group.AvgPrice.String() != "1.16" {
		t.Fatalf("vwap: %+v", d.Group)
	}

	// Fill reuse across groups is refused (UNIQUE trade_id).
	g2, err := eng.CreateGroup(ctx, manager, CreateGroupInput{
		GroupRef: "PG-G2", ManagerAccountID: manager, InstrumentID: instrID,
		Side: '1', Capacity: CapacityClient, Method: MethodManual,
		Eligible: []EligibleAccount{{AccountID: fundA, Capacity: CapacityClient}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.AttachFills(ctx, g2.ID, []int64{t1}); err == nil {
		t.Fatal("fill must not join a second group")
	}

	// PRO_RATA 1:4 → 2 / 8.
	rows, err := eng.Allocate(ctx, g.ID, []LegRequest{
		{AccountID: fundA, Weight: decimal.RequireFromString("1")},
		{AccountID: fundB, Weight: decimal.RequireFromString("4")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sum decimal.Decimal
	byFund := map[int64]decimal.Decimal{}
	for _, r := range rows {
		sum = sum.Add(r.Quantity)
		byFund[r.BeneficiaryAccountID] = byFund[r.BeneficiaryAccountID].Add(r.Quantity)
	}
	if !sum.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("conservation: %s", sum)
	}
	if byFund[fundA].String() != "2" || byFund[fundB].String() != "8" {
		t.Fatalf("pro-rata: %v", byFund)
	}
	// Park journal posted (allocated == filled → full park) and balanced.
	if len(rec.journals) != 1 {
		t.Fatalf("park journal: %d", len(rec.journals))
	}

	// Conservation trigger refuses an over-allocated write directly.
	if _, err := pool.Exec(ctx, `
		INSERT INTO trade_allocations
		 (trade_id, group_id, beneficiary_account_id, quantity, avg_price,
		  status, kind)
		VALUES ($1,$2,$3,'99','1.16','ALLOCATED','PRIMARY')`, t1, g.ID, fundA); err == nil {
		t.Fatal("conservation trigger must refuse active > fill qty")
	}

	// Append-only audit.
	if _, err := pool.Exec(ctx, `
		UPDATE trade_allocation_events SET after_state='{}' WHERE group_id=$1`,
		g.ID); err == nil {
		t.Fatal("audit UPDATE must be refused (append-only)")
	}

	// Fund-ops confirm: child PAY+RECEIVE legs + 2090 rebook.
	var legID int64
	for _, r := range rows {
		if r.BeneficiaryAccountID == fundA {
			legID = r.ID
		}
	}
	cf, err := eng.ConfirmFund(ctx, legID, "fundops:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.SettlementInstructionIDs) != 2 || cf.ConfirmationRef == "" {
		t.Fatalf("confirm: %+v", cf)
	}
	var nostro int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(nostro_account_id,0) FROM settlement_instructions
		 WHERE id=$1`, cf.SettlementInstructionIDs[0]).Scan(&nostro); err != nil {
		t.Fatal(err)
	}
	if nostro != 1 {
		t.Fatalf("receive leg nostro: %d", nostro)
	}
	if len(rec.journals) != 2 { // park + leg rebook
		t.Fatalf("rebook journal missing: %d", len(rec.journals))
	}

	// Settlement lock → post-lock mutation refused; dual-control
	// correction succeeds with a distinct role-eligible approver.
	roles := func(_ context.Context, id int64) (string, error) {
		if id == 88 {
			return "Finance Ops", nil
		}
		return "Support Agent", nil
	}
	svc, err := NewAllocationService(AllocationServiceDeps{Store: st, Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitToSettlement(ctx, g.ID, "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RejectFund(ctx, legID, "x", "late"); err == nil {
		t.Fatal("post-lock reject must be refused")
	}
	var fundBAlloc int64
	for _, r := range rows {
		if r.BeneficiaryAccountID == fundB {
			fundBAlloc = r.ID
		}
	}
	// Per-fill conservation (trigger + engine): fundB's leg sits on the
	// 3-qty fill, so a valid correction shrinks within headroom — growth
	// beyond the source fill is refused upstream as over-allocation.
	corr, err := svc.Correct(ctx, Actor{UserID: 42, ApproverID: 88}, fundBAlloc,
		CorrectInput{Quantity: decimal.RequireFromString("2"), Reason: "resize"})
	if err != nil {
		t.Fatal(err)
	}
	if corr.Replacement.Quantity.String() != "2" ||
		*corr.Replacement.CorrectsAllocationID != fundBAlloc {
		t.Fatalf("correction: %+v", corr)
	}
	// The offset/replacement pair + approver are in the immutable audit.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM trade_allocation_events
		 WHERE group_id=$1 AND event_type='CORRECTED' AND approved_by=88
		   AND before_state IS NOT NULL`, g.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("correction audit: %d %v", n, err)
	}

	// T+0 sweep: the group still carries unallocated remainder —
	// active = 7 (fundA CLAIMED) + 2 (fundB REPLACEMENT) = 9 of 10.
	res, err := eng.EscalateUnallocated(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated != 1 {
		t.Fatalf("sweep: %+v", res)
	}
	var code string
	if err := pool.QueryRow(ctx, `
		SELECT code FROM funding_ops_alerts
		 WHERE code='UNALLOCATED_BLOCK_TRADE' ORDER BY id DESC LIMIT 1`).Scan(&code); err != nil {
		t.Fatalf("durable P2 alert missing: %v", err)
	}
	// Idempotent sweep.
	res, err = eng.EscalateUnallocated(ctx, time.Now().UTC().Add(2*time.Hour))
	if err != nil || res.Escalated != 0 {
		t.Fatalf("repeat sweep: %+v %v", res, err)
	}

	// Down-migration sanity isn't exercised here (objects verified
	// structurally above); the reverse DDL is smoke-tested by apply+drop
	// in migration CI.
}
