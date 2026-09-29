// Task 1.3.5 — FlatBuffers message helpers over the generated
// exc.wire schema (services/internal/ipc/wire, schema:
// core/proto/exchange.fbs).

package ipc

import (
	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc/wire"
)

// OrderNewMsg carries the fields of wire.OrderNew.
type OrderNewMsg struct {
	OrderID       uint64
	AccountID     uint64
	InstrumentID  uint32
	Side          wire.Side
	Type          wire.OrderType
	Qty           int64 // scaled 1e8
	Price         int64 // scaled 1e8
	TIF           wire.TimeInForce
	ClientOrderID string
	// Aux fields added for stop/GTD/iceberg/STP/OCO leg fidelity
	// (Phase-14 Task 14.3.1) — 0 values are the wire "unset" convention.
	StpMode                 byte   // StpMode enum ordinal; 0xFF = unset
	Flags                   byte   // bit0 post_only, bit1 reduce_only; bit2 hidden, bit3 gslo
	StopPrice               int64  // scaled 1e8; 0 = none
	GtdExpiryNs             int64  // unixns; 0 = none
	DisplayQty              int64  // iceberg visible slice; 0 = full
	TradeGroupID            uint32 // 0 = none
	DiscretionaryOffsetPips int64  // 0 = none
	// ---- Phase-16 aux (exchange.fbs appends; the engine sibling owns
	//      decode — engines predating the schema see the defaults) ----
	PegMode            byte  // 0 none | 1 MID | 2 PRIMARY | 3 MARKET
	PegOffset          int64 // scaled 1e8, signed
	PegLimit           int64 // scaled 1e8; 0 = no collar
	TriggerSource      byte  // 0 LAST_PRICE | 1 MARK_PRICE | 2 INDEX_PRICE
	TrailingOffset     int64 // scaled 1e8; 0 = none
	TrailingOffsetUnit byte  // 0 none | 1 PIPS | 2 PERCENTAGE | 3 ABSOLUTE
	ActivationPrice    int64 // scaled 1e8; 0 = none
}

// EncodeOrderNewEvent serializes Event{seq, ts, type=OrderNew} into b and
// returns the finished buffer (valid until b is reused/reset).
func EncodeOrderNewEvent(b *flatbuffers.Builder, seq, ts uint64, m OrderNewMsg) []byte {
	coid := b.CreateString(m.ClientOrderID)
	wire.OrderNewStart(b)
	wire.OrderNewAddOrderId(b, m.OrderID)
	wire.OrderNewAddAccountId(b, m.AccountID)
	wire.OrderNewAddInstrumentId(b, m.InstrumentID)
	wire.OrderNewAddSide(b, m.Side)
	wire.OrderNewAddType(b, m.Type)
	wire.OrderNewAddQty(b, m.Qty)
	wire.OrderNewAddPrice(b, m.Price)
	wire.OrderNewAddTif(b, m.TIF)
	wire.OrderNewAddClientOrderId(b, coid)
	wire.OrderNewAddStpMode(b, m.StpMode)
	wire.OrderNewAddFlags(b, m.Flags)
	wire.OrderNewAddStopPrice(b, m.StopPrice)
	wire.OrderNewAddGtdExpiryNs(b, m.GtdExpiryNs)
	wire.OrderNewAddDisplayQty(b, m.DisplayQty)
	wire.OrderNewAddTradeGroupId(b, m.TradeGroupID)
	wire.OrderNewAddDiscretionaryOffsetPips(b, m.DiscretionaryOffsetPips)
	// Phase-16 aux fields — appended schema fields; 0 is the wire
	// "unset" convention on every one of them.
	wire.OrderNewAddPegMode(b, m.PegMode)
	wire.OrderNewAddPegOffset(b, m.PegOffset)
	wire.OrderNewAddPegLimit(b, m.PegLimit)
	wire.OrderNewAddTriggerSource(b, m.TriggerSource)
	wire.OrderNewAddTrailingOffset(b, m.TrailingOffset)
	wire.OrderNewAddTrailingOffsetUnit(b, m.TrailingOffsetUnit)
	wire.OrderNewAddActivationPrice(b, m.ActivationPrice)
	on := wire.OrderNewEnd(b)

	wire.EventStart(b)
	wire.EventAddSeq(b, seq)
	wire.EventAddTs(b, ts)
	wire.EventAddTypeType(b, wire.EventTypeOrderNew)
	wire.EventAddType(b, on)
	b.Finish(wire.EventEnd(b))
	return b.FinishedBytes()
}

// EncodeTradeFillEvent serializes Event{seq, ts, type=TradeFill}.
func EncodeTradeFillEvent(b *flatbuffers.Builder, seq, ts, tradeID,
	buyOrderID, sellOrderID uint64, price, qty, engineSeq int64) []byte {
	wire.TradeFillStart(b)
	wire.TradeFillAddTradeId(b, tradeID)
	wire.TradeFillAddBuyOrderId(b, buyOrderID)
	wire.TradeFillAddSellOrderId(b, sellOrderID)
	wire.TradeFillAddPrice(b, price)
	wire.TradeFillAddQty(b, qty)
	wire.TradeFillAddSeq(b, uint64(engineSeq))
	tf := wire.TradeFillEnd(b)

	wire.EventStart(b)
	wire.EventAddSeq(b, seq)
	wire.EventAddTs(b, ts)
	wire.EventAddTypeType(b, wire.EventTypeTradeFill)
	wire.EventAddType(b, tf)
	b.Finish(wire.EventEnd(b))
	return b.FinishedBytes()
}

// DecodeEvent roots a FlatBuffers Event over buf (zero-copy — buf must be
// kept alive while the returned objects are used).
func DecodeEvent(buf []byte) *wire.Event {
	return wire.GetRootAsEvent(buf, 0)
}

// EventOrderNew extracts the OrderNew union member, or nil.
func EventOrderNew(ev *wire.Event) *wire.OrderNew {
	if ev.TypeType() != wire.EventTypeOrderNew {
		return nil
	}
	var t flatbuffers.Table
	if !ev.Type(&t) {
		return nil
	}
	on := &wire.OrderNew{}
	on.Init(t.Bytes, t.Pos)
	return on
}

// EventTradeFill extracts the TradeFill union member, or nil.
func EventTradeFill(ev *wire.Event) *wire.TradeFill {
	if ev.TypeType() != wire.EventTypeTradeFill {
		return nil
	}
	var t flatbuffers.Table
	if !ev.Type(&t) {
		return nil
	}
	tf := &wire.TradeFill{}
	tf.Init(t.Bytes, t.Pos)
	return tf
}
