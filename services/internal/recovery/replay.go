// replay.go — Task 4.3.3: replay-from-archive. Downloads archived WAL
// segments for a date range and replays them through a read-only book /
// P&L reconstruction. Never touches live trading state: inputs are S3
// objects only, outputs are an in-memory report.
//
// Fail-closed per spec §2.7: missing objects, corrupt index, corrupt or
// truncated segments, and shard-seq gaps all abort the replay with a clear
// error (seq gaps can be downgraded to warnings with AllowSeqGaps).
package recovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"exchange/internal/objectstore"
)

// ReplayOptions selects the replay window.
type ReplayOptions struct {
	Shard          uint16
	InstrumentID   uint32            // 0 = all instruments on the shard
	From, To       time.Time         // UTC calendar dates, inclusive
	SnapshotEvery  time.Duration     // book-snapshot cadence; default 5m
	AllowSeqGaps   bool              // default false = fail closed on seq holes
	ResolveSymbols map[uint32]string // optional instrument_id→symbol labels
}

// SegmentReplay describes one replayed archive object.
type SegmentReplay struct {
	Key      string `json:"key"`
	Segment  string `json:"segment"`
	Date     string `json:"date"`
	Entries  uint64 `json:"entries"`
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	Bytes    int64  `json:"bytes"`
	Corrupt  bool   `json:"corrupt"`
}

// TradeRecord is one reconstructed fill.
type TradeRecord struct {
	Seq          uint64 `json:"seq"`
	TsNs         uint64 `json:"ts_ns"`
	Ts           string `json:"ts"`
	TradeID      uint64 `json:"trade_id"`
	InstrumentID uint32 `json:"instrument_id"`
	Symbol       string `json:"symbol,omitempty"`
	PriceTicks   int64  `json:"price_ticks"`
	QtyUnits     int64  `json:"qty_units"`
	BuyOrderID   uint64 `json:"buy_order_id"`
	SellOrderID  uint64 `json:"sell_order_id"`
	BuyAccount   uint64 `json:"buy_account"`
	SellAccount  uint64 `json:"sell_account"`
}

// BookLevel is one aggregated depth row: [price_ticks, qty_units, orders].
type BookLevel struct {
	PriceTicks int64 `json:"price_ticks"`
	QtyUnits   int64 `json:"qty_units"`
	Orders     int   `json:"orders"`
}

// BookSnapshotRecord is a periodic (5-min default) reconstructed book.
type BookSnapshotRecord struct {
	TsNs         uint64      `json:"ts_ns"`
	Ts           string      `json:"ts"`
	InstrumentID uint32      `json:"instrument_id"`
	Symbol       string      `json:"symbol,omitempty"`
	Source       string      `json:"source"` // "interval" | "wal_snapshot"
	Bids         []BookLevel `json:"bids"`
	Asks         []BookLevel `json:"asks"`
}

// PnLRecord is per-(account,instrument) average-cost P&L over the window.
// All values are in scaled units (price/qty are 1e8-scaled; realized and
// notional are qty·price/1e8 — the WAL "units" convention).
type PnLRecord struct {
	AccountID       uint64 `json:"account_id"`
	InstrumentID    uint32 `json:"instrument_id"`
	Symbol          string `json:"symbol,omitempty"`
	PositionUnits   int64  `json:"position_units"`   // signed net qty
	RealizedQuote   int64  `json:"realized_quote"`   // signed realized P&L in quote units
	UnrealizedQuote int64  `json:"unrealized_quote"` // marked at last seen trade price
	VolumeUnits     int64  `json:"volume_units"`     // gross traded qty
	NotionalQuote   int64  `json:"notional_quote"`   // gross qty·price/1e8
	Trades          int    `json:"trades"`
}

// ReplayReport is the CLI/JSON output.
type ReplayReport struct {
	Shard        uint16               `json:"shard"`
	InstrumentID uint32               `json:"instrument_id"`
	From         string               `json:"from"`
	To           string               `json:"to"`
	GeneratedAt  string               `json:"generated_at"`
	Segments     []SegmentReplay      `json:"segments"`
	CoveredDates []string             `json:"covered_dates"`
	MissingDates []string             `json:"missing_dates"`
	SeqGaps      []string             `json:"seq_gaps,omitempty"`
	Warnings     []string             `json:"warnings,omitempty"`
	Trades       []TradeRecord        `json:"trades"`
	Snapshots    []BookSnapshotRecord `json:"snapshots"`
	PnL          []PnLRecord          `json:"pnl"`
}

// Replayer runs read-only reconstruction against the archive bucket.
type Replayer struct {
	store objectstore.Client
	now   func() time.Time
}

func NewReplayer(store objectstore.Client) *Replayer {
	return &Replayer{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// restingOrder is one live book order in the reconstruction.
type restingOrder struct {
	orderID   uint64
	accountID uint64
	side      uint8
	price     int64
	remaining int64
}

// instrumentBook holds reconstructed state for one instrument.
type instrumentBook struct {
	orders map[uint64]*restingOrder // live resting orders
}

func newBook() *instrumentBook { return &instrumentBook{orders: map[uint64]*restingOrder{}} }

type levelAgg struct {
	qty   int64
	count int
}

func (b *instrumentBook) depth() (bids, asks []BookLevel) {
	bm := map[int64]*levelAgg{}
	am := map[int64]*levelAgg{}
	for _, o := range b.orders {
		if o.remaining <= 0 {
			continue
		}
		m := bm
		if o.side != 0 {
			m = am
		}
		a := m[o.price]
		if a == nil {
			a = &levelAgg{}
			m[o.price] = a
		}
		a.qty += o.remaining
		a.count++
	}
	bids = levelsOf(bm, true)
	asks = levelsOf(am, false)
	return
}

func levelsOf(m map[int64]*levelAgg, desc bool) []BookLevel {
	prices := make([]int64, 0, len(m))
	for p := range m {
		prices = append(prices, p)
	}
	sort.Slice(prices, func(i, j int) bool {
		if desc {
			return prices[i] > prices[j]
		}
		return prices[i] < prices[j]
	})
	out := make([]BookLevel, 0, len(prices))
	for _, p := range prices {
		out = append(out, BookLevel{PriceTicks: p, QtyUnits: m[p].qty, Orders: m[p].count})
	}
	return out
}

// account state for avg-cost P&L.
type acctPos struct {
	pos      int64 // signed net qty
	avgPrice int64 // average entry price of open position
	realized int64 // quote units
	volume   int64
	notional int64
	trades   int
}

// applyFill updates position with signed delta at price; realized math is
// integer-exact: P&L in qty·price units, scaled down by 1e8 at the end.
func (a *acctPos) applyFill(delta, price int64) {
	if a.pos == 0 || sign(delta) == sign(a.pos) {
		// Opening or adding: weighted average entry.
		total := abs(a.pos) + abs(delta)
		if total > 0 {
			// avg = (|pos|*avg + |d|*price)/total — keep in price ticks.
			a.avgPrice = (abs(a.pos)*a.avgPrice + abs(delta)*price) / total
		}
		a.pos += delta
		return
	}
	// Closing (possibly flipping).
	closed := min64(abs(delta), abs(a.pos))
	// realized += closed * (price - avg) * sign(pos)  [qty·price units]
	a.realized += closed * (price - a.avgPrice) * sign(a.pos)
	newPos := a.pos + delta
	if sign(newPos) != sign(a.pos) {
		// Flat or flipped: any residual opens at the trade price.
		a.avgPrice = price
	}
	a.pos = newPos
	if a.pos == 0 {
		a.avgPrice = 0
	}
}

func sign(v int64) int64 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}
func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Replay runs the full pipeline: index → range select → download → scan →
// reconstruct → report.
func (r *Replayer) Replay(ctx context.Context, o ReplayOptions) (*ReplayReport, error) {
	if o.SnapshotEvery <= 0 {
		o.SnapshotEvery = 5 * time.Minute
	}
	if o.To.Before(o.From) {
		return nil, errors.New("replay: --to before --from")
	}
	svc := NewArchiveService(r.store)
	idx, err := svc.LoadIndex(ctx, o.Shard)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}

	// Date-range select against the index.
	fromD := o.From.UTC().Format("2006-01-02")
	toD := o.To.UTC().Format("2006-01-02")
	var picked []SegmentMeta
	covered := map[string]bool{}
	for _, m := range idx.Segments {
		if m.Date >= fromD && m.Date <= toD {
			picked = append(picked, m)
			covered[m.Date] = true
		}
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].FirstSeq < picked[j].FirstSeq })

	rep := &ReplayReport{
		Shard:        o.Shard,
		InstrumentID: o.InstrumentID,
		From:         fromD,
		To:           toD,
		GeneratedAt:  r.now().Format(time.RFC3339),
	}
	for d := o.From; !d.After(o.To); d = d.AddDate(0, 0, 1) {
		ds := d.UTC().Format("2006-01-02")
		if covered[ds] {
			rep.CoveredDates = append(rep.CoveredDates, ds)
		} else {
			rep.MissingDates = append(rep.MissingDates, ds)
		}
	}
	if len(picked) == 0 {
		return rep, fmt.Errorf("replay: no archived segments for shard %d in %s..%s", o.Shard, fromD, toD)
	}

	rp := &replayState{
		o:        o,
		rep:      rep,
		books:    map[uint32]*instrumentBook{},
		orderAcc: map[uint64]orderAcc{},
		pnl:      map[[2]uint64]*acctPos{},
		lastPx:   map[uint32]int64{},
	}
	var prevSeq uint64
	havePrev := false
	for _, m := range picked {
		data, _, err := r.store.Get(ctx, m.Key)
		if err != nil {
			return rep, fmt.Errorf("replay: segment %s (%s) unreadable: %w", m.Segment, m.Key, err)
		}
		scan, entries, err := ScanSegment(data)
		if err != nil {
			return rep, fmt.Errorf("replay: segment %s: %w", m.Key, err)
		}
		if scan.Shard != o.Shard {
			return rep, fmt.Errorf("replay: segment %s shard %d, want %d", m.Key, scan.Shard, o.Shard)
		}
		if scan.Corrupt {
			return rep, fmt.Errorf("replay: segment %s corrupt at offset %d (fail closed)", m.Key, scan.CorruptOffset)
		}
		if len(entries) > 0 {
			if havePrev && entries[0].Seq > prevSeq+1 {
				gap := fmt.Sprintf("seq gap %d..%d before %s", prevSeq+1, entries[0].Seq-1, m.Key)
				if !o.AllowSeqGaps {
					return rep, fmt.Errorf("replay: %s (fail closed; --allow-seq-gaps to override)", gap)
				}
				rep.SeqGaps = append(rep.SeqGaps, gap)
			}
			if havePrev && entries[0].Seq <= prevSeq {
				rep.Warnings = append(rep.Warnings,
					fmt.Sprintf("seq overlap %d..%d at %s (duplicate archive)", entries[0].Seq, prevSeq, m.Key))
			}
			prevSeq, havePrev = entries[len(entries)-1].Seq, true
		}
		rep.Segments = append(rep.Segments, SegmentReplay{
			Key: m.Key, Segment: m.Segment, Date: m.Date,
			Entries: scan.Entries, Bytes: int64(len(data)),
			Corrupt: scan.Corrupt,
		})
		if len(entries) > 0 {
			rep.Segments[len(rep.Segments)-1].FirstSeq = entries[0].Seq
			rep.Segments[len(rep.Segments)-1].LastSeq = entries[len(entries)-1].Seq
		}
		if err := rp.apply(entries); err != nil {
			return rep, fmt.Errorf("replay: segment %s: %w", m.Key, err)
		}
	}
	rp.finish()
	return rep, nil
}

// orderAcc is the persistent order→account/side/instrument attribution map
// (orders remain known after fill/cancel so trades attribute correctly).
type orderAcc struct {
	accountID    uint64
	instrumentID uint32
	side         uint8
}

type replayState struct {
	o        ReplayOptions
	rep      *ReplayReport
	books    map[uint32]*instrumentBook
	orderAcc map[uint64]orderAcc
	pnl      map[[2]uint64]*acctPos // (account,instrument)
	lastPx   map[uint32]int64       // instrument → last trade price
	nextSnap uint64                 // next interval boundary (ns)
}

func (rs *replayState) book(inst uint32) *instrumentBook {
	b := rs.books[inst]
	if b == nil {
		b = newBook()
		rs.books[inst] = b
	}
	return b
}

func (rs *replayState) sym(inst uint32) string {
	if rs.o.ResolveSymbols != nil {
		return rs.o.ResolveSymbols[inst]
	}
	return ""
}

func (rs *replayState) apply(entries []Entry) error {
	for _, e := range entries {
		// Interval snapshots on the engine clock: TIME_TICK payloads carry
		// the deterministic logical clock; other entries use the header ts.
		ts := e.TimestampNs
		if e.Type == EvTimeTick {
			if tk, terr := DecodeTimeTick(e.Payload); terr == nil && tk.TickNs > 0 {
				ts = tk.TickNs
			}
		}
		// Cadence anchors to the first entry seen so the report doesn't
		// lead with an empty book.
		if rs.nextSnap == 0 {
			b := ts / uint64(rs.o.SnapshotEvery)
			rs.nextSnap = (b + 1) * uint64(rs.o.SnapshotEvery)
		}
		if ts >= rs.nextSnap {
			rs.emitSnapshots(ts)
			b := ts / uint64(rs.o.SnapshotEvery)
			rs.nextSnap = (b + 1) * uint64(rs.o.SnapshotEvery)
		}
		if err := rs.applyOne(e); err != nil {
			return fmt.Errorf("seq %d (%s): %w", e.Seq, e.Type, err)
		}
	}
	return nil
}

func (rs *replayState) emitSnapshots(tsNs uint64) {
	insts := make([]uint32, 0, len(rs.books))
	for id := range rs.books {
		if rs.o.InstrumentID == 0 || id == rs.o.InstrumentID {
			insts = append(insts, id)
		}
	}
	sort.Slice(insts, func(i, j int) bool { return insts[i] < insts[j] })
	for _, id := range insts {
		bids, asks := rs.books[id].depth()
		rs.rep.Snapshots = append(rs.rep.Snapshots, BookSnapshotRecord{
			TsNs: tsNs, Ts: time.Unix(0, int64(tsNs)).UTC().Format(time.RFC3339Nano),
			InstrumentID: id, Symbol: rs.sym(id), Source: "interval",
			Bids: bids, Asks: asks,
		})
	}
}

func (rs *replayState) applyOne(e Entry) error {
	switch e.Type {
	case EvOrderNew:
		o, err := DecodeOrderNew(e.Payload)
		if err != nil {
			return err
		}
		rs.orderAcc[o.OrderID] = orderAcc{o.AccountID, o.InstrumentID, o.Side}
		// Only resting orders join the book: MARKET (type 0) never rests,
		// IOC/FOK (tif 1/2) never rest, and STOP orders (stop_price != 0)
		// live off-book until triggered.
		if o.Type != 0 && o.TIF != 1 && o.TIF != 2 && o.StopPriceTicks == 0 {
			rs.book(o.InstrumentID).orders[o.OrderID] = &restingOrder{
				orderID: o.OrderID, accountID: o.AccountID,
				side: o.Side, price: o.PriceTicks, remaining: o.QtyUnits,
			}
		}
	case EvOrderCancel:
		c, err := DecodeOrderCancel(e.Payload)
		if err != nil {
			return err
		}
		if acc, ok := rs.orderAcc[c.OrderID]; ok {
			delete(rs.book(acc.instrumentID).orders, c.OrderID)
		}
	case EvOrderModify:
		m, err := DecodeOrderModify(e.Payload)
		if err != nil {
			return err
		}
		if acc, ok := rs.orderAcc[m.OrderID]; ok {
			if ro, ok2 := rs.book(acc.instrumentID).orders[m.OrderID]; ok2 {
				if m.NewPriceTicks != 0 {
					ro.price = m.NewPriceTicks
				}
				if m.NewQtyUnits != 0 {
					ro.remaining = m.NewQtyUnits
				}
			}
		}
	case EvTrade:
		t, err := DecodeTrade(e.Payload)
		if err != nil {
			return err
		}
		buy := rs.orderAcc[t.BuyOrderID]
		sell := rs.orderAcc[t.SellOrderID]
		if t.BuyOrderID == t.SellOrderID && t.BuyOrderID == 0 {
			rs.rep.Warnings = append(rs.rep.Warnings,
				fmt.Sprintf("seq %d: trade %d with zero order ids", e.Seq, t.TradeID))
		}
		// Resting side(s) lose qty in the book.
		for _, oid := range [...]uint64{t.BuyOrderID, t.SellOrderID} {
			if acc, ok := rs.orderAcc[oid]; ok {
				if ro, ok2 := rs.book(acc.instrumentID).orders[oid]; ok2 {
					ro.remaining -= t.QtyUnits
					if ro.remaining <= 0 {
						delete(rs.book(acc.instrumentID).orders, oid)
					}
				}
			}
		}
		rs.rep.Trades = append(rs.rep.Trades, TradeRecord{
			Seq: e.Seq, TsNs: e.TimestampNs,
			Ts:      time.Unix(0, int64(e.TimestampNs)).UTC().Format(time.RFC3339Nano),
			TradeID: t.TradeID, InstrumentID: t.InstrumentID,
			Symbol:     rs.sym(t.InstrumentID),
			PriceTicks: t.PriceTicks, QtyUnits: t.QtyUnits,
			BuyOrderID: t.BuyOrderID, SellOrderID: t.SellOrderID,
			BuyAccount: buy.accountID, SellAccount: sell.accountID,
		})
		if _, ok := rs.orderAcc[t.BuyOrderID]; !ok {
			rs.rep.Warnings = append(rs.rep.Warnings,
				fmt.Sprintf("seq %d: trade %d buy order %d unseen in window", e.Seq, t.TradeID, t.BuyOrderID))
		}
		if _, ok := rs.orderAcc[t.SellOrderID]; !ok {
			rs.rep.Warnings = append(rs.rep.Warnings,
				fmt.Sprintf("seq %d: trade %d sell order %d unseen in window", e.Seq, t.TradeID, t.SellOrderID))
		}
		rs.lastPx[t.InstrumentID] = t.PriceTicks
		rs.fill(t.InstrumentID, buy.accountID, t.QtyUnits, t.PriceTicks)   // buyer +
		rs.fill(t.InstrumentID, sell.accountID, -t.QtyUnits, t.PriceTicks) // seller −
	case EvBookSnapshot:
		s, err := DecodeBookSnapshot(e.Payload)
		if err != nil {
			return err
		}
		rs.loadSnapshot(s, e)
	case EvTimeTick, EvMarginReserve, EvMarginRelease:
		// Clock / margin-slice events: no book state; the ts check above
		// already advances the snapshot cadence on TIME_TICK.
	case EvPreventedMatch:
		// Book-level no-op by contract (WalEntry.hpp): accompanying
		// CANCEL/MODIFY entries carry the real state change.
	default:
		rs.rep.Warnings = append(rs.rep.Warnings,
			fmt.Sprintf("seq %d: unknown event type %d", e.Seq, uint8(e.Type)))
	}
	return nil
}

func (rs *replayState) fill(inst uint32, account uint64, delta, price int64) {
	key := [2]uint64{account, uint64(inst)}
	p := rs.pnl[key]
	if p == nil {
		p = &acctPos{}
		rs.pnl[key] = p
	}
	p.applyFill(delta, price)
	p.volume += abs(delta)
	p.notional += abs(delta) * price / 1e8
	p.trades++
}

// loadSnapshot rebuilds an instrument's book from a WAL BOOK_SNAPSHOT.
// With the ext block, orders map to levels exactly (level_index → price +
// side); without it we can only restore order→account attribution and warn.
func (rs *replayState) loadSnapshot(s BookSnapshotPayload, e Entry) {
	b := newBook()
	if s.ExtOK {
		for i, xe := range s.OrderExt {
			if int(xe.LevelIndex) >= len(s.Levels) {
				rs.rep.Warnings = append(rs.rep.Warnings,
					fmt.Sprintf("seq %d: snapshot ext order %d bad level_index %d",
						e.Seq, xe.OrderID, xe.LevelIndex))
				continue
			}
			lvl := s.Levels[xe.LevelIndex]
			rem := s.Orders[i].QtyUnits // pinned = remaining
			b.orders[xe.OrderID] = &restingOrder{
				orderID: xe.OrderID, accountID: s.Orders[i].AccountID,
				side: lvl.Side, price: lvl.PriceTicks, remaining: rem,
			}
			rs.orderAcc[xe.OrderID] = orderAcc{s.Orders[i].AccountID, s.InstrumentID, lvl.Side}
		}
	} else {
		rs.rep.Warnings = append(rs.rep.Warnings,
			fmt.Sprintf("seq %d: BOOK_SNAPSHOT lacks ext block — depth limited to level prices", e.Seq))
		for _, o := range s.Orders {
			rs.orderAcc[o.OrderID] = orderAcc{o.AccountID, s.InstrumentID, 0}
		}
	}
	rs.books[s.InstrumentID] = b
	bids, asks := b.depth()
	rs.rep.Snapshots = append(rs.rep.Snapshots, BookSnapshotRecord{
		TsNs: e.TimestampNs, Ts: time.Unix(0, int64(e.TimestampNs)).UTC().Format(time.RFC3339Nano),
		InstrumentID: s.InstrumentID, Symbol: rs.sym(s.InstrumentID),
		Source: "wal_snapshot", Bids: bids, Asks: asks,
	})
}

// finish materializes the P&L table with unrealized marked at last trade
// price.
func (rs *replayState) finish() {
	var keys [][2]uint64
	for k := range rs.pnl {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		p := rs.pnl[k]
		inst := uint32(k[1])
		mark := rs.lastPx[inst]
		var unreal int64
		if p.pos != 0 && mark != 0 {
			// (mark - avg) * pos, scaled to quote units.
			unreal = (mark - p.avgPrice) * p.pos / 1e8
		}
		rs.rep.PnL = append(rs.rep.PnL, PnLRecord{
			AccountID:       k[0],
			InstrumentID:    inst,
			Symbol:          rs.sym(inst),
			PositionUnits:   p.pos,
			RealizedQuote:   p.realized / 1e8,
			UnrealizedQuote: unreal,
			VolumeUnits:     p.volume,
			NotionalQuote:   p.notional,
			Trades:          p.trades,
		})
	}
	rs.rep.Warnings = dedup(rs.rep.Warnings)
	rs.rep.SeqGaps = dedup(rs.rep.SeqGaps)
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
