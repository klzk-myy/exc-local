// Phase-16 Task 16.3.14 — bracket/OTO composite orders (spec §6.2,
// §24 #87). This file is the SOLE owner of bracket logic (the older
// Task 16.3.5 entry is superseded).
//
// A bracket is a parent entry order plus SL/TP child configuration
// persisted atomically via InsertOrderBracketTx. Parent fills —
// including partials — spawn proportional SL+TP children through the
// Phase-14 OCO machinery (shared oco_group_id + OcoLink-before-OrderNew
// sequencing), so one child reaching terminal FILLED cancels the
// sibling atomically in-core (journaled reason-7 ORDER_CANCEL).
//
// Fill-vs-cancel determinism: placement runs inside
// PlaceBracketChildrenTx which locks the bracket row and computes
// delta = parent.filled_qty − placed_qty. A cancel landing mid-cascade
// flips state WORKING→CANCELLED under the same lock; a replayed fill
// sees delta ≤ 0 and places nothing — the ledger, not timing, owns the
// truth (WAL-compatible lifecycle: every edge is a durable row).
package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// BracketAck is the client-visible bracket acknowledgement.
type BracketAck struct {
	BracketID    int64  `json:"bracket_id"`
	Parent       *Ack   `json:"parent"`
	Status       string `json:"status"` // ACCEPTED | replayed parent status
	TransactTime string `json:"transact_time"`
}

// bracketChildCOID derives a deterministic per-pair client id so the
// placement dedup makes a retried fill event idempotent (≤64 chars).
func bracketChildCOID(bracketID, pairIndex int64, leg string) string {
	return fmt.Sprintf("brk%d.%d.%s", bracketID, pairIndex, leg)
}

// SubmitBracket validates all three legs, persists the parent + group
// atomically and dispatches only the parent — the children materialize
// on the first fill (Task 16.3.14 item 4). A parent rejection implies
// no group row and no children (the tx never ran).
func (s *Service) SubmitBracket(ctx context.Context, acct *Account,
	req *SubmitBracketRequest) (_ *BracketAck, err error) {
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
			"bracket persistence seam unavailable — submissions rejected (fail closed)")
	}
	if req == nil || req.Parent == nil {
		return nil, codeErr("INVALID_REQUEST",
			"bracket requires a parent order and child_sl/child_tp")
	}
	p := req.Parent
	sym = config.CanonicalSymbol(p.Symbol)
	inst, err := s.store.InstrumentBySymbol(ctx, sym)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INVALID_REQUEST", "unknown symbol %q", p.Symbol)
	}
	// Entry leg: only LIMIT or MARKET parents — protective children
	// cannot hang off conditional/algo parents (spec §6.2 schema).
	if p.OrderType != TypeLimit && p.OrderType != TypeMarket {
		return nil, codeErr("INVALID_REQUEST",
			"bracket parent must be LIMIT or MARKET, got %q", p.OrderType)
	}
	if err := s.checkAdmission(ctx, acct, inst, p.SessionID, p.ReduceOnly); err != nil {
		return nil, err
	}
	ref, err := s.store.ReferencePrice(ctx, inst.ID)
	if err != nil {
		return nil, errInternal("reference price", err)
	}
	if err := ValidateSubmit(p, inst, acct, ref, s.now()); err != nil {
		return nil, err
	}
	// Child-leg validation (item 3): every leg checks BEFORE
	// persistence — trigger required, optional limit positive.
	for name, c := range map[string]*BracketChild{
		"child_sl": &req.ChildSL, "child_tp": &req.ChildTP,
	} {
		if c.TriggerPrice == nil || !c.TriggerPrice.IsPositive() {
			return nil, codeErr("INVALID_REQUEST",
				"%s trigger_price must be positive", name)
		}
		if c.LimitPrice != nil && !c.LimitPrice.IsPositive() {
			return nil, codeErr("INVALID_REQUEST",
				"%s limit_price must be positive", name)
		}
		if len(c.ClientOrderID) > 64 {
			return nil, codeErr("INVALID_REQUEST",
				"%s client_order_id exceeds 64 chars", name)
		}
	}
	if s.limits != nil {
		evalPrice := decimal.Zero
		if p.Price != nil {
			evalPrice = *p.Price
		} else if ref != nil {
			evalPrice = *ref
		}
		if err := s.limits.CheckOrder(ctx, risk.OrderRequest{
			AccountID: acct.ID, KycTier: acct.KycTier, Symbol: inst.Symbol,
			Side: p.Side, Quantity: *p.Quantity, Price: evalPrice,
			ReduceOnly: p.ReduceOnly,
		}); err != nil {
			return nil, err
		}
	}
	if err := s.checkBalance(ctx, acct, inst, p, ref); err != nil {
		return nil, err
	}

	shard := s.shardFor(sym)
	seq := s.seq.Next()
	hash := ""
	if p.ClientOrderID != "" {
		hash = bracketHash(inst.ID, p, &req.ChildSL, &req.ChildTP)
	}
	o, b, err := s.composite.InsertOrderBracketTx(ctx, InsertParams{
		AccountID: acct.ID, InstrumentID: inst.ID,
		ClientOrderID: p.ClientOrderID, Side: p.Side,
		OrderType: p.OrderType, Quantity: *p.Quantity,
		QuoteQuantity: p.QuoteQuantity, Price: p.Price,
		StopPrice: p.StopPrice, DisplayQty: p.DisplayQty,
		TimeInForce: p.TimeInForce, ShardID: int(shard),
		OrderSeq: seq, PostOnly: p.PostOnly, ReduceOnly: p.ReduceOnly,
		STPMode: p.STPMode, SessionID: p.SessionID, RequestHash: hash,
	}, req.ChildSL, req.ChildTP, p.GTDExpiry)
	if err != nil {
		if c := DedupConflictRow(err); c != nil {
			if c.RequestHash == hash {
				stored, gerr := s.store.GetOrder(ctx, c.OrderID)
				if gerr != nil {
					return nil, errInternal("dedup replay fetch", gerr)
				}
				if stored == nil {
					return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
						"client_order_id %q resolves to a missing order", p.ClientOrderID)
				}
				sb, berr := s.composite.BracketByParent(ctx, stored.ID)
				if berr != nil || sb == nil {
					return nil, errInternal("bracket replay fetch",
						fmt.Errorf("%v", berr))
				}
				return &BracketAck{BracketID: sb.ID,
					Parent: &Ack{OrderID: stored.ID,
						ClientOrderID: stored.ClientOrderID,
						Status:        stored.Status, OrderSeq: stored.OrderSeq,
						Replay:       true,
						TransactTime: s.now().UTC().Format(time.RFC3339Nano)},
					Status:       "ACCEPTED",
					TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
			}
			return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
				"client_order_id %q already used with a different payload",
				p.ClientOrderID)
		}
		return nil, errInternal("bracket insert", err)
	}

	if s.sub != nil {
		bldr := flatbuffers.NewBuilder(256)
		payload := ipc.EncodeOrderNewEvent(bldr, seq,
			uint64(s.now().UnixNano()), orderNewMsg(o, acct, p))
		if err := s.sub.Send(ctx, shard, payload); err != nil {
			_ = s.store.MarkRejected(ctx, o.ID)
			_, _ = s.composite.SetBracketState(ctx, b.ID,
				[]string{BracketWorking}, BracketFailed)
			return nil, err
		}
		_ = s.store.MarkActive(ctx, o.ID)
	}
	return &BracketAck{
		BracketID: b.ID,
		Parent: &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
			Status: "ACTIVE", OrderSeq: seq,
			TransactTime: s.now().UTC().Format(time.RFC3339Nano)},
		Status:       "ACCEPTED",
		TransactTime: s.now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// bracketHash fingerprints the whole composite submission — a replayed
// parent client_order_id only replays when the child config matches.
func bracketHash(instID int64, p *SubmitRequest, sl, tp *BracketChild) string {
	var w strings.Builder
	w.WriteString("bracket|")
	w.WriteString(submitHash(instID, p))
	w.WriteString(decStr(sl.TriggerPrice))
	w.WriteByte('|')
	w.WriteString(decStr(sl.LimitPrice))
	w.WriteByte('|')
	w.WriteString(decStr(tp.TriggerPrice))
	w.WriteByte('|')
	w.WriteString(decStr(tp.LimitPrice))
	sum := sha256.Sum256([]byte(w.String()))
	return hex.EncodeToString(sum[:])
}

// childType maps a child config onto the wire order type: a limit
// price upgrades the stop to STOP_LIMIT.
func (c *BracketChild) childType() string {
	if c.LimitPrice != nil {
		return TypeStopLimit
	}
	return TypeStop
}

// childLeg builds the child SubmitRequest for placement + the wire
// encoder (side is the parent's opposite; expiry inherits parent GTD).
func (b *Bracket) childLeg(c *BracketChild, qty decimal.Decimal) *SubmitRequest {
	side := SideSell
	if b.ParentSide == SideSell {
		side = SideBuy
	}
	return &SubmitRequest{
		Side:      side,
		OrderType: c.childType(),
		Quantity:  &qty,
		Price:     c.LimitPrice,
		StopPrice: c.TriggerPrice,
		// Children inherit the parent GTD expiry; otherwise GTC
		// (Task 16.3.14 item 7).
		TimeInForce: ternaryTIF(b.GTDExpiry != nil),
		GTDExpiry:   b.GTDExpiry,
	}
}

func ternaryTIF(gtd bool) string {
	if gtd {
		return TIFGTD
	}
	return TIFGTC
}

// OnParentFill drives the fill cascade for one bracket: the uncovered
// fill delta (parent.filled_qty − placed_qty) materializes as a new
// SL/TP pair, dispatched OcoLink → OrderNew(SL) → OrderNew(TP) on the
// parent's shard — the same sequencing SubmitOCO guarantees (Task
// 16.3.14 items 4–6). Replay-safe by construction.
func (s *Service) onBracketFill(ctx context.Context, b *Bracket) {
	parent, err := s.store.GetOrder(ctx, b.ParentOrderID)
	if err != nil || parent == nil {
		return
	}
	acct, err := s.store.AccountByID(ctx, b.AccountID)
	if err != nil || acct == nil {
		return
	}
	inst, err := s.store.InstrumentByID(ctx, b.InstrumentID)
	if err != nil || inst == nil {
		return
	}
	// Delta is computed in-tx; the qty here only seeds the leg builder
	// (the tx overrides InsertParams.Quantity with the true delta).
	var probe decimal.Decimal = decimal.Zero
	slLeg := b.childLeg(&b.ChildSL, probe)
	tpLeg := b.childLeg(&b.ChildTP, probe)
	groupID := int64(s.seq.Next())
	sl, tp, err := s.composite.PlaceBracketChildrenTx(ctx, b.ID, groupID,
		bracketChildCOID(b.ID, int64(b.ChildSeq+1), "sl"),
		bracketChildCOID(b.ID, int64(b.ChildSeq+1), "tp"),
		s.insertParamsFor(acct, inst, slLeg, parent.ShardID),
		s.insertParamsFor(acct, inst, tpLeg, parent.ShardID))
	if err != nil || sl == nil {
		return // replay/no coverage or persistence failure — logged by caller convention
	}
	if s.sub != nil {
		shard := uint16(0)
		if parent.ShardID != nil {
			shard = uint16(*parent.ShardID)
		}
		ts := uint64(s.now().UnixNano())
		bldr := flatbuffers.NewBuilder(512)
		if err := s.sub.Send(ctx, shard,
			EncodeOcoLinkEvent(bldr, s.seq.Next(), ts, uint64(groupID),
				uint64(sl.ID), uint64(tp.ID), uint64(b.AccountID),
				uint32(b.InstrumentID))); err != nil {
			s.compensateChildren(ctx, b.ID, sl.ID, tp.ID)
			return
		}
		for _, leg := range []struct {
			o   *Order
			req *SubmitRequest
		}{{sl, slLeg}, {tp, tpLeg}} {
			nb := flatbuffers.NewBuilder(256)
			payload := ipc.EncodeOrderNewEvent(nb, s.seq.Next(),
				uint64(s.now().UnixNano()), orderNewMsg(leg.o, acct, leg.req))
			if err := s.sub.Send(ctx, shard, payload); err != nil {
				s.compensateChildren(ctx, b.ID, sl.ID, tp.ID)
				return
			}
			_ = s.store.MarkActive(ctx, leg.o.ID)
		}
	}
	// Parent fully covered → group FILLED once the parent itself is.
	if !parent.FilledQty.LessThan(parent.Quantity) {
		_, _ = s.composite.SetBracketState(ctx, b.ID,
			[]string{BracketWorking}, BracketFilled)
	}
}

// compensateChildren rejects a just-placed pair whose wire dispatch
// failed — an unlinked half-pair must never reach the engine as a
// standalone order (same compensation as SubmitOCO). The group row is
// marked FAILED so the recovery sweep does not loop on a broken pair.
func (s *Service) compensateChildren(ctx context.Context, bracketID, slID, tpID int64) {
	_ = s.store.MarkRejected(ctx, slID)
	_ = s.store.MarkRejected(ctx, tpID)
	_, _ = s.composite.SetBracketState(ctx, bracketID,
		[]string{BracketWorking}, BracketFailed)
}

// insertParamsFor builds the child InsertParams template — Quantity is
// a placeholder the placement tx overrides with the computed delta.
func (s *Service) insertParamsFor(acct *Account, inst *Instrument,
	leg *SubmitRequest, shardID *int) InsertParams {
	shard := 0
	if shardID != nil {
		shard = *shardID
	}
	qty := decimal.Zero
	if leg.Quantity != nil {
		qty = *leg.Quantity
	}
	return InsertParams{
		AccountID: acct.ID, InstrumentID: inst.ID,
		ClientOrderID: leg.ClientOrderID, Side: leg.Side,
		OrderType: leg.OrderType, Quantity: qty, Price: leg.Price,
		StopPrice: leg.StopPrice, TimeInForce: leg.TimeInForce,
		ShardID: shard, OrderSeq: s.seq.Next(), STPMode: leg.STPMode,
		SessionID:   leg.SessionID,
		RequestHash: submitHash(inst.ID, leg),
	}
}

// OnParentCancel sweeps the bracket when the parent dies: the group
// flips CANCELLED under a state CAS and every engine-live child pair is
// sent an OrderCancel (the consumer's OrderCancel echo applies the read
// model; reason-7 OCO sibling cancels ride the same seam).
func (s *Service) onBracketCancel(ctx context.Context, b *Bracket) {
	ok, err := s.composite.SetBracketState(ctx, b.ID,
		[]string{BracketWorking, BracketFilled}, BracketCancelled)
	if err != nil || !ok {
		return
	}
	childIDs, err := s.composite.BracketOpenChildIDs(ctx, b.ID)
	if err != nil {
		return
	}
	for _, id := range childIDs {
		o, err := s.store.GetOrder(ctx, id)
		if err != nil || o == nil || isTerminal(o.Status) {
			continue
		}
		// Fire-and-forget wire cancel — the out-ring echo applies the
		// read model; a nil submitter applies locally (test/dev).
		if s.sub == nil {
			_ = s.store.ApplyCancel(ctx, id)
			continue
		}
		shard := uint16(0)
		if o.ShardID != nil {
			shard = uint16(*o.ShardID)
		}
		bldr := flatbuffers.NewBuilder(128)
		_ = s.sub.Send(ctx, shard, EncodeCancelEvent(bldr, s.seq.Next(),
			uint64(s.now().UnixNano()), uint64(o.ID), uint64(o.AccountID)))
	}
}
