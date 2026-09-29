package copy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"exchange/pkg/decimal"
)

// AppropriatenessChecker is the Task 14.3.7 gate seam —
// *compliance.CategorizationService satisfies it. nil = fail closed at
// construction; the LISTED gate can never run blind.
type AppropriatenessChecker interface {
	Appropriateness(ctx context.Context, accountID int64, instrumentClass string) error
}

// MutableChecker is the freeze/closure gate (accounts.FreezeService).
type MutableChecker interface {
	AssertMutable(ctx context.Context, accountID int64) error
}

// Auditor is the compliance audit-log seam for misconduct suspension —
// production binds admin.Log inside a transaction (hash-chained audit
// chain); AdminAuditor is the shipped adapter. nil = fail closed at
// construction.
type Auditor interface {
	LogSuspension(ctx context.Context, strategyID, adminUserID int64,
		ip, reason string) error
}

// Service is the copy-trading product service.
type Service struct {
	store     Store
	approv    AppropriatenessChecker
	checker   MutableChecker
	rates     RateSource
	auditor   Auditor
	poster    JournalPoster
	subledger SubledgerWriter
	now       func() time.Time
}

// ServiceOption binds the optional-at-construction seams that gate money
// movement (poster/subledger are checked again at settlement time).
type ServiceOption func(*Service)

// WithJournalPoster binds the balanced-GL posting service (settlement's
// DoubleEntryLedgerService).
func WithJournalPoster(p JournalPoster) ServiceOption {
	return func(s *Service) { s.poster = p }
}

// WithSubledgerWriter binds the PAMM-taxonomy sub-ledger append seam.
func WithSubledgerWriter(w SubledgerWriter) ServiceOption {
	return func(s *Service) { s.subledger = w }
}

// NewService wires the product layer. store/checker are mandatory; approv
// is mandatory because the LISTED gate must never run blind; auditor is
// mandatory because misconduct suspension must never go unaudited; rates
// is mandatory because discovery serves computed-only stats — an
// uncomputable stat is a missing strategy, never a fabricated one.
func NewService(store Store, approv AppropriatenessChecker,
	checker MutableChecker, rates RateSource, auditor Auditor,
	opts ...ServiceOption) (*Service, error) {
	if store == nil || approv == nil || checker == nil || auditor == nil || rates == nil {
		return nil, fmt.Errorf("copy: store, appropriateness, mutable checker, auditor and rate source are required")
	}
	s := &Service{store: store, approv: approv, checker: checker,
		rates: rates, auditor: auditor, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Strategy lifecycle
// ---------------------------------------------------------------------------

// CreateStrategyInput is the POST /copy/strategies body (manager-side).
type CreateStrategyInput struct {
	ManagerAccountID int64
	DisplayName      string
	Description      string
	Currency         string // stats/AUM denomination
	InstrumentClass  string // SPOT|FORWARD|SWAP|NDF|OPTION
	ProfitSharePct   string // manager-set; admin ceiling enforced at parse
}

// CreateStrategy provisions an INCUBATING profile. profit_share_pct is
// capped at the hard ceiling (the DB CHECK backs this — code rejects
// first so the API returns a coded 4xx, not a 5xx CHECK violation).
func (s *Service) CreateStrategy(ctx context.Context, in CreateStrategyInput) (*Strategy, error) {
	if in.ManagerAccountID <= 0 {
		return nil, errorf(CodeInvalidRequest, "manager_account_id required")
	}
	name := strings.TrimSpace(in.DisplayName)
	if name == "" || len(name) > 128 {
		return nil, errorf(CodeInvalidRequest, "display_name required (≤128 chars)")
	}
	if len(in.Description) > 1024 {
		return nil, errorf(CodeInvalidRequest, "description exceeds 1024 chars")
	}
	if len(in.Currency) != 3 {
		return nil, errorf(CodeInvalidRequest, "currency must be ISO 4217")
	}
	class := strings.ToUpper(strings.TrimSpace(in.InstrumentClass))
	if class == "" {
		class = "SPOT"
	}
	switch class {
	case "SPOT", "FORWARD", "SWAP", "NDF", "OPTION":
	default:
		return nil, errorf(CodeInvalidRequest, "instrument_class %q unknown", in.InstrumentClass)
	}
	pct := decimal.Zero
	if in.ProfitSharePct != "" {
		d, err := decimal.NewFromString(in.ProfitSharePct)
		if err != nil || d.IsNegative() {
			return nil, errorf(CodeInvalidRequest, "profit_share_pct %q invalid", in.ProfitSharePct)
		}
		pct = d
	}
	if pct.GreaterThan(decimal.RequireFromString(ProfitShareCeiling)) {
		return nil, errorf(CodeInvalidRequest,
			"profit_share_pct %s exceeds the %s%% admin ceiling", pct, ProfitShareCeiling)
	}
	if err := s.checker.AssertMutable(ctx, in.ManagerAccountID); err != nil {
		return nil, err
	}
	return s.store.CreateStrategy(ctx, &Strategy{
		ManagerAccountID: in.ManagerAccountID,
		DisplayName:      name,
		Description:      in.Description,
		Currency:         strings.ToUpper(in.Currency),
		InstrumentClass:  class,
		ProfitSharePct:   pct,
		Status:           StatusIncubating,
	})
}

// List flips INCUBATING → LISTED after BOTH gates pass (spec §12.9):
//  1. ≥ IncubationDays since profile creation;
//  2. Appropriateness PASS on the strategy's instrument class for the
//     manager account (Task 14.3.7 — SPOT is exempt by definition,
//     leveraged classes require a live non-expired PASS).
//
// callerAccountID must be the strategy's manager — listing is a
// manager-initiated action; pass 0 from a privileged/admin caller.
// The status flip is an atomic predicate UPDATE; a concurrent INCUBATING
// read racing the gate can't produce a listed strategy that failed.
func (s *Service) List(ctx context.Context, strategyID, callerAccountID int64) (*Strategy, error) {
	st, err := s.store.StrategyForUpdate(ctx, strategyID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errorf(CodeNotFound, "strategy %d not found", strategyID)
	}
	if callerAccountID > 0 && callerAccountID != st.ManagerAccountID {
		return nil, errorf(CodeForbidden,
			"account %d is not the manager of strategy %d", callerAccountID, strategyID)
	}
	if st.Status == StatusSuspended {
		return nil, errorf(CodeForbidden,
			"strategy %d is SUSPENDED (%s) — listing barred", strategyID, st.SuspendReason)
	}
	if st.Status != StatusIncubating {
		return nil, errorf(CodeInvalidRequest,
			"strategy %d is %s — only INCUBATING can list", strategyID, st.Status)
	}
	age := s.now().UTC().Sub(st.IncubatingSince)
	if age < time.Duration(IncubationDays)*24*time.Hour {
		return nil, errorf(CodeForbidden,
			"strategy %d has incubated %dd — LISTED requires ≥%d days",
			strategyID, int(age.Hours()/24), IncubationDays)
	}
	if err := s.approv.Appropriateness(ctx, st.ManagerAccountID, st.InstrumentClass); err != nil {
		return nil, err // coded PRODUCT_NOT_PERMITTED / SERVICE_DEGRADED
	}
	if err := s.store.ListStrategy(ctx, strategyID); err != nil {
		return nil, err
	}
	return s.store.StrategyByID(ctx, strategyID)
}

// Suspend flips the strategy to SUSPENDED for compliance misconduct
// (scope breach, stat-manipulation attempt). No new follows afterwards;
// existing follows keep running per spec. The action is audit-logged
// through the admin chain — the audit write failing MUST fail the
// suspension: unaudited compliance actions are worse than retry-able
// errors (§2.7).
func (s *Service) Suspend(ctx context.Context, strategyID, adminUserID int64,
	ip, reason string) (*Strategy, error) {
	if adminUserID <= 0 {
		return nil, errorf(CodeInvalidRequest, "suspension actor required")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, errorf(CodeInvalidRequest, "suspension reason required")
	}
	st, err := s.store.StrategyByID(ctx, strategyID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errorf(CodeNotFound, "strategy %d not found", strategyID)
	}
	// Audit first: if the log write fails the status flip never happens —
	// Compliance Officer retries, never an unaudited suspension.
	if err := s.auditor.LogSuspension(ctx, strategyID, adminUserID, ip, reason); err != nil {
		return nil, errorf(CodeInternalError,
			"audit log failed — strategy %d NOT suspended: %v", strategyID, err)
	}
	if err := s.store.SuspendStrategy(ctx, strategyID, reason); err != nil {
		return nil, err
	}
	return s.store.StrategyByID(ctx, strategyID)
}

// ---------------------------------------------------------------------------
// Discovery — computed-only stats
// ---------------------------------------------------------------------------

// DiscoveryEntry pairs the LISTED strategy with its computed stats.
type DiscoveryEntry struct {
	Strategy
	Stats StrategyStats `json:"stats"`
}

// Discover returns LISTED strategies ranked by computed return. A
// strategy whose fills/rates can't be computed is EXCLUDED (fail closed —
// spec: self-reported or unverifiable performance is never served).
func (s *Service) Discover(ctx context.Context) ([]DiscoveryEntry, error) {
	listed, err := s.store.StrategiesByStatus(ctx, StatusListed)
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveryEntry, 0, len(listed))
	for _, st := range listed {
		fills, err := s.store.ManagerTrades(ctx, st.ManagerAccountID)
		if err != nil {
			return nil, err // store failure degrades the whole listing — fail closed
		}
		cs, err := ComputeStats(ctx, fills, st.Currency, s.rates)
		if err != nil {
			// Uncomputable single strategy → skip it, keep the rest; the
			// exclusion is itself fail-closed (§12.9: computed-only).
			continue
		}
		count, aum, err := s.store.FollowerStats(ctx, st.StrategyID)
		if err != nil {
			return nil, err
		}
		out = append(out, DiscoveryEntry{
			Strategy: st,
			Stats: StrategyStats{
				StrategyID:      st.StrategyID,
				ReturnPct:       cs.ReturnPct(),
				MaxDrawdown:     cs.MaxDrawdown,
				FollowerCount:   count,
				AUM:             aum,
				Currency:        st.Currency,
				IncubatingSince: st.IncubatingSince,
			},
		})
	}
	sortEntries(out)
	return out, nil
}

func sortEntries(e []DiscoveryEntry) {
	ss := make([]StrategyStats, len(e))
	for i := range e {
		ss[i] = e[i].Stats
	}
	SortStrategiesByReturn(ss)
	// re-attach in sorted order by strategy_id key
	byID := make(map[int64]DiscoveryEntry, len(e))
	for _, d := range e {
		byID[d.StrategyID] = d
	}
	for i := range e {
		e[i] = byID[ss[i].StrategyID]
	}
}
