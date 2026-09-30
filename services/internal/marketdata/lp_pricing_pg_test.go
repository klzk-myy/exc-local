// Gated: skipped unless EXC_PG_TEST=1 (same convention as
// export_pg_test.go; DSN honored from EXC_TEST_DSN then EXC_PG_DSN,
// defaulting to the dev database).
//
// Run: EXC_PG_TEST=1 go test ./internal/marketdata/ -run IntegrationLPPricing -v
package marketdata

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func lpPricingTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("EXC_PG_DSN")
	}
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func lpPricingHasTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name = $1)`,
		table).Scan(&ok); err != nil {
		t.Fatalf("table probe %s: %v", table, err)
	}
	return ok
}

// ensureLPPricingSchema applies migration 191 when the LP tables are
// absent; baseline anchors (instruments) must already exist — a bare
// scratch DB skips cleanly.
func ensureLPPricingSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if !lpPricingHasTable(t, ctx, pool, "instruments") {
		t.Skip("baseline table instruments missing — target a migrated database")
	}
	if lpPricingHasTable(t, ctx, pool, "liquidity_providers") {
		return
	}
	up, err := os.ReadFile("../db/migrations/191_liquidity_providers.up.sql")
	if err != nil {
		t.Fatalf("read migration 191: %v", err)
	}
	if _, err := pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("apply 191_liquidity_providers.up.sql: %v", err)
	}
}

// IntegrationLPPricingConfigConsumer drives the real persisted config:
// insert LP + instrument config → the marketdata consumer reads it →
// the markup/skew/staleness gates apply to quotes exactly as stored.
func TestIntegrationLPPricingConfigConsumer(t *testing.T) {
	pool := lpPricingTestPool(t)
	ctx := context.Background()
	ensureLPPricingSchema(t, ctx, pool)

	var instrID int64
	var sym string
	if err := pool.QueryRow(ctx,
		`SELECT id, symbol FROM instruments WHERE symbol IS NOT NULL
		 ORDER BY id LIMIT 1`).Scan(&instrID, &sym); err != nil {
		t.Skipf("no instrument seeded: %v", err)
	}

	var lpID int64
	name := fmt.Sprintf("lp-pricing-test-%d", time.Now().UnixNano())
	if err := pool.QueryRow(ctx, `
		INSERT INTO liquidity_providers
		    (name, status, connection_type, staleness_timeout_ms)
		VALUES ($1, 'ACTIVE', 'REST', 7000) RETURNING lp_id`, name).
		Scan(&lpID); err != nil {
		t.Fatalf("insert lp: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM liquidity_providers WHERE lp_id = $1`, lpID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO lp_instrument_configs
		    (lp_id, instrument_id, enabled, spread_markup_bid_bps,
		     spread_markup_ask_bps, skew_bps, staleness_timeout_ms)
		VALUES ($1, $2, true, 5, 10, 2, 250)`, lpID, instrID); err != nil {
		t.Fatalf("insert lp config: %v", err)
	}

	// The consumer reads the persisted rows verbatim.
	src := NewPgxLPConfigSource(pool)
	cfgs, err := src.LPConfigs(ctx)
	if err != nil {
		t.Fatalf("LPConfigs: %v", err)
	}
	var found *LPPricingConfig
	for i := range cfgs {
		if cfgs[i].LPID == lpID && cfgs[i].InstrumentID == instrID {
			found = &cfgs[i]
		}
	}
	if found == nil {
		t.Fatalf("inserted config not returned: %+v", cfgs)
	}
	if found.Status != "ACTIVE" || !found.Enabled || found.Symbol != sym ||
		found.LPStalenessMS != 7000 || found.StalenessTimeoutMS != 250 ||
		found.SpreadMarkupBidBps.String() != "5" ||
		found.SpreadMarkupAskBps.String() != "10" ||
		found.SkewBps.String() != "2" {
		t.Fatalf("persisted config = %+v", found)
	}

	// Filter reload → quote flows through the stored markup/skew.
	filter := NewLPPriceFilter(src, nil)
	if err := filter.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	now := time.Now()
	q := LPQuote{
		LPID: lpID, InstrumentID: instrID, Ts: now,
		Bids: []LPQuoteLevel{{Price: lpDecForTest("1.10000"), Qty: lpDecForTest("1")}},
		Asks: []LPQuoteLevel{{Price: lpDecForTest("1.20000"), Qty: lpDecForTest("1")}},
	}
	adj, cfg, reason := filter.Apply(q, now)
	if reason != "" {
		t.Fatalf("fresh quote dropped: %s", reason)
	}
	// bid′ = 1.1 × (1 + (2+5)/1e4) = 1.10077; ask′ = 1.2 × (1 + (2−10)/1e4)
	//      = 1.2 × 0.9992 = 1.19904
	if adj.Bids[0].Price.String() != "1.10077" {
		t.Fatalf("adjusted bid = %s, want 1.10077", adj.Bids[0].Price)
	}
	if adj.Asks[0].Price.String() != "1.19904" {
		t.Fatalf("adjusted ask = %s, want 1.19904", adj.Asks[0].Price)
	}
	if adj.Symbol != sym {
		t.Fatalf("symbol = %q, want %q", adj.Symbol, sym)
	}
	_ = cfg

	// Per-instrument staleness timeout (250ms) is honored over the LP
	// default (7000ms).
	q.Ts = now.Add(-300 * time.Millisecond)
	if _, _, r := filter.Apply(q, now); r != LPDropStale {
		t.Fatalf("300ms vs 250ms instrument gate = %q, want stale", r)
	}
	q.Ts = now.Add(-3 * time.Second)
	if _, _, r := filter.Apply(q, now); r != LPDropStale {
		t.Fatalf("3s vs 250ms gate = %q, want stale", r)
	}

	// Suspending the LP withdraws distribution after a reload — config
	// changes propagate without restart.
	if _, err := pool.Exec(ctx,
		`UPDATE liquidity_providers SET status = 'SUSPENDED' WHERE lp_id = $1`,
		lpID); err != nil {
		t.Fatalf("suspend lp: %v", err)
	}
	if err := filter.Reload(ctx); err != nil {
		t.Fatalf("reload after suspend: %v", err)
	}
	if _, _, r := filter.Apply(LPQuote{
		LPID: lpID, InstrumentID: instrID, Ts: now,
	}, now); r != LPDropLPInactive {
		t.Fatalf("suspended lp = %q, want lp_inactive", r)
	}
}
