// Phase-17 Task 17.3.2 — WAL snapshot reader tests: point-in-time
// marker reconstruction, pagination, ceiling, staleness, shard filter.
package marketdata

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"exchange/internal/recovery"
)

// --- WAL segment builders (exported recovery encoders — same bytes the
//     C++ WalWriter emits). -------------------------------------------------

func walOrderNew(orderID, acct uint64, inst uint32, side, flags uint8,
	price, qty int64) []byte {
	p := make([]byte, 80)
	binary.LittleEndian.PutUint64(p[0:8], orderID)
	binary.LittleEndian.PutUint64(p[8:16], acct)
	binary.LittleEndian.PutUint32(p[16:20], inst)
	p[20], p[21], p[22], p[23] = side, 1, 0, flags
	binary.LittleEndian.PutUint64(p[24:32], uint64(price))
	binary.LittleEndian.PutUint64(p[32:40], uint64(qty))
	binary.LittleEndian.PutUint64(p[40:48], uint64(qty))
	return p
}

func walOrderModify(orderID uint64, price, qty int64) []byte {
	p := make([]byte, 40)
	binary.LittleEndian.PutUint64(p[0:8], orderID)
	binary.LittleEndian.PutUint64(p[8:16], uint64(price))
	binary.LittleEndian.PutUint64(p[16:24], uint64(qty))
	return p
}

func walOrderCancel(orderID, acct uint64) []byte {
	p := make([]byte, 24)
	binary.LittleEndian.PutUint64(p[0:8], orderID)
	binary.LittleEndian.PutUint64(p[8:16], acct)
	return p
}

func walTrade(tradeID, buyID, sellID uint64, inst uint32, price, qty int64) []byte {
	p := make([]byte, 48)
	binary.LittleEndian.PutUint64(p[0:8], tradeID)
	binary.LittleEndian.PutUint64(p[8:16], buyID)
	binary.LittleEndian.PutUint64(p[16:24], sellID)
	binary.LittleEndian.PutUint32(p[24:28], inst)
	binary.LittleEndian.PutUint64(p[32:40], uint64(price))
	binary.LittleEndian.PutUint64(p[40:48], uint64(qty))
	return p
}

// walBookSnapshot builds a BOOK_SNAPSHOT payload with the ext trailer:
// header 24B + levelCount*16B + orderCount*48B + 16B ext hdr + N*60B.
func walBookSnapshot(inst uint32, bookSeq uint64,
	orders []recovery.SnapshotOrder, exts []recovery.SnapshotOrderExt) []byte {
	p := make([]byte, 24)
	binary.LittleEndian.PutUint32(p[0:4], inst)
	binary.LittleEndian.PutUint32(p[4:8], 0) // levelCount 0 — ext carries price
	binary.LittleEndian.PutUint64(p[8:16], uint64(len(orders)))
	binary.LittleEndian.PutUint64(p[16:24], bookSeq)
	for _, o := range orders {
		r := make([]byte, 48)
		binary.LittleEndian.PutUint64(r[0:8], o.OrderID)
		binary.LittleEndian.PutUint64(r[8:16], o.AccountID)
		binary.LittleEndian.PutUint64(r[16:24], uint64(o.QtyUnits))
		binary.LittleEndian.PutUint64(r[24:32], uint64(o.VisibleQtyUnits))
		p = append(p, r...)
	}
	x := make([]byte, 16)
	binary.LittleEndian.PutUint32(x[0:4], recovery.SnapExtMagic)
	binary.LittleEndian.PutUint16(x[4:6], recovery.SnapExtVersion)
	binary.LittleEndian.PutUint64(x[8:16], uint64(len(exts)))
	p = append(p, x...)
	for _, e := range exts {
		r := make([]byte, 60)
		binary.LittleEndian.PutUint64(r[0:8], e.OrderID)
		binary.LittleEndian.PutUint64(r[8:16], uint64(e.QtyUnits))
		binary.LittleEndian.PutUint64(r[16:24], uint64(e.FilledQty))
		binary.LittleEndian.PutUint64(r[24:32], uint64(e.PriceTicks))
		r[52], r[53], r[54] = e.Type, e.Side, e.Flags
		p = append(p, r...)
	}
	return p
}

// writeSegment emits {dir}/{name}.wal = FileHeader(shard)+entries.
func writeSegment(t *testing.T, dir, name string, shard uint16,
	seqStart uint64, entries ...[]byte) {
	t.Helper()
	out := recovery.FileHeader(shard)
	for i, e := range entries {
		typ := recovery.EventType(e[0])
		out = append(out, recovery.EncodeEntry(
			seqStart+uint64(i), 1_700_000_000_000_000_000+uint64(i), typ, e[1:])...)
	}
	if err := os.WriteFile(filepath.Join(dir, name), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// typed payload prefix helper (mirrors wal_test.go convention).
func tp(t recovery.EventType, body []byte) []byte {
	return append([]byte{byte(t)}, body...)
}

func l3ReaderDeps(dir string, inst uint32, shard int) L3SnapshotDeps {
	return L3SnapshotDeps{
		WalDirs:      func(string) []string { return []string{dir} },
		InstrumentID: func(s string) (uint32, bool) { return inst, true },
		Shard:        func(s string) (int, bool) { return shard, true },
	}
}

func TestL3SnapshotReader_RebuildsRestingBook(t *testing.T) {
	dir := t.TempDir()
	// seq 1: ADD buy 100 @1.10 (hidden bit4 set)
	// seq 2: ADD sell 200 @1.20
	// seq 3: MODIFY 100 → qty 7 @1.11
	// seq 4: TRADE partially fills sell 200 (taker buy 999 not resting)
	// seq 5: CANCEL 100
	// seq 6: ADD sell 300 @1.30 — different instrument (excluded)
	writeSegment(t, dir, "0.wal", 2, 1,
		tp(recovery.EvOrderNew, walOrderNew(100, 7, 1, 0, 1<<4, 110_000_000, 10_000_000)),
		tp(recovery.EvOrderNew, walOrderNew(200, 8, 1, 1, 0, 120_000_000, 5_000_000)),
		tp(recovery.EvOrderModify, walOrderModify(100, 111_000_000, 7_000_000)),
		tp(recovery.EvTrade, walTrade(1, 999, 200, 1, 120_000_000, 2_000_000)),
		tp(recovery.EvOrderCancel, walOrderCancel(100, 7)),
		tp(recovery.EvOrderNew, walOrderNew(300, 9, 2, 1, 0, 130_000_000, 1_000_000)),
	)
	r := NewL3SnapshotReader(l3ReaderDeps(dir, 1, 2))
	page, err := r.Snapshot(context.Background(), "EUR/USD", 0, 100)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if page.Snapshot.WalSeq != 6 {
		t.Fatalf("wal marker = %d, want 6", page.Snapshot.WalSeq)
	}
	if page.NextCursor != 0 || page.Snapshot.Count != 1 {
		t.Fatalf("page = %+v", page)
	}
	o := page.Snapshot.Orders[0]
	// Sell 200: orig 0.05, filled 0.02 → remaining 0.03 (units are
	// 1e8-scaled); hidden flag NOT set.
	if o.OrderID != 200 || o.Side != "SELL" || o.Hidden {
		t.Fatalf("order = %+v", o)
	}
	if o.Qty != "0.03" || o.Price != "1.2" {
		t.Fatalf("qty/price = %q/%q", o.Qty, o.Price)
	}
	if o.AccountHash != L3AccountHash(8) {
		t.Fatalf("account_hash = %d want %d", o.AccountHash, L3AccountHash(8))
	}
}

func TestL3SnapshotReader_SnapshotPlusTailReplay(t *testing.T) {
	dir := t.TempDir()
	// Segment 0.wal: seqs 1-3 incl. BOOK_SNAPSHOT at seq 2.
	writeSegment(t, dir, "0.wal", 0, 1,
		tp(recovery.EvOrderNew, walOrderNew(100, 7, 1, 0, 0, 110_000_000, 10_000_000)),
		tp(recovery.EvBookSnapshot, walBookSnapshot(1, 500,
			[]recovery.SnapshotOrder{
				{OrderID: 100, AccountID: 7, QtyUnits: 8_000_000},
				{OrderID: 101, AccountID: 8, QtyUnits: 4_000_000},
			},
			[]recovery.SnapshotOrderExt{
				{OrderID: 100, QtyUnits: 10_000_000, FilledQty: 2_000_000,
					PriceTicks: 110_000_000, Side: 0},
				{OrderID: 101, QtyUnits: 4_000_000, PriceTicks: 121_000_000, Side: 1},
			})),
		tp(recovery.EvOrderNew, walOrderNew(102, 9, 1, 1, 0, 122_000_000, 1_000_000)),
	)
	// Segment 1.wal: cancel of snapshot order 101.
	writeSegment(t, dir, "1.wal", 0, 4,
		tp(recovery.EvOrderCancel, walOrderCancel(101, 8)),
	)
	r := NewL3SnapshotReader(l3ReaderDeps(dir, 1, 0))
	page, err := r.Snapshot(context.Background(), "EUR/USD", 0, 100)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Book at marker: order 100 (orig 10, filled 2 → 8 remaining),
	// order 101 cancelled post-snapshot, order 102 added post-snapshot.
	if page.Snapshot.Count != 2 {
		t.Fatalf("count = %d orders=%+v", page.Snapshot.Count, page.Snapshot.Orders)
	}
	if page.Snapshot.Orders[0].OrderID != 100 ||
		page.Snapshot.Orders[0].Qty != "0.08" ||
		page.Snapshot.Orders[0].Price != "1.1" {
		t.Fatalf("snap order 100 = %+v", page.Snapshot.Orders[0])
	}
	if page.Snapshot.Orders[1].OrderID != 102 {
		t.Fatalf("snap order 102 = %+v", page.Snapshot.Orders[1])
	}
	if page.Snapshot.WalSeq != 4 {
		t.Fatalf("marker = %d want 4", page.Snapshot.WalSeq)
	}
}

func TestL3SnapshotReader_CursorPagination(t *testing.T) {
	dir := t.TempDir()
	var ents [][]byte
	for i := uint64(1); i <= 5; i++ {
		ents = append(ents, tp(recovery.EvOrderNew,
			walOrderNew(100+i, 7, 1, 0, 0, 110_000_000, 1_000_000)))
	}
	writeSegment(t, dir, "0.wal", 0, 1, ents...)
	r := NewL3SnapshotReader(l3ReaderDeps(dir, 1, 0))

	p1, err := r.Snapshot(context.Background(), "EUR/USD", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Snapshot.Count != 2 || p1.NextCursor != 102 {
		t.Fatalf("page1 = %+v cursor=%d", p1.Snapshot.Orders, p1.NextCursor)
	}
	p2, err := r.Snapshot(context.Background(), "EUR/USD", p1.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Snapshot.Orders[0].OrderID != 103 || p2.NextCursor != 104 {
		t.Fatalf("page2 = %+v cursor=%d", p2.Snapshot.Orders, p2.NextCursor)
	}
	p3, err := r.Snapshot(context.Background(), "EUR/USD", p2.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if p3.Snapshot.Count != 1 || p3.NextCursor != 0 {
		t.Fatalf("page3 = %+v cursor=%d", p3.Snapshot.Orders, p3.NextCursor)
	}
}

func TestL3SnapshotReader_TooLargeCeiling(t *testing.T) {
	dir := t.TempDir()
	var ents [][]byte
	for i := uint64(1); i <= 3; i++ {
		ents = append(ents, tp(recovery.EvOrderNew,
			walOrderNew(100+i, 7, 1, 0, 0, 110_000_000, 1_000_000)))
	}
	writeSegment(t, dir, "0.wal", 0, 1, ents...)
	d := l3ReaderDeps(dir, 1, 0)
	d.MaxOrders = 2
	r := NewL3SnapshotReader(d)
	if _, err := r.Snapshot(context.Background(), "EUR/USD", 0, 100); !errors.Is(err, ErrL3SnapshotTooLarge) {
		t.Fatalf("err = %v, want ErrL3SnapshotTooLarge", err)
	}
}

func TestL3SnapshotReader_StalenessGate(t *testing.T) {
	dir := t.TempDir()
	writeSegment(t, dir, "0.wal", 0, 1,
		tp(recovery.EvOrderNew, walOrderNew(1, 7, 1, 0, 0, 1, 1)))
	base := time.Unix(1_800_000_000, 0)
	calls := 0
	d := l3ReaderDeps(dir, 1, 0)
	d.Now = func() time.Time {
		calls++
		if calls == 1 {
			return base // marker capture
		}
		return base.Add(600 * time.Millisecond) // serve-time check
	}
	d.MaxStale = 500 * time.Millisecond
	r := NewL3SnapshotReader(d)
	if _, err := r.Snapshot(context.Background(), "EUR/USD", 0, 10); !errors.Is(err, ErrL3SnapshotStale) {
		t.Fatalf("err = %v, want ErrL3SnapshotStale", err)
	}
}

func TestL3SnapshotReader_FailClosed(t *testing.T) {
	dir := t.TempDir()
	r := NewL3SnapshotReader(l3ReaderDeps(dir, 1, 0))
	if _, err := r.Snapshot(context.Background(), "EUR/USD", 0, 10); !errors.Is(err, ErrL3WALUnavailable) {
		t.Fatalf("empty wal err = %v", err)
	}
	d := l3ReaderDeps(dir, 1, 0)
	d.InstrumentID = func(string) (uint32, bool) { return 0, false }
	r = NewL3SnapshotReader(d)
	if _, err := r.Snapshot(context.Background(), "ZZZ/ZZZ", 0, 10); !errors.Is(err, ErrL3UnknownSymbol) {
		t.Fatalf("unknown symbol err = %v", err)
	}
}

func TestL3AccountHash_MatchesEngineConvention(t *testing.T) {
	// exch::L3Publisher::hash_account = FNV-1a-64 over
	// "exc.l3.account.v1" || account_id little-endian. Pin the contract
	// with an independent literal implementation so a drift in
	// L3AccountHash (or the salt) trips here, not in production.
	const offset, prime = 14695981039346656037, 1099511628211
	ref := func(id uint64) uint64 {
		h := uint64(offset)
		mix := func(b byte) { h ^= uint64(b); h *= uint64(prime) }
		for _, c := range []byte("exc.l3.account.v1") {
			mix(c)
		}
		for i := 0; i < 8; i++ {
			mix(byte(id >> (8 * i)))
		}
		return h
	}
	for _, id := range []uint64{0, 1, 7, 42, 1 << 62, ^uint64(0)} {
		if got := L3AccountHash(id); got != ref(id) {
			t.Fatalf("L3AccountHash(%d) = %d, want %d", id, got, ref(id))
		}
	}
	if l3AccountHashSalt != "exc.l3.account.v1" {
		t.Fatalf("salt literal drifted: %q", l3AccountHashSalt)
	}
}

func TestL3SnapshotReader_HiddenFlagFromWal(t *testing.T) {
	dir := t.TempDir()
	writeSegment(t, dir, "0.wal", 0, 1,
		tp(recovery.EvOrderNew, walOrderNew(1, 7, 1, 0, 1<<4, 110_000_000, 1_000_000)),
		tp(recovery.EvOrderNew, walOrderNew(2, 7, 1, 0, 0, 111_000_000, 1_000_000)),
	)
	r := NewL3SnapshotReader(l3ReaderDeps(dir, 1, 0))
	page, err := r.Snapshot(context.Background(), "EUR/USD", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Snapshot.Orders[0].Hidden || page.Snapshot.Orders[1].Hidden {
		t.Fatalf("hidden flags = %+v", page.Snapshot.Orders)
	}
}
