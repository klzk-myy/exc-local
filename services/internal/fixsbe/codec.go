// codec.go — fixed-layout little-endian codec for the order-entry SBE
// schema (schema/order_entry.xml). Zero-deserialization design: decode
// is positional reads into POD structs — no allocation, no reflection,
// no per-field dispatch. Encode appends directly onto a caller buffer.
//
// Versioning rule (spec §8.6 / §24 #284): decoders MUST honor
// Header.BlockLength — known fields are read positionally and trailing
// bytes of a newer version's block are skipped. Schema ID/version
// lifecycle is the shared Phase-06 registry (internal/sbe.Registry).
package fixsbe

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	errTruncated   = errors.New("fixsbe: truncated message")
	errBodyLength  = errors.New("fixsbe: block length shorter than v1 layout")
	errWrongSchema = errors.New("fixsbe: wrong schema id")
)

// EncodeMessage appends header + body of m to buf and returns the
// extended slice. Unknown message types panic (encode is
// producer-side; a type the schema doesn't define must not ship).
func EncodeMessage(buf []byte, m Message) []byte {
	start := len(buf)
	buf = append(buf, make([]byte, headerSize)...)
	buf = appendBody(buf, m)
	h := Header{
		BlockLength: uint16(len(buf) - start - headerSize),
		TemplateID:  m.TemplateID(),
		SchemaID:    SchemaIDOrderEntry,
		Version:     SchemaVersionCurrent,
	}
	hdr := h.marshal()
	copy(buf[start:start+headerSize], hdr[:])
	return buf
}

// MarshalMessage encodes m as a standalone message.
func MarshalMessage(m Message) []byte { return EncodeMessage(nil, m) }

// DecodeMessage decodes one message at the head of b, returning the
// message and bytes consumed (header + blockLength). Header schema ID
// must be SchemaIDOrderEntry — wrong-schema traffic is an error, never
// silently reinterpreted.
func DecodeMessage(b []byte) (Message, int, error) {
	h, err := unmarshalHeader(b)
	if err != nil {
		return nil, 0, err
	}
	if h.SchemaID != SchemaIDOrderEntry {
		return nil, 0, fmt.Errorf("%w: %d", errWrongSchema, h.SchemaID)
	}
	total := headerSize + int(h.BlockLength)
	if len(b) < total {
		return nil, 0, errTruncated
	}
	body := b[headerSize:total]
	msg, err := decodeBody(h, body)
	if err != nil {
		return nil, 0, err
	}
	return msg, total, nil
}

// DecodeInbound is DecodeMessage restricted to the client→venue
// template set — outbound-only templates arriving inbound are a
// protocol violation (fail closed).
func DecodeInbound(b []byte) (Message, int, error) {
	m, n, err := DecodeMessage(b)
	if err != nil {
		return nil, 0, err
	}
	switch m.(type) {
	case NewOrder, CancelOrder, ReplaceOrder, Negotiate:
		return m, n, nil
	default:
		return nil, 0, fmt.Errorf("fixsbe: template %d not valid inbound", m.TemplateID())
	}
}

func appendBody(buf []byte, m Message) []byte {
	u16 := func(v uint16) { buf = binary.LittleEndian.AppendUint16(buf, v) }
	u32 := func(v uint32) { buf = binary.LittleEndian.AppendUint32(buf, v) }
	u64 := func(v uint64) { buf = binary.LittleEndian.AppendUint64(buf, v) }
	i64 := func(v int64) { u64(uint64(v)) }

	switch v := m.(type) {
	case NewOrder:
		buf = append(buf, v.ClOrdID[:]...)
		u64(v.AccountID)
		u32(v.InstrumentID)
		buf = append(buf, v.Side, v.OrdType, v.TimeInForce, v.Flags)
		i64(v.Price)
		i64(v.Qty)
		i64(v.StopPrice)
		i64(v.DisplayQty)
		u64(v.ExpireTimeNs)
		u64(v.TransactTimeNs)
	case CancelOrder:
		buf = append(buf, v.ClOrdID[:]...)
		buf = append(buf, v.OrigClOrdID[:]...)
		u64(v.OrderID)
		u64(v.AccountID)
		u32(v.InstrumentID)
		buf = append(buf, v.Side, 0)
		u16(0)
		u64(v.TransactTimeNs)
	case ReplaceOrder:
		buf = append(buf, v.ClOrdID[:]...)
		buf = append(buf, v.OrigClOrdID[:]...)
		u64(v.OrderID)
		u64(v.AccountID)
		u32(v.InstrumentID)
		buf = append(buf, v.Side, v.TimeInForce, v.Flags, 0)
		i64(v.Price)
		i64(v.Qty)
		i64(v.StopPrice)
		i64(v.DisplayQty)
		u64(v.ExpireTimeNs)
		u64(v.TransactTimeNs)
	case Negotiate:
		u16(v.SchemaID)
		u16(v.SchemaVersion)
		buf = append(buf, byte(v.ResponseCodec), 0)
		u16(0)
		buf = append(buf, v.SessionPubKey[:]...)
		buf = append(buf, v.Nonce[:]...)
		buf = append(buf, v.Signature[:]...)
	case ExecutionReport:
		u64(v.OrderID)
		u64(v.ExecID)
		buf = append(buf, v.ClOrdID[:]...)
		u32(v.InstrumentID)
		buf = append(buf, v.OrdStatus, v.ExecType, v.Side, 0)
		i64(v.Price)
		i64(v.LastPrice)
		i64(v.LastQty)
		i64(v.LeavesQty)
		i64(v.CumQty)
		u32(v.RejectCode)
		u32(0)
		u64(v.TransactTimeNs)
	case BusinessReject:
		buf = append(buf, v.ClOrdID[:]...)
		u16(v.RefTemplateID)
		u16(v.RejectReason)
		u32(v.RejectCode)
		u64(v.TransactTimeNs)
	case News:
		buf = append(buf, v.Headline[:]...)
		buf = append(buf, v.Body[:]...)
		buf = append(buf, v.Urgency, 0)
		u16(0)
		u32(v.NewsID)
		u64(v.EventTimeNs)
	case NegotiationResponse:
		u16(v.SchemaID)
		u16(v.SchemaVersion)
		buf = append(buf, v.Status, 0)
		u16(0)
		buf = append(buf, v.ServerNonce[:]...)
		u64(v.SunsetTimeNs)
		u32(v.ErrorCode)
		u32(0)
	default:
		panic(fmt.Sprintf("fixsbe: unencodable message type %T", m))
	}
	return buf
}

// decodeBody reads the fixed-layout v1 fields from body (exactly
// header.BlockLength bytes). A longer (newer-version) block is
// truncated to the known layout; shorter is an error.
func decodeBody(h Header, body []byte) (Message, error) {
	need := func(n int) error {
		if len(body) < n {
			return errBodyLength
		}
		return nil
	}
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(body[off:]) }
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(body[off:]) }
	u64 := func(off int) uint64 { return binary.LittleEndian.Uint64(body[off:]) }
	i64 := func(off int) int64 { return int64(binary.LittleEndian.Uint64(body[off:])) }
	str32 := func(off int) (c ClOrdID) { copy(c[:], body[off:off+32]); return }

	switch h.TemplateID {
	case TemplateNewOrder:
		if err := need(blockLenNewOrder); err != nil {
			return nil, err
		}
		return NewOrder{
			ClOrdID: str32(0), AccountID: u64(32), InstrumentID: u32(40),
			Side: body[44], OrdType: body[45], TimeInForce: body[46],
			Flags: body[47],
			Price: i64(48), Qty: i64(56), StopPrice: i64(64),
			DisplayQty: i64(72), ExpireTimeNs: u64(80),
			TransactTimeNs: u64(88),
		}, nil
	case TemplateCancelOrder:
		if err := need(blockLenCancelOrder); err != nil {
			return nil, err
		}
		return CancelOrder{
			ClOrdID: str32(0), OrigClOrdID: str32(32), OrderID: u64(64),
			AccountID: u64(72), InstrumentID: u32(80), Side: body[84],
			TransactTimeNs: u64(88),
		}, nil
	case TemplateReplaceOrder:
		if err := need(blockLenReplaceOrder); err != nil {
			return nil, err
		}
		return ReplaceOrder{
			ClOrdID: str32(0), OrigClOrdID: str32(32), OrderID: u64(64),
			AccountID: u64(72), InstrumentID: u32(80), Side: body[84],
			TimeInForce: body[85], Flags: body[86],
			Price: i64(88), Qty: i64(96), StopPrice: i64(104),
			DisplayQty: i64(112), ExpireTimeNs: u64(120),
			TransactTimeNs: u64(128),
		}, nil
	case TemplateNegotiate:
		if err := need(blockLenNegotiate); err != nil {
			return nil, err
		}
		var n Negotiate
		n.SchemaID = u16(0)
		n.SchemaVersion = u16(2)
		n.ResponseCodec = ResponseCodec(body[4])
		copy(n.SessionPubKey[:], body[8:40])
		copy(n.Nonce[:], body[40:56])
		copy(n.Signature[:], body[56:120])
		return n, nil
	case TemplateExecutionReport:
		if err := need(blockLenExecutionReport); err != nil {
			return nil, err
		}
		return ExecutionReport{
			OrderID: u64(0), ExecID: u64(8), ClOrdID: str32(16),
			InstrumentID: u32(48), OrdStatus: body[52], ExecType: body[53],
			Side:  body[54],
			Price: i64(56), LastPrice: i64(64), LastQty: i64(72),
			LeavesQty: i64(80), CumQty: i64(88), RejectCode: u32(96),
			TransactTimeNs: u64(104),
		}, nil
	case TemplateBusinessReject:
		if err := need(blockLenBusinessReject); err != nil {
			return nil, err
		}
		return BusinessReject{
			ClOrdID: str32(0), RefTemplateID: u16(32),
			RejectReason: u16(34), RejectCode: u32(36),
			TransactTimeNs: u64(40),
		}, nil
	case TemplateNews:
		if err := need(blockLenNews); err != nil {
			return nil, err
		}
		var n News
		copy(n.Headline[:], body[0:64])
		copy(n.Body[:], body[64:256])
		n.Urgency = body[256]
		n.NewsID = u32(260)
		n.EventTimeNs = u64(264)
		return n, nil
	case TemplateNegotiationResponse:
		if err := need(blockLenNegotiationResponse); err != nil {
			return nil, err
		}
		var r NegotiationResponse
		r.SchemaID = u16(0)
		r.SchemaVersion = u16(2)
		r.Status = body[4]
		copy(r.ServerNonce[:], body[8:24])
		r.SunsetTimeNs = u64(24)
		r.ErrorCode = u32(32)
		return r, nil
	default:
		return nil, fmt.Errorf("fixsbe: unknown template id %d", h.TemplateID)
	}
}
