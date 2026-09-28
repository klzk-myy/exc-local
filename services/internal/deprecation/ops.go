// Phase-09 Task 9.3.6 — operationalizing the deprecation policy built
// in Phase-05 Task 5.3.20 (this file extends, never re-implements, the
// rule store and header middleware).
//
// Two additions:
//
//  1. Sunset evaluation sweep: MarkSunset flips api_deprecations rows to
//     status='SUNSET' once sunset_at has passed (migration 196) — the
//     410 ENDPOINT_GONE enforcement was already time-based; the status
//     column makes the lifecycle queryable and drives the
//     deprecation_sunset_total counter. Sweep runs the transition check
//     on an interval (default 60s); rule staleness stays fail-closed
//     exactly as CachedRules already guarantees.
//
//  2. Telemetry on deprecated-route usage: every middleware decision
//     (announced-hit vs post-sunset-gone) feeds a Hit sink — the
//     production sink increments the Redis hash
//     deprecation:hits:{rule_id}:{yyyymmdd} (90d TTL) and the sweep
//     compacts durable daily rollups into api_deprecation_hits so
//     migration decisions survive Redis loss. Telemetry is
//     observability, not enforcement — a sink error is logged, never
//     fails the request.
package deprecation

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Hit is one observed use of a deprecation-governed route.
type Hit struct {
	Rule   Rule
	Gone   bool // true = post-sunset attempt (410 emitted)
	Method string
	Path   string
	At     time.Time
}

// HitSink consumes telemetry hits. nil-able — MiddlewareWithTelemetry
// treats nil as off.
type HitSink func(ctx context.Context, h Hit)

// RedisHitSink returns the production sink: per-day INCR on
// deprecation:hits:{rule_id}:{yyyymmdd} with a "gone" field so the admin
// surface can split announced-use from post-sunset attempts.
// Best-effort: sink errors are swallowed by the middleware call site —
// telemetry must never break the request path.
func RedisHitSink(rdb goredis.Cmdable) HitSink {
	return func(ctx context.Context, h Hit) {
		key := fmt.Sprintf("deprecation:hits:%d:%s", h.Rule.ID,
			h.At.UTC().Format("20060102"))
		field := "hits"
		if h.Gone {
			field = "gone_hits"
		}
		pipe := rdb.TxPipeline()
		pipe.HIncrBy(ctx, key, field, 1)
		pipe.Expire(ctx, key, 90*24*time.Hour)
		_, _ = pipe.Exec(ctx)
	}
}

// UsageCounts aggregates the Redis hit hashes into per-rule daily
// counters for the admin usage endpoint.
func UsageCounts(ctx context.Context, rdb goredis.Cmdable) (map[int64]map[string][2]int64, error) {
	out := map[int64]map[string][2]int64{}
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "deprecation:hits:*", 200).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			// deprecation:hits:{id}:{yyyymmdd}
			rest, ok := strings.CutPrefix(k, "deprecation:hits:")
			if !ok {
				continue
			}
			idStr, day, ok := strings.Cut(rest, ":")
			if !ok {
				continue
			}
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil || len(day) != 8 {
				continue
			}
			fields, err := rdb.HGetAll(ctx, k).Result()
			if err != nil {
				continue
			}
			m := out[id]
			if m == nil {
				m = map[string][2]int64{}
				out[id] = m
			}
			hits, _ := strconv.ParseInt(fields["hits"], 10, 64)
			gone, _ := strconv.ParseInt(fields["gone_hits"], 10, 64)
			m[day] = [2]int64{hits, gone}
		}
		if cursor = next; cursor == 0 {
			break
		}
	}
	return out, nil
}

// MiddlewareWithTelemetry is Middleware plus a hit sink — announced
// routes report (rule,false), post-sunset attempts (rule,true). This
// preserves the Phase-05 contract verbatim: headers and 410 emission are
// unchanged; only the observation hook is new.
func MiddlewareWithTelemetry(src Rules,
	writeProblem func(w http.ResponseWriter, r *http.Request, code int, errCode, msg string),
	now func() time.Time, sink HitSink) func(http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rules, err := src.Active(r.Context())
			if err == nil {
				if rule := match(rules, r.Method, r.URL.Path, now()); rule != nil {
					t := now()
					if !t.Before(rule.SunsetAt) {
						if sink != nil {
							sink(r.Context(), Hit{Rule: *rule, Gone: true,
								Method: r.Method, Path: r.URL.Path, At: t})
						}
						writeProblem(w, r, http.StatusGone, "ENDPOINT_GONE",
							fmt.Sprintf("endpoint sunset at %s; see %s",
								rule.SunsetAt.UTC().Format(time.RFC1123), rule.MigrationURL))
						return
					}
					if sink != nil {
						sink(r.Context(), Hit{Rule: *rule, Gone: false,
							Method: r.Method, Path: r.URL.Path, At: t})
					}
					w.Header().Set("Deprecation", fmt.Sprintf("@%d", rule.AnnouncedAt.Unix()))
					w.Header().Set("Sunset", rule.SunsetAt.UTC().Format(http.TimeFormat))
					w.Header().Add("Link", fmt.Sprintf(`<%s>; rel="deprecation"`, rule.MigrationURL))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// Sunset sweep
// ---------------------------------------------------------------------------

// MarkSunset flips announced rules whose sunset_at has passed to
// status='SUNSET' and returns the transitioned ids. Idempotent — safe
// to run on an interval.
func (s *Store) MarkSunset(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE api_deprecations
		   SET status = 'SUNSET', sunset_processed_at = now()
		 WHERE status = 'ANNOUNCED' AND sunset_at <= now()
		RETURNING id`)
	if err != nil {
		return nil, fmt.Errorf("deprecation sweep: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CompactUsage upserts yesterday's (and today's partial) Redis counters
// into api_deprecation_hits so usage telemetry is durable.
func (s *Store) CompactUsage(ctx context.Context, rdb goredis.Cmdable) (int, error) {
	if rdb == nil {
		return 0, nil
	}
	counts, err := UsageCounts(ctx, rdb)
	if err != nil {
		return 0, err
	}
	n := 0
	for ruleID, days := range counts {
		for day, hc := range days {
			tag, err := s.pool.Exec(ctx, `
				INSERT INTO api_deprecation_hits (rule_id, day, hits, gone_hits)
				VALUES ($1, $2::date, $3, $4)
				ON CONFLICT (rule_id, day) DO UPDATE SET
				    hits = EXCLUDED.hits, gone_hits = EXCLUDED.gone_hits`,
				ruleID, day, hc[0], hc[1])
			if err != nil {
				return n, fmt.Errorf("deprecation usage compact %d/%s: %w", ruleID, day, err)
			}
			n += int(tag.RowsAffected())
		}
	}
	return n, nil
}

// Sweeper runs the sunset transition + usage compaction on an interval.
type Sweeper struct {
	Store    *Store
	RDB      goredis.Cmdable // nil → usage compaction skipped
	Log      *slog.Logger
	Interval time.Duration // default 60s
	// OnSunset observes transitioned rule ids (metrics hook).
	OnSunset func(n int)
}

// Run executes one sweep immediately then every Interval until ctx
// ends.
func (sw *Sweeper) Run(ctx context.Context) {
	if sw.Log == nil {
		sw.Log = slog.Default()
	}
	iv := sw.Interval
	if iv <= 0 {
		iv = 60 * time.Second
	}
	sw.once(ctx)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sw.once(ctx)
		}
	}
}

func (sw *Sweeper) once(ctx context.Context) {
	ids, err := sw.Store.MarkSunset(ctx)
	if err != nil {
		sw.Log.Error("deprecation sunset sweep failed", "err", err)
	} else if len(ids) > 0 {
		sw.Log.Info("deprecation rules sunset", "ids", ids)
		if sw.OnSunset != nil {
			sw.OnSunset(len(ids))
		}
	}
	if n, err := sw.Store.CompactUsage(ctx, sw.RDB); err != nil {
		sw.Log.Error("deprecation usage compaction failed", "err", err)
	} else if n > 0 {
		sw.Log.Debug("deprecation usage compacted", "rows", n)
	}
}
