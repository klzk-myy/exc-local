// Task 6.3.2 — L2 book distribution with conflation (spec §10.1/§10.2).
//
// Pipeline: DeltaSource → per-symbol conflator → Server.Publish fanout.
//
//   - Conflation: book deltas coalesce per symbol; a flush fires on the
//     100ms window OR when 100 deltas have coalesced for the symbol,
//     whichever is first (§10.1).
//   - Sequence: every incoming delta consumes one per-symbol sequence in
//     the md:seq:{symbol} domain (§10.2). Emitted frames carry the
//     §10.9 contiguity envelope — first_seq/last_seq spanning the
//     coalesced delta range and prev_last_seq equal to the prior frame's
//     last_seq — so a dropped input delta becomes a real client-visible
//     gap and triggers resync instead of silently corrupting the book.
//   - Payload: top-20 levels per side (or fewer when the book is thin —
//     §10.1 never pads) + CRC32 depth checksum for the resync-on-mismatch
//     contract (§24 #83).
//   - Fanout: emitted to book@{symbol} and depth@{symbol} (the bare
//     depth@ form is the Task 6.3.15 default 20:100 variant).
//
// SLA instrumentation: emit path latency (delta receipt → subscriber
// enqueue) feeds Metrics for the §24 #99 p99 ≤ 100ms gate.
package marketdata

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"log/slog"
	"strings"
	"sync"
	"time"

	"exchange/pkg/decimal"
)

// Level is one aggregated price level (engine wire values — int64
// quantities of 1e-8 units per spec §5.3, rendered as decimal strings).
type Level struct {
	Price int64
	Qty   int64
	Count uint32
}

// BookDelta is one engine book update for a symbol — the aggregated
// top-of-book state at the event's engine sequence. Transport-agnostic:
// IPC ring, Aeron and JetStream adapters all produce this shape.
type BookDelta struct {
	Symbol    string
	Bids      []Level // best-first (highest first)
	Asks      []Level // best-first (lowest first)
	EngineSeq uint64  // engine event seq — diagnostics, not the stream seq
	Ts        time.Time
}

// DeltaSource feeds engine book deltas into the conflator. The Phase-06
// production source is the engine's {base}_{shard}_out IPC ring
// (FlatBuffers BookSnapshot events — see IPCDeltaSource); the JetStream
// fallback consumes the bridge's republished stream
// (NewJetStreamDeltaSource); tests drive the conflator with a channel
// stub. Implementations MUST NOT drop deltas silently — a dropped delta
// is a client-visible seq gap downstream.
type DeltaSource interface {
	// Deltas streams book deltas until ctx is cancelled or the source
	// exhausts. The channel is closed on termination.
	Deltas(ctx context.Context) (<-chan BookDelta, error)
}

// ---------------------------------------------------------------------------
// Depth update payload
// ---------------------------------------------------------------------------

// depthUpdate is the emitted L2 frame payload (§10.9 envelope + §24 #83
// checksum). Levels render as [price, qty, count] triples — price/qty as
// decimal strings (1e-8 wire scale), count as the aggregated order count.
type depthUpdate struct {
	Event       string     `json:"event"` // "depthUpdate"
	Symbol      string     `json:"symbol"`
	Bids        [][]string `json:"bids"`
	Asks        [][]string `json:"asks"`
	FirstSeq    uint64     `json:"first_seq"`
	LastSeq     uint64     `json:"last_seq"`
	PrevLastSeq uint64     `json:"prev_last_seq"`
	Seq         uint64     `json:"seq"` // == last_seq (envelope mirror)
	EngineSeq   uint64     `json:"engine_seq"`
	Coalesced   int        `json:"coalesced"` // deltas folded into this frame
	CRC32       uint32     `json:"crc32"`
	TsMs        int64      `json:"ts_ms"`
	Snapshot    bool       `json:"is_snapshot,omitempty"`
	// Task 6.3.15 echo: present only on parameterized
	// depth@{sym}:{levels}:{cadence} frames so a client can verify it
	// received the shape it subscribed to.
	Levels    int `json:"levels,omitempty"`
	CadenceMs int `json:"cadence_ms,omitempty"`
}

// renderLevels converts engine wire levels to [price, qty, count]
// triples capped at depth.
func renderLevels(lv []Level, depth int) [][]string {
	if depth > 0 && len(lv) > depth {
		lv = lv[:depth]
	}
	out := make([][]string, 0, len(lv))
	for _, l := range lv {
		out = append(out, []string{
			decimal.NewFromScaled(l.Price).String(),
			decimal.NewFromScaled(l.Qty).String(),
			fmt.Sprintf("%d", l.Count),
		})
	}
	return out
}

// depthChecksum computes the §24 #83 CRC32 over the canonical level
// encoding: per level, 8-byte big-endian price, 8-byte qty, 4-byte
// count, bids then asks in emitted order. Clients recompute over the
// received arrays; a mismatch means book corruption → resync.
func depthChecksum(bids, asks []Level, depth int) uint32 {
	h := crc32.NewIEEE()
	var buf [20]byte
	writeSide := func(lv []Level) {
		if depth > 0 && len(lv) > depth {
			lv = lv[:depth]
		}
		for _, l := range lv {
			binary.BigEndian.PutUint64(buf[0:8], uint64(l.Price))
			binary.BigEndian.PutUint64(buf[8:16], uint64(l.Qty))
			binary.BigEndian.PutUint32(buf[16:20], l.Count)
			_, _ = h.Write(buf[:])
		}
	}
	writeSide(bids)
	writeSide(asks)
	return h.Sum32()
}

// ---------------------------------------------------------------------------
// Conflator
// ---------------------------------------------------------------------------

// ConflatorConfig tunes Conflator. Zero values pick spec defaults.
type ConflatorConfig struct {
	Window      time.Duration // default 100ms (§10.1)
	MaxEvents   int           // default 100 coalesced deltas (§10.1)
	Depth       int           // default 20 levels per side
	InputBuffer int           // default 8192 inbound deltas
	Logger      *slog.Logger
	Now         func() time.Time
	// VariantSource returns the live depth@{symbol}:{levels}:{cadence}
	// parameterizations per symbol (Task 6.3.15) — production wiring
	// passes Server.ActiveDepthVariants. Nil ⇒ only the bare
	// depth@{symbol} (20:100) channel emits, plus book@{symbol}.
	VariantSource func() map[string][]DepthVariant
	// VariantSweep is the cadence-evaluation interval for parameterized
	// depth channels — default 10ms. Cadences (100/250/1000ms) emit on
	// the first sweep at-or-past their interval carrying fresh state.
	VariantSweep time.Duration
	// Journal is the durable gap log (Task 6.3.22, spec §10.7): input
	// drops and restart boundaries are journaled per symbol. Nil
	// disables journaling — the seq cursor contract is unaffected.
	Journal GapJournal
}

func (c *ConflatorConfig) defaults() {
	if c.Window <= 0 {
		c.Window = 100 * time.Millisecond
	}
	if c.MaxEvents <= 0 {
		c.MaxEvents = 100
	}
	if c.Depth <= 0 {
		c.Depth = 20
	}
	if c.InputBuffer <= 0 {
		c.InputBuffer = 8192
	}
	if c.VariantSweep <= 0 {
		c.VariantSweep = 10 * time.Millisecond
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// EmitFunc is the fanout seam — Server.Publish satisfies it.
type EmitFunc func(channel string, seq uint64, data any)

// pendingBook is one symbol's coalescing state between flushes.
type pendingBook struct {
	latest   BookDelta // newest delta wins — true conflation
	firstSeq uint64
	lastSeq  uint64
	count    int
}

// Conflator batches engine deltas per symbol into 100ms/100-event L2
// frames. Run drives the loop; Push is the direct-injection seam for
// tests and embedders.
type Conflator struct {
	cfg  ConflatorConfig
	src  DeltaSource
	seq  SeqStore // md:seq mirror; nil = in-memory only
	emit EmitFunc
	m    *Metrics

	in chan BookDelta

	mu          sync.Mutex
	pending     map[string]*pendingBook
	cursors     map[string]uint64 // last allocated seq per symbol
	seeded      map[string]bool   // cursor loaded from SeqStore
	lastEmitted map[string]uint64 // prev_last_seq per symbol
	latest      map[string]BookDelta
	lastEmitSeq map[string]uint64
	// variantTrack tracks each active depth-variant channel's last emit:
	// prev_last_seq chaining is PER CHANNEL (§10.9 — the client verifies
	// msg.prev_last_seq == prior frame's last_seq on ITS channel), so a
	// 1000ms-cadence variant chains to its own prior emit, not the
	// master's.
	variantTrack map[string]map[DepthVariant]*variantCursor
}

// variantCursor is one depth-variant channel's emit bookkeeping.
type variantCursor struct {
	lastSeq uint64    // last_seq of the channel's previous frame
	at      time.Time // when that frame emitted (cadence gate)
}

// NewConflator wires the pipeline. src may be nil (drive via Push);
// emit must not be nil; seq may be nil for in-memory-only cursors.
func NewConflator(cfg ConflatorConfig, src DeltaSource, seq SeqStore, emit EmitFunc, m *Metrics) *Conflator {
	cfg.defaults()
	if m == nil {
		m = NewMetrics()
	}
	return &Conflator{
		cfg:  cfg,
		src:  src,
		seq:  seq,
		emit: emit,
		m:    m,
		in:   make(chan BookDelta, cfg.InputBuffer),

		pending:      map[string]*pendingBook{},
		cursors:      map[string]uint64{},
		seeded:       map[string]bool{},
		lastEmitted:  map[string]uint64{},
		latest:       map[string]BookDelta{},
		lastEmitSeq:  map[string]uint64{},
		variantTrack: map[string]map[DepthVariant]*variantCursor{},
	}
}

// Metrics exposes the conflator's counters (shared with the server's
// when both receive the same *Metrics).
func (c *Conflator) Metrics() *Metrics { return c.m }

// Push injects a delta directly — the test/embedding seam alongside
// DeltaSource-driven Run.
func (c *Conflator) Push(d BookDelta) {
	select {
	case c.in <- d:
	default:
		// Input saturation: the delta is dropped AND a seq is burned for
		// the symbol so the next emitted frame shows a first_seq gap —
		// clients resync instead of silently missing the update
		// (fail-loud per §2.7). The burned seq lands in the gap journal
		// so the discontinuity survives restarts (Task 6.3.22).
		c.m.DeltasDropped.Add(1)
		c.cfg.Logger.Error("marketdata: conflator input saturated — delta dropped",
			"symbol", d.Symbol, "engine_seq", d.EngineSeq)
		c.mu.Lock()
		burned := c.allocSeqLocked(d.Symbol)
		c.mu.Unlock()
		noteGap(c.cfg.Journal, c.cfg.Logger, d.Symbol, SeqGap{
			From: burned, To: burned, Reason: GapInputSaturation,
		})
	}
}

// allocSeqLocked bumps the symbol cursor, seeding from the SeqStore on
// first touch. Seed failures degrade to a local cursor — loud in
// logs/metrics; the emitted seq envelope still makes any regression
// client-visible.
//
// Caller holds c.mu.
func (c *Conflator) allocSeqLocked(symbol string) uint64 {
	if !c.seeded[symbol] {
		c.seeded[symbol] = true
		if c.seq != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			v, err := c.seq.Load(ctx, symbol)
			cancel()
			if err != nil {
				c.m.SeqMirrorErrors.Add(1)
				c.cfg.Logger.Error("marketdata: seq seed failed — local cursor from 0",
					"symbol", symbol, "err", err)
				noteGap(c.cfg.Journal, c.cfg.Logger, symbol, SeqGap{
					From: 0, To: 0, Reason: GapSeedFailed,
				})
			} else {
				c.cursors[symbol] = v
				c.lastEmitted[symbol] = v
				if v > 0 {
					// Task 6.3.22 item 1: the cursor continued across a
					// restart — journal the continuity boundary so ops
					// can distinguish "no events yet" from "survived".
					noteGap(c.cfg.Journal, c.cfg.Logger, symbol, SeqGap{
						From: v, To: v, Reason: GapRestartBoundary,
					})
				}
			}
		}
	}
	c.cursors[symbol]++
	return c.cursors[symbol]
}

// absorb merges one delta into the pending window.
// Caller holds c.mu.
func (c *Conflator) absorbLocked(d BookDelta) {
	seq := c.allocSeqLocked(d.Symbol)
	p := c.pending[d.Symbol]
	if p == nil {
		p = &pendingBook{firstSeq: seq}
		c.pending[d.Symbol] = p
	}
	p.latest = d
	p.lastSeq = seq
	p.count++
	c.latest[d.Symbol] = d
	c.m.DeltasReceived.Add(1)
}

// flushSymbol emits one symbol's pending batch. Returns false when the
// symbol had nothing pending.
// Caller holds c.mu.
func (c *Conflator) flushSymbolLocked(symbol string, byCount bool) bool {
	p := c.pending[symbol]
	if p == nil {
		return false
	}
	delete(c.pending, symbol)

	prev := c.lastEmitted[symbol]
	upd := depthUpdate{
		Event:       "depthUpdate",
		Symbol:      symbol,
		Bids:        renderLevels(p.latest.Bids, c.cfg.Depth),
		Asks:        renderLevels(p.latest.Asks, c.cfg.Depth),
		FirstSeq:    p.firstSeq,
		LastSeq:     p.lastSeq,
		PrevLastSeq: prev,
		Seq:         p.lastSeq,
		EngineSeq:   p.latest.EngineSeq,
		Coalesced:   p.count,
		CRC32:       depthChecksum(p.latest.Bids, p.latest.Asks, c.cfg.Depth),
		TsMs:        c.cfg.Now().UnixMilli(),
	}
	c.lastEmitted[symbol] = p.lastSeq
	c.lastEmitSeq[symbol] = p.lastSeq

	if byCount {
		c.m.FlushesByCount.Add(1)
	} else {
		c.m.FlushesByWindow.Add(1)
	}
	c.m.ConflationEmits.Add(1)

	// Emit outside the lock would reorder frames — emit inline; the
	// fanout itself never blocks (Publish enqueues per-conn).
	c.emit("book@"+symbol, p.lastSeq, upd)
	// Bare depth@SYMBOL keeps its §24 #265 default meaning (20:100) —
	// the full master frame. Parameterized depth@{sym}:{lvl}:{cad}
	// variants ride emitVariantLocked on their own cadence.
	c.emit("depth@"+symbol, p.lastSeq, upd)

	// Mirror the cursor to md:seq:{symbol} — fire-and-forget with
	// bounded ctx; mirror failures are counted, never fatal.
	if c.seq != nil {
		seq := p.lastSeq
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := c.seq.Store(ctx, symbol, seq); err != nil {
				c.m.SeqMirrorErrors.Add(1)
				c.cfg.Logger.Warn("marketdata: seq mirror failed",
					"symbol", symbol, "seq", seq, "err", err)
			}
		}()
	}
	return true
}

// flushAllLocked emits every symbol's pending batch (window flush).
// Caller holds c.mu.
func (c *Conflator) flushAllLocked() {
	for sym := range c.pending {
		c.flushSymbolLocked(sym, false)
	}
}

// Run drives the conflation loop until ctx is cancelled: drain source
// deltas, flush per-symbol when MaxEvents coalesced, flush everything on
// the Window ticker.
func (c *Conflator) Run(ctx context.Context) error {
	var srcCh <-chan BookDelta
	if c.src != nil {
		ch, err := c.src.Deltas(ctx)
		if err != nil {
			return fmt.Errorf("marketdata: delta source: %w", err)
		}
		srcCh = ch
	}
	tick := time.NewTicker(c.cfg.Window)
	defer tick.Stop()
	var sweep *time.Ticker
	var sweepC <-chan time.Time
	if c.cfg.VariantSource != nil {
		sweep = time.NewTicker(c.cfg.VariantSweep)
		sweepC = sweep.C
		defer sweep.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.flushAllLocked()
			c.mu.Unlock()
			return ctx.Err()
		case d, ok := <-srcCh:
			if !ok {
				srcCh = nil
				continue
			}
			c.absorbAndMaybeFlush(d)
		case d := <-c.in:
			c.absorbAndMaybeFlush(d)
		case <-tick.C:
			c.mu.Lock()
			c.flushAllLocked()
			c.mu.Unlock()
		case <-sweepC:
			c.mu.Lock()
			c.sweepVariantsLocked()
			c.mu.Unlock()
		}
	}
}

func (c *Conflator) absorbAndMaybeFlush(d BookDelta) {
	c.mu.Lock()
	c.absorbLocked(d)
	over := c.pending[d.Symbol].count >= c.cfg.MaxEvents
	c.mu.Unlock()
	if over {
		c.mu.Lock()
		c.flushSymbolLocked(d.Symbol, true)
		c.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Task 6.3.15 — parameterized depth@ multiplexing (spec §24 #265).
//
// "Server multiplexes from internal 20-level snapshots": the master
// pipeline keeps the full §10.1 conflation contract; each subscribed
// variant (levels × cadence) emits the same latest-state payload sliced
// to its level count on its own cadence. Variants emit ONLY when the
// symbol cursor advanced since the variant's last emit (no-op ticks
// never fabricate frames) and never more often than the cadence.
// ---------------------------------------------------------------------------

// sweepVariantsLocked emits due depth-variant frames. Runs on the
// VariantSweep ticker (10ms default) so 100/250/1000ms cadences resolve
// to their own boundaries instead of snapping to the 100ms window.
// Caller holds c.mu.
func (c *Conflator) sweepVariantsLocked() {
	vars := c.cfg.VariantSource()
	if len(vars) == 0 {
		return
	}
	now := c.cfg.Now()
	for symbol, list := range vars {
		cur := c.cursors[symbol]
		if cur == 0 {
			continue // no deltas ever — nothing to emit
		}
		latest, ok := c.latest[symbol]
		if !ok {
			continue
		}
		track := c.variantTrack[symbol]
		if track == nil {
			track = map[DepthVariant]*variantCursor{}
			c.variantTrack[symbol] = track
		}
		for _, v := range list {
			vc := track[v]
			if vc == nil {
				vc = &variantCursor{}
				track[v] = vc
			}
			if cur <= vc.lastSeq {
				continue // no fresh state for this variant
			}
			if !vc.at.IsZero() &&
				now.Sub(vc.at) < time.Duration(v.CadenceMs)*time.Millisecond {
				continue // cadence gate — emit no more often than requested
			}
			c.emitVariantLocked(symbol, latest, v, vc, cur, now)
		}
	}
}

// emitVariantLocked emits one depth-variant frame on
// depth@{symbol}:{levels}:{cadence}: the latest 20-level state sliced to
// the variant's level count, with the variant channel's own
// first/prev/last seq chain and a CRC32 over the EMITTED slice (the
// checksum must verify what the client received — §24 #83).
// Caller holds c.mu.
func (c *Conflator) emitVariantLocked(symbol string, d BookDelta,
	v DepthVariant, vc *variantCursor, cursor uint64, now time.Time) {
	upd := depthUpdate{
		Event:       "depthUpdate",
		Symbol:      symbol,
		Bids:        renderLevels(d.Bids, v.Levels),
		Asks:        renderLevels(d.Asks, v.Levels),
		FirstSeq:    vc.lastSeq + 1,
		LastSeq:     cursor,
		PrevLastSeq: vc.lastSeq,
		Seq:         cursor,
		EngineSeq:   d.EngineSeq,
		CRC32:       depthChecksum(d.Bids, d.Asks, v.Levels),
		TsMs:        now.UnixMilli(),
		Levels:      v.Levels,
		CadenceMs:   v.CadenceMs,
	}
	vc.lastSeq = cursor
	vc.at = now
	c.m.DepthVariantEmits.Add(1)
	c.emit(fmt.Sprintf("depth@%s:%d:%d", symbol, v.Levels, v.CadenceMs),
		cursor, upd)
}

// ---------------------------------------------------------------------------
// SnapshotSource — full-state fallback for book@/depth@ resume resyncs.
// ---------------------------------------------------------------------------

// Snapshot implements SnapshotSource for "book@{symbol}",
// "depth@{symbol}", and the parameterized "depth@{symbol}:{levels}:
// {cadence}" form (Task 6.3.15): a resynced depth:5 client receives a
// 5-level snapshot, not a top-20 frame — and the CRC covers exactly the
// emitted slice. Unsupported params fail loud (INVALID_REQUEST at the
// wire layer); an absent book reports no-snapshot rather than a
// fabricated empty one.
func (c *Conflator) Snapshot(_ context.Context, channel string) (uint64, any, error) {
	at := strings.IndexByte(channel, '@')
	if at <= 0 {
		return 0, nil, fmt.Errorf("marketdata: malformed channel %q", channel)
	}
	typ, rest := channel[:at], channel[at+1:]
	levels := c.cfg.Depth
	var variant *DepthVariant
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		params := rest[i+1:]
		rest = rest[:i]
		if typ != "depth" {
			return 0, nil, fmt.Errorf("marketdata: no snapshot source for %q", typ)
		}
		v, err := parseDepthParams(params)
		if err != nil {
			return 0, nil, err
		}
		levels = v.Levels
		variant = &v
	}
	if typ != "book" && typ != "depth" {
		return 0, nil, fmt.Errorf("marketdata: no snapshot source for %q", typ)
	}
	symbol := rest

	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.latest[symbol]
	if !ok {
		return 0, nil, fmt.Errorf("marketdata: no book state for %q", symbol)
	}
	seq := c.cursors[symbol]
	upd := depthUpdate{
		Event:       "depthUpdate",
		Symbol:      symbol,
		Bids:        renderLevels(d.Bids, levels),
		Asks:        renderLevels(d.Asks, levels),
		FirstSeq:    seq,
		LastSeq:     seq,
		PrevLastSeq: c.lastEmitted[symbol],
		Seq:         seq,
		EngineSeq:   d.EngineSeq,
		CRC32:       depthChecksum(d.Bids, d.Asks, levels),
		TsMs:        c.cfg.Now().UnixMilli(),
		Snapshot:    true,
	}
	if variant != nil {
		upd.Levels = variant.Levels
		upd.CadenceMs = variant.CadenceMs
	}
	return seq, upd, nil
}
