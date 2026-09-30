// PostgreSQL-gated integration tests for Task 21.3.15 — member
// lifecycle, admission gate, jurisdiction licensing and the launch
// gate. EXC_PG_TEST=1; scratch-schema convention matches
// internal/compliance/integration_test.go.
package venue

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

func venuePool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@127.0.0.1:55433/postgres?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	schema := fmt.Sprintf("venue_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

func venueExecFile(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("exec %s: %v", name, err)
	}
}

// applyVenueSchema builds minimal anchors (accounts, instruments —
// the migration FKs land there) then the real 009/054/250/251 files.
func applyVenueSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE accounts (
		    id              BIGSERIAL PRIMARY KEY,
		    client_category VARCHAR(24) NOT NULL DEFAULT 'RETAIL'
		);
		CREATE TABLE instruments (
		    id              BIGSERIAL PRIMARY KEY,
		    symbol          VARCHAR(32) NOT NULL,
		    instrument_type VARCHAR(16) NOT NULL,
		    base_currency   CHAR(3) NOT NULL,
		    quote_currency  CHAR(3) NOT NULL
		);
		INSERT INTO instruments (symbol, instrument_type, base_currency,
		                       quote_currency)
		VALUES ('EURUSD','SPOT','EUR','USD'),
		       ('USDJPY','SPOT','USD','JPY')`); err != nil {
		t.Fatalf("fixture anchors: %v", err)
	}
	for _, m := range []string{
		"009_create_audit_hash_chain.up.sql",
		"054_regulatory_reporting.up.sql",
		"250_venue_governance.up.sql",
		"251_best_execution_reports.up.sql",
	} {
		venueExecFile(t, ctx, pool, m)
	}
}

func officerRole(context.Context, int64) (string, error) {
	return "Compliance Officer", nil
}

// admitMember drives one member through the full admission dossier —
// the gate inputs CheckTradingAccess requires.
func admitMember(t *testing.T, ctx context.Context, svc *Service,
	jurisdiction string) int64 {
	t.Helper()
	m, created, err := svc.RegisterMember(ctx, MemberInput{
		LegalName: "Acme Liquidity LP",
		LEI:       validLEI(), AccessModel: AccessDEA,
		RegulatoryStatus: "AUTHORIZED", Jurisdiction: jurisdiction,
	}, 100)
	if err != nil || !created {
		t.Fatalf("register: created=%v err=%v", created, err)
	}
	if _, err := svc.RecordDueDiligence(ctx, m.MemberID, DDCompleted,
		[]byte(`{"kyc":"pass"}`), 100); err != nil {
		t.Fatalf("dd: %v", err)
	}
	if _, err := svc.AttachAgreement(ctx, m.MemberID,
		"DEA_AGREEMENT", "docref://dea-1", 100); err != nil {
		t.Fatalf("agreement: %v", err)
	}
	reviewDue := time.Now().UTC().AddDate(1, 0, 0)
	if _, err := svc.Decide(ctx, m.MemberID, true, "admitted",
		reviewDue, 100); err != nil {
		t.Fatalf("decide: %v", err)
	}
	return m.MemberID
}

// linkAccount puts the member's LEI on an account via
// party_identifiers — the member-detection seam CheckTradingAccess
// reads.
func linkAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	svc *Service, memberID int64) int64 {
	t.Helper()
	m, err := svc.GetMember(ctx, memberID)
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	var acct int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts DEFAULT VALUES RETURNING id`).Scan(&acct); err != nil {
		t.Fatalf("account: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO party_identifiers (account_id, lei)
		 VALUES ($1,$2)`, acct, m.LEI); err != nil {
		t.Fatalf("party identifier: %v", err)
	}
	return acct
}

func TestITVenueAdmissionLifecycle(t *testing.T) {
	ctx, pool := venuePool(t)
	applyVenueSchema(t, ctx, pool)
	svc, err := NewService(pool, officerRole)
	if err != nil {
		t.Fatal(err)
	}
	memberID := admitMember(t, ctx, svc, "")
	acct := linkAccount(t, ctx, pool, svc, memberID)

	// Fully admitted member passes the gate.
	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); err != nil {
		t.Fatalf("admitted member blocked: %v", err)
	}
	// A product restriction narrows the gate.
	if _, err := svc.ApproveProducts(ctx, memberID,
		[]string{"FORWARD"}, nil, 100); err != nil {
		t.Fatalf("approve products: %v", err)
	}
	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); excerrors.CodeOf(err) != "PRODUCT_NOT_PERMITTED" {
		t.Fatalf("unlisted product must reject PRODUCT_NOT_PERMITTED, got %v", err)
	}
	if err := svc.CheckTradingAccess(ctx, acct, "FORWARD"); err != nil {
		t.Fatalf("approved product must pass: %v", err)
	}
	if _, err := svc.ApproveProducts(ctx, memberID, nil, nil, 100); err != nil {
		t.Fatalf("clear products: %v", err)
	}
	// Suspension blocks; reinstatement restores.
	if _, err := svc.Suspend(ctx, memberID, "margin breach review", 100); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); excerrors.CodeOf(err) != "FORBIDDEN" {
		t.Fatalf("suspended member must reject FORBIDDEN, got %v", err)
	}
	if _, err := svc.Reinstate(ctx, memberID, "review cleared", 100); err != nil {
		t.Fatalf("reinstate: %v", err)
	}
	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); err != nil {
		t.Fatalf("reinstated member blocked: %v", err)
	}
	// Termination is final — trading access never returns.
	if _, err := svc.Terminate(ctx, memberID, "withdrew membership", 100); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); excerrors.CodeOf(err) != "FORBIDDEN" {
		t.Fatalf("terminated member must reject FORBIDDEN, got %v", err)
	}
	// Immutable lifecycle ledger carries every transition.
	events, err := svc.MemberEvents(ctx, memberID, 100)
	if err != nil || len(events) < 7 {
		t.Fatalf("member events: n=%d err=%v", len(events), err)
	}
}

// Client accounts (no LEI in party_identifiers) are not venue members —
// member rules do not apply and the gate passes them through.
func TestITVenueGate_NonMemberPasses(t *testing.T) {
	ctx, pool := venuePool(t)
	applyVenueSchema(t, ctx, pool)
	svc, err := NewService(pool, officerRole)
	if err != nil {
		t.Fatal(err)
	}
	var acct int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts DEFAULT VALUES RETURNING id`).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); err != nil {
		t.Fatalf("non-member account blocked: %v", err)
	}
}

// Jurisdiction-scoped license prerequisites gate member trading: an
// evidenced LICENSE row admits, expiry (intraday lapse) rejects with
// JURISDICTION_UNLICENSED.
func TestITVenueJurisdictionLicense(t *testing.T) {
	ctx, pool := venuePool(t)
	applyVenueSchema(t, ctx, pool)
	svc, err := NewService(pool, officerRole)
	if err != nil {
		t.Fatal(err)
	}
	memberID := admitMember(t, ctx, svc, "EU")
	acct := linkAccount(t, ctx, pool, svc, memberID)

	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); err != nil {
		t.Fatalf("no EU prereq rows → pass: %v", err)
	}
	p, err := svc.EvidencePrerequisite(ctx, PrereqLicense, "EU",
		"", "docref://eu-license-1", nil, 100)
	if err != nil {
		t.Fatalf("evidence EU license: %v", err)
	}
	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); err != nil {
		t.Fatalf("evidenced license must pass: %v", err)
	}
	// Intraday lapse: the same member rejects on the next gate read.
	if _, err := svc.MarkPrereqExpired(ctx, p.PrereqID, 100); err != nil {
		t.Fatalf("expire license: %v", err)
	}
	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); excerrors.CodeOf(err) != "JURISDICTION_UNLICENSED" {
		t.Fatalf("lapsed license must reject JURISDICTION_UNLICENSED, got %v", err)
	}
}

// The launch gate fails closed while any required GLOBAL prerequisite
// is missing, and opens once all six are evidenced.
func TestITVenueLaunchGate(t *testing.T) {
	ctx, pool := venuePool(t)
	applyVenueSchema(t, ctx, pool)
	svc, err := NewService(pool, officerRole)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := svc.LaunchGate(ctx)
	if err != nil {
		t.Fatalf("launch gate: %v", err)
	}
	if rep.Ready || len(rep.Missing) != 6 {
		t.Fatalf("seeded prereqs are MISSING — gate must block: %+v", rep)
	}
	for _, kind := range []string{
		PrereqLicense, PrereqRegulatorAuth, PrereqLegalOpinion,
		PrereqBoardAppointment, PrereqCCOAppointment, PrereqMinFinancialRes,
	} {
		if _, err := svc.EvidencePrerequisite(ctx, kind, "GLOBAL",
			"", "docref://"+kind, nil, 100); err != nil {
			t.Fatalf("evidence %s: %v", kind, err)
		}
	}
	rep, err = svc.LaunchGate(ctx)
	if err != nil || !rep.Ready || len(rep.Missing) != 0 {
		t.Fatalf("all evidenced — gate must open: %+v err=%v", rep, err)
	}
}

// Overdue annual review: the sweep flags the member and CheckTradingAccess
// rejects — review currency is a trading-access condition.
func TestITVenueOverdueReview(t *testing.T) {
	ctx, pool := venuePool(t)
	applyVenueSchema(t, ctx, pool)
	svc, err := NewService(pool, officerRole)
	if err != nil {
		t.Fatal(err)
	}
	memberID := admitMember(t, ctx, svc, "")
	acct := linkAccount(t, ctx, pool, svc, memberID)
	if _, err := pool.Exec(ctx, `
		UPDATE venue_members SET annual_review_due = CURRENT_DATE - 1
		 WHERE member_id=$1`, memberID); err != nil {
		t.Fatalf("backdate review: %v", err)
	}
	if err := svc.CheckTradingAccess(ctx, acct, "SPOT"); excerrors.CodeOf(err) != "FORBIDDEN" {
		t.Fatalf("overdue review must reject FORBIDDEN, got %v", err)
	}
}
