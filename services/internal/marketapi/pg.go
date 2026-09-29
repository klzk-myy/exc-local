// PgStore is the PostgreSQL read/write model behind the market-data and
// venue REST surface. All reads are READ COMMITTED projections of stored
// rows — no synthetic data. DECIMAL columns are selected as ::text so the
// fixed-point contract reaches the wire untouched.
package marketapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// PgStore implements Store, BookSource, AnnouncementStore and
// MaintenanceStore over the gateway pool.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore wires the store.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

const instrumentCols = `
	id, symbol, base_currency, quote_currency, instrument_type::text,
	status::text, tick_size::text, lot_size::text, min_order_qty::text,
	max_order_qty::text, min_notional::text, min_price::text, max_price::text,
	price_band_pct_up::text, price_band_pct_down::text, max_spread_pips::text,
	max_open_orders, max_algo_orders, max_leverage, settlement_cycle,
	updated_at`

func scanInstrument(row pgx.Row) (*Instrument, error) {
	var i Instrument
	err := row.Scan(&i.ID, &i.Symbol, &i.BaseCurrency, &i.QuoteCurrency,
		&i.InstrumentType, &i.Status, &i.TickSize, &i.LotSize,
		&i.MinOrderQty, &i.MaxOrderQty, &i.MinNotional, &i.MinPrice,
		&i.MaxPrice, &i.PriceBandPctUp, &i.PriceBandPctDown,
		&i.MaxSpreadPips, &i.MaxOpenOrders, &i.MaxAlgoOrders,
		&i.MaxLeverage, &i.SettlementCycle, &i.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// ListInstruments returns every instrument ordered by symbol.
func (s *PgStore) ListInstruments(ctx context.Context) ([]Instrument, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+instrumentCols+` FROM instruments ORDER BY symbol`)
	if err != nil {
		return nil, fmt.Errorf("list instruments: %w", err)
	}
	defer rows.Close()
	var out []Instrument
	for rows.Next() {
		var i Instrument
		if err := rows.Scan(&i.ID, &i.Symbol, &i.BaseCurrency, &i.QuoteCurrency,
			&i.InstrumentType, &i.Status, &i.TickSize, &i.LotSize,
			&i.MinOrderQty, &i.MaxOrderQty, &i.MinNotional, &i.MinPrice,
			&i.MaxPrice, &i.PriceBandPctUp, &i.PriceBandPctDown,
			&i.MaxSpreadPips, &i.MaxOpenOrders, &i.MaxAlgoOrders,
			&i.MaxLeverage, &i.SettlementCycle, &i.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan instrument: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// InstrumentBySymbol returns nil for an unknown symbol.
func (s *PgStore) InstrumentBySymbol(ctx context.Context, symbol string) (*Instrument, error) {
	i, err := scanInstrument(s.pool.QueryRow(ctx,
		`SELECT `+instrumentCols+` FROM instruments WHERE symbol = $1`, symbol))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("instrument %s: %w", symbol, err)
	}
	return i, nil
}

// RecentTrades returns the public tape for symbol, newest first.
func (s *PgStore) RecentTrades(ctx context.Context, symbol string, limit int) ([]Trade, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.price::text, t.quantity::text,
		       COALESCE(t.trade_seq, 0),
		       (EXTRACT(EPOCH FROM t.created_at) * 1000)::bigint
		FROM trades t JOIN instruments i ON i.id = t.instrument_id
		WHERE i.symbol = $1
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT $2`, symbol, limit)
	if err != nil {
		return nil, fmt.Errorf("recent trades %s: %w", symbol, err)
	}
	defer rows.Close()
	out := make([]Trade, 0, limit)
	for rows.Next() {
		var tr Trade
		if err := rows.Scan(&tr.ID, &tr.Price, &tr.Quantity, &tr.Seq, &tr.TimeMs); err != nil {
			return nil, fmt.Errorf("scan trade: %w", err)
		}
		tr.Symbol = symbol
		out = append(out, tr)
	}
	return out, rows.Err()
}

// Ticker24h aggregates trades over [now-24h, now). Unknown symbol →
// (nil, nil); quiet symbol → zeroed Ticker with nil price fields.
func (s *PgStore) Ticker24h(ctx context.Context, symbol string, now time.Time) (*Ticker, error) {
	inst, err := s.InstrumentBySymbol(ctx, symbol)
	if err != nil {
		return nil, err
	}
	if inst == nil {
		return nil, nil
	}
	var (
		count                  int64
		low, high, first, last *string
		volume, quoteVolume    string
		firstAt, lastAt        *time.Time
	)
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       MIN(t.price)::text, MAX(t.price)::text,
		       (ARRAY_AGG(t.price ORDER BY t.created_at ASC,  t.id ASC ))[1]::text,
		       (ARRAY_AGG(t.price ORDER BY t.created_at DESC, t.id DESC))[1]::text,
		       COALESCE(SUM(t.quantity), 0)::text,
		       COALESCE(SUM(t.quantity * t.price), 0)::text,
		       MIN(t.created_at), MAX(t.created_at)
		FROM trades t JOIN instruments i ON i.id = t.instrument_id
		WHERE i.symbol = $1
		  AND t.created_at >= $2 AND t.created_at < $3`,
		symbol, now.Add(-24*time.Hour), now).
		Scan(&count, &low, &high, &first, &last, &volume, &quoteVolume,
			&firstAt, &lastAt)
	if err != nil {
		return nil, fmt.Errorf("ticker %s: %w", symbol, err)
	}
	tk := &Ticker{
		Symbol:       symbol,
		Window:       "24h",
		Volume:       volume,
		QuoteVolume:  quoteVolume,
		TradeCount:   count,
		ServerTimeMs: now.UnixMilli(),
	}
	if count == 0 {
		return tk, nil
	}
	tk.Open, tk.High, tk.Low, tk.Last = first, high, low, last
	if firstAt != nil {
		ms := firstAt.UnixMilli()
		tk.FirstTradeMs = &ms
	}
	if lastAt != nil {
		ms := lastAt.UnixMilli()
		tk.LastTradeMs = &ms
	}
	if first != nil && last != nil {
		if o, err1 := decimal.NewFromString(*first); err1 == nil {
			if c, err2 := decimal.NewFromString(*last); err2 == nil {
				chg := c.Sub(o)
				s := chg.String()
				tk.PriceChange = &s
				if !o.IsZero() {
					pct := chg.Div(o).Mul(decimal.NewFromInt(100)).Round(4)
					ps := pct.String()
					tk.PriceChangePct = &ps
				}
			}
		}
	}
	return tk, nil
}

// stats24hSQL aggregates the rolling 24h trade window per symbol, one row
// per instrument. The LEFT JOIN keeps quiet symbols present with zeroed
// aggregates (COUNT(t.id)=0 → NULL price columns). first/last are the
// window-edge trades by (created_at, id) — the same ordering Ticker24h
// uses. $1 = window start (now−24h), $2 = window end (now); the optional
// symbol predicate ($3) narrows to one instrument.
const stats24hSQL = `
	SELECT i.symbol, COUNT(t.id),
	       MIN(t.price)::text, MAX(t.price)::text,
	       (ARRAY_AGG(t.price ORDER BY t.created_at ASC,  t.id ASC ))[1]::text,
	       (ARRAY_AGG(t.price ORDER BY t.created_at DESC, t.id DESC))[1]::text,
	       COALESCE(SUM(t.quantity), 0)::text,
	       ROUND(COALESCE(SUM(t.quantity * t.price), 0), 8)::text,
	       MIN(t.created_at), MAX(t.created_at)
	FROM instruments i
	LEFT JOIN trades t
	  ON t.instrument_id = i.id
	 AND t.created_at >= $1 AND t.created_at < $2`

// scanStats24h maps one stats row into Stats24h. count==0 yields the
// zeroed row — symbol + volumes + window bounds, nil price fields.
func scanStats24h(row interface {
	Scan(...any) error
}, now time.Time) (*Stats24h, error) {
	var (
		st                     Stats24h
		low, high, first, last *string
		firstAt, lastAt        *time.Time
	)
	if err := row.Scan(&st.Symbol, &st.TradeCount, &low, &high,
		&first, &last, &st.Volume, &st.QuoteVolume, &firstAt, &lastAt); err != nil {
		return nil, err
	}
	st.Window = "24h"
	st.ServerTimeMs = now.UnixMilli()
	st.CloseTimeMs = now.UnixMilli()
	st.OpenTimeMs = now.Add(-24 * time.Hour).UnixMilli()
	if st.TradeCount == 0 {
		return &st, nil
	}
	st.Open, st.High, st.Low, st.Last = first, high, low, last
	if firstAt != nil {
		ms := firstAt.UnixMilli()
		st.FirstTradeMs = &ms
	}
	if lastAt != nil {
		ms := lastAt.UnixMilli()
		st.LastTradeMs = &ms
	}
	if first != nil && last != nil {
		if o, err1 := decimal.NewFromString(*first); err1 == nil {
			if c, err2 := decimal.NewFromString(*last); err2 == nil {
				chg := c.Sub(o)
				s := chg.String()
				st.PriceChange = &s
				if !o.IsZero() {
					pct := chg.Div(o).Mul(decimal.NewFromInt(100)).Round(4)
					ps := pct.String()
					st.PriceChangePct = &ps
				}
			}
		}
	}
	return &st, nil
}

// Stats24h implements Store.Stats24h — single-symbol rolling 24h
// statistics (Task 11.3.5). (nil, nil) for an unknown symbol.
func (s *PgStore) Stats24h(ctx context.Context, symbol string, now time.Time) (*Stats24h, error) {
	st, err := scanStats24h(s.pool.QueryRow(ctx,
		stats24hSQL+` WHERE i.symbol = $3 GROUP BY i.symbol`,
		now.Add(-24*time.Hour), now, symbol), now)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stats24h %s: %w", symbol, err)
	}
	return st, nil
}

// Stats24hAll implements Store.Stats24hAll — the venue-wide variant.
func (s *PgStore) Stats24hAll(ctx context.Context, now time.Time) ([]Stats24h, error) {
	rows, err := s.pool.Query(ctx,
		stats24hSQL+` GROUP BY i.symbol ORDER BY i.symbol`,
		now.Add(-24*time.Hour), now)
	if err != nil {
		return nil, fmt.Errorf("stats24h all: %w", err)
	}
	defer rows.Close()
	out := []Stats24h{}
	for rows.Next() {
		st, err := scanStats24h(rows, now)
		if err != nil {
			return nil, fmt.Errorf("scan stats24h: %w", err)
		}
		out = append(out, *st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats24h cursor: %w", err)
	}
	return out, nil
}

// Klines reads pre-materialized fx_klines bars (spec §10.3 contract).
// Returns bars ascending by open_time: the query takes the newest `limit`
// bars at or before `to` and reorders.
func (s *PgStore) Klines(ctx context.Context, symbol, timeframe string,
	from, to time.Time, limit int) ([]Kline, error) {
	if to.IsZero() {
		to = time.Now()
	}
	rows, err := s.pool.Query(ctx, `
		SELECT (EXTRACT(EPOCH FROM open_time) * 1000)::bigint,
		       open::text, high::text, low::text, close::text,
		       volume::text, quote_volume::text, trade_count, closed
		FROM fx_klines
		WHERE symbol = $1 AND timeframe = $2
		  AND open_time >= $3 AND open_time < $4
		ORDER BY open_time DESC
		LIMIT $5`, symbol, timeframe, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("klines %s %s: %w", symbol, timeframe, err)
	}
	defer rows.Close()
	var out []Kline
	for rows.Next() {
		var k Kline
		if err := rows.Scan(&k.OpenTimeMs, &k.Open, &k.High, &k.Low, &k.Close,
			&k.Volume, &k.QuoteVolume, &k.TradeCount, &k.Closed); err != nil {
			return nil, fmt.Errorf("scan kline: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// DESC fetch → ascending wire order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Snapshot derives the persisted L2 book from resting orders: status
// ACTIVE/PARTIALLY_FILLED rows with a price contribute (quantity -
// filled_qty) to their level. seq is the max engine-assigned book_seq on
// the resting rows — the durable cursor clients replay against. This is
// the stored-state view; the Phase-06 conflation engine can front it with
// a hotter source without changing the contract.
func (s *PgStore) Snapshot(ctx context.Context, symbol string, depth int) (*BookSnapshot, error) {
	inst, err := s.InstrumentBySymbol(ctx, symbol)
	if err != nil {
		return nil, err
	}
	if inst == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT o.side::text, o.price::text,
		       SUM(o.quantity - o.filled_qty)::text,
		       COALESCE(MAX(o.book_seq), 0)
		FROM orders o JOIN instruments i ON i.id = o.instrument_id
		WHERE i.symbol = $1
		  AND o.status IN ('ACTIVE', 'PARTIALLY_FILLED')
		  AND o.price IS NOT NULL
		GROUP BY o.side, o.price
		HAVING SUM(o.quantity - o.filled_qty) > 0`, symbol)
	if err != nil {
		return nil, fmt.Errorf("book %s: %w", symbol, err)
	}
	defer rows.Close()
	snap := &BookSnapshot{Symbol: symbol, Depth: depth,
		Bids: []BookLevel{}, Asks: []BookLevel{}}
	for rows.Next() {
		var side, price, qty string
		var seq int64
		if err := rows.Scan(&side, &price, &qty, &seq); err != nil {
			return nil, fmt.Errorf("scan book level: %w", err)
		}
		if seq > snap.Seq {
			snap.Seq = seq
		}
		lv := BookLevel{Price: price, Quantity: qty}
		if side == "BUY" {
			snap.Bids = append(snap.Bids, lv)
		} else {
			snap.Asks = append(snap.Asks, lv)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Bids best-first = highest price first; asks = lowest first.
	sort.Slice(snap.Bids, func(a, b int) bool {
		pa, _ := decimal.NewFromString(snap.Bids[a].Price)
		pb, _ := decimal.NewFromString(snap.Bids[b].Price)
		return pa.GreaterThan(pb)
	})
	sort.Slice(snap.Asks, func(a, b int) bool {
		pa, _ := decimal.NewFromString(snap.Asks[a].Price)
		pb, _ := decimal.NewFromString(snap.Asks[b].Price)
		return pa.LessThan(pb)
	})
	if depth > 0 {
		if len(snap.Bids) > depth {
			snap.Bids = snap.Bids[:depth]
		}
		if len(snap.Asks) > depth {
			snap.Asks = snap.Asks[:depth]
		}
	}
	return snap, nil
}

// ---------------------------------------------------------------------------
// Announcements (migration 171)
// ---------------------------------------------------------------------------

// ListAnnouncements returns announcements filtered per f, newest first.
func (s *PgStore) ListAnnouncements(ctx context.Context, f AnnouncementFilter) ([]Announcement, error) {
	where := "TRUE"
	args := []any{}
	if !f.IncludeAll {
		where += ` AND status = 'PUBLISHED' AND publish_at <= now()
		           AND (expires_at IS NULL OR expires_at > now())`
	}
	if f.Category != "" {
		args = append(args, f.Category)
		where += fmt.Sprintf(" AND category = $%d", len(args))
	}
	args = append(args, f.Limit)
	q := `SELECT id, title, body, category::text, status::text,
	             publish_at, expires_at, COALESCE(created_by, ''),
	             created_at, updated_at
	      FROM announcements WHERE ` + where +
		` ORDER BY publish_at DESC, id DESC LIMIT $` + fmt.Sprint(len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list announcements: %w", err)
	}
	defer rows.Close()
	var out []Announcement
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.Category, &a.Status,
			&a.PublishAt, &a.ExpiresAt, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan announcement: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Announcement returns one row by id; (nil, nil) when absent.
func (s *PgStore) Announcement(ctx context.Context, id int64) (*Announcement, error) {
	var a Announcement
	err := s.pool.QueryRow(ctx, `
		SELECT id, title, body, category::text, status::text,
		       publish_at, expires_at, COALESCE(created_by, ''),
		       created_at, updated_at
		FROM announcements WHERE id = $1`, id).
		Scan(&a.ID, &a.Title, &a.Body, &a.Category, &a.Status,
			&a.PublishAt, &a.ExpiresAt, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("announcement %d: %w", id, err)
	}
	return &a, nil
}

// CreateAnnouncement inserts and returns the stored row.
func (s *PgStore) CreateAnnouncement(ctx context.Context, a Announcement) (*Announcement, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO announcements
		    (title, body, category, status, publish_at, expires_at, created_by)
		VALUES ($1, $2, $3::announcement_category_enum, $4::announcement_status_enum,
		        $5, $6, $7)
		RETURNING id, publish_at, expires_at, created_at, updated_at`,
		a.Title, a.Body, a.Category, a.Status, a.PublishAt, a.ExpiresAt, a.CreatedBy).
		Scan(&a.ID, &a.PublishAt, &a.ExpiresAt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create announcement: %w", err)
	}
	return &a, nil
}

// UpdateAnnouncement rewrites the mutable fields; (nil, nil) when absent.
func (s *PgStore) UpdateAnnouncement(ctx context.Context, a Announcement) (*Announcement, error) {
	err := s.pool.QueryRow(ctx, `
		UPDATE announcements SET
		    title = $2, body = $3,
		    category = $4::announcement_category_enum,
		    status = $5::announcement_status_enum,
		    publish_at = $6, expires_at = $7, updated_at = now()
		WHERE id = $1
		RETURNING updated_at`,
		a.ID, a.Title, a.Body, a.Category, a.Status, a.PublishAt, a.ExpiresAt).
		Scan(&a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update announcement %d: %w", a.ID, err)
	}
	return &a, nil
}

// RetractAnnouncement flips status to RETRACTED; false when absent.
func (s *PgStore) RetractAnnouncement(ctx context.Context, id int64, by string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE announcements SET status = 'RETRACTED', updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("retract announcement %d: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ---------------------------------------------------------------------------
// Maintenance windows (migration 172)
// ---------------------------------------------------------------------------

// UpcomingMaintenance returns SCHEDULED/IN_PROGRESS windows that have not ended.
func (s *PgStore) UpcomingMaintenance(ctx context.Context, now time.Time) ([]MaintenanceWindow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, title, COALESCE(description, ''), scope::text,
		       symbols, status::text, starts_at, ends_at,
		       COALESCE(created_by, ''), created_at, updated_at
		FROM maintenance_windows
		WHERE status IN ('SCHEDULED', 'IN_PROGRESS') AND ends_at > $1
		ORDER BY starts_at ASC`, now)
	if err != nil {
		return nil, fmt.Errorf("upcoming maintenance: %w", err)
	}
	defer rows.Close()
	return scanWindows(rows)
}

// ListMaintenance returns all windows newest-first for the admin surface.
func (s *PgStore) ListMaintenance(ctx context.Context, limit int) ([]MaintenanceWindow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, title, COALESCE(description, ''), scope::text,
		       symbols, status::text, starts_at, ends_at,
		       COALESCE(created_by, ''), created_at, updated_at
		FROM maintenance_windows
		ORDER BY starts_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list maintenance: %w", err)
	}
	defer rows.Close()
	return scanWindows(rows)
}

func scanWindows(rows pgx.Rows) ([]MaintenanceWindow, error) {
	var out []MaintenanceWindow
	for rows.Next() {
		var m MaintenanceWindow
		if err := rows.Scan(&m.ID, &m.Title, &m.Description, &m.Scope,
			&m.Symbols, &m.Status, &m.StartsAt, &m.EndsAt,
			&m.CreatedBy, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan maintenance window: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateMaintenance inserts a window and returns the stored row.
func (s *PgStore) CreateMaintenance(ctx context.Context, m MaintenanceWindow) (*MaintenanceWindow, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO maintenance_windows
		    (title, description, scope, symbols, status, starts_at, ends_at, created_by)
		VALUES ($1, $2, $3::maintenance_scope_enum, $4,
		        $5::maintenance_status_enum, $6, $7, $8)
		RETURNING id, created_at, updated_at`,
		m.Title, m.Description, m.Scope, m.Symbols, m.Status,
		m.StartsAt, m.EndsAt, m.CreatedBy).
		Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create maintenance window: %w", err)
	}
	return &m, nil
}

// UpdateMaintenance rewrites the mutable fields; (nil, nil) when absent.
func (s *PgStore) UpdateMaintenance(ctx context.Context, m MaintenanceWindow) (*MaintenanceWindow, error) {
	err := s.pool.QueryRow(ctx, `
		UPDATE maintenance_windows SET
		    title = $2, description = $3,
		    scope = $4::maintenance_scope_enum, symbols = $5,
		    status = $6::maintenance_status_enum,
		    starts_at = $7, ends_at = $8, updated_at = now()
		WHERE id = $1
		RETURNING updated_at`,
		m.ID, m.Title, m.Description, m.Scope, m.Symbols,
		m.Status, m.StartsAt, m.EndsAt).Scan(&m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update maintenance window %d: %w", m.ID, err)
	}
	return &m, nil
}

// CancelMaintenance flips status to CANCELLED; false when absent.
func (s *PgStore) CancelMaintenance(ctx context.Context, id int64, by string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE maintenance_windows SET status = 'CANCELLED', updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("cancel maintenance window %d: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}
