// PgxStore — the migration-273 persistence implementation of the
// incident lifecycle Store seam. Close's InTx runs SERIALIZABLE with a
// SELECT ... FOR UPDATE on the incident row, matching the settlement
// store convention: the gate evaluates a stable snapshot and the
// transition commits atomically.
package dora

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxStore implements Store over pgx.
type PgxStore struct{ Pool *pgxpool.Pool }

// NewPgxStore wires the store.
func NewPgxStore(pool *pgxpool.Pool) *PgxStore { return &PgxStore{Pool: pool} }

// InTx runs fn in a SERIALIZABLE transaction (ClsStore convention).
func (s *PgxStore) InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("dora tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxDoraTx{tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("dora tx commit: %w", err)
	}
	return nil
}

const incidentCols = `id, ref, severity, material, status, title, classification,
	rca_status, root_cause, lessons_learned, detected_at, acknowledged_at,
	mitigated_at, resolved_at, closed_at, closed_by, created_at`

func scanIncident(row pgx.Row) (*Incident, bool, error) {
	var in Incident
	var cls []byte
	err := row.Scan(&in.ID, &in.Ref, &in.Severity, &in.Material, &in.Status,
		&in.Title, &cls, &in.RCAStatus, &in.RootCause,
		&in.Lessons, &in.DetectedAt, &in.AckedAt, &in.MitigatedAt,
		&in.ResolvedAt, &in.ClosedAt, &in.ClosedBy, &in.CreatedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(cls) > 0 {
		if err := json.Unmarshal(cls, &in.Classification); err != nil {
			return nil, false, fmt.Errorf("dora: classification jsonb: %w", err)
		}
	}
	return &in, true, nil
}

// InsertIncident persists the incident and returns its id. Ref uniqueness
// is the dedup boundary (UNIQUE on incidents.ref).
func (s *PgxStore) InsertIncident(ctx context.Context, i *Incident) (int64, error) {
	cls, err := json.Marshal(i.Classification)
	if err != nil {
		return 0, fmt.Errorf("dora: marshal classification: %w", err)
	}
	var id int64
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO incidents
		  (ref, severity, material, status, title, classification,
		   rca_status, root_cause, lessons_learned, detected_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id`,
		i.Ref, i.Severity, i.Material, i.Status, i.Title, cls,
		i.RCAStatus, i.RootCause, i.Lessons, i.DetectedAt).Scan(&id)
	return id, err
}

// GetIncident loads the incident by id.
func (s *PgxStore) GetIncident(ctx context.Context, id int64) (*Incident, bool, error) {
	return scanIncident(s.Pool.QueryRow(ctx,
		`SELECT `+incidentCols+` FROM incidents WHERE id = $1`, id))
}

// ListReports returns the incident's regulator-report rows.
func (s *PgxStore) ListReports(ctx context.Context, incidentID int64) ([]Report, error) {
	return listReports(ctx, s.Pool, incidentID)
}

// ListRemediations returns the incident's remediation items.
func (s *PgxStore) ListRemediations(ctx context.Context, incidentID int64) ([]Remediation, error) {
	return listRemediations(ctx, s.Pool, incidentID)
}

// InsertReport registers the report obligation row.
func (s *PgxStore) InsertReport(ctx context.Context, r *Report) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO incident_regulator_reports
		  (incident_id, kind, regulator, due_at, evidence_ref)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id`,
		r.IncidentID, r.Kind, r.Regulator, r.DueAt, r.EvidenceRef).Scan(&id)
	return id, err
}

// MarkReportSubmitted stamps submission; an already-submitted kind is a
// no-op (resubmission idempotency — returns true either way when the
// row exists).
func (s *PgxStore) MarkReportSubmitted(ctx context.Context, incidentID int64,
	kind string, by int64, at time.Time, evidenceRef string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE incident_regulator_reports
		   SET submitted_at = COALESCE(submitted_at, $3),
		       submitted_by = COALESCE(submitted_by, $4),
		       evidence_ref = CASE WHEN submitted_at IS NULL THEN $5
		                           ELSE evidence_ref END
		 WHERE incident_id = $1 AND kind = $2`,
		incidentID, kind, at, by, evidenceRef)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// InsertRemediation persists the corrective-action row.
func (s *PgxStore) InsertRemediation(ctx context.Context, m *Remediation) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO incident_remediations
		  (incident_id, action, owner, due_at, status)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id`, m.IncidentID, m.Action, m.Owner, m.DueAt, m.Status).Scan(&id)
	return id, err
}

// UpdateRemediationStatus applies the status transition; ACCEPTED stamps
// accepted_by/at, RESOLVED stamps resolved_at.
func (s *PgxStore) UpdateRemediationStatus(ctx context.Context, id int64,
	status string, at time.Time, actor int64) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE incident_remediations
		   SET status      = $2::varchar,
		       resolved_at = CASE WHEN $2::text IN ('RESOLVED','ACCEPTED')
		                          THEN COALESCE(resolved_at, $3) END,
		       accepted_by = CASE WHEN $2::text = 'ACCEPTED' THEN $4::bigint END,
		       accepted_at = CASE WHEN $2::text = 'ACCEPTED' THEN $3 END
		 WHERE id = $1`, id, status, at, actor)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateIncidentRCA stores the post-mortem fields.
func (s *PgxStore) UpdateIncidentRCA(ctx context.Context, id int64,
	rcaStatus, rootCause, lessons string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE incidents
		   SET rca_status = $2, root_cause = $3, lessons_learned = $4,
		       updated_at = now()
		 WHERE id = $1`, id, rcaStatus, rootCause, lessons)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// --- transactional view -----------------------------------------------------

type pgxDoraTx struct{ tx pgx.Tx }

// LockIncident SELECTs the row FOR UPDATE — the gate evaluates against a
// row no concurrent writer can mutate inside this transaction.
func (t pgxDoraTx) LockIncident(ctx context.Context, id int64) (*Incident, bool, error) {
	return scanIncident(t.tx.QueryRow(ctx,
		`SELECT `+incidentCols+` FROM incidents WHERE id = $1 FOR UPDATE`, id))
}

func (t pgxDoraTx) ListReports(ctx context.Context, incidentID int64) ([]Report, error) {
	return listReports(ctx, t.tx, incidentID)
}

func (t pgxDoraTx) ListRemediations(ctx context.Context, incidentID int64) ([]Remediation, error) {
	return listRemediations(ctx, t.tx, incidentID)
}

// SetClosed performs the CLOSED transition.
func (t pgxDoraTx) SetClosed(ctx context.Context, id, actor int64, at time.Time) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE incidents
		   SET status = 'CLOSED', closed_at = $2, closed_by = $3,
		       updated_at = now()
		 WHERE id = $1 AND status <> 'CLOSED'`, id, at, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("dora: incident %d not transitioned", id)
	}
	return nil
}

// --- shared row scanners -----------------------------------------------------

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func listReports(ctx context.Context, q querier, incidentID int64) ([]Report, error) {
	rows, err := q.Query(ctx, `
		SELECT id, incident_id, kind, regulator, due_at, submitted_at,
		       submitted_by, approved_at, approved_by, evidence_ref
		  FROM incident_regulator_reports
		 WHERE incident_id = $1 ORDER BY id`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.ID, &r.IncidentID, &r.Kind, &r.Regulator,
			&r.DueAt, &r.SubmittedAt, &r.SubmittedBy, &r.ApprovedAt,
			&r.ApprovedBy, &r.EvidenceRef); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func listRemediations(ctx context.Context, q querier, incidentID int64) ([]Remediation, error) {
	rows, err := q.Query(ctx, `
		SELECT id, incident_id, action, owner, due_at, status,
		       resolved_at, accepted_by, accepted_at
		  FROM incident_remediations
		 WHERE incident_id = $1 ORDER BY id`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Remediation
	for rows.Next() {
		var m Remediation
		if err := rows.Scan(&m.ID, &m.IncidentID, &m.Action, &m.Owner,
			&m.DueAt, &m.Status, &m.ResolvedAt, &m.AcceptedBy,
			&m.AcceptedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
