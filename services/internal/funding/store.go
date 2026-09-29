// Store layer for the funding package: one Store interface (services stay
// unit-testable) plus PgStore, the pgx implementation.
//
// Conventions:
//   - DECIMAL columns cross the wire as text (::text out, string →
//     ::numeric in) — the repo-wide convention that avoids the pgtype
//     decimal shim.
//   - Every method that participates in a multi-statement transition
//     takes an explicit pgx.Tx; the service opens the tx with
//     BeginTx(SERIALIZABLE) — spec §5.3/§14.6 isolation contract.
//   - Account-scoped idempotency conflicts surface as ErrIdemConflict;
//     the service resolves them into replay-or-422.
package funding

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// ErrIdemConflict wraps a 23505 unique-violation on an account-scoped
// idempotency key — the service resolves it into replay-or-mismatch.
var ErrIdemConflict = stderrors.New("funding: idempotency key conflict")

// isUniqueViolation reports whether err is a PostgreSQL 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return stderrors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---------------------------------------------------------------------------
// Row types
// ---------------------------------------------------------------------------

// AccountMeta is the account-ownership + state projection the funding
// services need (no sensitive columns).
type AccountMeta struct {
	ID           int64
	UserID       int64
	ParentID     *int64
	Status       string // ACTIVE | SUSPENDED | FROZEN | CLOSED
	KYCTier      string
	BaseCurrency string
}

// BalanceRow mirrors one balances row (total is generated).
type BalanceRow struct {
	Currency  string          `json:"currency"`
	Available decimal.Decimal `json:"available"`
	Locked    decimal.Decimal `json:"locked"`
	Total     decimal.Decimal `json:"total"`
}

// PositionRow is one open position joined to its instrument symbol.
type PositionRow struct {
	ID               int64            `json:"position_id"`
	InstrumentID     int64            `json:"instrument_id"`
	Symbol           string           `json:"symbol"`
	Side             string           `json:"side"` // LONG | SHORT
	Quantity         decimal.Decimal  `json:"quantity"`
	EntryPrice       decimal.Decimal  `json:"entry_price"`
	MarkPrice        *decimal.Decimal `json:"mark_price,omitempty"`
	UnrealizedPnL    decimal.Decimal  `json:"unrealized_pnl"`
	RealizedPnL      decimal.Decimal  `json:"realized_pnl"`
	LiquidationPrice *decimal.Decimal `json:"liquidation_price,omitempty"`
	MarginUsed       decimal.Decimal  `json:"margin_used"`
	OpenedAt         time.Time        `json:"opened_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

// NostroAccount is one correspondent-bank deposit destination (§5.18).
type NostroAccount struct {
	ID            int64     `json:"id"`
	Currency      string    `json:"currency"`
	BankName      string    `json:"bank_name"`
	BankCode      string    `json:"bank_code,omitempty"` // SWIFT BIC / routing
	AccountNumber string    `json:"account_number,omitempty"`
	IBAN          string    `json:"iban,omitempty"`
	Status        string    `json:"status"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// FundingTxRow is the funding_transactions projection returned by the
// funding-history endpoint.
type FundingTxRow struct {
	ID               int64            `json:"id"`
	AccountID        int64            `json:"account_id"`
	Currency         string           `json:"currency"`
	Type             string           `json:"type"`
	Amount           decimal.Decimal  `json:"amount"`
	Status           string           `json:"status"`
	Reference        *string          `json:"reference,omitempty"`
	BankMethod       *string          `json:"bank_method,omitempty"`
	ReferenceAccount *string          `json:"reference_account,omitempty"`
	USDAmount        *decimal.Decimal `json:"usd_amount,omitempty"`
	ReviewTier       *string          `json:"review_tier,omitempty"`
	ReviewDeadline   *time.Time       `json:"review_deadline,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	ConfirmedAt      *time.Time       `json:"confirmed_at,omitempty"`
	CompletedAt      *time.Time       `json:"completed_at,omitempty"`
}

// WithdrawalRow is the mutable withdrawal record the service works on.
type WithdrawalRow struct {
	ID               int64
	AccountID        int64
	Currency         string
	Amount           decimal.Decimal
	Status           string
	ReferenceAccount *string
	BankMethod       *string
	IdempotencyKey   *string
	PayloadSHA256    *string
	USDAmount        *decimal.Decimal
	ReviewTier       *string
	CreatedAt        time.Time
	ReviewDeadline   *time.Time
}

// DepositRow is the funding_transactions DEPOSIT insert the deposit
// guard and rail flows persist (account may be 0 for unreferenced wires —
// those carry NULL account_id via the suspense link only, never a 0 FK).
type DepositRow struct {
	ID               int64
	AccountID        int64
	Currency         string
	Amount           decimal.Decimal
	Status           string // PENDING | PENDING_REVIEW (quarantined)
	BankMethod       *string
	Reference        *string // bank_tx_id / wire reference
	ReferenceAccount *string // originator account
	IdempotencyKey   *string
	PayloadSHA256    *string
	USDAmount        *decimal.Decimal
	ReviewTier       *string
	CreatedAt        time.Time
}

// ConfirmationRow is the withdrawal_confirmations row (token material is
// hash-only — the plaintext token is never persisted).
type ConfirmationRow struct {
	ID           int64
	WithdrawalID int64
	Status       string // pending | confirmed | cancelled
	TokenHash    *string
	ExpiresAt    time.Time
	ConfirmedBy  *int64
	Method       *string
}

// TransferRow is the transfers journal row (Task 5.3.23/5.3.45).
type TransferRow struct {
	ID             int64           `json:"id"`
	AccountID      int64           `json:"account_id"` // initiating account
	FromAccountID  int64           `json:"from_account_id"`
	ToAccountID    int64           `json:"to_account_id"`
	Currency       string          `json:"currency"`
	Amount         decimal.Decimal `json:"amount"`
	Status         string          `json:"status"`
	Actor          string          `json:"actor"` // SELF | ADMIN | SYSTEM
	ActorID        *int64          `json:"actor_id,omitempty"`
	JournalEntryID *int64          `json:"journal_entry_id,omitempty"` // GL reference
	FailureReason  *string         `json:"failure_reason,omitempty"`
	IdempotencyKey *string         `json:"idempotency_key,omitempty"`
	PayloadSHA256  string          `json:"-"`
	CreatedAt      time.Time       `json:"created_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

// ChargebackRow is the dispute record (Task 5.3.18).
type ChargebackRow struct {
	ID                   int64           `json:"id"`
	AccountID            int64           `json:"account_id"`
	FundingTransactionID *int64          `json:"funding_transaction_id,omitempty"`
	CardNetwork          *string         `json:"card_network,omitempty"`
	Currency             string          `json:"currency"`
	Amount               decimal.Decimal `json:"amount"`
	Reason               string          `json:"reason"`
	Status               string          `json:"status"`
	OpenedBy             int64           `json:"opened_by"`
	OpenedAt             time.Time       `json:"opened_at"`
	SubmittedAt          *time.Time      `json:"submitted_at,omitempty"`
	ResolvedAt           *time.Time      `json:"resolved_at,omitempty"`
	ResolutionNote       *string         `json:"resolution_note,omitempty"`
}

// EvidenceRow is one chargeback_evidence row.
type EvidenceRow struct {
	ID           int64     `json:"id"`
	ChargebackID int64     `json:"chargeback_id"`
	Kind         string    `json:"kind"`
	Payload      []byte    `json:"payload"` // JSONB
	PayloadSHA   string    `json:"payload_sha256"`
	CollectedBy  *int64    `json:"collected_by,omitempty"`
	CollectedAt  time.Time `json:"collected_at"`
}

// SuspenseRow is one suspense_account_mappings row (migration 108, spec
// §5.46) — the compliance quarantine record for inbound deposits whose
// originator failed the third-party name-match or could not be attributed.
type SuspenseRow struct {
	ID                   int64           `json:"id"`
	BankTxID             string          `json:"bank_tx_id"`
	FundingTransactionID *int64          `json:"funding_transaction_id,omitempty"`
	AccountID            *int64          `json:"account_id,omitempty"`
	Rail                 *string         `json:"rail,omitempty"`
	Currency             string          `json:"currency"`
	Amount               decimal.Decimal `json:"amount"`
	OriginatorName       *string         `json:"originator_name,omitempty"`
	OriginatorAccount    *string         `json:"originator_account,omitempty"`
	NameMatchScore       *float64        `json:"name_match_score,omitempty"`
	UnmatchedReason      string          `json:"unmatched_reason"` // MISSING_REFERENCE|UNKNOWN_BENEFICIARY|NAME_MISMATCH|AMOUNT_DISCREPANCY
	GLAccount            string          `json:"gl_account"`       // 2150_SUSPENSE_DEPOSITS_{CCY}
	QuarantineStatus     string          `json:"quarantine_status"`
	QuarantinedAt        time.Time       `json:"quarantined_at"`
	SLAExpiresAt         time.Time       `json:"sla_expires_at"`
	AssignedInvestigator *int64          `json:"assigned_investigator_id,omitempty"`
	ResolutionNotes      *string         `json:"resolution_notes,omitempty"`
	ResolvedAt           *time.Time      `json:"resolved_at,omitempty"`
	JournalEntryID       *int64          `json:"journal_entry_id,omitempty"`
	ReturnPaymentID      *int64          `json:"return_payment_id,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
}

// RailPaymentRow is one rail_payments row (migration 108) — a persisted
// outbound rail instruction envelope (MT103/pacs.008 family) or the
// automated return wire (pacs.004/NACHA return) the deposit guard emits.
type RailPaymentRow struct {
	ID                   int64      `json:"id"`
	FundingTransactionID *int64     `json:"funding_transaction_id,omitempty"`
	SuspenseMappingID    *int64     `json:"suspense_mapping_id,omitempty"`
	Direction            string     `json:"direction"` // OUTBOUND | RETURN
	Rail                 string     `json:"rail"`
	MessageType          string     `json:"message_type"`
	EndToEndID           string     `json:"end_to_end_id"`
	UETR                 *string    `json:"uetr,omitempty"`
	Envelope             []byte     `json:"envelope"` // JSONB — typed wire fields
	Status               string     `json:"status"`
	ReturnCode           *string    `json:"return_code,omitempty"`
	ReturnReason         *string    `json:"return_reason,omitempty"`
	ValueDate            *time.Time `json:"value_date,omitempty"`
	DispatchedAt         *time.Time `json:"dispatched_at,omitempty"`
	SettledAt            *time.Time `json:"settled_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

// SuspenseFilter scopes GET /api/v1/admin/funding/quarantine.
type SuspenseFilter struct {
	AccountID *int64
	Status    string
	CursorTS  *time.Time // keyset (quarantined_at, id) <
	CursorID  int64
	Limit     int
}

// ---------------------------------------------------------------------------
// Filters (cursor-paginated, spec §8.8 envelope)
// ---------------------------------------------------------------------------

// FundingFilter scopes GET /api/v1/funding.
type FundingFilter struct {
	Type     string // DEPOSIT | WITHDRAWAL | … (optional)
	Currency string
	Status   string
	From     *time.Time
	To       *time.Time
	CursorTS *time.Time // keyset (created_at, id) <
	CursorID int64
	Limit    int
}

// TransferFilter scopes GET /api/v1/transfers.
type TransferFilter struct {
	PerspectiveID int64  // 0 = any caller-owned account
	Direction     string // "" | IN | OUT — relative to PerspectiveID or the visibility set
	Currency      string
	From          *time.Time
	To            *time.Time
	CursorTS      *time.Time
	CursorID      int64
	Limit         int
}

// ChargebackFilter scopes GET /api/v1/admin/chargebacks.
type ChargebackFilter struct {
	AccountID *int64
	Status    string
	CursorTS  *time.Time
	CursorID  int64
	Limit     int
}

// ---------------------------------------------------------------------------
// Store interface
// ---------------------------------------------------------------------------

// Store is the persistence seam. PgStore implements it over pgxpool; unit
// tests substitute fakes for the methods their subject reaches.
type Store interface {
	// BeginTx opens a SERIALIZABLE transaction (spec §14.6 isolation for
	// funding state transitions).
	BeginTx(ctx context.Context) (pgx.Tx, error)

	AccountMeta(ctx context.Context, id int64) (*AccountMeta, error)
	UserAccountIDs(ctx context.Context, userID int64) ([]int64, error)
	NostroAccounts(ctx context.Context, currency string) ([]NostroAccount, error)
	BalancesFor(ctx context.Context, accountID int64) ([]BalanceRow, error)
	PositionsFor(ctx context.Context, accountID int64) ([]PositionRow, error)

	InsertWithdrawal(ctx context.Context, tx pgx.Tx, w WithdrawalRow) (*WithdrawalRow, error)
	WithdrawalByIdemKey(ctx context.Context, accountID int64, key string) (*WithdrawalRow, error)
	WithdrawalForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*WithdrawalRow, error)
	WithdrawalStatus(ctx context.Context, id int64) (string, error)
	SetWithdrawalStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
		confirmedAt, reviewDeadline *time.Time) error
	MarkWithdrawalFailed(ctx context.Context, id int64) error
	InsertConfirmation(ctx context.Context, tx pgx.Tx, c ConfirmationRow) (int64, error)
	ConfirmationForUpdate(ctx context.Context, tx pgx.Tx, withdrawalID int64) (*ConfirmationRow, error)
	SetConfirmationStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
		confirmedBy int64, method string) error
	ExpiredPendingConfirmations(ctx context.Context, limit int) ([]int64, error)
	FundingHistory(ctx context.Context, accountIDs []int64, f FundingFilter) ([]FundingTxRow, int64, error)

	InsertTransfer(ctx context.Context, t TransferRow) (*TransferRow, error)
	TransferByIdemKey(ctx context.Context, accountID int64, key string) (*TransferRow, error)
	SetTransferResult(ctx context.Context, id int64, status string,
		journalID *int64, failure *string) error
	TransferHistory(ctx context.Context, accountIDs []int64, f TransferFilter) ([]TransferRow, int64, error)

	InsertChargeback(ctx context.Context, tx pgx.Tx, c ChargebackRow) (*ChargebackRow, error)
	ChargebackForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*ChargebackRow, error)
	Chargeback(ctx context.Context, id int64) (*ChargebackRow, error)
	UpdateChargebackStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
		submittedAt, resolvedAt *time.Time, note *string) error
	ListChargebacks(ctx context.Context, f ChargebackFilter) ([]ChargebackRow, int64, error)
	InsertEvidence(ctx context.Context, tx pgx.Tx, e EvidenceRow) error
	EvidenceFor(ctx context.Context, chargebackID int64) ([]EvidenceRow, error)
	EvidenceCount(ctx context.Context, tx pgx.Tx, chargebackID int64) (int, error)
	FundingTxSnapshot(ctx context.Context, id int64) ([]byte, error)
	TradesForEvidence(ctx context.Context, accountID int64,
		since, until time.Time, limit int) ([]EvidenceRow, error)
	FreezeEventsForEvidence(ctx context.Context, accountID int64, limit int) ([]EvidenceRow, error)
	AdminAudit(ctx context.Context, adminID int64, action, targetType string,
		targetID int64, before, after []byte, ip string) error

	// Phase-11 Task 11.3.1/11.3.11 — rails, returns & deposit quarantine.
	InsertDepositPending(ctx context.Context, tx pgx.Tx, d DepositRow) (*DepositRow, error)
	FundingTxForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*FundingTxRow, error)
	SetFundingTxStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
		completedAt *time.Time) error
	InsertSuspenseMapping(ctx context.Context, tx pgx.Tx, m SuspenseRow) (*SuspenseRow, error)
	SuspenseByBankTx(ctx context.Context, bankTxID string) (*SuspenseRow, error)
	SuspenseForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*SuspenseRow, error)
	SetSuspenseStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
		investigatorID *int64, notes *string, resolvedAt *time.Time) error
	SetSuspenseLinks(ctx context.Context, tx pgx.Tx, id int64,
		journalID, returnPaymentID *int64) error
	ListSuspense(ctx context.Context, f SuspenseFilter) ([]SuspenseRow, int64, error)
	InsertRailPayment(ctx context.Context, tx pgx.Tx, p RailPaymentRow) (*RailPaymentRow, error)
	RailPaymentForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*RailPaymentRow, error)
	RailPaymentByEndToEndID(ctx context.Context, endToEndID string) (*RailPaymentRow, error)
	SetRailPaymentStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
		returnCode, returnReason *string, dispatchedAt, settledAt *time.Time) error
}

// ---------------------------------------------------------------------------
// PgStore
// ---------------------------------------------------------------------------

// PgStore implements Store over the shared pgx pool. It performs no
// balance mutations — every money movement goes through the ledger.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore wires the store to the gateway's pool.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

// BeginTx opens a SERIALIZABLE transaction — the spec §14.6 isolation the
// funding/transfer state transitions require.
func (s *PgStore) BeginTx(ctx context.Context) (pgx.Tx, error) {
	return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
}

// AccountMeta reads the ownership/state projection for one account.
func (s *PgStore) AccountMeta(ctx context.Context, id int64) (*AccountMeta, error) {
	var m AccountMeta
	var parent *int64
	var base *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, parent_account_id, status::text, kyc_tier::text, base_currency
		FROM accounts WHERE id = $1`, id).
		Scan(&m.ID, &m.UserID, &parent, &m.Status, &m.KYCTier, &base)
	if err == pgx.ErrNoRows {
		return nil, errf("NOT_FOUND", "account %d not found", id)
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "account meta", err)
	}
	m.ParentID = parent
	if base != nil {
		m.BaseCurrency = *base
	}
	return &m, nil
}

// UserAccountIDs lists every account owned by userID (master + subs —
// subs inherit the master user_id at creation).
func (s *PgStore) UserAccountIDs(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM accounts WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "user accounts", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "user accounts scan", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// NostroAccounts lists ACTIVE nostro destinations for currency (§5.18;
// deposit instructions endpoint).
func (s *PgStore) NostroAccounts(ctx context.Context, currency string) ([]NostroAccount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, currency, bank_name, COALESCE(bank_code,''), COALESCE(account_number,''),
		       COALESCE(iban,''), status::text, updated_at
		FROM nostro_accounts
		WHERE currency = $1 AND status = 'ACTIVE'
		ORDER BY id`, currency)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "nostro accounts", err)
	}
	defer rows.Close()
	var out []NostroAccount
	for rows.Next() {
		var n NostroAccount
		if err := rows.Scan(&n.ID, &n.Currency, &n.BankName, &n.BankCode,
			&n.AccountNumber, &n.IBAN, &n.Status, &n.UpdatedAt); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "nostro scan", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// BalancesFor returns every balance row for the account (all currencies —
// Task 5.3.4 DoD).
func (s *PgStore) BalancesFor(ctx context.Context, accountID int64) ([]BalanceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, available::text, locked::text, total::text
		FROM balances WHERE account_id = $1 ORDER BY currency`, accountID)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "balances", err)
	}
	defer rows.Close()
	var out []BalanceRow
	for rows.Next() {
		var b BalanceRow
		var a, l, t string
		if err := rows.Scan(&b.Currency, &a, &l, &t); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "balances scan", err)
		}
		b.Available = decimal.RequireFromString(a)
		b.Locked = decimal.RequireFromString(l)
		b.Total = decimal.RequireFromString(t)
		out = append(out, b)
	}
	return out, rows.Err()
}

// PositionsFor returns open positions (quantity <> 0) joined to symbols.
func (s *PgStore) PositionsFor(ctx context.Context, accountID int64) ([]PositionRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.instrument_id, i.symbol, p.side::text,
		       p.quantity::text, p.entry_price::text, p.mark_price::text,
		       p.unrealized_pnl::text, p.realized_pnl::text,
		       p.liquidation_price::text, p.margin_used::text,
		       p.opened_at, p.updated_at
		FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1 AND p.quantity <> 0
		ORDER BY p.id`, accountID)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "positions", err)
	}
	defer rows.Close()
	var out []PositionRow
	for rows.Next() {
		var p PositionRow
		var qty, ep, upnl, rpnl, mu string
		var mp, lp *string
		if err := rows.Scan(&p.ID, &p.InstrumentID, &p.Symbol, &p.Side,
			&qty, &ep, &mp, &upnl, &rpnl, &lp, &mu,
			&p.OpenedAt, &p.UpdatedAt); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "positions scan", err)
		}
		p.Quantity = decimal.RequireFromString(qty)
		p.EntryPrice = decimal.RequireFromString(ep)
		if d, err := decText(mp); err == nil && mp != nil {
			p.MarkPrice = &d
		}
		p.UnrealizedPnL = decimal.RequireFromString(upnl)
		p.RealizedPnL = decimal.RequireFromString(rpnl)
		if d, err := decText(lp); err == nil && lp != nil {
			p.LiquidationPrice = &d
		}
		p.MarginUsed = decimal.RequireFromString(mu)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Withdrawals (Task 5.3.6)
// ---------------------------------------------------------------------------

// InsertWithdrawal inserts the funding_transactions row; a
// (account_id, idempotency_key) conflict surfaces as ErrIdemConflict.
func (s *PgStore) InsertWithdrawal(ctx context.Context, tx pgx.Tx, w WithdrawalRow) (*WithdrawalRow, error) {
	var idem, ph, tier *string
	if w.IdempotencyKey != nil {
		idem = w.IdempotencyKey
	}
	if w.PayloadSHA256 != nil {
		ph = w.PayloadSHA256
	}
	if w.ReviewTier != nil {
		tier = w.ReviewTier
	}
	var usd *string
	if w.USDAmount != nil {
		v := w.USDAmount.String()
		usd = &v
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO funding_transactions
		    (account_id, currency, type, amount, status, bank_method,
		     reference_account, idempotency_key, payload_sha256,
		     usd_amount, review_tier)
		VALUES ($1, $2, 'WITHDRAWAL', $3::numeric, 'PENDING', $4,
		        $5, $6, $7, $8::numeric, $9)
		RETURNING id, created_at`,
		w.AccountID, w.Currency, w.Amount.String(), w.BankMethod,
		w.ReferenceAccount, idem, ph, usd, tier).Scan(&w.ID, &w.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrIdemConflict
		}
		return nil, wrapCode("INTERNAL_ERROR", "insert withdrawal", err)
	}
	return &w, nil
}

// WithdrawalByIdemKey resolves a replayed key to the stored withdrawal.
func (s *PgStore) WithdrawalByIdemKey(ctx context.Context, accountID int64, key string) (*WithdrawalRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, account_id, currency, amount::text, status::text,
		       reference_account, bank_method::text, idempotency_key,
		       payload_sha256, usd_amount::text, review_tier,
		       created_at, review_deadline
		FROM funding_transactions
		WHERE account_id = $1 AND idempotency_key = $2 AND type = 'WITHDRAWAL'`,
		accountID, key)
	return scanWithdrawal(row)
}

// WithdrawalForUpdate locks the withdrawal row inside tx.
func (s *PgStore) WithdrawalForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*WithdrawalRow, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, account_id, currency, amount::text, status::text,
		       reference_account, bank_method::text, idempotency_key,
		       payload_sha256, usd_amount::text, review_tier,
		       created_at, review_deadline
		FROM funding_transactions
		WHERE id = $1 AND type = 'WITHDRAWAL' FOR UPDATE`, id)
	return scanWithdrawal(row)
}

// WithdrawalStatus returns the current status (status-only read for
// idempotent replay reporting).
func (s *PgStore) WithdrawalStatus(ctx context.Context, id int64) (string, error) {
	var st string
	err := s.pool.QueryRow(ctx,
		`SELECT status::text FROM funding_transactions WHERE id = $1`, id).Scan(&st)
	if err == pgx.ErrNoRows {
		return "", errf("NOT_FOUND", "withdrawal %d not found", id)
	}
	if err != nil {
		return "", wrapCode("INTERNAL_ERROR", "withdrawal status", err)
	}
	return st, nil
}

func scanWithdrawal(row pgx.Row) (*WithdrawalRow, error) {
	var w WithdrawalRow
	var amt, usd *string
	var method, ref, idem, ph, tier *string
	err := row.Scan(&w.ID, &w.AccountID, &w.Currency, &amt, &w.Status,
		&ref, &method, &idem, &ph, &usd, &tier, &w.CreatedAt, &w.ReviewDeadline)
	if err == pgx.ErrNoRows {
		return nil, errCode("NOT_FOUND", "withdrawal not found")
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "withdrawal scan", err)
	}
	if amt != nil {
		w.Amount = decimal.RequireFromString(*amt)
	}
	if usd != nil {
		d := decimal.RequireFromString(*usd)
		w.USDAmount = &d
	}
	w.ReferenceAccount, w.BankMethod = ref, method
	w.IdempotencyKey, w.PayloadSHA256, w.ReviewTier = idem, ph, tier
	return &w, nil
}

// SetWithdrawalStatus transitions the funding row inside tx.
func (s *PgStore) SetWithdrawalStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
	confirmedAt, reviewDeadline *time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE funding_transactions
		SET status = $2::funding_status_enum, updated_at = now(),
		    confirmed_at   = COALESCE($3, confirmed_at),
		    review_deadline = COALESCE($4, review_deadline)
		WHERE id = $1`, id, status, confirmedAt, reviewDeadline)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "withdrawal status update", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "withdrawal %d not found", id)
	}
	return nil
}

// MarkWithdrawalFailed records a create-time ledger failure (compensation
// row outside the posting tx — the journal never committed).
func (s *PgStore) MarkWithdrawalFailed(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE funding_transactions SET status = 'FAILED', updated_at = now()
		WHERE id = $1 AND status = 'PENDING'`, id)
	return err
}

// InsertConfirmation creates the pending confirmation row inside tx.
func (s *PgStore) InsertConfirmation(ctx context.Context, tx pgx.Tx, c ConfirmationRow) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO withdrawal_confirmations
		    (withdrawal_id, method, expires_at, token_hash, status)
		VALUES ($1, $2, $3, $4, 'pending') RETURNING id`,
		c.WithdrawalID, c.Method, c.ExpiresAt, c.TokenHash).Scan(&id)
	if err != nil {
		return 0, wrapCode("INTERNAL_ERROR", "insert confirmation", err)
	}
	return id, nil
}

// ConfirmationForUpdate locks the pending confirmation for a withdrawal.
func (s *PgStore) ConfirmationForUpdate(ctx context.Context, tx pgx.Tx, withdrawalID int64) (*ConfirmationRow, error) {
	var c ConfirmationRow
	err := tx.QueryRow(ctx, `
		SELECT id, withdrawal_id, status::text, token_hash, expires_at,
		       confirmed_by, method
		FROM withdrawal_confirmations
		WHERE withdrawal_id = $1 ORDER BY id DESC LIMIT 1 FOR UPDATE`,
		withdrawalID).
		Scan(&c.ID, &c.WithdrawalID, &c.Status, &c.TokenHash, &c.ExpiresAt,
			&c.ConfirmedBy, &c.Method)
	if err == pgx.ErrNoRows {
		return nil, errf("NOT_FOUND", "no confirmation for withdrawal %d", withdrawalID)
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "confirmation scan", err)
	}
	return &c, nil
}

// SetConfirmationStatus records the confirm/cancel outcome inside tx.
func (s *PgStore) SetConfirmationStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
	confirmedBy int64, method string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE withdrawal_confirmations
		SET status = $2::withdrawal_confirmation_status_enum,
		    confirmed_by = NULLIF($3, 0),
		    method = COALESCE(NULLIF($4, ''), method),
		    confirmed_at = CASE WHEN $2 = 'confirmed' THEN now() ELSE confirmed_at END
		WHERE id = $1`, id, status, confirmedBy, method)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "confirmation update", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "confirmation %d not found", id)
	}
	return nil
}

// ExpiredPendingConfirmations lists withdrawal ids whose confirmation
// window has lapsed — the expiry sweeper's work queue.
func (s *PgStore) ExpiredPendingConfirmations(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT wc.withdrawal_id
		FROM withdrawal_confirmations wc
		JOIN funding_transactions ft ON ft.id = wc.withdrawal_id
		WHERE wc.status = 'pending' AND wc.expires_at < now()
		  AND ft.status = 'PENDING'
		ORDER BY wc.withdrawal_id LIMIT $1`, limit)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "expired confirmations", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "expired scan", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FundingHistory pages funding_transactions for the caller's accounts —
// spec §8.8 keyset envelope (created_at, id), newest first. The count
// reflects the filtered universe; the cursor only windows the page.
func (s *PgStore) FundingHistory(ctx context.Context, accountIDs []int64, f FundingFilter) ([]FundingTxRow, int64, error) {
	where := []string{"account_id = ANY($1)"}
	args := []any{accountIDs}
	n := 1
	add := func(clause string, v any) {
		n++
		where = append(where, fmt.Sprintf(clause, n))
		args = append(args, v)
	}
	if f.Type != "" {
		add("type = $%d::funding_type_enum", f.Type)
	}
	if f.Currency != "" {
		add("currency = $%d", f.Currency)
	}
	if f.Status != "" {
		add("status = $%d::funding_status_enum", f.Status)
	}
	if f.From != nil {
		add("created_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("created_at <= $%d", *f.To)
	}
	cond := strings.Join(where, " AND ")

	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM funding_transactions WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, wrapCode("INTERNAL_ERROR", "funding history count", err)
	}
	if f.CursorTS != nil {
		n++
		cond += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", n, n+1)
		args = append(args, *f.CursorTS, f.CursorID)
		n++
	}
	n++
	q := fmt.Sprintf(`
		SELECT id, account_id, currency, type::text, amount::text, status::text,
		       reference, bank_method::text, reference_account,
		       usd_amount::text, review_tier, review_deadline,
		       created_at, confirmed_at, completed_at
		FROM funding_transactions
		WHERE %s
		ORDER BY created_at DESC, id DESC LIMIT $%d`, cond, n)
	args = append(args, f.Limit+1)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, wrapCode("INTERNAL_ERROR", "funding history", err)
	}
	defer rows.Close()
	var out []FundingTxRow
	for rows.Next() {
		var r FundingTxRow
		var amt, usd *string
		var ref, bm, ra, tier *string
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Currency, &r.Type, &amt,
			&r.Status, &ref, &bm, &ra, &usd, &tier, &r.ReviewDeadline,
			&r.CreatedAt, &r.ConfirmedAt, &r.CompletedAt); err != nil {
			return nil, 0, wrapCode("INTERNAL_ERROR", "funding scan", err)
		}
		if amt != nil {
			r.Amount = decimal.RequireFromString(*amt)
		}
		if usd != nil {
			d := decimal.RequireFromString(*usd)
			r.USDAmount = &d
		}
		r.Reference, r.BankMethod, r.ReferenceAccount, r.ReviewTier = ref, bm, ra, tier
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// ---------------------------------------------------------------------------
// Internal transfers (Task 5.3.23 + 5.3.45)
// ---------------------------------------------------------------------------

// InsertTransfer appends the PENDING journal row; an
// (account_id, idempotency_key) conflict returns ErrIdemConflict.
func (s *PgStore) InsertTransfer(ctx context.Context, t TransferRow) (*TransferRow, error) {
	var idem, ph *string
	if t.IdempotencyKey != nil {
		idem = t.IdempotencyKey
	}
	if t.PayloadSHA256 != "" {
		v := t.PayloadSHA256
		ph = &v
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO transfers
		    (account_id, from_account_id, to_account_id, currency, amount,
		     status, actor, actor_id, idempotency_key, payload_sha256)
		VALUES ($1, $2, $3, $4, $5::numeric, 'PENDING', $6, $7, $8, $9)
		RETURNING id, created_at`,
		t.AccountID, t.FromAccountID, t.ToAccountID, t.Currency,
		t.Amount.String(), t.Actor, t.ActorID, idem, ph).
		Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrIdemConflict
		}
		return nil, wrapCode("INTERNAL_ERROR", "insert transfer", err)
	}
	return &t, nil
}

// TransferByIdemKey resolves a replayed key to the stored transfer.
func (s *PgStore) TransferByIdemKey(ctx context.Context, accountID int64, key string) (*TransferRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, account_id, from_account_id, to_account_id, currency,
		       amount::text, status::text, actor::text, actor_id,
		       journal_entry_id, failure_reason, idempotency_key,
		       payload_sha256, created_at, completed_at
		FROM transfers WHERE account_id = $1 AND idempotency_key = $2`,
		accountID, key)
	return scanTransfer(row)
}

// SetTransferResult records the post-ledger outcome (COMPLETED + journal
// ref, or FAILED + reason). Called outside the posting tx — the journal
// is already committed-or-failed.
func (s *PgStore) SetTransferResult(ctx context.Context, id int64, status string,
	journalID *int64, failure *string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE transfers
		SET status = $2::transfer_status_enum,
		    journal_entry_id = COALESCE($3, journal_entry_id),
		    failure_reason   = COALESCE($4, failure_reason),
		    completed_at     = CASE WHEN $2 = 'COMPLETED' THEN now() ELSE completed_at END
		WHERE id = $1`, id, status, journalID, failure)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "transfer result update", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "transfer %d not found", id)
	}
	return nil
}

func scanTransfer(row pgx.Row) (*TransferRow, error) {
	var t TransferRow
	var amt, ph *string
	err := row.Scan(&t.ID, &t.AccountID, &t.FromAccountID, &t.ToAccountID,
		&t.Currency, &amt, &t.Status, &t.Actor, &t.ActorID, &t.JournalEntryID,
		&t.FailureReason, &t.IdempotencyKey, &ph, &t.CreatedAt, &t.CompletedAt)
	if err == pgx.ErrNoRows {
		return nil, errCode("NOT_FOUND", "transfer not found")
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "transfer scan", err)
	}
	if amt != nil {
		t.Amount = decimal.RequireFromString(*amt)
	}
	if ph != nil {
		t.PayloadSHA256 = *ph
	}
	return &t, nil
}

// TransferHistory pages the caller-visible transfer journal. Visibility
// is the caller's account set; Direction narrows relative to
// PerspectiveID (or the whole set when 0): OUT = initiated from it,
// IN = destined to it.
func (s *PgStore) TransferHistory(ctx context.Context, accountIDs []int64, f TransferFilter) ([]TransferRow, int64, error) {
	where := []string{"(from_account_id = ANY($1) OR to_account_id = ANY($1))"}
	args := []any{accountIDs}
	n := 1
	add := func(clause string, v any) {
		n++
		where = append(where, fmt.Sprintf(clause, n))
		args = append(args, v)
	}
	switch {
	case f.PerspectiveID != 0 && f.Direction == "IN":
		add("to_account_id = $%d", f.PerspectiveID)
	case f.PerspectiveID != 0 && f.Direction == "OUT":
		add("from_account_id = $%d", f.PerspectiveID)
	case f.PerspectiveID != 0:
		n++
		where = append(where, fmt.Sprintf("(from_account_id = $%d OR to_account_id = $%d)", n, n))
		args = append(args, f.PerspectiveID)
	case f.Direction == "IN":
		where = append(where, "to_account_id = ANY($1)")
	case f.Direction == "OUT":
		where = append(where, "from_account_id = ANY($1)")
	}
	if f.Currency != "" {
		add("currency = $%d", f.Currency)
	}
	if f.From != nil {
		add("created_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("created_at <= $%d", *f.To)
	}
	cond := strings.Join(where, " AND ")

	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM transfers WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, wrapCode("INTERNAL_ERROR", "transfer history count", err)
	}
	if f.CursorTS != nil {
		n++
		cond += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", n, n+1)
		args = append(args, *f.CursorTS, f.CursorID)
		n++
	}

	n++
	q := fmt.Sprintf(`
		SELECT id, account_id, from_account_id, to_account_id, currency,
		       amount::text, status::text, actor::text, actor_id,
		       journal_entry_id, failure_reason, idempotency_key,
		       payload_sha256, created_at, completed_at
		FROM transfers WHERE %s
		ORDER BY created_at DESC, id DESC LIMIT $%d`, cond, n)
	args = append(args, f.Limit+1)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, wrapCode("INTERNAL_ERROR", "transfer history", err)
	}
	defer rows.Close()
	var out []TransferRow
	for rows.Next() {
		var t TransferRow
		var amt, ph *string
		if err := rows.Scan(&t.ID, &t.AccountID, &t.FromAccountID, &t.ToAccountID,
			&t.Currency, &amt, &t.Status, &t.Actor, &t.ActorID,
			&t.JournalEntryID, &t.FailureReason, &t.IdempotencyKey, &ph,
			&t.CreatedAt, &t.CompletedAt); err != nil {
			return nil, 0, wrapCode("INTERNAL_ERROR", "transfer scan", err)
		}
		if amt != nil {
			t.Amount = decimal.RequireFromString(*amt)
		}
		if ph != nil {
			t.PayloadSHA256 = *ph
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// ---------------------------------------------------------------------------
// Chargebacks (Task 5.3.18)
// ---------------------------------------------------------------------------

// InsertChargeback appends a DISPUTE_OPENED dispute row inside tx.
func (s *PgStore) InsertChargeback(ctx context.Context, tx pgx.Tx, c ChargebackRow) (*ChargebackRow, error) {
	err := tx.QueryRow(ctx, `
		INSERT INTO chargebacks
		    (account_id, funding_transaction_id, card_network, currency,
		     amount, reason, status, opened_by)
		VALUES ($1, $2, $3, $4, $5::numeric, $6, 'DISPUTE_OPENED', $7)
		RETURNING id, opened_at`,
		c.AccountID, c.FundingTransactionID, c.CardNetwork, c.Currency,
		c.Amount.String(), c.Reason, c.OpenedBy).
		Scan(&c.ID, &c.OpenedAt)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "insert chargeback", err)
	}
	c.Status = "DISPUTE_OPENED"
	return &c, nil
}

// ChargebackForUpdate locks the dispute inside tx.
func (s *PgStore) ChargebackForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*ChargebackRow, error) {
	return scanChargeback(tx.QueryRow(ctx, chargebackCols+` FROM chargebacks WHERE id = $1 FOR UPDATE`, id))
}

// Chargeback reads the dispute (no lock — detail endpoint).
func (s *PgStore) Chargeback(ctx context.Context, id int64) (*ChargebackRow, error) {
	return scanChargeback(s.pool.QueryRow(ctx, chargebackCols+` FROM chargebacks WHERE id = $1`, id))
}

const chargebackCols = `
		SELECT id, account_id, funding_transaction_id, card_network, currency,
		       amount::text, reason, status::text, opened_by, opened_at,
		       submitted_at, resolved_at, resolution_note`

func scanChargeback(row pgx.Row) (*ChargebackRow, error) {
	var c ChargebackRow
	var amt *string
	err := row.Scan(&c.ID, &c.AccountID, &c.FundingTransactionID, &c.CardNetwork,
		&c.Currency, &amt, &c.Reason, &c.Status, &c.OpenedBy, &c.OpenedAt,
		&c.SubmittedAt, &c.ResolvedAt, &c.ResolutionNote)
	if err == pgx.ErrNoRows {
		return nil, errCode("NOT_FOUND", "chargeback not found")
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "chargeback scan", err)
	}
	if amt != nil {
		c.Amount = decimal.RequireFromString(*amt)
	}
	return &c, nil
}

// UpdateChargebackStatus applies a lifecycle transition inside tx.
func (s *PgStore) UpdateChargebackStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
	submittedAt, resolvedAt *time.Time, note *string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE chargebacks
		SET status = $2::chargeback_status_enum, updated_at = now(),
		    submitted_at    = COALESCE($3, submitted_at),
		    resolved_at     = COALESCE($4, resolved_at),
		    resolution_note = COALESCE($5, resolution_note)
		WHERE id = $1`, id, status, submittedAt, resolvedAt, note)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "chargeback status update", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "chargeback %d not found", id)
	}
	return nil
}

// ListChargebacks pages the dispute journal for the admin list endpoint.
func (s *PgStore) ListChargebacks(ctx context.Context, f ChargebackFilter) ([]ChargebackRow, int64, error) {
	where := []string{"TRUE"}
	args := []any{}
	n := 0
	add := func(clause string, v any) {
		n++
		where = append(where, fmt.Sprintf(clause, n))
		args = append(args, v)
	}
	if f.AccountID != nil {
		add("account_id = $%d", *f.AccountID)
	}
	if f.Status != "" {
		add("status = $%d::chargeback_status_enum", f.Status)
	}
	if f.CursorTS != nil {
		n++
		tsN := n
		n++
		where = append(where, fmt.Sprintf("(opened_at, id) < ($%d, $%d)", tsN, n))
		args = append(args, *f.CursorTS, f.CursorID)
	}
	cond := strings.Join(where, " AND ")

	var total int64
	cargs := args
	if f.CursorTS != nil {
		cargs = args[:len(args)-2]
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM chargebacks WHERE `+cond, cargs...).Scan(&total); err != nil {
		return nil, 0, wrapCode("INTERNAL_ERROR", "chargeback count", err)
	}
	n++
	q := fmt.Sprintf(`%s
		FROM chargebacks WHERE %s
		ORDER BY opened_at DESC, id DESC LIMIT $%d`, chargebackCols, cond, n)
	args = append(args, f.Limit+1)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, wrapCode("INTERNAL_ERROR", "chargeback list", err)
	}
	defer rows.Close()
	var out []ChargebackRow
	for rows.Next() {
		var c ChargebackRow
		var amt *string
		if err := rows.Scan(&c.ID, &c.AccountID, &c.FundingTransactionID,
			&c.CardNetwork, &c.Currency, &amt, &c.Reason, &c.Status,
			&c.OpenedBy, &c.OpenedAt, &c.SubmittedAt, &c.ResolvedAt,
			&c.ResolutionNote); err != nil {
			return nil, 0, wrapCode("INTERNAL_ERROR", "chargeback scan", err)
		}
		if amt != nil {
			c.Amount = decimal.RequireFromString(*amt)
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// InsertEvidence appends one hashed evidence row inside tx.
func (s *PgStore) InsertEvidence(ctx context.Context, tx pgx.Tx, e EvidenceRow) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO chargeback_evidence
		    (chargeback_id, kind, payload, payload_sha256, collected_by)
		VALUES ($1, $2, $3, $4, $5)`,
		e.ChargebackID, e.Kind, e.Payload, e.PayloadSHA, e.CollectedBy)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "evidence insert", err)
	}
	return nil
}

// EvidenceFor lists the collected bundle for a dispute.
func (s *PgStore) EvidenceFor(ctx context.Context, chargebackID int64) ([]EvidenceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, chargeback_id, kind, payload, payload_sha256,
		       collected_by, collected_at
		FROM chargeback_evidence WHERE chargeback_id = $1 ORDER BY id`,
		chargebackID)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "evidence list", err)
	}
	defer rows.Close()
	var out []EvidenceRow
	for rows.Next() {
		var e EvidenceRow
		if err := rows.Scan(&e.ID, &e.ChargebackID, &e.Kind, &e.Payload,
			&e.PayloadSHA, &e.CollectedBy, &e.CollectedAt); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "evidence scan", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EvidenceCount reports how many rows the bundle holds (submit gate).
func (s *PgStore) EvidenceCount(ctx context.Context, tx pgx.Tx, chargebackID int64) (int, error) {
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM chargeback_evidence WHERE chargeback_id = $1`,
		chargebackID).Scan(&n); err != nil {
		return 0, wrapCode("INTERNAL_ERROR", "evidence count", err)
	}
	return n, nil
}

// FundingTxSnapshot returns the disputed funding row as a JSON object —
// the evidence bundle's transaction record.
func (s *PgStore) FundingTxSnapshot(ctx context.Context, id int64) ([]byte, error) {
	var payload []byte
	err := s.pool.QueryRow(ctx, `
		SELECT to_jsonb(t) FROM (
		    SELECT id, account_id, currency, type::text, amount::text,
		           status::text, reference, bank_method::text,
		           reference_account, created_at, confirmed_at, completed_at
		    FROM funding_transactions WHERE id = $1) t`, id).Scan(&payload)
	if err == pgx.ErrNoRows {
		return nil, errf("NOT_FOUND", "funding transaction %d not found", id)
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "funding snapshot", err)
	}
	return payload, nil
}

// TradesForEvidence collects the account's fills in the dispute window —
// the trade-records half of automated evidence collection.
func (s *PgStore) TradesForEvidence(ctx context.Context, accountID int64,
	since, until time.Time, limit int) ([]EvidenceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT to_jsonb(t) FROM (
		    SELECT id, instrument_id, buyer_account_id, seller_account_id,
		           price::text, quantity::text, buyer_fee::text, seller_fee::text,
		           created_at
		    FROM trades
		    WHERE (buyer_account_id = $1 OR seller_account_id = $1)
		      AND created_at BETWEEN $2 AND $3
		    ORDER BY created_at DESC LIMIT $4) t`,
		accountID, since, until, limit)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "trade evidence", err)
	}
	defer rows.Close()
	var out []EvidenceRow
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "trade evidence scan", err)
		}
		out = append(out, EvidenceRow{Kind: "TRADE", Payload: payload})
	}
	return out, rows.Err()
}

// FreezeEventsForEvidence collects the account's legal-hold history.
func (s *PgStore) FreezeEventsForEvidence(ctx context.Context, accountID int64, limit int) ([]EvidenceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT to_jsonb(t) FROM (
		    SELECT id, account_id, action, reason, initiated_by, approved_by,
		           prev_status, new_status, created_at
		    FROM account_freeze_events
		    WHERE account_id = $1 ORDER BY id DESC LIMIT $2) t`,
		accountID, limit)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "freeze evidence", err)
	}
	defer rows.Close()
	var out []EvidenceRow
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "freeze evidence scan", err)
		}
		out = append(out, EvidenceRow{Kind: "FREEZE_EVENT", Payload: payload})
	}
	return out, rows.Err()
}

// AdminAudit mirrors the admin action into admin_audit_log (spec §5.9).
func (s *PgStore) AdminAudit(ctx context.Context, adminID int64, action, targetType string,
	targetID int64, before, after []byte, ip string) error {
	var ipParam *string
	if ip != "" {
		ipParam = &ip
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO admin_audit_log
		    (admin_user_id, action, target_type, target_id,
		     before_state, after_state, ip_address)
		VALUES ($1, $2, $3, $4, $5, $6, $7::inet)`,
		adminID, action, targetType, targetID, before, after, ipParam)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "admin audit", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Phase-11: deposits, suspense quarantine & rail payments (Tasks 11.3.1/11.3.11)
// ---------------------------------------------------------------------------

// InsertDepositPending persists an inbound DEPOSIT funding row inside tx.
// Idempotent on (account_id, idempotency_key) when a key is carried —
// ErrIdemConflict lets the service resolve replay vs mismatch.
func (s *PgStore) InsertDepositPending(ctx context.Context, tx pgx.Tx, d DepositRow) (*DepositRow, error) {
	status := d.Status
	if status == "" {
		status = FundingPending
	}
	var usd *string
	if d.USDAmount != nil {
		v := d.USDAmount.String()
		usd = &v
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO funding_transactions
		    (account_id, currency, type, amount, status, bank_method,
		     reference, reference_account, idempotency_key, payload_sha256,
		     usd_amount, review_tier)
		VALUES ($1, $2, 'DEPOSIT', $3::numeric, $4::funding_status_enum, $5,
		        $6, $7, $8, $9, $10::numeric, $11)
		RETURNING id, created_at`,
		d.AccountID, d.Currency, d.Amount.String(), status, d.BankMethod,
		d.Reference, d.ReferenceAccount, d.IdempotencyKey, d.PayloadSHA256,
		usd, d.ReviewTier).Scan(&d.ID, &d.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrIdemConflict
		}
		return nil, wrapCode("INTERNAL_ERROR", "insert deposit", err)
	}
	d.Status = status
	return &d, nil
}

// FundingTxForUpdate locks one funding_transactions row inside tx.
func (s *PgStore) FundingTxForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*FundingTxRow, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, account_id, currency, type::text, amount::text, status::text,
		       reference, bank_method::text, reference_account,
		       usd_amount::text, review_tier, review_deadline,
		       created_at, confirmed_at, completed_at
		FROM funding_transactions WHERE id = $1 FOR UPDATE`, id)
	return scanFundingTx(row)
}

// SetFundingTxStatus transitions a funding row inside tx (return/returned
// and deposit resolution paths).
func (s *PgStore) SetFundingTxStatus(ctx context.Context, tx pgx.Tx, id int64,
	status string, completedAt *time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE funding_transactions
		SET status = $2::funding_status_enum, updated_at = now(),
		    completed_at = COALESCE($3, completed_at)
		WHERE id = $1`, id, status, completedAt)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "funding status update", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "funding transaction %d not found", id)
	}
	return nil
}

func scanFundingTx(row pgx.Row) (*FundingTxRow, error) {
	var r FundingTxRow
	var amt, usd *string
	var ref, bm, ra, tier *string
	err := row.Scan(&r.ID, &r.AccountID, &r.Currency, &r.Type, &amt,
		&r.Status, &ref, &bm, &ra, &usd, &tier, &r.ReviewDeadline,
		&r.CreatedAt, &r.ConfirmedAt, &r.CompletedAt)
	if err == pgx.ErrNoRows {
		return nil, errCode("NOT_FOUND", "funding transaction not found")
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "funding tx scan", err)
	}
	if amt != nil {
		r.Amount = decimal.RequireFromString(*amt)
	}
	if usd != nil {
		d := decimal.RequireFromString(*usd)
		r.USDAmount = &d
	}
	r.Reference, r.BankMethod, r.ReferenceAccount, r.ReviewTier = ref, bm, ra, tier
	return &r, nil
}

// InsertSuspenseMapping persists one quarantine record inside tx; a
// bank_tx_id conflict returns ErrIdemConflict (dup wire notification).
func (s *PgStore) InsertSuspenseMapping(ctx context.Context, tx pgx.Tx, m SuspenseRow) (*SuspenseRow, error) {
	var score *string
	if m.NameMatchScore != nil {
		v := fmt.Sprintf("%.3f", *m.NameMatchScore)
		score = &v
	}
	gl := m.GLAccount
	if gl == "" {
		gl = "2150"
	}
	// A freshly routed suspense row is always quarantined; the
	// INVESTIGATING/RESOLVED/RETURNED_TO_SOURCE states are only reachable
	// via SetSuspenseStatus transitions post-insert.
	status := m.QuarantineStatus
	if status == "" {
		status = "QUARANTINED"
	}
	// ON CONFLICT DO NOTHING converts a duplicate bank_tx_id into a clean
	// ErrIdemConflict without raising — a raised error would poison the
	// caller's enclosing transaction (SQLSTATE 25P02).
	err := tx.QueryRow(ctx, `
		INSERT INTO suspense_account_mappings
		    (bank_tx_id, funding_transaction_id, account_id, rail, currency,
		     amount, originator_name, originator_account, name_match_score,
		     unmatched_reason, gl_account, quarantine_status, sla_expires_at,
		     journal_entry_id, return_payment_id)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9::numeric,
		        $10::unmatched_reason_enum, $11, $12::quarantine_status_enum,
		        $13, $14, $15)
		ON CONFLICT (bank_tx_id) DO NOTHING
		RETURNING id, quarantined_at, created_at`,
		m.BankTxID, m.FundingTransactionID, m.AccountID, m.Rail, m.Currency,
		m.Amount.String(), m.OriginatorName, m.OriginatorAccount, score,
		m.UnmatchedReason, gl, status, m.SLAExpiresAt,
		m.JournalEntryID, m.ReturnPaymentID).
		Scan(&m.ID, &m.QuarantinedAt, &m.CreatedAt)
	if err != nil {
		if stderrors.Is(err, pgx.ErrNoRows) || isUniqueViolation(err) {
			return nil, ErrIdemConflict
		}
		return nil, wrapCode("INTERNAL_ERROR", "insert suspense mapping", err)
	}
	return &m, nil
}

const suspenseCols = `
		SELECT id, bank_tx_id, funding_transaction_id, account_id, rail,
		       currency, amount::text, originator_name, originator_account,
		       name_match_score::text, unmatched_reason::text, gl_account,
		       quarantine_status::text, quarantined_at, sla_expires_at,
		       assigned_investigator_id, resolution_notes, resolved_at,
		       journal_entry_id, return_payment_id, created_at
		FROM suspense_account_mappings`

func scanSuspense(row pgx.Row) (*SuspenseRow, error) {
	var m SuspenseRow
	var amt, score *string
	err := row.Scan(&m.ID, &m.BankTxID, &m.FundingTransactionID, &m.AccountID,
		&m.Rail, &m.Currency, &amt, &m.OriginatorName, &m.OriginatorAccount,
		&score, &m.UnmatchedReason, &m.GLAccount, &m.QuarantineStatus,
		&m.QuarantinedAt, &m.SLAExpiresAt, &m.AssignedInvestigator,
		&m.ResolutionNotes, &m.ResolvedAt, &m.JournalEntryID,
		&m.ReturnPaymentID, &m.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, errCode("NOT_FOUND", "suspense mapping not found")
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "suspense scan", err)
	}
	if amt != nil {
		m.Amount = decimal.RequireFromString(*amt)
	}
	if score != nil {
		var f float64
		if _, serr := fmt.Sscanf(*score, "%f", &f); serr == nil {
			m.NameMatchScore = &f
		}
	}
	return &m, nil
}

// SuspenseByBankTx resolves a bank transaction id — the dedup key for
// repeated wire notifications.
func (s *PgStore) SuspenseByBankTx(ctx context.Context, bankTxID string) (*SuspenseRow, error) {
	return scanSuspense(s.pool.QueryRow(ctx,
		suspenseCols+` WHERE bank_tx_id = $1`, bankTxID))
}

// SuspenseForUpdate locks one quarantine row inside tx.
func (s *PgStore) SuspenseForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*SuspenseRow, error) {
	return scanSuspense(tx.QueryRow(ctx,
		suspenseCols+` WHERE id = $1 FOR UPDATE`, id))
}

// SetSuspenseStatus applies a quarantine lifecycle transition inside tx.
func (s *PgStore) SetSuspenseStatus(ctx context.Context, tx pgx.Tx, id int64, status string,
	investigatorID *int64, notes *string, resolvedAt *time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE suspense_account_mappings
		SET quarantine_status = $2::quarantine_status_enum, updated_at = now(),
		    assigned_investigator_id = COALESCE($3, assigned_investigator_id),
		    resolution_notes = COALESCE($4, resolution_notes),
		    resolved_at = COALESCE($5, resolved_at)
		WHERE id = $1`, id, status, investigatorID, notes, resolvedAt)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "suspense status update", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "suspense mapping %d not found", id)
	}
	return nil
}

// SetSuspenseLinks backfills the GL journal and return-wire references
// after the suspense row's dependent postings land.
func (s *PgStore) SetSuspenseLinks(ctx context.Context, tx pgx.Tx, id int64,
	journalID, returnPaymentID *int64) error {
	tag, err := tx.Exec(ctx, `
		UPDATE suspense_account_mappings
		SET journal_entry_id = COALESCE($2, journal_entry_id),
		    return_payment_id = COALESCE($3, return_payment_id),
		    updated_at = now()
		WHERE id = $1`, id, journalID, returnPaymentID)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "suspense links update", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "suspense mapping %d not found", id)
	}
	return nil
}

// ListSuspense pages the quarantine journal for the admin endpoint —
// keyset on (quarantined_at, id), newest first.
func (s *PgStore) ListSuspense(ctx context.Context, f SuspenseFilter) ([]SuspenseRow, int64, error) {
	where := []string{"TRUE"}
	args := []any{}
	n := 0
	add := func(clause string, v any) {
		n++
		where = append(where, fmt.Sprintf(clause, n))
		args = append(args, v)
	}
	if f.AccountID != nil {
		add("account_id = $%d", *f.AccountID)
	}
	if f.Status != "" {
		add("quarantine_status = $%d::quarantine_status_enum", f.Status)
	}
	if f.CursorTS != nil {
		n++
		tsN := n
		n++
		where = append(where, fmt.Sprintf("(quarantined_at, id) < ($%d, $%d)", tsN, n))
		args = append(args, *f.CursorTS, f.CursorID)
	}
	cond := strings.Join(where, " AND ")

	var total int64
	cargs := args
	if f.CursorTS != nil {
		cargs = args[:len(args)-2]
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM suspense_account_mappings WHERE `+cond, cargs...).Scan(&total); err != nil {
		return nil, 0, wrapCode("INTERNAL_ERROR", "suspense count", err)
	}
	n++
	q := fmt.Sprintf(`%s WHERE %s
		ORDER BY quarantined_at DESC, id DESC LIMIT $%d`, suspenseCols, cond, n)
	args = append(args, f.Limit+1)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, wrapCode("INTERNAL_ERROR", "suspense list", err)
	}
	defer rows.Close()
	var out []SuspenseRow
	for rows.Next() {
		var m SuspenseRow
		var amt, score *string
		if err := rows.Scan(&m.ID, &m.BankTxID, &m.FundingTransactionID,
			&m.AccountID, &m.Rail, &m.Currency, &amt, &m.OriginatorName,
			&m.OriginatorAccount, &score, &m.UnmatchedReason, &m.GLAccount,
			&m.QuarantineStatus, &m.QuarantinedAt, &m.SLAExpiresAt,
			&m.AssignedInvestigator, &m.ResolutionNotes, &m.ResolvedAt,
			&m.JournalEntryID, &m.ReturnPaymentID, &m.CreatedAt); err != nil {
			return nil, 0, wrapCode("INTERNAL_ERROR", "suspense list scan", err)
		}
		if amt != nil {
			m.Amount = decimal.RequireFromString(*amt)
		}
		if score != nil {
			var fv float64
			if _, serr := fmt.Sscanf(*score, "%f", &fv); serr == nil {
				m.NameMatchScore = &fv
			}
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// InsertRailPayment persists one outbound/return instruction envelope
// inside tx; an end_to_end_id conflict returns ErrIdemConflict.
func (s *PgStore) InsertRailPayment(ctx context.Context, tx pgx.Tx, p RailPaymentRow) (*RailPaymentRow, error) {
	status := p.Status
	if status == "" {
		status = RailPaymentPrepared
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO rail_payments
		    (funding_transaction_id, suspense_mapping_id, direction, rail,
		     message_type, end_to_end_id, uetr, envelope, status,
		     return_code, return_reason, value_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::rail_payment_status_enum,
		        $10, $11, $12)
		RETURNING id, created_at`,
		p.FundingTransactionID, p.SuspenseMappingID, p.Direction, p.Rail,
		p.MessageType, p.EndToEndID, p.UETR, p.Envelope, status,
		p.ReturnCode, p.ReturnReason, p.ValueDate).
		Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrIdemConflict
		}
		return nil, wrapCode("INTERNAL_ERROR", "insert rail payment", err)
	}
	p.Status = status
	return &p, nil
}

const railPaymentCols = `
		SELECT id, funding_transaction_id, suspense_mapping_id, direction,
		       rail, message_type, end_to_end_id, uetr, envelope, status::text,
		       return_code, return_reason, value_date, dispatched_at,
		       settled_at, created_at
		FROM rail_payments`

func scanRailPayment(row pgx.Row) (*RailPaymentRow, error) {
	var p RailPaymentRow
	err := row.Scan(&p.ID, &p.FundingTransactionID, &p.SuspenseMappingID,
		&p.Direction, &p.Rail, &p.MessageType, &p.EndToEndID, &p.UETR,
		&p.Envelope, &p.Status, &p.ReturnCode, &p.ReturnReason, &p.ValueDate,
		&p.DispatchedAt, &p.SettledAt, &p.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, errCode("NOT_FOUND", "rail payment not found")
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "rail payment scan", err)
	}
	return &p, nil
}

// RailPaymentForUpdate locks one instruction inside tx.
func (s *PgStore) RailPaymentForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*RailPaymentRow, error) {
	return scanRailPayment(tx.QueryRow(ctx,
		railPaymentCols+` WHERE id = $1 FOR UPDATE`, id))
}

// RailPaymentByEndToEndID resolves a message by its ISO 20022
// EndToEndId / trace reference — the return-matching key.
func (s *PgStore) RailPaymentByEndToEndID(ctx context.Context, endToEndID string) (*RailPaymentRow, error) {
	return scanRailPayment(s.pool.QueryRow(ctx,
		railPaymentCols+` WHERE end_to_end_id = $1`, endToEndID))
}

// SetRailPaymentStatus applies a rail status transition inside tx.
func (s *PgStore) SetRailPaymentStatus(ctx context.Context, tx pgx.Tx, id int64,
	status string, returnCode, returnReason *string,
	dispatchedAt, settledAt *time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE rail_payments
		SET status = $2::rail_payment_status_enum, updated_at = now(),
		    return_code = COALESCE($3, return_code),
		    return_reason = COALESCE($4, return_reason),
		    dispatched_at = COALESCE($5, dispatched_at),
		    settled_at = COALESCE($6, settled_at)
		WHERE id = $1`, id, status, returnCode, returnReason,
		dispatchedAt, settledAt)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "rail payment status update", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "rail payment %d not found", id)
	}
	return nil
}
