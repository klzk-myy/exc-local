// insurance_fund.go — Insurance Fund service (Phase-19 Task 19.3.4;
// spec §5.15, §13.4–§13.6c, §24 #35/#36/#94/#97/#218).
//
// The fund is the venue's loss-absorption layer, standing between a
// liquidated position and counterparty loss. Every movement is one
// atomic SERIALIZABLE transaction:
//
//	insurance_fund.balance (per-currency row, mig 016 + 230 unique idx)
//	  → insurance_fund_transactions audit row (mig 230)
//	  → balanced double-entry GL journal via the ledger poster's
//	    tx-scoped PostJournal (Task 3.3.6 seam)
//	  → wallet effects via ledger.AccountEffect when a client balance
//	    moves (LP rebate / NBP restitution / auction deficiency)
//
// GL pair conventions (chart of accounts, mig 088):
//
//	fund ← client penalty:   DR 2010_CUSTOMER_LIABILITY / CR 2210_INSURANCE_FUND_LIABILITY
//	fund → client (rebate,
//	  NBP, deficiency):      DR 2210_INSURANCE_FUND_LIABILITY / CR 2010_CUSTOMER_LIABILITY
//	house seeds fund:        DR 1150_INSURANCE_FUND_NOSTRO  / CR 2210_INSURANCE_FUND_LIABILITY
//	retained-earnings sweep: DR 3020_RETAINED_EARNINGS      / CR 2210_INSURANCE_FUND_LIABILITY
//	fund → house (manual out): DR 2210 / CR 1150
//	NBP beyond fund depth:   DR 5200_NBP_RESTITUTION_EXPENSE (house P&L) / CR 2010
//
// Balance semantics: signed; may go negative (spec §13.6 ADL threshold
// is expressed against a negative balance). Depletion rule per §13.6:
// balance < max(-$100K, 1% × fund target) — the fund is "depleted"
// BEFORE literal zero so ADL engages while the fund can still act.
package risk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// OpsAlert is the settlement package's alert shape (alias so
// settlement.PublisherAlerter satisfies OpsAlerter without an adapter).
type OpsAlert = settlement.OpsAlert

// OpsAlerter is the ops-paging seam — production binds
// settlement.PublisherAlerter over NATS (ops.alerts.*).
type OpsAlerter interface {
	Raise(ctx context.Context, a OpsAlert) error
}

// Alert severities (§2.7 paging tiers — P0 pages, P1 tickets).
const (
	SeverityP0 = settlement.SeverityP0
	SeverityP1 = settlement.SeverityP1
)

// OpsAlertSubject is the JetStream subject for risk-domain ops alerts.
const OpsAlertSubject = "ops.alerts.risk"

// Fund movement reason vocabulary — mirrors the migration-230 CHECK.
const (
	FundReasonLiquidationPenalty = "LIQUIDATION_PENALTY"
	FundReasonAuctionDeficiency  = "AUCTION_DEFICIENCY"
	FundReasonLPRebate           = "LP_REBATE"
	FundReasonNBPRestitution     = "NBP_RESTITUTION"
	FundReasonCapitalInjection   = "CAPITAL_INJECTION"
	FundReasonRetainedEarnings   = "RETAINED_EARNINGS_SWEEP"
	FundReasonContingentFacility = "CONTINGENT_FACILITY"
	FundReasonManualAdjustment   = "MANUAL_ADJUSTMENT"
)

// Fund error/alert codes. HTTP-mapped codes are registered in spec §23
// (LIQUIDATION_FAILED, INSURANCE_FUND_DEPLETED, INSURANCE_FUND_EXHAUSTED,
// NBP_RESERVE_DEFICIT); the remainder are internal-only operational
// alerts (§23 internal-only list).
const (
	CodeLiquidationFailed            = "LIQUIDATION_FAILED"              // §23, 500, L1
	CodeInsuranceFundDepleted        = "INSURANCE_FUND_DEPLETED"         // §23, 500, L1/L0
	CodeInsuranceFundExhausted       = "INSURANCE_FUND_EXHAUSTED"        // §23, 500, L0 fail-closed
	CodeNBPRestitutionReserveDeficit = "NBP_RESERVE_DEFICIT"             // §23, 500, L1
	CodeADLTriggered                 = "ADL_TRIGGERED"                   // internal L1 event
	CodeNBPDeficitTriggered          = "NBP_DEFICIT_TRIGGERED"           // internal L1 event
	CodeLiquidationWorkerLockTimeout = "LIQUIDATION_WORKER_LOCK_TIMEOUT" // internal L1 alert
	codeInsuranceFundLowBalance      = "INSURANCE_FUND_LOW_BALANCE"      // internal P1 alert
)

// Canonical defaults (Task 19.3.4 item 4; spec §13.6 ADL threshold).
var (
	// DefaultFundLowBalance is the P1 low-balance watermark: $100K.
	DefaultFundLowBalance = decimal.NewFromInt(100_000)
	// DefaultADLNegativeFloor is the §13.6 absolute depletion floor.
	DefaultADLNegativeFloor = decimal.NewFromInt(-100_000)
	// DefaultADLEquityFraction is the §13.6 "1% of fund equity" leg.
	DefaultADLEquityFraction = decimal.NewFromFloat(0.01)
	// DefaultLPRebateFraction is the §13.4 LP incentive: 0.05% of the
	// auction fill notional, paid from the fund.
	DefaultLPRebateFraction = decimal.NewFromFloat(0.0005)
)

// JournalPoster is the ledger seam — *settlement.LedgerService
// satisfies it. PostJournal runs inside the caller's SERIALIZABLE tx so
// the fund row, the audit row and the GL journal commit or abort as one.
type JournalPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
	PostJournal(ctx context.Context, tx pgx.Tx, j ledger.Journal) (ledger.PostResult, error)
}

// FundEventPublisher dispatches post-commit balance events
// (settlement.NatsPublisher in production).
type FundEventPublisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}

// FundMovement describes one fund credit or debit. Amount is the positive
// magnitude; Currency the denomination. AccountID non-zero carries a
// wallet effect on that account (rebate credit, penalty charge, NBP
// restitution, deficiency cover).
type FundMovement struct {
	Reason         string // one of FundReason*
	Currency       string
	Amount         decimal.Decimal
	ReferenceType  string // 'liquidation' | 'auction' | 'nbp' | 'manual' | 'sweep'
	ReferenceID    int64
	AccountID      int64  // wallet effect target; 0 = GL-only movement
	IdempotencyKey string // e.g. "liq-penalty:{event_id}" — replays dedup
	Narrative      string
}

// FundMovementResult reports the committed movement.
type FundMovementResult struct {
	BalanceAfter decimal.Decimal
	JournalID    int64
	MovementID   int64
	Replayed     bool
	events       []ledger.BalanceEvent // committed wallet events
}

// FundTxRow is one insurance_fund_transactions audit row (admin history).
type FundTxRow struct {
	ID             int64           `json:"id"`
	Currency       string          `json:"currency"`
	Direction      string          `json:"direction"`
	Amount         decimal.Decimal `json:"amount"`
	Reason         string          `json:"reason"`
	ReferenceType  *string         `json:"reference_type,omitempty"`
	ReferenceID    *int64          `json:"reference_id,omitempty"`
	AccountID      *int64          `json:"account_id,omitempty"`
	JournalEntryID *int64          `json:"journal_entry_id,omitempty"`
	BalanceAfter   decimal.Decimal `json:"balance_after"`
	CreatedAt      time.Time       `json:"created_at"`
}

// FundBalance is one per-currency fund row.
type FundBalance struct {
	Currency           string           `json:"currency"`
	Balance            decimal.Decimal  `json:"balance"`
	DepletionThreshold *decimal.Decimal `json:"depletion_threshold,omitempty"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

// InsuranceFundConfig tunes the service. Zero fields take the canonical
// defaults.
type InsuranceFundConfig struct {
	LowBalanceAlertUSD decimal.Decimal // P1 watermark per currency row
	ADLNegativeFloor   decimal.Decimal // §13.6 absolute depletion floor
	ADLEquityFraction  decimal.Decimal // §13.6 1%-of-target leg
	PostedBy           string          // journal posted_by identity
	MaxAttempts        int             // §5.40 serializable retry budget
}

func (c InsuranceFundConfig) normalize() InsuranceFundConfig {
	if !c.LowBalanceAlertUSD.IsPositive() {
		c.LowBalanceAlertUSD = DefaultFundLowBalance
	}
	if c.ADLNegativeFloor.IsZero() {
		c.ADLNegativeFloor = DefaultADLNegativeFloor
	}
	if !c.ADLEquityFraction.IsPositive() {
		c.ADLEquityFraction = DefaultADLEquityFraction
	}
	if c.PostedBy == "" {
		c.PostedBy = "risk-insurance-fund"
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	return c
}

// InsuranceFundService owns insurance_fund balance truth and every fund
// movement. Construct via NewInsuranceFundService.
type InsuranceFundService struct {
	pool    *pgxpool.Pool
	rdb     *excredis.Client
	poster  JournalPoster
	pub     FundEventPublisher
	alerter OpsAlerter
	cfg     InsuranceFundConfig
	now     func() time.Time
	logf    func(format string, args ...any)
}

// InsuranceFundDeps wires the service.
type InsuranceFundDeps struct {
	Pool    *pgxpool.Pool
	Redis   *excredis.Client // required when movements carry wallet effects
	Poster  JournalPoster    // required — every movement posts GL (§5.3 inv 4)
	Pub     FundEventPublisher
	Alerter OpsAlerter
	Config  InsuranceFundConfig
	Now     func() time.Time
	Logf    func(format string, args ...any)
}

// NewInsuranceFundService builds the service. Poster is mandatory —
// a fund movement without its GL journal is a §5.3 invariant breach.
func NewInsuranceFundService(d InsuranceFundDeps) (*InsuranceFundService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("insurance fund: nil pgx pool")
	}
	if d.Poster == nil {
		return nil, fmt.Errorf("insurance fund: nil journal poster — GL posting is mandatory (spec §5.3)")
	}
	s := &InsuranceFundService{
		pool: d.Pool, rdb: d.Redis, poster: d.Poster, pub: d.Pub,
		alerter: d.Alerter, cfg: d.Config.normalize(),
		now: d.Now, logf: d.Logf,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Balance reads
// ---------------------------------------------------------------------------

// Balance returns the fund row for ccy, or (nil, nil) when the currency
// has never been touched (a never-funded currency has balance zero —
// callers that need the distinction test for nil).
func (s *InsuranceFundService) Balance(ctx context.Context, ccy string) (*FundBalance, error) {
	var b FundBalance
	var bal, thr *string
	err := s.pool.QueryRow(ctx, `
		SELECT currency, balance::text, depletion_threshold::text, updated_at
		FROM insurance_fund WHERE currency = $1`, ccy).
		Scan(&b.Currency, &bal, &thr, &b.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insurance_fund read", err)
	}
	b.Balance = decimal.RequireFromString(*bal)
	if thr != nil {
		t := decimal.RequireFromString(*thr)
		b.DepletionThreshold = &t
	}
	return &b, nil
}

// Balances returns every fund row, currency-ordered (admin endpoint).
func (s *InsuranceFundService) Balances(ctx context.Context) ([]FundBalance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, balance::text, depletion_threshold::text, updated_at
		FROM insurance_fund ORDER BY currency`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insurance_fund list", err)
	}
	defer rows.Close()
	var out []FundBalance
	for rows.Next() {
		var b FundBalance
		var bal, thr *string
		if err := rows.Scan(&b.Currency, &bal, &thr, &b.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "insurance_fund scan", err)
		}
		b.Balance = decimal.RequireFromString(*bal)
		if thr != nil {
			t := decimal.RequireFromString(*thr)
			b.DepletionThreshold = &t
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// History returns fund movements newest-first with keyset pagination
// ((id < cursor) descending).
func (s *InsuranceFundService) History(ctx context.Context, ccy string,
	cursor int64, limit int) ([]FundTxRow, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id, currency, direction, amount::text, reason,
	             reference_type, reference_id, account_id, journal_entry_id,
	             balance_after::text, created_at
	      FROM insurance_fund_transactions WHERE ($1 = '' OR currency = $1)`
	args := []any{ccy}
	if cursor > 0 {
		q += ` AND id < $2`
		args = append(args, cursor)
	}
	q += ` ORDER BY id DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fund history", err)
	}
	defer rows.Close()
	var out []FundTxRow
	for rows.Next() {
		var t FundTxRow
		var amt, aft string
		if err := rows.Scan(&t.ID, &t.Currency, &t.Direction, &amt, &t.Reason,
			&t.ReferenceType, &t.ReferenceID, &t.AccountID, &t.JournalEntryID,
			&aft, &t.CreatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "fund history scan", err)
		}
		t.Amount = decimal.RequireFromString(amt)
		t.BalanceAfter = decimal.RequireFromString(aft)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Depletion & low-balance evaluation (spec §13.6)
// ---------------------------------------------------------------------------

// Depleted reports whether the fund is below the §13.6 ADL depletion
// threshold for ccy: balance < max(-$100K, 1% × fund target). The
// governance target (insurance_fund_governance.target_balance, mig 230)
// supplies fund equity; with no governance row the -$100K absolute floor
// applies. A per-currency depletion_threshold on the insurance_fund row
// (mig 016) overrides both when set.
func (s *InsuranceFundService) Depleted(ctx context.Context, ccy string) (bool, decimal.Decimal, error) {
	b, err := s.Balance(ctx, ccy)
	if err != nil {
		return false, decimal.Zero, err
	}
	bal := decimal.Zero
	if b != nil {
		bal = b.Balance
		if b.DepletionThreshold != nil {
			return bal.LessThan(*b.DepletionThreshold), bal, nil
		}
	}
	var target *string
	if err := s.pool.QueryRow(ctx, `
		SELECT target_balance::text FROM insurance_fund_governance
		WHERE currency = $1`, ccy).Scan(&target); err != nil && err != pgx.ErrNoRows {
		return false, decimal.Zero, excerrors.Wrap("INTERNAL_ERROR", "fund governance read", err)
	}
	floor := s.cfg.ADLNegativeFloor
	if target != nil {
		frac := decimal.RequireFromString(*target).Mul(s.cfg.ADLEquityFraction)
		if frac.GreaterThan(floor) {
			floor = frac
		}
	}
	return bal.LessThan(floor), bal, nil
}

// ---------------------------------------------------------------------------
// Movements — the atomic write path
// ---------------------------------------------------------------------------

// Credit adds amount to the fund (liquidation penalty, capital injection,
// replenishment sweep, contingent facility draw).
func (s *InsuranceFundService) Credit(ctx context.Context, m FundMovement) (*FundMovementResult, error) {
	return s.move(ctx, m, true)
}

// Debit subtracts amount from the fund (LP rebate, NBP restitution,
// auction deficiency cover, manual withdrawal). The balance may go
// negative (§13.6 depletion floor is negative) — Depleted() is the gate
// callers use to decide between fund absorption and the ADL fallback.
func (s *InsuranceFundService) Debit(ctx context.Context, m FundMovement) (*FundMovementResult, error) {
	return s.move(ctx, m, false)
}

// movementJournal builds the balanced GL journal for the movement.
// Returned effects are wallet mutations the caller posts through the
// ledger in the same transaction.
func (s *InsuranceFundService) movementJournal(m FundMovement, credit bool) (ledger.Journal, error) {
	ccy := m.Currency
	amt := m.Amount
	narrative := func(s string) string {
		if len(s) > 255 {
			return s[:255]
		}
		return s
	}
	var lines []ledger.Line
	var effects []ledger.AccountEffect
	switch {
	case credit && m.Reason == FundReasonLiquidationPenalty:
		// Liquidated client is charged; the fund absorbs the spread.
		lines = []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(ccy), ccy, amt,
				narrative(fmt.Sprintf("liquidation penalty ref %s/%d", m.ReferenceType, m.ReferenceID))),
			ledger.CreditLine(ledger.InsuranceFundLiability(ccy), ccy, amt,
				"liquidation penalty to insurance fund"),
		}
		effects = []ledger.AccountEffect{{
			AccountID: m.AccountID, Currency: ccy,
			AvailableDelta: amt.Neg(), AllowNegative: true,
		}}
	case credit && (m.Reason == FundReasonRetainedEarnings):
		lines = []ledger.Line{
			ledger.DebitLine(ledger.RetainedEarnings(ccy), ccy, amt,
				"replenishment sweep from retained earnings"),
			ledger.CreditLine(ledger.InsuranceFundLiability(ccy), ccy, amt,
				"insurance fund replenishment"),
		}
	case credit:
		// CAPITAL_INJECTION / CONTINGENT_FACILITY / MANUAL_ADJUSTMENT:
		// house earmarks segregated nostro cash to the fund.
		lines = []ledger.Line{
			ledger.DebitLine(ledger.InsuranceFundNostro(ccy), ccy, amt,
				narrative(fmt.Sprintf("%s into fund nostro", m.Reason))),
			ledger.CreditLine(ledger.InsuranceFundLiability(ccy), ccy, amt,
				"insurance fund capitalization"),
		}
	case !credit && m.Reason == FundReasonManualAdjustment:
		lines = []ledger.Line{
			ledger.DebitLine(ledger.InsuranceFundLiability(ccy), ccy, amt,
				"manual fund withdrawal (dual-control)"),
			ledger.CreditLine(ledger.InsuranceFundNostro(ccy), ccy, amt,
				"fund nostro release"),
		}
	case !credit:
		// LP_REBATE / NBP_RESTITUTION / AUCTION_DEFICIENCY — fund pays a
		// client wallet (house liability shifts fund → customer).
		label := map[string]string{
			FundReasonLPRebate:          "LP auction rebate",
			FundReasonNBPRestitution:    "retail NBP restitution",
			FundReasonAuctionDeficiency: "auction deficiency cover",
		}[m.Reason]
		lines = []ledger.Line{
			ledger.DebitLine(ledger.InsuranceFundLiability(ccy), ccy, amt,
				label+" — fund debit"),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, amt,
				narrative(label+" to client "+fmt.Sprint(m.AccountID))),
		}
		effects = []ledger.AccountEffect{{
			AccountID: m.AccountID, Currency: ccy,
			AvailableDelta: amt,
		}}
	}
	entryType := ledger.EntryLiquidation
	switch m.Reason {
	case FundReasonCapitalInjection, FundReasonRetainedEarnings,
		FundReasonContingentFacility, FundReasonManualAdjustment:
		entryType = ledger.EntryAdjustment
	case FundReasonNBPRestitution:
		entryType = ledger.EntryAdjustment
	}
	j := ledger.Journal{
		EntryType:      entryType,
		ReferenceID:    m.ReferenceID,
		Description:    narrative(fmt.Sprintf("insurance fund %s %s %s", m.Reason, m.Direction(credit), amt)),
		PostedBy:       s.cfg.PostedBy,
		IdempotencyKey: m.IdempotencyKey,
		Lines:          lines,
		Effects:        effects,
	}
	return j, j.Validate()
}

// Direction labels the movement for narratives.
func (m FundMovement) Direction(credit bool) string {
	if credit {
		return "CREDIT"
	}
	return "DEBIT"
}

// move is the single atomic write path:
//
//	(wallet locks when effects) → BEGIN SERIALIZABLE →
//	upsert+FOR UPDATE insurance_fund row → apply delta →
//	INSERT insurance_fund_transactions → PostJournal (lines+effects) →
//	COMMIT → dispatch BalanceChanged → low-balance/depletion alerts.
func (s *InsuranceFundService) move(ctx context.Context, m FundMovement, credit bool) (*FundMovementResult, error) {
	if !m.Amount.IsPositive() {
		return nil, excerrors.New("INVALID_REQUEST", "fund movement amount must be > 0")
	}
	if len(m.Currency) != 3 {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("fund currency %q must be 3-letter ISO", m.Currency))
	}
	j, err := s.movementJournal(m, credit)
	if err != nil {
		return nil, err
	}

	// Wallet effects need the §5.3 account mutex held across the tx —
	// postJournalTx FOR UPDATEs the balances row but the Redis lock is
	// the cross-service serialization contract.
	var lockToken string
	locked := []int64{}
	if len(j.Effects) > 0 {
		if s.rdb == nil {
			return nil, excerrors.New(ledger.CodeLedgerLockUnavailable,
				"fund movement carries a wallet effect but redis lock backend is nil")
		}
		var terr error
		lockToken, terr = fundLockToken()
		if terr != nil {
			return nil, excerrors.Wrap(ledger.CodeLedgerLockUnavailable, "lock token", terr)
		}
		for _, id := range j.AffectedAccounts() {
			ok, lerr := s.rdb.TryLockAccount(ctx, fmt.Sprint(id), lockToken, excredis.AccountLockTTL)
			if lerr != nil {
				s.unlockAll(ctx, locked, lockToken)
				return nil, excerrors.Wrap(ledger.CodeLedgerLockUnavailable,
					fmt.Sprintf("fund lock acct %d", id), lerr)
			}
			if !ok {
				s.unlockAll(ctx, locked, lockToken)
				return nil, excerrors.New(ledger.CodeAccountBusy,
					fmt.Sprintf("fund lock acct %d held by another writer", id))
			}
			locked = append(locked, id)
		}
		defer s.unlockAll(context.Background(), locked, lockToken)
	}

	var res *FundMovementResult
	var lastErr error
	for attempt := 0; attempt < s.cfg.MaxAttempts; attempt++ {
		res, lastErr = s.moveOnce(ctx, m, j, credit)
		if lastErr == nil {
			break
		}
		if !isFundRetryable(lastErr) {
			return nil, lastErr
		}
	}
	if lastErr != nil {
		return nil, excerrors.Wrap(ledger.CodeTxnConflictExhausted,
			"insurance fund movement retries exhausted", lastErr)
	}

	// Post-commit duties — dispatch balance events, then evaluate the
	// alert watermarks on the NEW committed balance.
	DispatchBalanceEvents(ctx, s.pub, res.events, s.logf)
	s.evaluateAlerts(ctx, m.Currency, res.BalanceAfter)
	return res, nil
}

// moveOnce runs the single SERIALIZABLE attempt.
func (s *InsuranceFundService) moveOnce(ctx context.Context, m FundMovement,
	j ledger.Journal, credit bool) (*FundMovementResult, error) {

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("insurance fund: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Upsert the per-currency row, then lock it.
	if _, err := tx.Exec(ctx, `
		INSERT INTO insurance_fund (currency, balance) VALUES ($1, 0)
		ON CONFLICT (currency) DO NOTHING`, m.Currency); err != nil {
		return nil, fmt.Errorf("insurance fund upsert %s: %w", m.Currency, err)
	}
	var balStr string
	if err := tx.QueryRow(ctx, `
		SELECT balance::text FROM insurance_fund WHERE currency = $1 FOR UPDATE`,
		m.Currency).Scan(&balStr); err != nil {
		return nil, fmt.Errorf("insurance fund lock %s: %w", m.Currency, err)
	}
	bal := decimal.RequireFromString(balStr)
	delta := m.Amount
	if !credit {
		delta = delta.Neg()
	}
	after := bal.Add(delta).Round(8)

	// The journal first — a ledger failure aborts before the balance row
	// mutates, keeping GL and fund balance in the same commit (or none).
	jr, err := s.poster.PostJournal(ctx, tx, j)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE insurance_fund SET balance = $2, updated_at = now()
		WHERE currency = $1`, m.Currency, after.String()); err != nil {
		return nil, fmt.Errorf("insurance fund update %s: %w", m.Currency, err)
	}

	dir := "CREDIT"
	if !credit {
		dir = "DEBIT"
	}
	var mvID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO insurance_fund_transactions
		    (currency, direction, amount, reason, reference_type, reference_id,
		     account_id, journal_entry_id, balance_after)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6::bigint,0),NULLIF($7::bigint,0),$8,$9)
		RETURNING id`,
		m.Currency, dir, m.Amount.String(), m.Reason, m.ReferenceType,
		m.ReferenceID, m.AccountID, jr.JournalID, after.String()).Scan(&mvID)
	if err != nil {
		return nil, fmt.Errorf("insurance fund tx row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("insurance fund commit: %w", err)
	}
	return &FundMovementResult{
		BalanceAfter: after, JournalID: jr.JournalID, MovementID: mvID,
		events: jr.Events,
	}, nil
}

// evaluateAlerts raises the P1 low-balance page (Task 19.3.4 item 4) and
// the L1 depletion transition on the post-commit balance.
func (s *InsuranceFundService) evaluateAlerts(ctx context.Context, ccy string, balance decimal.Decimal) {
	if s.alerter == nil {
		return
	}
	if balance.LessThan(s.cfg.LowBalanceAlertUSD) {
		s.raiseAlert(ctx, SeverityP1, codeInsuranceFundLowBalance, fmt.Sprintf(
			"insurance fund %s balance %s below low-balance watermark %s",
			ccy, balance, s.cfg.LowBalanceAlertUSD), map[string]string{
			"currency": ccy, "balance": balance.String(),
			"watermark": s.cfg.LowBalanceAlertUSD.String()})
	}
	depleted, bal, err := s.Depleted(ctx, ccy)
	if err != nil {
		s.logf("insurance fund: depletion check %s failed: %v", ccy, err)
		return
	}
	if depleted {
		s.raiseAlert(ctx, SeverityP1, CodeInsuranceFundDepleted, fmt.Sprintf(
			"insurance fund %s balance %s breached the §13.6 depletion threshold — ADL armed",
			ccy, bal), map[string]string{"currency": ccy, "balance": bal.String()})
	}
}

// raiseAlert delivers an ops alert with a bounded fresh context — the
// caller's ctx is often exhausted post-commit.
func (s *InsuranceFundService) raiseAlert(ctx context.Context, severity, code, summary string, details map[string]string) {
	if s.alerter == nil {
		s.logf("insurance fund: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	}); err != nil {
		s.logf("insurance fund: alert %s dispatch failed: %v", code, err)
	}
}

// unlockAll releases held account locks (best-effort on rollback paths).
func (s *InsuranceFundService) unlockAll(ctx context.Context, ids []int64, token string) {
	for _, id := range ids {
		if _, err := s.rdb.UnlockAccount(ctx, fmt.Sprint(id), token); err != nil {
			s.logf("insurance fund: unlock acct %d: %v", id, err)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func fundLockToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("fund-%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:])), nil
}

// isFundRetryable mirrors the §5.40 rule: SQLSTATE 40001/40P01 retry.
func isFundRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

// dispatchBalanceEvents publishes committed BalanceChanged payloads.
// Used by callers that post via the tx-scoped path and receive
// PostResult.Events back.
func DispatchBalanceEvents(ctx context.Context, pub FundEventPublisher,
	events []ledger.BalanceEvent, logf func(string, ...any)) {
	if pub == nil {
		return
	}
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			logf("insurance fund: balance event marshal: %v", err)
			continue
		}
		if err := pub.Publish(ctx, ledger.BalanceChangedSubject(ev.AccountID), payload); err != nil {
			logf("insurance fund: balance event dispatch acct %d: %v",
				ev.AccountID, err)
		}
	}
}
