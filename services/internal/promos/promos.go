// Package promos implements the governed write path for Phase-05 Task
// 5.3.15 fee promotion windows.
//
// The engine seam is untouched: settlement.FeeTier.RateBps already
// applies promo_maker_bps / promo_taker_bps strictly while
// fee_tiers.promo_until > now() (migration 012 columns). This package
// owns the *window lifecycle* — fee_promo_windows (migration 181):
//
//	Create  → PENDING_APPROVAL row
//	Approve → second principal applies promo_* onto fee_tiers and marks
//	          the window APPLIED (four-eyes: approver ≠ creator, inside
//	          the spec §8.2 15-minute dual-control window — a fee-tier
//	          change is on the dual-control list)
//	Reject  → REJECTED
//	Expire  → EXPIRED (window superseded / ends_at passed unapplied)
//
// Per-tier promo state is cleared by writing promo_until=NULL when the
// applied window's ends_at passes (ClearExpired).
package promos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// Window status values.
const (
	StatusPending  = "PENDING_APPROVAL"
	StatusApplied  = "APPLIED"
	StatusRejected = "REJECTED"
	StatusExpired  = "EXPIRED"
)

// ApprovalWindow is the spec §8.2 four-eyes ceiling for fee-tier
// changes — create→apply must complete inside it.
const ApprovalWindow = 15 * time.Minute

var (
	ErrNotFound    = errors.New("promos: window not found")
	ErrNotPending  = errors.New("promos: window is not pending approval")
	ErrFourEyes    = errors.New("promos: approver must differ from creator")
	ErrTooLate     = errors.New("promos: approval window (15m) elapsed")
	ErrBadWindow   = errors.New("promos: ends_at must be in the future")
	ErrNoRates     = errors.New("promos: at least one promo rate required")
	ErrRateRange   = errors.New("promos: promo rate must be within 0..10000 bps")
	ErrTierMissing = errors.New("promos: fee tier not found")
)

// Window is one fee_promo_windows row.
type Window struct {
	ID            int64            `json:"id"`
	FeeTierID     int64            `json:"fee_tier_id"`
	PromoMakerBps *decimal.Decimal `json:"promo_maker_bps,omitempty"`
	PromoTakerBps *decimal.Decimal `json:"promo_taker_bps,omitempty"`
	EndsAt        time.Time        `json:"ends_at"`
	Status        string           `json:"status"`
	CreatedBy     int64            `json:"created_by"`
	ApprovedBy    *int64           `json:"approved_by,omitempty"`
	ApprovedAt    *time.Time       `json:"approved_at,omitempty"`
	Note          *string          `json:"note,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// Store owns fee_promo_windows + the fee_tiers application write.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewStore binds the store.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("promos: nil pool")
	}
	return &Store{pool: pool, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *Store) SetClockForTest(now func() time.Time) { s.now = now }

func validRate(bps decimal.Decimal) bool {
	return !bps.IsNegative() && bps.LessThanOrEqual(decimal.NewFromInt(10000))
}

// Create registers a promo window in PENDING_APPROVAL. At least one side
// (maker and/or taker) must carry a rate; ends_at must be future —
// RateBps applies promos strictly while promo_until > now(), so a
// non-future end can never activate.
func (s *Store) Create(ctx context.Context, feeTierID, createdBy int64,
	maker, taker *decimal.Decimal, endsAt time.Time, note *string) (*Window, error) {

	if feeTierID <= 0 || createdBy <= 0 {
		return nil, fmt.Errorf("promos: fee_tier_id and created_by must be positive")
	}
	if maker == nil && taker == nil {
		return nil, ErrNoRates
	}
	if maker != nil && !validRate(*maker) {
		return nil, ErrRateRange
	}
	if taker != nil && !validRate(*taker) {
		return nil, ErrRateRange
	}
	if !endsAt.After(s.now()) {
		return nil, ErrBadWindow
	}
	// Fail fast if the tier doesn't exist — surfaces as ErrTierMissing
	// rather than a bare FK error.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM fee_tiers WHERE id=$1)`, feeTierID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("promos: tier check: %w", err)
	}
	if !exists {
		return nil, ErrTierMissing
	}

	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO fee_promo_windows
		 (fee_tier_id, promo_maker_bps, promo_taker_bps, ends_at, created_by, note)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		feeTierID, maker, taker, endsAt.UTC(), createdBy, note).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("promos: create window: %w", err)
	}
	return s.Get(ctx, id)
}

// Get returns one window by id.
func (s *Store) Get(ctx context.Context, id int64) (*Window, error) {
	w, err := scanWindow(s.pool.QueryRow(ctx,
		`SELECT `+windowCols+` FROM fee_promo_windows WHERE id=$1`, id).Scan)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("promos: read window: %w", err)
	}
	return w, nil
}

const windowCols = `id, fee_tier_id, promo_maker_bps, promo_taker_bps,
	ends_at, status, created_by, approved_by, approved_at, note,
	created_at, updated_at`

func scanWindow(scan func(dest ...any) error) (*Window, error) {
	var w Window
	var maker, taker *decimal.Decimal
	err := scan(&w.ID, &w.FeeTierID, &maker, &taker, &w.EndsAt,
		&w.Status, &w.CreatedBy, &w.ApprovedBy, &w.ApprovedAt, &w.Note,
		&w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return nil, err
	}
	w.PromoMakerBps = maker
	w.PromoTakerBps = taker
	return &w, nil
}

// List returns windows newest-first, optionally filtered by status ("" =
// all).
func (s *Store) List(ctx context.Context, status string, limit int) ([]Window, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var rows pgx.Rows
	var err error
	if status == "" {
		rows, err = s.pool.Query(ctx,
			`SELECT `+windowCols+` FROM fee_promo_windows
			  ORDER BY id DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT `+windowCols+` FROM fee_promo_windows
			  WHERE status=$1 ORDER BY id DESC LIMIT $2`, status, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("promos: list: %w", err)
	}
	defer rows.Close()
	out := []Window{}
	for rows.Next() {
		w, err := scanWindow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("promos: scan: %w", err)
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// Approve applies the pending window to fee_tiers — writes promo_until,
// promo_maker_bps (COALESCE keeps the prior maker side when the window
// only promotes the taker side), promo_taker_bps — and marks the window
// APPLIED, all inside one transaction so the tier row and the audit row
// can never diverge.
//
// Four-eyes: approverID must differ from the window's creator and the
// call must land inside ApprovalWindow (15m) of creation — both enforced
// here as defense-in-depth on top of the DB CHECKs.
func (s *Store) Approve(ctx context.Context, windowID, approverID int64) (*Window, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("promos: approve tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var w Window
	var maker, taker *decimal.Decimal
	err = tx.QueryRow(ctx,
		`SELECT `+windowCols+` FROM fee_promo_windows WHERE id=$1 FOR UPDATE`,
		windowID).Scan(&w.ID, &w.FeeTierID, &maker, &taker, &w.EndsAt,
		&w.Status, &w.CreatedBy, &w.ApprovedBy, &w.ApprovedAt, &w.Note,
		&w.CreatedAt, &w.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("promos: approve read: %w", err)
	}
	if w.Status != StatusPending {
		return nil, ErrNotPending
	}
	if w.CreatedBy == approverID {
		return nil, ErrFourEyes
	}
	if s.now().Sub(w.CreatedAt) > ApprovalWindow {
		return nil, ErrTooLate
	}
	if !w.EndsAt.After(s.now()) {
		return nil, ErrBadWindow
	}

	tag, err := tx.Exec(ctx,
		`UPDATE fee_tiers
		    SET promo_until     = $2,
		        promo_maker_bps = COALESCE($3, promo_maker_bps),
		        promo_taker_bps = COALESCE($4, promo_taker_bps)
		  WHERE id = $1`,
		w.FeeTierID, w.EndsAt, maker, taker)
	if err != nil {
		return nil, fmt.Errorf("promos: apply to tier: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrTierMissing
	}
	if _, err := tx.Exec(ctx,
		`UPDATE fee_promo_windows
		    SET status=$2, approved_by=$3, approved_at=$4, updated_at=now()
		  WHERE id=$1`,
		windowID, StatusApplied, approverID, s.now().UTC()); err != nil {
		return nil, fmt.Errorf("promos: mark applied: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("promos: approve commit: %w", err)
	}
	return s.Get(ctx, windowID)
}

// Reject moves a pending window to REJECTED (no tier write).
func (s *Store) Reject(ctx context.Context, windowID, approverID int64, reason *string) (*Window, error) {
	w, err := s.Get(ctx, windowID)
	if err != nil {
		return nil, err
	}
	if w.Status != StatusPending {
		return nil, ErrNotPending
	}
	if w.CreatedBy == approverID {
		return nil, ErrFourEyes
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE fee_promo_windows
		    SET status=$2, approved_by=$3, approved_at=$4,
		        note=COALESCE($5, note), updated_at=now()
		  WHERE id=$1 AND status='PENDING_APPROVAL'`,
		windowID, StatusRejected, approverID, s.now().UTC(), reason)
	if err != nil {
		return nil, fmt.Errorf("promos: reject: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotPending
	}
	return s.Get(ctx, windowID)
}

// ClearExpired flips applied windows whose ends_at has passed to EXPIRED
// and clears promo_until on their tiers when the tier's promo_until is
// still that window's end (a later overlapping window wins — the guard
// compares the tier's stored promo_until to this window's ends_at).
// Called periodically by the wiring layer; returns windows expired.
func (s *Store) ClearExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE fee_tiers ft
		    SET promo_until = NULL, promo_maker_bps = NULL, promo_taker_bps = NULL
		   FROM fee_promo_windows w
		  WHERE w.status='APPLIED' AND w.ends_at <= now()
		    AND ft.id = w.fee_tier_id
		    AND ft.promo_until = w.ends_at`)
	if err != nil {
		return 0, fmt.Errorf("promos: clear expired tiers: %w", err)
	}
	tag2, err := s.pool.Exec(ctx,
		`UPDATE fee_promo_windows SET status='EXPIRED', updated_at=now()
		  WHERE status='APPLIED' AND ends_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("promos: mark expired: %w", err)
	}
	// Pending windows that outlived their approval window also expire —
	// they can never be approved after ErrTooLate.
	tag3, err := s.pool.Exec(ctx,
		`UPDATE fee_promo_windows SET status='EXPIRED', updated_at=now()
		  WHERE status='PENDING_APPROVAL' AND created_at < now() - interval '15 minutes'`)
	if err != nil {
		return 0, fmt.Errorf("promos: expire stale pending: %w", err)
	}
	return tag.RowsAffected() + tag2.RowsAffected() + tag3.RowsAffected(), nil
}
