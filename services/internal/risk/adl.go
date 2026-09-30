// adl.go — Auto-Deleveraging (Phase-19 Task 19.3.19 indicator + the ADL
// execution leg of Tasks 19.3.3/19.3.20; spec §13.6, §13.11 item 2,
// §24 #94/#97/#269; migration 230 adl_directives).
//
// Three pieces, one file:
//
//	ADLScorer — pure ranking: score = unrealized_profit_pct ×
//	  effective_leverage per open CROSS/PORTFOLIO position. Positions
//	  with score ≤ 0 (zero/negative unrealized PnL) are excluded from
//	  the ranking and always show indicator level 1. The profitable set
//	  is bucketed into 5 equal rank bands: top 20% → level 5, next → 4,
//	  next → 3, next → 2, bottom 20% → also 2 (level 1 is reserved for
//	  non-profitable positions — "a loss-maker is always 1" and
//	  "profitable positions compete for levels 2–5" are both preserved).
//	  unrealized_profit_pct is the price-relative return:
//	  signedQty×(mark−entry) / (|qty|×entry) — multiplying by leverage
//	  yields the canonical ADL score without double-counting leverage
//	  (margin-based ROE would carry leverage twice).
//
//	ADLIndicatorPublisher — one TickOnce recomputes the whole universe
//	  (positions.unrealized economics refreshed from the Redis mark
//	  cache → stored mark → entry chain) and republishes, atomically in
//	  a single MULTI:
//
//	    adl:indicator:{account_id}   HASH  position_id → quintile ("1".."5")
//	                                     — the exact field encoding
//	                                     RedisADLIndicatorReader decodes
//	                                     for REST/WS private:positions.
//	    adl:priority:{symbol}:{side} ZSET  member account_id → max score
//	                                     (counterparty candidate rank cache;
//	                                     keys.go contract).
//
//	  Keys whose positions closed or dropped out of CROSS/PORTFOLIO
//	  mode are deleted in the same MULTI (DEL+rewrite per key — the
//	  publisher owns the whole adl:* keyspace). Any scoring failure
//	  (leverage resolution, mark batch read) aborts the tick before a
//	  single write: a quintile set computed over a partial universe is
//	  objectively wrong, and publishing it would mis-rank every account
//	  (§2.7 fail-closed).
//
//	ADLEngine — the §13.11 item-2 execution leg. TriggerADL is invoked
//	  when deficiency absorption is depleted (InsuranceFundService.
//	  Depleted is the exported seam — "balance < max(-$100K, 1% × fund
//	  target)"): it re-ranks the live universe through the same scorer,
//	  picks the highest-scored profitable counterparties holding the
//	  OPPOSING side on the liquidated instrument (a liquidated LONG is
//	  covered by deleveraging profitable SHORTs — their buy-back is the
//	  matching liquidity), emits one adl_directives row per counterparty
//	  (QUEUED → DISPATCHED), and produces the close intent through the
//	  existing accounts.OrderDispatcher.SubmitClose seam (reduce-only,
//	  LimitPrice = the liquidated position's bankruptcy price — the
//	  same CloseOrderRequest shape the liquidation engine uses; the
//	  dispatcher is reused, not edited). ReconcileADLFill consumes
//	  ADL_FILL reports: directive → FILLED plus liquidation_events rows
//	  (kind='ADL', adl_quintile) in ONE tx — idempotent replay-safe.
//
//	  liquidation_events has no counterparty_account_id column
//	  (migration 230): the counterparty linkage lives on the directive
//	  (adl_directives.account_id is the deleveraged counterparty) and
//	  the directive's liquidation_event_id backlink. When the fill
//	  report carries the liquidated-position context a second event row
//	  is written for the liquidated account so both sides see their own
//	  force-order record in GET /api/v1/account/liquidations.
//
//	  Fail-closed (§2.7): zero eligible counterparties or an uncovered
//	  shortfall pages ADL_NO_COUNTERPARTY / ADL_SHORTFALL (L1) and
//	  returns a coded error — a depleted-fund liquidation is never
//	  silently dropped.
package risk

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/accounts"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Internal L1 alert/event codes (§23 internal-only list — siblings of
// CodeADLTriggered in insurance_fund.go).
const (
	// CodeADLNoCounterparty fires when ADL executes but no profitable
	// counterparty exists on the opposing side — the deficit stays
	// uncovered and ops must see it immediately.
	CodeADLNoCounterparty = "ADL_NO_COUNTERPARTY"
	// CodeADLShortfall fires when counterparties covered only part of
	// the deficit quantity.
	CodeADLShortfall = "ADL_SHORTFALL"
	// CodeADLFillReconcileError fires when an ADL_FILL report cannot be
	// reconciled against its directive (unknown seq, over-fill, or a
	// vanished counterparty position row).
	CodeADLFillReconcileError = "ADL_FILL_RECONCILE_ERROR"
)

// AdlScanCadence is the §13.5/§24 #269 recompute cadence — the parent
// wiring binds TickOnce to the 2s liquidation-scanner tick (trades,
// liquidations and mark updates all flow through that cycle).
const AdlScanCadence = 2 * time.Second

// adlDirectiveStatus vocabulary — mirrors the migration-230 CHECK.
const (
	ADLStatusQueued     = "QUEUED"
	ADLStatusDispatched = "DISPATCHED"
	ADLStatusFilled     = "FILLED"
	ADLStatusFailed     = "FAILED"
)

// ---------------------------------------------------------------------------
// ADLScorer — score = unrealized_profit_pct × effective_leverage
// ---------------------------------------------------------------------------

// ADLScore is one position's ranking record. Quintile is 1–5 (1 lowest
// deleveraging priority). Positions with Score ≤ 0 keep Quintile 1 and
// never enter the ranked set.
type ADLScore struct {
	AccountID    int64
	PositionID   int64
	InstrumentID int64
	Symbol       string
	Side         string // LONG | SHORT — the POSITION side
	Quantity     decimal.Decimal
	Mark         decimal.Decimal
	ProfitPct    decimal.Decimal // price-relative unrealized profit
	Score        decimal.Decimal // ProfitPct × EffectiveLeverage
	Leverage     int
	Quintile     int // 1..5
}

// ADLScorer computes per-position ADL scores and quintiles. Marks ride
// the MarkCache batch seam (one MGET per tick — §13.1 zero-N+1); a nil
// cache degrades to the positions.mark_price mirror and finally entry.
// Effective leverage resolves through the canonical LeverageResolver —
// failures propagate (fail closed): an unresolvable leverage means the
// score is unknowable, and guessing it mis-ranks the deleveraging order.
type ADLScorer struct {
	marks MarkCache
	lev   LeverageResolver
}

// NewADLScorer wires the scorer. lev is mandatory; marks may be nil.
func NewADLScorer(marks MarkCache, lev LeverageResolver) (*ADLScorer, error) {
	if lev == nil {
		return nil, fmt.Errorf("adl scorer: nil leverage resolver")
	}
	return &ADLScorer{marks: marks, lev: lev}, nil
}

// ScoreUniverse scores every position in positions (the caller supplies
// the CROSS/PORTFOLIO open-position universe; non-CROSS/PORTFOLIO rows
// are re-filtered defensively) and assigns quintiles across the ranked
// set. The result is unsorted — callers needing rank order sort by
// Score desc (see ADLScoreLess).
func (s *ADLScorer) ScoreUniverse(ctx context.Context, positions []LiqPosition) ([]ADLScore, error) {
	syms := map[string]bool{}
	for _, p := range positions {
		if p.MarginMode == string(ModeCross) || p.MarginMode == string(ModePortfolio) {
			syms[p.Symbol] = true
		}
	}
	var live map[string]decimal.Decimal
	if s.marks != nil && len(syms) > 0 {
		m, err := s.marks.BatchMarks(ctx, sortedKeys(syms))
		if err != nil {
			return nil, excerrors.Wrap(CodeOracleUnavailable, "adl: mark batch", err)
		}
		live = m
	}

	scored := make([]ADLScore, 0, len(positions))
	levCache := map[[2]int64]int{}
	for _, p := range positions {
		if p.MarginMode != string(ModeCross) && p.MarginMode != string(ModePortfolio) {
			continue
		}
		// Mark precedence mirrors margin.go: live Redis mark → stored
		// positions.mark_price (already COALESCE'd to entry by the
		// store) — the result is always a positive number.
		mark := p.MarkPrice
		if s.marks != nil {
			if m, ok := live[p.Symbol]; ok && m.IsPositive() {
				mark = m
			}
		}
		if !mark.IsPositive() {
			mark = p.EntryPrice
		}
		// signedQty: the side flag is authoritative (stored sign
		// conventions differ across writers — ABS + flag is correct
		// under both).
		signed := p.Quantity.Abs()
		if p.Side == "SHORT" {
			signed = signed.Neg()
		}
		upnl := signed.Mul(mark.Sub(p.EntryPrice))
		var profitPct decimal.Decimal
		base := p.Quantity.Abs().Mul(p.EntryPrice)
		if base.IsPositive() {
			profitPct = upnl.Div(base)
		}

		var score decimal.Decimal
		var lev int
		if profitPct.IsPositive() {
			key := [2]int64{p.AccountID, p.InstrumentID}
			var ok bool
			lev, ok = levCache[key]
			if !ok {
				var err error
				lev, err = s.lev.Effective(ctx, p.AccountID, p.InstrumentID)
				if err != nil {
					return nil, excerrors.Wrap(CodeRiskLimitsInternal,
						fmt.Sprintf("adl: effective leverage acct %d instr %d",
							p.AccountID, p.InstrumentID), err)
				}
				levCache[key] = lev
			}
			score = profitPct.Mul(decimal.NewFromInt(int64(lev)))
		}
		scored = append(scored, ADLScore{
			AccountID:    p.AccountID,
			PositionID:   p.ID,
			InstrumentID: p.InstrumentID,
			Symbol:       p.Symbol,
			Side:         p.Side,
			Quantity:     p.Quantity.Abs(),
			Mark:         mark,
			ProfitPct:    profitPct,
			Score:        score,
			Leverage:     lev,
			Quintile:     1,
		})
	}
	assignADLQuintiles(scored)
	return scored, nil
}

// ADLScoreLess orders the ranked set: score desc, position id asc (the
// deterministic tie-break every consumer shares).
func ADLScoreLess(a, b ADLScore) bool {
	if a.Score.Equal(b.Score) {
		return a.PositionID < b.PositionID
	}
	return a.Score.GreaterThan(b.Score)
}

// assignADLQuintiles buckets the profitable positions (Score > 0):
// sorted best-first, position i of n takes
//
//	level = 5 − ⌊5i/n⌋, clamped to ≥ 2.
//
// i < n/5 → level 5 ("top 20% = 5"), i < 2n/5 → 4, i < 3n/5 → 3,
// i < 4n/5 → 2; the bottom band would be 1 but clamps to 2 — level 1 is
// reserved for non-profitable positions ("a loss-maker is always 1").
// Non-profitable positions stay at the initialized 1.
func assignADLQuintiles(scored []ADLScore) {
	idx := make([]int, 0, len(scored))
	for i, s := range scored {
		if s.Score.IsPositive() {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool {
		return ADLScoreLess(scored[idx[a]], scored[idx[b]])
	})
	n := int64(len(idx))
	for rank, i := range idx {
		level := 5 - int64(rank)*5/n
		if level < 2 {
			level = 2
		}
		scored[i].Quintile = int(level)
	}
}

// ---------------------------------------------------------------------------
// ADLIndicatorPublisher — periodic universe recompute + atomic republish
// ---------------------------------------------------------------------------

// ADLUniverseReader supplies the open-position universe for scoring —
// every quantity <> 0 position whose resolved margin mode is CROSS or
// PORTFOLIO (the resolved-mode expression includes the §13.1 category
// default for accounts with no margin_accounts row, so unmaterialized
// rows are never silently missed). PgADLStore implements it.
type ADLUniverseReader interface {
	Universe(ctx context.Context) ([]LiqPosition, error)
}

// ADLTickStats summarizes one publisher pass (observability/tests).
type ADLTickStats struct {
	Positions    int // scored positions in the universe
	Ranked       int // profitable positions (Quintile ≥ 2)
	Accounts     int // indicator hashes written
	PriorityKeys int // adl:priority ZSETs written
	KeysRemoved  int // stale adl:* keys deleted
}

// ADLIndicatorPublisher recomputes and republishes adl:* keys. Bind
// TickOnce to the 2s liquidation cadence (AdlScanCadence) — the same
// cycle trade/liquidation/mark updates flow through per Task 19.3.19.
type ADLIndicatorPublisher struct {
	rdb      *excredis.Client
	universe ADLUniverseReader
	scorer   *ADLScorer
	now      func() time.Time
	logf     func(format string, args ...any)
}

// ADLPublisherDeps wires the publisher; every field except Now/Logf is
// mandatory — a publisher that cannot read or write is a silent
// indicator outage (fail closed).
type ADLPublisherDeps struct {
	Redis    *excredis.Client
	Universe ADLUniverseReader
	Scorer   *ADLScorer
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

// NewADLIndicatorPublisher validates the deps.
func NewADLIndicatorPublisher(d ADLPublisherDeps) (*ADLIndicatorPublisher, error) {
	if d.Redis == nil {
		return nil, fmt.Errorf("adl publisher: nil redis")
	}
	if d.Universe == nil {
		return nil, fmt.Errorf("adl publisher: nil universe reader")
	}
	if d.Scorer == nil {
		return nil, fmt.Errorf("adl publisher: nil scorer")
	}
	p := &ADLIndicatorPublisher{
		rdb: d.Redis, universe: d.Universe, scorer: d.Scorer,
		now: d.Now, logf: d.Logf,
	}
	if p.now == nil {
		p.now = func() time.Time { return time.Now().UTC() }
	}
	if p.logf == nil {
		p.logf = func(string, ...any) {}
	}
	return p, nil
}

// adlWritePlan is the pure projection of one scored universe onto the
// keyspace — factored out so the hash/ZSET shape is unit-testable
// without Redis. indicator maps account → position_id → quintile;
// priority maps the ZSET key → member account_id → max score.
type adlWritePlan struct {
	indicator map[int64]map[int64]int
	priority  map[string]map[int64]float64
}

// planADLWrites computes the target state. Quintile-1 (non-profitable)
// positions DO publish their indicator field (the 1-light badge is
// informative to the client) but never enter adl:priority — only
// profitable positions are deleverage candidates.
func planADLWrites(scored []ADLScore) adlWritePlan {
	plan := adlWritePlan{
		indicator: map[int64]map[int64]int{},
		priority:  map[string]map[int64]float64{},
	}
	for _, s := range scored {
		acct := plan.indicator[s.AccountID]
		if acct == nil {
			acct = map[int64]int{}
			plan.indicator[s.AccountID] = acct
		}
		acct[s.PositionID] = s.Quintile
		if !s.Score.IsPositive() {
			continue
		}
		key := AdlPriorityKey(s.Symbol, s.Side)
		members := plan.priority[key]
		if members == nil {
			members = map[int64]float64{}
			plan.priority[key] = members
		}
		// One member per account per (symbol,side): the MAX score wins —
		// the account's most deleverageable leg drives its rank.
		f, _ := s.Score.Float64()
		if cur, ok := members[s.AccountID]; !ok || f > cur {
			members[s.AccountID] = f
		}
	}
	return plan
}

// TickOnce recomputes the universe and republishes atomically. A scoring
// error aborts BEFORE the first write — the previous (complete) image
// stays live rather than a partial mis-ranking.
func (p *ADLIndicatorPublisher) TickOnce(ctx context.Context) (ADLTickStats, error) {
	var stats ADLTickStats
	positions, err := p.universe.Universe(ctx)
	if err != nil {
		return stats, excerrors.Wrap(CodeRiskLimitsInternal, "adl: universe read", err)
	}
	scored, err := p.scorer.ScoreUniverse(ctx, positions)
	if err != nil {
		return stats, err
	}
	plan := planADLWrites(scored)

	// Discover stale keys (accounts flat now, symbol:side pairs with no
	// remaining profitable members).
	stale := []string{}
	for _, pattern := range []string{"adl:indicator:*", "adl:priority:*"} {
		iter := p.rdb.Scan(ctx, 0, pattern, 200).Iterator()
		for iter.Next(ctx) {
			stale = append(stale, iter.Val())
		}
		if err := iter.Err(); err != nil {
			return stats, excerrors.Wrap(CodeRiskLimitsInternal,
				"adl: key scan "+pattern, err)
		}
	}
	live := map[string]bool{}
	for acct := range plan.indicator {
		live[AdlIndicatorKey(acct)] = true
	}
	for key := range plan.priority {
		live[key] = true
	}

	pipe := p.rdb.TxPipeline()
	for _, key := range stale {
		if !live[key] {
			pipe.Del(ctx, key)
			stats.KeysRemoved++
		}
	}
	for acct, fields := range plan.indicator {
		key := AdlIndicatorKey(acct)
		fv := make(map[string]any, len(fields))
		for posID, q := range fields {
			fv[strconv.FormatInt(posID, 10)] = strconv.Itoa(q)
		}
		pipe.Del(ctx, key)
		pipe.HSet(ctx, key, fv)
		stats.Accounts++
	}
	for key, members := range plan.priority {
		zs := make([]goredis.Z, 0, len(members))
		for acct, score := range members {
			zs = append(zs, goredis.Z{Score: score, Member: acct})
		}
		pipe.Del(ctx, key)
		pipe.ZAdd(ctx, key, zs...)
		stats.PriorityKeys++
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return stats, excerrors.Wrap(CodeRiskLimitsInternal, "adl: indicator publish", err)
	}
	stats.Positions = len(scored)
	for _, s := range scored {
		if s.Score.IsPositive() {
			stats.Ranked++
		}
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// ADL engine — the §13.11 execution leg
// ---------------------------------------------------------------------------

// FundDepletionChecker is the insurance-fund depletion seam —
// *InsuranceFundService satisfies it (Depleted: balance < max(-$100K,
// 1% × fund target) per spec §13.6).
type FundDepletionChecker interface {
	Depleted(ctx context.Context, ccy string) (bool, decimal.Decimal, error)
}

// ADLDirective is one adl_directives row — a force-close order for one
// counterparty's opposing position at the liquidated position's
// bankruptcy price.
type ADLDirective struct {
	Seq             int64
	AccountID       int64 // deleveraged counterparty
	InstrumentID    int64
	Symbol          string
	Side            string // counterparty position side (opposing leg)
	Qty             decimal.Decimal
	BankruptcyPrice decimal.Decimal
	Score           decimal.Decimal
	Quintile        int
	Status          string
}

// ADLTriggerRequest is one depletion-driven execution request: the
// liquidated position's identity, the uncovered quantity and the
// bankruptcy price counterparties fill at.
type ADLTriggerRequest struct {
	LiquidatedAccountID  int64
	LiquidatedPositionID int64
	InstrumentID         int64
	Symbol               string
	LiquidatedSide       string          // LONG|SHORT — side of the liquidated position
	DeficitQty           decimal.Decimal // uncovered qty to deleverage
	BankruptcyPrice      decimal.Decimal // counterparty fill price
	Currency             string          // fund currency for the depletion gate (default USD)
	MarkPrice            decimal.Decimal // current mark (event rows)
}

// ADLResult reports one TriggerADL pass.
type ADLResult struct {
	Triggered      bool // false ⇒ fund not depleted, no work performed
	FundBalance    decimal.Decimal
	Currency       string
	Counterparties int
	Dispatched     int
	Failed         int
	CoveredQty     decimal.Decimal
	ShortfallQty   decimal.Decimal // >0 ⇒ ADL_SHORTFALL/ADL_NO_COUNTERPARTY paged
	Directives     []ADLDirective
}

// ADLFillReport is one ADL_FILL reconciliation message (the outbox
// consumer / order-ack path emits it). The liquidated-side context is
// carried by the report — adl_directives intentionally has no column
// for it (the counterparty row IS the directive's account_id).
type ADLFillReport struct {
	AdlSeq               int64
	FilledQty            decimal.Decimal
	FillPrice            decimal.Decimal
	MarkPrice            decimal.Decimal
	LiquidatedAccountID  int64 // 0 ⇒ liquidated-side event skipped
	LiquidatedPositionID int64
	LiquidatedSide       string // LONG|SHORT — required when AccountID > 0
}

// ADLReconcileResult reports one CompleteADLFill outcome.
type ADLReconcileResult struct {
	AlreadyReconciled   bool // replay of a FILLED directive — deduped
	CounterpartyEventID int64
	LiquidatedEventID   *int64
	Directive           ADLDirective
}

// ADLEngineStore is the PG seam the engine consumes; PgADLStore
// implements it.
type ADLEngineStore interface {
	ADLUniverseReader
	// InsertADLDirective persists a QUEUED directive, returning adl_seq.
	InsertADLDirective(ctx context.Context, d ADLDirective) (int64, error)
	// MarkADLDirectiveDispatched flips QUEUED → DISPATCHED; idempotent.
	MarkADLDirectiveDispatched(ctx context.Context, seq int64, at time.Time) error
	// MarkADLDirectiveFailed flips QUEUED|DISPATCHED → FAILED.
	MarkADLDirectiveFailed(ctx context.Context, seq int64) error
	// CompleteADLFill reconciles an ADL_FILL report: directive → FILLED
	// plus the liquidation_events rows, atomically and replay-safe.
	CompleteADLFill(ctx context.Context, r ADLFillReport) (*ADLReconcileResult, error)
}

// ADLEngine runs the §13.11 ADL execution leg: fund depletion →
// top-ranked profitable counterparties on the opposing side →
// reduce-only closes at the bankruptcy price → durable outbox rows →
// fill reconciliation.
type ADLEngine struct {
	store         ADLEngineStore
	scorer        *ADLScorer
	fund          FundDepletionChecker
	dispatch      accounts.OrderDispatcher
	alerter       OpsAlerter
	maxDirectives int
	now           func() time.Time
	logf          func(format string, args ...any)
}

// ADLEngineDeps wires the engine; Store/Scorer/Fund/Dispatch are
// mandatory (a depleted fund with no dispatcher must fail construction,
// never discover it mid-liquidation).
type ADLEngineDeps struct {
	Store    ADLEngineStore
	Scorer   *ADLScorer
	Fund     FundDepletionChecker
	Dispatch accounts.OrderDispatcher
	Alerter  OpsAlerter
	// MaxDirectivesPerTrigger bounds directives emitted in one trigger
	// (default 50) — a runaway deleveraging sweep must stay bounded.
	MaxDirectivesPerTrigger int
	Now                     func() time.Time
	Logf                    func(format string, args ...any)
}

// NewADLEngine validates the deps.
func NewADLEngine(d ADLEngineDeps) (*ADLEngine, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("adl engine: nil store")
	}
	if d.Scorer == nil {
		return nil, fmt.Errorf("adl engine: nil scorer")
	}
	if d.Fund == nil {
		return nil, fmt.Errorf("adl engine: nil fund depletion checker")
	}
	if d.Dispatch == nil {
		return nil, fmt.Errorf("adl engine: nil order dispatcher")
	}
	e := &ADLEngine{
		store: d.Store, scorer: d.Scorer, fund: d.Fund,
		dispatch: d.Dispatch, alerter: d.Alerter,
		maxDirectives: d.MaxDirectivesPerTrigger,
		now:           d.Now, logf: d.Logf,
	}
	if e.maxDirectives <= 0 {
		e.maxDirectives = 50
	}
	if e.now == nil {
		e.now = func() time.Time { return time.Now().UTC() }
	}
	if e.logf == nil {
		e.logf = func(string, ...any) {}
	}
	return e, nil
}

// adlCandidate is one counterparty's aggregated opposing exposure.
type adlCandidate struct {
	accountID int64
	qty       decimal.Decimal // total opposing qty the account holds
	score     decimal.Decimal // best (max) leg score
	quintile  int             // quintile of that best leg
}

// TriggerADL executes the §13.11 fallback: when the insurance fund is
// below its §13.6 depletion threshold, profitable counterparties on the
// opposing side are deleveraged best-score-first until DeficitQty is
// covered (or candidates/maxDirectives exhaust). Returns Triggered=false
// (no work, no error) when the fund is still above the floor — the
// depletion check is the gate even when the caller already suspects
// depletion (defense in depth).
//
// Fail-closed: zero candidates or an uncovered remainder pages an L1
// alert AND returns a coded error; a dispatch failure flips that
// directive to FAILED and the sweep continues with the next candidate.
func (e *ADLEngine) TriggerADL(ctx context.Context, req ADLTriggerRequest) (*ADLResult, error) {
	if req.InstrumentID <= 0 || req.Symbol == "" ||
		(req.LiquidatedSide != "LONG" && req.LiquidatedSide != "SHORT") ||
		!req.DeficitQty.IsPositive() || !req.BankruptcyPrice.IsPositive() {
		return nil, excerrors.New(CodeInvalidRequest,
			"adl trigger: instrument/side/deficit qty/bankruptcy price required")
	}
	ccy := req.Currency
	if ccy == "" {
		ccy = "USD"
	}
	res := &ADLResult{Currency: ccy}
	depleted, bal, err := e.fund.Depleted(ctx, ccy)
	if err != nil {
		return nil, excerrors.Wrap(CodeRiskLimitsInternal, "adl: fund depletion check", err)
	}
	res.FundBalance = bal
	if !depleted {
		e.logf("adl: fund %s balance %s above depletion floor — trigger skipped", ccy, bal)
		return res, nil
	}
	res.Triggered = true

	positions, err := e.store.Universe(ctx)
	if err != nil {
		return res, excerrors.Wrap(CodeRiskLimitsInternal, "adl: universe read", err)
	}
	scored, err := e.scorer.ScoreUniverse(ctx, positions)
	if err != nil {
		return res, err
	}

	opposing := "SHORT"
	if req.LiquidatedSide == "SHORT" {
		opposing = "LONG"
	}
	// Aggregate candidate exposure per counterparty account: total
	// opposing qty, ranked by the account's best leg score/quintile.
	cands := map[int64]*adlCandidate{}
	for _, s := range scored {
		if s.InstrumentID != req.InstrumentID || s.Side != opposing ||
			!s.Score.IsPositive() || s.AccountID == req.LiquidatedAccountID {
			continue
		}
		c := cands[s.AccountID]
		if c == nil {
			c = &adlCandidate{accountID: s.AccountID, score: s.Score, quintile: s.Quintile}
			cands[s.AccountID] = c
		}
		c.qty = c.qty.Add(s.Quantity)
		if s.Score.GreaterThan(c.score) {
			c.score, c.quintile = s.Score, s.Quintile
		}
	}
	ranked := make([]*adlCandidate, 0, len(cands))
	for _, c := range cands {
		ranked = append(ranked, c)
	}
	sort.Slice(ranked, func(i, j int) bool { // best score first, id tiebreak
		if ranked[i].score.Equal(ranked[j].score) {
			return ranked[i].accountID < ranked[j].accountID
		}
		return ranked[i].score.GreaterThan(ranked[j].score)
	})

	remaining := req.DeficitQty
	for _, c := range ranked {
		if !remaining.IsPositive() || len(res.Directives) >= e.maxDirectives {
			break
		}
		qty := c.qty
		if qty.GreaterThan(remaining) {
			qty = remaining
		}
		d := ADLDirective{
			AccountID: c.accountID, InstrumentID: req.InstrumentID,
			Symbol: req.Symbol, Side: opposing, Qty: qty,
			BankruptcyPrice: req.BankruptcyPrice, Score: c.score,
			Quintile: c.quintile, Status: ADLStatusQueued,
		}
		seq, err := e.store.InsertADLDirective(ctx, d)
		if err != nil {
			// The durable outbox failed — stop; emitted directives stay
			// valid but the sweep cannot continue without persistence.
			return res, excerrors.Wrap(CodeRiskLimitsInternal,
				"adl: directive insert", err)
		}
		d.Seq = seq
		res.Directives = append(res.Directives, d)
		res.Counterparties++

		// Close intent through the existing dispatcher seam — a
		// reduce-only close bounded at the bankruptcy price
		// (CloseOrderRequest.LimitPrice is the explicit cap/floor the
		// liquidation engine uses for §13.4 legs).
		closeSide := accounts.SideBuy
		if opposing == "LONG" {
			closeSide = accounts.SideSell
		}
		ack, err := e.dispatch.SubmitClose(ctx, accounts.CloseOrderRequest{
			AccountID:     c.accountID,
			InstrumentID:  req.InstrumentID,
			Side:          closeSide,
			Quantity:      qty,
			ReduceOnly:    true,
			LimitPrice:    req.BankruptcyPrice,
			ClientOrderID: fmt.Sprintf("adl-%d", seq),
		})
		if err != nil || ack == nil || !ack.Accepted {
			detail := ackDetail(ack)
			if err != nil {
				detail = err.Error()
			}
			e.logf("adl: directive %d dispatch failed acct %d: %s", seq, c.accountID, detail)
			if ferr := e.store.MarkADLDirectiveFailed(ctx, seq); ferr != nil {
				e.logf("adl: directive %d fail-mark: %v", seq, ferr)
			}
			res.Failed++
			continue // keep sweeping — the deficit still needs covering
		}
		if err := e.store.MarkADLDirectiveDispatched(ctx, seq, e.now()); err != nil {
			e.logf("adl: directive %d dispatch-mark: %v", seq, err)
		}
		res.Dispatched++
		res.CoveredQty = res.CoveredQty.Add(qty)
		remaining = remaining.Sub(qty)
	}

	res.ShortfallQty = remaining
	switch {
	case res.Counterparties == 0:
		e.raiseAlert(ctx, SeverityP1, CodeADLNoCounterparty, fmt.Sprintf(
			"ADL for %s (%s liquidation pos %d, deficit %s): no profitable counterparty on %s side — deficit uncovered",
			req.Symbol, req.LiquidatedSide, req.LiquidatedPositionID,
			req.DeficitQty, opposing), map[string]string{
			"symbol": req.Symbol, "instrument_id": fmt.Sprint(req.InstrumentID),
			"deficit_qty": req.DeficitQty.String(), "fund_balance": bal.String()})
		return res, excerrors.New(CodeADLNoCounterparty,
			fmt.Sprintf("adl: no counterparty for %s deficit %s", req.Symbol, req.DeficitQty))
	case remaining.IsPositive():
		e.raiseAlert(ctx, SeverityP1, CodeADLShortfall, fmt.Sprintf(
			"ADL for %s covered %s of %s deficit — %s shortfall after %d directives",
			req.Symbol, res.CoveredQty, req.DeficitQty, remaining, res.Dispatched),
			map[string]string{"symbol": req.Symbol, "covered": res.CoveredQty.String(),
				"shortfall": remaining.String()})
		return res, excerrors.New(CodeADLShortfall,
			fmt.Sprintf("adl: %s shortfall %s of %s", req.Symbol, remaining, req.DeficitQty))
	}
	e.raiseAlert(ctx, SeverityP1, CodeADLTriggered, fmt.Sprintf(
		"ADL executed for %s: %d counterparties deleveraged %s at bankruptcy price %s",
		req.Symbol, res.Dispatched, res.CoveredQty, req.BankruptcyPrice),
		map[string]string{"symbol": req.Symbol, "covered": res.CoveredQty.String(),
			"counterparties": fmt.Sprint(res.Counterparties)})
	return res, nil
}

// ReconcileADLFill consumes one ADL_FILL report: validates shape then
// commits the directive → FILLED transition plus the liquidation_events
// rows in one tx (CompleteADLFill). Replays of an already-FILLED
// directive dedupe through the store. An unreconcilable report pages
// ADL_FILL_RECONCILE_ERROR (L1) — a fill we cannot record is a risk
// bookkeeping failure, never a silent drop.
func (e *ADLEngine) ReconcileADLFill(ctx context.Context, r ADLFillReport) (*ADLReconcileResult, error) {
	if r.AdlSeq <= 0 || !r.FilledQty.IsPositive() || !r.FillPrice.IsPositive() {
		return nil, excerrors.New(CodeInvalidRequest,
			"adl fill: seq, positive qty and price required")
	}
	if r.LiquidatedAccountID > 0 &&
		(r.LiquidatedPositionID <= 0 ||
			(r.LiquidatedSide != "LONG" && r.LiquidatedSide != "SHORT")) {
		return nil, excerrors.New(CodeInvalidRequest,
			"adl fill: liquidated context needs position id and LONG|SHORT side")
	}
	res, err := e.store.CompleteADLFill(ctx, r)
	if err != nil {
		e.raiseAlert(ctx, SeverityP1, CodeADLFillReconcileError, fmt.Sprintf(
			"ADL_FILL for directive %d failed reconciliation: %v", r.AdlSeq, err),
			map[string]string{"adl_seq": fmt.Sprint(r.AdlSeq)})
		return nil, err
	}
	return res, nil
}

// raiseAlert pages ops on ADL-path failures (bounded fresh ctx — the
// caller's ctx is often spent post-commit).
func (e *ADLEngine) raiseAlert(_ context.Context, severity, code, summary string, details map[string]string) {
	if e.alerter == nil {
		e.logf("adl: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	}); err != nil {
		e.logf("adl: alert %s dispatch failed: %v", code, err)
	}
}

// ---------------------------------------------------------------------------
// PgADLStore — PostgreSQL implementation of the universe + outbox seams
// ---------------------------------------------------------------------------

// PgADLStore implements ADLUniverseReader + ADLEngineStore over pgx.
// Numerics cross as ::text (repo convention); a bad row fails the read
// (§2.7 — a ranking scan must never silently skip a position).
type PgADLStore struct {
	Pool *pgxpool.Pool
}

var (
	_ ADLUniverseReader = (*PgADLStore)(nil)
	_ ADLEngineStore    = (*PgADLStore)(nil)
	// *InsuranceFundService satisfies FundDepletionChecker via Depleted.
	_ FundDepletionChecker = (*InsuranceFundService)(nil)
)

// NewPgADLStore binds the pool; nil is rejected fail-closed.
func NewPgADLStore(pool *pgxpool.Pool) (*PgADLStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("adl store: nil pgx pool")
	}
	return &PgADLStore{Pool: pool}, nil
}

// Universe implements ADLUniverseReader — every open position whose
// resolved margin mode is CROSS/PORTFOLIO. The COALESCE expression
// mirrors liqPositionCols' resolved-mode leg exactly so accounts with
// no margin_accounts row rank under their §13.1 category default.
func (s *PgADLStore) Universe(ctx context.Context) ([]LiqPosition, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+liqPositionCols+liqPositionFrom+`
		WHERE p.quantity <> 0
		  AND COALESCE(ma.margin_mode::text,
		        CASE WHEN a.client_category::text IN ('PROFESSIONAL','ELIGIBLE_COUNTERPARTY')
		             THEN 'PORTFOLIO' ELSE 'CROSS' END)
		      IN ('CROSS','PORTFOLIO')
		ORDER BY p.account_id, p.id`)
	if err != nil {
		return nil, fmt.Errorf("adl universe: %w", err)
	}
	defer rows.Close()
	var out []LiqPosition
	for rows.Next() {
		p, err := scanLiqPosition(rows)
		if err != nil {
			return nil, fmt.Errorf("adl universe row: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// InsertADLDirective persists one QUEUED directive; returns adl_seq.
func (s *PgADLStore) InsertADLDirective(ctx context.Context, d ADLDirective) (int64, error) {
	if d.Quintile < 1 || d.Quintile > 5 {
		return 0, excerrors.New(CodeInvalidRequest,
			fmt.Sprintf("adl directive: quintile %d out of 1..5", d.Quintile))
	}
	var seq int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO adl_directives
		    (account_id, instrument_id, symbol, side, qty,
		     bankruptcy_price, score, quintile, status)
		VALUES ($1,$2,$3,$4::position_side_enum,$5::numeric,$6::numeric,$7::numeric,$8,$9)
		RETURNING adl_seq`,
		d.AccountID, d.InstrumentID, d.Symbol, d.Side, d.Qty.String(),
		d.BankruptcyPrice.String(), d.Score.String(), d.Quintile, ADLStatusQueued).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("adl directive insert acct %d: %w", d.AccountID, err)
	}
	return seq, nil
}

// MarkADLDirectiveDispatched flips QUEUED → DISPATCHED. Idempotent on
// an already-DISPATCHED row (dispatch retry); any other state fails
// closed — a FILLED/FAILED row must not regress.
func (s *PgADLStore) MarkADLDirectiveDispatched(ctx context.Context, seq int64, at time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE adl_directives SET status='DISPATCHED', dispatched_at=$2
		WHERE adl_seq=$1 AND status='QUEUED'`, seq, at)
	if err != nil {
		return fmt.Errorf("adl directive %d dispatch mark: %w", seq, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var status string
	err = s.Pool.QueryRow(ctx,
		`SELECT status FROM adl_directives WHERE adl_seq=$1`, seq).Scan(&status)
	if err == nil && status == ADLStatusDispatched {
		return nil
	}
	if err == pgx.ErrNoRows {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("adl directive %d not found", seq))
	}
	if err != nil {
		return fmt.Errorf("adl directive %d status read: %w", seq, err)
	}
	return fmt.Errorf("adl directive %d: cannot mark DISPATCHED from status %s", seq, status)
}

// MarkADLDirectiveFailed flips a live directive to FAILED.
func (s *PgADLStore) MarkADLDirectiveFailed(ctx context.Context, seq int64) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE adl_directives SET status='FAILED'
		WHERE adl_seq=$1 AND status IN ('QUEUED','DISPATCHED')`, seq)
	if err != nil {
		return fmt.Errorf("adl directive %d fail mark: %w", seq, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("adl directive %d: no live row to fail", seq)
	}
	return nil
}

// adlDirectiveCols is the adl_directives projection for reads.
const adlDirectiveCols = `
	adl_seq, account_id, instrument_id, symbol, side::text,
	qty::text, bankruptcy_price::text, score::text, quintile, status`

func scanADLDirective(row pgx.Row) (*ADLDirective, error) {
	var (
		d              ADLDirective
		qty, bp, score string
	)
	err := row.Scan(&d.Seq, &d.AccountID, &d.InstrumentID, &d.Symbol, &d.Side,
		&qty, &bp, &score, &d.Quintile, &d.Status)
	if err != nil {
		return nil, err
	}
	if d.Qty, err = decimal.NewFromString(qty); err != nil {
		return nil, fmt.Errorf("adl directive qty %q: %w", qty, err)
	}
	if d.BankruptcyPrice, err = decimal.NewFromString(bp); err != nil {
		return nil, fmt.Errorf("adl directive bankruptcy_price %q: %w", bp, err)
	}
	if d.Score, err = decimal.NewFromString(score); err != nil {
		return nil, fmt.Errorf("adl directive score %q: %w", score, err)
	}
	return &d, nil
}

// ADLDirectiveBySeq loads one directive (admin/debug reads).
func (s *PgADLStore) ADLDirectiveBySeq(ctx context.Context, seq int64) (*ADLDirective, error) {
	d, err := scanADLDirective(s.Pool.QueryRow(ctx,
		`SELECT `+adlDirectiveCols+` FROM adl_directives WHERE adl_seq=$1`, seq))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return d, err
}

// CompleteADLFill reconciles one ADL_FILL report atomically:
//
//	BEGIN → SELECT directive FOR UPDATE → (FILLED ⇒ replay, no-op) →
//	validate qty ≤ directive.qty → resolve the counterparty's position
//	row → INSERT liquidation_events (counterparty, kind='ADL',
//	adl_quintile) → optionally INSERT the liquidated-side event →
//	UPDATE directive FILLED + liquidation_event_id → COMMIT.
//
// A missing directive, an over-fill or a vanished counterparty position
// row are errors — the report is poison, not something to guess around.
func (s *PgADLStore) CompleteADLFill(ctx context.Context, r ADLFillReport) (*ADLReconcileResult, error) {
	res := &ADLReconcileResult{}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("adl fill: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	d, err := scanADLDirective(tx.QueryRow(ctx,
		`SELECT `+adlDirectiveCols+` FROM adl_directives WHERE adl_seq=$1 FOR UPDATE`, r.AdlSeq))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("adl directive %d not found", r.AdlSeq))
	}
	if err != nil {
		return nil, fmt.Errorf("adl directive %d lock: %w", r.AdlSeq, err)
	}
	res.Directive = *d
	if d.Status == ADLStatusFilled {
		res.AlreadyReconciled = true
		return res, tx.Commit(ctx) // replay — the outbox dedups
	}
	if d.Status != ADLStatusQueued && d.Status != ADLStatusDispatched {
		return nil, fmt.Errorf("adl directive %d in terminal status %s — cannot fill", r.AdlSeq, d.Status)
	}
	if r.FilledQty.GreaterThan(d.Qty) {
		return nil, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"adl fill %d: filled qty %s exceeds directive qty %s",
			r.AdlSeq, r.FilledQty, d.Qty))
	}

	// The counterparty's position row — latest matching row; the
	// reduce-only close may already have settled it (quantity=0), so
	// the lookup does NOT filter on quantity.
	var cpPosID int64
	err = tx.QueryRow(ctx, `
		SELECT id FROM positions
		WHERE account_id=$1 AND instrument_id=$2 AND side=$3::position_side_enum
		ORDER BY id DESC LIMIT 1`,
		d.AccountID, d.InstrumentID, d.Side).Scan(&cpPosID)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("adl fill %d: no position row for acct %d instr %d side %s",
			r.AdlSeq, d.AccountID, d.InstrumentID, d.Side)
	}
	if err != nil {
		return nil, fmt.Errorf("adl fill %d: counterparty position read: %w", r.AdlSeq, err)
	}
	if r.MarkPrice.IsZero() {
		r.MarkPrice = d.BankruptcyPrice // honest fallback — never a fabricated 0
	}

	// Counterparty event — their force-order record.
	err = tx.QueryRow(ctx, `
		INSERT INTO liquidation_events
		    (account_id, position_id, instrument_id,
		     kind, side, quantity, price, mark_price,
		     insurance_fund_contribution, penalty_amount, adl_quintile)
		VALUES ($1,$2,$3,'ADL',$4::position_side_enum,$5::numeric,$6::numeric,$7::numeric,0,0,$8)
		RETURNING id`,
		d.AccountID, cpPosID, d.InstrumentID, d.Side,
		r.FilledQty.String(), r.FillPrice.String(), r.MarkPrice.String(),
		d.Quintile).Scan(&res.CounterpartyEventID)
	if err != nil {
		return nil, fmt.Errorf("adl fill %d: counterparty event: %w", r.AdlSeq, err)
	}

	// Liquidated-side event when the report carries the context — both
	// parties see their own ADL row in force-order history.
	if r.LiquidatedAccountID > 0 {
		var id int64
		err = tx.QueryRow(ctx, `
			INSERT INTO liquidation_events
			    (account_id, position_id, instrument_id,
			     kind, side, quantity, price, mark_price,
			     insurance_fund_contribution, penalty_amount, adl_quintile)
			VALUES ($1,$2,$3,'ADL',$4::position_side_enum,$5::numeric,$6::numeric,$7::numeric,0,0,$8)
			RETURNING id`,
			r.LiquidatedAccountID, r.LiquidatedPositionID, d.InstrumentID,
			r.LiquidatedSide, r.FilledQty.String(), r.FillPrice.String(),
			r.MarkPrice.String(), d.Quintile).Scan(&id)
		if err != nil {
			return nil, fmt.Errorf("adl fill %d: liquidated event: %w", r.AdlSeq, err)
		}
		res.LiquidatedEventID = &id
	}

	tag, err := tx.Exec(ctx, `
		UPDATE adl_directives
		SET status='FILLED', fill_price=$2::numeric, filled_at=now(),
		    liquidation_event_id=$3
		WHERE adl_seq=$1 AND status <> 'FILLED'`,
		r.AdlSeq, r.FillPrice.String(), res.CounterpartyEventID)
	if err != nil {
		return nil, fmt.Errorf("adl fill %d: directive update: %w", r.AdlSeq, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("adl fill %d: directive raced to FILLED — aborting to stay idempotent", r.AdlSeq)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("adl fill %d: commit: %w", r.AdlSeq, err)
	}
	return res, nil
}
