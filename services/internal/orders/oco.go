// Phase-14 Task 14.3.1 — OCO (one-cancels-other) pair submission
// (spec §6.2/§6.5, §24 #47): POST /api/v1/orders/oco.
//
// Contract: exactly two legs on one instrument and one account are
// persisted in a single transaction sharing oco_group_id; the engine is
// then sequenced OcoLink → OrderNew(A) → OrderNew(B) on the pair's shard
// ring — the SPSC FIFO guarantees the link is installed before either leg
// can be admitted, so whichever leg reaches terminal FILLED first cancels
// the sibling atomically in-core (journaled reason-7 ORDER_CANCEL). The
// trailing leg of a sibling race surfaces OCO_SIBLING_CANCEL_RACE (§23,
// HTTP 409): synchronously here when a dedup replay resolves to a pair the
// engine already closed by race, and asynchronously as the cancelled leg's
// terminal state otherwise.
package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/config"
	"exchange/internal/ipc"
	"exchange/internal/risk"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// SubmitOcoRequest is the normalized POST /orders/oco body: Symbol is the
// pair-level instrument default (legs may carry their own symbol but must
// canonicalize to the same instrument); Legs holds exactly two
// SubmitRequest payloads.
type SubmitOcoRequest struct {
	Symbol string
	Legs   [2]*SubmitRequest
}

// OcoAck is the client-visible pair acknowledgement — one OcoGroupID plus
// the per-leg submission acks.
type OcoAck struct {
	OcoGroupID   int64   `json:"oco_group_id"`
	Legs         [2]*Ack `json:"legs"`
	Status       string  `json:"status"` // "ACCEPTED" | replay
	TransactTime string  `json:"transact_time"`
}

// ocoPairHash is the dedup fingerprint for an OCO submission — identical
// for both legs so a retried request replays the whole pair and a
// client_order_id reused against a different pair still collides. The
// "oco|" prefix keeps a leg's client_order_id distinct from a plain
// single-order submission with the same fields.
func ocoPairHash(instID int64, a, b *SubmitRequest) string {
	var w strings.Builder
	w.WriteString("oco|")
	w.WriteString(strconv.FormatInt(instID, 10))
	for _, r := range [2]*SubmitRequest{a, b} {
		w.WriteByte('|')
		w.WriteString(r.Side)
		w.WriteByte('|')
		w.WriteString(r.OrderType)
		w.WriteByte('|')
		w.WriteString(r.TimeInForce)
		w.WriteByte('|')
		w.WriteString(decStr(r.Quantity))
		w.WriteByte('|')
		w.WriteString(decStr(r.QuoteQuantity))
		w.WriteByte('|')
		w.WriteString(decStr(r.Price))
		w.WriteByte('|')
		w.WriteString(decStr(r.StopPrice))
		w.WriteByte('|')
		w.WriteString(decStr(r.DisplayQty))
	}
	sum := sha256.Sum256([]byte(w.String()))
	return hex.EncodeToString(sum[:])
}

// SubmitOCO validates, persists and dispatches an OCO pair (spec §6.2/§6.5,
// §24 #47). Ordering contract: the persisted rows precede the wire link,
// and the wire link precedes both legs' OrderNew — a dispatch failure at
// any point marks BOTH legs REJECTED so the read model can never claim a
// leg the engine holds without linkage.
func (s *Service) SubmitOCO(ctx context.Context, acct *Account,
	req *SubmitOcoRequest) (_ *OcoAck, err error) {
	start := s.now()
	sym := ""
	defer func() {
		if s.admission != nil && sym != "" {
			s.admission(ctx, sym, s.now().Sub(start),
				err != nil && risk.SystemicAdmissionCode(excerrors.CodeOf(err)))
		}
	}()

	if req == nil || req.Legs[0] == nil || req.Legs[1] == nil {
		return nil, codeErr("INVALID_REQUEST", "oco pair requires exactly two legs")
	}
	legA, legB := req.Legs[0], req.Legs[1]
	if legA.Symbol == "" {
		legA.Symbol = req.Symbol
	}
	if legB.Symbol == "" {
		legB.Symbol = req.Symbol
	}
	symA := config.CanonicalSymbol(legA.Symbol)
	symB := config.CanonicalSymbol(legB.Symbol)
	sym = symA
	if symA == "" || symA != symB {
		return nil, codeErr("INVALID_REQUEST",
			"oco legs must share one instrument symbol")
	}
	inst, err := s.store.InstrumentBySymbol(ctx, symA)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INVALID_REQUEST", "unknown symbol %q", req.Symbol)
	}

	ref, err := s.store.ReferencePrice(ctx, inst.ID)
	if err != nil {
		return nil, errInternal("reference price", err)
	}
	legs := [2]*SubmitRequest{legA, legB}
	for _, leg := range legs {
		// Per-leg admission + validation mirror the single-order Submit
		// path — the pair admits only when both legs would admit alone.
		if err := s.checkAdmission(ctx, acct, inst, leg.SessionID,
			leg.ReduceOnly); err != nil {
			return nil, err
		}
		if err := ValidateSubmit(leg, inst, acct, ref, s.now()); err != nil {
			return nil, err
		}
		if leg.QuoteQuantity != nil {
			// §22.1 quote-denominated conversion, identical to Submit.
			if ref == nil || !ref.IsPositive() {
				return nil, codeErr("QUOTE_QUANTITY_INVALID",
					"no reference price for quote-denominated conversion on %s",
					inst.Symbol)
			}
			base := leg.QuoteQuantity.Div(*ref)
			if inst.LotSize.IsPositive() {
				base = base.Div(inst.LotSize).Floor().Mul(inst.LotSize)
			}
			if !base.IsPositive() {
				return nil, codeErr("QUOTE_QUANTITY_INVALID",
					"quote_quantity %s converts to zero base at %s",
					*leg.QuoteQuantity, *ref)
			}
			q := base
			leg.Quantity = &q
		}
		if s.limits != nil {
			evalPrice := decimal.Zero
			if leg.Price != nil {
				evalPrice = *leg.Price
			} else if ref != nil {
				evalPrice = *ref
			}
			if err := s.limits.CheckOrder(ctx, risk.OrderRequest{
				AccountID:  acct.ID,
				KycTier:    acct.KycTier,
				Symbol:     inst.Symbol,
				Side:       leg.Side,
				Quantity:   *leg.Quantity,
				Price:      evalPrice,
				ReduceOnly: leg.ReduceOnly,
			}); err != nil {
				return nil, err
			}
		}
		if err := s.checkBalance(ctx, acct, inst, leg, ref); err != nil {
			return nil, err
		}
	}

	shard := s.shardFor(symA)
	groupID := int64(s.seq.Next())
	seqA, seqB := s.seq.Next(), s.seq.Next()
	hash := ocoPairHash(inst.ID, legA, legB)

	paramsFor := func(leg *SubmitRequest, seq uint64) InsertParams {
		return InsertParams{
			AccountID:     acct.ID,
			InstrumentID:  inst.ID,
			ClientOrderID: leg.ClientOrderID,
			Side:          leg.Side,
			OrderType:     leg.OrderType,
			Quantity:      *leg.Quantity,
			QuoteQuantity: leg.QuoteQuantity,
			Price:         leg.Price,
			StopPrice:     leg.StopPrice,
			DisplayQty:    leg.DisplayQty,
			TimeInForce:   leg.TimeInForce,
			ShardID:       int(shard),
			OrderSeq:      seq,
			PostOnly:      leg.PostOnly,
			ReduceOnly:    leg.ReduceOnly,
			STPMode:       leg.STPMode,
			SessionID:     leg.SessionID,
			RequestHash:   hash,
		}
	}
	oa, ob, err := s.store.InsertOcoPairTx(ctx, groupID,
		paramsFor(legA, seqA), paramsFor(legB, seqB))
	if err != nil {
		if c := DedupConflictRow(err); c != nil {
			// §8.7 idempotency on the pair: replay only when the stored
			// dedup row carries THIS pair's hash — anything else is the
			// standard 409 collision.
			if c.RequestHash == hash {
				return s.replayOco(ctx, acct, legs)
			}
			return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
				"client_order_id %q already used with a different payload",
				c.ClientOrderID)
		}
		return nil, errInternal("oco pair insert", err)
	}

	if s.sub != nil {
		// Wire order is the OCO sequencing contract: the link precedes
		// both legs so the engine installs it while both are unplaced.
		b := flatbuffers.NewBuilder(512)
		ts := uint64(s.now().UnixNano())
		if err := s.sub.Send(ctx, shard,
			EncodeOcoLinkEvent(b, s.seq.Next(), ts, uint64(groupID),
				uint64(oa.ID), uint64(ob.ID), uint64(acct.ID),
				uint32(inst.ID))); err != nil {
			_ = s.store.MarkRejected(ctx, oa.ID)
			_ = s.store.MarkRejected(ctx, ob.ID)
			return nil, err
		}
		legsOut := [2]struct {
			o   *Order
			req *SubmitRequest
			seq uint64
		}{{oa, legA, seqA}, {ob, legB, seqB}}
		for _, l := range legsOut {
			nb := flatbuffers.NewBuilder(256)
			payload := ipc.EncodeOrderNewEvent(nb, l.seq,
				uint64(s.now().UnixNano()), orderNewMsg(l.o, acct, l.req))
			if err := s.sub.Send(ctx, shard, payload); err != nil {
				// Compensate both rows — an unlinked half-pair must never
				// reach the engine as a standalone order either.
				_ = s.store.MarkRejected(ctx, oa.ID)
				_ = s.store.MarkRejected(ctx, ob.ID)
				return nil, err
			}
		}
		_ = s.store.MarkActive(ctx, oa.ID)
		_ = s.store.MarkActive(ctx, ob.ID)
	}
	return &OcoAck{
		OcoGroupID: groupID,
		Legs: [2]*Ack{
			{OrderID: oa.ID, ClientOrderID: oa.ClientOrderID,
				Status: "ACTIVE", OrderSeq: seqA,
				TransactTime: s.now().UTC().Format(time.RFC3339Nano)},
			{OrderID: ob.ID, ClientOrderID: ob.ClientOrderID,
				Status: "ACTIVE", OrderSeq: seqB,
				TransactTime: s.now().UTC().Format(time.RFC3339Nano)},
		},
		Status:       "ACCEPTED",
		TransactTime: s.now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// replayOco resolves a dedup hit on an identical OCO submission: fetches
// the stored pair via each leg's dedup row and replays the stored acks.
// When the engine already resolved the pair's sibling race — one leg
// FILLED, the other CANCELLED — the trailing replay is rejected
// OCO_SIBLING_CANCEL_RACE (spec §6.5/§23, HTTP 409): restating "accepted"
// for a pair the race closed would lie about the linkage.
func (s *Service) replayOco(ctx context.Context, acct *Account,
	legs [2]*SubmitRequest) (*OcoAck, error) {
	stored := [2]*Order{}
	for i, leg := range legs {
		row, err := s.store.DedupLookup(ctx, acct.ID, leg.ClientOrderID)
		if err != nil {
			return nil, errInternal("oco dedup replay", err)
		}
		if row == nil {
			return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
				"oco leg %q resolves to a missing dedup row", leg.ClientOrderID)
		}
		o, err := s.store.GetOrder(ctx, row.OrderID)
		if err != nil {
			return nil, errInternal("oco replay fetch", err)
		}
		if o == nil || o.OcoGroupID == nil {
			return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
				"client_order_id %q does not resolve to an oco pair leg",
				leg.ClientOrderID)
		}
		stored[i] = o
	}
	// Race-resolved pair: one leg terminally FILLED while its sibling was
	// OCO-cancelled — the replay must surface the §23 code, not acks.
	for i := range stored {
		other := stored[1-i]
		if stored[i].Status == "FILLED" && other != nil &&
			other.Status == "CANCELLED" && other.OcoGroupID != nil &&
			*stored[i].OcoGroupID == *other.OcoGroupID {
			return nil, codeErr("OCO_SIBLING_CANCEL_RACE",
				"oco pair %d already resolved — leg %d filled, leg %d "+
					"cancelled by the sibling race",
				*stored[i].OcoGroupID, stored[i].ID, other.ID)
		}
	}
	ts := s.now().UTC().Format(time.RFC3339Nano)
	return &OcoAck{
		OcoGroupID: *stored[0].OcoGroupID,
		Legs: [2]*Ack{
			{OrderID: stored[0].ID, ClientOrderID: stored[0].ClientOrderID,
				Status: stored[0].Status, OrderSeq: stored[0].OrderSeq,
				Replay: true, TransactTime: ts},
			{OrderID: stored[1].ID, ClientOrderID: stored[1].ClientOrderID,
				Status: stored[1].Status, OrderSeq: stored[1].OrderSeq,
				Replay: true, TransactTime: ts},
		},
		Status:       "ACCEPTED",
		TransactTime: ts,
	}, nil
}
