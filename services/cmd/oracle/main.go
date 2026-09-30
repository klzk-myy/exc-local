// Command oracle is the Phase-19.5 Price Oracle & Mark Price service
// (spec §15.3, §19.5; Task 19.5.3.1–3.7).
//
// It aggregates independent vendor feeds — Refinitiv (EXC_REFINITIV_URL
// /EXC_REFINITIV_KEY), Bloomberg BFIX (EXC_BFIX_URL /EXC_BFIX_TOKEN),
// ECB reference rates (EXC_ECB_URL) — computes mark (median of ≥2 fresh
// feeds) and index (volume-weighted) prices on a 1s cadence, enforces
// the 5s staleness gate fail-closed, drops >25bps divergent outliers,
// and publishes the contracted Redis keyspace (mark:{sym},
// mark_price:{sym}, oracle:mark:{sym}[:ts], index_price:{sym},
// oracle:index:{sym}[:ts], oracle:health:{sym}, oracle:fallback:{sym}).
//
// EXC_ORACLE_SIM=1 swaps the vendor adapters for two scripted Sim feeds
// (dev/test); a production boot with no vendor endpoints configured
// fails closed — fewer than two independent feeds can never satisfy
// the mark contract, so the process refuses to start.
//
// Boot: config → Redis → instrument universe (instruments table) →
// feeds → oracle.Service.Run.
package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"exchange/internal/api"
	"exchange/internal/config"
	"exchange/internal/db"
	ipcaeron "exchange/internal/ipc/aeron"
	"exchange/internal/oracle"
	"exchange/internal/oracle/feeds"
	"exchange/internal/redis"
	"exchange/internal/utils"
	"exchange/pkg/decimal"
	"exchange/pkg/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "oracle: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
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

	rdb := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	defer rdb.Close()

	// Symbol universe — the tradable instrument set. EXC_ORACLE_SYMBOLS
	// overrides for dev (comma-separated "EUR/USD,USD/JPY"); production
	// reads the instruments table.
	symbols, err := loadSymbols(ctx, cfg.Postgres.DSN)
	if err != nil {
		return err
	}
	if len(symbols) == 0 {
		return fmt.Errorf("oracle: empty instrument universe")
	}

	fd, err := buildFeeds(log)
	if err != nil {
		return err
	}

	// Sim mode self-drive: the scripted feeds hold quotes only via Set —
	// refresh every symbol each sub-second with a deterministic base
	// price plus ±5bp jitter so the dev daemon emits fresh marks without
	// external drivers. Production mode never reaches this block.
	if os.Getenv("EXC_ORACLE_SIM") == "1" {
		go driveSimFeeds(ctx, fd, symbols, log)
	}

	pub := &oracle.RedisPublisher{C: rdb.Client}

	// Optional Aeron side-band (§26 multicast 224.0.1.1:40456) — the C++
	// core's authoritative oracle source is the Redis keyspace its
	// refresher polls; the Aeron channel mirrors each round for the
	// spec'd sub-ms path. Bound only when a media driver is configured;
	// absent driver leaves Redis-only publication (fail-soft side band).
	if aeroDir, ok := os.LookupEnv("EXC_AERON_DIR"); ok {
		aero, aerr := ipcaeron.Connect(aeroDir, 5000)
		if aerr != nil {
			return fmt.Errorf("oracle: aeron connect: %w", aerr)
		}
		defer aero.Close()
		apub, aerr := aero.AddPublication(
			oracle.AeronOracleURI, oracle.AeronOracleStreamID, 5*time.Second)
		if aerr != nil {
			return fmt.Errorf("oracle: aeron publication: %w", aerr)
		}
		pub.Aeron = oracle.NewAeronMarkSink(apub)
		log.Info("oracle: aeron mark/index band bound",
			"uri", oracle.AeronOracleURI)
	}

	svc, err := oracle.NewService(oracle.Options{
		Feeds:     fd,
		Symbols:   symbols,
		Publisher: pub,
	})
	if err != nil {
		return err
	}

	// R9 health surface (Task 7.3.6) — the K8s pod probes /health/live +
	// /health/ready on EXC_ORACLE_HEALTH_ADDR (deploy/k8s/oracle-service).
	// Empty addr leaves the listener off (bare-metal/supervisord default).
	if addr := os.Getenv("EXC_ORACLE_HEALTH_ADDR"); addr != "" {
		mux := api.HealthMux([]api.Dependency{
			{Name: "redis", Required: true, Probe: func(ctx context.Context) error {
				return api.DependencyErr("redis", rdb.Ping(ctx))
			}},
		})
		srv := &http.Server{Addr: addr, Handler: mux,
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
				log.Error("oracle: health listener died", "err", err)
			}
		}()
		log.Info("oracle: health endpoint", "addr", addr)
	}

	log.Info("oracle: running", "symbols", len(symbols), "feeds", len(fd))
	svc.Run(ctx)
	return nil
}

// loadSymbols resolves the instrument universe — EXC_ORACLE_SYMBOLS
// first (dev override), then the instruments table.
func loadSymbols(ctx context.Context, dsn string) ([]string, error) {
	if env := os.Getenv("EXC_ORACLE_SYMBOLS"); env != "" {
		parts := strings.Split(env, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	}
	pool, err := db.NewPool(ctx, dsn, 4)
	if err != nil {
		return nil, fmt.Errorf("oracle: postgres: %w", err)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx,
		`SELECT symbol FROM instruments WHERE status = 'ACTIVE' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("oracle: instrument list: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// buildFeeds assembles the configured vendor adapters. EXC_ORACLE_SIM=1
// forces the scripted pair (sim_a/sim_b) — the fail-closed floor still
// applies (two independent names, distinct sources).
func buildFeeds(log interface {
	Info(string, ...any)
}) ([]oracle.Feed, error) {
	if os.Getenv("EXC_ORACLE_SIM") == "1" {
		return []oracle.Feed{feeds.NewSimFeed("sim_a"), feeds.NewSimFeed("sim_b")}, nil
	}
	var out []oracle.Feed
	if u := os.Getenv("EXC_REFINITIV_URL"); u != "" {
		out = append(out, &feeds.Refinitiv{URL: u, APIKey: os.Getenv("EXC_REFINITIV_KEY")})
	}
	if u := os.Getenv("EXC_BFIX_URL"); u != "" {
		out = append(out, &feeds.BFIX{URL: u, Token: os.Getenv("EXC_BFIX_TOKEN")})
	}
	if u := os.Getenv("EXC_ECB_URL"); u != "" {
		out = append(out, &feeds.ECB{URL: u})
	}
	if len(out) < oracle.MinFeeds {
		return nil, fmt.Errorf("oracle: %d feed endpoints configured — need ≥2 "+
			"(EXC_REFINITIV_URL, EXC_BFIX_URL, EXC_ECB_URL) or EXC_ORACLE_SIM=1",
			len(out))
	}
	return out, nil
}

// driveSimFeeds is the EXC_ORACLE_SIM=1 self-driver: each scripted feed
// gets the same deterministic per-symbol base price (FNV hash of the
// symbol mapped onto 0.5–2.0) with independent ±5bp sinusoidal jitter
// per feed — marks stay coherent (no divergence trips) while each feed
// remains an independent source.
func driveSimFeeds(ctx context.Context, fd []oracle.Feed, symbols []string,
	log interface{ Info(string, ...any) }) {
	bases := map[string]float64{}
	for _, s := range symbols {
		var h uint32 = 2166136261
		for i := 0; i < len(s); i++ {
			h = (h ^ uint32(s[i])) * 16777619
		}
		bases[s] = 0.5 + float64(h%1500)/1000.0 // 0.5–2.0 deterministic
	}
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	log.Info("oracle: sim driver ticking", "symbols", len(symbols))
	var n int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n++
		}
		for i, f := range fd {
			sim, ok := f.(*feeds.SimFeed)
			if !ok {
				continue
			}
			phase := float64(n+int64(i)*7) * 0.05
			jitter := 1.0 + 0.0005*math.Sin(phase) // ±5bp, feed-independent phase
			for _, s := range symbols {
				sim.Set(s, decimal.NewFromFloat(bases[s]*jitter))
			}
		}
	}
}
