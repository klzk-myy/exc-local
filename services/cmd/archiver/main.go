// Command archiver is the partition-archival + data-tiering runner —
// the scheduled/operator entrypoint distinct from the `exchange` CLI's
// restore-partition-archive drill.
//
//	archiver scan                      list eligible partitions (dry-run)
//	archiver run                       archive every eligible partition (hot→cold direct)
//	archiver run --partition=NAME      archive one named partition
//	archiver restore --partition=NAME  restore drill (same as exchange CLI)
//	archiver lifecycle [--apply]       Task 9.3.24: hot→warm→cold tier pass
//	archiver verify [--sample=N]       Task 9.3.17: WORM integrity drill
//	archiver hold add|release|list     Task 9.3.17: compliance holds
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
	"time"

	"exchange/internal/archiver"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/objectstore"
	"exchange/internal/operations/retention"
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

	case "lifecycle":
		fs := flag.NewFlagSet("lifecycle", flag.ExitOnError)
		dsn, bucket, _ := commonFlags(fs)
		apply := fs.Bool("apply", false, "perform moves (default: plan only)")
		policyPath := fs.String("policy", "", "tiering_policy.yaml (optional; overrides class schedule)")
		_ = fs.Parse(argv[1:])
		a, closer, err := build(ctx, *dsn, *bucket, 0)
		if err != nil {
			return err
		}
		defer closer()
		if *policyPath != "" {
			pol, err := retention.LoadYAML(*policyPath)
			if err != nil {
				return err
			}
			a.SetClassPolicies(classPoliciesFrom(pol))
		}
		rep, err := a.RunLifecycle(ctx, !*apply)
		if err != nil {
			return err
		}
		for _, mv := range rep.Moves {
			fmt.Printf("%-7s %-16s %-32s %-4s->%-4s rows=%d %s\n",
				mv.Result, mv.Parent, mv.Partition, mv.FromTier, mv.ToTier,
				mv.RowCount, mv.Detail)
		}
		fmt.Printf("archiver lifecycle: run=%s dry=%v moves=%d\n",
			rep.RunID, rep.DryRun, len(rep.Moves))
		return nil

	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		dsn, bucket, _ := commonFlags(fs)
		sample := fs.Int("sample", 3, "random archives to re-verify (0=all)")
		_ = fs.Parse(argv[1:])
		a, closer, err := build(ctx, *dsn, *bucket, 0)
		if err != nil {
			return err
		}
		defer closer()
		rep, err := a.VerifyDrill(ctx, *sample)
		for _, r := range rep.Results {
			mark := "OK"
			if !r.OK() {
				mark = "FAIL"
			}
			fmt.Printf("%-4s log#%-6d %-32s sha=%v etag=%v rows=%v %s\n",
				mark, r.ArchiveID, r.Partition, r.SHA256OK, r.ETagOK,
				r.RowCountOK, r.Detail)
		}
		fmt.Printf("archiver verify: run=%s sampled=%d failures=%d\n",
			rep.RunID, rep.Samples, rep.Failures)
		return err

	case "hold":
		return runHold(ctx, argv[1:])
	}
	return fmt.Errorf("unknown subcommand %q (scan|run|restore|lifecycle|verify|hold)", argv[0])
}

// classPoliciesFrom maps the retention YAML classes onto archiver class
// policies (partitioned classes only).
func classPoliciesFrom(p *retention.Policy) []archiver.ClassPolicy {
	var out []archiver.ClassPolicy
	for _, c := range p.Classes {
		if c.Store != retention.StorePartitionedPG {
			continue
		}
		out = append(out, archiver.ClassPolicy{
			Parent:     c.Table,
			HotDays:    c.HotDays,
			WarmDays:   c.WarmDays,
			RetainDays: c.RetainDays,
		})
	}
	return out
}

// runHold implements `archiver hold add|release|list`.
func runHold(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("usage: archiver hold add|release|list [flags]")
	}
	switch argv[0] {
	case "add":
		fs := flag.NewFlagSet("hold add", flag.ExitOnError)
		dsn, bucket, _ := commonFlags(fs)
		parent := fs.String("parent", "", "parent table (required)")
		partition := fs.String("partition", "", "partition name (empty = whole parent)")
		caseRef := fs.String("case-ref", "", "regulator case / matter ref (required)")
		reason := fs.String("reason", "", "hold reason (required)")
		by := fs.String("by", "", "compliance officer identity (required)")
		days := fs.Int("days", 0, "hold expiry in days (0 = indefinite)")
		_ = fs.Parse(argv[1:])
		if *parent == "" || *caseRef == "" || *reason == "" || *by == "" {
			return fmt.Errorf("hold add requires --parent --case-ref --reason --by")
		}
		a, closer, err := build(ctx, *dsn, *bucket, 0)
		if err != nil {
			return err
		}
		defer closer()
		var exp *time.Time
		if *days > 0 {
			t := time.Now().UTC().AddDate(0, 0, *days)
			exp = &t
		}
		id, err := a.AddHold(ctx, *parent, *partition, *caseRef, *reason, *by, exp)
		if err != nil {
			return err
		}
		fmt.Printf("hold #%d added: %s.%s case=%s expires=%v\n",
			id, *parent, orAll(*partition), *caseRef, exp)
		return nil

	case "release":
		fs := flag.NewFlagSet("hold release", flag.ExitOnError)
		dsn, bucket, _ := commonFlags(fs)
		id := fs.Int64("id", 0, "hold id (required)")
		by := fs.String("by", "", "releasing officer (required)")
		_ = fs.Parse(argv[1:])
		if *id == 0 || *by == "" {
			return fmt.Errorf("hold release requires --id and --by")
		}
		a, closer, err := build(ctx, *dsn, *bucket, 0)
		if err != nil {
			return err
		}
		defer closer()
		if err := a.ReleaseHold(ctx, *id, *by); err != nil {
			return err
		}
		fmt.Printf("hold #%d released by %s\n", *id, *by)
		return nil

	case "list":
		fs := flag.NewFlagSet("hold list", flag.ExitOnError)
		dsn, bucket, _ := commonFlags(fs)
		all := fs.Bool("all", false, "include released/expired holds")
		_ = fs.Parse(argv[1:])
		a, closer, err := build(ctx, *dsn, *bucket, 0)
		if err != nil {
			return err
		}
		defer closer()
		holds, err := a.ListHolds(ctx, !*all)
		if err != nil {
			return err
		}
		fmt.Printf("%-6s %-20s %-30s %-24s %-16s %s\n",
			"ID", "PARENT", "PARTITION", "CASE", "CREATED_BY", "ACTIVE")
		for _, h := range holds {
			fmt.Printf("%-6d %-20s %-30s %-24s %-16s %v\n",
				h.HoldID, h.ParentTable, orAll(deref(h.PartitionName)),
				h.CaseRef, h.CreatedBy, h.Active(time.Now().UTC()))
		}
		return nil
	}
	return fmt.Errorf("unknown hold subcommand %q (add|release|list)", argv[0])
}

func orAll(s string) string {
	if s == "" {
		return "<all>"
	}
	return s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
