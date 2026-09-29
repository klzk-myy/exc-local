// Service — Task 16.3.21 runtime: strategy CRUD + lifecycle, the sweep
// that executes due recurring conversions and drift-triggered
// rebalances, and the approval-gated template marketplace.
package strategies

import (
	"context"
	"strconv"
	"strings"
	"time"

	"exchange/internal/funding"
	"exchange/internal/instruments"
	"exchange/internal/marketapi"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// OrderPipeline is the admission surface — *orders.Service in
// production. Every execution is a firm CLOB order through it (R14);
// no path here touches the engine directly.
type OrderPipeline interface {
	Submit(ctx context.Context, acct *orders.Account, req *orders.SubmitRequest) (*orders.Ack, error)
	Cancel(ctx context.Context, acct *orders.Account, orderID int64,
		actor, requestID, ip string) (*orders.Ack, error)
	AccountByID(ctx context.Context, id int64) (*orders.Account, error)
	InstrumentBySymbol(ctx context.Context, symbol string) (*orders.Instrument, error)
}

// ReadModel is the order-store read seam — reference prices, balances,
// and order truth for run reconciliation.
type ReadModel interface {
	ReferencePrice(ctx context.Context, instrumentID int64) (*decimal.Decimal, error)
	AvailableBalance(ctx context.Context, accountID int64, currency string) (*decimal.Decimal, error)
	GetOrder(ctx context.Context, orderID int64) (*orders.Order, error)
}

// Sessions is the instruments.SessionService seam — market-hours truth.
type Sessions interface {
	Calendar() *instruments.SessionCalendar
}

// BookReader supplies the BBO for spread gating (*marketapi.PgStore).
type BookReader interface {
	Snapshot(ctx context.Context, symbol string, depth int) (*marketapi.BookSnapshot, error)
}

// BalanceLister reads account balances (*funding.PgStore).
type BalanceLister interface {
	BalancesFor(ctx context.Context, accountID int64) ([]funding.BalanceRow, error)
}

// UsdConverter values non-USD balances (*settlement.RedisUsdConverter).
type UsdConverter interface {
	ToUSD(ctx context.Context, currency string, amount decimal.Decimal) (decimal.Decimal, error)
}

// Service implements the Task 16.3.21 runtime.
type Service struct {
	st       *Store
	ords     OrderPipeline
	rm       ReadModel
	sess     Sessions
	book     BookReader
	balances BalanceLister
	usd      UsdConverter
	now      func() time.Time
}

// Options wires the service; BookReader/UsdConverter may be nil —
// spread gating and USD valuation then degrade honestly (recorded).
type Options struct {
	Store    *Store
	Orders   OrderPipeline
	Read     ReadModel
	Sessions Sessions // nil → market-hours gate fails closed (skips runs)
	Book     BookReader
	Balances BalanceLister // required for REBALANCE
	USD      UsdConverter
	Now      func() time.Time
}

func NewService(o Options) (*Service, error) {
	if o.Store == nil || o.Orders == nil || o.Read == nil {
		return nil, excerrors.New("CONFIG_INVALID",
			"strategies: store/orders/read model required")
	}
	s := &Service{st: o.Store, ords: o.Orders, rm: o.Read,
		sess: o.Sessions, book: o.Book, balances: o.Balances, usd: o.USD}
	s.now = o.Now
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

var validCCY = func(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func normCCY(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// validateConfig enforces the per-kind schema — shared by direct creates
// and template instantiation so a template can never mint an invalid
// strategy.
func (s *Service) validateConfig(ctx context.Context, in *CreateInput) (*Strategy, error) {
	st := &Strategy{Kind: strings.ToUpper(strings.TrimSpace(in.Kind)),
		Label: strings.TrimSpace(in.Label), Status: StatusActive}
	switch st.Kind {
	case KindRecurringConversion:
		st.FromCurrency = normCCY(in.FromCurrency)
		st.ToCurrency = normCCY(in.ToCurrency)
		if !validCCY(st.FromCurrency) || !validCCY(st.ToCurrency) ||
			st.FromCurrency == st.ToCurrency {
			return nil, errorf(CodeStrategyConfigInvalid,
				"from_currency/to_currency must be distinct ISO-4217 codes")
		}
		amt, err := decimal.NewFromString(strings.TrimSpace(in.Amount))
		if err != nil || !amt.IsPositive() {
			return nil, errorf(CodeStrategyConfigInvalid,
				"amount must be a positive decimal in %s", st.FromCurrency)
		}
		st.Amount = &amt
		st.Schedule = strings.ToUpper(strings.TrimSpace(in.Schedule))
		switch st.Schedule {
		case ScheduleDaily, ScheduleWeekly, ScheduleMonthly:
		default:
			return nil, errorf(CodeStrategyConfigInvalid,
				"schedule must be DAILY|WEEKLY|MONTHLY")
		}
		// The conversion pair must resolve now or at run time — hard-fail
		// at create when neither direction exists as an instrument.
		if _, _, err := s.resolvePair(ctx, st.FromCurrency, st.ToCurrency); err != nil {
			return nil, err
		}
		st.NextRunAt = ptrTime(NextSlot(st.Schedule, s.now()))
	case KindRebalance:
		if len(in.Targets) == 0 {
			return nil, errorf(CodeStrategyConfigInvalid,
				"targets must map currency → weight")
		}
		st.Targets = map[string]decimal.Decimal{}
		total := decimal.Zero
		for c, w := range in.Targets {
			ccy := normCCY(c)
			if !validCCY(ccy) {
				return nil, errorf(CodeStrategyConfigInvalid,
					"target currency %q is not ISO-4217", c)
			}
			wd, err := decimal.NewFromString(strings.TrimSpace(w))
			if err != nil || !wd.IsPositive() || wd.GreaterThan(decimal.One) {
				return nil, errorf(CodeStrategyConfigInvalid,
					"target weight %q invalid (0 < w ≤ 1)", w)
			}
			st.Targets[ccy] = wd
			total = total.Add(wd)
		}
		if !total.Equal(decimal.One) {
			return nil, errorf(CodeStrategyConfigInvalid,
				"target weights must sum to 1 (got %s)", total)
		}
		band, err := decimal.NewFromString(strings.TrimSpace(in.DriftBandPct))
		if err != nil || !band.IsPositive() || band.GreaterThan(decimal.NewFromInt(100)) {
			return nil, errorf(CodeStrategyConfigInvalid,
				"drift_band_pct must be in (0,100]")
		}
		st.DriftBandPct = &band
	default:
		return nil, errorf(CodeStrategyConfigInvalid,
			"kind must be RECURRING_CONVERSION|REBALANCE")
	}
	return st, nil
}

func ptrTime(t time.Time) *time.Time { return &t }

// resolvePair picks the conversion instrument + side: the direct
// "{from}/{to}" pair converts by SELLing `from`; the inverse
// "{to}/{from}" pair converts by BUYing `to` with `from` quote.
func (s *Service) resolvePair(ctx context.Context, from, to string) (
	*orders.Instrument, string, error) {

	if inst, err := s.ords.InstrumentBySymbol(ctx, from+"/"+to); err != nil {
		return nil, "", err
	} else if inst != nil {
		return inst, "SELL", nil
	}
	if inst, err := s.ords.InstrumentBySymbol(ctx, to+"/"+from); err != nil {
		return nil, "", err
	} else if inst != nil {
		return inst, "BUY", nil
	}
	return nil, "", errorf(CodeStrategyConfigInvalid,
		"no instrument covers %s/%s or %s/%s", from, to, to, from)
}

// ---------------------------------------------------------------------------
// CRUD + lifecycle
// ---------------------------------------------------------------------------

// Create registers a new account-owned strategy.
func (s *Service) Create(ctx context.Context, accountID int64,
	in CreateInput) (*Strategy, error) {

	st, err := s.validateConfig(ctx, &in)
	if err != nil {
		return nil, err
	}
	st.AccountID = accountID
	if err := s.st.Create(ctx, st); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Service) List(ctx context.Context, accountID int64) ([]Strategy, error) {
	out, err := s.st.List(ctx, accountID)
	if out == nil {
		out = []Strategy{}
	}
	return out, err
}

// Detail returns the strategy + recent runs.
func (s *Service) Detail(ctx context.Context, accountID, strategyID int64) (*Detail, error) {
	st, err := s.st.Get(ctx, accountID, strategyID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errorf(CodeStrategyNotFound, "strategy %d not found", strategyID)
	}
	runs, err := s.st.Runs(ctx, strategyID, 50)
	if err != nil {
		return nil, err
	}
	return &Detail{Strategy: *st, Runs: runs}, nil
}

// Pause halts scheduling; in-flight runs are left to settle (the orders
// already submitted are firm — cancellation only comes via Cancel).
func (s *Service) Pause(ctx context.Context, accountID, strategyID int64) (*Strategy, error) {
	st, err := s.st.SetStatus(ctx, accountID, strategyID, StatusPaused)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errorf(CodeStrategyNotFound,
			"strategy %d not found or not pausable", strategyID)
	}
	return st, nil
}

// Resume reactivates a paused strategy; a missed slot stays scheduled —
// the next sweep fires it once (catch-up) rather than dropping it.
func (s *Service) Resume(ctx context.Context, accountID, strategyID int64) (*Strategy, error) {
	st, err := s.st.SetStatus(ctx, accountID, strategyID, StatusActive)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errorf(CodeStrategyNotFound,
			"strategy %d not found or not resumable", strategyID)
	}
	return st, nil
}

// Cancel terminates the strategy and cancels any open run's child
// orders through the order pipeline.
func (s *Service) Cancel(ctx context.Context, accountID, strategyID int64) (*Strategy, error) {
	st, err := s.st.SetStatus(ctx, accountID, strategyID, StatusCancelled)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errorf(CodeStrategyNotFound,
			"strategy %d not found or already cancelled", strategyID)
	}
	// Cancel live children of any open run — zero orphan legs.
	acct, aerr := s.ords.AccountByID(ctx, accountID)
	open, oerr := s.st.openRunFor(ctx, strategyID)
	if oerr == nil && open != nil && aerr == nil && acct != nil {
		for _, oid := range open.OrderIDs {
			_, _ = s.ords.Cancel(ctx, acct, oid,
				"strategy:"+strconv.FormatInt(strategyID, 10), "", "")
		}
		_ = s.st.FinishRun(ctx, open.RunID, RunCancelled, "STRATEGY_CANCELLED",
			open.Legs, open.OrderIDs)
	}
	return st, nil
}
