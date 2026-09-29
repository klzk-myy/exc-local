// Order entry: 35=D / 35=F / 35:G dispatch onto the canonical
// orders.Service pipeline (Task 18.3.2). Every verdict emits an
// ExecutionReport or CancelReject through the report bus — nothing is
// acknowledged silently and nothing is rejected without a wire answer.
package fix

import (
	"context"
	"strconv"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/orders"
	excerrors "exchange/pkg/errors"
)

// resolveTargetOrder finds the order an F/G names: OrigClOrdID(41)
// resolves through the dedup index; OrderID(37) is honoured when the
// client supplies it instead (order id also lands in the audit).
func (a *App) resolveTargetOrder(ctx context.Context, row *Session,
	msg *quickfix.Message) (*orders.Order, string, *MappingError) {
	acct, err := a.sessionAccount(ctx, row)
	if err != nil {
		return nil, "", &MappingError{Code: excerrors.CodeOf(err),
			Detail: err.Error(), OrdReject: OrdRejReasonOther}
	}
	origClOrdID := optionalStr(msg, TagOrigClOrdID)
	if origClOrdID != "" {
		dup, derr := a.opt.OrderRead.DedupLookup(ctx, acct.ID, origClOrdID)
		if derr != nil {
			return nil, "", &MappingError{Code: "INTERNAL_ERROR",
				Detail:    derr.Error(),
				OrdReject: OrdRejReasonOther}
		}
		if dup == nil {
			return nil, "", &MappingError{Code: "ORDER_NOT_FOUND",
				Detail:    "OrigClOrdID(41) unknown",
				OrdReject: OrdRejReasonUnknownOrder}
		}
		o, gerr := a.opt.OrderRead.GetOrder(ctx, dup.OrderID)
		if gerr != nil {
			return nil, "", &MappingError{Code: "INTERNAL_ERROR",
				Detail: gerr.Error(), OrdReject: OrdRejReasonOther}
		}
		if o == nil || o.AccountID != acct.ID {
			return nil, "", &MappingError{Code: "ORDER_NOT_FOUND",
				Detail:    "order not found",
				OrdReject: OrdRejReasonUnknownOrder}
		}
		return o, origClOrdID, nil
	}
	if oidStr := optionalStr(msg, TagOrderID); oidStr != "" {
		oid, perr := strconv.ParseInt(oidStr, 10, 64)
		if perr != nil {
			return nil, "", &MappingError{Code: "INVALID_REQUEST",
				Detail:    "OrderID(37) is not numeric",
				OrdReject: OrdRejReasonOther}
		}
		o, gerr := a.opt.OrderRead.GetOrder(ctx, oid)
		if gerr != nil {
			return nil, "", &MappingError{Code: "INTERNAL_ERROR",
				Detail: gerr.Error(), OrdReject: OrdRejReasonOther}
		}
		if o == nil || o.AccountID != acct.ID {
			return nil, "", &MappingError{Code: "ORDER_NOT_FOUND",
				Detail:    "order not found",
				OrdReject: OrdRejReasonUnknownOrder}
		}
		return o, o.ClientOrderID, nil
	}
	return nil, "", &MappingError{Code: "INVALID_REQUEST",
		Detail:    "requires OrigClOrdID(41) or OrderID(37)",
		OrdReject: OrdRejReasonOther}
}

// onNewOrderSingle — 35=D → orders.Service.Submit.
func (a *App) onNewOrderSingle(ctx context.Context, msg *quickfix.Message,
	sessionID quickfix.SessionID, row *Session) {
	clOrdID := optionalStr(msg, TagClOrdID)
	symbol := optionalStr(msg, TagSymbol)
	side := optionalStr(msg, TagSide)

	if merr := a.checkEntitlement(ctx, row, msg, symbol); merr != nil {
		a.emit(sessionID, businessReject(MsgNewOrderSingle, clOrdID,
			merr.Code, BusinessRejectReasonNotEntitled))
		return
	}
	acct, err := a.sessionAccount(ctx, row)
	if err != nil {
		a.emit(sessionID, businessReject(MsgNewOrderSingle, clOrdID,
			excerrors.CodeOf(err), BusinessRejectReasonNotEntitled))
		return
	}
	// FIXT.1.1 sessions take the raw-bytes SP2 mapper (derivative tag
	// block + party groups); FIX.4.4 uses the FieldMap path.
	var req *orders.SubmitRequest
	var merr *MappingError
	if isSP2Session(sessionID) {
		req, merr = MapNewOrderSingleSP2([]byte(msg.String()), sessionID.String())
	} else {
		req, merr = MapNewOrderSingle(msg, sessionID.String())
	}
	if merr != nil {
		a.emit(sessionID, reportRejectedFor(sessionID, clOrdID, symbol, side,
			mapErrToErr(merr)))
		return
	}
	ack, err := a.opt.Orders.Submit(ctx, acct, req)
	if err != nil {
		a.emit(sessionID, reportRejectedFor(sessionID, clOrdID, symbol, side, err))
		return
	}
	o, _ := a.opt.OrderRead.GetOrder(ctx, ack.OrderID)
	a.bus.Emit(ReportEvent{SessionID: sessionID, OrderID: ack.OrderID,
		ClOrdID: clOrdID, Msg: reportAcceptedFor(sessionID, o, ack)})
}

// onOrderCancelRequest — 35=F → orders.Service.Cancel. Failures answer
// with 35=9 OrderCancelReject per spec §9.9.
func (a *App) onOrderCancelRequest(ctx context.Context, msg *quickfix.Message,
	sessionID quickfix.SessionID, row *Session) {
	clOrdID := optionalStr(msg, TagClOrdID)
	if row.AccountID == nil {
		a.emit(sessionID, businessReject(MsgOrderCancelRequest, clOrdID,
			"SESSION_NOT_ENTITLED", BusinessRejectReasonNotEntitled))
		return
	}
	o, origClOrdID, merr := a.resolveTargetOrder(ctx, row, msg)
	if merr != nil {
		reason := CxlRejReasonBrokerOption
		if merr.Code == "ORDER_NOT_FOUND" {
			reason = CxlRejReasonUnknownOrder
		}
		a.emit(sessionID, cancelReject(clOrdID, origClOrdID, "",
			CxlRejResponseToCancel, reason, merr.Code))
		return
	}
	acct, err := a.sessionAccount(ctx, row)
	if err != nil {
		a.emit(sessionID, businessReject(MsgOrderCancelRequest, clOrdID,
			excerrors.CodeOf(err), BusinessRejectReasonNotEntitled))
		return
	}
	ack, err := a.opt.Orders.Cancel(ctx, acct, o.ID,
		"fix:"+sessionID.String(), "", "")
	if err != nil {
		reason := CxlRejReasonBrokerOption
		if excerrors.CodeOf(err) == "ORDER_NOT_FOUND" {
			reason = CxlRejReasonUnknownOrder
		}
		a.emit(sessionID, cancelReject(clOrdID, origClOrdID,
			strconv.FormatInt(o.ID, 10), CxlRejResponseToCancel, reason,
			excerrors.CodeOf(err)))
		return
	}
	_ = ack // the hydrated row carries the terminal state for the report
	fresh, _ := a.opt.OrderRead.GetOrder(ctx, o.ID)
	if fresh == nil {
		fresh = o
	}
	a.bus.Emit(ReportEvent{SessionID: sessionID, OrderID: o.ID,
		ClOrdID: clOrdID, Msg: reportCanceledFor(sessionID, fresh, origClOrdID)})
}

// onOrderCancelReplace — 35=G → orders.Service.CancelReplace. The
// internal STALE_MODIFY fence is supplied from the freshly-read row;
// a CAS race surfaces as STALE_MODIFY → OrderCancelReject.
func (a *App) onOrderCancelReplace(ctx context.Context, msg *quickfix.Message,
	sessionID quickfix.SessionID, row *Session) {
	clOrdID := optionalStr(msg, TagClOrdID)
	if row.AccountID == nil {
		a.emit(sessionID, businessReject(MsgOrderCancelReplace, clOrdID,
			"SESSION_NOT_ENTITLED", BusinessRejectReasonNotEntitled))
		return
	}
	if merr := a.checkEntitlement(ctx, row, msg, optionalStr(msg, TagSymbol)); merr != nil {
		a.emit(sessionID, businessReject(MsgOrderCancelReplace, clOrdID,
			merr.Code, BusinessRejectReasonNotEntitled))
		return
	}
	o, origClOrdID, merr := a.resolveTargetOrder(ctx, row, msg)
	if merr != nil {
		reason := CxlRejReasonBrokerOption
		if merr.Code == "ORDER_NOT_FOUND" {
			reason = CxlRejReasonUnknownOrder
		}
		a.emit(sessionID, cancelReject(clOrdID, origClOrdID, "",
			CxlRejResponseToCancelReplace, reason, merr.Code))
		return
	}
	req, merr := MapCancelReplace(msg)
	if merr != nil {
		a.emit(sessionID, cancelReject(clOrdID, origClOrdID,
			strconv.FormatInt(o.ID, 10), CxlRejResponseToCancelReplace,
			CxlRejReasonBrokerOption, merr.Code))
		return
	}
	// Fence on the row we just locked-read: a racing mutation flips
	// order_seq and AmendCAS rejects — atomic, never silent.
	seq := o.OrderSeq
	req.OrderSeq = &seq
	acct, err := a.sessionAccount(ctx, row)
	if err != nil {
		a.emit(sessionID, businessReject(MsgOrderCancelReplace, clOrdID,
			excerrors.CodeOf(err), BusinessRejectReasonNotEntitled))
		return
	}
	updated, err := a.opt.Orders.CancelReplace(ctx, acct, o.ID, req,
		"fix:"+sessionID.String(), "", "")
	if err != nil {
		reason := CxlRejReasonBrokerOption
		code := excerrors.CodeOf(err)
		if code == "ORDER_NOT_FOUND" {
			reason = CxlRejReasonUnknownOrder
		}
		a.emit(sessionID, cancelReject(clOrdID, origClOrdID,
			strconv.FormatInt(o.ID, 10), CxlRejResponseToCancelReplace,
			reason, code))
		return
	}
	a.bus.Emit(ReportEvent{SessionID: sessionID, OrderID: o.ID,
		ClOrdID: clOrdID, Msg: reportReplacedFor(sessionID, updated, origClOrdID)})
}

func mapErrToErr(m *MappingError) error {
	if m == nil {
		return nil
	}
	return excerrors.New(m.Code, m.Detail)
}
