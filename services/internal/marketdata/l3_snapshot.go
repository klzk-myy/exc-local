// Phase-17 Task 17.3.2 — WAL-based L3 snapshot reader.
//
// Contract (spec §11.1 + task text):
//  1. Take a WAL position marker at request time (the prescan tail).
//  2. Rebuild the order-level book AS OF the marker: locate the newest
//     BOOK_SNAPSHOT entry for the instrument at/before the marker, then
//     replay ORDER_NEW/ORDER_MODIFY/ORDER_CANCEL/TRADE entries forward
//     to the marker (entries past the marker are ignored — the cut is
//     point-in-time).
//  3. Reconstruction runs on a dedicated reader (this object): it reads
//     sealed/append-only WAL segment FILES — it never touches engine
//     state and never blocks the matching loop.
//  4. Staleness bound 500 ms: if the marker-pinned rebuild cannot be
//     served within the budget the request fails (ErrL3SnapshotStale).
//  5. Books above 100,000 resting orders refuse with
//     ErrL3SnapshotTooLarge (→ HTTP 413 L3_SNAPSHOT_TOO_LARGE).
//  6. Cursor pagination: orders sorted by order_id; cursor is the last
//     order_id of the previous page (strictly-greater resume).
//
// account_hash provenance: WAL ORDER_NEW rows carry the real account_id.
// The L3 stream's account_hash convention is the engine's salted
// FNV-1a-64 ("exc.l3.account.v1" || account_id LE) — the SINGLE Go-side
// hash seam (L3AccountHash), matching exch::L3Publisher::hash_account in
// core/src/ipc/L3Publisher.cpp so snapshot and stream pseudonyms agree.
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"time"

	"exchange/internal/recovery"
	"exchange/pkg/decimal"
)

// Spec-pinned snapshot bounds.
const (
	// L3SnapshotMaxOrders — books above 100,000 resting orders refuse
	// (§23 L3_SNAPSHOT_TOO_LARGE).
	L3SnapshotMaxOrders = 100_000
	// L3SnapshotMaxStale — marker-to-serve latency bound 500 ms.
	L3SnapshotMaxStale = 500 * time.Millisecond
	// l3FlagHiddenBit mirrors core's kOrderFlagHidden (bit 4 of the WAL
	// ORDER_NEW flags byte) — the WAL-side hidden marker.
	l3FlagHiddenBit = 1 << 4
)

// l3AccountHashSalt is the wire-contract salt literal — byte-identical
// to exch::L3Publisher::kAccountHashSalt ("exc.l3.account.v1"), which the
// .fbs schema comment pins so old/new consumers agree on pseudonyms.
const l3AccountHashSalt = "exc.l3.account.v1"

// L3AccountHash is the account → account_hash mapping for WAL-derived
// rows: salted FNV-1a-64 over salt || account_id (little-endian 8B) —
// identical output to the engine's L3Publisher::hash_account, so the
// REST snapshot and the live stream present the same pseudonym for the
// same account.
func L3AccountHash(accountID uint64) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(l3AccountHashSalt))
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(accountID >> (8 * i))
	}
	_, _ = h.Write(b[:])
	return h.Sum64()
}

var (
	// ErrL3SnapshotTooLarge → HTTP 413 L3_SNAPSHOT_TOO_LARGE.
	ErrL3SnapshotTooLarge = errors.New(
		"marketdata: l3 snapshot exceeds the 100,000 resting-order ceiling")
	// ErrL3SnapshotStale — the marker-pinned rebuild missed the 500 ms
	// serve budget (or the WAL tail could not be observed at all).
	ErrL3SnapshotStale = errors.New(
		"marketdata: l3 snapshot could not be served inside the 500ms staleness budget")
	// ErrL3UnknownSymbol — no instrument/shard/WAL resolution for the symbol.
	ErrL3UnknownSymbol = errors.New("marketdata: unknown symbol")
	// ErrL3WALUnavailable — no readable WAL stream for the shard.
	ErrL3WALUnavailable = errors.New("marketdata: wal stream unavailable for symbol shard")
)

// L3SnapshotDeps are the resolution seams the WAL reader needs. All are
// injected by the wiring layer so the reader stays transport-agnostic.
type L3SnapshotDeps struct {
	// WalDirs returns the WAL segment director(ies) owning the symbol —
	// normally exactly one shard dir (reconciliationWalDirs resolution).
	WalDirs func(symbol string) []string
	// InstrumentID resolves the canonical symbol to the engine's
	// instrument_id (WAL rows are instrument-keyed, not symbol-keyed).
	InstrumentID func(symbol string) (uint32, bool)
	// Shard resolves the symbol to its engine shard id — the prescan's
	// wantShard (foreign-shard segments are corruption, not data).
	Shard func(symbol string) (int, bool)
	// AccountHash maps a WAL account_id to the L3 account_hash
	// convention; nil → L3AccountHash (FNV-1a-64).
	AccountHash func(accountID uint64) uint64
	Now         func() time.Time

	MaxOrders     int                          // default 100_000
	MaxStale      time.Duration                // default 500ms
	MaxConcurrent int                          // default 4 concurrent rebuilds
	ReadFile      func(string) ([]byte, error) // test seam; nil → os.ReadFile
}

func (d *L3SnapshotDeps) defaults() {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.MaxOrders <= 0 {
		d.MaxOrders = L3SnapshotMaxOrders
	}
	if d.MaxStale <= 0 {
		d.MaxStale = L3SnapshotMaxStale
	}
	if d.MaxConcurrent <= 0 {
		d.MaxConcurrent = 4
	}
	if d.ReadFile == nil {
		d.ReadFile = os.ReadFile
	}
	if d.AccountHash == nil {
		d.AccountHash = L3AccountHash
	}
}

// L3SnapshotReader is the dedicated WAL reconstruction reader. One
// instance serves many requests; a bounded semaphore caps concurrent
// rebuilds so snapshot pressure never multiplies unboundedly.
type L3SnapshotReader struct {
	d   L3SnapshotDeps
	sem chan struct{}
}

// NewL3SnapshotReader builds the reader from its resolution seams.
func NewL3SnapshotReader(d L3SnapshotDeps) *L3SnapshotReader {
	d.defaults()
	return &L3SnapshotReader{d: d, sem: make(chan struct{}, d.MaxConcurrent)}
}

// L3Page is one paginated snapshot cut.
type L3Page struct {
	Snapshot   L3Snapshot
	NextCursor uint64 // 0 = end of book
}

// Snapshot rebuilds the point-in-time order-level book for symbol at the
// request-time WAL marker, returning resting orders with order_id >
// cursor (ascending), up to limit rows.
func (r *L3SnapshotReader) Snapshot(ctx context.Context, symbol string,
	cursor uint64, limit int) (*L3Page, error) {
	if limit <= 0 {
		limit = 1000
	}
	instr, ok := r.d.InstrumentID(symbol)
	if !ok {
		return nil, ErrL3UnknownSymbol
	}
	shard, ok := r.d.Shard(symbol)
	if !ok {
		return nil, ErrL3UnknownSymbol
	}
	dirs := r.d.WalDirs(symbol)
	if len(dirs) == 0 {
		return nil, ErrL3WALUnavailable
	}

	// Concurrency bound: reconstruction is CPU+IO on the request path —
	// more than MaxConcurrent rebuilds queue-freeze the service, so the
	// pool refuses rather than degrading the book-serving budget.
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Step 1 — WAL position marker: scan every shard dir, take the max
	// valid tail seq. markerAt is the staleness anchor (spec §11.1).
	markerAt := r.d.Now()
	var markerSeq uint64
	type dirScan struct {
		prescan *recovery.RecWalPrescan
	}
	scans := make([]dirScan, 0, len(dirs))
	for _, dir := range dirs {
		// wantShard=-1: under EXC_WAL_DIRS several shard dirs may be
		// listed — a foreign-shard dir is SKIPPED (segment filter
		// below), not an error. Within a dir, segments foreign to this
		// symbol's shard are still corruption (per-shard dirs), so the
		// filter keeps the fail-closed posture.
		pre, err := recovery.RecScanWalDir(dir, -1)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrL3WALUnavailable, err)
		}
		// Keep only this shard's segments (foreign-shard dirs yield an
		// empty prescan here, contributing nothing).
		kept := *pre
		kept.Segments = kept.Segments[:0]
		for _, si := range pre.Segments {
			if int(si.Shard) == shard {
				kept.Segments = append(kept.Segments, si)
			}
		}
		if len(kept.Segments) == 0 {
			continue
		}
		// Fail closed: a lost seq range or damaged sealed segment below
		// the marker means the journal cannot prove point-in-time
		// completeness — serve nothing rather than a holed book (the
		// reconciliation leg applies the same INCONCLUSIVE rule).
		if len(pre.LostRanges) > 0 || pre.SealedDamage {
			return nil, fmt.Errorf("%w: wal dir %s has coverage defects "+
				"(lost_ranges=%d sealed_damage=%v)",
				ErrL3WALUnavailable, dir, len(pre.LostRanges), pre.SealedDamage)
		}
		for _, si := range kept.Segments {
			if si.HasEntries && si.LastSeq > markerSeq {
				markerSeq = si.LastSeq
			}
		}
		scans = append(scans, dirScan{prescan: &kept})
	}
	if markerSeq == 0 {
		return nil, ErrL3WALUnavailable
	}

	// Step 2 — newest BOOK_SNAPSHOT for the instrument at/before the
	// marker, across all dirs newest-first. Segments are seq-ordered;
	// per segment we keep the LAST matching snapshot (segment-local
	// newest). The first hit scanning newest→oldest wins.
	type snapHit struct {
		snap    recovery.BookSnapshotPayload
		snapSeq uint64
	}
	var base *snapHit
	var post []recovery.Entry

	// Collect (dir, segIdx) candidates newest-first by first_seq.
	type segRef struct {
		dir int
		seg int
	}
	var refs []segRef
	for di, s := range scans {
		for si, inf := range s.prescan.Segments {
			if !inf.HasEntries {
				continue
			}
			refs = append(refs, segRef{dir: di, seg: si})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		a := scans[refs[i].dir].prescan.Segments[refs[i].seg]
		b := scans[refs[j].dir].prescan.Segments[refs[j].seg]
		return a.FirstSeq > b.FirstSeq // newest first
	})

	// Pass A: find the newest usable snapshot. A snapshot is usable for
	// L3 only when the ext trailer decodes (per-order price/side); a
	// pinned-only snapshot cannot attribute prices per order — skip it
	// and fall back to older snapshots or full replay (never guess).
	for _, rf := range refs {
		inf := scans[rf.dir].prescan.Segments[rf.seg]
		if base != nil && inf.LastSeq < base.snapSeq {
			continue
		}
		if inf.FirstSeq > markerSeq {
			continue
		}
		ents, err := r.entries(inf.Path)
		if err != nil {
			return nil, err
		}
		var segPost []recovery.Entry
		for i := len(ents) - 1; i >= 0; i-- {
			e := ents[i]
			if e.Seq > markerSeq {
				continue
			}
			if e.Type == recovery.EvBookSnapshot {
				sp, derr := recovery.DecodeBookSnapshot(e.Payload)
				if derr == nil && sp.InstrumentID == instr && sp.ExtOK &&
					len(sp.OrderExt) == len(sp.Orders) {
					base = &snapHit{snap: sp, snapSeq: e.Seq}
					break
				}
			}
		}
		if base != nil {
			// Entries AFTER the snapshot in this segment feed the replay.
			for i := len(ents) - 1; i >= 0; i-- {
				e := ents[i]
				if e.Seq <= base.snapSeq {
					break
				}
				if e.Seq <= markerSeq {
					segPost = append(segPost, e)
				}
			}
			post = append(post, segPost...)
			break
		}
	}

	if base != nil {
		// Segments newer than the snapshot's own all replay forward.
		for _, rf := range refs {
			inf := scans[rf.dir].prescan.Segments[rf.seg]
			if inf.FirstSeq <= base.snapSeq {
				continue
			}
			ents, err := r.entries(inf.Path)
			if err != nil {
				return nil, err
			}
			for _, e := range ents {
				if e.Seq > base.snapSeq && e.Seq <= markerSeq {
					post = append(post, e)
				}
			}
		}
	} else {
		// No usable snapshot: full replay from stream base to marker —
		// the honest fallback (spec prefers snapshot+replay; absence
		// degrades to replay, never fabrication).
		for _, rf := range refs {
			inf := scans[rf.dir].prescan.Segments[rf.seg]
			ents, err := r.entries(inf.Path)
			if err != nil {
				return nil, err
			}
			for _, e := range ents {
				if e.Seq <= markerSeq {
					post = append(post, e)
				}
			}
		}
	}

	// Step 3 — rebuild. Orders are keyed by engine order_id; seqs in
	// `post` sort ascending (segments may arrive out of order across
	// dirs — sort defensively; the WAL seq domain is global per shard).
	sort.Slice(post, func(i, j int) bool { return post[i].Seq < post[j].Seq })
	book := map[uint64]*restingOrder{}
	if base != nil {
		r.applySnapshot(book, &base.snap)
	}
	for i := range post {
		r.applyEntry(book, instr, &post[i])
	}

	if len(book) > r.d.MaxOrders {
		return nil, ErrL3SnapshotTooLarge
	}

	// Step 4 — staleness gate: the marker-pinned cut must be SERVED
	// within the 500 ms budget (reconstruction latency is the staleness
	// a client actually observes — the marker itself is request-time).
	if r.d.Now().Sub(markerAt) > r.d.MaxStale {
		return nil, ErrL3SnapshotStale
	}

	// Step 5 — page: order_id ascending, strictly > cursor.
	ids := make([]uint64, 0, len(book))
	for id := range book {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	page := &L3Page{}
	snap := L3Snapshot{
		Symbol: symbol, WalSeq: markerSeq, Fresh: true,
		AsOfMs: markerAt.UnixMilli(),
	}
	for _, id := range ids {
		if id <= cursor {
			continue
		}
		if len(snap.Orders) >= limit {
			page.NextCursor = snap.Orders[len(snap.Orders)-1].OrderID
			break
		}
		o := book[id]
		snap.Orders = append(snap.Orders, L3RestingOrder{
			OrderID:     o.orderID,
			AccountHash: o.accountHash,
			Side:        o.side,
			Price:       o.price.String(),
			Qty:         o.remaining().String(),
			Hidden:      o.hidden,
		})
	}
	snap.Count = len(snap.Orders)
	page.Snapshot = snap
	return page, nil
}

// entries reads + scans one segment (through the test-seam file reader).
func (r *L3SnapshotReader) entries(path string) ([]recovery.Entry, error) {
	data, err := r.d.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: wal segment %s: %v", ErrL3WALUnavailable, path, err)
	}
	_, ents, err := recovery.ScanSegment(data)
	if err != nil {
		return nil, fmt.Errorf("%w: wal segment %s: %v", ErrL3WALUnavailable, path, err)
	}
	return ents, nil
}

// restingOrder is the rebuild accumulator (per-order book state). The
// reconciliation convention is preserved: orig tracks the last
// ORDER_NEW/ORDER_MODIFY total and fills accumulate separately —
// remaining = orig - filled (an ORDER_MODIFY rebases orig without
// resetting fills, matching walReplay.origQty/filledQty semantics).
type restingOrder struct {
	orderID     uint64
	accountHash uint64
	side        string
	price       decimal.Decimal
	orig        decimal.Decimal
	filled      decimal.Decimal
	hidden      bool
}

// remaining returns the resting (unfilled) quantity.
func (o *restingOrder) remaining() decimal.Decimal { return o.orig.Sub(o.filled) }

// applySnapshot seeds the book from a BOOK_SNAPSHOT ext decode: the
// pinned order array carries account_id + remaining qty; the ext array
// carries per-order price + side + flags (same stream order).
func (r *L3SnapshotReader) applySnapshot(book map[uint64]*restingOrder,
	sp *recovery.BookSnapshotPayload) {
	for i, pin := range sp.Orders {
		ext := sp.OrderExt[i]
		if ext.OrderID != pin.OrderID {
			continue // misaligned ext — row untrusted, skip (never guess)
		}
		if pin.QtyUnits <= 0 {
			continue
		}
		// The pinned record carries REMAINING qty as orig and the ext
		// record carries the fill so remaining() stays consistent with
		// replay-derived orders (orig - filled).
		book[pin.OrderID] = &restingOrder{
			orderID:     pin.OrderID,
			accountHash: r.d.AccountHash(pin.AccountID),
			side:        walSide(ext.Side),
			price:       decimal.NewFromScaled(ext.PriceTicks),
			orig:        decimal.NewFromScaled(ext.QtyUnits),
			filled:      decimal.NewFromScaled(ext.FilledQty),
			hidden:      ext.Flags&l3FlagHiddenBit != 0,
		}
	}
}

// applyEntry folds one replayed WAL entry into the book.
func (r *L3SnapshotReader) applyEntry(book map[uint64]*restingOrder,
	instr uint32, e *recovery.Entry) {
	switch e.Type {
	case recovery.EvOrderNew:
		o, err := recovery.DecodeOrderNew(e.Payload)
		if err != nil || o.InstrumentID != instr {
			return
		}
		if o.QtyUnits <= 0 {
			return
		}
		book[o.OrderID] = &restingOrder{
			orderID:     o.OrderID,
			accountHash: r.d.AccountHash(o.AccountID),
			side:        walSide(o.Side),
			price:       decimal.NewFromScaled(o.PriceTicks),
			orig:        decimal.NewFromScaled(o.QtyUnits),
			hidden:      o.Flags&l3FlagHiddenBit != 0,
		}
	case recovery.EvOrderModify:
		m, err := recovery.DecodeOrderModify(e.Payload)
		if err != nil {
			return
		}
		if o, ok := book[m.OrderID]; ok {
			o.price = decimal.NewFromScaled(m.NewPriceTicks)
			o.orig = decimal.NewFromScaled(m.NewQtyUnits)
		}
	case recovery.EvOrderCancel:
		c, err := recovery.DecodeOrderCancel(e.Payload)
		if err != nil {
			return
		}
		delete(book, c.OrderID)
	case recovery.EvTrade:
		t, err := recovery.DecodeTrade(e.Payload)
		if err != nil || t.InstrumentID != instr {
			return
		}
		// Both legs of a fill accumulate executed quantity — the
		// reconciliation replay's identical convention (a taker's own
		// remaining is its orig minus its fills too).
		r.reduce(book, t.BuyOrderID, t.QtyUnits)
		r.reduce(book, t.SellOrderID, t.QtyUnits)
	}
}

// reduce accrues a fill against a resting order.
func (r *L3SnapshotReader) reduce(book map[uint64]*restingOrder,
	orderID uint64, qtyUnits int64) {
	o, ok := book[orderID]
	if !ok {
		return
	}
	o.filled = o.filled.Add(decimal.NewFromScaled(qtyUnits))
	if !o.remaining().IsPositive() {
		delete(book, orderID)
	}
}

// walSide maps the wire Side byte to the public label.
func walSide(b uint8) string {
	if b == 0 {
		return "BUY"
	}
	return "SELL"
}
