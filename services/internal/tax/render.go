package tax

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

// RenderCSV produces the Task 5.3.19 CSV export — header, one row per
// lot disposal, blank line, per-currency totals. Decimals are rendered
// fixed-point (8dp quantum, §5.3) so downstream tooling parses them as
// exact strings, never floats.
func RenderCSV(r *Report) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "account_id,%d\n", r.AccountID)
	fmt.Fprintf(&b, "year,%d\n", r.Year)
	if !r.From.IsZero() {
		fmt.Fprintf(&b, "from,%s\n", r.From.UTC().Format(time.RFC3339))
	}
	if !r.To.IsZero() {
		fmt.Fprintf(&b, "to,%s\n", r.To.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "method,%s\n", r.Method)
	fmt.Fprintf(&b, "book_of_record_method,%s\n", r.BookOfRecordMethod)
	fmt.Fprintf(&b, "projection,%t\n", r.Projection)
	fmt.Fprintf(&b, "irc_871m_applicability,%s\n", csvField(r.IRC871m))
	fmt.Fprintf(&b, "inducement_statement,%s\n", csvField(r.Inducement))
	fmt.Fprintf(&b, "generated_at,%s\n", r.GeneratedAt.UTC().Format(time.RFC3339))
	b.WriteString("\nsymbol,currency,direction,open_trade_id,close_trade_id,opened_at,closed_at,quantity,unit_cost,unit_proceeds,proceeds,cost_basis,gain\n")
	for _, d := range r.Disposals {
		fmt.Fprintf(&b, "%s,%s,%s,%d,%d,%s,%s,%s,%s,%s,%s,%s,%s\n",
			csvField(d.Symbol), d.Currency, d.Direction,
			d.OpenTradeID, d.CloseTradeID,
			d.OpenedAt.UTC().Format(time.RFC3339),
			d.ClosedAt.UTC().Format(time.RFC3339),
			d.Quantity.StringFixed(8), d.UnitCost.StringFixed(8),
			d.UnitProceeds.StringFixed(8), d.Proceeds.StringFixed(8),
			d.CostBasis.StringFixed(8), d.Gain.StringFixed(8))
	}
	b.WriteString("\ncurrency,disposal_count,total_proceeds,total_cost_basis,net_gain\n")
	for _, s := range r.Summary {
		fmt.Fprintf(&b, "%s,%d,%s,%s,%s\n",
			s.Currency, s.DisposalCount,
			s.TotalProceeds.StringFixed(8), s.TotalCostBasis.StringFixed(8),
			s.NetGain.StringFixed(8))
	}
	if len(r.Flows) > 0 {
		b.WriteString("\nkind,direction,currency,amount,reference_id,at,narrative\n")
		for _, f := range r.Flows {
			fmt.Fprintf(&b, "%s,%s,%s,%s,%d,%s,%s\n",
				f.Kind, f.Direction, f.Currency, f.Amount.StringFixed(8),
				f.ReferenceID, f.At.UTC().Format(time.RFC3339),
				csvField(f.Narrative))
		}
	}
	if len(r.FlowTotals) > 0 {
		b.WriteString("\ncurrency,fees_paid,rebates,swap_paid,swap_received,net_cost\n")
		for _, s := range r.FlowTotals {
			fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%s\n",
				s.Currency, s.FeesPaid.StringFixed(8), s.Rebates.StringFixed(8),
				s.SwapPaid.StringFixed(8), s.SwapReceived.StringFixed(8),
				s.NetCost.StringFixed(8))
		}
	}
	return b.Bytes()
}

func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// ---------------------------------------------------------------------------
// Minimal PDF — hand-rolled single-family text pages (no dependency).
// Standard PDF/Helvetica, one content stream per page, fixed row layout.
// ---------------------------------------------------------------------------

// RenderPDF produces a valid PDF-1.4 document: title block + per-lot
// table + per-currency totals. Pagination at 40 rows/page.
func RenderPDF(r *Report) []byte {
	type line struct {
		text string
		size int
	}
	pages := [][]line{}
	cur := []line{}
	flush := func() {
		if len(cur) > 0 {
			pages = append(pages, cur)
			cur = nil
		}
	}
	emit := func(s string, size int) {
		cur = append(cur, line{s, size})
		if len(cur) >= 44 {
			flush()
		}
	}

	emit("Exchange — Realised Gain/Loss Tax Report", 16)
	emit(fmt.Sprintf("Account %d   Year %d   Method %s", r.AccountID, r.Year, r.Method), 11)
	if r.Projection {
		emit(fmt.Sprintf("PLANNING PROJECTION — not the book of record (filed method: %s)", r.BookOfRecordMethod), 10)
	}
	emit(fmt.Sprintf("IRC 871(m) applicability: %s", r.IRC871m), 9)
	if r.Inducement != "" {
		emit(fmt.Sprintf("Inducements: %s", r.Inducement), 9)
	}
	emit(fmt.Sprintf("Generated %s UTC", r.GeneratedAt.UTC().Format("2006-01-02 15:04:05")), 9)
	emit(" ", 8)
	emit(fmt.Sprintf("%-12s %-4s %-10s %12s %12s %12s %14s %14s",
		"Symbol", "Ccy", "Direction", "Qty", "UnitCost", "Proceeds", "CostBasis", "Gain"), 9)
	for _, d := range r.Disposals {
		emit(fmt.Sprintf("%-12s %-4s %-10s %12s %12s %12s %14s %14s",
			trunc(d.Symbol, 12), d.Currency, trunc(d.Direction, 10),
			d.Quantity.StringFixed(4), d.UnitCost.StringFixed(8),
			d.Proceeds.StringFixed(2), d.CostBasis.StringFixed(2),
			d.Gain.StringFixed(2)), 9)
	}
	emit(" ", 8)
	emit(fmt.Sprintf("%-8s %8s %16s %16s %16s",
		"Currency", "Count", "TotalProceeds", "TotalCostBasis", "NetGain"), 10)
	for _, s := range r.Summary {
		emit(fmt.Sprintf("%-8s %8d %16s %16s %16s",
			s.Currency, s.DisposalCount, s.TotalProceeds.StringFixed(2),
			s.TotalCostBasis.StringFixed(2), s.NetGain.StringFixed(2)), 10)
	}
	if len(r.FlowTotals) > 0 {
		emit(" ", 8)
		emit(fmt.Sprintf("%-8s %14s %14s %14s %14s %14s",
			"Currency", "FeesPaid", "Rebates", "SwapPaid", "SwapRcvd", "NetCost"), 10)
		for _, s := range r.FlowTotals {
			emit(fmt.Sprintf("%-8s %14s %14s %14s %14s %14s",
				s.Currency, s.FeesPaid.StringFixed(2), s.Rebates.StringFixed(2),
				s.SwapPaid.StringFixed(2), s.SwapReceived.StringFixed(2),
				s.NetCost.StringFixed(2)), 10)
		}
	}
	flush()
	if len(pages) == 0 {
		pages = [][]line{{
			{"Exchange — Realised Gain/Loss Tax Report", 16},
			{fmt.Sprintf("Account %d  Year %d  no disposals", r.AccountID, r.Year), 11},
		}}
	}

	// Assemble objects: 1=catalog 2=pages 3=font, then per page
	// (pageObj, contentObj).
	var out bytes.Buffer
	offsets := []int{}
	obj := func(id int, body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", id, body)
	}
	out.WriteString("%PDF-1.4\n")
	n := len(pages)
	kids := ""
	for i := 0; i < n; i++ {
		kids += fmt.Sprintf("%d 0 R ", 4+i*2)
	}
	obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	obj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n))
	obj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Courier >>")
	for i, pg := range pages {
		contentID := 4 + i*2 + 1
		obj(4+i*2, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>",
			contentID))
		var stream bytes.Buffer
		stream.WriteString("BT\n40 750 Td\n")
		for _, l := range pg {
			fmt.Fprintf(&stream, "/F1 %d Tf (%s) Tj 0 -%d Td\n",
				l.size, pdfEsc(l.text), l.size+6)
		}
		stream.WriteString("ET")
		obj(contentID, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream",
			stream.Len(), stream.String()))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", 4+2*n)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		4+2*n, xref)
	return out.Bytes()
}

func pdfEsc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `(`, `\(`)
	s = strings.ReplaceAll(s, `)`, `\)`)
	return s
}

func trunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
