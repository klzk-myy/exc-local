// ExecutionReport construction tests (Task 18.3.2 item 5) + ReportBus
// fan-out semantics (the drop-copy tap seam, spec §9.9).
package fix

import (
	"testing"

	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func testOrder() *orders.Order {
	px := dec("1.0850")
	return &orders.Order{
		ID:            42,
		AccountID:     11,
		ClientOrderID: "cli-1",
		Side:          orders.SideBuy,
		OrderType:     orders.TypeLimit,
		Quantity:      dec("100000"),
		Price:         &px,
		TimeInForce:   orders.TIFGTC,
		Status:        "PARTIALLY_FILLED",
		FilledQty:     dec("30000"),
		SessionID:     "FIX.4.4:EXC->CLI",
	}
}

func TestReportAccepted_Fields(t *testing.T) {
	o := testOrder()
	o.Status = "ACTIVE"
	m := reportAccepted(o, &orders.Ack{OrderID: 42, ClientOrderID: "cli-1"})
	if mt, _ := m.MsgType(); mt != MsgExecutionReport {
		t.Fatalf("msgtype: %v", mt)
	}
	if bodyStr(m, TagExecType) != ExecTypeNew {
		t.Fatalf("exectype: %q", bodyStr(m, TagExecType))
	}
	if bodyStr(m, TagOrdStatus) != OrdStatusNew {
		t.Fatalf("ordstatus: %q", bodyStr(m, TagOrdStatus))
	}
	if bodyStr(m, TagClOrdID) != "cli-1" || bodyStr(m, TagOrderID) != "42" {
		t.Fatalf("ids: %q %q", bodyStr(m, TagClOrdID), bodyStr(m, TagOrderID))
	}
	if bodyStr(m, TagLeavesQty) != "70000" || bodyStr(m, TagCumQty) != "30000" {
		t.Fatalf("qtys: leaves=%q cum=%q", bodyStr(m, TagLeavesQty), bodyStr(m, TagCumQty))
	}
	if bodyStr(m, TagPrice) != "1.085" {
		t.Fatalf("price: %q", bodyStr(m, TagPrice))
	}
	if bodyStr(m, TagExecID) == "" {
		t.Fatal("ExecID(17) must be stamped")
	}
	if !m.Body.Has(TagTransactTime) {
		t.Fatal("TransactTime(60) must be stamped")
	}
}

func TestReportRejected_CarriesCodeAndReason(t *testing.T) {
	m := reportRejected("cli-9", "EUR-USD", SideBuy,
		excerrors.New("INSUFFICIENT_BALANCE", "no funds"))
	if bodyStr(m, TagExecType) != ExecTypeRejected ||
		bodyStr(m, TagOrdStatus) != OrdStatusRejected {
		t.Fatalf("status: %q %q", bodyStr(m, TagExecType), bodyStr(m, TagOrdStatus))
	}
	if v, _ := m.Body.GetInt(TagOrdRejReason); v != OrdRejReasonExceedsLimit {
		t.Fatalf("rej reason: %d", v)
	}
	if bodyText(t, m) != "INSUFFICIENT_BALANCE" {
		t.Fatalf("text: %q", bodyText(t, m))
	}
	if bodyStr(m, TagClOrdID) != "cli-9" || bodyStr(m, TagSymbol) != "EUR-USD" {
		t.Fatalf("echoes missing")
	}
}

func TestReportFill_PartialVsFull(t *testing.T) {
	o := testOrder()
	m := reportFill(o, dec("1.0860"), dec("30000"))
	if bodyStr(m, TagExecType) != ExecTypeTrade {
		t.Fatalf("exectype: %q", bodyStr(m, TagExecType))
	}
	if bodyStr(m, TagOrdStatus) != OrdStatusPartiallyFilled {
		t.Fatalf("ordstatus: %q", bodyStr(m, TagOrdStatus))
	}
	if bodyStr(m, TagLastPx) != "1.086" || bodyStr(m, TagLastQty) != "30000" {
		t.Fatalf("last: %q %q", bodyStr(m, TagLastPx), bodyStr(m, TagLastQty))
	}
	// Full fill: FilledQty==Quantity → OrdStatus 2 + LeavesQty 0.
	o.Status = "FILLED"
	o.FilledQty = o.Quantity
	m = reportFill(o, dec("1.0860"), dec("70000"))
	if bodyStr(m, TagOrdStatus) != OrdStatusFilled || bodyStr(m, TagLeavesQty) != "0" {
		t.Fatalf("full: %q %q", bodyStr(m, TagOrdStatus), bodyStr(m, TagLeavesQty))
	}
}

func TestReportCanceledAndReplaced(t *testing.T) {
	o := testOrder()
	o.Status = "CANCELLED"
	m := reportCanceled(o, "orig-cli")
	if bodyStr(m, TagExecType) != ExecTypeCanceled ||
		bodyStr(m, TagOrdStatus) != OrdStatusCanceled ||
		bodyStr(m, TagOrigClOrdID) != "orig-cli" {
		t.Fatalf("cancel report wrong")
	}
	o.Status = "ACTIVE"
	m = reportReplaced(o, "orig-cli")
	if bodyStr(m, TagExecType) != ExecTypeReplaced {
		t.Fatalf("replace: %q", bodyStr(m, TagExecType))
	}
}

func TestReportExpired(t *testing.T) {
	o := testOrder()
	o.Status = "EXPIRED"
	m := reportExpired(o)
	if bodyStr(m, TagExecType) != ExecTypeExpired ||
		bodyStr(m, TagOrdStatus) != OrdStatusExpired {
		t.Fatalf("expired report wrong")
	}
}

func TestCancelReject_Fields(t *testing.T) {
	m := cancelReject("cli-2", "orig-1", "42", CxlRejResponseToCancel,
		CxlRejReasonUnknownOrder, "ORDER_NOT_FOUND")
	if mt, _ := m.MsgType(); mt != MsgOrderCancelReject {
		t.Fatalf("msgtype: %v", mt)
	}
	if v, _ := m.Body.GetInt(TagCxlRejReason); v != CxlRejReasonUnknownOrder {
		t.Fatalf("reason: %d", v)
	}
	if bodyStr(m, TagCxlRejResponseTo) != CxlRejResponseToCancel ||
		bodyStr(m, TagOrigClOrdID) != "orig-1" {
		t.Fatalf("cxl fields wrong")
	}
}

// ReportBus fans out to every tap; a panicking tap never breaks the
// others (the trading session must survive a wedged drop-copy relay).
func TestReportBus_TapFanOutAndIsolation(t *testing.T) {
	bus := &ReportBus{}
	var a, b int
	bus.WithTap(func(ReportEvent) {
		a++
		panic("wedged relay")
	}).WithTap(func(ReportEvent) { b++ })
	// SendToTarget fails in-process (no live session) — taps still run.
	_ = bus.Emit(ReportEvent{SessionID: testSessionID(), Msg: newReport()})
	if a != 1 || b != 1 {
		t.Fatalf("taps: %d/%d — panic must isolate", a, b)
	}
	// Nil tap is a no-op, not a footgun.
	bus.WithTap(nil)
	_ = bus.Emit(ReportEvent{SessionID: testSessionID(), Msg: newReport()})
	if b != 2 {
		t.Fatalf("tap count: %d", b)
	}
}
