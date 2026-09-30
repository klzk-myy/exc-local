package settlement

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// In-memory PositionStore for unit tests (no PostgreSQL needed).
// ---------------------------------------------------------------------------

type memPositionStore struct {
	mu        sync.Mutex
	positions map[posKey]*Position // side-scoped key (hedging-capable)
	fills     map[string]decimal.Decimal
	nextID    int64
	maxOpen   map[int64]int    // per-account override
	modes     map[int64]string // account position_mode override
	txErr     error
}

// posKey mirrors the migration-233 (account, instrument, side) unique
// index. NETTING accounts keep a single row by side-flipping in place.
type posKey struct {
	acct, instr int64
	side        string
}

func newMemPositionStore() *memPositionStore {
	return &memPositionStore{
		positions: map[posKey]*Position{},
		fills:     map[string]decimal.Decimal{},
		maxOpen:   map[int64]int{},
		modes:     map[int64]string{},
		nextID:    1,
	}
}

func (m *memPositionStore) InTx(ctx context.Context, fn func(context.Context, PositionTx) error) error {
	if m.txErr != nil {
		return m.txErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(ctx, &memPositionTx{m: m})
}

type memPositionTx struct{ m *memPositionStore }

func fillKey(tradeID uint64, accountID int64) string {
	return fmt.Sprintf("%d:%d", tradeID, accountID)
}

func (t *memPositionTx) FillApplied(_ context.Context, tradeID uint64, accountID int64) (bool, error) {
	_, ok := t.m.fills[fillKey(tradeID, accountID)]
	return ok, nil
}

func (t *memPositionTx) RecordFill(_ context.Context, f PositionFill, realized decimal.Decimal) error {
	k := fillKey(f.TradeID, f.AccountID)
	if _, dup := t.m.fills[k]; dup {
		return errors.New("duplicate fill")
	}
	t.m.fills[k] = realized
	return nil
}

// GetPositionForUpdate returns the netting row: the open row if any,
// else the most recent row for the pair (side is flipped in place on
// reversal — NETTING keeps one row).
func (t *memPositionTx) GetPositionForUpdate(_ context.Context, accountID, instrumentID int64) (*Position, error) {
	var best *Position
	for k, p := range t.m.positions {
		if k.acct != accountID || k.instr != instrumentID {
			continue
		}
		if best == nil || (!p.Quantity.IsZero() && best.Quantity.IsZero()) ||
			(best.Quantity.IsZero() == p.Quantity.IsZero() && p.ID > best.ID) {
			best = p
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	return &cp, nil
}

func (t *memPositionTx) GetSidePositionForUpdate(_ context.Context, accountID, instrumentID int64, side string) (*Position, error) {
	p := t.m.positions[posKey{accountID, instrumentID, side}]
	if p == nil {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (t *memPositionTx) GetPositionByIDForUpdate(_ context.Context, positionID int64) (*Position, error) {
	for _, p := range t.m.positions {
		if p.ID == positionID {
			cp := *p
			return &cp, nil
		}
	}
	return nil, nil
}

func (t *memPositionTx) UpsertPosition(_ context.Context, p *Position) error {
	cp := *p
	if cp.ID == 0 {
		cp.ID = t.m.nextID
		t.m.nextID++
		p.ID = cp.ID
	}
	// Re-key on side flip: a NETTING reversal rewrites the row's side —
	// the (acct,instr,side) map key must track it.
	for k, old := range t.m.positions {
		if old.ID == cp.ID && k.side != cp.Side {
			delete(t.m.positions, k)
		}
	}
	t.m.positions[posKey{cp.AccountID, cp.InstrumentID, cp.Side}] = &cp
	return nil
}

func (t *memPositionTx) CountOpenPositions(_ context.Context, accountID int64) (int, error) {
	n := 0
	for k, p := range t.m.positions {
		if k.acct == accountID && !p.Quantity.IsZero() {
			n++
		}
	}
	return n, nil
}

func (t *memPositionTx) MaxOpenPositions(_ context.Context, accountID int64) (int, bool, error) {
	v, ok := t.m.maxOpen[accountID]
	return v, ok, nil
}

func (t *memPositionTx) AccountPositionMode(_ context.Context, accountID int64) (string, error) {
	if m, ok := t.m.modes[accountID]; ok {
		return m, nil
	}
	return posModeNetting, nil
}

// stub oracle
type stubOracle struct {
	marks map[int64]decimal.Decimal
	err   error
}

func (s stubOracle) MarkPrice(_ context.Context, id int64) (decimal.Decimal, error) {
	if s.err != nil {
		return decimal.Zero, s.err
	}
	v, ok := s.marks[id]
	if !ok {
		return decimal.Zero, ErrNoMarkPrice
	}
	return v, nil
}

func d(s string) decimal.Decimal { return decimal.MustFromString(s) }

func svc(store *memPositionStore, oracle MarkPriceProvider) *PositionService {
	return NewPositionService(store, oracle, 0)
}

func TestPositionOpenLong(t *testing.T) {
	st := newMemPositionStore()
	s := svc(st, nil)
	upd, err := s.ProcessFill(context.Background(), PositionFill{
		TradeID: 1, AccountID: 10, InstrumentID: 100,
		Side: FillBuy, Price: d("1.1000"), Quantity: d("10000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Action != "OPENED" {
		t.Fatalf("action=%s", upd.Action)
	}
	p := upd.Position
	if p.Side != PositionLong || !p.Quantity.Equal(d("10000")) || !p.EntryPrice.Equal(d("1.1000")) {
		t.Fatalf("bad position: %+v", p)
	}
	// placeholder mark = last trade price → flat unrealized
	if !p.UnrealizedPnl.IsZero() || !p.MarkPrice.Equal(d("1.1000")) {
		t.Fatalf("mark/unrealized wrong: %+v", p)
	}
}

func TestPositionIncreaseVWAP(t *testing.T) {
	st := newMemPositionStore()
	s := svc(st, nil)
	s.ProcessFill(context.Background(), PositionFill{1, 10, 100, FillBuy, d("1.1000"), d("10000"), false, 0})
	upd, err := s.ProcessFill(context.Background(), PositionFill{
		TradeID: 2, AccountID: 10, InstrumentID: 100,
		Side: FillBuy, Price: d("1.1100"), Quantity: d("10000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Action != "INCREASED" {
		t.Fatalf("action=%s", upd.Action)
	}
	// VWAP = (10000*1.1 + 10000*1.11)/20000 = 1.105
	if !upd.Position.EntryPrice.Equal(d("1.1050")) {
		t.Fatalf("entry=%s want 1.1050", upd.Position.EntryPrice)
	}
	// mark = 1.11 (last trade): unrealized = 20000*(1.11-1.105)=100
	if !upd.Position.UnrealizedPnl.Equal(d("100.000000")) && !upd.Position.UnrealizedPnl.Equal(d("100")) {
		t.Fatalf("unrealized=%s want 100", upd.Position.UnrealizedPnl)
	}
}

func TestPositionPartialCloseRealized(t *testing.T) {
	st := newMemPositionStore()
	s := svc(st, nil)
	s.ProcessFill(context.Background(), PositionFill{1, 10, 100, FillBuy, d("1.1000"), d("10000"), false, 0})
	upd, err := s.ProcessFill(context.Background(), PositionFill{
		TradeID: 2, AccountID: 10, InstrumentID: 100,
		Side: FillSell, Price: d("1.1200"), Quantity: d("4000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Action != "REDUCED" {
		t.Fatalf("action=%s", upd.Action)
	}
	// realized = (1.12-1.10)*4000 = 80 quote ccy
	if !upd.RealizedPnLDelta.Equal(d("80.0000")) && !upd.RealizedPnLDelta.Equal(d("80")) {
		t.Fatalf("realized=%s want 80", upd.RealizedPnLDelta)
	}
	if !upd.Position.Quantity.Equal(d("6000")) || !upd.Position.RealizedPnl.Equal(d("80")) {
		t.Fatalf("pos: %+v", upd.Position)
	}
}

func TestPositionFullClose(t *testing.T) {
	st := newMemPositionStore()
	s := svc(st, nil)
	s.ProcessFill(context.Background(), PositionFill{1, 10, 100, FillSell, d("1.1000"), d("5000"), false, 0})
	upd, err := s.ProcessFill(context.Background(), PositionFill{
		TradeID: 2, AccountID: 10, InstrumentID: 100,
		Side: FillBuy, Price: d("1.0800"), Quantity: d("5000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Action != "CLOSED" {
		t.Fatalf("action=%s", upd.Action)
	}
	// SHORT: realized = (entry - price)*qty = (1.10-1.08)*5000 = 100
	if !upd.RealizedPnLDelta.Equal(d("100.0000")) && !upd.RealizedPnLDelta.Equal(d("100")) {
		t.Fatalf("realized=%s want 100", upd.RealizedPnLDelta)
	}
	if upd.Position.EffectiveSide() != PositionFlat || !upd.Position.Quantity.IsZero() {
		t.Fatalf("expected FLAT: %+v", upd.Position)
	}
	if !upd.Position.UnrealizedPnl.IsZero() {
		t.Fatalf("flat position must have zero unrealized: %s", upd.Position.UnrealizedPnl)
	}
}

func TestPositionReversal(t *testing.T) {
	st := newMemPositionStore()
	s := svc(st, nil)
	s.ProcessFill(context.Background(), PositionFill{1, 10, 100, FillBuy, d("1.1000"), d("10000"), false, 0})
	upd, err := s.ProcessFill(context.Background(), PositionFill{
		TradeID: 2, AccountID: 10, InstrumentID: 100,
		Side: FillSell, Price: d("1.0500"), Quantity: d("15000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Action != "REVERSED" {
		t.Fatalf("action=%s", upd.Action)
	}
	// realized on old LONG: (1.05-1.10)*10000 = -500; new SHORT 5000 @1.05
	if !upd.RealizedPnLDelta.Equal(d("-500.0000")) && !upd.RealizedPnLDelta.Equal(d("-500")) {
		t.Fatalf("realized=%s want -500", upd.RealizedPnLDelta)
	}
	p := upd.Position
	if p.Side != PositionShort || !p.Quantity.Equal(d("5000")) || !p.EntryPrice.Equal(d("1.0500")) {
		t.Fatalf("reversed pos: %+v", p)
	}
}

func TestPositionShortUnrealized(t *testing.T) {
	st := newMemPositionStore()
	oracle := stubOracle{marks: map[int64]decimal.Decimal{100: d("1.1200")}}
	s := svc(st, oracle)
	upd, err := s.ProcessFill(context.Background(), PositionFill{
		TradeID: 1, AccountID: 10, InstrumentID: 100,
		Side: FillSell, Price: d("1.1000"), Quantity: d("10000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// SHORT, mark 1.12 > entry 1.10 → unrealized = -10000*0.02 = -200
	if !upd.Position.UnrealizedPnl.Equal(d("-200")) {
		t.Fatalf("unrealized=%s want -200", upd.Position.UnrealizedPnl)
	}
	if upd.MarkStale {
		t.Fatal("mark should be fresh from oracle")
	}
}

func TestPositionMarkStaleFallback(t *testing.T) {
	st := newMemPositionStore()
	oracle := stubOracle{err: errors.New("oracle down")}
	s := svc(st, oracle)
	s.ProcessFill(context.Background(), PositionFill{1, 10, 100, FillBuy, d("1.1000"), d("10000"), false, 0})
	upd, err := s.ProcessFill(context.Background(), PositionFill{
		TradeID: 2, AccountID: 10, InstrumentID: 100,
		Side: FillBuy, Price: d("1.1100"), Quantity: d("10000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !upd.MarkStale {
		t.Fatal("expected MarkStale when oracle errors")
	}
	// falls back to position's existing mark (1.10 from first fill)
	if !upd.Position.MarkPrice.Equal(d("1.1000")) {
		t.Fatalf("mark=%s want fallback 1.1000", upd.Position.MarkPrice)
	}
}

func TestPositionLimitEnforced(t *testing.T) {
	st := newMemPositionStore()
	st.maxOpen[10] = 1
	s := svc(st, nil)
	if _, err := s.ProcessFill(context.Background(), PositionFill{1, 10, 100, FillBuy, d("1.1"), d("1000"), false, 0}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ProcessFill(context.Background(), PositionFill{2, 10, 200, FillBuy, d("1.2"), d("1000"), false, 0})
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != CodePositionLimitExceeded {
		t.Fatalf("want POSITION_LIMIT_EXCEEDED, got %v", err)
	}
	// same-instrument increase must NOT trip the limit
	if _, err := s.ProcessFill(context.Background(), PositionFill{3, 10, 100, FillBuy, d("1.1"), d("1000"), false, 0}); err != nil {
		t.Fatalf("increase on existing position must not be limited: %v", err)
	}
	// close then reopen is allowed (flat doesn't count)
	if _, err := s.ProcessFill(context.Background(), PositionFill{4, 10, 100, FillSell, d("1.1"), d("2000"), false, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessFill(context.Background(), PositionFill{5, 10, 200, FillBuy, d("1.2"), d("1000"), false, 0}); err != nil {
		t.Fatalf("reopen after close should pass: %v", err)
	}
}

func TestPositionDuplicateFill(t *testing.T) {
	st := newMemPositionStore()
	s := svc(st, nil)
	f := PositionFill{TradeID: 7, AccountID: 10, InstrumentID: 100, Side: FillBuy, Price: d("1.1"), Quantity: d("1000")}
	if _, err := s.ProcessFill(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	upd, err := s.ProcessFill(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !upd.Duplicate {
		t.Fatal("duplicate fill not detected")
	}
	// qty must remain 1000, not 2000
	if !upd.Position.Quantity.Equal(d("1000")) {
		t.Fatalf("double-applied: qty=%s", upd.Position.Quantity)
	}
}

func TestProcessTradeBothLegs(t *testing.T) {
	st := newMemPositionStore()
	s := svc(st, nil)
	buy, sell, err := s.ProcessTrade(context.Background(), 42, 100, 10, 20, d("1.1000"), d("10000"))
	if err != nil {
		t.Fatal(err)
	}
	if buy.Position.Side != PositionLong || sell.Position.Side != PositionShort {
		t.Fatalf("buy=%s sell=%s", buy.Position.Side, sell.Position.Side)
	}
	// replaying the same trade must be a no-op for both legs
	buy2, sell2, err := s.ProcessTrade(context.Background(), 42, 100, 10, 20, d("1.1000"), d("10000"))
	if err != nil {
		t.Fatal(err)
	}
	if !buy2.Duplicate || !sell2.Duplicate {
		t.Fatal("trade replay not deduped")
	}
}

func TestInvalidFillRejected(t *testing.T) {
	st := newMemPositionStore()
	s := svc(st, nil)
	for _, f := range []PositionFill{
		{0, 10, 100, FillBuy, d("1.1"), d("100"), false, 0},  // no trade id
		{1, 0, 100, FillBuy, d("1.1"), d("100"), false, 0},   // no account
		{1, 10, 100, "HOLD", d("1.1"), d("100"), false, 0},   // bad side
		{1, 10, 100, FillBuy, d("0"), d("100"), false, 0},    // zero price
		{1, 10, 100, FillBuy, d("1.1"), d("-100"), false, 0}, // negative qty
	} {
		if _, err := s.ProcessFill(context.Background(), f); err == nil {
			t.Fatalf("fill %+v should be rejected", f)
		}
	}
}
