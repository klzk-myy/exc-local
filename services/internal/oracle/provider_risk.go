package oracle

import (
	"context"
	"time"

	"exchange/internal/risk"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// risk.MarkPriceProvider adapter (Task 19.5.3.4) — the frozen Phase-19
// contract rides the oracle unchanged: margin evaluation, liquidation
// pricing and collateral conversion call the same two methods.
// ---------------------------------------------------------------------------

// RiskMarkProvider adapts *Provider (Redis mark reads + health) to
// risk.MarkPriceProvider. Every read carries provenance
// (MarkSourceOracle) and staleness — the Phase-19 consumers' audit
// contract. Stale reads still return the price with Stale=true — the
// STALE-PRICE ladder (risk/liquidation_fallback.go) is the
// liquidation-path consumer; margin evaluation decides its own gate.
type RiskMarkProvider struct {
	R       MarkReader
	Timeout time.Duration // per-read bound; default 2s
}

// NewRiskMarkProvider wires the adapter over a Redis-reading Provider.
func NewRiskMarkProvider(p *Provider) *RiskMarkProvider {
	return &RiskMarkProvider{R: p, Timeout: 2 * time.Second}
}

// GetMarkPrice implements risk.MarkPriceProvider.
func (a *RiskMarkProvider) GetMarkPrice(symbol string) (decimal.Decimal, error) {
	mp, err := a.GetMarkPriceWithProvenance(symbol)
	if err != nil {
		return decimal.Zero, err
	}
	return mp.Price, nil
}

// GetMarkPriceWithProvenance implements risk.MarkPriceProvider.
func (a *RiskMarkProvider) GetMarkPriceWithProvenance(symbol string) (risk.MarkPrice, error) {
	to := a.Timeout
	if to <= 0 {
		to = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	v, err := a.R.Mark(ctx, symbol)
	if err != nil {
		return risk.MarkPrice{}, err
	}
	if !v.Found {
		return risk.MarkPrice{}, risk.ErrMarkNotFound
	}
	return risk.MarkPrice{
		Symbol: symbol, Price: v.Price, Source: risk.MarkSourceOracle,
		ValidAt: v.ValidAt, Stale: v.Stale,
	}, nil
}

var _ risk.MarkPriceProvider = (*RiskMarkProvider)(nil)
