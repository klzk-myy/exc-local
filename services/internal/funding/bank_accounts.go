// Phase-11 Task 11.3.7 — beneficiary bank-account registry (spec §5.23,
// migration 040). Withdrawals may only target a VERIFIED beneficiary;
// the admin verification path is dual-controlled (four-eyes) and is the
// ONLY route to VERIFIED — a client can never self-verify.
//
// Security invariants enforced here:
//   - Registration requires an ACTIVE account at KYC tier T1+
//     (T0 → KYC_REQUIRED).
//   - Ownership isolation: every read/mutation is scoped to the owning
//     account_id; admin paths carry the admin actor id for audit.
//   - New-beneficiary hold: verification stamps
//     unlocked_at = verified_at + 24h (§24 #391 new-account hold) —
//     AssertWithdrawable refuses the destination until it lapses.
//   - Deleting + re-registering a beneficiary re-enters
//     PENDING_VERIFICATION — the 24h hold restarts on re-verification.
//
// The third-party deposit screen itself lives in deposit_guard.go
// (Task 11.3.11 cluster): this registry's VERIFIED beneficiary_name is
// the legal-name comparator source (pgLegalNameResolver in cmd/gateway).
package funding

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// CodeBankAccountNotVerified is the §23 surface for a withdrawal whose
// destination is not a registered, verified beneficiary (supersedes the
// deprecated ADDRESS_NOT_ALLOWLISTED).
const CodeBankAccountNotVerified = "BANK_ACCOUNT_NOT_VERIFIED"

// CodeBeneficiaryHoldActive rejects withdrawals to a beneficiary still
// inside its 24-hour post-verification hold (§24 #391).
const CodeBeneficiaryHoldActive = "BENEFICIARY_HOLD_ACTIVE"

// BankAccount statuses — bank_account_status_enum (migration 040).
const (
	BankAcctPending  = "PENDING_VERIFICATION"
	BankAcctVerified = "VERIFIED"
	BankAcctRejected = "REJECTED"
)

// Verification methods (verification_method column).
const (
	VerifyMethodBankStatement = "BANK_STATEMENT"
	VerifyMethodMicroDeposit  = "MICRO_DEPOSIT"
)

// BeneficiaryHoldWindow is the 24-hour new-beneficiary withdrawal hold
// (§24 #391 / Task 11.3.10 cooling lock applied at verification).
const BeneficiaryHoldWindow = 24 * time.Hour

// BankAccount is one beneficiary-registry row (bank_accounts, §5.23).
type BankAccount struct {
	BankAccountID      int64      `json:"bank_account_id"`
	AccountID          int64      `json:"account_id"`
	Currency           string     `json:"currency"`
	IBAN               *string    `json:"iban,omitempty"`
	AccountNumber      *string    `json:"account_number,omitempty"`
	SwiftBIC           *string    `json:"swift_bic,omitempty"`
	BICRouting         *string    `json:"bic_routing,omitempty"`
	BankName           string     `json:"bank_name"`
	BeneficiaryName    string     `json:"beneficiary_name"`
	Rail               string     `json:"rail"`
	Status             string     `json:"status"`
	VerificationMethod *string    `json:"verification_method,omitempty"`
	VerifiedAt         *time.Time `json:"verified_at,omitempty"`
	VerifiedBy         *int64     `json:"verified_by,omitempty"`
	UnlockedAt         *time.Time `json:"unlocked_at,omitempty"`
	RejectionReason    *string    `json:"rejection_reason,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// BankAccountInput is the POST /api/v1/funding/bank-accounts payload.
type BankAccountInput struct {
	Currency        string  `json:"currency"`
	IBAN            *string `json:"iban,omitempty"`
	AccountNumber   *string `json:"account_number,omitempty"`
	SwiftBIC        *string `json:"swift_bic,omitempty"`
	BICRouting      *string `json:"bic_routing,omitempty"`
	BankName        string  `json:"bank_name"`
	BeneficiaryName string  `json:"beneficiary_name"`
	Rail            string  `json:"rail"`
}

// ---------------------------------------------------------------------------
// Store seam — PgBankAccountStore implements it over pgx; unit tests
// substitute an in-memory fake.
// ---------------------------------------------------------------------------

// BeneficiaryRoleResolver resolves an admin user id to its §8.2 role —
// identical seam convention to admin.AdminRoleResolver; nil fails closed.
type BeneficiaryRoleResolver func(ctx context.Context, adminUserID int64) (string, error)

// verifyRoles may initiate OR approve a beneficiary verification —
// the route registry pins RoleFinanceOps; Compliance/Risk/Super Admin
// are equivalent-or-stronger fund-custody authorities.
var verifyRoles = map[string]bool{
	"Finance Ops":        true,
	"Compliance Officer": true,
	"Risk Manager":       true,
	"Super Admin":        true,
}

// BankAccountStore is the persistence seam for the registry.
type BankAccountStore interface {
	BeginTx(ctx context.Context) (pgx.Tx, error)
	AccountMeta(ctx context.Context, id int64) (*AccountMeta, error)

	InsertBankAccount(ctx context.Context, tx pgx.Tx, b *BankAccount) (*BankAccount, error)
	BankAccount(ctx context.Context, id int64) (*BankAccount, error)
	BankAccountForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*BankAccount, error)
	BankAccountsForAccount(ctx context.Context, accountID int64) ([]BankAccount, error)
	ListBankAccounts(ctx context.Context, status string, limit int) ([]BankAccount, error)
	// DeleteBankAccount removes a client-owned row; returns false when
	// (id, accountID) does not resolve — callers map to NOT_FOUND.
	DeleteBankAccount(ctx context.Context, accountID, id int64) (bool, error)
	// FindBeneficiary resolves a withdrawal destination reference
	// (IBAN or account number, case/space-insensitive) for one account.
	FindBeneficiary(ctx context.Context, accountID int64, reference string) (*BankAccount, error)
	// SetBankAccountStatus performs the verification-lifecycle
	// transition inside tx.
	SetBankAccountStatus(ctx context.Context, tx pgx.Tx, id int64,
		status string, verifiedBy *int64, method *string, reason *string,
		verifiedAt, unlockedAt *time.Time) error
	// AdminAuditTx writes the admin_audit_log row inside the same tx so
	// the lifecycle transition and its audit commit or fail together.
	AdminAuditTx(ctx context.Context, tx pgx.Tx, adminID int64,
		action, targetType string, targetID int64,
		before, after []byte, ip string) error
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// BankAccountService owns the beneficiary registry lifecycle.
type BankAccountService struct {
	store BankAccountStore
	roles BeneficiaryRoleResolver
	clock func() time.Time
	logf  func(format string, args ...any)
}

// NewBankAccountService wires the service; store + roles are required
// (a nil role resolver fails closed on every admin mutation).
func NewBankAccountService(store BankAccountStore,
	roles BeneficiaryRoleResolver) (*BankAccountService, error) {
	if store == nil || roles == nil {
		return nil, fmt.Errorf("funding: bank-account service requires store + role resolver")
	}
	return &BankAccountService{store: store, roles: roles, clock: time.Now,
		logf: func(string, ...any) {}}, nil
}

// WithClock overrides the clock (tests).
func (s *BankAccountService) WithClock(c func() time.Time) *BankAccountService {
	s.clock = c
	return s
}

// WithLogger wires a diagnostic sink.
func (s *BankAccountService) WithLogger(f func(format string, args ...any)) *BankAccountService {
	s.logf = f
	return s
}

// normalizeIBAN strips separators and uppercases (IBAN canonical form).
func normalizeIBAN(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// normalizeRef normalizes a destination reference for matching —
// IBAN and account-number comparisons are space/case-insensitive.
func normalizeRef(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// validate vets a registration payload; returns normalized fields.
func (in *BankAccountInput) validate() error {
	if _, err := normalizeCurrency(in.Currency); err != nil {
		return err
	}
	if in.IBAN != nil {
		iban := normalizeIBAN(*in.IBAN)
		if len(iban) < 8 || len(iban) > 34 {
			return errCode("INVALID_REQUEST", "iban must be 8–34 characters")
		}
		in.IBAN = &iban
	}
	if in.AccountNumber != nil {
		num := normalizeRef(*in.AccountNumber)
		if len(num) == 0 || len(num) > 64 {
			return errCode("INVALID_REQUEST", "account_number must be ≤64 chars")
		}
		in.AccountNumber = &num
	}
	if in.IBAN == nil && in.AccountNumber == nil {
		return errCode("INVALID_REQUEST",
			"iban or account_number is required — a withdrawable destination")
	}
	if in.SwiftBIC != nil {
		bic := strings.ToUpper(strings.TrimSpace(*in.SwiftBIC))
		if bic != "" && (len(bic) != 8 && len(bic) != 11) {
			return errCode("INVALID_REQUEST", "swift_bic must be 8 or 11 characters")
		}
		if bic == "" {
			in.SwiftBIC = nil
		} else {
			in.SwiftBIC = &bic
		}
	}
	if in.BICRouting != nil {
		r := strings.TrimSpace(*in.BICRouting)
		if len(r) > 32 {
			return errCode("INVALID_REQUEST", "bic_routing must be ≤32 chars")
		}
		if r == "" {
			in.BICRouting = nil
		} else {
			in.BICRouting = &r
		}
	}
	in.BankName = strings.TrimSpace(in.BankName)
	if in.BankName == "" || len(in.BankName) > 128 {
		return errCode("INVALID_REQUEST", "bank_name is required (≤128 chars)")
	}
	in.BeneficiaryName = strings.TrimSpace(in.BeneficiaryName)
	if in.BeneficiaryName == "" || len(in.BeneficiaryName) > 255 {
		return errCode("INVALID_REQUEST", "beneficiary_name is required (≤255 chars)")
	}
	in.Rail = strings.ToUpper(strings.TrimSpace(in.Rail))
	if !bankMethodDomain[in.Rail] {
		return errCode("INVALID_REQUEST",
			"rail must be one of SWIFT|SEPA|FEDNOW|ACH|CHAPS|TARGET2|WIRE|INTERNAL")
	}
	return nil
}

// Register creates a PENDING_VERIFICATION beneficiary for the account.
// Registration requires KYC tier T1+ on an ACTIVE account (task step 2).
func (s *BankAccountService) Register(ctx context.Context, accountID int64,
	in BankAccountInput) (*BankAccount, error) {
	if accountID <= 0 {
		return nil, errCode("INVALID_REQUEST", "account_id required")
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	meta, err := s.store.AccountMeta(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if meta.Status != "ACTIVE" {
		return nil, errf("FORBIDDEN",
			"account %d is %s — beneficiary registration requires an active account",
			accountID, meta.Status)
	}
	if meta.KYCTier == "T0" || meta.KYCTier == "" {
		return nil, errCode("KYC_REQUIRED",
			"beneficiary registration requires KYC tier T1 or higher")
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "beneficiary tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b := &BankAccount{
		AccountID:       accountID,
		Currency:        in.Currency,
		IBAN:            in.IBAN,
		AccountNumber:   in.AccountNumber,
		SwiftBIC:        in.SwiftBIC,
		BICRouting:      in.BICRouting,
		BankName:        in.BankName,
		BeneficiaryName: in.BeneficiaryName,
		Rail:            in.Rail,
		Status:          BankAcctPending,
	}
	out, err := s.store.InsertBankAccount(ctx, tx, b)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "beneficiary commit", err)
	}
	return out, nil
}

// ListMine enumerates the caller's beneficiaries (owner-scoped).
func (s *BankAccountService) ListMine(ctx context.Context, accountID int64) ([]BankAccount, error) {
	if accountID <= 0 {
		return nil, errCode("INVALID_REQUEST", "account_id required")
	}
	return s.store.BankAccountsForAccount(ctx, accountID)
}

// DeleteMine removes a client-owned beneficiary. Deleting a VERIFIED
// row re-arms the full registration + verification + 24h hold cycle on
// re-add — there is no "disable" shortcut around the hold.
func (s *BankAccountService) DeleteMine(ctx context.Context, accountID, id int64) error {
	if accountID <= 0 || id <= 0 {
		return errCode("INVALID_REQUEST", "account_id and id required")
	}
	ok, err := s.store.DeleteBankAccount(ctx, accountID, id)
	if err != nil {
		return err
	}
	if !ok {
		return errf("NOT_FOUND", "beneficiary %d not found", id)
	}
	return nil
}

// AdminList enumerates registry rows for the admin surface (?status=).
func (s *BankAccountService) AdminList(ctx context.Context, status string,
	limit int) ([]BankAccount, error) {
	st := strings.ToUpper(strings.TrimSpace(status))
	switch st {
	case "", BankAcctPending, BankAcctVerified, BankAcctRejected:
	default:
		return nil, errf("INVALID_REQUEST", "status %q not in registry lifecycle", status)
	}
	return s.store.ListBankAccounts(ctx, st, limit)
}

// requireVerifyRole resolves a user's admin role fail-closed.
func (s *BankAccountService) requireVerifyRole(ctx context.Context, userID int64) error {
	if s.roles == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"beneficiary verification requires an admin role — resolver unavailable")
	}
	role, err := s.roles(ctx, userID)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "admin role resolution", err)
	}
	if !verifyRoles[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"beneficiary verification requires Finance Ops or stronger")
	}
	return nil
}

// Verify performs the dual-controlled PENDING → VERIFIED transition
// (bank-statement review / completed micro-deposit evidence). The
// approver must be a distinct admin with an eligible role; the verified
// row carries a 24h withdrawal hold (unlocked_at).
func (s *BankAccountService) Verify(ctx context.Context, adminID, approverID int64,
	id int64, method, ip string) (*BankAccount, error) {
	if adminID <= 0 {
		return nil, errCode("UNAUTHORIZED", "admin identity required")
	}
	m := strings.ToUpper(strings.TrimSpace(method))
	if m == "" {
		m = VerifyMethodBankStatement
	}
	if m != VerifyMethodBankStatement && m != VerifyMethodMicroDeposit {
		return nil, errf("INVALID_REQUEST", "verification method %q unsupported", method)
	}
	if err := s.requireVerifyRole(ctx, adminID); err != nil {
		return nil, err
	}
	// Dual control (task step 2: "dual control for manual verify").
	if approverID <= 0 {
		return nil, errCode("DUAL_CONTROL_REQUIRED",
			"beneficiary verification requires a second authorizer (approver_id)")
	}
	if approverID == adminID {
		return nil, errCode("DUAL_CONTROL_REQUIRED",
			"approver must differ from the verifying admin")
	}
	if err := s.requireVerifyRole(ctx, approverID); err != nil {
		return nil, err
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "verify tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := s.store.BankAccountForUpdate(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errf("NOT_FOUND", "beneficiary %d not found", id)
	}
	if row.Status != BankAcctPending {
		return nil, errf("INVALID_REQUEST",
			"beneficiary %d is %s — only PENDING_VERIFICATION rows verify", id, row.Status)
	}
	now := s.clock().UTC()
	unlock := now.Add(BeneficiaryHoldWindow)
	before := fmt.Sprintf(`{"status":%q}`, row.Status)
	after := fmt.Sprintf(`{"status":%q,"verified_by":%d,"approved_by":%d,`+
		`"method":%q,"verified_at":%q,"unlocked_at":%q}`,
		BankAcctVerified, adminID, approverID, m,
		now.Format(time.RFC3339Nano), unlock.Format(time.RFC3339Nano))
	if err := s.store.SetBankAccountStatus(ctx, tx, id, BankAcctVerified,
		&adminID, &m, nil, &now, &unlock); err != nil {
		return nil, err
	}
	if err := s.store.AdminAuditTx(ctx, tx, adminID,
		"funding.beneficiary.verify", "bank_account", id,
		[]byte(before), []byte(after), ip); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "verify commit", err)
	}
	row.Status = BankAcctVerified
	row.VerifiedAt = &now
	row.VerifiedBy = &adminID
	row.VerificationMethod = &m
	row.UnlockedAt = &unlock
	return row, nil
}

// Reject performs the PENDING → REJECTED transition with a mandatory
// reason. Rejection is single-approver (it releases no funds — the
// conservative direction never needs a second key).
func (s *BankAccountService) Reject(ctx context.Context, adminID, id int64,
	reason, ip string) (*BankAccount, error) {
	if adminID <= 0 {
		return nil, errCode("UNAUTHORIZED", "admin identity required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errCode("INVALID_REQUEST", "rejection reason required")
	}
	if err := s.requireVerifyRole(ctx, adminID); err != nil {
		return nil, err
	}
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "reject tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := s.store.BankAccountForUpdate(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errf("NOT_FOUND", "beneficiary %d not found", id)
	}
	if row.Status != BankAcctPending {
		return nil, errf("INVALID_REQUEST",
			"beneficiary %d is %s — only PENDING_VERIFICATION rows reject", id, row.Status)
	}
	before := fmt.Sprintf(`{"status":%q}`, row.Status)
	after := fmt.Sprintf(`{"status":%q,"rejected_by":%d,"reason":%q}`,
		BankAcctRejected, adminID, reason)
	if err := s.store.SetBankAccountStatus(ctx, tx, id, BankAcctRejected,
		&adminID, nil, &reason, nil, nil); err != nil {
		return nil, err
	}
	if err := s.store.AdminAuditTx(ctx, tx, adminID,
		"funding.beneficiary.reject", "bank_account", id,
		[]byte(before), []byte(after), ip); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "reject commit", err)
	}
	row.Status = BankAcctRejected
	row.RejectionReason = &reason
	return row, nil
}

// AssertWithdrawable is the withdrawal-create gate (Task 11.3.7 step 3,
// wired via WithdrawalService.WithBeneficiaries): the destination
// reference must resolve to a VERIFIED beneficiary past its 24h hold.
// Emits BANK_ACCOUNT_NOT_VERIFIED (unregistered / unverified) and
// BENEFICIARY_HOLD_ACTIVE (inside the hold window).
func (s *BankAccountService) AssertWithdrawable(ctx context.Context,
	accountID int64, reference string) error {
	ref := normalizeRef(reference)
	if ref == "" {
		return errCode(CodeBankAccountNotVerified,
			"withdrawal requires a registered beneficiary destination")
	}
	row, err := s.store.FindBeneficiary(ctx, accountID, ref)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "beneficiary lookup", err)
	}
	if row == nil || row.Status != BankAcctVerified {
		return errf(CodeBankAccountNotVerified,
			"withdrawal destination is not a verified beneficiary "+
				"(register + verify via /api/v1/funding/bank-accounts)")
	}
	// 24h new-beneficiary hold (§24 #391): NULL unlocked_at falls back
	// to verified_at + 24h — never treated as already-open.
	unlock := row.UnlockedAt
	if unlock == nil && row.VerifiedAt != nil {
		u := row.VerifiedAt.Add(BeneficiaryHoldWindow)
		unlock = &u
	}
	if unlock == nil || s.clock().UTC().Before(*unlock) {
		until := s.clock().UTC()
		if unlock != nil {
			until = *unlock
		}
		return errf(CodeBeneficiaryHoldActive,
			"beneficiary within the 24-hour new-account hold — withdrawable after %s",
			until.Format(time.RFC3339))
	}
	return nil
}

// ---------------------------------------------------------------------------
// PgBankAccountStore — production implementation over the shared pool.
// ---------------------------------------------------------------------------

// PgBankAccountStore implements BankAccountStore over pgx.
type PgBankAccountStore struct {
	pool *pgxpool.Pool
}

// NewPgBankAccountStore wires the store.
func NewPgBankAccountStore(pool *pgxpool.Pool) *PgBankAccountStore {
	return &PgBankAccountStore{pool: pool}
}

// BeginTx opens a SERIALIZABLE tx (spec §14.6) for lifecycle transitions.
func (s *PgBankAccountStore) BeginTx(ctx context.Context) (pgx.Tx, error) {
	return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
}

// AccountMeta delegates to the funding projection (status + KYC tier).
func (s *PgBankAccountStore) AccountMeta(ctx context.Context, id int64) (*AccountMeta, error) {
	return NewPgStore(s.pool).AccountMeta(ctx, id)
}

const bankAccountCols = `
	bank_account_id, account_id, currency, iban, account_number,
	swift_bic, bic_routing, bank_name, beneficiary_name, rail::text,
	status::text, verification_method, verified_at, verified_by,
	unlocked_at, rejection_reason, created_at, updated_at`

func scanBankAccount(row pgx.Row) (*BankAccount, error) {
	var b BankAccount
	err := row.Scan(&b.BankAccountID, &b.AccountID, &b.Currency,
		&b.IBAN, &b.AccountNumber, &b.SwiftBIC, &b.BICRouting,
		&b.BankName, &b.BeneficiaryName, &b.Rail, &b.Status,
		&b.VerificationMethod, &b.VerifiedAt, &b.VerifiedBy,
		&b.UnlockedAt, &b.RejectionReason, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *PgBankAccountStore) InsertBankAccount(ctx context.Context, tx pgx.Tx,
	b *BankAccount) (*BankAccount, error) {
	err := tx.QueryRow(ctx, `
		INSERT INTO bank_accounts
		    (account_id, currency, iban, account_number, swift_bic,
		     bic_routing, bank_name, beneficiary_name, rail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::bank_account_rail_enum)
		RETURNING bank_account_id, created_at, updated_at`,
		b.AccountID, b.Currency, b.IBAN, b.AccountNumber, b.SwiftBIC,
		b.BICRouting, b.BankName, b.BeneficiaryName, b.Rail).
		Scan(&b.BankAccountID, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "insert beneficiary", err)
	}
	b.Status = BankAcctPending
	return b, nil
}

func (s *PgBankAccountStore) BankAccount(ctx context.Context, id int64) (*BankAccount, error) {
	b, err := scanBankAccount(s.pool.QueryRow(ctx,
		`SELECT `+bankAccountCols+` FROM bank_accounts WHERE bank_account_id = $1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "beneficiary read", err)
	}
	return b, nil
}

func (s *PgBankAccountStore) BankAccountForUpdate(ctx context.Context, tx pgx.Tx,
	id int64) (*BankAccount, error) {
	b, err := scanBankAccount(tx.QueryRow(ctx,
		`SELECT `+bankAccountCols+` FROM bank_accounts
		 WHERE bank_account_id = $1 FOR UPDATE`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "beneficiary lock", err)
	}
	return b, nil
}

func (s *PgBankAccountStore) BankAccountsForAccount(ctx context.Context,
	accountID int64) ([]BankAccount, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+bankAccountCols+` FROM bank_accounts
		 WHERE account_id = $1 ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "beneficiary list", err)
	}
	defer rows.Close()
	var out []BankAccount
	for rows.Next() {
		b, err := scanBankAccount(rows)
		if err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "beneficiary scan", err)
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func (s *PgBankAccountStore) ListBankAccounts(ctx context.Context,
	status string, limit int) ([]BankAccount, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var rows pgx.Rows
	var err error
	if status == "" {
		rows, err = s.pool.Query(ctx,
			`SELECT `+bankAccountCols+` FROM bank_accounts
			 ORDER BY created_at DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT `+bankAccountCols+` FROM bank_accounts
			 WHERE status = $1::bank_account_status_enum
			 ORDER BY created_at DESC LIMIT $2`, status, limit)
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "beneficiary list", err)
	}
	defer rows.Close()
	var out []BankAccount
	for rows.Next() {
		b, err := scanBankAccount(rows)
		if err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "beneficiary scan", err)
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func (s *PgBankAccountStore) DeleteBankAccount(ctx context.Context,
	accountID, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM bank_accounts
		 WHERE bank_account_id = $1 AND account_id = $2`, id, accountID)
	if err != nil {
		return false, wrapCode("INTERNAL_ERROR", "beneficiary delete", err)
	}
	return tag.RowsAffected() > 0, nil
}

// FindBeneficiary resolves the withdrawal destination. The reference is
// compared space/case-insensitively against BOTH iban and
// account_number — a client may quote either form.
func (s *PgBankAccountStore) FindBeneficiary(ctx context.Context,
	accountID int64, reference string) (*BankAccount, error) {
	b, err := scanBankAccount(s.pool.QueryRow(ctx,
		`SELECT `+bankAccountCols+` FROM bank_accounts
		 WHERE account_id = $1
		   AND (REPLACE(UPPER(iban),' ','') = $2
		        OR REPLACE(UPPER(account_number),' ','') = $2)
		 ORDER BY (status = 'VERIFIED') DESC, created_at DESC
		 LIMIT 1`, accountID, reference))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "beneficiary match", err)
	}
	return b, nil
}

func (s *PgBankAccountStore) SetBankAccountStatus(ctx context.Context, tx pgx.Tx,
	id int64, status string, verifiedBy *int64, method *string, reason *string,
	verifiedAt, unlockedAt *time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE bank_accounts
		   SET status = $2::bank_account_status_enum,
		       verification_method = COALESCE($3, verification_method),
		       verified_by = $4, verified_at = $5, unlocked_at = $6,
		       rejection_reason = $7, updated_at = now()
		 WHERE bank_account_id = $1`,
		id, status, method, verifiedBy, verifiedAt, unlockedAt, reason)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "beneficiary transition", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "beneficiary %d not found", id)
	}
	return nil
}

func (s *PgBankAccountStore) AdminAuditTx(ctx context.Context, tx pgx.Tx,
	adminID int64, action, targetType string, targetID int64,
	before, after []byte, ip string) error {
	var ipParam *string
	if ip != "" {
		ipParam = &ip
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log
		    (admin_user_id, action, target_type, target_id,
		     before_state, after_state, ip_address)
		VALUES ($1,$2,$3,$4,$5,$6,$7::inet)`,
		adminID, action, targetType, targetID, before, after, ipParam)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "admin audit", err)
	}
	return nil
}
