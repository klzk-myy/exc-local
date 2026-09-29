// session_lifecycle_test.go — Phase-15 Task 15.3.7 coverage.
//
// Unit tests pin the pure schedule math (sessionStateAt / nextBoundary /
// lastBoundary / event names) against the §6.7 weekly grid.
//
// TestSessionLifecycleWeeklyFlow is Redis-gated (EXC_REDIS_TEST=1) and
// drives the complete Friday-close → Sunday-open sequence with a fake
// clock: PRE_CLOSE advisory, CLOSED + Tom-Next rollover seam, PRE_OPEN
// auction CALL keys, Sunday-open UNCROSS + WS events, shard coverage,
// restart persistence, and idempotent re-evaluation.
package admin

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Pure schedule math
// ---------------------------------------------------------------------------

func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

func TestSessionStateAt(t *testing.T) {
	// 2026-10-02 is a Friday; 2026-10-04 a Sunday; 2026-10-05 a Monday.
	cases := []struct {
		now  time.Time
		want SessionState
	}{
		{at(2026, 10, 2, 0, 0), SessionOpen},   // Friday morning
		{at(2026, 10, 2, 21, 54), SessionOpen}, // last full minute before advisory
		{at(2026, 10, 2, 21, 55), SessionPreClose},
		{at(2026, 10, 2, 21, 59), SessionPreClose},
		{at(2026, 10, 2, 22, 0), SessionClosed}, // exact close instant
		{at(2026, 10, 2, 23, 59), SessionClosed},
		{at(2026, 10, 3, 12, 0), SessionClosed}, // Saturday
		{at(2026, 10, 4, 0, 0), SessionClosed},  // Sunday before pre-open
		{at(2026, 10, 4, 20, 44), SessionClosed},
		{at(2026, 10, 4, 20, 45), SessionPreOpen}, // exact pre-open instant
		{at(2026, 10, 4, 20, 59), SessionPreOpen},
		{at(2026, 10, 4, 21, 0), SessionOpen}, // exact open instant
		{at(2026, 10, 4, 23, 59), SessionOpen},
		{at(2026, 10, 5, 0, 0), SessionOpen},  // Monday
		{at(2026, 10, 7, 12, 0), SessionOpen}, // Wednesday
	}
	for _, tc := range cases {
		if got := sessionStateAt(tc.now); got != tc.want {
			t.Errorf("sessionStateAt(%s %s)=%s want %s",
				tc.now.Weekday(), tc.now.Format(time.RFC3339), got, tc.want)
		}
	}
}

func TestNextBoundary(t *testing.T) {
	cases := []struct {
		now    time.Time
		wantAt time.Time
		wantTo SessionState
	}{
		// Mid-week: next boundary is Friday 21:55.
		{at(2026, 10, 7, 9, 0), at(2026, 10, 9, 21, 55), SessionPreClose},
		// Friday advisory → close.
		{at(2026, 10, 9, 21, 56), at(2026, 10, 9, 22, 0), SessionClosed},
		// Closed Friday night → Sunday 20:45 pre-open (skips Saturday).
		{at(2026, 10, 9, 22, 30), at(2026, 10, 11, 20, 45), SessionPreOpen},
		{at(2026, 10, 10, 12, 0), at(2026, 10, 11, 20, 45), SessionPreOpen},
		// Pre-open → open.
		{at(2026, 10, 11, 20, 50), at(2026, 10, 11, 21, 0), SessionOpen},
		// Sunday open → next Friday advisory.
		{at(2026, 10, 11, 21, 1), at(2026, 10, 16, 21, 55), SessionPreClose},
	}
	for _, tc := range cases {
		gotAt, gotTo := nextBoundary(tc.now)
		if !gotAt.Equal(tc.wantAt) || gotTo != tc.wantTo {
			t.Errorf("nextBoundary(%s)=(%s,%s) want (%s,%s)",
				tc.now.Format(time.RFC3339), gotAt.Format(time.RFC3339), gotTo,
				tc.wantAt.Format(time.RFC3339), tc.wantTo)
		}
	}
}

func TestLastBoundary(t *testing.T) {
	cases := []struct {
		now    time.Time
		wantAt time.Time
		wantTo SessionState
	}{
		// At the exact open instant the 21:00 boundary is "last".
		{at(2026, 10, 4, 21, 0), at(2026, 10, 4, 21, 0), SessionOpen},
		{at(2026, 10, 4, 20, 50), at(2026, 10, 4, 20, 45), SessionPreOpen},
		// Saturday's last boundary is Friday 22:00.
		{at(2026, 10, 3, 8, 0), at(2026, 10, 2, 22, 0), SessionClosed},
		// Friday 21:57 → the 21:55 PRE_CLOSE boundary.
		{at(2026, 10, 2, 21, 57), at(2026, 10, 2, 21, 55), SessionPreClose},
		// Monday morning's last boundary is Sunday 21:00 OPEN.
		{at(2026, 10, 5, 9, 0), at(2026, 10, 4, 21, 0), SessionOpen},
	}
	for _, tc := range cases {
		gotAt, gotTo := lastBoundary(tc.now)
		if !gotAt.Equal(tc.wantAt) || gotTo != tc.wantTo {
			t.Errorf("lastBoundary(%s)=(%s,%s) want (%s,%s)",
				tc.now.Format(time.RFC3339), gotAt.Format(time.RFC3339), gotTo,
				tc.wantAt.Format(time.RFC3339), tc.wantTo)
		}
	}
}

func TestSessionEventName(t *testing.T) {
	want := map[SessionState]string{
		SessionOpen:     "session.open",
		SessionPreClose: "session.pre_close",
		SessionClosed:   "session.closed",
		SessionPreOpen:  "session.pre_open",
	}
	for st, name := range want {
		if got := sessionEventName(st); got != name {
			t.Errorf("sessionEventName(%s)=%s want %s", st, got, name)
		}
	}
}

func TestNewSessionServiceFailClosed(t *testing.T) {
	if _, err := NewSessionService(SessionDeps{}); err == nil {
		t.Fatal("nil rdb must fail")
	}
	if _, err := NewSessionService(SessionDeps{RDB: nil, Shards: []int{0}}); err == nil {
		t.Fatal("nil rdb must fail even with shards")
	}
}

// ---------------------------------------------------------------------------
// Redis-gated full weekly flow — injected clock
// ---------------------------------------------------------------------------

type fakePub struct {
	mu     sync.Mutex
	events []SessionEvent
}

func (f *fakePub) Publish(_ string, data any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ev, ok := data.(SessionEvent); ok {
		f.events = append(f.events, ev)
	}
}

func (f *fakePub) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	for i, e := range f.events {
		out[i] = e.Event
	}
	return out
}

func TestSessionLifecycleWeeklyFlow(t *testing.T) {
	rdb, ctx := redisGate(t)

	// Dedicated shard ids so the test never collides with a live
	// deployment's session:state:{0..n} keys.
	shards := []int{990001, 990002}
	syms := []string{"SLIT1/USD", "SLIT2/USD"}
	t.Cleanup(func() {
		for _, sh := range shards {
			_ = rdb.Del(context.Background(), sessionStateKey(sh)).Err()
		}
		for _, s := range syms {
			_ = rdb.Del(context.Background(), AuctionKeyPrefix+s).Err()
		}
		_ = rdb.Del(context.Background(), sessionCtlKey).Err()
		// Effects locks are per-boundary; sweep the plausible range.
		for _, ts := range []time.Time{
			at(2026, 10, 2, 21, 55), at(2026, 10, 2, 22, 0),
			at(2026, 10, 4, 20, 45), at(2026, 10, 4, 21, 0),
		} {
			_ = rdb.Del(context.Background(),
				sessionEffectsLockPrefix+fmt.Sprint(ts.Unix())).Err()
		}
	})

	pub := &fakePub{}
	var mu sync.Mutex
	rollCalls := 0
	var alerts []string

	var now atomic_clock
	svc, err := NewSessionService(SessionDeps{
		RDB: rdb, Shards: shards, Pub: pub,
		Rollover: func(_ context.Context, _ time.Time) (string, error) {
			mu.Lock()
			rollCalls++
			mu.Unlock()
			return "run=1 rolled=2", nil
		},
		Symbols: func(context.Context) ([]string, error) { return syms, nil },
		Alert: func(_ context.Context, sev, summary string) error {
			mu.Lock()
			defer mu.Unlock()
			alerts = append(alerts, sev+":"+summary)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	svc.SetClockForTest(func() time.Time { return now.t })

	eval := func(t_ time.Time) {
		t.Helper()
		if err := svc.Evaluate(ctx, t_); err != nil {
			t.Fatalf("evaluate @%s: %v", t_.Format(time.RFC3339), err)
		}
	}
	shardState := func(sh int) map[string]string {
		t.Helper()
		m, err := rdb.HGetAll(ctx, sessionStateKey(sh)).Result()
		if err != nil {
			t.Fatalf("HGETALL: %v", err)
		}
		return m
	}
	wantState := func(sh int, st SessionState) {
		t.Helper()
		if got := SessionState(shardState(sh)["state"]); got != st {
			t.Fatalf("shard %d state=%s want %s", sh, got, st)
		}
	}

	// -- Friday 21:30 UTC: plain OPEN week; boot reconcile writes keys.
	eval(at(2026, 10, 2, 21, 30))
	for _, sh := range shards {
		wantState(sh, SessionOpen)
		m := shardState(sh)
		if m["next_state"] != string(SessionPreClose) ||
			m["next_transition_at"] != at(2026, 10, 2, 21, 55).Format(time.RFC3339) {
			t.Fatalf("shard %d forward fields: %v", sh, m)
		}
	}

	// -- Friday 21:56: PRE_CLOSE advisory.
	eval(at(2026, 10, 2, 21, 56))
	for _, sh := range shards {
		wantState(sh, SessionPreClose)
	}
	if mu.Lock(); rollCalls != 0 {
		mu.Unlock()
		t.Fatal("rollover must not fire before close")
	} else {
		mu.Unlock()
	}

	// -- Friday 22:00+ε: CLOSED; rollover seam fires exactly once;
	//    session.closed broadcasts. Idempotent re-evaluation.
	eval(at(2026, 10, 2, 22, 1))
	eval(at(2026, 10, 2, 22, 2)) // repeat tick inside the same boundary
	for _, sh := range shards {
		wantState(sh, SessionClosed)
	}
	mu.Lock()
	if rollCalls != 1 {
		mu.Unlock()
		t.Fatalf("rollover calls=%d want 1", rollCalls)
	}
	mu.Unlock()
	gotEvents := pub.names()
	if !containsStr(gotEvents, "session.closed") ||
		!containsStr(gotEvents, "session.pre_close") {
		t.Fatalf("events=%v", gotEvents)
	}
	// Exactly one of each per boundary (single-fire lock).
	if countStr(gotEvents, "session.closed") != 1 {
		t.Fatalf("session.closed published %d times", countStr(gotEvents, "session.closed"))
	}

	// -- Saturday: still CLOSED, nothing re-fires.
	eval(at(2026, 10, 3, 12, 0))
	mu.Lock()
	if rollCalls != 1 {
		mu.Unlock()
		t.Fatal("rollover must not re-fire on weekend ticks")
	}
	mu.Unlock()

	// -- Sunday 20:46: PRE_OPEN; CALL auction keys land in the shared
	//    "CALL:{deadline_unix_ns}" contract with deadline = the 21:00
	//    open instant; session.pre_open broadcast.
	callDeadline := fmt.Sprintf("CALL:%d", at(2026, 10, 4, 21, 0).UnixNano())
	eval(at(2026, 10, 4, 20, 46))
	for _, sh := range shards {
		wantState(sh, SessionPreOpen)
	}
	for _, sym := range syms {
		raw, err := rdb.Get(ctx, AuctionKeyPrefix+sym).Result()
		if err != nil {
			t.Fatalf("auction key %s: %v", sym, err)
		}
		if raw != callDeadline {
			t.Fatalf("CALL key %s=%q want %q", sym, raw, callDeadline)
		}
	}
	if !containsStr(pub.names(), "session.pre_open") {
		t.Fatalf("events=%v", pub.names())
	}

	// -- Sunday 21:01: OPEN; the CALL key's deadline is the release —
	//    the C++ AuctionManager uncrosses and deletes it (no consumer
	//    here, so the key persists unchanged); session.open broadcast.
	eval(at(2026, 10, 4, 21, 1))
	for _, sh := range shards {
		wantState(sh, SessionOpen)
	}
	for _, sym := range syms {
		raw, err := rdb.Get(ctx, AuctionKeyPrefix+sym).Result()
		if err != nil {
			t.Fatalf("auction key %s: %v", sym, err)
		}
		if raw != callDeadline {
			t.Fatalf("OPEN must not rewrite the CALL contract: %q", raw)
		}
	}
	if !containsStr(pub.names(), "session.open") {
		t.Fatalf("events=%v", pub.names())
	}

	// -- Status: OPEN, consistent, full coverage, next = Friday 21:55.
	now.t = at(2026, 10, 4, 21, 5)
	st, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != SessionOpen || !st.Consistent || !st.MarketOpen {
		t.Fatalf("status: %+v", st)
	}
	if st.ShardCoverage != "2/2 shards OPEN" {
		t.Fatalf("coverage=%q", st.ShardCoverage)
	}
	if st.NextState != SessionPreClose ||
		st.NextTransitionAt != at(2026, 10, 9, 21, 55).Format(time.RFC3339) {
		t.Fatalf("next transition: %+v", st)
	}
	if len(st.PendingEffects) != 0 {
		t.Fatalf("pending effects must be drained: %v", st.PendingEffects)
	}

	// -- Restart persistence: a fresh service instance reads the same
	//    persisted state (no Evaluate needed to see it).
	svc2, err := NewSessionService(SessionDeps{RDB: rdb, Shards: shards})
	if err != nil {
		t.Fatalf("service2: %v", err)
	}
	svc2.SetClockForTest(func() time.Time { return at(2026, 10, 4, 21, 5) })
	st2, err := svc2.Status(ctx)
	if err != nil || st2.State != SessionOpen || !st2.Consistent {
		t.Fatalf("post-restart status: %+v err=%v", st2, err)
	}

	// -- Pending-ledger drain: seed a stuck pending set for the current
	//    state; the next Evaluate drains it and clears the ledger.
	if err := rdb.HSet(ctx, sessionCtlKey,
		"pending_state", string(SessionOpen),
		"pending_effects", `["ws"]`,
		"updated_at", time.Now().UTC().Format(time.RFC3339)).Err(); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	eventsBefore := len(pub.names())
	eval(at(2026, 10, 4, 21, 10))
	if len(pub.names()) <= eventsBefore {
		t.Fatal("pending ws effect must be drained")
	}
	if v, _ := rdb.HGet(ctx, sessionCtlKey, "pending_state").Result(); v != "" {
		t.Fatalf("pending ledger not cleared: %q", v)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(alerts) != 0 {
		t.Fatalf("no alerts expected in the happy path: %v", alerts)
	}
}

// atomic_clock is a hand-rolled settable clock for the status check
// (svc.SetClockForTest rebinds the function, not the value).
type atomic_clock struct{ t time.Time }

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func countStr(xs []string, s string) int {
	n := 0
	for _, x := range xs {
		if x == s {
			n++
		}
	}
	return n
}
