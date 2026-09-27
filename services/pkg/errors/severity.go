// Severity hierarchy for the L0–L3 error tiers of spec §2.7.2, enforcing
// Strict Fail-Closed Zero-Loss Pessimism (spec §2.7.1) across all services.
// Lower numeric values are more severe: L0 faults halt the matching core
// immediately and page P0.
package errors

import stderrors "errors"

// Severity is the four-tier error classification of spec §2.7.2.
type Severity int

const (
	// SeverityL0 Critical/Fatal: WAL CRC failure, zero-sum ledger imbalance,
	// PTP clock skew >100µs, matching-loop panic — immediate core halt,
	// crash diagnostics + WAL flush, warm-standby failover, P0 pager alert.
	SeverityL0 Severity = iota
	// SeverityL1 Systemic/Infrastructure degradation: Sentinel failover,
	// IPC watermark breach, replica lag — degradation-mode transition
	// (ReadOnly/Throttled/Maintenance), circuit breaker, P1.
	SeverityL1
	// SeverityL2 Transaction/State boundary error: margin shortfall,
	// credit exhaustion, arithmetic overflow — synchronous atomic
	// rejection with zero side-effects and reservation rollback, P2.
	SeverityL2
	// SeverityL3 Edge/Protocol rejection: malformed JSON/SBE, HMAC
	// mismatch, replay, rate-limit breach — fast reject at the API/FIX
	// gateway edge before internal IPC, P3.
	SeverityL3
)

// String returns the canonical tier name ("L0".."L3"; "L?" when invalid).
func (s Severity) String() string {
	switch s {
	case SeverityL0:
		return "L0"
	case SeverityL1:
		return "L1"
	case SeverityL2:
		return "L2"
	case SeverityL3:
		return "L3"
	default:
		return "L?"
	}
}

// Priority maps a tier to its pager/alert priority (L0→P0 … L3→P3).
func (s Severity) Priority() string {
	switch s {
	case SeverityL0:
		return "P0"
	case SeverityL1:
		return "P1"
	case SeverityL2:
		return "P2"
	case SeverityL3:
		return "P3"
	default:
		return "P?"
	}
}

// Fatal reports whether the tier mandates an immediate core halt
// (spec §2.7.2: only L0).
func (s Severity) Fatal() bool { return s == SeverityL0 }

// Valid reports whether s is one of the four defined tiers.
func (s Severity) Valid() bool { return SeverityL0 <= s && s <= SeverityL3 }

// SeverityFor classifies an error code per spec §2.7.2. Unknown codes
// default to SeverityL2 (transaction boundary reject): fail-closed
// pessimism — an unclassified failure may reject a transaction but must
// never be silently downgraded to an edge case or escalated to a halt.
func SeverityFor(code string) Severity {
	switch code {
	case CodeTimeSyncLossHalt:
		// "PTP clock skew >100µs" is an explicit §2.7.2 L0 trigger.
		return SeverityL0
	case CodeArithmeticOverflowDetected, CodeOrderBookCapacityExceeded:
		// Spec §3.6.1–§3.6.2: atomic rejection of the order mutation.
		return SeverityL2
	default:
		return SeverityL2
	}
}

// SeverityOf resolves the tier of err: a *Error contributes its Code, any
// other error defaults like SeverityFor for an unknown code.
func SeverityOf(err error) Severity {
	var e *Error
	if stderrors.As(err, &e) {
		return SeverityFor(e.Code)
	}
	return SeverityL2
}
