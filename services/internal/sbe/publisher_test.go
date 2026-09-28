// Publisher tests: byte-identical A/B packets, identical channel sequences,
// journal retention, heartbeat continuity, single-feed failure tolerance.
package sbe

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestPublisherABByteEquivalence(t *testing.T) {
	busA, busB := NewLoopbackBus(), NewLoopbackBus()
	rxA, rxB := busA.Subscribe(16), busB.Subscribe(16)
	j := NewJournal(64)
	var clock int64 = 1000
	p, err := NewPublisher(PublisherConfig{
		ChannelID: testChannel, SessionID: testSession,
		FeedA: busA.Sender(), FeedB: busB.Sender(), Journal: j,
		NowNs: func() int64 { clock++; return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := p.Publish(ctx, BookUpdate{InstrumentID: 1, Side: SideBid,
			Action: BookActionUpdate, PriceTicks: int64(100 + i), QtyLots: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Seq() != 6 {
		t.Fatalf("seq=%d, want 6", p.Seq())
	}
	if j.Len() != 6 {
		t.Fatalf("journal len=%d, want 6", j.Len())
	}
	// §24 #166: identical bytes + identical seq on both feeds.
	for i := 0; i < 6; i++ {
		dgA, err := rxA.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		dgB, err := rxB.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dgA, dgB) {
			t.Fatalf("packet %d: feed bytes diverge", i+1)
		}
		pA, _ := DecodePacket(dgA)
		if pA.Seq != uint64(i+1) || pA.SessionID != testSession {
			t.Fatalf("packet %d header seq=%d session=%d", i, pA.Seq, pA.SessionID)
		}
	}
}

func TestPublisherSingleFeedFailureContinues(t *testing.T) {
	busA := NewLoopbackBus()
	j := NewJournal(8)
	p, err := NewPublisher(PublisherConfig{
		ChannelID: testChannel, SessionID: testSession,
		FeedA:   busA.Sender(),
		FeedB:   failSender{err: errors.New("simulated feed B NIC failure")},
		Journal: j,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), Heartbeat{EventTimeNs: 1}); err != nil {
		t.Fatalf("single-feed failure must not fail publish: %v", err)
	}
	if p.Stats.FeedBSendErr.Load() != 1 {
		t.Fatal("feed B error not counted")
	}
}

func TestPublisherDualFailureJournals(t *testing.T) {
	j := NewJournal(8)
	e := errors.New("down")
	p, err := NewPublisher(PublisherConfig{
		ChannelID: testChannel, SessionID: testSession,
		FeedA: failSender{err: e}, FeedB: failSender{err: e}, Journal: j,
	})
	if err != nil {
		t.Fatal(err)
	}
	seq, err := p.Publish(context.Background(), Heartbeat{EventTimeNs: 1})
	if err == nil {
		t.Fatal("expected dual-failure error")
	}
	if seq != 1 || j.Len() != 1 {
		t.Fatalf("packet must remain journaled under seq 1: seq=%d journal=%d", seq, j.Len())
	}
	// Continuity preserved: next publish is seq 2 and replay can fill the
	// never-multicast seq 1.
	if _, err := p.Publish(context.Background(), Heartbeat{EventTimeNs: 2}); err == nil {
		t.Fatal("expected error")
	}
	dgs, err := j.Range(1, 2)
	if err != nil || len(dgs) != 2 {
		t.Fatalf("replayable range broken: %v", err)
	}
}

type failSender struct{ err error }

func (f failSender) Send(context.Context, []byte) error { return f.err }
