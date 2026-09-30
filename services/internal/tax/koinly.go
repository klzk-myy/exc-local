// koinly.go — Phase-20 Task 20.3.10: Koinly universal-CSV export.
//
// Koinly's generic CSV layout (12 columns):
//
//	Date, Sent Amount, Sent Currency, Received Amount, Received
//	Currency, Fee Amount, Fee Currency, Net Worth Amount, Net Worth
//	Currency, Label, Description, TxHash
//
// Mapping (spot FX, fiat-only):
//
//	LONG_CLOSE  — the closing SELL disposed of base for quote:
//	              Sent = quantity (base), Received = gross proceeds
//	              (qty × unit proceeds, quote), Fee = the sell-side fee
//	              share (quote; proceeds stored on the disposal are
//	              net — gross is reconstructed deterministically).
//	SHORT_CLOSE — the covering BUY spent quote to reacquire base:
//	              Sent = cost basis (quote), Received = quantity (base).
//	FEE lines   — paid: Fee-only row (Koinly expenses it);
//	              received (rebate): Received-only row, label "rebate".
//	ROLLOVER    — paid (CREDIT): Sent-only row, label "swap_paid";
//	              received (DEBIT): Received-only, label "swap_received".
//
// A disposal's fee share is derived as qty×unitProceeds − Proceeds
// (LONG_CLOSE) — deterministic, no float. Symbols carry both
// "EUR/USD" and "EURUSD" spellings in fixtures; base/quote parse
// handles both, falling back to the whole symbol as Sent currency.
package tax

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

// koinlyDate renders Koinly's preferred "YYYY-MM-DD HH:mm:ss" UTC
// timestamp.
func koinlyDate(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// splitSymbol returns (base, quote). Fallback: ("", symbol) so a
// malformed symbol still yields a usable Received currency.
func splitSymbol(symbol string) (string, string) {
	if i := strings.Index(symbol, "/"); i > 0 {
		return symbol[:i], symbol[i+1:]
	}
	if len(symbol) == 6 {
		return symbol[:3], symbol[3:]
	}
	return "", symbol
}

// RenderKoinlyCSV produces the Koinly import file: one row per
// disposal plus one per flow line. Deterministic ordering (disposals
// then flows, both already chronological) and fixed-point rendering.
func RenderKoinlyCSV(r *Report) []byte {
	var b bytes.Buffer
	b.WriteString("Date,Sent Amount,Sent Currency,Received Amount,Received Currency," +
		"Fee Amount,Fee Currency,Net Worth Amount,Net Worth Currency,Label,Description,TxHash\n")
	row := func(date, sentAmt, sentCcy, recvAmt, recvCcy, feeAmt, feeCcy,
		label, desc, txHash string) {
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%s,%s,,,%s,%s,%s\n",
			date, sentAmt, sentCcy, recvAmt, recvCcy, feeAmt, feeCcy,
			csvField(label), csvField(desc), csvField(txHash))
	}
	for _, d := range r.Disposals {
		base, quote := splitSymbol(d.Symbol)
		grossProceeds := d.Quantity.Mul(d.UnitProceeds).Round(8)
		feeShare := grossProceeds.Sub(d.Proceeds).Round(8)
		switch d.Direction {
		case "SHORT_CLOSE":
			row(koinlyDate(d.ClosedAt),
				d.CostBasis.StringFixed(8), quote,
				d.Quantity.StringFixed(8), base,
				"", "",
				"", fmt.Sprintf("short cover gain %s %s", d.Gain.StringFixed(8), d.Currency),
				fmt.Sprintf("trade:%d", d.CloseTradeID))
		default: // LONG_CLOSE
			feeAmt, feeCcy := "", ""
			if feeShare.IsPositive() {
				feeAmt, feeCcy = feeShare.StringFixed(8), quote
			}
			row(koinlyDate(d.ClosedAt),
				d.Quantity.StringFixed(8), base,
				grossProceeds.StringFixed(8), quote,
				feeAmt, feeCcy,
				"", fmt.Sprintf("close %s gain %s %s", d.Symbol, d.Gain.StringFixed(8), d.Currency),
				fmt.Sprintf("trade:%d", d.CloseTradeID))
		}
	}
	for _, f := range r.Flows {
		sentAmt, sentCcy, recvAmt, recvCcy := "", "", "", ""
		feeAmt, feeCcy := "", ""
		label := strings.ToLower(f.Kind)
		if f.Direction == "CREDIT" { // money out
			if f.Kind == "FEE" {
				feeAmt, feeCcy = f.Amount.StringFixed(8), f.Currency
				label = "fee"
			} else {
				sentAmt, sentCcy = f.Amount.StringFixed(8), f.Currency
				label = "swap_paid"
			}
		} else { // DEBIT — money in
			recvAmt, recvCcy = f.Amount.StringFixed(8), f.Currency
			if f.Kind == "FEE" {
				label = "rebate"
			} else {
				label = "swap_received"
			}
		}
		ref := ""
		if f.ReferenceID != 0 {
			ref = fmt.Sprintf("ledger:%d", f.ReferenceID)
		}
		row(koinlyDate(f.At), sentAmt, sentCcy, recvAmt, recvCcy,
			feeAmt, feeCcy, label, f.Narrative, ref)
	}
	return b.Bytes()
}
