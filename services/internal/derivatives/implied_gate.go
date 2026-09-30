// Phase-22 Task 22.3.12 — Multi-Leg Implied Matching: feature gate.
//
// The C++ implied-liquidity coordinator lives in
// core/src/matching/ImpliedMatcher.cpp (implied-in swap quotes from
// outright legs, implied-out outright quotes from swap+near legs,
// spec §6.3/§24). This file is the control-plane admission check the
// task requires: a deterministic, fail-closed gate over the
// feature_flags "implied_matching" row (migration 193 schema).
//
// Contract (spec §2.7):
//   - nil resolver, resolver error, unknown flag  → NOT admitted;
//   - Admit maps those to coded errors (SERVICE_DEGRADED when the flag
//     cannot be resolved, IMPLIED_MATCHING_UNAVAILABLE when it resolves
//     to off) — an unverifiable gate never silently enables implied
//     liquidity;
//   - Enabled is the non-coding probe for read-model/telemetry surfaces
//     and reads every failure as "off".
//
// The resolver seam is *flags.Store-shaped (Eval over
// flags.EvalContext) so the production store plugs in unmodified; tests
// substitute a stub. Nothing here mutates flag state.
package derivatives

import (
	"context"

	"exchange/internal/flags"
	excerrors "exchange/pkg/errors"
)

// ImpliedMatchingFlag is the feature_flags.name row governing implied
// liquidity (Task 22.3.12). The name is fixed — migration 193's nameRe
// owns the charset.
const ImpliedMatchingFlag = "implied_matching"

// Coded-admission constants (registered in internal/errs localCodes by
// this phase).
const (
	CodeImpliedMatchingUnavailable = "IMPLIED_MATCHING_UNAVAILABLE" // 503
	CodeServiceDegraded            = "SERVICE_DEGRADED"             // 503
)

// FlagResolver is the narrow flags.Store evaluation seam —
// (*flags.Store).Eval satisfies it.
type FlagResolver interface {
	Eval(ctx context.Context, name string, c flags.EvalContext) (bool, error)
}

// ImpliedGate admits implied-liquidity operations only while the
// feature flag resolves ON for the caller. Construct once per service
// boundary; the resolver is consulted per call (no caching — flag
// flips must take effect immediately, deterministically).
type ImpliedGate struct {
	resolver FlagResolver
}

// NewImpliedGate wires the gate. A nil resolver is legal and means
// "implied matching can never be admitted" — fail closed.
func NewImpliedGate(r FlagResolver) *ImpliedGate {
	return &ImpliedGate{resolver: r}
}

// eval resolves the flag once. ok=false distinguishes "flag off" from
// "flag unresolvable" (err set) so Admit can map them differently.
func (g *ImpliedGate) eval(ctx context.Context, c flags.EvalContext) (on bool, err error) {
	if g == nil || g.resolver == nil {
		return false, errImpliedGateNil
	}
	return g.resolver.Eval(ctx, ImpliedMatchingFlag, c)
}

// Enabled is the fail-closed probe: any resolver failure or unknown
// flag reads as off (mirrors flags.Store.Enabled semantics — a
// half-configured feature must not leak on).
func (g *ImpliedGate) Enabled(ctx context.Context, accountID int64,
	tier, subject string) bool {
	on, err := g.eval(ctx, flags.EvalContext{
		AccountID: accountID, Tier: tier, Subject: subject})
	return err == nil && on
}

// Admit is the admission check for implied-liquidity operations (curve
// link enablement, implied-pricing request surfaces, the engine-side
// enable signal). Off ⇒ IMPLIED_MATCHING_UNAVAILABLE; unresolvable ⇒
// SERVICE_DEGRADED — neither silently enables.
func (g *ImpliedGate) Admit(ctx context.Context, accountID int64,
	tier, subject string) error {
	on, err := g.eval(ctx, flags.EvalContext{
		AccountID: accountID, Tier: tier, Subject: subject})
	if err != nil {
		return excerrors.New(CodeServiceDegraded,
			"implied_matching flag unresolvable — admission fails closed")
	}
	if !on {
		return excerrors.New(CodeImpliedMatchingUnavailable,
			"implied matching is not enabled for this account/tier")
	}
	return nil
}

// errImpliedGateNil is the internal marker for a nil gate/resolver —
// Admit maps it to SERVICE_DEGRADED like any resolver failure.
var errImpliedGateNil = excerrors.New(CodeServiceDegraded,
	"implied-matching gate unconfigured")
