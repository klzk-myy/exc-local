// Phase-14 Task 14.3.2 — auto_halt_events audit store (migration 217).
//
// Records the detector-attribution layer for anomaly halts: the breaker
// transitions themselves are already audited by circuit_breaker_events
// (migration 206) — this table answers which detector fired, what it
// observed, and how the alert/notify fan-out went. Insert failures are
// surfaced to the caller (the service logs and continues — a telemetry
// outage never vetoes a safety halt, but the error is never silently
// dropped).
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgAutoHaltEventStore implements AutoHaltEventStore over PostgreSQL.
type PgAutoHaltEventStore struct {
	pool *pgxpool.Pool
}

// NewPgAutoHaltEventStore wires the store over the shared pool.
func NewPgAutoHaltEventStore(pool *pgxpool.Pool) *PgAutoHaltEventStore {
	return &PgAutoHaltEventStore{pool: pool}
}

// InsertAutoHaltEvent appends one audit row; observed serializes to
// jsonb ({} when nil).
func (s *PgAutoHaltEventStore) InsertAutoHaltEvent(ctx context.Context, ev AutoHaltEvent) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("auto-halt events: pgx pool is nil")
	}
	obs, err := json.Marshal(ev.Observed)
	if err != nil {
		return fmt.Errorf("auto-halt events: observed marshal: %w", err)
	}
	if len(obs) == 0 || string(obs) == "null" {
		obs = []byte("{}")
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO auto_halt_events
		    (symbol, scope, detector, action, observed, breaker_state,
		     alert_sent, notified, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		ev.Symbol, ev.Scope, ev.Detector, ev.Action, obs,
		ev.BreakerState, ev.AlertSent, ev.Notified, at)
	if err != nil {
		return fmt.Errorf("auto-halt events: insert: %w", err)
	}
	return nil
}

// RecentAutoHaltEvents returns the newest audit rows (admin/ops review).
func (s *PgAutoHaltEventStore) RecentAutoHaltEvents(ctx context.Context, limit int) ([]AutoHaltEvent, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("auto-halt events: pgx pool is nil")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT symbol, scope, detector, action,
		       COALESCE(observed,'{}'::jsonb), breaker_state,
		       alert_sent, notified, created_at
		  FROM auto_halt_events
		 ORDER BY event_id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("auto-halt events: list: %w", err)
	}
	defer rows.Close()
	var out []AutoHaltEvent
	for rows.Next() {
		var ev AutoHaltEvent
		var obs []byte
		if err := rows.Scan(&ev.Symbol, &ev.Scope, &ev.Detector, &ev.Action,
			&obs, &ev.BreakerState, &ev.AlertSent, &ev.Notified, &ev.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(obs, &ev.Observed); err == nil && len(ev.Observed) == 0 {
			ev.Observed = nil
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
