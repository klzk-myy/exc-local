// Phase-17 Task 17.3.1/17.3.2 seam — L3OrderEvent decode/encode.
//
// The wire contract is `table L3OrderEvent` in core/proto/exchange.fbs
// (union member 9 on wire.Event → wire.EventTypeL3OrderEvent=9), emitted
// by core/src/ipc/L3Publisher.cpp via the generated C++ builder. The Go
// accessors are generated (internal/ipc/wire/L3OrderEvent.go) — this
// file is the transport-neutral projection seam (L3OrderFields) plus
// the contract-validation gate: non-L3 events, malformed union tables,
// unknown kinds and zero order_id/l3_seq are rejected, never decoded
// into fabricated fields (spec §2.7).
//
// Field contract (declaration order in the .fbs):
//
//	instrument_id u32 · kind u8 (L3EventKind) · order_id u64 ·
//	account_hash u64 (salted FNV-1a pseudonym) · side u8 · price i64 ·
//	ref_price i64 · qty i64 (remaining AFTER the event) · qty_delta i64 ·
//	seq u64 (per-instrument L3 seq) · wal_seq u64 (triggering WAL row,
//	§24 #318) · trade_id u64 · fill_role u8 · flags u8 ·
//	cancel_reason u8.
//
// `ts` is the wire.Event envelope timestamp — every IPC event carries it
// there, so the L3 table does not repeat it.
package ipc

import (
	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc/wire"
)

// EventTypeL3OrderEvent aliases the generated union discriminator —
// kept in package ipc because every routing call site predates the
// regenerated wire package.
const EventTypeL3OrderEvent = wire.EventTypeL3OrderEvent

// L3 kind codes (aliased from the generated L3EventKind enum).
const (
	L3KindAdd     uint8 = uint8(wire.L3EventKindAdd)    // ORDER_ADD
	L3KindModify  uint8 = uint8(wire.L3EventKindModify) // ORDER_MODIFY
	L3KindCancel  uint8 = uint8(wire.L3EventKindCancel) // ORDER_CANCEL
	L3KindFill    uint8 = uint8(wire.L3EventKindFill)   // ORDER_EXECUTE
	L3KindExecute       = L3KindFill                    // transport-neutral alias
)

// L3 flag bits (mirroring the .fbs flags contract).
const (
	L3FlagHidden    uint8 = 1 << 0 // not L2-visible (hidden flag or PEG)
	L3FlagPegged    uint8 = 1 << 1
	L3FlagSynthetic uint8 = 1 << 2 // venue-synthetic leg (GSLO paired)
	L3FlagIceberg   uint8 = 1 << 3
	L3FlagDetail    uint8 = 1 << 4 // side/price/qty populated
)

// L3 fill-role codes (Fill legs only; 0 elsewhere).
const (
	L3RoleNone    uint8 = 0
	L3RoleTaker   uint8 = 1
	L3RoleMaker   uint8 = 2
	L3RoleAuction uint8 = 3
)

// L3OrderFields is the transport-neutral projection of one L3OrderEvent
// row — the full .fbs field set, decoded verbatim.
type L3OrderFields struct {
	InstrumentID  uint32
	Kind          uint8 // L3Kind*
	OrderID       uint64
	AccountHash   uint64
	Side          uint8 // wire.Side encoding: 0=BUY 1=SELL
	PriceTicks    int64
	RefPriceTicks int64
	QtyUnits      int64 // remaining AFTER the event
	QtyDelta      int64 // signed change applied by the event
	L3Seq         uint64
	WalSeq        uint64
	TradeID       uint64
	FillRole      uint8 // L3Role*
	Flags         uint8 // L3Flag*
	CancelReason  uint8 // kWalCancelReason* (Cancel only)

	// Hidden decodes Flags&L3FlagHidden — the §24 #197 redaction bit.
	Hidden bool
}

// DecodeL3OrderEvent extracts the L3OrderEvent union member. Returns
// (nil, false) for non-L3 events or a malformed union table — callers
// count drops and never guess fields (spec §2.7: no fabricated data).
func DecodeL3OrderEvent(ev *wire.Event) (*L3OrderFields, bool) {
	if ev == nil || ev.TypeType() != EventTypeL3OrderEvent {
		return nil, false
	}
	var t flatbuffers.Table
	if !ev.Type(&t) {
		return nil, false
	}
	var o wire.L3OrderEvent
	o.Init(t.Bytes, t.Pos)
	f := &L3OrderFields{
		InstrumentID:  o.InstrumentId(),
		Kind:          uint8(o.Kind()),
		OrderID:       o.OrderId(),
		AccountHash:   o.AccountHash(),
		Side:          uint8(o.Side()),
		PriceTicks:    o.Price(),
		RefPriceTicks: o.RefPrice(),
		QtyUnits:      o.Qty(),
		QtyDelta:      o.QtyDelta(),
		L3Seq:         o.Seq(),
		WalSeq:        o.WalSeq(),
		TradeID:       o.TradeId(),
		FillRole:      o.FillRole(),
		Flags:         o.Flags(),
		CancelReason:  o.CancelReason(),
	}
	f.Hidden = f.Flags&L3FlagHidden != 0
	if f.Kind > L3KindFill {
		return nil, false // unknown kind — never fabricate semantics
	}
	if f.OrderID == 0 || f.L3Seq == 0 {
		return nil, false // contract minimum: identity + sequence
	}
	return f, true
}

// MarshalL3Event builds a wire.Event carrying an L3OrderEvent union
// member — the encoder half of the seam, used by tests and by the
// dev-tooling replay path. Uses the generated builders so frames are
// schema-identical to the C++ CreateL3OrderEvent output.
func MarshalL3Event(envSeq, tsNs uint64, f *L3OrderFields) []byte {
	b := flatbuffers.NewBuilder(256)
	wire.L3OrderEventStart(b)
	wire.L3OrderEventAddInstrumentId(b, f.InstrumentID)
	wire.L3OrderEventAddKind(b, wire.L3EventKind(f.Kind))
	wire.L3OrderEventAddOrderId(b, f.OrderID)
	wire.L3OrderEventAddAccountHash(b, f.AccountHash)
	wire.L3OrderEventAddSide(b, wire.Side(f.Side))
	wire.L3OrderEventAddPrice(b, f.PriceTicks)
	wire.L3OrderEventAddRefPrice(b, f.RefPriceTicks)
	wire.L3OrderEventAddQty(b, f.QtyUnits)
	wire.L3OrderEventAddQtyDelta(b, f.QtyDelta)
	wire.L3OrderEventAddSeq(b, f.L3Seq)
	wire.L3OrderEventAddWalSeq(b, f.WalSeq)
	wire.L3OrderEventAddTradeId(b, f.TradeID)
	wire.L3OrderEventAddFillRole(b, f.FillRole)
	wire.L3OrderEventAddFlags(b, f.Flags)
	wire.L3OrderEventAddCancelReason(b, f.CancelReason)
	l3 := wire.L3OrderEventEnd(b)

	wire.EventStart(b)
	wire.EventAddSeq(b, envSeq)
	wire.EventAddTs(b, tsNs)
	wire.EventAddTypeType(b, EventTypeL3OrderEvent)
	wire.EventAddType(b, l3)
	b.Finish(wire.EventEnd(b))
	out := make([]byte, len(b.FinishedBytes()))
	copy(out, b.FinishedBytes())
	return out
}
