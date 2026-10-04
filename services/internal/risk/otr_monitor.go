// otr_monitor.go — Phase-13 Task 13.3.6: order-to-trade ratio (OTR)
// monitoring per MiFID II RTS 9.
//
// Every order event (new / modify / cancel) is counted in the Redis
// sliding-window zset `otr:events:{account}:{symbol}`; every fill counts
// in `otr:trades:{account}:{symbol}`. A breach is declared when, inside
// the account's resolved window (risk_limits.otr_window, default 60s —
// migration 047),
//
//	events > max_order_to_trade_ratio × max(trades, 1)
//
// (default ratio 500 — the §13.6a canonical figure). The effective
// ratio/window resolve through LimitsService's existing most-specific-
// wins lattice. The §9.6 market-maker allowance is mm_programs.
// otr_allowance (migration 045, Phase-18 Task 18.3.10), consulted via
// the WithMMAllowance seam: it applies ONLY over the venue default —
// an explicitly-scoped risk_limits row always wins, so a Risk-Manager
// override can never be silently loosened by program enrollment
// (supersedes the original 047-era design note that scoped risk_limits
// rows were the only MM mechanism).
//
// On breach the monitor writes `otr:breach:{account}` (value carries the
// observed counts for ops, flag TTL = 2×window as a decay safety net)
// and indexes the account in the `otr:breach:index` set so the sweeper
// can re-evaluate without a keyspace scan. The flag clears when EVERY
// active (account, symbol) pair returns under its limit — fills that
// reduce the ratio re-evaluate immediately, and pure window decay is
// handled by the Run sweeper. While the flag stands, orders.Service
// admission rejects new orders with OTR_LIMIT_EXCEEDED (cancels never
// consult the gate), and the engine's SuspensionRefresher feeds the same
// keyspace into PreTradeChecker — so a gateway bypass still hits the
// C++ fail-closed path.
//
// Fail-closed (spec §2.7): a Redis error on Admission rejects new
// orders; count failures increment otr_count_errors_total and mark the
// monitor degraded so the next Admission also fails closed while the
// outage persists.
package risk

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/observability"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// CodeOtrLimitExceeded is the canonical spec §23 rejection (HTTP 429) —
// already registered in errs; do not re-register.
const CodeOtrLimitExceeded = "OTR_LIMIT_EXCEEDED"

// otrBreachIndexKey is the Redis set of currently-breached account ids —
// the sweeper's worklist; C++ never matches it (KEYS otr:breach:* with
// the account-id parser rejecting the non-numeric "index" member name).
const otrBreachIndexKey = "otr:breach:index"

// Redis key builders — spec §4 naming, exact contract with the C++
// SuspensionRefresher (core/src/risk/SuspensionFlags.cpp).
func OtrEventsKey(accountID int64, symbol string) string {
	return fmt.Sprintf("otr:events:%d:%s", accountID, symbol)
}

// OtrTradesKey is the fill-side sliding window.
func OtrTradesKey(accountID int64, symbol string) string {
	return fmt.Sprintf("otr:trades:%d:%s", accountID, symbol)
}

// OtrBreachKey is the account-level breach flag consumed by the Go
// admission gate and the C++ PreTradeChecker.
func OtrBreachKey(accountID int64) string {
	return fmt.Sprintf("otr:breach:%d", accountID)
}

func otrActiveSymbolsKey(accountID int64) string {
	return fmt.Sprintf("otr:active:%d", accountID)
}

func otrSeqKey(accountID int64, symbol string) string {
	return fmt.Sprintf("otr:seq:%d:%s", accountID, symbol)
}

// otrRecordScript prunes the sliding windows, optionally records one
// event/fill member, then evaluates the pair's ratio. When the pair
// breaches it raises the account flag (+ index); the under-limit path
// never clears — another active pair may legitimately hold the flag, so
// clearing is owned by the Go-side all-pairs re-evaluation
// (reevaluateAccount) and the sweeper.
//
//	KEYS[1] events zset        KEYS[4] breach index set
//	KEYS[2] trades zset        KEYS[5] active-symbols set
//	KEYS[3] breach flag        KEYS[6] member seq counter
//	ARGV[1] now_ms   ARGV[2] window_ms   ARGV[3] ratio   ARGV[4] ttl_ms
//	ARGV[5] symbol   ARGV[6] incr: events|trades|none    ARGV[7] account
//	→ {events:int, trades:int, breached:0|1}
const otrRecordScript = `
local now   = tonumber(ARGV[1])
local win   = tonumber(ARGV[2])
local ratio = tonumber(ARGV[3])
local ttl   = tonumber(ARGV[4])
local cutoff = tostring(now - win)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', cutoff)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', cutoff)
if ARGV[6] ~= 'none' then
  local seq = redis.call('INCR', KEYS[6])
  redis.call('PEXPIRE', KEYS[6], ttl)
  local target = KEYS[1]
  if ARGV[6] == 'trades' then target = KEYS[2] end
  redis.call('ZADD', target, now, tostring(seq))
end
redis.call('PEXPIRE', KEYS[1], ttl)
redis.call('PEXPIRE', KEYS[2], ttl)
redis.call('SADD', KEYS[5], ARGV[5])
redis.call('PEXPIRE', KEYS[5], ttl)
local e = redis.call('ZCARD', KEYS[1])
local t = redis.call('ZCARD', KEYS[2])
local denom = t
if denom < 1 then denom = 1 end
local breached = (e > ratio * denom)
if breached then
  redis.call('SET', KEYS[3],
    'OTR_LIMIT_EXCEEDED events=' .. e .. ' trades=' .. t ..
    ' ratio=' .. ARGV[3] .. ' window_ms=' .. ARGV[2] ..
    ' symbol=' .. ARGV[5], 'PX', ttl)
  redis.call('SADD', KEYS[4], ARGV[7])
  redis.call('PEXPIRE', KEYS[4], ttl)
end
-- 4th element: the account-level breach flag AFTER this event counted —
-- lets the order-admission path fold Event+Admission into one eval.
local flagged = redis.call('EXISTS', KEYS[3])
return {e, t, breached, flagged}
`

// OtrLimitSource resolves the effective OTR ratio + window per
// (account, tier, symbol) — *LimitsService satisfies it.
type OtrLimitSource interface {
	EffectiveLimits(accountID int64, tier, symbol string) EffectiveLimits
}

// OtrMMAllowanceSource resolves the §9.6 market-maker OTR allowance
// for (account, symbol) — *marketmaking.Service satisfies it in
// production (migration-045 mm_programs.otr_allowance). A nil return
// means no program coverage; the allowance never LOOSENS an explicitly
// scoped risk_limits row — it applies only when the resolved ratio is
// the venue default.
type OtrMMAllowanceSource interface {
	OtrAllowance(ctx context.Context, accountID int64, symbol string) *decimal.Decimal
}

// OtrMetrics bound to the Prometheus registry.
type otrMetrics struct {
	mu        sync.Mutex            // guards ratios + breached maps
	ratios    map[[2]string]float64 // (account|symbol) → last ratio
	breached  map[int64]struct{}    // locally observed breach flags
	breaches  atomic.Uint64         // lifetime breach transitions
	countErrs atomic.Uint64         // Redis failures on count paths
	reg       *observability.Registry
}

// OtrMonitor counts order events and fills, evaluates the ratio, and
// owns the breach flag lifecycle.
type OtrMonitor struct {
	rdb    goredis.Cmdable
	limits OtrLimitSource
	mm     OtrMMAllowanceSource                                       // optional §9.6 MM allowance
	sink   observability.Sink                                         // optional P2 alert sink
	tier   func(ctx context.Context, accountID int64) (string, error) // optional sweep-time tier resolution
	logf   func(format string, args ...any)
	now    func() time.Time

	m otrMetrics
}

// NewOtrMonitor builds the monitor. rdb and limits must be non-nil —
// a monitor without its counters or its limit source is not a monitor
// (fail-closed: callers should skip construction instead of wiring nil).
func NewOtrMonitor(rdb goredis.Cmdable, limits OtrLimitSource) *OtrMonitor {
	m := &OtrMonitor{
		rdb: rdb, limits: limits,
		now:  time.Now,
		logf: func(string, ...any) {},
	}
	m.m.ratios = map[[2]string]float64{}
	m.m.breached = map[int64]struct{}{}
	return m
}

// WithAlerts binds the P2 alert sink (observability.FanoutSink /
// PublisherSink / LogSink). Nil disables alert dispatch.
func (m *OtrMonitor) WithAlerts(s observability.Sink) *OtrMonitor {
	m.sink = s
	return m
}

// WithMMAllowance binds the §9.6 market-maker allowance lookup
// (*marketmaking.Service). Nil disables — registered MMs then fall
// back to the venue/scoped ratios.
func (m *OtrMonitor) WithMMAllowance(src OtrMMAllowanceSource) *OtrMonitor {
	m.mm = src
	return m
}

// WithTierResolver binds the account→kyc_tier lookup used by the
// sweeper's re-evaluation (admission-time counts carry the caller's
// tier; the sweep needs its own source).
func (m *OtrMonitor) WithTierResolver(fn func(ctx context.Context, accountID int64) (string, error)) *OtrMonitor {
	m.tier = fn
	return m
}

// WithLogger binds the operator log line (log.Warn wrapper).
func (m *OtrMonitor) WithLogger(fn func(format string, args ...any)) *OtrMonitor {
	if fn != nil {
		m.logf = fn
	}
	return m
}

// WithMetrics registers the OTR gauges/counters on the Prometheus
// registry (Task 7.3.4 exposition):
//
//	exchange_otr_ratio{account_id,symbol}   — last computed events/trades ratio
//	exchange_otr_breach_active{account_id}  — 1 while the flag stands
//	exchange_otr_breaches_total             — lifetime breach transitions
//	exchange_otr_count_errors_total         — Redis failures on count paths
func (m *OtrMonitor) WithMetrics(reg *observability.Registry) *OtrMonitor {
	if reg == nil {
		return m
	}
	m.m.reg = reg
	reg.VecFunc("exchange_otr_ratio",
		"MiFID II RTS 9 order-to-trade ratio, last evaluation per account/symbol",
		"gauge", func() []observability.PullSample {
			var out []observability.PullSample
			m.m.mu.Lock()
			defer m.m.mu.Unlock()
			for k, v := range m.m.ratios {
				out = append(out, observability.PullSample{
					Labels: []string{"account_id", k[0], "symbol", k[1]},
					Value:  v,
				})
			}
			return out
		})
	reg.VecFunc("exchange_otr_breach_active",
		"1 while the account's otr:breach flag stands (0/1)", "gauge",
		func() []observability.PullSample {
			var out []observability.PullSample
			m.m.mu.Lock()
			defer m.m.mu.Unlock()
			for id := range m.m.breached {
				out = append(out, observability.PullSample{
					Labels: []string{"account_id", strconv.FormatInt(id, 10)},
					Value:  1,
				})
			}
			return out
		})
	reg.CounterFunc("exchange_otr_breaches_total",
		"Lifetime OTR breach transitions (flag raised)",
		func() float64 { return float64(m.m.breaches.Load()) })
	reg.CounterFunc("exchange_otr_count_errors_total",
		"Redis failures on OTR count paths",
		func() float64 { return float64(m.m.countErrs.Load()) })
	return m
}

// otrPairKeys are the per-(account,symbol) key tuple.
type otrPairKeys struct {
	events, trades, seq string
}

func pairKeys(accountID int64, symbol string) otrPairKeys {
	return otrPairKeys{
		events: OtrEventsKey(accountID, symbol),
		trades: OtrTradesKey(accountID, symbol),
		seq:    otrSeqKey(accountID, symbol),
	}
}

// evalOne runs the Lua record/eval for one pair. incr is "events",
// "trades" or "none" (pure re-evaluation). Returns (events, trades,
// breachedThisPair, accountFlagged) — flagged is the account-level
// breach flag after the increment, which the admission path consumes
// to skip a second EXISTS round trip.
func (m *OtrMonitor) evalOne(ctx context.Context, accountID int64,
	tier, symbol, incr string) (events, trades int64, breached, flagged bool, err error) {
	lim := m.limits.EffectiveLimits(accountID, tier, symbol)
	ratio := DefaultOtrRatio
	if lim.MaxOrderToTradeRatio != nil && lim.MaxOrderToTradeRatio.IsPositive() {
		ratio = *lim.MaxOrderToTradeRatio
	}
	// §9.6 MM allowance (migration-045 otr_allowance): raises the cap
	// ONLY over the venue default — an explicitly scoped risk_limits
	// row is an operator override and always wins. A lookup miss
	// degrades to the resolved ratio (the safe direction: absent
	// allowance can only tighten).
	if m.mm != nil && ratio.Equal(DefaultOtrRatio) {
		if a := m.mm.OtrAllowance(ctx, accountID, symbol); a != nil && a.IsPositive() {
			ratio = *a
		}
	}
	window := lim.OtrWindow
	if window <= 0 {
		window = DefaultOtrWindow
	}
	ttlMs := 2 * window.Milliseconds()
	if ttlMs < 1000 {
		ttlMs = 1000 // floor so keys never linger at zero TTL
	}
	k := pairKeys(accountID, symbol)
	res, err := m.rdb.Eval(ctx, otrRecordScript,
		[]string{k.events, k.trades, OtrBreachKey(accountID),
			otrBreachIndexKey, otrActiveSymbolsKey(accountID), k.seq},
		m.now().UnixMilli(), window.Milliseconds(), ratio.String(),
		ttlMs, symbol, incr, strconv.FormatInt(accountID, 10)).Result()
	if err != nil {
		return 0, 0, false, false, err
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 4 {
		return 0, 0, false, false, fmt.Errorf("otr eval: unexpected lua result %v", res)
	}
	events = toI64(arr[0])
	trades = toI64(arr[1])
	breached = toI64(arr[2]) == 1
	flagged = toI64(arr[3]) == 1
	return events, trades, breached, flagged, nil
}

func toI64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		x, _ := strconv.ParseInt(n, 10, 64)
		return x
	default:
		return 0
	}
}

// recordHit updates the metrics/alert bookkeeping after a Lua eval.
// pairBreached is the flag-raising verdict from the pair that was
// touched; fill-side calls that returned under-limit run
// reevaluateAccount separately.
func (m *OtrMonitor) recordHit(ctx context.Context, accountID int64,
	symbol string, events, trades int64, pairBreached bool) {
	// The gauge reports the actual ratio (events / max(trades,1)) — the
	// cap lives on the limit side, not the metric.
	denom := trades
	if denom < 1 {
		denom = 1
	}
	m.m.mu.Lock()
	m.m.ratios[[2]string{strconv.FormatInt(accountID, 10), symbol}] =
		float64(events) / float64(denom)
	if !pairBreached {
		m.m.mu.Unlock()
		return
	}
	_, seen := m.m.breached[accountID]
	m.m.breached[accountID] = struct{}{}
	m.m.mu.Unlock()
	if !seen {
		m.m.breaches.Add(1)
		m.raise(ctx, observability.Alert{
			Rule:     "otr_limit_breach",
			Severity: observability.SeverityP2,
			Code:     CodeOtrLimitExceeded,
			Summary:  fmt.Sprintf("OTR limit breached for account %d", accountID),
			Status:   "firing",
			Details: map[string]string{
				"account_id": strconv.FormatInt(accountID, 10),
				"symbol":     symbol,
				"events":     strconv.FormatInt(events, 10),
				"trades":     strconv.FormatInt(trades, 10),
			},
			FiredAt: m.now().UTC().Format(time.RFC3339Nano),
		})
	}
}

func (m *OtrMonitor) raise(ctx context.Context, a observability.Alert) {
	if m.sink == nil {
		return
	}
	if err := m.sink.Raise(ctx, a); err != nil {
		m.logf("otr: alert dispatch failed: %v", err)
	}
}

// countFail marks a count-path Redis failure — the monitor is degraded
// and Admission fails closed while the flag read is unreachable.
func (m *OtrMonitor) countFail(op string, accountID int64, err error) {
	m.m.countErrs.Add(1)
	m.logf("otr: %s count failed for account %d: %v", op, accountID, err)
}

// Event records one order event (new / modify / cancel) for the pair
// and evaluates the breach rule. Best-effort by contract: a Redis error
// is counted + logged, not returned — Admission fails closed on its own
// read anyway, and a counter hiccup must not strand the cancel path.
func (m *OtrMonitor) Event(ctx context.Context, accountID int64, tier, symbol string) {
	e, t, breached, _, err := m.evalOne(ctx, accountID, tier, symbol, "events")
	if err != nil {
		m.countFail("event", accountID, err)
		return
	}
	m.recordHit(ctx, accountID, symbol, e, t, breached)
}

// EventAdmission folds the orders.Service Event+Admission pair into a
// single Lua eval: the event counts first (breached accounts cannot
// spam-escape the window), then the script reports the account-level
// flag — identical ordering to the two-call contract, one round trip.
// An eval error both marks the monitor degraded and fails closed as
// SERVICE_DEGRADED, matching Admission's standalone contract.
func (m *OtrMonitor) EventAdmission(ctx context.Context,
	accountID int64, tier, symbol string) error {
	e, t, breached, flagged, err := m.evalOne(ctx, accountID, tier, symbol, "events")
	if err != nil {
		m.countFail("event+admission", accountID, err)
		return excerrors.New("SERVICE_DEGRADED",
			fmt.Sprintf("otr state unreadable — new orders rejected (fail closed): %v", err))
	}
	m.recordHit(ctx, accountID, symbol, e, t, breached)
	if flagged {
		return excerrors.New(CodeOtrLimitExceeded,
			fmt.Sprintf("order-to-trade ratio limit breached for account %d — cancels only", accountID))
	}
	return nil
}

// Fill records one executed trade for the pair. A fill raises the
// denominator, so an under-limit verdict re-evaluates every active pair
// and clears the account flag when nothing still breaches.
func (m *OtrMonitor) Fill(ctx context.Context, accountID int64, tier, symbol string) {
	e, t, breached, _, err := m.evalOne(ctx, accountID, tier, symbol, "trades")
	if err != nil {
		m.countFail("fill", accountID, err)
		return
	}
	m.recordHit(ctx, accountID, symbol, e, t, breached)
	if !breached {
		m.reevaluateAccount(ctx, accountID, tier)
	}
}

// Admission is the orders.Service gate: a standing breach flag rejects
// new orders with OTR_LIMIT_EXCEEDED; a Redis read error fails closed
// with SERVICE_DEGRADED. Cancels never consult this gate (Task 13.3.6:
// cancel-only during breach).
func (m *OtrMonitor) Admission(ctx context.Context, accountID int64) error {
	n, err := m.rdb.Exists(ctx, OtrBreachKey(accountID)).Result()
	if err != nil {
		m.m.countErrs.Add(1)
		return excerrors.New("SERVICE_DEGRADED",
			fmt.Sprintf("otr state unreadable — new orders rejected (fail closed): %v", err))
	}
	if n > 0 {
		return excerrors.New(CodeOtrLimitExceeded,
			fmt.Sprintf("order-to-trade ratio limit breached for account %d — cancels only", accountID))
	}
	return nil
}

// resolveTier returns the account's KYC tier for limit resolution —
// the configured resolver, else "" (tier-scoped rows simply skip).
func (m *OtrMonitor) resolveTier(ctx context.Context, accountID int64) string {
	if m.tier == nil {
		return ""
	}
	t, err := m.tier(ctx, accountID)
	if err != nil {
		m.logf("otr: tier resolve for account %d failed: %v", accountID, err)
		return ""
	}
	return t
}

// reevaluateAccount re-checks every active (account,symbol) pair and
// clears otr:breach:{account} only when ALL pairs are under their
// limits — one pair still over the line keeps the flag standing.
func (m *OtrMonitor) reevaluateAccount(ctx context.Context, accountID int64, tier string) {
	syms, err := m.rdb.SMembers(ctx, otrActiveSymbolsKey(accountID)).Result()
	if err != nil || len(syms) == 0 {
		if err != nil {
			m.countFail("reeval", accountID, err)
		}
		return
	}
	for _, sym := range syms {
		_, _, b, _, err := m.evalOne(ctx, accountID, tier, sym, "none")
		if err != nil {
			m.countFail("reeval", accountID, err)
			return // fail closed — do not clear on a partial read
		}
		if b {
			return // still breached on this pair
		}
	}
	pipe := m.rdb.TxPipeline()
	pipe.Del(ctx, OtrBreachKey(accountID))
	pipe.SRem(ctx, otrBreachIndexKey, strconv.FormatInt(accountID, 10))
	if _, err := pipe.Exec(ctx); err != nil {
		m.countFail("clear", accountID, err)
		return
	}
	m.m.mu.Lock()
	_, was := m.m.breached[accountID]
	delete(m.m.breached, accountID)
	m.m.mu.Unlock()
	if was {
		m.raise(ctx, observability.Alert{
			Rule:     "otr_limit_breach",
			Severity: observability.SeverityP2,
			Code:     CodeOtrLimitExceeded,
			Summary:  fmt.Sprintf("OTR breach cleared for account %d", accountID),
			Status:   "resolved",
			Details:  map[string]string{"account_id": strconv.FormatInt(accountID, 10)},
			FiredAt:  m.now().UTC().Format(time.RFC3339Nano),
		})
	}
}

// Run is the decay sweeper: breached accounts whose windows have aged
// out without new events (nothing keeps re-evaluating them) are
// re-checked and cleared. interval <= 0 defaults to 1s.
func (m *OtrMonitor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweep(ctx)
		}
	}
}

// sweep re-evaluates every account in the breach index.
func (m *OtrMonitor) sweep(ctx context.Context) {
	ids, err := m.rdb.SMembers(ctx, otrBreachIndexKey).Result()
	if err != nil {
		m.m.countErrs.Add(1)
		m.logf("otr: breach index read failed: %v", err)
		return
	}
	for _, raw := range ids {
		acct, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || acct <= 0 {
			// Foreign member — remove it so the index stays clean.
			m.rdb.SRem(ctx, otrBreachIndexKey, raw)
			continue
		}
		// The flag TTL may already have lapsed; re-evaluate either way —
		// the index entry must not outlive the flag.
		m.reevaluateAccount(ctx, acct, m.resolveTier(ctx, acct))
		if n, err := m.rdb.Exists(ctx, OtrBreachKey(acct)).Result(); err == nil && n == 0 {
			m.rdb.SRem(ctx, otrBreachIndexKey, raw)
		}
	}
}
