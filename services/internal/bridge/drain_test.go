package bridge

import (
	"context"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
)

// Task 9.3.23 item 4: after Run exits on cancel, Flush pushes the
// residual buffer to NATS before the process terminates.
func TestFlushDrainsBuffer(t *testing.T) {
	pub := newFakePublisher()
	b, err := New(testConfig(0, 64), pub, MapResolver{3: "EUR-USD"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pub.down.Store(true) // events buffer but don't publish

	fb := flatbuffers.NewBuilder(256)
	b.HandleFragment(newOrderEvent(fb, 1, 1001, 3))
	b.HandleFragment(newFillEvent(fb, 2, 1001, 2002))
	if b.Snapshot().BufferDepth == 0 {
		t.Fatal("events did not buffer")
	}

	// NATS recovers during the drain window → flush publishes in order.
	pub.down.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if b.Snapshot().BufferDepth != 0 {
		t.Fatalf("buffer depth %d after flush", b.Snapshot().BufferDepth)
	}
	if len(pub.pubs()) != 4 {
		t.Fatalf("published %d calls, want 4 (2 subjects × 2 events)", len(pub.pubs()))
	}
}

// Flush honors the shutdown deadline: with NATS down it returns the ctx
// error and reports the residual (never silently discards).
func TestFlushBoundedByContext(t *testing.T) {
	pub := newFakePublisher()
	pub.down.Store(true)
	b, err := New(testConfig(0, 64), pub, MapResolver{3: "EUR-USD"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fb := flatbuffers.NewBuilder(256)
	b.HandleFragment(newOrderEvent(fb, 1, 1001, 3))

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if err := b.Flush(ctx); err == nil {
		t.Fatal("flush with down publisher must not report success")
	}
	if b.Snapshot().BufferDepth == 0 {
		t.Fatal("buffer emptied despite failed publishes")
	}
}
