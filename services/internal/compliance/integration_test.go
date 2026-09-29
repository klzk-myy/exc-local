// PostgreSQL-gated integration tests for PgStore + migrations 203–205,
// 210. Convention matches internal/funding: EXC_PG_TEST=1 enables, each
// run builds a scratch schema with minimal anchor fixtures plus the real
// migration files (017 kyc_documents; 203–205 owned by this task; 210 is
// the PII-F1 sealing migration).
package compliance

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/auth"
)

// testBox builds a real AES-256-GCM SecretBox on a fixed test key —
// sealing assertions must exercise the production crypto path, not a
// stub.
func testBox(t *testing.T) *auth.SecretBox {
	t.Helper()
	box, err := auth.NewSecretBox([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	return box
}

func itPool(t *testing.T) (context.Context, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("kyc_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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

func execSQLFile(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "db", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("exec %s: %v", name, err)
	}
}

// applyKYCSchema builds the minimal anchors (accounts, risk_limits —
// the fixture owns their shape) then the real migrations.
func applyKYCSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE accounts (
		    id       BIGSERIAL PRIMARY KEY,
		    kyc_tier VARCHAR(4) NOT NULL DEFAULT 'T0',
		    client_category VARCHAR(24) NOT NULL DEFAULT 'RETAIL',
		    nbp      BOOLEAN NOT NULL DEFAULT TRUE
		);
		CREATE TABLE risk_limits (
		    id                   BIGSERIAL PRIMARY KEY,
		    account_id           BIGINT,
		    symbol               VARCHAR(32),
		    tier                 VARCHAR(16),
		    max_daily_volume     NUMERIC(28,8),
		    daily_withdraw_limit NUMERIC(28,8)
		)`); err != nil {
		t.Fatalf("fixture anchors: %v", err)
	}
	for _, m := range []string{
		"017_create_kyc_documents.up.sql",
		"203_kyc_submission.up.sql",
		"204_kyc_ops_matrix.up.sql",
		"205_tax_self_certifications.up.sql",
		"210_tax_pii_seal.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
}

func mkAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tier string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (kyc_tier) VALUES ($1) RETURNING id`, tier).Scan(&id); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return id
}

func TestITMigrationRoundTrip(t *testing.T) {
	ctx, pool := itPool(t)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE accounts (
		    id       BIGSERIAL PRIMARY KEY,
		    kyc_tier VARCHAR(4) NOT NULL DEFAULT 'T0',
		    client_category VARCHAR(24) NOT NULL DEFAULT 'RETAIL',
		    nbp      BOOLEAN NOT NULL DEFAULT TRUE
		);
		CREATE TABLE risk_limits (
		    id                   BIGSERIAL PRIMARY KEY,
		    account_id           BIGINT,
		    symbol               VARCHAR(32),
		    tier                 VARCHAR(16),
		    max_daily_volume     NUMERIC(28,8),
		    daily_withdraw_limit NUMERIC(28,8)
		)`); err != nil {
		t.Fatalf("fixture anchors: %v", err)
	}
	execSQLFile(t, ctx, pool, "017_create_kyc_documents.up.sql")
	for _, m := range []string{
		"203_kyc_submission.up.sql",
		"204_kyc_ops_matrix.up.sql",
		"205_tax_self_certifications.up.sql",
		"210_tax_pii_seal.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
	// Down in reverse order, then re-up — both directions must be clean.
	for _, m := range []string{
		"210_tax_pii_seal.down.sql",
		"205_tax_self_certifications.down.sql",
		"204_kyc_ops_matrix.down.sql",
		"203_kyc_submission.down.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = current_schema()
		    AND table_name IN ('kyc_submissions','kyc_tier_policies',
		                       'kyc_ops_matrix','tax_self_certifications')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d task tables survived the down migrations", n)
	}
	for _, m := range []string{
		"203_kyc_submission.up.sql",
		"204_kyc_ops_matrix.up.sql",
		"205_tax_self_certifications.up.sql",
		"210_tax_pii_seal.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
}

func TestITSubmissionLifecycle(t *testing.T) {
	ctx, pool := itPool(t)
	applyKYCSchema(t, ctx, pool)
	acct := mkAccount(t, ctx, pool, "T0")
	store, err := NewPgStore(pool, testBox(t))
	if err != nil {
		t.Fatal(err)
	}

	// Tier policy + matrix seeds (migration 204).
	pol, err := store.TierPolicy(ctx, TierT2)
	if err != nil || pol == nil {
		t.Fatalf("T2 policy: %v", err)
	}
	if pol.ReverifyMonths != 12 || pol.RescreenCadence != "DAILY" {
		t.Fatalf("T2 policy: %+v", pol)
	}
	rows, err := store.Matrix(ctx, TierT1, "US")
	if err != nil || len(rows) < 7 {
		t.Fatalf("T1/US matrix: %d rows, err=%v", len(rows), err)
	}

	// Tier stays T0 pre-review; submission lands PENDING_REVIEW.
	tier, err := store.AccountTier(ctx, acct)
	if err != nil || tier != "T0" {
		t.Fatalf("account tier: %q err=%v", tier, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	sub := &Submission{
		AccountID: acct, RequestedTier: TierT1, Status: SubPendingReview,
		Jurisdiction: "US", SubmittedAt: now, SLADueAt: now.Add(24 * time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateSubmission(ctx, sub); err != nil {
		t.Fatalf("create submission: %v", err)
	}
	doc := &Document{
		AccountID: acct, SubmissionID: sub.ID, Type: "PASSPORT",
		ObjectKey: "kyc/1/1/passport-x.jpg", Status: "PENDING",
		SHA256: fmt.Sprintf("%064x", 1), SizeBytes: 42,
		SSEAlgorithm: "aws:kms", CreatedAt: now,
	}
	if err := store.AttachDocument(ctx, doc); err != nil {
		t.Fatalf("attach doc: %v", err)
	}
	latest, err := store.LatestSubmission(ctx, acct)
	if err != nil || latest == nil || latest.ID != sub.ID {
		t.Fatalf("latest submission: %+v err=%v", latest, err)
	}
	if !latest.SLADueAt.Equal(sub.SLADueAt) {
		t.Fatalf("sla_due round-trip: %v != %v", latest.SLADueAt, sub.SLADueAt)
	}
	docs, err := store.ListAccountDocuments(ctx, acct)
	if err != nil || len(docs) != 1 || docs[0].SSEAlgorithm != "aws:kms" {
		t.Fatalf("documents: %+v err=%v", docs, err)
	}
	// Overdue feed: past the 24h SLA the submission surfaces.
	over, err := store.OverdueReviews(ctx, now.Add(25*time.Hour), 10)
	if err != nil || len(over) != 1 {
		t.Fatalf("overdue: %v rows err=%v", len(over), err)
	}
	if over, err := store.OverdueReviews(ctx, now, 10); err != nil || len(over) != 0 {
		t.Fatalf("pre-SLA overdue: %v", len(over))
	}
}

func TestITSelfCerts(t *testing.T) {
	ctx, pool := itPool(t)
	applyKYCSchema(t, ctx, pool)
	acct := mkAccount(t, ctx, pool, "T1")
	store, err := NewPgStore(pool, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, &fakeObjects{}, "kms", nil)

	cert, err := svc.SubmitSelfCert(ctx, SelfCertInput{
		AccountID: acct, FormType: "W-9", TIN: "123-45-6789",
		TINCountry: "US", TINKind: "SSN",
		Fields: []byte(`{"legal_name":"Jane Doe","address":"1 Main"}`)})
	if err != nil {
		t.Fatalf("self-cert: %v", err)
	}
	if cert.TINKind != "SSN" || cert.Status != "VALIDATED" {
		t.Fatalf("cert: %+v", cert)
	}
	ok, err := store.HasSelfCert(ctx, acct)
	if err != nil || !ok {
		t.Fatalf("has self-cert: %v err=%v", ok, err)
	}
	list, err := store.ListSelfCerts(ctx, acct)
	if err != nil || len(list) != 1 || string(list[0].TIN) != "123456789" {
		t.Fatalf("list: %+v err=%v", list, err)
	}

	// T0 account is gated out.
	acct2 := mkAccount(t, ctx, pool, "T0")
	_, err = svc.SubmitSelfCert(ctx, SelfCertInput{
		AccountID: acct2, FormType: "W-9", TIN: "123456789",
		TINCountry: "US", Fields: []byte(`{"legal_name":"B"}`)})
	if err == nil {
		t.Fatal("T0 self-cert must fail closed")
	}
}

// ssnShape matches a US SSN/EIN/ITIN-shaped digit run — what a pg_dump /
// raw-row reader would see if PII leaked.
var ssnShape = regexp.MustCompile(`\d{9}`)

// TestITSelfCertSealedAtRest is the PII-F1 proof: after a submit through
// the real service the raw row carries no plaintext TIN and no plaintext
// fields document — everything sensitive lives in the BYTEA blobs.
func TestITSelfCertSealedAtRest(t *testing.T) {
	ctx, pool := itPool(t)
	applyKYCSchema(t, ctx, pool)
	acct := mkAccount(t, ctx, pool, "T1")
	store, err := NewPgStore(pool, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, &fakeObjects{}, "kms", nil)

	if _, err := svc.SubmitSelfCert(ctx, SelfCertInput{
		AccountID: acct, FormType: "W-9", TIN: "123-45-6789",
		TINCountry: "US", TINKind: "SSN",
		Fields: []byte(`{"legal_name":"Jane Doe","address":"1 Main St"}`)}); err != nil {
		t.Fatalf("self-cert: %v", err)
	}

	// Raw-row inspection — what pg_dump surfaces.
	var tin *string
	var fieldsText string
	var tinSealed, fieldsSealed []byte
	if err := pool.QueryRow(ctx, `
		SELECT tin, fields::text, tin_sealed, fields_sealed
		  FROM tax_self_certifications WHERE account_id=$1`, acct).
		Scan(&tin, &fieldsText, &tinSealed, &fieldsSealed); err != nil {
		t.Fatalf("raw row: %v", err)
	}
	if tin != nil {
		t.Fatalf("plaintext tin persisted: %q", *tin)
	}
	if fieldsText != "{}" {
		t.Fatalf("plaintext fields persisted: %q", fieldsText)
	}
	if tinSealed == nil || fieldsSealed == nil {
		t.Fatal("sealed columns must be populated")
	}
	// The blobs themselves must not contain the SSN or the legal name —
	// GCM ciphertext is opaque, but assert it anyway (guards a future
	// "seal" regression that just copies bytes).
	if ssnShape.Match(tinSealed) {
		t.Fatal("tin_sealed contains an SSN-shaped digit run")
	}
	if string(fieldsSealed) == "" || len(fieldsSealed) < 30 {
		t.Fatalf("fields_sealed too small for nonce‖ct: %d bytes", len(fieldsSealed))
	}

	// Read path unseals — API contract unchanged for the owner.
	list, err := store.ListSelfCerts(ctx, acct)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v", err)
	}
	if list[0].TIN != "123456789" {
		t.Fatalf("unsealed tin: %q", list[0].TIN)
	}
	if string(list[0].Fields) == "" ||
		!strings.Contains(string(list[0].Fields), "Jane Doe") {
		t.Fatalf("unsealed fields: %s", list[0].Fields)
	}
}

// TestITSelfCertBackfill seeds a legacy plaintext row (the pre-210
// shape), runs SealTaxPIIBackfill, and asserts the row is sealed and the
// plaintext columns cleared — the exact migration-window transition.
func TestITSelfCertBackfill(t *testing.T) {
	ctx, pool := itPool(t)
	applyKYCSchema(t, ctx, pool)
	acct := mkAccount(t, ctx, pool, "T1")
	store, err := NewPgStore(pool, testBox(t))
	if err != nil {
		t.Fatal(err)
	}

	// Legacy row — what migration 205 wrote before 210 existed.
	if _, err := pool.Exec(ctx, `
		INSERT INTO tax_self_certifications
		    (account_id, form_type, tin, tin_country, tin_kind,
		     fields, status, tin_validated_at)
		VALUES ($1,'W-9','123456789','US','SSN',
		        '{"legal_name":"Legacy Name","address":"9 Old Rd"}'::jsonb,
		        'VALIDATED', now())`, acct); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	// And a second legacy row with no TIN (W-8BEN minimal).
	if _, err := pool.Exec(ctx, `
		INSERT INTO tax_self_certifications
		    (account_id, form_type, fields, status)
		VALUES ($1,'W-8BEN','{"legal_name":"No TIN"}'::jsonb,'SUBMITTED')`, acct); err != nil {
		t.Fatalf("seed legacy row 2: %v", err)
	}

	n, err := store.SealTaxPIIBackfill(ctx)
	if err != nil || n != 2 {
		t.Fatalf("backfill: n=%d err=%v", n, err)
	}
	// Idempotent — second run touches nothing.
	if n, err := store.SealTaxPIIBackfill(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill: n=%d err=%v", n, err)
	}

	// No plaintext left anywhere on the table.
	var leaks int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM tax_self_certifications
		 WHERE (tin IS NOT NULL AND tin <> '') OR fields <> '{}'::jsonb`).Scan(&leaks); err != nil {
		t.Fatal(err)
	}
	if leaks != 0 {
		t.Fatalf("%d rows still carry plaintext PII", leaks)
	}
	list, err := store.ListSelfCerts(ctx, acct)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v", err)
	}
	// Newest first: [0] is the W-8BEN (id 2), [1] the W-9 (id 1).
	if list[0].TIN != "" ||
		!strings.Contains(string(list[0].Fields), "No TIN") {
		t.Fatalf("backfilled no-TIN row: %+v", list[0])
	}
	if list[1].TIN != "123456789" ||
		!strings.Contains(string(list[1].Fields), "Legacy Name") {
		t.Fatalf("backfilled row unreadable: %+v", list[1])
	}
	// pg_dump-visibility assertion: no SSN-shaped digits in the raw row.
	var tinSealed []byte
	if err := pool.QueryRow(ctx, `
		SELECT tin_sealed FROM tax_self_certifications
		 WHERE form_type='W-9'`).Scan(&tinSealed); err != nil {
		t.Fatal(err)
	}
	if ssnShape.Match(tinSealed) {
		t.Fatal("tin_sealed exposes SSN-shaped digits")
	}
}

// TestITSelfCertUnseal covers the 210-down-migration pre-step:
// UnsealTaxPII restores the plaintext columns from the sealed blobs.
func TestITSelfCertUnseal(t *testing.T) {
	ctx, pool := itPool(t)
	applyKYCSchema(t, ctx, pool)
	acct := mkAccount(t, ctx, pool, "T1")
	store, err := NewPgStore(pool, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, &fakeObjects{}, "kms", nil)
	if _, err := svc.SubmitSelfCert(ctx, SelfCertInput{
		AccountID: acct, FormType: "W-9", TIN: "123-45-6789",
		TINCountry: "US", TINKind: "SSN",
		Fields: []byte(`{"legal_name":"Jane Doe"}`)}); err != nil {
		t.Fatalf("self-cert: %v", err)
	}
	n, err := store.UnsealTaxPII(ctx)
	if err != nil || n != 1 {
		t.Fatalf("unseal: n=%d err=%v", n, err)
	}
	var tin string
	var fieldsText string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(tin,''), fields::text
		  FROM tax_self_certifications WHERE account_id=$1`, acct).
		Scan(&tin, &fieldsText); err != nil {
		t.Fatal(err)
	}
	if tin != "123456789" || !strings.Contains(fieldsText, "Jane Doe") {
		t.Fatalf("restored plaintext wrong: tin=%q fields=%s", tin, fieldsText)
	}
	// Sealed columns remain (the down migration owns their drop) and the
	// read path still prefers them.
	list, err := store.ListSelfCerts(ctx, acct)
	if err != nil || len(list) != 1 || list[0].TIN != "123456789" {
		t.Fatalf("list after unseal: %+v err=%v", list, err)
	}
}
