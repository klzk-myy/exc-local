// Package webhooks implements the Phase-05 Task 5.3.17 signed outbound
// webhook pipeline — the single owner of webhook delivery for the whole
// platform (ownership pinned 2026-09-27, remediation #35; Phase-14 Task
// 14.3.12's divergent retry schedule and second dead-letter table are
// superseded and consume this pipeline).
//
// Contract:
//   - endpoints are registered per account with an event allowlist
//     (order_filled, order_cancelled, deposit_confirmed,
//     withdrawal_completed — spec §12.4/Task 5.3.17);
//   - every delivery is POSTed with an HMAC-SHA256 signature over
//     "<timestamp>.<body>" in X-Webhook-Signature, plus
//     X-Webhook-Timestamp / X-Webhook-Event / X-Webhook-Delivery-Id;
//   - failures retry with bounded exponential backoff (1s, 2s, 4s, 8s,
//     16s; max 5 attempts) — a delivery that exhausts its attempts lands
//     in the dead-letter set (status DEAD_LETTERED, queryable);
//   - endpoint secrets are stored AES-256-GCM wrapped (same SecretBox
//     envelope as api_keys.secret_enc, migration 025) and rotate with a
//     bounded predecessor-overlap window (≤72h, mirroring §8.8 key
//     rotation).
//
// Delivery failures are recorded in webhook_deliveries (migration 180),
// which is simultaneously the delivery log and the dead-letter queue.
package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ValidEvents is the Task 5.3.17 event vocabulary. Unknown events are
// rejected at registration — fail-closed, never silently subscribed.
var ValidEvents = map[string]bool{
	"order_filled":         true,
	"order_cancelled":      true,
	"deposit_confirmed":    true,
	"withdrawal_completed": true,
}

// ErrEndpointNotFound marks lookup failures — the handler maps it to
// 404 (and uses it to keep foreign endpoints indistinguishable from
// unknown ones).
var ErrEndpointNotFound = errors.New("webhooks: endpoint not found")

// Endpoint status values.
const (
	StatusActive   = "ACTIVE"
	StatusDisabled = "DISABLED"
)

// MaxSecretOverlap bounds secret_overlap_until on a rotated endpoint
// secret — the same 72h bound as API-key rotation (§8.8).
const MaxSecretOverlap = 72 * time.Hour

// SecretBox seals/unseals the per-endpoint signing secret; satisfied by
// auth.SecretBox. Kept as an interface so this package does not import
// the auth cluster — the wiring layer adapts it. Required: a nil box
// fails closed at registration.
type SecretBox interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(blob []byte) ([]byte, error)
}

// Endpoint is one webhook_endpoints row.
type Endpoint struct {
	ID                 int64      `json:"id"`
	EndpointID         string     `json:"endpoint_id"`
	AccountID          int64      `json:"account_id"`
	URL                string     `json:"url"`
	Events             []string   `json:"events"`
	Status             string     `json:"status"`
	SecretOverlapUntil *time.Time `json:"secret_overlap_until,omitempty"`
	CreatedBy          *int64     `json:"created_by,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	DisabledAt         *time.Time `json:"disabled_at,omitempty"`
}

// SubscribedTo reports whether the endpoint wants event.
func (e Endpoint) SubscribedTo(event string) bool {
	for _, ev := range e.Events {
		if ev == event {
			return true
		}
	}
	return false
}

// Store persists webhook_endpoints rows.
type Store struct {
	pool *pgxpool.Pool
	box  SecretBox
	now  func() time.Time
}

// NewStore binds the store. box is required — a nil box fails closed at
// the first registration attempt.
func NewStore(pool *pgxpool.Pool, box SecretBox) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("webhooks: nil pool")
	}
	return &Store{pool: pool, box: box, now: time.Now}, nil
}

// StoreForTest overrides the clock; tests only.
func (s *Store) StoreForTest(now func() time.Time) { s.now = now }

func newPublicID(prefix string) (string, error) {
	raw := make([]byte, 21)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("webhooks: id generation: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// validateEvents rejects unknown/empty event subscriptions.
func validateEvents(events []string) error {
	if len(events) == 0 {
		return fmt.Errorf("webhooks: at least one event is required")
	}
	seen := map[string]bool{}
	for _, e := range events {
		if !ValidEvents[e] {
			return fmt.Errorf("webhooks: unknown event %q (valid: order_filled, order_cancelled, deposit_confirmed, withdrawal_completed)", e)
		}
		if seen[e] {
			return fmt.Errorf("webhooks: duplicate event %q", e)
		}
		seen[e] = true
	}
	return nil
}

// Register creates an endpoint and returns it plus the plaintext signing
// secret — shown exactly once in the API response and never stored raw.
func (s *Store) Register(ctx context.Context, accountID, createdBy int64, url string, events []string) (*Endpoint, string, error) {
	if s.box == nil {
		return nil, "", fmt.Errorf("webhooks: secret box not configured")
	}
	if accountID <= 0 {
		return nil, "", fmt.Errorf("webhooks: account id must be positive")
	}
	if err := validateEvents(events); err != nil {
		return nil, "", err
	}
	if err := validateURL(url); err != nil {
		return nil, "", err
	}
	secret, err := newPublicID("s") // 28-char urlsafe secret
	if err != nil {
		return nil, "", err
	}
	enc, err := s.box.Seal([]byte(secret))
	if err != nil {
		return nil, "", fmt.Errorf("webhooks: seal secret: %w", err)
	}
	endpointID, err := newPublicID("wh_")
	if err != nil {
		return nil, "", err
	}
	var createdByArg any
	if createdBy > 0 {
		createdByArg = createdBy
	}
	var id int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO webhook_endpoints
		 (endpoint_id, account_id, url, events, secret_enc, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		endpointID, accountID, url, events, enc, createdByArg).Scan(&id)
	if err != nil {
		return nil, "", fmt.Errorf("webhooks: insert endpoint: %w", err)
	}
	ep, err := s.Get(ctx, endpointID)
	if err != nil {
		return nil, "", err
	}
	return ep, secret, nil
}

// urlRe mirrors the DB CHECK: http(s) with no whitespace.
func validateURL(url string) error {
	if len(url) < 9 || len(url) > 2048 {
		return fmt.Errorf("webhooks: url length out of bounds")
	}
	if url[:7] != "http://" && url[:8] != "https://" {
		return fmt.Errorf("webhooks: url must be http(s)")
	}
	for _, r := range url {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return fmt.Errorf("webhooks: url must not contain whitespace")
		}
	}
	return nil
}

const endpointCols = `id, endpoint_id, account_id, url, events, status,
	secret_overlap_until, created_by, created_at, disabled_at`

func scanEndpoint(scan func(dest ...any) error) (*Endpoint, error) {
	var e Endpoint
	var createdBy *int64
	err := scan(&e.ID, &e.EndpointID, &e.AccountID, &e.URL, &e.Events,
		&e.Status, &e.SecretOverlapUntil, &createdBy, &e.CreatedAt, &e.DisabledAt)
	if err != nil {
		return nil, err
	}
	e.CreatedBy = createdBy
	return &e, nil
}

// Get returns the endpoint by public id, regardless of state.
func (s *Store) Get(ctx context.Context, endpointID string) (*Endpoint, error) {
	e, err := scanEndpoint(s.pool.QueryRow(ctx,
		`SELECT `+endpointCols+` FROM webhook_endpoints WHERE endpoint_id=$1`,
		endpointID).Scan)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("%w: %q", ErrEndpointNotFound, endpointID)
	}
	if err != nil {
		return nil, fmt.Errorf("webhooks: endpoint read: %w", err)
	}
	return e, nil
}

// endpointByInternalID resolves a webhook_endpoints.id — used by the
// dispatcher, which works from claimed delivery rows.
func (s *Store) endpointByInternalID(ctx context.Context, id int64) (*Endpoint, error) {
	e, err := scanEndpoint(s.pool.QueryRow(ctx,
		`SELECT `+endpointCols+` FROM webhook_endpoints WHERE id=$1`, id).Scan)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("%w: id %d", ErrEndpointNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("webhooks: endpoint read: %w", err)
	}
	return e, nil
}

// List returns all endpoints for an account, newest first.
func (s *Store) List(ctx context.Context, accountID int64) ([]Endpoint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+endpointCols+` FROM webhook_endpoints
		  WHERE account_id=$1 ORDER BY id DESC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list: %w", err)
	}
	defer rows.Close()
	out := []Endpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// Disable marks the endpoint DISABLED (deliveries stop at the next
// attempt cycle). Owned check: only the owning account may disable.
func (s *Store) Disable(ctx context.Context, endpointID string, accountID int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE webhook_endpoints
		    SET status=$3, disabled_at=now(), updated_at=now()
		  WHERE endpoint_id=$1 AND account_id=$2 AND status='ACTIVE'`,
		endpointID, accountID, StatusDisabled)
	if err != nil {
		return fmt.Errorf("webhooks: disable: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %q (or already disabled)", ErrEndpointNotFound, endpointID)
	}
	return nil
}

// RotateSecret issues a new signing secret and records the predecessor
// valid until now+overlap (0 = immediate switch; ≤72h). Deliveries in
// flight sign with the current secret; consumers verifying during the
// overlap may accept either (documented in the delivery headers via
// X-Webhook-Signature-Prev for overlapping deliveries — see dispatch).
// Returns the new plaintext secret (shown once).
func (s *Store) RotateSecret(ctx context.Context, endpointID string, accountID int64, overlap time.Duration) (string, error) {
	if s.box == nil {
		return "", fmt.Errorf("webhooks: secret box not configured")
	}
	if overlap < 0 || overlap > MaxSecretOverlap {
		return "", fmt.Errorf("webhooks: rotation overlap must be within 0..72h")
	}
	secret, err := newPublicID("s")
	if err != nil {
		return "", err
	}
	enc, err := s.box.Seal([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("webhooks: seal secret: %w", err)
	}
	var until *time.Time
	if overlap > 0 {
		u := s.now().UTC().Add(overlap)
		until = &u
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE webhook_endpoints
		    SET prev_secret_enc=secret_enc, secret_enc=$3,
		        secret_overlap_until=$4, updated_at=now()
		  WHERE endpoint_id=$1 AND account_id=$2 AND status='ACTIVE'`,
		endpointID, accountID, enc, until)
	if err != nil {
		return "", fmt.Errorf("webhooks: rotate secret: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", fmt.Errorf("%w: %q (or disabled)", ErrEndpointNotFound, endpointID)
	}
	return secret, nil
}

// ---------------------------------------------------------------------------
// Deliveries — the store side of the dispatcher's queue.
// ---------------------------------------------------------------------------

// Delivery status values.
const (
	DeliveryPending      = "PENDING"
	DeliveryDelivered    = "DELIVERED"
	DeliveryDeadLettered = "DEAD_LETTERED"
)

// Delivery is one webhook_deliveries row.
type Delivery struct {
	ID             int64      `json:"id"`
	DeliveryID     string     `json:"delivery_id"`
	EndpointID     int64      `json:"endpoint_id"`
	AccountID      int64      `json:"account_id"`
	Event          string     `json:"event"`
	Payload        []byte     `json:"payload"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	LastStatusCode *int       `json:"last_status_code,omitempty"`
	LastError      *string    `json:"last_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
}

// Enqueue writes a PENDING delivery for every ACTIVE endpoint on the
// account subscribed to event. Called by the dispatcher's Publish path;
// domain emitters call Dispatcher.Publish, not this.
func (s *Store) Enqueue(ctx context.Context, accountID int64, event string, payload []byte) (int, error) {
	if !ValidEvents[event] {
		return 0, fmt.Errorf("webhooks: unknown event %q", event)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM webhook_endpoints
		  WHERE account_id=$1 AND status='ACTIVE' AND events @> ARRAY[$2]::text[]`,
		accountID, event)
	if err != nil {
		return 0, fmt.Errorf("webhooks: resolve endpoints: %w", err)
	}
	var epIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("webhooks: endpoint scan: %w", err)
		}
		epIDs = append(epIDs, id)
	}
	rows.Close()
	for _, epID := range epIDs {
		deliveryID, err := newPublicID("whd_")
		if err != nil {
			return 0, err
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO webhook_deliveries
			 (delivery_id, endpoint_id, account_id, event, payload)
			 VALUES ($1,$2,$3,$4,$5)`,
			deliveryID, epID, accountID, event, string(payload)); err != nil {
			return 0, fmt.Errorf("webhooks: enqueue delivery: %w", err)
		}
	}
	return len(epIDs), nil
}

const deliveryCols = `d.id, d.delivery_id, d.endpoint_id, d.account_id, d.event,
	d.payload, d.status, d.attempts, d.max_attempts, d.next_attempt_at,
	d.last_status_code, d.last_error, d.created_at, d.delivered_at`

func scanDelivery(scan func(dest ...any) error) (*Delivery, error) {
	var d Delivery
	err := scan(&d.ID, &d.DeliveryID, &d.EndpointID, &d.AccountID, &d.Event,
		&d.Payload, &d.Status, &d.Attempts, &d.MaxAttempts, &d.NextAttemptAt,
		&d.LastStatusCode, &d.LastError, &d.CreatedAt, &d.DeliveredAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// claimLease bounds how long a claimed delivery is in-flight before the
// row becomes claimable again (dead-worker reclamation path).
const claimLease = 60 * time.Second

// ClaimDue atomically leases up to limit due PENDING deliveries for a
// worker: attempts is incremented and next_attempt_at is pushed out by
// claimLease inside the same transaction, so a later poll cannot
// double-deliver a row the first worker is still POSTing, while a
// crashed worker's deliveries return to due after the lease expires.
// FOR UPDATE SKIP LOCKED makes concurrent dispatchers safe.
func (s *Store) ClaimDue(ctx context.Context, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("webhooks: claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`WITH due AS (
		    SELECT id FROM webhook_deliveries
		     WHERE status='PENDING' AND next_attempt_at <= now()
		     ORDER BY id
		     FOR UPDATE SKIP LOCKED
		     LIMIT $1
		 )
		 UPDATE webhook_deliveries d
		    SET attempts = d.attempts + 1,
		        next_attempt_at = now() + interval '60 seconds'
		   FROM due WHERE d.id = due.id
		 RETURNING d.id, d.delivery_id, d.endpoint_id, d.account_id, d.event,
		           d.payload, d.status, d.attempts, d.max_attempts,
		           d.next_attempt_at, d.last_status_code, d.last_error,
		           d.created_at, d.delivered_at`, limit)
	if err != nil {
		return nil, fmt.Errorf("webhooks: claim due: %w", err)
	}
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("webhooks: claim scan: %w", err)
		}
		out = append(out, *d)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("webhooks: claim commit: %w", err)
	}
	return out, nil
}

// CompleteDelivery records a successful attempt (attempts was already
// incremented at claim).
func (s *Store) CompleteDelivery(ctx context.Context, deliveryID int64, statusCode int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE webhook_deliveries
		    SET status=$2, last_status_code=$3,
		        last_error=NULL, delivered_at=now()
		  WHERE id=$1 AND status='PENDING'`,
		deliveryID, DeliveryDelivered, statusCode)
	if err != nil {
		return fmt.Errorf("webhooks: complete delivery: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("webhooks: delivery %d not pending", deliveryID)
	}
	return nil
}

// FailDelivery records a failed attempt (attempts was already
// incremented at claim): reschedules at next or, once attempts have
// reached max_attempts, moves the row to DEAD_LETTERED — the
// dead-letter queue.
func (s *Store) FailDelivery(ctx context.Context, deliveryID int64, statusCode int, cause string, next time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE webhook_deliveries
		    SET status=CASE WHEN attempts >= max_attempts THEN 'DEAD_LETTERED' ELSE 'PENDING' END,
		        next_attempt_at=$2, last_status_code=$3, last_error=$4
		  WHERE id=$1 AND status='PENDING'`,
		deliveryID, next, nullableCode(statusCode), nullableErr(cause))
	if err != nil {
		return fmt.Errorf("webhooks: fail delivery: %w", err)
	}
	return nil
}

func nullableCode(code int) any {
	if code <= 0 {
		return nil
	}
	return code
}

func nullableErr(e string) any {
	if e == "" {
		return nil
	}
	if len(e) > 512 {
		e = e[:512]
	}
	return e
}

// ListDeliveries returns an endpoint's deliveries (newest first, limit
// 500) — the delivery-log view behind GET /api/v1/webhooks/{id}/deliveries.
func (s *Store) ListDeliveries(ctx context.Context, endpointID string, accountID int64, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	ep, err := s.Get(ctx, endpointID)
	if err != nil {
		return nil, err
	}
	if ep.AccountID != accountID {
		return nil, fmt.Errorf("%w: %q", ErrEndpointNotFound, endpointID) // no cross-account existence oracle
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+deliveryCols+` FROM webhook_deliveries d
		  WHERE d.endpoint_id=$1 ORDER BY d.id DESC LIMIT $2`,
		ep.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list deliveries: %w", err)
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ActiveEndpointsFor returns every ACTIVE endpoint on accountID
// subscribed to event — the dispatcher's fan-out set.
func (s *Store) ActiveEndpointsFor(ctx context.Context, accountID int64, event string) ([]Endpoint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+endpointCols+` FROM webhook_endpoints
		  WHERE account_id=$1 AND status='ACTIVE' AND events @> ARRAY[$2]::text[]
		  ORDER BY id`, accountID, event)
	if err != nil {
		return nil, fmt.Errorf("webhooks: endpoints for event: %w", err)
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ErrDeliveryNotFound marks dead-letter/retransmit lookups that missed —
// the admin handler maps it to 404 (no cross-account existence oracle).
var ErrDeliveryNotFound = errors.New("webhooks: delivery not found")

// ListDeadLetters returns DEAD_LETTERED deliveries, oldest first — the
// admin dead-letter review view (Phase-14 Task 14.3.12). Joined to the
// endpoint row so the officer sees the failing URL without a second
// query.
func (s *Store) ListDeadLetters(ctx context.Context, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+deliveryCols+` FROM webhook_deliveries d
		  WHERE d.status=$1 ORDER BY d.id ASC LIMIT $2`,
		DeliveryDeadLettered, limit)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list dead letters: %w", err)
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// Retransmit moves one DEAD_LETTERED delivery back to PENDING with a
// fresh attempt budget (attempts=0, immediate next_attempt_at) — the
// manual admin retry path (Phase-14 Task 14.3.12). last_status_code /
// last_error keep the dead-letter diagnostic until the next attempt
// overwrites them; the requeue decision lands in admin_audit_log
// inside the same transaction as the status flip.
func (s *Store) Retransmit(ctx context.Context, deliveryID string,
	adminUserID int64) (*Delivery, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("webhooks: retransmit tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx,
		`UPDATE webhook_deliveries d
		    SET status='PENDING', attempts=0, next_attempt_at=now()
		  WHERE d.delivery_id=$1 AND d.status='DEAD_LETTERED'
		  RETURNING `+deliveryCols, deliveryID)
	if err != nil {
		return nil, fmt.Errorf("webhooks: retransmit: %w", err)
	}
	var out *Delivery
	for rows.Next() {
		rd, err := scanDelivery(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		out = rd
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhooks: retransmit: %w", err)
	}
	if out == nil {
		return nil, fmt.Errorf("%w: %q", ErrDeliveryNotFound, deliveryID)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_audit_log
		     (admin_user_id, action, target_type, target_id, after_state)
		 VALUES ($1,'webhook.retransmit','webhook_delivery',$2,$3)`,
		adminUserID, out.ID,
		fmt.Sprintf(`{"delivery_id":%q,"endpoint_id":%d,"event":%q}`,
			out.DeliveryID, out.EndpointID, out.Event)); err != nil {
		return nil, fmt.Errorf("webhooks: retransmit audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("webhooks: retransmit commit: %w", err)
	}
	return out, nil
}

// SecretsForDelivery unwraps the signing secrets to try for a delivery:
// current secret first, then the predecessor while inside its overlap
// window. Callers sign with the current secret; the predecessor is kept
// for verification-side documentation and future dual-signature headers.
func (s *Store) SecretsForDelivery(ctx context.Context, endpointID int64) (current, previous []byte, err error) {
	var cur, prev []byte
	var until *time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT secret_enc, prev_secret_enc, secret_overlap_until
		   FROM webhook_endpoints WHERE id=$1`, endpointID).
		Scan(&cur, &prev, &until)
	if err != nil {
		return nil, nil, fmt.Errorf("webhooks: secret read: %w", err)
	}
	if s.box == nil {
		return nil, nil, fmt.Errorf("webhooks: secret box not configured")
	}
	curRaw, err := s.box.Open(cur)
	if err != nil {
		return nil, nil, fmt.Errorf("webhooks: unseal secret: %w", err)
	}
	if len(prev) > 0 && until != nil && s.now().Before(*until) {
		prevRaw, err := s.box.Open(prev)
		if err != nil {
			return nil, nil, fmt.Errorf("webhooks: unseal prev secret: %w", err)
		}
		previous = prevRaw
	}
	return curRaw, previous, nil
}

// Events returns the sorted allowlist — used by the OpenAPI doc and
// registration error messages.
func Events() []string {
	out := make([]string, 0, len(ValidEvents))
	for e := range ValidEvents {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
