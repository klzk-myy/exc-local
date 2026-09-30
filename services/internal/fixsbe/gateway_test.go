// gateway_test.go — Task 18.3.8: SBE order-entry maps onto the same
// orders.Service admission path; drain gating; reject semantics.
package fixsbe

import (
	"context"
	"encoding/binary"
	"testing"

	"exchange/internal/orders"
	"exchange/pkg/decimal"
)

type fakeOrderAPI struct {
	acct      *orders.Account
	submitted *orders.SubmitRequest
	ack       *orders.Ack
	subErr    error
	cancelled bool
	cxlID     int64
	replaced  *orders.CancelReplaceRequest
	replOrd   *orders.Order
}

func (f *fakeOrderAPI) Submit(_ context.Context, _ *orders.Account,
	req *orders.SubmitRequest) (*orders.Ack, error) {
	f.submitted = req
	if f.subErr != nil {
		return nil, f.subErr
	}
	return f.ack, nil
}

func (f *fakeOrderAPI) Cancel(_ context.Context, _ *orders.Account,
	orderID int64, _, _, _ string) (*orders.Ack, error) {
	f.cancelled = true
	f.cxlID = orderID
	return &orders.Ack{OrderID: orderID, Status: "CANCELLED"}, nil
}

func (f *fakeOrderAPI) CancelReplace(_ context.Context, _ *orders.Account,
	orderID int64, req *orders.CancelReplaceRequest, _, _, _ string) (*orders.Order, error) {
	f.replaced = req
	return f.replOrd, nil
}

func (f *fakeOrderAPI) AccountByID(_ context.Context, _ int64) (*orders.Account, error) {
	return f.acct, nil
}

type fakeInstruments struct{ inst *orders.Instrument }

func (f fakeInstruments) InstrumentByID(_ context.Context, _ int64) (*orders.Instrument, error) {
	return f.inst, nil
}

type fakeDedup struct{ row *orders.DedupRow }

func (f fakeDedup) DedupLookup(_ context.Context, _ int64, _ string) (*orders.DedupRow, error) {
	return f.row, nil
}

func newGateway() (*Gateway, *fakeOrderAPI) {
	api := &fakeOrderAPI{
		acct: &orders.Account{ID: 7, Status: "ACTIVE"},
		ack:  &orders.Ack{OrderID: 555, Status: "ACTIVE"},
	}
	g := &Gateway{
		Orders:      api,
		Instruments: fakeInstruments{inst: &orders.Instrument{ID: 12, Symbol: "EURUSD"}},
		ClientIDs:   fakeDedup{},
	}
	return g, api
}

func sess() *SessionInfo { return &SessionInfo{ID: "s1", AccountID: 7, Codec: CodecSBE} }

// decodeOnly pulls the first outbound message out of a response stream.
func decodeOnly(t *testing.T, out []byte) Message {
	t.Helper()
	m, _, err := DecodeMessage(out)
	if err != nil {
		t.Fatalf("response decode: %v", err)
	}
	return m
}

func TestGatewayNewOrderMapsOntoSubmit(t *testing.T) {
	g, api := newGateway()
	frame := MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("ord-1"), AccountID: 7, InstrumentID: 12,
		Side: SideBuy, OrdType: OrdTypeLimit, TimeInForce: TIFGTC,
		Flags: FlagPostOnly, Price: 1_1000_0000, Qty: 10_0000_0000,
	})
	out, err := g.Handle(context.Background(), sess(), frame)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if api.submitted == nil {
		t.Fatal("orders.Service.Submit not reached")
	}
	if api.submitted.Symbol != "EURUSD" || api.submitted.Side != "BUY" ||
		api.submitted.OrderType != "LIMIT" || api.submitted.TimeInForce != "GTC" ||
		!api.submitted.PostOnly || api.submitted.ClientOrderID != "ord-1" ||
		api.submitted.SessionID != "s1" {
		t.Fatalf("submit mapping: %+v", api.submitted)
	}
	if api.submitted.Price == nil ||
		decimal.Scaled(*api.submitted.Price) != 1_1000_0000 {
		t.Fatalf("price mantissa mapping: %v", api.submitted.Price)
	}
	rep := decodeOnly(t, out).(ExecutionReport)
	if rep.OrderID != 555 || rep.ExecType != ExecTypeNew || rep.OrdStatus != OrdStatusNew {
		t.Fatalf("report: %+v", rep)
	}
}

func TestGatewayRejectsMalformedOrder(t *testing.T) {
	g, api := newGateway()
	// Missing side enum.
	frame := MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("bad"), AccountID: 7, InstrumentID: 12,
		Side: 9, OrdType: OrdTypeLimit, TimeInForce: TIFGTC, Qty: 1,
	})
	out, err := g.Handle(context.Background(), sess(), frame)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if api.submitted != nil {
		t.Fatal("malformed order reached Submit")
	}
	rep := decodeOnly(t, out).(ExecutionReport)
	if rep.ExecType != ExecTypeRejected || rep.RejectCode != RejMalformed {
		t.Fatalf("expected reject: %+v", rep)
	}
}

func TestGatewayEntitlementAndAccountMismatch(t *testing.T) {
	g, api := newGateway()
	// Session not entitled to instrument 99.
	s := &SessionInfo{ID: "s1", AccountID: 7, Instruments: map[uint32]bool{12: true}}
	out, _ := g.Handle(context.Background(), s, MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("n"), InstrumentID: 99, Side: SideBuy,
		OrdType: OrdTypeMarket, TimeInForce: TIFDay, Qty: 1,
	}))
	if api.submitted != nil {
		t.Fatal("unentitled order reached Submit")
	}
	if decodeOnly(t, out).(ExecutionReport).RejectCode != RejNotEntitled {
		t.Fatal("expected RejNotEntitled")
	}
	// Account mismatch between wire and session binding.
	out, _ = g.Handle(context.Background(), sess(), MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("n2"), AccountID: 999, InstrumentID: 12,
		Side: SideBuy, OrdType: OrdTypeMarket, TimeInForce: TIFDay, Qty: 1,
	}))
	if decodeOnly(t, out).(ExecutionReport).RejectCode != RejNotEntitled {
		t.Fatal("expected account-mismatch reject")
	}
}

// TestDrainBehavior — Task 18.3.17: draining sessions reject new/replace
// but preserves cancels.
func TestDrainBehavior(t *testing.T) {
	g, api := newGateway()
	s := sess()
	s.Draining = true
	out, _ := g.Handle(context.Background(), s, MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("n"), InstrumentID: 12, Side: SideBuy,
		OrdType: OrdTypeMarket, TimeInForce: TIFDay, Qty: 1,
	}))
	if api.submitted != nil {
		t.Fatal("order accepted during drain")
	}
	rj, ok := decodeOnly(t, out).(BusinessReject)
	if !ok || rj.RejectCode != RejDraining {
		t.Fatalf("expected draining business reject, got %+v", decodeOnly(t, out))
	}
	// Cancel still passes.
	out, _ = g.Handle(context.Background(), s, MarshalMessage(CancelOrder{
		ClOrdID: SetClOrdID("c"), OrderID: 555, InstrumentID: 12, Side: SideBuy,
	}))
	if !api.cancelled {
		t.Fatal("cancel blocked during drain")
	}
}

func TestCancelByOrigClOrdID(t *testing.T) {
	g, api := newGateway()
	g.ClientIDs = fakeDedup{row: &orders.DedupRow{OrderID: 4242}}
	out, _ := g.Handle(context.Background(), sess(), MarshalMessage(CancelOrder{
		ClOrdID: SetClOrdID("cxl"), OrigClOrdID: SetClOrdID("orig"),
		InstrumentID: 12, Side: SideSell,
	}))
	if !api.cancelled || api.cxlID != 4242 {
		t.Fatalf("origClOrdID resolution failed: %+v", api)
	}
	_ = out
}

func TestReplaceMapping(t *testing.T) {
	g, api := newGateway()
	api.replOrd = &orders.Order{ID: 555, Status: "ACTIVE", Side: "SELL",
		Price: decimalPtr("1.2345")}
	out, _ := g.Handle(context.Background(), sess(), MarshalMessage(ReplaceOrder{
		ClOrdID: SetClOrdID("r1"), OrderID: 555, InstrumentID: 12,
		Side: SideSell, TimeInForce: TIFGTC, Price: 1_2345_0000, Qty: 20_0000_0000,
	}))
	if api.replaced == nil || api.replaced.Mode != "STOP_ON_FAILURE" {
		t.Fatalf("replace mapping: %+v", api.replaced)
	}
	rep := decodeOnly(t, out).(ExecutionReport)
	if rep.ExecType != ExecTypeReplaced || rep.Price != 1_2345_0000 {
		t.Fatalf("replace report: %+v", rep)
	}
}

func decimalPtr(s string) *decimal.Decimal {
	d, _ := decimal.NewFromString(s)
	return &d
}

// TestStreamMultipleMessages — the handler walks a coalesced buffer and
// answers every frame.
func TestStreamMultipleMessages(t *testing.T) {
	g, api := newGateway()
	var buf []byte
	buf = MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("a"), InstrumentID: 12, Side: SideBuy,
		OrdType: OrdTypeMarket, TimeInForce: TIFDay, Qty: 1,
	})
	buf = EncodeMessage(buf, CancelOrder{
		ClOrdID: SetClOrdID("b"), OrderID: 555, InstrumentID: 12, Side: SideBuy,
	})
	out, err := g.Handle(context.Background(), sess(), buf)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if api.submitted == nil || !api.cancelled {
		t.Fatal("stream did not dispatch both messages")
	}
	// Two response frames.
	first, n, _ := DecodeMessage(out)
	second, n2, _ := DecodeMessage(out[n:])
	if n+n2 != len(out) || first == nil || second == nil {
		t.Fatal("response framing incomplete")
	}
}

// TestHeaderSentinel — header must not be confused for payload: a frame
// claiming template NewOrder with garbage body is rejected cleanly.
func TestFrameIntegrity(t *testing.T) {
	g, _ := newGateway()
	frame := MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("ok"), InstrumentID: 12, Side: SideBuy,
		OrdType: OrdTypeMarket, TimeInForce: TIFDay, Qty: 1,
	})
	// Corrupt block length downward → decode error path emits
	// BusinessReject + stream abort.
	binary.LittleEndian.PutUint16(frame[0:2], 16)
	out, err := g.Handle(context.Background(), sess(), frame[:headerSize+16])
	if err == nil {
		t.Fatal("corrupt frame accepted")
	}
	if decodeOnly(t, out).(BusinessReject).RejectCode != RejMalformed {
		t.Fatal("expected malformed reject")
	}
}
