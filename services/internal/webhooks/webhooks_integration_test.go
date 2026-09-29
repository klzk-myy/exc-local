// Task 5.3.17 — PG-backed webhook pipeline tests. Gated:
// EXC_PG_TEST=1, DSN via EXC_PG_DSN or EXC_TEST_DSN (default: scratch w2d, /tmp socket).
//
// Applies the real 180 migration plus a minimal mirrored accounts table
// (180's FK only references accounts.id).
//
// Run: EXC_PG_TEST=1 go test ./internal/webhooks/ -run Integration -v
package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func whTestDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("EXC_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://postgres@/w2d?host=/tmp&port=55433"
}

func whItest(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("wh_itest_%d", time.Now().UnixNano())

	cfg, err := pgx.ParseConfig(whTestDSN())
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	ddl, err := os.ReadFile("../db/migrations/180_webhooks.up.sql")
	if err != nil {
		conn.Close(ctx)
		t.Fatalf("read migration 180: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`CREATE TABLE accounts (id BIGINT PRIMARY KEY)`); err != nil {
		conn.Close(ctx)
		t.Fatalf("accounts ddl: %v", err)
	}
	if _, err := conn.Exec(ctx, string(ddl)); err != nil {
		conn.Close(ctx)
		t.Fatalf("apply 180: %v", err)
	}
	for _, id := range []int{1, 2} {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf(`INSERT INTO accounts (id) VALUES (%d)`, id)); err != nil {
			conn.Close(ctx)
			t.Fatalf("seed account: %v", err)
		}
	}
	conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(whTestDSN())
	if err != nil {
		t.Fatalf("pool dsn: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(ctx, whTestDSN())
		if err == nil {
			_, _ = c.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			c.Close(ctx)
		}
	})
	st, err := NewStore(pool, fakeBox{})
	if err != nil {
		t.Fatal(err)
	}
	return st, pool, ctx
}

// Register → secret sealed in DB (never raw); list; ownership scoping.
func TestIntegrationRegisterAndList(t *testing.T) {
	st, pool, ctx := whItest(t)

	ep, secret, err := st.Register(ctx, 1, 99,
		"https://hooks.example.com/order", []string{"order_filled"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if secret == "" || ep.Status != StatusActive {
		t.Fatalf("ep=%+v secret empty=%v", ep, secret == "")
	}
	// The stored envelope must NOT contain the raw secret.
	var enc []byte
	if err := pool.QueryRow(ctx,
		`SELECT secret_enc FROM webhook_endpoints WHERE id=$1`, ep.ID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if string(enc) == secret {
		t.Fatal("raw secret stored")
	}
	// fakeBox round-trip proves the envelope is openable.
	if got, err := (fakeBox{}).Open(enc); err != nil || string(got) != secret {
		t.Fatalf("sealed secret unrecoverable: %v", err)
	}

	// Owner lists it; foreign account does not.
	own, err := st.List(ctx, 1)
	if err != nil || len(own) != 1 {
		t.Fatalf("owner list=%v err=%v", own, err)
	}
	foreign, err := st.List(ctx, 2)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign list=%v err=%v", foreign, err)
	}

	// Disable: foreign account gets the opaque not-found; owner succeeds.
	if err := st.Disable(ctx, ep.EndpointID, 2); !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("foreign disable err=%v want ErrEndpointNotFound", err)
	}
	if err := st.Disable(ctx, ep.EndpointID, 1); err != nil {
		t.Fatalf("owner disable: %v", err)
	}
	got, err := st.Get(ctx, ep.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDisabled || got.DisabledAt == nil {
		t.Fatalf("disable not recorded: %+v", got)
	}
	// Unknown endpoint id is indistinguishable.
	if err := st.Disable(ctx, "wh_nonexistent", 1); !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("unknown disable err=%v", err)
	}
}

// Publish → Enqueue fans out only to ACTIVE endpoints subscribed to the
// event; ClaimDue leases; CompleteDelivery lands DELIVERED.
func TestIntegrationEnqueueClaimComplete(t *testing.T) {
	st, pool, ctx := whItest(t)

	_, _, err := st.Register(ctx, 1, 99,
		"https://a.example.com/h", []string{"order_filled"})
	if err != nil {
		t.Fatal(err)
	}
	// Endpoint subscribed to a different event only.
	_, _, err = st.Register(ctx, 1, 99,
		"https://b.example.com/h", []string{"deposit_confirmed"})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(st, nil, time.Hour)
	n, err := d.Publish(ctx, 1, "order_filled", map[string]any{"order_id": 7})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fan-out=%d want 1 (only subscribed endpoint)", n)
	}
	// Unknown event rejected.
	if _, err := d.Publish(ctx, 1, "price_tick", nil); err == nil {
		t.Fatal("unknown event published")
	}

	due, err := st.ClaimDue(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Attempts != 1 {
		t.Fatalf("claimed=%+v", due)
	}
	// Claimed rows are leased — a second claim sees nothing.
	if again, _ := st.ClaimDue(ctx, 10); len(again) != 0 {
		t.Fatalf("double-claim: %+v", again)
	}
	if err := st.CompleteDelivery(ctx, due[0].ID, 200); err != nil {
		t.Fatal(err)
	}
	var status string
	var code int
	if err := pool.QueryRow(ctx,
		`SELECT status, last_status_code FROM webhook_deliveries WHERE id=$1`,
		due[0].ID).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != DeliveryDelivered || code != 200 {
		t.Fatalf("status=%s code=%d", status, code)
	}
}

// FailDelivery reschedules while attempts < max; the attempt that
// reaches max_attempts lands DEAD_LETTERED.
func TestIntegrationDeadLetter(t *testing.T) {
	st, pool, ctx := whItest(t)
	ep, _, err := st.Register(ctx, 1, 99,
		"https://dead.example.com/h", []string{"order_filled"})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(st, nil, time.Hour)
	if _, err := d.Publish(ctx, 1, "order_filled", nil); err != nil {
		t.Fatal(err)
	}

	// Simulate four prior failures: attempts=4, due now.
	if _, err := pool.Exec(ctx, `
		UPDATE webhook_deliveries SET attempts=4, next_attempt_at=now()
		 WHERE endpoint_id=$1`, ep.ID); err != nil {
		t.Fatal(err)
	}
	due, err := st.ClaimDue(ctx, 10)
	if err != nil || len(due) != 1 || due[0].Attempts != 5 {
		t.Fatalf("claim=%v err=%v", due, err)
	}
	// Fifth attempt fails → dead-lettered.
	if err := st.FailDelivery(ctx, due[0].ID, 500, "http 500",
		time.Now().Add(backoffFor(due[0].Attempts-1))); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM webhook_deliveries WHERE id=$1`, due[0].ID).
		Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != DeliveryDeadLettered {
		t.Fatalf("status=%s want DEAD_LETTERED", status)
	}
	// Dead-lettered rows are never claimed again.
	if more, _ := st.ClaimDue(ctx, 10); len(more) != 0 {
		t.Fatal("dead-lettered row re-claimed")
	}
	// The delivery log is queryable per endpoint (owner only).
	log, err := st.ListDeliveries(ctx, ep.EndpointID, 1, 10)
	if err != nil || len(log) != 1 {
		t.Fatalf("delivery log=%v err=%v", log, err)
	}
	if _, err := st.ListDeliveries(ctx, ep.EndpointID, 2, 10); !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("foreign delivery log err=%v want ErrEndpointNotFound", err)
	}
}

// RotateSecret keeps the predecessor inside the overlap window;
// SecretsForDelivery exposes it; >72h overlap is rejected.
func TestIntegrationSecretRotation(t *testing.T) {
	st, _, ctx := whItest(t)
	ep, oldSecret, err := st.Register(ctx, 1, 99,
		"https://rot.example.com/h", []string{"order_filled"})
	if err != nil {
		t.Fatal(err)
	}
	newSecret, err := st.RotateSecret(ctx, ep.EndpointID, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if newSecret == oldSecret {
		t.Fatal("rotation returned the same secret")
	}
	cur, prev, err := st.SecretsForDelivery(ctx, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != newSecret || string(prev) != oldSecret {
		t.Fatal("rotation secrets wrong")
	}
	// Zero overlap → no predecessor.
	if _, err := st.RotateSecret(ctx, ep.EndpointID, 1, 0); err != nil {
		t.Fatal(err)
	}
	_, prev, err = st.SecretsForDelivery(ctx, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prev) != 0 {
		t.Fatal("predecessor exposed past overlap")
	}
	// Foreign account cannot rotate.
	if _, err := st.RotateSecret(ctx, ep.EndpointID, 2, time.Hour); !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("foreign rotate err=%v want ErrEndpointNotFound", err)
	}
}

// End-to-end: real dispatcher → httptest receiver. Verifies the wire
// contract — HMAC over "ts.body", all four headers, 2xx → DELIVERED.
func TestIntegrationDispatcherDelivers(t *testing.T) {
	st, pool, ctx := whItest(t)

	var gotSig, gotTs, gotEvent, gotID string
	var gotBody []byte
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotSig = r.Header.Get(HeaderSignature)
		gotTs = r.Header.Get(HeaderTimestamp)
		gotEvent = r.Header.Get(HeaderEvent)
		gotID = r.Header.Get(HeaderDeliveryID)
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ep, secret, err := st.Register(ctx, 1, 99, srv.URL,
		[]string{"order_filled"})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(st, srv.Client(), time.Hour)
	if _, err := d.Publish(ctx, 1, "order_filled",
		map[string]any{"order_id": 42}); err != nil {
		t.Fatal(err)
	}
	if n := d.Drain(ctx); n != 1 {
		t.Fatalf("drained=%d want 1", n)
	}
	if calls.Load() != 1 {
		t.Fatalf("server calls=%d", calls.Load())
	}
	// Verify the signature independently: hex(HMAC-SHA256(secret, "ts.body")).
	tsInt, err := strconv.ParseInt(gotTs, 10, 64)
	if err != nil {
		t.Fatalf("bad ts header %q", gotTs)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(gotTs))
	mac.Write([]byte("."))
	mac.Write(gotBody)
	if hex.EncodeToString(mac.Sum(nil)) != gotSig {
		t.Fatal("signature mismatch")
	}
	if tsInt <= 0 || gotEvent != "order_filled" || gotID == "" {
		t.Fatalf("headers incomplete: sig=%s ts=%s ev=%s id=%s",
			gotSig, gotTs, gotEvent, gotID)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM webhook_deliveries WHERE endpoint_id=$1`, ep.ID).
		Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != DeliveryDelivered {
		t.Fatalf("status=%s want DELIVERED", status)
	}
}

// A failing receiver: the delivery stays PENDING with a rescheduled
// next_attempt_at and the last_error recorded.
func TestIntegrationDispatcherRetry(t *testing.T) {
	st, _, ctx := whItest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, _, err := st.Register(ctx, 1, 99, srv.URL,
		[]string{"order_filled"}); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(st, srv.Client(), time.Hour)
	if _, err := d.Publish(ctx, 1, "order_filled", nil); err != nil {
		t.Fatal(err)
	}
	if n := d.Drain(ctx); n != 1 {
		t.Fatalf("drained=%d", n)
	}
	log, err := st.ListDeliveries(ctx,
		mustEndpoint(t, ctx, st), 1, 10)
	if err != nil || len(log) != 1 {
		t.Fatalf("log=%v err=%v", log, err)
	}
	dl := log[0]
	if dl.Status != DeliveryPending || dl.Attempts != 1 {
		t.Fatalf("delivery=%+v want PENDING attempts=1", dl)
	}
	if dl.LastStatusCode == nil || *dl.LastStatusCode != 500 {
		t.Fatalf("last_status_code=%v", dl.LastStatusCode)
	}
	if dl.LastError == nil || *dl.LastError == "" {
		t.Fatal("last_error not recorded")
	}
	if !dl.NextAttemptAt.After(time.Now()) {
		t.Fatal("retry not rescheduled")
	}
}

// mustEndpoint resolves the single endpoint for account 1 (test helper).
func mustEndpoint(t *testing.T, ctx context.Context, st *Store) string {
	t.Helper()
	eps, err := st.List(ctx, 1)
	if err != nil || len(eps) == 0 {
		t.Fatalf("no endpoint: %v", err)
	}
	return eps[0].EndpointID
}

// Phase-14 Task 14.3.12 — dead-letter review + manual retransmit.
// Retransmit requeues the delivery PENDING with a fresh attempt budget
// and writes admin_audit_log in the same transaction; the list view is
// the officer's dead-letter surface.
func TestIntegrationDeadLetterRetransmit(t *testing.T) {
	st, pool, ctx := whItest(t)
	// Retransmit audits into admin_audit_log — mirror the minimal shape
	// in the scratch schema (production table: migration 010).
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS admin_audit_log (
		    id            BIGSERIAL PRIMARY KEY,
		    admin_user_id BIGINT NOT NULL,
		    action        VARCHAR(128) NOT NULL,
		    target_type   VARCHAR(64),
		    target_id     BIGINT,
		    before_state  JSONB,
		    after_state   JSONB,
		    ip_address    INET,
		    created_at    TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatalf("audit ddl: %v", err)
	}
	ep, _, err := st.Register(ctx, 1, 99,
		"https://dead.example.com/h", []string{"order_filled"})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(st, nil, time.Hour)
	if _, err := d.Publish(ctx, 1, "order_filled", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE webhook_deliveries SET attempts=4, next_attempt_at=now()
		 WHERE endpoint_id=$1`, ep.ID); err != nil {
		t.Fatal(err)
	}
	due, err := st.ClaimDue(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim=%v err=%v", due, err)
	}
	if err := st.FailDelivery(ctx, due[0].ID, 500, "http 500",
		time.Now().Add(backoffFor(due[0].Attempts-1))); err != nil {
		t.Fatal(err)
	}

	// Dead-letter list surfaces exactly the exhausted delivery.
	dls, err := st.ListDeadLetters(ctx, 10)
	if err != nil || len(dls) != 1 {
		t.Fatalf("dead letters=%v err=%v", dls, err)
	}
	if dls[0].Status != DeliveryDeadLettered ||
		dls[0].DeliveryID != due[0].DeliveryID {
		t.Fatalf("dead letter row: %+v", dls[0])
	}

	// Unknown / not-dead-lettered ids miss with ErrDeliveryNotFound.
	if _, err := st.Retransmit(ctx, "whd_nope", 9); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("missing retransmit err=%v want ErrDeliveryNotFound", err)
	}

	// Retransmit: PENDING, fresh budget, claimable again, audit row.
	rd, err := st.Retransmit(ctx, due[0].DeliveryID, 9)
	if err != nil {
		t.Fatalf("retransmit: %v", err)
	}
	if rd.Status != DeliveryPending || rd.Attempts != 0 {
		t.Fatalf("retransmit row: %+v", rd)
	}
	var auditN int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log
		  WHERE action='webhook.retransmit' AND target_id=$1`,
		rd.ID).Scan(&auditN); err != nil || auditN != 1 {
		t.Fatalf("audit rows=%d err=%v", auditN, err)
	}
	again, err := st.ClaimDue(ctx, 10)
	if err != nil || len(again) != 1 {
		t.Fatalf("re-claim=%v err=%v", again, err)
	}
	// Re-transmitting a live (non-dead-lettered) delivery misses.
	if _, err := st.Retransmit(ctx, due[0].DeliveryID, 9); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("non-DL retransmit err=%v want ErrDeliveryNotFound", err)
	}
}
