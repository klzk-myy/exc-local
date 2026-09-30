// End-to-end timing harness for Task 18.3.12 AC:
// "Full failover and sequence re-synchronization completes in <5s."
//
// Two live FIX gateways can't be simulated on this host, so the test
// exercises the exact store/service seam the row describes — the real
// Redis coordination path (seqClaimScript + seqCASScript Lua on
// 127.0.0.1:16379) layered over the real fix_sessions/fix_messages
// PostgreSQL archive (127.0.0.1:5433, migration 030 in a scratch
// schema):
//
//	gw-primary claims + runs the session, archives outbound frames,
//	dies (lease lapses) → client TCP lands on gw-secondary →
//	ResumeOnLogon (Claim + Load) → ResendRequest issued →
//	PossDup replays consumed with ClOrdID dedup →
//	client ResendRequest → PG archive fetch → PlanResend ready.
//
// Wall time is measured in two bands:
//   - resume: secondary claim → resync plan + gap-fill stream ready;
//   - total:  primary death (lease expiry included) → the same point.
//
// Gated on EXC_REDIS_TEST=1 and EXC_PG_TEST=1 (repo convention).
//
// Run: EXC_REDIS_TEST=1 EXC_PG_TEST=1 go test ./internal/fix/ -run FailoverE2E -v
package fix

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

const failoverBound = 5 * time.Second

// failoverPGPool builds a scratch schema with only migration 030
// (fix_sessions + fix_messages) — the failover archive surface. The
// entitlement columns from 046 are irrelevant to this path.
func failoverPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("EXC_PG_DSN")
	}
	if dsn == "" {
		dsn = fixTestDSN
	}
	schema := fmt.Sprintf("fix_foe2e_%d", time.Now().UnixNano())

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	admin, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Skipf("postgres unreachable at %s: %v", dsn, err)
	}
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("schema conn: %v", err)
	}
	defer conn.Close(context.Background())
	sql, err := os.ReadFile("../db/migrations/030_create_fix_sessions.up.sql")
	if err != nil {
		t.Fatalf("read migration 030: %v", err)
	}
	if _, err := conn.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("apply migration 030: %v", err)
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), func() *pgxpool.Config {
		c, cerr := pgxpool.ParseConfig(dsn)
		if cerr != nil {
			t.Fatalf("pool cfg: %v", cerr)
		}
		c.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
		return c
	}())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// pgArchiver is the OutboundArchiver production binding described in
// gapfill.go: Store.Messages returns the raw archived frames and the
// caller peels MsgSeqNum(34)/MsgType(35)/SendingTime(52) off the wire
// bytes.
type pgArchiver struct{ st *PgStore }

func (a pgArchiver) Fetch(ctx context.Context, sessionID string, beginSeq, endSeq int64) ([]ArchivedMessage, error) {
	raws, err := a.st.Messages(ctx, sessionID, beginSeq, endSeq)
	if err != nil {
		return nil, err
	}
	out := make([]ArchivedMessage, 0, len(raws))
	for _, raw := range raws {
		m, err := peelArchived(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// peelArchived extracts 34/35/52 from a raw SOH-delimited frame.
func peelArchived(raw []byte) (ArchivedMessage, error) {
	m := ArchivedMessage{Raw: raw}
	for _, f := range strings.Split(string(raw), "\x01") {
		switch {
		case strings.HasPrefix(f, "34="):
			v, err := strconv.ParseInt(f[3:], 10, 64)
			if err != nil {
				return m, fmt.Errorf("peel 34: %w", err)
			}
			m.SeqNum = v
		case strings.HasPrefix(f, "35="):
			m.MsgType = f[3:]
		case strings.HasPrefix(f, "52="):
			if ts, err := time.Parse("20060102-15:04:05.000", f[3:]); err == nil {
				m.SendingTime = ts
			}
		}
	}
	if m.SeqNum == 0 || m.MsgType == "" {
		return m, fmt.Errorf("peel: incomplete frame %q", string(raw))
	}
	return m, nil
}

// foFrame builds a minimal raw outbound FIX frame for the archive.
func foFrame(msgType string, seq int64) []byte {
	return []byte(fmt.Sprintf(
		"8=FIX.4.4\x019=100\x0135=%s\x0134=%d\x0149=EX\x0156=CLI\x0152=%s\x0110=000\x01",
		msgType, seq, time.Now().UTC().Format("20060102-15:04:05.000")))
}

type failoverTiming struct {
	claimLoad time.Duration // ResumeOnLogon = Claim + Load (+ seed CAS)
	issueRR   time.Duration // build + persist + record the outbound ResendRequest
	replayIn  time.Duration // PossDup replays: dedup check + RecordInbound
	fetchPlan time.Duration // archive fetch + PlanResend
	resume    time.Duration // secondary claim → gap-fill stream ready
	total     time.Duration // primary death → gap-fill stream ready
}

// runFailoverScenario executes one full primary-death → secondary-resume
// cycle on the real stores and returns the segment timings.
func runFailoverScenario(t *testing.T, ctx context.Context, rdb *goredis.Client, pool *pgxpool.Pool, iter int) failoverTiming {
	t.Helper()
	sid := fmt.Sprintf("FIX.4.4:EX->FOE2E-%d-%d", iter, time.Now().UnixNano())
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), seqKey(sid)).Err()
		_ = rdb.Del(context.Background(), dedupKey(sid, "ORD-77")).Err()
	})
	const lease = 300 * time.Millisecond

	// The production seam: Redis Lua authoritative, fix_sessions fallback.
	store := NewFailoverStore(NewRedisSeqStore(rdb), NewPGSeqStore(pool), nil)
	dedup := NewRedisExecDedup(rdb)
	pgst := NewPgStore(pool)

	// --- primary gateway lifetime ---
	primary := NewFailover(store, dedup,
		FailoverConfig{GatewayID: "gw-primary", LeaseTTL: lease}, time.Now)
	if _, err := pool.Exec(ctx,
		`INSERT INTO fix_sessions (session_id, status) VALUES ($1,'ACTIVE')`, sid); err != nil {
		t.Fatalf("seed fix_sessions: %v", err)
	}
	plan, err := primary.ResumeOnLogon(ctx, sid, 1, false)
	if err != nil || plan.Action != ResyncInSync {
		t.Fatalf("primary logon: %+v err=%v", plan, err)
	}
	// Client traffic in=1..4 consumed by the primary.
	for seq := int64(1); seq <= 4; seq++ {
		if _, err := primary.RecordInbound(ctx, sid, seq); err != nil {
			t.Fatalf("primary inbound %d: %v", seq, err)
		}
	}
	// Outbound traffic seqs 1..9, archived for resend — mix admin
	// (Heartbeat, seqs 1/5/9) and application (ExecutionReport) frames.
	for seq := int64(1); seq <= 9; seq++ {
		mt := "8"
		if seq%4 == 1 {
			mt = "0"
		}
		if err := pgst.SaveMessage(ctx, sid, seq, foFrame(mt, seq)); err != nil {
			t.Fatalf("archive seq %d: %v", seq, err)
		}
		if _, err := primary.RecordOutbound(ctx, sid, seq); err != nil {
			t.Fatalf("primary outbound %d: %v", seq, err)
		}
	}
	// One in-flight order so the PossDup dedup path is exercised.
	if _, dup, err := primary.HandleNewOrder(ctx, sid, "ORD-77", false); err != nil || dup {
		t.Fatalf("primary order claim: dup=%v err=%v", dup, err)
	}
	if err := primary.ResolveDedup(ctx, sid, "ORD-77", DedupRecord{
		ClOrdID: "ORD-77", ExecID: "EX-77", OrderID: 77,
		Report: foFrame("8", 2),
	}); err != nil {
		t.Fatalf("primary dedup resolve: %v", err)
	}

	// --- primary dies: no renewal, lease lapses ---
	tDie := time.Now()
	time.Sleep(lease + 150*time.Millisecond) // lease expiry = failover detection window

	// --- secondary resume (post-detection) ---
	secondary := NewFailover(store, dedup,
		FailoverConfig{GatewayID: "gw-secondary", LeaseTTL: lease}, time.Now)

	tm := failoverTiming{}
	t0 := time.Now()

	// Client Logon arrives ahead of our stored expectation (it sent
	// seqs 5..7 while the primary was dying). Claim + Load = the 2
	// store RTTs the row's note cites.
	plan, err = secondary.ResumeOnLogon(ctx, sid, 8, false)
	if err != nil {
		t.Fatalf("secondary resume: %v", err)
	}
	tm.claimLoad = time.Since(t0)
	if plan.Action != ResyncIssueResendRequest || plan.BeginSeqNo != 5 || plan.EndSeqNo != 0 {
		t.Fatalf("resync plan = %+v, want ResendRequest[5,0]", plan)
	}
	if plan.Owner != "gw-secondary" || plan.Epoch < 2 {
		t.Fatalf("takeover plan owner/epoch = %+v", plan)
	}

	// Issue ResendRequest(35=2): goes out as seq 10, archived like any
	// outbound frame, counter advanced via the shared store.
	t1 := time.Now()
	rr := NewResendRequest(plan.BeginSeqNo, plan.EndSeqNo)
	if rr == nil {
		t.Fatal("resend request build returned nil")
	}
	if err := pgst.SaveMessage(ctx, sid, 10, foFrame("2", 10)); err != nil {
		t.Fatalf("archive RR: %v", err)
	}
	if _, err := secondary.RecordOutbound(ctx, sid, 10); err != nil {
		t.Fatalf("record RR outbound: %v", err)
	}
	tm.issueRR = time.Since(t1)

	// Bidirectional gap fill — inbound: client replays seqs 5,6,7 with
	// PossDupFlag; the NewOrderSingle is deduped on ClOrdID, admin
	// replays just advance the counter.
	t2 := time.Now()
	rec, dup, err := secondary.HandleNewOrder(ctx, sid, "ORD-77", true)
	if err != nil || !dup {
		t.Fatalf("replayed order: dup=%v err=%v", dup, err)
	}
	if rec.ExecID != "EX-77" || len(rec.Report) == 0 {
		t.Fatalf("dedup echo = %+v", rec)
	}
	for seq := int64(5); seq <= 7; seq++ {
		if _, err := secondary.RecordInbound(ctx, sid, seq); err != nil {
			t.Fatalf("secondary inbound %d: %v", seq, err)
		}
	}
	tm.replayIn = time.Since(t2)

	// Outbound: client missed seqs 7..9 → its ResendRequest(7,0) is
	// answered from the PG archive through the production peeling seam.
	t3 := time.Now()
	msgs, err := pgArchiver{pgst}.Fetch(ctx, sid, 7, 0)
	if err != nil {
		t.Fatalf("archive fetch: %v", err)
	}
	cur, err := store.Load(ctx, sid)
	if err != nil {
		t.Fatalf("load for currentOut: %v", err)
	}
	entries, err := PlanResend(7, 0, cur.OutSeqNum, msgs)
	if err != nil {
		t.Fatalf("plan resend: %v", err)
	}
	var replays, fills int
	for _, e := range entries {
		if e.Replay != nil {
			replays++
		} else {
			fills++
		}
	}
	if replays == 0 {
		t.Fatalf("resend plan has no app replays: %+v", entries)
	}
	tm.fetchPlan = time.Since(t3)

	tm.resume = time.Since(t0)
	tm.total = time.Since(tDie)

	// Continuity proof: inbound now expects 8 == client's watermark.
	if cur.InSeqNum != 8 {
		t.Fatalf("post-resync expected inbound = %d, want 8", cur.InSeqNum)
	}
	return tm
}

func medDur(ds []time.Duration) time.Duration {
	cp := make([]time.Duration, len(ds))
	copy(cp, ds)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}

func maxDur(ds []time.Duration) time.Duration {
	m := ds[0]
	for _, d := range ds[1:] {
		if d > m {
			m = d
		}
	}
	return m
}

// TestFailoverE2E runs the full scenario 5 times and asserts the <5s
// bound on both bands; every segment is logged so a miss reports the
// honest breakdown.
func TestFailoverE2E(t *testing.T) {
	if testing.Short() || !redisTestEnabled() {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	rdb := redisTestClient(t)
	pool := failoverPGPool(t)
	ctx := context.Background()

	var ts []failoverTiming
	for i := 0; i < 5; i++ {
		ts = append(ts, runFailoverScenario(t, ctx, rdb, pool, i))
		t.Logf("iter %d: claim+load=%s issueRR=%s replayIn=%s fetch+plan=%s resume=%s total(incl. lease wait)=%s",
			i, ts[i].claimLoad, ts[i].issueRR, ts[i].replayIn, ts[i].fetchPlan, ts[i].resume, ts[i].total)
	}
	collect := func(f func(failoverTiming) time.Duration) []time.Duration {
		out := make([]time.Duration, len(ts))
		for i, x := range ts {
			out[i] = f(x)
		}
		return out
	}
	resumes, totals := collect(func(x failoverTiming) time.Duration { return x.resume }),
		collect(func(x failoverTiming) time.Duration { return x.total })
	t.Logf("summary: resume median=%s max=%s | total median=%s max=%s | bound=%s",
		medDur(resumes), maxDur(resumes), medDur(totals), maxDur(totals), failoverBound)
	for i, x := range ts {
		if x.resume > failoverBound {
			t.Errorf("iter %d: resume %s exceeds 5s bound", i, x.resume)
		}
		if x.total > failoverBound {
			t.Errorf("iter %d: total failover %s exceeds 5s bound", i, x.total)
		}
	}
}

// TestFailoverResumeLatencyDistribution isolates the cited "2 store
// RTTs (Claim + Load)" — ResumeOnLogon alone across 50 fresh sessions
// on the real Redis store, reporting p50/p99.
func TestFailoverResumeLatencyDistribution(t *testing.T) {
	if testing.Short() || !redisTestEnabled() {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	rdb := redisTestClient(t)
	ctx := context.Background()
	store := NewRedisSeqStore(rdb)
	f := NewFailover(store, nil, FailoverConfig{GatewayID: "gw-e2e"}, time.Now)

	lats := make([]time.Duration, 0, 50)
	for i := 0; i < 50; i++ {
		sid := fmt.Sprintf("FIX.4.4:EX->LAT-%d", time.Now().UnixNano())
		t0 := time.Now()
		if _, err := f.ResumeOnLogon(ctx, sid, 1, false); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
		lats = append(lats, time.Since(t0))
		_ = rdb.Del(ctx, seqKey(sid)).Err()
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	t.Logf("ResumeOnLogon (Claim+Load+cold-seed CAS): n=50 p50=%s p99=%s max=%s",
		lats[len(lats)/2], lats[(len(lats)*99)/100], lats[len(lats)-1])
	if lats[(len(lats)*99)/100] > failoverBound {
		t.Errorf("resume p99 %s exceeds 5s bound", lats[(len(lats)*99)/100])
	}
}
