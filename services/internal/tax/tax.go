// Package tax implements Phase-05 Task 5.3.19 — realised gain/loss tax
// reporting over the account's actual trade history.
//
// Model: every instrument is BASE/QUOTE. A BUY fill opens a lot whose
// unit cost basis is the execution price (quote per base); a SELL fill
// consumes open lots under the selected method (FIFO default; LIFO,
// HIFO and AVG_COST are supported for forward-compat with Phase-20's
// calculator — the route exposes FIFO only until Phase-20 owns the
// method parameter). Each consumed lot slice is a Disposal with
// proceeds, allocated cost basis and realised gain. Fill fees are
// allocated pro-rata across the slices the fill produced: BUY fees
// raise basis, SELL fees reduce proceeds (spec §5.21 money movement
// semantics — fees are part of realised cost/proceeds).
//
// Sells that exceed open long quantity open a short lot; later buys
// close it — gain = (lot_open_price − cover_price) × qty. For a
// spot-only fiat book this is a degenerate path but keeps the engine
// total-correct rather than silently dropping basis.
//
// Source data is the trades table (real fills, not a stub) joined to
// instruments for symbol/quote currency.
package tax

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// Method selects the lot-matching algorithm.
type Method string

const (
	MethodFIFO    Method = "FIFO"
	MethodLIFO    Method = "LIFO"
	MethodHIFO    Method = "HIFO"
	MethodAvgCost Method = "AVG_COST"
)

// ParseMethod validates a method query param. Empty → FIFO (the
// Task 5.3.19 canonical method).
func ParseMethod(s string) (Method, error) {
	switch Method(s) {
	case "", MethodFIFO:
		return MethodFIFO, nil
	case MethodLIFO:
		return MethodLIFO, nil
	case MethodHIFO:
		return MethodHIFO, nil
	case MethodAvgCost:
		return MethodAvgCost, nil
	}
	return "", fmt.Errorf("tax: unknown method %q (fifo, lifo, hifo, avg_cost)", s)
}

// Fill is one executed trade leg for the account, normalised to the
// account's side.
type Fill struct {
	TradeID       int64
	InstrumentID  int64
	Symbol        string
	QuoteCurrency string
	Side          string // BUY | SELL — the account's side of the trade
	Quantity      decimal.Decimal
	Price         decimal.Decimal // quote per base
	Fee           decimal.Decimal // quote currency
	At            time.Time
}

// Disposal is one closed lot slice — the per-lot gain/loss row.
type Disposal struct {
	Symbol       string          `json:"symbol"`
	Currency     string          `json:"currency"`  // proceeds/basis denomination (quote ccy)
	Direction    string          `json:"direction"` // LONG_CLOSE | SHORT_CLOSE
	OpenTradeID  int64           `json:"open_trade_id"`
	CloseTradeID int64           `json:"close_trade_id"`
	OpenedAt     time.Time       `json:"opened_at"`
	ClosedAt     time.Time       `json:"closed_at"`
	Quantity     decimal.Decimal `json:"quantity"`
	UnitCost     decimal.Decimal `json:"unit_cost"`     // per-unit cost basis (quote/base)
	UnitProceeds decimal.Decimal `json:"unit_proceeds"` // per-unit proceeds
	Proceeds     decimal.Decimal `json:"proceeds"`      // qty × proceeds price ± fee share
	CostBasis    decimal.Decimal `json:"cost_basis"`    // qty × lot cost ± fee share
	Gain         decimal.Decimal `json:"gain"`          // proceeds − cost_basis
}

// CurrencySummary aggregates disposals per quote currency — a mixed-book
// account can never be summed across currencies.
type CurrencySummary struct {
	Currency       string          `json:"currency"`
	DisposalCount  int             `json:"disposal_count"`
	TotalProceeds  decimal.Decimal `json:"total_proceeds"`
	TotalCostBasis decimal.Decimal `json:"total_cost_basis"`
	NetGain        decimal.Decimal `json:"net_gain"`
}

// Report is the Task 5.3.19 response shape.
type Report struct {
	AccountID   int64             `json:"account_id"`
	Year        int               `json:"year"`
	Method      Method            `json:"method"`
	Disposals   []Disposal        `json:"disposals"`
	Summary     []CurrencySummary `json:"summary"`
	GeneratedAt time.Time         `json:"generated_at"`
}

// ---------------------------------------------------------------------------
// Lot engine — pure functions over a fill stream.
// ---------------------------------------------------------------------------

// lot is an open position fragment.
type lot struct {
	qty      decimal.Decimal // positive = long, negative = short
	unitCost decimal.Decimal
	openedAt time.Time
	tradeID  int64
}

// Compute walks fills (must be chronological) and returns every disposal
// whose closing fill lands in [yearStart, yearEnd). Lots opened before
// the window still contribute basis — the stream must therefore contain
// ALL fills up to yearEnd, not just in-year fills.
func Compute(fills []Fill, method Method, yearStart, yearEnd time.Time) []Disposal {
	books := map[int64][]lot{} // instrument → open lots
	var disposals []Disposal

	for _, f := range fills {
		open := books[f.InstrumentID]
		remaining := f.Quantity
		inYear := !f.At.Before(yearStart) && f.At.Before(yearEnd)
		// Fee pool for this fill — allocated pro-rata across slices.
		feeLeft := f.Fee

		closeQty := func(l *lot, qty decimal.Decimal) Disposal {
			feeShare := decimal.Zero
			if f.Quantity.IsPositive() {
				feeShare = feeLeft.Mul(qty).Div(remaining).Round(8)
			}
			feeLeft = feeLeft.Sub(feeShare)
			d := Disposal{
				Symbol:       f.Symbol,
				Currency:     f.QuoteCurrency,
				OpenTradeID:  l.tradeID,
				CloseTradeID: f.TradeID,
				OpenedAt:     l.openedAt,
				ClosedAt:     f.At,
				Quantity:     qty,
				UnitCost:     l.unitCost,
				UnitProceeds: f.Price,
			}
			if l.qty.IsPositive() {
				d.Direction = "LONG_CLOSE"
				d.Proceeds = qty.Mul(f.Price).Sub(feeShare).Round(8)
				// opening fee share already sits in unitCost — see lotCost.
				d.CostBasis = qty.Mul(l.unitCost).Round(8)
				d.Gain = d.Proceeds.Sub(d.CostBasis)
			} else {
				d.Direction = "SHORT_CLOSE"
				d.Proceeds = qty.Mul(l.unitCost).Round(8)
				d.CostBasis = qty.Mul(f.Price).Add(feeShare).Round(8)
				d.Gain = d.Proceeds.Sub(d.CostBasis)
			}
			return d
		}

		if method == MethodAvgCost {
			open, disposals = matchAvgCost(f, open, remaining, inYear, disposals, &feeLeft)
		} else {
			for remaining.IsPositive() && len(open) > 0 {
				idx := pickLot(open, method, f.Side)
				l := &open[idx]
				if !opposes(l.qty, f.Side) {
					break
				}
				qty := decimal.Min(remaining, l.qty.Abs())
				d := closeQty(l, qty)
				if inYear {
					disposals = append(disposals, d)
				}
				l.qty = l.qty.Add(signFor(f.Side).Mul(qty))
				if l.qty.IsZero() {
					open = append(open[:idx], open[idx+1:]...)
				}
				remaining = remaining.Sub(qty)
			}
			if remaining.IsPositive() {
				// Unmatched remainder opens a new lot in the fill's
				// direction; absorb the leftover fee pool so the unit
				// cost recovers the fill's full fee on close.
				unitFee := feeLeft.Div(remaining)
				unitCost := f.Price
				if f.Side == "BUY" {
					unitCost = unitCost.Add(unitFee)
				} else {
					unitCost = unitCost.Sub(unitFee)
				}
				open = append(open, lot{
					qty:      signFor(f.Side).Mul(remaining),
					unitCost: unitCost, openedAt: f.At, tradeID: f.TradeID,
				})
			}
		}
		books[f.InstrumentID] = open
	}
	return disposals
}

func signFor(side string) decimal.Decimal {
	if side == "BUY" {
		return decimal.NewFromInt(1)
	}
	return decimal.NewFromInt(-1)
}

// opposes reports whether an open lot's direction is closed by a fill of
// side (longs close on SELL, shorts on BUY).
func opposes(lotQty decimal.Decimal, side string) bool {
	if side == "SELL" {
		return lotQty.IsPositive()
	}
	return lotQty.IsNegative()
}

// pickLot returns the index of the lot the method consumes next. FIFO →
// oldest; LIFO → newest; HIFO → highest unit cost.
func pickLot(open []lot, method Method, side string) int {
	idx := -1
	for i := range open {
		if !opposes(open[i].qty, side) {
			continue
		}
		if idx == -1 {
			idx = i
			continue
		}
		switch method {
		case MethodLIFO:
			if open[i].openedAt.After(open[idx].openedAt) {
				idx = i
			}
		case MethodHIFO:
			if open[i].unitCost.GreaterThan(open[idx].unitCost) {
				idx = i
			}
		default: // FIFO
			if open[i].openedAt.Before(open[idx].openedAt) {
				idx = i
			}
		}
	}
	if idx == -1 {
		return 0
	}
	return idx
}

// matchAvgCost merges all open lots into a running average cost, the
// AVG_COST semantics. feeLeft points at the caller's fee pool.
func matchAvgCost(f Fill, open []lot, remaining decimal.Decimal, inYear bool,
	disposals []Disposal, feeLeft *decimal.Decimal) ([]lot, []Disposal) {

	// Consolidate the book into a single averaged net lot.
	var netQty, costSum decimal.Decimal
	var earliest time.Time
	var have bool
	for _, l := range open {
		netQty = netQty.Add(l.qty)
		costSum = costSum.Add(l.qty.Abs().Mul(l.unitCost))
		if !have || l.openedAt.Before(earliest) {
			earliest, have = l.openedAt, true
		}
	}
	open = open[:0]

	if !netQty.IsZero() && opposes(netQty, f.Side) {
		// Opposing net position — close up to min(|net|, qty) at average cost.
		avg := costSum.Div(netQty.Abs())
		qty := decimal.Min(remaining, netQty.Abs())
		feeShare := feeLeft.Mul(qty).Div(remaining).Round(8)
		*feeLeft = feeLeft.Sub(feeShare)
		d := Disposal{
			Symbol:       f.Symbol,
			Currency:     f.QuoteCurrency,
			OpenTradeID:  0, // averaged book has no single opener
			CloseTradeID: f.TradeID,
			OpenedAt:     earliest,
			ClosedAt:     f.At,
			Quantity:     qty,
			UnitCost:     avg,
			UnitProceeds: f.Price,
		}
		if netQty.IsPositive() {
			d.Direction = "LONG_CLOSE"
			d.Proceeds = qty.Mul(f.Price).Sub(feeShare).Round(8)
			d.CostBasis = qty.Mul(avg).Round(8)
		} else {
			d.Direction = "SHORT_CLOSE"
			d.Proceeds = qty.Mul(avg).Round(8)
			d.CostBasis = qty.Mul(f.Price).Add(feeShare).Round(8)
		}
		d.Gain = d.Proceeds.Sub(d.CostBasis)
		if inYear {
			disposals = append(disposals, d)
		}
		remaining = remaining.Sub(qty)
		netQty = netQty.Add(signFor(f.Side).Mul(qty))
		costSum = avg.Mul(netQty.Abs())
	}

	// Residual (or the whole fill when nothing opposed) merges into the book.
	if remaining.IsPositive() || !netQty.IsZero() {
		costSum = costSum.Add(remaining.Mul(f.Price).Add(*feeLeft))
		*feeLeft = decimal.Zero
		netQty = netQty.Add(signFor(f.Side).Mul(remaining))
		avg := decimal.Zero
		if !netQty.IsZero() {
			avg = costSum.Div(netQty.Abs())
		}
		openAt := earliest
		if !have || f.At.Before(earliest) {
			openAt = f.At
		}
		open = append(open, lot{qty: netQty, unitCost: avg, openedAt: openAt, tradeID: f.TradeID})
	}
	return open, disposals
}

// Summarize folds disposals into per-currency totals.
func Summarize(disposals []Disposal) []CurrencySummary {
	byCcy := map[string]*CurrencySummary{}
	for _, d := range disposals {
		s, ok := byCcy[d.Currency]
		if !ok {
			s = &CurrencySummary{Currency: d.Currency}
			byCcy[d.Currency] = s
		}
		s.DisposalCount++
		s.TotalProceeds = s.TotalProceeds.Add(d.Proceeds)
		s.TotalCostBasis = s.TotalCostBasis.Add(d.CostBasis)
		s.NetGain = s.NetGain.Add(d.Gain)
	}
	out := make([]CurrencySummary, 0, len(byCcy))
	for _, s := range byCcy {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// ---------------------------------------------------------------------------
// PgxSource — fills straight from the trades table.
// ---------------------------------------------------------------------------

// FillSource supplies the chronological fill stream; PgxSource reads
// trades (the execution book of record).
type FillSource interface {
	Fills(ctx context.Context, accountID int64, before time.Time) ([]Fill, error)
}

// ErrNoSource guards compute-only construction.
var ErrNoSource = errors.New("tax: nil fill source")

// PgxSource implements FillSource over pgx.
type PgxSource struct{ Pool *pgxpool.Pool }

// NewPgxSource wires the source.
func NewPgxSource(pool *pgxpool.Pool) *PgxSource { return &PgxSource{Pool: pool} }

// Fills returns every trade leg for the account before `before`,
// oldest first. The fee column is the account's own side (buyer_fee for
// buys, seller_fee for sells).
func (s *PgxSource) Fills(ctx context.Context, accountID int64, before time.Time) ([]Fill, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT t.id, t.instrument_id, i.symbol, i.quote_currency,
		       CASE WHEN t.buyer_account_id=$1 THEN 'BUY' ELSE 'SELL' END,
		       t.quantity::text, t.price::text,
		       COALESCE(CASE WHEN t.buyer_account_id=$1
		                     THEN t.buyer_fee ELSE t.seller_fee END,0)::text,
		       t.created_at
		  FROM trades t
		  JOIN instruments i ON i.id = t.instrument_id
		 WHERE (t.buyer_account_id=$1 OR t.seller_account_id=$1)
		   AND t.created_at < $2
		 ORDER BY t.created_at, t.id`, accountID, before.UTC())
	if err != nil {
		return nil, fmt.Errorf("tax: fills query: %w", err)
	}
	defer rows.Close()
	var out []Fill
	for rows.Next() {
		var f Fill
		var qty, px, fee string
		if err := rows.Scan(&f.TradeID, &f.InstrumentID, &f.Symbol,
			&f.QuoteCurrency, &f.Side, &qty, &px, &fee, &f.At); err != nil {
			return nil, fmt.Errorf("tax: fill scan: %w", err)
		}
		if f.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, fmt.Errorf("tax: qty %q: %w", qty, err)
		}
		if f.Price, err = decimal.NewFromString(px); err != nil {
			return nil, fmt.Errorf("tax: price %q: %w", px, err)
		}
		if f.Fee, err = decimal.NewFromString(fee); err != nil {
			return nil, fmt.Errorf("tax: fee %q: %w", fee, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Service ties the source to the engine.
type Service struct {
	source FillSource
	now    func() time.Time
}

// NewService wires the service; source is required.
func NewService(source FillSource) (*Service, error) {
	if source == nil {
		return nil, ErrNoSource
	}
	return &Service{source: source, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *Service) SetClockForTest(now func() time.Time) { s.now = now }

// Report builds the account's lot/disposal report for `year`.
func (s *Service) Report(ctx context.Context, accountID int64, year int, method Method) (*Report, error) {
	if accountID <= 0 {
		return nil, fmt.Errorf("tax: account id must be positive")
	}
	if year < 1970 || year > s.now().Year()+1 {
		return nil, fmt.Errorf("tax: year %d out of range", year)
	}
	yearStart := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	yearEnd := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC)
	fills, err := s.source.Fills(ctx, accountID, yearEnd)
	if err != nil {
		return nil, err
	}
	disposals := Compute(fills, method, yearStart, yearEnd)
	return &Report{
		AccountID:   accountID,
		Year:        year,
		Method:      method,
		Disposals:   disposals,
		Summary:     Summarize(disposals),
		GeneratedAt: s.now().UTC(),
	}, nil
}
