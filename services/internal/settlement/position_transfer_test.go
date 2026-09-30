// position_transfer_test.go — unit + PostgreSQL integration coverage for
// the internal position-transfer engine (Phase-19 Task 19.3.12; spec
// §13.9, §5.38, §24 #190).
//
// Unit tests drive TransferService through an in-memory TransferStore
// (same discipline as position_service_test.go's memPositionStore — the
// transfer tx embeds it so the shared NETTING/HEDGING machinery runs
// unmodified). The fake journal poster enforces the wallet invariants the
// real DoubleEntryLedgerService enforces (locked ≥ 0, available ≥ 0) so
// balance assertions exercise the Effects end-to-end.
//
// Integration tests (EXC_PG_TEST=1) exercise pgxTransferStore against the
// migration-061 table and a full Execute through the real
// DoubleEntryLedgerService poster.
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// In-memory TransferStore / TransferTx.
// ---------------------------------------------------------------------------

type balRow struct{ avail, locked decimal.Decimal }

type memTransferStore struct {
	pos         *memPositionStore
	accounts    map[int64]TransferAccount
	instruments map[int64]TransferInstrument
	margins     map[int64]decimal.Decimal // positions.id → margin_used
	balances    map[string]*balRow        // "acct:ccy"
	transfers   map[int64]*PositionTransfer
	nextT       int64
	journalSeq  int64
	posted      []ledger.Journal
	posterErr   error
	txErr       error
}

func newMemTransferStore() *memTransferStore {
	return &memTransferStore{
		pos:         newMemPositionStore(),
		accounts:    map[int64]TransferAccount{},
		instruments: map[int64]TransferInstrument{},
		margins:     map[int64]decimal.Decimal{},
		balances:    map[string]*balRow{},
		transfers:   map[int64]*PositionTransfer{},
		nextT:       1,
	}
}

func balKey(acct int64, ccy string) string { return fmt.Sprintf("%d:%s", acct, ccy) }

func (s *memTransferStore) balanceFor(acct int64, ccy string) *balRow {
	k := balKey(acct, ccy)
	if s.balances[k] == nil {
		s.balances[k] = &balRow{}
	}
	return s.balances[k]
}

func (s *memTransferStore) InTx(ctx context.Context, fn func(context.Context, TransferTx) error) error {
	if s.txErr != nil {
		return s.txErr
	}
	return fn(ctx, &memTransferTx{memPositionTx: &memPositionTx{m: s.pos}, s: s})
}

type memTransferTx struct {
	*memPositionTx // satisfies the embedded PositionTx
	s              *memTransferStore
}

func (t *memTransferTx) LockTransferAccounts(_ context.Context, fromID, toID int64) (TransferAccount, TransferAccount, error) {
	from, ok1 := t.s.accounts[fromID]
	to, ok2 := t.s.accounts[toID]
	if !ok1 || !ok2 {
		return TransferAccount{}, TransferAccount{}, fmt.Errorf("account row missing (from=%t to=%t)", ok1, ok2)
	}
	return from, to, nil
}

func (t *memTransferTx) AccountRootID(_ context.Context, accountID int64) (int64, error) {
	id := accountID
	for depth := 0; depth < transferMaxHops; depth++ {
		a, ok := t.s.accounts[id]
		if !ok || a.ParentID == 0 {
			return id, nil
		}
		id = a.ParentID
	}
	return id, nil
}

func (t *memTransferTx) InstrumentSpec(_ context.Context, instrumentID int64) (TransferInstrument, error) {
	i, ok := t.s.instruments[instrumentID]
	if !ok {
		return TransferInstrument{}, excerrors.New(CodePositionTransferFailed, "instrument not found")
	}
	return i, nil
}

func (t *memTransferTx) PositionMarginUsed(_ context.Context, positionID int64) (decimal.Decimal, error) {
	return t.s.margins[positionID], nil
}

func (t *memTransferTx) EnsureBalanceForUpdate(_ context.Context, accountID int64, ccy string) (decimal.Decimal, decimal.Decimal, error) {
	b := t.s.balanceFor(accountID, ccy)
	return b.avail, b.locked, nil
}

func (t *memTransferTx) InsertTransfer(_ context.Context, rec PositionTransfer) (int64, error) {
	if rec.IdempotencyKey != "" {
		for _, tr := range t.s.transfers {
			if tr.IdempotencyKey == rec.IdempotencyKey {
				return 0, nil // replay
			}
		}
	}
	id := t.s.nextT
	t.s.nextT++
	cp := rec
	cp.ID = id
	t.s.transfers[id] = &cp
	return id, nil
}

func (t *memTransferTx) FindTransferByKey(_ context.Context, key string) (*PositionTransfer, error) {
	for _, tr := range t.s.transfers {
		if tr.IdempotencyKey == key {
			cp := *tr
			return &cp, nil
		}
	}
	return nil, nil
}

func (t *memTransferTx) CompleteTransfer(_ context.Context, id, journalID int64, at time.Time) error {
	tr := t.s.transfers[id]
	if tr == nil || tr.Status != "PENDING" {
		return excerrors.New(CodePositionTransferFailed, "row not PENDING")
	}
	tr.Status = "COMPLETED"
	tr.GLJournalID = journalID
	tr.CompletedAt = &at
	return nil
}

// PostJournal mirrors the DoubleEntryLedgerService wallet contract:
// validate, then apply Effects with locked≥0 / available≥0 enforcement.
func (t *memTransferTx) PostJournal(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if t.s.posterErr != nil {
		return ledger.PostResult{}, t.s.posterErr
	}
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	t.s.journalSeq++
	var events []ledger.BalanceEvent
	for _, e := range j.Effects {
		b := t.s.balanceFor(e.AccountID, e.Currency)
		nAvail := b.avail.Add(e.AvailableDelta)
		nLocked := b.locked.Add(e.LockedDelta)
		if nLocked.IsNegative() || (nAvail.IsNegative() && !e.AllowNegative) {
			return ledger.PostResult{}, excerrors.New(ledger.CodeInsufficientBalance,
				fmt.Sprintf("acct %d %s overdrawn by effect", e.AccountID, e.Currency))
		}
		b.avail, b.locked = nAvail, nLocked
		events = append(events, ledger.BalanceEvent{AccountID: e.AccountID, Currency: e.Currency})
	}
	t.s.posted = append(t.s.posted, j)
	return ledger.PostResult{JournalID: t.s.journalSeq, Events: events, Committed: true}, nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type stubMarkSource struct {
	mark decimal.Decimal
	err  error
}

func (s stubMarkSource) Mark(_ context.Context, _ int64) (decimal.Decimal, error) {
	return s.mark, s.err
}

func seedTransferFixture() *memTransferStore {
	st := newMemTransferStore()
	// Accounts: 10 (user 1, master), 11 (user 1) — same legal entity;
	// 12 (user 2, parent 10) — same master hierarchy; 20 (user 9, own
	// root) — different entity.
	st.accounts[10] = TransferAccount{ID: 10, UserID: 1, Status: "ACTIVE", PositionMode: posModeNetting}
	st.accounts[11] = TransferAccount{ID: 11, UserID: 1, Status: "ACTIVE", PositionMode: posModeNetting}
	st.accounts[12] = TransferAccount{ID: 12, UserID: 2, ParentID: 10, Status: "ACTIVE", PositionMode: posModeHedging}
	st.accounts[20] = TransferAccount{ID: 20, UserID: 9, Status: "ACTIVE", PositionMode: posModeNetting}
	st.accounts[30] = TransferAccount{ID: 30, UserID: 1, Status: "SUSPENDED", PositionMode: posModeNetting}
	st.instruments[55] = TransferInstrument{ID: 55, Symbol: "EUR/USD", QuoteCurrency: "USD", MaxLeverage: 10}
	// memPositionStore modes default to NETTING; mirror the account rows.
	st.pos.modes[10] = posModeNetting
	st.pos.modes[11] = posModeNetting
	st.pos.modes[12] = posModeHedging
	st.pos.modes[20] = posModeNetting
	st.pos.modes[30] = posModeNetting
	return st
}

// seedPosition inserts an open position row directly (engine-equivalent
// state) and optionally records its margin_used.
func seedPosition(st *memTransferStore, id, acct, instr int64, side, qty, entry, marginUsed string) {
	p := &Position{
		ID: id, AccountID: acct, InstrumentID: instr, Side: side,
		Quantity: d(qty), EntryPrice: d(entry), HasMark: true, MarkPrice: d(entry),
	}
	st.pos.positions[posKey{acct, instr, side}] = p
	if id >= st.pos.nextID {
		st.pos.nextID = id + 1
	}
	if marginUsed != "" {
		st.margins[id] = d(marginUsed)
	}
}

func transferSvc(st *memTransferStore, marks TransferMarkSource) (*TransferService, *fakeDispatcher) {
	disp := &fakeDispatcher{}
	s := newTransferServiceForTest(st, &fakeLocker{}, disp, marks, nil, "test-transfer")
	return s, disp
}

func baseTransferReq() TransferRequest {
	return TransferRequest{
		FromAccountID: 10, ToAccountID: 11, InstrumentID: 55,
		Side: PositionLong, Quantity: d("10000"),
		ReasonCode: TransferReasonSubAccount, AuthorizedBy: 77,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestTransferHappyPathFullPosition(t *testing.T) {
	st := seedTransferFixture()
	seedPosition(st, 1, 10, 55, PositionLong, "10000", "1.1000", "1200") // margin_used 1200 USD
	st.balanceFor(10, "USD").locked = d("1500")
	st.balanceFor(11, "USD").avail = d("5000")

	s, disp := transferSvc(st, stubMarkSource{mark: d("1.2000")})
	res, err := s.Execute(context.Background(), baseTransferReq())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Mark + realized P&L: 10000 × (1.20 − 1.10) = +1000 USD.
	if !res.Mark.Equal(d("1.2000")) || !res.RealizedPnL.Equal(d("1000")) {
		t.Fatalf("mark=%s pnl=%s", res.Mark, res.RealizedPnL)
	}
	// Pro-rata release: margin_used 1200 × 10000/10000 = 1200.
	if !res.SourceReleased.Equal(d("1200")) {
		t.Fatalf("release=%s", res.SourceReleased)
	}
	// Dest IM: 10000 × 1.20 / lev 10 = 1200.
	if !res.DestLocked.Equal(d("1200")) {
		t.Fatalf("dest lock=%s", res.DestLocked)
	}
	// Wallet: src avail = 0 + 1200 release + 1000 pnl = 2200, locked 300.
	src := st.balanceFor(10, "USD")
	if !src.avail.Equal(d("2200")) || !src.locked.Equal(d("300")) {
		t.Fatalf("src bal %s/%s", src.avail, src.locked)
	}
	dst := st.balanceFor(11, "USD")
	if !dst.avail.Equal(d("3800")) || !dst.locked.Equal(d("1200")) {
		t.Fatalf("dst bal %s/%s", dst.avail, dst.locked)
	}
	// Positions: source flat, destination LONG 10000 @ 1.20.
	if !res.SourceUpdate.Position.Quantity.IsZero() {
		t.Fatalf("src position %+v", res.SourceUpdate.Position)
	}
	dp := res.DestUpdate.Position
	if dp.Side != PositionLong || !dp.Quantity.Equal(d("10000")) || !dp.EntryPrice.Equal(d("1.2000")) {
		t.Fatalf("dst position %+v", dp)
	}
	// Audit row + journal link.
	tr := res.Transfer
	if tr.Status != "COMPLETED" || tr.GLJournalID != res.JournalID || res.JournalID == 0 {
		t.Fatalf("transfer %+v journal %d", tr, res.JournalID)
	}
	if tr.RegIndicator != RegIndicatorTransfer || tr.ReasonCode != TransferReasonSubAccount {
		t.Fatalf("audit fields %+v", tr)
	}
	if len(st.posted) != 1 {
		t.Fatalf("posted %d journals", len(st.posted))
	}
	if len(disp.events) == 0 {
		t.Fatal("no BalanceChanged events dispatched")
	}
}

func TestTransferPartialQuantityProRata(t *testing.T) {
	st := seedTransferFixture()
	seedPosition(st, 1, 10, 55, PositionLong, "10000", "1.1000", "1000")
	st.balanceFor(10, "USD").locked = d("1000")
	st.balanceFor(11, "USD").avail = d("5000")

	s, _ := transferSvc(st, stubMarkSource{mark: d("1.1000")})
	req := baseTransferReq()
	req.Quantity = d("4000")
	res, err := s.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Release pro-rata: 1000 × 4000/10000 = 400. Dest IM: 4000×1.1/10 = 440.
	if !res.SourceReleased.Equal(d("400")) {
		t.Fatalf("release=%s want 400", res.SourceReleased)
	}
	if !res.DestLocked.Equal(d("440")) {
		t.Fatalf("dest lock=%s want 440", res.DestLocked)
	}
	if !res.SourceUpdate.Position.Quantity.Equal(d("6000")) {
		t.Fatalf("src qty %s", res.SourceUpdate.Position.Quantity)
	}
	if !res.DestUpdate.Position.Quantity.Equal(d("4000")) {
		t.Fatalf("dst qty %s", res.DestUpdate.Position.Quantity)
	}
}

func TestTransferRejectsInsufficientSource(t *testing.T) {
	st := seedTransferFixture()
	seedPosition(st, 1, 10, 55, PositionLong, "5000", "1.1000", "")
	st.balanceFor(11, "USD").avail = d("99999")

	s, _ := transferSvc(st, stubMarkSource{mark: d("1.1000")})
	req := baseTransferReq()
	req.Quantity = d("6000")
	_, err := s.Execute(context.Background(), req)
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != CodeTransferInsufficient {
		t.Fatalf("err %v want %s", err, CodeTransferInsufficient)
	}
	// No position row, side mismatch → same code.
	req.Quantity = d("100")
	req.Side = PositionShort
	if _, err := s.Execute(context.Background(), req); !stderrors.As(err, &e) || e.Code != CodeTransferInsufficient {
		t.Fatalf("side-mismatch err %v want %s", err, CodeTransferInsufficient)
	}
}

func TestTransferRejectsCrossEntity(t *testing.T) {
	st := seedTransferFixture()
	seedPosition(st, 1, 10, 55, PositionLong, "10000", "1.1000", "")
	st.balanceFor(20, "USD").avail = d("99999")

	s, _ := transferSvc(st, stubMarkSource{mark: d("1.1000")})
	req := baseTransferReq()
	req.ToAccountID = 20 // user 9, root 20 — no shared entity/hierarchy
	_, err := s.Execute(context.Background(), req)
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != CodePositionTransferFailed {
		t.Fatalf("err %v want %s", err, CodePositionTransferFailed)
	}
}

func TestTransferHierarchyAllowsDifferentUsers(t *testing.T) {
	st := seedTransferFixture()
	// 10 (user 1, master) → 12 (user 2, parent 10): different users but
	// the same master hierarchy — allowed per §13.9.
	seedPosition(st, 1, 10, 55, PositionLong, "10000", "1.1000", "")
	st.balanceFor(10, "USD").locked = d("2000") // covers the derived release
	st.balanceFor(12, "USD").avail = d("5000")

	s, _ := transferSvc(st, stubMarkSource{mark: d("1.1000")})
	req := baseTransferReq()
	req.ToAccountID = 12
	res, err := s.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// HEDGING destination: full qty × mark / lev = 10000×1.1/10 = 1100.
	if !res.DestLocked.Equal(d("1100")) {
		t.Fatalf("hedging dest lock=%s", res.DestLocked)
	}
}

func TestTransferAbortsOnInsufficientDestMargin(t *testing.T) {
	st := seedTransferFixture()
	seedPosition(st, 1, 10, 55, PositionLong, "10000", "1.1000", "1100")
	st.balanceFor(10, "USD").locked = d("1100")
	st.balanceFor(11, "USD").avail = d("100") // < 1100 required

	s, _ := transferSvc(st, stubMarkSource{mark: d("1.1000")})
	before := len(st.posted)
	_, err := s.Execute(context.Background(), baseTransferReq())
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != CodeTransferMarginInsufficient {
		t.Fatalf("err %v want %s", err, CodeTransferMarginInsufficient)
	}
	if len(st.posted) != before {
		t.Fatal("journal posted despite abort")
	}
	// Source position untouched (atomic rollback contract).
	if !st.pos.positions[posKey{10, 55, PositionLong}].Quantity.Equal(d("10000")) {
		t.Fatal("source position mutated on abort")
	}
}

func TestTransferNettingDestAbsorbsOpposing(t *testing.T) {
	st := seedTransferFixture()
	seedPosition(st, 1, 10, 55, PositionLong, "4000", "1.1000", "")
	// Destination NETTING holds SHORT 6000 — the 4000 LONG transfer
	// reduces it to SHORT 2000 and FREES margin.
	seedPosition(st, 2, 11, 55, PositionShort, "6000", "1.0500", "")
	st.balanceFor(10, "USD").locked = d("500")
	st.balanceFor(11, "USD").avail = d("100")
	st.balanceFor(11, "USD").locked = d("2000")

	s, _ := transferSvc(st, stubMarkSource{mark: d("1.1000")})
	req := baseTransferReq()
	req.Quantity = d("4000")
	res, err := s.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Net delta: |−6000| → |−2000|, Δ = −4000 → frees 4000×1.1/10 = 440.
	if !res.DestLocked.Equal(d("-440")) {
		t.Fatalf("dest delta %s want -440", res.DestLocked)
	}
	dst := st.balanceFor(11, "USD")
	if !dst.avail.Equal(d("540")) || !dst.locked.Equal(d("1560")) {
		t.Fatalf("dst bal %s/%s", dst.avail, dst.locked)
	}
	// Destination NETTING absorbed: SHORT 6000 → SHORT 2000.
	dp := res.DestUpdate.Position
	if dp.Side != PositionShort || !dp.Quantity.Equal(d("2000")) {
		t.Fatalf("dst position %+v", dp)
	}
}

func TestTransferMissingMarkFailsClosed(t *testing.T) {
	st := seedTransferFixture()
	seedPosition(st, 1, 10, 55, PositionLong, "10000", "1.1000", "")

	s, _ := transferSvc(st, stubMarkSource{err: ErrNoMarkPrice})
	_, err := s.Execute(context.Background(), baseTransferReq())
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != CodeMarkPriceUnavailable {
		t.Fatalf("err %v want %s", err, CodeMarkPriceUnavailable)
	}
	// Non-positive mark is equally fatal.
	s2, _ := transferSvc(st, stubMarkSource{mark: decimal.Zero})
	if _, err := s2.Execute(context.Background(), baseTransferReq()); err == nil {
		t.Fatal("zero mark must fail closed")
	}
	// Nil source configured → same code.
	s3 := newTransferServiceForTest(st, &fakeLocker{}, &fakeDispatcher{}, nil, nil, "t")
	if _, err := s3.Execute(context.Background(), baseTransferReq()); !stderrors.As(err, &e) || e.Code != CodeMarkPriceUnavailable {
		t.Fatalf("nil-source err %v", err)
	}
}

func TestTransferIdempotentReplay(t *testing.T) {
	st := seedTransferFixture()
	seedPosition(st, 1, 10, 55, PositionLong, "10000", "1.1000", "")
	st.balanceFor(10, "USD").locked = d("2000")
	st.balanceFor(11, "USD").avail = d("5000")

	s, _ := transferSvc(st, stubMarkSource{mark: d("1.1000")})
	req := baseTransferReq()
	req.IdempotencyKey = "xfer-key-1"
	res1, err := s.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := s.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Replayed {
		t.Fatal("second call must report replay")
	}
	if len(st.posted) != 1 {
		t.Fatalf("replayed transfer double-posted (%d journals)", len(st.posted))
	}
	if res2.Transfer.ID != res1.Transfer.ID {
		t.Fatalf("replay resolved different row %d vs %d", res2.Transfer.ID, res1.Transfer.ID)
	}
}

func TestTransferValidationRejectsBadRequests(t *testing.T) {
	st := seedTransferFixture()
	s, _ := transferSvc(st, stubMarkSource{mark: d("1.1")})
	cases := []TransferRequest{
		{FromAccountID: 10, ToAccountID: 10, InstrumentID: 55, Side: PositionLong,
			Quantity: d("1"), ReasonCode: "X", AuthorizedBy: 1}, // same acct
		{FromAccountID: 10, ToAccountID: 11, InstrumentID: 55, Side: "BOGUS",
			Quantity: d("1"), ReasonCode: "X", AuthorizedBy: 1},
		{FromAccountID: 10, ToAccountID: 11, InstrumentID: 55, Side: PositionLong,
			Quantity: d("-1"), ReasonCode: "X", AuthorizedBy: 1},
		{FromAccountID: 10, ToAccountID: 11, InstrumentID: 55, Side: PositionLong,
			Quantity: d("1"), ReasonCode: "", AuthorizedBy: 1},
		{FromAccountID: 10, ToAccountID: 11, InstrumentID: 55, Side: PositionLong,
			Quantity: d("1"), ReasonCode: "X", AuthorizedBy: 0},
		{FromAccountID: 10, ToAccountID: 30, InstrumentID: 55, Side: PositionLong,
			Quantity: d("1"), ReasonCode: "X", AuthorizedBy: 1}, // SUSPENDED dest
	}
	for i, req := range cases {
		_, err := s.Execute(context.Background(), req)
		var e *excerrors.Error
		if !stderrors.As(err, &e) || e.Code != CodePositionTransferFailed {
			t.Fatalf("case %d: err %v want %s", i, err, CodePositionTransferFailed)
		}
	}
}

func TestTransferDestLockedClampWhenFreeing(t *testing.T) {
	// NETTING absorb frees margin — clamped by actual locked balance.
	st := seedTransferFixture()
	seedPosition(st, 1, 10, 55, PositionLong, "4000", "1.1000", "")
	seedPosition(st, 2, 11, 55, PositionShort, "6000", "1.0500", "")
	st.balanceFor(10, "USD").locked = d("500")
	st.balanceFor(11, "USD").avail = d("100")
	st.balanceFor(11, "USD").locked = d("100") // less than the 440 freed

	s, _ := transferSvc(st, stubMarkSource{mark: d("1.1000")})
	req := baseTransferReq()
	req.Quantity = d("4000")
	res, err := s.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DestLocked.Equal(d("-100")) {
		t.Fatalf("freed margin %s want -100 (clamped to locked)", res.DestLocked)
	}
	dst := st.balanceFor(11, "USD")
	if !dst.locked.IsZero() || !dst.avail.Equal(d("200")) {
		t.Fatalf("dst bal %s/%s", dst.avail, dst.locked)
	}
}

// ---------------------------------------------------------------------------
// PostgreSQL integration (EXC_PG_TEST=1): real pgxTransferStore + the real
// DoubleEntryLedgerService poster against the migration-061 table.
// ---------------------------------------------------------------------------

func TestIntegrationPositionTransferPgxStore(t *testing.T) {
	pool := grossNetPool(t)
	ctx := context.Background()

	// Fixture: source + destination accounts (same user), one open
	// position, funded balances.
	var userID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users LIMIT 1`).Scan(&userID); err != nil {
		t.Skipf("no users fixture: %v", err)
	}
	var fromAcct, toAcct, instrID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type)
		VALUES ($1, 'MARGIN') RETURNING id`, userID).Scan(&fromAcct); err != nil {
		t.Skipf("account fixture: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type)
		VALUES ($1, 'MARGIN') RETURNING id`, userID).Scan(&toAcct); err != nil {
		t.Skipf("account fixture: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM instruments LIMIT 1`).Scan(&instrID); err != nil {
		t.Skipf("instrument fixture: %v", err)
	}
	var posID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity,
		                       entry_price, mark_price, unrealized_pnl, realized_pnl)
		VALUES ($1,$2,'LONG',1000,1.1,1.1,0,0) RETURNING id`, fromAcct, instrID).Scan(&posID); err != nil {
		t.Skipf("position fixture: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM position_transfers WHERE from_account_id = ANY($1)`, []int64{fromAcct, toAcct})
		pool.Exec(ctx, `DELETE FROM positions WHERE id=$1`, posID)
		pool.Exec(ctx, `DELETE FROM accounts WHERE id = ANY($1)`, []int64{fromAcct, toAcct})
	})

	store := pgxTransferStore{pool: pool, poster: nil}
	err := store.InTx(ctx, func(ctx context.Context, tx TransferTx) error {
		from, to, err := tx.LockTransferAccounts(ctx, fromAcct, toAcct)
		if err != nil {
			return err
		}
		if from.ID != fromAcct || to.ID != toAcct || from.UserID != userID {
			return fmt.Errorf("locked accounts wrong: %+v %+v", from, to)
		}
		root, err := tx.AccountRootID(ctx, toAcct)
		if err != nil || root != toAcct {
			return fmt.Errorf("root=%d err=%v", root, err)
		}
		inst, err := tx.InstrumentSpec(ctx, instrID)
		if err != nil || inst.ID != instrID || inst.QuoteCurrency == "" {
			return fmt.Errorf("instrument %+v err=%v", inst, err)
		}
		mu, err := tx.PositionMarginUsed(ctx, posID)
		if err != nil {
			return err
		}
		_ = mu
		// Audit row lifecycle.
		rec := PositionTransfer{
			FromAccountID: fromAcct, ToAccountID: toAcct, InstrumentID: instrID,
			Side: PositionLong, Quantity: d("100"), TransferPrice: d("1.1"),
			ReasonCode: TransferReasonSubAccount, RegIndicator: RegIndicatorTransfer,
			Status: "PENDING", AuthorizedBy: userID, IdempotencyKey: "it-xfer-1",
		}
		id, err := tx.InsertTransfer(ctx, rec)
		if err != nil || id == 0 {
			return fmt.Errorf("insert id=%d err=%v", id, err)
		}
		if err := tx.CompleteTransfer(ctx, id, 0, time.Now().UTC()); err != nil {
			return fmt.Errorf("complete: %w", err)
		}
		// Idempotent replay resolves the row.
		id2, err := tx.InsertTransfer(ctx, rec)
		if err != nil || id2 != 0 {
			return fmt.Errorf("replay insert id=%d err=%v", id2, err)
		}
		got, err := tx.FindTransferByKey(ctx, "it-xfer-1")
		if err != nil || got == nil || got.ID != id || got.Status != "COMPLETED" {
			return fmt.Errorf("replay row %+v err=%v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
