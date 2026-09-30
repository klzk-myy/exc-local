package oracle

import (
	"sort"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Mark / index computation (Task 19.5.3.2)
//
// mark  = median of ≥2 fresh, non-divergent feed mids
// index = volume/liquidity-weighted average of the same cohort
//
// Both are computed over the post-staleness, post-divergence cohort —
// stale or outlier feeds never leak into a published price.
// ---------------------------------------------------------------------------

// cohort is the fresh-quote set for one symbol after filtering.
type cohort struct {
	quotes []Quote
}

// median of mids — for an even cohort the midpoint of the two central
// values; for odd, the central value. Sorted copy, inputs untouched.
func (c cohort) median() decimal.Decimal {
	mids := make([]decimal.Decimal, len(c.quotes))
	for i, q := range c.quotes {
		mids[i] = q.Mid
	}
	sort.Slice(mids, func(i, j int) bool { return mids[i].LessThan(mids[j]) })
	n := len(mids)
	if n%2 == 1 {
		return mids[n/2]
	}
	return mids[n/2-1].Add(mids[n/2]).Div(decimal.NewFromInt(2))
}

// vwap — Σ(mid×weight)/Σweight; zero-weight feeds count as 1.
func (c cohort) vwap() decimal.Decimal {
	num, den := decimal.Zero, decimal.Zero
	for _, q := range c.quotes {
		w := q.Weight
		if !w.IsPositive() {
			w = decimal.NewFromInt(1)
		}
		num = num.Add(q.Mid.Mul(w))
		den = den.Add(w)
	}
	if !den.IsPositive() {
		return decimal.Zero
	}
	return num.Div(den)
}

// excludeDivergent drops quotes whose mid diverges from the cohort
// median by more than bps basis points. It iterates because dropping an
// outlier can shift the median — bounded at len-1 passes, deterministic
// and tiny (feed count ≤ ~5). Returns survivors + excluded feeds.
func excludeDivergent(qs []Quote, bps int64) (kept []Quote, dropped []string) {
	kept = append([]Quote(nil), qs...)
	for {
		if len(kept) < MinFeeds {
			return kept, dropped
		}
		med := cohort{quotes: kept}.median()
		// divergence in bps = |q - med| / med * 10000
		worst, worstBps := -1, int64(-1)
		for i, q := range kept {
			if !med.IsPositive() {
				return kept, dropped
			}
			dev := q.Mid.Sub(med).Abs().Div(med).Mul(decimal.NewFromInt(10000))
			b := dev.IntPart()
			if b > worstBps {
				worst, worstBps = i, b
			}
		}
		if worstBps <= bps {
			return kept, dropped
		}
		dropped = append(dropped, kept[worst].Feed)
		kept = append(kept[:worst], kept[worst+1:]...)
	}
}

// MarkResult carries the computed prices plus the audit trail for the
// evaluation round (stale feeds excluded, divergent feeds excluded).
type MarkResult struct {
	Symbol    string
	Mark      decimal.Decimal
	Index     decimal.Decimal
	Fresh     []string // feed names contributing
	Stale     []string // feed names excluded (> staleness gate)
	Divergent []string // feed names excluded (> divergence threshold)
	At        time.Time
	OK        bool // false → fewer than MinFeeds fresh post-filter quotes
}
