// Package balance — Task 3.3.9: multi-currency account accounting and
// realized-P&L settlement (spec §13.1 items 3–4, §16.4, §5.3; §24 #180).
//
// Realized position P&L is denominated in the instrument's QUOTE currency
// (settlement.PositionService tracks it; spec §13.1/§16.4). Settlement
// books it per the account's pnl_settlement_mode (migration 110):
//
//	QUOTE_CURRENCY — wallet effect on the (account_id, quote_ccy)
//	                 balances row; GL contra is 4040_REALIZED_TRADING_PNL.
//	SWEEP_TO_BASE  — amount converts at the oracle mark mid-rate
//	                 (internal/position Converter) and the wallet effect
//	                 lands on the (account_id, base_ccy) row; the GL quote
//	                 leg routes through 1200_MULTI_CURRENCY_CLEARING so
//	                 each currency still zero-sums. A currency_conversions
//	                 audit row records rate + hop path.
//
// ALL wallet mutations ride inside ledger.Journal.Effects — the
// JournalPoster (settlement.LedgerService, Task 3.3.6) applies balances +
// ledger_entries + journal_sums + journal_entries + ledger_lines in ONE
// SERIALIZABLE tx (zero GL bypass, spec §5.3 invariant 4). This package
// never writes balances directly.
//
// Fail-closed (§2.7): missing account, oracle failure, unbalanced journal
// or dedup conflict abort; nothing partial persists. The journal's
// IdempotencyKey (realized-pnl:{account}:{trade}) makes a replayed Post
// resolve to the original journal even if the audit-receipt insert needs
// a retry.
package balance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/internal/position"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Error codes emitted here; canonical registration lands in Phase-05
// Task 5.3.21 (spec §23).
const (
	CodeAccountNotFound   = "ACCOUNT_NOT_FOUND"         // HTTP 404, L2
	CodeInvalidSettlement = "INVALID_SETTLEMENT"        // HTTP 400, L2
	CodeDuplicateSettle   = "DUPLICATE_SETTLEMENT"      // HTTP 409, L2
	CodeJournalPosterNil  = "LEDGER_POSTER_UNAVAILABLE" // HTTP 503, L1
)

// P&L settlement modes (accounts.pnl_settlement_mode, migration 110).
const (
	SettleQuoteCurrency = "QUOTE_CURRENCY"
	SettleSweepToBase   = "SWEEP_TO_BASE"
)

// Reference types for currency_conversions.reference_type.
const (
	RefRealizedPnLSettle = "REALIZED_PNL_SETTLE" // booking decision receipt (rate=1 identity when no sweep)
	RefRealizedPnLSweep  = "REALIZED_PNL_SWEEP"  // the Q→base conversion leg
)

// glRealizedTradingPnL is the house-side realized trading gain/loss
// account seeded by migration 110 (REVENUE class — debited on client
// profit, credited on client loss, matching the SwapRolloverRevenue
// convention). Canonical builder belongs in internal/ledger/chart.go
// (GL owner); duplicated here until they add it.
func glRealizedTradingPnL(ccy string) string { return "4040_REALIZED_TRADING_PNL_" + ccy }

// ---------------------------------------------------------------------------
// JournalPoster seam — satisfied by *settlement.LedgerService
// (internal/settlement/ledger_service.go, Task 3.3.6).
// ---------------------------------------------------------------------------

// JournalPoster is the seam to the double-entry GL posting service. Post
// applies j.Lines to the GL AND j.Effects to the wallet rows in one
// SERIALIZABLE transaction; IdempotencyKey dedups replays.
type JournalPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// ---------------------------------------------------------------------------
// Store seam — account profile + conversion audit/dedup only (wallet rows
// are owned by the poster's Effects).
// ---------------------------------------------------------------------------

// Store is the persistence seam; PgxStore is the production
// implementation.
//
// Ordering contract (two-phase, crash-safe): the journal is posted FIRST —
// Post is idempotent on IdempotencyKey — and RecordSettlement is the
// settlement "done" marker. A crash between them leaves a committed
// journal but no receipt; the retry replays Post harmlessly (Replayed,
// no re-apply) and then records the receipt.
type Store interface {
	// AccountProfile returns (base_currency, pnl_settlement_mode) for the
	// account. Missing account → error.
	AccountProfile(ctx context.Context, accountID int64) (baseCurrency, settleMode string, err error)
	// ConversionExists reports whether the settlement receipt exists —
	// fast-path dedup so a replay returns Duplicate without posting.
	ConversionExists(ctx context.Context, referenceType string, referenceID int64, fromCurrency string) (bool, error)
	// RecordSettlement atomically inserts the audit rows: recs[0] is the
	// dedup receipt (unique (reference_type, reference_id, from_currency));
	// subsequent rows are conversion legs. Returns duplicate=true when the
	// receipt already exists — nothing is inserted then.
	RecordSettlement(ctx context.Context, journalEntryID int64, recs ...ConversionRecord) (duplicate bool, err error)
}

// ConversionRecord is the currency_conversions row (migration 110).
type ConversionRecord struct {
	AccountID     int64
	FromCurrency  string
	ToCurrency    string
	FromAmount    decimal.Decimal // signed
	ToAmount      decimal.Decimal // signed
	Rate          decimal.Decimal
	RatePath      string
	ReferenceType string
	ReferenceID   int64
}

// SettlementRequest asks the service to settle realized P&L.
type SettlementRequest struct {
	AccountID     int64
	TradeID       int64           // dedup + journal reference
	QuoteCurrency string          // instrument quote currency (P&L currency)
	RealizedPnL   decimal.Decimal // signed, in QuoteCurrency, <= 8dp
	Description   string
	PostedBy      string // service identity for the journal (default "settlement-service")
}

// SettlementResult reports what was booked.
type SettlementResult struct {
	AccountID      int64
	BookedCurrency string
	BookedAmount   decimal.Decimal // signed wallet delta on BookedCurrency
	BaseCurrency   string
	SettleMode     string
	Converted      bool
	Conversion     *position.Conversion // non-nil when a Q→base sweep ran
	JournalEntryID int64
	Duplicate      bool
	NoOp           bool // zero P&L — nothing booked
}

// SettlementService settles realized P&L into the multi-currency ledger.
type SettlementService struct {
	store  Store
	conv   *position.Converter
	poster JournalPoster
}

// NewSettlementService wires the service. poster may be nil only for
// structural tests — a nil poster fails closed at settle time (a
// settlement without GL posting must never commit).
func NewSettlementService(store Store, conv *position.Converter, poster JournalPoster) *SettlementService {
	return &SettlementService{store: store, conv: conv, poster: poster}
}

// SettleRealizedPnL books realized quote-currency P&L into the account's
// cash ledger (quote line or swept to base) via a balanced GL journal
// carrying wallet Effects, plus conversion audit rows. Idempotent per
// (REALIZED_PNL_SETTLE, trade_id): a replay returns Duplicate and never
// double-books.
func (s *SettlementService) SettleRealizedPnL(ctx context.Context, req SettlementRequest) (*SettlementResult, error) {
	if req.AccountID <= 0 || req.TradeID <= 0 {
		return nil, excerrors.New(CodeInvalidSettlement, "account_id and trade_id must be > 0")
	}
	q := strings.ToUpper(strings.TrimSpace(req.QuoteCurrency))
	if len(q) != 3 {
		return nil, excerrors.New(CodeInvalidSettlement, "invalid quote currency "+req.QuoteCurrency)
	}
	// Zero precision loss: reject sub-quantum P&L rather than silently
	// rounding (DECIMAL(28,8), spec §5.3).
	if !req.RealizedPnL.Round(8).Equal(req.RealizedPnL) {
		return nil, excerrors.New(CodeInvalidSettlement,
			"realized P&L "+req.RealizedPnL.String()+" exceeds DECIMAL(28,8) quantum")
	}
	if s.poster == nil {
		return nil, excerrors.New(CodeJournalPosterNil,
			"no JournalPoster configured — refusing to settle without GL posting")
	}
	res := &SettlementResult{AccountID: req.AccountID}
	if req.RealizedPnL.IsZero() {
		res.NoOp = true
		return res, nil // explicit no-op — zero P&L needs no ledger movement
	}
	postedBy := req.PostedBy
	if postedBy == "" {
		postedBy = "settlement-service"
	}

	base, mode, err := s.store.AccountProfile(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}
	base = strings.ToUpper(base)
	res.BaseCurrency, res.SettleMode = base, mode

	// Fast-path dedup: receipt already recorded → done.
	dup, err := s.store.ConversionExists(ctx, RefRealizedPnLSettle, req.TradeID, q)
	if err != nil {
		return nil, fmt.Errorf("settlement dedup check: %w", err)
	}
	if dup {
		res.Duplicate = true
		return res, nil
	}

	var j ledger.Journal
	var sweepRec *ConversionRecord
	if mode == SettleSweepToBase && q != base {
		// Sweep: P&L converts to base at the oracle mid-rate; only the
		// base-currency wallet line moves (§24 #180 — "settled
		// unambiguously in base ledger"). GL carries the quote leg
		// through 1200_MULTI_CURRENCY_CLEARING so each currency
		// zero-sums.
		cv, err := s.conv.Convert(ctx, req.RealizedPnL, q, base)
		if err != nil {
			return nil, fmt.Errorf("realized P&L conversion %s→%s: %w", q, base, err)
		}
		settled := cv.ToAmount.Round(8)
		res.Converted, res.Conversion = true, &cv
		j = sweepJournal(req, postedBy, q, base, req.RealizedPnL, settled)
		res.BookedCurrency, res.BookedAmount = base, settled
		sweepRec = &ConversionRecord{
			AccountID:     req.AccountID,
			FromCurrency:  q,
			ToCurrency:    base,
			FromAmount:    req.RealizedPnL,
			ToAmount:      settled,
			Rate:          cv.Rate,
			RatePath:      cv.Path,
			ReferenceType: RefRealizedPnLSweep,
			ReferenceID:   req.TradeID,
		}
	} else {
		j = quoteJournal(req, postedBy, q)
		res.BookedCurrency, res.BookedAmount = q, req.RealizedPnL
	}
	j.IdempotencyKey = fmt.Sprintf("realized-pnl:%d:%d", req.AccountID, req.TradeID)

	if err := j.Validate(); err != nil {
		return nil, err
	}
	// Phase 1: post the journal (poster is idempotent on IdempotencyKey).
	pr, err := s.poster.Post(ctx, j)
	if err != nil {
		return nil, fmt.Errorf("journal post: %w", err)
	}
	res.JournalEntryID = pr.JournalID

	// Phase 2: the settlement receipt is the "done" marker; a crash between
	// phases replays Post harmlessly and lands here on retry.
	recs := []ConversionRecord{{
		AccountID:     req.AccountID,
		FromCurrency:  q,
		ToCurrency:    q,
		FromAmount:    req.RealizedPnL,
		ToAmount:      req.RealizedPnL,
		Rate:          decimal.One,
		RatePath:      "identity",
		ReferenceType: RefRealizedPnLSettle,
		ReferenceID:   req.TradeID,
	}}
	if sweepRec != nil {
		recs = append(recs, *sweepRec)
	}
	recordedDup, err := s.store.RecordSettlement(ctx, pr.JournalID, recs...)
	if err != nil {
		return nil, fmt.Errorf("settlement receipt: %w", err)
	}
	if recordedDup {
		// Concurrent settle won the race; our Post replayed harmlessly.
		res.Duplicate = true
	}
	return res, nil
}

// quoteJournal builds the single-currency settlement journal:
//
//	client profit X>0:  D 4040_REALIZED_TRADING_PNL_Q / C 2010_CUSTOMER_LIABILITY_Q
//	client loss   X<0:  D 2010_CUSTOMER_LIABILITY_Q   / C 4040_REALIZED_TRADING_PNL_Q
//
// The wallet Effect applies the signed delta to the client's quote line;
// AllowNegative permits a loss on a thin balance (margin/NBP consequence
// is owned by Phase-19, not the ledger gate).
func quoteJournal(req SettlementRequest, postedBy, q string) ledger.Journal {
	x := req.RealizedPnL
	ax := x.Abs()
	narr := fmt.Sprintf("realized P&L %s %s trade %d", x, q, req.TradeID)
	j := ledger.Journal{
		EntryType:   ledger.EntrySettlement,
		ReferenceID: req.TradeID,
		Description: narrative(req, narr),
		PostedBy:    postedBy,
		Effects: []ledger.AccountEffect{{
			AccountID: req.AccountID, Currency: q,
			AvailableDelta: x, AllowNegative: true,
		}},
	}
	if x.IsPositive() {
		j.Lines = []ledger.Line{
			ledger.DebitLine(glRealizedTradingPnL(q), q, ax, narr+" leg=house-pnl"),
			ledger.CreditLine(ledger.CustomerLiability(q), q, ax, narr+" leg=client"),
		}
	} else {
		j.Lines = []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(q), q, ax, narr+" leg=client"),
			ledger.CreditLine(glRealizedTradingPnL(q), q, ax, narr+" leg=house-pnl"),
		}
	}
	return j
}

// sweepJournal builds the two-currency sweep journal (per-currency
// zero-sum, §5.21). Client profit (X>0, converted Y>0 in base):
//
//	quote Q: D 4040_REALIZED_TRADING_PNL_Q X  / C 1200_MULTI_CURRENCY_CLEARING_Q X
//	base  B: D 1200_MULTI_CURRENCY_CLEARING_B Y / C 2010_CUSTOMER_LIABILITY_B Y
//
// For a client loss every line's debit/credit flips (house receives the
// P&L; the client's base line is debited). Only the base-currency wallet
// row carries an Effect — the quote leg settles through clearing.
func sweepJournal(req SettlementRequest, postedBy, q, base string, x, y decimal.Decimal) ledger.Journal {
	ax, ay := x.Abs(), y.Abs()
	narr := fmt.Sprintf("realized P&L %s %s swept→%s trade %d", x, q, base, req.TradeID)
	j := ledger.Journal{
		EntryType:   ledger.EntrySettlement,
		ReferenceID: req.TradeID,
		Description: narrative(req, narr),
		PostedBy:    postedBy,
		Effects: []ledger.AccountEffect{{
			AccountID: req.AccountID, Currency: base,
			AvailableDelta: y, AllowNegative: true,
		}},
	}
	if x.IsPositive() {
		j.Lines = []ledger.Line{
			ledger.DebitLine(glRealizedTradingPnL(q), q, ax, narr+" leg=house-pnl"),
			ledger.CreditLine(ledger.MultiCcyClearing(q), q, ax, narr+" leg=fx-out"),
			ledger.DebitLine(ledger.MultiCcyClearing(base), base, ay, narr+" leg=fx-in"),
			ledger.CreditLine(ledger.CustomerLiability(base), base, ay, narr+" leg=client"),
		}
	} else {
		j.Lines = []ledger.Line{
			ledger.DebitLine(ledger.MultiCcyClearing(q), q, ax, narr+" leg=fx-out"),
			ledger.CreditLine(glRealizedTradingPnL(q), q, ax, narr+" leg=house-pnl"),
			ledger.DebitLine(ledger.CustomerLiability(base), base, ay, narr+" leg=client"),
			ledger.CreditLine(ledger.MultiCcyClearing(base), base, ay, narr+" leg=fx-in"),
		}
	}
	return j
}

func narrative(req SettlementRequest, base string) string {
	if req.Description == "" {
		return base
	}
	return base + " (" + req.Description + ")"
}

// ---------------------------------------------------------------------------
// PgxStore — production Store over pgxpool (PostgreSQL 16).
// ---------------------------------------------------------------------------

// PgxStore implements Store.
type PgxStore struct {
	Pool *pgxpool.Pool
}

// NewPgxStore wraps a pool.
func NewPgxStore(pool *pgxpool.Pool) *PgxStore { return &PgxStore{Pool: pool} }

func (s *PgxStore) AccountProfile(ctx context.Context, accountID int64) (string, string, error) {
	var base, mode string
	err := s.Pool.QueryRow(ctx,
		`SELECT base_currency, pnl_settlement_mode FROM accounts WHERE id = $1`,
		accountID).Scan(&base, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", excerrors.New(CodeAccountNotFound, fmt.Sprintf("account %d not found", accountID))
	}
	return base, mode, err
}

func (s *PgxStore) ConversionExists(ctx context.Context, referenceType string, referenceID int64, fromCurrency string) (bool, error) {
	var n int
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM currency_conversions
		  WHERE reference_type = $1 AND reference_id = $2 AND from_currency = $3`,
		referenceType, referenceID, fromCurrency).Scan(&n)
	return n > 0, err
}

// RecordSettlement inserts the audit rows atomically in one tx. recs[0]
// (the REALIZED_PNL_SETTLE receipt) uses ON CONFLICT DO NOTHING on the
// dedup index: a conflict → rollback → duplicate, nothing written.
func (s *PgxStore) RecordSettlement(ctx context.Context, journalEntryID int64, recs ...ConversionRecord) (bool, error) {
	if len(recs) == 0 {
		return false, excerrors.New(CodeInvalidSettlement, "RecordSettlement requires >= 1 record")
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("settlement tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var jid any
	if journalEntryID > 0 {
		jid = journalEntryID
	}
	var id int64
	err = tx.QueryRow(ctx,
		`INSERT INTO currency_conversions
		   (account_id, from_currency, to_currency, from_amount, to_amount,
		    rate, rate_path, reference_type, reference_id, journal_entry_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (reference_type, reference_id, from_currency) DO NOTHING
		 RETURNING id`,
		recs[0].AccountID, recs[0].FromCurrency, recs[0].ToCurrency,
		recs[0].FromAmount, recs[0].ToAmount, recs[0].Rate, recs[0].RatePath,
		recs[0].ReferenceType, recs[0].ReferenceID, jid).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // receipt exists → duplicate
	}
	if err != nil {
		return false, err
	}
	for _, rec := range recs[1:] {
		if _, err := tx.Exec(ctx,
			`INSERT INTO currency_conversions
			   (account_id, from_currency, to_currency, from_amount, to_amount,
			    rate, rate_path, reference_type, reference_id, journal_entry_id)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			rec.AccountID, rec.FromCurrency, rec.ToCurrency, rec.FromAmount,
			rec.ToAmount, rec.Rate, rec.RatePath, rec.ReferenceType,
			rec.ReferenceID, jid); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("settlement tx commit: %w", err)
	}
	return false, nil
}
