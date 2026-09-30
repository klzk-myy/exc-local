// Command analytics is the Phase-20 ClickHouse ingest daemon
// (Tasks 20.3.1 + 20.3.11, spec §16.1/§16.6, §24 #65/#68/#322).
//
// Boot sequence: shared config (logging, nats urls, postgres dsn) ->
// ClickHouse native dial -> disk spool open -> JetStream connect ->
// durable pull consumers on `trades` + `analytics` -> loops:
//
//	consumer(trades)    TradeFill -> ticks + trades (ack after durable)
//	consumer(analytics) OrderNew -> fill-rate denominator metric
//	Ingester.Recover    pings CH, drains the spool in 10k paced batches
//	Ingester.PollIncome PG ledger_entries -> income_ledger (60s cadence)
//
// Env: EXC_CONFIG / config.yaml (postgres.dsn, nats.urls, logging),
// EXC_CH_URL or EXC_CH_NATIVE_URL (+ EXC_CH_USER / EXC_CH_PASSWORD /
// EXC_CH_DATABASE), EXC_CH_SPOOL_DIR (default
// /var/spool/exchange/clickhouse_buffer).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/analytics"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/marketdata/ohlcv"
	excnats "exchange/internal/nats"
	"exchange/internal/utils"
	"exchange/pkg/decimal"
	"exchange/pkg/logging"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "analytics: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := flag.NewFlagSet("analytics", flag.ExitOnError)
	metricsAddr := fs.String("metrics-addr", "127.0.0.1:9200",
		"/metrics + /healthz listen address; empty disables")
	durable := fs.String("durable", "ch-etl",
		"JetStream durable name (per-stream suffix appended)")
	noIncome := fs.Bool("no-income-poll", false,
		"disable the PG->income_ledger projection")
	_ = fs.Parse(argv)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level, _ := logging.ParseLevel(cfg.Logging.Level)
	log, err := logging.New(level, cfg.Logging.Format)
	if err != nil {
		return err
	}

	ctx, stop := utils.SignalContext()
	defer stop()

	// ClickHouse — the spool exists precisely so ingest survives a dead
	// cluster, so a failed boot dial is NOT fatal: connRef stays nil and
	// reDialLoop re-dials in the background.
	chCfg := analytics.ConfigFromEnv()
	connRef := &connHolder{}
	if conn, err := analytics.Dial(ctx, chCfg); err != nil {
		log.Warn("clickhouse dial failed at boot; running spool-only until recover",
			"addr", chCfg.Addr, "err", err)
	} else {
		connRef.set(conn)
	}
	defer func() { _ = connRef.Close() }()

	opts := analytics.OptionsFromEnv()
	spool, err := analytics.OpenSpool(opts.SpoolDir, opts.SpoolMaxBytes)
	if err != nil {
		return err
	}
	defer spool.Close()

	metrics := analytics.NewIngestMetrics()
	ing := analytics.NewIngester(connRef, spool, opts, metrics, log)

	// PostgreSQL — income_ledger projection source and the fx_klines hot
	// store for the candle engine below. Optional so the pure tick path
	// can run without OLTP (e.g. load tests).
	var pgQuerier analytics.PGQuerier
	var pgPool *pgxpool.Pool
	if !*noIncome && cfg.Postgres.DSN != "" {
		p, err := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
		if err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		defer p.Close()
		pgQuerier = p
		pgPool = p
	}

	// NATS JetStream — cold-path backbone.
	urls := cfg.NATS.URLList()
	ncfg := excnats.DefaultConfig(urls)
	ncfg.Name = "analytics-etl"
	nc, err := excnats.Connect(ctx, ncfg, log)
	if err != nil {
		return err
	}
	defer nc.Close()

	// Task 20.3.3 — the Phase-6 candle engine (spec §10.3/§16.2) lives in
	// this daemon: decoded TradeFills fan out through the consumer's
	// OnTradeFill hook into the in-memory aggregator; PGStore writes the
	// fx_klines hot read model (open bars included) and CHArchiveSink
	// mirrors each closed bar into the 12 ohlcv_* SummingMergeTree tables
	// (5-year TTL). connHolder satisfies analytics.Conn, so a CH outage
	// degrades to PG-only while archive errors stay non-fatal counters on
	// the engine's metrics. The wire fill carries no taker-side flag —
	// TakerBuyVolume stays zero rather than publishing a guessed side.
	// A crash can lose an as-yet-unflushed open-bucket delta; the CH
	// projection is rebuildable from `ticks` (documented honest seam).
	candleDeps := ohlcv.Deps{
		Archive: ohlcv.NewCHArchiveSink(analytics.NewOHLCVStore(connRef)),
	}
	if pgPool != nil {
		candleDeps.Store = ohlcv.NewPGStore(pgPool)
	} else {
		log.Warn("postgres disabled — candle engine runs without the fx_klines hot store")
	}
	candle := ohlcv.NewEngine(ohlcv.Config{Logger: log}, candleDeps)

	cons := analytics.NewConsumer(nc, ing, analytics.ConsumerOptions{
		OnTradeFill: func(symbol string, tradeID, seq uint64, priceE8, qtyE8 int64, ts time.Time) {
			candle.HandleTrade(ctx, ohlcv.TradeEvent{
				TradeID:  tradeID,
				Symbol:   symbol, // canonical "EUR/USD" — parseSubject maps the token
				Price:    decimal.NewFromScaled(priceE8),
				Quantity: decimal.NewFromScaled(qtyE8),
				Seq:      seq,
				Ts:       ts,
			})
		},
	}, metrics, log)

	// Wall-clock finalize — Run's Source loop is unused (fills arrive via
	// the hook), so Advance ticks here on the engine's own cadence.
	go func() {
		t := time.NewTicker(ohlcv.DefaultAdvanceTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				candle.Advance(ctx, now)
			}
		}
	}()

	var wg sync.WaitGroup
	for _, stream := range []string{"trades", "analytics"} {
		s := stream
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := cons.RunStream(ctx, s, *durable+"-"+s); err != nil &&
				!errors.Is(err, context.Canceled) {
				log.Error("consumer exited", "stream", s, "err", err)
			}
		}()
	}

	// Spool recovery + CH redial loops.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ing.Recover(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		connRef.reDialLoop(ctx, chCfg, log)
	}()

	if pgQuerier != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ing.PollIncome(ctx, pgQuerier)
		}()
	}

	// Metrics + health endpoint.
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			snap := metrics.Snapshot()
			fmt.Fprintf(w, `{"status":"ok","ch_connected":%v,"spool_entries":%d,"spool_bytes":%d}`,
				connRef.healthy(), snap.SpoolEntries, snap.SpoolBytes)
		})
		srv := &http.Server{Addr: *metricsAddr, Handler: mux,
			ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
		go func() {
			if err := srv.ListenAndServe(); err != nil &&
				!errors.Is(err, http.ErrServerClosed) {
				log.Error("analytics: metrics listener died", "err", err)
			}
		}()
		log.Info("analytics metrics endpoint", "addr", *metricsAddr)
	}

	log.Info("analytics started",
		"ch", chCfg.Addr, "db", chCfg.Database,
		"spool", spool.Dir(), "nats", nc.ConnectedURL())

	wg.Wait()
	return nil
}

// connHolder swaps the live ClickHouse Conn under a RWMutex so the reDial
// loop can replace a dead conn after a boot-time (or mid-run) outage while
// in-flight inserts keep using the last conn — which simply errors and
// diverts to the spool.
type connHolder struct {
	mu   sync.RWMutex
	conn analytics.Conn
}

func (h *connHolder) get() analytics.Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conn
}

func (h *connHolder) set(c analytics.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conn = c
}

func (h *connHolder) healthy() bool { return h.get() != nil }

func (h *connHolder) Exec(ctx context.Context, q string, args ...any) error {
	c := h.get()
	if c == nil {
		return fmt.Errorf("clickhouse: not connected")
	}
	return c.Exec(ctx, q, args...)
}

func (h *connHolder) Query(ctx context.Context, q string, args ...any) (driver.Rows, error) {
	c := h.get()
	if c == nil {
		return nil, fmt.Errorf("clickhouse: not connected")
	}
	return c.Query(ctx, q, args...)
}

func (h *connHolder) PrepareBatch(ctx context.Context, q string, opts ...driver.PrepareBatchOption) (driver.Batch, error) {
	c := h.get()
	if c == nil {
		return nil, fmt.Errorf("clickhouse: not connected")
	}
	return c.PrepareBatch(ctx, q, opts...)
}

func (h *connHolder) Ping(ctx context.Context) error {
	c := h.get()
	if c == nil {
		return fmt.Errorf("clickhouse: not connected")
	}
	return c.Ping(ctx)
}

func (h *connHolder) Close() error {
	c := h.get()
	if c == nil {
		return nil
	}
	return c.Close()
}

// reDialLoop re-establishes the CH conn whenever Ping fails — including a
// boot-time outage — so inserts resume without a daemon restart once the
// cluster is back.
func (h *connHolder) reDialLoop(ctx context.Context, cfg analytics.Config, log *slog.Logger) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := h.Ping(ctx); err == nil {
			continue
		}
		c, err := analytics.Dial(ctx, cfg)
		if err != nil {
			continue
		}
		old := h.get()
		h.set(c)
		if old != nil {
			_ = old.Close()
		}
		log.Info("clickhouse reconnected", "addr", cfg.Addr)
	}
}
