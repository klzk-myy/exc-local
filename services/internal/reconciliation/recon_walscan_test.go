package reconciliation

// recon_walscan_test.go — WAL replay over real segment bytes built with
// the recovery package's own encoder (spec §3.4 wire format).

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"exchange/internal/recovery"
)

// payload encoders for the packed WalEntry.hpp layouts (little-endian).
func orderNewPayload(orderID, instr uint64, qty int64) []byte {
	p := make([]byte, 80)
	binary.LittleEndian.PutUint64(p[0:8], orderID)
	binary.LittleEndian.PutUint32(p[16:20], uint32(instr))
	binary.LittleEndian.PutUint64(p[32:40], uint64(qty))
	return p
}
func orderCancelPayload(orderID uint64) []byte {
	p := make([]byte, 24)
	binary.LittleEndian.PutUint64(p[0:8], orderID)
	return p
}
func orderModifyPayload(orderID uint64, newQty int64) []byte {
	p := make([]byte, 40)
	binary.LittleEndian.PutUint64(p[0:8], orderID)
	binary.LittleEndian.PutUint64(p[16:24], uint64(newQty))
	return p
}
func tradePayload(tradeID, buyID, sellID, instr uint64, qty int64) []byte {
	p := make([]byte, 48)
	binary.LittleEndian.PutUint64(p[0:8], tradeID)
	binary.LittleEndian.PutUint64(p[8:16], buyID)
	binary.LittleEndian.PutUint64(p[16:24], sellID)
	binary.LittleEndian.PutUint32(p[24:28], uint32(instr))
	binary.LittleEndian.PutUint64(p[32:40], uint64(100000000)) // px ticks
	binary.LittleEndian.PutUint64(p[40:48], uint64(qty))
	return p
}

func writeSegment(t *testing.T, dir, name string, shard uint16, entries [][]byte) string {
	t.Helper()
	data := recovery.FileHeader(shard)
	for _, e := range entries {
		data = append(data, e...)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWalReplayRestingBook(t *testing.T) {
	dir := t.TempDir()
	// order 1 NEW 10u; trade fills 4 against it; order 2 NEW 3 then CANCEL;
	// order 3 NEW 7 then MODIFY to 5.
	writeSegment(t, dir, "0.wal", 0, [][]byte{
		recovery.EncodeEntry(0, 100, recovery.EvOrderNew, orderNewPayload(1, 9, 10)),
		recovery.EncodeEntry(1, 110, recovery.EvTrade, tradePayload(50, 1, 88, 9, 4)),
		recovery.EncodeEntry(2, 120, recovery.EvOrderNew, orderNewPayload(2, 9, 3)),
		recovery.EncodeEntry(3, 130, recovery.EvOrderCancel, orderCancelPayload(2)),
		recovery.EncodeEntry(4, 140, recovery.EvOrderNew, orderNewPayload(3, 9, 7)),
		recovery.EncodeEntry(5, 150, recovery.EvOrderModify, orderModifyPayload(3, 5)),
	})
	r, err := replayWal(context.Background(), []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Complete() {
		t.Fatalf("clean journal must be complete: %v", r.Problems())
	}
	resting := r.RestingOrders()
	if len(resting) != 2 {
		t.Fatalf("resting orders: %+v", resting)
	}
	// order 1: 10 − 4 = 6 remaining; order 3: modified to 5.
	if resting[0].OrderID != 1 || resting[0].RemainingQty != 6 {
		t.Fatalf("order1 remaining %+v", resting[0])
	}
	if resting[1].OrderID != 3 || resting[1].RemainingQty != 5 {
		t.Fatalf("order3 remaining %+v", resting[1])
	}
	if len(r.TradeMap()) != 1 || r.TradeMap()[50].QtyUnits != 4 {
		t.Fatalf("trade map %+v", r.TradeMap())
	}
}

func TestWalReplayCorruptTailIsIncomplete(t *testing.T) {
	dir := t.TempDir()
	data := recovery.FileHeader(0)
	data = append(data, recovery.EncodeEntry(0, 1, recovery.EvOrderNew, orderNewPayload(1, 9, 10))...)
	data = append(data, 0xDE, 0xAD, 0xBE, 0xEF) // torn tail
	if err := os.WriteFile(filepath.Join(dir, "0.wal"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := replayWal(context.Background(), []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete() {
		t.Fatal("torn tail must mark replay incomplete")
	}
	if len(r.Problems()) == 0 {
		t.Fatal("coverage problems must be listed")
	}
}

func TestWalReplayGapIsIncomplete(t *testing.T) {
	dir := t.TempDir()
	writeSegment(t, dir, "0.wal", 0, [][]byte{
		recovery.EncodeEntry(0, 1, recovery.EvOrderNew, orderNewPayload(1, 9, 10)),
	})
	writeSegment(t, dir, "5.wal", 0, [][]byte{
		recovery.EncodeEntry(5, 2, recovery.EvOrderNew, orderNewPayload(2, 9, 3)),
	})
	r, err := replayWal(context.Background(), []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete() {
		t.Fatal("seq gap must mark replay incomplete")
	}
}

func TestWalReplayNoDirs(t *testing.T) {
	_, err := replayWal(context.Background(), nil)
	if err == nil {
		t.Fatal("empty dir list must error")
	}
}
