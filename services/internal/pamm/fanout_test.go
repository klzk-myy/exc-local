package pamm

import (
	"context"
	"fmt"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"exchange/internal/ipc"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// fakeMsg is a minimal jetstream.Msg for HandleMsg tests.
type fakeMsg struct {
	data []byte
	ack  bool
	nak  bool
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) { return nil, nil }
func (m *fakeMsg) Data() []byte                              { return m.data }
func (m *fakeMsg) Headers() nats.Header                      { return nil }
func (m *fakeMsg) Subject() string                           { return "trades.0.EUR/USD" }
func (m *fakeMsg) Reply() string                             { return "" }
func (m *fakeMsg) Ack() error                                { m.ack = true; return nil }
func (m *fakeMsg) DoubleAck(context.Context) error           { m.ack = true; return nil }
func (m *fakeMsg) Nak() error                                { m.nak = true; return nil }
func (m *fakeMsg) NakWithDelay(time.Duration) error          { m.nak = true; return nil }
func (m *fakeMsg) InProgress() error                         { return nil }
func (m *fakeMsg) Term() error                               { return nil }
func (m *fakeMsg) TermWithReason(string) error               { return nil }

type fakeResolver struct {
	rt  settlement.ResolvedTrade
	err error
	got []settlement.EngineFill
}

func (r *fakeResolver) Resolve(_ context.Context, f settlement.EngineFill) (settlement.ResolvedTrade, error) {
	r.got = append(r.got, f)
	return r.rt, r.err
}

type fakeLegHandler struct {
	own    map[int64]bool // account IDs this handler owns
	err    error
	legs   []MasterLeg
	called int
}

func (h *fakeLegHandler) Handle(_ context.Context, leg MasterLeg) (bool, error) {
	h.called++
	if !h.own[leg.AccountID] {
		return false, h.err
	}
	h.legs = append(h.legs, leg)
	return true, h.err
}

func fillFrame(tradeID, buyOrderID, sellOrderID uint64, price, qty int64) []byte {
	b := flatbuffers.NewBuilder(256)
	return ipc.EncodeTradeFillEvent(b, 1, 2, tradeID, buyOrderID, sellOrderID, price, qty, 9)
}

func TestFanout_HandleMsg_DispatchesBothLegs(t *testing.T) {
	res := &fakeResolver{rt: settlement.ResolvedTrade{
		InstrumentID: 1, BuyerAccountID: 500, SellerAccountID: 600}}
	pool := &fakeLegHandler{own: map[int64]bool{500: true}}
	other := &fakeLegHandler{own: map[int64]bool{600: true}}

	fan, err := NewTradesFanout(res, pool, other)
	if err != nil {
		t.Fatal(err)
	}
	msg := &fakeMsg{data: fillFrame(7, 11, 12, 110000000, 4000000000)}
	if err := fan.HandleMsg(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(pool.legs) != 1 || pool.legs[0].AccountID != 500 || pool.legs[0].Side != "BUY" {
		t.Fatalf("pool legs %+v", pool.legs)
	}
	// qty 4000000000 scaled = 40; price 110000000 scaled = 1.1.
	if !pool.legs[0].Quantity.Equal(decimal.RequireFromString("40")) ||
		!pool.legs[0].Price.Equal(decimal.RequireFromString("1.1")) {
		t.Fatalf("leg values %+v", pool.legs[0])
	}
	if len(other.legs) != 1 || other.legs[0].AccountID != 600 || other.legs[0].Side != "SELL" {
		t.Fatalf("other legs %+v", other.legs)
	}
	// The handler chain runs per leg: pool saw both legs (owned buyer,
	// declined seller), other saw only the seller leg (chain stopped on
	// the buyer leg at pool).
	if pool.called != 2 || other.called != 1 {
		t.Fatalf("calls pool=%d other=%d", pool.called, other.called)
	}
}

func TestFanout_HandleMsg_PoisonAndNonFill(t *testing.T) {
	res := &fakeResolver{}
	h := &fakeLegHandler{}
	fan, err := NewTradesFanout(res, h)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, {1, 2, 3}, make([]byte, 64)} {
		if err := fan.HandleMsg(context.Background(), &fakeMsg{data: data}); err != nil {
			t.Fatalf("poison frame must be swallowed (ACKed), got %v", err)
		}
	}
	if len(res.got) != 0 || h.called != 0 {
		t.Fatal("non-fill frames must not resolve or dispatch")
	}
}

func TestFanout_HandleMsg_ResolverErrorIsFatal(t *testing.T) {
	res := &fakeResolver{err: fmt.Errorf("pg down")}
	fan, _ := NewTradesFanout(res, &fakeLegHandler{})
	err := fan.HandleMsg(context.Background(), &fakeMsg{data: fillFrame(7, 11, 12, 1, 1)})
	if err == nil {
		t.Fatal("resolver failure must error → NAK for redelivery")
	}
}

func TestFanout_HandleMsg_HandlerErrorIsFatal(t *testing.T) {
	res := &fakeResolver{rt: settlement.ResolvedTrade{
		InstrumentID: 1, BuyerAccountID: 500, SellerAccountID: 600}}
	h := &fakeLegHandler{own: map[int64]bool{500: true}, err: fmt.Errorf("store down")}
	fan, _ := NewTradesFanout(res, h)
	if err := fan.HandleMsg(context.Background(), &fakeMsg{data: fillFrame(7, 11, 12, 1, 1)}); err == nil {
		t.Fatal("handler failure must error → NAK for redelivery")
	}
}

func TestFanout_HandleMsg_UnownedLegsIgnored(t *testing.T) {
	res := &fakeResolver{rt: settlement.ResolvedTrade{
		InstrumentID: 1, BuyerAccountID: 500, SellerAccountID: 600}}
	fan, _ := NewTradesFanout(res, &fakeLegHandler{}) // owns nothing
	if err := fan.HandleMsg(context.Background(),
		&fakeMsg{data: fillFrame(7, 11, 12, 1, 1)}); err != nil {
		t.Fatal(err)
	}
	if len(res.got) != 1 {
		t.Fatal("trade resolved")
	}
}
