package bridge

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
)

func testBridge(t *testing.T, res Resolver) *Bridge {
	t.Helper()
	b, err := New(Config{
		ShardID:           0,
		AeronURI:          "aeron:ipc?alias=orders_out",
		AeronStreamID:     1002,
		BufferSize:        8,
		PublishTimeout:    testTimeout,
		ReconnectWait:     testBackoff,
		HeartbeatInterval: testHeartbeat,
		OrderIndexSize:    16,
	}, &fakePublisher{}, res, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func encodeCancelEvent(seq uint64, orderID uint64) []byte {
	fb := flatbuffers.NewBuilder(128)
	wire.OrderCancelStart(fb)
	wire.OrderCancelAddOrderId(fb, orderID)
	wire.OrderCancelAddAccountId(fb, 1)
	oc := wire.OrderCancelEnd(fb)
	wire.EventStart(fb)
	wire.EventAddSeq(fb, seq)
	wire.EventAddTs(fb, 1)
	wire.EventAddTypeType(fb, wire.EventTypeOrderCancel)
	wire.EventAddType(fb, oc)
	fb.Finish(wire.EventEnd(fb))
	return append([]byte(nil), fb.FinishedBytes()...)
}

func encodeSnapshotEvent(seq uint64, instrumentID uint32) []byte {
	fb := flatbuffers.NewBuilder(128)
	wire.BookSnapshotStart(fb)
	wire.BookSnapshotAddInstrumentId(fb, instrumentID)
	wire.BookSnapshotAddSeq(fb, seq)
	bs := wire.BookSnapshotEnd(fb)
	wire.EventStart(fb)
	wire.EventAddSeq(fb, seq)
	wire.EventAddTs(fb, 1)
	wire.EventAddTypeType(fb, wire.EventTypeBookSnapshot)
	wire.EventAddType(fb, bs)
	fb.Finish(wire.EventEnd(fb))
	return append([]byte(nil), fb.FinishedBytes()...)
}

func encodeTimeTickEvent(seq uint64) []byte {
	fb := flatbuffers.NewBuilder(64)
	wire.TimeTickStart(fb)
	wire.TimeTickAddTickNs(fb, 42)
	tt := wire.TimeTickEnd(fb)
	wire.EventStart(fb)
	wire.EventAddSeq(fb, seq)
	wire.EventAddTs(fb, 1)
	wire.EventAddTypeType(fb, wire.EventTypeTimeTick)
	wire.EventAddType(fb, tt)
	fb.Finish(wire.EventEnd(fb))
	return append([]byte(nil), fb.FinishedBytes()...)
}

func newOrderEvent(b *flatbuffers.Builder, seq, orderID uint64, instrumentID uint32) []byte {
	b.Reset()
	return append([]byte(nil), ipc.EncodeOrderNewEvent(b, seq, 1, ipc.OrderNewMsg{
		OrderID: orderID, AccountID: 7, InstrumentID: instrumentID,
		Side: wire.SideBuy, Type: wire.OrderTypeLimit,
		Qty: 1, Price: 1, TIF: wire.TimeInForceGTC,
	})...)
}

func newFillEvent(b *flatbuffers.Builder, seq, buyID, sellID uint64) []byte {
	b.Reset()
	return append([]byte(nil),
		ipc.EncodeTradeFillEvent(b, seq, 1, 900, buyID, sellID, 1, 1, int64(seq))...)
}

func TestStreamMapping(t *testing.T) {
	cases := map[wire.EventType][]string{
		wire.EventTypeTradeFill:    {"trades", "settlements"},
		wire.EventTypeOrderNew:     {"compliance", "analytics"},
		wire.EventTypeOrderCancel:  {"compliance", "analytics"},
		wire.EventTypeOrderAmend:   {"compliance", "analytics"},
		wire.EventTypeBookSnapshot: {"analytics"},
		wire.EventTypeTimeTick:     nil,
		wire.EventTypeNONE:         nil,
	}
	for et, want := range cases {
		got := streamsForEvent(et)
		if len(got) != len(want) {
			t.Fatalf("%s: streams %v want %v", et, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: streams %v want %v", et, got, want)
			}
		}
	}
}

func TestRouteSubjects(t *testing.T) {
	res := MapResolver{3: "EUR-USD"}
	b := testBridge(t, res)
	fb := flatbuffers.NewBuilder(256)

	b.HandleFragment(newOrderEvent(fb, 1, 1001, 3))
	e, _, ok := b.buf.Head()
	if !ok {
		t.Fatal("order event was not buffered")
	}
	want := []string{"compliance.0.EUR-USD", "analytics.0.EUR-USD"}
	if len(e.subjects) != len(want) {
		t.Fatalf("subjects %v want %v", e.subjects, want)
	}
	for i := range want {
		if e.subjects[i] != want[i] {
			t.Fatalf("subjects %v want %v", e.subjects, want)
		}
	}
	if e.msgID != "s0-1" {
		t.Fatalf("msgID %q want s0-1", e.msgID)
	}
	b.buf.Pop()

	// Fill for known order ids routes on the order index.
	b.HandleFragment(newFillEvent(fb, 2, 1001, 2002))
	e, _, _ = b.buf.Head()
	want = []string{"trades.0.EUR-USD", "settlements.0.EUR-USD"}
	for i := range want {
		if e.subjects[i] != want[i] {
			t.Fatalf("fill subjects %v want %v", e.subjects, want)
		}
	}
	b.buf.Pop()

	// Cancel of a known order.
	b.HandleFragment(encodeCancelEvent(3, 1001))
	e, _, _ = b.buf.Head()
	if e.subjects[0] != "compliance.0.EUR-USD" {
		t.Fatalf("cancel subjects %v", e.subjects)
	}
	b.buf.Pop()

	// Book snapshot by instrument id.
	b.HandleFragment(encodeSnapshotEvent(4, 3))
	e, _, _ = b.buf.Head()
	if e.subjects[0] != "analytics.0.EUR-USD" {
		t.Fatalf("snapshot subjects %v", e.subjects)
	}
	b.buf.Pop()
}

func TestRouteFallbacks(t *testing.T) {
	res := MapResolver{3: "EUR-USD"}
	b := testBridge(t, res)
	fb := flatbuffers.NewBuilder(256)

	// Unconfigured instrument id -> instr-<id> token (keeps per-instrument
	// ordering) and counts as unrouted.
	b.HandleFragment(newOrderEvent(fb, 1, 1001, 99))
	e, _, _ := b.buf.Head()
	if e.subjects[0] != "compliance.0.instr-99" {
		t.Fatalf("subjects %v", e.subjects)
	}
	b.buf.Pop()

	// Fill with no order context -> UNKNOWN ordering domain.
	b.HandleFragment(newFillEvent(fb, 2, 4242, 4343))
	e, _, _ = b.buf.Head()
	if e.subjects[0] != "trades.0.UNKNOWN" || e.subjects[1] != "settlements.0.UNKNOWN" {
		t.Fatalf("unrouted fill subjects %v", e.subjects)
	}
	if b.Snapshot().Unrouted != 2 {
		t.Fatalf("unrouted=%d want 2", b.Snapshot().Unrouted)
	}
}

func TestRouteSkipsControlEvents(t *testing.T) {
	b := testBridge(t, MapResolver{})
	b.HandleFragment(encodeTimeTickEvent(1))
	if b.buf.Len() != 0 {
		t.Fatal("TimeTick must not be republished")
	}
}

func TestOrderIndexBounded(t *testing.T) {
	o := newOrderIndex(4)
	for i := uint64(0); i < 10; i++ {
		o.put(i, "SYM")
	}
	if o.len() != 4 {
		t.Fatalf("orderIndex len=%d want 4", o.len())
	}
	for i := uint64(0); i < 6; i++ {
		if _, ok := o.get(i); ok {
			t.Fatalf("order %d should have been evicted", i)
		}
	}
	for i := uint64(6); i < 10; i++ {
		if s, ok := o.get(i); !ok || s != "SYM" {
			t.Fatalf("order %d missing", i)
		}
	}
}
