// Phase-14 Task 14.3.7 — the appropriateness admission seam. These
// tests pin the service-side contract: the gate is consulted on every
// new-order admission path AFTER the breaker chain, its coded rejection
// propagates verbatim, a nil seam fails closed, and reduce_only orders
// bypass it entirely (the close-only posture spec §5.2 requires after
// a downgrade or expired assessment).
package orders

import (
	"context"
	"errors"
	"testing"
	"time"

	"exchange/internal/config"
	excerrors "exchange/pkg/errors"
)

// denyProduct rejects every gated class with the registry code the real
// gate emits (PRODUCT_NOT_PERMITTED).
type denyProduct struct{ calls int }

func (d *denyProduct) Appropriateness(context.Context, int64, string) error {
	d.calls++
	return excerrors.New("PRODUCT_NOT_PERMITTED", "appropriateness assessment required")
}

// errProduct simulates a checker-side outage — the verdict is
// unverifiable, admission must still fail closed.
type errProduct struct{}

func (errProduct) Appropriateness(context.Context, int64, string) error {
	return errors.New("category store unavailable")
}

func newSvcWithProduct(t *testing.T, st *fakeStore, gate AppropriatenessGate) *Service {
	t.Helper()
	shards, err := config.LoadShardMap("")
	if err != nil {
		t.Fatalf("shard map: %v", err)
	}
	svc, err := NewService(Options{
		Store: st, Submitter: &fakeSubmitter{}, ShardMap: shards,
		KillSwitch: openKill{}, Breakers: openBreakers{},
		Product:    gate,
		AckTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestProductGate_RejectionPropagates(t *testing.T) {
	st := newFakeStore()
	gate := &denyProduct{}
	svc := newSvcWithProduct(t, st, gate)
	_, err := svc.Submit(context.Background(), st.acct, submitReq())
	if codeOf(t, err) != "PRODUCT_NOT_PERMITTED" {
		t.Fatalf("gate rejection must propagate verbatim, got %v", err)
	}
	if gate.calls != 1 {
		t.Fatalf("gate consulted exactly once, got %d", gate.calls)
	}
}

func TestProductGate_NilSeamFailsClosed(t *testing.T) {
	st := newFakeStore()
	svc := newSvcWithProduct(t, st, nil)
	_, err := svc.Submit(context.Background(), st.acct, submitReq())
	if codeOf(t, err) != "SERVICE_DEGRADED" {
		t.Fatalf("nil product gate must degrade closed, got %v", err)
	}
}

func TestProductGate_CheckerErrorFailsClosed(t *testing.T) {
	st := newFakeStore()
	svc := newSvcWithProduct(t, st, errProduct{})
	if _, err := svc.Submit(context.Background(), st.acct, submitReq()); err == nil {
		t.Fatal("checker error must not admit")
	}
}

func TestProductGate_ReduceOnlyBypasses(t *testing.T) {
	st := newFakeStore()
	gate := &denyProduct{}
	svc := newSvcWithProduct(t, st, gate)
	req := submitReq()
	req.ReduceOnly = true
	// The admission gate is what this test isolates — a
	// PRODUCT_NOT_PERMITTED here means the bypass broke. (Other
	// downstream verdicts are fine: reduce_only also skips the balance
	// check, so submit may legitimately fail later in the pipeline.)
	if _, err := svc.Submit(context.Background(), st.acct, req); err != nil {
		if codeOf(t, err) == "PRODUCT_NOT_PERMITTED" {
			t.Fatalf("reduce_only must bypass the appropriateness gate: %v", err)
		}
	}
	if gate.calls != 0 {
		t.Fatalf("reduce_only must never consult the gate, got %d calls", gate.calls)
	}
}

func TestProductGate_ModifyConsultsOrderFlag(t *testing.T) {
	// ModifyRequest carries no reduce_only — the stored order's flag
	// governs. A normal order's amend is gated; a reduce_only order's
	// amend is exempt (still close-only).
	st := newFakeStore()
	gate := &denyProduct{}
	svc := newSvcWithProduct(t, st, gate)

	st.seed(openOrder()) // ID 42, OrderSeq 5, ReduceOnly=false
	_, err := svc.Modify(context.Background(), st.acct, 42,
		&ModifyRequest{Price: d("1.06"), OrderSeq: u64p(5)}, "u", "r1", "127.0.0.1")
	if codeOf(t, err) != "PRODUCT_NOT_PERMITTED" {
		t.Fatalf("non-reduce amend must be gated, got %v", err)
	}

	ro := openOrder()
	ro.ID = 43
	ro.ReduceOnly = true
	st.seed(ro)
	if _, err := svc.Modify(context.Background(), st.acct, 43,
		&ModifyRequest{Price: d("1.06"), OrderSeq: u64p(5)}, "u", "r1", "127.0.0.1"); err != nil {
		if codeOf(t, err) == "PRODUCT_NOT_PERMITTED" {
			t.Fatalf("reduce_only amend must bypass the gate: %v", err)
		}
	}
	if gate.calls != 1 {
		t.Fatalf("only the non-reduce amend consults the gate, got %d calls", gate.calls)
	}
}
