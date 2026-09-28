package bridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
)

const (
	testTimeout   = time.Second
	testBackoff   = time.Millisecond
	testHeartbeat = 20 * time.Millisecond
)

// fakePublisher is a controllable Publisher for unit tests: it can be
// toggled "down" to simulate a NATS outage, and records every call.
type fakePublisher struct {
	mu         sync.Mutex
	down       atomic.Bool
	connected  atomic.Bool
	published  []pubCall
	heartbeats []hbCall
}

type pubCall struct {
	subject string
	msgID   string
	payload []byte
}

type hbCall struct {
	subject string
	payload []byte
}

func newFakePublisher() *fakePublisher {
	f := &fakePublisher{}
	f.connected.Store(true)
	return f
}

func (f *fakePublisher) PublishEvent(_ context.Context, subject, msgID string, payload []byte) error {
	if f.down.Load() {
		return errors.New("nats: connection down")
	}
	f.mu.Lock()
	f.published = append(f.published, pubCall{subject, msgID, append([]byte(nil), payload...)})
	f.mu.Unlock()
	return nil
}

func (f *fakePublisher) PublishHeartbeat(subject string, payload []byte) error {
	f.mu.Lock()
	f.heartbeats = append(f.heartbeats, hbCall{subject, append([]byte(nil), payload...)})
	f.mu.Unlock()
	return nil
}

func (f *fakePublisher) Connected() bool { return f.connected.Load() && !f.down.Load() }

func (f *fakePublisher) pubs() []pubCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pubCall(nil), f.published...)
}

func (f *fakePublisher) hbs() []hbCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hbCall(nil), f.heartbeats...)
}

func waitForCond(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startBridge(t *testing.T, cfg Config, pub Publisher, res Resolver) (*Bridge, context.CancelFunc) {
	t.Helper()
	b, err := New(cfg, pub, res, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return b, cancel
}

func testConfig(shard uint32, bufSize int) Config {
	return Config{
		ShardID:           shard,
		AeronURI:          "aeron:ipc?alias=orders_out",
		AeronStreamID:     1002,
		BufferSize:        bufSize,
		PublishTimeout:    testTimeout,
		ReconnectWait:     testBackoff,
		HeartbeatInterval: time.Hour, // disabled unless the test needs it
		OrderIndexSize:    64,
	}
}

// End-to-end: fragment in -> JetStream subject publishes out, fan-out to
// trades + settlements with the resolver's symbol.
func TestBridgePublishesToSubjects(t *testing.T) {
	pub := newFakePublisher()
	b, _ := startBridge(t, testConfig(0, 64), pub, MapResolver{3: "EUR-USD"})
	fb := flatbuffers.NewBuilder(256)

	b.HandleFragment(newOrderEvent(fb, 1, 1001, 3))
	b.HandleFragment(newFillEvent(fb, 2, 1001, 2002))

	waitForCond(t, 2*time.Second, "4 publishes", func() bool {
		return len(pub.pubs()) == 4
	})
	got := pub.pubs()
	wantSubjects := []string{
		"compliance.0.EUR-USD", "analytics.0.EUR-USD",
		"trades.0.EUR-USD", "settlements.0.EUR-USD",
	}
	for i := range wantSubjects {
		if got[i].subject != wantSubjects[i] {
			t.Fatalf("publish %d subject %q want %q", i, got[i].subject, wantSubjects[i])
		}
	}
	// Payload is the verbatim flatbuffers Event, msgID dedups on seq.
	if got[2].msgID != "s0-2" || len(got[2].payload) == 0 {
		t.Fatalf("fill publish msgID=%q payloadLen=%d", got[2].msgID, len(got[2].payload))
	}
	s := b.Snapshot()
	if s.Published != 4 || s.Received != 2 || s.BufferDepth != 0 {
		t.Fatalf("snapshot %+v", s)
	}
}

// NATS outage: events accumulate in the bounded buffer; on recovery they
// replay in order and bridge_nats_reconnect_total ticks.
func TestBridgeBuffersDuringOutageAndReplays(t *testing.T) {
	pub := newFakePublisher()
	pub.down.Store(true)
	b, _ := startBridge(t, testConfig(0, 100), pub, MapResolver{3: "EUR-USD"})
	fb := flatbuffers.NewBuilder(256)

	const n = 10
	for i := 0; i < n; i++ {
		b.HandleFragment(newFillEvent(fb, uint64(i+1), 1, 2))
	}
	// Wait until the drain loop has actually observed the outage
	// (publish failures), not just until the events are enqueued —
	// otherwise down=false could race ahead of the first attempt.
	waitForCond(t, 2*time.Second, "outage observed", func() bool {
		return b.Snapshot().PublishErr > 0 && b.Snapshot().BufferDepth == n
	})
	if len(pub.pubs()) != 0 {
		t.Fatal("no publishes expected while NATS is down")
	}

	pub.down.Store(false)
	waitForCond(t, 5*time.Second, "replay", func() bool {
		return len(pub.pubs()) == 2*n // trades + settlements
	})
	got := pub.pubs()
	for i := 0; i < n; i++ {
		if got[2*i].subject != "trades.0.UNKNOWN" || got[2*i+1].subject != "settlements.0.UNKNOWN" {
			t.Fatalf("replay %d: %v", i, got[2*i])
		}
		if got[2*i].msgID != got[2*i+1].msgID {
			t.Fatalf("fanout msgID mismatch: %q vs %q", got[2*i].msgID, got[2*i+1].msgID)
		}
	}
	s := b.Snapshot()
	if s.Reconnects != 1 {
		t.Fatalf("reconnects=%d want 1", s.Reconnects)
	}
	if s.BufferDepth != 0 {
		t.Fatalf("buffer should be drained, depth=%d", s.BufferDepth)
	}
}

// Buffer bound: while NATS is down, pushing past capacity evicts the
// oldest events and counts them — engine-facing ingest never blocks.
func TestBridgeBufferBound(t *testing.T) {
	pub := newFakePublisher()
	pub.down.Store(true)
	b, _ := startBridge(t, testConfig(0, 16), pub, MapResolver{})
	fb := flatbuffers.NewBuilder(256)

	for i := 0; i < 40; i++ {
		b.HandleFragment(newFillEvent(fb, uint64(i+1), 1, 2))
	}
	waitForCond(t, 2*time.Second, "buffer bound", func() bool {
		return b.Snapshot().BufferDepth == 16
	})
	s := b.Snapshot()
	if s.Dropped != 24 {
		t.Fatalf("dropped=%d want 24", s.Dropped)
	}
	// Surviving head is seq 25 (0-indexed event 25th pushed).
	e, _, ok := b.buf.Head()
	if !ok || e.seq != 25 {
		t.Fatalf("oldest surviving seq=%v want 25", e)
	}
}

// Heartbeat is emitted on bridge.health.<shard> at the configured cadence.
func TestBridgeHeartbeat(t *testing.T) {
	pub := newFakePublisher()
	cfg := testConfig(2, 16)
	cfg.HeartbeatInterval = 20 * time.Millisecond
	b, _ := startBridge(t, cfg, pub, MapResolver{})

	waitForCond(t, 2*time.Second, "heartbeats", func() bool {
		return len(pub.hbs()) >= 2
	})
	hb := pub.hbs()[0]
	if hb.subject != "bridge.health.2" {
		t.Fatalf("heartbeat subject %q want bridge.health.2", hb.subject)
	}
	if len(hb.payload) == 0 {
		t.Fatal("heartbeat payload empty")
	}
	if b.Snapshot().Heartbeats < 2 {
		t.Fatalf("heartbeats metric=%d", b.Snapshot().Heartbeats)
	}
}

// A garbage fragment must not panic into the Aeron cgo trampoline.
func TestBridgeMalformedFragment(t *testing.T) {
	pub := newFakePublisher()
	b, _ := startBridge(t, testConfig(0, 16), pub, MapResolver{})

	b.HandleFragment(nil)
	b.HandleFragment([]byte{0x00})
	b.HandleFragment([]byte("this is not a flatbuffer event...................."))

	s := b.Snapshot()
	if s.Malformed < 2 { // empty + 1-byte counted before decode
		t.Fatalf("malformed=%d", s.Malformed)
	}
	if s.Received != 0 {
		t.Fatalf("received=%d want 0", s.Received)
	}
}
