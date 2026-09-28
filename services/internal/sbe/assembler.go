// Consumer-side A/B assembler (Task 6.3.6 item 4): joins both feeds,
// arbitrates packets by channel sequence (first-arriving copy wins),
// suppresses duplicates, detects gaps and channel resets, and emits an
// ordered message stream ONLY where continuity is proven — during snapshot
// recovery, live incrementals are queued and emitted after exact convergence
// (spec §10.4: "do not publish a book until snapshot plus queued
// incrementals converge").
//
// Recovery ladder on a detected gap:
//  1. TCP replay [expected, minPending-1] from the retained journal.
//  2. Replay out of range → SBE_REPLAY_GAP_EXCEEDED (L3) → snapshot:
//     fetch full state, set expected = LastSeq+1, drain queued incrementals.
//
// Both-feeds loss therefore converges through TCP snapshot + replay — the
// §10.6 SBE_MULTICAST_RECOVERY path.
package sbe

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
)

// EventKind tags assembler transitions for the instrumentation of Task
// 6.3.6 item 5. Names equal the §27.2 catalog / §10.6 alert labels — these
// are internal observability events, not §23 HTTP codes.
type EventKind string

const (
	EventDuplicateSuppressed EventKind = "DUPLICATE_SUPPRESSED"
	EventGapDetected         EventKind = "GAP_DETECTED"
	EventReplayApplied       EventKind = "REPLAY_APPLIED"
	EventReplayGapExceeded   EventKind = "SBE_REPLAY_GAP_EXCEEDED" // L3
	EventSnapshotRecovery    EventKind = "SBE_MULTICAST_RECOVERY"  // §10.6
	EventFeedDesync          EventKind = "SBE_FEED_A_DESYNC"       // L1
	EventChannelReset        EventKind = "CHANNEL_RESET"
	EventStalePacket         EventKind = "STALE_PACKET"
	EventCorruptPacket       EventKind = "CORRUPT_PACKET"
)

// Event is emitted through AssemblerConfig.OnEvent.
type Event struct {
	Kind EventKind
	Feed FeedID
	Seq  uint64
	Want uint64 // expected seq at detection (gap/reset events)
}

// Envelope is one delivered message. Seq is the channel sequence of the
// carrying packet. Replay/Snapshot flag provenance so consumers can rebuild
// books correctly (snapshot state vs live/replayed incrementals).
type Envelope struct {
	Seq      uint64
	Msg      Message
	Replay   bool // delivered via replay fill
	Snapshot bool // delivered as part of a snapshot burst
}

// AssemblerConfig wires an Assembler.
type AssemblerConfig struct {
	ChannelID uint16
	Replay    ReplaySource   // gap repair; nil → always escalate to snapshot
	Snapshot  SnapshotSource // out-of-range recovery; required for StartWithSnapshot
	// Emit receives ordered envelopes; nil discards (stats only).
	Emit func(Envelope)
	// OnEvent receives transition events; nil discards.
	OnEvent func(Event)
	// PendingLimit bounds out-of-order buffered packets (default 1024).
	// Overflow escalates straight to snapshot recovery.
	PendingLimit int
	// DupWindow bounds the duplicate-detection horizon (default 4096):
	// datagram CRCs are retained for the last ~2×DupWindow seqs so a
	// duplicate delivery can be byte-compared for SBE_FEED_A_DESYNC.
	DupWindow uint64
}

// Assembler merges feeds A/B into one ordered stream. Not goroutine-safe —
// drive it from the consumer's single read loop per channel.
type Assembler struct {
	cfg      AssemblerConfig
	started  bool
	session  uint64 // publisher epoch of the stream being assembled
	expected uint64
	pending  map[uint64]*Packet // out-of-order arrivals, both feeds
	recent   map[uint64]uint32  // seq → crc32(datagram) for dup/desync checks
	// Re-entrancy guards: snapshot/replay sources may deliver live
	// datagrams while servicing a request (snapshot racing incrementals);
	// nested arrivals buffer into pending and the in-flight ladder drains
	// them after convergence.
	recovering bool
	inSnapshot bool

	Stats AssemblerStats
}

// AssemblerStats are cumulative counters for §10.4 instrumentation.
type AssemblerStats struct {
	PacketsAccepted    uint64
	Duplicates         uint64
	Desyncs            uint64
	Gaps               uint64
	ReplayedPackets    uint64
	SnapshotRecoveries uint64
	ChannelResets      uint64
	MessagesEmitted    uint64
}

// NewAssembler validates cfg.
func NewAssembler(cfg AssemblerConfig) (*Assembler, error) {
	if cfg.PendingLimit == 0 {
		cfg.PendingLimit = 1024
	}
	if cfg.DupWindow == 0 {
		cfg.DupWindow = 4096
	}
	return &Assembler{
		cfg:     cfg,
		pending: make(map[uint64]*Packet),
		recent:  make(map[uint64]uint32),
	}, nil
}

// Expected returns the next awaited channel sequence (0 before start).
func (a *Assembler) Expected() uint64 { return a.expected }

func (a *Assembler) emit(env Envelope) {
	a.Stats.MessagesEmitted++
	if a.cfg.Emit != nil {
		a.cfg.Emit(env)
	}
}

func (a *Assembler) event(k EventKind, feed FeedID, seq, want uint64) {
	if a.cfg.OnEvent != nil {
		a.cfg.OnEvent(Event{Kind: k, Feed: feed, Seq: seq, Want: want})
	}
}

// StartWithSnapshot pulls the authoritative snapshot before joining the
// feeds — the canonical institutional join of spec §10.4. Live packets
// arriving afterwards at seq > LastSeq are applied incrementally.
func (a *Assembler) StartWithSnapshot(ctx context.Context) error {
	if a.cfg.Snapshot == nil {
		return errors.New("sbe: no snapshot source configured")
	}
	if err := a.recoverSnapshot(ctx); err != nil {
		return err
	}
	a.recover(ctx) // handle anything buffered before/while joining
	return nil
}

func (a *Assembler) applySnapshot(snap *Snapshot) {
	for _, m := range snap.Messages {
		a.emit(Envelope{Seq: snap.LastSeq, Msg: m, Snapshot: true})
	}
	a.expected = snap.LastSeq + 1
	a.started = true
	a.session = snap.SessionID
	a.Stats.SnapshotRecoveries++
	// Everything at/below the snapshot horizon is now stale; so are
	// packets from a different publisher epoch than the snapshot's.
	for seq, p := range a.pending {
		if seq <= snap.LastSeq || (snap.SessionID != 0 && p.SessionID != snap.SessionID) {
			delete(a.pending, seq)
		}
	}
}

// OnDatagram feeds one received datagram from feed (FeedA or FeedB) into the
// assembler. Synchronous and deterministic: gap repair pulls replay inline.
func (a *Assembler) OnDatagram(ctx context.Context, feed FeedID, dg []byte) error {
	pkt, err := DecodePacket(dg)
	if err != nil {
		return fmt.Errorf("sbe: feed %s: %w", feed, err) // L3 malformed drop
	}
	if pkt.ChannelID != a.cfg.ChannelID {
		return nil // foreign channel — not ours
	}
	crc := crc32.ChecksumIEEE(dg)

	// Adopt the publisher epoch when unknown (snapshot without SessionID).
	if a.started && a.session == 0 && pkt.SessionID != 0 {
		a.session = pkt.SessionID
	}

	// Channel reset: a NEWER session epoch arrived (publisher restart —
	// SessionID defaults to boot-time ns, so epochs are ordered). Detected
	// on any packet of the new session — not on sequence heuristics — so
	// delayed duplicates of low seqs can never masquerade as a reset. An
	// OLDER epoch is a stale straggler, dropped without resetting.
	if a.started && a.session != 0 && pkt.SessionID != a.session {
		if pkt.SessionID < a.session {
			a.event(EventStalePacket, feed, pkt.Seq, a.expected)
			return nil
		}
		a.Stats.ChannelResets++
		a.event(EventChannelReset, feed, pkt.Seq, a.expected)
		a.started = false
		a.expected = 0
		a.session = pkt.SessionID
		a.pending = make(map[uint64]*Packet)
		a.recent = make(map[uint64]uint32) // old-epoch CRCs must not desync the new epoch
		if a.cfg.Snapshot != nil {
			_ = a.recoverSnapshot(ctx) // emits SBE_MULTICAST_RECOVERY
		}
		// Process the triggering packet against the fresh horizon.
		if a.started && pkt.Seq == a.expected {
			if a.deliver(pkt, crc, false) {
				a.drainPending()
			}
		} else if a.started && pkt.Seq > a.expected {
			a.handleGap(ctx, feed, pkt, crc)
		}
		return nil
	}

	if !a.started {
		// Cold join without prior snapshot: adopt the first arriving
		// packet as the stream head. Consumers needing full history call
		// StartWithSnapshot first.
		a.started = true
		a.session = pkt.SessionID
		a.expected = pkt.Seq
	}

	switch {
	case pkt.Seq == a.expected:
		if a.deliver(pkt, crc, false) {
			a.drainPending()
		}
	case pkt.Seq < a.expected:
		a.handleOld(feed, pkt, crc)
	default:
		a.handleGap(ctx, feed, pkt, crc)
	}
	return nil
}

// deliver emits a packet's messages and advances expected. Returns false on
// corrupt payload (expected unchanged — a journaled replay copy or snapshot
// re-delivers the seq).
func (a *Assembler) deliver(pkt *Packet, crc uint32, replayed bool) bool {
	msgs, err := pkt.DecodeMessages()
	if err != nil {
		a.event(EventCorruptPacket, FeedA, pkt.Seq, a.expected)
		return false
	}
	for _, m := range msgs {
		a.emit(Envelope{Seq: pkt.Seq, Msg: m, Replay: replayed})
	}
	a.recent[pkt.Seq] = crc
	a.expected = pkt.Seq + 1
	a.Stats.PacketsAccepted++
	a.pruneRecent()
	return true
}

// handleOld processes a below-expected packet in the SAME session: a
// duplicate delivery (or stale straggler). Byte-compare against the
// accepted/pending copy detects feed divergence (SBE_FEED_A_DESYNC, L1).
func (a *Assembler) handleOld(feed FeedID, pkt *Packet, crc uint32) {
	a.Stats.Duplicates++
	a.checkDesync(feed, pkt, crc)
	a.event(EventDuplicateSuppressed, feed, pkt.Seq, a.expected)
}

// checkDesync byte-compares a duplicate seq against the accepted/pending
// copy — mismatch means feeds A and B diverged (SBE_FEED_A_DESYNC, L1).
func (a *Assembler) checkDesync(feed FeedID, pkt *Packet, crc uint32) {
	if old, ok := a.recent[pkt.Seq]; ok && old != crc {
		a.Stats.Desyncs++
		a.event(EventFeedDesync, feed, pkt.Seq, a.expected)
		return
	}
	if p, ok := a.pending[pkt.Seq]; ok && crc32.ChecksumIEEE(p.Raw) != crc {
		a.Stats.Desyncs++
		a.event(EventFeedDesync, feed, pkt.Seq, a.expected)
	}
}

// handleGap buffers an above-expected packet and runs the recovery ladder.
func (a *Assembler) handleGap(ctx context.Context, feed FeedID, pkt *Packet, crc uint32) {
	if old, dup := a.pending[pkt.Seq]; dup {
		// Same missing seq arrived on the other feed — dedup it.
		if crc32.ChecksumIEEE(old.Raw) != crc {
			a.Stats.Desyncs++
			a.event(EventFeedDesync, feed, pkt.Seq, a.expected)
		}
		a.event(EventDuplicateSuppressed, feed, pkt.Seq, a.expected)
		return
	}
	a.Stats.Gaps++
	a.event(EventGapDetected, feed, pkt.Seq, a.expected)
	a.pending[pkt.Seq] = pkt
	a.recent[pkt.Seq] = crc
	if a.recovering || a.inSnapshot {
		return // a ladder is in flight; it drains pending
	}
	if len(a.pending) > a.cfg.PendingLimit {
		// Gap deeper than the reorder buffer: skip replay, straight to
		// snapshot recovery (bounded memory per spec §2.7 pessimism).
		if err := a.recoverSnapshot(ctx); err == nil {
			a.recover(ctx)
		}
		return
	}
	a.recover(ctx) // no-op while a ladder is already in flight
}

// recover runs the recovery ladder until pending is drained or progress
// stalls. Single-flight via the recovering flag — nested invocations (from
// recoverSnapshot or datagrams arriving mid-fetch) return immediately and
// the in-flight loop drains their buffered packets.
func (a *Assembler) recover(ctx context.Context) {
	if a.recovering || a.inSnapshot {
		return
	}
	a.recovering = true
	defer func() { a.recovering = false }()

	var lastExpected uint64
	stalls := 0
	for {
		next := a.minPending()
		if next == 0 {
			return
		}
		if next <= a.expected {
			a.drainPending()
			continue
		}
		// Hole: [expected, next-1].
		if a.cfg.Replay != nil {
			dgs, err := a.cfg.Replay.Replay(ctx, a.cfg.ChannelID, a.expected, next-1)
			if err == nil {
				for _, dg := range dgs {
					pkt, derr := DecodePacket(dg)
					if derr != nil || pkt.Seq < a.expected {
						continue
					}
					if a.session != 0 && pkt.SessionID != a.session {
						continue // replay of a stale epoch
					}
					if pkt.Seq > a.expected {
						break // journal hole — escalate via snapshot below
					}
					if !a.deliver(pkt, crc32.ChecksumIEEE(dg), true) {
						break
					}
					a.Stats.ReplayedPackets++
				}
				a.event(EventReplayApplied, FeedA, a.expected, 0)
			} else if errors.Is(err, ErrReplayGapExceeded) {
				a.event(EventReplayGapExceeded, FeedA, a.expected, next-1)
			} else {
				// Transient failure: pending stays buffered; the next
				// arriving packet re-triggers recovery.
				return
			}
		}
		// Replay absent, out of range, or left a hole: escalate to snapshot.
		if a.expected < next {
			if err := a.recoverSnapshot(ctx); err != nil {
				return
			}
		}
		// Stall guard: two consecutive iterations without advancing
		// expected means neither replay nor snapshot can bridge the hole —
		// leave pending buffered and let the next packet re-trigger.
		if a.expected == lastExpected {
			stalls++
			if stalls >= 2 {
				return
			}
		} else {
			stalls = 0
			lastExpected = a.expected
		}
	}
}

// drainPending emits contiguous buffered packets from expected upward.
func (a *Assembler) drainPending() {
	for {
		pkt, ok := a.pending[a.expected]
		if !ok {
			return
		}
		delete(a.pending, a.expected)
		if !a.deliver(pkt, crc32.ChecksumIEEE(pkt.Raw), false) {
			return
		}
	}
}

// recoverSnapshot pulls a snapshot, applies it, then re-runs the ladder for
// any incrementals queued past the new horizon.
func (a *Assembler) recoverSnapshot(ctx context.Context) error {
	if a.cfg.Snapshot == nil {
		return errors.New("sbe: no snapshot source configured")
	}
	if a.inSnapshot {
		// A nested request (e.g. channel-reset arrival while a snapshot is
		// in flight): the outer snapshot converges the stream.
		return errors.New("sbe: snapshot already in flight")
	}
	a.inSnapshot = true
	defer func() { a.inSnapshot = false }()
	snap, err := a.cfg.Snapshot.Snapshot(ctx, a.cfg.ChannelID)
	if err != nil {
		return err
	}
	if err := snap.Validate(); err != nil {
		return err
	}
	a.event(EventSnapshotRecovery, FeedA, snap.LastSeq, a.expected)
	a.applySnapshot(snap)
	a.drainPending()
	// Any hole past the snapshot horizon is left to the caller's recover
	// loop (calling recover() here would no-op under the inSnapshot guard).
	return nil
}

func (a *Assembler) minPending() uint64 {
	var min uint64
	for seq := range a.pending {
		if min == 0 || seq < min {
			min = seq
		}
	}
	return min
}

func (a *Assembler) pruneRecent() {
	if len(a.recent) <= 2*int(a.cfg.DupWindow) {
		return
	}
	var floor uint64
	if a.expected > a.cfg.DupWindow {
		floor = a.expected - a.cfg.DupWindow
	}
	for seq := range a.recent {
		if seq < floor {
			delete(a.recent, seq)
		}
	}
}

// PendingSeqs returns buffered out-of-order seqs (diagnostics).
func (a *Assembler) PendingSeqs() []uint64 {
	out := make([]uint64, 0, len(a.pending))
	for seq := range a.pending {
		out = append(out, seq)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
