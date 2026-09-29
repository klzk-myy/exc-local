// gateway.go — Task 18.3.8: the SBE order-entry gateway mapping decoded
// binary messages onto the canonical orders.Service pipeline. Binary
// ingress is a different serialization of the same admission contract —
// validate → persist intent → dispatch to the engine — never a
// second-order path.
package fixsbe

import (
	"context"
	"fmt"
	"time"

	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// SessionInfo is the negotiated SBE session context the transport layer
// (transport.go) binds after Ed25519 proof + schema negotiation.
type SessionInfo struct {
	ID          string // fixsbe_sessions.session_id
	AccountID   int64
	Codec       ResponseCodec
	Draining    bool  // maintenance drain in progress (Task 18.3.17)
	Instruments map[uint32]bool // nil = all instruments entitled
}

// Entitled reports whether the session may trade instrumentID.
func (s *SessionInfo) Entitled(instrumentID uint32) bool {
	return s.Instruments == nil || s.Instruments[instrumentID]
}

// OrderAPI is the slice of orders.Service the SBE gateway consumes;
// *orders.Service satisfies it.
type OrderAPI interface {
	Submit(ctx context.Context, acct *orders.Account, req *orders.SubmitRequest) (*orders.Ack, error)
	Cancel(ctx context.Context, acct *orders.Account, orderID int64,
		actor, requestID, ip string) (*orders.Ack, error)
	CancelReplace(ctx context.Context, acct *orders.Account, orderID int64,
		req *orders.CancelReplaceRequest, actor, requestID, ip string) (*orders.Order, error)
	AccountByID(ctx context.Context, id int64) (*orders.Account, error)
}

// InstrumentIndex resolves the wire instrument id to the orders
// pipeline's instrument snapshot. *orders.PgStore satisfies it.
type InstrumentIndex interface {
	InstrumentByID(ctx context.Context, id int64) (*orders.Instrument, error)
}

// ClientOrderIndex resolves OrigClOrdID → order id for
// cancel/replace-by-ClOrdID (client_order_id_dedup). *orders.PgStore
// satisfies it.
type ClientOrderIndex interface {
	DedupLookup(ctx context.Context, accountID int64, clientOrderID string) (*orders.DedupRow, error)
}

// Gateway handles one decoded SBE message stream. Construct per
// listener; all state lives in the seams.
type Gateway struct {
	Orders      OrderAPI
	Instruments InstrumentIndex
	ClientIDs   ClientOrderIndex
	Now         func() time.Time
}

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Reject codes carried in BusinessReject.RejectCode / ExecutionReport
// .RejectCode (venue binary-code enumeration — NOT the §23 string
// codes; the string is logged, the numeric rides the wire).
const (
	RejOther             uint32 = 1
	RejNotEntitled       uint32 = 2
	RejUnknownInstrument uint32 = 3
	RejOrderNotFound     uint32 = 4
	RejDraining          uint32 = 5
	RejThrottled         uint32 = 6
	RejMalformed         uint32 = 7
)

// mantToDec converts the fixed-scale mantissa to a pipeline decimal via
// the shared core wire convention (decimal.NewFromScaled — int64 count
// of 10^-8 units, identical to the FlatBuffers engine schema);
// mantissa 0 means absent → nil pointer (presence semantics).
func mantToDec(m int64) *decimal.Decimal {
	if m == 0 {
		return nil
	}
	d := decimal.NewFromScaled(m)
	return &d
}

func decToMant(d *decimal.Decimal) int64 {
	if d == nil {
		return 0
	}
	return decimal.Scaled(*d)
}

func sideIn(v uint8) string {
	switch v {
	case SideBuy:
		return orders.SideBuy
	case SideSell:
		return orders.SideSell
	}
	return ""
}

func ordTypeIn(v uint8) string {
	switch v {
	case OrdTypeMarket:
		return orders.TypeMarket
	case OrdTypeLimit:
		return orders.TypeLimit
	case OrdTypeStop:
		return orders.TypeStop
	case OrdTypeStopLimit:
		return orders.TypeStopLimit
	}
	return ""
}

func tifIn(v uint8) string {
	switch v {
	case TIFDay:
		return orders.TIFDAY
	case TIFGTC:
		return orders.TIFGTC
	case TIFIOC:
		return orders.TIFIOC
	case TIFFOK:
		return orders.TIFFOK
	case TIFGTD:
		return orders.TIFGTD
	}
	return ""
}

func sideOut(v string) uint8 {
	if v == orders.SideSell {
		return SideSell
	}
	return SideBuy
}

func statusOut(v string) uint8 {
	switch v {
	case "PENDING", "RESERVED", "ACTIVE":
		return OrdStatusNew
	case "PARTIALLY_FILLED":
		return OrdStatusPartiallyFilled
	case "FILLED":
		return OrdStatusFilled
	case "CANCELLED":
		return OrdStatusCancelled
	case "REJECTED":
		return OrdStatusRejected
	case "EXPIRED":
		return OrdStatusExpired
	}
	return OrdStatusNew
}

// Handle decodes every complete frame in buf and returns the encoded
// response frames. A decode error aborts the stream with a trailing
// BusinessReject — malformed binary input is always answered, never
// silently dropped.
func (g *Gateway) Handle(ctx context.Context, sess *SessionInfo, buf []byte) ([]byte, error) {
	var out []byte
	for len(buf) > 0 {
		m, n, err := DecodeInbound(buf)
		if err != nil {
			out = EncodeMessage(out, BusinessReject{
				RejectReason: 1, RejectCode: RejMalformed,
				TransactTimeNs: uint64(g.now().UnixNano()),
			})
			return out, err
		}
		out = g.dispatch(ctx, sess, m, out)
		buf = buf[n:]
	}
	return out, nil
}

func (g *Gateway) dispatch(ctx context.Context, sess *SessionInfo, m Message, out []byte) []byte {
	switch v := m.(type) {
	case NewOrder:
		return g.onNewOrder(ctx, sess, &v, out)
	case CancelOrder:
		return g.onCancel(ctx, sess, &v, out)
	case ReplaceOrder:
		return g.onReplace(ctx, sess, &v, out)
	case Negotiate:
		// Negotiate is handled by the transport handshake before any
		// order frame; a mid-stream Negotiate is a protocol violation.
		return EncodeMessage(out, BusinessReject{
			ClOrdID:       ClOrdID{},
			RefTemplateID: TemplateNegotiate,
			RejectCode:    RejMalformed,
			TransactTimeNs: uint64(g.now().UnixNano()),
		})
	default:
		return EncodeMessage(out, BusinessReject{
			RejectCode: RejMalformed, TransactTimeNs: uint64(g.now().UnixNano()),
		})
	}
}

// drainBlocked enforces the Task 18.3.17 maintenance-drain rule: while
// a session is draining, only cancels pass — new/replace order flow is
// rejected so clients can flatten state and reconnect to the
// replacement endpoint named in the News advisories.
func (g *Gateway) drainBlocked(sess *SessionInfo, c ClOrdID, tmpl uint16, out []byte) ([]byte, bool) {
	if sess.Draining {
		return EncodeMessage(out, BusinessReject{
			ClOrdID:        c,
			RefTemplateID:  tmpl,
			RejectCode:     RejDraining,
			TransactTimeNs: uint64(g.now().UnixNano()),
		}), true
	}
	return out, false
}

func (g *Gateway) rejectExec(c ClOrdID, inst uint32, side uint8, code uint32, out []byte, tns uint64) []byte {
	return EncodeMessage(out, ExecutionReport{
		ClOrdID: c, InstrumentID: inst, Side: side,
		OrdStatus: OrdStatusRejected, ExecType: ExecTypeRejected,
		RejectCode: code, TransactTimeNs: tns,
	})
}

func (g *Gateway) sessionAccount(ctx context.Context, sess *SessionInfo) (*orders.Account, error) {
	acct, err := g.Orders.AccountByID(ctx, sess.AccountID)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, excerrors.New("SESSION_NOT_ENTITLED", "bound account not found")
	}
	return acct, nil
}

func (g *Gateway) onNewOrder(ctx context.Context, sess *SessionInfo, m *NewOrder, out []byte) []byte {
	tns := uint64(g.now().UnixNano())
	if o, blocked := g.drainBlocked(sess, m.ClOrdID, TemplateNewOrder, out); blocked {
		return o
	}
	if m.AccountID != 0 && int64(m.AccountID) != sess.AccountID {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejNotEntitled, out, tns)
	}
	if !sess.Entitled(m.InstrumentID) {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejNotEntitled, out, tns)
	}
	side := sideIn(m.Side)
	ordType := ordTypeIn(m.OrdType)
	tif := tifIn(m.TimeInForce)
	if side == "" || ordType == "" || tif == "" {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejMalformed, out, tns)
	}
	inst, err := g.Instruments.InstrumentByID(ctx, int64(m.InstrumentID))
	if err != nil || inst == nil {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejUnknownInstrument, out, tns)
	}
	qty := mantToDec(m.Qty)
	if qty == nil || !qty.IsPositive() {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejMalformed, out, tns)
	}
	req := &orders.SubmitRequest{
		Symbol:        inst.Symbol,
		Side:          side,
		OrderType:     ordType,
		TimeInForce:   tif,
		ClientOrderID: m.ClOrdID.Str(),
		Quantity:      qty,
		Price:         mantToDec(m.Price),
		StopPrice:     mantToDec(m.StopPrice),
		DisplayQty:    mantToDec(m.DisplayQty),
		PostOnly:      m.Flags&FlagPostOnly != 0,
		ReduceOnly:    m.Flags&FlagReduceOnly != 0,
		Hidden:        m.Flags&FlagHidden != 0,
		SessionID:     sess.ID,
	}
	if m.ExpireTimeNs != 0 {
		t := time.Unix(0, int64(m.ExpireTimeNs))
		req.GTDExpiry = &t
	}
	acct, err := g.sessionAccount(ctx, sess)
	if err != nil {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejNotEntitled, out, tns)
	}
	ack, err := g.Orders.Submit(ctx, acct, req)
	if err != nil {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side,
			mapRejectCode(err), out, tns)
	}
	return EncodeMessage(out, ExecutionReport{
		OrderID:    uint64(ack.OrderID),
		ClOrdID:    m.ClOrdID,
		InstrumentID: m.InstrumentID,
		OrdStatus:  statusOut(ack.Status),
		ExecType:   ExecTypeNew,
		Side:       m.Side,
		Price:      decToMant(req.Price),
		TransactTimeNs: tns,
	})
}

// resolveOrderID finds the internal order id: explicit OrderID wins;
// else OrigClOrdID resolves through the dedup index.
func (g *Gateway) resolveOrderID(ctx context.Context, sess *SessionInfo,
	orderID uint64, orig ClOrdID) (int64, error) {
	if orderID != 0 {
		return int64(orderID), nil
	}
	s := orig.Str()
	if s == "" || g.ClientIDs == nil {
		return 0, fmt.Errorf("no order id and no origClOrdId resolver")
	}
	row, err := g.ClientIDs.DedupLookup(ctx, sess.AccountID, s)
	if err != nil {
		return 0, err
	}
	if row == nil {
		return 0, excerrors.New("ORDER_NOT_FOUND", fmt.Sprintf("origClOrdId %q unknown", s))
	}
	return row.OrderID, nil
}

func (g *Gateway) onCancel(ctx context.Context, sess *SessionInfo, m *CancelOrder, out []byte) []byte {
	tns := uint64(g.now().UnixNano())
	// Cancels pass during maintenance drains (Task 18.3.17 "preserve
	// cancels").
	if m.AccountID != 0 && int64(m.AccountID) != sess.AccountID {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejNotEntitled, out, tns)
	}
	orderID, err := g.resolveOrderID(ctx, sess, m.OrderID, m.OrigClOrdID)
	if err != nil {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejOrderNotFound, out, tns)
	}
	acct, err := g.sessionAccount(ctx, sess)
	if err != nil {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejNotEntitled, out, tns)
	}
	ack, err := g.Orders.Cancel(ctx, acct, orderID, "fixsbe:"+sess.ID,
		m.ClOrdID.Str(), "")
	if err != nil {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side,
			mapRejectCode(err), out, tns)
	}
	return EncodeMessage(out, ExecutionReport{
		OrderID:    uint64(ack.OrderID),
		ClOrdID:    m.ClOrdID,
		InstrumentID: m.InstrumentID,
		OrdStatus:  statusOut(ack.Status),
		ExecType:   ExecTypeCancelled,
		Side:       m.Side,
		TransactTimeNs: tns,
	})
}

func (g *Gateway) onReplace(ctx context.Context, sess *SessionInfo, m *ReplaceOrder, out []byte) []byte {
	tns := uint64(g.now().UnixNano())
	if o, blocked := g.drainBlocked(sess, m.ClOrdID, TemplateReplaceOrder, out); blocked {
		return o
	}
	if m.AccountID != 0 && int64(m.AccountID) != sess.AccountID {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejNotEntitled, out, tns)
	}
	orderID, err := g.resolveOrderID(ctx, sess, m.OrderID, m.OrigClOrdID)
	if err != nil {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejOrderNotFound, out, tns)
	}
	tif := tifIn(m.TimeInForce)
	req := &orders.CancelReplaceRequest{
		Mode: "STOP_ON_FAILURE",
		ModifyRequest: orders.ModifyRequest{
			Price:      mantToDec(m.Price),
			Quantity:   mantToDec(m.Qty),
			StopPrice:  mantToDec(m.StopPrice),
			DisplayQty: mantToDec(m.DisplayQty),
		},
	}
	if m.TimeInForce != TIFDay {
		req.TimeInForce = tif
	}
	if m.ExpireTimeNs != 0 {
		t := time.Unix(0, int64(m.ExpireTimeNs))
		req.GTDExpiry = &t
	}
	acct, err := g.sessionAccount(ctx, sess)
	if err != nil {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side, RejNotEntitled, out, tns)
	}
	o, err := g.Orders.CancelReplace(ctx, acct, orderID, req,
		"fixsbe:"+sess.ID, m.ClOrdID.Str(), "")
	if err != nil {
		return g.rejectExec(m.ClOrdID, m.InstrumentID, m.Side,
			mapRejectCode(err), out, tns)
	}
	return EncodeMessage(out, ExecutionReport{
		OrderID:      uint64(o.ID),
		ClOrdID:      m.ClOrdID,
		InstrumentID: m.InstrumentID,
		OrdStatus:    statusOut(o.Status),
		ExecType:     ExecTypeReplaced,
		Side:         sideOut(o.Side),
		Price:        decToMant(o.Price),
		TransactTimeNs: tns,
	})
}

// mapRejectCode folds a pipeline error code into the wire reject
// enumeration.
func mapRejectCode(err error) uint32 {
	switch excerrors.CodeOf(err) {
	case "SESSION_NOT_ENTITLED":
		return RejNotEntitled
	case "ORDER_NOT_FOUND":
		return RejOrderNotFound
	case "SESSION_THROTTLED":
		return RejThrottled
	case "INVALID_REQUEST", "VALIDATION_FAILED":
		return RejMalformed
	}
	return RejOther
}
