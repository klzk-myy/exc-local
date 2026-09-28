package ohlcv

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CandleStore persists candles to the fx_klines read model (migration
// 173). Save is an upsert: the engine writes the in-progress bar at the
// throttle cadence (Config.PersistOpen) and the final bar at close —
// matching the migration's "upserts the in-progress bar and finalizes on
// interval close" contract.
type CandleStore interface {
	Save(ctx context.Context, c Candle) error
}

// klineUpsert writes or refreshes one bar. The WHERE guard enforces the
// closed-candle immutability rule at the database layer: once a row is
// closed=true, later upserts (engine replay, duplicate finalize) are
// no-ops — corrections never flow through this path, they reconcile via
// the LateTrade counter/hook.
const klineUpsert = `
INSERT INTO fx_klines
    (instrument_id, symbol, timeframe, open_time,
     open, high, low, close, volume, quote_volume, trade_count, closed)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (instrument_id, timeframe, open_time) DO UPDATE SET
    open          = EXCLUDED.open,
    high          = EXCLUDED.high,
    low           = EXCLUDED.low,
    close         = EXCLUDED.close,
    volume        = EXCLUDED.volume,
    quote_volume  = EXCLUDED.quote_volume,
    trade_count   = EXCLUDED.trade_count,
    closed        = EXCLUDED.closed,
    updated_at    = now()
WHERE fx_klines.closed = FALSE`

// PGStore is the production CandleStore over the gateway pgx pool
// (internal/db.NewPool). Decimals are bound as fixed-point strings so the
// numeric cast happens inside PostgreSQL — the same convention the
// marketapi read side uses (::text out, text in).
type PGStore struct {
	pool *pgxpool.Pool

	mu  sync.Mutex
	ids map[string]int64 // symbol → instruments.id resolution cache
}

// NewPGStore wires the store.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool, ids: make(map[string]int64)}
}

// Save upserts one candle. When c.InstrumentID is 0 the id is resolved
// from instruments by symbol (cached); an unknown symbol is an error —
// rows are never written with a fabricated instrument id.
func (s *PGStore) Save(ctx context.Context, c Candle) error {
	id := c.InstrumentID
	if id == 0 {
		var err error
		id, err = s.instrumentID(ctx, c.Symbol)
		if err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, klineUpsert,
		id, c.Symbol, c.Interval.String(), c.OpenTime,
		c.Open.StringFixed(8), c.High.StringFixed(8),
		c.Low.StringFixed(8), c.Close.StringFixed(8),
		c.Volume.StringFixed(8), c.QuoteVolume.StringFixed(8),
		c.TradeCount, c.Closed)
	if err != nil {
		return fmt.Errorf("fx_klines upsert %s %s %s: %w",
			c.Symbol, c.Interval, c.OpenTime.UTC().Format("2006-01-02T15:04:05Z"), err)
	}
	return nil
}

// instrumentID resolves instruments.id for symbol with an in-process
// cache. Misses are not cached so a listing that arrives later resolves.
func (s *PGStore) instrumentID(ctx context.Context, symbol string) (int64, error) {
	s.mu.Lock()
	id, ok := s.ids[symbol]
	s.mu.Unlock()
	if ok {
		return id, nil
	}
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol = $1`, symbol).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("resolve instrument %s: %w", symbol, err)
	}
	s.mu.Lock()
	s.ids[symbol] = id
	s.mu.Unlock()
	return id, nil
}
