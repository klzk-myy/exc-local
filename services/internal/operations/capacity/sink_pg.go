// PgSink — the capacity_reports artifact store (migration 274). One
// row per quarterly run: the full report as JSONB plus the columns ops
// dashboards query directly (period, worst action, action counts).
package capacity

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgSink implements Sink over pgx.
type PgSink struct{ Pool *pgxpool.Pool }

// NewPgSink wires the sink.
func NewPgSink(pool *pgxpool.Pool) *PgSink { return &PgSink{Pool: pool} }

// Store persists the report; a sink failure must surface (the caller
// turns it into INTERNAL_ERROR) — quarterly evidence that never lands
// is a compliance gap, not a log line.
func (s *PgSink) Store(ctx context.Context, r *Report) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	actions, err := json.Marshal(r.Actions)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO capacity_reports
		  (report_id, quarter, period_start, period_end, generated_at,
		   horizon_days, worst_action, resources_count, actions, report)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		r.ReportID, r.Quarter, r.PeriodStart, r.PeriodEnd, r.GeneratedAt,
		r.HorizonDays, r.WorstAction, len(r.Resources), actions, payload)
	return err
}
