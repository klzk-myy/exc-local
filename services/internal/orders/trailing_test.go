package orders

// Phase-3 Task 2 (IMP-PLAN) — trailing-stop / discretionary-offset
// submit-path coverage: the fold from algo_params, the legality matrix,
// wire-field rendering in orderNewMsg, and the FIX custom-tag surface
// (the fix package tests cover the tag mapping).

import (
	"context"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/pkg/decimal"
)

func stopReq() *SubmitRequest {
	return &SubmitRequest{
		Symbol: "EURUSD", Side: SideSell, OrderType: TypeStop,
		TimeInForce: TIFGTC, Quantity: d("1000"), StopPrice: d("1.04000"),
	}
}

func TestTrailingStopFirstClassFields(t *testing.T) {
	req := stopReq()
	req.TrailingOffset = d("5")
	req.TrailingOffsetUnit = TrailUnitPips
	ref := decimal.MustFromString("1.05")
	if err := ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now()); err != nil {
		t.Fatalf("trailing STOP rejected: %v", err)
	}
	if req.AlgoType != AlgoTrailingStop {
		t.Fatalf("algo_type not synthesized: %q", req.AlgoType)
	}
	if len(req.AlgoParams) == 0 {
		t.Fatal("algo_params document not synthesized")
	}
}

func TestTrailingStopLegalityMatrix(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	cases := []struct {
		name string
		mut  func(*SubmitRequest)
		want string // "" = pass
	}{
		{"limit_with_trailing_rejected", func(r *SubmitRequest) {
			r.OrderType = TypeLimit
			r.Price = d("1.05")
			r.StopPrice = nil
			r.TrailingOffset = d("5")
			r.TrailingOffsetUnit = TrailUnitPips
		}, "INVALID_REQUEST"},
		{"offset_without_unit", func(r *SubmitRequest) {
			r.TrailingOffset = d("5")
		}, "INVALID_REQUEST"},
		{"unit_without_offset", func(r *SubmitRequest) {
			r.TrailingOffsetUnit = TrailUnitPips
		}, "INVALID_REQUEST"},
		{"bad_unit", func(r *SubmitRequest) {
			r.TrailingOffset = d("5")
			r.TrailingOffsetUnit = "FURLONGS"
		}, "INVALID_REQUEST"},
		{"fractional_pips_rejected", func(r *SubmitRequest) {
			r.TrailingOffset = d("1.5")
			r.TrailingOffsetUnit = TrailUnitPips
		}, "INVALID_REQUEST"},
		{"percentage_ok", func(r *SubmitRequest) {
			r.TrailingOffset = d("0.25")
			r.TrailingOffsetUnit = TrailUnitPercentage
		}, ""},
		{"sub_basis_point_rejected", func(r *SubmitRequest) {
			r.TrailingOffset = d("0.005")
			r.TrailingOffsetUnit = TrailUnitPercentage
		}, "INVALID_REQUEST"},
		{"absolute_ok", func(r *SubmitRequest) {
			r.TrailingOffset = d("0.00020")
			r.TrailingOffsetUnit = TrailUnitAbsolute
		}, ""},
		{"negative_offset", func(r *SubmitRequest) {
			r.TrailingOffset = d("-5")
			r.TrailingOffsetUnit = TrailUnitPips
		}, "INVALID_REQUEST"},
		{"activation_without_trail", func(r *SubmitRequest) {
			r.ActivationPrice = d("1.03")
		}, "INVALID_REQUEST"},
		{"activation_with_trail", func(r *SubmitRequest) {
			r.TrailingOffset = d("5")
			r.TrailingOffsetUnit = TrailUnitPips
			r.ActivationPrice = d("1.03")
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := stopReq()
			tc.mut(req)
			err := ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected reject: %v", err)
				}
				return
			}
			if got := codeOf(t, err); got != tc.want {
				t.Fatalf("want %s, got %s (%v)", tc.want, got, err)
			}
		})
	}
}

func TestTrailingStopAlgoParamsFold(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	// The /orders/algo spelling: algo_type + algo_params (the handler
	// sets type=STOP on this surface — the body carries no `type` key).
	req := &SubmitRequest{
		Symbol: "EURUSD", Side: SideSell, OrderType: TypeStop,
		TimeInForce: TIFGTC, Quantity: d("1000"),
		AlgoType: AlgoTrailingStop,
		AlgoParams: []byte(`{"trailing_offset":"2","trailing_unit":"PIPS",
			"activation_price":"1.03000","trigger_source":"MARK_PRICE"}`),
	}
	if err := ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now()); err != nil {
		t.Fatalf("fold submit rejected: %v", err)
	}
	if req.OrderType != TypeStop {
		t.Fatalf("order type not derived to STOP: %q", req.OrderType)
	}
	if req.TrailingOffset == nil || !req.TrailingOffset.Equal(decimal.MustFromString("2")) {
		t.Fatalf("offset not folded: %v", req.TrailingOffset)
	}
	if req.TrailingOffsetUnit != TrailUnitPips {
		t.Fatalf("unit not folded: %q", req.TrailingOffsetUnit)
	}
	if req.ActivationPrice == nil || !req.ActivationPrice.Equal(decimal.MustFromString("1.03000")) {
		t.Fatalf("activation_price not folded: %v", req.ActivationPrice)
	}
	if req.TriggerSource != TriggerSourceMark {
		t.Fatalf("trigger_source not folded: %q", req.TriggerSource)
	}
}

func TestTrailingStopAlgoParamsFoldRejects(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	req := &SubmitRequest{
		Symbol: "EURUSD", Side: SideSell, OrderType: TypeLimit,
		TimeInForce: TIFGTC, Quantity: d("1000"), Price: d("1.05"),
		AlgoType:   AlgoTrailingStop,
		AlgoParams: []byte(`{"trailing_offset":"2","trailing_unit":"PIPS"}`),
	}
	if got := codeOf(t, ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("TRAILING_STOP params on LIMIT: want INVALID_REQUEST, got %s", got)
	}
}

func TestOrderNewMsgTrailingWireFields(t *testing.T) {
	req := stopReq()
	req.TrailingOffset = d("2")
	req.TrailingOffsetUnit = TrailUnitPips
	req.ActivationPrice = d("1.03000")
	o := &Order{ID: 42, AccountID: 7, InstrumentID: 1, Side: SideSell,
		OrderType: TypeStop, TimeInForce: TIFGTC}
	m := orderNewMsg(o, testAcct(), req)
	if m.Type != wire.OrderTypeStopMarket {
		t.Fatalf("wire type %d, want StopMarket", m.Type)
	}
	if m.TrailingOffsetUnit != 1 {
		t.Fatalf("trail unit %d, want 1 (PIPS)", m.TrailingOffsetUnit)
	}
	if m.TrailingOffset != 2 {
		t.Fatalf("trail offset %d, want 2 pips", m.TrailingOffset)
	}
	if m.ActivationPrice != decimal.Scaled(decimal.MustFromString("1.03000")) {
		t.Fatalf("activation price %d not scaled", m.ActivationPrice)
	}
}

func TestOrderNewMsgTrailingUnitsWire(t *testing.T) {
	o := &Order{ID: 1, AccountID: 7, Side: SideSell,
		OrderType: TypeStop, TimeInForce: TIFGTC}
	for _, tc := range []struct {
		unit string
		in   string
		want int64
	}{
		{TrailUnitPips, "3", 3},
		{TrailUnitPercentage, "0.25", 25},      // pct*100
		{TrailUnitAbsolute, "0.00020", 20_000}, // 1e8-scaled ticks
	} {
		req := stopReq()
		req.TrailingOffset = d(tc.in)
		req.TrailingOffsetUnit = tc.unit
		m := orderNewMsg(o, testAcct(), req)
		if m.TrailingOffset != tc.want {
			t.Fatalf("unit %s: wire %d, want %d", tc.unit, m.TrailingOffset, tc.want)
		}
	}
}

func TestDiscretionaryOffsetWireAndLegality(t *testing.T) {
	ref := decimal.MustFromString("1.05")
	req := submitReq() // LIMIT
	req.DiscretionaryOffsetPips = d("3")
	if err := ValidateSubmit(req, testInst(), testAcct(), &ref, time.Now()); err != nil {
		t.Fatalf("discretionary LIMIT rejected: %v", err)
	}
	m := orderNewMsg(&Order{ID: 1, Side: SideBuy, OrderType: TypeLimit,
		TimeInForce: TIFGTC}, testAcct(), req)
	if m.DiscretionaryOffsetPips != 3 {
		t.Fatalf("wire discretionary_offset_pips %d, want 3", m.DiscretionaryOffsetPips)
	}

	bad := stopReq()
	bad.DiscretionaryOffsetPips = d("3") // on a STOP — illegal
	if got := codeOf(t, ValidateSubmit(bad, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("discretionary on STOP: want INVALID_REQUEST, got %s", got)
	}
	frac := submitReq()
	frac.DiscretionaryOffsetPips = d("1.5")
	if got := codeOf(t, ValidateSubmit(frac, testInst(), testAcct(), &ref, time.Now())); got != "INVALID_REQUEST" {
		t.Fatalf("fractional pips: want INVALID_REQUEST, got %s", got)
	}
}

// TestSubmitTrailingStopEndToEnd drives Service.Submit (fake store +
// fake submitter) and decodes the FlatBuffers OrderNew the engine would
// receive — the acceptance proof for Phase-3 Task 2: the fields survive
// REST → service → wire.
func TestSubmitTrailingStopEndToEnd(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)

	req := stopReq()
	req.ClientOrderID = "trail-1"
	req.StopPrice = nil // trailing anchors off the reference price
	req.TrailingOffset = d("5")
	req.TrailingOffsetUnit = TrailUnitPips
	req.TriggerSource = TriggerSourceMark
	req.ActivationPrice = d("1.03000")

	ack, err := svc.Submit(context.Background(), st.acct, req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if ack.Status != "ACTIVE" || len(sub.sent) != 1 {
		t.Fatalf("expected ACTIVE + 1 dispatch, got %+v / %d", ack, len(sub.sent))
	}

	ev := ipc.DecodeEvent(sub.sent[0])
	if ev == nil || ev.TypeType() != wire.EventTypeOrderNew {
		t.Fatalf("sent payload is not OrderNew: %v", ev)
	}
	var tb flatbuffers.Table
	if !ev.Type(&tb) {
		t.Fatal("union table missing")
	}
	on := &wire.OrderNew{}
	on.Init(tb.Bytes, tb.Pos)
	if on.Type() != wire.OrderTypeStopMarket {
		t.Fatalf("wire type %d", on.Type())
	}
	if on.TrailingOffset() != 5 || on.TrailingOffsetUnit() != 1 {
		t.Fatalf("trail %d unit %d", on.TrailingOffset(), on.TrailingOffsetUnit())
	}
	if on.TriggerSource() != 1 { // MARK_PRICE
		t.Fatalf("trigger_source %d", on.TriggerSource())
	}
	if on.ActivationPrice() != decimal.Scaled(decimal.MustFromString("1.03000")) {
		t.Fatalf("activation %d", on.ActivationPrice())
	}
	// Persisted row must be self-describing.
	o := st.orders[ack.OrderID]
	if len(o.AlgoParams) == 0 {
		t.Fatal("persisted row lacks the synthesized algo_params document")
	}
}

// --- spec §27 R8: 90-day GTC cap, end-to-end ------------------------------

// A GTC submit auto-converts to GTD at +90d: the wire frame carries
// TIF=GTD + gtd_expiry_ns so the engine's expiry heap fires the
// auto-expire deterministically, and the persisted row reflects it.
func TestSubmitGTCCapEndToEnd(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)

	before := time.Now()
	ack, err := svc.Submit(context.Background(), st.acct, submitReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	after := time.Now()

	ev := ipc.DecodeEvent(sub.sent[0])
	if ev == nil || ev.TypeType() != wire.EventTypeOrderNew {
		t.Fatalf("sent payload is not OrderNew: %v", ev)
	}
	var tb flatbuffers.Table
	if !ev.Type(&tb) {
		t.Fatal("union table missing")
	}
	on := &wire.OrderNew{}
	on.Init(tb.Bytes, tb.Pos)
	if on.Tif() != wire.TimeInForceGTD {
		t.Fatalf("wire TIF %d, want GTD", on.Tif())
	}
	lo := before.AddDate(0, 0, 90).UnixNano()
	hi := after.AddDate(0, 0, 90).UnixNano()
	if ns := on.GtdExpiryNs(); ns < lo || ns > hi {
		t.Fatalf("gtd_expiry_ns %d outside [now+90d] window [%d,%d]", ns, lo, hi)
	}
	o := st.orders[ack.OrderID]
	if o.TimeInForce != TIFGTD || o.GTDExpiry == nil {
		t.Fatalf("persisted row lacks cap: tif=%s expiry=%v",
			o.TimeInForce, o.GTDExpiry)
	}
}

// An engine expiry echo (reason=1) must emit the R8 order.expired /
// GTD_EXPIRED private notice — distinct from a user cancel.
func TestOnCancelExpiredEmitsGTDExpired(t *testing.T) {
	st := newFakeStore()
	sink := &notifySink{}
	svc := auctionSvc(t, st, &fakeSubmitter{}, openAuctionGate{}, sink)
	ctx := context.Background()

	ack, err := svc.Submit(ctx, st.acct, submitReq())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	_ = st.ApplyCancel(ctx, ack.OrderID)
	svc.OnCancel(ctx, ack.OrderID, CancelReasonExpired)

	d := sink.last(ChanOrderExpired)
	if d == nil || d["reason"] != ReasonGTDExpired || d["status"] != "EXPIRED" ||
		d["order_id"] != ack.OrderID {
		t.Fatalf("order.expired missing/malformed: %+v", d)
	}
	// A plain user cancel emits no expiry notice.
	sink.notes = nil
	svc.OnCancel(ctx, ack.OrderID, CancelReasonUser)
	if sink.last(ChanOrderExpired) != nil {
		t.Fatal("user cancel emitted order.expired")
	}
}

// A DAY submit picks up the session-close stamp through the bound
// seam — the engine rejects DAY without gtd_expiry_ns, so the gateway
// must always supply it.
func TestSubmitDAYGetsSessionCloseExpiry(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub)
	closeAt := time.Date(2026, 10, 2, 22, 0, 0, 0, time.UTC)
	svc.dayExpiry = func(time.Time) *time.Time { return &closeAt }

	req := submitReq()
	req.TimeInForce = TIFDAY
	ack, err := svc.Submit(context.Background(), st.acct, req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	ev := ipc.DecodeEvent(sub.sent[0])
	var tb flatbuffers.Table
	if !ev.Type(&tb) {
		t.Fatal("union table missing")
	}
	on := &wire.OrderNew{}
	on.Init(tb.Bytes, tb.Pos)
	if on.Tif() != wire.TimeInForceDAY {
		t.Fatalf("wire TIF %d, want DAY", on.Tif())
	}
	if on.GtdExpiryNs() != closeAt.UnixNano() {
		t.Fatalf("day expiry ns %d, want %d", on.GtdExpiryNs(), closeAt.UnixNano())
	}
	if o := st.orders[ack.OrderID]; o.GTDExpiry == nil ||
		!o.GTDExpiry.Equal(closeAt) {
		t.Fatalf("persisted day expiry %v, want %v", o.GTDExpiry, closeAt)
	}
}

// A nil seam leaves the expiry unset — the engine fails closed rather
// than resting an undeadline'd DAY order.
func TestSubmitDAYNilSeamLeavesExpiryUnset(t *testing.T) {
	st := newFakeStore()
	sub := &fakeSubmitter{}
	svc := newSvc(t, st, sub) // dayExpiry nil

	req := submitReq()
	req.TimeInForce = TIFDAY
	if _, err := svc.Submit(context.Background(), st.acct, req); err != nil {
		t.Fatalf("submit: %v", err)
	}
	ev := ipc.DecodeEvent(sub.sent[0])
	var tb flatbuffers.Table
	if !ev.Type(&tb) {
		t.Fatal("union table missing")
	}
	on := &wire.OrderNew{}
	on.Init(tb.Bytes, tb.Pos)
	if on.GtdExpiryNs() != 0 {
		t.Fatalf("expiry stamped despite nil seam: %d", on.GtdExpiryNs())
	}
}
