// store.go — ShadowStore implementations: PostgreSQL (migration 227
// sor_shadow_orders + sor_fill_dedup) and in-memory for tests.
package sor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// Execer is the pgx-agnostic query seam used across the codebase. Rows
// go through the Scan-only contract so *sql.Row AND pgx.Row both serve
// it (PgxExecer adapts the repo's pgxpool).
type Execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, q string, args ...any) rowScanner
}

// PgxExecer adapts *pgxpool.Pool onto Execer — Phase-3 Task 4 wires the
// gateway's pgx pool into this store.
type PgxExecer struct{ Pool *pgxpool.Pool }

// ExecContext runs the statement via pgx Exec.
func (p PgxExecer) ExecContext(ctx context.Context, q string,
	args ...any) (sql.Result, error) {
	tag, err := p.Pool.Exec(ctx, q, args...)
	return cmdTagResult{tag}, err
}

// QueryRowContext maps pgx.QueryRow onto the Scan-only row contract —
// pgx.Row already has Scan(dest ...any) error.
func (p PgxExecer) QueryRowContext(ctx context.Context, q string,
	args ...any) rowScanner {
	return p.Pool.QueryRow(ctx, q, args...)
}

// cmdTagResult is pgconn.CommandTag as sql.Result (LastInsertId is a
// MySQL-ism; Postgres never populates it).
type cmdTagResult struct{ pgconn.CommandTag }

func (r cmdTagResult) RowsAffected() (int64, error) {
	return r.CommandTag.RowsAffected(), nil
}

func (r cmdTagResult) LastInsertId() (int64, error) {
	return 0, errors.New("sor: LastInsertId unsupported on Postgres")
}

// PgShadowStore persists shadow orders and fill dedup via database/sql.
type PgShadowStore struct {
	DB  Execer
	Now func() time.Time
}

// NewPgShadowStore — Now nil → time.Now.
func NewPgShadowStore(db Execer) *PgShadowStore {
	return &PgShadowStore{DB: db, Now: time.Now}
}

func (p *PgShadowStore) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// CreateShadow inserts a PENDING_ROUTE shadow order.
func (p *PgShadowStore) CreateShadow(ctx context.Context, s *ShadowOrder) (*ShadowOrder, error) {
	now := p.now()
	err := p.DB.QueryRowContext(ctx,
		`INSERT INTO sor_shadow_orders
			(parent_order_id, account_id, venue_id, external_order_id,
			 symbol, side, qty, filled_qty, state, attempt, created_at, updated_at)
		 VALUES ($1,$2,$3,'',$4,$5,$6,'0',$7,$8,$9,$9)
		 RETURNING id`,
		s.ParentOrderID, s.AccountID, s.VenueID, s.Symbol, s.Side,
		s.Qty.String(), string(s.State), s.Attempt, now,
	).Scan(&s.ID)
	if err != nil {
		return nil, fmt.Errorf("sor: insert shadow: %w", err)
	}
	s.CreatedAt, s.UpdatedAt = now, now
	return s, nil
}

// UpdateShadow persists the mutable fields.
func (p *PgShadowStore) UpdateShadow(ctx context.Context, s *ShadowOrder) error {
	var avg *string
	if s.AvgFillPrice != nil {
		v := s.AvgFillPrice.String()
		avg = &v
	}
	_, err := p.DB.ExecContext(ctx,
		`UPDATE sor_shadow_orders SET
			external_order_id=$2, filled_qty=$3, avg_fill_price=$4,
			state=$5, last_error=$6, routed_at=$7, completed_at=$8,
			updated_at=$9
		 WHERE id=$1`,
		s.ID, s.ExternalOrderID, s.FilledQty.String(), avg,
		string(s.State), s.LastError, s.RoutedAt, s.CompletedAt, p.now())
	return err
}

// OpenByParent returns the non-terminal shadow for a parent order.
func (p *PgShadowStore) OpenByParent(ctx context.Context, parentID int64) (*ShadowOrder, error) {
	row := p.DB.QueryRowContext(ctx,
		`SELECT id, parent_order_id, account_id, venue_id, external_order_id,
			symbol, side, qty, filled_qty, avg_fill_price, state, attempt,
			last_error, created_at, updated_at, routed_at, completed_at
		 FROM sor_shadow_orders
		 WHERE parent_order_id=$1
		   AND state NOT IN ('FILLED_EXTERNAL','CANCELLED_EXTERNAL')
		 ORDER BY id DESC LIMIT 1`, parentID)
	return scanShadow(row)
}

// ShadowByExternalID resolves a venue report to its shadow row.
func (p *PgShadowStore) ShadowByExternalID(ctx context.Context, venueID, extID string) (*ShadowOrder, error) {
	row := p.DB.QueryRowContext(ctx,
		`SELECT id, parent_order_id, account_id, venue_id, external_order_id,
			symbol, side, qty, filled_qty, avg_fill_price, state, attempt,
			last_error, created_at, updated_at, routed_at, completed_at
		 FROM sor_shadow_orders
		 WHERE venue_id=$1 AND external_order_id=$2`, venueID, extID)
	return scanShadow(row)
}

// RecordFill inserts the dedup row; unique-conflict → inserted=false.
func (p *PgShadowStore) RecordFill(ctx context.Context, shadowID int64,
	venueID, extID, execID string, qty, price decimal.Decimal) (bool, error) {
	_, err := p.DB.ExecContext(ctx,
		`INSERT INTO sor_fill_dedup
			(shadow_order_id, venue_id, external_order_id, exec_id, qty, price)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (venue_id, external_order_id, exec_id) DO NOTHING`,
		shadowID, venueID, extID, execID, qty.String(), price.String())
	if err != nil {
		return false, err
	}
	// Re-check existence to distinguish insert vs conflict.
	var n int64
	err = p.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sor_fill_dedup
		  WHERE venue_id=$1 AND external_order_id=$2 AND exec_id=$3
		    AND shadow_order_id=$4`,
		venueID, extID, execID, shadowID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanShadow(row rowScanner) (*ShadowOrder, error) {
	var s ShadowOrder
	var avg, state, lastErr sql.NullString
	var qty, filled string
	var routedAt, completedAt sql.NullTime
	err := row.Scan(&s.ID, &s.ParentOrderID, &s.AccountID, &s.VenueID,
		&s.ExternalOrderID, &s.Symbol, &s.Side, &qty, &filled, &avg,
		&state, &s.Attempt, &lastErr, &s.CreatedAt, &s.UpdatedAt,
		&routedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Qty, _ = decimal.NewFromString(qty)
	s.FilledQty, _ = decimal.NewFromString(filled)
	if avg.Valid {
		if d, err := decimal.NewFromString(avg.String); err == nil {
			s.AvgFillPrice = &d
		}
	}
	s.State = State(state.String)
	s.LastError = lastErr.String
	if routedAt.Valid {
		s.RoutedAt = &routedAt.Time
	}
	if completedAt.Valid {
		s.CompletedAt = &completedAt.Time
	}
	return &s, nil
}

// MemoryShadowStore is the deterministic in-test ShadowStore.
type MemoryShadowStore struct {
	mu      sync.Mutex
	nextID  int64
	shadows map[int64]*ShadowOrder
	fills   map[string]bool // venue|ext|exec → recorded
	Now     func() time.Time
}

// NewMemoryShadowStore builds an empty store.
func NewMemoryShadowStore() *MemoryShadowStore {
	return &MemoryShadowStore{
		shadows: map[int64]*ShadowOrder{},
		fills:   map[string]bool{},
		Now:     time.Now,
	}
}

func (m *MemoryShadowStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// CreateShadow inserts a PENDING_ROUTE row copy.
func (m *MemoryShadowStore) CreateShadow(_ context.Context, s *ShadowOrder) (*ShadowOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	cp := *s
	cp.ID = m.nextID
	cp.CreatedAt, cp.UpdatedAt = m.now(), m.now()
	m.shadows[cp.ID] = &cp
	out := cp
	return &out, nil
}

// UpdateShadow replaces the stored copy.
func (m *MemoryShadowStore) UpdateShadow(_ context.Context, s *ShadowOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	cp.UpdatedAt = m.now()
	m.shadows[s.ID] = &cp
	return nil
}

// OpenByParent returns the newest non-terminal shadow.
func (m *MemoryShadowStore) OpenByParent(_ context.Context, parentID int64) (*ShadowOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *ShadowOrder
	for _, s := range m.shadows {
		if s.ParentOrderID == parentID && !s.State.Terminal() {
			if best == nil || s.ID > best.ID {
				best = s
			}
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	return &cp, nil
}

// ShadowByExternalID resolves venue+external id.
func (m *MemoryShadowStore) ShadowByExternalID(_ context.Context, venueID, extID string) (*ShadowOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.shadows {
		if s.VenueID == venueID && s.ExternalOrderID == extID {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

// RecordFill inserts the dedup row — false when (venue, ext, exec) seen.
func (m *MemoryShadowStore) RecordFill(_ context.Context, shadowID int64,
	venueID, extID, execID string, _, _ decimal.Decimal) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := venueID + "|" + extID + "|" + execID
	if m.fills[key] {
		return false, nil
	}
	m.fills[key] = true
	return true, nil
}
