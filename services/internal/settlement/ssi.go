// ssi.go — Standing Settlement Instruction registry (Phase-24 Task
// 24.3.9; spec §5.26, §17.7, §24 #155).
//
// SSIs are client-managed per-currency settlement targets. Every SSI is
// verified against the Phase-11 beneficiary registry (bank_accounts,
// migration 040) before it can be activated — an SSI pointing at an
// unverified beneficiary is refused fail-closed (SSI_NOT_VERIFIED).
// Settlement instructions default to the account's ACTIVE default SSI
// when no per-trade instruction is given.
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// CodeSsiNotVerified — the SSI's beneficiary does not resolve to a
// VERIFIED bank_accounts registry row for the account (422).
const CodeSsiNotVerified = "SSI_NOT_VERIFIED"

// SsiStatus mirrors ssi_status_enum.
type SsiStatus string

const (
	SsiActive  SsiStatus = "ACTIVE"
	SsiRevoked SsiStatus = "REVOKED"
)

// StandingSettlementInstruction is one standing_settlement_instructions
// row (spec §5.26 field vocabulary).
type StandingSettlementInstruction struct {
	ID                     int64      `json:"id"`
	AccountID              int64      `json:"account_id"`
	BankAccountID          *int64     `json:"bank_account_id,omitempty"`
	Currency               string     `json:"currency"`
	NostroOrBeneficiaryRef string     `json:"nostro_or_beneficiary_ref"`
	BIC                    string     `json:"bic,omitempty"`
	BeneficiaryBank        string     `json:"beneficiary_bank,omitempty"`
	Status                 SsiStatus  `json:"status"`
	IsDefault              bool       `json:"is_default"`
	VerifiedAt             *time.Time `json:"verified_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	RevokedAt              *time.Time `json:"revoked_at,omitempty"`
}

// VerifiedBeneficiary is the registry row an SSI is verified against
// (bank_accounts VERIFIED — Phase-11 Task 11.3.7).
type VerifiedBeneficiary struct {
	BankAccountID int64
	AccountID     int64
	Currency      string
	IBAN          string
	AccountNumber string
	SwiftBIC      string
	BankName      string
}

// SsiStore is the registry persistence seam.
type SsiStore interface {
	// VerifiedBankAccount resolves a VERIFIED beneficiary for the
	// account; found=false when none matches.
	VerifiedBankAccount(ctx context.Context, accountID, bankAccountID int64) (*VerifiedBeneficiary, bool, error)
	// InsertSSI persists a verified SSI.
	InsertSSI(ctx context.Context, s StandingSettlementInstruction) (int64, error)
	// SSI returns one row; found=false when absent.
	SSI(ctx context.Context, id int64) (*StandingSettlementInstruction, bool, error)
	// RevokeSSI flips ACTIVE→REVOKED (idempotent on already-revoked).
	RevokeSSI(ctx context.Context, id int64, at time.Time) error
	// ActiveSSIFor returns the account's ACTIVE SSI for the currency —
	// the default when flagged, else the newest ACTIVE. found=false
	// means none is registered.
	ActiveSSIFor(ctx context.Context, accountID int64, currency string) (*StandingSettlementInstruction, bool, error)
	// ListSSIs pages the account's registry rows.
	ListSSIs(ctx context.Context, accountID int64) ([]StandingSettlementInstruction, error)
	// ClearDefault unsets is_default on the account+currency ACTIVE rows
	// (runs inside the caller's tx via the same connection-level store —
	// the unique partial index enforces at most one default).
	ClearDefault(ctx context.Context, accountID int64, currency string) error
}

// SsiService is the SSI registry with beneficiary-registry verification.
type SsiService struct {
	store SsiStore
	clock func() time.Time
}

// NewSsiService wires the registry; store is required (fail-closed).
func NewSsiService(store SsiStore, clock func() time.Time) (*SsiService, error) {
	if store == nil {
		return nil, fmt.Errorf("ssi: nil store")
	}
	if clock == nil {
		clock = time.Now
	}
	return &SsiService{store: store, clock: clock}, nil
}

// RegisterSSI verifies the beneficiary against bank_accounts and inserts
// the ACTIVE SSI. bankAccountID is required for client SSIs (spec §17.7 —
// "verified against the Phase-11 bank_accounts registry before use"); the
// stored ref/BIC are cross-checked against the registry row so a drifted
// instruction cannot point elsewhere.
func (s *SsiService) RegisterSSI(ctx context.Context, accountID, bankAccountID int64,
	currency, beneficiaryRef, bic string, isDefault bool) (*StandingSettlementInstruction, error) {

	currency = strings.ToUpper(strings.TrimSpace(currency))
	if accountID <= 0 || bankAccountID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "ssi: account_id and bank_account_id required")
	}
	if !currencyRe.MatchString(currency) {
		return nil, excerrors.New("INVALID_REQUEST", "ssi: currency must be a 3-letter ISO code")
	}
	ben, found, err := s.store.VerifiedBankAccount(ctx, accountID, bankAccountID)
	if err != nil {
		return nil, fmt.Errorf("ssi: beneficiary lookup: %w", err)
	}
	if !found {
		return nil, excerrors.New(CodeSsiNotVerified, fmt.Sprintf(
			"ssi: bank account %d is not a VERIFIED beneficiary of account %d", bankAccountID, accountID))
	}
	if ben.Currency != currency {
		return nil, excerrors.New(CodeSsiNotVerified, fmt.Sprintf(
			"ssi: beneficiary currency %s != SSI currency %s", ben.Currency, currency))
	}
	// Cross-check the claimed target against the registry row — the SSI
	// may only point at the registered IBAN/account + BIC.
	regRef := ben.IBAN
	if regRef == "" {
		regRef = ben.AccountNumber
	}
	beneficiaryRef = strings.TrimSpace(beneficiaryRef)
	if beneficiaryRef == "" {
		beneficiaryRef = regRef
	}
	if regRef != "" && beneficiaryRef != regRef {
		return nil, excerrors.New(CodeSsiNotVerified, fmt.Sprintf(
			"ssi: beneficiary_ref %q does not match registered account %q", beneficiaryRef, regRef))
	}
	bic = strings.ToUpper(strings.TrimSpace(bic))
	if bic == "" {
		bic = ben.SwiftBIC
	}
	if ben.SwiftBIC != "" && bic != "" && bic != strings.ToUpper(ben.SwiftBIC) {
		return nil, excerrors.New(CodeSsiNotVerified, fmt.Sprintf(
			"ssi: bic %q does not match registered BIC %q", bic, ben.SwiftBIC))
	}
	if bic != "" && !bicRe.MatchString(bic) {
		return nil, excerrors.New("INVALID_REQUEST", "ssi: malformed BIC")
	}

	if isDefault {
		if err := s.store.ClearDefault(ctx, accountID, currency); err != nil {
			return nil, fmt.Errorf("ssi: clear default: %w", err)
		}
	}
	now := s.clock().UTC()
	row := StandingSettlementInstruction{
		AccountID:              accountID,
		BankAccountID:          &bankAccountID,
		Currency:               currency,
		NostroOrBeneficiaryRef: beneficiaryRef,
		BIC:                    bic,
		BeneficiaryBank:        ben.BankName,
		Status:                 SsiActive,
		IsDefault:              isDefault,
		VerifiedAt:             &now,
	}
	id, err := s.store.InsertSSI(ctx, row)
	if err != nil {
		return nil, err
	}
	row.ID = id
	return &row, nil
}

// RevokeSSI marks the SSI revoked — mid-batch netting resolves the
// CURRENT active SSI at dispatch (never a revoked row).
func (s *SsiService) RevokeSSI(ctx context.Context, id int64) (*StandingSettlementInstruction, error) {
	row, found, err := s.store.SSI(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("ssi %d not found", id))
	}
	if row.Status == SsiRevoked {
		return row, nil // idempotent
	}
	if err := s.store.RevokeSSI(ctx, id, s.clock().UTC()); err != nil {
		return nil, err
	}
	row.Status = SsiRevoked
	return row, nil
}

// DefaultSSI resolves the account's ACTIVE default for a currency.
func (s *SsiService) DefaultSSI(ctx context.Context, accountID int64, currency string) (*StandingSettlementInstruction, bool, error) {
	return s.store.ActiveSSIFor(ctx, accountID, strings.ToUpper(strings.TrimSpace(currency)))
}

// List returns the account's SSI registry rows.
func (s *SsiService) List(ctx context.Context, accountID int64) ([]StandingSettlementInstruction, error) {
	return s.store.ListSSIs(ctx, accountID)
}

// ---------------------------------------------------------------------------
// PgxSsiStore
// ---------------------------------------------------------------------------

// PgxSsiStore implements SsiStore over pgx.
type PgxSsiStore struct{ Pool *pgxpool.Pool }

// NewPgxSsiStore wires the store.
func NewPgxSsiStore(pool *pgxpool.Pool) *PgxSsiStore { return &PgxSsiStore{Pool: pool} }

// VerifiedBankAccount resolves a VERIFIED bank_accounts row.
func (s *PgxSsiStore) VerifiedBankAccount(ctx context.Context, accountID, bankAccountID int64) (*VerifiedBeneficiary, bool, error) {
	var b VerifiedBeneficiary
	var iban, acct, bic *string
	err := s.Pool.QueryRow(ctx, `
		SELECT bank_account_id, account_id, currency,
		       iban, account_number, swift_bic, bank_name
		  FROM bank_accounts
		 WHERE bank_account_id = $1 AND account_id = $2 AND status = 'VERIFIED'`,
		bankAccountID, accountID).
		Scan(&b.BankAccountID, &b.AccountID, &b.Currency, &iban, &acct, &bic, &b.BankName)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if iban != nil {
		b.IBAN = *iban
	}
	if acct != nil {
		b.AccountNumber = *acct
	}
	if bic != nil {
		b.SwiftBIC = strings.ToUpper(*bic)
	}
	return &b, true, nil
}

// InsertSSI persists the row.
func (s *PgxSsiStore) InsertSSI(ctx context.Context, row StandingSettlementInstruction) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO standing_settlement_instructions
		    (account_id, bank_account_id, currency, nostro_or_beneficiary_ref,
		     bic, beneficiary_bank, status, is_default, verified_at)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),$7,$8,$9)
		RETURNING id`,
		row.AccountID, row.BankAccountID, row.Currency, row.NostroOrBeneficiaryRef,
		row.BIC, row.BeneficiaryBank, string(row.Status), row.IsDefault, row.VerifiedAt).
		Scan(&id)
	return id, err
}

// SSI loads one row.
func (s *PgxSsiStore) SSI(ctx context.Context, id int64) (*StandingSettlementInstruction, bool, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT id, account_id, bank_account_id, currency, nostro_or_beneficiary_ref,
		       COALESCE(bic,''), COALESCE(beneficiary_bank,''), status::text,
		       is_default, verified_at, created_at, revoked_at
		  FROM standing_settlement_instructions WHERE id = $1`, id)
	return scanSSI(row)
}

// RevokeSSI flips ACTIVE → REVOKED.
func (s *PgxSsiStore) RevokeSSI(ctx context.Context, id int64, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE standing_settlement_instructions
		   SET status = 'REVOKED', revoked_at = $2, updated_at = now()
		 WHERE id = $1 AND status = 'ACTIVE'`, id, at)
	return err
}

// ActiveSSIFor returns the ACTIVE default (preferred) or newest ACTIVE.
func (s *PgxSsiStore) ActiveSSIFor(ctx context.Context, accountID int64, currency string) (*StandingSettlementInstruction, bool, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT id, account_id, bank_account_id, currency, nostro_or_beneficiary_ref,
		       COALESCE(bic,''), COALESCE(beneficiary_bank,''), status::text,
		       is_default, verified_at, created_at, revoked_at
		  FROM standing_settlement_instructions
		 WHERE account_id = $1 AND currency = $2 AND status = 'ACTIVE'
		 ORDER BY is_default DESC, id DESC LIMIT 1`, accountID, currency)
	return scanSSI(row)
}

// ListSSIs pages the account's registry.
func (s *PgxSsiStore) ListSSIs(ctx context.Context, accountID int64) ([]StandingSettlementInstruction, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, account_id, bank_account_id, currency, nostro_or_beneficiary_ref,
		       COALESCE(bic,''), COALESCE(beneficiary_bank,''), status::text,
		       is_default, verified_at, created_at, revoked_at
		  FROM standing_settlement_instructions
		 WHERE account_id = $1 ORDER BY currency, id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StandingSettlementInstruction
	for rows.Next() {
		var row StandingSettlementInstruction
		var bankID *int64
		var status string
		if err := rows.Scan(&row.ID, &row.AccountID, &bankID, &row.Currency,
			&row.NostroOrBeneficiaryRef, &row.BIC, &row.BeneficiaryBank, &status,
			&row.IsDefault, &row.VerifiedAt, &row.CreatedAt, &row.RevokedAt); err != nil {
			return nil, err
		}
		row.BankAccountID = bankID
		row.Status = SsiStatus(status)
		out = append(out, row)
	}
	return out, rows.Err()
}

// ClearDefault unsets is_default for the account+currency ACTIVE rows.
func (s *PgxSsiStore) ClearDefault(ctx context.Context, accountID int64, currency string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE standing_settlement_instructions
		   SET is_default = FALSE, updated_at = now()
		 WHERE account_id = $1 AND currency = $2 AND status = 'ACTIVE' AND is_default`,
		accountID, currency)
	return err
}

func scanSSI(row pgx.Row) (*StandingSettlementInstruction, bool, error) {
	var r StandingSettlementInstruction
	var status string
	err := row.Scan(&r.ID, &r.AccountID, &r.BankAccountID, &r.Currency,
		&r.NostroOrBeneficiaryRef, &r.BIC, &r.BeneficiaryBank, &status,
		&r.IsDefault, &r.VerifiedAt, &r.CreatedAt, &r.RevokedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	r.Status = SsiStatus(status)
	return &r, true, nil
}
