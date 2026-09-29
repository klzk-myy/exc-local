// dbexec is a dev/drill SQL executor — runs one SQL statement (or a
// migration-style multi-statement file) against the configured Postgres
// DSN and prints the result. It exists for operator drills and CI
// evidence gathering (e.g. Phase-13.5 Task 13.5.3.3 verify-audit
// tamper/clean runs against a scratch schema) — it is NOT a migration
// tool; schema changes ship as numbered files under
// internal/db/migrations.
//
//	# single statement
//	go run ./cmd/dbexec --sql "CREATE SCHEMA drill_x"
//	# file (multi-statement, e.g. a migration fixture into a scratch schema)
//	go run ./cmd/dbexec --file internal/db/migrations/009_create_audit_hash_chain.up.sql
//	# row-returning statement prints rows as JSON
//	go run ./cmd/dbexec --sql "SELECT id FROM audit_hash_chain"
//
// Connection: EXC_POSTGRES_DSN (or postgres.dsn in config.yaml). Append
// ?search_path=<schema> to scope a run to a scratch schema.
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
)

func main() {
	sqlFlag := flag.String("sql", "", "SQL text to execute")
	fileFlag := flag.String("file", "", "path to a SQL file to execute")
	timeout := flag.Duration("timeout", 30*time.Second, "statement timeout")
	flag.Parse()

	var sql string
	var fileMode bool
	switch {
	case *sqlFlag != "" && *fileFlag != "":
		fatal("--sql and --file are mutually exclusive")
	case *sqlFlag != "":
		sql = *sqlFlag
	case *fileFlag != "":
		b, err := os.ReadFile(*fileFlag)
		if err != nil {
			fatal("read %s: %v", *fileFlag, err)
		}
		sql = string(b)
		fileMode = true
	default:
		fatal("pass --sql or --file")
	}

	cfg, err := config.Load()
	if err != nil {
		fatal("config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	pool, err := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	if err != nil {
		fatal("connect: %v", err)
	}
	defer pool.Close()

	if fileMode {
		// Migration-style files are multi-statement — run them through
		// the simple protocol (PgConn.Exec), which accepts a whole
		// script, unlike the extended-protocol Query path.
		conn, err := pool.Acquire(ctx)
		if err != nil {
			fatal("acquire: %v", err)
		}
		defer conn.Release()
		res := conn.Conn().PgConn().Exec(ctx, sql)
		if _, err := res.ReadAll(); err != nil {
			fatal("exec: %v", err)
		}
		fmt.Println("dbexec: OK (script executed)")
		return
	}

	rows, err := pool.Query(ctx, sql)
	if err != nil {
		fatal("exec: %v", err)
	}
	defer rows.Close()

	var out []map[string]any
	cols := rows.FieldDescriptions()
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			fatal("row: %v", err)
		}
		rec := map[string]any{}
		for i, c := range cols {
			rec[string(c.Name)] = vals[i]
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		fatal("rows: %v", err)
	}
	if len(out) == 0 {
		fmt.Println("dbexec: OK (no rows)")
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fatal("encode: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dbexec: "+format+"\n", args...)
	os.Exit(2)
}
