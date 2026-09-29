// bench_test.go — Task 18.3.8 ingress budget evidence.
//
// The plan's target is <5µs binary ingress (frame → decoded message →
// mapped request, excluding orders.Service business work). Two numbers
// are measured:
//
//	BenchmarkIngressDecode — frame decode + inbound template check only.
//	BenchmarkIngressGateway — decode + full dispatch against a stub
//	OrderAPI that returns immediately (measures everything in this
//	package; the orders.Service call itself is stubbed since its
//	latency belongs to the order pipeline, not the gateway).
//
// Run: go test -bench=. -benchmem ./internal/fixsbe/
package fixsbe

import (
	"context"
	"testing"

	"exchange/internal/orders"
)

var sinkBytes []byte
var sinkMsg Message

// BenchmarkIngressDecode measures raw frame → decoded Message.
func BenchmarkIngressDecode(b *testing.B) {
	frame := MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("bench"), AccountID: 7, InstrumentID: 12,
		Side: SideBuy, OrdType: OrdTypeLimit, TimeInForce: TIFGTC,
		Flags: FlagPostOnly, Price: 1_1000_0000, Qty: 10_0000_0000,
		TransactTimeNs: 42,
	})
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, _, err := DecodeInbound(frame)
		if err != nil {
			b.Fatal(err)
		}
		sinkMsg = m
	}
}

// stubOrders satisfies OrderAPI with zero business cost.
type stubOrders struct{ acct *orders.Account }

func (s *stubOrders) Submit(_ context.Context, _ *orders.Account,
	_ *orders.SubmitRequest) (*orders.Ack, error) {
	return &orders.Ack{OrderID: 1, Status: "ACTIVE"}, nil
}
func (s *stubOrders) Cancel(_ context.Context, _ *orders.Account,
	id int64, _, _, _ string) (*orders.Ack, error) {
	return &orders.Ack{OrderID: id, Status: "CANCELLED"}, nil
}
func (s *stubOrders) CancelReplace(_ context.Context, _ *orders.Account,
	id int64, _ *orders.CancelReplaceRequest, _, _, _ string) (*orders.Order, error) {
	return &orders.Order{ID: id, Status: "ACTIVE"}, nil
}
func (s *stubOrders) AccountByID(_ context.Context, _ int64) (*orders.Account, error) {
	return s.acct, nil
}

// BenchmarkIngressGateway measures decode + entitlement checks +
// request mapping + response encode — the complete binary ingress path
// this package owns.
func BenchmarkIngressGateway(b *testing.B) {
	g := &Gateway{
		Orders:      &stubOrders{acct: &orders.Account{ID: 7}},
		Instruments: fakeInstruments{inst: &orders.Instrument{ID: 12, Symbol: "EURUSD"}},
		ClientIDs:   fakeDedup{},
	}
	s := sess()
	frame := MarshalMessage(NewOrder{
		ClOrdID: SetClOrdID("bench"), AccountID: 7, InstrumentID: 12,
		Side: SideBuy, OrdType: OrdTypeLimit, TimeInForce: TIFGTC,
		Flags: FlagPostOnly, Price: 1_1000_0000, Qty: 10_0000_0000,
		TransactTimeNs: 42,
	})
	ctx := context.Background()
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := g.Handle(ctx, s, frame)
		if err != nil {
			b.Fatal(err)
		}
		sinkBytes = out
	}
}
