package marketdata

// Phase-3 Task 4 (IMP-PLAN) — SBEFeed coverage: BookDelta → incremental
// BookUpdate diffing (upsert + delete), unknown-symbol drop accounting,
// and BookKeeper convergence on the emitted packet stream.

import (
	"context"
	"sync"
	"testing"
	"time"

	"exchange/internal/sbe"
)

type capSender struct {
	mu  sync.Mutex
	out [][]byte
}

func (c *capSender) Send(_ context.Context, dg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(dg))
	copy(cp, dg)
	c.out = append(c.out, cp)
	return nil
}

func (c *capSender) datagrams() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out
}

func newTestFeed(t *testing.T) (*SBEFeed, *capSender, *capSender) {
	t.Helper()
	a, b := &capSender{}, &capSender{}
	pub, err := sbe.NewPublisher(sbe.PublisherConfig{
		ChannelID: 7, FeedA: a, FeedB: b, Journal: sbe.NewJournal(64),
	})
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	return NewSBEFeed(pub, map[string]uint32{"EUR/USD": 1}, nil), a, b
}

func drive(t *testing.T, f *SBEFeed, deltas ...BookDelta) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tap := f.Tap()
	go f.Run(ctx, 24*time.Hour) // heartbeats off for determinism
	for _, d := range deltas {
		tap <- d
	}
	// The tap channel is buffered — drain deterministically by waiting
	// for the publisher's seq to cover every non-empty emit. Deltas that
	// produce zero messages don't consume seq; poll until quiescent.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
		if len(tap) == 0 && len(deltas) > 0 {
			return
		}
	}
}

func delta(sym string, bids, asks []Level) BookDelta {
	return BookDelta{Symbol: sym, Bids: bids, Asks: asks, Ts: time.Now()}
}

func TestSBEFeedUpsertAndDelete(t *testing.T) {
	f, fa, fb := newTestFeed(t)

	drive(t, f,
		delta("EUR/USD",
			[]Level{{Price: 105000, Qty: 100, Count: 1}},
			[]Level{{Price: 105010, Qty: 200, Count: 2}}),
		// Bid level changes qty; a second bid appears; ask unchanged.
		delta("EUR/USD",
			[]Level{{Price: 105000, Qty: 150, Count: 1},
				{Price: 104990, Qty: 50, Count: 1}},
			[]Level{{Price: 105010, Qty: 200, Count: 2}}),
		// Best bid vanishes → explicit delete.
		delta("EUR/USD",
			[]Level{{Price: 104990, Qty: 50, Count: 1}},
			nil),
	)

	dgs := fa.datagrams()
	if len(dgs) != len(fb.datagrams()) {
		t.Fatalf("dual feeds diverged: A=%d B=%d", len(dgs), len(fb.datagrams()))
	}
	if len(dgs) < 2 {
		t.Fatalf("expected ≥2 published packets, got %d", len(dgs))
	}

	var updates, deletes int
	bk := sbe.NewBookKeeper()
	for _, dg := range dgs {
		p, err := sbe.DecodePacket(dg)
		if err != nil {
			t.Fatalf("decode packet: %v", err)
		}
		msgs, err := p.DecodeMessages()
		if err != nil {
			t.Fatalf("decode messages: %v", err)
		}
		for _, m := range msgs {
			if bu, ok := m.(sbe.BookUpdate); ok {
				bk.Apply(sbe.Envelope{Seq: p.Seq, Msg: m})
				if bu.Action == sbe.BookActionDelete {
					deletes++
				} else {
					updates++
				}
			}
		}
	}
	if updates == 0 || deletes == 0 {
		t.Fatalf("want upserts+deletes, got %d/%d", updates, deletes)
	}

	// Convergence: the reconstructed book equals the last delta.
	b := bk.Books[1]
	if b == nil {
		t.Fatal("no reconstructed book for instrument 1")
	}
	if len(b.Bids) != 1 || b.Bids[104990] != 50 {
		t.Fatalf("bids: %+v", b.Bids)
	}
	if len(b.Asks) != 0 {
		t.Fatalf("asks not cleared by delete: %+v", b.Asks)
	}
}

func TestSBEFeedUnknownSymbolDrops(t *testing.T) {
	f, fa, _ := newTestFeed(t)
	drive(t, f, delta("GBP/USD", []Level{{Price: 1, Qty: 1}}, nil))
	if got := f.DroppedNoID(); got != 1 {
		t.Fatalf("dropped counter %d, want 1", got)
	}
	if len(fa.datagrams()) != 0 {
		t.Fatal("unknown symbol published a packet")
	}
}

func TestSBEFeedIdenticalDeltaEmitsNothing(t *testing.T) {
	f, fa, _ := newTestFeed(t)
	d := delta("EUR/USD", []Level{{Price: 1, Qty: 1}}, nil)
	drive(t, f, d, d)
	if len(fa.datagrams()) != 1 {
		t.Fatalf("identical delta re-published: %d packets", len(fa.datagrams()))
	}
}
