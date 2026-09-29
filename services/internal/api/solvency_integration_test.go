// EXC_PG_TEST=1 gated — Task 13.3.7 proof endpoint against the real
// SolvencyPgStore + a generated snapshot (scratch schema).
//
// Run: EXC_PG_TEST=1 go test ./internal/api -run SolvencyEndpoint -v
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/auth"
	"exchange/internal/reconciliation"
)

const solvAPITestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

func solvAPIFixture(t *testing.T) (*reconciliation.SolvencyPgStore, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = solvAPITestDSN
	}
	schema := fmt.Sprintf("solvapi_itest_%d", time.Now().UnixNano())
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
		sql, err := os.ReadFile("../db/migrations/" + m)
		if err != nil {
			conn.Close(ctx)
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			conn.Close(ctx)
			t.Fatalf("apply %s: %v", m, err)
		}
		conn.Close(ctx)
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

	store, err := reconciliation.NewSolvencyStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return store, pool
}

// TestSolvencyEndpointPG runs the real chain: fixture rows → Generate →
// the HTTP handler → caller-scoped proof, plus the foreign-account gate.
func TestSolvencyEndpointPG(t *testing.T) {
	store, pool := solvAPIFixture(t)
	ctx := context.Background()

	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("solvapi-%d@x.test", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
		uid).Scan(&aid); err != nil {
		t.Fatalf("account: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available, locked)
		 VALUES ($1,'USD','12.5','0')`, aid); err != nil {
		t.Fatalf("balance: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO nostro_accounts (currency, bank_name, balance)
		 VALUES ('USD','Test Bank','100')`); err != nil {
		t.Fatalf("nostro: %v", err)
	}

	svc, err := reconciliation.NewSolvencyService(store, store,
		reconciliation.DevHMACSigner{Key: []byte("itest")})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if _, err := svc.Generate(ctx); err != nil {
		t.Fatalf("generate: %v", err)
	}

	claims := &auth.Claims{Subject: fmt.Sprint(uid), AccountID: aid, Scopes: []string{"read"}}

	// Owner fetch → 200 with the salted leaf + sibling path.
	r := httptest.NewRequest("GET",
		fmt.Sprintf("/api/v1/solvency/proof?account_id=%d&currency=USD", aid), nil)
	r = r.WithContext(auth.WithClaims(r.Context(), *claims))
	rec := httptest.NewRecorder()
	SolvencyProof(store).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("own proof code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"leaf_hash"`, `"salt"`, `"path"`, `"merkle_root"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}

	// Foreign account_id → 403.
	r = httptest.NewRequest("GET",
		fmt.Sprintf("/api/v1/solvency/proof?account_id=%d&currency=USD", aid+999), nil)
	r = r.WithContext(auth.WithClaims(r.Context(), *claims))
	rec = httptest.NewRecorder()
	SolvencyProof(store).ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign proof code=%d", rec.Code)
	}

	// Public latest → 200 with root + signature.
	rec = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/v1/solvency/latest", nil)
	SolvencyLatest(store).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"solvent":true`) {
		t.Fatalf("latest code=%d body=%s", rec.Code, rec.Body)
	}

	// Account-level convenience → proofs array covering USD.
	r = httptest.NewRequest("GET", "/api/v1/account/solvency-proof", nil)
	r = r.WithContext(auth.WithClaims(r.Context(), *claims))
	rec = httptest.NewRecorder()
	AccountSolvencyProof(store).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"USD"`) {
		t.Fatalf("account proofs code=%d body=%s", rec.Code, rec.Body)
	}
}
