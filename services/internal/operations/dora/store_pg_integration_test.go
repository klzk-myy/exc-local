// PgxStore integration test — gated: EXC_PG_TEST=1, DSN via EXC_PG_DSN
// or EXC_TEST_DSN (testenv convention). Applies migration 273 into a
// scratch schema and drives Open → RaiseReport → SubmitReport →
// remediation accept → Close over the real store, then verifies the
// gate blocks a dirty close on the same path.
//
// Run: EXC_PG_TEST=1 go test ./internal/operations/dora/ -run Integration -v
package dora

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

func dsn() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("EXC_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://postgres@/w2d?host=/tmp&port=55433"
}

func pgStore(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("dora_itest_%d", time.Now().UnixNano())
	cfg, err := pgx.ParseConfig(dsn())
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	up, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations",
		"273_dora_incidents.up.sql"))
	if err != nil {
		conn.Close(ctx)
		t.Fatalf("read migration: %v", err)
	}
	if _, err := conn.Exec(ctx, string(up)); err != nil {
		conn.Close(ctx)
		t.Fatalf("apply migration 273: %v", err)
	}
	poolCfg, err := pgxpool.ParseConfig(dsn())
	if err != nil {
		conn.Close(ctx)
		t.Fatalf("pool dsn: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		conn.Close(ctx)
		t.Fatalf("pool: %v", err)
	}
	conn.Close(ctx)
	return pool, func() {
		pool.Close()
		drop, err := pgx.Connect(ctx, dsn())
		if err == nil {
			_, _ = drop.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
			drop.Close(ctx)
		}
	}
}

func TestIntegration_PGClosureGate(t *testing.T) {
	pool, cleanup := pgStore(t)
	defer cleanup()
	s, err := New(NewPgxStore(pool))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	old := t0.AddDate(0, -1, -2) // detected long enough ago that all deadlines passed
	s.SetClock(func() time.Time { return tLate })

	inc, err := s.Open(ctx, &Incident{
		Severity: SeverityP0, Title: "pg gate check", DetectedAt: old})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !inc.Material {
		t.Fatal("P0 must be material")
	}

	// Dirty close: reports missing → blocked.
	if _, err := s.Close(ctx, inc.ID, 1); excerrors.CodeOf(err) != "INCIDENT_CLOSURE_BLOCKED" {
		t.Fatalf("dirty close code = %s (%v)", excerrors.CodeOf(err), err)
	}

	for _, k := range []string{ReportInitial, ReportIntermediate, ReportFinal} {
		if _, err := s.RaiseReport(ctx, inc.ID, k, "NCA"); err != nil {
			t.Fatalf("raise %s: %v", k, err)
		}
		if err := s.SubmitReport(ctx, inc.ID, k, 7, "ev://report"); err != nil {
			t.Fatalf("submit %s: %v", k, err)
		}
	}
	if err := s.RecordRCA(ctx, inc.ID, RCAComplete, "root", "lesson"); err != nil {
		t.Fatalf("rca: %v", err)
	}
	m, err := s.AddRemediation(ctx, &Remediation{
		IncidentID: inc.ID, Action: "fix", Owner: "sre",
		DueAt: tLate.Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("add rem: %v", err)
	}
	// Resolved-but-unaccepted still blocks.
	if err := s.SetRemediationStatus(ctx, m.ID, RemResolved, 7); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := s.Close(ctx, inc.ID, 1); excerrors.CodeOf(err) != "INCIDENT_CLOSURE_BLOCKED" {
		t.Fatalf("unaccepted close code = %s", excerrors.CodeOf(err))
	}
	if err := s.SetRemediationStatus(ctx, m.ID, RemAccepted, 7); err != nil {
		t.Fatalf("accept: %v", err)
	}
	closed, err := s.Close(ctx, inc.ID, 1)
	if err != nil {
		t.Fatalf("clean close: %v", err)
	}
	if closed.Status != StatusClosed {
		t.Fatalf("status %s", closed.Status)
	}
}
