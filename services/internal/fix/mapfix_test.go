// Task 18.3.2 mapping tests — FIX 35=D/35=G → canonical requests and
// the complete internal-status ↔ FIX OrdStatus map (spec §9.9).
package fix

import (
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/orders"
)

func newOrderMsg() *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgNewOrderSingle))
	m.Body.SetString(TagClOrdID, "cli-1")
	m.Body.SetString(TagSymbol, "eurusd")
	m.Body.SetString(TagSide, SideBuy)
	m.Body.SetString(TagOrderQty, "100000")
	m.Body.SetString(TagOrdType, OrdTypeLimit)
	m.Body.SetString(TagPrice, "1.0850")
	return m
}

func TestMapNewOrderSingle_Limit(t *testing.T) {
	req, merr := MapNewOrderSingle(newOrderMsg(), "FIX.4.4:EXC->CLI")
	if merr != nil {
		t.Fatalf("map: %v", merr)
	}
	if req.Symbol != "EUR/USD" {
		t.Fatalf("symbol canonicalization: %q", req.Symbol)
	}
	if req.Side != orders.SideBuy || req.OrderType != orders.TypeLimit {
		t.Fatalf("side/type: %q %q", req.Side, req.OrderType)
	}
	if req.ClientOrderID != "cli-1" || req.SessionID != "FIX.4.4:EXC->CLI" {
		t.Fatalf("clord/session: %+v", req)
	}
	if req.Price == nil || req.Price.String() != "1.085" {
		t.Fatalf("price: %v", req.Price)
	}
	if req.Quantity == nil || req.Quantity.String() != "100000" {
		t.Fatalf("qty: %v", req.Quantity)
	}
	// Absent TimeInForce → DAY (FIX convention).
	if req.TimeInForce != orders.TIFDAY {
		t.Fatalf("tif default: %q", req.TimeInForce)
	}
}

func TestMapNewOrderSingle_MarketNeedsNoPrice(t *testing.T) {
	m := newOrderMsg()
	m.Body.SetString(TagOrdType, OrdTypeMarket)
	m.Body.Remove(TagPrice)
	m.Body.SetString(TagTimeInForce, TIFIOC)
	req, merr := MapNewOrderSingle(m, "s")
	if merr != nil {
		t.Fatalf("map: %v", merr)
	}
	if req.OrderType != orders.TypeMarket || req.Price != nil {
		t.Fatalf("market req: %+v", req)
	}
	if req.TimeInForce != orders.TIFIOC {
		t.Fatalf("tif: %q", req.TimeInForce)
	}
}

func TestMapNewOrderSingle_StopLimitRequiresBothPrices(t *testing.T) {
	m := newOrderMsg()
	m.Body.SetString(TagOrdType, OrdTypeStopLimit)
	m.Body.SetString(TagStopPx, "1.0800")
	req, merr := MapNewOrderSingle(m, "s")
	if merr != nil || req.OrderType != orders.TypeStopLimit ||
		req.StopPrice == nil || req.Price == nil {
		t.Fatalf("stoplimit: %v %+v", merr, req)
	}
	// Missing StopPx rejects.
	m2 := newOrderMsg()
	m2.Body.SetString(TagOrdType, OrdTypeStop)
	m2.Body.Remove(TagPrice)
	if _, merr := MapNewOrderSingle(m2, "s"); merr == nil || merr.Code != "INVALID_REQUEST" {
		t.Fatalf("missing stoppx must reject: %v", merr)
	}
}

func TestMapNewOrderSingle_IcebergViaMaxFloor(t *testing.T) {
	m := newOrderMsg()
	m.Body.SetString(TagMaxFloor, "10000")
	req, merr := MapNewOrderSingle(m, "s")
	if merr != nil || req.OrderType != orders.TypeIceberg {
		t.Fatalf("iceberg: %v %+v", merr, req)
	}
	if req.DisplayQty == nil || req.DisplayQty.String() != "10000" {
		t.Fatalf("display: %v", req.DisplayQty)
	}
	// MaxFloor >= qty rejects.
	m.Body.SetString(TagMaxFloor, "100000")
	if _, merr := MapNewOrderSingle(m, "s"); merr == nil {
		t.Fatal("maxfloor>=qty must reject")
	}
}

func TestMapNewOrderSingle_GTDRequiresExpireTime(t *testing.T) {
	m := newOrderMsg()
	m.Body.SetString(TagTimeInForce, TIFGTD)
	if _, merr := MapNewOrderSingle(m, "s"); merr == nil || merr.Code != "INVALID_REQUEST" {
		t.Fatalf("GTD w/o ExpireTime must reject: %v", merr)
	}
	m.Body.SetField(TagExpireTime, quickfix.FIXUTCTimestamp{
		Time: time.Now().UTC().Add(time.Hour)})
	req, merr := MapNewOrderSingle(m, "s")
	if merr != nil || req.TimeInForce != orders.TIFGTD || req.GTDExpiry == nil {
		t.Fatalf("gtd: %v %+v", merr, req)
	}
}

func TestMapNewOrderSingle_Rejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(m *quickfix.Message)
		want string
	}{
		{"missing clordid", func(m *quickfix.Message) { m.Body.Remove(TagClOrdID) }, "INVALID_REQUEST"},
		{"clordid too long", func(m *quickfix.Message) {
			m.Body.SetString(TagClOrdID, string(make([]byte, 65)))
		}, "INVALID_REQUEST"},
		{"missing symbol", func(m *quickfix.Message) { m.Body.Remove(TagSymbol) }, "INVALID_REQUEST"},
		{"bad side", func(m *quickfix.Message) { m.Body.SetString(TagSide, "9") }, "INVALID_REQUEST"},
		{"bad ordtype", func(m *quickfix.Message) { m.Body.SetString(TagOrdType, "F") }, "INVALID_REQUEST"},
		{"bad tif", func(m *quickfix.Message) { m.Body.SetString(TagTimeInForce, "2") }, "INVALID_REQUEST"},
		{"missing qty", func(m *quickfix.Message) { m.Body.Remove(TagOrderQty) }, "INVALID_REQUEST"},
		{"non-positive qty", func(m *quickfix.Message) { m.Body.SetString(TagOrderQty, "-5") }, "INVALID_REQUEST"},
		{"limit no price", func(m *quickfix.Message) { m.Body.Remove(TagPrice) }, "INVALID_REQUEST"},
		{"bad decimal", func(m *quickfix.Message) { m.Body.SetString(TagPrice, "abc") }, "INVALID_REQUEST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newOrderMsg()
			tc.mut(m)
			req, merr := MapNewOrderSingle(m, "s")
			if merr == nil {
				t.Fatalf("expected reject, got req %+v", req)
			}
			if merr.Code != tc.want {
				t.Fatalf("code: %q want %q (%v)", merr.Code, tc.want, merr)
			}
			if merr.OrdReject != OrdRejReasonOther {
				t.Fatalf("ordRejReason: %d", merr.OrdReject)
			}
		})
	}
}

func TestMapCancelReplace(t *testing.T) {
	m := quickfix.NewMessage()
	m.Body.SetString(TagPrice, "1.0900")
	req, merr := MapCancelReplace(m)
	if merr != nil || req.Price == nil || req.Mode != "STOP_ON_FAILURE" {
		t.Fatalf("replace: %v %+v", merr, req)
	}
	// No mutable fields → reject.
	if _, merr := MapCancelReplace(quickfix.NewMessage()); merr == nil {
		t.Fatal("empty replace must reject")
	}
}

// FIX↔internal status map completeness (user constraint: "complete and
// tested"). Every internal order_status maps to exactly one FIX 39.
func TestOrdStatusMap_Complete(t *testing.T) {
	want := map[string]string{
		"PENDING":          OrdStatusPendingNew,
		"RESERVED":         OrdStatusNew,
		"ACTIVE":           OrdStatusNew,
		"PARTIALLY_FILLED": OrdStatusPartiallyFilled,
		"FILLED":           OrdStatusFilled,
		"CANCELLED":        OrdStatusCanceled,
		"REJECTED":         OrdStatusRejected,
		"EXPIRED":          OrdStatusExpired,
	}
	for internal, fixStatus := range want {
		if got := ordStatusFIX(internal); got != fixStatus {
			t.Fatalf("%s → %q want %q", internal, got, fixStatus)
		}
	}
	// Unknown internal states must still produce a defined value —
	// never "".
	if ordStatusFIX("SOMETHING_ELSE") == "" {
		t.Fatal("unknown status must map to a defined FIX value")
	}
}

func TestOrdRejReasonMap(t *testing.T) {
	cases := map[string]int{
		"ORDER_NOT_FOUND":                 OrdRejReasonUnknownOrder,
		"UNKNOWN_SYMBOL":                  OrdRejReasonUnknownSymbol,
		"TRADING_HALTED":                  OrdRejReasonExchangeClosed,
		"CIRCUIT_BREAKER_OPEN":            OrdRejReasonExchangeClosed,
		"INSUFFICIENT_BALANCE":            OrdRejReasonExceedsLimit,
		"IDEMPOTENCY_KEY_COLLISION":       OrdRejReasonDuplicateOrder,
		"GATEWAY_TIMEOUT_MATCHING_ENGINE": OrdRejReasonTooLate,
		"STALE_MODIFY":                    OrdRejReasonTooLate,
		"SESSION_NOT_ENTITLED":            OrdRejReasonOther,
		"SESSION_THROTTLED":               OrdRejReasonOther,
		"WHATEVER_UNLISTED":               OrdRejReasonOther,
	}
	for code, want := range cases {
		if got := ordRejReason(code); got != want {
			t.Fatalf("%s → %d want %d", code, got, want)
		}
	}
}

func TestSideAndTIFMaps(t *testing.T) {
	if s, _ := MapSide(SideSell); s != orders.SideSell {
		t.Fatalf("side sell: %q", s)
	}
	if _, merr := MapSide("X"); merr == nil {
		t.Fatal("bad side must reject")
	}
	for fixTif, internal := range map[string]string{
		TIFDay: orders.TIFDAY, TIFGTC: orders.TIFGTC, TIFIOC: orders.TIFIOC,
		TIFFOK: orders.TIFFOK,
	} {
		m := quickfix.NewMessage()
		m.Body.SetString(TagTimeInForce, fixTif)
		got, _, merr := MapTimeInForce(m)
		if merr != nil || got != internal {
			t.Fatalf("tif %s → %q %v", fixTif, got, merr)
		}
	}
}

// Phase-3 Task 2 — venue custom tags 20003–20006: trailing stops and
// whole-pip discretionary offsets over FIX 4.4 (spec §6.11).

func TestMapNewOrderSingle_TrailingStop(t *testing.T) {
	m := newOrderMsg()
	m.Body.SetString(TagOrdType, OrdTypeStop)
	m.Body.Remove(TagPrice)
	m.Body.SetString(TagTrailingOffset, "5")
	m.Body.SetString(TagTrailingOffsetUnit, "PIPS")
	m.Body.SetString(TagActivationPrice, "1.0300")
	req, merr := MapNewOrderSingle(m, "s")
	if merr != nil {
		t.Fatalf("trailing map: %v", merr)
	}
	if req.OrderType != orders.TypeStop {
		t.Fatalf("type %q", req.OrderType)
	}
	if req.TrailingOffset == nil || req.TrailingOffset.String() != "5" {
		t.Fatalf("offset: %v", req.TrailingOffset)
	}
	if req.TrailingOffsetUnit != orders.TrailUnitPips {
		t.Fatalf("unit %q", req.TrailingOffsetUnit)
	}
	if req.ActivationPrice == nil || req.ActivationPrice.String() != "1.03" {
		t.Fatalf("activation: %v", req.ActivationPrice)
	}
}

func TestMapNewOrderSingle_TrailingRequiresStopOrdType(t *testing.T) {
	m := newOrderMsg()
	m.Body.SetString(TagTrailingOffset, "5")
	m.Body.SetString(TagTrailingOffsetUnit, "PIPS")
	if _, merr := MapNewOrderSingle(m, "s"); merr == nil || merr.Code != "INVALID_REQUEST" {
		t.Fatalf("trailing on LIMIT must reject, got %v", merr)
	}
}

func TestMapNewOrderSingle_StopWithoutStopPxStillRejects(t *testing.T) {
	m := newOrderMsg()
	m.Body.SetString(TagOrdType, OrdTypeStop)
	m.Body.Remove(TagPrice)
	if _, merr := MapNewOrderSingle(m, "s"); merr == nil {
		t.Fatal("plain STOP without StopPx must reject")
	}
}

func TestMapNewOrderSingle_DiscretionaryOffset(t *testing.T) {
	m := newOrderMsg()
	m.Body.SetString(TagDiscretionaryOffPip, "3")
	req, merr := MapNewOrderSingle(m, "s")
	if merr != nil {
		t.Fatalf("map: %v", merr)
	}
	if req.DiscretionaryOffsetPips == nil || req.DiscretionaryOffsetPips.String() != "3" {
		t.Fatalf("disc offset: %v", req.DiscretionaryOffsetPips)
	}
}
