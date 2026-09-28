// PostgreSQL-backed InstrumentFilterSource for the account filters
// endpoint (Task 5.3.40). Reads the instruments table columns owned by
// migrations 001 (tick/lot/qty/bands/leverage/status) and 050
// (min_notional).
package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgInstrumentFilterSource resolves InstrumentFilters from the
// instruments table.
type PgInstrumentFilterSource struct {
	pool *pgxpool.Pool
}

// NewPgInstrumentFilterSource wires the source.
func NewPgInstrumentFilterSource(pool *pgxpool.Pool) *PgInstrumentFilterSource {
	return &PgInstrumentFilterSource{pool: pool}
}

// Filters implements InstrumentFilterSource; unknown symbols return
// (nil, nil).
func (s *PgInstrumentFilterSource) Filters(ctx context.Context, symbol string) (*InstrumentFilters, error) {
	var f InstrumentFilters
	var tick, lot, minQty, maxQty, minNot, pbUp, pbDown *string
	err := s.pool.QueryRow(ctx, `
		SELECT symbol, status::text, tick_size::text, lot_size::text,
		       min_order_qty::text, max_order_qty::text, min_notional::text,
		       price_band_pct_up::text, price_band_pct_down::text,
		       max_leverage, settlement_cycle
		FROM instruments WHERE symbol = $1`, symbol).
		Scan(&f.Symbol, &f.Status, &tick, &lot, &minQty, &maxQty, &minNot,
			&pbUp, &pbDown, &f.MaxLeverage, &f.SettlementCycle)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("instrument filters %s: %w", symbol, err)
	}
	f.TickSize = strOr(tick, "0")
	f.LotSize = strOr(lot, "0")
	f.MinOrderQty = strOr(minQty, "0")
	f.MaxOrderQty = strOr(maxQty, "0")
	f.MinNotional = strOr(minNot, "0")
	f.PriceBandPctUp = strOr(pbUp, "0")
	f.PriceBandPctDown = strOr(pbDown, "0")
	return &f, nil
}

func strOr(p *string, d string) string {
	if p == nil {
		return d
	}
	return *p
}
