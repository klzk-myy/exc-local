// Persistence + read seams for the algo framework. Decimals cross the
// wire as `::text` / `$n::numeric` per the repo pgx convention.
package algo

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// Store is the algo-state persistence seam — PgStore implements it over
// migration-224 tables; tests may substitute fakes.
type Store interface {
	// InsertParent persists a NEW parent; on (account_id,
	// client_order_id) unique conflict it returns the existing row and
	// dup=true so the caller replays the stored parent (§8.7).
	InsertParent(ctx context.Context, p *Parent) (*Parent, bool, error)
	GetParent(ctx context.Context, id int64) (*Parent, error)
	// CASStatus flips status only when the row is in one of the `from`
	// states — the transition guard that resolves API↔driver races.
	CASStatus(ctx context.Context, id int64, from []string, to,
		errorDetail string) (bool, error)
	// SaveState persists the driver cursor JSON (state column).
	SaveState(ctx context.Context, id int64, state []byte) error
	// SetFilled updates the parent's aggregate filled_qty.
	SetFilled(ctx context.Context, id int64, filled decimal.Decimal) error
	// ActiveParents returns parents the engine must adopt at boot
	// (PENDING/RUNNING/PAUSED — migration 224 partial index).
	ActiveParents(ctx context.Context) ([]Parent, error)
	// PendingDue returns PENDING parents whose start_at arrived.
	PendingDue(ctx context.Context, now time.Time, limit int) ([]Parent, error)
	// ListParents returns an account's parents, newest first; status
	// "" = all, otherwise a status filter.
	ListParents(ctx context.Context, accountID int64, status string,
		limit int) ([]Parent, error)

	// InsertChild persists a durable PENDING intent BEFORE dispatch —
	// a crash between insert and dispatch leaves a replayable row whose
	// derived client_order_id is reused (never a double submit).
	InsertChild(ctx context.Context, c *Child) (*Child, error)
	UpdateChild(ctx context.Context, c *Child) error
	Children(ctx context.Context, parentID int64) ([]Child, error)
	// OpenChildren returns children whose order may still be live
	// (PENDING/SUBMITTED/OPEN/PARTIAL).
	OpenChildren(ctx context.Context, parentID int64) ([]Child, error)
}

// ---------------------------------------------------------------------------
// PgStore
// ---------------------------------------------------------------------------

type PgStore struct{ pool *pgxpool.Pool }

func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

func isNoRows(err error) bool { return stderrors.Is(err, pgx.ErrNoRows) }

const parentCols = `
	id, account_id, algo_type, symbol, side, status, total_qty::text,
	filled_qty::text, params, state, start_at, expires_at,
	COALESCE(client_order_id,''), COALESCE(error_detail,''),
	created_at, updated_at, completed_at`

func scanParent(row pgx.Row) (*Parent, error) {
	p := &Parent{}
	var qty, filled string
	err := row.Scan(&p.ID, &p.AccountID, &p.Type, &p.Symbol, &p.Side,
		&p.Status, &qty, &filled, &p.Params, &p.State,
		&p.StartAt, &p.ExpiresAt, &p.ClientOrderID, &p.ErrorDetail,
		&p.CreatedAt, &p.UpdatedAt, &p.CompletedAt)
	if err != nil {
		return nil, err
	}
	p.TotalQty = decimal.RequireFromString(qty)
	p.FilledQty = decimal.RequireFromString(filled)
	return p, nil
}

// InsertParent writes the NEW parent. The dedup read precedes insert so
// a replay returns the stored parent; a 23505 race re-reads inside the
// same statement loop (single statement, no tx needed).
func (s *PgStore) InsertParent(ctx context.Context, p *Parent) (*Parent, bool, error) {
	if p.ClientOrderID != "" {
		existing, err := s.parentByCID(ctx, p.AccountID, p.ClientOrderID)
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			return existing, true, nil
		}
	}
	params := p.Params
	if len(params) == 0 {
		params = []byte(`{}`)
	}
	state := p.State
	if len(state) == 0 {
		state = []byte(`{}`)
	}
	row, err := scanParent(s.pool.QueryRow(ctx, `
		INSERT INTO algo_orders (account_id, algo_type, symbol, side,
		    status, total_qty, params, state, start_at, expires_at,
		    client_order_id)
		VALUES ($1,$2,$3,$4,'NEW',$5::numeric,$6,$7,$8,$9,NULLIF($10,''))
		RETURNING `+parentCols,
		p.AccountID, p.Type, p.Symbol, p.Side, p.TotalQty.String(),
		params, state, p.StartAt, p.ExpiresAt, p.ClientOrderID))
	if err != nil {
		var pgErr *pgconn.PgError
		if stderrors.As(err, &pgErr) && pgErr.Code == "23505" && p.ClientOrderID != "" {
			existing, rerr := s.parentByCID(ctx, p.AccountID, p.ClientOrderID)
			if rerr != nil {
				return nil, false, fmt.Errorf("algo parent conflict lookup: %w", rerr)
			}
			if existing != nil {
				return existing, true, nil
			}
		}
		return nil, false, fmt.Errorf("insert algo parent: %w", err)
	}
	return row, false, nil
}

func (s *PgStore) parentByCID(ctx context.Context, accountID int64, cid string) (*Parent, error) {
	p, err := scanParent(s.pool.QueryRow(ctx,
		`SELECT `+parentCols+` FROM algo_orders
		  WHERE account_id=$1 AND client_order_id=$2`, accountID, cid))
	if isNoRows(err) {
		return nil, nil
	}
	return p, err
}

func (s *PgStore) GetParent(ctx context.Context, id int64) (*Parent, error) {
	p, err := scanParent(s.pool.QueryRow(ctx,
		`SELECT `+parentCols+` FROM algo_orders WHERE id=$1`, id))
	if isNoRows(err) {
		return nil, nil
	}
	return p, err
}

// CASStatus is the state-machine guard: UPDATE … WHERE status=ANY(from).
// Terminal states also stamp completed_at; a non-empty errorDetail lands
// on error_detail.
func (s *PgStore) CASStatus(ctx context.Context, id int64, from []string,
	to, errorDetail string) (bool, error) {
	q := `UPDATE algo_orders SET status=$2, updated_at=now()`
	args := []any{id, to}
	n := 2
	if isTerminalStatus(to) {
		q += `, completed_at=now()`
	}
	if errorDetail != "" {
		n++
		q += fmt.Sprintf(`, error_detail=$%d`, n)
		args = append(args, errorDetail)
	}
	n++
	q += fmt.Sprintf(` WHERE id=$1 AND status = ANY($%d) RETURNING id`, n)
	args = append(args, from)
	var rid int64
	err := s.pool.QueryRow(ctx, q, args...).Scan(&rid)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *PgStore) SaveState(ctx context.Context, id int64, state []byte) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE algo_orders SET state=$2, updated_at=now() WHERE id=$1`,
		id, state)
	return err
}

func (s *PgStore) SetFilled(ctx context.Context, id int64, filled decimal.Decimal) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE algo_orders SET filled_qty=$2::numeric, updated_at=now() WHERE id=$1`,
		id, filled.String())
	return err
}

func (s *PgStore) ActiveParents(ctx context.Context) ([]Parent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+parentCols+` FROM algo_orders
		  WHERE status IN ('PENDING','RUNNING','PAUSED') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Parent
	for rows.Next() {
		p, err := scanParent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *PgStore) PendingDue(ctx context.Context, now time.Time, limit int) ([]Parent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+parentCols+` FROM algo_orders
		  WHERE status='PENDING' AND start_at IS NOT NULL AND start_at <= $1
		  ORDER BY start_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Parent
	for rows.Next() {
		p, err := scanParent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *PgStore) ListParents(ctx context.Context, accountID int64,
	status string, limit int) ([]Parent, error) {
	q := `SELECT ` + parentCols + ` FROM algo_orders WHERE account_id=$1`
	args := []any{accountID}
	if status != "" {
		q += ` AND status=$2`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Parent
	for rows.Next() {
		p, err := scanParent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// children
// ---------------------------------------------------------------------------

const childCols = `
	id, algo_order_id, seq, slice_index, role, symbol, side,
	client_order_id, order_id, qty::text, price::text, status,
	filled_qty::text, detail, dispatched_at, resolved_at`

func scanChild(row pgx.Row) (*Child, error) {
	c := &Child{}
	var qty, price, filled *string
	err := row.Scan(&c.ID, &c.AlgoOrderID, &c.Seq, &c.SliceIndex, &c.Role,
		&c.Symbol, &c.Side, &c.ClientOrderID, &c.OrderID, &qty, &price,
		&c.Status, &filled, &c.Detail, &c.DispatchedAt, &c.ResolvedAt)
	if err != nil {
		return nil, err
	}
	if qty != nil {
		c.Qty = decimal.RequireFromString(*qty)
	}
	if price != nil {
		c.Price = mustDecPtr(*price)
	}
	if filled != nil {
		c.FilledQty = decimal.RequireFromString(*filled)
	}
	return c, nil
}

func mustDecPtr(s string) *decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return nil
	}
	return &d
}

// InsertChild writes the durable PENDING intent; the derived
// client_order_id is computed here (algo:{parent}:{seq}) so retries of
// the same child row always carry the same idempotency key.
func (s *PgStore) InsertChild(ctx context.Context, c *Child) (*Child, error) {
	if c.ClientOrderID == "" {
		c.ClientOrderID = childCID(c.AlgoOrderID, c.Seq)
	}
	row, err := scanChild(s.pool.QueryRow(ctx, `
		INSERT INTO algo_order_children (algo_order_id, seq, slice_index,
		    role, symbol, side, client_order_id, qty, price, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9::numeric,'PENDING')
		RETURNING `+childCols,
		c.AlgoOrderID, c.Seq, c.SliceIndex, c.Role, c.Symbol, c.Side,
		c.ClientOrderID, c.Qty.String(), decStr(c.Price)))
	if err != nil {
		return nil, fmt.Errorf("insert algo child: %w", err)
	}
	return row, nil
}

// UpdateChild rewrites the mutable child columns after dispatch/resolve.
func (s *PgStore) UpdateChild(ctx context.Context, c *Child) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE algo_order_children SET
		    order_id=$2, qty=$3::numeric, price=$4::numeric, status=$5,
		    filled_qty=$6::numeric, detail=$7, dispatched_at=$8,
		    resolved_at=$9
		WHERE id=$1`,
		c.ID, c.OrderID, c.Qty.String(), decStr(c.Price), c.Status,
		c.FilledQty.String(), c.Detail, c.DispatchedAt, c.ResolvedAt)
	return err
}

func decStr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}

func (s *PgStore) Children(ctx context.Context, parentID int64) ([]Child, error) {
	return s.childrenWhere(ctx, `algo_order_id=$1`, parentID)
}

func (s *PgStore) OpenChildren(ctx context.Context, parentID int64) ([]Child, error) {
	return s.childrenWhere(ctx,
		`algo_order_id=$1 AND status IN ('PENDING','SUBMITTED','OPEN','PARTIAL')`,
		parentID)
}

func (s *PgStore) childrenWhere(ctx context.Context, where string, parentID int64) ([]Child, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+childCols+` FROM algo_order_children
		  WHERE `+where+` ORDER BY seq`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Child
	for rows.Next() {
		c, err := scanChild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Quote + reference + volume read seams
// ---------------------------------------------------------------------------

// QuoteSource resolves top-of-book for mid computation. PgTopOfBook is
// the production binding over the persisted book (the same stored-state
// view marketapi.PgStore.Snapshot serves); a hotter BBO feed can bind
// the same seam later without touching drivers.
type QuoteSource interface {
	// BestBidAsk returns the best bid/ask for symbol; ok=false when a
	// side is absent (sparse/empty book — callers fail closed, spec
	// §6.6 empty-book safeguard).
	BestBidAsk(ctx context.Context, symbol string) (bid, ask decimal.Decimal, ok bool, err error)
}

// RefSource resolves the last-trade reference price — the fallback when
// the book is empty on one side (same seam orders admission uses).
type RefSource interface {
	ReferencePrice(ctx context.Context, symbol string) (*decimal.Decimal, error)
}

// VolumeProfileSource returns normalized historical volume weights for
// the VWAP schedule (Task 16.3.2). PgVolumeProfile is the wired impl;
// a ClickHouse bucket source can bind the same seam when the Phase-23
// analytics read path lands.
type VolumeProfileSource interface {
	VolumeProfile(ctx context.Context, symbol string, buckets int) ([]decimal.Decimal, error)
}

// PipSizeSource resolves instruments.pip_size — the unit for the
// anti-gaming discretion band (0–3 pips) and scaled-order pip spacing.
type PipSizeSource interface {
	PipSize(ctx context.Context, symbol string) (decimal.Decimal, error)
}

// PgPipSize reads instruments.pip_size (migration 087).
type PgPipSize struct{ pool *pgxpool.Pool }

func NewPgPipSize(pool *pgxpool.Pool) *PgPipSize { return &PgPipSize{pool: pool} }

func (s *PgPipSize) PipSize(ctx context.Context, symbol string) (decimal.Decimal, error) {
	var p string
	err := s.pool.QueryRow(ctx,
		`SELECT pip_size::text FROM instruments WHERE symbol=$1`,
		strings.ToUpper(strings.TrimSpace(symbol))).Scan(&p)
	if isNoRows(err) {
		return decimal.Zero, codeErr("INVALID_REQUEST", "unknown symbol %q", symbol)
	}
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.RequireFromString(p), nil
}

// VolumeSource reports observed trade volume in a recent window for VP
// (Task 16.3.18).
type VolumeSource interface {
	VolumeSince(ctx context.Context, symbol string, since time.Time) (decimal.Decimal, error)
}

// PgTopOfBook resolves best bid/ask from the persisted resting-order
// book (orders.status ACTIVE/PARTIALLY_FILLED, price not null,
// remaining qty > 0) — one read per side.
type PgTopOfBook struct{ pool *pgxpool.Pool }

func NewPgTopOfBook(pool *pgxpool.Pool) *PgTopOfBook { return &PgTopOfBook{pool: pool} }

func (s *PgTopOfBook) BestBidAsk(ctx context.Context, symbol string) (bid, ask decimal.Decimal, ok bool, err error) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	var bidS, askS *string
	err = s.pool.QueryRow(ctx, `
		SELECT MAX(o.price)::text
		  FROM orders o JOIN instruments i ON i.id = o.instrument_id
		 WHERE i.symbol = $1 AND o.side = 'BUY'
		   AND o.status IN ('ACTIVE','PARTIALLY_FILLED')
		   AND o.price IS NOT NULL AND o.quantity > o.filled_qty`, sym).Scan(&bidS)
	if err != nil {
		return decimal.Zero, decimal.Zero, false, err
	}
	err = s.pool.QueryRow(ctx, `
		SELECT MIN(o.price)::text
		  FROM orders o JOIN instruments i ON i.id = o.instrument_id
		 WHERE i.symbol = $1 AND o.side = 'SELL'
		   AND o.status IN ('ACTIVE','PARTIALLY_FILLED')
		   AND o.price IS NOT NULL AND o.quantity > o.filled_qty`, sym).Scan(&askS)
	if err != nil {
		return decimal.Zero, decimal.Zero, false, err
	}
	if bidS == nil || askS == nil {
		return decimal.Zero, decimal.Zero, false, nil
	}
	return decimal.RequireFromString(*bidS), decimal.RequireFromString(*askS), true, nil
}

// PgRefPrice resolves the last-trade reference for a symbol.
type PgRefPrice struct{ pool *pgxpool.Pool }

func NewPgRefPrice(pool *pgxpool.Pool) *PgRefPrice { return &PgRefPrice{pool: pool} }

func (s *PgRefPrice) ReferencePrice(ctx context.Context, symbol string) (*decimal.Decimal, error) {
	var px *string
	err := s.pool.QueryRow(ctx, `
		SELECT t.price::text FROM trades t
		  JOIN instruments i ON i.id = t.instrument_id
		 WHERE i.symbol = $1 ORDER BY t.id DESC LIMIT 1`,
		strings.ToUpper(strings.TrimSpace(symbol))).Scan(&px)
	if isNoRows(err) || px == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, perr := decimal.NewFromString(*px)
	if perr != nil {
		return nil, perr
	}
	return &d, nil
}

// PgVolumeProfile builds a trailing-24h bucketed volume share from the
// persisted trades tape. Buckets with no trades contribute zero; the
// caller falls back to a flat profile when the covered mass is too thin
// (documented in vwap.go — fail to a neutral schedule, never invent
// volume). When the Phase-23 ClickHouse bucket store lands it binds the
// same VolumeProfileSource seam.
type PgVolumeProfile struct{ pool *pgxpool.Pool }

func NewPgVolumeProfile(pool *pgxpool.Pool) *PgVolumeProfile {
	return &PgVolumeProfile{pool: pool}
}

func (s *PgVolumeProfile) VolumeProfile(ctx context.Context, symbol string, buckets int) ([]decimal.Decimal, error) {
	if buckets <= 0 {
		return nil, fmt.Errorf("buckets must be positive")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT floor(EXTRACT(EPOCH FROM t.created_at - now() + interval '24 hours')
		             / (86400.0 / $2))::int AS bucket,
		       SUM(t.quantity)::text
		  FROM trades t JOIN instruments i ON i.id = t.instrument_id
		 WHERE i.symbol = $1
		   AND t.created_at >= now() - interval '24 hours'
		 GROUP BY bucket`,
		strings.ToUpper(strings.TrimSpace(symbol)), buckets)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]decimal.Decimal, buckets)
	for rows.Next() {
		var b int
		var q string
		if err := rows.Scan(&b, &q); err != nil {
			return nil, err
		}
		if b >= 0 && b < buckets {
			out[b] = decimal.RequireFromString(q)
		}
	}
	return out, rows.Err()
}

// PgVolumeSource is the durable fallback for VP observed volume —
// reads the trades tape directly when the NATS tracker is unwired.
type PgVolumeSource struct{ pool *pgxpool.Pool }

func NewPgVolumeSource(pool *pgxpool.Pool) *PgVolumeSource {
	return &PgVolumeSource{pool: pool}
}

func (s *PgVolumeSource) VolumeSince(ctx context.Context, symbol string, since time.Time) (decimal.Decimal, error) {
	var q *string
	err := s.pool.QueryRow(ctx, `
		SELECT SUM(t.quantity)::text
		  FROM trades t JOIN instruments i ON i.id = t.instrument_id
		 WHERE i.symbol = $1 AND t.created_at >= $2`,
		strings.ToUpper(strings.TrimSpace(symbol)), since).Scan(&q)
	if err != nil {
		return decimal.Zero, err
	}
	if q == nil {
		return decimal.Zero, nil
	}
	return decimal.RequireFromString(*q), nil
}
