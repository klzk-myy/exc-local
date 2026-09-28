package recovery

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func buildSegment(shard uint16, seqStart uint64, tsBase uint64,
	payloads ...[]byte) []byte {
	out := FileHeader(shard)
	for i, p := range payloads {
		typ := EvOrderNew
		// caller tags type via first byte convention for tests
		_ = typ
		out = append(out, EncodeEntry(seqStart+uint64(i), tsBase+uint64(i), EventType(p[0]), p[1:])...)
	}
	return out
}

func payload(t EventType, body []byte) []byte {
	return append([]byte{byte(t)}, body...)
}

func orderNewBytes(orderID, acctID uint64, inst uint32, side, typ, tif uint8,
	price, qty int64) []byte {
	p := make([]byte, 80)
	binary.LittleEndian.PutUint64(p[0:8], orderID)
	binary.LittleEndian.PutUint64(p[8:16], acctID)
	binary.LittleEndian.PutUint32(p[16:20], inst)
	p[20], p[21], p[22] = side, typ, tif
	binary.LittleEndian.PutUint64(p[24:32], uint64(price))
	binary.LittleEndian.PutUint64(p[32:40], uint64(qty))
	binary.LittleEndian.PutUint64(p[40:48], uint64(qty)) // visible
	return payload(EvOrderNew, p)
}

func tradeBytes(tradeID, buyID, sellID uint64, inst uint32, price, qty int64) []byte {
	p := make([]byte, 48)
	binary.LittleEndian.PutUint64(p[0:8], tradeID)
	binary.LittleEndian.PutUint64(p[8:16], buyID)
	binary.LittleEndian.PutUint64(p[16:24], sellID)
	binary.LittleEndian.PutUint32(p[24:28], inst)
	binary.LittleEndian.PutUint64(p[32:40], uint64(price))
	binary.LittleEndian.PutUint64(p[40:48], uint64(qty))
	return payload(EvTrade, p)
}

func tickBytes(ns uint64) []byte {
	p := make([]byte, 16)
	binary.LittleEndian.PutUint64(p[0:8], ns)
	return payload(EvTimeTick, p)
}

func cancelBytes(orderID, acct uint64) []byte {
	p := make([]byte, 24)
	binary.LittleEndian.PutUint64(p[0:8], orderID)
	binary.LittleEndian.PutUint64(p[8:16], acct)
	return payload(EvOrderCancel, p)
}

func TestScanSegmentRoundTrip(t *testing.T) {
	seg := buildSegment(3, 1000, 1700000000000000000,
		orderNewBytes(1, 101, 1, 1, 1, 0, 110000000, 5000000),
		tickBytes(1700000001000000000),
		tradeBytes(7, 2, 1, 1, 110000000, 5000000))
	// Pad to 4KB boundary then another entry region? Pad must land on
	// boundary — append pad then leave zeros tail.
	padLen := int(alignUp4K(uint64(len(seg)))) - len(seg)
	seg = append(seg, EncodePad(padLen)...)
	seg = append(seg, make([]byte, 8192)...) // preallocated zero tail

	res, entries, err := ScanSegment(seg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Corrupt || !res.HeaderOK {
		t.Fatalf("scan: %+v", res)
	}
	if res.Shard != 3 || res.Entries != 3 || res.LastSeq != 1002 {
		t.Fatalf("scan: %+v", res)
	}
	if res.Pads != 1 {
		t.Fatalf("expected 1 pad, got %d", res.Pads)
	}
	if len(entries) != 3 || entries[0].Seq != 1000 {
		t.Fatalf("entries: %+v", entries)
	}
	o, err := DecodeOrderNew(entries[0].Payload)
	if err != nil || o.OrderID != 1 || o.AccountID != 101 || o.PriceTicks != 110000000 {
		t.Fatalf("order decode: %+v %v", o, err)
	}
	tr, err := DecodeTrade(entries[2].Payload)
	if err != nil || tr.TradeID != 7 || tr.SellOrderID != 1 {
		t.Fatalf("trade decode: %+v %v", tr, err)
	}
}

func TestScanSegmentCorrupt(t *testing.T) {
	seg := buildSegment(0, 0, 1,
		orderNewBytes(1, 1, 1, 0, 1, 0, 100, 10))
	seg[len(seg)-6] ^= 0xFF // corrupt payload near end
	res, _, err := ScanSegment(seg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Corrupt {
		t.Fatal("expected corrupt detection")
	}
}

func TestScanSegmentBadHeader(t *testing.T) {
	_, _, err := ScanSegment([]byte{1, 2, 3})
	if err != ErrShortFile {
		t.Fatalf("want ErrShortFile, got %v", err)
	}
	bad := make([]byte, 64)
	if _, _, err := ScanSegment(bad); err != ErrBadHeader {
		t.Fatalf("want ErrBadHeader, got %v", err)
	}
}

// TestScanRealSegment exercises the reader against the engine-written
// fixture in the repo (wal/3/0.wal) — proves CRC32C and framing match the
// C++ writer bit-for-bit.
func TestScanRealSegment(t *testing.T) {
	path := filepath.Join("..", "..", "..", "wal", "3", "0.wal")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("repo wal fixture absent: %v", err)
	}
	res, entries, err := ScanSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	if !res.HeaderOK || res.Shard != 3 {
		t.Fatalf("header: %+v", res)
	}
	if res.Corrupt {
		t.Fatalf("corrupt at %d on engine-written segment", res.CorruptOffset)
	}
	if res.Entries == 0 {
		t.Fatal("no entries decoded from live segment")
	}
	// seq contiguity across the segment
	for i := 1; i < len(entries); i++ {
		if entries[i].Seq != entries[i-1].Seq+1 {
			t.Fatalf("seq discontinuity at entry %d: %d -> %d",
				i, entries[i-1].Seq, entries[i].Seq)
		}
	}
	t.Logf("scanned %d entries, last_seq=%d, pads=%d", res.Entries, res.LastSeq, res.Pads)
}
