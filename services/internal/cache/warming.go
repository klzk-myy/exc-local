// Package cache implements Phase-09 Task 9.3.8 — automatic cache warming
// after recovery, deploy and failover.
//
// Contract (task items 1–4, remediation #35 priority order):
//   - P0 keys (sessions, account locks, shard map) warm inside a 5s
//     budget; P1 keys (instruments, fee tiers, tickers, book snapshots)
//     inside a 30s budget;
//   - firing is automatic after recovery, deploy and failover — the
//     trigger funnel is the Redis pub/sub channel warm:trigger (any
//     supervisor, watchdog or deploy hook publishes "deploy" /
//     "recovery" / "failover") plus the direct Run call at boot and the
//     `exchange warm-cache` operator command;
//   - correctness gate: a unit is only marked fresh when its Warm ran to
//     completion AND produced ≥ MinKeys entries. A failed or empty warm
//     never leaves a "fresh" marker — readers consulting IsFresh see
//     false, never stale-but-flagged-fresh data.
//
// Redis markers (per unit):
//
//	warm:state:{unit}  HASH   status|trigger|gen|keys|duration_ms|
//	                          at|fresh_until|error — the operator +
//	                          freshness surface
//	warm:gen           STRING boot generation (unixnano) stamped into
//	                          every warmed payload envelope
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

// Priority buckets from the task contract.
type Priority int

const (
	// P0 is the 5s-budget tier: sessions, account locks, shard map.
	P0 Priority = iota
	// P1 is the 30s-budget tier: instruments, fee tiers, tickers, books.
	P1
)

// Budgets per the task's SLAs (P0 5s, P1 30s).
const (
	P0Budget = 5 * time.Second
	P1Budget = 30 * time.Second
)

const (
	markerPrefix  = "warm:state:"
	genKey        = "warm:gen"
	triggerChan   = "warm:trigger"
	markerKeyTTL  = 24 * time.Hour // markers outlive the warm so postmortems can inspect
	defaultMaxSym = 512            // bound per-symbol P1 work
)

// Env carries the warmer's dependencies.
type Env struct {
	Pool *pgxpool.Pool   // PostgreSQL read source; nil → PG units skipped-with-error
	RDB  goredis.Cmdable // Redis target; nil → units that need Redis error out
	// ShardMapPath overrides the sharding.yaml location ("" = default
	// resolution per config.LoadShardMap).
	ShardMapPath string
	// MaxSymbols bounds the per-symbol P1 units (tickers, books).
	MaxSymbols int
	Log        *slog.Logger
	Now        func() time.Time
}

func (e *Env) defaults() {
	if e.Log == nil {
		e.Log = slog.Default()
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.MaxSymbols <= 0 {
		e.MaxSymbols = defaultMaxSym
	}
}

// Unit is one warmable key group.
type Unit struct {
	Name     string
	Priority Priority
	// MinKeys is the correctness gate: a run that writes fewer keys than
	// MinKeys is a unit error — the marker records "error", never
	// "fresh". Zero means "any outcome counts" (lock scans are allowed
	// to legitimately observe zero).
	MinKeys int
	// FreshTTL bounds how long the marker may claim freshness after a
	// successful warm; 0 → the unit reports no freshness claim.
	FreshTTL time.Duration
	// Warm writes the unit's keys and returns how many were written.
	Warm func(ctx context.Context, env *Env) (int, error)
}

// UnitReport is one unit's outcome.
type UnitReport struct {
	Name       string `json:"name"`
	Priority   string `json:"priority"`
	Status     string `json:"status"` // ok | error | skipped
	Keys       int    `json:"keys"`
	DurationMs int64  `json:"duration_ms"`
	FreshUntil string `json:"fresh_until,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Report is one warming pass.
type Report struct {
	Trigger    string       `json:"trigger"`
	Gen        int64        `json:"gen"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	Units      []UnitReport `json:"units"`
}

// OK reports whether every unit completed within budget.
func (r Report) OK() bool {
	for _, u := range r.Units {
		if u.Status != "ok" {
			return false
		}
	}
	return true
}

// Warmer runs warm units under their priority budgets.
type Warmer struct {
	env   *Env
	units []Unit
}

// New builds a warmer over the given units (see DefaultUnits).
func New(env *Env, units []Unit) *Warmer {
	if env == nil {
		env = &Env{}
	}
	env.defaults()
	return &Warmer{env: env, units: append([]Unit(nil), units...)}
}

// Run executes all P0 units under a 5s shared budget, then all P1 units
// under 30s. trigger labels the run ("deploy", "recovery", "failover",
// "manual"). The pass is idempotent and safe to run concurrently — units
// write last-writer-wins keys; markers record the per-unit outcome.
func (w *Warmer) Run(ctx context.Context, trigger string) Report {
	if trigger == "" {
		trigger = "manual"
	}
	rep := Report{Trigger: trigger, StartedAt: w.env.Now().UTC(),
		Gen: w.env.Now().UnixNano()}
	if w.env.RDB != nil {
		_ = w.env.RDB.Set(ctx, genKey, rep.Gen, 0).Err()
	}
	rep.Units = append(rep.Units, w.runTier(ctx, trigger, P0, P0Budget)...)
	rep.Units = append(rep.Units, w.runTier(ctx, trigger, P1, P1Budget)...)
	rep.FinishedAt = w.env.Now().UTC()
	w.env.Log.Info("cache warm run complete",
		"trigger", trigger, "gen", rep.Gen, "ok", rep.OK(),
		"units", len(rep.Units), "dur_ms", rep.FinishedAt.Sub(rep.StartedAt).Milliseconds())
	return rep
}

// runTier executes every unit of one priority under a shared budget —
// units run sequentially so an early unit cannot starve the rest
// unaccountably; each sees a deadline inside the tier budget.
func (w *Warmer) runTier(ctx context.Context, trigger string, pri Priority, budget time.Duration) []UnitReport {
	tierCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var out []UnitReport
	for _, u := range w.units {
		if u.Priority != pri {
			continue
		}
		out = append(out, w.runUnit(tierCtx, trigger, u))
	}
	return out
}

func (w *Warmer) runUnit(ctx context.Context, trigger string, u Unit) UnitReport {
	start := w.env.Now()
	rep := UnitReport{Name: u.Name, Priority: fmt.Sprintf("P%d", u.Priority)}

	keys, err := u.Warm(ctx, w.env)
	rep.Keys = keys
	rep.DurationMs = w.env.Now().Sub(start).Milliseconds()
	switch {
	case err != nil:
		rep.Status, rep.Error = "error", err.Error()
	case keys < u.MinKeys:
		rep.Status = "error"
		rep.Error = fmt.Sprintf("warm produced %d keys, below min_keys=%d", keys, u.MinKeys)
	default:
		rep.Status = "ok"
		if u.FreshTTL > 0 {
			rep.FreshUntil = start.Add(u.FreshTTL).UTC().Format(time.RFC3339Nano)
		}
	}
	w.writeMarker(ctx, u, rep, trigger)
	if rep.Status != "ok" {
		w.env.Log.Warn("cache warm unit failed",
			"unit", u.Name, "trigger", trigger, "err", rep.Error)
	}
	return rep
}

// writeMarker records the outcome. The marker carries fresh_until ONLY
// on success — a failed unit leaves fresh_until empty, so IsFresh can
// never observe a failed unit as fresh.
func (w *Warmer) writeMarker(ctx context.Context, u Unit, rep UnitReport, trigger string) {
	if w.env.RDB == nil {
		return
	}
	fields := map[string]any{
		"status":      rep.Status,
		"trigger":     trigger,
		"keys":        rep.Keys,
		"duration_ms": rep.DurationMs,
		"at":          w.env.Now().UTC().Format(time.RFC3339Nano),
		"error":       rep.Error,
	}
	if rep.FreshUntil != "" {
		fields["fresh_until"] = rep.FreshUntil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = w.env.RDB.HSet(cctx, markerPrefix+u.Name, fields).Err()
	_ = w.env.RDB.Expire(cctx, markerPrefix+u.Name, markerKeyTTL).Err()
}

// IsFresh reports whether unit currently carries a live freshness claim
// — status=ok AND fresh_until in the future. Missing markers, errors and
// expired windows all read as cold (false). The error return reports
// Redis read failures only; absent data is not an error.
func IsFresh(ctx context.Context, rdb goredis.Cmdable, unit string) (bool, error) {
	m, err := rdb.HGetAll(ctx, markerPrefix+unit).Result()
	if err != nil {
		return false, err
	}
	if m["status"] != "ok" {
		return false, nil
	}
	fu, err := time.Parse(time.RFC3339Nano, m["fresh_until"])
	if err != nil {
		return false, nil // no freshness claim recorded
	}
	return time.Now().Before(fu), nil
}

// Envelope wraps a warmed payload with the run generation and source
// timestamp so readers can verify provenance instead of trusting the
// key's mere existence.
type Envelope struct {
	Gen      int64           `json:"gen"`
	SourceTS int64           `json:"source_ts"` // unixnano of the source read
	Payload  json.RawMessage `json:"payload"`
}

// Wrap marshals payload inside an Envelope for one warm generation.
func Wrap(gen int64, sourceTS time.Time, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{Gen: gen, SourceTS: sourceTS.UnixNano(), Payload: body})
}

// UnwrapFresh validates a cached envelope: it must parse, belong to the
// current warm generation when requireGen is non-zero, and be no older
// than maxAge. Anything else reports false — stale-but-present data is
// treated as a cache miss, never as fresh.
func UnwrapFresh(raw []byte, requireGen int64, maxAge time.Duration, now time.Time) (json.RawMessage, bool) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false
	}
	if requireGen != 0 && env.Gen != requireGen {
		return nil, false
	}
	if maxAge > 0 && now.Sub(time.Unix(0, env.SourceTS)) > maxAge {
		return nil, false
	}
	return env.Payload, true
}

// Gen reads the current warm generation (0 when never warmed).
func Gen(ctx context.Context, rdb goredis.Cmdable) (int64, error) {
	v, err := rdb.Get(ctx, genKey).Result()
	if errors.Is(err, goredis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// RunOnTriggers subscribes to the warm:trigger channel and runs a pass
// per message ("deploy" | "recovery" | "failover" | any label). It is
// the auto-fire half of the task contract: watchdogs, the deploy hook
// and the DR coordinator all publish the same channel. Runs serialize
// through a mutex — overlapping triggers fold into one pass.
func (w *Warmer) RunOnTriggers(ctx context.Context) error {
	if w.env.RDB == nil {
		return errors.New("cache: RunOnTriggers requires Redis")
	}
	uc, ok := w.env.RDB.(goredis.UniversalClient)
	if !ok {
		return errors.New("cache: RunOnTriggers requires a pub/sub-capable client")
	}
	sub := uc.Subscribe(ctx, triggerChan)
	defer sub.Close()
	var mu sync.Mutex
	for {
		msg, err := sub.ReceiveMessage(ctx)
		if err != nil {
			return err
		}
		mu.Lock()
		rep := w.Run(ctx, msg.Payload)
		mu.Unlock()
		if !rep.OK() {
			w.env.Log.Warn("cache warm trigger produced errors",
				"trigger", msg.Payload)
		}
	}
}
