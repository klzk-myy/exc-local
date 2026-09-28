// Packet-framing tests: golden datagram, round-trip, malformed rejection.
package sbe

import (
	"bytes"
	"testing"
)

func TestPacketGoldenVector(t *testing.T) {
	hb := MarshalMessage(Heartbeat{EventTimeNs: 0x3132333435363738})
	dg, err := EncodePacket(0x0A0B, 0x0C0D0E0F10111213, 0x0102030405060708, [][]byte{hb})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := mustHex(t,
		"5053"+ // magic "SP"
			"0B0A"+ // channelID
			"0100"+ // msgCount=1
			"0000"+ // flags
			"131211100F0E0D0C"+ // sessionID
			"0807060504030201"+ // seq
			"1000"+ // msgLen=16
			"08000100010001003837363534333231") // heartbeat
	if !bytes.Equal(dg, want) {
		t.Fatalf("datagram mismatch\n got %x\nwant %x", dg, want)
	}
	seq, ok := PeekPacketSeq(dg)
	if !ok || seq != 0x0102030405060708 {
		t.Fatalf("peek seq=%x ok=%v", seq, ok)
	}
	p, err := DecodePacket(dg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.ChannelID != 0x0A0B || p.SessionID != 0x0C0D0E0F10111213 || p.Seq != 0x0102030405060708 {
		t.Fatalf("header mismatch: %+v", p)
	}
	msgs, err := p.DecodeMessages()
	if err != nil {
		t.Fatalf("decode msgs: %v", err)
	}
	if len(msgs) != 1 || msgs[0].(Heartbeat).EventTimeNs != 0x3132333435363738 {
		t.Fatalf("messages mismatch: %+v", msgs)
	}
}

func TestPacketRoundTrip_MultiMessage(t *testing.T) {
	msgs := [][]byte{
		MarshalMessage(BookUpdate{InstrumentID: 1, Side: SideBid, Action: BookActionUpdate,
			LevelCount: 1, PriceTicks: 100, QtyLots: 10, EventTimeNs: 5}),
		MarshalMessage(Trade{TradeID: 9, InstrumentID: 1, AggressorSide: SideAsk,
			PriceTicks: 100, QtyLots: 10, EventTimeNs: 6}),
	}
	dg, err := EncodePacket(7, 42, 99, msgs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	p, err := DecodePacket(dg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p.Messages) != 2 {
		t.Fatalf("msg count %d, want 2", len(p.Messages))
	}
	if !bytes.Equal(p.Messages[0], msgs[0]) || !bytes.Equal(p.Messages[1], msgs[1]) {
		t.Fatal("payload mismatch")
	}
}

func TestPacketDecodeRejectsMalformed(t *testing.T) {
	hb := MarshalMessage(Heartbeat{EventTimeNs: 1})
	good, err := EncodePacket(1, 1, 1, [][]byte{hb})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":         {},
		"short_header":  good[:10],
		"bad_magic":     append([]byte{0xFF, 0xFF}, good[2:]...),
		"truncated_msg": good[:len(good)-3],
		"trailing":      append(append([]byte{}, good...), 0x00),
		"huge_count":    append([]byte{0x50, 0x53, 0x01, 0x00, 0xFF, 0xFF}, make([]byte, packetHeaderSize-6)...),
	}
	for name, dg := range cases {
		if _, err := DecodePacket(dg); err == nil {
			t.Fatalf("%s: expected decode error", name)
		}
	}
}
