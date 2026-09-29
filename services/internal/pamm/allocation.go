package pamm

import (
	"sort"

	"exchange/pkg/decimal"
)

// Share is one pro-rata weight: an allocation id (or follow id) plus its
// capital weight. IDs are the deterministic tiebreak.
type Share struct {
	ID     int64
	Weight decimal.Decimal // invested capital / allocation notional
}

// Allocation is one pro-rata result keyed by the share's ID.
type ShareAllocation struct {
	ID       int64
	Quantity decimal.Decimal
}

// quantum is the DECIMAL(28,8) fixed-point unit — the smallest assignable
// quantity slice (spec §5.3).
var quantum = decimal.NewFromScaled(1) // 1e-8

// AllocateProRata distributes total across shares in proportion to weight.
//
// Rounding rule (deterministic, documented, fixed-point only):
//  1. each raw share r_i = total × w_i / Σw is FLOORED to the 1e-8 quantum —
//     an investor can never receive more than its proportional share plus
//     one quantum;
//  2. the leftover (total − Σ floor_i, always an integral number of
//     quanta because all inputs are 8dp) is distributed ONE QUANTUM AT A
//     TIME to the shares with the largest fractional remainder
//     (r_i − floor_i), ties broken by ascending ID. Iterating sorted-by-
//     remainder order keeps the distribution stable across runs and
//     machines — map iteration is never used.
//
// Σ outputs == total exactly. Zero-weight shares receive zero. A zero or
// negative total returns zero allocations (never an error — a fill of 0
// is a no-op); a negative/empty weight set is a coded INVALID_REQUEST —
// silent misallocation is worse than loud failure (§2.7).
func AllocateProRata(total decimal.Decimal, shares []Share) ([]ShareAllocation, error) {
	if total.IsNegative() {
		return nil, errorf(CodeInvalidRequest, "pro-rata total %s is negative", total)
	}
	if len(shares) == 0 {
		return nil, errorf(CodeInvalidRequest, "pro-rata over empty share set")
	}
	var sum decimal.Decimal
	for i, s := range shares {
		if s.ID <= 0 {
			return nil, errorf(CodeInvalidRequest, "share %d: id must be positive", i)
		}
		if s.Weight.IsNegative() {
			return nil, errorf(CodeInvalidRequest, "share %d: negative weight %s", i, s.Weight)
		}
		sum = sum.Add(s.Weight)
	}
	if !sum.IsPositive() {
		return nil, errorf(CodeInvalidRequest, "pro-rata weight sum is zero")
	}
	if total.IsZero() {
		out := make([]ShareAllocation, len(shares))
		for i, s := range shares {
			out[i] = ShareAllocation{ID: s.ID, Quantity: decimal.Zero}
		}
		return out, nil
	}

	type rem struct {
		idx  int
		frac decimal.Decimal
	}
	out := make([]ShareAllocation, len(shares))
	rems := make([]rem, 0, len(shares))
	assigned := decimal.Zero
	for i, s := range shares {
		raw := total.Mul(s.Weight).Div(sum) // exact decimal math
		floor := raw.Truncate(8)            // floor at 1e-8 (positive → truncate == floor)
		out[i] = ShareAllocation{ID: s.ID, Quantity: floor}
		assigned = assigned.Add(floor)
		if frac := raw.Sub(floor); frac.IsPositive() {
			rems = append(rems, rem{idx: i, frac: frac})
		}
	}
	// Largest-remainder order; ascending ID breaks exact ties so the
	// distribution is a pure function of the inputs.
	sort.Slice(rems, func(a, b int) bool {
		if c := rems[a].frac.Cmp(rems[b].frac); c != 0 {
			return c > 0
		}
		return shares[rems[a].idx].ID < shares[rems[b].idx].ID
	})
	leftover := total.Sub(assigned) // integral number of 1e-8 quanta
	for leftover.IsPositive() {
		progressed := false
		for _, r := range rems {
			if !leftover.IsPositive() {
				break
			}
			out[r.idx].Quantity = out[r.idx].Quantity.Add(quantum)
			leftover = leftover.Sub(quantum)
			progressed = true
		}
		if !progressed {
			// Unreachable with positive weights — inputs are 8dp so
			// floor+remainders always cover the total. Fail loudly, never
			// silently misallocate.
			return nil, errorf(CodeAllocationMismatch,
				"pro-rata remainder %s has no recipient share", leftover)
		}
	}
	return out, nil
}
