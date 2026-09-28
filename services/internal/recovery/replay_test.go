package recovery_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"exchange/internal/objectstore"
	"exchange/internal/recovery"
)

const (
	px110 = int64(110000000) // 1.1000 × 1e8
	px120 = int64(120000000)
	qty   = int64(1000000)
)

// buildBookSnapshotPayload constructs a BOOK_SNAPSHOT payload with the
// ext trailer: 1 level + orders, then ext header + ext records.
func snapPayload(inst uint32, tsNs uint64) []byte {
	// One bid level @ px110 with one resting order id=9 acct=104 rem 500.
	var b []byte
	hdr := make([]byte, 24)
	binary64(hdr[0:], uint64(0)) // overwritten below (u32) — see le32
	le32(hdr[0:], inst)
	le32(hdr[4:], 1)       // level_count
	binary64(hdr[8:], 1)   // order_count
	binary64(hdr[16:], 42) // book_seq
	b = append(b, hdr...)
	lvl := make([]byte, 16)
	binary64(lvl[0:], uint64(px110))
	lvl[8] = 0 // bid
	b = append(b, lvl...)
	ord := make([]byte, 48)
	binary64(ord[0:], 9)
	binary64(ord[8:], 104)
	binary64(ord[16:], uint64(500))
	binary64(ord[24:], uint64(500))
	b = append(b, ord...)
	xh := make([]byte, 16)
	le32(xh[0:], recovery.SnapExtMagic)
	xh[4], xh[5] = 1, 0
	binary64(xh[8:], 1)
	b = append(b, xh...)
	xe := make([]byte, 60)
	binary64(xe[0:], 9)
	binary64(xe[8:], uint64(500))
	binary64(xe[24:], uint64(px110))
	binary64(xe[32:], tsNs)
	le32(xe[48:], 0) // level_index
	xe[53] = 0       // side buy
	b = append(b, xe...)
	return pload(recovery.EvBookSnapshot, b)
}

func archiveSegments(t *testing.T, c objectstore.Client, shard uint16,
	segs map[string][]byte) {
	t.Helper()
	svc := recovery.NewArchiveService(c)
	for name, data := range segs {
		if _, err := svc.ArchiveSegment(context.Background(), shard, name, data, time.Now()); err != nil {
			t.Fatalf("archive %s: %v", name, err)
		}
	}
}

func TestReplayFromArchive(t *testing.T) {
	c, done := testStack(t, "exchange-wal")
	defer done()
	ctx := context.Background()

	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	ts0 := uint64(base.UnixNano())
	min := uint64(time.Minute)

	// Segment 1 (seq 0..4): resting ask, aggressive buy, trade, tick, sell
	// back at higher price.
	seg1 := segment(0, 0, ts0, 1,
		[][]byte{
			encOrderNew(1, 101, 1, 1, 1, 0, px110, qty), // resting ask
			encOrderNew(2, 102, 1, 0, 1, 1, px110, qty), // IOC buy
			encTrade(1, 2, 1, 1, px110, qty),            // 102 buys from 101
			encTick(ts0 + 5*min + 1),                    // crosses 5m boundary
		})
	// Segment 2 (seq 4..7), next day: 102 sells to new bid, book snapshot.
	ts1 := uint64(base.AddDate(0, 0, 1).UnixNano())
	seg2 := segment(0, 4, ts1, 1,
		[][]byte{
			encOrderNew(4, 103, 1, 0, 1, 0, px120, qty), // resting bid @1.20
			encOrderNew(3, 102, 1, 1, 0, 1, px120, qty), // market sell
			encTrade(2, 4, 3, 1, px120, qty),            // 102 sells to 103
			snapPayload(1, ts1+4),                       // snapshot loads book
		})

	archiveSegments(t, c, 0, map[string][]byte{
		"00000000000000000000.wal": seg1,
		"00000000000000000004.wal": seg2,
	})

	rep, err := recovery.NewReplayer(c).Replay(ctx, recovery.ReplayOptions{
		Shard: 0, InstrumentID: 1,
		From: base, To: base.AddDate(0, 0, 1),
		SnapshotEvery:  5 * time.Minute,
		ResolveSymbols: map[uint32]string{1: "EUR/USD"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(rep.Trades) != 2 {
		t.Fatalf("trades=%d", len(rep.Trades))
	}
	if rep.Trades[0].BuyAccount != 102 || rep.Trades[0].SellAccount != 101 ||
		rep.Trades[0].PriceTicks != px110 {
		t.Fatalf("trade0 %+v", rep.Trades[0])
	}
	if rep.Trades[1].BuyAccount != 103 || rep.Trades[1].SellAccount != 102 {
		t.Fatalf("trade1 %+v", rep.Trades[1])
	}

	// P&L: acct102 round-trip 1.10→1.20 → realized +qty*(px120-px110)/1e8.
	var p102 *recovery.PnLRecord
	for i := range rep.PnL {
		if rep.PnL[i].AccountID == 102 {
			p102 = &rep.PnL[i]
		}
	}
	if p102 == nil {
		t.Fatalf("no pnl for 102: %+v", rep.PnL)
	}
	wantRealized := qty * (px120 - px110) / 1e8 // = 100000
	if p102.RealizedQuote != wantRealized || p102.PositionUnits != 0 {
		t.Fatalf("pnl 102: %+v want realized=%d pos=0", p102, wantRealized)
	}
	// acct101 short 1e6 @1.10 → unrealized vs mark px120.
	var p101 *recovery.PnLRecord
	for i := range rep.PnL {
		if rep.PnL[i].AccountID == 101 {
			p101 = &rep.PnL[i]
		}
	}
	if p101 == nil || p101.PositionUnits != -qty {
		t.Fatalf("pnl 101: %+v", p101)
	}
	// (mark-avg)*pos/1e8 = (1.2e8-1.1e8)*(-1e6)/1e8 = -100000
	if p101.UnrealizedQuote != -100000 {
		t.Fatalf("unrealized: %d", p101.UnrealizedQuote)
	}

	// Snapshots: at least the interval one after the tick + wal_snapshot.
	var interval, walSnap int
	for _, s := range rep.Snapshots {
		if s.Source == "interval" {
			interval++
		}
		if s.Source == "wal_snapshot" {
			walSnap++
			// snapshot-loaded book: bid @px110 qty 500
			if len(s.Bids) != 1 || s.Bids[0].PriceTicks != px110 || s.Bids[0].QtyUnits != 500 {
				t.Fatalf("snapshot depth %+v", s.Bids)
			}
		}
	}
	if interval == 0 || walSnap != 1 {
		t.Fatalf("interval=%d walSnap=%d", interval, walSnap)
	}
	if len(rep.MissingDates) != 0 {
		t.Fatalf("missing dates: %v", rep.MissingDates)
	}
}

func TestReplayMissingRange(t *testing.T) {
	c, done := testStack(t, "exchange-wal")
	defer done()
	_, err := recovery.NewReplayer(c).Replay(context.Background(), recovery.ReplayOptions{
		Shard: 9, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		To: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err == nil || !strings.Contains(err.Error(), "no archived segments") {
		t.Fatalf("want no-segments error, got %v", err)
	}
}

func TestReplayCorruptSegmentFailsClosed(t *testing.T) {
	c, done := testStack(t, "exchange-wal")
	defer done()
	ctx := context.Background()
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	// Valid first entry anchors the archive date; corrupted tail record.
	seg := segment(0, 0, uint64(base.UnixNano()), 1,
		[][]byte{encTick(uint64(base.UnixNano())),
			encOrderNew(1, 1, 1, 0, 1, 0, 100, 10)})
	seg[len(seg)-3] ^= 0xFF // corrupt CRC region of last entry
	archiveSegments(t, c, 0, map[string][]byte{"0.wal": seg})

	_, err := recovery.NewReplayer(c).Replay(ctx, recovery.ReplayOptions{
		Shard: 0, From: base, To: base,
	})
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want corrupt error, got %v", err)
	}
}

func TestReplaySeqGapFailsClosed(t *testing.T) {
	c, done := testStack(t, "exchange-wal")
	defer done()
	ctx := context.Background()
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	ts := uint64(base.UnixNano())
	// Two segments, same date, seq 0..0 and 50..50 → gap.
	s1 := segment(0, 0, ts, 1, [][]byte{encTick(ts)})
	s2 := segment(0, 50, ts+1, 1, [][]byte{encTick(ts + 1)})
	archiveSegments(t, c, 0, map[string][]byte{"0.wal": s1, "50.wal": s2})

	_, err := recovery.NewReplayer(c).Replay(ctx, recovery.ReplayOptions{
		Shard: 0, From: base, To: base,
	})
	if err == nil || !strings.Contains(err.Error(), "seq gap") {
		t.Fatalf("want seq-gap error, got %v", err)
	}
	rep, err := recovery.NewReplayer(c).Replay(ctx, recovery.ReplayOptions{
		Shard: 0, From: base, To: base, AllowSeqGaps: true,
	})
	if err != nil {
		t.Fatalf("allow-gaps should pass: %v", err)
	}
	if len(rep.SeqGaps) != 1 {
		t.Fatalf("gaps %+v", rep.SeqGaps)
	}
}

func TestReplayMissingObjectFailsClosed(t *testing.T) {
	c, done := testStack(t, "exchange-wal")
	defer done()
	ctx := context.Background()
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	ts := uint64(base.UnixNano())
	archiveSegments(t, c, 0, map[string][]byte{
		"0.wal": segment(0, 0, ts, 1, [][]byte{encTick(ts)}),
	})
	// Remove the object but keep the index → replay must fail closed.
	if err := c.Delete(ctx, "0/2026-01-05/0.wal"); err != nil {
		t.Fatal(err)
	}
	_, err := recovery.NewReplayer(c).Replay(ctx, recovery.ReplayOptions{
		Shard: 0, From: base, To: base,
	})
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("want unreadable error, got %v", err)
	}
}
