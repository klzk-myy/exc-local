// Phase-13 Task 13.3.9 item 3 — circuit_breaker_events audit store
// (migration 206).
//
// Every state transition (automated trip, hold→probe advance, probe
// resolution, manual trip, dual-controlled reset) lands one row with the
// trigger parameters and the acting principal (0/NULL for automated).
// Insert failures are surfaced to the caller — the service logs and
// continues because a safety transition must never be vetoed by a
// telemetry outage, but the error is never silently dropped.
package risk

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgBreakerEventStore implements BreakerEventStore over PostgreSQL.
type PgBreakerEventStore struct {
	pool *pgxpool.Pool
}

// NewPgBreakerEventStore wires the store over the shared pool.
func NewPgBreakerEventStore(pool *pgxpool.Pool) *PgBreakerEventStore {
	return &PgBreakerEventStore{pool: pool}
}

// InsertBreakerEvent appends one audit row. actor_id NULL marks an
// automated transition; trigger params serialize to jsonb ({} when nil).
func (s *PgBreakerEventStore) InsertBreakerEvent(ctx context.Context, ev BreakerEvent) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("circuit-breaker events: pgx pool is nil")
	}
	trig, err := json.Marshal(ev.Trigger)
	if err != nil {
		return fmt.Errorf("circuit-breaker events: trigger marshal: %w", err)
	}
	if len(trig) == 0 || string(trig) == "null" {
		trig = []byte("{}")
	}
	var actor *int64
	if ev.ActorID > 0 {
		actor = &ev.ActorID
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO circuit_breaker_events
		    (scope, target_id, from_state, to_state, reason, trigger,
		     actor_id, hold_ms, probes, created_at)
		VALUES ($1::circuit_breaker_scope_enum, $2,
		        $3::circuit_breaker_state_enum, $4::circuit_breaker_state_enum,
		        $5, $6, $7, $8, $9, $10)`,
		ev.Scope, ev.ID, ev.FromState, ev.ToState, ev.Reason,
		trig, actor, ev.HoldMS, ev.Probes, ev.At)
	if err != nil {
		return fmt.Errorf("circuit-breaker events: insert: %w", err)
	}
	return nil
}

// RecentEvents returns the newest audit rows (admin/ops surface).
func (s *PgBreakerEventStore) RecentEvents(ctx context.Context, limit int) ([]BreakerEvent, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("circuit-breaker events: pgx pool is nil")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT scope::text, target_id, from_state::text, to_state::text,
		       reason, COALESCE(trigger,'{}'::jsonb), COALESCE(actor_id,0),
		       hold_ms, probes, created_at
		  FROM circuit_breaker_events
		 ORDER BY event_id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("circuit-breaker events: list: %w", err)
	}
	defer rows.Close()
	var out []BreakerEvent
	for rows.Next() {
		var ev BreakerEvent
		var trig []byte
		if err := rows.Scan(&ev.Scope, &ev.ID, &ev.FromState, &ev.ToState,
			&ev.Reason, &trig, &ev.ActorID, &ev.HoldMS, &ev.Probes, &ev.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(trig, &ev.Trigger); err == nil && len(ev.Trigger) == 0 {
			ev.Trigger = nil
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
