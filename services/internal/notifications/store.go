package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Delivery is the notification_deliveries row (migration 028) — the
// §24 #100 per-channel tracking record.
type Delivery struct {
	ID          int64           `json:"id"`
	UserID      int64           `json:"user_id"`
	Channel     string          `json:"channel"`
	Event       string          `json:"event"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	LastError   *string         `json:"last_error,omitempty"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Store is the persistence seam the service and dispatcher need.
// PgStore implements it; tests substitute an in-memory fake.
type Store interface {
	InsertDelivery(ctx context.Context, d Delivery) (int64, error)
	// RecordAttempt persists the post-attempt count + last error
	// (status stays QUEUED while retries remain).
	RecordAttempt(ctx context.Context, id int64, attempts int, lastErr string) error
	// MarkDelivered sets DELIVERED + delivered_at.
	MarkDelivered(ctx context.Context, id int64, attempts int, at time.Time) error
	// MarkStatus sets a terminal/intermediate status with last_error.
	MarkStatus(ctx context.Context, id int64, status string, attempts int, lastErr string) error
	// DeadLetter marks the delivery DEAD_LETTERED and copies it into
	// notification_dead_letters atomically.
	DeadLetter(ctx context.Context, id int64, attempts int, lastErr string) error
	// GetPreferences returns the stored row or (nil, nil) — nil means
	// defaults, not an error.
	GetPreferences(ctx context.Context, userID int64) (*Preferences, error)
	// PutPreferences validates + upserts the row, returning the stored
	// projection (authoritative updated_at).
	PutPreferences(ctx context.Context, p *Preferences) (*Preferences, error)
	// RecentDeliveries supports tests + future support views.
	RecentDeliveries(ctx context.Context, userID int64, limit int) ([]Delivery, error)
}

// RecipientDirectory resolves the delivery address for address-bound
// channels (email → users.email, sms → users.phone).
type RecipientDirectory interface {
	Recipient(ctx context.Context, userID int64) (email, phone string, err error)
}

// PgStore is the production Store + RecipientDirectory over pgx.
type PgStore struct{ pool *pgxpool.Pool }

// NewPgStore builds the store on the shared pool.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

// InsertDelivery inserts a QUEUED delivery row and returns its id.
func (s *PgStore) InsertDelivery(ctx context.Context, d Delivery) (int64, error) {
	status := d.Status
	if status == "" {
		status = StatusQueued
	}
	max := d.MaxAttempts
	if max <= 0 {
		max = MaxAttempts
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO notification_deliveries
		    (user_id, channel, event, payload, status, max_attempts)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id`,
		d.UserID, d.Channel, d.Event, d.Payload, status, max).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("notifications: insert delivery: %w", err)
	}
	return id, nil
}

// RecordAttempt stores the current attempt count + last error.
func (s *PgStore) RecordAttempt(ctx context.Context, id int64, attempts int, lastErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE notification_deliveries
		   SET attempts = $2, last_error = NULLIF($3,''), updated_at = now()
		 WHERE id = $1`, id, attempts, lastErr)
	if err != nil {
		return fmt.Errorf("notifications: record attempt: %w", err)
	}
	return nil
}

// MarkDelivered transitions QUEUED → DELIVERED.
func (s *PgStore) MarkDelivered(ctx context.Context, id int64, attempts int, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE notification_deliveries
		   SET status = 'DELIVERED', attempts = $2, delivered_at = $3,
		       last_error = NULL, updated_at = now()
		 WHERE id = $1`, id, attempts, at.UTC())
	if err != nil {
		return fmt.Errorf("notifications: mark delivered: %w", err)
	}
	return nil
}

// MarkStatus writes a status + attempt/error bookkeeping.
func (s *PgStore) MarkStatus(ctx context.Context, id int64, status string, attempts int, lastErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE notification_deliveries
		   SET status = $2, attempts = $3, last_error = NULLIF($4,''), updated_at = now()
		 WHERE id = $1`, id, status, attempts, lastErr)
	if err != nil {
		return fmt.Errorf("notifications: mark %s: %w", status, err)
	}
	return nil
}

// DeadLetter terminalizes a delivery: status flip + dead-letter copy in
// one transaction — the task's notification_dead_letters record can
// never diverge from the delivery it summarizes.
func (s *PgStore) DeadLetter(ctx context.Context, id int64, attempts int, lastErr string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("notifications: dead-letter tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var d Delivery
	var last *string
	err = tx.QueryRow(ctx, `
		UPDATE notification_deliveries
		   SET status = 'DEAD_LETTERED', attempts = $2,
		       last_error = NULLIF($3,''), updated_at = now()
		 WHERE id = $1
		RETURNING user_id, channel, event, payload, last_error`,
		id, attempts, lastErr).
		Scan(&d.UserID, &d.Channel, &d.Event, &d.Payload, &last)
	if err != nil {
		return fmt.Errorf("notifications: dead-letter update: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO notification_dead_letters
		    (delivery_id, user_id, channel, event, payload, attempts, last_error)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, d.UserID, d.Channel, d.Event, d.Payload, attempts, last)
	if err != nil {
		return fmt.Errorf("notifications: dead-letter insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("notifications: dead-letter commit: %w", err)
	}
	return nil
}

// GetPreferences reads the per-user row; absent → (nil, nil).
func (s *PgStore) GetPreferences(ctx context.Context, userID int64) (*Preferences, error) {
	var matrix []byte
	var enabled bool
	var start, end *string
	var updatedAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT matrix, quiet_enabled,
		       to_char(quiet_start,'HH24:MI'), to_char(quiet_end,'HH24:MI'),
		       updated_at
		  FROM notification_preferences WHERE user_id = $1`, userID).
		Scan(&matrix, &enabled, &start, &end, &updatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("notifications: get preferences: %w", err)
	}
	p := &Preferences{UserID: userID, UpdatedAt: updatedAt}
	if len(matrix) > 0 {
		if err := json.Unmarshal(matrix, &p.Matrix); err != nil {
			return nil, fmt.Errorf("notifications: decode preference matrix: %w", err)
		}
	}
	if p.Matrix == nil {
		p.Matrix = map[string]map[string]bool{}
	}
	p.Quiet.Enabled = enabled
	if start != nil {
		p.Quiet.Start = *start
	}
	if end != nil {
		p.Quiet.End = *end
	}
	return p, nil
}

// PutPreferences validates then upserts one row (insert-or-replace on
// the user PK — the single-row design makes the whole PUT atomic).
func (s *PgStore) PutPreferences(ctx context.Context, p *Preferences) (*Preferences, error) {
	if p == nil || p.UserID <= 0 {
		return nil, fmt.Errorf("notifications: preferences require user_id")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	matrix, err := json.Marshal(p.Matrix)
	if err != nil {
		return nil, fmt.Errorf("notifications: encode matrix: %w", err)
	}
	var start, end *string
	if p.Quiet.Enabled {
		start = &p.Quiet.Start
		end = &p.Quiet.End
	}
	var updatedAt time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO notification_preferences
		    (user_id, matrix, quiet_enabled, quiet_start, quiet_end)
		VALUES ($1,$2,$3,$4::time,$5::time)
		ON CONFLICT (user_id) DO UPDATE
		   SET matrix = EXCLUDED.matrix,
		       quiet_enabled = EXCLUDED.quiet_enabled,
		       quiet_start = EXCLUDED.quiet_start,
		       quiet_end = EXCLUDED.quiet_end,
		       updated_at = now()
		RETURNING updated_at`,
		p.UserID, matrix, p.Quiet.Enabled, start, end).Scan(&updatedAt)
	if err != nil {
		return nil, fmt.Errorf("notifications: put preferences: %w", err)
	}
	out := *p
	out.UpdatedAt = updatedAt
	return &out, nil
}

// RecentDeliveries lists a user's latest deliveries (support/audit).
func (s *PgStore) RecentDeliveries(ctx context.Context, userID int64, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, channel, event, payload, status, attempts,
		       max_attempts, last_error, delivered_at, created_at
		  FROM notification_deliveries
		 WHERE user_id = $1 ORDER BY id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("notifications: recent deliveries: %w", err)
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.UserID, &d.Channel, &d.Event,
			&d.Payload, &d.Status, &d.Attempts, &d.MaxAttempts,
			&d.LastError, &d.DeliveredAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Recipient resolves the address columns from the users row.
func (s *PgStore) Recipient(ctx context.Context, userID int64) (string, string, error) {
	var email string
	var phone *string
	err := s.pool.QueryRow(ctx,
		`SELECT email, phone FROM users WHERE id = $1`, userID).
		Scan(&email, &phone)
	if err != nil {
		return "", "", fmt.Errorf("notifications: recipient lookup: %w", err)
	}
	if phone == nil {
		return email, "", nil
	}
	return email, *phone, nil
}
