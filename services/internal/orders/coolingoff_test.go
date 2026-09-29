// Cooling-off admission gate — Phase-14 Tasks 14.3.11/14.3.12.
//
// The gate rejects leveraged order entry with COOLING_OFF_ACTIVE while
// a live self-exclusion window covers the account owner. Scope tests:
// leveraged-only consultation (SPOT accounts and non-marginable
// instruments stay open), reduce_only bypass (the activation saga
// dispatches its own closes), nil-seam skip, and fail-closed on gate
// errors — plus coverage of the modify + batch entry paths.
package orders

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"exchange/internal/config"

	excerrors "exchange/pkg/errors"
)

// coolingFake counts gate consultations and replays a canned verdict.
type coolingFake struct {
	calls int
	err   error
}

func (f *coolingFake) AssertLeverageEntryAllowed(_ context.Context, _ int64) error {
	f.calls++
	return f.err
}

func marginAcct() *Account {
	return &Account{ID: 7, Type: "MARGIN", KycTier: "T1", Status: "ACTIVE"}
}

// Leveraged entry on a covered account rejects with the gate's own
// coded error — the user's order never reaches the book.
func TestCoolingOffRejectsLeveragedEntry(t *testing.T) {
	st := newFakeStore()
	st.acct = marginAcct()
	gate := &coolingFake{err: excerrors.New("COOLING_OFF_ACTIVE",
		"self-exclusion active")}
	shards, err := config.LoadShardMap("")
	if err != nil {
		t.Fatalf("shard map: %v", err)
	}
	svc, err := NewService(Options{
		Store: st, Submitter: &fakeSubmitter{}, ShardMap: shards,
		KillSwitch: openKill{}, Breakers: openBreakers{},
		Product: openProduct{}, CoolingOff: gate,
		AckTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.Submit(context.Background(), st.acct, submitReq()); err == nil ||
		codeOf(t, err) != "COOLING_OFF_ACTIVE" {
		t.Fatalf("got %v, want COOLING_OFF_ACTIVE", err)
	}
	if gate.calls != 1 {
		t.Fatalf("gate consulted %d times, want 1", gate.calls)
	}
	if len(st.orders) != 0 {
		t.Fatalf("rejected order persisted: %d rows", len(st.orders))
	}
}

// A gate lookup failure fails closed — leveraged entry is rejected
// rather than silently admitted (spec §2.7).
func TestCoolingOffGateErrorFailsClosed(t *testing.T) {
	st := newFakeStore()
	st.acct = marginAcct()
	gate := &coolingFake{err: excerrors.New("SERVICE_DEGRADED",
		"cooling-off state unverifiable")}
	svc := newSvc(t, st, &fakeSubmitter{})
	svc.WithCoolingOff(gate)
	if _, err := svc.Submit(context.Background(), st.acct, submitReq()); err == nil ||
		codeOf(t, err) != "SERVICE_DEGRADED" {
		t.Fatalf("got %v, want SERVICE_DEGRADED", err)
	}
	if gate.calls != 1 {
		t.Fatalf("gate consulted %d times, want 1", gate.calls)
	}
}

// Scope: the gate is consulted only for leveraged entry — SPOT accounts
// and non-marginable instruments never touch it, and reduce_only legs
// bypass it (the activation saga itself closes through Submit).
func TestCoolingOffGateScope(t *testing.T) {
	newGateSvc := func(t *testing.T, st *fakeStore) (*Service, *coolingFake) {
		t.Helper()
		gate := &coolingFake{err: excerrors.New("COOLING_OFF_ACTIVE", "x")}
		svc := newSvc(t, st, &fakeSubmitter{})
		svc.WithCoolingOff(gate)
		return svc, gate
	}
	ctx := context.Background()

	// SPOT account + levered instrument → gate skipped (spot
	// conversions stay available by spec).
	st := newFakeStore() // testAcct() is SPOT
	svc, gate := newGateSvc(t, st)
	if _, err := svc.Submit(ctx, st.acct, submitReq()); err != nil {
		t.Fatalf("spot submit: %v", err)
	}
	if gate.calls != 0 {
		t.Fatalf("SPOT account consulted gate %d times", gate.calls)
	}

	// MARGIN account + zero-leverage instrument → gate skipped.
	st = newFakeStore()
	st.acct = marginAcct()
	st.inst.MaxLeverage = 0
	svc, gate = newGateSvc(t, st)
	if _, err := svc.Submit(ctx, st.acct, submitReq()); err != nil {
		t.Fatalf("non-levered submit: %v", err)
	}
	if gate.calls != 0 {
		t.Fatalf("non-levered instrument consulted gate %d times", gate.calls)
	}

	// reduce_only on a leveraged instrument → gate skipped (the close
	// path must never deadlock on its own flag).
	st = newFakeStore()
	st.acct = marginAcct()
	svc, gate = newGateSvc(t, st)
	req := submitReq()
	req.ReduceOnly = true
	if _, err := svc.Submit(ctx, st.acct, req); err != nil {
		t.Fatalf("reduce-only submit: %v", err)
	}
	if gate.calls != 0 {
		t.Fatalf("reduce-only consulted gate %d times", gate.calls)
	}

	// Nil gate (unwired) → admission proceeds (constructor seam is
	// optional per Options.CoolingOff contract).
	st = newFakeStore()
	st.acct = marginAcct()
	svc = newSvc(t, st, &fakeSubmitter{})
	if _, err := svc.Submit(ctx, st.acct, submitReq()); err != nil {
		t.Fatalf("unwired submit: %v", err)
	}
}

// The gate also fires on the batch entry path — one rejected entry
// must not silently admit; batch results surface per-entry codes.
func TestCoolingOffBatchEntry(t *testing.T) {
	st := newFakeStore()
	st.acct = marginAcct()
	gate := &coolingFake{err: excerrors.New("COOLING_OFF_ACTIVE", "x")}
	svc := newSvc(t, st, &fakeSubmitter{})
	svc.WithCoolingOff(gate)
	_, err := svc.BatchSubmit(context.Background(), st.acct,
		[]*SubmitRequest{
			{Symbol: "EURUSD", Side: SideBuy, OrderType: TypeLimit,
				TimeInForce: TIFGTC, Quantity: d("1000"), Price: d("1.05"),
				ClientOrderID: "b1"},
			{Symbol: "EURUSD", Side: SideBuy, OrderType: TypeLimit,
				TimeInForce: TIFGTC, Quantity: d("1000"), Price: d("1.05"),
				ClientOrderID: "b2"},
		}, "u", "")
	var bf *BatchSubmitFailure
	if !stderrors.As(err, &bf) {
		t.Fatalf("batch submit: got %v, want BatchSubmitFailure", err)
	}
	res := bf.Results
	if len(res) != 2 {
		t.Fatalf("batch results=%d, want 2", len(res))
	}
	for i, r := range res {
		if r.Error != "COOLING_OFF_ACTIVE" {
			t.Fatalf("entry %d: got %q, want COOLING_OFF_ACTIVE", i, r.Error)
		}
	}
	if gate.calls != 2 {
		t.Fatalf("gate consulted %d times, want 2 (per entry)", gate.calls)
	}
	if len(st.orders) != 0 {
		t.Fatalf("rejected batch persisted: %d rows", len(st.orders))
	}
}

// Modify re-runs admission — a covered account cannot lift a resting
// order's price/qty while excluded.
func TestCoolingOffModifyEntry(t *testing.T) {
	st := newFakeStore()
	st.acct = marginAcct()
	gate := &coolingFake{err: nil}
	svc := newSvc(t, st, &fakeSubmitter{})
	svc.WithCoolingOff(gate)
	ctx := context.Background()

	ack, err := svc.Submit(ctx, st.acct, submitReq())
	if err != nil {
		t.Fatalf("seed submit: %v", err)
	}
	if gate.calls != 1 {
		t.Fatalf("submit consulted gate %d times", gate.calls)
	}
	// Window opens mid-flight → the amend rejects.
	gate.err = excerrors.New("COOLING_OFF_ACTIVE", "self-exclusion active")
	mr := &ModifyRequest{Price: d("1.06"), OrderSeq: u64p(ack.OrderSeq)}
	if _, err := svc.Modify(ctx, st.acct, ack.OrderID, mr, "u", "r1", ""); err == nil ||
		codeOf(t, err) != "COOLING_OFF_ACTIVE" {
		t.Fatalf("modify: got %v, want COOLING_OFF_ACTIVE", err)
	}
	// Cancel stays open — self-exclusion never traps resting orders.
	if _, err := svc.Cancel(ctx, st.acct, ack.OrderID, "u", "", ""); err != nil {
		t.Fatalf("cancel under cooling-off must stay open: %v", err)
	}
}

// A plain (non-coded) gate error also fails closed — uncoded seam
// failures must never pass through as admission.
func TestCoolingOffPlainErrorFailsClosed(t *testing.T) {
	st := newFakeStore()
	st.acct = marginAcct()
	gate := &coolingFake{err: stderrors.New("pg: connection refused")}
	svc := newSvc(t, st, &fakeSubmitter{})
	svc.WithCoolingOff(gate)
	if _, err := svc.Submit(context.Background(), st.acct, submitReq()); err == nil {
		t.Fatalf("plain gate error admitted — must fail closed")
	}
}
