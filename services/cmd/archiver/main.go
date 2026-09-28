// Command archiver is the Task 4.3.7 partition-archival runner — the
// scheduled/operator entrypoint distinct from the `exchange` CLI's
// restore-partition-archive drill.
//
//	archiver scan                      list eligible partitions (dry-run)
//	archiver run                       archive every eligible partition
//	archiver run --partition=NAME      archive one named partition
//	archiver restore --partition=NAME  restore drill (same as exchange CLI)
//
// Env: EXC_POSTGRES_DSN (or config.yaml postgres.dsn), EXC_S3_ENDPOINT,
// EXC_S3_REGION, EXC_S3_ACCESS_KEY_ID, EXC_S3_SECRET_ACCESS_KEY,
// EXC_S3_ARCHIVE_BUCKET (default exchange-partition-archive).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"exchange/internal/archiver"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/objectstore"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "archiver: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("usage: archiver scan|run|restore [flags]")
	}
	ctx := context.Background()
	switch argv[0] {
	case "scan":
		fs := flag.NewFlagSet("scan", flag.ExitOnError)
		dsn, bucket, cutoff := commonFlags(fs)
		parents := fs.String("parents", "", "comma-separated parent tables (default spec set)")
		_ = fs.Parse(argv[1:])
		a, closer, err := build(ctx, *dsn, *bucket, *cutoff)
		if err != nil {
			return err
		}
		defer closer()
		if *parents != "" {
			a.SetParents(strings.Split(*parents, ","))
		}
		parts, err := a.EligiblePartitions(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-28s %-28s %-40s %s\n", "PARENT", "PARTITION", "BOUND", "RANGE_END")
		for _, p := range parts {
			fmt.Printf("%-28s %-28s %-40s %s\n",
				p.ParentSchema+"."+p.Parent, p.Schema+"."+p.Name,
				p.BoundExpr, p.RangeEnd.Format("2006-01-02"))
		}
		fmt.Printf("archiver scan: %d eligible (cutoff %dd)\n", len(parts), *cutoff)
		return nil

	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		dsn, bucket, cutoff := commonFlags(fs)
		only := fs.String("partition", "", "archive only this partition")
		parents := fs.String("parents", "", "comma-separated parent tables (default spec set)")
		_ = fs.Parse(argv[1:])
		a, closer, err := build(ctx, *dsn, *bucket, *cutoff)
		if err != nil {
			return err
		}
		defer closer()
		if *parents != "" {
			a.SetParents(strings.Split(*parents, ","))
		}
		var parts []archiver.Partition
		if *only != "" {
			all, err := a.ListPartitions(ctx)
			if err != nil {
				return err
			}
			for _, p := range all {
				if p.Name == *only {
					parts = append(parts, p)
				}
			}
			if len(parts) == 0 {
				return fmt.Errorf("partition %q not found under configured parents", *only)
			}
		} else {
			parts, err = a.EligiblePartitions(ctx)
			if err != nil {
				return err
			}
		}
		var done int
		for _, p := range parts {
			le, err := a.ArchivePartition(ctx, p)
			if err != nil {
				return fmt.Errorf("archived %d, then partition %s: %w", done, p.Name, err)
			}
			fmt.Printf("archived %s.%s -> s3://%s/%s (log #%d)\n",
				p.Schema, p.Name, bucketOf(a), le.S3Key, le.ArchiveID)
			done++
		}
		fmt.Printf("archiver run: %d partition(s) archived\n", done)
		return nil

	case "restore":
		fs := flag.NewFlagSet("restore", flag.ExitOnError)
		dsn, bucket, _ := commonFlags(fs)
		part := fs.String("partition", "", "partition name (required)")
		into := fs.String("into-schema", "archive_restore", "restore target schema")
		_ = fs.Parse(argv[1:])
		if *part == "" {
			return fmt.Errorf("--partition is required")
		}
		a, closer, err := build(ctx, *dsn, *bucket, 0)
		if err != nil {
			return err
		}
		defer closer()
		res, err := a.RestorePartition(ctx, *part, *into)
		if err != nil {
			return err
		}
		fmt.Printf("archiver restore: %s -> %s rows=%d manifest=%d parity=%v\n",
			res.Partition, res.IntoTable, res.LoadedRows, res.ManifestRows, res.Parity)
		return nil
	}
	return fmt.Errorf("unknown subcommand %q (scan|run|restore)", argv[0])
}

func bucketOf(a *archiver.Archiver) string {
	b := os.Getenv("EXC_S3_ARCHIVE_BUCKET")
	if b == "" {
		b = archiver.DefaultPartitionBucket
	}
	return b
}

func commonFlags(fs *flag.FlagSet) (dsn, bucket *string, cutoff *int) {
	dsn = fs.String("dsn", "", "postgres DSN (default: config/env)")
	bucket = fs.String("bucket", os.Getenv("EXC_S3_ARCHIVE_BUCKET"), "WORM archive bucket")
	cutoff = fs.Int("cutoff-days", 90, "archive partitions older than N days")
	return
}

func build(ctx context.Context, dsn, bucket string, cutoff int) (*archiver.Archiver, func(), error) {
	if dsn == "" {
		cfg, err := config.Load()
		if err != nil {
			return nil, nil, err
		}
		dsn = cfg.Postgres.DSN
	}
	if bucket == "" {
		bucket = archiver.DefaultPartitionBucket
	}
	pool, err := db.NewPool(ctx, dsn, 4)
	if err != nil {
		return nil, nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("postgres: %w", err)
	}
	ocfg := objectstore.ConfigFromEnv(bucket, os.Getenv)
	var store *objectstore.S3Client
	if ocfg.Endpoint != "" {
		store, err = objectstore.NewDev(ctx, ocfg)
	} else {
		store, err = objectstore.NewAWS(ctx, ocfg)
	}
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	a := archiver.New(pool, store)
	if cutoff > 0 {
		a.SetCutoffDays(cutoff)
	}
	return a, pool.Close, nil
}
