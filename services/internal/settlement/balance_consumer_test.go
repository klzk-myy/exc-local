// balance_consumer_test.go — coverage for the post-commit JetStream
// republish seam (TradeRepublisher / WithRepublisher).
package settlement

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
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
