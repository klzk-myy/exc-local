// ExecutionReport (35=8) construction + the ReportBus fan-out seam.
// Every order-lifecycle event reaching a FIX client is one
// ExecutionReport — the same tap surface drop copy (Tasks 18.3.4/18.3.6)
// attaches to, so PB/compliance relays see a byte-identical stream.
package fix

import (
	"fmt"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ReportEvent is one emitted ExecutionReport: the destination session,
// the order it describes (when resolvable) and the outgoing message.
// Taps MUST NOT mutate Msg — they share the instance.
type ReportEvent struct {
	SessionID quickfix.SessionID
	OrderID   int64
	ClOrdID   string
	Msg       *quickfix.Message
}

// ReportTap observes every emitted ExecutionReport (drop-copy seam).
type ReportTap func(ReportEvent)

// ReportBus fans ExecutionReports to the owning session plus any
// registered taps. Zero taps is the common case (drop copy unwired).
type ReportBus struct {
	mu   sync.RWMutex
	taps []ReportTap
}

// WithTap registers a fan-out observer. Panics in a tap are isolated —
// a misbehaving drop-copy relay must never break the trading session.
func (b *ReportBus) WithTap(t ReportTap) *ReportBus {
	if t == nil {
		return b
	}
	b.mu.Lock()
	b.taps = append(b.taps, t)
	b.mu.Unlock()
	return b
}

// Emit sends the report to its session then fans out to taps.
// quickfix.SendToTarget stamps MsgSeqNum/SendingTime and archives the
// frame through the PG message store (resend replay).
func (b *ReportBus) Emit(ev ReportEvent) error {
	err := quickfix.SendToTarget(ev.Msg, ev.SessionID)
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, t := range b.taps {
		func() {
			defer func() { _ = recover() }()
			t(ev)
		}()
	}
	return err
}

// report builds one ExecutionReport body (header MsgType + session
// routing are filled by SendToTarget). execID mints are per-process
// unique monotonic.
var execSeq = struct {
	mu sync.Mutex
	n  uint64
}{n: uint64(time.Now().UnixNano())}

func nextExecID() string {
	execSeq.mu.Lock()
	execSeq.n++
	n := execSeq.n
	execSeq.mu.Unlock()
	return fmt.Sprintf("EX%d", n)
}

func newReport() *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgExecutionReport))
	m.Body.SetString(TagExecID, nextExecID())
	m.Body.SetField(TagTransactTime, quickfix.FIXUTCTimestamp{Time: time.Now().UTC()})
	return m
}

// setOrderFields stamps the echo tags a client expects on every 35=8.
func setOrderFields(m *quickfix.Message, o *orders.Order) {
	if o == nil {
		return
	}
	m.Body.SetString(TagOrderID, fmt.Sprint(o.ID))
	m.Body.SetString(TagClOrdID, o.ClientOrderID)
	if o.Side == orders.SideSell {
		m.Body.SetString(TagSide, SideSell)
	} else {
		m.Body.SetString(TagSide, SideBuy)
	}
	m.Body.SetString(TagOrderQty, o.Quantity.String())
	m.Body.SetString(TagCumQty, o.FilledQty.String())
	leaves := o.Quantity.Sub(o.FilledQty)
	if leaves.IsNegative() {
		leaves = decimal.Zero
	}
	m.Body.SetString(TagLeavesQty, leaves.String())
	if o.AvgFillPrice != nil {
		m.Body.SetString(TagAvgPx, o.AvgFillPrice.String())
	}
	if o.Price != nil {
		m.Body.SetString(TagPrice, o.Price.String())
	}
	m.Body.SetString(TagOrdStatus, ordStatusFIX(o.Status))
	m.Body.SetString(TagTimeInForce, tifFIX(o.TimeInForce))
	m.Body.SetString(TagOrdType, ordTypeFIX(o.OrderType))
}

// tifFIX maps internal TIF vocabulary back to TimeInForce(59).
func tifFIX(tif string) string {
	switch tif {
	case orders.TIFGTC:
		return TIFGTC
	case orders.TIFIOC:
		return TIFIOC
	case orders.TIFFOK:
		return TIFFOK
	case orders.TIFGTD:
		return TIFGTD
	default:
		return TIFDay
	}
}

// ordTypeFIX maps internal order types back to OrdType(40).
func ordTypeFIX(t string) string {
	switch t {
	case orders.TypeMarket, orders.TypeMOO, orders.TypeMOC:
		return OrdTypeMarket
	case orders.TypeStop:
		return OrdTypeStop
	case orders.TypeStopLimit:
		return OrdTypeStopLimit
	default:
		return OrdTypeLimit
	}
}

// reportAccepted — ExecType=0 New on a live Submit ack.
func reportAccepted(o *orders.Order, ack *orders.Ack) *quickfix.Message {
	m := newReport()
	setOrderFields(m, o)
	if o == nil {
		// Ack without a hydrated row (shouldn't happen — kept
		// defensive): the mandatory trio still lands.
		m.Body.SetString(TagOrderID, fmt.Sprint(ack.OrderID))
		m.Body.SetString(TagClOrdID, ack.ClientOrderID)
		m.Body.SetString(TagOrdStatus, OrdStatusNew)
	}
	m.Body.SetString(TagExecType, ExecTypeNew)
	return m
}

// reportRejected — ExecType=8 Rejected for a refused submission.
func reportRejected(clOrdID, symbol, side string, err error) *quickfix.Message {
	m := newReport()
	m.Body.SetString(TagExecType, ExecTypeRejected)
	m.Body.SetString(TagOrdStatus, OrdStatusRejected)
	m.Body.SetString(TagClOrdID, clOrdID)
	if symbol != "" {
		m.Body.SetString(TagSymbol, symbol)
	}
	if side != "" {
		m.Body.SetString(TagSide, side)
	}
	code := excerrors.CodeOf(err)
	m.Body.SetInt(TagOrdRejReason, ordRejReason(code))
	m.Body.SetString(TagText, code)
	return m
}

// reportCanceled — ExecType=4 Canceled (client cancel, CoD sweep, or
// engine-side cancel echo reaching the owning session).
func reportCanceled(o *orders.Order, origClOrdID string) *quickfix.Message {
	m := newReport()
	setOrderFields(m, o)
	if origClOrdID != "" {
		m.Body.SetString(TagOrigClOrdID, origClOrdID)
	}
	m.Body.SetString(TagExecType, ExecTypeCanceled)
	m.Body.SetString(TagOrdStatus, OrdStatusCanceled)
	return m
}

// reportReplaced — ExecType=5 Replaced on a successful cancel-replace.
func reportReplaced(o *orders.Order, origClOrdID string) *quickfix.Message {
	m := newReport()
	setOrderFields(m, o)
	if origClOrdID != "" {
		m.Body.SetString(TagOrigClOrdID, origClOrdID)
	}
	m.Body.SetString(TagExecType, ExecTypeReplaced)
	return m
}

// reportFill — ExecType=F Trade for one engine TradeFill leg.
func reportFill(o *orders.Order, lastPx, lastQty decimal.Decimal) *quickfix.Message {
	m := newReport()
	setOrderFields(m, o)
	m.Body.SetString(TagExecType, ExecTypeTrade)
	m.Body.SetString(TagLastPx, lastPx.String())
	m.Body.SetString(TagLastQty, lastQty.String())
	return m
}

// reportExpired — ExecType=C Expired for engine expiry echoes.
func reportExpired(o *orders.Order) *quickfix.Message {
	m := newReport()
	setOrderFields(m, o)
	m.Body.SetString(TagExecType, ExecTypeExpired)
	m.Body.SetString(TagOrdStatus, OrdStatusExpired)
	return m
}

// businessReject builds 35=j — the entitlement/throttle/business-level
// reject of spec §9.9 (session Reject 35=3 is reserved for framing).
// Delegates to the gapfill.go constructor so every 35=j carries the
// identical tag set (RefSeqNum(45)=0 — none of the app-level emits
// know the offending seq; quickfixgo stamps it on real resends).
func businessReject(refMsgType, refID, text string, reason int) *quickfix.Message {
	return NewBusinessReject(0, refMsgType, refID, reason, text)
}

// cancelReject builds 35=9 OrderCancelReject.
func cancelReject(clOrdID, origClOrdID, orderID, responseTo string,
	reason int, text string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgOrderCancelReject))
	m.Body.SetString(TagClOrdID, clOrdID)
	if origClOrdID != "" {
		m.Body.SetString(TagOrigClOrdID, origClOrdID)
	}
	m.Body.SetString(TagOrderID, orderID)
	m.Body.SetString(TagOrdStatus, OrdStatusRejected)
	m.Body.SetString(TagCxlRejResponseTo, responseTo)
	m.Body.SetInt(TagCxlRejReason, reason)
	m.Body.SetString(TagText, text)
	return m
}
