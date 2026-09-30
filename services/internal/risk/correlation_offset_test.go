// correlation_offset_test.go — Task 19.3.18 correlation-offset tests.
// Coverage: 90-day matrix computation, |ρ|>0.7 offset through the
// PORTFOLIO margin path, sign-aware hedges, configurable per-pair
// factor (≤0.8), 20%-of-gross regulatory floor, portfolio-only scoping,
// audit trail + bounded Redis persistence.
package risk

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"testing"

	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type retSeriesFake struct {
	series map[string][]float64
	err    error
}

func (f retSeriesFake) DailyReturns(_ context.Context, sym string, _ int) ([]float64, error) {
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.series[sym]; ok {
		return r, nil
	}
	if r, ok := f.series[normalizeCorrSymbol(sym)]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("no series for %s", sym)
}

type corrAuditMem struct {
	mu  sync.Mutex
	evs []CorrelationAuditEvent
}

func (a *corrAuditMem) AuditOffset(_ context.Context, ev CorrelationAuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evs = append(a.evs, ev)
	return nil
}
func (a *corrAuditMem) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.evs)
}

// testSeries builds positively / negatively correlated return sets.
var (
	seriesA = []float64{0.001, -0.002, 0.003, -0.001, 0.002, -0.003, 0.001, -0.002, 0.0025, -0.0015}
	seriesB = []float64{0.0012, -0.0021, 0.0031, -0.0009, 0.0018, -0.0032, 0.0011, -0.0019, 0.0026, -0.0014}
	seriesC = []float64{-0.001, 0.002, -0.003, 0.001, -0.002, 0.003, -0.001, 0.002, -0.0025, 0.0015}
	seriesD = []float64{0.0005, 0.0009, -0.0011, 0.0002, -0.0007, 0.0008, -0.0004, 0.0006, 0.001, -0.0009}
)

func newMatrix(t *testing.T, audit CorrelationAuditSink) *CorrelationMatrix {
	t.Helper()
	m := NewCorrelationMatrix(CorrelationMatrixDeps{
		Source: retSeriesFake{series: map[string][]float64{
			"EURUSD": seriesA, "GBPUSD": seriesB,
			"USDCHF": seriesC, "AUDUSD": seriesD,
		}},
		Audit: audit,
	})
	if err := m.Refresh(context.Background(),
		[]string{"EUR/USD", "GBP/USD", "USD/CHF", "AUD/USD"}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Matrix computation
// ---------------------------------------------------------------------------

func TestCorrelationMatrixRefreshAndOffset(t *testing.T) {
	m := newMatrix(t, nil)
	// EUR/USD & GBP/USD strongly positively correlated → +ρ×10⁴.
	rho, ok := m.Offset("EUR/USD", "GBP/USD")
	if !ok || rho < 9000 {
		t.Fatalf("EURUSD|GBPUSD rho_bps = %v ok=%v, want ≈ +10000", rho, ok)
	}
	// EUR/USD vs USD/CHF — the natural-hedge pair → −ρ.
	rho, ok = m.Offset("EURUSD", "USDCHF")
	if !ok || rho > -9000 {
		t.Fatalf("EURUSD|USDCHF rho_bps = %v ok=%v, want ≈ −10000", rho, ok)
	}
	// Ordering/symbol-form agnostic.
	rho2, ok := m.Offset("CHFUSD", "EURUSD")
	_ = rho2
	_, _ = ok, rho2
	if r2, ok2 := m.Offset("USD/CHF", "EUR/USD"); !ok2 || math.Abs(r2-rho) > 1 {
		t.Fatalf("order-agnostic Offset mismatch: %v vs %v", r2, rho)
	}
	// Unknown pair → no estimate.
	if _, ok := m.Offset("EUR/USD", "NZD/JPY"); ok {
		t.Fatal("uncomputed pair must report no estimate")
	}
}

func TestCorrelationGroups(t *testing.T) {
	m := newMatrix(t, nil)
	groups := m.Groups()
	// EURUSD+GBPUSD+USDCHF collapse into one |ρ|>0.7 component; AUDUSD
	// (low correlation) stays out.
	if len(groups) != 1 {
		t.Fatalf("groups = %v, want one component", groups)
	}
	if len(groups[0]) != 3 {
		t.Fatalf("group members = %v, want 3", groups[0])
	}
}

func TestCorrelationFactorClampAndDefault(t *testing.T) {
	m := newMatrix(t, nil)
	f, ok := m.OffsetFactor("EUR/USD", "GBP/USD")
	if !ok || f != 0.5 {
		t.Fatalf("default factor = %v, want 0.5", f)
	}
	if err := m.SetFactor(context.Background(), "EURUSD", "GBPUSD", 0.9); err == nil {
		t.Fatal("factor > 0.8 must reject")
	}
	if err := m.SetFactor(context.Background(), "EURUSD", "GBPUSD", 0); err == nil {
		t.Fatal("factor 0 must reject")
	}
	if err := m.SetFactor(context.Background(), "EURUSD", "GBPUSD", 0.7); err != nil {
		t.Fatal(err)
	}
	if f, _ := m.OffsetFactor("EUR/USD", "GBP/USD"); f != 0.7 {
		t.Fatalf("override factor = %v, want 0.7", f)
	}
}

func TestCorrelationAuditPerEpoch(t *testing.T) {
	audit := &corrAuditMem{}
	m := newMatrix(t, audit)
	// Same epoch: two Offset calls on the pair audit once.
	m.Offset("EUR/USD", "GBP/USD")
	m.Offset("EUR/USD", "GBP/USD")
	if audit.count() != 1 {
		t.Fatalf("per-epoch dedupe: audit = %d, want 1", audit.count())
	}
	// A new matrix epoch audits the fresh computation.
	if err := m.Refresh(context.Background(),
		[]string{"EUR/USD", "GBP/USD", "USD/CHF", "AUD/USD"}); err != nil {
		t.Fatal(err)
	}
	m.Offset("EUR/USD", "GBP/USD")
	if audit.count() != 2 {
		t.Fatalf("new epoch must re-audit, got %d", audit.count())
	}
}

// ---------------------------------------------------------------------------
// Portfolio-margin integration through MarginService
// ---------------------------------------------------------------------------

func portfolioFixture() (*marginStoreFake, markCacheFake) {
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModePortfolio, Status: "NORMAL"},
		category: CategoryProfessional,
		balances: []BalanceAmount{{Currency: "USD", Available: d("1000000")}},
		positions: []MarginPosition{
			{ID: 1, Symbol: "EUR/USD", Side: "LONG", Quantity: d("100000"),
				EntryPrice: d("1.2000"), MarginUsed: d("4000"), QuoteCurrency: "USD"},
			{ID: 2, Symbol: "USD/CHF", Side: "LONG", Quantity: d("100000"),
				EntryPrice: d("0.9000"), MarginUsed: d("4000"), QuoteCurrency: "USD"},
		},
	}
	marks := markCacheFake{m: map[string]decimal.Decimal{
		"EUR/USD": d("1.2000"), "USD/CHF": d("0.9000"),
	}}
	return store, marks
}

func TestPortfolioMarginCorrelationOffset(t *testing.T) {
	store, marks := portfolioFixture()
	// Engine-facing contract: Offset returns ρ×10⁴. EUR/USD × USD/CHF
	// instrument correlation −0.9 → position-return ρ −0.9 (both LONG).
	corr := CorrelationFunc(func(a, b string) (float64, bool) {
		return -9000, true
	})
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks, Correlation: corr})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	// gross = 8000; offset = min(4000,4000) × 0.9 × 0.5 = 1800 → 6200.
	if !snap.UsedMargin.Equal(d("6200")) {
		t.Fatalf("used margin = %s, want 6200 (hedge offset applied)", snap.UsedMargin)
	}
}

func TestPortfolioMarginNoOffsetInCross(t *testing.T) {
	store, marks := portfolioFixture()
	store.account.Mode = ModeCross
	corr := CorrelationFunc(func(a, b string) (float64, bool) { return -9000, true })
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks, Correlation: corr})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UsedMargin.Equal(d("8000")) {
		t.Fatalf("CROSS must not offset, used = %s, want 8000", snap.UsedMargin)
	}
}

// corrFactorFake implements Offset + OffsetFactor for floor/cap tests.
type corrFactorFake struct {
	rho    float64
	factor float64
}

func (f corrFactorFake) Offset(_, _ string) (float64, bool)       { return f.rho, true }
func (f corrFactorFake) OffsetFactor(_, _ string) (float64, bool) { return f.factor, true }

func TestPortfolioMarginRegulatoryFloor(t *testing.T) {
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModePortfolio},
		category: CategoryProfessional,
		balances: []BalanceAmount{{Currency: "USD", Available: d("1000000")}},
		positions: []MarginPosition{
			{ID: 1, Symbol: "A/USD", Side: "LONG", Quantity: d("1"),
				EntryPrice: d("1"), MarginUsed: d("4000"), QuoteCurrency: "USD"},
			{ID: 2, Symbol: "B/USD", Side: "LONG", Quantity: d("1"),
				EntryPrice: d("1"), MarginUsed: d("4000"), QuoteCurrency: "USD"},
			{ID: 3, Symbol: "C/USD", Side: "LONG", Quantity: d("1"),
				EntryPrice: d("1"), MarginUsed: d("4000"), QuoteCurrency: "USD"},
		},
	}
	marks := markCacheFake{m: map[string]decimal.Decimal{
		"A/USD": d("1"), "B/USD": d("1"), "C/USD": d("1"),
	}}
	// All pairs ρ=−1 with per-pair factor 0.8 → pair credits
	// 3×(4000×1.0×0.8)=9600 hit the 80%-of-gross cap exactly, and the
	// 20%-of-gross floor binds the net at 2400.
	corr := corrFactorFake{rho: -10000, factor: 0.8}
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks, Correlation: corr})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UsedMargin.Equal(d("2400")) {
		t.Fatalf("used = %s, want 2400 (20%% regulatory floor)", snap.UsedMargin)
	}
}

func TestPortfolioMarginRealMatrixBinding(t *testing.T) {
	store, marks := portfolioFixture()
	matrix := newMatrix(t, &corrAuditMem{})
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks, Correlation: matrix})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	// ρ(EURUSD,USDCHF) ≈ −1 → offset ≈ min×~1×0.5 ≈ 2000 → used ≈ 6000.
	if snap.UsedMargin.GreaterThan(d("6200")) || snap.UsedMargin.LessThan(d("5800")) {
		t.Fatalf("used = %s, want ≈6000 with real matrix binding", snap.UsedMargin)
	}
}

// ---------------------------------------------------------------------------
// Gated Redis integration — persisted matrix + bounded audit trail
// ---------------------------------------------------------------------------

// TestRedisCorrelationMatrix proves Refresh persists margin:corr:* keys,
// a cold matrix recovers via LoadPersisted, and the audit list captures
// the computation trail. Run:
// EXC_REDIS_TEST=1 go test ./internal/risk/ -run TestRedisCorrelation -v
func TestRedisCorrelationMatrix(t *testing.T) {
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14)
	ctx := context.Background()
	if err := rdb.Ping(ctx); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("redis flushdb: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	m := NewCorrelationMatrix(CorrelationMatrixDeps{
		Source: retSeriesFake{series: map[string][]float64{
			"EURUSD": seriesA, "GBPUSD": seriesB, "USDCHF": seriesC,
		}},
		Redis: rdb.Client,
		Audit: NewRedisCorrelationAudit(rdb.Client),
	})
	if err := m.Refresh(ctx, []string{"EUR/USD", "GBP/USD", "USD/CHF"}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, err := rdb.Get(ctx, CorrelationKey("EURUSD", "GBPUSD")).Float64()
	if err != nil {
		t.Fatalf("persisted pair read: %v", err)
	}
	if got < 0.9 {
		t.Fatalf("persisted rho = %v, want ≈1", got)
	}
	if _, err := rdb.Get(ctx, CorrelationComputedAtKey).Result(); err != nil {
		t.Fatalf("watermark missing: %v", err)
	}
	if err := m.SetFactor(ctx, "EURUSD", "GBPUSD", 0.6); err != nil {
		t.Fatal(err)
	}
	if v, err := rdb.Get(ctx, CorrelationFactorKey("EURUSD", "GBPUSD")).Float64(); err != nil || v != 0.6 {
		t.Fatalf("factor persist = %v %v, want 0.6", v, err)
	}
	// Audit lands in the bounded list once per epoch per pair.
	m.Offset("EUR/USD", "GBP/USD")
	m.Offset("EUR/USD", "GBP/USD")
	n, err := rdb.LLen(ctx, CorrelationAuditKey).Result()
	if err != nil || n != 1 {
		t.Fatalf("audit list len = %d %v, want 1", n, err)
	}
	// Cold recovery.
	cold := NewCorrelationMatrix(CorrelationMatrixDeps{Redis: rdb.Client})
	if err := cold.LoadPersisted(ctx); err != nil {
		t.Fatalf("load persisted: %v", err)
	}
	rho, ok := cold.Offset("GBP/USD", "EUR/USD")
	if !ok || rho < 9000 {
		t.Fatalf("cold Offset = %v ok=%v, want ≈10000", rho, ok)
	}
	if f, _ := cold.OffsetFactor("EURUSD", "GBPUSD"); f != 0.6 {
		t.Fatalf("cold factor = %v, want 0.6", f)
	}
}
