// Task 6.3.22 item 3 — symbol/channel entitlement enforcement
// (spec §10.7; mirrors the FIX SESSION_NOT_ENTITLED model →
// WS ENTITLEMENT_REQUIRED per §23 registry remediation #24).
//
// Entitlement tables are a deployment artifact (Phase-07 backoffice owns
// the source of truth; this package consumes a checker seam). A nil
// checker admits every bind — public FX market data is open by default;
// deployments that gate per-symbol or per-account access wire a concrete
// EntitlementChecker through Config.Entitlements and denials surface
// ENTITLEMENT_REQUIRED at subscribe, resume and resync (one gate,
// gateChannel).
package marketdata

import "exchange/internal/ws"

// EntitlementChecker vets one channel bind for one session. ok=false
// rejects the bind with ENTITLEMENT_REQUIRED; reason is wire-safe (it is
// rendered into the error frame verbatim) and may be empty — the gate
// substitutes a generic denial message.
//
// Implementations must be non-blocking and side-effect free: the gate
// runs on the subscribe hot path, once per channel in the frame, and is
// called again on resume/resync — keep it cheap.
type EntitlementChecker interface {
	Check(sess *ws.Session, ch Channel) (ok bool, reason string)
}

// EntitlementFunc adapts a function to EntitlementChecker.
type EntitlementFunc func(sess *ws.Session, ch Channel) (bool, string)

// Check implements EntitlementChecker.
func (f EntitlementFunc) Check(sess *ws.Session, ch Channel) (bool, string) {
	return f(sess, ch)
}

// StaticEntitlements is the table-driven EntitlementChecker for
// deployments whose entitlement set is loaded at boot (or refreshed by
// swapping the maps under Reload). All fields are optional; a channel
// that matches no rule is admitted — entitlement is deny-list/allow-list
// driven, never implicit.
type StaticEntitlements struct {
	// DenyChannels denies verbatim channel tokens ("depth@USD/TRY:20:100",
	// "private:executions") to every session.
	DenyChannels map[string]bool
	// DenySymbols denies every channel whose Target is the symbol —
	// e.g. geo-restricted or suspended instruments.
	DenySymbols map[string]bool
	// AllowAccountSymbols maps accountID → the symbol set that account
	// may bind (per-account entitlement tiers). A present account entry
	// is an allow-list: symbols absent from it are denied. Accounts with
	// no entry are unrestricted — the table gates only what it lists.
	AllowAccountSymbols map[int64]map[string]bool
	// DenyPrivate denies private:* binds for accounts in the set —
	// the kill-switch shape (entitlement revoked, stream refuses binds;
	// already-bound conns are torn down separately by the operator).
	DenyPrivate map[int64]bool
}

// Check implements EntitlementChecker.
func (e *StaticEntitlements) Check(sess *ws.Session, ch Channel) (bool, string) {
	if e == nil {
		return true, ""
	}
	if e.DenyChannels[ch.Raw] {
		return false, "channel not entitled"
	}
	if ch.Private {
		if sess != nil && e.DenyPrivate[sess.AccountID] {
			return false, "private stream access revoked"
		}
		return true, ""
	}
	if e.DenySymbols[ch.Target] {
		return false, "symbol " + ch.Target + " not entitled"
	}
	if len(e.AllowAccountSymbols) > 0 && sess != nil && sess.Authenticated {
		if allow, listed := e.AllowAccountSymbols[sess.AccountID]; listed &&
			!allow[ch.Target] {
			return false, "symbol " + ch.Target + " outside account entitlements"
		}
	}
	return true, ""
}
