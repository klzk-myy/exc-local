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
//
// Phase-20 Task 20.3.10 deltas:
//   - Reports accept an arbitrary [from,to) UTC window (the year param
//     remains a convenience for the canonical year range).
//   - The report carries the in-window ledger flow lines (FEE +
//     ROLLOVER rows from ledger_entries — the account's append-only
//     book of record, migration 102) so fees and swap accruals sit next
//     to the disposals they belong to; direction DEBIT = money in,
//     CREDIT = money out.
//   - Koinly export (RenderKoinlyCSV in koinly.go) maps disposals and
//     flow lines onto the Koinly universal CSV layout.
//   - A per-account daily generation cap (limiter.go) rate-limits
//     report issuance to TaxReportsPerDay per UTC day.
//   - Every generated document carries NoInducementStatement.
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

// Report is the Task 5.3.19 response shape. Method is the requested lot
// method; BookOfRecordMethod is always FIFO — the filed method (spec
// §12.7). When Method != FIFO the report is a planning projection, not
// the book of record: Projection=true and renders carry the label.
// IRC871m is the §871(m) dividend-equivalent withholding applicability
// note: "N/A" — a spot-only fiat FX venue has no equity swaps to withhold
// on (Phase-12 Task 12.3.13 documentation requirement).
//
// Task 20.3.10 adds: From/To delimit the disposal window (year reports
// set them to the year's UTC bounds); Flows/FlowTotals carry the
// in-window fee and swap ledger lines; Inducement is the fixed
// no-inducement declaration printed on every generated document.
type Report struct {
	AccountID          int64             `json:"account_id"`
	Year               int               `json:"year"`
	From               time.Time         `json:"from"` // window start (inclusive, UTC)
	To                 time.Time         `json:"to"`   // window end (exclusive, UTC)
	Method             Method            `json:"method"`
	BookOfRecordMethod Method            `json:"book_of_record_method"`
	Projection         bool              `json:"projection"`
	IRC871m            string            `json:"irc_871m_applicability"`
	Disposals          []Disposal        `json:"disposals"`
	Summary            []CurrencySummary `json:"summary"`
	Flows              []FlowLine        `json:"flows,omitempty"`
	FlowTotals         []FlowSummary     `json:"flow_totals,omitempty"`
	Inducement         string            `json:"inducement_statement"`
	GeneratedAt        time.Time         `json:"generated_at"`
}

// NoInducementStatement is the fixed declaration carried on every
// generated tax document (Task 20.3.10 — R10: no IB/retrocession flow).
const NoInducementStatement = "No third-party inducements paid or received (venue has no IB/retrocession flow per R10)."

// FlowLine is one in-window ledger flow surfaced on the report —
// FEE (commissions, conversion fees) and ROLLOVER (Tom-Next swap
// accruals) rows from ledger_entries. Direction is the ledger's own:
// DEBIT = money in, CREDIT = money out (migration 102 contract).
type FlowLine struct {
	Kind        string          `json:"kind"`      // FEE | ROLLOVER
	Direction   string          `json:"direction"` // DEBIT | CREDIT
	Currency    string          `json:"currency"`
	Amount      decimal.Decimal `json:"amount"` // positive
	At          time.Time       `json:"at"`
	ReferenceID int64           `json:"reference_id,omitempty"`
	Narrative   string          `json:"narrative,omitempty"`
}

// FlowSummary aggregates the flow lines per currency — the fee/swap
// totals a filer reconciles against the ex-post cost disclosure
// (analytics.CostsDisclosureService reads the same ledger rows, so the
// two documents reconcile by construction).
type FlowSummary struct {
	Currency     string          `json:"currency"`
	FeesPaid     decimal.Decimal `json:"fees_paid"`     // FEE credits
	Rebates      decimal.Decimal `json:"rebates"`       // FEE debits (money in)
	SwapPaid     decimal.Decimal `json:"swap_paid"`     // ROLLOVER credits
	SwapReceived decimal.Decimal `json:"swap_received"` // ROLLOVER debits
	NetCost      decimal.Decimal `json:"net_cost"`      // paid − received
}

// FlowSource supplies the in-window ledger flow lines. PgxSource
// implements it alongside FillSource — both read the same PG pool, so
// NewService auto-detects the capability (no wiring change needed).
type FlowSource interface {
	Flows(ctx context.Context, accountID int64, from, to time.Time) ([]FlowLine, error)
}

// SummarizeFlows folds flow lines into per-currency totals.
func SummarizeFlows(flows []FlowLine) []FlowSummary {
	byCcy := map[string]*FlowSummary{}
	for _, f := range flows {
		s, ok := byCcy[f.Currency]
		if !ok {
			s = &FlowSummary{Currency: f.Currency}
			byCcy[f.Currency] = s
		}
		cost := f.Direction == "CREDIT" // money out = cost
		switch f.Kind {
		case "FEE":
			if cost {
				s.FeesPaid = s.FeesPaid.Add(f.Amount)
			} else {
				s.Rebates = s.Rebates.Add(f.Amount)
			}
		case "ROLLOVER":
			if cost {
				s.SwapPaid = s.SwapPaid.Add(f.Amount)
			} else {
				s.SwapReceived = s.SwapReceived.Add(f.Amount)
			}
		}
	}
	out := make([]FlowSummary, 0, len(byCcy))
	for _, s := range byCcy {
		s.NetCost = s.FeesPaid.Add(s.SwapPaid).
			Sub(s.Rebates).Sub(s.SwapReceived)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
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

// Flows implements FlowSource — the account's in-window FEE and
// ROLLOVER ledger entries (ledger_entries, migration 102; the PG book
// of record the Task 20.3.14 ex-post disclosure reconciles against).
// Entry narratives are the posting engine's prefix convention
// ("COMMISSION trade=…", "SWAP rollover=…" — see internal/ledger and
// internal/settlement posting callers); rows are returned verbatim.
func (s *PgxSource) Flows(ctx context.Context, accountID int64, from, to time.Time) ([]FlowLine, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT entry_type::text, direction::text, currency, amount::text,
		       posted_at, COALESCE(reference_id, 0), COALESCE(description, '')
		  FROM ledger_entries
		 WHERE account_id = $1 AND posted_at >= $2 AND posted_at < $3
		   AND entry_type::text IN ('FEE', 'ROLLOVER')
		 ORDER BY posted_at, id`, accountID, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("tax: flows query: %w", err)
	}
	defer rows.Close()
	var out []FlowLine
	for rows.Next() {
		var f FlowLine
		var amt string
		if err := rows.Scan(&f.Kind, &f.Direction, &f.Currency, &amt,
			&f.At, &f.ReferenceID, &f.Narrative); err != nil {
			return nil, fmt.Errorf("tax: flow scan: %w", err)
		}
		if f.Amount, err = decimal.NewFromString(amt); err != nil {
			return nil, fmt.Errorf("tax: flow amount %q: %w", amt, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Service ties the source to the engine. When the source also
// implements FlowSource (PgxSource does), reports carry the in-window
// fee/swap ledger lines automatically — the existing NewPgxSource
// wiring needs no change.
type Service struct {
	source FillSource
	flows  FlowSource // nil when the fill source cannot read ledger rows
	now    func() time.Time
}

// NewService wires the service; source is required.
func NewService(source FillSource) (*Service, error) {
	if source == nil {
		return nil, ErrNoSource
	}
	s := &Service{source: source, now: time.Now}
	if fs, ok := source.(FlowSource); ok {
		s.flows = fs
	}
	return s, nil
}

// SetFlowSource overrides/attaches the ledger-flow seam — for sources
// that cannot implement FlowSource themselves (test fakes stay tiny).
func (s *Service) SetFlowSource(fs FlowSource) { s.flows = fs }

// SetClockForTest overrides the clock; tests only.
func (s *Service) SetClockForTest(now func() time.Time) { s.now = now }

// Report builds the account's lot/disposal report for `year` — the
// canonical convenience wrapper around ReportRange using the year's
// UTC bounds.
func (s *Service) Report(ctx context.Context, accountID int64, year int, method Method) (*Report, error) {
	if year < 1970 || year > s.now().Year()+1 {
		return nil, fmt.Errorf("tax: year %d out of range", year)
	}
	return s.ReportRange(ctx, accountID,
		time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC), method)
}

// ReportRange builds the account's lot/disposal report for the
// [from,to) UTC window (Task 20.3.10). Lots opened before `from` still
// contribute basis — the fill stream covers everything before `to`.
// Fee/swap flow lines are loaded for the same window when a flow
// source is wired.
func (s *Service) ReportRange(ctx context.Context, accountID int64,
	from, to time.Time, method Method) (*Report, error) {

	if accountID <= 0 {
		return nil, fmt.Errorf("tax: account id must be positive")
	}
	from, to = from.UTC(), to.UTC()
	if !from.Before(to) {
		return nil, fmt.Errorf("tax: from must precede to (%s !< %s)", from, to)
	}
	// A window still open at generation time is clamped to now — the
	// report must never project into the future.
	if now := s.now().UTC(); to.After(now) {
		to = now
	}
	fills, err := s.source.Fills(ctx, accountID, to)
	if err != nil {
		return nil, err
	}
	disposals := Compute(fills, method, from, to)
	rep := &Report{
		AccountID:          accountID,
		Year:               from.Year(),
		From:               from,
		To:                 to,
		Method:             method,
		BookOfRecordMethod: MethodFIFO,
		Projection:         method != MethodFIFO,
		IRC871m:            "N/A — spot FX venue (no §871(m) dividend-equivalent instruments)",
		Disposals:          disposals,
		Summary:            Summarize(disposals),
		Inducement:         NoInducementStatement,
		GeneratedAt:        s.now().UTC(),
	}
	if s.flows != nil {
		flows, err := s.flows.Flows(ctx, accountID, from, to)
		if err != nil {
			return nil, fmt.Errorf("tax: flows load: %w", err)
		}
		rep.Flows = flows
		rep.FlowTotals = SummarizeFlows(flows)
	}
	return rep, nil
}
