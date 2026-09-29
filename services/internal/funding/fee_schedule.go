// Phase-11 Task 11.3.9 — funding fee schedule engine.
//
// funding_fee_tiers (migration 198) is the versioned schedule keyed by
// (rail, currency, direction, account_tier):
//
//	fee = flat_fee + amount * percentage_bps / 10000, clamped
//	      to [min_fee, max_fee]
//
// '*' is the wildcard for currency and account_tier — resolution prefers
// the exact row over the wildcard row, then the newest effective_date.
// account_tier carries the KYC tier (T0|T1|T2) — free_tier_monthly_count
// is the per-tier monthly allowance ("T2 gets 5 free withdrawals",
// task item 3), enforced via funding_fee_free_usage.
//
// Versions: an update inserts a new row (version+1, supersedes_id → old).
// Superseded rows stay non-retired so a future-dated effective_date
// schedules a change without a fee gap; retired_at marks an explicit
// withdrawal and drops the row out of resolution entirely. Every admin
// mutation writes admin_audit_log + the §5.8 hash-chain link inside the
// same transaction via admin.Log.
//
// Errors (spec §23): FEE_INVALID_INPUT on malformed schedule payloads,
// FEE_TIER_NOT_FOUND when no schedule resolves, FUNDING_RATE_ERROR on a
// computation/store failure, FUNDING_FEE_EXCEEDS_AMOUNT (422) when the
// computed fee ≥ the transaction amount, UNAUTHORIZED_ROLE when the
// caller lacks the Finance Ops binding.
package funding

import (
	"context"
	"fmt"
	"strings"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// CodeFundingFeeExceedsAmount is the §23 422 emitted when a computed
// funding fee would consume (or exceed) the transaction amount.
const CodeFundingFeeExceedsAmount = "FUNDING_FEE_EXCEEDS_AMOUNT"

// FeeTierWildcard matches any currency/account_tier in fee resolution.
const FeeTierWildcard = "*"

// feeDirectionDomain is the funding_fee_tiers direction domain.
var feeDirectionDomain = map[string]bool{"DEPOSIT": true, "WITHDRAWAL": true}

// feeAccountTierDomain is the funding_fee_tiers account_tier domain:
// the KYC tiers (T0|T1|T2, accounts.kyc_tier_enum) plus the wildcard.
var feeAccountTierDomain = map[string]bool{
	FeeTierWildcard: true, "T0": true, "T1": true, "T2": true,
}

// FundingFeeTier is one funding_fee_tiers row (a schedule version).
type FundingFeeTier struct {
	ID                   int64            `json:"id"`
	Rail                 string           `json:"rail"` // bank_method_enum value
	Currency             string           `json:"currency"`
	Direction            string           `json:"direction"`    // DEPOSIT|WITHDRAWAL
	AccountTier          string           `json:"account_tier"` // '*'|T0|T1|T2
	FlatFee              decimal.Decimal  `json:"flat_fee"`
	PercentageBps        decimal.Decimal  `json:"percentage_bps"`
	MinFee               decimal.Decimal  `json:"min_fee"`
	MaxFee               *decimal.Decimal `json:"max_fee"` // nil = uncapped
	FreeTierMonthlyCount int              `json:"free_tier_monthly_count"`
	EffectiveDate        time.Time        `json:"effective_date"`
	Version              int              `json:"version"`
	SupersedesID         *int64           `json:"supersedes_id,omitempty"`
	RetiredAt            *time.Time       `json:"retired_at,omitempty"`
	RetiredBy            *int64           `json:"retired_by,omitempty"`
	CreatedBy            int64            `json:"created_by"`
	CreatedAt            time.Time        `json:"created_at"`
}

// Fee returns the schedule fee for amount: flat + amount×bps/10⁴ clamped
// to [min_fee, max_fee], rounded to the 8dp money quantum.
func (t FundingFeeTier) Fee(amount decimal.Decimal) decimal.Decimal {
	fee := t.FlatFee.Add(amount.Mul(t.PercentageBps).Div(decimal.NewFromInt(10_000)))
	if fee.LessThan(t.MinFee) {
		fee = t.MinFee
	}
	if t.MaxFee != nil && fee.GreaterThan(*t.MaxFee) {
		fee = *t.MaxFee
	}
	return fee.Round(8)
}

// ---------------------------------------------------------------------------
// Admin payloads
// ---------------------------------------------------------------------------

// FeeTierCreate is the POST /api/v1/admin/funding/fees payload.
// flat_fee/percentage_bps/min_fee are decimal strings (explicit values —
// money parameters never silently default).
type FeeTierCreate struct {
	Rail                 string     `json:"rail"`
	Currency             string     `json:"currency"`
	Direction            string     `json:"direction"`
	AccountTier          string     `json:"account_tier"`
	FlatFee              string     `json:"flat_fee"`
	PercentageBps        string     `json:"percentage_bps"`
	MinFee               string     `json:"min_fee"`
	MaxFee               *string    `json:"max_fee"`
	FreeTierMonthlyCount *int       `json:"free_tier_monthly_count"`
	EffectiveDate        *time.Time `json:"effective_date"`
}

// FeeTierUpdate is the PUT …/fees/{id} payload — the successor version's
// complete fee parameter set. Identity fields (rail/currency/direction/
// account_tier) are immutable; retargeting is retire + create.
type FeeTierUpdate struct {
	FlatFee              string     `json:"flat_fee"`
	PercentageBps        string     `json:"percentage_bps"`
	MinFee               string     `json:"min_fee"`
	MaxFee               *string    `json:"max_fee"` // absent/null = uncapped
	FreeTierMonthlyCount *int       `json:"free_tier_monthly_count"`
	EffectiveDate        *time.Time `json:"effective_date"`
}

// FeeTierFilter scopes the admin list.
type FeeTierFilter struct {
	Rail       string
	Currency   string
	Direction  string
	Tier       string
	IncludeAll bool // include superseded + retired versions (default: live only)
	Limit      int
}

func parseFeeAmount(raw, field string) (decimal.Decimal, error) {
	if strings.TrimSpace(raw) == "" {
		return decimal.Zero, errf("FEE_INVALID_INPUT", "%s is required (decimal string)", field)
	}
	d, err := decimal.NewFromString(raw)
	if err != nil || d.IsNegative() {
		return decimal.Zero, errf("FEE_INVALID_INPUT", "%s must be a non-negative decimal string", field)
	}
	if d.Round(8).Compare(d) != 0 {
		return decimal.Zero, errf("FEE_INVALID_INPUT", "%s exceeds the 8dp money quantum", field)
	}
	return d, nil
}

func parseFeeBps(raw string) (decimal.Decimal, error) {
	if strings.TrimSpace(raw) == "" {
		return decimal.Zero, errCode("FEE_INVALID_INPUT", "percentage_bps is required (decimal string)")
	}
	d, err := decimal.NewFromString(raw)
	if err != nil || d.IsNegative() {
		return decimal.Zero, errCode("FEE_INVALID_INPUT", "percentage_bps must be a non-negative decimal string")
	}
	if d.GreaterThan(decimal.NewFromInt(10_000)) {
		return decimal.Zero, errCode("FEE_INVALID_INPUT", "percentage_bps exceeds 10000 (100%)")
	}
	return d, nil
}

// normalizeTierFields validates + normalizes the schedule identity. The
// returned strings are uppercased and domain-checked.
func normalizeTierFields(rail, currency, direction, tier string) (string, string, string, string, error) {
	rail = strings.ToUpper(strings.TrimSpace(rail))
	if !bankMethodDomain[rail] {
		return "", "", "", "", errf("FEE_INVALID_INPUT", "rail %q not in the bank_method_enum domain", rail)
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency != FeeTierWildcard {
		var cerr error
		if currency, cerr = normalizeCurrency(currency); cerr != nil {
			return "", "", "", "", errCode("FEE_INVALID_INPUT", "currency must be ISO 4217 or '*'")
		}
	}
	direction = strings.ToUpper(strings.TrimSpace(direction))
	if !feeDirectionDomain[direction] {
		return "", "", "", "", errf("FEE_INVALID_INPUT", "direction %q must be DEPOSIT|WITHDRAWAL", direction)
	}
	tier = strings.ToUpper(strings.TrimSpace(tier))
	if tier == "" {
		tier = FeeTierWildcard
	}
	if !feeAccountTierDomain[tier] {
		return "", "", "", "", errf("FEE_INVALID_INPUT", "account_tier %q must be '*'|T0|T1|T2", tier)
	}
	return rail, currency, direction, tier, nil
}

// ---------------------------------------------------------------------------
// Fee engine — the seam deposit/withdrawal flows + the estimate endpoint
// consult (task item 1). The engine never fabricates a schedule: no row
// resolves to FEE_TIER_NOT_FOUND, not "free".
// ---------------------------------------------------------------------------

// FeeScheduleStore is the persistence seam for schedule resolution,
// free-tier usage and conversion records. PgFeeScheduleStore
// (fee_schedule_pg.go) is the production implementation; unit tests
// substitute fakes.
type FeeScheduleStore interface {
	// FeeTierAt resolves the applicable schedule row: exact
	// (currency, account_tier) rows win over '*' wildcards, then newest
	// effective_date ≤ at among non-retired versions. (nil, nil) = none.
	FeeTierAt(ctx context.Context, rail, currency, direction, accountTier string,
		at time.Time) (*FundingFeeTier, error)
	// FreeUsageCount reports consumed free movements for the UTC month
	// starting monthStart.
	FreeUsageCount(ctx context.Context, accountID int64, direction string,
		monthStart time.Time) (int, error)
	// RecordFreeUsage consumes one free movement (upsert-increment).
	RecordFreeUsage(ctx context.Context, accountID int64, direction string,
		monthStart time.Time) error
}

// AccountMetaSource resolves the caller's account tier/state
// (funding.Store.AccountMeta on PgStore; the small seam keeps the engine
// unit-testable).
type AccountMetaSource interface {
	AccountMeta(ctx context.Context, id int64) (*AccountMeta, error)
}

// FeeEstimateRequest is the fee-estimate input (account resolved from
// the caller's claims by the handler).
type FeeEstimateRequest struct {
	AccountID int64
	Rail      string
	Currency  string
	Direction string
	Amount    decimal.Decimal
}

// FreeTierStatus reports the monthly free-movement allowance applied to
// the estimate (nil when the schedule grants none).
type FreeTierStatus struct {
	MonthlyAllowance int  `json:"monthly_allowance"`
	Used             int  `json:"used"`
	Remaining        int  `json:"remaining"`
	Applies          bool `json:"applies"` // this movement would be free
}

// FeeEstimate is the POST /api/v1/funding/fee-estimate response.
type FeeEstimate struct {
	Rail         string          `json:"rail"`
	Currency     string          `json:"currency"`
	Direction    string          `json:"direction"`
	AccountID    int64           `json:"account_id"`
	AccountTier  string          `json:"account_tier"`
	Amount       decimal.Decimal `json:"amount"`
	ScheduledFee decimal.Decimal `json:"scheduled_fee"` // schedule formula result
	Fee          decimal.Decimal `json:"fee"`           // charged (0 when free_tier applies)
	NetAmount    decimal.Decimal `json:"net_amount"`    // amount − fee
	FreeTier     *FreeTierStatus `json:"free_tier,omitempty"`
	Tier         *FundingFeeTier `json:"tier"` // resolved schedule row
}

// FeeService is the schedule-consultation engine (task item 1):
// Estimate for the REST surface, Charge for the deposit/withdrawal
// flow-side posting seam (debit customer liability → credit
// funding_fee_revenue, task item 5).
type FeeService struct {
	store    FeeScheduleStore
	accounts AccountMetaSource
	poster   JournalPoster // nil tolerated: Charge then fails closed
	now      func() time.Time
}

// NewFeeService wires the engine. store and accounts are required.
func NewFeeService(store FeeScheduleStore, accounts AccountMetaSource,
	poster JournalPoster) (*FeeService, error) {
	if store == nil || accounts == nil {
		return nil, fmt.Errorf("funding: fee service requires store + account meta")
	}
	return &FeeService{store: store, accounts: accounts, poster: poster,
		now: time.Now}, nil
}

// monthStart returns the first instant of t's UTC calendar month — the
// funding_fee_free_usage period key.
func monthStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// assess resolves the account tier and schedule row and computes the
// fee — shared by Estimate and Charge. Returns the caller's KYC tier, the
// resolved row, the free-tier status and the scheduled (pre-free-tier)
// fee.
func (s *FeeService) assess(ctx context.Context, req FeeEstimateRequest) (*FundingFeeTier, *FreeTierStatus, decimal.Decimal, string, error) {
	rail, currency, direction, _, err := normalizeTierFields(req.Rail, req.Currency, req.Direction, "T0")
	if err != nil {
		return nil, nil, decimal.Zero, "", err
	}
	if currency == FeeTierWildcard {
		return nil, nil, decimal.Zero, "", errCode("FEE_INVALID_INPUT", "currency must be a concrete ISO 4217 code, not '*'")
	}
	if !req.Amount.IsPositive() {
		return nil, nil, decimal.Zero, "", errCode("FEE_INVALID_INPUT", "amount must be positive")
	}
	if req.Amount.Round(8).Compare(req.Amount) != 0 {
		return nil, nil, decimal.Zero, "", errCode("FEE_INVALID_INPUT", "amount exceeds the 8dp money quantum")
	}
	meta, err := s.accounts.AccountMeta(ctx, req.AccountID)
	if err != nil {
		return nil, nil, decimal.Zero, "", err
	}
	now := s.now().UTC()
	tier, err := s.store.FeeTierAt(ctx, rail, currency, direction, meta.KYCTier, now)
	if err != nil {
		return nil, nil, decimal.Zero, "", wrapCode("FUNDING_RATE_ERROR", "fee schedule resolution", err)
	}
	if tier == nil {
		return nil, nil, decimal.Zero, "", errf("FEE_TIER_NOT_FOUND",
			"no funding fee schedule for rail=%s currency=%s direction=%s tier=%s",
			rail, currency, direction, meta.KYCTier)
	}
	scheduled := tier.Fee(req.Amount)

	var free *FreeTierStatus
	if tier.FreeTierMonthlyCount > 0 {
		used, err := s.store.FreeUsageCount(ctx, req.AccountID, direction, monthStart(now))
		if err != nil {
			return nil, nil, decimal.Zero, "", wrapCode("FUNDING_RATE_ERROR", "free-tier usage", err)
		}
		rem := tier.FreeTierMonthlyCount - used
		if rem < 0 {
			rem = 0
		}
		free = &FreeTierStatus{
			MonthlyAllowance: tier.FreeTierMonthlyCount,
			Used:             used,
			Remaining:        rem,
			Applies:          used < tier.FreeTierMonthlyCount,
		}
	}
	return tier, free, scheduled, meta.KYCTier, nil
}

// Estimate implements POST /api/v1/funding/fee-estimate — read-only, no
// usage is consumed.
func (s *FeeService) Estimate(ctx context.Context, req FeeEstimateRequest) (*FeeEstimate, error) {
	tier, free, scheduled, tierName, err := s.assess(ctx, req)
	if err != nil {
		return nil, err
	}
	fee := scheduled
	if free != nil && free.Applies {
		fee = decimal.Zero
	}
	if fee.Compare(req.Amount) >= 0 {
		return nil, errf(CodeFundingFeeExceedsAmount,
			"scheduled fee %s meets or exceeds the transaction amount %s",
			fee.String(), req.Amount.String())
	}
	return &FeeEstimate{
		Rail:         tier.Rail,
		Currency:     tier.Currency,
		Direction:    tier.Direction,
		AccountID:    req.AccountID,
		AccountTier:  tierName,
		Amount:       req.Amount,
		ScheduledFee: scheduled,
		Fee:          fee,
		NetAmount:    req.Amount.Sub(fee),
		FreeTier:     free,
		Tier:         tier,
	}, nil
}

// FeeCharge is one flow-side charge request (withdrawal dispatch or
// deposit credit calling into the engine, task items 1+5).
type FeeCharge struct {
	AccountID   int64
	UserID      int64
	Rail        string
	Currency    string
	Direction   string // DEPOSIT|WITHDRAWAL
	Amount      decimal.Decimal
	FundingTxID int64  // funding_transactions.id the fee attaches to
	Idempotency string // optional journal idempotency suffix
	Description string
}

// FeeAssessment is the charge outcome: what was charged (or waived) and
// the committed journal id.
type FeeAssessment struct {
	Fee          decimal.Decimal `json:"fee"`
	ScheduledFee decimal.Decimal `json:"scheduled_fee"`
	FreeApplied  bool            `json:"free_applied"`
	TierID       int64           `json:"tier_id"`
	JournalID    int64           `json:"journal_id,omitempty"`
}

// Charge assesses the schedule and posts the fee journal:
// DR customer_liability / CR funding_fee_revenue with the wallet
// available-balance effect (task item 5). poster nil fails closed
// SERVICE_DEGRADED — a charged flow never proceeds without the GL write.
// A free-applied movement consumes one free_tier slot and posts nothing.
func (s *FeeService) Charge(ctx context.Context, c FeeCharge) (*FeeAssessment, error) {
	tier, free, scheduled, _, err := s.assess(ctx, FeeEstimateRequest{
		AccountID: c.AccountID, Rail: c.Rail, Currency: c.Currency,
		Direction: c.Direction, Amount: c.Amount,
	})
	if err != nil {
		return nil, err
	}
	fee := scheduled
	freeApplied := free != nil && free.Applies
	if freeApplied {
		fee = decimal.Zero
	}
	if fee.Compare(c.Amount) >= 0 {
		return nil, errf(CodeFundingFeeExceedsAmount,
			"scheduled fee %s meets or exceeds the transaction amount %s",
			fee.String(), c.Amount.String())
	}
	now := s.now().UTC()
	if freeApplied {
		if err := s.store.RecordFreeUsage(ctx, c.AccountID, tier.Direction, monthStart(now)); err != nil {
			return nil, wrapCode("FUNDING_RATE_ERROR", "free-tier consume", err)
		}
		return &FeeAssessment{Fee: fee, ScheduledFee: scheduled,
			FreeApplied: true, TierID: tier.ID}, nil
	}
	if fee.IsZero() {
		return &FeeAssessment{Fee: fee, ScheduledFee: scheduled, TierID: tier.ID}, nil
	}
	if s.poster == nil {
		return nil, errCode("SERVICE_DEGRADED", "ledger poster not wired — fee cannot be charged")
	}
	idem := fmt.Sprintf("funding-fee:%d", c.FundingTxID)
	if c.Idempotency != "" {
		idem += ":" + c.Idempotency
	}
	desc := c.Description
	if desc == "" {
		desc = fmt.Sprintf("funding %s fee %s %s (tier %d)",
			strings.ToLower(tier.Direction), fee, tier.Currency, tier.ID)
	}
	res, perr := s.poster.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryFee,
		ReferenceID:    c.FundingTxID,
		Description:    desc,
		PostedBy:       fmt.Sprintf("user:%d", c.UserID),
		IdempotencyKey: idem,
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(tier.Currency), tier.Currency, fee,
				"funding fee charged to client balance"),
			ledger.CreditLine(ledger.FundingFeeRevenue(tier.Currency), tier.Currency, fee,
				"funding fee revenue (Task 11.3.9)"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      c.AccountID,
			Currency:       tier.Currency,
			AvailableDelta: fee.Neg(),
		}},
	})
	if perr != nil && !res.Committed {
		return nil, perr
	}
	return &FeeAssessment{Fee: fee, ScheduledFee: scheduled,
		TierID: tier.ID, JournalID: res.JournalID}, nil
}

// ---------------------------------------------------------------------------
// Admin service — Finance Ops CRUD over the versioned schedule.
// ---------------------------------------------------------------------------

// RoleResolver resolves an admin user id to its spec §8.2 role name —
// same seam shape as admin.AdminRoleResolver / accounts.RoleResolver.
// A nil resolver fails closed on every gated call.
type RoleResolver func(ctx context.Context, adminUserID int64) (string, error)

// FeeAdminActor is the authenticated administrator performing a schedule
// mutation — mirrors admin.AdminActor without the package edge.
type FeeAdminActor struct {
	UserID   int64
	ClientIP string
}

// feeScheduleRoles may read AND mutate the funding fee schedule — the
// §8.2 role the route registry pins (Finance Ops) plus Super Admin.
var feeScheduleRoles = map[string]bool{
	"Finance Ops": true,
	"Super Admin": true,
}

// FeeScheduleAdminStore extends FeeScheduleStore with the mutation +
// audit seam (PgFeeScheduleStore implements the whole interface).
type FeeScheduleAdminStore interface {
	FeeScheduleStore
	// ListFeeTiers returns schedule rows per the filter. The default
	// view is resolution-relevant rows (retired_at IS NULL);
	// IncludeAll adds superseded/retired history.
	ListFeeTiers(ctx context.Context, f FeeTierFilter) ([]FundingFeeTier, error)
	// FeeTierByID returns (nil, nil) for an unknown id.
	FeeTierByID(ctx context.Context, id int64) (*FundingFeeTier, error)
	// FeeTierVersionChain returns every version of the row's
	// (rail, currency, direction, account_tier) group, newest first.
	FeeTierVersionChain(ctx context.Context, id int64) ([]FundingFeeTier, error)
	// CreateFeeTier inserts version 1 of a group + the audit row in one
	// transaction. A (group, effective_date) collision is a coded
	// FEE_INVALID_INPUT conflict.
	CreateFeeTier(ctx context.Context, t FundingFeeTier, actor FeeAdminActor) (*FundingFeeTier, error)
	// CreateFeeTierVersion inserts the successor version (version+1,
	// supersedes_id = id) under the same group + audit row atomically.
	CreateFeeTierVersion(ctx context.Context, id int64, t FundingFeeTier, actor FeeAdminActor) (*FundingFeeTier, error)
	// RetireFeeTier sets retired_at/retired_by — the row leaves
	// resolution but stays on record. (nil, nil) when unknown.
	RetireFeeTier(ctx context.Context, id int64, actor FeeAdminActor) (*FundingFeeTier, error)
}

// FeeScheduleService is the Finance Ops CRUD surface. Role resolution
// runs inside the service (defense in depth — identical to admin.LPService):
// the route's Finance Ops pin and this check must both pass.
type FeeScheduleService struct {
	store    FeeScheduleAdminStore
	resolver RoleResolver
}

// NewFeeScheduleService wires the admin service; store is required, a
// nil resolver fails closed.
func NewFeeScheduleService(store FeeScheduleAdminStore, resolver RoleResolver) (*FeeScheduleService, error) {
	if store == nil {
		return nil, fmt.Errorf("funding: fee schedule service requires a store")
	}
	return &FeeScheduleService{store: store, resolver: resolver}, nil
}

// requireFeeRole resolves the actor's role and enforces the Finance Ops /
// Super Admin gate. Fail closed: nil resolver or lookup error →
// UNAUTHORIZED_ROLE.
func (s *FeeScheduleService) requireFeeRole(ctx context.Context, actor FeeAdminActor) error {
	if s.resolver == nil {
		return errCode("UNAUTHORIZED_ROLE", "role resolver not wired (fail closed)")
	}
	role, err := s.resolver(ctx, actor.UserID)
	if err != nil {
		return wrapCode("UNAUTHORIZED_ROLE", "role resolution", err)
	}
	if !feeScheduleRoles[role] {
		return errf("UNAUTHORIZED_ROLE", "role %q cannot administer the funding fee schedule", role)
	}
	return nil
}

// List serves GET /api/v1/admin/funding/fees.
func (s *FeeScheduleService) List(ctx context.Context, actor FeeAdminActor,
	f FeeTierFilter) ([]FundingFeeTier, error) {
	if err := s.requireFeeRole(ctx, actor); err != nil {
		return nil, err
	}
	rows, err := s.store.ListFeeTiers(ctx, f)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []FundingFeeTier{}
	}
	return rows, nil
}

// Get serves GET /api/v1/admin/funding/fees/{id}.
func (s *FeeScheduleService) Get(ctx context.Context, actor FeeAdminActor,
	id int64) (*FundingFeeTier, error) {
	if err := s.requireFeeRole(ctx, actor); err != nil {
		return nil, err
	}
	t, err := s.store.FeeTierByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errf("FEE_TIER_NOT_FOUND", "funding fee schedule %d not found", id)
	}
	return t, nil
}

// Versions serves GET /api/v1/admin/funding/fees/{id}/versions — the full
// version chain of the row's schedule group.
func (s *FeeScheduleService) Versions(ctx context.Context, actor FeeAdminActor,
	id int64) ([]FundingFeeTier, error) {
	if err := s.requireFeeRole(ctx, actor); err != nil {
		return nil, err
	}
	rows, err := s.store.FeeTierVersionChain(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errf("FEE_TIER_NOT_FOUND", "funding fee schedule %d not found", id)
	}
	return rows, nil
}

// Create serves POST /api/v1/admin/funding/fees.
func (s *FeeScheduleService) Create(ctx context.Context, actor FeeAdminActor,
	req FeeTierCreate) (*FundingFeeTier, error) {
	if err := s.requireFeeRole(ctx, actor); err != nil {
		return nil, err
	}
	t, err := buildFeeTier(req, 0, 1)
	if err != nil {
		return nil, err
	}
	t.CreatedBy = actor.UserID
	return s.store.CreateFeeTier(ctx, *t, actor)
}

// Update serves PUT /api/v1/admin/funding/fees/{id} — inserts the
// successor version; the prior row stays in the version chain.
func (s *FeeScheduleService) Update(ctx context.Context, actor FeeAdminActor,
	id int64, req FeeTierUpdate) (*FundingFeeTier, error) {
	if err := s.requireFeeRole(ctx, actor); err != nil {
		return nil, err
	}
	cur, err := s.store.FeeTierByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, errf("FEE_TIER_NOT_FOUND", "funding fee schedule %d not found", id)
	}
	if cur.RetiredAt != nil {
		return nil, errf("FEE_INVALID_INPUT",
			"funding fee schedule %d is retired — create a new version of a live row or a new schedule", id)
	}
	c := FeeTierCreate{
		Rail:          cur.Rail,
		Currency:      cur.Currency,
		Direction:     cur.Direction,
		AccountTier:   cur.AccountTier,
		FlatFee:       req.FlatFee,
		PercentageBps: req.PercentageBps,
		MinFee:        req.MinFee,
		MaxFee:        req.MaxFee,
	}
	if req.FreeTierMonthlyCount != nil {
		c.FreeTierMonthlyCount = req.FreeTierMonthlyCount
	} else {
		n := cur.FreeTierMonthlyCount
		c.FreeTierMonthlyCount = &n
	}
	if req.EffectiveDate != nil {
		c.EffectiveDate = req.EffectiveDate
	}
	t, err := buildFeeTier(c, id, cur.Version+1)
	if err != nil {
		return nil, err
	}
	t.CreatedBy = actor.UserID
	return s.store.CreateFeeTierVersion(ctx, id, *t, actor)
}

// Retire serves DELETE /api/v1/admin/funding/fees/{id} — the row leaves
// resolution (retired_at set) but stays on record for audit.
func (s *FeeScheduleService) Retire(ctx context.Context, actor FeeAdminActor,
	id int64) (*FundingFeeTier, error) {
	if err := s.requireFeeRole(ctx, actor); err != nil {
		return nil, err
	}
	t, err := s.store.RetireFeeTier(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errf("FEE_TIER_NOT_FOUND", "funding fee schedule %d not found", id)
	}
	return t, nil
}

// buildFeeTier validates a create/update payload into a row. version and
// supersedes come from the caller (1/0 for create; n+1/id for update).
func buildFeeTier(req FeeTierCreate, supersedesID int64, version int) (*FundingFeeTier, error) {
	rail, ccy, dir, tier, err := normalizeTierFields(req.Rail, req.Currency,
		req.Direction, req.AccountTier)
	if err != nil {
		return nil, err
	}
	flat, err := parseFeeAmount(req.FlatFee, "flat_fee")
	if err != nil {
		return nil, err
	}
	bps, err := parseFeeBps(req.PercentageBps)
	if err != nil {
		return nil, err
	}
	minFee, err := parseFeeAmount(req.MinFee, "min_fee")
	if err != nil {
		return nil, err
	}
	var maxFee *decimal.Decimal
	if req.MaxFee != nil && strings.TrimSpace(*req.MaxFee) != "" {
		d, err := parseFeeAmount(*req.MaxFee, "max_fee")
		if err != nil {
			return nil, err
		}
		if d.LessThan(minFee) {
			return nil, errf("FEE_INVALID_INPUT",
				"max_fee %s below min_fee %s", d.String(), minFee.String())
		}
		maxFee = &d
	}
	free := 0
	if req.FreeTierMonthlyCount != nil {
		free = *req.FreeTierMonthlyCount
		if free < 0 {
			return nil, errCode("FEE_INVALID_INPUT", "free_tier_monthly_count must be >= 0")
		}
	}
	eff := time.Now().UTC()
	if req.EffectiveDate != nil {
		eff = req.EffectiveDate.UTC()
	}
	t := &FundingFeeTier{
		Rail: rail, Currency: ccy, Direction: dir, AccountTier: tier,
		FlatFee: flat, PercentageBps: bps, MinFee: minFee, MaxFee: maxFee,
		FreeTierMonthlyCount: free, EffectiveDate: eff, Version: version,
	}
	if supersedesID > 0 {
		t.SupersedesID = &supersedesID
	}
	return t, nil
}
