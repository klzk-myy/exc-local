// Async engine feed: the FIX gateway's own consumer on
// aeron:ipc?alias=orders_out (stream 1002). Fragments are pushed
// through orders.Consumer.HandleFragment — the same decode +
// ApplyFill/ApplyCancel + pending-resolve pipeline the REST gateway's
// shm consumer uses — and the consumer's fill/cancel hooks emit
// ExecutionReports for FIX-attributed orders (orders.session_id =
// FIX compID pair) to the owning session, plus any drop-copy taps on
// the ReportBus.
package fix

import (
	"context"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/orders"
	"exchange/pkg/decimal"
)

// FragmentSource is the Aeron/mock streaming seam — the same shape
// settlement.AeronSource exposes: Pump pulls fragments and calls back;
// <0 signals a transport error.
type FragmentSource interface {
	Pump(limit int, deliver func(payload []byte)) int
}

// FragmentSink consumes one outbound-engine frame — production value:
// (*orders.Consumer).HandleFragment.
type FragmentSink func(payload []byte)

// OutFeed drains engine lifecycle echoes for FIX-owned orders.
type OutFeed struct {
	src  FragmentSource
	sink FragmentSink
}

// NewOutFeed binds the feed to a fragment source and the canonical
// consumer handler.
func NewOutFeed(src FragmentSource, sink FragmentSink) *OutFeed {
	return &OutFeed{src: src, sink: sink}
}

// Run pumps until ctx cancels. One goroutine per gateway process.
func (f *OutFeed) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if f.src.Pump(256, f.sink) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}
}

// sessionFor maps an orders.session_id string back to a live
// quickfix.SessionID — reports only reach still-connected sessions
// (a disconnected session's reports are moot; its orders were
// mass-cancelled by CoD or intentionally preserved).
func (a *App) sessionFor(orderSessionID string) (quickfix.SessionID, bool) {
	if orderSessionID == "" {
		return quickfix.SessionID{}, false
	}
	for _, id := range a.ActiveSessions() {
		if id.String() == orderSessionID {
			return id, true
		}
	}
	return quickfix.SessionID{}, false
}

// ReportHooks returns the (fill, cancel) consumer hooks emitting
// ExecutionReports — wire them with
// consumer.WithFillHook(fill).WithCancelHook(cancel). read re-hydrates
// the row the consumer just applied so OrdStatus/quantities are exact.
func (a *App) ReportHooks(read OrderRead) (
	fill func(orderID int64, price, qty decimal.Decimal),
	cancel func(orderID int64, reason uint8)) {
	fill = func(orderID int64, price, qty decimal.Decimal) {
		ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		o, err := read.GetOrder(ctx, orderID)
		if err != nil || o == nil {
			return
		}
		sid, ok := a.sessionFor(o.SessionID)
		if !ok {
			return
		}
		a.Report().Emit(ReportEvent{SessionID: sid, OrderID: orderID,
			ClOrdID: o.ClientOrderID, Msg: reportFill(o, price, qty)})
	}
	cancel = func(orderID int64, reason uint8) {
		ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		o, err := read.GetOrder(ctx, orderID)
		if err != nil || o == nil {
			return
		}
		sid, ok := a.sessionFor(o.SessionID)
		if !ok {
			return
		}
		var m *quickfix.Message
		if reason == orders.CancelReasonExpired {
			m = reportExpired(o)
		} else {
			m = reportCanceled(o, "")
		}
		a.Report().Emit(ReportEvent{SessionID: sid, OrderID: orderID,
			ClOrdID: o.ClientOrderID, Msg: m})
	}
	return
}
