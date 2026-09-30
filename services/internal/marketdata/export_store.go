// Phase-23 Task 23.3.2 — export_jobs persistence.
//
// Table: export_jobs (migration 256). Lifecycle:
// PENDING → RUNNING → COMPLETED | FAILED. PENDING rows are claimed with
// a single-statement UPDATE ... FOR UPDATE SKIP LOCKED so concurrent
// workers never double-render; COMPLETED rows missing notified_at are
// the re-notify scan for the link-email leg.
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// exportJobCols is the scan projection (status cast to text).
const exportJobCols = `id, account_id, kind, symbol, interval, format,
	from_ts, to_ts, row_limit, status::text, object_ref, row_count, sha256,
	truncated, error, expires_at, notified_at, created_at, started_at,
	finished_at`

// PgJobStore is the production JobStore over pgxpool.
type PgJobStore struct {
	Pool *pgxpool.Pool
}

// NewPgJobStore wraps a pool.
func NewPgJobStore(pool *pgxpool.Pool) *PgJobStore { return &PgJobStore{Pool: pool} }

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	var from, to, notified, started, finished *time.Time
	var objectRef, sha, errText *string
	var rowCount *int64
	if err := row.Scan(&j.ID, &j.AccountID, &j.Kind, &j.Symbol, &j.Interval,
		&j.Format, &from, &to, &j.RowLimit, &j.Status, &objectRef,
		&rowCount, &sha, &j.Truncated, &errText, &j.ExpiresAt, &notified,
		&j.CreatedAt, &started, &finished); err != nil {
		return nil, err
	}
	if from != nil {
		j.From = from.UTC()
	}
	if to != nil {
		j.To = to.UTC()
	}
	if objectRef != nil {
		j.ObjectRef = *objectRef
	}
	if rowCount != nil {
		j.RowCount = *rowCount
	}
	if sha != nil {
		j.SHA256 = *sha
	}
	if errText != nil {
		j.Error = *errText
	}
	j.NotifiedAt = notified
	j.StartedAt = started
	j.FinishedAt = finished
	return &j, nil
}

// Insert implements JobStore — writes the PENDING row.
func (s *PgJobStore) Insert(ctx context.Context, j Job) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO export_jobs
		    (account_id, kind, symbol, interval, format, from_ts, to_ts,
		     row_limit, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'PENDING',now(),now())
		RETURNING id`,
		j.AccountID, j.Kind, j.Symbol, j.Interval, j.Format,
		nullTime(j.From), nullTime(j.To), j.RowLimit).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("marketdata: insert export job: %w", err)
	}
	return id, nil
}

// Get implements JobStore — (nil, nil) on miss.
func (s *PgJobStore) Get(ctx context.Context, id int64) (*Job, error) {
	j, err := scanJob(s.Pool.QueryRow(ctx, `
		SELECT `+exportJobCols+` FROM export_jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("marketdata: export job %d: %w", id, err)
	}
	return j, nil
}

// ListForAccount implements JobStore — (created_at,id) DESC keyset.
func (s *PgJobStore) ListForAccount(ctx context.Context, accountID int64,
	afterCreatedAt time.Time, afterID int64, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + exportJobCols + ` FROM export_jobs WHERE account_id = $1`
	args := []any{accountID}
	if !afterCreatedAt.IsZero() {
		q += ` AND (created_at, id) < ($2, $3)`
		args = append(args, afterCreatedAt, afterID)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("marketdata: list export jobs acct %d: %w", accountID, err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("marketdata: scan export job: %w", err)
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// ClaimPending implements JobStore — one-statement atomic claims; the
// SKIP LOCKED sub-select is the multi-worker contract. Each claim is a
// single row (LIMIT 1) so the update never blocks on a held row; the
// loop repeats until the batch fills or the queue drains.
func (s *PgJobStore) ClaimPending(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 10
	}
	const claimSQL = `
		UPDATE export_jobs
		SET status = 'RUNNING', started_at = now(), updated_at = now()
		WHERE id = (
		    SELECT id FROM export_jobs
		    WHERE status = 'PENDING'
		    ORDER BY id
		    FOR UPDATE SKIP LOCKED
		    LIMIT 1
		)
		RETURNING ` + exportJobCols
	var out []Job
	for len(out) < limit {
		j, err := scanJob(s.Pool.QueryRow(ctx, claimSQL))
		if errors.Is(err, pgx.ErrNoRows) {
			break // queue drained
		}
		if err != nil {
			return out, fmt.Errorf("marketdata: claim export jobs: %w", err)
		}
		out = append(out, *j)
	}
	return out, nil
}

// Complete implements JobStore — RUNNING→COMPLETED stamps the artifact
// ref, sha256, row count and the 24h link expiry.
func (s *PgJobStore) Complete(ctx context.Context, id int64, c Completion) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE export_jobs
		SET status = 'COMPLETED', object_ref = $2, row_count = $3,
		    sha256 = $4, truncated = $5, expires_at = $6,
		    finished_at = $7, updated_at = now(), error = NULL
		WHERE id = $1 AND status = 'RUNNING'`,
		id, c.ObjectRef, c.RowCount, c.SHA256, c.Truncated,
		c.ExpiresAt, c.At)
	if err != nil {
		return fmt.Errorf("marketdata: complete export job %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("marketdata: export job %d not RUNNING", id)
	}
	return nil
}

// Fail implements JobStore — RUNNING→FAILED, terminal.
func (s *PgJobStore) Fail(ctx context.Context, id int64, errMsg string, at time.Time) error {
	if len(errMsg) > 4000 {
		errMsg = errMsg[:4000]
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE export_jobs
		SET status = 'FAILED', error = $2, finished_at = $3, updated_at = now()
		WHERE id = $1 AND status = 'RUNNING'`, id, errMsg, at)
	if err != nil {
		return fmt.Errorf("marketdata: fail export job %d: %w", id, err)
	}
	return nil
}

// MarkNotified implements JobStore.
func (s *PgJobStore) MarkNotified(ctx context.Context, id int64, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE export_jobs SET notified_at = $2, updated_at = now()
		WHERE id = $1 AND status = 'COMPLETED'`, id, at)
	if err != nil {
		return fmt.Errorf("marketdata: notify-stamp export job %d: %w", id, err)
	}
	return nil
}

// Unnotified implements JobStore — COMPLETED rows whose link email has
// not landed yet (oldest first).
func (s *PgJobStore) Unnotified(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+exportJobCols+` FROM export_jobs
		WHERE status = 'COMPLETED' AND notified_at IS NULL
		ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("marketdata: unnotified export jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("marketdata: scan unnotified job: %w", err)
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// MemJobStore — in-memory JobStore for unit tests (mirrors the reporting
// MemConfirmationStore pattern; not for production wiring).
// ---------------------------------------------------------------------------

// MemJobStore is the in-memory JobStore.
type MemJobStore struct {
	mu   sync.Mutex
	next int64
	rows map[int64]*Job
	// FailErr forces Insert/Get failures (degradation tests).
	FailErr error
}

// NewMemJobStore builds the empty store.
func NewMemJobStore() *MemJobStore {
	return &MemJobStore{next: 1, rows: map[int64]*Job{}}
}

// Insert implements JobStore.
func (m *MemJobStore) Insert(_ context.Context, j Job) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailErr != nil {
		return 0, m.FailErr
	}
	j.ID = m.next
	m.next++
	if j.Status == "" {
		j.Status = JobPending
	}
	cp := j
	m.rows[j.ID] = &cp
	return j.ID, nil
}

// Get implements JobStore.
func (m *MemJobStore) Get(_ context.Context, id int64) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailErr != nil {
		return nil, m.FailErr
	}
	j := m.rows[id]
	if j == nil {
		return nil, nil
	}
	cp := *j
	return &cp, nil
}

// ListForAccount implements JobStore — (created_at,id) DESC keyset.
func (m *MemJobStore) ListForAccount(_ context.Context, accountID int64,
	afterCreatedAt time.Time, afterID int64, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailErr != nil {
		return nil, m.FailErr
	}
	if limit <= 0 {
		limit = 100
	}
	var out []Job
	for _, j := range m.rows {
		if j.AccountID != accountID {
			continue
		}
		if !afterCreatedAt.IsZero() {
			if j.CreatedAt.After(afterCreatedAt) ||
				(j.CreatedAt.Equal(afterCreatedAt) && j.ID >= afterID) {
				continue
			}
		}
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].ID > out[b].ID
		}
		return out[a].CreatedAt.After(out[b].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ClaimPending implements JobStore — marks up to limit oldest PENDING
// rows RUNNING in FIFO order.
func (m *MemJobStore) ClaimPending(_ context.Context, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailErr != nil {
		return nil, m.FailErr
	}
	if limit <= 0 {
		limit = 10
	}
	var ids []int64
	for id, j := range m.rows {
		if j.Status == JobPending {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	var out []Job
	now := time.Now().UTC()
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		j := m.rows[id]
		j.Status = JobRunning
		j.StartedAt = &now
		out = append(out, *j)
	}
	return out, nil
}

// Complete implements JobStore.
func (m *MemJobStore) Complete(_ context.Context, id int64, c Completion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.rows[id]
	if j == nil || j.Status != JobRunning {
		return fmt.Errorf("marketdata: export job %d not RUNNING", id)
	}
	j.Status = JobCompleted
	j.ObjectRef = c.ObjectRef
	j.RowCount = c.RowCount
	j.SHA256 = c.SHA256
	j.Truncated = c.Truncated
	j.Error = ""
	exp := c.ExpiresAt.UTC()
	j.ExpiresAt = &exp
	at := c.At.UTC()
	j.FinishedAt = &at
	return nil
}

// Fail implements JobStore.
func (m *MemJobStore) Fail(_ context.Context, id int64, errMsg string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.rows[id]
	if j == nil || j.Status != JobRunning {
		return fmt.Errorf("marketdata: export job %d not RUNNING", id)
	}
	j.Status = JobFailed
	j.Error = errMsg
	t := at.UTC()
	j.FinishedAt = &t
	return nil
}

// MarkNotified implements JobStore.
func (m *MemJobStore) MarkNotified(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.rows[id]
	if j == nil || j.Status != JobCompleted {
		return fmt.Errorf("marketdata: export job %d not COMPLETED", id)
	}
	t := at.UTC()
	j.NotifiedAt = &t
	return nil
}

// Unnotified implements JobStore.
func (m *MemJobStore) Unnotified(_ context.Context, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.rows {
		if j.Status == JobCompleted && j.NotifiedAt == nil {
			out = append(out, *j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
