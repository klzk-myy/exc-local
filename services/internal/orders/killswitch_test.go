// Task 11.3.4/11.3.8/11.3.12 — service-level kill-switch admission tests:
// the orders.Service seam (not just the HTTP gate) rejects new orders on
// every entry path while cancels stay exempt.
package orders

import (
	"context"
	"errors"
	"testing"
	"time"

	"exchange/internal/config"
)

// haltedKill suspends every admission (scope GLOBAL).
type haltedKill struct{ scope, detail string }

func (h haltedKill) OrderHalt(context.Context, int64, string, string, string) (string, string, error) {
	return h.scope, h.detail, nil
}

// errKill simulates a flag-store outage — the seam fails closed.
type errKill struct{}

func (errKill) OrderHalt(context.Context, int64, string, string, string) (string, string, error) {
	return "", "", errors.New("redis down")
}

func newSvcWithKill(t *testing.T, st *fakeStore, kill KillSwitch) *Service {
	t.Helper()
	shards, err := config.LoadShardMap("")
	if err != nil {
		t.Fatalf("shard map: %v", err)
	}
	svc, err := NewService(Options{
		Store: st, Submitter: &fakeSubmitter{}, ShardMap: shards,
		KillSwitch: kill, Breakers: openBreakers{}, Product: openProduct{},
		AckTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestKillSwitch_SubmitRejectedWhenSuspended(t *testing.T) {
	st := newFakeStore()
	svc := newSvcWithKill(t, st, haltedKill{scope: "GLOBAL", detail: "GLOBAL halt"})
	_, err := svc.Submit(context.Background(), st.acct, submitReq())
	if codeOf(t, err) != "TRADING_HALTED" {
		t.Fatalf("suspended admission must reject TRADING_HALTED, got %v", err)
	}
	// Scoped (per-account) suspension carries the scope in the detail.
	st2 := newFakeStore()
	svc2 := newSvcWithKill(t, st2, haltedKill{scope: "ACCOUNT", detail: "ACCOUNT[7] halt"})
	_, err = svc2.Submit(context.Background(), st2.acct, submitReq())
	if codeOf(t, err) != "TRADING_HALTED" {
		t.Fatalf("scoped suspension must reject TRADING_HALTED, got %v", err)
	}
}

func TestKillSwitch_SubmitFailsClosed(t *testing.T) {
	st := newFakeStore()
	svc := newSvcWithKill(t, st, errKill{})
	_, err := svc.Submit(context.Background(), st.acct, submitReq())
	if codeOf(t, err) != "TRADING_HALTED" {
		t.Fatalf("resolver error must fail closed, got %v", err)
	}

	// Nil seam: a service without a wired resolver must not admit.
	st2 := newFakeStore()
	svc2 := newSvcWithKill(t, st2, nil)
	_, err = svc2.Submit(context.Background(), st2.acct, submitReq())
	if codeOf(t, err) != "TRADING_HALTED" {
		t.Fatalf("nil kill-switch seam must fail closed, got %v", err)
	}
}

func TestKillSwitch_CancelSurvivesHalt(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub) // openKill — lets the order reach ACTIVE
	ctx := context.Background()

	ack, err := svc.Submit(ctx, st.acct, submitReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Swap to the suspended seam — cancels must still flow (the
	// service-level cancel path never consults the kill switch).
	svc.kill = haltedKill{scope: "GLOBAL", detail: "GLOBAL halt"}
	cack, err := svc.Cancel(ctx, st.acct, ack.OrderID, "tester", "r1", "127.0.0.1")
	if err != nil {
		t.Fatalf("cancel under halt must succeed: %v", err)
	}
	if cack.Status != "CANCELLED" {
		t.Fatalf("cancel ack: %+v", cack)
	}
}
