// Command snapshot_builder writes the daily balance_snapshots set
// (Phase-20 Task 20.3.13, spec §16.7 / §24 #362, migration 093).
//
// One row per (account_id, currency, snapshot_date) for every funded
// account — zero balances included — each carrying the account's
// open-position set and chained via hash/prev_hash to the account's
// previous snapshot (per-account SHA-256 chain; genesis =
// audit.GenesisPrevHash). Runs at the 23:59 UTC boundary so the row
// captures the last ledger image of the UTC trading day.
//
// Modes:
//
//	snapshot_builder                 # daemon: run daily at 23:59 UTC
//	snapshot_builder -once           # snapshot today (UTC) and exit
//	snapshot_builder -once -date 2026-09-27   # build/backfill that day
//
// Backfill rule (fail-closed chain discipline): a date may only be
// (re)built while it is the account's chain tip — a crashed partial
// build redoes the tip day cleanly, but inserting a day earlier than
// the tip is refused because it would fork the ordered chain.
//
// Environment (EXC_ prefix, config.Load parity):
//
//	EXC_POSTGRES_DSN / postgres.dsn    OLTP pool (required)
//	EXC_PG_MAX_CONNS                   pool cap (default 4)
//	EXC_SNAPSHOT_RUN_AT                HH:MM UTC run time (default 23:59)
//	EXC_LOGGING_LEVEL / EXC_LOGGING_FORMAT
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot_builder: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		once    = flag.Bool("once", false, "build one day and exit")
		dateStr = flag.String("date", "", "UTC day to build YYYY-MM-DD (default: today)")
		dsn     = flag.String("dsn", "", "PostgreSQL DSN (default: config postgres.dsn / EXC_POSTGRES_DSN)")
	)
	flag.Parse()

	level, _ := logging.ParseLevel(envOr("EXC_LOGGING_LEVEL", "info"))
	format := envOr("EXC_LOGGING_FORMAT", "json")
	log, err := logging.New(level, format)
	if err != nil {
		return err
	}

	if strings.TrimSpace(*dsn) == "" {
		if v := strings.TrimSpace(os.Getenv("EXC_POSTGRES_DSN")); v != "" {
			*dsn = v
		} else {
			cfg, cerr := config.Load()
			if cerr != nil {
				return fmt.Errorf("postgres dsn: %w", cerr)
			}
			*dsn = cfg.Postgres.DSN
		}
	}
	maxConns := int32(4)
	if v := strings.TrimSpace(os.Getenv("EXC_PG_MAX_CONNS")); v != "" {
		n, cerr := strconv.ParseInt(v, 10, 32)
		if cerr != nil || n <= 0 {
			return fmt.Errorf("EXC_PG_MAX_CONNS %q: %w", v, cerr)
		}
		maxConns = int32(n)
	}

	ctx, stop := utils.SignalContext()
	defer stop()

	pool, err := db.NewPool(ctx, *dsn, maxConns)
	if err != nil {
		return fmt.Errorf("postgres pool: %w", err)
	}
	defer pool.Close()

	builder := analytics.NewSnapshotBuilder(pool)
	runAt := envOr("EXC_SNAPSHOT_RUN_AT", "23:59")
	if _, err := snapshotNextRun(time.Now().UTC(), runAt); err != nil {
		return err // validate the schedule before doing any work
	}

	if *once {
		day, err := snapshotTargetDay(*dateStr)
		if err != nil {
			return err
		}
		res, err := builder.BuildDay(ctx, day)
		if err != nil {
			return err
		}
		log.Info("snapshot build complete", "date", res.Date,
			"accounts", res.Accounts, "rows", res.Rows)
		return nil
	}

	// Daemon mode: sleep until the next 23:59 UTC boundary, snapshot the
	// day that boundary closes (nextRun's date IS that day — at 23:59 the
	// snapshot covers the UTC day still in progress; the 24/5 FX week has
	// no post-close drift window like the 01:00 S3 export).
	for {
		next, err := snapshotNextRun(time.Now().UTC(), runAt)
		if err != nil {
			return err
		}
		log.Info("next snapshot scheduled", "at", next.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			log.Info("snapshot_builder shutting down")
			return nil
		case <-time.After(time.Until(next)):
		}
		res, err := builder.BuildDay(ctx, next)
		if err != nil {
			// Fail loud but stay alive — the chain is per-account
			// idempotent, so tomorrow's run resumes cleanly and a manual
			// `-once -date` tip rebuild is the remediation path.
			log.Error("daily snapshot failed", "date",
				next.Format("2006-01-02"), "error", err)
			continue
		}
		log.Info("snapshot build complete", "date", res.Date,
			"accounts", res.Accounts, "rows", res.Rows)
	}
}

// snapshotTargetDay resolves -once's day: explicit -date, else today UTC.
func snapshotTargetDay(dateStr string) (time.Time, error) {
	if dateStr == "" {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	d, err := time.ParseInLocation("2006-01-02", dateStr, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("-date %q: %w", dateStr, err)
	}
	return d, nil
}

// snapshotNextRun returns the next occurrence of HH:MM UTC strictly
// after now (same convention as cmd/s3-market-data-exporter).
func snapshotNextRun(now time.Time, hhmm string) (time.Time, error) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("EXC_SNAPSHOT_RUN_AT %q: want HH:MM", hhmm)
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return time.Time{}, fmt.Errorf("EXC_SNAPSHOT_RUN_AT: %w", err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return time.Time{}, fmt.Errorf("EXC_SNAPSHOT_RUN_AT: %w", err)
	}
	if h > 23 || m > 59 {
		return time.Time{}, fmt.Errorf("EXC_SNAPSHOT_RUN_AT %q out of range", hhmm)
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
