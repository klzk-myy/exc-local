// Assembler tests: A/B arbitration, dedup, gap→replay→resume, out-of-range
// snapshot recovery, channel reset, feed desync — all deterministic over
// loopback datagrams (no real multicast required).
package sbe

import (
	"context"
	"testing"
)

const testChannel uint16 = 7
const testSession uint64 = 111

func mkDg(t *testing.T, seq uint64, msgs ...Message) []byte {
	t.Helper()
	enc := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		enc = append(enc, MarshalMessage(m))
	}
	dg, err := EncodePacket(testChannel, testSession, seq, enc)
	if err != nil {
		t.Fatalf("encode packet: %v", err)
	}
	return dg
}

func mkDgSession(t *testing.T, session, seq uint64, msgs ...Message) []byte {
	t.Helper()
	enc := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		enc = append(enc, MarshalMessage(m))
	}
	dg, err := EncodePacket(testChannel, session, seq, enc)
	if err != nil {
		t.Fatalf("encode packet: %v", err)
	}
	return dg
}

// collect captures emitted envelopes and feeds them into a BookKeeper.
type collect struct {
	envs []Envelope
	book *BookKeeper
	evts []Event
}

func (c *collect) emit(e Envelope) {
	c.envs = append(c.envs, e)
	c.book.Apply(e)
}

func (c *collect) event(e Event) { c.evts = append(c.evts, e) }

func (c *collect) kinds() []EventKind {
	var out []EventKind
	for _, e := range c.evts {
		out = append(out, e.Kind)
	}
	return out
}

func hasEvent(kinds []EventKind, k EventKind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// emittedSeqs returns the channel seqs actually emitted (deduped view).
func (c *collect) emittedSeqs() []uint64 {
	var out []uint64
	last := uint64(0)
	for _, e := range c.envs {
		if e.Snapshot {
			continue
		}
		if e.Seq != last {
			out = append(out, e.Seq)
			last = e.Seq
		}
	}
	return out
}

func TestDedupBySeq(t *testing.T) {
	c := &collect{book: NewBookKeeper()}
	a, err := NewAssembler(AssemblerConfig{ChannelID: testChannel, Emit: c.emit, OnEvent: c.event})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dg1 := mkDg(t, 1, BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate, PriceTicks: 100, QtyLots: 10})
	dg2 := mkDg(t, 2, Trade{TradeID: 5, InstrumentID: 1, AggressorSide: SideAsk, PriceTicks: 101, QtyLots: 3})

	// Deliver each packet on A then B (dup).
	for _, dg := range [][]byte{dg1, dg2} {
		if err := a.OnDatagram(ctx, FeedA, dg); err != nil {
			t.Fatal(err)
		}
		if err := a.OnDatagram(ctx, FeedB, dg); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(c.envs); got != 2 {
		t.Fatalf("emitted %d envelopes, want 2", got)
	}
	if a.Stats.Duplicates != 2 {
		t.Fatalf("duplicates=%d, want 2", a.Stats.Duplicates)
	}
	if a.Expected() != 3 {
		t.Fatalf("expected=%d, want 3", a.Expected())
	}
}

func TestSingleFeedLossZeroGap(t *testing.T) {
	// §24 #166 / §10.6: feed A drops packets; B delivers all — the consumer
	// sees zero gap and zero recovery events.
	c := &collect{book: NewBookKeeper()}
	a, err := NewAssembler(AssemblerConfig{ChannelID: testChannel, Emit: c.emit, OnEvent: c.event})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	droppedOnA := map[uint64]bool{3: true, 4: true, 5: true}
	for seq := uint64(1); seq <= 8; seq++ {
		dg := mkDg(t, seq, BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate, PriceTicks: int64(seq), QtyLots: 1})
		if !droppedOnA[seq] {
			if err := a.OnDatagram(ctx, FeedA, dg); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.OnDatagram(ctx, FeedB, dg); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(c.emittedSeqs()); got != 8 {
		t.Fatalf("emitted seqs %d, want 8", got)
	}
	if a.Stats.Gaps != 0 {
		t.Fatalf("gaps=%d, want 0 — feed B covered loss", a.Stats.Gaps)
	}
	b := c.book.Books[1]
	if b == nil || len(b.Bids) != 8 {
		t.Fatalf("book levels %v, want 8", b)
	}
}

func TestGapReplayResumeContinuity(t *testing.T) {
	// Drop seqs 4..6 on BOTH feeds; journal retains everything. Packet 7's
	// arrival triggers replay [4,6]; the stream resumes in order.
	j := NewJournal(64)
	var dgs [][]byte
	for seq := uint64(1); seq <= 10; seq++ {
		dg := mkDg(t, seq, BookUpdate{InstrumentID: 1, Side: SideAsk, Action: BookActionUpdate, PriceTicks: int64(seq), QtyLots: uint64(seq)})
		if err := j.Append(seq, dg); err != nil {
			t.Fatal(err)
		}
		dgs = append(dgs, dg)
	}
	c := &collect{book: NewBookKeeper()}
	a, err := NewAssembler(AssemblerConfig{
		ChannelID: testChannel,
		Replay:    JournalSource{J: j},
		Emit:      c.emit, OnEvent: c.event,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, i := range []int{0, 1, 2, 6, 7, 8, 9} { // seqs 1,2,3,7,8,9,10 — 4..6 lost on both feeds
		if err := a.OnDatagram(ctx, FeedA, dgs[i]); err != nil {
			t.Fatal(err)
		}
	}
	got := c.emittedSeqs()
	if len(got) != 10 {
		t.Fatalf("emitted %d seqs %v, want 1..10", len(got), got)
	}
	for i, s := range got {
		if s != uint64(i+1) {
			t.Fatalf("seq[%d]=%d, want %d", i, s, i+1)
		}
	}
	if a.Stats.ReplayedPackets != 3 {
		t.Fatalf("replayed=%d, want 3", a.Stats.ReplayedPackets)
	}
	if !hasEvent(c.kinds(), EventGapDetected) || !hasEvent(c.kinds(), EventReplayApplied) {
		t.Fatalf("missing events: %v", c.kinds())
	}
	// Replay flag provenance.
	var replayed int
	for _, e := range c.envs {
		if e.Replay {
			replayed++
		}
	}
	if replayed != 3 {
		t.Fatalf("replay-flagged envelopes %d, want 3", replayed)
	}
}

func TestGapBeyondRetention_SnapshotRecovery(t *testing.T) {
	// Journal retains only the last 5 packets; consumer misses seqs 4..11 —
	// replay [4,11] is out of range → SBE_REPLAY_GAP_EXCEEDED → snapshot.
	j := NewJournal(5)
	var dgs [][]byte
	for seq := uint64(1); seq <= 12; seq++ {
		dg := mkDg(t, seq, BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate, PriceTicks: int64(seq), QtyLots: uint64(seq)})
		if err := j.Append(seq, dg); err != nil {
			t.Fatal(err)
		}
		dgs = append(dgs, dg)
	}
	c := &collect{book: NewBookKeeper()}
	a, err := NewAssembler(AssemblerConfig{
		ChannelID: testChannel,
		Replay:    JournalSource{J: j},
		Snapshot:  JournalSnapshotter{J: j, SessionID: testSession},
		Emit:      c.emit, OnEvent: c.event,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ { // 1,2,3 arrive
		if err := a.OnDatagram(ctx, FeedA, dgs[i]); err != nil {
			t.Fatal(err)
		}
	}
	// seq 12 arrives — hole 4..11 exceeds the 5-packet journal.
	if err := a.OnDatagram(ctx, FeedA, dgs[11]); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(c.kinds(), EventReplayGapExceeded) {
		t.Fatalf("missing SBE_REPLAY_GAP_EXCEEDED event: %v", c.kinds())
	}
	if !hasEvent(c.kinds(), EventSnapshotRecovery) {
		t.Fatalf("missing SBE_MULTICAST_RECOVERY event: %v", c.kinds())
	}
	if a.Expected() != 13 {
		t.Fatalf("expected=%d, want 13 (snapshot lastSeq 12 + 1)", a.Expected())
	}
	// Snapshot rebuilt state from the retained window: seqs 8..12 →
	// prices 8..12 → 5 bid levels; the pre-gap live levels are replaced.
	b := c.book.Books[1]
	if b == nil || len(b.Bids) != 5 {
		t.Fatalf("reconstructed book has %v levels, want 5", b)
	}
	if _, ok := b.Bids[12]; !ok {
		t.Fatal("missing level priceTicks=12 from snapshot")
	}
	// Live stream resumes after the snapshot.
	if err := a.OnDatagram(ctx, FeedA, mkDg(t, 13, BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate, PriceTicks: 13, QtyLots: 1})); err != nil {
		t.Fatal(err)
	}
	if len(c.book.Books[1].Bids) != 6 {
		t.Fatal("post-snapshot incremental not applied")
	}
}

func TestSnapshotRacesLiveIncrementals(t *testing.T) {
	// While the snapshot is being fetched, live packets 13 and 14 arrive.
	// They must queue and apply only after the snapshot (spec §10.4
	// convergence requirement).
	j := NewJournal(5)
	var dgs [][]byte
	for seq := uint64(1); seq <= 14; seq++ {
		dg := mkDg(t, seq, BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate, PriceTicks: int64(seq), QtyLots: 1})
		if err := j.Append(seq, dg); err != nil {
			t.Fatal(err)
		}
		dgs = append(dgs, dg)
	}
	c := &collect{book: NewBookKeeper()}
	var a *Assembler
	// Snapshot source that delivers live packets mid-fetch (re-entrancy).
	src := &racingSnapshotter{
		inner: JournalSnapshotter{J: j, SessionID: testSession},
		deliver: func() {
			_ = a.OnDatagram(context.Background(), FeedB, dgs[12]) // seq 13
			_ = a.OnDatagram(context.Background(), FeedB, dgs[13]) // seq 14
		},
	}
	a, _ = NewAssembler(AssemblerConfig{
		ChannelID: testChannel,
		Replay:    JournalSource{J: j},
		Snapshot:  src,
		Emit:      c.emit, OnEvent: c.event,
	})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := a.OnDatagram(ctx, FeedA, dgs[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.OnDatagram(ctx, FeedA, dgs[11]); err != nil { // seq 12 → gap
		t.Fatal(err)
	}
	if a.Expected() != 15 {
		t.Fatalf("expected=%d, want 15", a.Expected())
	}
	// Journal retains seqs 10..14 → snapshot state is 5 levels; queued
	// live packets 13/14 fell inside the snapshot horizon and were
	// suppressed rather than double-applied.
	if len(c.book.Books[1].Bids) != 5 {
		t.Fatalf("book has %d levels, want 5", len(c.book.Books[1].Bids))
	}
}

// racingSnapshotter fires live datagrams into the assembler during Snapshot.
type racingSnapshotter struct {
	inner   SnapshotSource
	deliver func()
	fired   bool
}

func (r *racingSnapshotter) Snapshot(ctx context.Context, channelID uint16) (*Snapshot, error) {
	if !r.fired {
		r.fired = true
		r.deliver()
	}
	return r.inner.Snapshot(ctx, channelID)
}

func TestChannelResetOnNewSession(t *testing.T) {
	c := &collect{book: NewBookKeeper()}
	a, err := NewAssembler(AssemblerConfig{
		ChannelID: testChannel,
		Emit:      c.emit, OnEvent: c.event,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for seq := uint64(1); seq <= 5; seq++ {
		if err := a.OnDatagram(ctx, FeedA, mkDg(t, seq)); err != nil {
			t.Fatal(err)
		}
	}
	// Publisher restarts: new session epoch, seq restarts at 1. The session
	// field — not seq==1 — is the reset signal.
	resetSnap := BuildSnapshotSeq(testChannel, 222 /* new session */, 0, 0, 0, []Message{
		BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate, PriceTicks: 1, QtyLots: 1},
	})
	a.cfg.Snapshot = StaticSnapshotSource{Snap: resetSnap}
	if err := a.OnDatagram(ctx, FeedB, mkDgSession(t, 222, 1,
		BookUpdate{InstrumentID: 2, Side: SideAsk, Action: BookActionUpdate, PriceTicks: 9, QtyLots: 9})); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(c.kinds(), EventChannelReset) {
		t.Fatalf("missing CHANNEL_RESET: %v", c.kinds())
	}
	if a.Stats.ChannelResets != 1 {
		t.Fatalf("resets=%d, want 1", a.Stats.ChannelResets)
	}
	// A delayed OLD-epoch straggler (session 111 < 222) is stale — dropped
	// without a second reset.
	before := a.Stats.ChannelResets
	if err := a.OnDatagram(ctx, FeedB, mkDg(t, 1)); err != nil { // old session
		t.Fatal(err)
	}
	if a.Stats.ChannelResets != before {
		t.Fatal("old-epoch straggler triggered a spurious reset")
	}
	if a.session != 222 {
		t.Fatalf("session=%d, want 222", a.session)
	}
	if !hasEvent(c.kinds(), EventStalePacket) {
		t.Fatalf("missing STALE_PACKET: %v", c.kinds())
	}
}

func TestFeedDesyncByteDivergence(t *testing.T) {
	c := &collect{book: NewBookKeeper()}
	a, err := NewAssembler(AssemblerConfig{ChannelID: testChannel, Emit: c.emit, OnEvent: c.event})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dg := mkDg(t, 1, BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate, PriceTicks: 100, QtyLots: 10})
	if err := a.OnDatagram(ctx, FeedA, dg); err != nil {
		t.Fatal(err)
	}
	// Feed B's copy of seq 1 carries mutated bytes (same seq, different
	// payload) — SBE_FEED_A_DESYNC.
	mut := append([]byte{}, dg...)
	mut[len(mut)-1] ^= 0xFF // flip last payload byte
	if err := a.OnDatagram(ctx, FeedB, mut); err != nil {
		t.Fatal(err)
	}
	if a.Stats.Desyncs != 1 || !hasEvent(c.kinds(), EventFeedDesync) {
		t.Fatalf("desync not detected: stats=%v events=%v", a.Stats.Desyncs, c.kinds())
	}
	if got := len(c.envs); got != 1 {
		t.Fatalf("emitted %d, want 1 (mutated dup suppressed)", got)
	}
}

func TestBothFeedsGapAtDifferentSeqs(t *testing.T) {
	// Edge case from the SDD checklist: feed A loses seqs 4,5; feed B loses
	// 5,6 — seq 5 is lost on BOTH, so arbitration alone cannot cover it;
	// replay fills the shared hole and the disjoint losses are invisible.
	j := NewJournal(16)
	var dgs [][]byte
	for seq := uint64(1); seq <= 8; seq++ {
		dg := mkDg(t, seq, BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate, PriceTicks: int64(seq), QtyLots: 1})
		if err := j.Append(seq, dg); err != nil {
			t.Fatal(err)
		}
		dgs = append(dgs, dg)
	}
	c := &collect{book: NewBookKeeper()}
	a, err := NewAssembler(AssemblerConfig{
		ChannelID: testChannel, Replay: JournalSource{J: j},
		Emit: c.emit, OnEvent: c.event,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	aDrops := map[uint64]bool{4: true, 5: true}
	bDrops := map[uint64]bool{5: true, 6: true}
	for i := uint64(1); i <= 8; i++ {
		if !aDrops[i] {
			if err := a.OnDatagram(ctx, FeedA, dgs[i-1]); err != nil {
				t.Fatal(err)
			}
		}
		if !bDrops[i] {
			if err := a.OnDatagram(ctx, FeedB, dgs[i-1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := c.emittedSeqs()
	if len(got) != 8 {
		t.Fatalf("emitted %v, want 1..8", got)
	}
	for i, s := range got {
		if s != uint64(i+1) {
			t.Fatalf("gap in emitted seqs %v", got)
		}
	}
	if a.Stats.ReplayedPackets != 1 {
		t.Fatalf("replayed=%d, want 1 (seq 5)", a.Stats.ReplayedPackets)
	}
}

func TestDelayedDupOfFirstSeqIsNotAReset(t *testing.T) {
	// Regression guard: a delayed Feed-B duplicate of seq 1 must not read
	// as a channel reset (session ID proves epoch).
	c := &collect{book: NewBookKeeper()}
	a, err := NewAssembler(AssemblerConfig{ChannelID: testChannel, Emit: c.emit, OnEvent: c.event})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dg1 := mkDg(t, 1, Heartbeat{EventTimeNs: 1})
	dg2 := mkDg(t, 2, Heartbeat{EventTimeNs: 2})
	if err := a.OnDatagram(ctx, FeedA, dg1); err != nil {
		t.Fatal(err)
	}
	if err := a.OnDatagram(ctx, FeedA, dg2); err != nil {
		t.Fatal(err)
	}
	if err := a.OnDatagram(ctx, FeedB, dg1); err != nil { // late dup of seq 1
		t.Fatal(err)
	}
	if a.Stats.ChannelResets != 0 {
		t.Fatal("delayed dup of seq 1 misdetected as channel reset")
	}
	if a.Stats.Duplicates != 1 {
		t.Fatalf("duplicates=%d, want 1", a.Stats.Duplicates)
	}
}
