package reconciliation

// walscan.go — WAL → book/trade replay for the ORDERS and TRADES legs.
//
// The engine WAL (internal/recovery/wal.go) is the matching core's own
// journal: ORDER_NEW/CANCEL/MODIFY/TRADE per shard. Replaying it
// reconstructs the core-side resting book and trade stream — the most
// authoritative "core state" projection available, since no
// AdminGetOrders/AdminGetPositions query seam exists (ruling R2).
//
// Fail-closed coverage rule: any scan defect (corrupt segment, lost
// range, foreign shard, torn mid-stream tail) or a trimmed stream base
// marks the replay INCOMPLETE — a partial journal produces INCONCLUSIVE
// findings, never a financial verdict on incomplete input.

import (
	"context"
	"fmt"
	"os"
	"sort"

	"exchange/internal/recovery"
)

// walReplay is the merged, replayed state across all WAL shard dirs.
type walReplay struct {
	// Live book legs: order_id → latest open qty in engine units (the
	// ORDER_NEW/MODIFY qty minus journaled TRADE fills).
	origQty   map[uint64]int64
	filledQty map[uint64]int64
	live      map[uint64]bool
	// order_id → instrument_id for every order ever journaled (kept for
	// halt-scope resolution even after the order leaves the book).
	orderInstr map[uint64]uint32
	// Trade stream: trade_id → executed legs.
	trades map[uint64]walTrade

	minTs, maxTs uint64 // observed event-time span (ns)
	complete     bool   // false → coverage defects → leg inconclusive
	problems     []string
	segments     int
	entries      int
}

type walTrade struct {
	TradeID      uint64
	QtyUnits     int64
	PxTicks      int64
	InstrumentID uint32
	TsNs         uint64
}

// walSource abstracts the replay for unit tests.
type walSource interface {
	Replay(ctx context.Context, dirs []string) (*walReplay, error)
}

type replayWalSource struct{}

func (replayWalSource) Replay(ctx context.Context, dirs []string) (*walReplay, error) {
	return replayWal(ctx, dirs)
}

// replayWal merges per-shard replays. Order/trade ids are engine-global
// (assigned by the core's sequencer), so shard streams merge by id.
func replayWal(ctx context.Context, dirs []string) (*walReplay, error) {
	r := &walReplay{
		origQty:    map[uint64]int64{},
		filledQty:  map[uint64]int64{},
		live:       map[uint64]bool{},
		orderInstr: map[uint64]uint32{},
		trades:     map[uint64]walTrade{},
		complete:   true,
	}
	if len(dirs) == 0 {
		return r, fmt.Errorf("no WAL directories configured")
	}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if err := r.replayDir(dir); err != nil {
			return r, fmt.Errorf("wal dir %s: %w", dir, err)
		}
	}
	return r, nil
}

// replayDir validates one shard dir (RecScanWalDir — magic, shard,
// sequence coverage, corruption) then replays each segment's events.
// Coverage problems are recorded, not fatal: the merged replay still
// reports complete=false so callers degrade to INCONCLUSIVE.
func (r *walReplay) replayDir(dir string) error {
	pre, err := recovery.RecScanWalDir(dir, -1)
	if err != nil {
		r.complete = false
		r.problems = append(r.problems, fmt.Sprintf("prescan %s: %v", dir, err))
		return nil
	}
	if pre.HasEntries && pre.StreamBase > 0 {
		r.complete = false
		r.problems = append(r.problems, fmt.Sprintf(
			"%s: stream base trimmed to seq %d — journal coverage incomplete", dir, pre.StreamBase))
	}
	for _, g := range pre.LostRanges {
		r.complete = false
		r.problems = append(r.problems, fmt.Sprintf(
			"%s: lost sequence range [%d,%d) corrupt=%v", dir, g.Begin, g.End, g.CorruptCaused))
	}
	if pre.SealedDamage {
		r.complete = false
		r.problems = append(r.problems, dir+": corrupt sealed segment(s)")
	}
	if pre.TailCorrupt {
		r.complete = false
		r.problems = append(r.problems, fmt.Sprintf(
			"%s: torn tail at %s (valid_end %d)", dir, pre.TailPath, pre.TailValidEnd))
	}
	if pre.Divergence == "overlap" {
		r.complete = false
		r.problems = append(r.problems, dir+": divergent journal copies (seq overlap)")
	}
	for _, seg := range pre.Segments {
		r.segments++
		if err := r.replaySegment(seg.Path); err != nil {
			r.complete = false
			r.problems = append(r.problems, fmt.Sprintf("%s: %v", dir, err))
		}
	}
	return nil
}

// replaySegment decodes one segment image and folds its entries.
// ScanSegment already stops at the first corrupt offset, so entries are
// the valid prefix only.
func (r *walReplay) replaySegment(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	_, entries, err := recovery.ScanSegment(data)
	if err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	for _, e := range entries {
		r.entries++
		if r.entries == 1 || e.TimestampNs < r.minTs {
			r.minTs = e.TimestampNs
		}
		if e.TimestampNs > r.maxTs {
			r.maxTs = e.TimestampNs
		}
		switch e.Type {
		case recovery.EvOrderNew:
			ev, derr := recovery.DecodeOrderNew(e.Payload)
			if derr != nil {
				return derr
			}
			r.origQty[ev.OrderID] = ev.QtyUnits
			r.filledQty[ev.OrderID] = 0
			r.live[ev.OrderID] = true
			r.orderInstr[ev.OrderID] = ev.InstrumentID
		case recovery.EvOrderModify:
			ev, derr := recovery.DecodeOrderModify(e.Payload)
			if derr != nil {
				return derr
			}
			r.origQty[ev.OrderID] = ev.NewQtyUnits
		case recovery.EvOrderCancel:
			ev, derr := recovery.DecodeOrderCancel(e.Payload)
			if derr != nil {
				return derr
			}
			delete(r.live, ev.OrderID)
		case recovery.EvTrade:
			ev, derr := recovery.DecodeTrade(e.Payload)
			if derr != nil {
				return derr
			}
			r.trades[ev.TradeID] = walTrade{
				TradeID:  ev.TradeID,
				QtyUnits: ev.QtyUnits, PxTicks: ev.PriceTicks,
				InstrumentID: ev.InstrumentID, TsNs: e.TimestampNs,
			}
			r.filledQty[ev.BuyOrderID] += ev.QtyUnits
			r.filledQty[ev.SellOrderID] += ev.QtyUnits
		}
	}
	return nil
}

// RestingOrder is one replayed open order (remaining qty > 0).
type RestingOrder struct {
	OrderID      uint64
	RemainingQty int64
	InstrumentID uint32
}

// RestingOrders returns the replayed open book sorted by order id.
func (r *walReplay) RestingOrders() []RestingOrder {
	out := make([]RestingOrder, 0, len(r.live))
	for id := range r.live {
		rem := r.origQty[id] - r.filledQty[id]
		if rem > 0 {
			out = append(out, RestingOrder{
				OrderID:      id,
				RemainingQty: rem,
				InstrumentID: r.orderInstr[id],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	return out
}

// TradeMap returns the journaled trade id → trade map (keyed for lookup).
func (r *walReplay) TradeMap() map[uint64]walTrade { return r.trades }

// Problems lists the coverage defects (empty → journal complete).
func (r *walReplay) Problems() []string { return r.problems }

// Complete reports whether the journal replayed cleanly end-to-end.
func (r *walReplay) Complete() bool { return r.complete }

// Coverage returns (min_ts_ns, max_ts_ns, entries) for detail payloads.
func (r *walReplay) Coverage() (minTs, maxTs uint64, entries int) {
	return r.minTs, r.maxTs, r.entries
}
