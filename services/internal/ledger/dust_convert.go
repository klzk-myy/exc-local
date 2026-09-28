// dust_convert.go — Dust-Balance Conversion to Base Currency
// (Phase-03 Task 3.3.20; spec §5.21b, §24 #364; migration 113).
//
// Stranded sub-min-notional balances get a one-way conversion path into the
// account's base currency. One sweep is:
//
//  1. Eligibility — balance is positive and below the instrument's
//     min_notional (in the pair's quote currency), nothing is locked by
//     resting orders, the account is ACTIVE. Swap-free accounts are
//     included: conversion is not financing (§5.21b.2).
//  2. Pricing — mark mid-rate for dust→base minus the disclosed
//     CONVERSION_SPREAD fee-schedule line (§5.21a.3). No disclosed spread
//     fails closed (CONVERSION_SPREAD_UNDISCLOSED): client money is never
//     converted at an undisclosed rate.
//  3. GL — balanced per-currency legs. Dust ccy: client liability debit
//     into 1200_MULTI_CURRENCY_CLEARING. Base ccy: clearing debit, client
//     liability credit at mid−spread, remainder to
//     4400_CONVERSION_SPREAD_REVENUE. Zero-sum per currency holds by
//     construction and is re-verified by Journal.Validate.
//  4. Idempotency & rate limit — dust_sweeps carries
//     UNIQUE(account_id, currency, sweep_date): one sweep per currency per
//     UTC day per account; a replayed sweep returns the recorded result.
//
// The package stays I/O-free: persistence lives behind DustStore, pricing
// behind MidPricer, posting behind JournalPoster (satisfied by
// settlement.DoubleEntryLedgerService).
package ledger

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// Error codes for the dust domain. RATE_LIMIT_EXCEEDED / INVALID_REQUEST /
// FORBIDDEN are canonical spec §23 codes; the sweep-specific codes are
// scaffold pending Phase-05 Task 5.3.21 registration.
const (
	CodeInvalidRequest              = "INVALID_REQUEST"               // §23
	CodeForbidden                   = "FORBIDDEN"                     // §23
	CodeRateLimitExceeded           = "RATE_LIMIT_EXCEEDED"           // §23
	CodeConversionSpreadUndisclosed = "CONVERSION_SPREAD_UNDISCLOSED" // scaffold
	CodeDustPriceUnavailable        = "PRICE_ORACLE_UNAVAILABLE"      // §23
)

// MidPricer supplies mark mid-rates: units of quote per 1 unit of base.
// position.Converter.Rate adapts structurally (direct, inverse, USD-cross);
// oracle failures must surface, never fall back silently (§2.7).
type MidPricer interface {
	MidRate(ctx context.Context, base, quote string) (decimal.Decimal, error)
}

// MidRateResolver is the structural shape of position.Converter.Rate —
// declared as an interface so this file needs no import edge.
type MidRateResolver interface {
	Rate(ctx context.Context, base, quote string) (rate decimal.Decimal, path string, err error)
}

// ConverterMidPricer adapts a MidRateResolver (e.g. *position.Converter)
// to MidPricer, inheriting its direct/inverse/USD-cross resolution.
type ConverterMidPricer struct{ Resolver MidRateResolver }

// MidRate implements MidPricer.
func (p ConverterMidPricer) MidRate(ctx context.Context, base, quote string) (decimal.Decimal, error) {
	if p.Resolver == nil {
		return decimal.Zero, excerrors.New(CodeDustPriceUnavailable, "nil mid-rate resolver")
	}
	r, _, err := p.Resolver.Rate(ctx, base, quote)
	return r, err
}

// JournalPoster is the minimal posting seam satisfied by
// settlement.DoubleEntryLedgerService — declared here so the sweeper never
// imports its consumer (the ledger package owns the contract).
type JournalPoster interface {
	Post(ctx context.Context, j Journal) (PostResult, error)
}

// DustInstrument is the pair governing a dust→base conversion: its
// min_notional (denominated in QuoteCurrency) is the dust threshold.
type DustInstrument struct {
	InstrumentID  int64
	Symbol        string
	BaseCurrency  string
	QuoteCurrency string
	MinNotional   decimal.Decimal // in QuoteCurrency
}

// DustSweepRecord is one dust_sweeps row (migration 113).
type DustSweepRecord struct {
	ID             int64
	AccountID      int64
	Currency       string // dust currency debited
	BaseCurrency   string // base currency credited
	SweepDate      time.Time
	InstrumentID   int64
	DustAmount     decimal.Decimal
	CreditedAmount decimal.Decimal
	MidRate        decimal.Decimal
	SpreadBps      decimal.Decimal
	JournalEntryID int64 // 0 = claimed but not yet posted
}

// DustStore is the persistence seam; PgDustStore implements it over pgx.
type DustStore interface {
	// AccountGoodStanding reports whether the account may transact
	// (status = 'ACTIVE').
	AccountGoodStanding(ctx context.Context, accountID int64) (bool, error)
	// AccountBaseCurrency returns accounts.base_currency (migration 110).
	AccountBaseCurrency(ctx context.Context, accountID int64) (string, error)
	// DustBalance returns the account's (available, locked) for ccy;
	// found=false when no balances row exists.
	DustBalance(ctx context.Context, accountID int64, ccy string) (available, locked decimal.Decimal, found bool, err error)
	// DustInstrument resolves the ACTIVE instrument pairing dust↔base and
	// its min_notional; found=false when no such instrument exists.
	DustInstrument(ctx context.Context, dustCcy, baseCcy string) (DustInstrument, bool, error)
	// ConversionSpreadBps returns the ACTIVE disclosed conversion spread
	// for the instrument (per-instrument line wins over the global
	// instrument_id NULL default); found=false when none is disclosed.
	ConversionSpreadBps(ctx context.Context, instrumentID int64) (decimal.Decimal, bool, error)
	// ClaimSweep inserts the sweep row for (account, currency, day).
	// claimed=false means a row already exists for the day.
	ClaimSweep(ctx context.Context, rec DustSweepRecord) (claimed bool, sweepID int64, err error)
	// SweepFor reads back the recorded sweep for (account, currency, day).
	SweepFor(ctx context.Context, accountID int64, ccy string, day time.Time) (DustSweepRecord, bool, error)
	// CompleteSweep back-fills journal_entry_id once the journal posted.
	CompleteSweep(ctx context.Context, sweepID, journalEntryID int64) error
	// ReleaseSweep deletes an unposted claim (journal_entry_id IS NULL) so
	// a failed sweep does not consume the day's slot. Never deletes a
	// posted sweep.
	ReleaseSweep(ctx context.Context, sweepID int64) error
}

// DustSweepResult reports one conversion (or its idempotent replay).
type DustSweepResult struct {
	SweepID        int64
	DustCurrency   string
	DustAmount     decimal.Decimal
	BaseCurrency   string
	CreditedAmount decimal.Decimal
	MidRate        decimal.Decimal
	SpreadBps      decimal.Decimal
	JournalID      int64
	Replayed       bool // true = recorded earlier today; nothing reposted
	Pending        bool // true = claimed but post crashed; caller may retry
}

// DustConverter sweeps eligible dust balances into the account base
// currency. All collaborators are required seams.
type DustConverter struct {
	store  DustStore
	pricer MidPricer
	poster JournalPoster
	clock  func() time.Time
}

// NewDustConverter wires the sweeper.
func NewDustConverter(store DustStore, pricer MidPricer, poster JournalPoster, clock func() time.Time) (*DustConverter, error) {
	if store == nil || pricer == nil || poster == nil {
		return nil, fmt.Errorf("dust converter: nil store, pricer or poster")
	}
	if clock == nil {
		clock = time.Now
	}
	return &DustConverter{store: store, pricer: pricer, poster: poster, clock: clock}, nil
}

// Sweep converts the account's dust balance of ccy into its base currency.
// Idempotent per (account, currency, UTC day): a same-day repeat returns
// the recorded result (Replayed=true). A claim left unposted by a crash is
// reported as Pending so the caller's retry re-runs the sweep.
func (d *DustConverter) Sweep(ctx context.Context, accountID int64, ccy string) (*DustSweepResult, error) {
	ccy = strings.ToUpper(strings.TrimSpace(ccy))
	if accountID <= 0 || len(ccy) != 3 {
		return nil, excerrors.New(CodeInvalidRequest,
			"dust sweep requires a positive account id and ISO currency")
	}
	// ── Eligibility (spec §5.21b.1) ───────────────────────────────────────
	ok, err := d.store.AccountGoodStanding(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("dust: account standing: %w", err)
	}
	if !ok {
		return nil, excerrors.New(CodeForbidden,
			fmt.Sprintf("account %d is not in good standing", accountID))
	}
	base, err := d.store.AccountBaseCurrency(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("dust: base currency: %w", err)
	}
	base = strings.ToUpper(strings.TrimSpace(base))
	if ccy == base {
		return nil, excerrors.New(CodeInvalidRequest,
			"dust sweep of the base currency itself is meaningless")
	}
	avail, locked, found, err := d.store.DustBalance(ctx, accountID, ccy)
	if err != nil {
		return nil, fmt.Errorf("dust: balance read: %w", err)
	}
	if !found || !avail.IsPositive() {
		return nil, excerrors.New(CodeInvalidRequest,
			fmt.Sprintf("account %d has no positive %s balance to convert", accountID, ccy))
	}
	if !locked.IsZero() {
		return nil, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"account %d %s balance has %s locked by resting orders — ineligible",
			accountID, ccy, locked))
	}
	inst, found, err := d.store.DustInstrument(ctx, ccy, base)
	if err != nil {
		return nil, fmt.Errorf("dust: instrument resolve: %w", err)
	}
	if !found {
		return nil, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"no ACTIVE instrument pairs %s↔%s — no dust path", ccy, base))
	}
	mid, err := d.pricer.MidRate(ctx, ccy, base)
	if err != nil {
		return nil, excerrors.Wrap(CodeDustPriceUnavailable,
			fmt.Sprintf("mark mid %s→%s", ccy, base), err)
	}
	if !mid.IsPositive() {
		return nil, excerrors.New(CodeDustPriceUnavailable,
			fmt.Sprintf("non-positive mark mid %s→%s: %s", ccy, base, mid))
	}

	// min_notional is denominated in the instrument's QUOTE currency —
	// compare the dust holding in that unit.
	var dustInQuote decimal.Decimal
	switch inst.QuoteCurrency {
	case base:
		dustInQuote = avail.Mul(mid)
	case ccy:
		dustInQuote = avail
	default:
		return nil, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"instrument %s quote %s is neither dust nor base ccy", inst.Symbol, inst.QuoteCurrency))
	}
	if inst.MinNotional.IsPositive() && dustInQuote.GreaterThanOrEqual(inst.MinNotional) {
		return nil, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"%s balance %s (≈%s %s) is at/above min_notional %s — not dust",
			ccy, avail, dustInQuote, inst.QuoteCurrency, inst.MinNotional))
	}

	spreadBps, disclosed, err := d.store.ConversionSpreadBps(ctx, inst.InstrumentID)
	if err != nil {
		return nil, fmt.Errorf("dust: conversion spread: %w", err)
	}
	if !disclosed {
		return nil, excerrors.New(CodeConversionSpreadUndisclosed, fmt.Sprintf(
			"no ACTIVE CONVERSION_SPREAD disclosure for instrument %d (%s) — refusing to convert at an undisclosed rate",
			inst.InstrumentID, inst.Symbol))
	}

	// ── Pricing: mark mid minus disclosed spread ──────────────────────────
	grossBase := avail.Mul(mid)
	credited := grossBase.Mul(decimal.NewFromInt(10_000).Sub(spreadBps)).
		Div(decimal.NewFromInt(10_000)).Round(8)
	if !credited.IsPositive() || credited.GreaterThan(grossBase) {
		return nil, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"conversion of %s %s at mid %s spread %s bps yields non-positive credit", avail, ccy, mid, spreadBps))
	}
	spreadRetained := grossBase.Sub(credited).Round(8)

	day := dayStartUTC(d.clock())
	claim := DustSweepRecord{
		AccountID: accountID, Currency: ccy, BaseCurrency: base,
		SweepDate: day, InstrumentID: inst.InstrumentID,
		DustAmount: avail, CreditedAmount: credited,
		MidRate: mid, SpreadBps: spreadBps,
	}
	claimed, sweepID, err := d.store.ClaimSweep(ctx, claim)
	if err != nil {
		return nil, fmt.Errorf("dust: claim sweep: %w", err)
	}
	if !claimed {
		// Same (account, ccy, day) already swept — replay or report pending.
		existing, ok, err := d.store.SweepFor(ctx, accountID, ccy, day)
		if err != nil {
			return nil, fmt.Errorf("dust: read sweep: %w", err)
		}
		if !ok {
			return nil, excerrors.New(CodeRateLimitExceeded,
				"dust sweep already claimed today for this currency")
		}
		res := &DustSweepResult{
			SweepID: existing.ID, DustCurrency: existing.Currency,
			DustAmount: existing.DustAmount, BaseCurrency: existing.BaseCurrency,
			CreditedAmount: existing.CreditedAmount, MidRate: existing.MidRate,
			SpreadBps: existing.SpreadBps, JournalID: existing.JournalEntryID,
		}
		if existing.JournalEntryID == 0 {
			res.Pending = true
		} else {
			res.Replayed = true
		}
		return res, nil
	}

	// ── Balanced GL journal (spec §5.21b.2) ───────────────────────────────
	narrative := fmt.Sprintf("DUST_CONVERT acct=%d %s→%s mid=%s spread_bps=%s",
		accountID, ccy, base, mid.String(), spreadBps.String())
	// The clearing debit is the exact sum of its credit legs so the
	// per-currency zero-sum holds regardless of mid-rate rounding
	// (grossBase can carry >8dp from inverse/cross rates).
	clearingDebit := credited.Add(spreadRetained)
	lines := []Line{
		// dust leg: client liability down into multi-currency clearing
		DebitLine(CustomerLiability(ccy), ccy, avail, narrative+" leg=dust"),
		CreditLine(MultiCcyClearing(ccy), ccy, avail, narrative+" leg=dust"),
		// base leg: clearing pays the client at mid−spread; remainder is
		// conversion revenue
		DebitLine(MultiCcyClearing(base), base, clearingDebit, narrative+" leg=base"),
		CreditLine(CustomerLiability(base), base, credited, narrative+" leg=base"),
	}
	if spreadRetained.IsPositive() {
		lines = append(lines,
			CreditLine(ConversionSpreadRevenue(base), base, spreadRetained, narrative+" leg=spread"))
	}
	j := Journal{
		EntryType:      EntryTransfer,
		ReferenceID:    sweepID,
		Description:    narrative,
		PostedBy:       "dust-converter",
		IdempotencyKey: fmt.Sprintf("dust:%d:%s:%d", accountID, ccy, sweepID),
		Lines:          lines,
		Effects: []AccountEffect{
			{AccountID: accountID, Currency: ccy, AvailableDelta: avail.Neg()},
			{AccountID: accountID, Currency: base, AvailableDelta: credited},
		},
	}
	res, err := d.poster.Post(ctx, j)
	if err != nil {
		// Post failed — release the claim so the day's slot is not consumed
		// by a sweep that never settled.
		_ = d.store.ReleaseSweep(context.WithoutCancel(ctx), sweepID)
		return nil, err
	}
	if err := d.store.CompleteSweep(ctx, sweepID, res.JournalID); err != nil {
		// Funds are final (committed) — the completion marker is an audit
		// back-fill; surface the failure but keep the committed journal id.
		return &DustSweepResult{
				SweepID: sweepID, DustCurrency: ccy, DustAmount: avail,
				BaseCurrency: base, CreditedAmount: credited, MidRate: mid,
				SpreadBps: spreadBps, JournalID: res.JournalID,
			}, excerrors.Wrap(CodeBalanceDispatchFailed,
				"dust sweep posted but journal_entry_id back-fill failed", err)
	}
	return &DustSweepResult{
		SweepID: sweepID, DustCurrency: ccy, DustAmount: avail,
		BaseCurrency: base, CreditedAmount: credited, MidRate: mid,
		SpreadBps: spreadBps, JournalID: res.JournalID, Replayed: res.Replayed,
	}, nil
}

// dayStartUTC normalises t to the UTC calendar day.
func dayStartUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// SQL for the DustStore wiring layer (migration 113). The ledger package is
// I/O-free by contract — these constants are the exact statements the
// pgx executor (settlement/balance wiring) binds; same pattern as
// SwapAccrualInsertSQL in swap_accrual.go.
// ---------------------------------------------------------------------------

const (
	// DustStandingSQL — accounts.status = 'ACTIVE' gate.
	DustStandingSQL = `SELECT status = 'ACTIVE' FROM accounts WHERE id = $1`
	// DustBaseCurrencySQL — accounts.base_currency (migration 110).
	DustBaseCurrencySQL = `SELECT base_currency FROM accounts WHERE id = $1`
	// DustBalanceSQL — (available, locked) for the dust currency.
	DustBalanceSQL = `SELECT available::text, locked::text FROM balances WHERE account_id = $1 AND currency = $2`
	// DustInstrumentSQL — ACTIVE instrument pairing dust↔base in either
	// orientation; min_notional is denominated in quote_currency.
	DustInstrumentSQL = `
		SELECT id, symbol, base_currency, quote_currency, min_notional::text
		FROM instruments
		WHERE status = 'ACTIVE'
		  AND ((base_currency = $2 AND quote_currency = $3)
		    OR (base_currency = $3 AND quote_currency = $2))
		ORDER BY id LIMIT 1`
	// DustSpreadSQL — ACTIVE disclosed CONVERSION_SPREAD: per-instrument
	// line wins over the global (instrument_id IS NULL) default.
	DustSpreadSQL = `
		SELECT amount::text FROM non_trading_fee_schedule
		WHERE kind = 'CONVERSION_SPREAD' AND status = 'ACTIVE'
		  AND currency = $2
		  AND (instrument_id = $1 OR instrument_id IS NULL)
		  AND effective_from <= now()
		ORDER BY instrument_id NULLS LAST LIMIT 1`
	// DustClaimSweepSQL — the daily claim; no row returned = already swept.
	DustClaimSweepSQL = `
		INSERT INTO dust_sweeps
		    (account_id, currency, base_currency, sweep_date, instrument_id,
		     dust_amount, credited_amount, mid_rate, spread_bps)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8::numeric,$9::numeric)
		ON CONFLICT (account_id, currency, sweep_date) DO NOTHING
		RETURNING id`
	// DustSweepForSQL — read back today's recorded sweep.
	DustSweepForSQL = `
		SELECT id, account_id, currency, base_currency, sweep_date, instrument_id,
		       dust_amount::text, credited_amount::text, mid_rate::text,
		       spread_bps::text, COALESCE(journal_entry_id, 0)
		FROM dust_sweeps
		WHERE account_id = $1 AND currency = $2 AND sweep_date = $3`
	// DustCompleteSweepSQL — back-fill the committed journal id.
	DustCompleteSweepSQL = `
		UPDATE dust_sweeps SET journal_entry_id = $2
		WHERE id = $1 AND journal_entry_id IS NULL`
	// DustReleaseSweepSQL — free a crashed (unposted) claim only.
	DustReleaseSweepSQL = `
		DELETE FROM dust_sweeps WHERE id = $1 AND journal_entry_id IS NULL`
)
