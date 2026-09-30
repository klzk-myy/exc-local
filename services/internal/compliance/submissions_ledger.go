// SubmissionsLedger — the migration-059 regulatory_submissions transport
// ledger + wire log (Task 21.3.16, spec §5.36): one durable row per
// outbound vendor submission, plus an append-only request/response log.
// The artifact being transported lives in regulatory_report_submissions
// (054); the ledger records the wire attempt, ACK/NACK state, repair
// state and resubmission history.
package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// TransportRow is one regulatory_submissions ledger row.
type TransportRow struct {
	ID                  int64           `json:"id"`
	ReportSubmissionID  int64           `json:"report_submission_id,omitempty"`
	ReportEventID       int64           `json:"report_event_id,omitempty"`
	TradeID             int64           `json:"trade_id,omitempty"`
	RegulationType      string          `json:"regulation_type"`
	DestinationType     string          `json:"destination_type"`
	DestinationEndpoint string          `json:"destination_endpoint"`
	BatchID             string          `json:"batch_id,omitempty"`
	Payload             json.RawMessage `json:"payload"`
	PayloadXML          string          `json:"payload_xml,omitempty"`
	PayloadHash         string          `json:"payload_hash"`
	SchemaName          string          `json:"schema_name,omitempty"`
	SchemaVersion       string          `json:"schema_version,omitempty"`
	Attempt             int             `json:"attempt"`
	AckStatus           string          `json:"ack_status"`
	ErrorCode           string          `json:"error_code,omitempty"`
	ErrorDetail         string          `json:"error_detail,omitempty"`
	ExternalRef         string          `json:"external_ref,omitempty"`
	RepairAttempts      int             `json:"repair_attempts"`
	NextAttemptAt       *time.Time      `json:"next_attempt_at,omitempty"`
	LastErrorAt         *time.Time      `json:"last_error_at,omitempty"`
	RepairedBy          int64           `json:"repaired_by,omitempty"`
	RepairedAt          *time.Time      `json:"repaired_at,omitempty"`
	SubmittedAt         *time.Time      `json:"submitted_at,omitempty"`
	ResolvedAt          *time.Time      `json:"resolved_at,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// Transport ack states (migration 059 CHECK constraint).
const (
	TkPending   = "PENDING"
	TkSubmitted = "SUBMITTED"
	TkAcked     = "ACK"
	TkNacked    = "NACK"
	TkRepairing = "REPAIRING"
	TkResolved  = "RESOLVED"
	TkFailed    = "FAILED"
)

// SubmissionsLedger implements the 059 transport ledger.
type SubmissionsLedger struct {
	Pool *pgxpool.Pool
}

// NewSubmissionsLedger wires the store; nil pool fails closed.
func NewSubmissionsLedger(pool *pgxpool.Pool) (*SubmissionsLedger, error) {
	if pool == nil {
		return nil, fmt.Errorf("regreport: nil pg pool for transport ledger")
	}
	return &SubmissionsLedger{Pool: pool}, nil
}

func hashBody(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Insert lands a transport row (per artifact attempt) + wire OUT log.
// Idempotent-ish: the (report_submission_id, attempt) unique key
// surfaces 23505 — the dispatcher treats that as "already ledgered".
func (l *SubmissionsLedger) Insert(ctx context.Context, r *TransportRow) error {
	return pgx.BeginFunc(ctx, l.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO regulatory_submissions
			    (report_submission_id, report_event_id, trade_id,
			     regulation_type, destination_type, destination_endpoint,
			     batch_id, payload, payload_xml, payload_hash,
			     schema_name, schema_version, attempt, ack_status,
			     next_attempt_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			RETURNING id, created_at, updated_at`,
			nilID(r.ReportSubmissionID), nilID(r.ReportEventID),
			nilID(r.TradeID), r.RegulationType, r.DestinationType,
			r.DestinationEndpoint, nilStr2(r.BatchID), r.Payload,
			nilStr2(r.PayloadXML), r.PayloadHash,
			nilStr2(r.SchemaName), nilStr2(r.SchemaVersion), r.Attempt,
			r.AckStatus, r.NextAttemptAt).Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt)
		if err != nil {
			return fmt.Errorf("regreport: insert transport: %w", err)
		}
		if len(r.Payload) > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO regulatory_submission_log
				    (submission_id, direction, body_hash, body, endpoint)
				VALUES ($1,'OUT',$2,$3,$4)`,
				r.ID, hashBody(r.Payload), r.Payload, r.DestinationEndpoint); err != nil {
				return fmt.Errorf("regreport: wire log out: %w", err)
			}
		}
		_, err = audit.Append(ctx, tx, "regulatory_submissions", &r.ID, "INSERT", nil)
		return err
	})
}

// LatestFor returns the newest transport row for an artifact (or nil).
func (l *SubmissionsLedger) LatestFor(ctx context.Context, reportSubmissionID int64) (*TransportRow, error) {
	r, err := scanTransport(l.Pool.QueryRow(ctx,
		`SELECT `+transportCols+` FROM regulatory_submissions
		 WHERE report_submission_id=$1 ORDER BY attempt DESC, id DESC LIMIT 1`,
		reportSubmissionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("regreport: latest transport: %w", err)
	}
	return r, nil
}

// AppendResponse logs a wire IN frame (verdict body) — immutable.
func (l *SubmissionsLedger) AppendResponse(ctx context.Context, transportID int64,
	httpStatus int, body []byte, endpoint string) error {
	var doc json.RawMessage
	if json.Unmarshal(body, &doc) == nil && len(doc) > 0 {
		// parsed body
	} else {
		doc, _ = json.Marshal(map[string]any{"raw": string(body[:min(len(body), 2048)])})
	}
	_, err := l.Pool.Exec(ctx, `
		INSERT INTO regulatory_submission_log
		    (submission_id, direction, http_status, body_hash, body, endpoint)
		VALUES ($1,'IN',$2,$3,$4,$5)`,
		transportID, httpStatus, hashBody(body), doc, endpoint)
	if err != nil {
		return fmt.Errorf("regreport: wire log in: %w", err)
	}
	return nil
}

// SetVerdict records the parsed verdict on the transport row.
func (l *SubmissionsLedger) SetVerdict(ctx context.Context, id int64,
	ackStatus, code, detail, ref string, at time.Time) error {
	tag, err := l.Pool.Exec(ctx, `
		UPDATE regulatory_submissions
		   SET ack_status=$2, error_code=$3, error_detail=$4,
		       external_ref=COALESCE($5, external_ref),
		       resolved_at=CASE WHEN $2 IN ('ACK','NACK','RESOLVED') THEN $6
		                    ELSE resolved_at END,
		       submitted_at=COALESCE(submitted_at, $6),
		       updated_at=now()
		 WHERE id=$1`,
		id, ackStatus, nilStr2(code), nilStr2(detail), nilStr2(ref), at)
	if err != nil {
		return fmt.Errorf("regreport: verdict %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("transport row %d not found", id))
	}
	return nil
}

// FailAttempt records a transport-level failure with backoff
// (store-and-forward, cap 2h per §14.9 item 3).
func (l *SubmissionsLedger) FailAttempt(ctx context.Context, id int64,
	code, detail string, nextAttemptAt time.Time, at time.Time) error {
	_, err := l.Pool.Exec(ctx, `
		UPDATE regulatory_submissions
		   SET ack_status='FAILED', error_code=$2, error_detail=$3,
		       next_attempt_at=$4, last_error_at=$5,
		       repair_attempts=repair_attempts+1, updated_at=now()
		 WHERE id=$1`, id, nilStr2(code), nilStr2(detail), nextAttemptAt, at)
	if err != nil {
		return fmt.Errorf("regreport: fail %d: %w", id, err)
	}
	return nil
}

// ListTransport — transport-ledger inspection feed (?status= filter;
// empty = all), newest first.
func (l *SubmissionsLedger) ListTransport(ctx context.Context, status string, limit int) ([]TransportRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT ` + transportCols + ` FROM regulatory_submissions`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" WHERE ack_status=$%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := l.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("regreport: transport list: %w", err)
	}
	defer rows.Close()
	var out []TransportRow
	for rows.Next() {
		r, err := scanTransport(rows)
		if err != nil {
			return nil, fmt.Errorf("regreport: transport scan: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// FindTransportByRef resolves a transport row by the vendor's external
// ref (async callback correlation).
func (l *SubmissionsLedger) FindTransportByRef(ctx context.Context, ref string) (*TransportRow, error) {
	r, err := scanTransport(l.Pool.QueryRow(ctx,
		`SELECT `+transportCols+` FROM regulatory_submissions
		 WHERE external_ref=$1 ORDER BY id DESC LIMIT 1`, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("regreport: ref %s: %w", ref, err)
	}
	return r, nil
}

// MarkRepairing moves a NACKED row into the officer repair flow.
func (l *SubmissionsLedger) MarkRepairing(ctx context.Context, id int64,
	adminUserID int64, clientIP string) error {
	return pgx.BeginFunc(ctx, l.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE regulatory_submissions
			   SET ack_status='REPAIRING', repaired_by=$2,
			       repaired_at=now(), updated_at=now()
			 WHERE id=$1 AND ack_status IN ('NACK','FAILED')`,
			id, adminUserID)
		if err != nil {
			return fmt.Errorf("regreport: mark repairing: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("transport %d not in a repairable state", id))
		}
		_, _, err = admin.Log(ctx, tx, admin.AuditEntry{
			AdminUserID: adminUserID,
			Action:      "regreport.transport_repair",
			TargetType:  "regulatory_submission",
			TargetID:    &id,
			AfterState:  map[string]any{"ack_status": TkRepairing},
			IPAddress:   clientIP,
		})
		return err
	})
}

// RepairQueue — Compliance Officer queue: NACKED/FAILED transport rows
// plus the joined artifact + event keys, oldest SLA pressure first.
func (l *SubmissionsLedger) RepairQueue(ctx context.Context, limit int) ([]RepairQueueItem, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := l.Pool.Query(ctx, `
		SELECT rs.id, rs.report_submission_id, rs.report_event_id,
		       rs.trade_id, rs.regulation_type, rs.destination_type,
		       rs.ack_status, rs.error_code, rs.error_detail,
		       rs.external_ref, rs.repair_attempts, rs.next_attempt_at,
		       rs.submitted_at, rs.created_at,
		       e.uti, e.instrument_code
		  FROM regulatory_submissions rs
		  LEFT JOIN regulatory_report_events e
		    ON e.event_id = rs.report_event_id
		 WHERE rs.ack_status IN ('NACK','FAILED','REPAIRING')
		 ORDER BY rs.created_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("regreport: repair queue: %w", err)
	}
	defer rows.Close()
	var out []RepairQueueItem
	for rows.Next() {
		var it RepairQueueItem
		var rsID, evID, tid *int64
		var ecode, edetail, ref *string
		var next, sub *time.Time
		var uti, icode *string
		if err := rows.Scan(&it.TransportID, &rsID, &evID, &tid,
			&it.RegulationType, &it.DestinationType, &it.AckStatus,
			&ecode, &edetail, &ref, &it.RepairAttempts, &next, &sub,
			&it.CreatedAt, &uti, &icode); err != nil {
			return nil, fmt.Errorf("regreport: queue scan: %w", err)
		}
		if rsID != nil {
			it.ReportSubmissionID = *rsID
		}
		if evID != nil {
			it.EventID = *evID
		}
		if tid != nil {
			it.TradeID = *tid
		}
		it.ErrorCode, it.ErrorDetail = deref(ecode), deref(edetail)
		it.ExternalRef = deref(ref)
		it.UTI, it.InstrumentCode = deref(uti), deref(icode)
		it.NextAttemptAt = next
		it.SubmittedAt = sub
		out = append(out, it)
	}
	return out, rows.Err()
}

// RepairQueueItem is one repair-queue row for the officer UI.
type RepairQueueItem struct {
	TransportID        int64      `json:"transport_id"`
	ReportSubmissionID int64      `json:"report_submission_id"`
	EventID            int64      `json:"event_id"`
	TradeID            int64      `json:"trade_id"`
	RegulationType     string     `json:"regulation_type"`
	DestinationType    string     `json:"destination_type"`
	AckStatus          string     `json:"ack_status"`
	ErrorCode          string     `json:"error_code,omitempty"`
	ErrorDetail        string     `json:"error_detail,omitempty"`
	ExternalRef        string     `json:"external_ref,omitempty"`
	RepairAttempts     int        `json:"repair_attempts"`
	NextAttemptAt      *time.Time `json:"next_attempt_at,omitempty"`
	SubmittedAt        *time.Time `json:"submitted_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UTI                string     `json:"uti,omitempty"`
	InstrumentCode     string     `json:"instrument_code,omitempty"`
}

const transportCols = `id, report_submission_id, report_event_id, trade_id,
	regulation_type, destination_type, destination_endpoint, batch_id,
	payload, payload_xml, payload_hash, schema_name, schema_version,
	attempt, ack_status, error_code, error_detail, external_ref,
	repair_attempts, next_attempt_at, last_error_at, repaired_by,
	repaired_at, submitted_at, resolved_at, created_at, updated_at`

func scanTransport(row pgx.Row) (*TransportRow, error) {
	var r TransportRow
	var rsID, evID, tid *int64
	var ep, batch, xmlBody, schemaName, schemaVer *string
	var ecode, edetail, ref *string
	var rby *int64
	err := row.Scan(&r.ID, &rsID, &evID, &tid, &r.RegulationType,
		&r.DestinationType, &ep, &batch, &r.Payload, &xmlBody,
		&r.PayloadHash, &schemaName, &schemaVer, &r.Attempt,
		&r.AckStatus, &ecode, &edetail, &ref, &r.RepairAttempts,
		&r.NextAttemptAt, &r.LastErrorAt, &rby, &r.RepairedAt,
		&r.SubmittedAt, &r.ResolvedAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if rsID != nil {
		r.ReportSubmissionID = *rsID
	}
	if evID != nil {
		r.ReportEventID = *evID
	}
	if tid != nil {
		r.TradeID = *tid
	}
	r.DestinationEndpoint = deref(ep)
	r.BatchID = deref(batch)
	r.PayloadXML = deref(xmlBody)
	r.SchemaName = deref(schemaName)
	r.SchemaVersion = deref(schemaVer)
	r.ErrorCode = deref(ecode)
	r.ErrorDetail = deref(edetail)
	r.ExternalRef = deref(ref)
	if rby != nil {
		r.RepairedBy = *rby
	}
	return &r, nil
}

func nilID(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nilStr2(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
