package reconciliation

// recon_store.go — Task 13.3.2 pgx persistence for the migration-207
// report surface (reconciliation_runs + reconciliation_findings) plus
// the read model the admin endpoints consume.
//
// File is Recon-prefixed: this package is concurrently owned by the
// Task 13.3.7 solvency machinery (merkle_tree.go, service.go,
// signers.go, store.go) — same coexistence convention as
// internal/recovery's Rec-/Orch- namespaces.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// PgStore is the production Store + the admin read model.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore wires the store over the shared pool.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

// InsertRun opens a RUNNING row.
func (s *PgStore) InsertRun(ctx context.Context, startedAt time.Time) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO reconciliation_runs (started_at, status)
		VALUES ($1, 'RUNNING') RETURNING id`, startedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("reconciliation_runs insert: %w", err)
	}
	return id, nil
}

// InsertFindings batch-inserts the sweep's findings in one round trip.
func (s *PgStore) InsertFindings(ctx context.Context, runID int64, fs []Finding) error {
	if len(fs) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, f := range fs {
		var detail []byte
		if len(f.Detail) > 0 {
			d, err := json.Marshal(f.Detail)
			if err != nil {
				return fmt.Errorf("finding detail marshal: %w", err)
			}
			detail = d
		}
		var exp, act, dlt *decimal.Decimal
		if f.Expected != nil {
			v := f.Expected.Round(8)
			exp = &v
		}
		if f.Actual != nil {
			v := f.Actual.Round(8)
			act = &v
		}
		if f.Delta != nil {
			v := f.Delta.Round(8)
			dlt = &v
		}
		var hs, ht *string
		if f.HaltScope != "" {
			hs = &f.HaltScope
			if f.HaltTarget != "" {
				ht = &f.HaltTarget
			}
		}
		b.Queue(`
			INSERT INTO reconciliation_findings
			    (run_id, category, subject, leg, expected, actual, delta,
			     unit, severity, halt_scope, halt_target, detail)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			runID, string(f.Category), f.Subject, f.Leg,
			exp, act, dlt, string(f.Unit), string(f.Severity),
			hs, ht, detail)
	}
	br := s.pool.SendBatch(ctx, b)
	defer br.Close()
	for range fs {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("reconciliation_findings insert: %w", err)
		}
	}
	return nil
}

// FinishRun finalizes the run row (status, counts, halts, finished_at).
func (s *PgStore) FinishRun(ctx context.Context, runID int64, status RunStatus,
	categories, mismatches, inconclusive int, halts []HaltRecord, runErr string) error {
	var hj []byte
	if len(halts) > 0 {
		b, err := json.Marshal(halts)
		if err != nil {
			return fmt.Errorf("halts marshal: %w", err)
		}
		hj = b
	}
	var errText *string
	if runErr != "" {
		errText = &runErr
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE reconciliation_runs
		   SET status = $2, finished_at = now(),
		       categories_checked = $3::int,
		       mismatch_count = $4::int, inconclusive_count = $5::int,
		       findings_count = ($4::int + $5::int) +
		           (SELECT count(*) FROM reconciliation_findings f
		             WHERE f.run_id = $1 AND f.severity = 'INFO'),
		       halts_emitted = $6, error = $7
		 WHERE id = $1`, runID, string(status), categories,
		mismatches, inconclusive, hj, errText)
	if err != nil {
		return fmt.Errorf("reconciliation_runs finish: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("reconciliation_runs finish: run %d not found", runID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Admin read model — GET /api/v1/admin/reconciliation/{latest,runs}
// ---------------------------------------------------------------------------

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	var fin *time.Time
	var hj []byte
	var errText *string
	err := row.Scan(&r.ID, &r.StartedAt, &fin, &r.Status,
		&r.CategoriesChecked, &r.FindingsCount, &r.MismatchCount,
		&r.InconclusiveCount, &hj, &errText)
	if err != nil {
		return nil, err
	}
	r.FinishedAt = fin
	if len(hj) > 0 {
		if err := json.Unmarshal(hj, &r.Halts); err != nil {
			return nil, fmt.Errorf("run %d halts decode: %w", r.ID, err)
		}
	}
	if errText != nil {
		r.Error = *errText
	}
	return &r, nil
}

const runCols = `id, started_at, finished_at, status, categories_checked,
	findings_count, mismatch_count, inconclusive_count, halts_emitted, error`

// LatestRun returns the newest finished (or in-flight) run plus its
// findings; (nil, nil) when the engine has never run.
func (s *PgStore) LatestRun(ctx context.Context) (*Run, []FindingRow, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runCols+` FROM reconciliation_runs ORDER BY id DESC LIMIT 1`))
	if err == pgx.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("latest run: %w", err)
	}
	fs, err := s.RunFindings(ctx, r.ID)
	if err != nil {
		return nil, nil, err
	}
	return r, fs, nil
}

// ListRuns returns the newest runs (bounded).
func (s *PgStore) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+runCols+` FROM reconciliation_runs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RunFindings returns one run's findings in insertion order.
func (s *PgStore) RunFindings(ctx context.Context, runID int64) ([]FindingRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, category, subject, leg, expected, actual, delta,
		       unit, severity, COALESCE(halt_scope,''), COALESCE(halt_target,''),
		       detail, created_at
		  FROM reconciliation_findings WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("run findings: %w", err)
	}
	defer rows.Close()
	var out []FindingRow
	for rows.Next() {
		var fr FindingRow
		var exp, act, dlt *decimal.Decimal
		var detail []byte
		err := rows.Scan(&fr.ID, &fr.RunID, &fr.Category, &fr.Subject, &fr.Leg,
			&exp, &act, &dlt, &fr.Unit, &fr.Severity,
			&fr.HaltScope, &fr.HaltTarget, &detail, &fr.CreatedAt)
		if err != nil {
			return nil, err
		}
		fr.Expected, fr.Actual, fr.Delta = exp, act, dlt
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &fr.Detail)
		}
		out = append(out, fr)
	}
	return out, rows.Err()
}
