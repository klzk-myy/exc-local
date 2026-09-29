package fix

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"exchange/internal/config"
	"exchange/internal/marketmaking"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// fakePipeline records the quote-sourced order flow — every leg must
// ride Submit/Cancel/MassCancel, never a private bypass.
type fakePipeline struct {
	mu           sync.Mutex
	acct         *orders.Account
	instruments  map[string]*orders.Instrument
	orders       map[int64]*orders.SubmitRequest
	nextID       int64
	submitErrFor map[string]error // key: client_order_id prefix → error
	cancelled    []int64
	massCancels  []orders.MassCancelScope
}

func newFakePipeline() *fakePipeline {
	return &fakePipeline{
		acct: &orders.Account{ID: 7},
		instruments: map[string]*orders.Instrument{
			"EUR/USD": {ID: 42, Symbol: "EUR/USD"},
			"GBP/USD": {ID: 43, Symbol: "GBP/USD"},
		},
		orders:       map[int64]*orders.SubmitRequest{},
		nextID:       100,
		submitErrFor: map[string]error{},
	}
}

func (f *fakePipeline) AccountByID(_ context.Context, id int64) (*orders.Account, error) {
	if id != f.acct.ID {
		return nil, nil
	}
	return f.acct, nil
}

func (f *fakePipeline) InstrumentBySymbol(_ context.Context, sym string) (*orders.Instrument, error) {
	// Mirrors orders.Service: the store is keyed by the canonical form.
	return f.instruments[config.CanonicalSymbol(sym)], nil
}

func (f *fakePipeline) Submit(_ context.Context, _ *orders.Account, req *orders.SubmitRequest) (*orders.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for prefix, err := range f.submitErrFor {
		if strings.HasPrefix(req.ClientOrderID, prefix) {
			return nil, err
		}
	}
	f.nextID++
	cp := *req
	f.orders[f.nextID] = &cp
	return &orders.Ack{OrderID: f.nextID, ClientOrderID: req.ClientOrderID, Status: "ACCEPTED"}, nil
}

func (f *fakePipeline) Cancel(_ context.Context, _ *orders.Account, orderID int64,
	_, _, _ string) (*orders.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.orders[orderID]; !ok {
		return nil, excerrors.New("ORDER_NOT_FOUND", "no such order")
	}
	delete(f.orders, orderID)
	f.cancelled = append(f.cancelled, orderID)
	return &orders.Ack{OrderID: orderID, Status: "CANCELLED"}, nil
}

func (f *fakePipeline) MassCancel(_ context.Context, scope orders.MassCancelScope,
	_, _, _ string) (*orders.MassCancelResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.massCancels = append(f.massCancels, scope)
	n := 0
	for id, req := range f.orders {
		if scope.SessionID != "" && req.SessionID != scope.SessionID {
			continue
		}
		delete(f.orders, id)
		n++
	}
	return &orders.MassCancelResult{Cancelled: n, Scope: scope}, nil
}

type fakeEntitlement struct {
	programs map[string]*marketmaking.Program // "acct|inst"
}

func (f *fakeEntitlement) Entitled(_ context.Context, accountID, instrumentID int64) (*marketmaking.Program, error) {
	return f.programs[fmt.Sprintf("%d|%d", accountID, instrumentID)], nil
}

type fakeLockout struct{ locked map[string]bool }

func (f fakeLockout) MMPLocked(_ context.Context, accountID, instrumentID int64) bool {
	return f.locked[fmt.Sprintf("%d|%d", accountID, instrumentID)]
}

type obsRecorder struct {
	mu   sync.Mutex
	seen int
}

func (o *obsRecorder) ObserveQuote(_ context.Context, _, _ int64,
	_, _, _, _ *decimal.Decimal) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen++
	return nil
}

func activeProgram(inst int64) *marketmaking.Program {
	return &marketmaking.Program{
		ID: 1, AccountID: 7, InstrumentID: &inst,
		MinQuoteSize: mustMM("1000"), MaxSpreadBps: mustMM("10"),
		PresencePct: mustMM("80"), MMPMaxFills: 10, MMPWindowMs: 1000,
		Status: marketmaking.StatusActive,
	}
}

func mustMM(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

func twoSidedEntry(id, sym string) QuoteEntry {
	return QuoteEntry{
		QuoteEntryID: id, Symbol: sym,
		BidPx: decPtr("1.1000"), BidSize: decPtr("100000"),
		OfferPx: decPtr("1.1002"), OfferSize: decPtr("100000"),
	}
}

// decPtr adapts the shared dec() helper (report_test.go) for pointer fields.
func decPtr(s string) *decimal.Decimal {
	v := dec(s)
	return &v
}

// --- Firm-liquidity gate (§6.4 / FX Global Code P17) --------------------------

func TestMassQuoteRejectsNonZeroHoldTime(t *testing.T) {
	pipe := newFakePipeline()
	ent := &fakeEntitlement{programs: map[string]*marketmaking.Program{
		"7|42": activeProgram(42),
	}}
	svc, err := NewQuoteService(pipe, ent, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := svc.SubmitMassQuote(context.Background(), "FIX.4.4:V->MM", 7, &MassQuote{
		QuoteSetID: "s1",
		Entries:    []QuoteEntry{twoSidedEntry("e1", "EURUSD")},
		HoldTime:   50 * time.Millisecond, // last-look attempt
	})
	if err == nil || excerrors.CodeOf(err) != marketmaking.CodeQuoteRequestRejected {
		t.Fatalf("non-zero hold time must reject QUOTE_REQUEST_REJECTED: %v", err)
	}
	if ack == nil || len(ack.Entries) != 1 ||
		ack.Entries[0].Status != QuoteStatusRejected ||
		!strings.Contains(ack.Entries[0].Text, "firm liquidity") {
		t.Fatalf("per-entry reject expected: %+v", ack)
	}
	if len(pipe.orders) != 0 {
		t.Fatalf("no quote leg may reach the book on reject: %d", len(pipe.orders))
	}
}

func TestMassQuoteRequiresSetID(t *testing.T) {
	pipe := newFakePipeline()
	svc, _ := NewQuoteService(pipe, &fakeEntitlement{}, nil, nil)
	if _, err := svc.SubmitMassQuote(context.Background(), "s", 7,
		&MassQuote{Entries: []QuoteEntry{twoSidedEntry("e", "EURUSD")}}); err == nil {
		t.Fatal("missing QuoteSetID must reject")
	}
}

// --- Entitlement + MMP lockout ----------------------------------------------

func TestMassQuoteRejectsUnentitled(t *testing.T) {
	pipe := newFakePipeline()
	svc, _ := NewQuoteService(pipe, &fakeEntitlement{programs: map[string]*marketmaking.Program{}}, nil, nil)
	ack, err := svc.SubmitMassQuote(context.Background(), "sess", 7, &MassQuote{
		QuoteSetID: "s1",
		Entries:    []QuoteEntry{twoSidedEntry("e1", "EURUSD")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ack.Entries[0].Status != QuoteStatusRejected ||
		!strings.Contains(ack.Entries[0].Text, "SESSION_NOT_ENTITLED") {
		t.Fatalf("no program must reject entry: %+v", ack.Entries[0])
	}
}

func TestMassQuoteRejectsWhenMMPLocked(t *testing.T) {
	pipe := newFakePipeline()
	ent := &fakeEntitlement{programs: map[string]*marketmaking.Program{"7|42": activeProgram(42)}}
	lock := fakeLockout{locked: map[string]bool{"7|42": true}}
	svc, _ := NewQuoteService(pipe, ent, lock, nil)
	ack, err := svc.SubmitMassQuote(context.Background(), "sess", 7, &MassQuote{
		QuoteSetID: "s1",
		Entries:    []QuoteEntry{twoSidedEntry("e1", "EURUSD")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ack.Entries[0].Status != QuoteStatusRejected ||
		!strings.Contains(ack.Entries[0].Text, "MMP_LOCKED_OUT") {
		t.Fatalf("locked account must reject: %+v", ack.Entries[0])
	}
	if len(pipe.orders) != 0 {
		t.Fatal("no orders may post while MMP-locked")
	}
}

// --- Quote-set lifecycle -----------------------------------------------------

func TestQuoteSetLifecycle(t *testing.T) {
	pipe := newFakePipeline()
	ent := &fakeEntitlement{programs: map[string]*marketmaking.Program{
		"7|42": activeProgram(42), "7|43": activeProgram(43),
	}}
	obs := &obsRecorder{}
	svc, err := NewQuoteService(pipe, ent, nil, obs)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess := "FIX.4.4:VENUE->MM1"

	// Set s1: two instruments × two sides = 4 resting legs.
	ack, err := svc.SubmitMassQuote(ctx, sess, 7, &MassQuote{
		QuoteSetID: "s1",
		Entries: []QuoteEntry{
			twoSidedEntry("e1", "EURUSD"),
			twoSidedEntry("e2", "GBPUSD"),
		},
	})
	if err != nil || !ack.Accepted() {
		t.Fatalf("set must accept: %v %+v", err, ack)
	}
	if len(pipe.orders) != 4 {
		t.Fatalf("2 entries × 2 sides = 4 legs: %d", len(pipe.orders))
	}
	if obs.seen != 2 {
		t.Fatalf("one obligation sample per entry: %d", obs.seen)
	}
	// Every leg is a quote-sourced GTC LIMIT on the FIX session.
	for _, r := range pipe.orders {
		if r.OrderType != orders.TypeLimit || r.TimeInForce != orders.TIFGTC {
			t.Fatalf("quotes are GTC LIMIT: %+v", r)
		}
		if !strings.HasPrefix(r.ClientOrderID, "mq:s1:") {
			t.Fatalf("quote attribution missing: %q", r.ClientOrderID)
		}
		if r.SessionID != sess {
			t.Fatalf("quote legs carry the FIX session scope: %q", r.SessionID)
		}
	}

	// Re-quote same set+entry → prior legs cancelled before replace.
	before := map[int64]bool{}
	for id := range pipe.orders {
		before[id] = true
	}
	ack, err = svc.SubmitMassQuote(ctx, sess, 7, &MassQuote{
		QuoteSetID: "s1",
		Entries:    []QuoteEntry{twoSidedEntry("e1", "EURUSD")},
	})
	if err != nil || !ack.Accepted() {
		t.Fatalf("re-quote must accept: %v %+v", err, ack)
	}
	if len(pipe.cancelled) != 2 {
		t.Fatalf("prior bid+ask must cancel on replace: %v", pipe.cancelled)
	}
	for _, id := range pipe.cancelled {
		if !before[id] {
			t.Fatalf("cancelled %d was not a prior leg", id)
		}
	}
	if len(pipe.orders) != 4 { // e1 replaced (cancel2+add2) + e2 untouched
		t.Fatalf("book must hold 4 legs after replace: %d", len(pipe.orders))
	}

	// 35=Z per-set cancel drops just that set's legs.
	n, err := svc.CancelQuotes(ctx, sess, 7, &QuoteCancel{QuoteSetID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 || len(pipe.orders) != 0 {
		t.Fatalf("per-set cancel must drop the 4 tracked legs: n=%d open=%d",
			n, len(pipe.orders))
	}
	// Idempotent: second cancel of the same set reports 0.
	n, _ = svc.CancelQuotes(ctx, sess, 7, &QuoteCancel{QuoteSetID: "s1"})
	if n != 0 {
		t.Fatalf("re-cancel must be a no-op: %d", n)
	}
}

func TestQuoteCancelScopes(t *testing.T) {
	pipe := newFakePipeline()
	ent := &fakeEntitlement{programs: map[string]*marketmaking.Program{
		"7|42": activeProgram(42), "7|43": activeProgram(43),
	}}
	svc, _ := NewQuoteService(pipe, ent, nil, nil)
	ctx := context.Background()
	sess := "sess-A"
	if _, err := svc.SubmitMassQuote(ctx, sess, 7, &MassQuote{
		QuoteSetID: "s1",
		Entries: []QuoteEntry{
			twoSidedEntry("e1", "EURUSD"), twoSidedEntry("e2", "GBPUSD"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Cancel per symbol → mass-cancel seam scoped account+instrument+session.
	n, err := svc.CancelQuotes(ctx, sess, 7, &QuoteCancel{
		CancelType: QuoteCancelPerSymbol, Symbol: "EURUSD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pipe.massCancels) != 1 {
		t.Fatalf("symbol cancel must ride MassCancel: %v", pipe.massCancels)
	}
	sc := pipe.massCancels[0]
	if sc.AccountID != 7 || sc.InstrumentID != 42 || sc.SessionID != sess {
		t.Fatalf("per-symbol scope wrong: %+v", sc)
	}
	if n != 4 { // fake cancels all session orders — count reports the sweep
		t.Fatalf("mass-cancel result: %d", n)
	}
	// Cancel all → scope carries account+session only.
	if _, err := svc.SubmitMassQuote(ctx, sess, 7, &MassQuote{
		QuoteSetID: "s2", Entries: []QuoteEntry{twoSidedEntry("e1", "EURUSD")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelQuotes(ctx, sess, 7,
		&QuoteCancel{CancelType: QuoteCancelAllQuotes}); err != nil {
		t.Fatal(err)
	}
	sc = pipe.massCancels[1]
	if sc.AccountID != 7 || sc.InstrumentID != 0 || sc.SessionID != sess {
		t.Fatalf("all-quotes scope wrong: %+v", sc)
	}
	// Unsupported type rejects.
	if _, err := svc.CancelQuotes(ctx, sess, 7,
		&QuoteCancel{CancelType: 9}); err == nil {
		t.Fatal("unknown cancel type must reject")
	}
}

// --- Atomic two-sided intent --------------------------------------------------

func TestAskLegFailureUnwindsBid(t *testing.T) {
	pipe := newFakePipeline()
	pipe.submitErrFor["mq:s1:1:e1:SELL"] = excerrors.New("ORDER_REJECTED", "ask leg fails")
	ent := &fakeEntitlement{programs: map[string]*marketmaking.Program{"7|42": activeProgram(42)}}
	svc, _ := NewQuoteService(pipe, ent, nil, nil)
	ack, err := svc.SubmitMassQuote(context.Background(), "sess", 7, &MassQuote{
		QuoteSetID: "s1", Entries: []QuoteEntry{twoSidedEntry("e1", "EURUSD")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ack.Entries[0].Status != QuoteStatusRejected ||
		!strings.Contains(ack.Entries[0].Text, "ask leg") {
		t.Fatalf("ask failure must reject entry: %+v", ack.Entries[0])
	}
	// The accepted bid leg is unwound — no phantom one-sided quote.
	if len(pipe.orders) != 0 {
		t.Fatalf("bid must unwind on ask failure: %d live", len(pipe.orders))
	}
	if len(pipe.cancelled) != 1 {
		t.Fatalf("unwind must cancel the placed bid: %v", pipe.cancelled)
	}
}
