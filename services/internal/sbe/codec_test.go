// Codec tests: golden byte vectors, round-trips, forward-compatible
// decoding, and unknown enum/template sentinels (Task 6.3.18 item 4).
package sbe

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad golden hex: %v", err)
	}
	return b
}

// Golden vectors were computed by hand from the documented field order and
// verified against the decoder; changing the wire layout breaks them.

func TestBookUpdateGoldenVector(t *testing.T) {
	m := BookUpdate{
		InstrumentID: 0x01020304,
		Side:         SideAsk,
		Action:       BookActionUpdate,
		LevelCount:   0x05060708,
		PriceTicks:   0x1122334455667788,
		QtyLots:      0x2122232425262728,
		EventTimeNs:  0x3132333435363738,
	}
	want := mustHex(t,
		"2200020001000100"+ // header: blk=34 tpl=2 schema=1 ver=1
			"04030201"+ // instrumentID
			"0100"+ // side=ask, action=update
			"08070605"+ // levelCount
			"8877665544332211"+ // priceTicks
			"2827262524232221"+ // qtyLots
			"3837363534333231") // eventTimeNs
	got := MarshalMessage(m)
	if !bytes.Equal(got, want) {
		t.Fatalf("encode mismatch\n got %x\nwant %x", got, want)
	}
	dm, n, err := DecodeMessage(want)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n != len(want) {
		t.Fatalf("consumed %d, want %d", n, len(want))
	}
	if dm != Message(m) {
		t.Fatalf("round-trip mismatch: %+v", dm)
	}
}

func TestTradeGoldenVector(t *testing.T) {
	m := Trade{
		TradeID:       0x4142434445464748,
		InstrumentID:  0x51525354,
		AggressorSide: SideBid,
		PriceTicks:    0x6162636465666768,
		QtyLots:       0x7172737475767778,
		EventTimeNs:   -0x7E7D7C7B7A797878,
	}
	want := mustHex(t,
		"2600030001000100"+ // header: blk=38 tpl=3 schema=1 ver=1
			"4847464544434241"+ // tradeID
			"54535251"+ // instrumentID
			"00"+"00"+ // aggressor=bid, reserved
			"6867666564636261"+ // priceTicks
			"7877767574737271"+ // qtyLots
			"8887868584838281") // eventTimeNs
	got := MarshalMessage(m)
	if !bytes.Equal(got, want) {
		t.Fatalf("encode mismatch\n got %x\nwant %x", got, want)
	}
	dm, _, err := DecodeMessage(want)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dm != Message(m) {
		t.Fatalf("round-trip mismatch: %+v", dm)
	}
}

func TestHeartbeatGoldenVector(t *testing.T) {
	m := Heartbeat{EventTimeNs: 0x3132333435363738}
	want := mustHex(t,
		"0800010001000100"+ // header: blk=8 tpl=1 schema=1 ver=1
			"3837363534333231")
	if got := MarshalMessage(m); !bytes.Equal(got, want) {
		t.Fatalf("encode mismatch\n got %x\nwant %x", got, want)
	}
}

func TestAllMessagesRoundTrip(t *testing.T) {
	msgs := []Message{
		Heartbeat{EventTimeNs: -42},
		BookUpdate{InstrumentID: 7, Side: SideBid, Action: BookActionDelete,
			LevelCount: 3, PriceTicks: -100, QtyLots: 0, EventTimeNs: 99},
		Trade{TradeID: 1, InstrumentID: 2, AggressorSide: SideAsk,
			PriceTicks: 1100050, QtyLots: 500, EventTimeNs: 7},
		SnapshotMarker{InstrumentID: 0, Type: SnapshotEnd, LastSeq: 42, EventTimeNs: 1},
		SecurityStatus{InstrumentID: 9, Status: StatusSuspended, EventTimeNs: 5},
		SecurityDefinition{InstrumentID: 9, Symbol: [8]byte{'E', 'U', 'R', 'U', 'S', 'D'},
			TickSizeTicks: 10, LotSizeLots: 1000, Status: StatusActive, EventTimeNs: 6},
	}
	for _, m := range msgs {
		enc := MarshalMessage(m)
		dm, n, err := DecodeMessage(enc)
		if err != nil {
			t.Fatalf("%T decode: %v", m, err)
		}
		if n != len(enc) {
			t.Fatalf("%T consumed %d of %d", m, n, len(enc))
		}
		if dm != m {
			t.Fatalf("%T round-trip mismatch\n got %+v\nwant %+v", m, dm, m)
		}
	}
}

func TestDecodeSkipsExtendedBlock_ForwardCompat(t *testing.T) {
	// Simulate a v2 peer that appended 4 bytes to the Trade block (spec §8.6:
	// backward-compatible field additions only).
	m := Trade{TradeID: 77, InstrumentID: 3, AggressorSide: SideAsk,
		PriceTicks: 100, QtyLots: 2, EventTimeNs: 9}
	enc := MarshalMessage(m)
	// Bump version to 2, blockLength 38→42, append 4 extra bytes.
	v2 := make([]byte, 0, len(enc)+4)
	v2 = append(v2, enc[:headerSize]...)
	binary.LittleEndian.PutUint16(v2[0:2], 42)
	binary.LittleEndian.PutUint16(v2[6:8], 2)
	v2 = append(v2, enc[headerSize:]...)
	v2 = append(v2, 0xDE, 0xAD, 0xBE, 0xEF)

	dm, n, err := DecodeMessage(v2)
	if err != nil {
		t.Fatalf("forward decode failed: %v", err)
	}
	if n != len(v2) {
		t.Fatalf("consumed %d, want %d", n, len(v2))
	}
	got, ok := dm.(Trade)
	if !ok {
		t.Fatalf("decoded %T, want Trade", dm)
	}
	if got != m {
		t.Fatalf("field mismatch: %+v", got)
	}
}

func TestDecodeUnknownTemplateSentinel(t *testing.T) {
	buf := make([]byte, headerSize+4)
	binary.LittleEndian.PutUint16(buf[0:2], 4)    // blockLength
	binary.LittleEndian.PutUint16(buf[2:4], 9999) // unknown template
	binary.LittleEndian.PutUint16(buf[4:6], 1)
	binary.LittleEndian.PutUint16(buf[6:8], 3)
	copy(buf[headerSize:], []byte{0xAA, 0xBB, 0xCC, 0xDD})
	m, n, err := DecodeMessage(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	u, ok := m.(UnknownMessage)
	if !ok {
		t.Fatalf("decoded %T, want UnknownMessage sentinel", m)
	}
	if u.TemplateID() != 9999 || !bytes.Equal(u.Body, []byte{0xAA, 0xBB, 0xCC, 0xDD}) {
		t.Fatalf("bad sentinel: %+v", u)
	}
	if n != len(buf) {
		t.Fatalf("consumed %d, want %d", n, len(buf))
	}
}

func TestDecodeUnknownEnumSentinel(t *testing.T) {
	m := BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate,
		LevelCount: 1, PriceTicks: 5, QtyLots: 5, EventTimeNs: 5}
	enc := MarshalMessage(m)
	enc[headerSize+4] = 0x09 // unknown side wire value
	dm, _, err := DecodeMessage(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	bu := dm.(BookUpdate)
	if bu.Side != SideUnknown {
		t.Fatalf("side=%v, want SideUnknown sentinel", bu.Side)
	}
}

func TestDecodeTruncated(t *testing.T) {
	if _, _, err := DecodeMessage([]byte{0x01, 0x02}); err == nil {
		t.Fatal("expected truncation error")
	}
	enc := MarshalMessage(Heartbeat{EventTimeNs: 1})
	if _, _, err := DecodeMessage(enc[:len(enc)-1]); err == nil {
		t.Fatal("expected truncation error")
	}
}
