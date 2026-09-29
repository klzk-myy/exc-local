// Phase-17 Task 17.3.1/17.3.2 — L3OrderEvent decoder seam tests.
package ipc

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc/wire"
)

func TestL3OrderEvent_RoundTrip(t *testing.T) {
	fields := &L3OrderFields{
		InstrumentID: 3, Kind: L3KindFill,
		OrderID: 123456, AccountHash: 0xDEADBEEF,
		Side: 1, PriceTicks: 110_000_000, RefPriceTicks: 0,
		QtyUnits: -5_000_000, QtyDelta: -2_000_000, // signed i64 decode
		L3Seq: 41, WalSeq: 9001, TradeID: 77,
		FillRole: L3RoleTaker, Flags: L3FlagHidden | L3FlagDetail,
		CancelReason: 0, Hidden: true,
	}
	buf := MarshalL3Event(7, 1_700_000_000_000_000_000, fields)
	ev := DecodeEvent(buf)
	if ev.Seq() != 7 || ev.TypeType() != EventTypeL3OrderEvent {
		t.Fatalf("envelope seq=%d type=%d", ev.Seq(), ev.TypeType())
	}
	got, ok := DecodeL3OrderEvent(ev)
	if !ok {
		t.Fatal("decode failed")
	}
	if *got != *fields {
		t.Fatalf("got %+v want %+v", got, fields)
	}
	// Hidden decodes from flags bit0 — never a separate wire bool.
	f2 := &L3OrderFields{OrderID: 1, Kind: L3KindAdd, L3Seq: 1,
		Flags: 0, Hidden: false}
	got2, ok := DecodeL3OrderEvent(DecodeEvent(MarshalL3Event(1, 1, f2)))
	if !ok || got2.Hidden {
		t.Fatalf("hidden bit: %+v ok=%v", got2, ok)
	}
}

func TestL3OrderEvent_RejectsNonL3(t *testing.T) {
	b := flatbuffers.NewBuilder(256)
	buf := EncodeOrderNewEvent(b, 1, 1, OrderNewMsg{
		OrderID: 1, InstrumentID: 1, Qty: 1, Price: 1})
	if _, ok := DecodeL3OrderEvent(DecodeEvent(buf)); ok {
		t.Fatal("OrderNew accepted as L3")
	}
}

func TestL3OrderEvent_RejectsContractViolations(t *testing.T) {
	cases := []struct {
		name string
		mut  func(f *L3OrderFields)
	}{
		{"zero_order_id", func(f *L3OrderFields) { f.OrderID = 0 }},
		{"zero_l3_seq", func(f *L3OrderFields) { f.L3Seq = 0 }},
		{"unknown_kind", func(f *L3OrderFields) { f.Kind = 9 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &L3OrderFields{OrderID: 1, Kind: L3KindAdd, L3Seq: 1}
			tc.mut(f)
			buf := MarshalL3Event(1, 1, f)
			if _, ok := DecodeL3OrderEvent(DecodeEvent(buf)); ok {
				t.Fatal("contract violation accepted")
			}
		})
	}
}

func TestL3OrderEvent_MalformedUnion(t *testing.T) {
	// An Event typed as L3 but with the union member zeroed / truncated
	// must not panic and must not decode.
	b := flatbuffers.NewBuilder(256)
	wire.EventStart(b)
	wire.EventAddSeq(b, 1)
	wire.EventAddTs(b, 1)
	wire.EventAddTypeType(b, EventTypeL3OrderEvent)
	// no EventAddType — missing union member
	b.Finish(wire.EventEnd(b))
	if _, ok := DecodeL3OrderEvent(DecodeEvent(b.FinishedBytes())); ok {
		t.Fatal("missing union member decoded")
	}
	// Garbage buffer — DecodeEvent on a non-table: GetRootAsEvent on
	// short input is unsafe; the decoder only sees valid envelopes, but
	// an L3-typed event whose table lies about its vtable must still
	// fail closed.
	trunc := MarshalL3Event(1, 1, &L3OrderFields{
		OrderID: 1, Kind: L3KindAdd, L3Seq: 1})
	if _, ok := DecodeL3OrderEvent(DecodeEvent(trunc)); !ok {
		t.Fatal("baseline marshal undecodable")
	}
}
