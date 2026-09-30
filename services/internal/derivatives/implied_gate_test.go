// implied_gate_test.go — the Task 22.3.12 implied-matching admission
// matrix: nil resolver, resolver error, flag off, flag on; Admit codes
// and the non-coding Enabled probe. No PG.
package derivatives

import (
	"context"
	"errors"
	"testing"

	"exchange/internal/flags"
)

type stubFlagResolver struct {
	on   bool
	err  error
	gotN string
}

func (s *stubFlagResolver) Eval(_ context.Context, name string,
	_ flags.EvalContext) (bool, error) {
	s.gotN = name
	return s.on, s.err
}

func TestImpliedGateAdmitMatrix(t *testing.T) {
	ctx := context.Background()

	// nil resolver → fail closed as SERVICE_DEGRADED (the gate cannot
	// verify the flag, so nothing is admitted).
	g := NewImpliedGate(nil)
	assertCode(t, g.Admit(ctx, 7, "basic", "subj"),
		CodeServiceDegraded, "nil resolver admit")
	if g.Enabled(ctx, 7, "basic", "subj") {
		t.Fatal("nil resolver Enabled must read off")
	}
	// nil gate pointer — same fail-closed read.
	var nilGate *ImpliedGate
	assertCode(t, nilGate.Admit(ctx, 7, "basic", "subj"),
		CodeServiceDegraded, "nil gate admit")
	if nilGate.Enabled(ctx, 7, "basic", "subj") {
		t.Fatal("nil gate Enabled must read off")
	}

	// Resolver error → Admit SERVICE_DEGRADED; Enabled off.
	bad := &stubFlagResolver{err: errors.New("flags store down")}
	gb := NewImpliedGate(bad)
	assertCode(t, gb.Admit(ctx, 7, "basic", "subj"),
		CodeServiceDegraded, "resolver error admit")
	if gb.Enabled(ctx, 7, "basic", "subj") {
		t.Fatal("resolver-error Enabled must read off")
	}
	if bad.gotN != ImpliedMatchingFlag {
		t.Fatalf("resolver consulted flag %q want %q", bad.gotN, ImpliedMatchingFlag)
	}

	// Flag off → Admit IMPLIED_MATCHING_UNAVAILABLE; Enabled off.
	off := &stubFlagResolver{on: false}
	gf := NewImpliedGate(off)
	assertCode(t, gf.Admit(ctx, 7, "basic", "subj"),
		CodeImpliedMatchingUnavailable, "flag off admit")
	if gf.Enabled(ctx, 7, "basic", "subj") {
		t.Fatal("flag-off Enabled must read off")
	}

	// Flag on → both admit.
	on := &stubFlagResolver{on: true}
	gn := NewImpliedGate(on)
	if err := gn.Admit(ctx, 7, "basic", "subj"); err != nil {
		t.Fatalf("flag on Admit: %v", err)
	}
	if !gn.Enabled(ctx, 7, "basic", "subj") {
		t.Fatal("flag on Enabled must read on")
	}
}
