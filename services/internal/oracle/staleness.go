package oracle

import (
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Staleness gates (Task 19.5.3.3) — the 5-second fail-closed ladder.
//
// A quote is fresh while now − quote.Ts ≤ StaleAfter (5s). Stale quotes
// are excluded from mark computation entirely; a symbol whose fresh
// cohort drops below MinFeeds (2) fails closed — the service publishes
// UNAVAILABLE health and consumers halt margin-increasing flow
// (PRICE_ORACLE_UNAVAILABLE) rather than trading on a fabricated mark.
// ---------------------------------------------------------------------------

// StalenessTier classifies how stale a symbol's freshest quote is —
// the liquidation-fallback ladder's rungs (Task 19.5.3.6 step 2).
type StalenessTier int

const (
	// TierFresh — inside the 5s gate; normal marks.
	TierFresh StalenessTier = iota
	// TierStaleShort — 5–15s: last mark ±2% haircut, liquidation only.
	TierStaleShort
	// TierStaleMedium — 15–60s: ±5% haircut, auction-only liquidation.
	TierStaleMedium
	// TierStaleLong — >60s: FORCE_CASH at last mark ±10% + P0 alert.
	TierStaleLong
)

// TierStaleness maps a staleness duration to its ladder rung.
func TierStaleness(age time.Duration) StalenessTier {
	switch {
	case age <= StaleAfter:
		return TierFresh
	case age <= 15*time.Second:
		return TierStaleShort
	case age <= 60*time.Second:
		return TierStaleMedium
	default:
		return TierStaleLong
	}
}

// HaircutPct is the tier's liquidation haircut: 2% / 5% / 10% (spec
// Task 19.5.3.6). TierFresh reports 0 — the ladder never applies.
func (t StalenessTier) HaircutPct() decimal.Decimal {
	switch t {
	case TierStaleShort:
		return decimal.RequireFromString("0.02")
	case TierStaleMedium:
		return decimal.RequireFromString("0.05")
	case TierStaleLong:
		return decimal.RequireFromString("0.10")
	default:
		return decimal.Zero
	}
}

// AuctionOnly reports whether the tier restricts liquidation closes to
// the auction ladder (≥15s stale per the task's tiered response).
func (t StalenessTier) AuctionOnly() bool { return t >= TierStaleMedium }

// ForceCash reports whether the tier forces immediate cash settlement
// at the widened band (>60s stale — §13.4 FORCE_CASH path).
func (t StalenessTier) ForceCash() bool { return t == TierStaleLong }

// filterFresh splits quotes at the 5s staleness gate — stale entries
// are never eligible for the mark cohort.
func filterFresh(qs []Quote, now time.Time, staleAfter time.Duration) (fresh, stale []Quote) {
	for _, q := range qs {
		if q.Ts.IsZero() || now.Sub(q.Ts) > staleAfter {
			stale = append(stale, q)
			continue
		}
		fresh = append(fresh, q)
	}
	return fresh, stale
}

// staleSeconds returns the age of the freshest quote in the cohort
// (−1 when the cohort is empty — no observation at all).
func staleSeconds(qs []Quote, now time.Time) float64 {
	var best time.Time
	for _, q := range qs {
		if q.Ts.After(best) {
			best = q.Ts
		}
	}
	if best.IsZero() {
		return -1
	}
	return now.Sub(best).Seconds()
}
