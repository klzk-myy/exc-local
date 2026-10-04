// balance_consumer_test.go — coverage for the post-commit JetStream
// republish seam (TradeRepublisher / WithRepublisher).
package settlement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	excerrors "exchange/pkg/errors"
)

// recordingRepublisher captures every PublishEvent call; commitBefore
// lets tests observe whether the settlement commit preceded the first
// publish.
type recordingRepublisher struct {
	mu     sync.Mutex
	calls  []republishCall
	err    error
	onCall func()
}

type republishCall struct {
	subject string
	msgID   string
	payload []byte
}

func (r *recordingRepublisher) PublishEvent(_ context.Context, subject, msgID string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, republishCall{subject, msgID, append([]byte(nil), payload...)})
	if r.onCall != nil {
		r.onCall()
	}
	return r.err
}

func (r *recordingRepublisher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestConsumerRepublishesCommittedFills(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	rt := resolvedRM(42, 1, 2)
	rt.Symbol = "EUR/USD"
	resolver := &fakeResolver{m: map[uint64]ResolvedTrade{42: rt}}

	fb := flatbuffers.NewBuilder(256)
	frame := encodeFill(fb, 7, 42, 101, 202, 125000000, 10000_00000000)
	var delivered [][]byte
	src := FuncSource(func(limit int, deliver func([]byte)) int {
		if len(delivered) > 0 {
			return 0
		}
		delivered = append(delivered, frame)
		deliver(frame)
		return 1
	})

	repub := &recordingRepublisher{}
	c, err := NewFillConsumer(svc, resolver, src, 3, 1, time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	c.WithRepublisher(repub)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for repub.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	repub.mu.Lock()
	defer repub.mu.Unlock()
	if len(repub.calls) != 2 {
		t.Fatalf("republish calls=%d, want 2 (trades + settlements)", len(repub.calls))
	}
	wantSubjects := map[string]bool{
		"trades.3.EUR-USD":      false,
		"settlements.3.EUR-USD": false,
	}
	for _, call := range repub.calls {
		if _, ok := wantSubjects[call.subject]; !ok {
			t.Fatalf("unexpected subject %q", call.subject)
		}
		wantSubjects[call.subject] = true
		if call.msgID != "s3-7" {
			t.Fatalf("msgID=%q, want s3-7 (s{shard}-{engine_seq})", call.msgID)
		}
		if string(call.payload) != string(frame) {
			t.Fatalf("payload not the raw frame (%d vs %d bytes)", len(call.payload), len(frame))
		}
	}
	for subj, seen := range wantSubjects {
		if !seen {
			t.Fatalf("subject %s never published", subj)
		}
	}
	if c.Metrics().Republished != 2 {
		t.Fatalf("republished=%d", c.Metrics().Republished)
	}
}

func TestConsumerRepublishFailureHalts(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	rt := resolvedRM(42, 1, 2)
	rt.Symbol = "EUR/USD"
	resolver := &fakeResolver{m: map[uint64]ResolvedTrade{42: rt}}

	src := FuncSource(func(limit int, deliver func([]byte)) int {
		fb := flatbuffers.NewBuilder(256)
		deliver(encodeFill(fb, 7, 42, 101, 202, 125000000, 10000_00000000))
		return 1
	})

	repub := &recordingRepublisher{err: errors.New("nats down")}
	c, err := NewFillConsumer(svc, resolver, src, 0, 1, time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	c.WithRepublisher(repub)

	err = c.Run(context.Background())
	if err == nil || (!errors.Is(err, repub.err) && !strings.Contains(err.Error(), "republish")) {
		t.Fatalf("Run err=%v, want republish failure", err)
	}
	// The commit succeeded before the publish — replay republishes under
	// the same msg id (idempotent), so halting is the correct behavior.
	if store.committed() != 1 {
		t.Fatalf("commit must land before republish aborts (commits=%d)", store.committed())
	}
}

func TestConsumerRepublishDeadLettersBadSubject(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	rt := resolvedRM(42, 1, 2)
	rt.Symbol = "EUR.USD" // '.' is a subject separator — deterministic fail
	resolver := &fakeResolver{m: map[uint64]ResolvedTrade{42: rt}}

	src := FuncSource(func(limit int, deliver func([]byte)) int {
		fb := flatbuffers.NewBuilder(256)
		deliver(encodeFill(fb, 7, 42, 101, 202, 125000000, 10000_00000000))
		return 1
	})

	var logLines []string
	var mu sync.Mutex
	repub := &recordingRepublisher{}
	c, err := NewFillConsumer(svc, resolver, src, 0, 1, time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	c.WithRepublisher(repub).WithLogger(func(f string, a ...any) {
		mu.Lock()
		logLines = append(logLines, fmt.Sprintf(f, a...))
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for c.Metrics().RepubDropped == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	// The fill committed; the bad subject dead-lettered instead of
	// aborting — a restart loop here would wedge the ring forever on a
	// failure replay can never fix.
	if store.committed() == 0 {
		t.Fatal("fill must commit despite dead-lettered republish")
	}
	if repub.count() != 0 {
		t.Fatalf("publisher must not be called for an invalid subject (calls=%d)", repub.count())
	}
	if c.Metrics().RepubDropped == 0 {
		t.Fatal("RepubDropped not counted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logLines) == 0 || !strings.Contains(logLines[0], "dead-letter") {
		t.Fatalf("expected dead-letter log line, got %v", logLines)
	}
}

func TestConsumerNoRepublisherIsANoop(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	resolver := &fakeResolver{m: map[uint64]ResolvedTrade{42: resolvedRM(42, 1, 2)}}

	fb := flatbuffers.NewBuilder(256)
	frame := encodeFill(fb, 7, 42, 101, 202, 125000000, 10000_00000000)
	delivered := false
	src := FuncSource(func(limit int, deliver func([]byte)) int {
		if delivered {
			return 0
		}
		delivered = true
		deliver(frame)
		return 1
	})
	c, err := NewFillConsumer(svc, resolver, src, 0, 1, time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for store.committed() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if store.committed() != 1 {
		t.Fatalf("commits=%d", store.committed())
	}
}

// backlogResolver wraps fakeResolver with the BacklogSource seam the
// boot-time repair reads (migration 281 durable republish source).
type backlogResolver struct {
	*fakeResolver
	fills []BacklogFill
	err   error
}

func (r *backlogResolver) RepublishBacklog(_ context.Context, shardID int64,
	lookback time.Duration) ([]BacklogFill, error) {
	return r.fills, r.err
}

// TestConsumerRepublishBacklog covers the crash-window repair: a fill
// committed in a previous run (raw_frame persisted on processed_trades)
// is re-emitted at Run start under the same subject/msgID contract —
// JetStream dedup makes the overlap a no-op.
func TestConsumerRepublishBacklog(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})

	fb := flatbuffers.NewBuilder(256)
	frame := encodeFill(fb, 7, 42, 101, 202, 125000000, 10000_00000000)
	resolver := &backlogResolver{
		fakeResolver: &fakeResolver{m: map[uint64]ResolvedTrade{}},
		fills: []BacklogFill{
			{TradeID: 42, Symbol: "EUR/USD", EngineSeq: 7, Raw: frame},
			{TradeID: 43, Symbol: "EUR/USD", EngineSeq: 8, Raw: frame},
		},
	}
	repub := &recordingRepublisher{}
	src := FuncSource(func(int, func([]byte)) int { return 0 })
	c, err := NewFillConsumer(svc, resolver, src, 3, 1, time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	c.WithRepublisher(repub)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for repub.count() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	repub.mu.Lock()
	defer repub.mu.Unlock()
	if len(repub.calls) != 4 {
		t.Fatalf("backlog republish calls=%d, want 4 (2 fills × trades+settlements)", len(repub.calls))
	}
	want := map[string]int{ // subject → msgID
		"trades.3.EUR-USD s3-7":      0,
		"settlements.3.EUR-USD s3-7": 0,
		"trades.3.EUR-USD s3-8":      0,
		"settlements.3.EUR-USD s3-8": 0,
	}
	for _, call := range repub.calls {
		key := call.subject + " " + call.msgID
		if _, ok := want[key]; !ok {
			t.Fatalf("unexpected backlog publish %s", key)
		}
		want[key]++
	}
	for k, n := range want {
		if n != 1 {
			t.Fatalf("%s published %d times", k, n)
		}
	}
}

// chanSource adapts a buffered channel of frames to FillSource — the
// same shape the gateway wires over settleQueues.
func chanSource(q chan []byte) FuncSource {
	return FuncSource(func(limit int, deliver func([]byte)) int {
		n := 0
		for n < limit {
			select {
			case p := <-q:
				deliver(p)
				n++
			default:
				return n
			}
		}
		return n
	})
}

// TestConsumerShutdownDrainsQueuedFills — cancellation with frames still
// queued must drain and commit them, not abandon the tail. Regression:
// the frame tap stops before the consumer ctx is cancelled (ring_drain
// → settle_drain → sweepStop), but the queue can still hold frames the
// pump hasn't reached — a bare ctx.Err() exit strands them (read-model
// FILLED, no settled leg).
func TestConsumerShutdownDrainsQueuedFills(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	m := map[uint64]ResolvedTrade{}
	for _, id := range []uint64{50, 51, 52} {
		rt := resolvedRM(id, 1, 2)
		rt.Symbol = "EUR/USD"
		m[id] = rt
	}
	q := make(chan []byte, 8)
	for i, id := range []uint64{50, 51, 52} {
		fb := flatbuffers.NewBuilder(256)
		q <- encodeFill(fb, uint64(100+i), id, 101, 202, 125000000, 10000_00000000)
	}
	c, err := NewFillConsumer(svc, &fakeResolver{m: m}, chanSource(q), 0, 100, time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // frames still in the queue when the consumer ctx dies
	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err=%v, want context.Canceled", err)
	}
	if store.committed() == 0 {
		t.Fatal("queued fills must drain to a commit on shutdown")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, id := range []int64{50, 51, 52} {
		if _, ok := store.processed[id]; !ok {
			t.Fatalf("trade %d not committed on shutdown drain", id)
		}
	}
}

// gatedResolver blocks inside Resolve until released — it honours ctx
// cancellation like the real PG resolver, so a mid-resolve cancel is
// observable.
type gatedResolver struct {
	m       map[uint64]ResolvedTrade
	entered chan struct{}
	release chan struct{}
}

func (r *gatedResolver) Resolve(ctx context.Context, f EngineFill) (ResolvedTrade, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		return ResolvedTrade{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return ResolvedTrade{}, err
	}
	rt, ok := r.m[f.TradeID]
	if !ok {
		return ResolvedTrade{}, excerrors.New(CodeTradeFillUnresolvable, "unknown trade")
	}
	rt.Fill = f
	return rt, nil
}

// TestConsumerShutdownCompletesInFlightFill — a fill pumped but still
// resolving when ctx is cancelled must resolve to completion (on the
// WithoutCancel commit context) and commit. Regression: resolving on the
// caller ctx stranded the whole pump window — orders FILLED, legs lost.
func TestConsumerShutdownCompletesInFlightFill(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	rt := resolvedRM(60, 1, 2)
	rt.Symbol = "EUR/USD"
	resolver := &gatedResolver{
		m:       map[uint64]ResolvedTrade{60: rt},
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	fb := flatbuffers.NewBuilder(256)
	q := make(chan []byte, 4)
	q <- encodeFill(fb, 200, 60, 101, 202, 125000000, 10000_00000000)
	c, err := NewFillConsumer(svc, resolver, chanSource(q), 0, 1, time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	<-resolver.entered // Resolve is in flight
	cancel()
	close(resolver.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err=%v, want context.Canceled", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.processed[60]; !ok {
		t.Fatal("in-flight fill dropped on cancel — must resolve and commit")
	}
}

// TestConsumerRepublishBacklogScanFailsClosed — a backlog scan error
// aborts Run before any consume (same fail-closed posture as a publish
// failure: restart until the store answers).
func TestConsumerRepublishBacklogScanFailsClosed(t *testing.T) {
	store := newFakeStore()
	svc := newBalanceServiceForTest(store, &fakeLocker{}, &fakeDispatcher{}, &fakeManagedPoster{}, &fakeAlerter{})
	resolver := &backlogResolver{
		fakeResolver: &fakeResolver{m: map[uint64]ResolvedTrade{}},
		err:          errors.New("pg down"),
	}
	repub := &recordingRepublisher{}
	src := FuncSource(func(int, func([]byte)) int { return 0 })
	c, err := NewFillConsumer(svc, resolver, src, 3, 1, time.Millisecond)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	c.WithRepublisher(repub)
	err = c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "backlog") {
		t.Fatalf("Run err=%v, want backlog scan failure", err)
	}
	if repub.count() != 0 {
		t.Fatalf("no publishes expected on scan failure (calls=%d)", repub.count())
	}
}
