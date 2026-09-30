// margin_engine.go — Phase-19 Task 19.3.26: event-driven mark-price
// margin engine with priority-queue liquidation dispatch (spec §13.4a,
// §24 #410).
//
// THE DESIGN — one computation, two store speeds:
//
// The canonical account margin snapshot math lives EXCLUSIVELY in
// MarginService.evaluate (margin.go) — this file never re-derives
// margin math. The engine drives the same Evaluate code path against a
// per-account in-memory store adapter (legsStore over the engine's
// cached evaluation legs) and an in-memory MarkCache (engineMarks over
// the tick-fed mark map). A recompute is therefore pure decimal math —
// microseconds per account, no I/O — while remaining bit-identical to
// the authoritative evaluation (same code, same collateral valuator,
// same correlation/volatility seams).
//
// Structure vs price separation (§2.7 honesty):
//
//   - PRICE freshness is instantaneous: every tick updates the mark map
//     before the affected accounts re-evaluate.
//   - STRUCTURE freshness (balances, position set, FX conversion pairs,
//     threshold overrides) is bounded by LegsTTL: stale-or-missing legs
//     queue a background refresh through the full PG-backed MarginStore,
//     after which the account re-evaluates through the identical fast
//     path. A position opened <LegsTTL ago is caught by the refresh;
//     the §13.5 2s scanner remains the secondary watchdog throughout.
//
// Breach dispatch:
//
//   - CROSS/PORTFOLIO account whose recomputed level sits at-or-below the
//     resolved stop-out (category default, or the per-account
//     account_margin_thresholds override read at leg load) dispatches a
//     STOP_OUT job into the durable liquidation:queue through the
//     LiquidationDispatcher seam — same queue, same dedup/anti-stranding
//     contract the scanner uses; crash safety preserved.
//   - ISOLATED accounts are delegated to the IsolatedAccountChecker
//     seam (isolated_margin.go): per-position (allocated+uPnL)/MMR
//     levels, auto-replenish, and position-scoped ISOLATED_MARGIN_DEFICIT
//     dispatch — account-level liquidation never applies.
//   - The dispatch dedup key makes repeated enqueues safe; the engine
//     additionally suppresses re-dispatch for RedispatchInterval after a
//     successful enqueue and clears the mark on recovery. A dispatch
//     FAILURE pages P0 (an un-liquidatable breached account is the
//     worst-case blind spot) and retries on the next tick.
//
// Locking model: e.mu guards legs/index/heap/watch maps; legs structs
// and e.pairs/e.pairToCcy maps are IMMUTABLE once installed (copy-on-
// write), so the per-account evaluation runs outside the mutex on a
// captured snapshot. e.markMu guards only the mark map. HandleTick is
// safe to call concurrently but is normally driven single-threaded by
// Run's consume loop.
package risk

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Engine cadence defaults.
const (
	// EngineLegsTTL bounds structural staleness of cached evaluation
	// legs; 2s mirrors the §13.5 scanner cadence the engine supersedes.
	EngineLegsTTL = 2 * time.Second
	// EngineIndexRefresh is the wholesale OpenPositionIndex rebuild
	// cadence (incremental updates happen on every leg load).
	EngineIndexRefresh = 30 * time.Second
	// EngineDrainInterval is the cadence for re-asserting still-breached
	// heap-top accounts — belt & braces over enqueue failures between
	// ticks on unrelated symbols.
	EngineDrainInterval = 500 * time.Millisecond
	// EngineRedispatch suppresses duplicate stop-out dispatches for this
	// window; the queue's 3600s dedup already coalesces — this merely
	// bounds Redis writes while an account stays breached.
	EngineRedispatch = 30 * time.Second
	// engineRefreshQueue bounds pending leg-refresh requests; overflow
	// drops are re-requested by the next tick (legs stay stale-flagged).
	engineRefreshQueue = 4096
	// engineMaxStopOut is the largest stop-out any tier resolves
	// (institutional 100%); the drain stops scanning past it.
	engineMaxStopOutPct = int64(100)
)

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// MarkObserver is the optional tick fan-in — production binds
// *StubMarkPriceProvider so every consumed tick also feeds the
// last-trade mark cache other services read (markprice.go contract).
type MarkObserver interface {
	Observe(symbol string, price decimal.Decimal, at time.Time)
}

// MarginSnapshotSink is the margin:level publication seam — production
// binds RedisMarginSnapshotSink (HSET margin:level:{acct} +
// WriteMarginSnapshot on status change). statusChanged lets the sink
// persist only on transitions per the MarginStore contract.
type MarginSnapshotSink interface {
	Publish(ctx context.Context, snap *MarginSnapshot, statusChanged bool) error
}

// isolatedAccountChecker is the Task-19.3.27 delegation seam — the
// IsolatedMarginService satisfies it; tests may substitute fakes.
type isolatedAccountChecker interface {
	CheckAccount(ctx context.Context, legs *accountLegs, snap *MarginSnapshot,
		rateToUSD func(ccy string) (decimal.Decimal, bool)) error
}

// ---------------------------------------------------------------------------
// Cached evaluation legs + the in-memory MarginStore adapter
// ---------------------------------------------------------------------------

// accountLegs is the engine's cached copy of one account's evaluation
// inputs — IMMUTABLE after install (refresh builds a fresh struct and
// swaps the map pointer; readers never see a torn write).
type accountLegs struct {
	acct       *MarginAccount // nil ⇒ no margin_accounts row (default mode)
	category   string
	mode       MarginMode
	baseCcy    string // accounts.base_currency; USD when unresolvable
	thresholds MarginThresholds
	balances   []BalanceAmount
	positions  []MarginPosition
	ccys       map[string]bool // non-USD currencies needing conversion
	loadedAt   time.Time
}

// legsStore adapts one accountLegs snapshot + the engine's FX-pair map
// to the MarginStore seam so the UNMODIFIED MarginService.Evaluate path
// computes on in-memory data. Single-account scope: methods ignore the
// id argument's distinctness (Evaluate always passes the legs owner).
type legsStore struct {
	legs  *accountLegs
	pairs map[string]FxPair
}

var (
	_ MarginStore = legsStore{}
	_ interface {
		AccountBaseCurrency(context.Context, int64) (string, error)
	} = legsStore{}
)

func (s legsStore) MarginAccount(context.Context, int64) (*MarginAccount, error) {
	return s.legs.acct, nil
}
func (s legsStore) AccountCategory(context.Context, int64) (string, error) {
	return s.legs.category, nil
}
func (s legsStore) Balances(context.Context, int64) ([]BalanceAmount, error) {
	return s.legs.balances, nil
}
func (s legsStore) MarginPositions(context.Context, int64) ([]MarginPosition, error) {
	return s.legs.positions, nil
}
func (s legsStore) AccountBaseCurrency(context.Context, int64) (string, error) {
	return s.legs.baseCcy, nil
}
func (s legsStore) FxPairInstruments(_ context.Context, ccys []string) (map[string]FxPair, error) {
	out := make(map[string]FxPair, len(ccys))
	for _, c := range ccys {
		if fp, ok := s.pairs[c]; ok {
			out[c] = fp
		}
	}
	return out, nil
}

// Not-on-the-eval-path members of MarginStore — honest errors so a
// wiring mistake surfaces instead of silently no-opping.
func (s legsStore) SetMarginMode(context.Context, int64, MarginMode) error {
	return fmt.Errorf("margin engine legs store: SetMarginMode unsupported")
}
func (s legsStore) OpenPositionCount(context.Context, int64) (int64, error) {
	return int64(len(s.legs.positions)), nil
}
func (s legsStore) OpenPositionIndex(context.Context) (map[string][]int64, error) {
	return nil, fmt.Errorf("margin engine legs store: OpenPositionIndex unsupported")
}
func (s legsStore) WriteMarginSnapshot(context.Context, MarginSnapshot) error {
	return fmt.Errorf("margin engine legs store: WriteMarginSnapshot unsupported")
}

// engineMarks adapts the tick-fed mark map to MarkCache — BatchMarks is
// a plain map read under the mark lock (no MGET in the hot path).
type engineMarks struct{ e *MarginEngine }

func (m engineMarks) BatchMarks(_ context.Context, symbols []string) (map[string]decimal.Decimal, error) {
	out := make(map[string]decimal.Decimal, len(symbols))
	m.e.markMu.RLock()
	defer m.e.markMu.RUnlock()
	for _, s := range symbols {
		if v, ok := m.e.marks[s]; ok {
			out[s] = v
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// MarginEngine
// ---------------------------------------------------------------------------

// MarginEngine is the Task-19.3.26 event-driven evaluator.
type MarginEngine struct {
	store    MarginStore
	seed     MarkCache
	source   MarkDeltaSource
	disp     LiquidationDispatcher
	isolated isolatedAccountChecker
	sink     MarginSnapshotSink
	obs      MarkObserver
	corr     CorrelationProvider
	coll     CollateralValuator
	vol      IMMultiplierSource
	oi       OpenInterestSource
	adv      ADVSource
	thSrc    AccountThresholdsSource
	alerter  OpsAlerter
	now      func() time.Time
	logf     func(format string, args ...any)

	legsTTL    time.Duration
	indexEvery time.Duration
	drainEvery time.Duration
	redispatch time.Duration

	mu         sync.Mutex
	legs       map[int64]*accountLegs
	pairs      map[string]FxPair
	pairToCcy  map[string]string             // conversion-pair symbol → ccy
	index      map[string]map[int64]struct{} // position symbol → accounts
	acctSyms   map[int64]map[string]struct{} // account → position symbols
	ccyWatch   map[string]map[int64]struct{} // conversion ccy → accounts
	heap       *MarginLevelHeap
	breached   map[int64]time.Time
	lastStat   map[int64]string
	evalErrs   int // consecutive failures across accounts → P1 page
	refreshSet map[int64]struct{}

	markMu sync.RWMutex
	marks  map[string]decimal.Decimal

	refreshCh chan int64
	sinkErrs  int
}

// MarginEngineDeps wires the engine. Store and Dispatcher are required
// (fail-closed: no leg source or no breach path = no engine). Source may
// be nil for a PushMark-only embedding; Marks may be nil when marks are
// exclusively tick-fed.
type MarginEngineDeps struct {
	Store      MarginStore
	Marks      MarkCache
	Source     MarkDeltaSource
	Dispatcher LiquidationDispatcher
	Isolated   isolatedAccountChecker
	Sink       MarginSnapshotSink

	Correlation CorrelationProvider
	Collateral  CollateralValuator
	Volatility  IMMultiplierSource
	// OI / ADV — the §13.12 add-on denominators forwarded into the
	// per-eval MarginService; absent ⇒ pessimistic legs apply.
	OI         OpenInterestSource
	ADV        ADVSource
	Thresholds AccountThresholdsSource
	Observer   MarkObserver
	Alerter    OpsAlerter

	Now  func() time.Time
	Logf func(format string, args ...any)

	LegsTTL            time.Duration // <=0 ⇒ EngineLegsTTL
	IndexRefresh       time.Duration // <=0 ⇒ EngineIndexRefresh
	DrainInterval      time.Duration // <=0 ⇒ EngineDrainInterval
	RedispatchInterval time.Duration // <=0 ⇒ EngineRedispatch
}

// NewMarginEngine builds the engine.
func NewMarginEngine(d MarginEngineDeps) (*MarginEngine, error) {
	if d.Store == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "margin engine: store is nil")
	}
	if d.Dispatcher == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "margin engine: dispatcher is nil")
	}
	e := &MarginEngine{
		store: d.Store, seed: d.Marks, source: d.Source,
		disp: d.Dispatcher, isolated: d.Isolated, sink: d.Sink,
		obs: d.Observer, corr: d.Correlation, coll: d.Collateral,
		vol: d.Volatility, oi: d.OI, adv: d.ADV,
		thSrc: d.Thresholds, alerter: d.Alerter,
		now: d.Now, logf: d.Logf,
		legsTTL:    EngineLegsTTL,
		indexEvery: EngineIndexRefresh,
		drainEvery: EngineDrainInterval,
		redispatch: EngineRedispatch,
		legs:       map[int64]*accountLegs{},
		pairs:      map[string]FxPair{},
		pairToCcy:  map[string]string{},
		index:      map[string]map[int64]struct{}{},
		acctSyms:   map[int64]map[string]struct{}{},
		ccyWatch:   map[string]map[int64]struct{}{},
		heap:       NewMarginLevelHeap(),
		breached:   map[int64]time.Time{},
		lastStat:   map[int64]string{},
		evalErrs:   0,
		refreshSet: map[int64]struct{}{},
		marks:      map[string]decimal.Decimal{},
		refreshCh:  make(chan int64, engineRefreshQueue),
	}
	if e.legsTTL <= 0 {
		e.legsTTL = EngineLegsTTL
	}
	if d.LegsTTL > 0 {
		e.legsTTL = d.LegsTTL
	}
	if d.IndexRefresh > 0 {
		e.indexEvery = d.IndexRefresh
	}
	if d.DrainInterval > 0 {
		e.drainEvery = d.DrainInterval
	}
	if d.RedispatchInterval > 0 {
		e.redispatch = d.RedispatchInterval
	}
	if e.now == nil {
		e.now = func() time.Time { return time.Now().UTC() }
	}
	if e.logf == nil {
		e.logf = func(string, ...any) {}
	}
	return e, nil
}

// evaluator builds a per-eval MarginService bound to the account's
// captured legs — the SAME evaluation code as production, in-memory.
func (e *MarginEngine) evaluator(legs *accountLegs, pairs map[string]FxPair) (*MarginService, error) {
	return NewMarginService(MarginOptions{
		Store:       legsStore{legs: legs, pairs: pairs},
		Marks:       engineMarks{e},
		Correlation: e.corr,
		Collateral:  e.coll,
		Volatility:  e.vol,
		OI:          e.oi,
		ADV:         e.adv,
		Now:         e.now,
	})
}

// ---------------------------------------------------------------------------
// Tick consumption
// ---------------------------------------------------------------------------

// Run consumes the mark-delta feed until ctx ends (or the feed closes —
// the caller restarts the engine, same discipline as CollateralMonitor).
// It also drives the leg-refresh worker, the index rebuild ticker and
// the breached-account drain.
func (e *MarginEngine) Run(ctx context.Context) error {
	var ticks <-chan MarkTick
	if e.source != nil {
		ch, err := e.source.Marks(ctx)
		if err != nil {
			return fmt.Errorf("margin engine: mark source: %w", err)
		}
		ticks = ch
	}
	go e.refreshWorker(ctx)
	if err := e.RefreshIndex(ctx); err != nil {
		e.logf("margin engine: initial index rebuild: %v", err) // retried on ticker
	}
	idx := time.NewTicker(e.indexEvery)
	drain := time.NewTicker(e.drainEvery)
	defer idx.Stop()
	defer drain.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-idx.C:
			if err := e.RefreshIndex(ctx); err != nil {
				e.logf("margin engine: index rebuild: %v", err)
			}
		case <-drain.C:
			e.DrainBreaches(ctx)
		case tick, ok := <-ticks:
			if !ok {
				return fmt.Errorf("margin engine: mark feed closed")
			}
			e.HandleTick(ctx, tick)
		}
	}
}

// HandleTick processes one mark tick: update the mark map, find every
// account the tick touches (direct position holders plus accounts whose
// cached legs consume the symbol as a currency conversion), re-evaluate
// each in-memory, re-rank the heap, publish, and dispatch breaches —
// all before the next tick is read.
func (e *MarginEngine) HandleTick(ctx context.Context, tick MarkTick) {
	if tick.Symbol == "" || !tick.Price.IsPositive() {
		return
	}
	if e.obs != nil {
		e.obs.Observe(tick.Symbol, tick.Price, tick.Ts)
	}
	e.markMu.Lock()
	e.marks[tick.Symbol] = tick.Price
	e.markMu.Unlock()

	e.mu.Lock()
	accts := e.affectedLocked(tick.Symbol)
	e.mu.Unlock()

	overlay := map[string]decimal.Decimal{tick.Symbol: tick.Price}
	for _, acct := range accts {
		e.evaluateAccount(ctx, acct, overlay)
	}
}

// PushMark is the inline/test feed — identical to HandleTick
// (markprice.go names this the "direct PushMark hook").
func (e *MarginEngine) PushMark(ctx context.Context, tick MarkTick) {
	e.HandleTick(ctx, tick)
}

// affectedLocked is the tick→account fan-out: holders of a position in
// the symbol, plus accounts whose conversion currencies reprice through
// it (a EUR/USD tick reprices EUR balances and JPY-quoted positions'
// USD legs when EUR/USD is their conversion pair).
func (e *MarginEngine) affectedLocked(symbol string) []int64 {
	set := map[int64]struct{}{}
	for acct := range e.index[symbol] {
		set[acct] = struct{}{}
	}
	if ccy, ok := e.pairToCcy[symbol]; ok {
		for acct := range e.ccyWatch[ccy] {
			set[acct] = struct{}{}
		}
	}
	out := make([]int64, 0, len(set))
	for acct := range set {
		out = append(out, acct)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

// evaluateAccount is the hot path: capture the account's legs under the
// mutex, evaluate unlocked on the immutable snapshot, then apply heap /
// bookkeeping / publish / dispatch.
func (e *MarginEngine) evaluateAccount(ctx context.Context, accountID int64,
	overlay map[string]decimal.Decimal) {

	e.mu.Lock()
	legs := e.legs[accountID]
	if legs == nil || e.now().Sub(legs.loadedAt) > e.legsTTL {
		e.queueRefreshLocked(accountID)
	}
	if legs == nil {
		e.mu.Unlock()
		return // cold account — the refresh worker evaluates after load
	}
	pairs := e.pairs
	e.mu.Unlock()

	svc, err := e.evaluator(legs, pairs)
	if err != nil {
		e.noteEvalErr(accountID, err)
		return
	}
	snap, err := svc.EvaluateOverlay(ctx, accountID, overlay)
	if err != nil {
		e.noteEvalErr(accountID, err)
		return
	}
	e.resetEvalErrs()

	// Bookkeeping under the mutex — capture post actions for the
	// unlocked I/O tail.
	e.mu.Lock()
	e.heap.Upsert(accountID, snap.LevelPct)
	statusChanged := snap.Status != e.lastStat[accountID]
	e.lastStat[accountID] = snap.Status
	breach := false
	if legs.mode != ModeIsolated && snap.LevelPct != nil &&
		snap.LevelPct.LessThanOrEqual(legs.thresholds.StopOut) {
		breach = true
	} else if snap.LevelPct == nil ||
		snap.LevelPct.GreaterThan(legs.thresholds.StopOut) {
		delete(e.breached, accountID) // recovered — dispatch gate resets
	}
	e.mu.Unlock()

	if e.sink != nil {
		if perr := e.sink.Publish(ctx, snap, statusChanged); perr != nil {
			e.noteSinkErr(accountID, perr)
		}
	}
	if legs.mode == ModeIsolated {
		if e.isolated != nil {
			if cerr := e.isolated.CheckAccount(ctx, legs, snap,
				func(ccy string) (decimal.Decimal, bool) {
					return e.RateToUSD(ctx, ccy)
				}); cerr != nil {
				e.logf("margin engine: isolated check acct %d: %v", accountID, cerr)
			}
		}
		return
	}
	if breach {
		e.dispatchBreach(ctx, snap)
	}
}

// dispatchBreach pushes the STOP_OUT job — dedup-windowed at the engine
// AND the queue (EnqueueDedup). Failures page P0 and retry next tick.
func (e *MarginEngine) dispatchBreach(ctx context.Context, snap *MarginSnapshot) {
	now := e.now()
	e.mu.Lock()
	if t, ok := e.breached[snap.AccountID]; ok && now.Sub(t) < e.redispatch {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	enq, err := e.disp.Dispatch(ctx, StopOutJob(snap, now), snap)
	if err != nil {
		e.raiseAlert(ctx, "P0", "LIQUIDATION_DISPATCH_FAILED", fmt.Sprintf(
			"stop-out dispatch failed for account %d at level %s%%",
			snap.AccountID, pctStr(snap.LevelPct)), map[string]string{
			"account_id": fmt.Sprint(snap.AccountID),
			"reason":     LiquidationReasonStopOut})
		return
	}
	e.mu.Lock()
	e.breached[snap.AccountID] = now
	e.mu.Unlock()
	if !enq {
		e.logf("margin engine: acct %d stop-out coalesced by queue dedup", snap.AccountID)
	}
}

// DrainBreaches re-asserts every still-breached heap entry — the
// watchdog inside the event path, guarding against a dispatch that
// failed when no subsequent tick repriced that account.
func (e *MarginEngine) DrainBreaches(ctx context.Context) {
	e.mu.Lock()
	ordered := e.heap.Ordered()
	type target struct{ snapAcct int64 }
	var stale []int64
	max := decimal.NewFromInt(engineMaxStopOutPct)
	for _, en := range ordered {
		if !en.HasLevel || en.Level.GreaterThan(max) {
			break // ascending order — nothing past here can breach
		}
		legs := e.legs[en.AccountID]
		if legs == nil || legs.mode == ModeIsolated {
			continue
		}
		if !en.Level.LessThanOrEqual(legs.thresholds.StopOut) {
			continue
		}
		if t, ok := e.breached[en.AccountID]; ok && e.now().Sub(t) < e.redispatch {
			continue
		}
		stale = append(stale, en.AccountID)
	}
	e.mu.Unlock()

	for _, acct := range stale {
		// Re-evaluate fresh rather than trusting the drain-time level:
		// a recovery between heap update and drain must not dispatch.
		e.evaluateAccount(ctx, acct, nil)
	}
}

// ---------------------------------------------------------------------------
// Leg loading / refresh
// ---------------------------------------------------------------------------

// loadLegs reads the account's full structural set through the
// canonical MarginStore (PG round trips — called off the hot path by
// the refresh worker and WarmAccount) and installs a fresh immutable
// snapshot plus the incremental index/watch updates.
func (e *MarginEngine) loadLegs(ctx context.Context, accountID int64) (*accountLegs, error) {
	acct, err := e.store.MarginAccount(ctx, accountID)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "margin account", err)
	}
	cat, err := e.store.AccountCategory(ctx, accountID)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "account category", err)
	}
	balances, err := e.store.Balances(ctx, accountID)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "balances", err)
	}
	positions, err := e.store.MarginPositions(ctx, accountID)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "positions", err)
	}
	mode := DefaultMode(cat)
	if acct != nil {
		mode = acct.Mode
	}

	// Conversion universe: every balance + position quote currency.
	ccys := map[string]bool{}
	for _, b := range balances {
		ccys[b.Currency] = true
	}
	for _, p := range positions {
		ccys[p.QuoteCurrency] = true
	}
	delete(ccys, "USD")
	pairs, err := e.store.FxPairInstruments(ctx, sortedKeys(ccys))
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "fx pair instruments", err)
	}

	baseCcy := "USD"
	if bs, ok := e.store.(interface {
		AccountBaseCurrency(context.Context, int64) (string, error)
	}); ok {
		if c, cerr := bs.AccountBaseCurrency(ctx, accountID); cerr == nil && c != "" {
			baseCcy = c
		}
	}

	th := ThresholdsFor(cat)
	if e.thSrc != nil {
		ov, terr := e.thSrc.Thresholds(ctx, accountID)
		if terr != nil {
			// A failed override read must NOT silently fall back to a
			// LOOSER default (margin.go's contract): fail the load.
			return nil, errCode(CodeRiskLimitsInternal, "margin thresholds", terr)
		}
		th = ov
	}

	// Seed marks for every symbol this account needs (position marks +
	// conversion pairs) — absent marks stay absent (evaluate falls back
	// to stored/entry marks per the canonical path).
	if e.seed != nil {
		syms := map[string]bool{}
		for _, p := range positions {
			syms[p.Symbol] = true
		}
		for _, fp := range pairs {
			syms[fp.Symbol] = true
		}
		if seeds, serr := e.seed.BatchMarks(ctx, sortedKeys(syms)); serr == nil {
			e.markMu.Lock()
			for k, v := range seeds {
				e.marks[k] = v
			}
			e.markMu.Unlock()
		}
	}

	legs := &accountLegs{
		acct: acct, category: cat, mode: mode, baseCcy: baseCcy,
		thresholds: th, balances: balances, positions: positions,
		ccys: ccys, loadedAt: e.now(),
	}
	e.installLegs(accountID, legs, pairs)
	return legs, nil
}

// installLegs swaps the account's immutable legs and maintains the
// derived indexes (copy-on-write pairs, incremental position index,
// currency watch). Position symbols are recorded into the SAME
// index the rebuild owns — legs are the freshest truth for their
// account.
func (e *MarginEngine) installLegs(accountID int64, legs *accountLegs, newPairs map[string]FxPair) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(newPairs) > 0 {
		merged := make(map[string]FxPair, len(e.pairs)+len(newPairs))
		for k, v := range e.pairs {
			merged[k] = v
		}
		for k, v := range newPairs {
			merged[k] = v
		}
		p2c := make(map[string]string, len(merged))
		for c, fp := range merged {
			if fp.Symbol != "" {
				p2c[fp.Symbol] = c
			}
		}
		e.pairs = merged
		e.pairToCcy = p2c
	}

	// Currency watch: drop old memberships, add new.
	if old := e.legs[accountID]; old != nil {
		for c := range old.ccys {
			if s, ok := e.ccyWatch[c]; ok {
				delete(s, accountID)
				if len(s) == 0 {
					delete(e.ccyWatch, c)
				}
			}
		}
	}
	for c := range legs.ccys {
		s, ok := e.ccyWatch[c]
		if !ok {
			s = map[int64]struct{}{}
			e.ccyWatch[c] = s
		}
		s[accountID] = struct{}{}
	}

	// Position-symbol index for THIS account: supersede prior symbols.
	if oldSyms, ok := e.acctSyms[accountID]; ok {
		for sym := range oldSyms {
			if s, ok2 := e.index[sym]; ok2 {
				delete(s, accountID)
				if len(s) == 0 {
					delete(e.index, sym)
				}
			}
		}
	}
	syms := map[string]struct{}{}
	for _, p := range legs.positions {
		syms[p.Symbol] = struct{}{}
		s, ok := e.index[p.Symbol]
		if !ok {
			s = map[int64]struct{}{}
			e.index[p.Symbol] = s
		}
		s[accountID] = struct{}{}
	}
	if len(syms) > 0 {
		e.acctSyms[accountID] = syms
	} else {
		delete(e.acctSyms, accountID)
	}
	e.legs[accountID] = legs
}

// RefreshIndex rebuilds the position-symbol → accounts fan-out from the
// store and evicts state for accounts with no remaining open positions.
func (e *MarginEngine) RefreshIndex(ctx context.Context) error {
	idx, err := e.store.OpenPositionIndex(ctx)
	if err != nil {
		return errCode(CodeRiskLimitsInternal, "open position index", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	next := make(map[string]map[int64]struct{}, len(idx))
	acctSyms := map[int64]map[string]struct{}{}
	for sym, accts := range idx {
		s := make(map[int64]struct{}, len(accts))
		for _, a := range accts {
			s[a] = struct{}{}
			m, ok := acctSyms[a]
			if !ok {
				m = map[string]struct{}{}
				acctSyms[a] = m
			}
			m[sym] = struct{}{}
		}
		next[sym] = s
	}
	e.index = next
	e.acctSyms = acctSyms
	for acct, old := range e.legs {
		if _, alive := acctSyms[acct]; alive {
			continue
		}
		// No open positions remain — evict everywhere.
		delete(e.legs, acct)
		e.heap.Remove(acct)
		delete(e.breached, acct)
		delete(e.lastStat, acct)
		for c := range old.ccys {
			if s, ok := e.ccyWatch[c]; ok {
				delete(s, acct)
				if len(s) == 0 {
					delete(e.ccyWatch, c)
				}
			}
		}
	}
	return nil
}

// WarmAccount forces a leg load + evaluation — tests, ops tooling and
// the order pipeline's post-fill hook use it to bring a new account
// onto the fast path immediately.
func (e *MarginEngine) WarmAccount(ctx context.Context, accountID int64) error {
	if _, err := e.loadLegs(ctx, accountID); err != nil {
		return err
	}
	e.evaluateAccount(ctx, accountID, nil)
	return nil
}

// ForgetAccount evicts all cached state (position fully closed / mode
// switch) — the next tick re-enters via the refresh path only if the
// account still holds indexed positions.
func (e *MarginEngine) ForgetAccount(accountID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if old, ok := e.legs[accountID]; ok {
		for c := range old.ccys {
			if s, ok2 := e.ccyWatch[c]; ok2 {
				delete(s, accountID)
				if len(s) == 0 {
					delete(e.ccyWatch, c)
				}
			}
		}
	}
	if syms, ok := e.acctSyms[accountID]; ok {
		for sym := range syms {
			if s, ok2 := e.index[sym]; ok2 {
				delete(s, accountID)
				if len(s) == 0 {
					delete(e.index, sym)
				}
			}
		}
	}
	delete(e.acctSyms, accountID)
	delete(e.legs, accountID)
	delete(e.breached, accountID)
	delete(e.lastStat, accountID)
	e.heap.Remove(accountID)
}

// queueRefreshLocked enqueues a deduped leg-refresh request (caller
// holds e.mu). A full channel drops the request — the account stays
// stale-flagged and the next tick re-requests.
func (e *MarginEngine) queueRefreshLocked(accountID int64) {
	if _, ok := e.refreshSet[accountID]; ok {
		return
	}
	select {
	case e.refreshCh <- accountID:
		e.refreshSet[accountID] = struct{}{}
	default:
	}
}

// refreshWorker drains leg-refresh requests: load → install → evaluate
// through the identical fast path.
func (e *MarginEngine) refreshWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case acct := <-e.refreshCh:
			e.mu.Lock()
			delete(e.refreshSet, acct)
			e.mu.Unlock()
			if _, err := e.loadLegs(ctx, acct); err != nil {
				e.logf("margin engine: leg load acct %d: %v", acct, err)
				e.noteEvalErr(acct, err)
				continue
			}
			e.evaluateAccount(ctx, acct, nil)
		}
	}
}

// ---------------------------------------------------------------------------
// Shared helpers / observability
// ---------------------------------------------------------------------------

// TrackedSymbols returns every symbol the engine currently values:
// open-position symbols plus the conversion pairs accounts depend on.
// VolatilityScaler.Refresh and friends iterate this set.
func (e *MarginEngine) TrackedSymbols() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	set := map[string]bool{}
	for sym := range e.index {
		set[sym] = true
	}
	for sym := range e.pairToCcy {
		set[sym] = true
	}
	return sortedKeys(set)
}

// RateToUSD resolves one currency's USD conversion from the engine's
// pair map + tick marks — the seam the isolated-margin service consumes
// for boundary maintenance. Exported for standalone wiring; internally
// the eval path uses the legs-scoped rateToUSD inside Evaluate.
func (e *MarginEngine) RateToUSD(_ context.Context, ccy string) (decimal.Decimal, bool) {
	if ccy == "USD" {
		return decimal.NewFromInt(1), true
	}
	e.mu.Lock()
	fp, ok := e.pairs[ccy]
	e.mu.Unlock()
	if !ok || fp.Symbol == "" {
		return decimal.Zero, false
	}
	e.markMu.RLock()
	m, ok := e.marks[fp.Symbol]
	e.markMu.RUnlock()
	if !ok || !m.IsPositive() {
		return decimal.Zero, false
	}
	if fp.Inverted {
		return decimal.NewFromInt(1).Div(m), true
	}
	return m, true
}

// HeapEntry reports the account's queued level (tests / ops probes).
func (e *MarginEngine) HeapEntry(accountID int64) (MarginQueueEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.heap.Get(accountID)
}

// HeapOrdered reports the priority queue worst-first (tests / ops).
func (e *MarginEngine) HeapOrdered() []MarginQueueEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.heap.Ordered()
}

// MarkFor reports the engine's current mark (tests).
func (e *MarginEngine) MarkFor(symbol string) (decimal.Decimal, bool) {
	e.markMu.RLock()
	defer e.markMu.RUnlock()
	v, ok := e.marks[symbol]
	return v, ok
}

// LegsLoaded reports whether the account has cached legs (tests).
func (e *MarginEngine) LegsLoaded(accountID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.legs[accountID] != nil
}

func (e *MarginEngine) noteEvalErr(accountID int64, err error) {
	e.mu.Lock()
	e.evalErrs++
	n := e.evalErrs
	e.mu.Unlock()
	e.logf("margin engine: evaluate acct %d: %v", accountID, err)
	if n >= 3 {
		e.raiseAlert(context.Background(), "P1", "MARGIN_ENGINE_EVAL_DEGRADED",
			fmt.Sprintf("margin engine evaluations failing (%d consecutive); last acct %d",
				n, accountID), map[string]string{"account_id": fmt.Sprint(accountID)})
	}
}

func (e *MarginEngine) resetEvalErrs() {
	e.mu.Lock()
	e.evalErrs = 0
	e.mu.Unlock()
}

func (e *MarginEngine) noteSinkErr(accountID int64, err error) {
	e.mu.Lock()
	e.sinkErrs++
	n := e.sinkErrs
	e.mu.Unlock()
	e.logf("margin engine: snapshot publish acct %d: %v", accountID, err)
	if n >= 3 {
		e.raiseAlert(context.Background(), "P1", "MARGIN_SNAPSHOT_PUBLISH_DEGRADED",
			fmt.Sprintf("margin:level publication failing (%d consecutive)", n), nil)
	}
}

func (e *MarginEngine) raiseAlert(ctx context.Context, severity, code, summary string,
	details map[string]string) {
	if e.alerter == nil {
		e.logf("margin engine: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := e.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	}); err != nil {
		e.logf("margin engine: alert %s dispatch failed: %v", code, err)
	}
}

func pctStr(p *decimal.Decimal) string {
	if p == nil {
		return "∞"
	}
	return p.String()
}

// ---------------------------------------------------------------------------
// RedisMarginSnapshotSink — production margin:level publisher
// ---------------------------------------------------------------------------

// RedisMarginSnapshotSink writes the canonical margin:level:{account}
// HASH on every evaluation and persists to margin_accounts via
// MarginStore.WriteMarginSnapshot on status transitions only — the
// contract both comments declare (keys.go cluster-B ownership +
// margin.go's "status transitions only" persistence rule).
type RedisMarginSnapshotSink struct {
	RDB   *excredis.Client
	Store MarginStore // optional — nil skips the PG transition write
}

// NewRedisMarginSnapshotSink binds the sink; nil redis is rejected.
func NewRedisMarginSnapshotSink(rdb *excredis.Client, store MarginStore) (*RedisMarginSnapshotSink, error) {
	if rdb == nil {
		return nil, fmt.Errorf("margin snapshot sink: nil redis")
	}
	return &RedisMarginSnapshotSink{RDB: rdb, Store: store}, nil
}

// Publish implements MarginSnapshotSink.
func (s *RedisMarginSnapshotSink) Publish(ctx context.Context, snap *MarginSnapshot,
	statusChanged bool) error {

	fields := map[string]any{
		"equity":      snap.Equity.String(),
		"used_margin": snap.UsedMargin.String(),
		"status":      snap.Status,
		"updated_at":  snap.Ts.UTC().Format(time.RFC3339Nano),
	}
	if snap.LevelPct != nil {
		fields["margin_level_pct"] = snap.LevelPct.String()
	}
	if err := s.RDB.HSet(ctx, MarginLevelKey(snap.AccountID), fields).Err(); err != nil {
		return fmt.Errorf("margin:level write acct %d: %w", snap.AccountID, err)
	}
	if statusChanged && s.Store != nil {
		if err := s.Store.WriteMarginSnapshot(ctx, *snap); err != nil {
			return errCode(CodeRiskLimitsInternal, "margin snapshot persist", err)
		}
	}
	return nil
}

// EvaluateOverlay is the engine-facing exported entry to margin.go's
// overlay evaluation — declared here because Task 19.3.26's write scope
// forbids editing margin.go. The overlay supersedes stale cache marks
// with the tick that triggered this evaluation.
func (s *MarginService) EvaluateOverlay(ctx context.Context, accountID int64,
	overlay map[string]decimal.Decimal) (*MarginSnapshot, error) {
	return s.evaluate(ctx, accountID, overlay)
}
