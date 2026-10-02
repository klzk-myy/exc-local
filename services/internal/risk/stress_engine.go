// stress_engine.go — Phase-19 Task 19.3.13 §1–§4: the margin-model
// stress suite and daily backtester (spec §13.10, §24 #206).
//
// OFFLINE BATCH ONLY — the engine replays a point-in-time portfolio
// snapshot through hypothetical scenarios and persists one
// margin_model_runs row per scenario (migration 064). It never runs in
// the matching hot path, never takes the account ledger locks, and
// never mutates positions: every outcome is a hypothetical measured
// against the liquidation machinery's own constants (§13.4 floor
// bands, force-cash factors, the 1%-of-OI auction trigger).
//
//   - Stress suite: ScenarioLibrary() rate shocks (±1σ/2σ/3σ per
//     instrument group), volatility spikes, weekend gap moves, a
//     flash-crash replay seam (FlashCrashProvider — Phase-8/Phase-20
//     tick datasets bind it), and an insurance-fund depletion cascade.
//     RunStressSuite measures per-scenario margin shortfalls, auction
//     failures and fund drawdown, then evaluates the adequacy metric:
//     the USD fund balance must cover the worst-1% scenario shortfall.
//   - Backtester: daily comparison of the predicted liquidation floor
//     per event kind (DIRECT_CLOSE → mark, AUCTION_FILL → ×0.98/×1.02,
//     FORCE_CASH/ADL → ×0.95/×1.05) vs realized fills in
//     liquidation_events (migration 230); breaches persist and page a
//     Risk Manager review case; a Basel-style rolling 250-day breach
//     count gates the model (>4 = review).
//
// Entry points: StressEngine.RunStressSuite / TickOnce +
// StressScheduler.RunDue (weekly), Backtester.RunDailyBacktest /
// RunDue (daily) — production binds the tickers; binding is the
// parent's job.
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Scenario kind vocabulary.
const (
	ScenarioKindRateShock       = "RATE_SHOCK"
	ScenarioKindVolatilitySpike = "VOLATILITY_SPIKE"
	ScenarioKindWeekendGap      = "WEEKEND_GAP"
	ScenarioKindFlashCrash      = "FLASH_CRASH"
	ScenarioKindFundDepletion   = "FUND_DEPLETION"
	backtestScenarioName        = "DAILY_SLIPPAGE"
	backtestMaxCatchUpDays      = 31 // backtest RunDue replay ceiling (days)
)

// GroupDailySigma is the per-instrument-group one-day σ the rate-shock
// scenarios scale by (calibration defaults owned by the Risk Manager —
// majors move tighter than exotics). NOT a live margin parameter.
var GroupDailySigma = map[string]decimal.Decimal{
	GroupMajor:  decimal.RequireFromString("0.006"),
	GroupMinor:  decimal.RequireFromString("0.009"),
	GroupExotic: decimal.RequireFromString("0.018"),
}

// StressScenario is one library entry. Price impact resolves per
// position in precedence order:
//
//  1. SymbolShifts[symbol] — explicit per-symbol move (flash-crash
//     replay injects its tick-derived moves here),
//  2. UniformShift — every position moves the same fraction (weekend
//     gap),
//  3. GroupDailySigma[group] × SigmaMultiple — the σ-scaled rate
//     shock / volatility-spike default.
//
// VolMultiplier re-margins the book (volatility spikes raise required
// margin, not just prices); FundDrainFraction removes part of the fund
// before the adequacy check (depletion cascade).
type StressScenario struct {
	Name              string                     `json:"name"`
	Kind              string                     `json:"kind"`
	SigmaMultiple     decimal.Decimal            `json:"sigma_multiple,omitempty"`
	UniformShift      decimal.Decimal            `json:"uniform_shift,omitempty"`
	SymbolShifts      map[string]decimal.Decimal `json:"symbol_shifts,omitempty"`
	VolMultiplier     decimal.Decimal            `json:"vol_multiplier,omitempty"`
	FundDrainFraction decimal.Decimal            `json:"fund_drain_fraction,omitempty"`
	Description       string                     `json:"description"`
}

// ShiftFor resolves the scenario's signed price shift for one position.
func (s StressScenario) ShiftFor(symbol, group string) decimal.Decimal {
	if v, ok := s.SymbolShifts[symbol]; ok {
		return v
	}
	if !s.UniformShift.IsZero() {
		return s.UniformShift
	}
	sig := GroupDailySigma[group]
	if sig.IsZero() {
		sig = GroupDailySigma[GroupExotic] // unknown group → widest σ (§2.7)
	}
	return sig.Mul(s.SigmaMultiple)
}

// VolMult is the effective re-margining factor (default 1 — no hike).
func (s StressScenario) VolMult() decimal.Decimal {
	if s.VolMultiplier.IsPositive() {
		return s.VolMultiplier
	}
	return decimal.One
}

// ScenarioLibrary is the canonical weekly suite. Flash-crash replay
// entries are appended by the FlashCrashProvider seam at run time —
// the library ships without them so the suite is deterministic when no
// provider is bound.
func ScenarioLibrary() []StressScenario {
	lib := []StressScenario{}
	for _, mult := range []string{"-3", "-2", "-1", "1", "2", "3"} {
		m := decimal.RequireFromString(mult)
		sign := "+"
		if m.IsNegative() {
			sign = "-"
		}
		abs := mult
		if abs[0] == '-' {
			abs = abs[1:]
		}
		lib = append(lib, StressScenario{
			Name:          fmt.Sprintf("RATE_SHOCK_%s%sSIGMA", sign, abs),
			Kind:          ScenarioKindRateShock,
			SigmaMultiple: m,
			Description:   fmt.Sprintf("parallel %s%sσ rate move per instrument group", sign, abs),
		})
	}
	lib = append(lib,
		StressScenario{
			Name:          "VOL_SPIKE_DOWN_2X",
			Kind:          ScenarioKindVolatilitySpike,
			SigmaMultiple: decimal.NewFromInt(-2),
			VolMultiplier: decimal.NewFromInt(2),
			Description:   "−2σ move with required margin doubled — the margin-call cascade case",
		},
		StressScenario{
			Name:         "WEEKEND_GAP_DOWN_2PCT",
			Kind:         ScenarioKindWeekendGap,
			UniformShift: decimal.RequireFromString("-0.02"),
			Description:  "uniform −2% gap at the Monday Sydney open",
		},
		StressScenario{
			Name:         "WEEKEND_GAP_UP_2PCT",
			Kind:         ScenarioKindWeekendGap,
			UniformShift: decimal.RequireFromString("0.02"),
			Description:  "uniform +2% gap at the Monday Sydney open",
		},
		StressScenario{
			Name:              "FUND_DEPLETION_CASCADE",
			Kind:              ScenarioKindFundDepletion,
			SigmaMultiple:     decimal.NewFromInt(-3),
			FundDrainFraction: decimal.RequireFromString("0.9"),
			Description:       "−3σ move with the fund pre-drained to 10% — adequacy under near-depleted depth",
		},
	)
	return lib
}

// ---------------------------------------------------------------------------
// Portfolio snapshot seam
// ---------------------------------------------------------------------------

// StressPosition is one open position as the suite sees it — quote-ccy
// quantities carried alongside the account's USD economics.
type StressPosition struct {
	PositionID       int64
	AccountID        int64
	InstrumentID     int64
	Symbol           string
	Group            string          // MAJOR | MINOR | EXOTIC
	Side             string          // LONG | SHORT
	Quantity         decimal.Decimal // signed (SHORT negative)
	MarkPrice        decimal.Decimal
	LiquidationPrice decimal.Decimal // may be 0
	MarginUsedUSD    decimal.Decimal // this position's USD margin slice
}

// StressAccount is one margin account's snapshot (USD numeraire — the
// margin engine's canonical unit, spec §13.1).
type StressAccount struct {
	AccountID     int64
	EquityUSD     decimal.Decimal
	UsedMarginUSD decimal.Decimal
	Positions     []StressPosition
}

// StressPortfolioSource supplies the point-in-time book + the
// per-instrument OI the 1% auction-trigger denominator needs.
type StressPortfolioSource interface {
	Snapshot(ctx context.Context) ([]StressAccount, error)
	InstrumentOI(ctx context.Context) (map[int64]decimal.Decimal, error)
}

// PgStressPortfolioSource implements the seam over pgx: CROSS/PORTFOLIO
// margin accounts (the liquidation engine's own candidate set — ISOLATED
// positions liquidate independently and are excluded, mirroring
// AccountsForScan) with their open positions.
type PgStressPortfolioSource struct{ pool *pgxpool.Pool }

// NewPgStressPortfolioSource binds the pool — nil rejected fail-closed.
func NewPgStressPortfolioSource(pool *pgxpool.Pool) (*PgStressPortfolioSource, error) {
	if pool == nil {
		return nil, fmt.Errorf("stress portfolio source: nil pgx pool")
	}
	return &PgStressPortfolioSource{pool: pool}, nil
}

// Snapshot implements StressPortfolioSource.
func (s *PgStressPortfolioSource) Snapshot(ctx context.Context) ([]StressAccount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ma.account_id, ma.equity::text, ma.used_margin::text,
		       p.id, p.instrument_id, i.symbol, i.base_currency, i.quote_currency,
		       p.side::text, p.quantity::text,
		       COALESCE(p.mark_price, p.entry_price)::text,
		       COALESCE(p.liquidation_price, 0)::text,
		       p.margin_used::text
		FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		JOIN margin_accounts ma ON ma.account_id = p.account_id
		WHERE p.quantity <> 0 AND ma.margin_mode IN ('CROSS','PORTFOLIO')
		ORDER BY ma.account_id, p.id`)
	if err != nil {
		return nil, fmt.Errorf("stress snapshot: %w", err)
	}
	defer rows.Close()
	idx := map[int64]int{}
	var out []StressAccount
	for rows.Next() {
		var (
			acctID, instrID, posID  int64
			equity, used, sym, base string
			quote, side, qty, mark  string
			liq, mu                 string
		)
		if err := rows.Scan(&acctID, &equity, &used, &posID, &instrID,
			&sym, &base, &quote, &side, &qty, &mark, &liq, &mu); err != nil {
			return nil, fmt.Errorf("stress snapshot scan: %w", err)
		}
		slot, ok := idx[acctID]
		if !ok {
			eq, err := decimal.NewFromString(equity)
			if err != nil {
				return nil, fmt.Errorf("stress acct %d equity %q: %w", acctID, equity, err)
			}
			um, err := decimal.NewFromString(used)
			if err != nil {
				return nil, fmt.Errorf("stress acct %d used_margin %q: %w", acctID, used, err)
			}
			out = append(out, StressAccount{AccountID: acctID, EquityUSD: eq, UsedMarginUSD: um})
			slot = len(out) - 1
			idx[acctID] = slot
		}
		p := StressPosition{
			PositionID: posID, AccountID: acctID, InstrumentID: instrID,
			Symbol: sym, Side: side,
			Group: ClassifyInstrumentGroup(base, quote),
		}
		var err error
		if p.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, fmt.Errorf("stress pos %d qty %q: %w", posID, qty, err)
		}
		if p.MarkPrice, err = decimal.NewFromString(mark); err != nil {
			return nil, fmt.Errorf("stress pos %d mark %q: %w", posID, mark, err)
		}
		if p.LiquidationPrice, err = decimal.NewFromString(liq); err != nil {
			return nil, fmt.Errorf("stress pos %d liq %q: %w", posID, liq, err)
		}
		if p.MarginUsedUSD, err = decimal.NewFromString(mu); err != nil {
			return nil, fmt.Errorf("stress pos %d margin_used %q: %w", posID, mu, err)
		}
		out[slot].Positions = append(out[slot].Positions, p)
	}
	return out, rows.Err()
}

// InstrumentOI implements StressPortfolioSource — notional OI per
// instrument (same projection as PgLiquidationStore.OpenInterest).
func (s *PgStressPortfolioSource) InstrumentOI(ctx context.Context) (map[int64]decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT instrument_id, COALESCE(SUM(ABS(quantity) * COALESCE(mark_price, entry_price)), 0)::text
		FROM positions WHERE quantity <> 0 GROUP BY instrument_id`)
	if err != nil {
		return nil, fmt.Errorf("stress oi: %w", err)
	}
	defer rows.Close()
	out := map[int64]decimal.Decimal{}
	for rows.Next() {
		var id int64
		var txt string
		if err := rows.Scan(&id, &txt); err != nil {
			return nil, fmt.Errorf("stress oi scan: %w", err)
		}
		d, err := decimal.NewFromString(txt)
		if err != nil {
			return nil, fmt.Errorf("stress oi instr %d %q: %w", id, txt, err)
		}
		out[id] = d
	}
	return out, rows.Err()
}

// FundBalanceSource supplies the USD fund balance for the adequacy
// metric (per-currency segmentation keeps adequacy in the USD leg —
// shortfalls are USD-numeraire).
type FundBalanceSource interface {
	FundBalanceUSD(ctx context.Context) (decimal.Decimal, error)
}

// InsuranceFundBalanceSource adapts *InsuranceFundService to the seam —
// the production binding; a nil/absent USD row reads as 0 (an unfunded
// currency covers nothing).
type InsuranceFundBalanceSource struct{ Svc *InsuranceFundService }

// FundBalanceUSD implements FundBalanceSource.
func (a InsuranceFundBalanceSource) FundBalanceUSD(ctx context.Context) (decimal.Decimal, error) {
	if a.Svc == nil {
		return decimal.Zero, excerrors.New(CodeRiskLimitsInternal,
			"fund balance source: nil insurance fund service")
	}
	b, err := a.Svc.Balance(ctx, "USD")
	if err != nil {
		return decimal.Zero, err
	}
	if b == nil {
		return decimal.Zero, nil
	}
	return b.Balance, nil
}

// PgFundBalanceSource is the pool-only variant (no service wiring).
type PgFundBalanceSource struct{ pool *pgxpool.Pool }

// NewPgFundBalanceSource binds the pool.
func NewPgFundBalanceSource(pool *pgxpool.Pool) *PgFundBalanceSource {
	return &PgFundBalanceSource{pool: pool}
}

// FundBalanceUSD implements FundBalanceSource.
func (s *PgFundBalanceSource) FundBalanceUSD(ctx context.Context) (decimal.Decimal, error) {
	var txt *string
	err := s.pool.QueryRow(ctx,
		`SELECT balance::text FROM insurance_fund WHERE currency = 'USD'`).Scan(&txt)
	if err == pgx.ErrNoRows {
		return decimal.Zero, nil
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("fund balance USD: %w", err)
	}
	return decimal.RequireFromString(*txt), nil
}

// FlashCrashProvider is the Phase-8/Phase-20 replay seam: historical
// tick datasets bind here to inject FLASH_CRASH_* scenarios into the
// suite. nil provider → no replay scenarios (library stays canonical).
type FlashCrashProvider interface {
	Scenarios(ctx context.Context) ([]StressScenario, error)
}

// ---------------------------------------------------------------------------
// Stress engine
// ---------------------------------------------------------------------------

// StressEngineConfig tunes the hypothetical-evaluation constants.
type StressEngineConfig struct {
	// StopOutFraction: an account is driven to liquidation when
	// stressed equity < stressed used-margin × this fraction (the
	// retail §13.6d stop-out, conservative for institutional books).
	StopOutFraction decimal.Decimal // default 0.5
	// DirectCloseSlippage — exit-cost fraction for positions below the
	// 1%-of-OI auction trigger (liquidation slippageBps = 200bps).
	DirectCloseSlippage decimal.Decimal // default 0.02
	// AuctionSlippage — exit-cost fraction for auction-sized positions
	// (FORCE_CASH mark×0.95/×1.05 band = 5% worst case).
	AuctionSlippage decimal.Decimal // default 0.05
	// AuctionOIFraction — the §13.4 1%-of-OI trigger denominator.
	AuctionOIFraction decimal.Decimal // default 0.01
}

func (c StressEngineConfig) normalize() StressEngineConfig {
	if !c.StopOutFraction.IsPositive() {
		c.StopOutFraction = decimal.RequireFromString("0.5")
	}
	if !c.DirectCloseSlippage.IsPositive() {
		c.DirectCloseSlippage = decimal.RequireFromString("0.02")
	}
	if !c.AuctionSlippage.IsPositive() {
		c.AuctionSlippage = decimal.RequireFromString("0.05")
	}
	if !c.AuctionOIFraction.IsPositive() {
		c.AuctionOIFraction = decimal.RequireFromString("0.01")
	}
	return c
}

// StressEngine replays scenarios against the portfolio snapshot.
type StressEngine struct {
	source  StressPortfolioSource
	store   ModelRunStore
	fund    FundBalanceSource
	flash   FlashCrashProvider // optional
	alerter OpsAlerter
	cfg     StressEngineConfig
	now     func() time.Time
	logf    func(format string, args ...any)
	// InitiatedBy — optional executor identity stamped on every run row
	// (nil = scheduler, the scheduled-sweep convention).
	InitiatedBy *int64
}

// NewStressEngine wires the engine. Source, store and fund are
// mandatory — a suite that cannot snapshot, persist, or measure
// adequacy is a defect, not a degraded engine (§2.7).
func NewStressEngine(source StressPortfolioSource, store ModelRunStore,
	fund FundBalanceSource, flash FlashCrashProvider, alerter OpsAlerter,
	cfg StressEngineConfig, now func() time.Time,
	logf func(string, ...any)) (*StressEngine, error) {

	if source == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "stress engine: nil portfolio source")
	}
	if store == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "stress engine: nil run store")
	}
	if fund == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "stress engine: nil fund balance source")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &StressEngine{source: source, store: store, fund: fund, flash: flash,
		alerter: alerter, cfg: cfg.normalize(), now: now, logf: logf}, nil
}

// ScenarioResult is one scenario's measured outcome.
type ScenarioResult struct {
	Scenario                 string `json:"scenario"`
	Kind                     string `json:"kind"`
	AccountsEvaluated        int    `json:"accounts_evaluated"`
	AccountsLiquidated       int    `json:"accounts_liquidated"`
	ShortfallAccounts        int    `json:"shortfall_accounts"` // = persisted breach_count
	AuctionPositions         int    `json:"auction_positions"`
	AuctionFailures          int    `json:"auction_failures"` // auction-sized legs in negative-equity accounts
	GrossShortfallUSD        string `json:"gross_shortfall_usd"`
	FundDrawdownUSD          string `json:"fund_drawdown_usd"`
	WorstAccountShortfallUSD string `json:"worst_account_shortfall_usd"`
	FundAfterDrainUSD        string `json:"fund_after_drain_usd"`
	Adequate                 bool   `json:"adequate"`
	RunID                    int64  `json:"run_id,omitempty"`
}

// StressSuiteResult is the suite aggregate.
type StressSuiteResult struct {
	Results                 []ScenarioResult `json:"results"`
	WorstOnePctShortfallUSD string           `json:"worst_one_pct_shortfall_usd"`
	FundBalanceUSD          string           `json:"fund_balance_usd"`
	Adequate                bool             `json:"adequate"`
	RanAt                   time.Time        `json:"ran_at"`
}

// WorstOnePctShortfall implements WorstCaseShortfallSource — the
// adequacy leg of CalibratedFundTarget. The latest suite's worst-1%
// shortfall is read from persisted runs (a fresh suite is not re-run).
func (e *StressEngine) WorstOnePctShortfall(ctx context.Context) (decimal.Decimal, error) {
	latest, err := e.store.LatestRun(ctx, RunKindStress)
	if err != nil {
		return decimal.Zero, err
	}
	if latest == nil {
		return decimal.Zero, nil // no suite yet — no proven shortfall metric
	}
	var m struct {
		WorstOnePctShortfallUSD string `json:"worst_one_pct_shortfall_usd"`
	}
	if err := json.Unmarshal(latest.ResultMetrics, &m); err != nil {
		return decimal.Zero, fmt.Errorf("stress run %d metrics: %w", latest.RunID, err)
	}
	if m.WorstOnePctShortfallUSD == "" {
		return decimal.Zero, nil
	}
	return decimal.NewFromString(m.WorstOnePctShortfallUSD)
}

// evalScenario runs one scenario over the snapshot (pure math — no IO).
func (e *StressEngine) evalScenario(accounts []StressAccount,
	oi map[int64]decimal.Decimal, sc StressScenario,
	fundUSD decimal.Decimal) ScenarioResult {

	res := ScenarioResult{Scenario: sc.Name, Kind: sc.Kind}
	fundAfter := fundUSD
	if sc.FundDrainFraction.IsPositive() {
		fundAfter = fundUSD.Mul(decimal.One.Sub(sc.FundDrainFraction)).Round(8)
	}
	res.FundAfterDrainUSD = fundAfter.String()

	gross := decimal.Zero
	worst := decimal.Zero
	for _, a := range accounts {
		res.AccountsEvaluated++
		pnlDelta := decimal.Zero
		for _, p := range a.Positions {
			shift := sc.ShiftFor(p.Symbol, p.Group)
			pnlDelta = pnlDelta.Add(p.Quantity.Mul(p.MarkPrice).Mul(shift))
		}
		stressedEquity := a.EquityUSD.Add(pnlDelta)
		stressedUsed := a.UsedMarginUSD.Mul(sc.VolMult())

		liquidated := stressedEquity.IsNegative()
		if !liquidated && stressedUsed.IsPositive() {
			liquidated = stressedEquity.LessThan(stressedUsed.Mul(e.cfg.StopOutFraction))
		}
		exitCost := decimal.Zero
		auctionLegs := 0
		if liquidated {
			res.AccountsLiquidated++
			for _, p := range a.Positions {
				shift := sc.ShiftFor(p.Symbol, p.Group)
				notional := p.Quantity.Abs().Mul(p.MarkPrice).Mul(decimal.One.Add(shift))
				slip := e.cfg.DirectCloseSlippage
				if o := oi[p.InstrumentID]; o.IsPositive() &&
					notional.GreaterThan(o.Mul(e.cfg.AuctionOIFraction)) {
					slip = e.cfg.AuctionSlippage
					auctionLegs++
				}
				exitCost = exitCost.Add(notional.Mul(slip))
			}
		}
		res.AuctionPositions += auctionLegs
		shortfall := stressedEquity.Sub(exitCost).Neg()
		if !shortfall.IsPositive() {
			continue
		}
		res.ShortfallAccounts++
		res.AuctionFailures += auctionLegs
		gross = gross.Add(shortfall)
		if shortfall.GreaterThan(worst) {
			worst = shortfall
		}
	}
	res.GrossShortfallUSD = gross.Round(8).String()
	res.FundDrawdownUSD = gross.Round(8).String()
	res.WorstAccountShortfallUSD = worst.Round(8).String()
	res.Adequate = !gross.GreaterThan(fundAfter)
	return res
}

// WorstPctileShortfall returns the shortfall at the pct-tail boundary
// of the scenario drawdown distribution: sort desc, take index
// ceil(n×pct/100)−1. For the library size (<100 scenarios) pct=1 is
// the worst single scenario — the coverage the fund must carry.
func WorstPctileShortfall(results []ScenarioResult, pct int) decimal.Decimal {
	if len(results) == 0 {
		return decimal.Zero
	}
	draws := make([]decimal.Decimal, 0, len(results))
	for _, r := range results {
		d, err := decimal.NewFromString(r.FundDrawdownUSD)
		if err == nil {
			draws = append(draws, d)
		}
	}
	if len(draws) == 0 {
		return decimal.Zero
	}
	sort.Slice(draws, func(i, j int) bool { return draws[i].GreaterThan(draws[j]) })
	if pct <= 0 {
		pct = 1
	}
	k := (len(draws)*pct + 99) / 100 // ceil
	if k > len(draws) {
		k = len(draws)
	}
	return draws[k-1]
}

// scenarioMetrics marshals the metrics jsonb persisted on the run row.
func scenarioMetrics(res ScenarioResult, worst1pct decimal.Decimal) json.RawMessage {
	b, err := json.Marshal(res)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return b
	}
	m["worst_one_pct_shortfall_usd"] = worst1pct.String()
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return out
}

// RunStressSuite executes the canonical library (+ any flash-crash
// replay scenarios the provider supplies) and persists one
// margin_model_runs row per scenario — kind=STRESS, status=PASS when
// the scenario's drawdown is covered by the (post-drain) fund. An
// inadequacy routes a P1 Risk Manager review alert carrying every run
// id (the §13.10 review-case seam is the OpsAlerter).
func (e *StressEngine) RunStressSuite(ctx context.Context) (*StressSuiteResult, error) {
	accounts, err := e.source.Snapshot(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeRiskLimitsInternal, "stress snapshot", err)
	}
	oi, err := e.source.InstrumentOI(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeRiskLimitsInternal, "stress oi", err)
	}
	fundUSD, err := e.fund.FundBalanceUSD(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeRiskLimitsInternal, "stress fund balance", err)
	}
	scenarios := ScenarioLibrary()
	if e.flash != nil {
		extra, err := e.flash.Scenarios(ctx)
		if err != nil {
			return nil, excerrors.Wrap(CodeRiskLimitsInternal, "flash-crash scenarios", err)
		}
		scenarios = append(scenarios, extra...)
	}

	res := &StressSuiteResult{RanAt: e.now(), FundBalanceUSD: fundUSD.String()}
	results := make([]ScenarioResult, 0, len(scenarios))
	for _, sc := range scenarios {
		results = append(results, e.evalScenario(accounts, oi, sc, fundUSD))
	}
	worst1 := WorstPctileShortfall(results, 1)
	res.WorstOnePctShortfallUSD = worst1.String()
	res.Adequate = !worst1.GreaterThan(fundUSD)

	var inadequate []string
	for i := range results {
		r := &results[i]
		status := RunStatusPass
		if !r.Adequate {
			status = RunStatusFail
			inadequate = append(inadequate, r.Scenario)
		}
		id, err := e.store.InsertRun(ctx, ModelRun{
			Kind:          RunKindStress,
			Scenario:      r.Scenario,
			Status:        status,
			ResultMetrics: scenarioMetrics(*r, worst1),
			BreachCount:   r.ShortfallAccounts,
			InitiatedBy:   e.InitiatedBy,
			CreatedAt:     e.now(),
		})
		if err != nil {
			return res, excerrors.Wrap(CodeRiskLimitsInternal,
				fmt.Sprintf("stress run persist %s", r.Scenario), err)
		}
		r.RunID = id
	}
	res.Results = results

	if !res.Adequate {
		e.raise(ctx, codeMarginModelAdequacy, fmt.Sprintf(
			"insurance fund adequacy breach: worst-1%% scenario shortfall %s exceeds fund balance %s (inadequate scenarios: %v)",
			worst1, fundUSD, inadequate), map[string]string{
			"worst_one_pct_shortfall_usd": worst1.String(),
			"fund_balance_usd":            fundUSD.String()})
	}
	return res, nil
}

// TickOnce is the on-demand entry point — identical to RunStressSuite;
// the parent's scheduler binds it to a ticker/cron and the admin
// surface binds it to the "run now" control.
func (e *StressEngine) TickOnce(ctx context.Context) (*StressSuiteResult, error) {
	return e.RunStressSuite(ctx)
}

func (e *StressEngine) raise(ctx context.Context, code, summary string, details map[string]string) {
	if e.alerter == nil {
		e.logf("stress engine: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.alerter.Raise(actx, OpsAlert{
		Severity: SeverityP1, Code: code, Summary: summary, Details: details,
	}); err != nil {
		e.logf("stress engine: alert %s dispatch failed: %v", code, err)
	}
}

// StressScheduler is the weekly driver: RunDue executes the suite when
// the newest STRESS run is older than Interval (or none exists — the
// first-run edge runs immediately).
type StressScheduler struct {
	Engine   *StressEngine
	Interval time.Duration // default 7×24h
}

// RunDue runs the suite when due. Returns (ran, error).
func (s *StressScheduler) RunDue(ctx context.Context) (bool, error) {
	if s.Engine == nil {
		return false, excerrors.New(CodeRiskLimitsInternal, "stress scheduler: nil engine")
	}
	interval := s.Interval
	if interval <= 0 {
		interval = 7 * 24 * time.Hour
	}
	latest, err := s.Engine.store.LatestRun(ctx, RunKindStress)
	if err != nil {
		return false, err
	}
	if latest != nil && s.Engine.now().Sub(latest.CreatedAt) < interval {
		return false, nil
	}
	_, err = s.Engine.RunStressSuite(ctx)
	return err == nil, err
}

// ---------------------------------------------------------------------------
// Backtester — predicted floors vs realized slippage (Task 19.3.13 §3)
// ---------------------------------------------------------------------------

// BaselGreenMaxBreaches is the rolling 250-day exceedance ceiling —
// more than 4 breaches in 250 trading days puts the model in review
// (Basel traffic-light convention adapted to liquidation slippage).
const BaselGreenMaxBreaches = 4

// BacktestWindowDays is the rolling count window.
const BacktestWindowDays = 250

// RealizedLiquidation is one liquidation_events row as the backtester
// sees it.
type RealizedLiquidation struct {
	EventID   int64
	Kind      string // DIRECT_CLOSE | AUCTION_FILL | FORCE_CASH | ADL
	Side      string // LONG | SHORT
	Quantity  decimal.Decimal
	Price     decimal.Decimal // realized close price
	MarkPrice decimal.Decimal // mark at close (may be 0 = unrecorded)
}

// BacktestSource supplies realized liquidations for one UTC day.
type BacktestSource interface {
	LiquidationsForDay(ctx context.Context, day time.Time) ([]RealizedLiquidation, error)
}

// PgBacktestSource implements the seam over liquidation_events
// (migration 230).
type PgBacktestSource struct{ pool *pgxpool.Pool }

// NewPgBacktestSource binds the pool.
func NewPgBacktestSource(pool *pgxpool.Pool) (*PgBacktestSource, error) {
	if pool == nil {
		return nil, fmt.Errorf("backtest source: nil pgx pool")
	}
	return &PgBacktestSource{pool: pool}, nil
}

// LiquidationsForDay implements BacktestSource — [day 00:00, +24h).
func (s *PgBacktestSource) LiquidationsForDay(ctx context.Context, day time.Time) ([]RealizedLiquidation, error) {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, side::text, quantity::text, price::text,
		       COALESCE(mark_price, 0)::text
		FROM liquidation_events
		WHERE created_at >= $1 AND created_at < $2
		ORDER BY id`, start, start.Add(24*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("backtest events %s: %w", start.Format("2006-01-02"), err)
	}
	defer rows.Close()
	var out []RealizedLiquidation
	for rows.Next() {
		var (
			ev               RealizedLiquidation
			qty, price, mark string
		)
		if err := rows.Scan(&ev.EventID, &ev.Kind, &ev.Side, &qty, &price, &mark); err != nil {
			return nil, fmt.Errorf("backtest scan: %w", err)
		}
		var err error
		if ev.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, fmt.Errorf("event %d qty %q: %w", ev.EventID, qty, err)
		}
		if ev.Price, err = decimal.NewFromString(price); err != nil {
			return nil, fmt.Errorf("event %d price %q: %w", ev.EventID, price, err)
		}
		if ev.MarkPrice, err = decimal.NewFromString(mark); err != nil {
			return nil, fmt.Errorf("event %d mark %q: %w", ev.EventID, mark, err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// PredictedBound is the liquidation model's predicted worst fill for
// an event: the price beyond which a realized fill counts as a model
// breach. LONG legs close by selling — the bound is a floor; SHORT
// legs buy — the bound is a cap.
//
//	DIRECT_CLOSE → mark (the model's predicted price is the mark)
//	AUCTION_FILL → mark ×0.98 / ×1.02 (the §13.4 CALL floor band)
//	FORCE_CASH/ADL → mark ×0.95 / ×1.05 (the §13.4 failover band)
//
// (0, false) when no bound can be derived — an unmarked event is a
// data defect, and the caller counts it as a breach rather than
// skipping it (fail-closed evaluation).
func PredictedBound(ev RealizedLiquidation) (bound decimal.Decimal, floor bool, ok bool) {
	if !ev.MarkPrice.IsPositive() {
		return decimal.Zero, false, false
	}
	floor = ev.Side != "SHORT"
	switch ev.Kind {
	case "AUCTION_FILL":
		if floor {
			return ev.MarkPrice.Mul(decimal.RequireFromString("0.98")).Round(8), true, true
		}
		return ev.MarkPrice.Mul(decimal.RequireFromString("1.02")).Round(8), false, true
	case "FORCE_CASH", "ADL":
		if floor {
			return ev.MarkPrice.Mul(AuctionForceCashLongFactor).Round(8), true, true
		}
		return ev.MarkPrice.Mul(AuctionForceCashShortFactor).Round(8), false, true
	default: // DIRECT_CLOSE and any future kind — mark is the prediction
		return ev.MarkPrice, floor, true
	}
}

// Breached reports whether the realized fill beat the predicted bound
// in the adverse direction (sell below floor / buy above cap).
func Breached(ev RealizedLiquidation) bool {
	bound, floor, ok := PredictedBound(ev)
	if !ok {
		return true // unmarked event — count as breach (§2.7)
	}
	if floor {
		return ev.Price.LessThan(bound)
	}
	return ev.Price.GreaterThan(bound)
}

// BacktestResult is one day's evaluation.
type BacktestResult struct {
	Day              time.Time `json:"day"`
	EventsEvaluated  int       `json:"events_evaluated"`
	Breaches         int       `json:"breaches"`
	UnmarkedEvents   int       `json:"unmarked_events"`
	MaxSlippageBps   string    `json:"max_slippage_bps"`
	Rolling250dCount int       `json:"rolling_250d_breach_count"`
	// Coverage is the Kupiec/Christoffersen verdict over the rolling
	// window's per-day breach flags (nil until ≥1 prior run exists).
	Coverage *CoverageTestResult `json:"coverage,omitempty"`
	RunID    int64               `json:"run_id,omitempty"`
}

// Backtester runs the daily comparison + rolling Basel count.
type Backtester struct {
	source  BacktestSource
	store   ModelRunStore
	alerter OpsAlerter
	now     func() time.Time
	logf    func(format string, args ...any)
	// InitiatedBy — optional executor identity stamped on run rows.
	InitiatedBy *int64
}

// NewBacktester wires the backtester; source + store mandatory.
func NewBacktester(source BacktestSource, store ModelRunStore,
	alerter OpsAlerter, now func() time.Time,
	logf func(string, ...any)) (*Backtester, error) {

	if source == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "backtester: nil source")
	}
	if store == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "backtester: nil run store")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Backtester{source: source, store: store, alerter: alerter,
		now: now, logf: logf}, nil
}

// RunDailyBacktest evaluates one UTC day, persists the BACKTEST run
// row, and routes the Risk Manager review alert on any breach or when
// the rolling 250-day count exceeds the green-zone ceiling. A day with
// zero liquidations is a clean PASS row, not a skip — the register
// must show the evaluation happened (SDD edge: empty backtest day).
func (b *Backtester) RunDailyBacktest(ctx context.Context, day time.Time) (*BacktestResult, error) {
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	events, err := b.source.LiquidationsForDay(ctx, day)
	if err != nil {
		return nil, excerrors.Wrap(CodeRiskLimitsInternal, "backtest events", err)
	}
	res := &BacktestResult{Day: day, EventsEvaluated: len(events)}
	maxSlip := decimal.Zero
	for _, ev := range events {
		bound, _, ok := PredictedBound(ev)
		if !ok {
			res.UnmarkedEvents++
			res.Breaches++
			continue
		}
		if Breached(ev) {
			res.Breaches++
		}
		// |price − bound| / bound × 10⁴ — the realized slippage magnitude.
		if bound.IsPositive() {
			slip := ev.Price.Sub(bound).Abs().Div(bound).Mul(decimal.NewFromInt(10_000))
			if slip.GreaterThan(maxSlip) {
				maxSlip = slip
			}
		}
	}
	res.MaxSlippageBps = maxSlip.Round(2).String()

	// Basel coverage tests over the window's ordered breach flags —
	// persisted in result_metrics so reviewers see the statistical
	// verdict, not just the raw count. Today's flag is appended to the
	// register's history before the row lands.
	if cov, covErr := b.windowCoverage(ctx, *res); covErr == nil &&
		cov.Observations > 0 {
		res.Coverage = &cov
	}

	status := RunStatusPass
	if res.Breaches > 0 {
		status = RunStatusFail
	}
	metrics, _ := json.Marshal(res)
	runID, err := b.store.InsertRun(ctx, ModelRun{
		Kind:          RunKindBacktest,
		Scenario:      backtestScenarioName,
		Status:        status,
		ResultMetrics: metrics,
		BreachCount:   res.Breaches,
		InitiatedBy:   b.InitiatedBy,
		CreatedAt:     b.now(),
	})
	if err != nil {
		return res, excerrors.Wrap(CodeRiskLimitsInternal, "backtest run persist", err)
	}
	res.RunID = runID

	rolling, err := b.store.SumBreaches(ctx, RunKindBacktest,
		b.now().Add(-BacktestWindowDays*24*time.Hour))
	if err != nil {
		return res, excerrors.Wrap(CodeRiskLimitsInternal, "backtest rolling count", err)
	}
	res.Rolling250dCount = rolling

	if res.Coverage != nil && res.Coverage.Rejected95 {
		b.raise(ctx, codeMarginModelAdequacy, fmt.Sprintf(
			"margin backtest coverage test rejects the model at 95%% over %d days (Kupiec p=%.4f, independence p=%.4f) — model review required",
			res.Coverage.Observations, res.Coverage.KupiecPValue,
			res.Coverage.IndependencePValue),
			map[string]string{"run_id": fmt.Sprint(runID),
				"observations": fmt.Sprint(res.Coverage.Observations)})
	}

	if res.Breaches > 0 {
		b.raise(ctx, codeBacktestBreach, fmt.Sprintf(
			"margin backtest %s: %d/%d liquidation fills breached predicted floors (max slippage %s bps)",
			day.Format("2006-01-02"), res.Breaches, res.EventsEvaluated, res.MaxSlippageBps),
			map[string]string{"day": day.Format("2006-01-02"),
				"breaches": fmt.Sprint(res.Breaches), "run_id": fmt.Sprint(runID)})
	}
	if rolling > BaselGreenMaxBreaches {
		b.raise(ctx, codeMarginModelAdequacy, fmt.Sprintf(
			"margin backtest rolling %d-day breach count %d exceeds Basel green-zone ceiling %d — model review required",
			BacktestWindowDays, rolling, BaselGreenMaxBreaches),
			map[string]string{"rolling_breaches": fmt.Sprint(rolling)})
	}
	return res, nil
}

// windowCoverage reconstructs the rolling window's event totals and
// ordered per-day breach flags from the register, then evaluates
// Kupiec POF (events) + Christoffersen independence (days). today
// carries the in-flight day's flag + counts (its row isn't persisted
// yet when RunDailyBacktest computes this).
func (b *Backtester) windowCoverage(ctx context.Context, today BacktestResult) (CoverageTestResult, error) {
	runs, err := b.store.ListRuns(ctx, RunKindBacktest,
		b.now().Add(-BacktestWindowDays*24*time.Hour))
	if err != nil {
		return CoverageTestResult{}, err
	}
	flags := make([]bool, 0, len(runs)+1)
	events, breaches := today.EventsEvaluated, today.Breaches
	for _, r := range runs {
		flags = append(flags, r.BreachCount > 0)
		breaches += r.BreachCount
		// events_evaluated lives in the persisted metrics blob.
		var m struct {
			EventsEvaluated int `json:"events_evaluated"`
		}
		if len(r.ResultMetrics) > 0 &&
			json.Unmarshal(r.ResultMetrics, &m) == nil {
			events += m.EventsEvaluated
		}
		// A breach implies ≥1 evaluated event even when the metrics
		// blob predates the events_evaluated field.
		if r.BreachCount > m.EventsEvaluated {
			events += r.BreachCount - m.EventsEvaluated
		}
	}
	flags = append(flags, today.Breaches > 0)
	return EvaluateCoverage(events, breaches, flags, BaselExpectedBreachProb), nil
}

// RunDue replays every uncovered day from the day after the latest
// BACKTEST run up to yesterday (bounded by backtestMaxCatchUpDays); a
// fresh register backtests yesterday only. Returns the days evaluated.
func (b *Backtester) RunDue(ctx context.Context) (int, error) {
	now := b.now()
	yesterday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).
		Add(-24 * time.Hour)
	latest, err := b.store.LatestRun(ctx, RunKindBacktest)
	if err != nil {
		return 0, err
	}
	from := yesterday
	if latest != nil {
		next := time.Date(latest.CreatedAt.Year(), latest.CreatedAt.Month(),
			latest.CreatedAt.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
		if next.After(yesterday) {
			return 0, nil // already covered through yesterday
		}
		if next.Before(yesterday.Add(-backtestMaxCatchUpDays * 24 * time.Hour)) {
			next = yesterday.Add(-backtestMaxCatchUpDays * 24 * time.Hour)
		}
		from = next
	}
	n := 0
	for d := from; !d.After(yesterday); d = d.Add(24 * time.Hour) {
		if _, err := b.RunDailyBacktest(ctx, d); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (b *Backtester) raise(ctx context.Context, code, summary string, details map[string]string) {
	if b.alerter == nil {
		b.logf("backtester: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.alerter.Raise(actx, OpsAlert{
		Severity: SeverityP1, Code: code, Summary: summary, Details: details,
	}); err != nil {
		b.logf("backtester: alert %s dispatch failed: %v", code, err)
	}
}
