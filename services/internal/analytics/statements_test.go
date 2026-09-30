// statements_test.go — Phase-20 Task 20.3.6/20.3.7 unit tests (no PG).
// Live-DB coverage lives in statements_pg_test.go (EXC_PG_TEST=1).
package analytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ---- FileStore --------------------------------------------------------

func TestMemFileStoreRoundtrip(t *testing.T) {
	m := NewMemFileStore()
	ref, err := m.Put(context.Background(), "a/b.pdf", []byte("pdf-bytes"), "application/pdf")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	body, err := m.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(body) != "pdf-bytes" {
		t.Fatalf("roundtrip mismatch: %q", body)
	}
	if _, err := m.Get(context.Background(), "missing"); err == nil {
		t.Fatal("expected miss error")
	}
	m.Err = errors.New("store down")
	if _, err := m.Put(context.Background(), "x", nil, ""); err == nil {
		t.Fatal("expected put error")
	}
	if _, err := m.Get(context.Background(), ref); err == nil {
		t.Fatal("expected get error")
	}
}

// ---- period parsing ---------------------------------------------------

func TestParseStatementPeriod(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want StatementPeriod
	}{
		{"", ""}, {"daily", StmtPeriodDaily}, {"DAILY", StmtPeriodDaily},
		{"monthly", StmtPeriodMonthly}, {"MONTHLY", StmtPeriodMonthly},
	} {
		got, err := ParseStatementPeriod(tc.in)
		if err != nil {
			t.Fatalf("period %q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("period %q = %q want %q", tc.in, got, tc.want)
		}
	}
	if _, err := ParseStatementPeriod("WEEKLY"); err == nil {
		t.Fatal("expected invalid period error")
	}
}

// ---- statement rendering ----------------------------------------------

func sampleStatement() *Statement {
	return &Statement{
		AccountID:   42,
		Period:      StmtPeriodDaily,
		PeriodStart: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		GeneratedAt: time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC),
		Currencies: []CurrencyStatement{{
			Currency: "USD",
			Opening:  decimal.RequireFromString("1000"),
			NetMove:  decimal.RequireFromString("250.5"),
			Closing:  decimal.RequireFromString("1250.5"),
			Movements: []LedgerMovement{{
				ID: 7, EntryType: "TRADE_SETTLEMENT", Direction: "DEBIT",
				Amount: decimal.RequireFromString("250.5"), Currency: "USD",
				PostedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
			}},
		}},
		Trades: []StatementTrade{{
			TradeID: 9, Symbol: "EURUSD", Side: "BUY",
			Price:    decimal.RequireFromString("1.0850"),
			Quantity: decimal.RequireFromString("100000"),
			Fee:      decimal.RequireFromString("2.5"), FeeCcy: "USD",
			ExecutedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
		}},
		Pending: []PendingMovement{{
			FundingID: 3, Type: "DEPOSIT", Status: "PENDING",
			Amount: decimal.RequireFromString("500"), Currency: "USD",
			CreatedAt: time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC),
		}},
		Positions: []PositionPnl{{
			Symbol: "EURUSD", Side: "LONG",
			Quantity:      decimal.RequireFromString("100000"),
			UnrealizedPnl: decimal.RequireFromString("-12.34"),
			Currency:      "USD",
		}},
	}
}

func TestRenderStatementCSVFlagsPending(t *testing.T) {
	csv := string(RenderStatementCSV(sampleStatement()))
	for _, want := range []string{
		"account_id,42", "currency,opening_balance",
		"USD,1000.00000000,250.50000000,1250.50000000",
		"pending_movements (FLAGGED", "DEPOSIT,PENDING",
		"open_positions", "EURUSD,LONG",
	} {
		if !strings.Contains(csv, want) {
			t.Fatalf("csv missing %q:\n%s", want, csv)
		}
	}
	// pending movements must not be folded into totals — the closing
	// balance line equals opening+net_movement exactly.
	if !strings.Contains(csv, "1250.50000000") {
		t.Fatal("closing total missing")
	}
}

func TestRenderStatementPDFValid(t *testing.T) {
	pdf := RenderStatementPDF(sampleStatement())
	if !strings.HasPrefix(string(pdf), "%PDF-1.4") {
		t.Fatal("missing pdf magic")
	}
	if !strings.Contains(string(pdf), "%%EOF") {
		t.Fatal("missing EOF marker")
	}
	if !strings.Contains(string(pdf), "PENDING") {
		t.Fatal("pending movement not surfaced in pdf")
	}
}

func TestLedgerMovementSignedAmount(t *testing.T) {
	d := decimal.RequireFromString("10")
	deb := LedgerMovement{Direction: "DEBIT", Amount: d}
	cred := LedgerMovement{Direction: "CREDIT", Amount: d}
	if !deb.SignedAmount().Equal(d) {
		t.Fatal("debit should be positive")
	}
	if !cred.SignedAmount().Equal(d.Neg()) {
		t.Fatal("credit should be negative")
	}
}

// ---- scan coercions ----------------------------------------------------

func TestScanCoercions(t *testing.T) {
	if n, err := scanInt64(int64(7)); err != nil || n != 7 {
		t.Fatalf("scanInt64 int64: %v", err)
	}
	if n, err := scanInt64([]byte("42")); err != nil || n != 42 {
		t.Fatalf("scanInt64 bytes: %v", err)
	}
	if _, err := scanInt64(struct{}{}); err == nil {
		t.Fatal("scanInt64 should reject unsupported type")
	}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got, err := scanTime(ts); err != nil || !got.Equal(ts) {
		t.Fatalf("scanTime: %v", err)
	}
	if d, err := scanDecimal("12.5"); err != nil || !d.Equal(decimal.RequireFromString("12.5")) {
		t.Fatalf("scanDecimal: %v", err)
	}
	if d, err := scanDecimal(3.5); err != nil || !d.Equal(decimal.RequireFromString("3.5")) {
		t.Fatalf("scanDecimal float: %v", err)
	}
}

// ---- invoice rendering -------------------------------------------------

func TestRenderInvoiceDocuments(t *testing.T) {
	inv := &Invoice{
		InvoiceID: 11, AccountID: 42,
		Month:            time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Currency:         "USD",
		TradingFees:      decimal.RequireFromString("120.5"),
		MMRebates:        decimal.Zero, // suspended MM program → zero line
		ConnectivityFees: decimal.RequireFromString("50"),
		Total:            decimal.RequireFromString("170.5"),
		Status:           "ISSUED",
		GeneratedAt:      time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC),
	}
	csv := string(renderInvoiceCSV(inv))
	for _, want := range []string{"trading_fees,120.50000000", "mm_rebates,0.00000000",
		"connectivity_fees,50.00000000", "total,170.50000000"} {
		if !strings.Contains(csv, want) {
			t.Fatalf("invoice csv missing %q:\n%s", want, csv)
		}
	}
	pdf := renderInvoicePDF(inv)
	if !strings.HasPrefix(string(pdf), "%PDF-1.4") ||
		!strings.Contains(string(pdf), "%%EOF") {
		t.Fatal("invoice pdf malformed")
	}
}

// ---- P&L CSV -----------------------------------------------------------

func TestRenderPnLCSV(t *testing.T) {
	pls := []PeriodPnL{{
		Currency: "USD",
		Lines: []FlowLine{
			{AccountCode: "4010_TRADING_FEES_USD", AccountName: "Trading fees",
				AccountType: "REVENUE", Currency: "USD",
				Net: decimal.RequireFromString("100")},
			{AccountCode: "5010_LP_COSTS_USD", AccountName: "LP costs",
				AccountType: "EXPENSE", Currency: "USD",
				Net: decimal.RequireFromString("40")},
		},
		Revenue:   decimal.RequireFromString("100"),
		Expenses:  decimal.RequireFromString("40"),
		NetIncome: decimal.RequireFromString("60"),
	}}
	checks := []ReconCheck{{
		Name: "fee_income", Currency: "USD",
		Expected: decimal.RequireFromString("100"),
		Actual:   decimal.RequireFromString("100"),
		OK:       true,
	}}
	csv := string(RenderPnLCSV(pls,
		time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), checks))
	for _, want := range []string{"report,pnl", "4010_TRADING_FEES_USD",
		"NET_INCOME,,REVENUE_MINUS_EXPENSE,60.00000000",
		"fee_income,USD,100.00000000,100.00000000,0.00000000,true"} {
		if !strings.Contains(csv, want) {
			t.Fatalf("pnl csv missing %q:\n%s", want, csv)
		}
	}
}

// ---- ERP adapters ------------------------------------------------------

func TestRunIDForDeterministic(t *testing.T) {
	d := time.Date(2026, 9, 30, 15, 30, 0, 0, time.UTC)
	if RunIDFor(d) != "erp-20260930" {
		t.Fatalf("unexpected run id %q", RunIDFor(d))
	}
	if RunIDFor(d) != RunIDFor(d.Add(5*time.Hour)) {
		t.Fatal("run id must be stable across the day")
	}
}

func TestSFTPDropAdapterWritesBundle(t *testing.T) {
	dir := t.TempDir()
	a := &SFTPDropAdapter{Dir: dir, Endpoint: "sftp://erp.example/in"}
	fileSum := sha256.Sum256([]byte("csv-body"))
	b := ERPBundle{
		RunID: "erp-20260930", Seq: 7,
		BusinessDate: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		Manifest:     []byte(`{"run_id":"erp-20260930"}`),
		Files: []ERPFile{{
			Name: "journals.csv", ContentType: "text/csv",
			SHA256: hex.EncodeToString(fileSum[:]),
			Body:   []byte("csv-body"),
		}},
	}
	ref, err := a.Deliver(context.Background(), b)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if !strings.HasPrefix(ref, "dir://") {
		t.Fatalf("unexpected ref %q", ref)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "erp-20260930", "journals.csv")); string(got) != "csv-body" {
		t.Fatal("bundle file not written")
	}
	if _, err := os.Stat(filepath.Join(dir, "erp-20260930", "manifest.json")); err != nil {
		t.Fatal("manifest not written")
	}
}

func TestSFTPDropAdapterFailsClosedNoDir(t *testing.T) {
	a := &SFTPDropAdapter{}
	if _, err := a.Deliver(context.Background(), ERPBundle{}); err == nil {
		t.Fatal("expected fail-closed delivery without Dir")
	}
}

func TestWebhookAdapterPostsBundle(t *testing.T) {
	var got webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	a := &WebhookAdapter{URL: srv.URL}
	sum := sha256.Sum256([]byte("body"))
	b := ERPBundle{
		RunID: "erp-20260930", Seq: 3,
		BusinessDate: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		Manifest:     []byte(`{"run_id":"erp-20260930","seq":3}`),
		Files: []ERPFile{{Name: "tb.csv", ContentType: "text/csv",
			SHA256: hex.EncodeToString(sum[:]), Body: []byte("body")}},
	}
	if _, err := a.Deliver(context.Background(), b); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got.RunID != "erp-20260930" || got.Seq != 3 {
		t.Fatalf("payload mismatch: %+v", got)
	}
	sumHex := hex.EncodeToString(sum[:])
	if len(got.Files) != 1 || got.Files[0].SHA256 != sumHex {
		t.Fatal("file checksum not propagated")
	}
}

func TestWebhookAdapterNon2xxFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	a := &WebhookAdapter{URL: srv.URL}
	if _, err := a.Deliver(context.Background(), ERPBundle{Manifest: []byte("{}")}); err == nil {
		t.Fatal("expected non-2xx delivery failure")
	}
}
