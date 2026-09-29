// Phase-16 Task 16.3.20 — OPO / OPOCO net-proceeds order lists (spec
// §5.39 migration-075, §6.10, §24 #287). A list binds one WORKING BUY
// leg to one (OPO) or two (OPOCO) pending SELL legs. When the working
// leg fills, its net received base quantity — fill quantity less
// commission, floored to the instrument lot size — becomes the pending
// SELL quantity; the received quantity is "locked" as proceeds on the
// list row and the sub-lot residue is released at activation.
//
// The pending legs are validated only at activation (the spec defers
// pending filters until the working order fully fills), placed through
// the standard order pipeline in one transaction with the list state
// flip (ActivatePendingTx), and OPOCO links the pair through the
// Phase-14 OCO machinery. Recovery replays from the durable rows —
// list state is fully reconstructable from orders + fills.
package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/config"
	"exchange/internal/ipc"
	"exchange/internal/risk"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// OrderListAck is the client-visible list acknowledgement.
type OrderListAck struct {
	ListID          int64  `json:"list_id"`
	ContingencyType string `json:"contingency_type"`
	Working         *Ack   `json:"working"`
	Status          string `json:"status"` // list state
	TransactTime    string `json:"transact_time"`
}

// netPendingQty computes the pending SELL size from the working BUY's
// net received base: gross = filled − commission(base), then floored to
// the instrument lot size. residue = gross − net is the sub-lot
// remainder released (unlocked) at activation.
func netPendingQty(filledQty, commissionBase, lotSize decimal.Decimal) (net, residue decimal.Decimal) {
	gross := filledQty.Sub(commissionBase)
	if !gross.IsPositive() {
		return decimal.Zero, decimal.Zero
	}
	net = gross
	if lotSize.IsPositive() {
		net = gross.Div(lotSize).Floor().Mul(lotSize)
	}
	return net, gross.Sub(net)
}

// SubmitOrderList validates, persists (one tx: list row + working
// order + leg rows) and dispatches the working leg. Pending legs get
// structural checks only — their filter validation is deferred to
// activation per spec (the pending quantity is recomputed then anyway).
func (s *Service) SubmitOrderList(ctx context.Context, acct *Account,
	req *SubmitOrderListRequest) (_ *OrderListAck, err error) {
	start := s.now()
	sym := ""
	defer func() {
		if s.admission != nil && sym != "" {
			s.admission(ctx, sym, s.now().Sub(start),
				err != nil && risk.SystemicAdmissionCode(excerrors.CodeOf(err)))
		}
	}()
	if s.composite == nil {
		return nil, codeErr("SERVICE_DEGRADED",
			"order-list persistence seam unavailable — submissions rejected (fail closed)")
	}
	if req == nil || req.Working == nil {
		return nil, codeErr("INVALID_REQUEST",
			"order list requires a working order and pending legs")
	}
	pendingN := 0
	switch req.ContingencyType {
	case ContingencyOPO:
		pendingN = 1
	case ContingencyOPOCO:
		pendingN = 2
	default:
		return nil, codeErr("INVALID_REQUEST",
			"contingency_type must be OPO or OPOCO, got %q", req.ContingencyType)
	}
	if len(req.Pending) != pendingN {
		return nil, codeErr("INVALID_REQUEST",
			"%s requires exactly %d pending leg(s), got %d",
			req.ContingencyType, pendingN, len(req.Pending))
	}
	w := req.Working
	if w.Symbol == "" {
		w.Symbol = req.Symbol
	}
	sym = config.CanonicalSymbol(w.Symbol)
	inst, err := s.store.InstrumentBySymbol(ctx, sym)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INVALID_REQUEST", "unknown symbol %q", req.Symbol)
	}
	// The §6.10 OPO contract pins the entry leg to BUY and the pending
	// legs to SELL — proceeds flow from the fill into the exits. Every
	// leg must be a continuous-matching wire type: session-queued
	// (MOO/MOC) and gateway-executor (FIXING) types have no engine
	// presence at dispatch time and would fabricate composites the
	// auction/fixing pipelines never see.
	if w.Side != SideBuy {
		return nil, codeErr("INVALID_REQUEST",
			"%s working leg must be BUY, got %q", req.ContingencyType, w.Side)
	}
	if IsAuctionType(w.OrderType) || w.OrderType == TypeFixing {
		return nil, codeErr("INVALID_REQUEST",
			"%s working leg cannot be session/gateway-bound type %q",
			req.ContingencyType, w.OrderType)
	}
	for i, leg := range req.Pending {
		if leg.Side != SideSell {
			return nil, codeErr("INVALID_REQUEST",
				"%s pending leg %d must be SELL, got %q",
				req.ContingencyType, i, leg.Side)
		}
		if IsAuctionType(leg.OrderType) || leg.OrderType == TypeFixing {
			return nil, codeErr("INVALID_REQUEST",
				"%s pending leg %d cannot be session/gateway-bound type %q",
				req.ContingencyType, i, leg.OrderType)
		}
		if leg.Symbol != "" && config.CanonicalSymbol(leg.Symbol) != sym {
			return nil, codeErr("INVALID_REQUEST",
				"pending leg %d symbol %q differs from working symbol %q",
				i, leg.Symbol, sym)
		}
		leg.Symbol = sym
		leg.SessionID = w.SessionID
	}
	if err := s.checkAdmission(ctx, acct, inst, w.SessionID, w.ReduceOnly); err != nil {
		return nil, err
	}
	ref, err := s.store.ReferencePrice(ctx, inst.ID)
	if err != nil {
		return nil, errInternal("reference price", err)
	}
	if err := ValidateSubmit(w, inst, acct, ref, s.now()); err != nil {
		return nil, err
	}
	// Quote-denominated working leg converts exactly like Submit.
	if w.QuoteQuantity != nil {
		if ref == nil || !ref.IsPositive() {
			return nil, codeErr("QUOTE_QUANTITY_INVALID",
				"no reference price for quote-denominated conversion on %s", inst.Symbol)
		}
		base := w.QuoteQuantity.Div(*ref)
		if inst.LotSize.IsPositive() {
			base = base.Div(inst.LotSize).Floor().Mul(inst.LotSize)
		}
		if !base.IsPositive() {
			return nil, codeErr("QUOTE_QUANTITY_INVALID",
				"quote_quantity %s converts to zero base at %s", *w.QuoteQuantity, *ref)
		}
		q := base
		w.Quantity = &q
	}
	if s.limits != nil {
		evalPrice := decimal.Zero
		if w.Price != nil {
			evalPrice = *w.Price
		} else if ref != nil {
			evalPrice = *ref
		}
		if err := s.limits.CheckOrder(ctx, risk.OrderRequest{
			AccountID: acct.ID, KycTier: acct.KycTier, Symbol: inst.Symbol,
			Side: w.Side, Quantity: *w.Quantity, Price: evalPrice,
			ReduceOnly: w.ReduceOnly,
		}); err != nil {
			return nil, err
		}
	}
	if err := s.checkBalance(ctx, acct, inst, w, ref); err != nil {
		return nil, err
	}
	// Structural pending checks only — filters re-run at activation.
	for i, leg := range req.Pending {
		if leg.OrderType == "" {
			return nil, codeErr("INVALID_REQUEST",
				"pending leg %d requires a type", i)
		}
		if len(leg.ClientOrderID) > 64 {
			return nil, codeErr("INVALID_REQUEST",
				"pending leg %d client_order_id exceeds 64 chars", i)
		}
		if leg.ClientOrderID == "" {
			leg.ClientOrderID = fmt.Sprintf("ol{list}.p%d", i+1)
		}
	}

	shard := s.shardFor(sym)
	seq := s.seq.Next()
	hash := ""
	if req.ClientOrderID != "" {
		hash = orderListHash(inst.ID, req)
	}
	legs := []OrderListLeg{{
		LegIndex: 0, Role: "WORKING", State: "PLACED",
		Params: mustJSON(submitToJSON(w)),
	}}
	for i, leg := range req.Pending {
		raw, err := json.Marshal(submitToJSON(leg))
		if err != nil {
			return nil, errInternal("pending leg encode", err)
		}
		legs = append(legs, OrderListLeg{
			LegIndex: i + 1, Role: "PENDING", State: "PENDING", Params: raw,
		})
	}
	list, o, err := s.composite.InsertOrderListTx(ctx, &OrderList{
		AccountID: acct.ID, InstrumentID: inst.ID,
		ContingencyType: req.ContingencyType, ClientOrderID: req.ClientOrderID,
	}, InsertParams{
		AccountID: acct.ID, InstrumentID: inst.ID,
		ClientOrderID: w.ClientOrderID, Side: w.Side,
		OrderType: w.OrderType, Quantity: *w.Quantity,
		QuoteQuantity: w.QuoteQuantity, Price: w.Price,
		StopPrice: w.StopPrice, DisplayQty: w.DisplayQty,
		TimeInForce: w.TimeInForce, ShardID: int(shard),
		OrderSeq: seq, PostOnly: w.PostOnly, ReduceOnly: w.ReduceOnly,
		STPMode: w.STPMode, SessionID: w.SessionID, RequestHash: hash,
	}, legs)
	if err != nil {
		if c := DedupConflictRow(err); c != nil {
			if c.RequestHash == hash {
				stored, gerr := s.store.GetOrder(ctx, c.OrderID)
				if gerr != nil {
					return nil, errInternal("dedup replay fetch", gerr)
				}
				if stored == nil {
					return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
						"client_order_id %q resolves to a missing order", w.ClientOrderID)
				}
				sl, _, lerr := s.composite.OrderListByWorking(ctx, stored.ID)
				if lerr != nil || sl == nil {
					return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
						"client_order_id %q does not resolve to an order list",
						w.ClientOrderID)
				}
				return &OrderListAck{ListID: sl.ID,
					ContingencyType: sl.ContingencyType,
					Working: &Ack{OrderID: stored.ID,
						ClientOrderID: stored.ClientOrderID,
						Status:        stored.Status, OrderSeq: stored.OrderSeq,
						Replay:       true,
						TransactTime: s.now().UTC().Format(time.RFC3339Nano)},
					Status:       sl.State,
					TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
			}
			return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
				"client_order_id %q already used with a different payload",
				c.ClientOrderID)
		}
		return nil, errInternal("order-list insert", err)
	}

	if s.sub != nil {
		bldr := flatbuffers.NewBuilder(256)
		payload := ipc.EncodeOrderNewEvent(bldr, seq,
			uint64(s.now().UnixNano()), orderNewMsg(o, acct, w))
		if err := s.sub.Send(ctx, shard, payload); err != nil {
			_ = s.store.MarkRejected(ctx, o.ID)
			_, _ = s.composite.SetOrderListState(ctx, list.ID,
				[]string{ListStateExecuting}, ListStateFailed, "WORKING_DISPATCH_FAILED")
			return nil, err
		}
		_ = s.store.MarkActive(ctx, o.ID)
	}
	return &OrderListAck{
		ListID:          list.ID,
		ContingencyType: list.ContingencyType,
		Working: &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
			Status: "ACTIVE", OrderSeq: seq,
			TransactTime: s.now().UTC().Format(time.RFC3339Nano)},
		Status:       ListStateExecuting,
		TransactTime: s.now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func orderListHash(instID int64, req *SubmitOrderListRequest) string {
	var w strings.Builder
	w.WriteString("orderlist|")
	w.WriteString(req.ContingencyType)
	w.WriteByte('|')
	w.WriteString(submitHash(instID, req.Working))
	for _, leg := range req.Pending {
		w.WriteByte('|')
		w.WriteString(leg.Side)
		w.WriteByte('|')
		w.WriteString(leg.OrderType)
		w.WriteByte('|')
		w.WriteString(decStr(leg.Price))
		w.WriteByte('|')
		w.WriteString(decStr(leg.StopPrice))
	}
	sum := sha256.Sum256([]byte(w.String()))
	return hex.EncodeToString(sum[:])
}

// submitToJSON snapshots a pending leg for the params JSONB column —
// replayed verbatim at activation.
func submitToJSON(r *SubmitRequest) map[string]any {
	m := map[string]any{
		"symbol":          r.Symbol,
		"side":            r.Side,
		"type":            r.OrderType,
		"time_in_force":   r.TimeInForce,
		"client_order_id": r.ClientOrderID,
	}
	if r.Quantity != nil {
		m["quantity"] = r.Quantity.String()
	}
	if r.Price != nil {
		m["price"] = r.Price.String()
	}
	if r.StopPrice != nil {
		m["stop_price"] = r.StopPrice.String()
	}
	if r.TimeInForce == TIFGTD && r.GTDExpiry != nil {
		m["gtd_expiry"] = r.GTDExpiry.UTC().Format(time.RFC3339)
	}
	if r.STPMode != "" {
		m["stp_mode"] = r.STPMode
	}
	if r.ReduceOnly {
		m["reduce_only"] = true
	}
	return m
}

// legFromParams rehydrates a pending leg's SubmitRequest snapshot.
func legFromParams(raw []byte) (*SubmitRequest, error) {
	r, err := ParseSubmit(raw)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ---------------------------------------------------------------------------
// Activation — working-leg fill → net-proceeds pending placement
// ---------------------------------------------------------------------------

// onListWorkingFill runs the net-proceeds cascade for one EXECUTING
// list whose working order just filled (fully — partial fills do not
// activate pending legs; spec #287 places on the completed working).
func (s *Service) onListWorkingFill(ctx context.Context, l *OrderList,
	legs []OrderListLeg, parent *Order) {
	inst, err := s.store.InstrumentByID(ctx, l.InstrumentID)
	if err != nil || inst == nil {
		return
	}
	acct, err := s.store.AccountByID(ctx, l.AccountID)
	if err != nil || acct == nil {
		return
	}
	if parent == nil {
		parent, err = s.store.GetOrder(ctx, l.WorkingOrderID)
		if err != nil || parent == nil {
			return
		}
	}
	// Activation fires on the completed working leg only.
	if parent.FilledQty.LessThan(parent.Quantity) || !parent.FilledQty.IsPositive() {
		return
	}

	// Net received base = filled qty − commission charged in base.
	commBase := decimal.Zero
	if s.commission != nil {
		avg := decimal.Zero
		if parent.AvgFillPrice != nil {
			avg = *parent.AvgFillPrice
		}
		fee, cerr := s.commission(ctx, acct.ID,
			parent.FilledQty, inst.LotSize, parent.FilledQty.Mul(avg))
		if cerr == nil && fee.IsPositive() && avg.IsPositive() {
			commBase = fee.Div(avg)
		}
	}
	net, _ := netPendingQty(parent.FilledQty, commBase, inst.LotSize)
	if !net.IsPositive() || net.LessThan(inst.MinOrderQty) {
		_, _ = s.composite.SetOrderListState(ctx, l.ID,
			[]string{ListStateExecuting}, ListStateFailed,
			"NET_PROCEEDS_INSUFFICIENT")
		return
	}

	// Rehydrate + activate each pending leg with the computed qty —
	// the deferred filter validation runs NOW against the true
	// quantity (spec: pending filters apply only once the working leg
	// fully fills).
	ref, _ := s.store.ReferencePrice(ctx, inst.ID)
	pendingParams := make([]InsertParams, 0, len(legs)-1)
	pendingReqs := make([]*SubmitRequest, 0, len(legs)-1)
	for _, leg := range legs {
		if leg.Role != "PENDING" {
			continue
		}
		r, err := legFromParams(leg.Params)
		if err != nil {
			_, _ = s.composite.SetOrderListState(ctx, l.ID,
				[]string{ListStateExecuting}, ListStateFailed,
				"PENDING_PARAMS_INVALID")
			return
		}
		// Derived client id when the snapshot carries none — the
		// placeholder "ol{list}.pN" resolves against the real list id.
		if r.ClientOrderID == "" || strings.HasPrefix(r.ClientOrderID, "ol{list}") {
			r.ClientOrderID = fmt.Sprintf("ol%d.p%d", l.ID, leg.LegIndex)
		}
		q := net
		r.Quantity = &q
		if err := ValidateSubmit(r, inst, acct, ref, s.now()); err != nil {
			_, _ = s.composite.SetOrderListState(ctx, l.ID,
				[]string{ListStateExecuting}, ListStateFailed,
				excerrors.CodeOf(err))
			return
		}
		pendingParams = append(pendingParams, s.insertParamsFor(acct, inst, r, parent.ShardID))
		pendingReqs = append(pendingReqs, r)
	}
	var ocoGroup *int64
	if l.ContingencyType == ContingencyOPOCO {
		g := int64(s.seq.Next())
		ocoGroup = &g
	}
	placed, ok, err := s.composite.ActivatePendingTx(ctx, l.ID,
		parent.FilledQty, net, ocoGroup, pendingParams)
	if err != nil || !ok {
		return // replay or persistence failure
	}
	if s.sub == nil {
		return
	}
	shard := uint16(0)
	if parent.ShardID != nil {
		shard = uint16(*parent.ShardID)
	}
	// OPOCO wires the pair through the OCO contract — OcoLink before
	// either OrderNew on the shard ring.
	if ocoGroup != nil && len(placed) == 2 {
		b := flatbuffers.NewBuilder(512)
		if err := s.sub.Send(ctx, shard,
			EncodeOcoLinkEvent(b, s.seq.Next(), uint64(s.now().UnixNano()),
				uint64(*ocoGroup), uint64(placed[0].ID), uint64(placed[1].ID),
				uint64(acct.ID), uint32(inst.ID))); err != nil {
			s.failPendingDispatch(ctx, l.ID, placed)
			return
		}
	}
	for i, o := range placed {
		nb := flatbuffers.NewBuilder(256)
		payload := ipc.EncodeOrderNewEvent(nb, s.seq.Next(),
			uint64(s.now().UnixNano()), orderNewMsg(o, acct, pendingReqs[i]))
		if err := s.sub.Send(ctx, shard, payload); err != nil {
			s.failPendingDispatch(ctx, l.ID, placed)
			return
		}
		_ = s.store.MarkActive(ctx, o.ID)
	}
}

// failPendingDispatch compensates a placed pending leg whose wire send
// failed — the engine never saw it, so neither must PG claim it live.
func (s *Service) failPendingDispatch(ctx context.Context, listID int64, placed []*Order) {
	for _, o := range placed {
		_ = s.store.MarkRejected(ctx, o.ID)
	}
	_, _ = s.composite.SetOrderListState(ctx, listID,
		[]string{ListStateAllDone}, ListStateFailed, "PENDING_DISPATCH_FAILED")
}

// onLegCancel reacts to a cancel event on a list-linked order: a
// working-leg cancel closes the list; a pending-leg cancel marks the
// leg, and once every pending leg is terminal the list closes
// CANCELLED (proceeds rows stay for the audit trail).
func (s *Service) onLegCancel(ctx context.Context, l *OrderList,
	legs []OrderListLeg, orderID int64) {
	for _, leg := range legs {
		if leg.OrderID == nil || *leg.OrderID != orderID {
			continue
		}
		if leg.Role == "WORKING" {
			_, _ = s.composite.SetOrderListState(ctx, l.ID,
				[]string{ListStateExecuting, ListStateAllDone},
				ListStateCancelled, "")
			return
		}
		// PENDING leg cancelled — mark the leg row; the list closes once
		// no pending leg remains live. For OPOCO the OCO sibling cancel
		// lands as a second event on the same path (idempotent).
		_ = s.composite.ListLegPlaced(ctx, leg.ID, orderID, "CANCELLED")
		allDead := true
		for _, other := range legs {
			if other.Role != "PENDING" || other.OrderID == nil ||
				other.ID == leg.ID {
				continue
			}
			if o, err := s.store.GetOrder(ctx, *other.OrderID); err == nil &&
				o != nil && !isTerminal(o.Status) {
				allDead = false
			}
		}
		if allDead {
			_, _ = s.composite.SetOrderListState(ctx, l.ID,
				[]string{ListStateAllDone}, ListStateCancelled, "")
		}
		return
	}
}

// CancelOrderList implements DELETE /order-lists/{id}: the working
// order (if live) and every live pending order cancel through their
// owning paths, then the list flips CANCELLED — the residue above the
// pending quantity was never locked anywhere else (proceeds bookkeeping
// is ledger-side only; no direct balance mutation, §5.3 boundary).
func (s *Service) CancelOrderList(ctx context.Context, acct *Account,
	listID int64, actor, requestID, ip string) (*OrderList, error) {
	if s.composite == nil {
		return nil, codeErr("SERVICE_DEGRADED",
			"order-list persistence seam unavailable")
	}
	l, legs, err := s.composite.OrderListGet(ctx, listID)
	if err != nil {
		return nil, errInternal("order-list read", err)
	}
	if l == nil || l.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order list %d not found", listID)
	}
	if l.State != ListStateExecuting && l.State != ListStateAllDone {
		return l, nil // already terminal — idempotent
	}
	// Cancel the working leg while it is still open.
	if wo, err := s.store.GetOrder(ctx, l.WorkingOrderID); err == nil &&
		wo != nil && !isTerminal(wo.Status) {
		if IsAuctionType(wo.OrderType) {
			_ = s.store.ApplyCancel(ctx, wo.ID)
		} else if err := s.cancelOne(ctx, wo); err != nil {
			return nil, err
		}
	}
	// Cancel live pending legs.
	for _, leg := range legs {
		if leg.OrderID == nil {
			continue
		}
		o, err := s.store.GetOrder(ctx, *leg.OrderID)
		if err != nil || o == nil || isTerminal(o.Status) {
			continue
		}
		if err := s.cancelOne(ctx, o); err != nil {
			return nil, err
		}
	}
	_, _ = s.composite.SetOrderListState(ctx, l.ID,
		[]string{ListStateExecuting, ListStateAllDone},
		ListStateCancelled, "")
	_ = s.store.WriteAudit(ctx, []AuditEntry{{
		OrderID: l.WorkingOrderID, AccountID: acct.ID,
		Operation: "ORDER_LIST_CANCEL", FieldName: "state",
		OldValue: l.State, NewValue: ListStateCancelled,
		ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
	}})
	l.State = ListStateCancelled
	return l, nil
}

// ---------------------------------------------------------------------------
// Reads — the §6.10 composite-list query endpoints (Task 16.3.24)
// ---------------------------------------------------------------------------

// OrderListView serializes a list row for the query endpoints — legs
// ride along on the detail variant only.
func OrderListView(l *OrderList) map[string]any {
	v := map[string]any{
		"list_id":          l.ID,
		"account_id":       l.AccountID,
		"instrument_id":    l.InstrumentID,
		"contingency_type": l.ContingencyType,
		"state":            l.State,
		"working_order_id": l.WorkingOrderID,
		"locked_proceeds":  l.LockedProceeds.String(),
		"net_pending_qty":  l.NetPendingQty.String(),
		"created_at":       l.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":       l.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if l.ClientOrderID != "" {
		v["client_order_id"] = l.ClientOrderID
	}
	if l.FailReason != "" {
		v["fail_reason"] = l.FailReason
	}
	if l.ClosedAt != nil {
		v["closed_at"] = l.ClosedAt.UTC().Format(time.RFC3339Nano)
	}
	return v
}

// OrderListLegView serializes one leg row.
func OrderListLegView(l *OrderListLeg) map[string]any {
	v := map[string]any{
		"leg_index":  l.LegIndex,
		"role":       l.Role,
		"state":      l.State,
		"created_at": l.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if l.OrderID != nil {
		v["order_id"] = *l.OrderID
	}
	if len(l.Params) > 0 {
		var m map[string]any
		if json.Unmarshal(l.Params, &m) == nil {
			v["params"] = m
		}
	}
	return v
}

// OrderLists serves GET /order-lists (open) and /order-lists/history —
// cursor-paged per the shared envelope.
func (s *Service) OrderLists(ctx context.Context, acctID int64, openOnly bool,
	cursorAt time.Time, cursorID int64, limit int) ([]OrderList, int64, error) {
	if s.composite == nil {
		return nil, 0, codeErr("SERVICE_DEGRADED",
			"order-list persistence seam unavailable")
	}
	rows, total, err := s.composite.OrderListsPage(ctx, acctID, openOnly,
		cursorAt, cursorID, limit)
	if err != nil {
		return nil, 0, errInternal("order-lists query", err)
	}
	return rows, total, nil
}

// OrderListDetail serves GET /order-lists/{id} — account-scoped.
func (s *Service) OrderListDetail(ctx context.Context, acct *Account,
	listID int64) (*OrderList, []OrderListLeg, error) {
	if s.composite == nil {
		return nil, nil, codeErr("SERVICE_DEGRADED",
			"order-list persistence seam unavailable")
	}
	l, legs, err := s.composite.OrderListGet(ctx, listID)
	if err != nil {
		return nil, nil, errInternal("order-list read", err)
	}
	if l == nil || l.AccountID != acct.ID {
		return nil, nil, codeErr("ORDER_NOT_FOUND",
			"order list %d not found", listID)
	}
	return l, legs, nil
}
