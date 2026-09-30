// gross_net_test.go — unit + PostgreSQL integration coverage for the
// per-instrument GROSS/NET settlement mode (Phase-19 Task 19.3.6; spec
// §5.1 settlement_mode, §17.1, §24 #98).
//
// Unit tests drive the service through in-memory GrossNetStore fakes —
// grouping math, mode filtering, claim-before-send, zero-net settle,
// fail-closed unknown mode. Integration tests (EXC_PG_TEST=1) verify the
// migration-031 column, per-type defaults, and the real store SQL against
// dev PostgreSQL.
package settlement

import (
	"context"
	stderrors "errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// In-memory GrossNetStore fake.
// ---------------------------------------------------------------------------

type memGrossNetStore struct {
	legs    []NettableLeg
	modes   map[int64]SettlementMode
	claims  []gnClaim
	modeErr error
	legsErr error
	txErr   error
	// failClaimAfter: when >0, ClaimLegs short-claims to force the
	// constituent-moved abort path.
	failClaimAfter int
}

type gnClaim struct {
	ids     []int64
	ref     string
	payload string
	settle  bool
}

func (m *memGrossNetStore) SettlementModeFor(_ context.Context, id int64) (SettlementMode, error) {
	if m.modeErr != nil {
		return "", m.modeErr
	}
	md, ok := m.modes[id]
	if !ok {
		return "", excerrors.New(CodeSettlementNotFound, "no instrument")
	}
	return md, nil
}

func (m *memGrossNetStore) DueLegsEnriched(_ context.Context, _ time.Time) ([]NettableLeg, error) {
	if m.legsErr != nil {
		return nil, m.legsErr
	}
	return m.legs, nil
}

func (m *memGrossNetStore) InTx(ctx context.Context, fn func(context.Context, GrossNetTx) error) error {
	if m.txErr != nil {
		return m.txErr
	}
	return fn(ctx, &memGrossNetTx{m: m})
}

type memGrossNetTx struct{ m *memGrossNetStore }

func (t *memGrossNetTx) ClaimLegs(_ context.Context, ids []int64, ref string,
	format MessageFormat, payload string, _ time.Time, settle bool) (int, error) {
	n := len(ids)
	if t.m.failClaimAfter > 0 && n > t.m.failClaimAfter {
		n = t.m.failClaimAfter
	}
	t.m.claims = append(t.m.claims, gnClaim{ids: ids, ref: ref, payload: payload, settle: settle})
	return n, nil
}

func netLeg(id, tradeID, acct, cp int64, instr int64, ccy string, amt string,
	dir SettlementDirection, day time.Time, nostro NostroAccount) NettableLeg {
	return NettableLeg{
		Instruction: SettlementInstruction{
			ID: id, TradeID: tradeID, AccountID: acct, Currency: ccy,
			Amount: decimal.RequireFromString(amt), Direction: dir,
			SettlementDate: day, Status: SettlePending,
		},
		InstrumentID:   instr,
		Symbol:         "EUR/USD",
		Mode:           SettlementNet,
		CounterpartyID: cp,
		Nostro:         nostro,
	}
}

var testNostro = NostroAccount{
	ID: 9, Currency: "USD", BankName: "Correspondent",
	BankCode: "CITIUS33", AccountNumber: "111-222-333",
}

func newGrossNetSvc(t *testing.T, st *memGrossNetStore, disp *NullDispatcher) *GrossNetService {
	t.Helper()
	if disp == nil {
		disp = &NullDispatcher{}
	}
	s, err := NewGrossNetService(st, GrossNetOptions{
		SenderBIC:  "EXCHUS33",
		Dispatcher: disp,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Grouping math
// ---------------------------------------------------------------------------

func TestGroupNetLegsAggregatesPerPairCurrencyDay(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	legs := []NettableLeg{
		// acct 10 owes cp 20: PAY 100 USD (leg 1), PAY 30 USD (leg 3)
		netLeg(1, 100, 10, 20, 55, "USD", "100", DirectionPay, day, testNostro),
		netLeg(3, 102, 10, 20, 55, "USD", "30", DirectionPay, day, testNostro),
		// and is owed: RECEIVE 60 USD (leg 2) → net PAY 70
		netLeg(2, 101, 10, 20, 55, "USD", "60", DirectionReceive, day, testNostro),
		// different counterparty → separate batch
		netLeg(4, 103, 10, 30, 55, "USD", "40", DirectionPay, day, testNostro),
		// different currency → separate batch
		netLeg(5, 104, 10, 20, 55, "EUR", "50", DirectionPay, day, testNostro),
		// different settlement date → separate batch
		netLeg(6, 105, 10, 20, 55, "USD", "10", DirectionPay, day.Add(24*time.Hour), testNostro),
		// GROSS-mode leg is ignored by the grouper
		{Instruction: SettlementInstruction{ID: 99, AccountID: 10, Currency: "USD",
			Amount: decimal.RequireFromString("5"), Direction: DirectionPay, SettlementDate: day},
			Mode: SettlementGross, CounterpartyID: 20},
	}
	batches := GroupNetLegs(legs)
	if len(batches) != 4 {
		t.Fatalf("batches=%d want 4: %+v", len(batches), batches)
	}
	// Locate the (acct 10, cp 20, USD, day) batch — output is sorted by
	// (date, account, counterparty, currency).
	var b NetBatch
	for _, cand := range batches {
		if cand.AccountID == 10 && cand.CounterpartyID == 20 &&
			cand.Currency == "USD" && cand.SettlementDate.Equal(day) {
			b = cand
		}
	}
	if b.Currency != "USD" {
		t.Fatal("USD batch not found")
	}
	if b.AccountID != 10 || b.CounterpartyID != 20 || b.Currency != "USD" || !b.SettlementDate.Equal(day) {
		t.Fatalf("batch dims wrong: %+v", b)
	}
	if !b.GrossPay.Equal(d("130")) || !b.GrossReceive.Equal(d("60")) {
		t.Fatalf("gross pay/recv %s/%s", b.GrossPay, b.GrossReceive)
	}
	if !b.NetAmount.Equal(d("-70")) || b.Direction != DirectionPay {
		t.Fatalf("net %s dir %s want -70 PAY", b.NetAmount, b.Direction)
	}
	wantIDs := []int64{1, 2, 3}
	if len(b.InstructionIDs) != 3 {
		t.Fatalf("instr ids %v", b.InstructionIDs)
	}
	for i, id := range wantIDs {
		if b.InstructionIDs[i] != id {
			t.Fatalf("instr ids %v", b.InstructionIDs)
		}
	}
	if b.Ref() != swiftRef("NB", 1) {
		t.Fatalf("ref %q", b.Ref())
	}
}

func TestGroupNetLegsFullyOffset(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	legs := []NettableLeg{
		netLeg(1, 100, 10, 20, 55, "USD", "100", DirectionPay, day, testNostro),
		netLeg(2, 101, 10, 20, 55, "USD", "100", DirectionReceive, day, testNostro),
	}
	b := GroupNetLegs(legs)[0]
	if !b.NetAmount.IsZero() || b.Direction != "" {
		t.Fatalf("net=%s dir=%q want zero/empty", b.NetAmount, b.Direction)
	}
}

// ---------------------------------------------------------------------------
// Service seams
// ---------------------------------------------------------------------------

func TestSettlementModeForPassesThrough(t *testing.T) {
	st := &memGrossNetStore{modes: map[int64]SettlementMode{55: SettlementNet}}
	s := newGrossNetSvc(t, st, nil)
	m, err := s.SettlementModeFor(context.Background(), 55)
	if err != nil || m != SettlementNet {
		t.Fatalf("mode=%s err=%v", m, err)
	}
	if _, err := s.SettlementModeFor(context.Background(), 99); err == nil {
		t.Fatal("missing instrument must fail closed")
	}
}

func TestNetSettlementBatchesFiltersGross(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	st := &memGrossNetStore{legs: []NettableLeg{
		netLeg(1, 100, 10, 20, 55, "USD", "100", DirectionPay, day, testNostro),
		netLeg(2, 101, 10, 20, 55, "USD", "40", DirectionReceive, day, testNostro),
		{Instruction: SettlementInstruction{ID: 3, AccountID: 10, Currency: "USD",
			Amount: decimal.RequireFromString("9"), Direction: DirectionPay, SettlementDate: day},
			Mode: SettlementGross, CounterpartyID: 20},
	}}
	s := newGrossNetSvc(t, st, nil)
	b, err := s.NetSettlementBatches(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	// Pay 100, receive 40 → net PAY 60 (NetAmount −60, account pays).
	if len(b) != 1 || !b[0].NetAmount.Equal(d("-60")) || b[0].Direction != DirectionPay {
		t.Fatalf("batches=%+v", b)
	}
}

// ---------------------------------------------------------------------------
// Mode-aware dispatch
// ---------------------------------------------------------------------------

func TestDispatchDueSplitsModes(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	st := &memGrossNetStore{legs: []NettableLeg{
		// GROSS leg — dispatches individually.
		{Instruction: SettlementInstruction{ID: 1, TradeID: 50, AccountID: 10,
			Currency: "USD", Amount: decimal.RequireFromString("500"),
			Direction: DirectionPay, SettlementDate: day, Status: SettlePending},
			Mode: SettlementGross, CounterpartyID: 20, Nostro: testNostro},
		// NET legs — acct 10 pays cp 20 100 and receives 40 → one PAY 60 payment.
		netLeg(2, 51, 10, 20, 55, "USD", "100", DirectionPay, day, testNostro),
		netLeg(3, 52, 10, 20, 55, "USD", "40", DirectionReceive, day, testNostro),
	}}
	disp := &NullDispatcher{}
	s := newGrossNetSvc(t, st, disp)

	rep, err := s.DispatchDue(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Due != 3 || rep.GrossClaimed != 1 || rep.Batches != 1 ||
		rep.NetClaimed != 2 || rep.NetDispatched != 1 {
		t.Fatalf("report %+v", rep)
	}
	if len(rep.Errors) != 0 || len(rep.BatchErrors) != 0 {
		t.Fatalf("errors: %+v %+v", rep.Errors, rep.BatchErrors)
	}
	if len(disp.Sent) != 2 {
		t.Fatalf("sent %d want 2 (1 gross leg + 1 net batch)", len(disp.Sent))
	}
	// The gross message carries the leg ref; the net message carries NB.
	var grossMsg, netMsg *OutboundMessage
	for i := range disp.Sent {
		switch {
		case disp.Sent[i].MessageID == swiftRef("SI", 1):
			grossMsg = &disp.Sent[i]
		case disp.Sent[i].MessageID == swiftRef("NB", 2):
			netMsg = &disp.Sent[i]
		}
	}
	if grossMsg == nil || netMsg == nil {
		t.Fatalf("message ids: %+v", disp.Sent)
	}
	if !grossMsg.Amount.Equal(d("500")) {
		t.Fatalf("gross amount %s", grossMsg.Amount)
	}
	if !netMsg.Amount.Equal(d("60")) || netMsg.Currency != "USD" {
		t.Fatalf("net amount %s %s", netMsg.Amount, netMsg.Currency)
	}
}

func TestDispatchDueZeroNetSettlesInPlace(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	st := &memGrossNetStore{legs: []NettableLeg{
		netLeg(1, 51, 10, 20, 55, "USD", "100", DirectionPay, day, testNostro),
		netLeg(2, 52, 10, 20, 55, "USD", "100", DirectionReceive, day, testNostro),
	}}
	disp := &NullDispatcher{}
	s := newGrossNetSvc(t, st, disp)
	rep, err := s.DispatchDue(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Netted != 2 || rep.NetDispatched != 0 || len(disp.Sent) != 0 {
		t.Fatalf("rep %+v sent %d", rep, len(disp.Sent))
	}
	if len(st.claims) != 1 || !st.claims[0].settle || st.claims[0].payload != "NETTED:FULLY_OFFSET" {
		t.Fatalf("claims %+v", st.claims)
	}
}

func TestDispatchDueUnknownModeFailsClosed(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	st := &memGrossNetStore{legs: []NettableLeg{
		{Instruction: SettlementInstruction{ID: 1, AccountID: 10, Currency: "USD",
			Amount: decimal.RequireFromString("10"), Direction: DirectionPay,
			SettlementDate: day, Status: SettlePending},
			Mode: SettlementMode("BOGUS"), CounterpartyID: 20},
	}}
	s := newGrossNetSvc(t, st, nil)
	rep, err := s.DispatchDue(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Errors) != 1 {
		t.Fatalf("errors %+v", rep.Errors)
	}
	var e *excerrors.Error
	if !stderrors.As(rep.Errors[0].Err, &e) || e.Code != CodeSettlementModeUnknown {
		t.Fatalf("err %v want %s", rep.Errors[0].Err, CodeSettlementModeUnknown)
	}
}

func TestDispatchDueBatchClaimShortfallAborts(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	st := &memGrossNetStore{
		failClaimAfter: 1, // only 1 of the 2 legs claims — mid-claim move
		legs: []NettableLeg{
			netLeg(1, 51, 10, 20, 55, "USD", "100", DirectionPay, day, testNostro),
			netLeg(2, 52, 10, 20, 55, "USD", "40", DirectionPay, day, testNostro),
		},
	}
	disp := &NullDispatcher{}
	s := newGrossNetSvc(t, st, disp)
	rep, err := s.DispatchDue(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.BatchErrors) != 1 {
		t.Fatalf("batch errors %+v", rep.BatchErrors)
	}
	if rep.NetDispatched != 0 || len(disp.Sent) != 0 {
		t.Fatalf("aborted batch dispatched: rep %+v sent %d", rep, len(disp.Sent))
	}
	var e *excerrors.Error
	if !stderrors.As(rep.BatchErrors[0].Err, &e) || e.Code != CodeSettlementStateConflict {
		t.Fatalf("err %v want %s", rep.BatchErrors[0].Err, CodeSettlementStateConflict)
	}
}

func TestNewGrossNetServiceRejectsBadConfig(t *testing.T) {
	if _, err := NewGrossNetService(nil, GrossNetOptions{SenderBIC: "EXCHUS33"}); err == nil {
		t.Fatal("nil store must fail")
	}
	st := &memGrossNetStore{}
	if _, err := NewGrossNetService(st, GrossNetOptions{SenderBIC: "bad"}); err == nil {
		t.Fatal("bad BIC must fail")
	}
	if _, err := NewGrossNetService(st, GrossNetOptions{SenderBIC: "EXCHUS33", Format: "MT103"}); err == nil {
		t.Fatal("unknown format must fail")
	}
}

// ---------------------------------------------------------------------------
// PostgreSQL integration (EXC_PG_TEST=1) — migration 031 column + store SQL.
// ---------------------------------------------------------------------------

func grossNetPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestIntegrationSettlementModeColumn(t *testing.T) {
	pool := grossNetPool(t)
	ctx := context.Background()

	// Migration 031 must be applied: column + enum + per-type defaults.
	var enumOK bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_type WHERE typname='settlement_mode_enum')`).
		Scan(&enumOK); err != nil || !enumOK {
		t.Fatalf("settlement_mode_enum missing: %v", err)
	}

	// Spot rows defaulted GROSS; FORWARD/SWAP/NDF would be NET.
	var spotGross int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM instruments
		 WHERE instrument_type='SPOT' AND settlement_mode='GROSS'`).Scan(&spotGross); err != nil {
		t.Fatal(err)
	}
	if spotGross == 0 {
		t.Fatal("no SPOT instruments carry GROSS default")
	}

	// PgxGrossNetStore.SettlementModeFor resolves a real row.
	st := NewPgxGrossNetStore(pool)
	var id int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE instrument_type='SPOT' LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	mode, err := st.SettlementModeFor(ctx, id)
	if err != nil || mode != SettlementGross {
		t.Fatalf("mode=%s err=%v", mode, err)
	}
	if _, err := st.SettlementModeFor(ctx, -1); err == nil {
		t.Fatal("missing instrument must fail closed")
	}
}

func TestIntegrationClaimLegsAtomic(t *testing.T) {
	pool := grossNetPool(t)
	ctx := context.Background()
	st := NewPgxGrossNetStore(pool)

	// Fixture: one trade + two PENDING instructions, both unclaimed.
	var acct, instrID, tradeID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM accounts LIMIT 1`).Scan(&acct); err != nil {
		t.Skipf("accounts empty: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM instruments LIMIT 1`).Scan(&instrID); err != nil {
		t.Skipf("instruments empty: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
		                    buyer_account_id, seller_account_id, price, quantity)
		VALUES ($1, 0, 0, $2, $2, 1.1, 1000) RETURNING id`,
		instrID, acct).Scan(&tradeID); err != nil {
		t.Skipf("trade fixture: %v", err)
	}
	// leg_ux is (trade_id, account_id, currency, direction) — vary currency.
	ccys := []string{"USD", "EUR"}
	var ids [2]int64
	for i := range ids {
		err := pool.QueryRow(ctx, `
			INSERT INTO settlement_instructions
			    (trade_id, account_id, currency, amount, direction,
			     settlement_date, nostro_account_id, status)
			VALUES ($2, $3, $4, $1, 'PAY', CURRENT_DATE, NULL, 'PENDING')
			RETURNING id`, 100+i, tradeID, acct, ccys[i]).Scan(&ids[i])
		if err != nil {
			t.Skipf("cannot build instruction fixture: %v", err)
		}
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM settlement_instructions WHERE id = ANY($1)`, ids[:])
		pool.Exec(ctx, `DELETE FROM trades WHERE id = $1`, tradeID)
	})

	err := st.InTx(ctx, func(ctx context.Context, tx GrossNetTx) error {
		n, err := tx.ClaimLegs(ctx, ids[:], "NBTEST1", FormatMT202, "payload", time.Now().UTC(), false)
		if err != nil || n != 2 {
			t.Fatalf("claimed=%d err=%v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Claimed rows are invisible to a second claim (idempotent).
	err = st.InTx(ctx, func(ctx context.Context, tx GrossNetTx) error {
		n, err := tx.ClaimLegs(ctx, ids[:], "NBTEST2", FormatMT202, "p2", time.Now().UTC(), false)
		if err != nil {
			return err
		}
		if n != 0 {
			t.Fatalf("re-claim got %d rows", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var msgID string
	if err := pool.QueryRow(ctx,
		`SELECT swift_message_id FROM settlement_instructions WHERE id=$1`, ids[0]).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	if msgID != "NBTEST1" {
		t.Fatalf("msg id %q", msgID)
	}
}
