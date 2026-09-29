package orders

// Regression test for the Phase-13.5 Task 13.5.3.9 pen-test finding
// (tests/pentest findings register F-IPC-1): Consumer.handle decoded
// inbound ring frames via ipc.DecodeEvent with NO panic guard, unlike
// every sibling decode site (marketdata/events.go,
// settlement/balance_consumer.go, bridge/bridge.go) — one corrupt shm
// slot panicked the read-model drain goroutine. handle() now recovers
// and counts malformed frames; this test is the canary.

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc/wire"
)

// TestConsumerHandle_MalformedFrame fires hostile flatbuffers at the
// consumer handler. Pre-fix these panicked inside the generated
// accessors; post-fix they increment Malformed() and return.
func TestConsumerHandle_MalformedFrame(t *testing.T) {
	c := NewConsumer(&ShmSubmitter{}, newFakeStore(), newPendingConfirms())
	inputs := [][]byte{
		{0xFF, 0xFF, 0xFF, 0xFF, 0x10, 0x00, 0x00, 0x00}, // uoffset=-1
		{0x08, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF}, // vtable OOB
		make([]byte, 0),                      // empty
		make([]byte, 4),                      // header only
		{0x04, 0x00, 0x00, 0x00, 0x04, 0x00}, // truncated table
	}
	for i, payload := range inputs {
		c.handle(payload) // must not panic
		_ = i
	}
	if c.Malformed() != int64(len(inputs)) {
		t.Fatalf("malformed counter = %d, want %d", c.Malformed(), len(inputs))
	}
}

// Control: a validly-encoded OrderCancel still decodes and applies.
func TestConsumerHandle_ValidCancelStillApplies(t *testing.T) {
	st := newFakeStore()
	st.orders[424242] = &Order{ID: 424242, AccountID: 77, Status: "ACTIVE"}
	c := NewConsumer(&ShmSubmitter{}, st, newPendingConfirms())

	b := flatbuffers.NewBuilder(128)
	wire.OrderCancelStart(b)
	wire.OrderCancelAddOrderId(b, 424242)
	wire.OrderCancelAddAccountId(b, 77)
	oc := wire.OrderCancelEnd(b)
	wire.EventStart(b)
	wire.EventAddSeq(b, 1)
	wire.EventAddTs(b, 123)
	wire.EventAddTypeType(b, wire.EventTypeOrderCancel)
	wire.EventAddType(b, oc)
	b.Finish(wire.EventEnd(b))

	c.handle(b.FinishedBytes())
	if c.Malformed() != 0 {
		t.Fatalf("valid frame counted malformed: %d", c.Malformed())
	}
	got := st.orders[424242]
	if got == nil || !isTerminal(got.Status) && got.Status != "CANCELLED" {
		t.Fatalf("valid cancel not applied: %+v", got)
	}
}
