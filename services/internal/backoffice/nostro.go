// Package backoffice implements the Phase-24 correspondent-banking
// backoffice (spec §17; Phase-24 Tasks 24.3.1–24.3.5): nostro/vostro
// account management, daily reconciliation, settlement-confirmation
// tracking, SWIFT message tracking and the compliance reporting export.
//
// Task 24.3.1 — Nostro/Vostro Account Management (this file):
//
//   - nostro_accounts (migration 018) is the registry; migration 261 adds
//     account_role (NOSTRO = our money held at the correspondent bank,
//     VOSTRO = the correspondent's money held by us) and the
//     (currency, bank, account) dedup index.
//   - Real-time balance tracking: PostPendingMovements consumes the
//     PENDING intent rows Phase-03's SettlementService writes into
//     nostro_movements (migration 112 — PAY leg DEBITs the nostro,
//     RECEIVE leg CREDITs it, spec §17.1) and applies the
//     nostro_accounts.balance mutation inside one SERIALIZABLE tx per
//     movement. The bank-statement polling seam (NostroStatementSource in
//     reconciliation.go) is the second balance-tracking input.
//   - A DEBIT that would push the nostro balance below zero is still
//     posted — the payment already happened at the correspondent — but
//     lands a durable P1 NOSTRO_OVERDRAWN alert (funding_ops_alerts
//     trail, same convention as Phase-11 Task 11.3.6). Hiding the
//     overdraft would break reconciliation forever.
//
// Fail-closed (spec §2.7): unknown roles/currencies, duplicate registry
// rows and store failures are coded errors — never silent skips.
package backoffice

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Scaffold error codes — registered in internal/errs (localRows) with
// this phase's landing.
const (
	// CodeNostroAccountExists: the (currency, bank, account) registry
	// row already exists — dedup is enforced, never double-counted.
	CodeNostroAccountExists = "NOSTRO_ACCOUNT_EXISTS"
	// CodeNostroNotFound: account id does not resolve.
	CodeNostroNotFound = "NOSTRO_ACCOUNT_NOT_FOUND"
	// CodeNostroOverdrawn: posting a DEBIT pushed a nostro balance below
	// zero — durable P1 ops alert code (never an HTTP rejection; the
	// payment already happened).
	CodeNostroOverdrawn = "NOSTRO_OVERDRAWN"
)

// OpsAlertRow is the durable ops-alert row landed in
// funding_ops_alerts — the same alert trail Phase-11 Task 11.3.6
// established for the nostro domain (migration 199).
type OpsAlertRow struct {
	Code                 string           `json:"code"`
	Severity             string           `json:"severity"` // P0|P1|P2|P3
	FundingTransactionID *int64           `json:"funding_transaction_id,omitempty"`
	AccountID            *int64           `json:"account_id,omitempty"`
	Currency             *string          `json:"currency,omitempty"`
	Amount               *decimal.Decimal `json:"amount,omitempty"`
	Summary              string           `json:"summary"`
	Detail               []byte           `json:"detail,omitempty"` // JSONB — carries dedup_key
}

// AccountRole mirrors nostro_account_role_enum (migration 261).
type AccountRole string

const (
	RoleNostro AccountRole = "NOSTRO" // our account at the correspondent bank
	RoleVostro AccountRole = "VOSTRO" // correspondent's account with us
)

// NostroAccount is the full nostro_accounts row including role+balance.
type NostroAccount struct {
	ID            int64           `json:"id"`
	Currency      string          `json:"currency"`
	BankName      string          `json:"bank_name"`
	BankCode      string          `json:"bank_code,omitempty"` // SWIFT BIC / routing code
	AccountNumber string          `json:"account_number,omitempty"`
	IBAN          string          `json:"iban,omitempty"`
	Role          AccountRole     `json:"role"`
	Balance       decimal.Decimal `json:"balance"` // shopspring marshals decimal as a JSON string
	Status        string          `json:"status"`  // ACTIVE|SUSPENDED|CLOSED (nostro_status_enum 018)
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// CreateAccountInput is the POST /api/v1/admin/nostro-accounts body.
type CreateAccountInput struct {
	Currency      string
	BankName      string
	BankCode      string
	AccountNumber string
	IBAN          string
	Role          AccountRole
}

// AccountFilter scopes ListAccounts — empty fields are wildcards.
type AccountFilter struct {
	Currency string
	Role     AccountRole
	Status   string
	Limit    int
}

// NostroMovement is the nostro_movements intent row Phase-03 writes on
// settlement confirmation; this package owns the POSTED transition and
// the balance application (Phase-24 seam).
type NostroMovement struct {
	ID                      int64           `json:"id"`
	SettlementInstructionID int64           `json:"settlement_instruction_id"`
	NostroAccountID         int64           `json:"nostro_account_id"`
	Currency                string          `json:"currency"`
	Amount                  decimal.Decimal `json:"amount"`
	Direction               string          `json:"direction"` // DEBIT|CREDIT (nostro_movement_direction_enum)
	Status                  string          `json:"status"`    // PENDING|POSTED|VOID
	ConfirmationRef         string          `json:"confirmation_ref,omitempty"`
	// SwiftMessageID is the settlement leg's :20: reference (joined from
	// settlement_instructions) — the reconciliation auto-match key.
	SwiftMessageID string     `json:"swift_message_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	PostedAt       *time.Time `json:"posted_at,omitempty"`
}

// signedAmount returns +amount for CREDIT, −amount for DEBIT.
func (m NostroMovement) signedAmount() decimal.Decimal {
	if m.Direction == "DEBIT" {
		return m.Amount.Neg()
	}
	return m.Amount
}

// ---------------------------------------------------------------------------
// Store seam — PgNostroStore implements it over pgx; tests fake it.
// ---------------------------------------------------------------------------

// NostroStore is the persistence seam for account management and the
// movement poster.
type NostroStore interface {
	// InsertAccount writes the registry row; the (currency, bank,
	// account) unique index makes re-creation a conflict.
	InsertAccount(ctx context.Context, a *NostroAccount) error
	// ListAccounts returns registry rows matching the filter.
	ListAccounts(ctx context.Context, f AccountFilter) ([]NostroAccount, error)
	// PendingMovements returns the oldest unposted intent rows.
	PendingMovements(ctx context.Context, limit int) ([]NostroMovement, error)
	// MovementForInstruction resolves the movement row written for a
	// settlement leg (nil when none recorded).
	MovementForInstruction(ctx context.Context, instructionID int64) (*NostroMovement, error)
	// InTx runs fn inside a SERIALIZABLE transaction.
	InTx(ctx context.Context, fn func(ctx context.Context, tx NostroTx) error) error
}

// NostroTx is the transactional view inside NostroStore.InTx.
type NostroTx interface {
	// MovementForUpdate SELECTs the movement FOR UPDATE.
	MovementForUpdate(ctx context.Context, id int64) (*NostroMovement, error)
	// AccountForUpdate SELECTs the nostro account FOR UPDATE.
	AccountForUpdate(ctx context.Context, id int64) (*NostroAccount, error)
	// ApplyBalance adds delta (signed) to nostro_accounts.balance and
	// returns the post-update balance.
	ApplyBalance(ctx context.Context, accountID int64, delta decimal.Decimal) (decimal.Decimal, error)
	// MarkMovementPosted flips PENDING→POSTED; false when the row was
	// already out of PENDING (idempotent replay guard).
	MarkMovementPosted(ctx context.Context, id int64, at time.Time) (bool, error)
	// InsertAlert lands a durable funding_ops_alerts row.
	InsertAlert(ctx context.Context, a OpsAlertRow) (int64, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// NostroService owns account registry writes, balance reads and the
// nostro_movements → balance poster (the Phase-24 seam the settlement
// service declared).
type NostroService struct {
	store NostroStore
	clock func() time.Time
	logf  func(format string, args ...any)
}

// NewNostroService wires the service; the store is required.
func NewNostroService(store NostroStore) (*NostroService, error) {
	if store == nil {
		return nil, fmt.Errorf("backoffice: nil nostro store")
	}
	return &NostroService{store: store, clock: time.Now}, nil
}

// WithClock overrides the clock (tests).
func (s *NostroService) WithClock(c func() time.Time) *NostroService {
	s.clock = c
	return s
}

// WithLogger wires a log sink.
func (s *NostroService) WithLogger(f func(format string, args ...any)) *NostroService {
	s.logf = f
	return s
}

func (s *NostroService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// CreateAccount registers a nostro/vostro account (Finance Ops+).
// Validates fail-closed: 3-letter currency, known role, a bank name, and
// at least one account locator (number or IBAN) — a correspondent row
// with no locator can never receive a wire.
func (s *NostroService) CreateAccount(ctx context.Context, in CreateAccountInput) (*NostroAccount, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.BankCode = strings.ToUpper(strings.TrimSpace(in.BankCode))
	in.IBAN = strings.ToUpper(strings.TrimSpace(in.IBAN))
	if !currencyRe.MatchString(in.Currency) {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("currency %q must be a 3-letter ISO code", in.Currency))
	}
	role := in.Role
	if role == "" {
		role = RoleNostro
	}
	if role != RoleNostro && role != RoleVostro {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("role %q must be NOSTRO or VOSTRO", in.Role))
	}
	if in.BankName == "" || len(in.BankName) > 128 {
		return nil, excerrors.New("INVALID_REQUEST",
			"bank_name is required (max 128 chars)")
	}
	if in.AccountNumber == "" && in.IBAN == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"account_number or iban is required — a correspondent row needs a locator")
	}
	if len(in.BankCode) > 32 || len(in.AccountNumber) > 64 || len(in.IBAN) > 34 {
		return nil, excerrors.New("INVALID_REQUEST",
			"bank_code/account_number/iban exceed column limits")
	}
	a := &NostroAccount{
		Currency:      in.Currency,
		BankName:      in.BankName,
		BankCode:      in.BankCode,
		AccountNumber: in.AccountNumber,
		IBAN:          in.IBAN,
		Role:          role,
		Status:        "ACTIVE",
	}
	if err := s.store.InsertAccount(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// ListAccounts returns the registry with balances.
func (s *NostroService) ListAccounts(ctx context.Context, f AccountFilter) ([]NostroAccount, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 500
	}
	rows, err := s.store.ListAccounts(ctx, f)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []NostroAccount{}
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// Movement poster — the nostro_accounts.balance mutation Phase-03
// declared as the Phase-24 seam.
// ---------------------------------------------------------------------------

// PostResult reports one posting outcome.
type PostResult struct {
	MovementID   int64           `json:"movement_id"`
	Posted       bool            `json:"posted"` // false = already POSTED (idempotent replay)
	Overdrawn    bool            `json:"overdrawn,omitempty"`
	BalanceAfter decimal.Decimal `json:"balance_after"`
}

// PostMovement applies one nostro_movements intent row to
// nostro_accounts.balance inside a SERIALIZABLE tx: DEBIT subtracts,
// CREDIT adds. A negative result posts anyway — the correspondent-side
// payment is a fact — and lands a durable P1 NOSTRO_OVERDRAWN alert.
// Idempotent: a non-PENDING row returns Posted=false with the current
// balance untouched.
func (s *NostroService) PostMovement(ctx context.Context, movementID int64) (*PostResult, error) {
	res := &PostResult{MovementID: movementID}
	err := s.store.InTx(ctx, func(ctx context.Context, tx NostroTx) error {
		m, err := tx.MovementForUpdate(ctx, movementID)
		if err != nil {
			return err
		}
		if m == nil {
			return excerrors.New("NOT_FOUND",
				fmt.Sprintf("nostro movement %d not found", movementID))
		}
		if m.Status != "PENDING" {
			res.Posted = false
			return nil // idempotent replay — never double-apply
		}
		acct, err := tx.AccountForUpdate(ctx, m.NostroAccountID)
		if err != nil {
			return err
		}
		if acct == nil {
			return excerrors.New(CodeNostroNotFound,
				fmt.Sprintf("nostro account %d not found", m.NostroAccountID))
		}
		bal, err := tx.ApplyBalance(ctx, acct.ID, m.signedAmount())
		if err != nil {
			return err
		}
		res.BalanceAfter = bal
		ok, err := tx.MarkMovementPosted(ctx, movementID, s.clock().UTC())
		if err != nil {
			return err
		}
		if !ok {
			return excerrors.New("INTERNAL_ERROR", fmt.Sprintf(
				"movement %d lost PENDING status mid-post", movementID))
		}
		res.Posted = true
		if bal.IsNegative() {
			res.Overdrawn = true
			ccy := acct.Currency
			detail := fmt.Sprintf(
				`{"movement_id":%d,"nostro_account_id":%d,"dedup_key":"nostro-overdrawn:%d"}`,
				m.ID, acct.ID, m.ID)
			if _, err := tx.InsertAlert(ctx, OpsAlertRow{
				Code:     CodeNostroOverdrawn,
				Severity: "P1",
				Currency: &ccy,
				Amount:   &m.Amount,
				Summary: fmt.Sprintf(
					"nostro %d (%s %s) overdrawn to %s by movement %d",
					acct.ID, acct.BankName, acct.Currency, bal.String(), m.ID),
				Detail: []byte(detail),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// PostMovementForInstruction posts the movement recorded for one
// settlement leg — the confirmation path calls this right after the leg
// flips SETTLED. No movement row (leg confirmed before the intent
// pipeline existed) is a no-op, not an error — the instruction state is
// the record of truth.
func (s *NostroService) PostMovementForInstruction(ctx context.Context, instructionID int64) (*PostResult, error) {
	m, err := s.store.MovementForInstruction(ctx, instructionID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return &PostResult{Posted: false}, nil
	}
	return s.PostMovement(ctx, m.ID)
}

// PostPendingMovements drains the PENDING intent queue — the sweep
// running on the settlement-date scheduler cadence. Per-item errors are
// logged and counted, never abort the pass.
func (s *NostroService) PostPendingMovements(ctx context.Context, limit int) (posted int, errs []error, err error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.store.PendingMovements(ctx, limit)
	if err != nil {
		return 0, nil, err
	}
	for _, m := range rows {
		res, perr := s.PostMovement(ctx, m.ID)
		if perr != nil {
			s.log("backoffice: post movement %d: %v", m.ID, perr)
			errs = append(errs, perr)
			continue
		}
		if res.Posted {
			posted++
		}
	}
	return posted, errs, nil
}

// ---------------------------------------------------------------------------
// PgNostroStore — PostgreSQL implementation (numerics cross the wire as text,
// the package convention).
// ---------------------------------------------------------------------------

// PgNostroStore implements every backoffice persistence seam over pgx.
type PgNostroStore struct {
	Pool *pgxpool.Pool
}

// Compile-time seam proofs — PgNostroStore backs the account registry,
// the SWIFT journal, the confirmation path and the recon store.
var (
	_ NostroStore       = (*PgNostroStore)(nil)
	_ SwiftStore        = (*PgNostroStore)(nil)
	_ ConfirmationStore = (*PgNostroStore)(nil)
	_ NostroReconStore  = (*PgNostroStore)(nil)
)

// NewPgNostroStore wires the store.
func NewPgNostroStore(pool *pgxpool.Pool) *PgNostroStore { return &PgNostroStore{Pool: pool} }

const nostroAccountCols = `id, currency, bank_name, COALESCE(bank_code,''),
	COALESCE(account_number,''), COALESCE(iban,''),
	COALESCE(account_role::text,'NOSTRO'), balance::text, status::text,
	created_at, updated_at`

func scanAccount(row pgx.Row) (*NostroAccount, error) {
	var a NostroAccount
	var bal string
	err := row.Scan(&a.ID, &a.Currency, &a.BankName, &a.BankCode,
		&a.AccountNumber, &a.IBAN, (*string)(&a.Role), &bal, &a.Status,
		&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.Balance, err = decimal.NewFromString(bal)
	if err != nil {
		return nil, fmt.Errorf("nostro account %d balance %q: %w", a.ID, bal, err)
	}
	return &a, nil
}

func (s *PgNostroStore) InsertAccount(ctx context.Context, a *NostroAccount) error {
	row := s.Pool.QueryRow(ctx, `
		INSERT INTO nostro_accounts
		    (currency, bank_name, bank_code, account_number, iban, account_role, status)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6::nostro_account_role_enum,'ACTIVE')
		RETURNING `+nostroAccountCols,
		a.Currency, a.BankName, a.BankCode, a.AccountNumber, a.IBAN,
		string(a.Role))
	out, err := scanAccount(row)
	if err != nil {
		if isUniqueViolation(err) {
			return excerrors.New(CodeNostroAccountExists, fmt.Sprintf(
				"nostro account for %s at %s already registered",
				a.Currency, a.BankName))
		}
		return fmt.Errorf("insert nostro account: %w", err)
	}
	*a = *out
	return nil
}

func isUniqueViolation(err error) bool {
	type sqlState interface {
		SQLState() string
	}
	for e := err; e != nil; {
		if se, ok := e.(sqlState); ok && se.SQLState() == "23505" {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return false
}

func (s *PgNostroStore) ListAccounts(ctx context.Context, f AccountFilter) ([]NostroAccount, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+nostroAccountCols+`
		  FROM nostro_accounts
		 WHERE ($1 = '' OR currency = $1)
		   AND ($2 = '' OR account_role::text = $2)
		   AND ($3 = '' OR status::text = $3)
		 ORDER BY currency, bank_name, id
		 LIMIT $4`, f.Currency, string(f.Role), f.Status, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NostroAccount
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

const movementCols = `m.id, m.settlement_instruction_id, m.nostro_account_id,
	m.currency, m.amount::text, m.direction::text, m.status::text,
	COALESCE(m.confirmation_ref,''), COALESCE(si.swift_message_id,''),
	m.created_at, m.posted_at`

func scanMovement(row pgx.Row) (*NostroMovement, error) {
	var m NostroMovement
	var amt string
	err := row.Scan(&m.ID, &m.SettlementInstructionID, &m.NostroAccountID,
		&m.Currency, &amt, &m.Direction, &m.Status, &m.ConfirmationRef,
		&m.SwiftMessageID, &m.CreatedAt, &m.PostedAt)
	if err != nil {
		return nil, err
	}
	m.Amount, err = decimal.NewFromString(amt)
	if err != nil {
		return nil, fmt.Errorf("movement %d amount %q: %w", m.ID, amt, err)
	}
	return &m, nil
}

func (s *PgNostroStore) PendingMovements(ctx context.Context, limit int) ([]NostroMovement, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+movementCols+`
		  FROM nostro_movements m
		  LEFT JOIN settlement_instructions si
		    ON si.id = m.settlement_instruction_id
		 WHERE m.status = 'PENDING'
		 ORDER BY m.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NostroMovement
	for rows.Next() {
		m, err := scanMovement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *PgNostroStore) MovementForInstruction(ctx context.Context, instructionID int64) (*NostroMovement, error) {
	m, err := scanMovement(s.Pool.QueryRow(ctx, `
		SELECT `+movementCols+`
		  FROM nostro_movements m
		  LEFT JOIN settlement_instructions si
		    ON si.id = m.settlement_instruction_id
		 WHERE m.settlement_instruction_id = $1`, instructionID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return m, err
}

func (s *PgNostroStore) InTx(ctx context.Context, fn func(ctx context.Context, tx NostroTx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("backoffice tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxNostroTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("backoffice tx commit: %w", err)
	}
	return nil
}

type pgxNostroTx struct{ tx pgx.Tx }

func (t pgxNostroTx) MovementForUpdate(ctx context.Context, id int64) (*NostroMovement, error) {
	m, err := scanMovement(t.tx.QueryRow(ctx, `
		SELECT `+movementCols+`
		  FROM nostro_movements m
		  LEFT JOIN settlement_instructions si
		    ON si.id = m.settlement_instruction_id
		 WHERE m.id = $1 FOR UPDATE OF m`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return m, err
}

func (t pgxNostroTx) AccountForUpdate(ctx context.Context, id int64) (*NostroAccount, error) {
	a, err := scanAccount(t.tx.QueryRow(ctx, `
		SELECT `+nostroAccountCols+`
		  FROM nostro_accounts WHERE id = $1 FOR UPDATE`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (t pgxNostroTx) ApplyBalance(ctx context.Context, accountID int64, delta decimal.Decimal) (decimal.Decimal, error) {
	var bal string
	err := t.tx.QueryRow(ctx, `
		UPDATE nostro_accounts
		   SET balance = balance + $2::numeric, updated_at = now()
		 WHERE id = $1 RETURNING balance::text`,
		accountID, delta.String()).Scan(&bal)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromString(bal)
}

func (t pgxNostroTx) MarkMovementPosted(ctx context.Context, id int64, at time.Time) (bool, error) {
	tag, err := t.tx.Exec(ctx, `
		UPDATE nostro_movements SET status = 'POSTED', posted_at = $2
		 WHERE id = $1 AND status = 'PENDING'`, id, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (t pgxNostroTx) InsertAlert(ctx context.Context, a OpsAlertRow) (int64, error) {
	var id int64
	var amt *string
	if a.Amount != nil {
		s := a.Amount.String()
		amt = &s
	}
	err := t.tx.QueryRow(ctx, `
		INSERT INTO funding_ops_alerts
		    (code, severity, funding_transaction_id, account_id, currency,
		     amount, summary, detail)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8) RETURNING id`,
		a.Code, a.Severity, a.FundingTransactionID, a.AccountID, a.Currency,
		amt, a.Summary, a.Detail).Scan(&id)
	return id, err
}
