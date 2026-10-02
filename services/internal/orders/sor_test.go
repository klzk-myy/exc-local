package orders

// Phase-3 Task 4 (IMP-PLAN) — SOR consult coverage: the router sits
// between row persistence and engine dispatch; local decisions dispatch
// normally, external routes park the parent RESERVED, router errors
// reject the row so it never reads as live.

import (
	"context"
	"errors"
	"testing"
	"time"

	"exchange/internal/sor"
	"exchange/pkg/decimal"
)

type stubSORRouter struct {
	res *sor.RouteResult
	err error
	got *sor.ParentOrder
}

func (s *stubSORRouter) Route(_ context.Context, p *sor.ParentOrder,
	_ sor.BookView) (*sor.RouteResult, error) {
	s.got = p
	return s.res, s.err
}

type stubSORBook struct{ v sor.BookView }

func (b stubSORBook) View(string) sor.BookView { return b.v }

func newSORSvc(t *testing.T, st *fakeStore, sub *fakeSubmitter,
	router SORRouter, book SORBookView) *Service {
	t.Helper()
	svc := newSvc(t, st, sub)
	svc.WithSOR(router, book)
	return svc
}

func TestSubmitSORLocalDecisionDispatches(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	router := &stubSORRouter{res: &sor.RouteResult{Local: true}}
	svc := newSORSvc(t, st, sub, router, stubSORBook{})

	ack, err := svc.Submit(context.Background(), st.acct, submitReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if router.got == nil {
		t.Fatal("router never consulted")
	}
	if ack.Status != "ACTIVE" || len(sub.sent) != 1 {
		t.Fatalf("local route must dispatch: %+v / %d sends", ack, len(sub.sent))
	}
}

func TestSubmitSORExternalRouteParksReserved(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	router := &stubSORRouter{res: &sor.RouteResult{
		Shadow: &sor.ShadowOrder{ID: 9, State: sor.StateRouted}}}
	svc := newSORSvc(t, st, sub, router, stubSORBook{})

	ack, err := svc.Submit(context.Background(), st.acct, submitReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if ack.Status != "ROUTED_EXTERNAL" {
		t.Fatalf("ack status %q, want ROUTED_EXTERNAL", ack.Status)
	}
	if len(sub.sent) != 0 {
		t.Fatal("external route must not dispatch to the engine")
	}
	o := st.orders[ack.OrderID]
	if o == nil || o.Status != "RESERVED" {
		t.Fatalf("parent order status %q, want RESERVED", o.Status)
	}
}

func TestSubmitSORErrorMarksRejected(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	router := &stubSORRouter{err: errors.New("SOR_TIMEOUT")}
	svc := newSORSvc(t, st, sub, router, stubSORBook{})

	_, err := svc.Submit(context.Background(), st.acct, submitReq())
	if err == nil {
		t.Fatal("router error must surface to the client")
	}
	for _, o := range st.orders {
		if o.Status != "REJECTED" {
			t.Fatalf("order %d left status %s after SOR failure", o.ID, o.Status)
		}
	}
	if len(sub.sent) != 0 {
		t.Fatal("failed route must not dispatch")
	}
}

func TestSubmitSOREligibilitySkipsManagedOrders(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	router := &stubSORRouter{res: &sor.RouteResult{Local: true}}
	svc := newSORSvc(t, st, sub, router, stubSORBook{})

	// Post-only must never route — it exists to rest on the book.
	req := submitReq()
	req.PostOnly = true
	if _, err := svc.Submit(context.Background(), st.acct, req); err != nil {
		t.Fatalf("post-only submit: %v", err)
	}
	if router.got != nil {
		t.Fatal("post-only order consulted the router")
	}

	// Trailing stop: engine-managed lifecycle — ineligible.
	req2 := submitReq()
	req2.StopPrice = d("1.04")
	req2.OrderType = TypeStop
	if _, err := svc.Submit(context.Background(), st.acct, req2); err != nil {
		t.Fatalf("stop submit: %v", err)
	}
	if router.got != nil {
		t.Fatal("stop order consulted the router")
	}
}

func TestSORBookViewCacheStaleness(t *testing.T) {
	c := sor.NewBookViewCache()
	now := decimal.NewFromInt(0)
	_ = now
	clock := time.Unix(1_000, 0)
	c.Now = func() time.Time { return clock }
	c.Observe("EUR/USD",
		[]sor.Level{{Price: 105000000, Qty: 1_000_00000000, Count: 1}},
		[]sor.Level{{Price: 105010000, Qty: 2_000_00000000, Count: 2}})

	v := c.View("EUR/USD")
	if !v.HasBBO || !v.BestBid.Equal(decimal.NewFromScaled(105000000)) ||
		!v.BidDepth.Equal(decimal.NewFromScaled(1_000_00000000)) {
		t.Fatalf("view: %+v", v)
	}
	// Stale entries must read as no-BBO — never route on phantom books.
	clock = clock.Add(11 * time.Second)
	if v2 := c.View("EUR/USD"); v2.HasBBO {
		t.Fatalf("stale view still live: %+v", v2)
	}
}
