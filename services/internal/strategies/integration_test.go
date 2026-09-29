// PG-gated integration coverage for Task 16.3.21 — runs only when
// EXC_PG_TEST=1:
//
//	EXC_PG_TEST=1 EXC_TEST_DSN='postgres://...' \
//	    go test ./internal/strategies/ -run TestIT -v
//
// The order pipeline / read model / balances / USD converter are fakes;
// the store is real PostgreSQL against the true 077 schema and the
// session calendar is the real DST-aware instruments.SessionCalendar.
package strategies

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/funding"
	"exchange/internal/instruments"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func itPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	schema := fmt.Sprintf("strat_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

// fixtureDDL: upstream tables 077/009/010 lean on. accounts.id is the
// strategy/template FK target; orders exists for completeness though
// run legs reference order ids loosely (BIGINT[], no FK).
const fixtureDDL = `
CREATE TABLE accounts (
    id      BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    status  VARCHAR(12) NOT NULL DEFAULT 'ACTIVE'
);
`

func itSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, fixtureDDL); err != nil {
		t.Fatalf("fixture ddl: %v", err)
	}
	for _, m := range []string{
		"009_create_audit_hash_chain.up.sql",
		"010_create_admin_audit_log.up.sql",
		"077_recurring_rebalancing_strategies.up.sql",
	} {
		body, err := os.ReadFile(filepath.Join("..", "db", "migrations", m))
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("exec %s: %v", m, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, user_id) VALUES (10,1),(20,2)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// ---- fakes ---------------------------------------------------------------

type fakePipe struct {
	mu        sync.Mutex
	nextID    int64
	insts     map[string]*orders.Instrument
	submitted []*orders.SubmitRequest
	cancelled []int64
	submitErr error
	acct      *orders.Account
}

func (f *fakePipe) Submit(_ context.Context, _ *orders.Account,
	req *orders.SubmitRequest) (*orders.Ack, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	f.nextID++
	f.submitted = append(f.submitted, req)
	return &orders.Ack{OrderID: f.nextID, Status: "ACTIVE"}, nil
}

func (f *fakePipe) Cancel(_ context.Context, _ *orders.Account, orderID int64,
	_, _, _ string) (*orders.Ack, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, orderID)
	return &orders.Ack{OrderID: orderID, Status: "CANCELLED"}, nil
}

func (f *fakePipe) AccountByID(_ context.Context, id int64) (*orders.Account, error) {
	if f.acct != nil && f.acct.ID == id {
		return f.acct, nil
	}
	return nil, nil
}

func (f *fakePipe) InstrumentBySymbol(_ context.Context,
	symbol string) (*orders.Instrument, error) {

	return f.insts[symbol], nil
}

type fakeRM struct {
	ref *decimal.Decimal
}

func (f *fakeRM) ReferencePrice(_ context.Context, _ int64) (*decimal.Decimal, error) {
	return f.ref, nil
}

func (f *fakeRM) AvailableBalance(_ context.Context, _ int64,
	_ string) (*decimal.Decimal, error) {

	return nil, nil
}

func (f *fakeRM) GetOrder(_ context.Context, orderID int64) (*orders.Order, error) {
	if orderID == 0 {
		return nil, nil
	}
	// Fakes report submitted orders as live (non-terminal) so runs stay
	// SUBMITTED until a terminal state is simulated.
	return &orders.Order{ID: orderID, Status: "ACTIVE"}, nil
}

type sessStub struct{ cal *instruments.SessionCalendar }

func (s sessStub) Calendar() *instruments.SessionCalendar { return s.cal }

type balStub struct{ rows []funding.BalanceRow }

func (b balStub) BalancesFor(_ context.Context,
	_ int64) ([]funding.BalanceRow, error) {

	return b.rows, nil
}

type usdStub struct{ rates map[string]string }

func (u usdStub) ToUSD(_ context.Context, ccy string,
	amount decimal.Decimal) (decimal.Decimal, error) {

	r, ok := u.rates[ccy]
	if !ok {
		return decimal.Zero, fmt.Errorf("no rate for %s", ccy)
	}
	return amount.Mul(decimal.RequireFromString(r)), nil
}

var (
	// 2026-10-03 is a Saturday — venue dark (WEEKEND).
	marketClosedAt = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	// 2026-10-07 15:00Z is Wednesday 11:00 ET — venue open.
	marketOpenAt = time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
)

func itSvc(t *testing.T, pool *pgxpool.Pool, now time.Time,
	pipe *fakePipe, bals BalanceLister) *Service {

	t.Helper()
	cal, err := instruments.NewSessionCalendar()
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}
	if pipe == nil {
		pipe = &fakePipe{
			insts: map[string]*orders.Instrument{
				"EUR/USD": {ID: 1, Symbol: "EUR/USD",
					BaseCurrency: "EUR", QuoteCurrency: "USD",
					Status: "ACTIVE"},
			},
			acct: &orders.Account{ID: 10, UserID: 1, Type: "SPOT", Status: "ACTIVE"},
		}
	}
	svc, err := NewService(Options{
		Store:    NewStore(pool),
		Orders:   pipe,
		Read:     &fakeRM{ref: decPtrV(t, "1.10")},
		Sessions: sessStub{cal},
		Balances: bals,
		USD:      usdStub{rates: map[string]string{"USD": "1", "EUR": "1.10"}},
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc
}

func decPtrV(t *testing.T, s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

// IT: a due recurring slot while the venue is closed records an honest
// SKIPPED run (MARKET_CLOSED) and the schedule advances — never a
// fabricated execution.
func TestITRecurringMarketClosedSkip(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	svc := itSvc(t, pool, marketClosedAt, nil, nil)

	st, err := svc.Create(ctx, 10, CreateInput{
		Kind:         KindRecurringConversion,
		FromCurrency: "EUR", ToCurrency: "USD",
		Amount: "100", Schedule: ScheduleDaily,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force the slot due now (Saturday).
	if _, err := pool.Exec(ctx,
		`UPDATE strategies SET next_run_at=$1 WHERE strategy_id=$2`,
		marketClosedAt.Add(-time.Hour), st.StrategyID); err != nil {
		t.Fatalf("force due: %v", err)
	}
	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	det, err := svc.Detail(ctx, 10, st.StrategyID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(det.Runs) != 1 {
		t.Fatalf("runs %d", len(det.Runs))
	}
	r := det.Runs[0]
	if r.Status != RunSkipped || r.SkipReason != "MARKET_CLOSED" {
		t.Fatalf("run %+v", r)
	}
	if len(r.OrderIDs) != 0 {
		t.Fatalf("orders submitted while closed: %v", r.OrderIDs)
	}
	// Rescheduled, not stuck on the missed slot.
	if det.Strategy.NextRunAt == nil ||
		!det.Strategy.NextRunAt.After(r.ScheduledFor) {
		t.Fatalf("next_run_at %v scheduled_for %v",
			det.Strategy.NextRunAt, r.ScheduledFor)
	}
	// A second sweep must not refire the consumed slot.
	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	det2, _ := svc.Detail(ctx, 10, st.StrategyID)
	if len(det2.Runs) != 1 {
		t.Fatalf("double-fired: %d runs", len(det2.Runs))
	}
}

// IT: drift beyond the band opens a rebalance run; an underweight
// currency produces a BUY_DEFICIT leg through the order pipeline.
func TestITRebalanceDriftTrigger(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	pipe := &fakePipe{
		insts: map[string]*orders.Instrument{
			"EUR/USD": {ID: 1, Symbol: "EUR/USD",
				BaseCurrency: "EUR", QuoteCurrency: "USD", Status: "ACTIVE"},
		},
		acct: &orders.Account{ID: 10, UserID: 1, Type: "SPOT", Status: "ACTIVE"},
	}
	bals := balStub{rows: []funding.BalanceRow{
		{Currency: "USD", Total: decimal.RequireFromString("780")},
		{Currency: "EUR", Total: decimal.RequireFromString("200")}, // 220 USD
	}}
	svc := itSvc(t, pool, marketOpenAt, pipe, bals)

	st, err := svc.Create(ctx, 10, CreateInput{
		Kind:         KindRebalance,
		Targets:      map[string]string{"USD": "0.5", "EUR": "0.5"},
		DriftBandPct: "5",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	det, err := svc.Detail(ctx, 10, st.StrategyID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(det.Runs) != 1 {
		t.Fatalf("runs %d", len(det.Runs))
	}
	r := det.Runs[0]
	if r.Status != RunSubmitted || r.Kind != KindRebalance {
		t.Fatalf("run %+v", r)
	}
	var eurLeg *RunLeg
	for i := range r.Legs {
		if r.Legs[i].Currency == "EUR" {
			eurLeg = &r.Legs[i]
		}
	}
	if eurLeg == nil || eurLeg.Action != "BUY_DEFICIT" ||
		eurLeg.Status != "PLACED" || eurLeg.OrderID == nil {
		t.Fatalf("eur leg %+v", eurLeg)
	}
	// EUR deficit: target 0.5×1000=500 USD vs actual 220 → 280 USD buy.
	if len(pipe.submitted) != 1 ||
		pipe.submitted[0].Side != "BUY" ||
		pipe.submitted[0].Symbol != "EUR/USD" {
		t.Fatalf("submitted %+v", pipe.submitted)
	}
	if pipe.submitted[0].QuoteQuantity == nil ||
		!pipe.submitted[0].QuoteQuantity.Equal(decimal.RequireFromString("280")) {
		t.Fatalf("quote qty %+v", pipe.submitted[0].QuoteQuantity)
	}
	// Within-band drift opens nothing once the submitted run settles.
	if _, err := pool.Exec(ctx,
		`UPDATE strategy_runs SET status='COMPLETED', completed_at=now()
		 WHERE strategy_id=$1`, st.StrategyID); err != nil {
		t.Fatal(err)
	}
	svc.balances = balStub{rows: []funding.BalanceRow{
		{Currency: "USD", Total: decimal.RequireFromString("490")},
		{Currency: "EUR", Total: decimal.RequireFromString("464")}, // 510.4 USD
	}}
	if err := svc.evaluateRebalance(ctx, st, marketOpenAt); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	det3, _ := svc.Detail(ctx, 10, st.StrategyID)
	if len(det3.Runs) != 1 {
		t.Fatalf("runs after in-band drift: %d", len(det3.Runs))
	}
}

// IT: the marketplace gate — PENDING templates refuse instantiation;
// approve → instantiate copies config only (no code fields exist); a
// smuggled key fails publish outright.
func TestITTemplateApprovalCopySemantics(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	svc := itSvc(t, pool, marketOpenAt, nil, nil)

	// Executable-code smuggle vector: unknown config key → rejected.
	_, err := svc.PublishTemplate(ctx, 10, TemplatePublishInput{
		Name: "evil", Kind: KindRebalance,
		Config: []byte(`{"script":"rm -rf /","targets":{"USD":"1"},"drift_band_pct":"5"}`),
	})
	var e *excerrors.Error
	if err == nil || !errors.As(err, &e) || e.Code != CodeStrategyConfigInvalid {
		t.Fatalf("smuggle publish: %v", err)
	}

	tpl, err := svc.PublishTemplate(ctx, 10, TemplatePublishInput{
		Name: "usd-eur split", Kind: KindRebalance,
		Config: []byte(`{"targets":{"USD":"0.5","EUR":"0.5"},"drift_band_pct":"5","label":"half-half"}`),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if tpl.Status != TemplatePending {
		t.Fatalf("status %s", tpl.Status)
	}
	// Not approved → instantiate refused.
	if _, err := svc.Instantiate(ctx, 20, tpl.TemplateID); err == nil {
		t.Fatal("instantiated a PENDING template")
	} else if errors.As(err, &e) && e.Code != CodeTemplateNotApproved {
		t.Fatalf("code %s", e.Code)
	}
	// Approve as admin (writes admin_audit_log + hash chain in-tx).
	got, err := svc.DecideTemplate(ctx, tpl.TemplateID, true, 99, "127.0.0.1", "")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.Status != TemplateApproved {
		t.Fatalf("status %s", got.Status)
	}
	var audits int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log
		 WHERE action LIKE 'strategy_template.%'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("audit rows %d", audits)
	}
	// Instantiate on a different account: config copied by value.
	st, err := svc.Instantiate(ctx, 20, tpl.TemplateID)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	if st.AccountID != 20 || st.TemplateID == nil ||
		*st.TemplateID != tpl.TemplateID {
		t.Fatalf("strategy %+v", st)
	}
	if st.Targets["USD"].String() != "0.5" || st.Targets["EUR"].String() != "0.5" {
		t.Fatalf("targets %+v", st.Targets)
	}
	if st.DriftBandPct == nil || st.DriftBandPct.String() != "5" {
		t.Fatalf("band %+v", st.DriftBandPct)
	}
	if st.Label != "half-half" {
		t.Fatalf("label %q", st.Label)
	}
	// Mutating the template afterwards can never rewrite the strategy
	// (copy-by-value) — retire it and confirm the strategy stands.
	if _, err := pool.Exec(ctx,
		`UPDATE strategy_templates SET status='RETIRED', config='{}'::jsonb
		 WHERE template_id=$1`, tpl.TemplateID); err != nil {
		t.Fatal(err)
	}
	det, err := svc.Detail(ctx, 20, st.StrategyID)
	if err != nil {
		t.Fatal(err)
	}
	if det.Strategy.Targets["USD"].String() != "0.5" {
		t.Fatalf("template mutation leaked: %+v", det.Strategy.Targets)
	}
}
