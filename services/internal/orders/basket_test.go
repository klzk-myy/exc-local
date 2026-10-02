package orders

// Phase-3 Task 4 (IMP-PLAN) — cross-shard basket coverage: leg/shape
// validation, the REST→service→wire BasketSubmit render, coordinator-shard
// selection, ledger persistence, and the BasketResult consumer projection.

import (
	"context"
	"fmt"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/pkg/decimal"
)

// basketFake wraps fakeStore with the migration-283 ledger methods plus a
// second instrument on a different shard (USD/JPY → shard 1) so the
// coordinator-shard selection is exercised for real.
type basketFake struct {
	*fakeStore
	jpy   *Instrument
	op    *basketRow
	legRs []basketLegRow
	upd   *basketRow
}

func (b *basketFake) InstrumentBySymbol(_ context.Context, sym string) (*Instrument, error) {
	switch sym {
	case "USD/JPY":
		return b.jpy, nil
	default:
		return b.fakeStore.InstrumentBySymbol(context.Background(), sym)
	}
}

func (b *basketFake) InsertBasket(_ context.Context, r basketRow,
	legs []basketLegRow) error {
	b.op = &r
	b.legRs = legs
	return nil
}

func (b *basketFake) BasketByOp(_ context.Context, hi, lo uint64) (*basketRow, []basketLegRow, error) {
	if b.op == nil || b.op.OpHi != hi || b.op.OpLo != lo {
		return nil, nil, nil
	}
	return b.op, b.legRs, nil
}

func (b *basketFake) UpdateBasketResult(_ context.Context, _, _ uint64,
	status string, code, filled, unwound int, slippage int64) error {
	b.upd = &basketRow{Status: status, Code: code, LegsFilled: filled,
		LegsUnwound: unwound, SlippageTicks: slippage}
	return nil
}

func newBasketFake() *basketFake {
	jpy := *testInst()
	jpy.ID = 2
	jpy.Symbol = "USD/JPY"
	jpy.BaseCurrency = "USD"
	jpy.QuoteCurrency = "JPY"
	jpy.TickSize = decimal.MustFromString("0.001")
	return &basketFake{fakeStore: newFakeStore(), jpy: &jpy}
}

func twoLegReq() *BasketSubmitRequest {
	return &BasketSubmitRequest{
		OpID: "client-key-1",
		Legs: []BasketLegRequest{
			{Symbol: "EUR/USD", Side: SideBuy, Quantity: d("1000"),
				LimitPrice: d("1.05")},
			{Symbol: "USD/JPY", Side: SideSell, Quantity: d("1000")},
		},
	}
}

func TestSubmitBasketEndToEnd(t *testing.T) {
	st := newBasketFake()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st.fakeStore, sub)
	// Re-point the service's store at the wrapper so the basketStore
	// narrowing resolves (newSvc stored the inner fake).
	svc.store = st

	ack, err := svc.SubmitBasket(context.Background(), st.acct, twoLegReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(ack.OrderIDs) != 2 || ack.Status != "MATCHING" {
		t.Fatalf("ack: %+v", ack)
	}
	// Coordinator = lowest participating shard — EUR/USD lands on 0,
	// USD/JPY on 1 per config/sharding.yaml.
	if ack.Coordinator != 0 {
		t.Fatalf("coordinator shard %d, want 0", ack.Coordinator)
	}
	if len(ack.OpID) != 32 {
		t.Fatalf("op id %q not 32-hex", ack.OpID)
	}
	// Deterministic id: the same client key must render the same op id
	// so a resubmission hits engine-side op dedup.
	if hi, lo, err := opIDToUint128("client-key-1"); err != nil ||
		fmt.Sprintf("%016x%016x", hi, lo) != ack.OpID {
		t.Fatal("op id not deterministic from client key")
	}

	if len(sub.sent) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(sub.sent))
	}
	ev := ipc.DecodeEvent(sub.sent[0])
	if ev == nil || ev.TypeType() != wire.EventTypeBasketSubmit {
		t.Fatalf("sent payload is not BasketSubmit: %v", ev)
	}
	var tb flatbuffers.Table
	if !ev.Type(&tb) {
		t.Fatal("union table missing")
	}
	bs := &wire.BasketSubmit{}
	bs.Init(tb.Bytes, tb.Pos)
	hi, lo, err := opIDToUint128("client-key-1")
	if err != nil {
		t.Fatalf("op id: %v", err)
	}
	if bs.OpIdHi() != hi || bs.OpIdLo() != lo {
		t.Fatalf("op id wire %x:%x, want %x:%x", bs.OpIdHi(), bs.OpIdLo(), hi, lo)
	}
	if bs.AccountId() != uint64(st.acct.ID) {
		t.Fatalf("account %d, want %d", bs.AccountId(), st.acct.ID)
	}
	if bs.LegsLength() != 2 {
		t.Fatalf("legs %d, want 2", bs.LegsLength())
	}
	var leg wire.BasketLeg
	if !bs.Legs(&leg, 0) {
		t.Fatal("leg 0 missing")
	}
	if leg.InstrumentId() != 1 || leg.Side() != wire.SideBuy ||
		leg.Qty() != decimal.Scaled(decimal.MustFromString("1000")) ||
		leg.LimitPrice() != decimal.Scaled(decimal.MustFromString("1.05")) {
		t.Fatalf("leg 0 wire: inst=%d side=%d qty=%d limit=%d",
			leg.InstrumentId(), leg.Side(), leg.Qty(), leg.LimitPrice())
	}
	if !bs.Legs(&leg, 1) {
		t.Fatal("leg 1 missing")
	}
	if leg.InstrumentId() != 2 || leg.Side() != wire.SideSell ||
		leg.LimitPrice() != 0 {
		t.Fatalf("leg 1 wire: inst=%d side=%d limit=%d",
			leg.InstrumentId(), leg.Side(), leg.LimitPrice())
	}

	// Ledger row + leg rows persisted; leg order rows carry the op id.
	if st.op == nil || st.op.OpHi != hi || st.op.OpLo != lo ||
		st.op.LegCount != 2 || st.op.Status != "MATCHING" {
		t.Fatalf("ledger row: %+v", st.op)
	}
	if len(st.legRs) != 2 || st.legRs[0].LegIndex != 0 || st.legRs[1].LegIndex != 1 {
		t.Fatalf("leg rows: %+v", st.legRs)
	}
	for _, oid := range ack.OrderIDs {
		o := st.orders[oid]
		if o == nil || o.AlgoType == nil || *o.AlgoType != "BASKET_LEG" {
			t.Fatalf("leg order %d not marked BASKET_LEG", oid)
		}
	}
}

func TestSubmitBasketLegCountBounds(t *testing.T) {
	st := newBasketFake()
	svc := newSvc(t, st.fakeStore, &fakeSubmitter{})
	svc.store = st
	ctx := context.Background()

	one := &BasketSubmitRequest{Legs: twoLegReq().Legs[:1]}
	if got := codeOf(t, errFrom(svc.SubmitBasket(ctx, st.acct, one))); got != "INVALID_REQUEST" {
		t.Fatalf("1-leg basket: want INVALID_REQUEST, got %s", got)
	}
	nine := &BasketSubmitRequest{}
	for i := 0; i < 9; i++ {
		nine.Legs = append(nine.Legs, twoLegReq().Legs[0])
	}
	if got := codeOf(t, errFrom(svc.SubmitBasket(ctx, st.acct, nine))); got != "INVALID_REQUEST" {
		t.Fatalf("9-leg basket: want INVALID_REQUEST, got %s", got)
	}
}

func TestSubmitBasketUnknownSymbolFailsClosed(t *testing.T) {
	st := newBasketFake()
	svc := newSvc(t, st.fakeStore, &fakeSubmitter{})
	svc.store = st

	req := twoLegReq()
	req.Legs[1].Symbol = "XXX/YYY"
	_, err := svc.SubmitBasket(context.Background(), st.acct, req)
	if got := codeOf(t, err); got != "UNKNOWN_SYMBOL" {
		t.Fatalf("want UNKNOWN_SYMBOL, got %s", got)
	}
	// Leg 0's order row must be compensated to REJECTED — never live.
	for _, o := range st.orders {
		if o.Status != "REJECTED" {
			t.Fatalf("orphan leg order %d left status %s", o.ID, o.Status)
		}
	}
}

func TestSubmitBasketSendFailureCompensates(t *testing.T) {
	st := newBasketFake()
	sub := &fakeSubmitter{fail: true}
	svc := newSvc(t, st.fakeStore, sub)
	svc.store = st

	_, err := svc.SubmitBasket(context.Background(), st.acct, twoLegReq())
	if err == nil {
		t.Fatal("expected send failure")
	}
	// Basket row flips REJECTED; leg rows are marked rejected too.
	if st.upd == nil || st.upd.Status != "REJECTED" {
		t.Fatalf("ledger not compensated: %+v", st.upd)
	}
	for _, o := range st.orders {
		if o.Status != "REJECTED" {
			t.Fatalf("leg order %d not compensated: %s", o.ID, o.Status)
		}
	}
}

func TestBasketStatusRoundTrip(t *testing.T) {
	st := newBasketFake()
	svc := newSvc(t, st.fakeStore, &fakeSubmitter{})
	svc.store = st
	ctx := context.Background()

	ack, err := svc.SubmitBasket(ctx, st.acct, twoLegReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	st.op.Status = "COMMITTED"
	st.op.LegsFilled = 2

	got, err := svc.BasketStatus(ctx, st.acct, ack.OpID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got.Status != "COMMITTED" || got.LegsFilled != 2 || len(got.Legs) != 2 {
		t.Fatalf("status: %+v", got)
	}
	if _, err := svc.BasketStatus(ctx, st.acct, "zz"); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("bad op id: %v", err)
	}
	if _, err := svc.BasketStatus(ctx, st.acct, "00000000000000000000000000000000"); codeOf(t, err) != "NOT_FOUND" {
		t.Fatalf("unknown op: %v", err)
	}
	// Cross-account reads of a real op read as NOT_FOUND, never leaked.
	if _, err := svc.BasketStatus(ctx, &Account{ID: st.acct.ID + 1},
		ack.OpID); codeOf(t, err) != "NOT_FOUND" {
		t.Fatalf("foreign-account op leaked: %v", err)
	}
}

// TestBasketResultConsumerProjection — the terminal OptResult frame must
// land on the ledger via the store narrowing.
func TestBasketResultConsumerProjection(t *testing.T) {
	st := newBasketFake()
	svc := newSvc(t, st.fakeStore, &fakeSubmitter{})
	svc.store = st
	ctx := context.Background()

	if _, err := svc.SubmitBasket(ctx, st.acct, twoLegReq()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	hi, lo, err := opIDToUint128("client-key-1")
	if err != nil {
		t.Fatalf("op id: %v", err)
	}

	cons := &Consumer{store: st}
	b := flatbuffers.NewBuilder(256)
	wire.BasketResultStart(b)
	wire.BasketResultAddOpIdHi(b, hi)
	wire.BasketResultAddOpIdLo(b, lo)
	wire.BasketResultAddAccountId(b, uint64(st.acct.ID))
	wire.BasketResultAddStatus(b, 2) // COMMITTED
	wire.BasketResultAddLegCount(b, 2)
	wire.BasketResultAddLegsFilled(b, 2)
	wire.BasketResultAddSlippage(b, 7)
	resOff := wire.BasketResultEnd(b)
	wire.EventStart(b)
	wire.EventAddSeq(b, 9)
	wire.EventAddTypeType(b, wire.EventTypeBasketResult)
	wire.EventAddType(b, resOff)
	b.Finish(wire.EventEnd(b))
	cons.handle(b.FinishedBytes())

	if st.upd == nil || st.upd.Status != "COMMITTED" ||
		st.upd.LegsFilled != 2 || st.upd.SlippageTicks != 7 {
		t.Fatalf("projection: %+v", st.upd)
	}
}

func errFrom(_ *BasketAck, err error) error { return err }
