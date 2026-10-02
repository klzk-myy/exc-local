// sbefeed.go — SBE multicast market-data producer (IMP-PLAN Phase-3
// Task 4; Phase-06 Task 6.3.6, spec §10.4 — "institutional A/B multicast
// + TCP replay/snapshot recovery").
//
// The engine's aggregated BookDelta stream is top-of-book state, while
// the institutional schema carries incremental level events (spec §10.4
// "no conflation"). SBEFeed diffs consecutive deltas per symbol so a
// level that disappears emits BookActionDelete rather than silently
// vanishing from the reconstructed book — the BookKeeper convergence
// contract (§10.4 AC "converge to the engine book exactly") depends on
// deletes being explicit.
//
// Wiring: one tap channel on the same FanIn/Tee the WS producers ride —
// the shm ring keeps its single reader; this feed is a downstream
// consumer, not a second SPSC endpoint.
package marketdata

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"exchange/internal/sbe"
)

// SBEFeed translates BookDelta → sbe.Message packets onto the dual-feed
// publisher. One instance per process (Publish is not goroutine-safe).
type SBEFeed struct {
	Pub *sbe.Publisher
	// IDs maps canonical symbol → wire instrument_id (the inverse of the
	// EXC_MARKETDATA_INSTRUMENTS resolver). Symbols absent here are
	// counted and skipped — a feed must never guess an id.
	IDs map[string]uint32
	Log *slog.Logger // nil → slog.Default()

	// prev[sym][0]=bids, [1]=asks — price-tick → (qty, count) state used
	// to diff consecutive deltas into update/delete actions.
	prev map[string]*bookSides
	tap  chan BookDelta

	droppedNoID  atomic.Uint64
	levelsUpsert atomic.Uint64
	levelsDelete atomic.Uint64
}

type bookSides struct {
	bids map[int64]Level
	asks map[int64]Level
}

// NewSBEFeed builds the producer bound to pub.
func NewSBEFeed(pub *sbe.Publisher, ids map[string]uint32, log *slog.Logger) *SBEFeed {
	if log == nil {
		log = slog.Default()
	}
	return &SBEFeed{Pub: pub, IDs: ids, Log: log,
		prev: make(map[string]*bookSides)}
}

// Tap returns the BookDelta channel for TeeDeltaSource(up, feed.Tap()).
// Run consumes it until ctx cancels or the source closes.
func (f *SBEFeed) Tap() chan<- BookDelta {
	ch := make(chan BookDelta, 1024)
	f.tap = ch
	return ch
}

// Run is the consumer loop — call in a goroutine. Publishes one packet
// per delta; a heartbeat ticker (HeartbeatInterval, default 1s) keeps
// channel-seq continuity on quiet books per MoldUDP64 semantics.
func (f *SBEFeed) Run(ctx context.Context, heartbeatEvery time.Duration) {
	if heartbeatEvery <= 0 {
		heartbeatEvery = time.Second
	}
	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-f.tap:
			if !ok {
				return
			}
			f.emitDelta(ctx, d)
		case <-hb.C:
			if _, err := f.Pub.Publish(ctx, sbe.Heartbeat{
				EventTimeNs: time.Now().UnixNano()}); err != nil {
				f.Log.Warn("sbe: heartbeat publish failed", "err", err)
			}
		}
	}
}

// emitDelta diffs d against the retained book state and publishes the
// resulting BookUpdate set in one packet (one engine delta = one channel
// seq, preserving per-event atomicity for replay consumers).
func (f *SBEFeed) emitDelta(ctx context.Context, d BookDelta) {
	id, ok := f.IDs[d.Symbol]
	if !ok {
		f.droppedNoID.Add(1)
		return
	}
	st := f.prev[d.Symbol]
	if st == nil {
		st = &bookSides{bids: map[int64]Level{}, asks: map[int64]Level{}}
		f.prev[d.Symbol] = st
	}
	now := d.Ts.UnixNano()
	var msgs []sbe.Message
	msgs, st.bids = diffSide(msgs, id, sbe.SideBid, st.bids, d.Bids, now, f)
	msgs, st.asks = diffSide(msgs, id, sbe.SideAsk, st.asks, d.Asks, now, f)
	if len(msgs) == 0 {
		return // identical delta — nothing new to state
	}
	if _, err := f.Pub.Publish(ctx, msgs...); err != nil {
		f.Log.Error("sbe: book publish failed",
			"symbol", d.Symbol, "err", err)
	}
}

// diffSide emits upserts for new/changed levels and deletes for vanished
// ones, returning the message list and the fresh retained state.
func diffSide(msgs []sbe.Message, inst uint32, side sbe.Side,
	prev map[int64]Level, next []Level, now int64,
	f *SBEFeed) ([]sbe.Message, map[int64]Level) {
	seen := make(map[int64]bool, len(next))
	for _, lv := range next {
		seen[lv.Price] = true
		if p, ok := prev[lv.Price]; !ok || p.Qty != lv.Qty || p.Count != lv.Count {
			msgs = append(msgs, sbe.BookUpdate{
				InstrumentID: inst, Side: side, Action: sbe.BookActionUpdate,
				LevelCount: lv.Count, PriceTicks: lv.Price,
				QtyLots: uint64(lv.Qty), EventTimeNs: now})
			f.levelsUpsert.Add(1)
		}
	}
	for price := range prev {
		if !seen[price] {
			msgs = append(msgs, sbe.BookUpdate{
				InstrumentID: inst, Side: side, Action: sbe.BookActionDelete,
				PriceTicks: price, EventTimeNs: now})
			f.levelsDelete.Add(1)
		}
	}
	fresh := make(map[int64]Level, len(next))
	for _, lv := range next {
		fresh[lv.Price] = lv
	}
	return msgs, fresh
}

// DroppedNoID counts deltas skipped because the symbol had no wire id.
func (f *SBEFeed) DroppedNoID() uint64 { return f.droppedNoID.Load() }
