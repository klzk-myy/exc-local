// im_spread_offsets.go — Phase-19 Task 19.3.25 back-fit: the narrow
// seam binding the Phase-22 spread-recognition service
// (margin.SpreadOffsetService, Task 22.3.13) to the UMR/SIMM
// assessment's IMSpreadOffsetSource contract.
//
// The recognition service's APPLIED rows are the single source of
// truth: each row carries the deterministic spread id, both consumed
// leg refs (single-consumption was enforced at detection), and the
// relief vs naked sum. IMAggregate then removes the consumed legs
// BEFORE bucket aggregation and charges each pair its bounded margin —
// the §15.7 "offsets before aggregation, never double-counted" rule.
package derivatives

import (
	"context"

	excmargin "exchange/internal/margin"
)

// MarginSpreadOffsetSource adapts *margin.SpreadOffsetService.LiveOffsets
// (APPLIED rows only — BROKEN/DETECTED rows never reach aggregation) to
// IMSpreadOffsetSource. Declared adapter-side, not via a naked function
// type, so the production wiring is greppable.
type MarginSpreadOffsetSource struct {
	Svc *excmargin.SpreadOffsetService
}

// SpreadOffsets implements IMSpreadOffsetSource.
func (s MarginSpreadOffsetSource) SpreadOffsets(ctx context.Context,
	accountID int64) ([]IMSpreadOffset, error) {
	if s.Svc == nil {
		return nil, nil // unwired → no relief (documented nil behavior)
	}
	live, err := s.Svc.LiveOffsets(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]IMSpreadOffset, 0, len(live))
	for _, sp := range live {
		out = append(out, IMSpreadOffset{
			SpreadID:   sp.ID,
			SpreadType: sp.Kind,
			LegRefs:    append([]int64(nil), sp.LegRefs...),
			ReliefUSD:  sp.OffsetUSD,
		})
	}
	return out, nil
}

var _ IMSpreadOffsetSource = MarginSpreadOffsetSource{}
