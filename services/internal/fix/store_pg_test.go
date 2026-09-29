// PostgreSQL-gated PgStore tests (Task 18.3.1 persistence contract:
// sequence reload across restart, message archive for ResendRequest,
// transactional entitlement updates). Gated behind EXC_PG_TEST=1 like
// the other integration suites; needs migrations 030 + 046 applied.
//
//	EXC_PG_TEST=1 EXC_PG_DSN='postgres://...' go test ./internal/fix -run PGStore
package fix

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func pgDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("EXC_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://postgres@/w2d?host=/tmp&port=55433"
}

// pgStorePool applies 030+046 into a throwaway schema so the suite
// exercises the real DDL, never a hand-rolled approximation.
func pgStorePool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("fix_it_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, pgDSN())
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	// 046 references accounts/api_keys — minimal parents so the FKs bind.
	setup := `
SET search_path TO ` + schema + `;
CREATE TABLE accounts (id BIGINT PRIMARY KEY);
CREATE TABLE api_keys (id BIGINT PRIMARY KEY, key_id VARCHAR(64), key_hash VARCHAR(128), account_id BIGINT, status VARCHAR(16));
`
	if _, err := admin.Exec(ctx, setup); err != nil {
		t.Fatalf("parents: %v", err)
	}
	for _, mig := range []string{
		"030_create_fix_sessions.up.sql",
		"046_fix_sessions_entitlement.up.sql",
	} {
		body, err := os.ReadFile(filepath.Join("..", "db", "migrations", mig))
		if err != nil {
			t.Fatalf("read %s: %v", mig, err)
		}
		if _, err := admin.Exec(ctx, "SET search_path TO "+schema+"; "+string(body)); err != nil {
			t.Fatalf("apply %s: %v", mig, err)
		}
	}

	cfg, err := pgxpool.ParseConfig(pgDSN())
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), pgDSN())
		if err == nil {
			_, _ = c.Exec(context.Background(),
				"DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			c.Close(context.Background())
		}
	})
	return ctx, pool
}

func TestPGStore_SessionLifecycleAndSeq(t *testing.T) {
	ctx, pool := pgStorePool(t)
	st := NewPgStore(pool)

	sid := testSessionID().String()
	row, err := st.CreateSession(ctx, &Session{
		SessionID: sid, ProtocolVersion: "FIX.4.4",
		AllowedInstruments: "EUR/USD,GBP/USD", MaxMsgsPerSec: 50,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Status != "DISCONNECTED" || row.SenderSeqNum != 1 || row.TargetSeqNum != 1 {
		t.Fatalf("defaults: %+v", row)
	}
	// Duplicate create → ON CONFLICT return existing, not an error.
	dup, err := st.CreateSession(ctx, &Session{SessionID: sid})
	if err != nil || dup == nil {
		t.Fatalf("create idempotent: %v %v", dup, err)
	}

	now := time.Now().UTC()
	if err := st.SetStatus(ctx, sid, "ACTIVE", &now); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := st.SetSeq(ctx, sid, 9, 14); err != nil {
		t.Fatalf("setseq: %v", err)
	}
	s, tg, ok, err := st.SeqState(ctx, sid)
	if err != nil || !ok || s != 9 || tg != 14 {
		t.Fatalf("seqstate: %d %d %v %v", s, tg, ok, err)
	}

	// Simulate restart: a fresh Store instance over the same pool sees
	// persisted sequence state — the "resume without message loss"
	// guarantee (Task 18.3.1 item 3).
	st2 := NewPgStore(pool)
	s, tg, ok, err = st2.SeqState(ctx, sid)
	if err != nil || !ok || s != 9 || tg != 14 {
		t.Fatalf("seqstate after restart: %d %d %v %v", s, tg, ok, err)
	}
	got, err := st2.SessionByID(ctx, sid)
	if err != nil || got == nil || got.AllowedInstruments != "EUR/USD,GBP/USD" {
		t.Fatalf("reload: %+v %v", got, err)
	}
	if got.Status != "ACTIVE" || got.LastHeartbeatAt == nil {
		t.Fatalf("status persisted: %+v", got)
	}
}

func TestPGStore_MessageArchive(t *testing.T) {
	ctx, pool := pgStorePool(t)
	st := NewPgStore(pool)
	sid := "FIX.4.4:EXC->ARCH"
	if _, err := st.CreateSession(ctx, &Session{SessionID: sid}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for seq := int64(2); seq <= 5; seq++ {
		if err := st.SaveMessage(ctx, sid, seq,
			[]byte(fmt.Sprintf("8=FIX.4.49=%d35=8...", seq))); err != nil {
			t.Fatalf("save %d: %v", seq, err)
		}
	}
	msgs, err := st.Messages(ctx, sid, 3, 4)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("range: %d %v", len(msgs), err)
	}
	if string(msgs[0]) == "" || string(msgs[1]) == "" {
		t.Fatal("raw bytes must round-trip")
	}
	// Empty range → empty slice, never error.
	msgs, err = st.Messages(ctx, sid, 90, 95)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("empty range: %d %v", len(msgs), err)
	}
	if err := st.PurgeMessages(ctx, sid); err != nil {
		t.Fatalf("purge: %v", err)
	}
	msgs, _ = st.Messages(ctx, sid, 2, 5)
	if len(msgs) != 0 {
		t.Fatalf("purge left %d rows", len(msgs))
	}
}

func TestPGStore_EntitlementUpdateTx(t *testing.T) {
	ctx, pool := pgStorePool(t)
	st := NewPgStore(pool)
	sid := "FIX.4.4:EXC->ADM"
	if _, err := st.CreateSession(ctx, &Session{SessionID: sid}); err != nil {
		t.Fatalf("create: %v", err)
	}
	acct := int64(11)
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id) VALUES (11)`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	row, err := st.UpdateEntitlement(ctx, sid, EntitlementUpdate{
		AccountID:          &acct,
		ClearAPIKey:        true,
		AllowedInstruments: ptrStr("EUR/USD"),
		MaxMsgsPerSec:      ptrInt(25),
	})
	if err != nil {
		t.Fatalf("entitle: %v", err)
	}
	if row.AccountID == nil || *row.AccountID != 11 || row.APIKeyID != nil ||
		row.AllowedInstruments != "EUR/USD" || row.MaxMsgsPerSec != 25 {
		t.Fatalf("row: %+v", row)
	}
	// Partial patch touches ONLY the named fields.
	row, err = st.UpdateEntitlement(ctx, sid, EntitlementUpdate{
		CancelOnDisconnect: ptrBool(false),
	})
	if err != nil || row.MaxMsgsPerSec != 25 || row.CancelOnDisconnect {
		t.Fatalf("patch: %+v %v", row, err)
	}
	// Unknown session → nil, nil (handler maps to 404).
	row, err = st.UpdateEntitlement(ctx, "FIX.4.4:NO->PE", EntitlementUpdate{MaxMsgsPerSec: ptrInt(1)})
	if err != nil || row != nil {
		t.Fatalf("missing: %+v %v", row, err)
	}
}

func ptrStr(s string) *string { return &s }
func ptrInt(i int) *int       { return &i }
func ptrBool(b bool) *bool    { return &b }
