// Phase-11 flows-cluster store extensions — Tasks 11.3.2 (withdrawal
// lifecycle), 11.3.3 (deposit lifecycle), 11.3.6 (nostro-aware
// dispatch) and 11.3.10 (withdrawal whitelist mode).
//
// Purely additive: every PgStore method here is new; store.go is
// untouched so the sibling clusters' Store fakes keep compiling. Each
// service declares the narrow store seam it consumes (a superset of the
// migration-199 tables); *PgStore satisfies all of them.
package funding

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Row types (migration 078 + 199 + bank_accounts 040 read model)
// ---------------------------------------------------------------------------

// Whitelist mode values (withdrawal_whitelist_mode_enum, migration 078).
const (
	WhitelistModeAllowAll = "ALLOW_ALL"
	WhitelistModeOnly     = "WHITELIST_ONLY"
)

// WhitelistSettingsRow is one withdrawal_whitelist_settings row. A nil
// row means the account never configured a mode → ALLOW_ALL default.
type WhitelistSettingsRow struct {
	AccountID           int64      `json:"account_id"`
	Mode                string     `json:"mode"`                            // ALLOW_ALL | WHITELIST_ONLY
	TimelockUntil       *time.Time `json:"timelock_until,omitempty"`        // re-enable latch
	WithdrawalLockUntil *time.Time `json:"withdrawal_lock_until,omitempty"` // post-disable egress lock
	UpdatedBy           *int64     `json:"updated_by,omitempty"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// BeneficiaryRow is the bank_accounts (migration 040, spec §5.23)
// projection the whitelist gate needs: a destination is whitelisted
// when a VERIFIED row matches and its unlocked_at (verified_at + 24h
// addition timelock) has lapsed.
type BeneficiaryRow struct {
	BankAccountID   int64      `json:"bank_account_id"`
	AccountID       int64      `json:"account_id"`
	Currency        string     `json:"currency"`
	IBAN            *string    `json:"iban,omitempty"`
	AccountNumber   *string    `json:"account_number,omitempty"`
	SwiftBIC        *string    `json:"swift_bic,omitempty"`
	BankName        string     `json:"bank_name"`
	BeneficiaryName string     `json:"beneficiary_name"`
	Rail            string     `json:"rail"`
	Status          string     `json:"status"` // PENDING_VERIFICATION | VERIFIED | REJECTED
	VerifiedAt      *time.Time `json:"verified_at,omitempty"`
	UnlockedAt      *time.Time `json:"unlocked_at,omitempty"`
}

// BeneficiaryVerified is the bank_accounts status that admits a
// destination under WHITELIST_ONLY (subject to unlocked_at).
const BeneficiaryVerified = "VERIFIED"

// DestinationHoldRow is one withdrawal_destination_holds row — the
// first-seen registry for destinations outside the verified beneficiary
// registry (24h unverified-destination hold, Task 11.3.2 step 6).
type DestinationHoldRow struct {
	ID          int64     `json:"id"`
	AccountID   int64     `json:"account_id"`
	Destination string    `json:"destination"`
	FirstSeen   time.Time `json:"first_seen"`
	UnlockedAt  time.Time `json:"unlocked_at"`
}

// DepositConfirmationRow is one deposit_confirmations row — one bank
// source's independent verification of an inbound deposit.
type DepositConfirmationRow struct {
	ID                   int64     `json:"id"`
	FundingTransactionID int64     `json:"funding_transaction_id"`
	Source               string    `json:"source"`
	SenderName           *string   `json:"sender_name,omitempty"`
	SenderAccount        *string   `json:"sender_account,omitempty"`
	PayloadSHA256        *string   `json:"-"`
	ReceivedBy           *int64    `json:"received_by,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// DispatchQueueRow is one withdrawal_dispatch_queue row.
type DispatchQueueRow struct {
	ID           int64      `json:"id"`
	WithdrawalID int64      `json:"withdrawal_id"`
	Status       string     `json:"status"` // QUEUED | DISPATCHED | CANCELLED
	Reason       string     `json:"reason"`
	QueuedAt     time.Time  `json:"queued_at"`
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	Attempts     int        `json:"attempts"`
	LastError    *string    `json:"last_error,omitempty"`
}

// Dispatch queue status / reason values.
const (
	QueueQueued     = "QUEUED"
	QueueDispatched = "DISPATCHED"
	QueueCancelled  = "CANCELLED"

	QueueReasonNostroInsufficient = "NOSTRO_INSUFFICIENT"
	QueueReasonDestinationHold    = "DESTINATION_HOLD"
	QueueReasonDispatchError      = "DISPATCH_ERROR"
	// QueueReasonTravelRuleMissing — Phase-21 Task 21.3.2: the FATF R.16
	// gate found required originator/beneficiary fields absent; the wire
	// stays queued until an officer supplies them.
	QueueReasonTravelRuleMissing = "TRAVEL_RULE_MISSING_INFO"
)

// DispatchableWithdrawal is the CONFIRMED-withdrawal projection the
// nostro dispatcher locks and releases (adds hold_until over
// WithdrawalRow — that type predates migration 199). USDAmount is the
// converted figure the Phase-21 travel-rule gate thresholds on.
type DispatchableWithdrawal struct {
	ID               int64
	AccountID        int64
	Currency         string
	Amount           decimal.Decimal
	USDAmount        *decimal.Decimal
	Status           string
	ReferenceAccount *string
	BankMethod       *string
	HoldUntil        *time.Time
}

// FundingOpsAlertRow is one funding_ops_alerts row — the durable funding
// alert trail (NOSTRO_INSUFFICIENT_FUNDS queueing, dispatch failures,
// deposit review breaches). The NATS OpsAlerter page rides alongside.
type FundingOpsAlertRow struct {
	ID                   int64            `json:"id"`
	Code                 string           `json:"code"`
	Severity             string           `json:"severity"`
	FundingTransactionID *int64           `json:"funding_transaction_id,omitempty"`
	AccountID            *int64           `json:"account_id,omitempty"`
	Currency             *string          `json:"currency,omitempty"`
	Amount               *decimal.Decimal `json:"amount,omitempty"`
	Summary              string           `json:"summary"`
	Detail               []byte           `json:"detail,omitempty"` // JSONB
	Status               string           `json:"status"`           // OPEN | ACKED | RESOLVED
	CreatedAt            time.Time        `json:"created_at"`
	ResolvedAt           *time.Time       `json:"resolved_at,omitempty"`
}

// ReplenishmentRow is one nostro_replenishment_requests row — a
// dual-controlled reserve→operating nostro movement.
type ReplenishmentRow struct {
	ID             int64           `json:"id"`
	Currency       string          `json:"currency"`
	Amount         decimal.Decimal `json:"amount"`
	SourceNostroID int64           `json:"source_nostro_id"`
	TargetNostroID int64           `json:"target_nostro_id"`
	Status         string          `json:"status"`                 // PENDING_APPROVAL | EXECUTED | REJECTED
	RequestedBy    *int64          `json:"requested_by,omitempty"` // NULL = dispatcher auto-request
	ApprovedBy     *int64          `json:"approved_by,omitempty"`
	DecisionNote   *string         `json:"decision_note,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
}

const (
	ReplenPending  = "PENDING_APPROVAL"
	ReplenExecuted = "EXECUTED"
	ReplenRejected = "REJECTED"
)

// NostroBalanceRow is the balance-carrying nostro_accounts read model
// (NostroAccount in store.go omits balance; dispatch needs it).
type NostroBalanceRow struct {
	ID            int64           `json:"id"`
	Currency      string          `json:"currency"`
	BankName      string          `json:"bank_name"`
	BankCode      *string         `json:"bank_code,omitempty"`
	AccountNumber *string         `json:"account_number,omitempty"`
	IBAN          *string         `json:"iban,omitempty"`
	Balance       decimal.Decimal `json:"balance"`
	Status        string          `json:"status"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Whitelist settings + beneficiary registry (078 + 040)
// ---------------------------------------------------------------------------

// WhitelistSettings reads the account's settings row; absent → nil,nil
// (the service treats nil as the ALLOW_ALL default).
func (s *PgStore) WhitelistSettings(ctx context.Context, accountID int64) (*WhitelistSettingsRow, error) {
	return scanWhitelist(s.pool.QueryRow(ctx, `
		SELECT account_id, mode::text, timelock_until, withdrawal_lock_until,
		       updated_by, updated_at
		FROM withdrawal_whitelist_settings WHERE account_id = $1`, accountID))
}

// WhitelistSettingsForUpdate locks (or notes the absence of) the
// settings row inside tx.
func (s *PgStore) WhitelistSettingsForUpdate(ctx context.Context, tx pgx.Tx,
	accountID int64) (*WhitelistSettingsRow, error) {
	return scanWhitelist(tx.QueryRow(ctx, `
		SELECT account_id, mode::text, timelock_until, withdrawal_lock_until,
		       updated_by, updated_at
		FROM withdrawal_whitelist_settings WHERE account_id = $1 FOR UPDATE`,
		accountID))
}

func scanWhitelist(row pgx.Row) (*WhitelistSettingsRow, error) {
	var r WhitelistSettingsRow
	err := row.Scan(&r.AccountID, &r.Mode, &r.TimelockUntil,
		&r.WithdrawalLockUntil, &r.UpdatedBy, &r.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "whitelist settings", err)
	}
	return &r, nil
}

// UpsertWhitelistSettings writes the full settings row inside tx.
func (s *PgStore) UpsertWhitelistSettings(ctx context.Context, tx pgx.Tx,
	r WhitelistSettingsRow) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO withdrawal_whitelist_settings
		    (account_id, mode, timelock_until, withdrawal_lock_until, updated_by, updated_at)
		VALUES ($1, $2::withdrawal_whitelist_mode_enum, $3, $4, $5, now())
		ON CONFLICT (account_id) DO UPDATE SET
		    mode                  = EXCLUDED.mode,
		    timelock_until        = EXCLUDED.timelock_until,
		    withdrawal_lock_until = EXCLUDED.withdrawal_lock_until,
		    updated_by            = EXCLUDED.updated_by,
		    updated_at            = now()`,
		r.AccountID, r.Mode, r.TimelockUntil, r.WithdrawalLockUntil, r.UpdatedBy)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "whitelist upsert", err)
	}
	return nil
}

const beneficiaryCols = `
		bank_account_id, account_id, currency, iban, account_number,
		swift_bic, bank_name, beneficiary_name, rail::text, status::text,
		verified_at, unlocked_at`

// BeneficiaryByDestination resolves the best bank_accounts match for a
// destination reference — VERIFIED rows win, IBAN match before
// account_number. Returns nil,nil when no row matches.
func (s *PgStore) BeneficiaryByDestination(ctx context.Context,
	accountID int64, destination string) (*BeneficiaryRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+beneficiaryCols+`
		FROM bank_accounts
		WHERE account_id = $1
		  AND (upper(iban) = upper($2) OR upper(account_number) = upper($2))
		ORDER BY (status = 'VERIFIED') DESC, bank_account_id
		LIMIT 1`, accountID, destination)
	return scanBeneficiary(row)
}

func scanBeneficiary(row pgx.Row) (*BeneficiaryRow, error) {
	var b BeneficiaryRow
	err := row.Scan(&b.BankAccountID, &b.AccountID, &b.Currency, &b.IBAN,
		&b.AccountNumber, &b.SwiftBIC, &b.BankName, &b.BeneficiaryName,
		&b.Rail, &b.Status, &b.VerifiedAt, &b.UnlockedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "beneficiary lookup", err)
	}
	return &b, nil
}

// WhitelistedBeneficiaries lists the account's VERIFIED beneficiary rows
// — the effective whitelist membership view.
func (s *PgStore) WhitelistedBeneficiaries(ctx context.Context,
	accountID int64) ([]BeneficiaryRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+beneficiaryCols+`
		FROM bank_accounts
		WHERE account_id = $1 AND status = 'VERIFIED'
		ORDER BY bank_account_id`, accountID)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "beneficiary list", err)
	}
	defer rows.Close()
	var out []BeneficiaryRow
	for rows.Next() {
		var b BeneficiaryRow
		if err := rows.Scan(&b.BankAccountID, &b.AccountID, &b.Currency,
			&b.IBAN, &b.AccountNumber, &b.SwiftBIC, &b.BankName,
			&b.BeneficiaryName, &b.Rail, &b.Status, &b.VerifiedAt,
			&b.UnlockedAt); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "beneficiary scan", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Destination holds + withdrawal review/dispatch columns (199)
// ---------------------------------------------------------------------------

// DestinationHold reads the account/destination first-seen row.
func (s *PgStore) DestinationHold(ctx context.Context, tx pgx.Tx,
	accountID int64, destination string) (*DestinationHoldRow, error) {
	var h DestinationHoldRow
	err := tx.QueryRow(ctx, `
		SELECT id, account_id, destination, first_seen, unlocked_at
		FROM withdrawal_destination_holds
		WHERE account_id = $1 AND destination = upper($2)`, accountID, destination).
		Scan(&h.ID, &h.AccountID, &h.Destination, &h.FirstSeen, &h.UnlockedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "destination hold", err)
	}
	return &h, nil
}

// UpsertDestinationHold inserts the first-seen row; on a raced insert
// the existing row is returned (idempotent).
func (s *PgStore) UpsertDestinationHold(ctx context.Context, tx pgx.Tx,
	accountID int64, destination string, unlockedAt time.Time) (*DestinationHoldRow, error) {
	var h DestinationHoldRow
	err := tx.QueryRow(ctx, `
		INSERT INTO withdrawal_destination_holds
		    (account_id, destination, unlocked_at)
		VALUES ($1, upper($2), $3)
		ON CONFLICT (account_id, destination) DO UPDATE
		    SET destination = EXCLUDED.destination          -- no-op: fetch the winner
		RETURNING id, account_id, destination, first_seen, unlocked_at`,
		accountID, destination, unlockedAt).
		Scan(&h.ID, &h.AccountID, &h.Destination, &h.FirstSeen, &h.UnlockedAt)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "destination hold upsert", err)
	}
	return &h, nil
}

// WithdrawalByID is the non-locking withdrawal read (compensation paths
// that need the row without opening a tx).
func (s *PgStore) WithdrawalByID(ctx context.Context, id int64) (*WithdrawalRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, account_id, currency, amount::text, status::text,
		       reference_account, bank_method::text, idempotency_key,
		       payload_sha256, usd_amount::text, review_tier,
		       created_at, review_deadline
		FROM funding_transactions
		WHERE id = $1 AND type = 'WITHDRAWAL'`, id)
	return scanWithdrawal(row)
}

// SetWithdrawalHoldUntil stamps/clears funding_transactions.hold_until.
func (s *PgStore) SetWithdrawalHoldUntil(ctx context.Context, tx pgx.Tx,
	id int64, until *time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE funding_transactions SET hold_until = $2, updated_at = now()
		WHERE id = $1 AND type = 'WITHDRAWAL'`, id, until)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "withdrawal hold_until", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "withdrawal %d not found", id)
	}
	return nil
}

// SetWithdrawalReview stamps the admin review verdict
// (PENDING_REVIEW → CONFIRMED | FAILED) with reviewer attribution.
func (s *PgStore) SetWithdrawalReview(ctx context.Context, tx pgx.Tx,
	id int64, status string, reviewedBy int64, at time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE funding_transactions
		SET status = $2::funding_status_enum, reviewed_by = $3,
		    reviewed_at = $4, updated_at = now()
		WHERE id = $1 AND type = 'WITHDRAWAL'`,
		id, status, reviewedBy, at)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "withdrawal review", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "withdrawal %d not found", id)
	}
	return nil
}

// SetWithdrawalCompleted marks the withdrawal COMPLETED at dispatch.
func (s *PgStore) SetWithdrawalCompleted(ctx context.Context, tx pgx.Tx,
	id int64, at time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE funding_transactions
		SET status = 'COMPLETED', completed_at = $2, updated_at = now()
		WHERE id = $1 AND type = 'WITHDRAWAL'`, id, at)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "withdrawal complete", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "withdrawal %d not found", id)
	}
	return nil
}

// LastCompletedWithdrawalAt returns the completed_at of the most recent
// COMPLETED withdrawal to the same destination — the 30-minute
// same-bank-account cooldown window (Task 11.3.2 step 5).
func (s *PgStore) LastCompletedWithdrawalAt(ctx context.Context,
	accountID int64, destination string) (*time.Time, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(completed_at) FROM funding_transactions
		WHERE account_id = $1 AND type = 'WITHDRAWAL' AND status = 'COMPLETED'
		  AND upper(reference_account) = upper($2)`,
		accountID, destination).Scan(&t)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "cooldown lookup", err)
	}
	return t, nil
}

// WithdrawalForDispatch locks a withdrawal for the dispatch decision —
// includes hold_until (migration 199) which WithdrawalForUpdate's older
// projection does not carry.
func (s *PgStore) WithdrawalForDispatch(ctx context.Context, tx pgx.Tx,
	id int64) (*DispatchableWithdrawal, error) {
	var w DispatchableWithdrawal
	var amt, usd *string
	err := tx.QueryRow(ctx, `
		SELECT id, account_id, currency, amount::text, usd_amount::text,
		       status::text, reference_account, bank_method::text, hold_until
		FROM funding_transactions
		WHERE id = $1 AND type = 'WITHDRAWAL' FOR UPDATE`, id).
		Scan(&w.ID, &w.AccountID, &w.Currency, &amt, &usd, &w.Status,
			&w.ReferenceAccount, &w.BankMethod, &w.HoldUntil)
	if err == pgx.ErrNoRows {
		return nil, errf("NOT_FOUND", "withdrawal %d not found", id)
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "withdrawal dispatch lock", err)
	}
	if amt != nil {
		w.Amount = decimal.RequireFromString(*amt)
	}
	if usd != nil {
		v := decimal.RequireFromString(*usd)
		w.USDAmount = &v
	}
	return &w, nil
}

// ConfirmedForDispatch lists CONFIRMED withdrawals whose destination
// hold has lapsed (or was never set) — the dispatcher work queue scan.
func (s *PgStore) ConfirmedForDispatch(ctx context.Context, limit int) ([]DispatchableWithdrawal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, currency, amount::text, usd_amount::text,
		       status::text, reference_account, bank_method::text, hold_until
		FROM funding_transactions
		WHERE type = 'WITHDRAWAL' AND status = 'CONFIRMED'
		  AND (hold_until IS NULL OR hold_until <= now())
		ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "dispatch scan", err)
	}
	defer rows.Close()
	var out []DispatchableWithdrawal
	for rows.Next() {
		var w DispatchableWithdrawal
		var amt, usd *string
		if err := rows.Scan(&w.ID, &w.AccountID, &w.Currency, &amt, &usd,
			&w.Status, &w.ReferenceAccount, &w.BankMethod,
			&w.HoldUntil); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "dispatch scan row", err)
		}
		if amt != nil {
			w.Amount = decimal.RequireFromString(*amt)
		}
		if usd != nil {
			v := decimal.RequireFromString(*usd)
			w.USDAmount = &v
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Deposit confirmations + discovery (199)
// ---------------------------------------------------------------------------

// InsertDepositConfirmation appends one source confirmation; a repeat
// (funding_transaction_id, source) pair maps 23505 → ErrIdemConflict.
func (s *PgStore) InsertDepositConfirmation(ctx context.Context, tx pgx.Tx,
	c DepositConfirmationRow) (*DepositConfirmationRow, error) {
	// ON CONFLICT keeps the enclosing tx usable — the caller resolves
	// replay-vs-mismatch with further statements inside the same tx
	// (a bare-INSERT 23505 would abort it → 25P02).
	err := tx.QueryRow(ctx, `
		INSERT INTO deposit_confirmations
		    (funding_transaction_id, source, sender_name, sender_account,
		     payload_sha256, received_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (funding_transaction_id, source) DO NOTHING
		RETURNING id, created_at`,
		c.FundingTransactionID, c.Source, c.SenderName, c.SenderAccount,
		c.PayloadSHA256, c.ReceivedBy).
		Scan(&c.ID, &c.CreatedAt)
	if err != nil {
		if stderrors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIdemConflict
		}
		return nil, wrapCode("INTERNAL_ERROR", "deposit confirmation insert", err)
	}
	return &c, nil
}

// DepositConfirmations lists the recorded source confirmations inside tx.
func (s *PgStore) DepositConfirmations(ctx context.Context, tx pgx.Tx,
	fundingTxID int64) ([]DepositConfirmationRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, funding_transaction_id, source, sender_name,
		       sender_account, payload_sha256, received_by, created_at
		FROM deposit_confirmations
		WHERE funding_transaction_id = $1 ORDER BY id`, fundingTxID)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit confirmations", err)
	}
	defer rows.Close()
	var out []DepositConfirmationRow
	for rows.Next() {
		var c DepositConfirmationRow
		if err := rows.Scan(&c.ID, &c.FundingTransactionID, &c.Source,
			&c.SenderName, &c.SenderAccount, &c.PayloadSHA256,
			&c.ReceivedBy, &c.CreatedAt); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "confirmation scan", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DepositByIdemKey resolves a replayed (account_id, idempotency_key)
// DEPOSIT row for the §8.8 replay-or-mismatch check.
func (s *PgStore) DepositByIdemKey(ctx context.Context, accountID int64,
	key string) (*FundingTxRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, account_id, currency, type::text, amount::text, status::text,
		       reference, bank_method::text, reference_account,
		       usd_amount::text, review_tier, review_deadline,
		       created_at, confirmed_at, completed_at
		FROM funding_transactions
		WHERE account_id = $1 AND idempotency_key = $2 AND type = 'DEPOSIT'`,
		accountID, key)
	r, err := scanFundingTx(row)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// DepositByReference finds a PENDING DEPOSIT row for the account with the
// given client/bank reference — the intent-matching seam.
func (s *PgStore) DepositByReference(ctx context.Context, accountID int64,
	reference string) (*FundingTxRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, account_id, currency, type::text, amount::text, status::text,
		       reference, bank_method::text, reference_account,
		       usd_amount::text, review_tier, review_deadline,
		       created_at, confirmed_at, completed_at
		FROM funding_transactions
		WHERE account_id = $1 AND type = 'DEPOSIT' AND status = 'PENDING'
		  AND reference = $2
		ORDER BY id LIMIT 1`, accountID, reference)
	return scanFundingTx(row)
}

// DepositByBankRef finds a DEPOSIT row for the account carrying this
// bank transaction reference in ANY status — the ingest dedup anchor:
// a re-polled statement line must never create a second deposit, even
// after the first already resolved (PENDING_REVIEW / COMPLETED /
// REJECTED).
func (s *PgStore) DepositByBankRef(ctx context.Context, accountID int64,
	reference string) (*FundingTxRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, account_id, currency, type::text, amount::text, status::text,
		       reference, bank_method::text, reference_account,
		       usd_amount::text, review_tier, review_deadline,
		       created_at, confirmed_at, completed_at
		FROM funding_transactions
		WHERE account_id = $1 AND type = 'DEPOSIT' AND reference = $2
		ORDER BY id LIMIT 1`, accountID, reference)
	return scanFundingTx(row)
}

// StampDepositBankRef re-stamps an adopted client-intent row with the
// detected bank transaction reference + computed review tier so later
// statement re-polls dedup on the bank reference rather than creating
// a duplicate deposit.
func (s *PgStore) StampDepositBankRef(ctx context.Context, tx pgx.Tx,
	id int64, bankRef, reviewTier string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE funding_transactions
		SET reference = $2, review_tier = $3, updated_at = now()
		WHERE id = $1 AND type = 'DEPOSIT' AND status = 'PENDING'`,
		id, bankRef, reviewTier)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "stamp bank reference", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("INVALID_LIFECYCLE_TRANSITION",
			"deposit %d is no longer adoptable", id)
	}
	return nil
}

// FundingTxPayloadSHA returns the stored payload hash for idempotent
// replay verification.
func (s *PgStore) FundingTxPayloadSHA(ctx context.Context, tx pgx.Tx,
	id int64) (*string, error) {
	var ph *string
	if err := tx.QueryRow(ctx,
		`SELECT payload_sha256 FROM funding_transactions WHERE id = $1`, id).
		Scan(&ph); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "payload hash", err)
	}
	return ph, nil
}

// DepositVelocity aggregates the account's deposit count + sum in the
// trailing window — the STANDARD-tier velocity check input (in-flight
// deposits count: PENDING/CONFIRMED/PENDING_REVIEW/COMPLETED).
func (s *PgStore) DepositVelocity(ctx context.Context, accountID int64,
	since time.Time) (int64, decimal.Decimal, error) {
	var n int64
	var sum *string
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(amount), 0)::text
		FROM funding_transactions
		WHERE account_id = $1 AND type = 'DEPOSIT' AND created_at > $2
		  AND status IN ('PENDING','CONFIRMED','PENDING_REVIEW','COMPLETED')`,
		accountID, since).Scan(&n, &sum)
	if err != nil {
		return 0, decimal.Zero, wrapCode("INTERNAL_ERROR", "deposit velocity", err)
	}
	total := decimal.Zero
	if sum != nil {
		total = decimal.RequireFromString(*sum)
	}
	return n, total, nil
}

// SetDepositReview transitions a deposit with the review-deadline stamp
// (PENDING_REVIEW) or the terminal verdict (COMPLETED/FAILED).
func (s *PgStore) SetDepositReview(ctx context.Context, tx pgx.Tx, id int64,
	status string, deadline *time.Time, reviewedBy *int64, at time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE funding_transactions
		SET status = $2::funding_status_enum,
		    review_deadline = COALESCE($3, review_deadline),
		    reviewed_by = COALESCE($4, reviewed_by),
		    reviewed_at = $5,
		    confirmed_at = COALESCE($6, confirmed_at),
		    updated_at = now()
		WHERE id = $1 AND type = 'DEPOSIT'`,
		id, status, deadline, reviewedBy, at, at)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "deposit review", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "deposit %d not found", id)
	}
	return nil
}

// SetDepositCompleted marks the deposit COMPLETED at credit.
func (s *PgStore) SetDepositCompleted(ctx context.Context, tx pgx.Tx,
	id int64, at time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE funding_transactions
		SET status = 'COMPLETED', completed_at = $2,
		    confirmed_at = COALESCE(confirmed_at, $2), updated_at = now()
		WHERE id = $1 AND type = 'DEPOSIT'`, id, at)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "deposit complete", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "deposit %d not found", id)
	}
	return nil
}

// ExpiredReviewDeadlines lists funding rows whose 4h PENDING_REVIEW SLA
// lapsed — the deposit/withdrawal review-breach sweep input.
func (s *PgStore) ExpiredReviewDeadlines(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM funding_transactions
		WHERE status = 'PENDING_REVIEW' AND review_deadline IS NOT NULL
		  AND review_deadline < now()
		ORDER BY review_deadline LIMIT $1`, limit)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "review deadline scan", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "review deadline scan row", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BumpReviewDeadline rolls a PENDING_REVIEW row's deadline forward —
// the SLA-breach sweep moves each breached epoch exactly one window so
// every 4h of continued breach alerts once (never silently cleared).
func (s *PgStore) BumpReviewDeadline(ctx context.Context, tx pgx.Tx,
	id int64, deadline time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE funding_transactions
		SET review_deadline = $2
		WHERE id = $1 AND status = 'PENDING_REVIEW'`, id, deadline)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "review deadline bump", err)
	}
	if tag.RowsAffected() != 1 {
		return errCode("INVALID_LIFECYCLE_TRANSITION",
			"funding row is not PENDING_REVIEW")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Dispatch queue + ops alerts + replenishment + nostro balances (199/018)
// ---------------------------------------------------------------------------

// InsertDispatchQueue enqueues a CONFIRMED withdrawal; a repeat for the
// same withdrawal replays to the existing row (UNIQUE withdrawal_id).
func (s *PgStore) InsertDispatchQueue(ctx context.Context, tx pgx.Tx,
	withdrawalID int64, reason string) (*DispatchQueueRow, error) {
	var q DispatchQueueRow
	err := tx.QueryRow(ctx, `
		INSERT INTO withdrawal_dispatch_queue (withdrawal_id, reason)
		VALUES ($1, $2)
		ON CONFLICT (withdrawal_id) DO UPDATE SET reason = EXCLUDED.reason
		RETURNING id, withdrawal_id, status, reason, queued_at, dispatched_at,
		          attempts, last_error`, withdrawalID, reason).
		Scan(&q.ID, &q.WithdrawalID, &q.Status, &q.Reason, &q.QueuedAt,
			&q.DispatchedAt, &q.Attempts, &q.LastError)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "dispatch queue insert", err)
	}
	return &q, nil
}

// DispatchQueueForUpdate locks the queue row for a withdrawal inside tx.
func (s *PgStore) DispatchQueueForUpdate(ctx context.Context, tx pgx.Tx,
	withdrawalID int64) (*DispatchQueueRow, error) {
	var q DispatchQueueRow
	err := tx.QueryRow(ctx, `
		SELECT id, withdrawal_id, status, reason, queued_at, dispatched_at,
		       attempts, last_error
		FROM withdrawal_dispatch_queue
		WHERE withdrawal_id = $1 FOR UPDATE`, withdrawalID).
		Scan(&q.ID, &q.WithdrawalID, &q.Status, &q.Reason, &q.QueuedAt,
			&q.DispatchedAt, &q.Attempts, &q.LastError)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "dispatch queue lock", err)
	}
	return &q, nil
}

// SetDispatchQueueStatus updates the queue row inside tx.
func (s *PgStore) SetDispatchQueueStatus(ctx context.Context, tx pgx.Tx,
	id int64, status string, lastErr *string, dispatchedAt *time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE withdrawal_dispatch_queue
		SET status = $2, attempts = attempts + 1, last_error = $3,
		    dispatched_at = COALESCE($4, dispatched_at)
		WHERE id = $1`, id, status, lastErr, dispatchedAt)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "dispatch queue update", err)
	}
	return nil
}

// InsertFundingOpsAlert persists the durable alert row inside tx.
func (s *PgStore) InsertFundingOpsAlert(ctx context.Context, tx pgx.Tx,
	a FundingOpsAlertRow) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO funding_ops_alerts
		    (code, severity, funding_transaction_id, account_id, currency,
		     amount, summary, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		a.Code, a.Severity, a.FundingTransactionID, a.AccountID, a.Currency,
		decPtr(a.Amount), a.Summary, a.Detail).Scan(&id)
	if err != nil {
		return 0, wrapCode("INTERNAL_ERROR", "ops alert insert", err)
	}
	return id, nil
}

// ListFundingOpsAlerts returns the durable alert trail (OPEN first).
func (s *PgStore) ListFundingOpsAlerts(ctx context.Context, limit int) ([]FundingOpsAlertRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, code, severity, funding_transaction_id, account_id,
		       currency, amount::text, summary, detail, status, created_at,
		       resolved_at
		FROM funding_ops_alerts ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "ops alert list", err)
	}
	defer rows.Close()
	var out []FundingOpsAlertRow
	for rows.Next() {
		var a FundingOpsAlertRow
		var amt *string
		if err := rows.Scan(&a.ID, &a.Code, &a.Severity,
			&a.FundingTransactionID, &a.AccountID, &a.Currency, &amt,
			&a.Summary, &a.Detail, &a.Status, &a.CreatedAt,
			&a.ResolvedAt); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "ops alert scan", err)
		}
		if amt != nil {
			v := decimal.RequireFromString(*amt)
			a.Amount = &v
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// NostroBalances lists nostro_accounts rows (with balance) for a
// currency; empty currency lists all currencies.
func (s *PgStore) NostroBalances(ctx context.Context, currency string) ([]NostroBalanceRow, error) {
	q := `
		SELECT id, currency, bank_name, bank_code, account_number, iban,
		       balance::text, status::text, updated_at
		FROM nostro_accounts`
	args := []any{}
	if currency != "" {
		q += ` WHERE currency = $1`
		args = append(args, currency)
	}
	q += ` ORDER BY currency, id`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "nostro balances", err)
	}
	defer rows.Close()
	var out []NostroBalanceRow
	for rows.Next() {
		var n NostroBalanceRow
		var bal *string
		if err := rows.Scan(&n.ID, &n.Currency, &n.BankName, &n.BankCode,
			&n.AccountNumber, &n.IBAN, &bal, &n.Status, &n.UpdatedAt); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "nostro balance scan", err)
		}
		if bal != nil {
			n.Balance = decimal.RequireFromString(*bal)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NostroForUpdate locks a nostro_accounts row inside tx.
func (s *PgStore) NostroForUpdate(ctx context.Context, tx pgx.Tx,
	id int64) (*NostroBalanceRow, error) {
	var n NostroBalanceRow
	var bal *string
	err := tx.QueryRow(ctx, `
		SELECT id, currency, bank_name, bank_code, account_number, iban,
		       balance::text, status::text, updated_at
		FROM nostro_accounts WHERE id = $1 FOR UPDATE`, id).
		Scan(&n.ID, &n.Currency, &n.BankName, &n.BankCode, &n.AccountNumber,
			&n.IBAN, &bal, &n.Status, &n.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, errf("NOT_FOUND", "nostro account %d not found", id)
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "nostro lock", err)
	}
	if bal != nil {
		n.Balance = decimal.RequireFromString(*bal)
	}
	return &n, nil
}

// AdjustNostroBalance applies a signed delta to a nostro row inside tx —
// the replenishment book entry. Negative results are refused
// (INSUFFICIENT_BALANCE) — a nostro balance never goes overdrawn.
func (s *PgStore) AdjustNostroBalance(ctx context.Context, tx pgx.Tx,
	id int64, delta decimal.Decimal) (*NostroBalanceRow, error) {
	var n NostroBalanceRow
	var bal *string
	err := tx.QueryRow(ctx, `
		UPDATE nostro_accounts
		SET balance = balance + $2::numeric, updated_at = now()
		WHERE id = $1 AND balance + $2::numeric >= 0
		RETURNING id, currency, bank_name, bank_code, account_number, iban,
		          balance::text, status::text, updated_at`,
		id, delta.String()).
		Scan(&n.ID, &n.Currency, &n.BankName, &n.BankCode, &n.AccountNumber,
			&n.IBAN, &bal, &n.Status, &n.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, errCode("INSUFFICIENT_BALANCE",
			"nostro source balance cannot cover the replenishment")
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "nostro adjust", err)
	}
	if bal != nil {
		n.Balance = decimal.RequireFromString(*bal)
	}
	return &n, nil
}

// ---------------------------------------------------------------------------
// Replenishment requests (199)
// ---------------------------------------------------------------------------

// InsertReplenishmentRequest records a PENDING_APPROVAL request inside tx.
func (s *PgStore) InsertReplenishmentRequest(ctx context.Context, tx pgx.Tx,
	r ReplenishmentRow) (*ReplenishmentRow, error) {
	err := tx.QueryRow(ctx, `
		INSERT INTO nostro_replenishment_requests
		    (currency, amount, source_nostro_id, target_nostro_id,
		     requested_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, status, created_at`,
		r.Currency, r.Amount.String(), r.SourceNostroID, r.TargetNostroID,
		r.RequestedBy).
		Scan(&r.ID, &r.Status, &r.CreatedAt)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "replenishment insert", err)
	}
	return &r, nil
}

// ReplenishmentForUpdate locks a request inside tx.
func (s *PgStore) ReplenishmentForUpdate(ctx context.Context, tx pgx.Tx,
	id int64) (*ReplenishmentRow, error) {
	var r ReplenishmentRow
	var amt *string
	err := tx.QueryRow(ctx, `
		SELECT id, currency, amount::text, source_nostro_id,
		       target_nostro_id, status, requested_by, approved_by,
		       decision_note, created_at, decided_at
		FROM nostro_replenishment_requests WHERE id = $1 FOR UPDATE`, id).
		Scan(&r.ID, &r.Currency, &amt, &r.SourceNostroID, &r.TargetNostroID,
			&r.Status, &r.RequestedBy, &r.ApprovedBy, &r.DecisionNote,
			&r.CreatedAt, &r.DecidedAt)
	if err == pgx.ErrNoRows {
		return nil, errf("NOT_FOUND", "replenishment request %d not found", id)
	}
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "replenishment lock", err)
	}
	if amt != nil {
		r.Amount = decimal.RequireFromString(*amt)
	}
	return &r, nil
}

// SetReplenishmentStatus stamps the decision inside tx.
func (s *PgStore) SetReplenishmentStatus(ctx context.Context, tx pgx.Tx,
	id int64, status string, approvedBy int64, note *string, at time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE nostro_replenishment_requests
		SET status = $2, approved_by = $3, decision_note = $4, decided_at = $5
		WHERE id = $1`, id, status, approvedBy, note, at)
	if err != nil {
		return wrapCode("INTERNAL_ERROR", "replenishment status", err)
	}
	if tag.RowsAffected() == 0 {
		return errf("NOT_FOUND", "replenishment request %d not found", id)
	}
	return nil
}

// ListReplenishments returns requests, newest first.
func (s *PgStore) ListReplenishments(ctx context.Context, limit int) ([]ReplenishmentRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, currency, amount::text, source_nostro_id,
		       target_nostro_id, status, requested_by, approved_by,
		       decision_note, created_at, decided_at
		FROM nostro_replenishment_requests ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "replenishment list", err)
	}
	defer rows.Close()
	var out []ReplenishmentRow
	for rows.Next() {
		var r ReplenishmentRow
		var amt *string
		if err := rows.Scan(&r.ID, &r.Currency, &amt, &r.SourceNostroID,
			&r.TargetNostroID, &r.Status, &r.RequestedBy, &r.ApprovedBy,
			&r.DecisionNote, &r.CreatedAt, &r.DecidedAt); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "replenishment scan", err)
		}
		if amt != nil {
			r.Amount = decimal.RequireFromString(*amt)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
