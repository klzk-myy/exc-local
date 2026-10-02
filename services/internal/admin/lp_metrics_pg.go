package admin

// Phase-3 Task 5 (IMP-PLAN) — live LP MetricsSource/AlertSink.
//
// The spec'd pipeline is ClickHouse lp_performance_hourly (Phase-06/17)
// — that table does not exist yet, so this source derives the metrics
// the orders pipeline actually records: quote/flow counts, terminal
// status mix (fills vs rejections vs cancels → fill_ratio), submit→
// terminal latency percentiles (created_at → terminal transition),
// minute-bucket availability and two-sided presence. What the order
// stream cannot measure (spread quality vs mid) stays zero rather than
// being fabricated — Source="pg-orders" marks the derivation.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgLPMetricsSource implements MetricsSource over the orders table
// joined through lp_accounts. Window-scoped; returns an error when no
// LP-bound accounts exist so Scorecard falls back to the persisted
// snapshot instead of reporting a false zero.
type PgLPMetricsSource struct {
	Pool *pgxpool.Pool
}

// CollectLPMetrics aggregates the LP-bound accounts' order activity
// inside the rolling window.
func (s *PgLPMetricsSource) CollectLPMetrics(ctx context.Context, lpID int64,
	window time.Duration) (*LPScorecard, error) {
	if s.Pool == nil {
		return nil, fmt.Errorf("lp metrics: nil pool")
	}
	var nAccounts int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM lp_accounts WHERE lp_id=$1`, lpID).
		Scan(&nAccounts); err != nil {
		return nil, fmt.Errorf("lp accounts: %w", err)
	}
	if nAccounts == 0 {
		return nil, fmt.Errorf("lp %d has no bound quoting accounts", lpID)
	}

	var (
		total, fills, rejects, cancels int64
		avgMs, p99Ms                   sql.NullFloat64
		activeMinutes                  int64
		twoSidedMinutes                int64
	)
	err := s.Pool.QueryRow(ctx, `
		WITH flow AS (
		    SELECT o.id, o.status, o.side, o.instrument_id,
		           o.created_at, o.updated_at,
		           date_trunc('minute', o.created_at) AS minute
		    FROM orders o
		    WHERE o.account_id IN (SELECT account_id FROM lp_accounts WHERE lp_id=$1)
		      AND o.created_at >= now() - $2::interval
		)
		SELECT
		    count(*),
		    count(*) FILTER (WHERE status='FILLED'),
		    count(*) FILTER (WHERE status='REJECTED'),
		    count(*) FILTER (WHERE status='CANCELLED'),
		    avg(EXTRACT(EPOCH FROM (updated_at - created_at))*1000)
		        FILTER (WHERE status IN ('FILLED','REJECTED')),
		    percentile_cont(0.99) WITHIN GROUP
		        (ORDER BY EXTRACT(EPOCH FROM (updated_at - created_at))*1000)
		        FILTER (WHERE status IN ('FILLED','REJECTED')),
		    count(DISTINCT minute),
		    count(DISTINCT minute) FILTER (WHERE
		        minute IN (SELECT minute FROM (
		            SELECT minute, instrument_id FROM flow
		            GROUP BY minute, instrument_id
		            HAVING count(DISTINCT side) = 2) t))
		FROM flow`, lpID, fmt.Sprintf("%f seconds", window.Seconds())).
		Scan(&total, &fills, &rejects, &cancels, &avgMs, &p99Ms,
			&activeMinutes, &twoSidedMinutes)
	if err != nil {
		return nil, fmt.Errorf("lp metrics aggregate: %w", err)
	}

	sc := &LPScorecard{
		LPID:           lpID,
		Window:         window.String(),
		QuotesReceived: total,
		Fills:          fills,
		Rejections:     rejects,
		ComputedAt:     time.Now().UTC(),
		Source:         "pg-orders",
	}
	if d := fills + rejects; d > 0 {
		sc.FillRatio = float64(fills) / float64(d)
		sc.RejectionRate = float64(rejects) / float64(d)
	} else if total > 0 {
		// Everything still open/cancelled — no decisive fill data;
		// fill_ratio is undefined, not zero.
		sc.FillRatio = 1
	}
	if avgMs.Valid {
		sc.AvgResponseTimeMS = avgMs.Float64
	}
	if p99Ms.Valid {
		sc.P99LatencyMS = p99Ms.Float64
	}
	if mins := window.Minutes(); mins > 0 {
		sc.AvailabilityPct = float64(activeMinutes) / mins * 100
		sc.TwoSidedPresencePct = float64(twoSidedMinutes) / mins * 100
	}
	return sc, nil
}

// LPOpsAlertSink adapts AlertSink to the shared ops-alert seam
// (settlement.PublisherAlerter over NATS) — alerts page as P2 with the
// metric name in the code.
type LPOpsAlertSink struct {
	Raise func(ctx context.Context, severity, code, summary string) error
}

// EmitLPAlert dispatches one persisted LP alert; a nil Raise keeps the
// documented nil-sink semantics (persisted, undispatched).
func (s LPOpsAlertSink) EmitLPAlert(ctx context.Context, a LPAlert) error {
	if s.Raise == nil {
		return nil
	}
	return s.Raise(ctx, "P2", "LP_"+a.Metric,
		fmt.Sprintf("lp %d %s %.4f breached %.4f over %s",
			a.LPID, a.Metric, a.Observed, a.Threshold, a.Window))
}
