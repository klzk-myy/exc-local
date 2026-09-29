// codec_test.go — Task 18.3.8 AC: codec round-trips, forward
// compatibility, malformed input, inbound-only template discipline.
package fixsbe

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestNewOrderRoundTrip(t *testing.T) {
	in := NewOrder{
		ClOrdID:        SetClOrdID("cl-42"),
		AccountID:      77,
		InstrumentID:   12,
		Side:           SideSell,
		OrdType:        OrdTypeLimit,
		TimeInForce:    TIFGTC,
		Flags:          FlagPostOnly,
		Price:          1_2345_6789,
		Qty:            50_0000_0000,
		StopPrice:      1_2300_0000,
		DisplayQty:     10_0000_0000,
		ExpireTimeNs:   uint64(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano()),
		TransactTimeNs: 123456789,
	}
	frame := MarshalMessage(in)
	m, n, err := DecodeMessage(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n != len(frame) {
		t.Fatalf("consumed %d of %d", n, len(frame))
	}
	out, ok := m.(NewOrder)
	if !ok {
		t.Fatalf("type %T", m)
	}
	if out != in {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", in, out)
	}
}

func TestCancelReplaceRoundTrip(t *testing.T) {
	for _, m := range []Message{
		CancelOrder{ClOrdID: SetClOrdID("c1"), OrigClOrdID: SetClOrdID("o1"),
			OrderID: 99, AccountID: 5, InstrumentID: 7, Side: SideBuy,
			TransactTimeNs: 1},
		ReplaceOrder{ClOrdID: SetClOrdID("c2"), OrigClOrdID: SetClOrdID("o2"),
			OrderID: 98, AccountID: 5, InstrumentID: 7, Side: SideSell,
			TimeInForce: TIFIOC, Price: 5, Qty: 6, TransactTimeNs: 2},
	} {
		frame := MarshalMessage(m)
		got, n, err := DecodeMessage(frame)
		if err != nil {
			t.Fatalf("decode %T: %v", m, err)
		}
		if n != len(frame) {
			t.Fatalf("consumed %d of %d", n, len(frame))
		}
		if got != m {
			t.Fatalf("%T mismatch: %+v vs %+v", m, m, got)
		}
	}
}

// TestForwardCompatibility — a newer schema version appends fields;
// the v1 decoder must read the known prefix and skip the tail (the
// BlockLength contract, spec §24 #284).
func TestForwardCompatibility(t *testing.T) {
	frame := MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("fwd"), InstrumentID: 1, Side: SideBuy,
		OrdType: OrdTypeMarket, TimeInForce: TIFDay, Qty: 100,
	})
	// Forge a "v2" frame: blockLength + 16 appended bytes.
	extra := make([]byte, 16)
	for i := range extra {
		extra[i] = byte(i + 1)
	}
	var forged []byte
	forged = append(forged, frame[:headerSize]...)
	binary.LittleEndian.PutUint16(forged[0:2], blockLenNewOrder+16)
	forged = append(forged, frame[headerSize:]...)
	forged = append(forged, extra...)
	m, n, err := DecodeMessage(forged)
	if err != nil {
		t.Fatalf("forward-compat decode: %v", err)
	}
	if n != len(forged) {
		t.Fatalf("consumed %d of %d", n, len(forged))
	}
	no := m.(NewOrder)
	if no.ClOrdID.Str() != "fwd" || no.Qty != 100 {
		t.Fatalf("v1 fields corrupted: %+v", no)
	}
}

func TestMalformedInputs(t *testing.T) {
	good := MarshalMessage(NewOrder{ClOrdID: SetClOrdID("x"), Qty: 1})

	// Truncated header.
	if _, _, err := DecodeMessage(good[:4]); err == nil {
		t.Fatal("short header accepted")
	}
	// Truncated body.
	if _, _, err := DecodeMessage(good[:len(good)-3]); err == nil {
		t.Fatal("short body accepted")
	}
	// Wrong schema id.
	bad := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(bad[4:6], 1)
	if _, _, err := DecodeMessage(bad); err == nil {
		t.Fatal("wrong schema accepted")
	}
	// Unknown template id.
	unk := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(unk[2:4], 777)
	if _, _, err := DecodeMessage(unk); err == nil {
		t.Fatal("unknown template accepted")
	}
	// BlockLength shorter than v1 layout.
	short := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(short[0:2], 32)
	if _, _, err := DecodeMessage(short[:headerSize+32]); err == nil {
		t.Fatal("undersized block accepted")
	}
}

// TestInboundOnly — outbound templates on the inbound wire are a
// protocol violation (fail closed).
func TestInboundOnly(t *testing.T) {
	for _, m := range []Message{ExecutionReport{}, News{}, NegotiationResponse{}} {
		frame := MarshalMessage(m)
		if _, _, err := DecodeInbound(frame); err == nil {
			t.Fatalf("outbound template %d accepted inbound", m.TemplateID())
		}
	}
}

func TestNegotiateRoundTrip(t *testing.T) {
	var n Negotiate
	n.SchemaID = SchemaIDOrderEntry
	n.SchemaVersion = 1
	n.ResponseCodec = CodecTagValue
	for i := range n.SessionPubKey {
		n.SessionPubKey[i] = byte(i)
	}
	for i := range n.Signature {
		n.Signature[i] = byte(63 - i)
	}
	m, _, err := DecodeInbound(MarshalMessage(n))
	if err != nil {
		t.Fatalf("decode negotiate: %v", err)
	}
	got := m.(Negotiate)
	if got != n {
		t.Fatalf("negotiate mismatch")
	}
}

func TestNewsRoundTrip(t *testing.T) {
	var n News
	copy(n.Headline[:], "MAINTENANCE")
	copy(n.Body[:], "drain to gw2")
	n.Urgency = NewsUrgencyFlash
	n.NewsID = 9
	n.EventTimeNs = 42
	m, _, err := DecodeMessage(MarshalMessage(n))
	if err != nil {
		t.Fatalf("decode news: %v", err)
	}
	got := m.(News)
	if got != n {
		t.Fatalf("news mismatch")
	}
}
