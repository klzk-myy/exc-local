// Unit tests for the Task 11.3.12 SCOPE_RAIL gate (rail_control.go):
// suspended rail refuses with SETTLEMENT_RAIL_REJECTED, lookup errors
// fail closed, nil seam preserves the legacy path.
package funding

import (
	"context"
	"errors"
	"testing"
)

type fakeRailCtl struct {
	suspended bool
	reason    string
	err       error
}

func (f fakeRailCtl) RailSuspended(context.Context, string) (bool, string, error) {
	return f.suspended, f.reason, f.err
}

func TestAssertRailOperational(t *testing.T) {
	ctx := context.Background()

	// Nil seam — unwired callers keep the legacy path (the dispatch-level
	// gate on RailService still applies when wired).
	if err := AssertRailOperational(ctx, nil, "SEPA"); err != nil {
		t.Fatalf("nil controller must pass: %v", err)
	}
	// Empty rail (no method supplied) — nothing to gate.
	if err := AssertRailOperational(ctx, fakeRailCtl{suspended: true}, ""); err != nil {
		t.Fatalf("empty rail must pass: %v", err)
	}
	// Suspended → SETTLEMENT_RAIL_REJECTED with the operator reason.
	err := AssertRailOperational(ctx,
		fakeRailCtl{suspended: true, reason: "SEPA scheme outage"}, "sepa")
	if codeOf(err) != "SETTLEMENT_RAIL_REJECTED" {
		t.Fatalf("suspended rail: %v", err)
	}
	// Open rail passes.
	if err := AssertRailOperational(ctx, fakeRailCtl{}, "SWIFT"); err != nil {
		t.Fatalf("open rail must pass: %v", err)
	}
	// Lookup error fails closed (INTERNAL_ERROR, not silent pass).
	err = AssertRailOperational(ctx, fakeRailCtl{err: errors.New("redis down")}, "SEPA")
	if codeOf(err) != "INTERNAL_ERROR" {
		t.Fatalf("lookup error must fail closed, got %v", err)
	}
}
