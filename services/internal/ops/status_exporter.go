// Package ops implements Phase-09 Task 9.3.25 — the public status page
// feed and operational health exporter.
//
// Contract (task items + spec §19.3):
//   - one Aggregator ingests component probes (matching-engine shards,
//     Aeron bridges, gateway proxies, PostgreSQL, ClickHouse — each a
//     caller-supplied Probe so the package stays transport-agnostic) and
//     the authoritative degradation mode (Redis system:degradation:* —
//     the same seam DegradationGate reads, never a parallel truth);
//   - it computes the aggregate operational status and publishes
//     status:current to Redis for GET /api/v1/system/status plus
//     per-component status:component:{name} hashes;
//   - transitions append to ops_status_events (migration 197) — the
//     uptime windows served to the public page are computed from that
//     ledger, never fabricated;
//   - a non-Normal mode activation opens a public ops_incidents row;
//     recovery to Normal resolves it.
//
// Fail-closed (spec §2.7): a mode read failure reports Maintenance; an
// unprobeable critical component reports down; "unknown" is a state the
// page renders explicitly, not silently-green.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	exchredis "exchange/internal/redis"
)

// Aggregate status vocabulary (statuspage.io-shaped; stored in
// ops_status_events.to_state — CHECK constraint must match).
const (
	StateOperational  = "operational"
	StateDegraded     = "degraded_performance"
	StatePartial      = "partial_outage"
	StateMajor        = "major_outage"
	StateMaintenance  = "maintenance"
	StateUnknown      = "unknown"
	componentWildcard = "system"
)

// Component is one probe target.
type Component struct {
	Name     string `json:"name"`
	Critical bool   `json:"critical"` // down ⇒ major_outage
	Probe    func(ctx context.Context) error
}

// ComponentHealth is one probe result.
type ComponentHealth struct {
	Name      string  `json:"name"`
	State     string  `json:"state"` // operational | degraded | down | unknown
	Critical  bool    `json:"critical"`
	LatencyMs float64 `json:"latency_ms"`
	Detail    string  `json:"detail,omitempty"`
}

// Status is the published aggregate — the GET /api/v1/system/status
// payload shape and the status:current Redis document.
type Status struct {
	Status     string            `json:"status"` // operational|degraded_performance|partial_outage|major_outage|maintenance|unknown
	Mode       string            `json:"mode"`   // spec §2.4 degradation mode
	Components []ComponentHealth `json:"components"`
	// Metrics carries the exporter's optional gauges (API p99, engine
	// loop p99) — populated by whoever owns the measurement.
	Metrics   map[string]float64 `json:"metrics,omitempty"`
	Source    string             `json:"source"` // "aggregator" | "gateway-local"
	UpdatedAt time.Time          `json:"updated_at"`
}

// ModeReader is the degradation-mode read seam — *redis.Client
// satisfies it.
type ModeReader interface {
	GetDegradationMode(ctx context.Context) (exchredis.DegradationState, error)
}

// Aggregator probes components, computes the aggregate, publishes.
type Aggregator struct {
	pool  *pgxpool.Pool // nil → events/incidents skipped (in-memory only)
	rdb   goredis.Cmdable
	mode  ModeReader
	comps []Component
	log   *slog.Logger
	now   func() time.Time

	mu   sync.Mutex
	last Status
	have bool
}

// NewAggregator binds the aggregator. rdb is required for publish;
// mode may be nil (mode reports Normal only when it is absent AND
// unpublished — degraded readers must supply it).
func NewAggregator(pool *pgxpool.Pool, rdb goredis.Cmdable, mode ModeReader,
	comps []Component, log *slog.Logger) *Aggregator {
	if log == nil {
		log = slog.Default()
	}
	return &Aggregator{pool: pool, rdb: rdb, mode: mode,
		comps: comps, log: log, now: time.Now}
}

// SetClockForTest replaces the clock (tests only).
func (a *Aggregator) SetClockForTest(now func() time.Time) { a.now = now }

const (
	statusCurrentKey = "status:current"
	componentPrefix  = "status:component:"
	eventsList       = "status:events"
	probeTimeout     = 2 * time.Second
	statusTTL        = 30 * time.Second // exporter cadence is ~1s; stale key = dead exporter
)

// Collect probes every component and computes the aggregate. It never
// errors: probe failures are component states, mode failures are
// Maintenance — the result is always a complete, truthful Status.
func (a *Aggregator) Collect(ctx context.Context) *Status {
	st := &Status{Source: "aggregator", UpdatedAt: a.now().UTC(),
		Mode: "Normal"}

	if a.mode != nil {
		if ds, err := a.mode.GetDegradationMode(ctx); err == nil && ds.Mode != "" {
			st.Mode = string(ds.Mode)
		} else {
			st.Mode = string(exchredis.ModeMaintenance) // §2.7 fail-closed
		}
	}

	var wg sync.WaitGroup
	st.Components = make([]ComponentHealth, len(a.comps))
	for i, c := range a.comps {
		wg.Add(1)
		go func(i int, c Component) {
			defer wg.Done()
			h := ComponentHealth{Name: c.Name, Critical: c.Critical}
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			start := a.now()
			err := c.Probe(pctx)
			h.LatencyMs = float64(a.now().Sub(start).Microseconds()) / 1000
			switch {
			case err == nil:
				h.State = "operational"
			case errors.Is(err, context.DeadlineExceeded):
				h.State, h.Detail = "down", "probe timeout"
			default:
				h.State, h.Detail = "down", err.Error()
			}
			st.Components[i] = h
		}(i, c)
	}
	wg.Wait()
	sort.Slice(st.Components, func(i, j int) bool {
		return st.Components[i].Name < st.Components[j].Name
	})

	st.Status = aggregate(st.Mode, st.Components)
	return st
}

// aggregate maps mode + component states → the public status word.
func aggregate(mode string, comps []ComponentHealth) string {
	if mode == "Maintenance" {
		return StateMaintenance
	}
	var critDown, otherDown bool
	for _, c := range comps {
		switch c.State {
		case "down":
			if c.Critical {
				critDown = true
			} else {
				otherDown = true
			}
		case "unknown":
			otherDown = true
		}
	}
	switch {
	case critDown:
		return StateMajor
	case otherDown:
		return StatePartial
	case mode != "" && mode != "Normal":
		// ReadOnly / MarketDataOnly / SpotOnly / Throttled with all
		// components reachable is a degraded, not partial, posture.
		return StateDegraded
	default:
		return StateOperational
	}
}

// Publish collects and publishes: Redis status:current (TTL'd so a dead
// exporter's document expires rather than being served as truth), the
// per-component hashes, and — on transitions — ops_status_events rows
// and auto incident open/resolve.
func (a *Aggregator) Publish(ctx context.Context) (*Status, error) {
	st := a.Collect(ctx)

	if a.rdb != nil {
		raw, err := json.Marshal(st)
		if err == nil {
			_ = a.rdb.Set(ctx, statusCurrentKey, raw, statusTTL).Err()
		}
		pipe := a.rdb.TxPipeline()
		for _, c := range st.Components {
			pipe.HSet(ctx, componentPrefix+c.Name, map[string]any{
				"state":      c.State,
				"critical":   c.Critical,
				"latency_ms": fmt.Sprintf("%.3f", c.LatencyMs),
				"detail":     c.Detail,
				"at":         st.UpdatedAt.Format(time.RFC3339Nano),
			})
			pipe.Expire(ctx, componentPrefix+c.Name, statusTTL)
		}
		_, _ = pipe.Exec(ctx)
	}

	a.mu.Lock()
	prev, had := a.last, a.have
	a.last, a.have = *st, true
	a.mu.Unlock()

	if !had || prev.Status != st.Status || prev.Mode != st.Mode {
		a.recordTransition(ctx, &prev, st, had)
	}
	for _, c := range st.Components {
		var p *ComponentHealth
		if had {
			for i := range prev.Components {
				if prev.Components[i].Name == c.Name {
					p = &prev.Components[i]
					break
				}
			}
		}
		if !had || p == nil || p.State != c.State {
			a.recordEvent(ctx, c.Name, stateOrEmpty(p), c.State,
				st.Mode, c.Detail)
		}
	}
	return st, nil
}

func stateOrEmpty(c *ComponentHealth) string {
	if c == nil {
		return ""
	}
	return c.State
}

// recordTransition appends the aggregate event and drives the incident
// lifecycle: non-Normal mode opens a public notice; Normal resolves open
// auto-posted incidents.
func (a *Aggregator) recordTransition(ctx context.Context, prev, cur *Status, had bool) {
	a.recordEvent(ctx, componentWildcard,
		func() string {
			if had {
				return prev.Status
			}
			return ""
		}(),
		cur.Status, cur.Mode, "aggregate status transition")

	if a.pool == nil {
		return
	}
	if cur.Mode != "Normal" && (!had || prev.Mode != cur.Mode) {
		sev := "P2"
		switch cur.Mode {
		case "ReadOnly", "MarketDataOnly", "SpotOnly":
			sev = "P1"
		case "Maintenance":
			sev = "P2"
		case "Throttled":
			sev = "P2"
		}
		if _, err := a.pool.Exec(ctx, `
			INSERT INTO ops_incidents (title, severity, status, public, mode, summary)
			VALUES ($1, $2, 'OPEN', true, $3, $4)`,
			fmt.Sprintf("Degradation mode %s activated", cur.Mode), sev, cur.Mode,
			fmt.Sprintf("Degradation mode %s entered at %s; aggregate status %s",
				cur.Mode, cur.UpdatedAt.Format(time.RFC3339), cur.Status)); err != nil {
			a.log.Error("status incident post failed", "err", err)
		}
	}
	if cur.Mode == "Normal" && had && prev.Mode != "Normal" {
		if _, err := a.pool.Exec(ctx, `
			UPDATE ops_incidents SET status='RESOLVED', resolved_at=now()
			 WHERE status='OPEN' AND mode IS NOT NULL AND mode <> 'Normal'
			   AND postmortem_url IS NULL`); err != nil {
			a.log.Error("status incident resolve failed", "err", err)
		}
	}
}

func (a *Aggregator) recordEvent(ctx context.Context, component, from, to, mode, detail string) {
	if a.pool == nil {
		return
	}
	var fromArg any
	if from != "" {
		fromArg = from
	}
	if _, err := a.pool.Exec(ctx, `
		INSERT INTO ops_status_events (component, from_state, to_state, mode, detail)
		VALUES ($1, $2, $3, $4, $5)`,
		component, fromArg, to, mode, detail); err != nil {
		a.log.Error("status event write failed", "component", component, "err", err)
	}
	if a.rdb != nil {
		raw, _ := json.Marshal(map[string]any{
			"component": component, "from": from, "to": to,
			"mode": mode, "at": a.now().UTC().Format(time.RFC3339Nano),
		})
		_ = a.rdb.LPush(ctx, eventsList, raw).Err()
		_ = a.rdb.LTrim(ctx, eventsList, 0, 499).Err()
	}
}

// Run publishes every interval (default 1s — the status page must
// reflect mode changes within 3s per the task AC).
func (a *Aggregator) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	if _, err := a.Publish(ctx); err != nil {
		a.log.Error("status publish failed", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := a.Publish(ctx); err != nil {
				a.log.Error("status publish failed", "err", err)
			}
		}
	}
}

// Uptime computes the uptime fraction for one component over the window
// ending at now, from the ops_status_events ledger: each event marks the
// start of a state; gaps before the first in-window event inherit the
// last pre-window state. Windows with no history return (0,false) —
// unknown uptime is reported as unknown, not 100%.
func (a *Aggregator) Uptime(ctx context.Context, component string, window time.Duration) (float64, bool, error) {
	if a.pool == nil {
		return 0, false, errors.New("ops: uptime requires postgres")
	}
	since := a.now().Add(-window)

	var prior string
	_ = a.pool.QueryRow(ctx, `
		SELECT to_state FROM ops_status_events
		 WHERE component = $1 AND at < $2
		 ORDER BY at DESC LIMIT 1`, component, since).Scan(&prior)

	rows, err := a.pool.Query(ctx, `
		SELECT to_state, at FROM ops_status_events
		 WHERE component = $1 AND at >= $2 ORDER BY at`, component, since)
	if err != nil {
		return 0, false, err
	}
	type ev struct {
		to string
		at time.Time
	}
	var evs []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.to, &e.at); err != nil {
			rows.Close()
			return 0, false, err
		}
		evs = append(evs, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	if prior == "" && len(evs) == 0 {
		return 0, false, nil // no history — unknown, not 100%
	}

	up := func(state string) bool { return state == StateOperational }
	var den time.Duration
	cursor := since
	cur := prior
	if cur == "" {
		// No pre-window state: the measured window starts at the first
		// observed event (unknown history is excluded, not counted up).
		cursor = evs[0].at
		cur = evs[0].to
		den = a.now().Sub(evs[0].at)
	} else {
		den = window
	}
	var upDur time.Duration
	for _, e := range evs {
		if up(cur) {
			upDur += e.at.Sub(cursor)
		}
		cursor = e.at
		cur = e.to
	}
	if up(cur) {
		upDur += a.now().Sub(cursor)
	}
	if den <= 0 {
		return 0, false, nil
	}
	return float64(upDur) / float64(den), true, nil
}

// Current returns the last published status:current document — the
// gateway's public handler reads this first.
func Current(ctx context.Context, rdb goredis.Cmdable) (*Status, error) {
	raw, err := rdb.Get(ctx, statusCurrentKey).Result()
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, fmt.Errorf("status:current corrupt: %w", err)
	}
	return &st, nil
}
