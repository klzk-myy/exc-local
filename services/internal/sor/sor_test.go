// sor_test.go — Task 18.3.14: the 5-state SOR lifecycle, venue walk,
// timeout, race prevention, FILL_BRIDGE dedup.
package sor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func d(s string) decimal.Decimal {
	v, _ := decimal.NewFromString(s)
	return v
}

func decPtr(s string) *decimal.Decimal {
	v, _ := decimal.NewFromString(s)
	return &v
}

func parent() *ParentOrder {
	return &ParentOrder{
		OrderID: 100, AccountID: 7, Symbol: "EURUSD", Side: "BUY",
		Qty: d("1000000"), LimitPrice: decPtr("1.10"),
		ClientOrderID: "cl-1",
	}
}

// thinBook forces external routing (no depth on the ask side).
func thinBook() BookView {
	return BookView{Symbol: "EURUSD", HasBBO: true,
		BestBid: d("1.10"), BestAsk: d("1.1001"),
		BidDepth: d("100"), AskDepth: d("100")}
}

// fatBook keeps routing local.
func fatBook() BookView {
	return BookView{Symbol: "EURUSD", HasBBO: true,
		BestBid: d("1.10"), BestAsk: d("1.1001"),
		BidDepth: d("50000000"), AskDepth: d("50000000")}
}

func thresholds() Thresholds {
	return Thresholds{MinDepth: d("5000000"), MaxSpreadBps: d("5")}
}

// eventLog serializes publish-callback appends — the router invokes the
// callback from its background event goroutine while the test goroutine
// reads the log (a bare slice here is a data race under -race).
type eventLog struct {
	mu  sync.Mutex
	evs []*FillBridgeEvent
}

func (l *eventLog) add(ev *FillBridgeEvent) {
	l.mu.Lock()
	l.evs = append(l.evs, ev)
	l.mu.Unlock()
}

func (l *eventLog) all() []*FillBridgeEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*FillBridgeEvent(nil), l.evs...)
}

func newRouter(store *MemoryShadowStore, venues ...VenueConnector) (*Router, *eventLog) {
	pubs := &eventLog{}
	r := NewRouter(store, venues, func(_ context.Context, ev *FillBridgeEvent) error {
		pubs.add(ev)
		return nil
	}, thresholds())
	// Functional tests are not exercising the timeout path — give the
	// loopback ack goroutine scheduling headroom on contended -race
	// runners (the canonical 500ms budget lapses under CI preemption).
	// Timeout-path tests override r.Timeout explicitly after this call.
	r.Timeout = 5 * time.Second
	r.Start(context.Background())
	return r, pubs
}

func TestLocalWhenLiquid(t *testing.T) {
	r, _ := newRouter(NewMemoryShadowStore())
	defer r.Stop()
	res, err := r.Route(context.Background(), parent(), fatBook())
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if !res.Local || res.Shadow != nil {
		t.Fatalf("expected local decision: %+v", res)
	}
}

func TestRouteFillLifecycle(t *testing.T) {
	v := NewLoopbackConnector("ecna", "EXCH", "ECNA")
	defer v.Close()
	v.FillScript = []FillPlan{
		{Qty: d("400000"), Price: d("1.1005")},             // partial
		{Qty: d("600000"), Price: d("1.1010"), Done: true}, // terminal
	}
	v.DelayFills = 5 * time.Millisecond
	store := NewMemoryShadowStore()
	r, pubs := newRouter(store, v)
	defer r.Stop()

	res, err := r.Route(context.Background(), parent(), thinBook())
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if res.Local || res.Shadow == nil || res.Shadow.State != StateRouted {
		t.Fatalf("expected ROUTED shadow: %+v", res.Shadow)
	}
	// Wait for the fills to reconcile.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sh, _ := store.ShadowByExternalID(context.Background(), "ecna", res.Shadow.ExternalOrderID)
		if sh != nil && sh.State == StateFilled {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	sh, _ := store.ShadowByExternalID(context.Background(), "ecna", res.Shadow.ExternalOrderID)
	if sh.State != StateFilled {
		t.Fatalf("expected FILLED_EXTERNAL, got %s", sh.State)
	}
	if sh.FilledQty.String() != "1000000" {
		t.Fatalf("filled qty %s", sh.FilledQty)
	}
	// Two FILL_BRIDGE events published.
	if got := pubs.all(); len(got) != 2 {
		t.Fatalf("expected 2 FILL_BRIDGE events, got %d", len(got))
	}
	for _, ev := range pubs.all() {
		if ev.EventType != "FILL_BRIDGE" || ev.ParentOrderID != 100 {
			t.Fatalf("bad event %+v", ev)
		}
	}
}

func TestPartialFillState(t *testing.T) {
	v := NewLoopbackConnector("ecna", "EXCH", "ECNA")
	defer v.Close()
	v.FillScript = []FillPlan{{Qty: d("300000"), Price: d("1.1005")}}
	store := NewMemoryShadowStore()
	r, _ := newRouter(store, v)
	defer r.Stop()
	res, err := r.Route(context.Background(), parent(), thinBook())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sh, _ := store.ShadowByExternalID(context.Background(), "ecna", res.Shadow.ExternalOrderID)
		if sh != nil && sh.State == StatePartiallyFilled {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("never reached PARTIALLY_FILLED_EXTERNAL")
}

func TestTimeoutWalksToNextVenue(t *testing.T) {
	dead := NewLoopbackConnector("dead", "EXCH", "DEAD")
	defer dead.Close()
	dead.Silent = true // never responds → 500ms budget lapses
	live := NewLoopbackConnector("ecnb", "EXCH", "ECNB")
	defer live.Close()
	live.FillScript = []FillPlan{{Qty: d("1000000"), Price: d("1.10"), Done: true}}

	store := NewMemoryShadowStore()
	r, _ := newRouter(store, dead, live)
	defer r.Stop()
	r.Timeout = 60 * time.Millisecond // shrink the budget for the test

	res, err := r.Route(context.Background(), parent(), thinBook())
	if err != nil {
		t.Fatalf("route should succeed on venue 2: %v", err)
	}
	if res.Shadow.VenueID != "ecnb" || res.Shadow.Attempt != 2 {
		t.Fatalf("expected attempt 2 on ecnb: %+v", res.Shadow)
	}
}

func TestAllVenuesDeadSORTimeout(t *testing.T) {
	dead := NewLoopbackConnector("dead", "EXCH", "DEAD")
	defer dead.Close()
	dead.Silent = true
	store := NewMemoryShadowStore()
	r, _ := newRouter(store, dead)
	defer r.Stop()
	r.Timeout = 40 * time.Millisecond

	_, err := r.Route(context.Background(), parent(), thinBook())
	if err == nil {
		t.Fatal("expected SOR_TIMEOUT")
	}
	if code := excerrors.CodeOf(err); code != "SOR_TIMEOUT" {
		t.Fatalf("expected SOR_TIMEOUT, got %v", err)
	}
}

func TestVenueRejectFailsClosed(t *testing.T) {
	v := NewLoopbackConnector("ecna", "EXCH", "ECNA")
	defer v.Close()
	v.RejectWith = "no liquidity"
	store := NewMemoryShadowStore()
	r, _ := newRouter(store, v)
	defer r.Stop()
	r.Timeout = 50 * time.Millisecond
	_, err := r.Route(context.Background(), parent(), thinBook())
	if err == nil || !strings.Contains(err.Error(), "SOR_TIMEOUT") {
		t.Fatalf("expected SOR_TIMEOUT after reject walk, got %v", err)
	}
}

// TestConcurrentRouteGuard — the parent already holding a live shadow
// order cannot route again (no concurrent local+external order).
func TestConcurrentRouteGuard(t *testing.T) {
	v := NewLoopbackConnector("ecna", "EXCH", "ECNA")
	defer v.Close()
	v.AckDelay = 5 * time.Millisecond
	store := NewMemoryShadowStore()
	r, _ := newRouter(store, v)
	defer r.Stop()

	res, err := r.Route(context.Background(), parent(), thinBook())
	if err != nil {
		t.Fatal(err)
	}
	_ = res
	// Second route for the same parent while shadow is live → rejected.
	_, err = r.Route(context.Background(), parent(), thinBook())
	if err == nil || excerrors.CodeOf(err) != "ROUTING_REJECTED" {
		t.Fatalf("expected ROUTING_REJECTED, got %v", err)
	}
}

// TestReleaseForLocal — external cancel completes before the parent is
// freed for local submission.
func TestReleaseForLocal(t *testing.T) {
	v := NewLoopbackConnector("ecna", "EXCH", "ECNA")
	defer v.Close()
	store := NewMemoryShadowStore()
	r, _ := newRouter(store, v)
	defer r.Stop()

	if _, err := r.Route(context.Background(), parent(), thinBook()); err != nil {
		t.Fatal(err)
	}
	ok, err := r.ReleaseForLocal(context.Background(), 100)
	if err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
	open, _ := store.OpenByParent(context.Background(), 100)
	if open != nil {
		t.Fatalf("shadow still open: %+v", open)
	}
	// Now a re-route is permitted (shadow terminal).
	if _, err := r.Route(context.Background(), parent(), thinBook()); err != nil {
		t.Fatalf("re-route after release: %v", err)
	}
}

// TestFillDedup — a replayed exec report (same exec id) must not
// double-apply the fill.
func TestFillDedup(t *testing.T) {
	store := NewMemoryShadowStore()
	r, pubs := newRouter(store)
	defer r.Stop()
	ctx := context.Background()
	sh, _ := store.CreateShadow(ctx, &ShadowOrder{
		ParentOrderID: 1, AccountID: 7, VenueID: "ecna",
		ExternalOrderID: "E1", Symbol: "EURUSD", Side: "BUY",
		Qty: d("100"), State: StateRouted,
	})
	ev := VenueEvent{
		VenueID: "ecna", ExternalOrderID: "E1", ExecID: "x1",
		Kind: VenueFill, Qty: d("50"), Price: d("1.10"), CumQty: d("50"),
		LeavesQty: d("50"), At: time.Now(),
	}
	r.onVenueEvent(ctx, "ecna", ev)
	r.onVenueEvent(ctx, "ecna", ev) // replay
	got, _ := store.ShadowByExternalID(ctx, "ecna", "E1")
	if got.FilledQty.String() != "50" {
		t.Fatalf("dedup failed: filled=%s", got.FilledQty)
	}
	if got.State != StatePartiallyFilled {
		t.Fatalf("state %s", got.State)
	}
	if got := pubs.all(); len(got) != 1 {
		t.Fatalf("dedup published twice: %d", len(got))
	}
	_ = sh
}

// TestLoadVenueConfig — env-blocked production wiring seam.
func TestLoadVenueConfig(t *testing.T) {
	specs, err := LoadVenueConfig("")
	if err != nil || specs != nil {
		t.Fatal("empty env must yield no venues")
	}
	specs, err = LoadVenueConfig("ecna:10.0.0.1:5001, ecnb:10.0.0.2:5001")
	if err != nil || len(specs) != 2 || specs[0].ID != "ecna" || specs[1].Port != 5001 {
		t.Fatalf("parse: %+v %v", specs, err)
	}
	if _, err := LoadVenueConfig("bad"); err == nil {
		t.Fatal("malformed venue spec accepted")
	}
	if _, err := LoadVenueConfig("x:h:0"); err == nil {
		t.Fatal("bad port accepted")
	}
}
