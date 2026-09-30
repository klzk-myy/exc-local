// collateral_service.go — intraday dynamic collateral re-haircutting
// (Phase-19 Task 19.3.28; spec §13.6e surface, §24 #412).
//
// The CollateralMonitor subscribes to the mark-delta feed
// (MarkDeltaSource — production binds RedisMarkSource over the
// MarkChannelPattern "mark:*" psubscribe) for every eligible non-USD
// collateral currency's USD conversion pair. When a currency's mark
// deviates > 100 bps from its intraday anchor it:
//
//  1. re-anchors (a ratchet — the next trigger needs ANOTHER >100bps,
//     bounding recompute volume on trending moves);
//  2. recomputes effective collateral equity for every account the
//     CollateralAccountSource reports as exposed (production binds a
//     multi-currency-collateral account query) via the injected
//     MarginEvaluator seam — MarginService.Evaluate, whose §13.6b
//     collateral leg already reprices haircuts at the new mark;
//  3. when the fresh snapshot breaches (status MARGIN_CALL or
//     LIQUIDATING) invokes the MarginCallEvaluator seam — production
//     binds MarginCallService.Evaluate — so the §13.3 lifecycle emits
//     the margin-call/stop-out trigger through the existing machinery.
//
// The intraday anchor is the first mark observed on the UTC day
// (24/5 FX session boundary); a symbol's anchor resets at each UTC
// day roll and re-bases on every trigger.
//
// Fail-closed (§2.7): tick decode errors are the feed's problem (the
// source drops malformed frames); account recompute failures are logged
// and page ops after the configured streak — a collateral-currency
// crash that cannot be re-evaluated is a risk-blind spot, never a
// silent pass.
package risk

import (
	"context"
	"fmt"
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// collateralTriggerBps is the §13.6e/Task-19.3.28 deviation trigger.
const collateralTriggerBps = 100.0

// collateralRecomputeCooldown bounds per-account re-evaluations after a
// trigger so a sustained move cannot spin the margin pipeline.
const collateralRecomputeCooldown = 10 * time.Second

// collateralPairRefresh is the cadence for re-resolving the eligible
// currency→conversion-pair map (schedule edits pick up within a minute;
// schedule haircuts themselves apply per-valuation).
const collateralPairRefresh = time.Minute

// ---------------------------------------------------------------------------
// Monitor seams — narrow interfaces, production binds the real services
// ---------------------------------------------------------------------------

// CollateralScheduleReader exposes the eligible non-USD collateral
// currency set — CollateralService satisfies it.
type CollateralScheduleReader interface {
	EligibleCurrencies(ctx context.Context) ([]string, error)
}

// CollateralPairSource resolves each collateral currency's USD
// conversion instrument — the same contract MarginStore.FxPairInstruments
// provides (production binds it directly).
type CollateralPairSource func(ctx context.Context, ccys []string) (map[string]FxPair, error)

// CollateralAccountSource lists the accounts whose effective collateral
// equity must be recomputed when ccy reprices — production binds the
// multi-currency-collateral account index.
type CollateralAccountSource interface {
	CollateralAccounts(ctx context.Context, ccy string) ([]int64, error)
}

// CollateralAccountFunc adapts a function to CollateralAccountSource.
type CollateralAccountFunc func(ctx context.Context, ccy string) ([]int64, error)

// CollateralAccounts implements CollateralAccountSource.
func (f CollateralAccountFunc) CollateralAccounts(ctx context.Context, ccy string) ([]int64, error) {
	return f(ctx, ccy)
}

// MarginEvaluator is the recompute seam — MarginService.Evaluate.
type MarginEvaluator interface {
	Evaluate(ctx context.Context, accountID int64) (*MarginSnapshot, error)
}

// MarginCallEvaluator is the §13.3 trigger seam —
// MarginCallService.Evaluate. Invoked per account only when the fresh
// snapshot shows a breach (status != NORMAL).
type MarginCallEvaluator interface {
	Evaluate(ctx context.Context, accountID int64) error
}

// ---------------------------------------------------------------------------
// CollateralMonitor
// ---------------------------------------------------------------------------

// CollateralMonitor watches collateral-currency marks for >100bps
// intraday moves and fans out margin re-evaluation + margin-call
// triggers. Construct via NewCollateralMonitor; drive with Run (blocking
// consume loop) or feed ticks directly via HandleTick (engine inline
// path / tests).
type CollateralMonitor struct {
	schedule CollateralScheduleReader
	marks    MarkDeltaSource
	pairs    CollateralPairSource
	accounts CollateralAccountSource
	margin   MarginEvaluator
	calls    MarginCallEvaluator
	alerter  OpsAlerter
	now      func() time.Time
	logf     func(format string, args ...any)

	triggerBps float64
	cooldown   time.Duration

	mu       sync.Mutex
	symToCcy map[string]string // conversion-pair symbol → collateral ccy
	anchor   map[string]collateralAnchor
	lastEval map[int64]time.Time
	evalErrs int // consecutive recompute failures → P1 page
}

// collateralAnchor is a symbol's intraday deviation anchor: the UTC day
// the anchor was set on plus the anchoring mark.
type collateralAnchor struct {
	day   time.Time // UTC midnight the anchor belongs to
	price decimal.Decimal
}

// CollateralMonitorDeps wires the monitor. Schedule, Marks, Margin are
// required — without any of them the monitor would either watch nothing
// or evaluate nothing; Accounts/Calls may be nil (no fan-out / no
// trigger emission) for staged wiring.
type CollateralMonitorDeps struct {
	Schedule CollateralScheduleReader
	Marks    MarkDeltaSource
	Pairs    CollateralPairSource
	Accounts CollateralAccountSource
	Margin   MarginEvaluator
	Calls    MarginCallEvaluator
	Alerter  OpsAlerter
	Now      func() time.Time
	Logf     func(format string, args ...any)

	// TriggerBps overrides the 100bps trigger (tests); <=0 = default.
	TriggerBps float64
	// Cooldown overrides the per-account recompute interval; <=0 = 10s.
	Cooldown time.Duration
}

// NewCollateralMonitor builds the monitor.
func NewCollateralMonitor(d CollateralMonitorDeps) (*CollateralMonitor, error) {
	if d.Schedule == nil {
		return nil, fmt.Errorf("collateral monitor: nil schedule reader")
	}
	if d.Marks == nil {
		return nil, fmt.Errorf("collateral monitor: nil mark delta source")
	}
	if d.Margin == nil {
		return nil, fmt.Errorf("collateral monitor: nil margin evaluator")
	}
	m := &CollateralMonitor{
		schedule: d.Schedule, marks: d.Marks, pairs: d.Pairs,
		accounts: d.Accounts, margin: d.Margin, calls: d.Calls,
		alerter: d.Alerter, now: d.Now, logf: d.Logf,
		triggerBps: collateralTriggerBps,
		cooldown:   collateralRecomputeCooldown,
		symToCcy:   map[string]string{},
		anchor:     map[string]collateralAnchor{},
		lastEval:   map[int64]time.Time{},
	}
	if d.TriggerBps > 0 {
		m.triggerBps = d.TriggerBps
	}
	if d.Cooldown > 0 {
		m.cooldown = d.Cooldown
	}
	if m.now == nil {
		m.now = func() time.Time { return time.Now().UTC() }
	}
	if m.logf == nil {
		m.logf = func(string, ...any) {}
	}
	return m, nil
}

// refreshPairs re-resolves eligible collateral currencies → conversion
// pair symbols (the watch set). Pair-source errors keep the previous
// watch set — a transient store hiccup must not blind the monitor.
func (m *CollateralMonitor) refreshPairs(ctx context.Context) {
	if m.pairs == nil {
		return
	}
	ccys, err := m.schedule.EligibleCurrencies(ctx)
	if err != nil {
		m.logf("collateral monitor: eligible currencies read: %v", err)
		return
	}
	pairs, err := m.pairs(ctx, ccys)
	if err != nil {
		m.logf("collateral monitor: pair resolution: %v", err)
		return
	}
	m.mu.Lock()
	next := map[string]string{}
	for ccy, fp := range pairs {
		if fp.Symbol != "" {
			next[fp.Symbol] = ccy
		}
	}
	m.symToCcy = next
	m.mu.Unlock()
}

// Run resolves the watch set, refreshes it every minute, and consumes
// mark ticks until ctx ends. The returned error is the feed failure —
// callers restart the monitor (the delta source contract closes the
// channel on termination).
func (m *CollateralMonitor) Run(ctx context.Context) error {
	m.refreshPairs(ctx)
	ticks, err := m.marks.Marks(ctx)
	if err != nil {
		return fmt.Errorf("collateral monitor: mark source: %w", err)
	}
	refresh := time.NewTicker(collateralPairRefresh)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refresh.C:
			m.refreshPairs(ctx)
		case tick, ok := <-ticks:
			if !ok {
				return fmt.Errorf("collateral monitor: mark feed closed")
			}
			m.HandleTick(ctx, tick)
		}
	}
}

// HandleTick processes one mark tick — the engine's inline feed and the
// Run loop share this path. Non-watch-set symbols return immediately.
func (m *CollateralMonitor) HandleTick(ctx context.Context, tick MarkTick) {
	m.mu.Lock()
	ccy, watched := m.symToCcy[tick.Symbol]
	m.mu.Unlock()
	if !watched || !tick.Price.IsPositive() {
		return
	}
	if !m.checkDeviation(tick) {
		return
	}
	m.recompute(ctx, ccy)
}

// checkDeviation maintains the intraday anchor and reports whether the
// tick breaches the trigger band. UTC-day roll or a trigger re-bases the
// anchor (ratchet semantics).
func (m *CollateralMonitor) checkDeviation(tick MarkTick) bool {
	day := tick.Ts.UTC().Truncate(24 * time.Hour)
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.anchor[tick.Symbol]
	if !ok || !a.day.Equal(day) || !a.price.IsPositive() {
		m.anchor[tick.Symbol] = collateralAnchor{day: day, price: tick.Price}
		return false
	}
	diff := tick.Price.Sub(a.price).Abs()
	bps := diff.Div(a.price).Mul(decimal.NewFromInt(10000)).InexactFloat64()
	if bps <= m.triggerBps {
		return false
	}
	m.anchor[tick.Symbol] = collateralAnchor{day: day, price: tick.Price}
	return true
}

// recompute fans out margin re-evaluation to the exposed accounts and
// drives the §13.3 trigger seam on breach. Per-account cooldown keeps a
// trending move from stampeding the pipeline.
func (m *CollateralMonitor) recompute(ctx context.Context, ccy string) {
	if m.accounts == nil {
		return
	}
	accts, err := m.accounts.CollateralAccounts(ctx, ccy)
	if err != nil {
		m.logf("collateral monitor: account lookup %s: %v", ccy, err)
		return
	}
	now := m.now()
	for _, acct := range accts {
		m.mu.Lock()
		if t, ok := m.lastEval[acct]; ok && now.Sub(t) < m.cooldown {
			m.mu.Unlock()
			continue
		}
		m.lastEval[acct] = now
		m.mu.Unlock()

		snap, err := m.margin.Evaluate(ctx, acct)
		if err != nil {
			m.noteEvalErr(acct, ccy, err)
			continue
		}
		m.mu.Lock()
		m.evalErrs = 0
		m.mu.Unlock()
		if snap == nil || snap.Status == "NORMAL" {
			continue
		}
		// Breach — emit through the §13.3 lifecycle seam (production
		// binds MarginCallService.Evaluate; it owns window keys, the
		// persistent order block, notifications and the liquidation
		// enqueue path).
		if m.calls != nil {
			if cerr := m.calls.Evaluate(ctx, acct); cerr != nil {
				m.logf("collateral monitor: margin-call evaluate acct %d (%s move): %v",
					acct, ccy, cerr)
			}
		}
	}
	m.evictEvals(now)
}

// noteEvalErr records a recompute failure and pages ops on a streak —
// repeated failures during a collateral-currency move mean equity may
// be over-stated against fresh marks (the §2.7 blind-spot case).
func (m *CollateralMonitor) noteEvalErr(acct int64, ccy string, err error) {
	m.mu.Lock()
	m.evalErrs++
	n := m.evalErrs
	m.mu.Unlock()
	m.logf("collateral monitor: margin evaluate acct %d (%s move): %v", acct, ccy, err)
	if n >= 3 && m.alerter != nil {
		actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if aerr := m.alerter.Raise(actx, OpsAlert{
			Severity: SeverityP1, Code: "COLLATERAL_REEVAL_FAILED",
			Summary: fmt.Sprintf("collateral re-evaluation failing (%d consecutive) during %s move",
				n, ccy),
			Details: map[string]string{"currency": ccy},
		}); aerr != nil {
			m.logf("collateral monitor: alert dispatch failed: %v", aerr)
		}
	}
}

// evictEvals drops cooldown entries older than the cooldown so the map
// cannot grow unboundedly across accounts.
func (m *CollateralMonitor) evictEvals(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for acct, t := range m.lastEval {
		if now.Sub(t) > m.cooldown {
			delete(m.lastEval, acct)
		}
	}
}

// WatchedSymbols reports the current watch set (tests / ops debug).
func (m *CollateralMonitor) WatchedSymbols() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.symToCcy))
	for k, v := range m.symToCcy {
		out[k] = v
	}
	return out
}
