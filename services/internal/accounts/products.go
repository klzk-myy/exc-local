// products.go — Account Product Profiles (Phase-14 Task 14.3.13;
// spec §5.41, §24 #369; migration 095).
//
// Every account resolves to a product profile (accounts.product_profile_id;
// NULL = the STANDARD seed per spec §5.2). The profile owns:
//
//   - pricing_plan  — the single fee-model source consumed by the Phase-03
//     Task 3.3.13 commission engine (PgProfileFeeModelSource reads the same
//     join; there is no account-level override).
//   - instrument_scope — allowlisted instrument classes the order gate
//     enforces (ProductGateService in product_gate.go).
//   - subunit_divisor — Task 3.3.21 minor-unit ledger convention
//     (1 STANDARD / 100 CENT); STANDARD↔CENT switches additionally require
//     every balance row to be zero (ledger.ValidateProfileSwitch,
//     migration 096 convention).
//   - min_deposit   — surfaced to funding/UI; enforced by the funding
//     rails where applicable.
//
// Admin surface: profile create/update (pricing/scope/divisor changes)
// run through the admin.DualControlService maker-checker queue
// (OpProductProfileChange) — ApplyCreateTx/ApplyUpdateTx are the
// in-transaction executors; every mutation also writes admin_audit_log
// inside the same transaction. Account assignment (AssignProfile) is a
// single-approver Compliance-Officer action, audit-logged, and rejected
// INVALID_REQUEST while the account has open exposure (open position,
// resting order or pending settlement — the same read path the Task
// 14.3.9 closure preconditions use) or, for divisor-changing switches,
// while any balance is non-zero.
package accounts

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/admin"
	"exchange/internal/ledger"
)

// Profile lifecycle (migration 095 CHECK).
const (
	ProfileStatusActive  = "ACTIVE"
	ProfileStatusRetired = "RETIRED"
)

// Pricing plans (mirrors settlement.FeeModel; §5.41).
const (
	PricingSpreadMarkup       = "SPREAD_MARKUP"
	PricingRawSpreadCommision = "RAW_SPREAD_COMMISSION"
)

// InstrumentClasses is the §24 #152 instrument-class axis — the domain
// instrument_scope, positive_classes and negative_classes draw from.
var InstrumentClasses = map[string]bool{
	"SPOT": true, "FORWARD": true, "SWAP": true, "NDF": true, "OPTION": true,
}

// canonicalClasses renders a class set in deterministic order.
func canonicalClasses(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if !InstrumentClasses[c] {
			return nil, errorf(CodeInvalidRequest,
				"instrument class %q not in SPOT|FORWARD|SWAP|NDF|OPTION", c)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ProductProfile is one account_product_profiles row.
type ProductProfile struct {
	ProfileID       int64           `json:"profile_id"`
	Code            string          `json:"code"`
	PricingPlan     string          `json:"pricing_plan"`
	InstrumentScope []string        `json:"instrument_scope"`
	SubunitDivisor  int64           `json:"subunit_divisor"`
	MinDeposit      decimal.Decimal `json:"min_deposit"`
	Status          string          `json:"status"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// InScope reports whether the instrument class is allowlisted.
func (p *ProductProfile) InScope(class string) bool {
	class = strings.ToUpper(strings.TrimSpace(class))
	for _, c := range p.InstrumentScope {
		if c == class {
			return true
		}
	}
	return false
}

// ProfileInput carries the mutable profile fields for create/update.
type ProfileInput struct {
	Code            string   `json:"code"`
	PricingPlan     string   `json:"pricing_plan"`
	InstrumentScope []string `json:"instrument_scope"`
	SubunitDivisor  int64    `json:"subunit_divisor"`
	MinDeposit      string   `json:"min_deposit"` // decimal string
	Status          string   `json:"status"`      // update only: ACTIVE|RETIRED
}

// validate checks the input domain (service-side mirror of the CHECKs).
func (in *ProfileInput) validate(forUpdate bool) error {
	if !forUpdate {
		in.Code = strings.ToUpper(strings.TrimSpace(in.Code))
		if in.Code == "" || len(in.Code) > 32 {
			return newError(CodeInvalidRequest, "code required (≤32 chars)")
		}
	}
	in.PricingPlan = strings.ToUpper(strings.TrimSpace(in.PricingPlan))
	if in.PricingPlan != PricingSpreadMarkup && in.PricingPlan != PricingRawSpreadCommision {
		return errorf(CodeInvalidRequest, "pricing_plan must be %s or %s",
			PricingSpreadMarkup, PricingRawSpreadCommision)
	}
	scope, err := canonicalClasses(in.InstrumentScope)
	if err != nil {
		return err
	}
	if len(scope) == 0 {
		return newError(CodeInvalidRequest,
			"instrument_scope must allowlist at least one class")
	}
	in.InstrumentScope = scope
	if in.SubunitDivisor == 0 {
		in.SubunitDivisor = 1
	}
	if !ledger.ValidDivisor(in.SubunitDivisor) {
		return errorf(CodeInvalidRequest, "subunit_divisor must be 1 or 100")
	}
	if in.MinDeposit == "" {
		in.MinDeposit = "0"
	}
	md, err := decimal.NewFromString(strings.TrimSpace(in.MinDeposit))
	if err != nil || md.IsNegative() {
		return newError(CodeInvalidRequest, "min_deposit must be a non-negative decimal")
	}
	in.MinDeposit = md.String()
	if forUpdate && in.Status != "" {
		in.Status = strings.ToUpper(strings.TrimSpace(in.Status))
		if in.Status != ProfileStatusActive && in.Status != ProfileStatusRetired {
			return errorf(CodeInvalidRequest, "status must be %s or %s",
				ProfileStatusActive, ProfileStatusRetired)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// ProfileService owns profile reads, the admin CRUD executors and the
// guarded account→profile assignment.
type ProfileService struct {
	pool     *pgxpool.Pool
	resolver RoleResolver
	now      func() time.Time
}

// NewProfileService wires the service. resolver may be nil — privileged
// calls then fail closed UNAUTHORIZED_ROLE while reads keep working.
func NewProfileService(pool *pgxpool.Pool, resolver RoleResolver) *ProfileService {
	return &ProfileService{pool: pool, resolver: resolver, now: time.Now}
}

// profileAdminRoles may define profiles and assign them (spec §8.2; the
// route registry pins RoleComplianceOfficer — Super Admin inherits).
var profileAdminRoles = map[string]bool{
	RoleComplianceOfficer: true,
	RoleSuperAdmin:        true,
}

func (s *ProfileService) requireRole(ctx context.Context, adminUserID int64) error {
	if s.resolver == nil {
		return newError(CodeUnauthorizedRole,
			"role resolver not configured — profile administration rejected")
	}
	role, err := s.resolver(ctx, adminUserID)
	if err != nil {
		return errorf("INTERNAL_ERROR", "role lookup: %v", err)
	}
	if !profileAdminRoles[role] {
		return newError(CodeUnauthorizedRole,
			"product-profile administration requires Compliance Officer or Super Admin")
	}
	return nil
}

// ProfileForAccount implements ledger.ProductProfileProvider — NULL
// product_profile_id resolves to the STANDARD seed (spec §5.2:
// NULL = default venue profile), so every pre-095 and unassigned
// account prices/gates as STANDARD without a data backfill.
func (s *ProfileService) ProfileForAccount(ctx context.Context, accountID int64) (ledger.ProductProfile, error) {
	p, err := s.AccountProfile(ctx, accountID)
	if err != nil {
		return ledger.ProductProfile{}, err
	}
	return ledger.ProductProfile{
		ProfileID:      p.ProfileID,
		Code:           p.Code,
		PricingPlan:    p.PricingPlan,
		SubunitDivisor: p.SubunitDivisor,
		Status:         p.Status,
	}, nil
}

// AccountProfile resolves the account's effective profile (full row
// including instrument_scope). Unknown account → ACCOUNT_NOT_FOUND.
func (s *ProfileService) AccountProfile(ctx context.Context, accountID int64) (*ProductProfile, error) {
	return profileForAccount(ctx, s.pool, accountID)
}

// profileForAccount is the tx-or-pool shared read.
func profileForAccount(ctx context.Context, q querier, accountID int64) (*ProductProfile, error) {
	var (
		p     ProductProfile
		min   string
		scope []string
		found bool
	)
	err := q.QueryRow(ctx, `
		SELECT p.profile_id, p.code, p.pricing_plan, p.instrument_scope,
		       p.subunit_divisor, p.min_deposit::text, p.status,
		       p.created_at, p.updated_at, TRUE
		  FROM accounts a
		  JOIN account_product_profiles p
		    ON p.profile_id = a.product_profile_id
		 WHERE a.id = $1`, accountID).
		Scan(&p.ProfileID, &p.Code, &p.PricingPlan, &scope,
			&p.SubunitDivisor, &min, &p.Status, &p.CreatedAt, &p.UpdatedAt, &found)
	if err == pgx.ErrNoRows {
		// NULL product_profile_id or dangling FK → STANDARD default,
		// but only when the account itself exists.
		var exists bool
		if err := q.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`,
			accountID).Scan(&exists); err != nil {
			return nil, errorf("INTERNAL_ERROR", "account probe: %v", err)
		}
		if !exists {
			return nil, errorf("ACCOUNT_NOT_FOUND", "account %d not found", accountID)
		}
		std, err := profileByCode(ctx, q, ledger.ProfileCodeStandard)
		if err != nil {
			return nil, err
		}
		if std == nil {
			// Pre-095 seed missing — static fallback, still a valid
			// STANDARD profile (divisor 1 / SPREAD_MARKUP).
			return &ProductProfile{
				Code:            ledger.ProfileCodeStandard,
				PricingPlan:     ledger.StandardProfile.PricingPlan,
				InstrumentScope: []string{"SPOT", "FORWARD", "SWAP", "NDF", "OPTION"},
				SubunitDivisor:  ledger.SubunitDivisorStandard,
				MinDeposit:      decimal.Zero,
				Status:          ProfileStatusActive,
			}, nil
		}
		return std, nil
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "profile read: %v", err)
	}
	p.InstrumentScope = scope
	if p.MinDeposit, err = decimal.NewFromString(min); err != nil {
		return nil, errorf("INTERNAL_ERROR", "min_deposit parse: %v", err)
	}
	return &p, nil
}

// querier abstracts pool/tx reads.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func profileByCode(ctx context.Context, q querier, code string) (*ProductProfile, error) {
	return scanProfile(q.QueryRow(ctx, `
		SELECT profile_id, code, pricing_plan, instrument_scope,
		       subunit_divisor, min_deposit::text, status, created_at, updated_at
		  FROM account_product_profiles WHERE code = $1`, code))
}

// ProfileByID loads one profile row (nil when absent).
func (s *ProfileService) ProfileByID(ctx context.Context, id int64) (*ProductProfile, error) {
	return scanProfile(s.pool.QueryRow(ctx, `
		SELECT profile_id, code, pricing_plan, instrument_scope,
		       subunit_divisor, min_deposit::text, status, created_at, updated_at
		  FROM account_product_profiles WHERE profile_id = $1`, id))
}

func scanProfile(row pgx.Row) (*ProductProfile, error) {
	var (
		p     ProductProfile
		min   string
		scope []string
	)
	err := row.Scan(&p.ProfileID, &p.Code, &p.PricingPlan, &scope,
		&p.SubunitDivisor, &min, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "profile scan: %v", err)
	}
	p.InstrumentScope = scope
	p.MinDeposit, err = decimal.NewFromString(min)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "min_deposit parse: %v", err)
	}
	return &p, nil
}

// ListProfiles returns profiles newest-first; includeRetired=false is the
// venue-info view (assignable profiles only).
func (s *ProfileService) ListProfiles(ctx context.Context, includeRetired bool) ([]ProductProfile, error) {
	q := `SELECT profile_id, code, pricing_plan, instrument_scope,
	             subunit_divisor, min_deposit::text, status, created_at, updated_at
	        FROM account_product_profiles`
	if !includeRetired {
		q += ` WHERE status = 'ACTIVE'`
	}
	q += ` ORDER BY profile_id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "profile list: %v", err)
	}
	defer rows.Close()
	var out []ProductProfile
	for rows.Next() {
		var (
			p     ProductProfile
			min   string
			scope []string
		)
		if err := rows.Scan(&p.ProfileID, &p.Code, &p.PricingPlan, &scope,
			&p.SubunitDivisor, &min, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, errorf("INTERNAL_ERROR", "profile scan: %v", err)
		}
		p.InstrumentScope = scope
		p.MinDeposit, _ = decimal.NewFromString(min)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Dual-control executors — pricing/scope/divisor changes (task text:
// "pricing/scope changes dual-controlled, audit-logged"). The api layer
// submits admin.OpProductProfileChange; these run inside the approval
// transaction so the mutation and its four-eyes record commit together.
// ---------------------------------------------------------------------------

// ApplyCreateTx inserts the profile inside the dual-control tx.
func (s *ProfileService) ApplyCreateTx(ctx context.Context, tx pgx.Tx,
	actorUserID int64, in ProfileInput, clientIP string) (*ProductProfile, error) {
	if err := in.validate(false); err != nil {
		return nil, err
	}
	var p ProductProfile
	var min string
	var scope []string
	err := tx.QueryRow(ctx, `
		INSERT INTO account_product_profiles
		    (code, pricing_plan, instrument_scope, subunit_divisor, min_deposit)
		VALUES ($1, $2, $3, $4, $5::decimal)
		RETURNING profile_id, code, pricing_plan, instrument_scope,
		          subunit_divisor, min_deposit::text, status, created_at, updated_at`,
		in.Code, in.PricingPlan, in.InstrumentScope, in.SubunitDivisor, in.MinDeposit).
		Scan(&p.ProfileID, &p.Code, &p.PricingPlan, &scope,
			&p.SubunitDivisor, &min, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, errorf(CodeInvalidRequest,
				"profile code %q already exists", in.Code)
		}
		return nil, errorf("INTERNAL_ERROR", "profile insert: %v", err)
	}
	p.InstrumentScope = scope
	p.MinDeposit, _ = decimal.NewFromString(min)
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actorUserID,
		Action:      "product_profile.create",
		TargetType:  "account_product_profile",
		TargetID:    &p.ProfileID,
		AfterState:  p,
		IPAddress:   clientIP,
	}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit log: %v", err)
	}
	return &p, nil
}

// ApplyUpdateTx updates pricing_plan/scope/divisor/min_deposit/status
// inside the dual-control tx; the before-image rides the audit row.
func (s *ProfileService) ApplyUpdateTx(ctx context.Context, tx pgx.Tx,
	actorUserID, profileID int64, in ProfileInput, clientIP string) (*ProductProfile, error) {
	if err := in.validate(true); err != nil {
		return nil, err
	}
	before, err := scanProfile(tx.QueryRow(ctx, `
		SELECT profile_id, code, pricing_plan, instrument_scope,
		       subunit_divisor, min_deposit::text, status, created_at, updated_at
		  FROM account_product_profiles WHERE profile_id = $1 FOR UPDATE`, profileID))
	if err != nil {
		return nil, err
	}
	if before == nil {
		return nil, errorf(CodeNotFound, "product profile %d not found", profileID)
	}
	status := in.Status
	if status == "" {
		status = before.Status
	}
	var p ProductProfile
	var min string
	var scope []string
	err = tx.QueryRow(ctx, `
		UPDATE account_product_profiles
		   SET pricing_plan = $2, instrument_scope = $3, subunit_divisor = $4,
		       min_deposit = $5::decimal, status = $6, updated_at = now()
		 WHERE profile_id = $1
		RETURNING profile_id, code, pricing_plan, instrument_scope,
		          subunit_divisor, min_deposit::text, status, created_at, updated_at`,
		profileID, in.PricingPlan, in.InstrumentScope, in.SubunitDivisor,
		in.MinDeposit, status).
		Scan(&p.ProfileID, &p.Code, &p.PricingPlan, &scope,
			&p.SubunitDivisor, &min, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "profile update: %v", err)
	}
	p.InstrumentScope = scope
	p.MinDeposit, _ = decimal.NewFromString(min)
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actorUserID,
		Action:      "product_profile.update",
		TargetType:  "account_product_profile",
		TargetID:    &p.ProfileID,
		BeforeState: before,
		AfterState:  p,
		IPAddress:   clientIP,
	}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit log: %v", err)
	}
	return &p, nil
}

// ---------------------------------------------------------------------------
// Assignment — PUT /api/v1/admin/accounts/{id}/product-profile
// ---------------------------------------------------------------------------

// OpenExposure mirrors the Task 14.3.9 closure precondition read path:
// open positions (positions.quantity <> 0), resting orders (the
// orders.OpenStatuses set) and pending settlement legs.
type OpenExposure struct {
	OpenPositions      int64 `json:"open_positions"`
	RestingOrders      int64 `json:"resting_orders"`
	PendingSettlements int64 `json:"pending_settlements"`
}

// Any reports whether any exposure remains.
func (e OpenExposure) Any() bool {
	return e.OpenPositions > 0 || e.RestingOrders > 0 || e.PendingSettlements > 0
}

// OpenExposure is the shared precondition read for profile switching.
// Kept pool-scoped (not tx): the caller serializes the check+write.
func (s *ProfileService) OpenExposure(ctx context.Context, accountID int64) (OpenExposure, error) {
	var e OpenExposure
	if err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM positions
		    WHERE account_id = $1 AND quantity <> 0),
		  (SELECT count(*) FROM orders
		    WHERE account_id = $1
		      AND status IN ('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED')),
		  (SELECT count(*) FROM settlement_instructions
		    WHERE account_id = $1 AND status = 'PENDING')`,
		accountID).Scan(&e.OpenPositions, &e.RestingOrders, &e.PendingSettlements); err != nil {
		return e, errorf("INTERNAL_ERROR", "open-exposure read: %v", err)
	}
	return e, nil
}

// AssignProfile moves the account onto the named profile (single
// Compliance-Officer approver + audit). Rejected INVALID_REQUEST while
// open exposure exists or — for a divisor-changing switch — while any
// balance is non-zero (migration 096 / ledger.ValidateProfileSwitch).
// RETIRED targets and re-assignment to the same profile are rejected.
func (s *ProfileService) AssignProfile(ctx context.Context, actor AdminActor,
	accountID int64, code string) (*ProductProfile, error) {
	if err := s.requireRole(ctx, actor.UserID); err != nil {
		return nil, err
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return nil, newError(CodeInvalidRequest, "profile code required")
	}
	target, err := profileByCode(ctx, s.pool, code)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, errorf(CodeNotFound, "product profile %q not found", code)
	}
	if target.Status != ProfileStatusActive {
		return nil, errorf(CodeInvalidRequest,
			"profile %s is %s — retired profiles take no new assignment",
			target.Code, target.Status)
	}
	current, err := s.AccountProfile(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if current.ProfileID == target.ProfileID {
		return nil, errorf(CodeInvalidRequest,
			"account %d already on profile %s", accountID, target.Code)
	}
	exp, err := s.OpenExposure(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if exp.Any() {
		return nil, errorf(CodeInvalidRequest,
			"profile switch rejected — open exposure: %d position(s), %d resting order(s), %d pending settlement(s)",
			exp.OpenPositions, exp.RestingOrders, exp.PendingSettlements)
	}
	// Divisor change (STANDARD↔CENT): every balance row must be zero.
	if current.SubunitDivisor != target.SubunitDivisor {
		bals, err := s.subunitBalances(ctx, accountID)
		if err != nil {
			return nil, err
		}
		if err := ledger.ValidateProfileSwitch(
			ledger.ProductProfile{
				ProfileID: current.ProfileID, Code: current.Code,
				PricingPlan:    current.PricingPlan,
				SubunitDivisor: current.SubunitDivisor, Status: current.Status,
			},
			ledger.ProductProfile{
				ProfileID: target.ProfileID, Code: target.Code,
				PricingPlan:    target.PricingPlan,
				SubunitDivisor: target.SubunitDivisor, Status: target.Status,
			}, bals); err != nil {
			return nil, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Re-check inside the tx on the locked row: a racing order/position
	// commit between the read and the update cannot be excluded by
	// predicate reads, so the account row lock serializes the switch
	// against other admin mutations; the exposure read is repeated for
	// defence in depth.
	var acctStatus string
	if err := tx.QueryRow(ctx,
		`SELECT status::text FROM accounts WHERE id = $1 FOR UPDATE`,
		accountID).Scan(&acctStatus); err != nil {
		return nil, errorf("INTERNAL_ERROR", "account lock: %v", err)
	}
	if acctStatus == "CLOSED" {
		return nil, errorf(CodeInvalidRequest,
			"account %d is closed — no profile assignment", accountID)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET product_profile_id = $2, updated_at = now()
		 WHERE id = $1`, accountID, target.ProfileID); err != nil {
		return nil, errorf("INTERNAL_ERROR", "profile assignment: %v", err)
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "account.product_profile",
		TargetType:  "account",
		TargetID:    &accountID,
		BeforeState: map[string]any{"product_profile": current.Code},
		AfterState: map[string]any{
			"product_profile": target.Code, "profile_id": target.ProfileID,
			"open_exposure": exp,
		},
	}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit log: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit: %v", err)
	}
	return target, nil
}

// subunitBalances returns (currency,total) pairs for the §5.41.2
// divisor-switch zero-balance guard.
func (s *ProfileService) subunitBalances(ctx context.Context, accountID int64) ([]ledger.SubunitBalance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, total::text FROM balances WHERE account_id = $1`, accountID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "balance read: %v", err)
	}
	defer rows.Close()
	var out []ledger.SubunitBalance
	for rows.Next() {
		var b ledger.SubunitBalance
		var tot string
		if err := rows.Scan(&b.Currency, &tot); err != nil {
			return nil, errorf("INTERNAL_ERROR", "balance scan: %v", err)
		}
		b.Total, err = decimal.NewFromString(tot)
		if err != nil {
			return nil, errorf("INTERNAL_ERROR", "balance parse: %v", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
