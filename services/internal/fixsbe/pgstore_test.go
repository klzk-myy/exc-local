package fixsbe

// EXC_PG_TEST=1 gated integration test — dev PostgreSQL :5433 with
// migration 228 applied. Covers PgSessionStore's hex-key lookup contract
// and LoadRegistry's state normalisation.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"exchange/internal/db"
)

func pgStore(t *testing.T) *PgSessionStore {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	pool, err := db.NewPool(context.Background(), dsn, 4)
	if err != nil {
		t.Fatalf("pg connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewPgSessionStore(pool)
}

func TestPgSessionStoreLookup(t *testing.T) {
	st := pgStore(t)
	ctx := context.Background()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	copy(key[:], pub)
	compID := "TEST_SBE_" + hex.EncodeToString(pub[:4])

	// Unknown key resolves to nil, not an error.
	got, err := st.SessionByPubKey(ctx, key)
	if err != nil || got != nil {
		t.Fatalf("unknown key: sess=%v err=%v", got, err)
	}

	_, err = st.pool.Exec(ctx, `
		INSERT INTO fixsbe_sessions
		    (session_key, comp_id, sni, order_encoding, report_encoding,
		     sbe_schema_id, sbe_schema_version, state)
		VALUES ($1, $2, 'sbe.example.com', 'SBE', 'SBE', 1, 1, 'ACTIVE')`,
		hex.EncodeToString(key[:]), compID)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(ctx,
			`DELETE FROM fixsbe_sessions WHERE comp_id = $1`, compID)
	})

	got, err = st.SessionByPubKey(ctx, key)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil || got.SessionID != compID ||
		got.SNIHostname != "sbe.example.com" || got.Status != "ACTIVE" {
		t.Fatalf("session row: %+v", got)
	}
	if got.PubKey != key {
		t.Fatal("pubkey not echoed back")
	}
}

func TestLoadRegistrySeededSchema(t *testing.T) {
	st := pgStore(t)
	reg, err := LoadRegistry(context.Background(), st.pool)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	neg, err := reg.Negotiate(1, 1, time.Now())
	if err != nil {
		t.Fatalf("negotiate seeded schema 1 v1: %v", err)
	}
	if neg.Deprecated {
		t.Fatal("seeded schema reported deprecated")
	}
	// Unknown version must fail closed.
	if _, err := reg.Negotiate(1, 99, time.Now()); err == nil {
		t.Fatal("unknown version negotiated — registry not fail-closed")
	}
}
