// Fixed-layout little-endian codec for the market-data SBE schema.
//
// Encoding rule per spec §8.6: fields are only ever appended at the end of a
// block inside a schema version line, so a decoder MUST honor header
// BlockLength — known fields are read positionally and any trailing bytes of
// a newer version's block are skipped. That keeps mixed-version A/B feed
// consumers forward-compatible (§24 #284 forward-decode check).
package sbe

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Codec-level errors (L3 edge/protocol faults per spec §2.7.2).
var (
	errTruncated  = errors.New("sbe: truncated message")
	errBodyLength = errors.New("sbe: block length shorter than v1 layout")
)

// Block lengths for version 1 of each template.
const (
	blockLenHeartbeat          = 8
	blockLenBookUpdate         = 34
	blockLenTrade              = 38
	blockLenSnapshotMarker     = 22
	blockLenSecurityStatus     = 14
	blockLenSecurityDefinition = 38
)

// EncodeMessage appends the SBE header + fixed-layout body of m to buf and
// returns the extended slice.
func EncodeMessage(buf []byte, m Message) []byte {
	start := len(buf)
	buf = append(buf, make([]byte, headerSize)...)
	buf = appendBody(buf, m)
	h := Header{
		BlockLength: uint16(len(buf) - start - headerSize),
		TemplateID:  m.TemplateID(),
		SchemaID:    SchemaIDMarketData,
		Version:     m.Version(),
	}
	hdr := h.marshal()
	copy(buf[start:start+headerSize], hdr[:])
	return buf
}

// MarshalMessage returns m encoded as a standalone message.
func MarshalMessage(m Message) []byte {
	return EncodeMessage(nil, m)
}

// DecodeMessage decodes one message at the head of b and returns the message
// plus bytes consumed (header + blockLength). Unknown template IDs decode to
// the UnknownMessage sentinel; unknown enum wire values decode to the
// per-enum Unknown sentinel.
func DecodeMessage(b []byte) (Message, int, error) {
	h, err := unmarshalHeader(b)
	if err != nil {
		return nil, 0, err
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

func appendBody(buf []byte, m Message) []byte {
	switch v := m.(type) {
	case Heartbeat:
		return binary.LittleEndian.AppendUint64(buf, uint64(v.EventTimeNs))
	case BookUpdate:
		buf = binary.LittleEndian.AppendUint32(buf, v.InstrumentID)
		buf = append(buf, byte(v.Side), byte(v.Action))
		buf = binary.LittleEndian.AppendUint32(buf, v.LevelCount)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(v.PriceTicks))
		buf = binary.LittleEndian.AppendUint64(buf, v.QtyLots)
		return binary.LittleEndian.AppendUint64(buf, uint64(v.EventTimeNs))
	case Trade:
		buf = binary.LittleEndian.AppendUint64(buf, v.TradeID)
		buf = binary.LittleEndian.AppendUint32(buf, v.InstrumentID)
		buf = append(buf, byte(v.AggressorSide), 0 /* reserved */)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(v.PriceTicks))
		buf = binary.LittleEndian.AppendUint64(buf, v.QtyLots)
		return binary.LittleEndian.AppendUint64(buf, uint64(v.EventTimeNs))
	case SnapshotMarker:
		buf = binary.LittleEndian.AppendUint32(buf, v.InstrumentID)
		buf = append(buf, byte(v.Type), 0 /* reserved */)
		buf = binary.LittleEndian.AppendUint64(buf, v.LastSeq)
		return binary.LittleEndian.AppendUint64(buf, uint64(v.EventTimeNs))
	case SecurityStatus:
		buf = binary.LittleEndian.AppendUint32(buf, v.InstrumentID)
		buf = append(buf, byte(v.Status), 0 /* reserved */)
		return binary.LittleEndian.AppendUint64(buf, uint64(v.EventTimeNs))
	case SecurityDefinition:
		buf = binary.LittleEndian.AppendUint32(buf, v.InstrumentID)
		buf = append(buf, v.Symbol[:]...)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(v.TickSizeTicks))
		buf = binary.LittleEndian.AppendUint64(buf, v.LotSizeLots)
		buf = append(buf, byte(v.Status), 0 /* reserved */)
		return binary.LittleEndian.AppendUint64(buf, uint64(v.EventTimeNs))
	case UnknownMessage:
		// Pass-through for relaying messages from newer schema versions.
		return append(buf, v.Body...)
	default:
		panic(fmt.Sprintf("sbe: unencodable message type %T", m))
	}
}

// decodeBody decodes the fixed-layout v1 fields from body. body is exactly
// header.BlockLength bytes; a longer (newer-version) block is truncated to
// the known layout, a shorter one is an error.
func decodeBody(h Header, body []byte) (Message, error) {
	need := func(n int) error {
		if len(body) < n {
			return errBodyLength
		}
		return nil
	}
	switch h.TemplateID {
	case TemplateHeartbeat:
		if err := need(blockLenHeartbeat); err != nil {
			return nil, err
		}
		return Heartbeat{EventTimeNs: int64(binary.LittleEndian.Uint64(body[0:8]))}, nil
	case TemplateBookUpdate:
		if err := need(blockLenBookUpdate); err != nil {
			return nil, err
		}
		return BookUpdate{
			InstrumentID: binary.LittleEndian.Uint32(body[0:4]),
			Side:         sideFromWire(body[4]),
			Action:       bookActionFromWire(body[5]),
			LevelCount:   binary.LittleEndian.Uint32(body[6:10]),
			PriceTicks:   int64(binary.LittleEndian.Uint64(body[10:18])),
			QtyLots:      binary.LittleEndian.Uint64(body[18:26]),
			EventTimeNs:  int64(binary.LittleEndian.Uint64(body[26:34])),
		}, nil
	case TemplateTrade:
		if err := need(blockLenTrade); err != nil {
			return nil, err
		}
		return Trade{
			TradeID:       binary.LittleEndian.Uint64(body[0:8]),
			InstrumentID:  binary.LittleEndian.Uint32(body[8:12]),
			AggressorSide: sideFromWire(body[12]),
			PriceTicks:    int64(binary.LittleEndian.Uint64(body[14:22])),
			QtyLots:       binary.LittleEndian.Uint64(body[22:30]),
			EventTimeNs:   int64(binary.LittleEndian.Uint64(body[30:38])),
		}, nil
	case TemplateSnapshotMarker:
		if err := need(blockLenSnapshotMarker); err != nil {
			return nil, err
		}
		return SnapshotMarker{
			InstrumentID: binary.LittleEndian.Uint32(body[0:4]),
			Type:         snapshotMarkerFromWire(body[4]),
			LastSeq:      binary.LittleEndian.Uint64(body[6:14]),
			EventTimeNs:  int64(binary.LittleEndian.Uint64(body[14:22])),
		}, nil
	case TemplateSecurityStatus:
		if err := need(blockLenSecurityStatus); err != nil {
			return nil, err
		}
		return SecurityStatus{
			InstrumentID: binary.LittleEndian.Uint32(body[0:4]),
			Status:       tradingStatusFromWire(body[4]),
			EventTimeNs:  int64(binary.LittleEndian.Uint64(body[6:14])),
		}, nil
	case TemplateSecurityDefinition:
		if err := need(blockLenSecurityDefinition); err != nil {
			return nil, err
		}
		var sym [8]byte
		copy(sym[:], body[4:12])
		return SecurityDefinition{
			InstrumentID:  binary.LittleEndian.Uint32(body[0:4]),
			Symbol:        sym,
			TickSizeTicks: int64(binary.LittleEndian.Uint64(body[12:20])),
			LotSizeLots:   binary.LittleEndian.Uint64(body[20:28]),
			Status:        tradingStatusFromWire(body[28]),
			EventTimeNs:   int64(binary.LittleEndian.Uint64(body[30:38])),
		}, nil
	default:
		cp := make([]byte, len(body))
		copy(cp, body)
		return UnknownMessage{Hdr: h, Body: cp}, nil
	}
}
