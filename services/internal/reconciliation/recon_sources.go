package reconciliation

// sources.go — the data seams each checker reads. Every checker takes a
// narrow source interface (fakes in unit tests); PgLegs implements them
// all over the shared pgx pool. Read-only everywhere — reconciliation
// NEVER mutates the tables it verifies.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/recovery"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// BALANCES
// ---------------------------------------------------------------------------

// BalancesSource rebuilds the ledger↔wallet diff (the existing
// Phase-04 reconcile's scan legs — never duplicated).
type BalancesSource interface {
	WalletDiff(ctx context.Context) (*recovery.RecReconcileResult, error)
}

// ---------------------------------------------------------------------------
// GENERAL_LEDGER
// ---------------------------------------------------------------------------

// GLSource reads the §5.21 journal/line aggregates.
type GLSource interface {
	// CurrencySums returns SUM(debit)/SUM(credit) per currency across all
	// ledger_lines (the global zero-sum leg).
	CurrencySums(ctx context.Context) ([]GLCurrencySum, error)
	// JournalImbalances returns (journal, currency) pairs whose stored
	// lines do not zero-sum — should be empty while the deferred trigger
	// holds; a row means the invariant escaped the write path.
	JournalImbalances(ctx context.Context) ([]GLJournalImbalance, error)
}

type GLCurrencySum struct {
	Currency string
	Debits   decimal.Decimal
	Credits  decimal.Decimal
}

type GLJournalImbalance struct {
	JournalEntryID int64
	Currency       string
	Debits         decimal.Decimal
	Credits        decimal.Decimal
}

// ---------------------------------------------------------------------------
// ORDERS / TRADES (PG leg; the WAL leg is walSource in walscan.go)
// ---------------------------------------------------------------------------

// OrderSource reads the open-order projection + instrument symbols.
type OrderSource interface {
	OpenOrders(ctx context.Context) ([]OpenOrder, error)
	InstrumentSymbols(ctx context.Context) (map[int64]string, error)
}

// OpenOrder is a resting (ACTIVE | PARTIALLY_FILLED) order row.
type OpenOrder struct {
	ID           int64
	AccountID    int64
	InstrumentID int64
	RemainingQty decimal.Decimal // quantity − filled_qty
	Status       string
	CreatedAt    time.Time
}

// TradeSource reads the persisted trade tape.
type TradeSource interface {
	PgTrades(ctx context.Context) ([]PgTrade, error)
}

type PgTrade struct {
	ID           int64
	InstrumentID int64
	Quantity     decimal.Decimal
	Price        decimal.Decimal
}

// ---------------------------------------------------------------------------
// POSITIONS / PNL
// ---------------------------------------------------------------------------

// PositionSource reads the position projection and its fill ledger.
type PositionSource interface {
	Positions(ctx context.Context) ([]PgPosition, error)
	// FillNets returns per-(account, instrument) signed net quantity and
	// realized-P&L sums rebuilt from position_fills (BUY +q, SELL −q).
	FillNets(ctx context.Context) ([]FillNet, error)
}

type PgPosition struct {
	AccountID     int64
	InstrumentID  int64
	Side          string // LONG | SHORT
	Quantity      decimal.Decimal
	EntryPrice    decimal.Decimal
	MarkPrice     *decimal.Decimal
	UnrealizedPnL decimal.Decimal
	RealizedPnL   decimal.Decimal
}

type FillNet struct {
	AccountID    int64
	InstrumentID int64
	NetQty       decimal.Decimal // BUY − SELL
	RealizedPnL  decimal.Decimal
}

// SignedQty renders the stored side+quantity as a signed net.
func (p PgPosition) SignedQty() decimal.Decimal {
	if p.Side == "SHORT" {
		return p.Quantity.Neg()
	}
	return p.Quantity
}

// ---------------------------------------------------------------------------
// FUNDING / SETTLEMENT / FEES
// ---------------------------------------------------------------------------

// FundingSource reads the internal funding legs.
type FundingSource interface {
	// CompletedWithdrawals returns WITHDRAWAL rows in terminal COMPLETED
	// state plus their rail-payment disposition (nil = no rail leg).
	CompletedWithdrawals(ctx context.Context) ([]WithdrawalLeg, error)
	// QuarantinedDeposits returns CONFIRMED/COMPLETED deposits whose
	// suspense mapping is still quarantined (inconsistent states).
	QuarantinedDeposits(ctx context.Context) ([]QuarantineLeg, error)
	// DepositForBankTx resolves a bank-statement reference to the deposit
	// funding row (suspense bank_tx_id or funding reference); nil, nil
	// means the bank credit is unattributed.
	DepositForBankTx(ctx context.Context, bankTxID string) (*DepositLeg, error)
}

type WithdrawalLeg struct {
	FundingID     int64
	AccountID     int64
	Currency      string
	Amount        decimal.Decimal
	BankMethod    string
	RailPaymentID *int64
	RailStatus    string // "" = no rail_payments row
}

type QuarantineLeg struct {
	FundingID        int64
	MappingID        int64
	AccountID        int64
	Currency         string
	Amount           decimal.Decimal
	Status           string // funding status
	QuarantineStatus string
}

// DepositLeg is the deposit-side row a bank statement matches against.
type DepositLeg struct {
	FundingID int64
	AccountID int64
	Currency  string
	Amount    decimal.Decimal
	Status    string
}

// StatementSource is the bank-statement seam (ruling R3): the
// production feed lands with Phase-24 statement ingestion; until wired
// the engine wires nil → the statement leg is a standing INCONCLUSIVE
// marker, never a fabricated pass.
type StatementSource interface {
	StatementLines(ctx context.Context, since time.Time) ([]StatementLine, error)
}

// StatementLine is one bank-side settled credit/debit.
type StatementLine struct {
	BankTxID  string
	Currency  string
	Amount    decimal.Decimal // signed: + credit, − debit
	ValueDate time.Time
}

// SettlementSource reads instruction↔nostro-movement legs.
type SettlementSource interface {
	// SettledLegs returns SETTLED instructions joined to their movement
	// (nil movement fields = no row written).
	SettledLegs(ctx context.Context) ([]SettlementLeg, error)
	// PostedOrphans returns POSTED movements whose instruction is not
	// SETTLED (movement applied but instruction claims otherwise).
	PostedOrphans(ctx context.Context) ([]NostroOrphan, error)
	// OverduePending returns PENDING instructions whose settlement_date
	// passed (INCONCLUSIVE — ops signal, ruling R6).
	OverduePending(ctx context.Context, now time.Time) ([]int64, error)
}

type SettlementLeg struct {
	InstructionID  int64
	Currency       string
	Amount         decimal.Decimal
	Direction      string // PAY | RECEIVE
	MovementID     *int64
	MovementAmount decimal.Decimal
	MovementDir    string // DEBIT | CREDIT | ""
	MovementStatus string // PENDING | POSTED | VOID | ""
}

type NostroOrphan struct {
	MovementID      int64
	InstructionID   int64
	InstructionStat string
	Currency        string
	Amount          decimal.Decimal
}

// FeeSource reads per-trade expected fees vs collected ledger rows.
type FeeSource interface {
	// ExpectedFees returns trades rows joined to instruments for the
	// fee-currency resolution (buyer fee in base, seller fee in quote).
	ExpectedFees(ctx context.Context) ([]ExpectedFee, error)
	// CollectedFees nets the wallet FEE ledger rows per
	// (reference_id=trade, account, currency): DEBIT − CREDIT… a fee
	// charge is value OUT of the wallet → CREDIT direction; the net is
	// reported as outflow-positive.
	CollectedFees(ctx context.Context) ([]CollectedFee, error)
	// FeeRevenue returns the GL revenue credited per trade reference
	// (ledger_lines on 4010_TRADING_FEE_REVENUE_* via journal_entries
	// entry_type='FEE').
	FeeRevenue(ctx context.Context) ([]FeeRevenueRow, error)
}

type ExpectedFee struct {
	TradeID       int64
	BaseCurrency  string
	QuoteCurrency string
	BuyerID       int64
	SellerID      int64
	BuyerFee      decimal.Decimal // base currency
	SellerFee     decimal.Decimal // quote currency
}

type CollectedFee struct {
	TradeID   int64
	AccountID int64
	Currency  string
	Outflow   decimal.Decimal // credit − debit (positive = fee charged)
}

type FeeRevenueRow struct {
	TradeID  int64
	Currency string
	Revenue  decimal.Decimal // credit − debit on 4010_*
}

// ===========================================================================
// PgLegs — the production implementation over the shared pool.
// ===========================================================================

// PgLegs implements every checker source seam over pgx.
type PgLegs struct{ pool *pgxpool.Pool }

// NewPgLegs wires the production leg store.
func NewPgLegs(pool *pgxpool.Pool) *PgLegs { return &PgLegs{pool: pool} }

// testScopedExclude is the shared exclusion for integration-harness
// accounts (accounts.test_scoped, migration 278): deliberately
// inconsistent fixture state must never escalate into production
// halts — the halt records stand, the drift just stops being
// re-reported.
const testScopedExclude = `NOT IN (SELECT id FROM accounts WHERE test_scoped)`

// WalletDiff composes the Phase-04 reconcile's scan legs — the same
// query, never a second implementation.
func (l *PgLegs) WalletDiff(ctx context.Context) (*recovery.RecReconcileResult, error) {
	res, err := recovery.RecWalletReconcileDiff(ctx, l.pool)
	if err != nil {
		return nil, err
	}
	// RecWalletReconcileDiff is shared with the Phase-04 restore check —
	// it intentionally scans every wallet, so the test-scope exclusion
	// is applied to the returned mismatches here.
	rows, err := l.pool.Query(ctx,
		`SELECT id FROM accounts WHERE test_scoped`)
	if err != nil {
		return nil, err
	}
	excluded := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		excluded[id] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(excluded) == 0 {
		return res, nil
	}
	kept := res.Mismatches[:0]
	for _, m := range res.Mismatches {
		if _, ok := excluded[m.AccountID]; !ok {
			kept = append(kept, m)
		}
	}
	res.Mismatches = kept
	return res, nil
}

func (l *PgLegs) CurrencySums(ctx context.Context) ([]GLCurrencySum, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT currency, COALESCE(SUM(debit_amount),0), COALESCE(SUM(credit_amount),0)
		  FROM ledger_lines GROUP BY currency ORDER BY currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GLCurrencySum
	for rows.Next() {
		var s GLCurrencySum
		if err := rows.Scan(&s.Currency, &s.Debits, &s.Credits); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (l *PgLegs) JournalImbalances(ctx context.Context) ([]GLJournalImbalance, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT journal_entry_id, currency, SUM(debit_amount), SUM(credit_amount)
		  FROM ledger_lines GROUP BY journal_entry_id, currency
		 HAVING SUM(debit_amount) <> SUM(credit_amount)
		 ORDER BY journal_entry_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GLJournalImbalance
	for rows.Next() {
		var j GLJournalImbalance
		if err := rows.Scan(&j.JournalEntryID, &j.Currency, &j.Debits, &j.Credits); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (l *PgLegs) OpenOrders(ctx context.Context) ([]OpenOrder, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT id, account_id, instrument_id,
		       quantity - filled_qty, status::text, created_at
		  FROM orders
		 WHERE status IN ('ACTIVE','PARTIALLY_FILLED')
		   AND account_id `+testScopedExclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenOrder
	for rows.Next() {
		var o OpenOrder
		if err := rows.Scan(&o.ID, &o.AccountID, &o.InstrumentID,
			&o.RemainingQty, &o.Status, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (l *PgLegs) InstrumentSymbols(ctx context.Context) (map[int64]string, error) {
	rows, err := l.pool.Query(ctx, `SELECT id, symbol FROM instruments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var sym string
		if err := rows.Scan(&id, &sym); err != nil {
			return nil, err
		}
		out[id] = sym
	}
	return out, rows.Err()
}

func (l *PgLegs) PgTrades(ctx context.Context) ([]PgTrade, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT id, instrument_id, quantity, price FROM trades
		 WHERE buyer_account_id `+testScopedExclude+`
		   AND seller_account_id `+testScopedExclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PgTrade
	for rows.Next() {
		var t PgTrade
		if err := rows.Scan(&t.ID, &t.InstrumentID, &t.Quantity, &t.Price); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (l *PgLegs) Positions(ctx context.Context) ([]PgPosition, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT account_id, instrument_id, side::text, quantity, entry_price,
		       mark_price, unrealized_pnl, realized_pnl
		  FROM positions
		 WHERE account_id `+testScopedExclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PgPosition
	for rows.Next() {
		var p PgPosition
		if err := rows.Scan(&p.AccountID, &p.InstrumentID, &p.Side,
			&p.Quantity, &p.EntryPrice, &p.MarkPrice,
			&p.UnrealizedPnL, &p.RealizedPnL); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (l *PgLegs) FillNets(ctx context.Context) ([]FillNet, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT account_id, instrument_id,
		       SUM(CASE WHEN side='BUY' THEN quantity ELSE -quantity END),
		       SUM(realized_pnl)
		  FROM position_fills
		 WHERE account_id `+testScopedExclude+`
		 GROUP BY account_id, instrument_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FillNet
	for rows.Next() {
		var f FillNet
		if err := rows.Scan(&f.AccountID, &f.InstrumentID,
			&f.NetQty, &f.RealizedPnL); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (l *PgLegs) CompletedWithdrawals(ctx context.Context) ([]WithdrawalLeg, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT f.id, f.account_id, f.currency, f.amount,
		       COALESCE(f.bank_method::text,''), rp.id, COALESCE(rp.status::text,'')
		  FROM funding_transactions f
		  LEFT JOIN rail_payments rp ON rp.funding_transaction_id = f.id
		   AND rp.direction = 'OUTBOUND'
		 WHERE f.type = 'WITHDRAWAL' AND f.status = 'COMPLETED'
		   AND f.account_id `+testScopedExclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WithdrawalLeg
	for rows.Next() {
		var w WithdrawalLeg
		var rpID *int64
		if err := rows.Scan(&w.FundingID, &w.AccountID, &w.Currency,
			&w.Amount, &w.BankMethod, &rpID, &w.RailStatus); err != nil {
			return nil, err
		}
		w.RailPaymentID = rpID
		out = append(out, w)
	}
	return out, rows.Err()
}

func (l *PgLegs) QuarantinedDeposits(ctx context.Context) ([]QuarantineLeg, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT f.id, m.id, COALESCE(m.account_id, f.account_id),
		       f.currency, f.amount, f.status::text,
		       m.quarantine_status::text
		  FROM funding_transactions f
		  JOIN suspense_account_mappings m ON m.funding_transaction_id = f.id
		 WHERE f.type = 'DEPOSIT'
		   AND f.status IN ('CONFIRMED','COMPLETED')
		   AND m.quarantine_status IN ('QUARANTINED','INVESTIGATING')
		   AND COALESCE(m.account_id, f.account_id) `+testScopedExclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuarantineLeg
	for rows.Next() {
		var q QuarantineLeg
		if err := rows.Scan(&q.FundingID, &q.MappingID, &q.AccountID,
			&q.Currency, &q.Amount, &q.Status, &q.QuarantineStatus); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (l *PgLegs) DepositForBankTx(ctx context.Context, bankTxID string) (*DepositLeg, error) {
	var d DepositLeg
	err := l.pool.QueryRow(ctx, `
		SELECT f.id, f.account_id, f.currency, f.amount, f.status::text
		  FROM funding_transactions f
		 WHERE f.type = 'DEPOSIT' AND (
		       f.reference = $1
		       OR f.id IN (SELECT funding_transaction_id
		                     FROM suspense_account_mappings
		                    WHERE bank_tx_id = $1))
		   AND f.account_id `+testScopedExclude+`
		 ORDER BY f.id LIMIT 1`, bankTxID).
		Scan(&d.FundingID, &d.AccountID, &d.Currency, &d.Amount, &d.Status)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (l *PgLegs) SettledLegs(ctx context.Context) ([]SettlementLeg, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT si.id, si.currency, si.amount, si.direction::text,
		       nm.id, COALESCE(nm.amount,0), COALESCE(nm.direction::text,''),
		       COALESCE(nm.status::text,'')
		  FROM settlement_instructions si
		  LEFT JOIN nostro_movements nm ON nm.settlement_instruction_id = si.id
		 WHERE si.status = 'SETTLED'
		   AND si.account_id `+testScopedExclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettlementLeg
	for rows.Next() {
		var s SettlementLeg
		var nmID *int64
		if err := rows.Scan(&s.InstructionID, &s.Currency, &s.Amount,
			&s.Direction, &nmID, &s.MovementAmount,
			&s.MovementDir, &s.MovementStatus); err != nil {
			return nil, err
		}
		s.MovementID = nmID
		out = append(out, s)
	}
	return out, rows.Err()
}

func (l *PgLegs) PostedOrphans(ctx context.Context) ([]NostroOrphan, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT nm.id, nm.settlement_instruction_id, si.status::text,
		       nm.currency, nm.amount
		  FROM nostro_movements nm
		  JOIN settlement_instructions si ON si.id = nm.settlement_instruction_id
		 WHERE nm.status = 'POSTED' AND si.status <> 'SETTLED'
		   AND si.account_id `+testScopedExclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NostroOrphan
	for rows.Next() {
		var o NostroOrphan
		if err := rows.Scan(&o.MovementID, &o.InstructionID,
			&o.InstructionStat, &o.Currency, &o.Amount); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (l *PgLegs) OverduePending(ctx context.Context, now time.Time) ([]int64, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT id FROM settlement_instructions
		 WHERE status = 'PENDING' AND settlement_date IS NOT NULL
		   AND settlement_date < $1::date
		   AND account_id `+testScopedExclude, now.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (l *PgLegs) ExpectedFees(ctx context.Context) ([]ExpectedFee, error) {
	// PHYSICAL_DELIVERY fills post their fee legs inside the T+n
	// settlement-confirm journal (spec §5.45.2 — fee recognition travels
	// with confirmation), so a PD trade expects collected fees only once
	// a settlement instruction for it has actually SETTLED. Before that
	// — no instruction yet, leg in flight, or value date unreached —
	// there is nothing collectible, and flagging it would be a false
	// positive against a designed deferral. Rolling-margin rows carry
	// settlement_date NULL → collected at execution.
	rows, err := l.pool.Query(ctx, `
		SELECT t.id, i.base_currency, i.quote_currency,
		       t.buyer_account_id, t.seller_account_id,
		       COALESCE(t.buyer_fee,0), COALESCE(t.seller_fee,0)
		  FROM trades t JOIN instruments i ON i.id = t.instrument_id
		 WHERE t.buyer_account_id `+testScopedExclude+`
		   AND t.seller_account_id `+testScopedExclude+`
		   AND (t.settlement_date IS NULL
		    OR EXISTS (SELECT 1 FROM settlement_instructions si
		                WHERE si.trade_id = t.id AND si.status = 'SETTLED'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExpectedFee
	for rows.Next() {
		var f ExpectedFee
		if err := rows.Scan(&f.TradeID, &f.BaseCurrency, &f.QuoteCurrency,
			&f.BuyerID, &f.SellerID, &f.BuyerFee, &f.SellerFee); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (l *PgLegs) CollectedFees(ctx context.Context) ([]CollectedFee, error) {
	// Only 'fee:{trade}:{acct}:{role}' journal legs count as collected
	// trading fee — commission (commission:*) and maker-rebate
	// (makerrebate:*) journals also post entry_type='FEE' rows with the
	// trade reference, but settle against the 4030/5100 GL lines, not
	// trades.*_fee. Keying on the journal idempotency key keeps the
	// models disjoint.
	//
	// OPS CONTRACT: manual fee corrections on a trade must post under a
	// `fee:{trade}:{account}:{role}`-pattern idempotency key — FEE legs
	// under any other key sharing the trade's reference_id are invisible
	// to this leg and will surface as expected-vs-collected mismatch.
	rows, err := l.pool.Query(ctx, `
		SELECT le.reference_id, le.account_id, le.currency,
		       SUM(CASE WHEN le.direction='CREDIT' THEN le.amount ELSE -le.amount END)
		  FROM ledger_entries le
		  JOIN journal_entries je ON je.id = le.journal_entry_id
		 WHERE le.entry_type='FEE' AND le.reference_id IS NOT NULL
		   AND je.idempotency_key LIKE 'fee:%'
		   AND le.account_id `+testScopedExclude+`
		 GROUP BY le.reference_id, le.account_id, le.currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollectedFee
	for rows.Next() {
		var f CollectedFee
		if err := rows.Scan(&f.TradeID, &f.AccountID, &f.Currency, &f.Outflow); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (l *PgLegs) FeeRevenue(ctx context.Context) ([]FeeRevenueRow, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT je.reference_id, ll.currency,
		       SUM(ll.credit_amount - ll.debit_amount)
		  FROM journal_entries je
		  JOIN ledger_lines ll ON ll.journal_entry_id = je.id
		 WHERE je.entry_type = 'FEE' AND je.reference_id IS NOT NULL
		   AND ll.account_code LIKE '4010\_TRADING\_FEE\_REVENUE\_%'
		   AND NOT EXISTS (SELECT 1 FROM trades t
		                    WHERE t.id = je.reference_id
		                      AND (t.buyer_account_id IN (SELECT id FROM accounts WHERE test_scoped)
		                       OR  t.seller_account_id IN (SELECT id FROM accounts WHERE test_scoped)))
		 GROUP BY je.reference_id, ll.currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeeRevenueRow
	for rows.Next() {
		var f FeeRevenueRow
		if err := rows.Scan(&f.TradeID, &f.Currency, &f.Revenue); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// DefaultCheckers — the nine-category wiring for production.
// ---------------------------------------------------------------------------

// DefaultCheckers builds the Task 13.3.2 checker set over the production
// seams. stmt may be nil — the statement leg then reports INCONCLUSIVE
// (ruling R3).
func DefaultCheckers(l *PgLegs, wal walSource, stmt StatementSource) []Checker {
	if wal == nil {
		wal = replayWalSource{}
	}
	return []Checker{
		BalancesChecker{Src: l},
		PositionsChecker{Src: l},
		OrdersChecker{Src: l, Wal: wal},
		TradesChecker{Src: l, Wal: wal},
		FundingChecker{Src: l, Statements: stmt},
		SettlementChecker{Src: l},
		FeesChecker{Src: l},
		PnLChecker{Src: l},
		GeneralLedgerChecker{Src: l},
	}
}
