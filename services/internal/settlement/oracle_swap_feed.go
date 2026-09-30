// oracle_swap_feed.go — Phase-19.5 Task 19.5.3.5 binding: the
// PriceOracle's published Tom-Next points (fwd_points:{pair}) become
// the SwapRateIngester's feed source, retiring the placeholder rate
// table path for venues that run the oracle.
//
// The adapter translates the oracle's per-pair {long,short} doc into
// the daily swapRateWire sheet the existing ingester validates,
// resolves and persists (effective-dated swap_rates rows). A missing
// or stale row surfaces per-symbol as an ingest rejection — the batch
// stays honest and the STALE_FORWARD_POINTS semantics hold.
package settlement

import (
	"context"
	"fmt"
	"time"

	"exchange/internal/oracle/rates"
	excerrors "exchange/pkg/errors"
)

// OracleSwapRateFeed implements SwapRateFeed over the oracle's
// fwd_points:{pair} keyspace. Pairs enumerates the universe (production
// wires marketStore.ListInstruments); a pair with no fresh row is
// skipped — never fabricated.
type OracleSwapRateFeed struct {
	Rates *rates.Store
	// Pairs returns the instrument universe to harvest — e.g. a live
	// instruments-table listing. Required.
	Pairs func(ctx context.Context) ([]string, error)
}

// FetchSwapRates implements SwapRateFeed — reads every pair's
// fwd_points doc; absent/stale pairs are skipped (the ingester reports
// the harvested count honestly and the store's LatestSwapRate fallback
// carries the gap day).
func (f OracleSwapRateFeed) FetchSwapRates(ctx context.Context,
	asOf time.Time) ([]swapRateWire, error) {

	if f.Rates == nil || f.Pairs == nil {
		return nil, fmt.Errorf("oracle swap feed: unwired (rates store/pairs resolver nil)")
	}
	syms, err := f.Pairs(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeSwapFeedUnavailable, "oracle pair list", err)
	}
	rows := make([]swapRateWire, 0, len(syms))
	for _, sym := range syms {
		sp, err := f.Rates.SwapPointFor(ctx, sym)
		if err != nil {
			continue // absent/stale — never fabricate a rate
		}
		rows = append(rows, swapRateWire{
			Symbol:          sym,
			EffectiveDate:   asOf.Format("2006-01-02"),
			LongSwapPoints:  sp.Long,
			ShortSwapPoints: sp.Short,
			Source:          sp.Source,
		})
	}
	if len(rows) == 0 {
		return nil, excerrors.New(CodeSwapFeedUnavailable,
			"oracle fwd points: no fresh rows — feed refused")
	}
	return rows, nil
}
