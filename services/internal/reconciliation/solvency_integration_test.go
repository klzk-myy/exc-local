// EXC_PG_TEST=1 gated integration tests — Task 13.3.7 solvency
// snapshot/proof persistence against a scratch-schema PostgreSQL.
//
//	PG: EXC_PG_DSN or postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable
//
// Run: EXC_PG_TEST=1 go test ./internal/reconciliation -run Solvency -v
package reconciliation

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

const solvTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

// solvFixture applies the migration subset solvency reads/writes (users →
// accounts → balances → nostro_accounts → 209 tables) into a throwaway
// schema and returns the store bound to it.
func solvFixture(t *testing.T) (*SolvencyPgStore, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = solvTestDSN
	}
	schema := fmt.Sprintf("solv_itest_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable (%v)", err)
	}
	if err := boot.Ping(ctx); err != nil {
		boot.Close(ctx)
		t.Skipf("postgres unreachable (%v)", err)
	}
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		conn, err := pgx.Connect(c2, dsn)
		if err == nil {
			_, _ = conn.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c2)
		}
	})
	boot.Close(ctx)

	for _, m := range []string{
		"002_create_users.up.sql",
		"003_create_accounts.up.sql",
		"004_create_balances.up.sql",
		"018_create_nostro_accounts.up.sql",
		"209_solvency_snapshots.up.sql",
	} {
		solvMigExec(t, ctx, dsn, schema, "../db/migrations/"+m)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	store, err := NewSolvencyStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return store, pool
}

// solvMigExec applies a migration file over the simple protocol
// (multi-statement scripts cannot ride pgx prepared statements).
func solvMigExec(t *testing.T, ctx context.Context, dsn, schema, file string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(ctx)
	sql, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}

func solvUserAccount(t *testing.T, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	var uid, aid int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
		uid).Scan(&aid); err != nil {
		t.Fatalf("account: %v", err)
	}
	return aid
}

func TestSolvencySnapshotRoundTrip(t *testing.T) {
	store, pool := solvFixture(t)
	ctx := context.Background()

	a1 := solvUserAccount(t, pool, "solv-a@x.test")
	a2 := solvUserAccount(t, pool, "solv-b@x.test")
	// a1: USD funded + zero-balance EUR row (must still be included);
	// a2: USD + a locked EUR balance.
	for _, stmt := range []struct {
		acct             int64
		ccy, avail, lock string
	}{
		{a1, "USD", "100.5", "0"},
		{a1, "EUR", "0", "0"},
		{a2, "USD", "50", "0"},
		{a2, "EUR", "7", "0.25"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO balances (account_id, currency, available, locked)
			 VALUES ($1,$2,$3,$4)`, stmt.acct, stmt.ccy, stmt.avail, stmt.lock); err != nil {
			t.Fatalf("balance: %v", err)
		}
	}
	for _, n := range []struct{ ccy, bal string }{
		{"USD", "300"}, {"EUR", "20"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO nostro_accounts (currency, bank_name, balance)
			 VALUES ($1,'Test Correspondent',$2)`, n.ccy, n.bal); err != nil {
			t.Fatalf("nostro: %v", err)
		}
	}

	svc, err := NewSolvencyService(store, store, DevHMACSigner{Key: []byte("itest")})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	snap, err := svc.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if snap.LeafCount != 4 || !snap.Solvent {
		t.Fatalf("snap: leaves=%d solvent=%t", snap.LeafCount, snap.Solvent)
	}
	if snap.Liabilities["USD"] != "150.5" || snap.Liabilities["EUR"] != "7.25" {
		t.Fatalf("liabilities=%v", snap.Liabilities)
	}
	if snap.SignerKind != SignerKindDevHMAC {
		t.Fatalf("signer kind=%s", snap.SignerKind)
	}

	// LatestSnapshot round-trips the persisted row.
	latest, err := store.LatestSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if latest.ID != snap.ID || latest.MerkleRoot != snap.MerkleRoot ||
		latest.Signature != snap.Signature {
		t.Fatalf("latest %+v", latest)
	}

	// ProofFor returns a cryptographically valid proof — re-verify
	// client-side from the wire fields alone.
	proof, err := store.ProofFor(ctx, a2, "EUR")
	if err != nil {
		t.Fatalf("ProofFor: %v", err)
	}
	// balance::text carries the DECIMAL(28,8) scale ("7.25000000") — the
	// leaf preimage normalizes via decimal.String(), so compare
	// numerically.
	if proof.AccountID != a2 || proof.MerkleRoot != snap.MerkleRoot {
		t.Fatalf("proof %+v", proof)
	}
	var saltB [SaltBytes]byte
	sb, err := hex.DecodeString(proof.Salt)
	if err != nil {
		t.Fatal(err)
	}
	copy(saltB[:], sb)
	steps := make([]ProofStep, len(proof.Path))
	for i, s := range proof.Path {
		h, err := ParseHexDigest(s.Hash)
		if err != nil {
			t.Fatalf("path hash: %v", err)
		}
		steps[i] = ProofStep{Hash: h, Right: s.Position == "right"}
	}
	bal, err := decimal.NewFromString(proof.Balance)
	if err != nil {
		t.Fatal(err)
	}
	leaf := LeafInput{AccountID: a2, Currency: "EUR", Balance: bal, Salt: saltB}
	root, _ := ParseHexDigest(snap.MerkleRoot)
	if !Verify(leaf, steps, root) {
		t.Fatal("persisted proof does not verify client-side")
	}
	// leaf_hash column matches the recomputed digest.
	if proof.LeafHash != HexDigest(leafDigest(leaf)) {
		t.Fatal("leaf_hash column mismatch")
	}

	// ProofsForAccount returns both currencies for a2.
	proofs, err := store.ProofsForAccount(ctx, a2)
	if err != nil || len(proofs) != 2 {
		t.Fatalf("ProofsForAccount: %v n=%d", err, len(proofs))
	}

	// Zero-balance leaf exists and is fetchable.
	zp, err := store.ProofFor(ctx, a1, "EUR")
	if err != nil {
		t.Fatalf("zero-balance proof: %v", err)
	}
	zb, _ := decimal.NewFromString(zp.Balance)
	if !zb.IsZero() {
		t.Fatalf("zero-balance proof balance=%s", zp.Balance)
	}

	// Foreign/absent leaf → NOT_FOUND.
	if _, err := store.ProofFor(ctx, a1, "JPY"); err == nil {
		t.Fatal("absent currency must be NOT_FOUND")
	}
	if _, err := store.ProofFor(ctx, a2+999, "USD"); err == nil {
		t.Fatal("foreign account must be NOT_FOUND")
	}

	// Atomicity probe: proof rows equal leaf_count exactly.
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM solvency_proofs WHERE snapshot_id=$1`, snap.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("proof rows=%d, want 4", n)
	}
}

func TestSolvencyNegativeBalanceAbortsPG(t *testing.T) {
	store, pool := solvFixture(t)
	ctx := context.Background()
	a1 := solvUserAccount(t, pool, "solv-neg@x.test")
	if _, err := pool.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available, locked)
		 VALUES ($1,'USD','-5','0')`, a1); err != nil {
		t.Fatalf("balance: %v", err)
	}
	svc, err := NewSolvencyService(store, store, DevHMACSigner{Key: []byte("itest")})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if _, err := svc.Generate(ctx); err == nil {
		t.Fatal("negative liability must abort generation")
	}
	var snaps int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM solvency_snapshots`).Scan(&snaps); err != nil {
		t.Fatal(err)
	}
	if snaps != 0 {
		t.Fatal("aborted run published a snapshot")
	}
}
