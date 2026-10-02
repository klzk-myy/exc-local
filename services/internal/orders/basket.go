// basket.go — cross-shard basket submission (IMP-PLAN Phase-3 Task 4,
// spec §2.2a optimistic TRY_MATCH path).
//
// Request shape:
//   POST /api/v1/orders/basket
//   {"op_id":"<client key>", "legs":[{"symbol","side","quantity",
//    "limit_price?","client_order_id?"}, ...]}
//
// The 128-bit op id derives deterministically from the client's op_id key
// (SHA-256 → hi/lo) — a resubmission hits the coordinator's op dedup and
// replays the cached OptResult instead of re-executing. Absent a key, a
// random op id is minted.
//
// Each leg inserts a real `orders` row (status PENDING, IOC) so the
// ordinary fill projection updates it — fills land on the leg's order id
// exactly like a client order. The `baskets`/`basket_legs` rows (migration
// 283) make the op queryable via GET /api/v1/orders/basket/{op_id}.

package orders

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/config"
	"exchange/internal/ipc/wire"
	"exchange/pkg/decimal"
)

// BasketLegRequest is one leg of a basket — 2..8 legs per spec §2.2a.
type BasketLegRequest struct {
	Symbol        string           `json:"symbol"`
	Side          string           `json:"side"`
	Quantity      *decimal.Decimal `json:"quantity"`
	LimitPrice    *decimal.Decimal `json:"limit_price,omitempty"`
	ClientOrderID string           `json:"client_order_id,omitempty"`
}

// BasketSubmitRequest is the POST /orders/basket body.
type BasketSubmitRequest struct {
	OpID string             `json:"op_id,omitempty"`
	Legs []BasketLegRequest `json:"legs"`
}

// BasketAck is the submit response — the op settles asynchronously
// (≤500µs TRY_MATCH window); poll BasketStatus for the terminal result.
type BasketAck struct {
	OpID         string  `json:"op_id"` // hex(128-bit)
	OrderIDs     []int64 `json:"order_ids"`
	Status       string  `json:"status"` // MATCHING at submit
	Coordinator  int     `json:"coordinator_shard"`
	TransactTime string  `json:"transact_time"`
}

// BasketStatus is the read-side projection: op row + per-leg order rows.
type BasketStatus struct {
	OpID        string      `json:"op_id"`
	Status      string      `json:"status"`
	Code        int         `json:"code"`
	LegCount    int         `json:"leg_count"`
	LegsFilled  int         `json:"legs_filled"`
	LegsUnwound int         `json:"legs_unwound"`
	Slippage    int64       `json:"slippage_ticks"`
	Legs        []BasketLeg `json:"legs"`
}

// BasketLeg mirrors one basket_legs row + the joined order status.
type BasketLeg struct {
	LegIndex     int    `json:"leg_index"`
	OrderID      int64  `json:"order_id"`
	InstrumentID int64  `json:"instrument_id"`
	ShardID      int    `json:"shard_id"`
	OrderStatus  string `json:"order_status,omitempty"`
	FilledQty    string `json:"filled_qty,omitempty"`
}

// basketRow / basketLegRow are the store-shaped records.
type basketRow struct {
	OpHi, OpLo    uint64
	AccountID     int64
	Status        string
	Code          int
	LegCount      int
	LegsFilled    int
	LegsUnwound   int
	SlippageTicks int64
}
type basketLegRow struct {
	LegIndex     int
	OrderID      int64
	InstrumentID int64
	ShardID      int
	OrderStatus  string
	FilledQty    string
}

// basketStore narrows Store for the migration-283 tables — services wired
// with a store lacking it fail closed (BASKET_UNAVAILABLE), never partial.
type basketStore interface {
	InsertBasket(ctx context.Context, b basketRow, legs []basketLegRow) error
	BasketByOp(ctx context.Context, opHi, opLo uint64) (*basketRow, []basketLegRow, error)
	UpdateBasketResult(ctx context.Context, opHi, opLo uint64,
		status string, code, legsFilled, legsUnwound int, slippage int64) error
}

// ParseBasketSubmit validates the wire-level request shape. Field bounds
// beyond shape (leg count 2..8, positive qty, BUY/SELL) are re-checked in
// SubmitBasket — parse is intentionally shallow like ParseSubmit.
func ParseBasketSubmit(body []byte) (*BasketSubmitRequest, error) {
	var r BasketSubmitRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return &r, nil
}

// opIDToUint128 renders the client key (or a fresh random id) onto the
// engine's 128-bit op identity. Deterministic for non-empty keys — the
// coordinator's dedup then makes retried submissions replay, not re-fire.
// A failed random mint is an error, never a fallthrough to the keyed hash:
// hashing "" would mint the same op id for every keyless submission.
func opIDToUint128(key string) (hi, lo uint64, err error) {
	if key != "" {
		sum := sha256.Sum256([]byte("basket:" + key))
		return binary.BigEndian.Uint64(sum[:8]),
			binary.BigEndian.Uint64(sum[8:16]), nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, 0, err
	}
	return binary.BigEndian.Uint64(b[:8]),
		binary.BigEndian.Uint64(b[8:]), nil
}

// SubmitBasket executes the canonical path: validate → persist leg orders +
// op rows → encode BasketSubmit → send to the coordinator shard (lowest
// participating shard, spec §2.2a). The engine owns matching/2PC — this
// service only transports; any persistence/transport failure marks every
// leg row REJECTED so a dropped submit never reads as live.
func (s *Service) SubmitBasket(ctx context.Context, acct *Account,
	req *BasketSubmitRequest) (*BasketAck, error) {
	if req == nil {
		return nil, codeErr("INVALID_REQUEST", "basket request required")
	}
	if len(req.Legs) < 2 || len(req.Legs) > 8 {
		return nil, codeErr("INVALID_REQUEST",
			"basket leg count %d outside 2..8", len(req.Legs))
	}
	if s.sub == nil {
		return nil, codeErr("SERVICE_DEGRADED", "engine ingress channel unavailable")
	}
	bs, ok := s.store.(basketStore)
	if !ok {
		return nil, codeErr("SERVICE_DEGRADED", "basket ledger unavailable")
	}

	opHi, opLo, err := opIDToUint128(req.OpID)
	if err != nil {
		return nil, errInternal("basket op id mint", err)
	}
	opHex := fmt.Sprintf("%016x%016x", opHi, opLo)

	legs := make([]BasketLegWire, len(req.Legs))
	legRows := make([]basketLegRow, len(req.Legs))
	orders := make([]*Order, 0, len(req.Legs))
	coordShard := uint16(0xFFFF)

	fail := func(err error) (*BasketAck, error) {
		for _, o := range orders {
			_ = s.store.MarkRejected(ctx, o.ID)
		}
		return nil, err
	}

	for i, l := range req.Legs {
		if l.Quantity == nil || !l.Quantity.IsPositive() {
			return fail(codeErr("INVALID_REQUEST",
				"leg %d: quantity must be positive", i))
		}
		if l.Side != SideBuy && l.Side != SideSell {
			return fail(codeErr("INVALID_REQUEST",
				"leg %d: side must be BUY or SELL", i))
		}
		inst, err := s.store.InstrumentBySymbol(ctx,
			config.CanonicalSymbol(l.Symbol))
		if err != nil || inst == nil {
			return fail(codeErr("UNKNOWN_SYMBOL",
				"leg %d: unknown symbol %q", i, l.Symbol))
		}
		shard := s.shardFor(inst.Symbol)
		if shard < coordShard {
			coordShard = shard
		}
		orderType := TypeMarket
		if l.LimitPrice != nil {
			orderType = TypeLimit
		}
		o, _, err := s.insertOrderTx(ctx, InsertParams{
			AccountID:     acct.ID,
			InstrumentID:  inst.ID,
			ClientOrderID: l.ClientOrderID,
			Side:          l.Side,
			OrderType:     orderType,
			Quantity:      *l.Quantity,
			Price:         l.LimitPrice,
			TimeInForce:   TIFIOC,
			ShardID:       int(shard),
			OrderSeq:      s.seq.Next(),
			AlgoType:      strPtrOrNil("BASKET_LEG"),
			AlgoParams: json.RawMessage(fmt.Sprintf(
				`{"basket_op_id":%q,"leg_index":%d}`, opHex, i)),
		}, nil)
		if err != nil {
			return fail(errInternal("basket leg insert", err))
		}
		orders = append(orders, o)

		side := wire.SideBuy
		if l.Side == SideSell {
			side = wire.SideSell
		}
		limit := int64(0)
		if l.LimitPrice != nil {
			limit = decimal.Scaled(*l.LimitPrice)
		}
		legs[i] = BasketLegWire{
			ShardID:      uint32(shard),
			InstrumentID: uint32(inst.ID),
			OrderID:      uint64(o.ID),
			Side:         side,
			QtyUnits:     decimal.Scaled(*l.Quantity),
			LimitPrice:   limit,
		}
		legRows[i] = basketLegRow{
			LegIndex: i, OrderID: o.ID, InstrumentID: inst.ID,
			ShardID: int(shard),
		}
	}

	if err := bs.InsertBasket(ctx, basketRow{
		OpHi: opHi, OpLo: opLo, AccountID: acct.ID,
		Status: "MATCHING", LegCount: len(legs),
	}, legRows); err != nil {
		return fail(errInternal("basket ledger insert", err))
	}

	b := flatbuffers.NewBuilder(512)
	payload := EncodeBasketSubmitEvent(b, s.seq.Next(),
		uint64(s.now().UnixNano()), opHi, opLo,
		uint64(acct.ID), legs)
	if err := s.sub.Send(ctx, coordShard, payload); err != nil {
		// Compensate: the engine never saw the basket — rows must not
		// read as live. Basket row flips to REJECTED loudly.
		_ = bs.UpdateBasketResult(ctx, opHi, opLo, "REJECTED", 0, 0, 0, 0)
		return fail(err)
	}

	return &BasketAck{
		OpID:         opHex,
		OrderIDs:     legOrderIDs(orders),
		Status:       "MATCHING",
		Coordinator:  int(coordShard),
		TransactTime: s.now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// BasketStatus reads the op record + joined leg order rows. opHex is the
// hex(128-bit) id echoed by BasketAck. Rows belonging to another account
// read as NOT_FOUND — the op id leaks nothing cross-account.
func (s *Service) BasketStatus(ctx context.Context, acct *Account,
	opHex string) (*BasketStatus, error) {
	bs, ok := s.store.(basketStore)
	if !ok {
		return nil, codeErr("SERVICE_DEGRADED", "basket ledger unavailable")
	}
	hi, lo, err := parseOpHex(opHex)
	if err != nil {
		return nil, codeErr("INVALID_REQUEST", "%s", err)
	}
	row, legs, err := bs.BasketByOp(ctx, hi, lo)
	if err != nil {
		return nil, errInternal("basket lookup", err)
	}
	if row == nil || acct == nil || row.AccountID != acct.ID {
		return nil, codeErr("NOT_FOUND", "basket not found")
	}
	out := &BasketStatus{
		OpID: opHex, Status: row.Status, Code: row.Code,
		LegCount: row.LegCount, LegsFilled: row.LegsFilled,
		LegsUnwound: row.LegsUnwound, Slippage: row.SlippageTicks,
	}
	for _, l := range legs {
		out.Legs = append(out.Legs, BasketLeg{
			LegIndex: l.LegIndex, OrderID: l.OrderID,
			InstrumentID: l.InstrumentID, ShardID: l.ShardID,
			OrderStatus: l.OrderStatus, FilledQty: l.FilledQty,
		})
	}
	return out, nil
}

func parseOpHex(s string) (uint64, uint64, error) {
	if len(s) != 32 {
		return 0, 0, errors.New("op_id must be 32 hex chars")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return 0, 0, fmt.Errorf("op_id not hex: %w", err)
	}
	return binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:]), nil
}

func legOrderIDs(orders []*Order) []int64 {
	ids := make([]int64, len(orders))
	for i, o := range orders {
		ids[i] = o.ID
	}
	return ids
}
