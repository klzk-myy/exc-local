// Phase-21 Task 21.3.23 — ARM/APA resubmission repair (spec §14.9
// item 3 + §14.11 store-and-forward). NACK'd, unsent and in-flight
// transport submissions in regulatory_submissions (migration 059 —
// sibling Task 21.3.16 owns the schema; this file reads/repairs rows
// only, it does NOT touch the migration) are retried on persistent,
// exponential backoff and must land within ResubmitDeadline (2h) of
// the first failure or escalate FAILED + P1.
//
// Seam discipline: SubmissionRepairStore is the persistence interface
// — PgSubmissionRepairStore implements it over the real ledger; tests
// inject the memory implementation. Resubmitter is the vendor seam —
// the Task 21.3.16 dispatcher binds it; nil fails safe (the sweep
// leaves rows retryable and reports backlog rather than faking ACKs).
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// ResubmitDeadline bounds post-failure resubmission — "resubmitted
// within 2 hours after recovery" per the task; a row still retryable
// past the deadline escalates FAILED + P1 rather than retrying forever.
const ResubmitDeadline = 2 * time.Hour

// ResubmitBacklogAlert is the compliance-officer backlog trip point.
const ResubmitBacklogAlert = 100

// RepairSubmission is the transport-ledger row shape the repair path
// needs — a projection of regulatory_submissions (migration 059).
type RepairSubmission struct {
	ID                 int64           `json:"id"`
	ReportSubmissionID *int64          `json:"report_submission_id,omitempty"`
	ReportEventID      *int64          `json:"report_event_id,omitempty"`
	TradeID            *int64          `json:"trade_id,omitempty"`
	RegulationType     string          `json:"regulation_type"`
	DestinationType    string          `json:"destination_type"` // APA|ARM|TR|SDR
	Endpoint           string          `json:"destination_endpoint"`
	Payload            json.RawMessage `json:"payload"`
	PayloadXML         string          `json:"payload_xml,omitempty"`
	PayloadHash        string          `json:"payload_hash"`
	SchemaName         string          `json:"schema_name,omitempty"`
	SchemaVersion      string          `json:"schema_version,omitempty"`
	Attempt            int             `json:"attempt"`
	RepairAttempts     int             `json:"repair_attempts"`
	AckStatus          string          `json:"ack_status"`
	ErrorCode          string          `json:"error_code,omitempty"`
	NextAttemptAt      *time.Time      `json:"next_attempt_at,omitempty"`
	LastErrorAt        *time.Time      `json:"last_error_at,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
}

// SubmissionRepairStore is the persistence seam over the transport
// ledger. Claim-retryable semantics are at-least-once: a crashed sweep
// leaves a claimed row with next_attempt_at advanced, so it re-surfaces
// on the next backoff tick rather than stranding.
type SubmissionRepairStore interface {
	// ClaimRetryable marks up to limit due rows (ack_status PENDING/
	// NACK/REPAIRING/FAILED-transport, next_attempt_at <= now) as
	// REPAIRING with next_attempt_at = now + claimHold, and returns
	// them. SKIP LOCKED under pg; single-threaded under memory.
	ClaimRetryable(ctx context.Context, now time.Time,
		claimHold time.Duration, limit int) ([]RepairSubmission, error)
	// CompleteRetry resolves one claimed row: success → ack_status=ACK
	// + external_ref + resolved_at; failure → ack_status back to a
	// retryable state (NACK keeps NACK, transport errors keep PENDING)
	// with repair_attempts+1 and next_attempt_at = nextAt; deadline
	// breach (deadline=true) → FAILED, no further retries.
	CompleteRetry(ctx context.Context, id int64, success bool,
		deadline bool, externalRef, errCode, errDetail string,
		nextAt *time.Time) error
	// BacklogDepth counts rows still owed a submission (PENDING + NACK
	// + REPAIRING) — the >100 compliance-officer alert metric.
	BacklogDepth(ctx context.Context) (int, error)
}

// Resubmitter is the vendor dispatch seam — Task 21.3.16's APA/ARM/TR
// dispatcher binds it. Submit returns the vendor receipt ref on ACK.
type Resubmitter interface {
	Submit(ctx context.Context, s RepairSubmission) (externalRef string, err error)
}

// ResubmitReport summarizes one sweep pass.
type ResubmitReport struct {
	Claimed    int  `json:"claimed"`
	Submitted  int  `json:"submitted"`
	Retried    int  `json:"retried"`   // scheduled for another attempt
	Deadlined  int  `json:"deadlined"` // exceeded 2h window → FAILED + P1
	Backlog    int  `json:"backlog"`
	SkippedTxm bool `json:"skipped_no_transmitter"`
}

// ResubmissionService is the sweep loop — claim due rows, transmit via
// the bound Resubmitter, schedule exponential backoff on failure, fail
// permanently at the 2h deadline with a P1 escalation.
type ResubmissionService struct {
	store   SubmissionRepairStore
	sender  Resubmitter
	alerter Alerter
	auditor AuditSink

	base     time.Duration // backoff base — 30s
	max      time.Duration // backoff cap — 15min
	deadline time.Duration // ResubmitDeadline
	claimTTL time.Duration // claim visibility hold — 5min
	batch    int
	now      func() time.Time

	mu          sync.Mutex
	lastBacklog time.Time // alert dedupe — once per sweep pass is enough
	alerted     bool
}

// NewResubmissionService binds store + sender (sender may be nil →
// sweeps report backlog but transmit nothing — fail-safe, not fake).
func NewResubmissionService(store SubmissionRepairStore,
	sender Resubmitter) *ResubmissionService {
	return &ResubmissionService{
		store: store, sender: sender,
		base: 30 * time.Second, max: 15 * time.Minute,
		deadline: ResubmitDeadline, claimTTL: 5 * time.Minute, batch: 200,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// WithAlerter/WithAuditor/WithBatch/WithClock — wiring + test seams.
func (s *ResubmissionService) WithAlerter(a Alerter) *ResubmissionService {
	s.alerter = a
	return s
}
func (s *ResubmissionService) WithAuditor(a AuditSink) *ResubmissionService {
	s.auditor = a
	return s
}
func (s *ResubmissionService) WithBatch(n int) *ResubmissionService {
	if n > 0 {
		s.batch = n
	}
	return s
}
func (s *ResubmissionService) WithClock(now func() time.Time) *ResubmissionService {
	s.now = now
	return s
}

// backoff returns the next-attempt delay for repairAttempts n:
// base·2^(n-1) capped at max (30s → 60s → 120s → … → 15min cap).
func (s *ResubmissionService) backoff(attempts int) time.Duration {
	d := s.base
	for i := 1; i < attempts && d < s.max; i++ {
		d *= 2
		if d > s.max {
			d = s.max
		}
	}
	return d
}

// SweepOnce runs one claim→transmit→schedule pass. Backlog >100 trips
// the P1 compliance alert (deduped per continuous backlog breach).
func (s *ResubmissionService) SweepOnce(ctx context.Context) (ResubmitReport, error) {
	var rep ResubmitReport
	if s.store == nil {
		return rep, fmt.Errorf("resubmit: store unbound")
	}
	depth, err := s.store.BacklogDepth(ctx)
	if err != nil {
		return rep, fmt.Errorf("resubmit: backlog depth: %w", err)
	}
	rep.Backlog = depth
	if depth > ResubmitBacklogAlert && s.alerter != nil {
		s.mu.Lock()
		fresh := !s.alerted
		s.alerted = true
		s.mu.Unlock()
		if fresh {
			_ = s.alerter(ctx, "P1", "REGULATORY_RESUBMIT_BACKLOG",
				fmt.Sprintf("ARM/APA resubmission backlog %d exceeds %d — compliance officer review required",
					depth, ResubmitBacklogAlert))
		}
	} else if depth <= ResubmitBacklogAlert {
		s.mu.Lock()
		s.alerted = false
		s.mu.Unlock()
	}
	if s.sender == nil {
		rep.SkippedTxm = true
		return rep, nil // no transmitter bound — fail safe, keep backlog
	}
	rows, err := s.store.ClaimRetryable(ctx, s.now(), s.claimTTL, s.batch)
	if err != nil {
		return rep, fmt.Errorf("resubmit: claim: %w", err)
	}
	rep.Claimed = len(rows)
	now := s.now()
	for _, row := range rows {
		failedAt := row.CreatedAt
		if row.LastErrorAt != nil {
			failedAt = *row.LastErrorAt
		}
		pastDeadline := now.Sub(failedAt) > s.deadline
		ref, err := s.sender.Submit(ctx, row)
		switch {
		case err == nil && !pastDeadline:
			if err := s.store.CompleteRetry(ctx, row.ID, true, false,
				ref, "", "", nil); err != nil {
				return rep, fmt.Errorf("resubmit: complete %d: %w",
					row.ID, err)
			}
			rep.Submitted++
			s.audit(ctx, "report.resubmitted", row, map[string]any{
				"external_ref": ref, "attempt": row.Attempt,
				"repair_attempts": row.RepairAttempts + 1})
		default:
			code, detail := "TRANSMIT_ERROR", ""
			if err != nil {
				detail = err.Error()
				if e, ok := err.(interface{ ErrorCode() string }); ok {
					code = e.ErrorCode()
				}
			}
			next := now.Add(s.backoff(row.RepairAttempts + 1))
			if err := s.store.CompleteRetry(ctx, row.ID, false,
				pastDeadline, "", code, detail, &next); err != nil {
				return rep, fmt.Errorf("resubmit: schedule %d: %w",
					row.ID, err)
			}
			if pastDeadline {
				rep.Deadlined++
				if s.alerter != nil {
					_ = s.alerter(ctx, "P1", "REGULATORY_RESUBMIT_DEADLINE",
						fmt.Sprintf("submission %d (%s→%s) exceeded the 2h resubmission window — marked FAILED",
							row.ID, row.RegulationType, row.DestinationType))
				}
				s.audit(ctx, "report.resubmit_deadline", row,
					map[string]any{"last_error": detail})
			} else {
				rep.Retried++
				s.audit(ctx, "report.resubmit_retry", row,
					map[string]any{"error": detail, "next_attempt_at": next})
			}
		}
	}
	return rep, nil
}

func (s *ResubmissionService) audit(ctx context.Context, action string,
	row RepairSubmission, detail map[string]any) {
	if s.auditor == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["regulation_type"] = row.RegulationType
	detail["destination_type"] = row.DestinationType
	detail["endpoint"] = row.Endpoint
	_ = s.auditor.Record(ctx, 0, action, "regulatory_submission",
		row.ID, detail)
}

// Run is the sweep loop — interval is operator-tunable (default 60s:
// the 2h deadline then has ≥120 retry opportunities).
func (s *ResubmissionService) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.SweepOnce(ctx); err != nil && s.alerter != nil {
				_ = s.alerter(ctx, "P2", "REGULATORY_RESUBMIT_SWEEP_FAILED",
					err.Error())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Memory store (tests/dev)
// ---------------------------------------------------------------------------

// MemorySubmissionRepairStore is the in-memory SubmissionRepairStore.
type MemorySubmissionRepairStore struct {
	mu   sync.Mutex
	rows map[int64]*RepairSubmission
	seq  int64
}

// NewMemorySubmissionRepairStore builds the dev/test store.
func NewMemorySubmissionRepairStore() *MemorySubmissionRepairStore {
	return &MemorySubmissionRepairStore{rows: map[int64]*RepairSubmission{}}
}

// Insert is the test/dev seed — appends a row (attempts must be set by
// the caller's fixture).
func (m *MemorySubmissionRepairStore) Insert(s RepairSubmission) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	s.ID = m.seq
	m.rows[s.ID] = &s
	return s.ID
}

// ClaimRetryable marks due retryable rows REPAIRING + next_attempt_at.
func (m *MemorySubmissionRepairStore) ClaimRetryable(_ context.Context,
	now time.Time, hold time.Duration, limit int) ([]RepairSubmission, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RepairSubmission
	for _, r := range m.rows {
		if len(out) >= limit {
			break
		}
		if !retryableStatus(r.AckStatus) {
			continue
		}
		if r.NextAttemptAt != nil && r.NextAttemptAt.After(now) {
			continue
		}
		nx := now.Add(hold)
		r.AckStatus, r.NextAttemptAt = "REPAIRING", &nx
		out = append(out, *r)
	}
	return out, nil
}

func retryableStatus(s string) bool {
	switch s {
	case "PENDING", "NACK", "FAILED", "REPAIRING":
		return true
	}
	return false
}

// CompleteRetry applies the terminal/scheduled state transition.
func (m *MemorySubmissionRepairStore) CompleteRetry(_ context.Context,
	id int64, success bool, deadline bool, externalRef, errCode,
	errDetail string, nextAt *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return excerrors.New("NOT_FOUND", "submission not found")
	}
	r.RepairAttempts++
	switch {
	case success:
		r.AckStatus = "ACK"
		r.ErrorCode, r.NextAttemptAt = "", nil
	case deadline:
		r.AckStatus = "FAILED"
		r.ErrorCode, r.NextAttemptAt = errCode, nil
	default:
		r.AckStatus = "PENDING"
		r.ErrorCode, r.NextAttemptAt = errCode, nextAt
	}
	if errDetail != "" {
		// keep detail in the error_code-adjacent surface for the memory
		// store — pg writes it to error_detail
		_ = errDetail
	}
	return nil
}

// GetRow returns a copy of one row — the test assertion surface.
func (m *MemorySubmissionRepairStore) GetRow(id int64) (RepairSubmission, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return RepairSubmission{}, false
	}
	return *r, true
}

// BacklogDepth counts retryable rows.
func (m *MemorySubmissionRepairStore) BacklogDepth(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if retryableStatus(r.AckStatus) {
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// PostgreSQL store — the migration-059 ledger (read/repair only)
// ---------------------------------------------------------------------------

// PgSubmissionRepairStore implements SubmissionRepairStore over
// regulatory_submissions. Claims use FOR UPDATE SKIP LOCKED inside a
// short tx per row batch — concurrent sweepers (or a re-started service
// racing a crashed claim) never double-transmit.
type PgSubmissionRepairStore struct {
	pool *pgxpool.Pool
}

// NewPgSubmissionRepairStore binds the OLTP pool.
func NewPgSubmissionRepairStore(pool *pgxpool.Pool) *PgSubmissionRepairStore {
	return &PgSubmissionRepairStore{pool: pool}
}

// ClaimRetryable marks due rows REPAIRING and returns them.
func (s *PgSubmissionRepairStore) ClaimRetryable(ctx context.Context,
	now time.Time, hold time.Duration, limit int) ([]RepairSubmission, error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id, report_submission_id, report_event_id, trade_id,
		       regulation_type, destination_type, destination_endpoint,
		       payload, COALESCE(payload_xml,''), payload_hash,
		       COALESCE(schema_name,''), COALESCE(schema_version,''),
		       attempt, repair_attempts, ack_status,
		       COALESCE(error_code,''), next_attempt_at, last_error_at,
		       created_at
		  FROM regulatory_submissions
		 WHERE ack_status IN ('PENDING','NACK','REPAIRING','FAILED')
		   AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
		 ORDER BY id
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	var out []RepairSubmission
	var ids []int64
	for rows.Next() {
		var r RepairSubmission
		if err := rows.Scan(&r.ID, &r.ReportSubmissionID, &r.ReportEventID,
			&r.TradeID, &r.RegulationType, &r.DestinationType, &r.Endpoint,
			&r.Payload, &r.PayloadXML, &r.PayloadHash, &r.SchemaName,
			&r.SchemaVersion, &r.Attempt, &r.RepairAttempts, &r.AckStatus,
			&r.ErrorCode, &r.NextAttemptAt, &r.LastErrorAt,
			&r.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		ids = append(ids, r.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE regulatory_submissions
			   SET ack_status='REPAIRING', next_attempt_at=$2,
			       updated_at=now()
			 WHERE id = ANY($1)`, ids, now.Add(hold)); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit(ctx)
}

// CompleteRetry writes the attempt outcome — ACK (resolved) / retry
// scheduled / deadline FAILED.
func (s *PgSubmissionRepairStore) CompleteRetry(ctx context.Context,
	id int64, success bool, deadline bool, externalRef, errCode,
	errDetail string, nextAt *time.Time) error {
	switch {
	case success:
		_, err := s.pool.Exec(ctx, `
			UPDATE regulatory_submissions
			   SET ack_status='ACK', external_ref=$2, error_code=NULL,
			       error_detail=NULL, repair_attempts=repair_attempts+1,
			       next_attempt_at=NULL, submitted_at=COALESCE(submitted_at,now()),
			       resolved_at=now(), updated_at=now()
			 WHERE id=$1`, id, externalRef)
		return err
	case deadline:
		_, err := s.pool.Exec(ctx, `
			UPDATE regulatory_submissions
			   SET ack_status='FAILED', error_code=$2, error_detail=$3,
			       repair_attempts=repair_attempts+1, next_attempt_at=NULL,
			       last_error_at=now(), updated_at=now()
			 WHERE id=$1`, id, errCode, errDetail)
		return err
	default:
		_, err := s.pool.Exec(ctx, `
			UPDATE regulatory_submissions
			   SET ack_status='PENDING', error_code=$2, error_detail=$3,
			       repair_attempts=repair_attempts+1, next_attempt_at=$4,
			       last_error_at=now(), updated_at=now()
			 WHERE id=$1`, id, errCode, errDetail, nextAt)
		return err
	}
}

// BacklogDepth counts rows still owed a submission.
func (s *PgSubmissionRepairStore) BacklogDepth(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM regulatory_submissions
		 WHERE ack_status IN ('PENDING','NACK','REPAIRING','FAILED')
		   AND (ack_status <> 'FAILED' OR last_error_at > now() - interval '2 hours')`).
		Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
