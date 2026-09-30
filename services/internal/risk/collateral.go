// collateral.go — collateral eligibility, haircuts & concentration limits
// (Phase-19 Task 19.3.8; spec §5.24, §13.6b, §24 #145).
//
// DEVIATION RECORDED: the phase plan lists
// services/internal/margin/collateral.go — superseded: Phase-19 margin
// code lives in internal/risk (same package as MarginService, which is
// the sole consumer seam).
//
// The CollateralService computes the §13.6b haircut-adjusted collateral
// equity that REPLACES the face-value balance leg inside
// MarginService.evaluate (wired via MarginOptions.Collateral):
//
//		equity_collateral = Σ eligible(balance_ccy × (1 − haircut_pct/100) × rate_to_usd)
//
//	  - a currency absent from collateral_schedule (migration 041) or with
//	    eligible=false contributes ZERO — fail-closed on unknown assets
//	    (migration comment: "the CollateralValuator fails closed on
//	    unknown/ineligible entries");
//	  - a currency with no USD conversion rate contributes zero and is
//	    disclosed (conservative under-valuation, §2.7 — never inflated);
//	  - DEBIT balances (total ≤ 0) are valued at FACE — a liability is not
//	    collateral; haircutting a debt would understate it;
//	  - CONCENTRATION: a single non-USD currency's adjusted value may not
//	    exceed max_concentration_pct of the account's gross adjusted
//	    collateral equity; the excess is valued at zero for margining but
//	    remains withdrawable (disclosed via CollateralValuation.Concentrated);
//	  - USD is exempt from concentration (it is the §13.1 numeraire).
//
// Admin seam (PUT /api/v1/admin/collateral-schedule is wired by the
// route cluster): UpdateSchedule validates + persists rows and the
// admin_audit_log entry in ONE transaction; the schedule cache TTL is
// 5s so edits apply on the next equity recompute within the §5s bound.
package risk

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// collateralScheduleTTL bounds schedule-change propagation: edits apply
// on the next equity recompute within 5s (Task 19.3.8 DoD).
const collateralScheduleTTL = 5 * time.Second

// collateralCcyRe is the migration-041 CHECK constraint
// (currency ~ '^[A-Z]{3}$') mirrored for fail-fast validation.
var collateralCcyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// ---------------------------------------------------------------------------
// Schedule types + persistence seam
// ---------------------------------------------------------------------------

// CollateralScheduleEntry is one collateral_schedule row (migration 041).
// HaircutPct is the PERCENTAGE DEDUCTED (5.00 ⇒ 95% counts toward
// equity); MaxConcentrationPct bounds the share of account collateral
// equity a non-USD currency may supply.
type CollateralScheduleEntry struct {
	Currency            string
	Eligible            bool
	HaircutPct          decimal.Decimal
	MaxConcentrationPct decimal.Decimal
}

// CollateralScheduleStore is the Postgres seam for collateral_schedule;
// PgCollateralScheduleStore implements it — tests substitute a fake.
type CollateralScheduleStore interface {
	// CollateralSchedule returns every schedule row.
	CollateralSchedule(ctx context.Context) ([]CollateralScheduleEntry, error)
	// PutCollateralSchedule upserts the given rows and appends the
	// admin_audit_log entry (before/after images) in ONE transaction.
	PutCollateralSchedule(ctx context.Context, actor int64,
		rows []CollateralScheduleEntry, before []CollateralScheduleEntry) error
}

// ---------------------------------------------------------------------------
// Valuation contract — the MarginService seam
// ---------------------------------------------------------------------------

// CollateralValuation is one account's adjusted collateral result.
type CollateralValuation struct {
	// EquityUSD is the haircut + concentration adjusted collateral
	// equity (USD numeraire) — the value MarginService adds to equity.
	EquityUSD decimal.Decimal
	// Unpriced lists eligible currencies with no usable USD rate —
	// conservative zero contribution, disclosed (§2.7).
	Unpriced []string
	// Ineligible lists currencies contributing zero because the
	// schedule has no eligible row for them (absent or eligible=false).
	Ineligible []string
	// Concentrated lists non-USD currencies whose adjusted value
	// exceeded max_concentration_pct — the excess was zero-weighted
	// (still withdrawable; affects margin equity only).
	Concentrated []string
}

// CollateralValuator is the Task-19.3.8 seam MarginService consults for
// the balance leg of equity. rateToUSD is supplied by the caller (it
// shares the evaluation's batch-fetched marks — no N+1).
type CollateralValuator interface {
	Valuate(ctx context.Context, balances []BalanceAmount,
		rateToUSD func(ccy string) (decimal.Decimal, bool)) (*CollateralValuation, error)
}

// ---------------------------------------------------------------------------
// CollateralService — valuator + schedule admin surface
// ---------------------------------------------------------------------------

// CollateralService implements CollateralValuator over the migration-041
// schedule and owns the admin read/write surface. The schedule is cached
// for collateralScheduleTTL (5s propagation bound); a refresh failure
// with a warm cache falls back to the last-known schedule and pages ops
// (stale haircuts still bound equity — never inflated), while a cold
// failure returns the error (fail-closed: an unpriceable schedule must
// not fabricate eligibility).
type CollateralService struct {
	store   CollateralScheduleStore
	alerter OpsAlerter
	now     func() time.Time
	logf    func(format string, args ...any)

	mu      sync.Mutex
	cache   map[string]CollateralScheduleEntry
	cacheAt time.Time
}

// CollateralDeps wires the service. Store is required.
type CollateralDeps struct {
	Store   CollateralScheduleStore
	Alerter OpsAlerter
	Now     func() time.Time
	Logf    func(format string, args ...any)
}

// NewCollateralService builds the service.
func NewCollateralService(d CollateralDeps) (*CollateralService, error) {
	if d.Store == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "collateral: schedule store is nil")
	}
	s := &CollateralService{store: d.Store, alerter: d.Alerter,
		now: d.Now, logf: d.Logf}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// scheduleMap returns the cached schedule, refreshing past the TTL.
// Stale-cache fallback on refresh failure keeps the last-known haircuts
// binding (conservative) and raises a P1 page; a cold read failure is
// returned so the margin evaluation aborts instead of guessing.
func (s *CollateralService) scheduleMap(ctx context.Context) (map[string]CollateralScheduleEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil && s.now().Sub(s.cacheAt) < collateralScheduleTTL {
		return s.cache, nil
	}
	rows, err := s.store.CollateralSchedule(ctx)
	if err != nil {
		if s.cache != nil {
			s.raiseAlert(SeverityP1, "COLLATERAL_SCHEDULE_READ_DEGRADED",
				fmt.Sprintf("collateral schedule refresh failed; serving cached schedule (%d entries): %v",
					len(s.cache), err))
			return s.cache, nil
		}
		return nil, fmt.Errorf("collateral schedule read: %w", err)
	}
	m := make(map[string]CollateralScheduleEntry, len(rows))
	for _, r := range rows {
		m[r.Currency] = r
	}
	s.cache = m
	s.cacheAt = s.now()
	return m, nil
}

// Valuate implements CollateralValuator — the §13.6b adjusted equity.
func (s *CollateralService) Valuate(ctx context.Context, balances []BalanceAmount,
	rateToUSD func(ccy string) (decimal.Decimal, bool)) (*CollateralValuation, error) {

	sched, err := s.scheduleMap(ctx)
	if err != nil {
		return nil, err
	}
	v := &CollateralValuation{}
	type leg struct {
		ccy string
		adj decimal.Decimal // haircut-adjusted USD value
	}
	var legs []leg
	for _, b := range balances {
		total := b.Total()
		if !total.IsPositive() {
			// Liabilities are not collateral — face value, no haircut.
			if total.IsNegative() {
				r, ok := rateToUSD(b.Currency)
				if !ok {
					v.Unpriced = append(v.Unpriced, b.Currency)
					continue
				}
				v.EquityUSD = v.EquityUSD.Add(total.Mul(r))
			}
			continue
		}
		ent, ok := sched[b.Currency]
		if !ok || !ent.Eligible {
			v.Ineligible = append(v.Ineligible, b.Currency)
			continue
		}
		r, ok := rateToUSD(b.Currency)
		if !ok || !r.IsPositive() {
			// Stale/missing oracle rate ⇒ conservative zero — disclosed,
			// never inflated (§2.7 stale-oracle rule).
			v.Unpriced = append(v.Unpriced, b.Currency)
			continue
		}
		haircut := decimal.One.Sub(ent.HaircutPct.Div(decimal.NewFromInt(100)))
		legs = append(legs, leg{ccy: b.Currency, adj: total.Mul(r).Mul(haircut)})
	}

	// Gross adjusted collateral equity is the concentration denominator.
	gross := decimal.Zero
	for _, l := range legs {
		gross = gross.Add(l.adj)
	}
	hundred := decimal.NewFromInt(100)
	for i, l := range legs {
		contribution := l.adj
		if l.ccy != "USD" {
			ent := sched[l.ccy]
			allowed := gross.Mul(ent.MaxConcentrationPct).Div(hundred)
			if l.adj.GreaterThan(allowed) {
				contribution = allowed
				v.Concentrated = append(v.Concentrated, l.ccy)
				legs[i].adj = allowed
			}
		}
		v.EquityUSD = v.EquityUSD.Add(contribution.Round(8))
	}
	sort.Strings(v.Unpriced)
	sort.Strings(v.Ineligible)
	sort.Strings(v.Concentrated)
	return v, nil
}

// ---------------------------------------------------------------------------
// Admin surface — PUT /api/v1/admin/collateral-schedule backing
// ---------------------------------------------------------------------------

// Schedule returns the live schedule for the admin read surface —
// always straight from the store (never the cache: an admin must see
// durable state, not a ≤5s-old image).
func (s *CollateralService) Schedule(ctx context.Context) ([]CollateralScheduleEntry, error) {
	rows, err := s.store.CollateralSchedule(ctx)
	if err != nil {
		return nil, fmt.Errorf("collateral schedule read: %w", err)
	}
	return rows, nil
}

// EligibleCurrencies lists the schedule's eligible non-USD currencies —
// the collateral monitor's subscription universe (Task 19.3.28).
func (s *CollateralService) EligibleCurrencies(ctx context.Context) ([]string, error) {
	rows, err := s.store.CollateralSchedule(ctx)
	if err != nil {
		return nil, fmt.Errorf("collateral schedule read: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Eligible && r.Currency != "USD" {
			out = append(out, r.Currency)
		}
	}
	sort.Strings(out)
	return out, nil
}

// UpdateSchedule validates and persists schedule rows (upsert per
// currency — unlisted currencies keep their rows), writes the
// admin_audit_log entry in the same transaction, and invalidates the
// cache so the change applies on the next equity recompute (≤5s).
// actor is the admin user id (Risk Manager+ per the task); actor <= 0
// rejects — a schedule change must always be attributable.
func (s *CollateralService) UpdateSchedule(ctx context.Context, actor int64,
	rows []CollateralScheduleEntry) error {

	if actor <= 0 {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"collateral schedule update requires an actor")
	}
	if len(rows) == 0 {
		return excerrors.New(CodeInvalidRequest,
			"collateral schedule update: empty row set")
	}
	hundred := decimal.NewFromInt(100)
	seen := map[string]bool{}
	for _, r := range rows {
		if !collateralCcyRe.MatchString(r.Currency) {
			return excerrors.New(CodeInvalidRequest,
				fmt.Sprintf("collateral schedule: bad currency %q (want ^[A-Z]{3}$)", r.Currency))
		}
		if seen[r.Currency] {
			return excerrors.New(CodeInvalidRequest,
				fmt.Sprintf("collateral schedule: duplicate currency %s", r.Currency))
		}
		seen[r.Currency] = true
		if r.HaircutPct.IsNegative() || r.HaircutPct.GreaterThan(hundred) {
			return excerrors.New(CodeInvalidRequest,
				fmt.Sprintf("collateral schedule: haircut_pct %s outside [0,100] for %s",
					r.HaircutPct, r.Currency))
		}
		if !r.MaxConcentrationPct.IsPositive() || r.MaxConcentrationPct.GreaterThan(hundred) {
			return excerrors.New(CodeInvalidRequest,
				fmt.Sprintf("collateral schedule: max_concentration_pct %s outside (0,100] for %s",
					r.MaxConcentrationPct, r.Currency))
		}
	}
	before, err := s.store.CollateralSchedule(ctx)
	if err != nil {
		return fmt.Errorf("collateral schedule before-image read: %w", err)
	}
	if err := s.store.PutCollateralSchedule(ctx, actor, rows, before); err != nil {
		return fmt.Errorf("collateral schedule update: %w", err)
	}
	// Invalidate: zero the timestamp so the next Valuate refreshes while
	// the last-known schedule remains the degraded-read fallback.
	s.mu.Lock()
	s.cacheAt = time.Time{}
	s.mu.Unlock()
	return nil
}

// raiseAlert pages ops on degraded collateral state (detached ctx like
// the sibling services — an alert must outlive the request ctx).
func (s *CollateralService) raiseAlert(severity, code, summary string) {
	if s.alerter == nil {
		s.logf("collateral: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary,
	}); err != nil {
		s.logf("collateral: alert %s dispatch failed: %v", code, err)
	}
}

// ---------------------------------------------------------------------------
// PgCollateralScheduleStore
// ---------------------------------------------------------------------------

// PgCollateralScheduleStore implements CollateralScheduleStore over pgx.
type PgCollateralScheduleStore struct {
	Pool *pgxpool.Pool
}

// NewPgCollateralScheduleStore binds the pool.
func NewPgCollateralScheduleStore(pool *pgxpool.Pool) (*PgCollateralScheduleStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("collateral store: nil pgx pool")
	}
	return &PgCollateralScheduleStore{Pool: pool}, nil
}

// CollateralSchedule implements CollateralScheduleStore.
func (s *PgCollateralScheduleStore) CollateralSchedule(ctx context.Context) ([]CollateralScheduleEntry, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT currency, eligible, haircut_pct::text, max_concentration_pct::text
		  FROM collateral_schedule ORDER BY currency`)
	if err != nil {
		return nil, fmt.Errorf("collateral schedule query: %w", err)
	}
	defer rows.Close()
	var out []CollateralScheduleEntry
	for rows.Next() {
		var e CollateralScheduleEntry
		var h, c string
		if err := rows.Scan(&e.Currency, &e.Eligible, &h, &c); err != nil {
			return nil, fmt.Errorf("collateral schedule scan: %w", err)
		}
		if e.HaircutPct, err = decimal.NewFromString(h); err != nil {
			return nil, fmt.Errorf("collateral schedule haircut %q: %w", h, err)
		}
		if e.MaxConcentrationPct, err = decimal.NewFromString(c); err != nil {
			return nil, fmt.Errorf("collateral schedule concentration %q: %w", c, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PutCollateralSchedule implements CollateralScheduleStore: per-currency
// upserts + the admin_audit_log row commit or abort together (the
// §13.6b "audit-logged" requirement — same tx pattern as
// PgMarginThresholdStore.SetThresholds).
func (s *PgCollateralScheduleStore) PutCollateralSchedule(ctx context.Context, actor int64,
	rows []CollateralScheduleEntry, before []CollateralScheduleEntry) error {

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO collateral_schedule (currency, eligible, haircut_pct, max_concentration_pct)
			VALUES ($1, $2, $3::numeric, $4::numeric)
			ON CONFLICT (currency) DO UPDATE
			  SET eligible = EXCLUDED.eligible,
			      haircut_pct = EXCLUDED.haircut_pct,
			      max_concentration_pct = EXCLUDED.max_concentration_pct,
			      updated_at = now()`,
			r.Currency, r.Eligible, r.HaircutPct.String(), r.MaxConcentrationPct.String()); err != nil {
			return fmt.Errorf("collateral schedule upsert %s: %w", r.Currency, err)
		}
	}
	beforeJSON, err := scheduleJSON(before)
	if err != nil {
		return err
	}
	afterJSON, err := scheduleJSON(rows)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log (admin_user_id, action, target_type, target_id,
		                             before_state, after_state)
		VALUES ($1, 'risk.collateral_schedule.update', 'collateral_schedule', 0,
		        $2::jsonb, $3::jsonb)`,
		actor, beforeJSON, afterJSON); err != nil {
		return fmt.Errorf("collateral schedule audit: %w", err)
	}
	return tx.Commit(ctx)
}

// scheduleJSON serializes a schedule slice for the audit images.
func scheduleJSON(rows []CollateralScheduleEntry) (string, error) {
	out := "["
	for i, r := range rows {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(`{"currency":%q,"eligible":%t,"haircut_pct":%q,"max_concentration_pct":%q}`,
			r.Currency, r.Eligible, r.HaircutPct.String(), r.MaxConcentrationPct.String())
	}
	return out + "]", nil
}

// compile-time seam assertions.
var (
	_ CollateralValuator      = (*CollateralService)(nil)
	_ CollateralScheduleStore = (*PgCollateralScheduleStore)(nil)
)
