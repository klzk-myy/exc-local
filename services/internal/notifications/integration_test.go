// EXC_PG_TEST=1 gated integration tests — dev PostgreSQL + live Redis.
//
//	PG:    EXC_PG_DSN or postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable
//	Redis: EXC_REDIS_TEST_ADDR or 127.0.0.1:16379 (compose dev primary);
//	       EXC_REDIS_TEST_PASSWORD optional.
//
// Run: EXC_PG_TEST=1 go test ./internal/notifications -v
package notifications

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	excredis "exchange/internal/redis"
)

const defaultTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
const defaultTestRedis = "127.0.0.1:16379"

func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres/Redis integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pg connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("postgres unreachable at %s: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testRedis(t *testing.T) *excredis.Client {
	t.Helper()
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = defaultTestRedis
	}
	// DB 15, not the gateway's DB 0 — the live notification worker
	// drains notifications:pending on db 0 and races this test's queue
	// assertions (observed: pending=0 flake). EXC_REDIS_TEST_DB
	// overrides for environments where 15 is already claimed.
	db := 15
	if v := os.Getenv("EXC_REDIS_TEST_DB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			db = n
		}
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), db)
	if err := rdb.Ping(context.Background()); err != nil {
		t.Skipf("redis unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// mkUser creates a throwaway user row for FK-bound tests.
func mkUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"notif-test-"+time.Now().Format("20060102150405.000000")+"@x.test").Scan(&id)
	if err != nil {
		t.Fatalf("mkUser: %v", err)
	}
	t.Cleanup(func() {
		// Children first (FK-bound).
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM notification_dead_letters WHERE user_id=$1`, id)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM notification_deliveries WHERE user_id=$1`, id)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM notification_preferences WHERE user_id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id)
	})
	return id
}

func TestPgPreferencesRoundTrip(t *testing.T) {
	pool := pgPool(t)
	st := NewPgStore(pool)
	uid := mkUser(t, pool)
	ctx := context.Background()

	if p, err := st.GetPreferences(ctx, uid); err != nil || p != nil {
		t.Fatalf("empty read: p=%v err=%v", p, err)
	}
	p := &Preferences{
		UserID: uid,
		Matrix: map[string]map[string]bool{
			EventDepositConfirmed: {ChannelEmail: false, ChannelPush: true},
		},
		Quiet: QuietHours{Enabled: true, Start: "22:00", End: "07:00"},
	}
	out, err := st.PutPreferences(ctx, p)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if out.UpdatedAt.IsZero() {
		t.Fatal("updated_at not stamped")
	}
	got, err := st.GetPreferences(ctx, uid)
	if err != nil || got == nil {
		t.Fatalf("get: %v p=%v", err, got)
	}
	if got.Enabled(EventDepositConfirmed, ChannelEmail) {
		t.Fatal("matrix disable lost")
	}
	if !got.Enabled(EventDepositConfirmed, ChannelPush) {
		t.Fatal("matrix enable lost")
	}
	if got.Quiet.Start != "22:00" || got.Quiet.End != "07:00" || !got.Quiet.Enabled {
		t.Fatalf("quiet hours round-trip failed: %+v", got.Quiet)
	}
	// Upsert replaces.
	p.Quiet = QuietHours{Enabled: false}
	if _, err := st.PutPreferences(ctx, p); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	got, _ = st.GetPreferences(ctx, uid)
	if got.Quiet.Enabled {
		t.Fatal("quiet disable lost")
	}
	// Validation surfaces through the store too.
	bad := &Preferences{UserID: uid, Matrix: map[string]map[string]bool{
		"nonsense": {ChannelEmail: true}}}
	if _, err := st.PutPreferences(ctx, bad); err == nil {
		t.Fatal("invalid matrix stored")
	}
}

func TestPgDeliveryLifecycleAndDeadLetter(t *testing.T) {
	pool := pgPool(t)
	st := NewPgStore(pool)
	uid := mkUser(t, pool)
	ctx := context.Background()

	id, err := st.InsertDelivery(ctx, Delivery{
		UserID: uid, Channel: ChannelEmail, Event: EventDepositConfirmed,
		Payload: []byte(`{"amount":"10.00","currency":"USD"}`),
		Status:  StatusQueued, MaxAttempts: MaxAttempts})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.RecordAttempt(ctx, id, 3, "provider timeout"); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if err := st.DeadLetter(ctx, id, MaxAttempts, "provider timeout"); err != nil {
		t.Fatalf("dead letter: %v", err)
	}
	var status string
	var dlCount int
	if err := pool.QueryRow(ctx,
		`SELECT status FROM notification_deliveries WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusDeadLettered {
		t.Fatalf("status=%s", status)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notification_dead_letters WHERE delivery_id=$1`, id).Scan(&dlCount); err != nil {
		t.Fatal(err)
	}
	if dlCount != 1 {
		t.Fatalf("dead_letters rows=%d", dlCount)
	}
	// Delivered path on a second leg.
	id2, _ := st.InsertDelivery(ctx, Delivery{
		UserID: uid, Channel: ChannelWS, Event: EventOrderFilled,
		Payload: []byte(`{}`), Status: StatusQueued})
	now := time.Now().UTC()
	if err := st.MarkDelivered(ctx, id2, 1, now); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	rows, err := st.RecentDeliveries(ctx, uid, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("recent=%v err=%v", rows, err)
	}
}

func TestPgRecipientAndAntiPhish(t *testing.T) {
	pool := pgPool(t)
	st := NewPgStore(pool)
	uid := mkUser(t, pool)
	email, phone, err := st.Recipient(context.Background(), uid)
	if err != nil {
		t.Fatalf("recipient: %v", err)
	}
	if email == "" {
		t.Fatal("recipient email empty")
	}
	_ = phone
	// Anti-phish: column may not exist yet (sibling task) — the seam
	// must return unset-or-code, never a hard error either way.
	code, err := PgAntiPhish{Pool: pool}.Code(context.Background(), uid)
	if err != nil {
		t.Fatalf("antiphish lookup errored (should degrade to unset): %v", err)
	}
	_ = code // "" when column/row unset — banner path
}

func TestRedisQueueRoundTrip(t *testing.T) {
	rdb := testRedis(t)
	q := NewQueue(rdb.Client)
	ctx := context.Background()
	// Isolate: flush the three keys.
	_ = rdb.Del(ctx, PendingKey, ProcessingKey, RetryKey).Err()
	t.Cleanup(func() {
		_ = rdb.Del(ctx, PendingKey, ProcessingKey, RetryKey).Err()
	})

	item := QueueItem{DeliveryID: 42, UserID: 7, Channel: ChannelEmail,
		Event: EventOrderFilled, Payload: []byte(`{"order_id":9}`)}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	n, _ := q.PendingLen(ctx)
	if n != 1 {
		t.Fatalf("pending=%d", n)
	}
	got, raw, ok, err := q.Pop(ctx, 2*time.Second)
	if err != nil || !ok {
		t.Fatalf("pop: ok=%v err=%v", ok, err)
	}
	if got.DeliveryID != 42 || got.Event != EventOrderFilled {
		t.Fatalf("item=%+v", got)
	}
	// Claimed → not in pending; ack removes from processing.
	if err := q.Ack(ctx, raw); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, _ := rdb.LLen(ctx, ProcessingKey).Result(); n != 0 {
		t.Fatalf("processing=%d after ack", n)
	}
	// Crash recovery: pop again → don't ack → RequeueAll restores it.
	_, _, ok, _ = q.Pop(ctx, time.Second)
	_ = q.Enqueue(ctx, item)             // second pending copy for recovery test
	_, _, _, _ = q.Pop(ctx, time.Second) // claim it into processing (no ack)
	moved, err := q.RequeueAll(ctx)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if moved < 1 {
		t.Fatalf("requeued=%d want ≥1", moved)
	}
	// Retry scheduling + promote.
	past := time.Now().Add(-time.Second)
	if err := q.ScheduleRetry(ctx, item, past); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	promoted, err := q.PromoteDue(ctx, time.Now(), 10)
	if err != nil || promoted != 1 {
		t.Fatalf("promoted=%d err=%v", promoted, err)
	}
}
