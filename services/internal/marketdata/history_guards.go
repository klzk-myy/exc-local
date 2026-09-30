// Phase-23 Task 23.3.8 — defensive guards for historical market-data
// reads (spec §2.7, §16.6, §24 #325).
//
// Three mechanisms, shared by every history surface in this package
// (historical.go, tick_data_api.go, export.go, block_trades.go,
// swap_rates.go, sentiment.go):
//
//   - Query timeout: a hard 10s context.WithTimeout bounds every
//     ClickHouse history query; handlers translate a deadline trip into
//     HISTORICAL_QUERY_TIMEOUT (HTTP 504, §23) with a narrower-range hint.
//   - Repeated-query cache: serialized responses for IDENTICAL
//     method+path+query requests are parked in Redis for 60s — but ONLY
//     for fully closed intervals (to < now − publicationDelay); an open
//     interval cached would freeze the tape mid-flight. The cache is
//     strictly best-effort: a Redis outage must never block or fail the
//     query — misses and store failures both fall through to ClickHouse.
//   - Pre-open privacy masking: participant identifiers and institutional
//     order tags on history projections are masked for rows flagged
//     pre-open at ingest (24/5 venue — the CH schemas carry no flag
//     column, so "pre-open" is resolved against the session open the
//     caller supplies). The PUBLIC tape additionally masks participant
//     identity unconditionally (spec §24 #325 "mask counterparty
//     identities in public tapes") — MaskParticipantFields covers that;
//     MaskPreOpen covers the bounded pre-open window.
package marketdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// §24 #325 canonical values.
const (
	// DefaultHistoryQueryTimeout is the hard ceiling on one historical
	// data query into ClickHouse (Task 23.3.8 item 1).
	DefaultHistoryQueryTimeout = 10 * time.Second
	// DefaultHistoryCacheTTL is the Redis TTL for cached closed-interval
	// responses (Task 23.3.8 item 2).
	DefaultHistoryCacheTTL = 60 * time.Second
	// DefaultPreOpenWindow bounds how far before a session open a row may
	// still count as pre-open. The venue is 24/5 (weekly open Sunday
	// 21:00 UTC); a pre-open auction/indicative window is minutes, never
	// days — the bound prevents a session open timestamp from masking
	// rows that belong to earlier sessions.
	DefaultPreOpenWindow = 15 * time.Minute
	// historyCachePrefix namespaces the response-cache keys.
	historyCachePrefix = "histq:"
)

// HistoryQueryGuard carries the defensive-read parameters for the
// history surfaces. Zero fields fall back to the §24 #325 defaults, so
// a zero-value guard is a valid strict guard.
type HistoryQueryGuard struct {
	// Timeout is the hard cap on one ClickHouse history query
	// (≤0 → 10s).
	Timeout time.Duration
	// CacheTTL is the Redis TTL applied to cached closed-interval
	// responses (≤0 → 60s).
	CacheTTL time.Duration
	// PreOpenWindow bounds [open−window, open) as the pre-open masking
	// interval (≤0 → 15min). See DefaultPreOpenWindow.
	PreOpenWindow time.Duration
	// Now supplies the clock for closed-interval and pre-open
	// computations; nil → time.Now (UTC).
	Now func() time.Time
}

func (g HistoryQueryGuard) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return DefaultHistoryQueryTimeout
}

func (g HistoryQueryGuard) cacheTTL() time.Duration {
	if g.CacheTTL > 0 {
		return g.CacheTTL
	}
	return DefaultHistoryCacheTTL
}

func (g HistoryQueryGuard) preOpenWindow() time.Duration {
	if g.PreOpenWindow > 0 {
		return g.PreOpenWindow
	}
	return DefaultPreOpenWindow
}

// MaskingWindow returns the effective pre-open window used by
// MaskPreOpenWindowed (exported so handler code sharing a guard uses
// the same bound).
func (g HistoryQueryGuard) MaskingWindow() time.Duration { return g.preOpenWindow() }

func (g HistoryQueryGuard) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

// Clock returns the guard's current time — the same clock that drives
// timeouts, closed-interval checks and masking so a request's view of
// "now" stays internally consistent.
func (g HistoryQueryGuard) Clock() time.Time { return g.now() }

// QueryContext returns ctx bounded by the guard timeout. Callers defer
// the cancel; a trip is detected via ctx.Err() == context.DeadlineExceeded
// (or errors.Is on the store error) and mapped to
// HISTORICAL_QUERY_TIMEOUT.
func (g HistoryQueryGuard) QueryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, g.timeout())
}

// ---------------------------------------------------------------------------
// Repeated-query cache (best-effort Redis)
// ---------------------------------------------------------------------------

// HistoryCacheKey builds the dedup key for one history request:
// method + path + negotiated format + access tier + the request query
// (params sorted — ?a=1&b=2 and ?b=2&a=1 dedup to one entry). The access
// tier and format participate because the same URL yields different
// bytes per tier (window clamping, participant fields) and per
// negotiated representation.
func HistoryCacheKey(method, path, format, accessTier string, q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(format))
	h.Write([]byte{0})
	h.Write([]byte(accessTier))
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			h.Write([]byte{0})
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write([]byte(v))
		}
	}
	return historyCachePrefix + hex.EncodeToString(h.Sum(nil))
}

// ClosedInterval reports whether the interval is fully closed for
// caching: a non-zero `to` at or before now−delay, where delay is the
// access tier's publication delay (15min free, 0 premium/staff). An
// open `to` (zero) or a `to` still inside the delay horizon is never
// cached — the tail of the tape is still moving.
func (g HistoryQueryGuard) ClosedInterval(to time.Time, delay time.Duration) bool {
	return !to.IsZero() && !to.UTC().After(g.now().Add(-delay))
}

// HistoryKV is the narrow Redis surface the response cache needs —
// every goredis.Cmdable (*redis.Client, *redis.ClusterClient,
// *redis.Ring) satisfies it; tests substitute in-memory fakes.
type HistoryKV interface {
	Get(ctx context.Context, key string) *goredis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *goredis.StatusCmd
}

// CacheGet returns the cached response bytes for key. A nil store,
// a miss, or ANY store error all report (nil, false) — the cache is
// best-effort and can never block the query path.
func (g HistoryQueryGuard) CacheGet(ctx context.Context, rdb HistoryKV, key string) ([]byte, bool) {
	if rdb == nil {
		return nil, false
	}
	res, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	return res, true
}

// CachePut stores payload under key with the guard TTL. A nil store or
// a store error is swallowed — the query result is already computed and
// the response must never depend on cache health.
func (g HistoryQueryGuard) CachePut(ctx context.Context, rdb HistoryKV, key string, payload []byte) {
	if rdb == nil || len(payload) == 0 {
		return
	}
	_ = rdb.Set(ctx, key, payload, g.cacheTTL()).Err()
}

// ---------------------------------------------------------------------------
// Pre-open privacy masking
// ---------------------------------------------------------------------------

// ParticipantFields carries the participant-identifying fields a
// history projection may expose to entitled readers — the CH `trades`
// schema columns (maker_account_id, taker_account_id, buy_order_id,
// sell_order_id per deploy/clickhouse/schema/002_trades.sql) plus an
// order tag for projections that carry one (L3 replay rows). The
// `ticks` schema carries NO participant columns (schema 001 —
// TickRowValues is ts/symbol/price/qty/side/trade_id/event_seq/
// shard_id), so masking lands on the trades/L3 projections, per the
// Task 23.3.8 remediation guidance.
type ParticipantFields struct {
	MakerAccountID int64 // buy-side account (002 naming caveat)
	TakerAccountID int64 // sell-side account
	BuyOrderID     uint64
	SellOrderID    uint64
	OrderTag       string // institutional order tag, when carried
}

// Masked reports whether p is already fully masked.
func (p *ParticipantFields) Masked() bool {
	return p.MakerAccountID == 0 && p.TakerAccountID == 0 &&
		p.BuyOrderID == 0 && p.SellOrderID == 0 && p.OrderTag == ""
}

// Mask zeroes every participant identifier — masked to "" on the wire.
func (p *ParticipantFields) Mask() {
	p.MakerAccountID, p.TakerAccountID = 0, 0
	p.BuyOrderID, p.SellOrderID = 0, 0
	p.OrderTag = ""
}

// ParticipantCarrier is the masking seam: a history row that exposes
// its event time and its (mutable) participant fields. Slices of
// *HistoryTrade satisfy it; sibling products implement the same two
// methods for their row types.
type ParticipantCarrier interface {
	EventTime() time.Time
	ParticipantView() *ParticipantFields
}

// MaskParticipantFields zeroes participant identity on EVERY row — the
// unconditional public-tape mask (spec §24 #325: counterparty
// identities never appear in public tapes). Returns the number of rows
// that were unmasked before the call.
func MaskParticipantFields[T ParticipantCarrier](rows []T) int {
	n := 0
	for _, r := range rows {
		if p := r.ParticipantView(); p != nil && !p.Masked() {
			p.Mask()
			n++
		}
	}
	return n
}

// MaskPreOpenWindowed zeroes participant identity on rows whose event
// time falls inside the pre-open window [sessionOpenUTC−window,
// sessionOpenUTC) — rows flagged pre-open at ingest. The window is
// DELIBERATELY bounded: the venue trades 24/5 with no per-day open, so
// a bare "ts < open" rule would mask rows belonging to earlier sessions
// — an over-mask defect, not a privacy control. Rows outside the window
// are untouched. A zero sessionOpenUTC masks nothing (no live pre-open
// window applies); window ≤ 0 falls back to DefaultPreOpenWindow.
// Returns the masked count.
func MaskPreOpenWindowed[T ParticipantCarrier](rows []T, sessionOpenUTC time.Time, window time.Duration) int {
	if sessionOpenUTC.IsZero() {
		return 0
	}
	if window <= 0 {
		window = DefaultPreOpenWindow
	}
	open := sessionOpenUTC.UTC()
	start := open.Add(-window)
	n := 0
	for _, r := range rows {
		ts := r.EventTime()
		if (ts.Equal(start) || ts.After(start)) && ts.Before(open) {
			if p := r.ParticipantView(); p != nil && !p.Masked() {
				p.Mask()
				n++
			}
		}
	}
	return n
}

// MaskPreOpen applies the default pre-open window — convenience for
// sibling files that carry no tuned guard.
func MaskPreOpen[T ParticipantCarrier](rows []T, sessionOpenUTC time.Time) int {
	return MaskPreOpenWindowed(rows, sessionOpenUTC, 0)
}

// VenueWeekOpenUTC returns the open of the venue's 24/5 weekly session
// containing t: Sunday 21:00 UTC (Sydney open, spec §6.7 — trading runs
// 21:00 Sun → 22:00 Fri). For t inside the weekend close it returns the
// NEXT Sunday 21:00 open — i.e. the open the next session's pre-open
// window anchors to.
func VenueWeekOpenUTC(t time.Time) time.Time {
	t = t.UTC()
	// sunday = the most recent Sunday 21:00 UTC at or before t's day —
	// the open of the week containing t. For early-Sunday t (<21:00) it
	// lands later today, which the weekend-close branch handles.
	sunday := time.Date(t.Year(), t.Month(), t.Day(), 21, 0, 0, 0, time.UTC)
	sunday = sunday.AddDate(0, 0, -int(sunday.Weekday()))
	if t.Before(sunday) {
		return sunday // early Sunday: still in the weekend close
	}
	close := sunday.AddDate(0, 0, 5).Add(time.Hour) // Friday 22:00
	if t.Before(close) {
		return sunday // in-session
	}
	return sunday.AddDate(0, 0, 7) // weekend close → next week's open
}
