// Phase-11 Task 11.3.12 — SCOPE_RAIL suspension consumption on the
// funding path. The admin kill-switch service raises halt:rail:{RAIL}
// (Redis key owned by internal/redis.HaltKey); this file defines the
// narrow read seam every funding flow calls before dispatching or
// ingesting on a rail.
//
// Fail closed (spec §2.7): a lookup error or nil checker binds to the
// strictest-safe answer for that rail only — the instruction is refused
// with SETTLEMENT_RAIL_REJECTED while sibling rails continue to serve.
// The outbound-dispatch gate lives on RailService (rails.go WithGate);
// this seam covers the withdrawal-create admission check and the
// inbound-wire deposit screen.
package funding

import (
	"context"
	"strings"
)

// RailController reports whether a banking rail is currently suspended
// by a scoped kill-switch (SCOPE_RAIL). *admin.KillSwitchResolver
// satisfies it in production; the RailService gate adapter covers the
// dispatch path. reason carries the operator's suspension note.
type RailController interface {
	RailSuspended(ctx context.Context, rail string) (suspended bool, reason string, err error)
}

// AssertRailOperational refuses an operation on a suspended rail with
// SETTLEMENT_RAIL_REJECTED (§23, registered by Task 11.3.11). A nil
// controller skips the check — composition roots must wire it; checker
// errors refuse the operation with INTERNAL_ERROR (fail closed — the
// same failure mode the RailService dispatch gate uses).
func AssertRailOperational(ctx context.Context, c RailController, rail string) error {
	if c == nil {
		return nil // seam not wired — dispatch-level gate still applies
	}
	r := strings.ToUpper(strings.TrimSpace(rail))
	if r == "" {
		return nil
	}
	suspended, reason, err := c.RailSuspended(ctx, r)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "rail suspension check", err)
	}
	if suspended {
		return errf("SETTLEMENT_RAIL_REJECTED",
			"rail %s suspended by kill-switch: %s", r, reason)
	}
	return nil
}
