// Command postmortem-archive renders the public post-mortem archive
// (Phase-09 Task 9.3.25 — "historical incident post-mortems publicly
// accessible").
//
// It is a build-time artifact generator, not a service: it reads the
// §9.3.9 authored post-mortem archive (docs/incidents/INC-*.md) plus,
// when a DSN is available, the ops_incidents public incident store, runs
// every document through the ops.Sanitize pipeline (compliance masking:
// emails, phones, IBANs, account/order linkage ids, credentials, person
// names in blameless metadata, IPs, internal hostnames), and writes a
// static public layout:
//
//	<out>/postmortems/index.html
//	<out>/postmortems/index.json
//	<out>/postmortems/{incident-id}/index.html
//
// The status-page host serves (or syncs to object storage) the output
// directory verbatim — no HTTP route is added by this command. With
// -link, generated artifact URLs are written back into
// ops_incidents.postmortem_url so GET /api/v1/system/incidents surfaces
// them on the next read.
//
// Usage:
//
//	postmortem-archive -incidents docs/incidents -out status/public
//	postmortem-archive -incidents docs/incidents -out status/public \
//	    -dsn postgres://... -base-url https://status.example.com -link
//
// Flags:
//
//	-incidents   authored post-mortem directory (default docs/incidents;
//	             absent dir = docs source skipped, store may still publish)
//	-out         output root for the static archive (default status/public)
//	-dsn         PostgreSQL DSN (default: EXC_POSTGRES_DSN / config
//	             postgres.dsn); enables the ops_incidents source
//	-base-url    public base URL prepended to artifact paths for
//	             postmortem_url writes (default: relative paths)
//	-link        write artifact URLs into ops_incidents.postmortem_url
//	             (requires -dsn; never overwrites an existing link)
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/ops"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "postmortem-archive: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	incidentsDir := flag.String("incidents", "docs/incidents",
		"authored post-mortem directory (INC-*.md)")
	out := flag.String("out", "status/public",
		"output root for the static archive")
	dsn := flag.String("dsn", "",
		"PostgreSQL DSN (default EXC_POSTGRES_DSN / config postgres.dsn)")
	baseURL := flag.String("base-url", "",
		"public base URL for postmortem_url link-back")
	link := flag.Bool("link", false,
		"write artifact URLs into ops_incidents.postmortem_url")
	flag.Parse()

	level, _ := logging.ParseLevel(envOr("EXC_LOGGING_LEVEL", "info"))
	format := envOr("EXC_LOGGING_FORMAT", "json")
	log, err := logging.New(level, format)
	if err != nil {
		return err
	}

	ctx, stop := utils.SignalContext()
	defer stop()

	var sources []ops.Source
	if st, err := os.Stat(*incidentsDir); err == nil && st.IsDir() {
		sources = append(sources, ops.DirSource{Dir: *incidentsDir})
	} else {
		log.Info("post-mortem doc dir absent — docs source skipped",
			"dir", *incidentsDir)
	}

	// PostgreSQL: needed for the ops_incidents source and -link. Absent
	// DSN is not fatal — the authored docs alone still render.
	resolvedDSN := *dsn
	if resolvedDSN == "" {
		resolvedDSN = os.Getenv("EXC_POSTGRES_DSN")
	}
	if resolvedDSN == "" {
		if cfg, cerr := config.Load(); cerr == nil {
			resolvedDSN = cfg.Postgres.DSN
		}
	}
	var pool *pgxpool.Pool
	if resolvedDSN != "" {
		p, err := db.NewPool(ctx, resolvedDSN, 2)
		if err != nil {
			return fmt.Errorf("postgres pool: %w", err)
		}
		defer p.Close()
		pool = p
		sources = append(sources, ops.StoreSource{Pool: p})
	} else {
		log.Info("no postgres dsn — ops_incidents source skipped")
	}
	if *link && pool == nil {
		return fmt.Errorf("-link requires a postgres dsn")
	}

	gen := ops.Generator{}
	m, err := gen.Generate(ctx, ops.DirWriter{Root: *out}, sources...)
	if err != nil {
		return err
	}
	redactions := 0
	for _, e := range m.Entries {
		for _, n := range e.Redactions {
			redactions += n
		}
	}
	log.Info("post-mortem archive rendered",
		"artifacts", len(m.Entries), "redactions", redactions,
		"out", *out)

	if *link {
		n, err := ops.LinkIncidents(ctx, pool, *baseURL, m)
		if err != nil {
			return err
		}
		log.Info("incident rows linked", "updated", n)
	}
	return nil
}
