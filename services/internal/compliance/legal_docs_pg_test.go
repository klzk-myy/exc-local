// legal_docs_pg_test.go — Task 22.3.11 PG-gated coverage: migration 043
// schema exercise, SERIALIZABLE register/execute/terminate, the order
// gate against real rows, and the umr_in_scope flag.
// Gated: EXC_PG_TEST=1, EXC_PG_DSN (default docker-compose :5433).
package compliance

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

func legalScratch(t *testing.T, extra []string, down bool) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	ctx := context.Background()
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	schema := fmt.Sprintf("legal_it_%d", time.Now().UnixNano())
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Skipf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Skipf("create schema: %v", err)
	}
	conn.Close(ctx)
	exec := func(file string) {
		t.Helper()
		c, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		c.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
		c.RuntimeParams["search_path"] = schema + ",public"
		cc, err := pgx.ConnectConfig(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		defer cc.Close(ctx)
		sql, err := os.ReadFile(filepath.Join("..", "db", "migrations", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := cc.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	for _, f := range []string{
		"002_create_users.up.sql",
		"003_create_accounts.up.sql",
		"010_create_admin_audit_log.up.sql",
	} {
		exec(f)
	}
	for _, f := range extra {
		exec(f)
	}
	if down {
		exec("043_legal_agreements.down.sql")
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Skipf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func legalSeedAccount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("legal_%d@example.com", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, kyc_tier, umr_in_scope)
		 VALUES ($1,'MARGIN','T1',TRUE) RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return aid
}

func TestPgLegalDocs_FullLifecycleAndGate(t *testing.T) {
	pool := legalScratch(t, []string{"043_legal_agreements.up.sql"}, false)
	ctx := context.Background()
	acct := legalSeedAccount(t, pool)

	svc, err := NewPgLegalDocService(pool, legalOfficerRole)
	if err != nil {
		t.Fatal(err)
	}
	// Gate rejects before docs exist.
	if err := svc.AdmitOrder(ctx, acct, ClassNDF, "PROFESSIONAL", false); excerrors.CodeOf(err) != CodeLegalDocRequired {
		t.Fatalf("expected LEGAL_DOC_REQUIRED, got %v", err)
	}
	// umr_in_scope flag reads through.
	inScope, err := svc.UMRInScope(ctx, acct)
	if err != nil || !inScope {
		t.Fatalf("umr flag %v err %v", inScope, err)
	}
	// Register + execute both agreements.
	for _, ty := range []string{AgreementISDA, AgreementCSA} {
		a, err := svc.Register(ctx, 9001, LegalAgreementInput{
			AccountID: acct, AgreementType: ty, DocumentURL: "s3://docs/" + ty})
		if err != nil {
			t.Fatalf("register %s: %v", ty, err)
		}
		if _, err := svc.Execute(ctx, 9001, a.ID, "s3://signed/"+ty); err != nil {
			t.Fatalf("execute %s: %v", ty, err)
		}
	}
	if err := svc.AdmitOrder(ctx, acct, ClassNDF, "PROFESSIONAL", false); err != nil {
		t.Fatalf("gated admission rejected post-docs: %v", err)
	}
	if err := svc.AdmitOrder(ctx, acct, ClassOption, "PROFESSIONAL", false); err != nil {
		t.Fatalf("option rejected post-docs: %v", err)
	}
	// Duplicate open agreement rejected at the DB level.
	a, err := svc.Register(ctx, 9001, LegalAgreementInput{
		AccountID: acct, AgreementType: AgreementISDA})
	if err == nil || excerrors.CodeOf(err) != "DERIVATIVE_STATE_CONFLICT" {
		t.Fatalf("dup register: %v %v", a, err)
	}
	// Terminate → gate rejects again.
	list, _ := svc.List(ctx, acct)
	if _, err := svc.Terminate(ctx, 9001, list[0].ID); err != nil {
		t.Fatal(err)
	}
	err = svc.AdmitOrder(ctx, acct, ClassNDF, "PROFESSIONAL", false)
	if excerrors.CodeOf(err) != CodeLegalDocRequired {
		t.Fatalf("post-termination admission: %v", err)
	}
	// reduce-only still passes after termination.
	if err := svc.AdmitOrder(ctx, acct, ClassNDF, "PROFESSIONAL", true); err != nil {
		t.Fatalf("reduce-only rejected: %v", err)
	}
}

func TestPgLegalDocs_DownMigration(t *testing.T) {
	pool := legalScratch(t, []string{"043_legal_agreements.up.sql"}, true)
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_tables
		 WHERE schemaname=current_schema() AND tablename='legal_agreements'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("legal_agreements survived the down migration")
	}
	var hasCol bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		   SELECT 1 FROM information_schema.columns
		   WHERE table_schema=current_schema()
		     AND table_name='accounts' AND column_name='umr_in_scope')`).Scan(&hasCol); err != nil {
		t.Fatal(err)
	}
	if hasCol {
		t.Fatal("umr_in_scope survived the down migration")
	}
}
