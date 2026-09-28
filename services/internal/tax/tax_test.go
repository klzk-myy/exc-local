// Task 5.3.19 — lot-engine unit tests (pure compute, no PG) plus
// renderer checks. PG-backed fill-source tests live in
// tax_integration_test.go (EXC_PG_TEST=1).
package tax

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return d
}

func at(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

// threeBuysThenSell: three 100-unit buys at 1.10 / 1.20 / 1.15 (no fees)
// then a 150-unit sell at 1.30 — distinguishable under every method:
//
//	FIFO → lots 1.10×100 + 1.20×50  → gain 25.00
//	LIFO → lots 1.15×100 + 1.20×50  → gain 20.00
//	HIFO → lots 1.20×100 + 1.15×50  → gain 17.50
//	AVG  → 1.15 avg ×150            → gain 22.50
func threeBuysThenSell() []Fill {
	return []Fill{
		{TradeID: 1, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "BUY", Quantity: dec0("100"), Price: dec0("1.10"), At: at(2026, 1, 5)},
		{TradeID: 2, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "BUY", Quantity: dec0("100"), Price: dec0("1.20"), At: at(2026, 1, 10)},
		{TradeID: 3, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "BUY", Quantity: dec0("100"), Price: dec0("1.15"), At: at(2026, 1, 15)},
		{TradeID: 4, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "SELL", Quantity: dec0("150"), Price: dec0("1.30"), At: at(2026, 2, 1)},
	}
}

// dec0 is dec without a *testing.T — fills are package-level.
func dec0(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func gains(t *testing.T, ds []Disposal) decimal.Decimal {
	t.Helper()
	g := decimal.Zero
	for _, d := range ds {
		g = g.Add(d.Gain)
	}
	return g
}

func year2026() (time.Time, time.Time) {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
}

func TestComputeFIFO(t *testing.T) {
	y0, y1 := year2026()
	ds := Compute(threeBuysThenSell(), MethodFIFO, y0, y1)
	if len(ds) != 2 {
		t.Fatalf("fifo disposals=%d want 2", len(ds))
	}
	if ds[0].OpenTradeID != 1 || ds[1].OpenTradeID != 2 {
		t.Fatalf("fifo consumed wrong lots: %+v", ds)
	}
	if got := gains(t, ds); !got.Equal(dec(t, "25")) {
		t.Fatalf("fifo gain=%s want 25", got)
	}
	if ds[0].Direction != "LONG_CLOSE" {
		t.Fatalf("direction=%s", ds[0].Direction)
	}
}

func TestComputeLIFO(t *testing.T) {
	y0, y1 := year2026()
	ds := Compute(threeBuysThenSell(), MethodLIFO, y0, y1)
	if len(ds) != 2 {
		t.Fatalf("lifo disposals=%d want 2", len(ds))
	}
	if ds[0].OpenTradeID != 3 || ds[1].OpenTradeID != 2 {
		t.Fatalf("lifo consumed wrong lots: %+v", ds)
	}
	if got := gains(t, ds); !got.Equal(dec(t, "20")) {
		t.Fatalf("lifo gain=%s want 20", got)
	}
}

func TestComputeHIFO(t *testing.T) {
	y0, y1 := year2026()
	ds := Compute(threeBuysThenSell(), MethodHIFO, y0, y1)
	if len(ds) != 2 {
		t.Fatalf("hifo disposals=%d want 2", len(ds))
	}
	if ds[0].OpenTradeID != 2 || ds[1].OpenTradeID != 3 {
		t.Fatalf("hifo consumed wrong lots: %+v", ds)
	}
	if got := gains(t, ds); !got.Equal(dec(t, "17.5")) {
		t.Fatalf("hifo gain=%s want 17.5", got)
	}
}

func TestComputeAvgCost(t *testing.T) {
	y0, y1 := year2026()
	ds := Compute(threeBuysThenSell(), MethodAvgCost, y0, y1)
	if len(ds) != 1 {
		t.Fatalf("avg disposals=%d want 1 (single averaged lot): %+v", len(ds), ds)
	}
	d := ds[0]
	if !d.UnitCost.Equal(dec(t, "1.15")) {
		t.Fatalf("avg unit cost=%s want 1.15", d.UnitCost)
	}
	if !d.CostBasis.Equal(dec(t, "172.5")) || !d.Gain.Equal(dec(t, "22.5")) {
		t.Fatalf("avg basis=%s gain=%s", d.CostBasis, d.Gain)
	}
}

// Pre-year lots contribute basis: a buy in 2025 closes against a sell in
// 2026 — the disposal lands in the 2026 report with the 2025 open date.
func TestComputePreYearLotBasis(t *testing.T) {
	fills := []Fill{
		{TradeID: 1, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "BUY", Quantity: dec0("10"), Price: dec0("1.00"), At: at(2025, 11, 1)},
		{TradeID: 2, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "SELL", Quantity: dec0("10"), Price: dec0("1.10"), At: at(2026, 1, 5)},
	}
	y0, y1 := year2026()
	ds := Compute(fills, MethodFIFO, y0, y1)
	if len(ds) != 1 {
		t.Fatalf("disposals=%d want 1", len(ds))
	}
	if ds[0].OpenedAt.Year() != 2025 {
		t.Fatalf("opened_at=%v — pre-year lot date lost", ds[0].OpenedAt)
	}
	if !ds[0].Gain.Equal(dec(t, "1")) { // 10 × (1.10 − 1.00)
		t.Fatalf("gain=%s want 1", ds[0].Gain)
	}
	// The pre-year close-out-of-window disposal itself must NOT appear
	// in the 2025 report (only in-year closings are reported).
	y25s := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	y25e := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if ds25 := Compute(fills, MethodFIFO, y25s, y25e); len(ds25) != 0 {
		t.Fatalf("2025 report disposals=%d want 0 (nothing closed in 2025)", len(ds25))
	}
}

// Fee accounting: buy fees raise basis (inside unitCost); sell fees
// reduce proceeds pro-rata across the slices the fill produced.
func TestComputeFees(t *testing.T) {
	fills := []Fill{
		{TradeID: 1, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "BUY", Quantity: dec0("100"), Price: dec0("1.10"),
			Fee: dec0("0.11"), At: at(2026, 1, 5)}, // unitCost 1.1011
		{TradeID: 2, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "BUY", Quantity: dec0("100"), Price: dec0("1.20"),
			Fee: dec0("0.12"), At: at(2026, 1, 10)}, // unitCost 1.2012
		{TradeID: 3, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "SELL", Quantity: dec0("150"), Price: dec0("1.30"),
			Fee: dec0("0.15"), At: at(2026, 2, 1)},
	}
	y0, y1 := year2026()
	ds := Compute(fills, MethodFIFO, y0, y1)
	if len(ds) != 2 {
		t.Fatalf("disposals=%d want 2", len(ds))
	}
	// Slice A: 100 × (1.30 − 1.1011) − 0.10 fee share = 19.79
	if !ds[0].Gain.Equal(dec(t, "19.79")) {
		t.Fatalf("slice A gain=%s want 19.79", ds[0].Gain)
	}
	// Slice B: 50 × (1.30 − 1.2012) − 0.05 fee share = 4.89
	if !ds[1].Gain.Equal(dec(t, "4.89")) {
		t.Fatalf("slice B gain=%s want 4.89", ds[1].Gain)
	}
	// Total: proceeds 194.85, basis 170.17 → 24.68.
	s := Summarize(ds)
	if len(s) != 1 || s[0].Currency != "USD" {
		t.Fatalf("summary=%+v", s)
	}
	if !s[0].NetGain.Equal(dec(t, "24.68")) {
		t.Fatalf("net gain=%s want 24.68", s[0].NetGain)
	}
	if s[0].DisposalCount != 2 {
		t.Fatalf("disposal_count=%d", s[0].DisposalCount)
	}
}

// A sell exceeding open longs opens a short lot; the later buy closes it.
func TestComputeShortRoundTrip(t *testing.T) {
	fills := []Fill{
		{TradeID: 1, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "BUY", Quantity: dec0("50"), Price: dec0("1.00"), At: at(2026, 1, 5)},
		{TradeID: 2, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "SELL", Quantity: dec0("100"), Price: dec0("1.20"), At: at(2026, 1, 10)},
		// 50 close long +1.00 gain; 50 open short @1.20.
		{TradeID: 3, InstrumentID: 7, Symbol: "EURUSD", QuoteCurrency: "USD",
			Side: "BUY", Quantity: dec0("50"), Price: dec0("1.10"), At: at(2026, 1, 20)},
		// covers short: 50 × (1.20 − 1.10) = +5
	}
	y0, y1 := year2026()
	ds := Compute(fills, MethodFIFO, y0, y1)
	if len(ds) != 2 {
		t.Fatalf("disposals=%d want 2: %+v", len(ds), ds)
	}
	var dirs []string
	for _, d := range ds {
		dirs = append(dirs, d.Direction)
	}
	if dirs[0] != "LONG_CLOSE" || dirs[1] != "SHORT_CLOSE" {
		t.Fatalf("directions=%v", dirs)
	}
	if got := gains(t, ds); !got.Equal(dec(t, "15")) { // 10 + 5
		t.Fatalf("total gain=%s want 15", got)
	}
}

func TestParseMethod(t *testing.T) {
	for _, in := range []string{"", "FIFO", "LIFO", "HIFO", "AVG_COST"} {
		if _, err := ParseMethod(in); err != nil {
			t.Fatalf("ParseMethod(%q): %v", in, err)
		}
	}
	if _, err := ParseMethod("fifo"); err == nil {
		t.Fatal("lowercase must be rejected (canonical uppercase)")
	}
	if _, err := ParseMethod("SPECIFIC_ID"); err == nil {
		t.Fatal("unknown method accepted")
	}
}

// Service.Report bounds: non-positive account and absurd years fail
// before the source is touched.
func TestReportValidation(t *testing.T) {
	svc, err := NewService(fakeSource{nil})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Report(t.Context(), 0, 2026, MethodFIFO); err == nil {
		t.Fatal("account_id=0 accepted")
	}
	if _, err := svc.Report(t.Context(), 1, 1900, MethodFIFO); err == nil {
		t.Fatal("year 1900 accepted")
	}
	if _, err := NewService(nil); err == nil {
		t.Fatal("nil source accepted")
	}
}

type fakeSource struct{ fills []Fill }

func (f fakeSource) Fills(_ context.Context, _ int64, _ time.Time) ([]Fill, error) {
	return f.fills, nil
}

func TestServiceReport(t *testing.T) {
	svc, err := NewService(fakeSource{threeBuysThenSell()})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Report(t.Context(), 42, 2026, MethodFIFO)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AccountID != 42 || rep.Year != 2026 || rep.Method != MethodFIFO {
		t.Fatalf("report meta=%+v", rep)
	}
	if len(rep.Disposals) != 2 || len(rep.Summary) != 1 {
		t.Fatalf("report=%+v", rep)
	}
}

// ---------------------------------------------------------------------------
// Renderers
// ---------------------------------------------------------------------------

func testReport(t *testing.T) *Report {
	t.Helper()
	svc, err := NewService(fakeSource{threeBuysThenSell()})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Report(t.Context(), 7, 2026, MethodFIFO)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestRenderCSV(t *testing.T) {
	csv := string(RenderCSV(testReport(t)))
	for _, want := range []string{
		"account_id,7", "year,2026", "method,FIFO",
		"symbol,currency,direction,open_trade_id,close_trade_id",
		"currency,disposal_count,total_proceeds,total_cost_basis,net_gain",
		"EURUSD,USD,LONG_CLOSE",
		"USD,2,", // summary row
	} {
		if !strings.Contains(csv, want) {
			t.Fatalf("csv missing %q:\n%s", want, csv)
		}
	}
}

func TestRenderPDF(t *testing.T) {
	pdf := RenderPDF(testReport(t))
	if !strings.HasPrefix(string(pdf), "%PDF-1.4") {
		t.Fatal("missing %PDF header")
	}
	for _, want := range []string{"xref", "trailer", "%%EOF", "/Type /Catalog", "/Type /Page"} {
		if !strings.Contains(string(pdf), want) {
			t.Fatalf("pdf missing %q", want)
		}
	}
	// Empty report still produces a valid document.
	empty := &Report{AccountID: 1, Year: 2026, Method: MethodFIFO,
		GeneratedAt: time.Now()}
	if p := RenderPDF(empty); !strings.HasPrefix(string(p), "%PDF-1.4") {
		t.Fatal("empty report produced invalid pdf")
	}
}
