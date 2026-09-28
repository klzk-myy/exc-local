// Task 6.3.9 — WS session resume & sequence replay (spec §10.2, §10.7).
// Task 6.3.24 — L2/L3 resynchronization contract & gap detection
// (spec §10.9, §24 #408).
//
// Every published channel event carries a monotonic channel-scoped seq
// and is appended to the channel's ring buffer (60s / 10,000-message
// horizon, whichever is smaller — private channels keep one ring PER
// ACCOUNT per Task 6.3.9 item 7). A reconnecting client sends
//
//	{"action":"resume","channel":"book@EUR/USD","last_seq":12345}
//
// and a mid-stream client repairing a detected gap (prev_last_seq
// discontinuity or CRC32 mismatch) sends
//
//	{"action":"resync","channel":"depth@EUR/USD:5:100","last_seq":12345}
//	{"action":"resync","symbol":"EUR/USD","last_seq":12345}
//
// — the §10.9 item-2 resynchronization contract. Both paths share the
// same verdict logic: replay the buffered frames strictly after last_seq
// (verbatim, original seq/ts_ms) then continue live, or — when the
// cursor is outside the horizon — emit an explicit resync directive plus
// a full snapshot frame when the channel type has a SnapshotSource wired
// (Task 6.3.9 item 5; §10.7's "explicit resync frame" requirement).
//
// Sequence cursors are channel-scoped and durable for L2 (the
// md:seq:{symbol} Redis mirror survives restarts — Task 6.3.22: seq
// never resets to 0); an in-memory ring still empties on restart, so a
// client whose seq predates the buffer always receives resync+snapshot —
// never a silent gap (fail-closed client state synchronization,
// remediation #38). Every resync verdict lands in the gap journal
// (Task 6.3.22 bounded gap log).
package marketdata

import (
	"context"
	"fmt"

	"exchange/internal/ws"
)

// SnapshotSource supplies the full-state fallback for a channel type
// when a resume/resync cursor falls outside the replay horizon (and for
// last_seq=0 cold starts). Producers register one per channel type via
// Server.SetSnapshotSource; the L2 conflator provides book@/depth@, the
// reference-price stream provides referencePrice@.
type SnapshotSource interface {
	// Snapshot returns the authoritative channel state. channel is the
	// verbatim subscription token ("book@EUR/USD", "depth@EUR/USD:5:100").
	// Seq is the snapshot's sequence in the channel domain — deltas with
	// seq <= Seq must be discarded by the client (§10.9 application rule).
	Snapshot(ctx context.Context, channel string) (seq uint64, data any, err error)
}

// handleResume answers {"action":"resume","channel":..,"last_seq":..}.
// The resume implies an idempotent (re)subscription — after replay or
// resync the conn is bound to the channel so streaming continues live.
func (c *Conn) handleResume(f clientFrame) {
	if f.Channel == "" {
		c.sendError(f.rid(), "resume", "INVALID_REQUEST",
			"resume requires a channel", 0)
		return
	}
	ch, err := ParseChannel(f.Channel)
	if err != nil {
		c.sendError(f.rid(), "resume", channelCode(err), err.Error(), 0)
		return
	}
	sess := c.session()
	if code, reason := c.gateChannel(sess, ch); code != "" {
		c.sendError(f.rid(), "resume", code, reason, 0)
		return
	}

	// Resume binds the subscription (idempotent — class budgets apply).
	// If the quota rejects the bind, surface the error and skip replay:
	// resuming a channel we will not stream leaves the client half-synced.
	if err := c.ensureSubscribed(f.rid(), ch); err != nil {
		return // error frame already emitted
	}

	res := c.srv.replayFor(&sess, ch, f.LastSeq)
	c.applyReplayVerdict(ch, res, f.LastSeq)
}

// ---------------------------------------------------------------------------
// Task 6.3.24 — {"action":"resync"} mid-stream gap repair (spec §10.9).
// ---------------------------------------------------------------------------

// handleResync implements the client-initiated resynchronization
// contract: a client that detects a prev_last_seq discontinuity or a
// CRC32 checksum mismatch sends {"action":"resync","symbol":"..",
// "last_seq":N} (or the verbatim "channel" form) and receives either the
// missed buffered deltas or a resync directive + full snapshot.
//
// Channel resolution order:
//  1. explicit "channel" field (verbatim subscription token);
//  2. "symbol" → the conn's existing L2/L3-class binding for that
//     symbol (replays the exact ring the client is consuming);
//  3. "symbol" with no prior binding → depth@{symbol} (the canonical
//     L2 channel for §10.9's L2/L3 contract).
func (c *Conn) handleResync(f clientFrame) {
	raw := f.Channel
	if raw == "" && f.Symbol != "" {
		raw = c.resolveSyncChannel(f.Symbol)
	}
	if raw == "" {
		c.sendError(f.rid(), "resync", "INVALID_REQUEST",
			"resync requires a channel or symbol", 0)
		return
	}
	ch, err := ParseChannel(raw)
	if err != nil {
		c.sendError(f.rid(), "resync", channelCode(err), err.Error(), 0)
		return
	}
	sess := c.session()
	if code, reason := c.gateChannel(sess, ch); code != "" {
		c.sendError(f.rid(), "resync", code, reason, 0)
		return
	}
	if err := c.ensureSubscribed(f.rid(), ch); err != nil {
		return // error frame already emitted
	}
	res := c.srv.replayFor(&sess, ch, f.LastSeq)
	c.applyReplayVerdict(ch, res, f.LastSeq)
}

// resolveSyncChannel maps a bare symbol to the conn's existing L2/L3
// subscription for it (the ring the client is actually consuming), or
// the canonical depth@{symbol} channel when no binding exists.
func (c *Conn) resolveSyncChannel(symbol string) string {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	var depth, other string
	for _, sub := range c.subs {
		if sub.ch.Target != symbol || sub.ch.Private {
			continue
		}
		switch sub.ch.Type {
		case "depth":
			depth = sub.ch.Raw // verbatim — replays the param-variant ring
		case "book", "l3", "l3Book":
			if other == "" {
				other = sub.ch.Raw
			}
		}
	}
	if depth != "" {
		return depth
	}
	if other != "" {
		return other
	}
	return "depth@" + symbol
}

// replayFor evaluates the resume cursor against the channel's replay
// buffer under the hub lock — a consistent cut vs concurrent Publish.
// Private channels replay the caller's PER-ACCOUNT ring (Task 6.3.9
// item 7): another account's frames are never observable here.
func (s *Server) replayFor(sess *ws.Session, ch Channel, lastSeq uint64) ReplayResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.channels[ch.Raw]
	if cs == nil {
		return ReplayResult{Verdict: ReplayEmpty}
	}
	if ch.Private {
		if rb := cs.priv[sess.AccountID]; rb != nil {
			return rb.replay(lastSeq)
		}
		return ReplayResult{Verdict: ReplayEmpty}
	}
	return cs.ring.replay(lastSeq)
}

// applyReplayVerdict emits the replay-or-resync decision for one resume
// or resync request: verbatim frame replay + "resumed" confirmation, or
// the explicit resync directive + snapshot fallback. Resync verdicts are
// journaled (Task 6.3.22 bounded gap log).
func (c *Conn) applyReplayVerdict(ch Channel,
	res ReplayResult, lastSeq uint64) {
	now := c.srv.cfg.Now().UnixMilli()
	switch res.Verdict {
	case ReplayOK, ReplayUpToDate:
		// Task 6.3.2 item 5: a replay deeper than 1000 frames takes the
		// snapshot path instead of flooding the reconnecting client.
		if len(res.Msgs) > maxReplayFrames {
			c.journalGap(ch, SeqGap{
				From: lastSeq + 1, To: res.Tail, Reason: GapResumeHorizon,
			})
			c.emitResync(ch, "gap_too_large", lastSeq)
			return
		}
		for _, m := range res.Msgs {
			c.enqueue(m) // verbatim replay — original seq/ts_ms preserved
		}
		b, _ := marshalFrame(resumedFrame{
			Type: "resumed", Channel: ch.Raw,
			FromSeq: lastSeq + 1, ToSeq: res.Tail,
			Count: len(res.Msgs), TsMs: now,
		})
		c.enqueue(b)

	case ReplayEmpty:
		// No frames buffered yet. A zero cursor is the cold-start form
		// (Task 6.3.9 edge case: seq=0 → full snapshot); a non-zero
		// cursor against an empty buffer is unverifiable → resync.
		reason := "gap_too_large"
		if lastSeq != 0 {
			reason = "invalid_sequence"
			c.journalGap(ch, SeqGap{
				From: lastSeq, To: lastSeq, Reason: GapResumeInvalid,
			})
		}
		c.emitResync(ch, reason, lastSeq)

	case ReplayGapTooLarge:
		c.journalGap(ch, SeqGap{
			From: lastSeq + 1, To: res.Tail, Reason: GapResumeHorizon,
		})
		c.emitResync(ch, "gap_too_large", lastSeq)

	case ReplayInvalidSeq:
		c.journalGap(ch, SeqGap{
			From: lastSeq, To: res.Tail, Reason: GapResumeInvalid,
		})
		c.emitResync(ch, "invalid_sequence", lastSeq)
	}
}

// journalGap writes a resume/resync discontinuity to the durable gap
// journal (Task 6.3.22). The key is the channel's seq domain: the symbol
// for public channels, private:{channel}:{account} for private streams.
func (c *Conn) journalGap(ch Channel, g SeqGap) {
	key := ch.Raw
	if ch.Private {
		key = fmt.Sprintf("%s:%d", ch.Raw, c.session().AccountID)
	}
	noteGap(c.srv.cfg.Journal, c.srv.cfg.Logger, key, g)
}

// ensureSubscribed binds the conn to the channel honoring the same
// gates as subscribe (class budget, global cap). Already-bound channels
// are a no-op. Returns nil on success; on rejection the error frame has
// been emitted and the caller abandons the resume.
func (c *Conn) ensureSubscribed(requestID string, ch Channel) error {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if _, ok := c.subs[ch.Raw]; ok {
		return nil
	}
	if ch.Class == ClassL2 && c.l2n >= c.srv.cfg.MaxL2Subscriptions {
		c.sendError(requestID, "resume", "WS_MAX_SUBSCRIPTIONS_EXCEEDED",
			"L2 subscription budget (20 channels) exceeded", 0)
		return errResumeRejected
	}
	if ch.Class == ClassL3 && c.l3n >= c.srv.cfg.MaxL3Subscriptions {
		c.sendError(requestID, "resume", "WS_MAX_SUBSCRIPTIONS_EXCEEDED",
			"L3 subscription budget (5 channels) exceeded", 0)
		return errResumeRejected
	}
	if len(c.subs) >= c.srv.cfg.MaxSubscriptions {
		c.sendError(requestID, "resume", "WS_MAX_SUBSCRIPTIONS_EXCEEDED",
			"subscription limit reached", 0)
		return errResumeRejected
	}
	c.subs[ch.Raw] = &subscription{ch: ch, class: ch.Class}
	switch ch.Class {
	case ClassL2:
		c.l2n++
	case ClassL3:
		c.l3n++
	}
	c.srv.subscribe(c, ch)
	return nil
}

var errResumeRejected = fmt.Errorf("marketdata: resume subscription rejected")

// maxReplayFrames is the Task 6.3.2 item 5 replay bound: a cursor more
// than 1000 frames behind the tail takes the snapshot path even when
// every frame is still buffered — replay floods are slower for the
// client than one resync+snapshot.
const maxReplayFrames = 1000

// emitResync sends the explicit resync directive (§10.7, §10.9 item 2)
// followed by a full snapshot frame when the channel type has a
// SnapshotSource wired (Task 6.3.9 item 5). The snapshot's seq is the
// client's new last_update_id: buffered deltas with last_seq <= seq are
// discarded, the first frame with prev_last_seq == seq resumes the chain
// (zero crossed books after reconnect per §24 #408). A source that
// errors or is absent leaves the resync frame alone — fail-closed, the
// client must refetch state via REST (GET /api/v1/book/{symbol}?depth=20
// for L2 per §10.3).
func (c *Conn) emitResync(ch Channel, reason string, lastSeq uint64) {
	now := c.srv.cfg.Now().UnixMilli()
	b, _ := marshalFrame(resyncFrame{
		Type: "resync", Channel: ch.Raw, Reason: reason,
		LastSeq: lastSeq, TsMs: now,
	})
	c.enqueue(b)
	c.srv.metrics.ResyncDirectives.Add(1)

	src := c.srv.snapshotSource(ch.Type)
	if src == nil {
		return
	}
	seq, data, err := src.Snapshot(context.Background(), ch.Raw)
	if err != nil {
		c.srv.cfg.Logger.Warn("marketdata: snapshot source failed",
			"channel", ch.Raw, "err", err)
		return
	}
	b, _ = marshalFrame(snapshotFrame{
		Type: "snapshot", Channel: ch.Raw, Reason: reason,
		Seq: seq, Data: data, TsMs: now,
	})
	c.enqueue(b)
}
