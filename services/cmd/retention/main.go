// Command retention is the nightly unified-retention enforcer
// (Phase-09 Task 9.3.22, spec §19.12, §24 #212).
//
//	retention check                     dry-run: report drift, exit 1 on violations
//	retention apply                     enforce: move partitions, purge expired rows
//	retention report                    print the policy matrix
//
// Flags:
//
//	--dsn          postgres DSN (default: config/env EXC_POSTGRES_DSN)
//	--policy       path to tiering_policy.yaml (default:
//	               infrastructure/data-tiering/tiering_policy.yaml or
//	               the builtin spec §19.12 schedule)
//	--bucket       WORM archive bucket for object_store checks
//
// ClickHouse TTL drift checks activate only when --ch-url (or
// EXC_CLICKHOUSE_URL) is set; otherwise they report SKIPPED.
//
// Every check writes a retention_audit_log row keyed by run_id. Exit
// codes: 0 clean, 1 violations found, 2 operational error.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/archiver"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/objectstore"
	"exchange/internal/operations/retention"
)

func main() {
	code := 2
	if err := run(os.Args[1:]); err != nil {
		if ee, ok := err.(exitErr); ok {
			fmt.Fprintln(os.Stderr, "retention:", ee.msg)
			code = ee.code
		} else {
			fmt.Fprintln(os.Stderr, "retention:", err)
		}
	}
	os.Exit(code)
}

// exitErr carries a chosen exit code out of run.
type exitErr struct {
	code int
	msg  string
}

func (e exitErr) Error() string { return e.msg }

func run(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("usage: retention check|apply|report [flags]")
	}
	ctx := context.Background()
	switch argv[0] {
	case "check", "apply":
		apply := argv[0] == "apply"
		fs := flag.NewFlagSet(argv[0], flag.ExitOnError)
		dsn := fs.String("dsn", "", "postgres DSN")
		policyPath := fs.String("policy", "", "tiering_policy.yaml path")
		bucket := fs.String("bucket", os.Getenv("EXC_S3_ARCHIVE_BUCKET"), "WORM bucket")
		chURL := fs.String("ch-url", os.Getenv("EXC_CLICKHOUSE_URL"),
			"ClickHouse HTTP base (empty = skip CH checks)")
		_ = fs.Parse(argv[1:])

		pol, err := loadPolicy(*policyPath)
		if err != nil {
			return err
		}
		pool, err := openPool(ctx, *dsn)
		if err != nil {
			return err
		}
		defer pool.Close()

		enf := retention.New(pool, pol)

		// Object store seam (devs3 endpoint when EXC_S3_ENDPOINT set).
		b := *bucket
		if b == "" {
			b = archiver.DefaultPartitionBucket
		}
		ocfg := objectstore.ConfigFromEnv(b, os.Getenv)
		var store objectstore.Client
		if ocfg.Endpoint != "" {
			store, err = objectstore.NewDev(ctx, ocfg)
		} else {
			store, err = objectstore.NewAWS(ctx, ocfg)
		}
		if err != nil {
			return fmt.Errorf("objectstore: %w", err)
		}
		enf.SetStore(store)

		if *chURL != "" {
			enf.SetCH(&httpCH{base: *chURL})
		}

		// Apply-mode mover: the archiver lifecycle runner.
		a := archiver.New(pool, store)
		if err := applyClassPolicies(a, pol); err != nil {
			return err
		}
		enf.SetMover(moverFunc{a})

		rep, err := enf.Run(ctx, apply)
		if err != nil {
			return err
		}
		printReport(rep)
		if rep.Violations > 0 {
			// Structured marker for log alerting — spec §23 code.
			fmt.Fprintf(os.Stderr,
				`{"level":"error","code":"RETENTION_POLICY_VIOLATION","run_id":%q,"violations":%d}`+"\n",
				rep.RunID, rep.Violations)
			return exitErr{code: 1,
				msg: fmt.Sprintf("%d retention violation(s), run %s", rep.Violations, rep.RunID)}
		}
		if rep.Errors > 0 {
			return exitErr{code: 1,
				msg: fmt.Sprintf("%d check(s) errored, run %s", rep.Errors, rep.RunID)}
		}
		return nil

	case "report":
		fs := flag.NewFlagSet("report", flag.ExitOnError)
		policyPath := fs.String("policy", "", "tiering_policy.yaml path")
		_ = fs.Parse(argv[1:])
		pol, err := loadPolicy(*policyPath)
		if err != nil {
			return err
		}
		fmt.Printf("%-24s %-20s %6s %6s %7s %-9s %s\n",
			"CLASS", "STORE/TABLE", "HOT(d)", "WARM(d)", "RETAIN(d)", "PURGE", "BASIS")
		for _, c := range pol.Classes {
			fmt.Printf("%-24s %-20s %6d %6d %7d %-9v %s\n",
				c.Name, c.Store+"/"+c.Table, c.HotDays, c.WarmDays,
				c.RetainDays, c.Purgeable, c.Basis)
		}
		return nil
	}
	return fmt.Errorf("unknown subcommand %q (check|apply|report)", argv[0])
}

// loadPolicy loads the YAML policy, falling back to the builtin schedule
// and searching default paths.
func loadPolicy(path string) (*retention.Policy, error) {
	if path == "" {
		for _, cand := range []string{
			"infrastructure/data-tiering/tiering_policy.yaml",
			"../infrastructure/data-tiering/tiering_policy.yaml",
			"../../infrastructure/data-tiering/tiering_policy.yaml",
		} {
			if _, err := os.Stat(cand); err == nil {
				path = cand
				break
			}
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr,
			"retention: no policy file found — using builtin spec §19.12 schedule")
		return retention.BuiltinPolicy(), nil
	}
	return retention.LoadYAML(path)
}

// applyClassPolicies configures the archiver's per-parent schedule from
// the policy's partitioned classes.
func applyClassPolicies(a *archiver.Archiver, p *retention.Policy) error {
	var cps []archiver.ClassPolicy
	for _, c := range p.Classes {
		if c.Store != retention.StorePartitionedPG {
			continue
		}
		cps = append(cps, archiver.ClassPolicy{
			Parent:     c.Table,
			HotDays:    c.HotDays,
			WarmDays:   c.WarmDays,
			RetainDays: c.RetainDays,
		})
	}
	norm, err := archiver.NormalizePolicies(cps)
	if err != nil {
		return err
	}
	a.SetClassPolicies(norm)
	return nil
}

// moverFunc adapts the Archiver to retention.Mover.
type moverFunc struct{ a *archiver.Archiver }

func (m moverFunc) MoveParent(ctx context.Context, parent string) error {
	return m.a.RunLifecycleParent(ctx, parent)
}

// httpCH is the minimal ClickHouse-HTTP querier for TTL drift checks.
type httpCH struct{ base string }

func (h *httpCH) QueryCSV(ctx context.Context, query string) (io.ReadCloser, error) {
	u := strings.TrimRight(h.base, "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u,
		strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("clickhouse HTTP %d: %s", resp.StatusCode, body)
	}
	return resp.Body, nil
}

func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		dsn = os.Getenv("EXC_POSTGRES_DSN")
	}
	if dsn == "" {
		cfg, err := config.Load()
		if err != nil {
			return nil, err
		}
		dsn = cfg.Postgres.DSN
	}
	pool, err := db.NewPool(ctx, dsn, 4)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return pool, nil
}

func printReport(rep *retention.Report) {
	fmt.Printf("%-9s %-24s %-36s %-16s %s\n",
		"RESULT", "CLASS", "TARGET", "CHECK", "DETAIL")
	for _, f := range rep.Findings {
		fmt.Printf("%-9s %-24s %-36s %-16s %s\n",
			f.Result, f.DataType, f.Target, f.Check, f.Detail)
	}
	fmt.Printf("retention %s: run=%s findings=%d violations=%d actions=%d errors=%d\n",
		rep.Mode, rep.RunID, len(rep.Findings), rep.Violations,
		rep.Actions, rep.Errors)
}
