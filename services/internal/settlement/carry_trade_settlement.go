// Automated FX Carry Trade Swap Yield Tracking & Bot Settlement —
// Phase-03 Task 3.3.15 (spec §15.3, §24 #288; schema migration 115).
//
// A carry-trade bot holds a hedged allocation — typically a LONG position
// in a high-yield pair (earning positive swap) against SHORT hedge legs —
// and monetizes the overnight interest differential. At each 17:00 ET
// rollover (triple-swap Wednesday included) this engine:
//
//  1. loads ACTIVE allocations (carry_trade_allocations + legs — the bots
//     themselves are Phase-16 Task 16.3.x's; bot_ref is the handle);
//  2. prices each leg through the SAME swap math as the position-level
//     engine (SwapEngine.priceAccrual — rate, day-count, markup,
//     swap-free carve-out all shared), then nets the signed client deltas
//     per accrual currency;
//  3. posts ONE balanced GL journal per (allocation, currency) crediting
//     or debiting the bot's sub-account balance (EOD_ROLLOVER entry type;
//     idempotency key carry:{allocation}:{yyyymmdd}:{ccy});
//  4. records the distribution with per-leg detail and the running
//     cumulative yield (carry_yield_records / carry_yield_totals) for bot
//     performance reporting.
//
// Ownership note: allocations own their legs exclusively — a position that
// is a carry leg MUST NOT also run through the per-position ProcessRollover
// path, or the swap would post twice. The rollover service (Task 3.3.7)
// enforces the exclusion when it builds the position set.
//
// Fail-closed (spec §2.7): per-allocation failures are collected in the
// report and leave that allocation untouched; store/policy load failures
// abort the run.
package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// CodeCarrySettlementInternal is a scaffold error code pending canonical
// registration in Phase-05 Task 5.3.21.
const CodeCarrySettlementInternal = "CARRY_SETTLEMENT_INTERNAL"

// CarryStatus mirrors carry_allocation_status_enum (migration 115).
type CarryStatus string

const (
	CarryActive CarryStatus = "ACTIVE"
	CarryPaused CarryStatus = "PAUSED"
	CarryClosed CarryStatus = "CLOSED"
)

// CarryLeg is one hedged leg of an allocation: a positions row the bot
// owns. Quantity/LotSize/Notional are denormalized from positions and
// instruments at load time (same conventions as SwapPosition).
type CarryLeg struct {
	ID           int64
	AllocationID int64
	PositionID   int64
	InstrumentID int64
	Symbol       string
	Base         string
	Quote        string
	Side         string          // LONG | SHORT
	Quantity     decimal.Decimal // base-currency units
	LotSize      decimal.Decimal
	Notional     decimal.Decimal // accrual-ccy notional for the markup leg
}

// accrualCcy is the currency the leg's swap settles in — the pair's quote
// currency, matching the position-level convention.
func (l CarryLeg) accrualCcy() string { return strings.ToUpper(l.Quote) }

// asSwapPosition adapts the leg for SwapEngine.priceAccrual.
func (l CarryLeg) asSwapPosition(accountID int64) SwapPosition {
	return SwapPosition{
		PositionID:   l.PositionID,
		AccountID:    accountID,
		InstrumentID: l.InstrumentID,
		Symbol:       l.Symbol,
		Base:         l.Base,
		Quote:        l.Quote,
		Side:         l.Side,
		Quantity:     l.Quantity,
		LotSize:      l.LotSize,
		Notional:     l.Notional,
		AccrualCcy:   l.accrualCcy(),
	}
}

// CarryAllocation is one open carry-trade bot allocation. The daily yield
// lands on AccountID — the bot's sub-account.
type CarryAllocation struct {
	ID        int64
	AccountID int64
	BotRef    string // Phase-16 bot registry handle
	Status    CarryStatus
	Legs      []CarryLeg
}

// CarryLegYield is the per-leg audit slice persisted in
// carry_yield_records.leg_detail (decimal strings, never float64).
type CarryLegYield struct {
	PositionID       int64  `json:"position_id"`
	Symbol           string `json:"symbol"`
	Side             string `json:"side"`
	Days             int    `json:"days"`
	SwapPoints       string `json:"swap_points"`
	InterbankAmount  string `json:"interbank_amount"`
	MarkupAmount     string `json:"markup_amount"`
	Delta            string `json:"delta"` // signed client delta
	RateStale        bool   `json:"rate_stale,omitempty"`
	SwapFreeForegone string `json:"swap_free_foregone,omitempty"`
}

// CarryYieldRecord is one daily distribution row (carry_yield_records).
type CarryYieldRecord struct {
	AllocationID    int64
	AccrualDate     time.Time // UTC-normalized roll date
	Currency        string
	Days            int             // max leg day-count in this bucket
	GrossCredit     decimal.Decimal // Σ positive leg deltas
	GrossDebit      decimal.Decimal // Σ |negative leg deltas|
	NetYield        decimal.Decimal // signed net = credit − debit
	CumulativeYield decimal.Decimal // running total, set by the store
	LegDetail       []CarryLegYield
	JournalEntryID  int64 // 0 when net == 0 (nothing posted)
}

// CarryStore is the persistence seam; PgCarryStore implements it over pgx.
type CarryStore interface {
	// ActiveCarryAllocations returns every status='ACTIVE' allocation
	// with its legs.
	ActiveCarryAllocations(ctx context.Context) ([]CarryAllocation, error)
	// RecordCarryYield persists one distribution atomically — the yield
	// row plus the running cumulative total — and returns the new
	// cumulative yield for (allocation, currency). Replay on
	// (allocation_id, accrual_date, currency) returns the stored
	// cumulative without re-accumulating.
	RecordCarryYield(ctx context.Context, rec CarryYieldRecord) (decimal.Decimal, error)
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// CarryLegResult is one leg's priced outcome.
type CarryLegResult struct {
	PositionID int64  `json:"position_id"`
	Symbol     string `json:"symbol"`
	Delta      string `json:"delta,omitempty"`
	Err        string `json:"err,omitempty"`
}

// CarryAllocationResult is one allocation's settlement outcome.
type CarryAllocationResult struct {
	AllocationID int64             `json:"allocation_id"`
	AccountID    int64             `json:"account_id"`
	BotRef       string            `json:"bot_ref"`
	JournalIDs   []int64           `json:"journal_ids"` // one per non-zero currency bucket
	NetYields    map[string]string `json:"net_yields"`  // currency → signed decimal
	Cumulative   map[string]string `json:"cumulative"`  // currency → running total
	Legs         []CarryLegResult  `json:"legs"`
	Err          string            `json:"err,omitempty"`
}

// CarryReport summarizes one SettleDay run.
type CarryReport struct {
	RollDate    time.Time               `json:"roll_date"`
	Allocations int                     `json:"allocations"`
	Settled     int                     `json:"settled"`
	Failed      int                     `json:"failed"`
	Results     []CarryAllocationResult `json:"results"`
}

// CarrySettlementEngine settles daily carry yield. It shares the swap
// engine's rate store, calendar, clock and poster so leg pricing is
// identical to the per-position path.
type CarrySettlementEngine struct {
	swaps *SwapEngine
	store CarryStore
}

// NewCarrySettlementEngine wires the service around an existing SwapEngine
// (single source of rate/day-count/markup truth). store is required.
func NewCarrySettlementEngine(swaps *SwapEngine, store CarryStore) (*CarrySettlementEngine, error) {
	if swaps == nil || store == nil {
		return nil, fmt.Errorf("carry settlement: swap engine and store are required")
	}
	return &CarrySettlementEngine{swaps: swaps, store: store}, nil
}

// SettleDay runs the daily 17:00 ET distribution for all ACTIVE
// allocations: net earned swap across each allocation's hedged legs,
// posted to the bot sub-account and recorded with cumulative metrics.
// rollInstant is any instant inside the rollover being settled (the NY
// civil date is derived via the engine's RolloverClock).
func (e *CarrySettlementEngine) SettleDay(ctx context.Context, rollInstant time.Time) (*CarryReport, error) {
	rollDate := e.swaps.rc.RollDate(rollInstant)
	rep := &CarryReport{RollDate: rollDate}

	policies, err := e.swaps.store.ActiveSwapMarkupPolicies(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeCarrySettlementInternal,
			"carry: load markup policies", err)
	}
	allocs, err := e.store.ActiveCarryAllocations(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeCarrySettlementInternal,
			"carry: load allocations", err)
	}
	rep.Allocations = len(allocs)

	for _, alloc := range allocs {
		res := e.settleAllocation(ctx, alloc, rollDate, policies)
		rep.Results = append(rep.Results, res)
		if res.Err != "" {
			rep.Failed++
		} else {
			rep.Settled++
		}
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
	}
	return rep, nil
}

// settleAllocation prices every leg, nets deltas per accrual currency,
// posts one journal per non-zero bucket and records the distribution.
func (e *CarrySettlementEngine) settleAllocation(ctx context.Context, alloc CarryAllocation, rollDate time.Time, policies []ledger.SwapMarkupPolicy) CarryAllocationResult {
	res := CarryAllocationResult{
		AllocationID: alloc.ID, AccountID: alloc.AccountID, BotRef: alloc.BotRef,
		NetYields: map[string]string{}, Cumulative: map[string]string{},
	}
	fail := func(err error) CarryAllocationResult {
		res.Err = err.Error()
		return res
	}
	if len(alloc.Legs) == 0 {
		return fail(excerrors.New(CodeCarrySettlementInternal,
			fmt.Sprintf("allocation %d (%s) has no legs", alloc.ID, alloc.BotRef)))
	}

	// Price every leg through the shared engine math; bucket by currency.
	type bucket struct {
		net, grossCredit, grossDebit decimal.Decimal
		days                         int
		detail                       []CarryLegYield
	}
	buckets := map[string]*bucket{}
	for _, leg := range alloc.Legs {
		lr := CarryLegResult{PositionID: leg.PositionID, Symbol: leg.Symbol}
		acc, rate, days, stale, err := e.swaps.priceAccrual(ctx, leg.asSwapPosition(alloc.AccountID), rollDate, policies)
		if err != nil {
			lr.Err = err.Error()
			res.Legs = append(res.Legs, lr)
			return fail(excerrors.Wrap(CodeCarrySettlementInternal,
				fmt.Sprintf("leg position %d", leg.PositionID), err))
		}
		delta := acc.Record.ClientDelta
		lr.Delta = delta.String()
		res.Legs = append(res.Legs, lr)

		ccy := leg.accrualCcy()
		b := buckets[ccy]
		if b == nil {
			b = &bucket{}
			buckets[ccy] = b
		}
		b.net = b.net.Add(delta)
		if delta.IsPositive() {
			b.grossCredit = b.grossCredit.Add(delta)
		} else if delta.IsNegative() {
			b.grossDebit = b.grossDebit.Add(delta.Neg())
		}
		if days > b.days {
			b.days = days
		}
		ly := CarryLegYield{
			PositionID: leg.PositionID, Symbol: leg.Symbol, Side: leg.Side,
			Days:            days,
			SwapPoints:      legPoints(rate, leg.Side),
			InterbankAmount: acc.Record.InterbankAmount.String(),
			MarkupAmount:    acc.Record.MarkupAmount.String(),
			Delta:           delta.String(),
			RateStale:       stale,
		}
		if acc.Record.SwapFree {
			ly.SwapFreeForegone = acc.Record.ForegoneAmount.String()
		}
		b.detail = append(b.detail, ly)
	}

	// Deterministic currency order for reproducible posting order.
	ccys := make([]string, 0, len(buckets))
	for ccy := range buckets {
		ccys = append(ccys, ccy)
	}
	sort.Strings(ccys)

	for _, ccy := range ccys {
		b := buckets[ccy]
		rec := CarryYieldRecord{
			AllocationID: alloc.ID, AccrualDate: rollDate, Currency: ccy,
			Days: b.days, GrossCredit: b.grossCredit, GrossDebit: b.grossDebit,
			NetYield: b.net, LegDetail: b.detail,
		}
		if !b.net.IsZero() {
			j := carryJournal(alloc, rollDate, ccy, b.net)
			posted, err := e.swaps.poster.Post(ctx, j)
			if err != nil {
				return fail(excerrors.Wrap(CodeCarrySettlementInternal,
					fmt.Sprintf("allocation %d %s yield posting", alloc.ID, ccy), err))
			}
			rec.JournalEntryID = posted.JournalID
			res.JournalIDs = append(res.JournalIDs, posted.JournalID)
		}
		cumulative, err := e.store.RecordCarryYield(ctx, rec)
		if err != nil {
			return fail(excerrors.Wrap(CodeCarrySettlementInternal,
				fmt.Sprintf("allocation %d %s yield record (journal %d committed)",
					alloc.ID, ccy, rec.JournalEntryID), err))
		}
		res.NetYields[ccy] = b.net.String()
		res.Cumulative[ccy] = cumulative.String()
	}
	return res
}

// legPoints picks the applied point column for narrative detail.
func legPoints(rate SwapRate, side string) string {
	if strings.EqualFold(side, string(ledger.SwapShort)) {
		return rate.ShortPoints.String()
	}
	return rate.LongPoints.String()
}

// carryJournal builds the balanced distribution journal: positive net
// yield credits the bot sub-account (house revenue contra-debit); negative
// debits it. Entry type EOD_ROLLOVER — this IS the rollover accrual for
// carry-bot-owned legs.
func carryJournal(alloc CarryAllocation, rollDate time.Time, ccy string, net decimal.Decimal) ledger.Journal {
	amt := net.Abs()
	dateStr := rollDate.Format("2006-01-02")
	desc := fmt.Sprintf("CARRY %s alloc=%d net=%s %s", dateStr, alloc.ID, net.String(), ccy)
	j := ledger.Journal{
		EntryType:      ledger.EntryEODRollover,
		ReferenceID:    alloc.ID,
		Description:    desc,
		PostedBy:       "carry-settlement",
		IdempotencyKey: fmt.Sprintf("carry:%d:%s:%s", alloc.ID, rollDate.Format("20060102"), ccy),
		Effects: []ledger.AccountEffect{{
			AccountID:      alloc.AccountID,
			Currency:       ccy,
			AvailableDelta: net,
			AllowNegative:  true,
		}},
	}
	if net.IsPositive() {
		j.Lines = []ledger.Line{
			ledger.DebitLine(ledger.SwapRolloverRevenue(ccy), ccy, amt, desc+" leg=interbank"),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, amt, desc+" leg=interbank"),
		}
	} else {
		j.Lines = []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(ccy), ccy, amt, desc+" leg=interbank"),
			ledger.CreditLine(ledger.SwapRolloverRevenue(ccy), ccy, amt, desc+" leg=interbank"),
		}
	}
	return j
}

// ---------------------------------------------------------------------------
// PgCarryStore — production CarryStore over pgx (PostgreSQL 16)
// ---------------------------------------------------------------------------

// PgCarryStore implements CarryStore over carry_trade_allocations,
// carry_trade_legs, carry_yield_records and carry_yield_totals
// (migration 115). Numerics cross the wire as text.
type PgCarryStore struct {
	Pool *pgxpool.Pool
}

// NewPgCarryStore wraps a pool.
func NewPgCarryStore(pool *pgxpool.Pool) *PgCarryStore {
	return &PgCarryStore{Pool: pool}
}

// ActiveCarryAllocations implements CarryStore.
func (s *PgCarryStore) ActiveCarryAllocations(ctx context.Context) ([]CarryAllocation, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT a.id, a.account_id, a.bot_ref, a.status::text,
		       l.id, l.position_id, l.instrument_id, l.side::text,
		       i.symbol, i.base_currency, i.quote_currency, i.lot_size::text,
		       p.quantity::text,
		       (p.quantity * COALESCE(p.mark_price, p.entry_price))::text
		FROM carry_trade_allocations a
		JOIN carry_trade_legs l ON l.allocation_id = a.id
		JOIN instruments i ON i.id = l.instrument_id
		JOIN positions  p ON p.id = l.position_id
		WHERE a.status = 'ACTIVE'
		ORDER BY a.id, l.id`)
	if err != nil {
		return nil, fmt.Errorf("carry allocations: %w", err)
	}
	defer rows.Close()

	byID := map[int64]*CarryAllocation{}
	var order []int64
	for rows.Next() {
		var (
			a                      CarryAllocation
			st                     string
			leg                    CarryLeg
			symbol, lot, qty, notl string
		)
		if err := rows.Scan(&a.ID, &a.AccountID, &a.BotRef, &st,
			&leg.ID, &leg.PositionID, &leg.InstrumentID, &leg.Side,
			&symbol, &leg.Base, &leg.Quote, &lot, &qty, &notl); err != nil {
			return nil, fmt.Errorf("carry allocations scan: %w", err)
		}
		leg.Symbol = symbol
		var err error
		if leg.LotSize, err = decimal.NewFromString(lot); err != nil {
			return nil, fmt.Errorf("leg lot size %q: %w", lot, err)
		}
		if leg.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, fmt.Errorf("leg qty %q: %w", qty, err)
		}
		if leg.Notional, err = decimal.NewFromString(notl); err != nil {
			return nil, fmt.Errorf("leg notional %q: %w", notl, err)
		}
		cur := byID[a.ID]
		if cur == nil {
			a.Status = CarryStatus(st)
			byID[a.ID] = &a
			order = append(order, a.ID)
			cur = &a
		}
		leg.AllocationID = a.ID
		cur.Legs = append(cur.Legs, leg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]CarryAllocation, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// RecordCarryYield implements CarryStore — the yield row and the running
// total commit in one transaction so cumulative metrics can never diverge
// from the distribution audit.
func (s *PgCarryStore) RecordCarryYield(ctx context.Context, rec CarryYieldRecord) (decimal.Decimal, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return decimal.Zero, fmt.Errorf("carry yield tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	detail, err := json.Marshal(rec.LegDetail)
	if err != nil {
		return decimal.Zero, fmt.Errorf("carry yield leg detail encode: %w", err)
	}

	// Insert the audit row first; a same-day replay (journal already
	// committed) resolves to the stored cumulative without re-adding.
	var recordID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO carry_yield_records
		    (allocation_id, journal_entry_id, accrual_date, currency, days,
		     gross_credit, gross_debit, net_yield, cumulative_yield, leg_detail)
		VALUES ($1, NULLIF($2,0), $3, $4, $5, $6::numeric, $7::numeric, $8::numeric, 0, $9::jsonb)
		ON CONFLICT (allocation_id, accrual_date, currency) DO NOTHING
		RETURNING id`,
		rec.AllocationID, rec.JournalEntryID, rec.AccrualDate, rec.Currency,
		rec.Days, rec.GrossCredit.String(), rec.GrossDebit.String(),
		rec.NetYield.String(), string(detail)).Scan(&recordID)
	if err == pgx.ErrNoRows {
		// Replay: return the recorded cumulative.
		var cum string
		if err := tx.QueryRow(ctx, `
			SELECT cumulative_yield::text FROM carry_yield_records
			WHERE allocation_id = $1 AND accrual_date = $2 AND currency = $3`,
			rec.AllocationID, rec.AccrualDate, rec.Currency).Scan(&cum); err != nil {
			return decimal.Zero, fmt.Errorf("carry yield replay lookup: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return decimal.Zero, fmt.Errorf("carry yield replay commit: %w", err)
		}
		return decimal.NewFromString(cum)
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("carry yield insert: %w", err)
	}

	// New record → bump the running total only when yield actually moved.
	var cum string
	if rec.NetYield.IsZero() {
		err = tx.QueryRow(ctx, `
			SELECT cumulative_yield::text FROM carry_yield_totals
			WHERE allocation_id = $1 AND currency = $2`,
			rec.AllocationID, rec.Currency).Scan(&cum)
		if err == pgx.ErrNoRows {
			cum = "0"
		} else if err != nil {
			return decimal.Zero, fmt.Errorf("carry totals read: %w", err)
		}
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO carry_yield_totals (allocation_id, currency, cumulative_yield, distributions)
			VALUES ($1, $2, $3::numeric, 1)
			ON CONFLICT (allocation_id, currency) DO UPDATE SET
			    cumulative_yield = carry_yield_totals.cumulative_yield + EXCLUDED.cumulative_yield,
			    distributions    = carry_yield_totals.distributions + 1,
			    updated_at       = now()
			RETURNING cumulative_yield::text`,
			rec.AllocationID, rec.Currency, rec.NetYield.String()).Scan(&cum)
		if err != nil {
			return decimal.Zero, fmt.Errorf("carry totals upsert: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE carry_yield_records SET cumulative_yield = $2::numeric WHERE id = $1`,
		recordID, cum); err != nil {
		return decimal.Zero, fmt.Errorf("carry yield cumulative update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return decimal.Zero, fmt.Errorf("carry yield commit: %w", err)
	}
	return decimal.NewFromString(cum)
}
