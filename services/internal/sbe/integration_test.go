// End-to-end pipeline tests over LoopbackBus transports plus real TCP
// loopback for the replay/snapshot servers (real multicast is a deploy-time
// check — see package doc).
package sbe

import (
	"context"
	"testing"
	"time"
)

// harness wires publisher → two buses → assembler, with per-feed drop sets.
type harness struct {
	pub   *Publisher
	j     *Journal
	rxA   *LoopbackReceiver
	rxB   *LoopbackReceiver
	asm   *Assembler
	col   *collect
	dropA map[uint64]bool
	dropB map[uint64]bool
}

func newHarness(t *testing.T, journalCap int, snap SnapshotSource) *harness {
	t.Helper()
	busA, busB := NewLoopbackBus(), NewLoopbackBus()
	h := &harness{j: NewJournal(journalCap), dropA: map[uint64]bool{}, dropB: map[uint64]bool{}}
	var clock int64 = 5000
	p, err := NewPublisher(PublisherConfig{
		ChannelID: testChannel, SessionID: testSession,
		FeedA:   FilterSender(busA.Sender(), func(seq uint64, _ []byte) bool { return h.dropA[seq] }),
		FeedB:   FilterSender(busB.Sender(), func(seq uint64, _ []byte) bool { return h.dropB[seq] }),
		Journal: h.j, NowNs: func() int64 { clock++; return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.pub, h.rxA, h.rxB = p, busA.Subscribe(4096), busB.Subscribe(4096)
	h.col = &collect{book: NewBookKeeper()}
	a, err := NewAssembler(AssemblerConfig{
		ChannelID: testChannel,
		Replay:    JournalSource{J: h.j},
		Snapshot:  snap,
		Emit:      h.col.emit, OnEvent: h.col.event,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.asm = a
	return h
}

// pump drains both bus channels into the assembler in deterministic
// A-then-B order per packet.
func (h *harness) pump(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		select {
		case dg := <-h.rxA.Ch():
			_ = h.asm.OnDatagram(ctx, FeedA, dg)
		default:
		}
		select {
		case dg := <-h.rxB.Ch():
			_ = h.asm.OnDatagram(ctx, FeedB, dg)
		default:
		}
	}
}

func TestEndToEnd_ABMulticast_ReplayGapFill(t *testing.T) {
	h := newHarness(t, 64, JournalSnapshotter{J: nil})
	// Fix the snapshot source to see the live journal.
	h.asm.cfg.Snapshot = JournalSnapshotter{J: h.j, SessionID: testSession}
	ctx := context.Background()

	// Publish 10 book updates; drop 4,5 on feed A and 6 on feed B.
	h.dropA[4], h.dropA[5] = true, true
	h.dropB[6] = true
	for i := 0; i < 10; i++ {
		if _, err := h.pub.Publish(ctx, BookUpdate{InstrumentID: 1, Side: SideAsk,
			Action: BookActionUpdate, PriceTicks: int64(200 + i), QtyLots: 1}); err != nil {
			t.Fatal(err)
		}
	}
	h.pump(ctx, 10)

	got := h.col.emittedSeqs()
	if len(got) != 10 {
		t.Fatalf("emitted seqs %v, want 1..10", got)
	}
	for i, s := range got {
		if s != uint64(i+1) {
			t.Fatalf("gap in emitted seqs: %v", got)
		}
	}
	// Book converged: 10 ask levels.
	b := h.col.book.Books[1]
	if b == nil || len(b.Asks) != 10 {
		t.Fatalf("reconstructed book wrong: %+v", b)
	}
	// Asymmetric delivery order (A's seq-6 arrived before B's seq-4) fired
	// gap detection and a bounded replay fill — but zero client-visible
	// gap: the emitted stream is contiguous 1..10.
	if h.asm.Stats.Duplicates == 0 {
		t.Fatal("expected duplicate suppression to have engaged")
	}
}

func TestEndToEnd_BothFeedsLost_TCPReplayAndSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHarness(t, 4, nil) // journal cap 4 → deep gaps exceed retention
	h.asm.cfg.Snapshot = nil   // set below after TCP servers bind

	// Wire real TCP replay + snapshot servers backed by the journal.
	rs := &ReplayServer{Sources: map[uint16]*Journal{testChannel: h.j}}
	go func() { _ = rs.ListenAndServe(ctx, "127.0.0.1:0") }()
	deadline := time.Now().Add(5 * time.Second)
	for rs.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("replay server did not bind")
		}
		time.Sleep(time.Millisecond)
	}
	snapSrc := JournalSnapshotter{J: h.j, SessionID: testSession}
	ss := &SnapshotServer{Sources: map[uint16]SnapshotSource{testChannel: snapSrc}}
	go func() { _ = ss.ListenAndServe(ctx, "127.0.0.1:0") }()
	for ss.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("snapshot server did not bind")
		}
		time.Sleep(time.Millisecond)
	}
	h.asm.cfg.Replay = TCPReplaySource{Addr: rs.Addr().String()}
	h.asm.cfg.Snapshot = TCPSnapshotSource{Addr: ss.Addr().String()}

	// Publish 12 packets; deliver 1..3, lose 4..11 on BOTH feeds, then 12.
	dualDrop := map[uint64]bool{4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true}
	h.dropA, h.dropB = dualDrop, map[uint64]bool{}
	for k := range dualDrop {
		h.dropB[k] = true
	}
	for i := 0; i < 12; i++ {
		if _, err := h.pub.Publish(ctx, BookUpdate{InstrumentID: 1, Side: SideBid,
			Action: BookActionUpdate, PriceTicks: int64(300 + i), QtyLots: 2}); err != nil {
			t.Fatal(err)
		}
	}
	h.pump(ctx, 12)

	// Journal retains 9..12 → replay [4,11] is out of range → TCP snapshot
	// at lastSeq=12 covers the hole (SBE_MULTICAST_RECOVERY).
	if !hasEvent(h.col.kinds(), EventReplayGapExceeded) || !hasEvent(h.col.kinds(), EventSnapshotRecovery) {
		t.Fatalf("expected replay-gap → snapshot recovery: %v", h.col.kinds())
	}
	if h.asm.Expected() != 13 {
		t.Fatalf("expected=%d, want 13", h.asm.Expected())
	}
	// JournalSnapshotter folds ONLY retained packets (seqs 9..12 →
	// prices 308..311) — the snapshot must reconstruct exactly 4 levels.
	b := h.col.book.Books[1]
	if b == nil || len(b.Bids) != 4 {
		t.Fatalf("snapshot book has %v levels, want 4 (retained)", b)
	}
	// Continuity resumes on live packets.
	if _, err := h.pub.Publish(ctx, BookUpdate{InstrumentID: 1, Side: SideBid,
		Action: BookActionUpdate, PriceTicks: 999, QtyLots: 1}); err != nil {
		t.Fatal(err)
	}
	h.pump(ctx, 1)
	if h.asm.Expected() != 14 || len(h.col.book.Books[1].Bids) != 5 {
		t.Fatalf("post-recovery live incremental failed: expected=%d book=%+v",
			h.asm.Expected(), h.col.book.Books[1])
	}
}

func TestDeterministicReplayOrdering(t *testing.T) {
	// Replayed packets must reproduce identical bytes AND seq ordering
	// (Task 6.3.6: deterministic replay).
	j := NewJournal(64)
	ctx := context.Background()
	var orig [][]byte
	for seq := uint64(1); seq <= 20; seq++ {
		dg := mkDg(t, seq, Heartbeat{EventTimeNs: int64(seq)})
		if err := j.Append(seq, dg); err != nil {
			t.Fatal(err)
		}
		orig = append(orig, dg)
	}
	dgs, err := (JournalSource{J: j}).Replay(ctx, testChannel, 3, 17)
	if err != nil {
		t.Fatal(err)
	}
	if len(dgs) != 15 {
		t.Fatalf("replayed %d packets, want 15", len(dgs))
	}
	for i, dg := range dgs {
		p, err := DecodePacket(dg)
		if err != nil {
			t.Fatal(err)
		}
		if p.Seq != uint64(i+3) {
			t.Fatalf("replayed seq %d at position %d", p.Seq, i+3)
		}
		if string(dg) != string(orig[i+2]) {
			t.Fatalf("replayed bytes differ at seq %d", i+3)
		}
	}
}
