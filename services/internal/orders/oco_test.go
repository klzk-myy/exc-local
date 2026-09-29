package orders

// Phase-14 Task 14.3.1 — OCO pair submission tests (spec §6.2/§6.5,
// §24 #47): wire ordering (OcoLink before both legs), transactional pair
// persistence + dedup semantics, idempotent replay, the sibling-race
// OCO_SIBLING_CANCEL_RACE surface (§23, HTTP 409), and reason-7 cancel
// propagation through the outbound consumer.

import (
	"context"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
)

// ocoPair builds a same-instrument two-leg request — the canonical
// "first to fill kills the other" shape (spec §6.5).
func ocoPair(coidA, coidB, priceA, priceB string) *SubmitOcoRequest {
	return &SubmitOcoRequest{
		Symbol: "EURUSD",
		Legs: [2]*SubmitRequest{
			{Symbol: "EURUSD", Side: SideBuy, OrderType: TypeLimit,
				TimeInForce: TIFGTC, ClientOrderID: coidA,
				Quantity: d("1000"), Price: d(priceA)},
			{Symbol: "EURUSD", Side: SideBuy, OrderType: TypeLimit,
				TimeInForce: TIFGTC, ClientOrderID: coidB,
				Quantity: d("1000"), Price: d(priceB)},
		},
	}
}

func decodeEventAt(t *testing.T, payload []byte) *wire.Event {
	t.Helper()
	ev := ipc.DecodeEvent(payload)
	if ev == nil {
		t.Fatalf("undecodable wire event")
	}
	return ev
}

// The wire contract: OcoLink precedes both legs' OrderNew on the shard
// ring, and both persisted rows share the pair's oco_group_id.
func TestSubmitOCO_LinkBeforeLegs(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)

	ack, err := svc.SubmitOCO(context.Background(), st.acct,
		ocoPair("oco-a", "oco-b", "1.04000", "1.06000"))
	if err != nil {
		t.Fatalf("SubmitOCO: %v", err)
	}
	if ack.Status != "ACCEPTED" || ack.OcoGroupID == 0 {
		t.Fatalf("ack: %+v", ack)
	}
	if len(sub.sent) != 3 {
		t.Fatalf("want 3 wire frames (link + 2 legs), got %d", len(sub.sent))
	}

	link := decodeEventAt(t, sub.sent[0])
	if link.TypeType() != wire.EventTypeOcoLink {
		t.Fatalf("frame[0] type %s, want OcoLink", link.TypeType())
	}
	var tab flatbuffers.Table
	if !link.Type(&tab) {
		t.Fatalf("link payload missing")
	}
	ol := &wire.OcoLink{}
	ol.Init(tab.Bytes, tab.Pos)
	if ol.LinkId() != uint64(ack.OcoGroupID) ||
		ol.OrderIdA() != uint64(ack.Legs[0].OrderID) ||
		ol.OrderIdB() != uint64(ack.Legs[1].OrderID) ||
		ol.AccountId() != uint64(st.acct.ID) ||
		ol.InstrumentId() != uint32(st.inst.ID) {
		t.Fatalf("link payload mismatch: %+v", ol)
	}
	for i, leg := range ack.Legs {
		ev := decodeEventAt(t, sub.sent[1+i])
		if ev.TypeType() != wire.EventTypeOrderNew {
			t.Fatalf("frame[%d] type %s, want OrderNew", 1+i, ev.TypeType())
		}
		if st.orders[leg.OrderID].Status != "ACTIVE" {
			t.Fatalf("leg %d not ACTIVE: %s", leg.OrderID,
				st.orders[leg.OrderID].Status)
		}
	}
	// Both rows share the pair's group id and carry the pair hash in dedup.
	for _, leg := range ack.Legs {
		o := st.orders[leg.OrderID]
		if o.OcoGroupID == nil || *o.OcoGroupID != ack.OcoGroupID {
			t.Fatalf("leg %d missing oco_group_id: %+v", leg.OrderID, o.OcoGroupID)
		}
		row := st.dedup[dedupKey{st.acct.ID, leg.ClientOrderID}]
		if row == nil || row.RequestHash == "" {
			t.Fatalf("leg %d dedup row missing hash", leg.OrderID)
		}
	}
}

// Pair-level dedup: a retry with the identical pair replays the stored
// acks without a second dispatch.
func TestSubmitOCO_ReplayIsIdempotent(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	ctx := context.Background()

	first, err := svc.SubmitOCO(ctx, st.acct, ocoPair("r-a", "r-b", "1.04", "1.06"))
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	second, err := svc.SubmitOCO(ctx, st.acct, ocoPair("r-a", "r-b", "1.04", "1.06"))
	if err != nil {
		t.Fatalf("replay submit: %v", err)
	}
	if len(sub.sent) != 3 {
		t.Fatalf("replay dispatched: %d frames", len(sub.sent))
	}
	if second.OcoGroupID != first.OcoGroupID ||
		second.Legs[0].OrderID != first.Legs[0].OrderID ||
		second.Legs[1].OrderID != first.Legs[1].OrderID ||
		!second.Legs[0].Replay || !second.Legs[1].Replay {
		t.Fatalf("replay ack mismatch: %+v", second)
	}
}

// A stored FILLED+CANCELLED pair means the engine already resolved the
// sibling race — the replay surfaces §23 OCO_SIBLING_CANCEL_RACE (409).
func TestSubmitOCO_RaceReplayReturns409(t *testing.T) {
	st := newFakeStore()
	svc := newSvc(t, st, &fakeSubmitter{})
	ctx := context.Background()

	ack, err := svc.SubmitOCO(ctx, st.acct, ocoPair("x-a", "x-b", "1.04", "1.06"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	st.orders[ack.Legs[0].OrderID].Status = "FILLED"
	st.orders[ack.Legs[1].OrderID].Status = "CANCELLED"

	_, err = svc.SubmitOCO(ctx, st.acct, ocoPair("x-a", "x-b", "1.04", "1.06"))
	if codeOf(t, err) != "OCO_SIBLING_CANCEL_RACE" {
		t.Fatalf("want OCO_SIBLING_CANCEL_RACE, got %v", err)
	}
}

// client_order_id reused with a different pair → §8.7 409 collision, and
// the atomicity contract: NOTHING from the new pair persists.
func TestSubmitOCO_CollisionRollsBackPair(t *testing.T) {
	st := newFakeStore()
	svc := newSvc(t, st, &fakeSubmitter{})
	ctx := context.Background()

	if _, err := svc.SubmitOCO(ctx, st.acct,
		ocoPair("c-a", "c-b", "1.04", "1.06")); err != nil {
		t.Fatalf("seed submit: %v", err)
	}
	before := len(st.orders)
	// Leg B reuses "c-a" but the pair hash differs → collision.
	_, err := svc.SubmitOCO(ctx, st.acct, ocoPair("new-a", "c-a", "1.05", "1.07"))
	if codeOf(t, err) != "IDEMPOTENCY_KEY_COLLISION" {
		t.Fatalf("want collision, got %v", err)
	}
	if len(st.orders) != before {
		t.Fatalf("rolled-back pair left rows: %d → %d", before, len(st.orders))
	}
}

// Cross-instrument legs are malformed — an OCO pair is defined per
// instrument (spec §6.5); the service refuses before persistence.
func TestSubmitOCO_CrossInstrumentRejected(t *testing.T) {
	st := newFakeStore()
	svc := newSvc(t, st, &fakeSubmitter{})
	req := ocoPair("y-a", "y-b", "1.04", "1.06")
	req.Legs[1].Symbol = "USDJPY"
	_, err := svc.SubmitOCO(context.Background(), st.acct, req)
	if codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

// A transport failure mid-dispatch marks BOTH legs REJECTED — the read
// model must never show a leg the engine holds without linkage.
func TestSubmitOCO_DispatchFailureRejectsBoth(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{fail: true}
	svc := newSvc(t, st, sub)
	_, err := svc.SubmitOCO(context.Background(), st.acct,
		ocoPair("f-a", "f-b", "1.04", "1.06"))
	if err == nil {
		t.Fatalf("dispatch failure must surface")
	}
	for _, o := range st.orders {
		if o.Status != "REJECTED" {
			t.Fatalf("order %d status %s, want REJECTED", o.ID, o.Status)
		}
	}
}

// Reason-7 OrderCancel from the engine: the sibling cancel applies like
// any cancel AND leaves an OCO_SIBLING_CANCEL audit row (spec §6.5
// terminal notice distinct from a user cancel).
func TestConsumer_OcoReasonAudit(t *testing.T) {
	st := newFakeStore()
	gid := int64(999)
	st.orders[11] = &Order{ID: 11, AccountID: 77, Status: "ACTIVE",
		OcoGroupID: &gid}
	c := NewConsumer(&ShmSubmitter{}, st, newPendingConfirms())

	b := flatbuffers.NewBuilder(128)
	wire.OrderCancelStart(b)
	wire.OrderCancelAddOrderId(b, 11)
	wire.OrderCancelAddAccountId(b, 77)
	wire.OrderCancelAddReason(b, CancelReasonOcoLink)
	oc := wire.OrderCancelEnd(b)
	wire.EventStart(b)
	wire.EventAddSeq(b, 1)
	wire.EventAddTs(b, 123)
	wire.EventAddTypeType(b, wire.EventTypeOrderCancel)
	wire.EventAddType(b, oc)
	b.Finish(wire.EventEnd(b))

	c.handle(b.FinishedBytes())
	if st.orders[11].Status != "CANCELLED" {
		t.Fatalf("oco cancel not applied: %s", st.orders[11].Status)
	}
	found := false
	for _, a := range st.auditRows {
		if a.Operation == "OCO_SIBLING_CANCEL" && a.OrderID == 11 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no OCO_SIBLING_CANCEL audit row: %+v", st.auditRows)
	}
}

// ParseSubmitOco: structural errors reject at the HTTP edge.
func TestParseSubmitOco(t *testing.T) {
	req, err := ParseSubmitOco([]byte(`{
		"symbol": "EURUSD",
		"legs": [
			{"client_order_id":"a","side":"BUY","order_type":"LIMIT",
			 "time_in_force":"GTC","quantity":"1000","price":"1.04"},
			{"client_order_id":"b","side":"BUY","order_type":"LIMIT",
			 "time_in_force":"GTC","quantity":"1000","price":"1.06"}
		]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Symbol != "EURUSD" || req.Legs[0] == nil || req.Legs[1] == nil {
		t.Fatalf("parsed request malformed: %+v", req)
	}
	for _, bad := range []string{
		`{"symbol":"EURUSD"}`, // legs missing
		`{"legs":[]}`,         // zero legs
		`{"legs":[{"side":"BUY","order_type":"LIMIT","quantity":"1"}]}`, // one leg
		`{"legs":[{},{},{}]}`, // three legs
	} {
		if _, err := ParseSubmitOco([]byte(bad)); err == nil {
			t.Fatalf("malformed body accepted: %s", bad)
		}
	}
}
