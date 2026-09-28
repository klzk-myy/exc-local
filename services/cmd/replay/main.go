// Command replay is the standalone Task 4.3.3 entrypoint — equivalent to
// `exchange replay-from-archive` for environments where the operator CLI
// is not deployed. Flag-style, JSON report on stdout.
//
//	replay --shard=0 --instrument-id=1 --from=2026-01-01 --to=2026-01-31
//	replay --shard=0 --symbol=EUR/USD --from=... --to=...   (needs postgres)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/objectstore"
	"exchange/internal/recovery"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "replay: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		shard  = flag.Uint("shard", 0, "shard id")
		symbol = flag.String("symbol", "", "instrument symbol (resolves via postgres)")
		instID = flag.Uint("instrument-id", 0, "instrument id (skips DB lookup)")
		from   = flag.String("from", "", "YYYY-MM-DD (required)")
		to     = flag.String("to", "", "YYYY-MM-DD inclusive (required)")
		iv     = flag.Duration("snapshot-interval", 5*time.Minute, "book snapshot cadence")
		gaps   = flag.Bool("allow-seq-gaps", false, "warn instead of failing on seq gaps")
		bucket = flag.String("bucket", os.Getenv("EXC_S3_WAL_BUCKET"), "archive bucket")
	)
	flag.Parse()
	if *from == "" || *to == "" {
		return fmt.Errorf("--from and --to are required")
	}
	f, err := time.ParseInLocation("2006-01-02", *from, time.UTC)
	if err != nil {
		return fmt.Errorf("bad --from: %w", err)
	}
	t, err := time.ParseInLocation("2006-01-02", *to, time.UTC)
	if err != nil {
		return fmt.Errorf("bad --to: %w", err)
	}
	if *bucket == "" {
		*bucket = recovery.DefaultWalBucket
	}

	ctx := context.Background()
	var labels map[uint32]string
	if *symbol != "" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		pool, err := db.NewPool(ctx, cfg.Postgres.DSN, 2)
		if err != nil {
			return fmt.Errorf("--symbol needs postgres: %w", err)
		}
		id, m, err := recovery.ResolveInstrumentID(ctx, pool, *symbol)
		pool.Close()
		if err != nil {
			return err
		}
		*instID = uint(id)
		labels = m
	}

	ocfg := objectstore.ConfigFromEnv(*bucket, os.Getenv)
	var store *objectstore.S3Client
	if ocfg.Endpoint != "" {
		store, err = objectstore.NewDev(ctx, ocfg)
	} else {
		store, err = objectstore.NewAWS(ctx, ocfg)
	}
	if err != nil {
		return err
	}
	rep, err := recovery.NewReplayer(store).Replay(ctx, recovery.ReplayOptions{
		Shard: uint16(*shard), InstrumentID: uint32(*instID),
		From: f, To: t, SnapshotEvery: *iv,
		AllowSeqGaps: *gaps, ResolveSymbols: labels,
	})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
