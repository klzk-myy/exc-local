// margin_ctl.go — MARGIN_CTL wire codec (Phase-19 Task 19.3.11,
// spec §13.1/§24 #176).
//
// Byte-identical mirror of the C++ codec in
// core/include/risk/CrossShardMarginCoordinator.h and
// core/src/risk/CrossShardMarginCoordinator.cpp. The C++ side memcpy's
// packed structs; every field here is little-endian with the exact
// width/order of the #pragma pack(1) wire structs. Nothing else may
// define this framing — the shared-memory/Aeron channel between a
// shard engine and the Go risk coordinator carries these frames.
//
// Frame = MarginCtlHeader (8B) + body. Bodies:
//
//	ReserveReq  (type 1) — 53B  engine → coordinator
//	ReserveAck  (type 2) — 36B  coordinator → engine
//	ReserveNack (type 3) — 24B  coordinator → engine
//	Release     (type 4) — 21B  bidirectional
//
// Decode accepts trailing padding (forward-compat, same as C++).
package ipc

import (
	"encoding/binary"
	"fmt"
)

// MarginCtlMagic is the frame magic — "MRGC" little-endian
// (kMarginCtlMagic in the C++ header).
const MarginCtlMagic uint32 = 0x4D475243

// MarginCtlVersion is the wire version both sides must agree on.
const MarginCtlVersion uint16 = 1

// MarginCtlHeaderLen is sizeof(MarginCtlHeader): magic u32 + type u8 +
// flags u8 + version u16.
const MarginCtlHeaderLen = 8

// MarginCtlMaxFrame is the largest control frame: header + REQ body.
const MarginCtlMaxFrame = MarginCtlHeaderLen + 53

// Body lengths (static_assert'd in the C++ header).
const (
	marginReserveReqLen  = 53
	marginReserveAckLen  = 36
	marginReserveNackLen = 24
	marginReleaseLen     = 21
)

// MarginCtlType mirrors enum class MarginCtlType : uint8_t.
type MarginCtlType uint8

const (
	MarginCtlReserveReq  MarginCtlType = 1 // MARGIN_RESERVE_REQ
	MarginCtlReserveAck  MarginCtlType = 2 // MARGIN_RESERVE_ACK
	MarginCtlReserveNack MarginCtlType = 3 // MARGIN_RESERVE_NACK
	MarginCtlRelease     MarginCtlType = 4 // MARGIN_RELEASE
)

// MarginReleaseReason mirrors enum class ReleaseReason : uint8_t.
type MarginReleaseReason uint8

const (
	// MarginReleaseComplete — order lifecycle done; normal release.
	MarginReleaseComplete MarginReleaseReason = 1
	// MarginReleaseOrderRejected — admission denied; slice never used.
	MarginReleaseOrderRejected MarginReleaseReason = 2
	// MarginReleaseTimeoutCompensate — hard-deadline cancel / late-ACK
	// unwind (the 2PC timeout path, Task 19.3.20).
	MarginReleaseTimeoutCompensate MarginReleaseReason = 3
	// MarginReleaseExpired — expires_at_ns reached.
	MarginReleaseExpired MarginReleaseReason = 4
	// MarginReleaseCoordinatorInitiated — inbound RELEASE from the
	// coordinator.
	MarginReleaseCoordinatorInitiated MarginReleaseReason = 5
	// MarginReleaseRecoveryOrphan — recovered slice released on the
	// restart sweep.
	MarginReleaseRecoveryOrphan MarginReleaseReason = 6
)

// MarginNackReason mirrors enum class NackReason : uint32_t.
type MarginNackReason uint32

const (
	MarginNackInsufficientHeadroom MarginNackReason = 1
	MarginNackUnknownAccount       MarginNackReason = 2
	MarginNackCoordinatorOverload  MarginNackReason = 3
	MarginNackShardUnreachable     MarginNackReason = 4
)

// MarginCoordinatorPicks is the dst_shard sentinel: let the coordinator
// pick which shard hosts the slice (kMarginCoordinatorPicks).
const MarginCoordinatorPicks uint32 = 0xFFFFFFFF

// MarginReqFlagCorrelationOffset is req_flags bit 0 — the reservation
// participates in the §13.1 correlation-offset portfolio margin path.
const MarginReqFlagCorrelationOffset uint8 = 1 << 0

// MarginCtlDecode mirrors enum class MarginCtlDecode.
type MarginCtlDecode uint8

const (
	MarginCtlDecodeOK MarginCtlDecode = iota
	MarginCtlDecodeTooShort
	MarginCtlDecodeBadMagic
	MarginCtlDecodeBadVersion
	MarginCtlDecodeUnknownType
)

// String mirrors margin_ctl_decode_str.
func (d MarginCtlDecode) String() string {
	switch d {
	case MarginCtlDecodeOK:
		return "Ok"
	case MarginCtlDecodeTooShort:
		return "TooShort"
	case MarginCtlDecodeBadMagic:
		return "BadMagic"
	case MarginCtlDecodeBadVersion:
		return "BadVersion"
	case MarginCtlDecodeUnknownType:
		return "UnknownType"
	}
	return "?"
}

// ---------------------------------------------------------------------------
// Wire bodies — field order and widths are the packed C++ struct layout.
// Amounts are int64 counts of 10^-8 units (decimal.ScaleFactor).
// ---------------------------------------------------------------------------

// MarginReserveReqBody mirrors MarginReserveReqBody (53 bytes).
type MarginReserveReqBody struct {
	// ReservationID is issuer-assigned: high16=issuer shard, low48=seq.
	ReservationID uint64
	AccountID     uint64
	OrderID       uint64 // admission context (0 = none)
	SrcShard      uint32 // shard whose order needs the slice (consumer)
	// DstShard is the host shard or MarginCoordinatorPicks.
	DstShard     uint32
	InstrumentID uint32
	ReqFlags     uint8  // MarginReqFlag*
	Amount       int64  // requested slice, 1e8-scaled ticks
	ExpiresAtNs  uint64 // CLOCK_REALTIME expiry; 0 = no expiry
}

// MarginReserveAckBody mirrors MarginReserveAckBody (36 bytes).
type MarginReserveAckBody struct {
	ReservationID uint64
	AccountID     uint64
	ShardID       uint32 // shard hosting the granted slice
	GrantedAmount int64  // <= requested (partial slice grant allowed)
	ExpiresAtNs   uint64 // authoritative expiry assigned by coordinator
}

// MarginReserveNackBody mirrors MarginReserveNackBody (24 bytes).
type MarginReserveNackBody struct {
	ReservationID uint64
	AccountID     uint64
	ShardID       uint32
	Reason        uint32 // MarginNackReason
}

// MarginReleaseBody mirrors MarginReleaseBody (21 bytes).
type MarginReleaseBody struct {
	ReservationID uint64
	AccountID     uint64
	ShardID       uint32 // shard hosting the slice being released
	Reason        uint8  // MarginReleaseReason
}

// marginBodyLen mirrors the C++ margin_body_len helper.
func marginBodyLen(t MarginCtlType) int {
	switch t {
	case MarginCtlReserveReq:
		return marginReserveReqLen
	case MarginCtlReserveAck:
		return marginReserveAckLen
	case MarginCtlReserveNack:
		return marginReserveNackLen
	case MarginCtlRelease:
		return marginReleaseLen
	}
	return 0
}

// ---------------------------------------------------------------------------
// WAL payloads (WalEventType::MARGIN_RESERVE / MARGIN_RELEASE) — the
// recovery/audit seam between the engine WAL and the coordinator books.
// ---------------------------------------------------------------------------

// Wal margin payload lengths (static_assert'd in the C++ header).
const (
	WalMarginReserveLen = 53
	WalMarginReleaseLen = 18
)

// WAL origin tags (MarginReservation.origin on the C++ side).
const (
	// WalMarginOriginLocal — ACK to our own REQ; slice usable by our
	// admissions.
	WalMarginOriginLocal uint8 = 0
	// WalMarginOriginHosted — our ACK to a coordinator REQ; slice of
	// local headroom locked for a remote shard.
	WalMarginOriginHosted uint8 = 1
	// WalMarginOriginIntent — outbound REQ issued; uncommitted (replay
	// reseeds the id sequence only).
	WalMarginOriginIntent uint8 = 2
)

// WalMarginReservePayload mirrors WalMarginReservePayload (53 bytes).
type WalMarginReservePayload struct {
	ReservationID uint64
	AccountID     uint64
	OrderID       uint64 // admission context (0 = none)
	ConsumerShard uint32 // shard whose orders may draw on the slice
	HostShard     uint32 // shard holding the locked headroom
	InstrumentID  uint32
	Origin        uint8  // 0 = Local, 1 = Hosted
	Amount        int64  // committed slice, 1e8-scaled ticks
	ExpiresAtNs   uint64
}

// WalMarginReleasePayload mirrors WalMarginReleasePayload (18 bytes).
type WalMarginReleasePayload struct {
	ReservationID uint64
	AccountID     uint64
	Origin        uint8
	Reason        uint8 // MarginReleaseReason
}

// ---------------------------------------------------------------------------
// Encode
// ---------------------------------------------------------------------------

// MarginCtlEncode serializes header+body into buf (cap must be >=
// MarginCtlMaxFrame for any type). Returns bytes written, 0 on bad
// args — mirroring margin_ctl_encode: nil/short dst or an unknown type
// yields 0 and writes nothing.
//
// body must be *MarginReserveReqBody / *MarginReserveAckBody /
// *MarginReserveNackBody / *MarginReleaseBody matching t, or a
// []byte of exactly the wire length (pre-encoded callers). Any other
// shape yields 0 — fail closed, never emit a partial frame.
func MarginCtlEncode(buf []byte, t MarginCtlType, body any) int {
	blen := marginBodyLen(t)
	if blen == 0 || body == nil {
		return 0
	}
	total := MarginCtlHeaderLen + blen
	if len(buf) < total {
		return 0
	}
	var b [marginReserveReqLen]byte // largest body
	switch t {
	case MarginCtlReserveReq:
		switch m := body.(type) {
		case *MarginReserveReqBody:
			binary.LittleEndian.PutUint64(b[0:], m.ReservationID)
			binary.LittleEndian.PutUint64(b[8:], m.AccountID)
			binary.LittleEndian.PutUint64(b[16:], m.OrderID)
			binary.LittleEndian.PutUint32(b[24:], m.SrcShard)
			binary.LittleEndian.PutUint32(b[28:], m.DstShard)
			binary.LittleEndian.PutUint32(b[32:], m.InstrumentID)
			b[36] = m.ReqFlags
			binary.LittleEndian.PutUint64(b[37:], uint64(m.Amount))
			binary.LittleEndian.PutUint64(b[45:], m.ExpiresAtNs)
		case []byte:
			if len(m) != blen {
				return 0
			}
			copy(b[:], m)
		default:
			return 0
		}
	case MarginCtlReserveAck:
		switch m := body.(type) {
		case *MarginReserveAckBody:
			binary.LittleEndian.PutUint64(b[0:], m.ReservationID)
			binary.LittleEndian.PutUint64(b[8:], m.AccountID)
			binary.LittleEndian.PutUint32(b[16:], m.ShardID)
			binary.LittleEndian.PutUint64(b[20:], uint64(m.GrantedAmount))
			binary.LittleEndian.PutUint64(b[28:], m.ExpiresAtNs)
		case []byte:
			if len(m) != blen {
				return 0
			}
			copy(b[:], m)
		default:
			return 0
		}
	case MarginCtlReserveNack:
		switch m := body.(type) {
		case *MarginReserveNackBody:
			binary.LittleEndian.PutUint64(b[0:], m.ReservationID)
			binary.LittleEndian.PutUint64(b[8:], m.AccountID)
			binary.LittleEndian.PutUint32(b[16:], m.ShardID)
			binary.LittleEndian.PutUint32(b[20:], m.Reason)
		case []byte:
			if len(m) != blen {
				return 0
			}
			copy(b[:], m)
		default:
			return 0
		}
	case MarginCtlRelease:
		switch m := body.(type) {
		case *MarginReleaseBody:
			binary.LittleEndian.PutUint64(b[0:], m.ReservationID)
			binary.LittleEndian.PutUint64(b[8:], m.AccountID)
			binary.LittleEndian.PutUint32(b[16:], m.ShardID)
			b[20] = m.Reason
		case []byte:
			if len(m) != blen {
				return 0
			}
			copy(b[:], m)
		default:
			return 0
		}
	}
	binary.LittleEndian.PutUint32(buf[0:], MarginCtlMagic)
	buf[4] = uint8(t)
	buf[5] = 0 // flags — reserved, must be 0
	binary.LittleEndian.PutUint16(buf[6:], MarginCtlVersion)
	copy(buf[MarginCtlHeaderLen:], b[:blen])
	return total
}

// ---------------------------------------------------------------------------
// Decode — MarginCtlView is the C++ MarginCtlView union equivalent: Type
// selects which body field is meaningful.
// ---------------------------------------------------------------------------

// MarginCtlView is the decoded frame view; exactly one body field is
// populated per Type.
type MarginCtlView struct {
	Type MarginCtlType
	Req  MarginReserveReqBody
	Ack  MarginReserveAckBody
	Nack MarginReserveNackBody
	Rel  MarginReleaseBody
}

// MarginCtlDecodeFrame validates a frame into out — mirroring
// margin_ctl_decode: rejects too-short frames, bad magic, bad version,
// unknown type; permits trailing padding. The caller's buf is never
// retained — all fields are copied out.
func MarginCtlDecodeFrame(buf []byte, out *MarginCtlView) MarginCtlDecode {
	if out == nil || len(buf) < MarginCtlHeaderLen {
		return MarginCtlDecodeTooShort
	}
	if binary.LittleEndian.Uint32(buf[0:]) != MarginCtlMagic {
		return MarginCtlDecodeBadMagic
	}
	if binary.LittleEndian.Uint16(buf[6:]) != MarginCtlVersion {
		return MarginCtlDecodeBadVersion
	}
	t := MarginCtlType(buf[4])
	blen := marginBodyLen(t)
	if blen == 0 {
		return MarginCtlDecodeUnknownType
	}
	if len(buf) < MarginCtlHeaderLen+blen { // trailing pad tolerated
		return MarginCtlDecodeTooShort
	}
	p := buf[MarginCtlHeaderLen:]
	out.Type = t
	switch t {
	case MarginCtlReserveReq:
		out.Req = MarginReserveReqBody{
			ReservationID: binary.LittleEndian.Uint64(p[0:]),
			AccountID:     binary.LittleEndian.Uint64(p[8:]),
			OrderID:       binary.LittleEndian.Uint64(p[16:]),
			SrcShard:      binary.LittleEndian.Uint32(p[24:]),
			DstShard:      binary.LittleEndian.Uint32(p[28:]),
			InstrumentID:  binary.LittleEndian.Uint32(p[32:]),
			ReqFlags:      p[36],
			Amount:        int64(binary.LittleEndian.Uint64(p[37:])),
			ExpiresAtNs:   binary.LittleEndian.Uint64(p[45:]),
		}
	case MarginCtlReserveAck:
		out.Ack = MarginReserveAckBody{
			ReservationID: binary.LittleEndian.Uint64(p[0:]),
			AccountID:     binary.LittleEndian.Uint64(p[8:]),
			ShardID:       binary.LittleEndian.Uint32(p[16:]),
			GrantedAmount: int64(binary.LittleEndian.Uint64(p[20:])),
			ExpiresAtNs:   binary.LittleEndian.Uint64(p[28:]),
		}
	case MarginCtlReserveNack:
		out.Nack = MarginReserveNackBody{
			ReservationID: binary.LittleEndian.Uint64(p[0:]),
			AccountID:     binary.LittleEndian.Uint64(p[8:]),
			ShardID:       binary.LittleEndian.Uint32(p[16:]),
			Reason:        binary.LittleEndian.Uint32(p[20:]),
		}
	case MarginCtlRelease:
		out.Rel = MarginReleaseBody{
			ReservationID: binary.LittleEndian.Uint64(p[0:]),
			AccountID:     binary.LittleEndian.Uint64(p[8:]),
			ShardID:       binary.LittleEndian.Uint32(p[16:]),
			Reason:        p[20],
		}
	}
	return MarginCtlDecodeOK
}

// ---------------------------------------------------------------------------
// WAL payload codec — same fixed-layout discipline; used by the Go-side
// recovery path and the bridge replay tooling.
// ---------------------------------------------------------------------------

// EncodeWalMarginReserve serializes the 53-byte WAL reserve payload.
func EncodeWalMarginReserve(p *WalMarginReservePayload) []byte {
	b := make([]byte, WalMarginReserveLen)
	binary.LittleEndian.PutUint64(b[0:], p.ReservationID)
	binary.LittleEndian.PutUint64(b[8:], p.AccountID)
	binary.LittleEndian.PutUint64(b[16:], p.OrderID)
	binary.LittleEndian.PutUint32(b[24:], p.ConsumerShard)
	binary.LittleEndian.PutUint32(b[28:], p.HostShard)
	binary.LittleEndian.PutUint32(b[32:], p.InstrumentID)
	b[36] = p.Origin
	binary.LittleEndian.PutUint64(b[37:], uint64(p.Amount))
	binary.LittleEndian.PutUint64(b[45:], p.ExpiresAtNs)
	return b
}

// DecodeWalMarginReserve parses a 53-byte WAL reserve payload; len must
// be at least WalMarginReserveLen.
func DecodeWalMarginReserve(buf []byte) (WalMarginReservePayload, error) {
	var p WalMarginReservePayload
	if len(buf) < WalMarginReserveLen {
		return p, fmt.Errorf("wal margin reserve: short payload %d < %d", len(buf), WalMarginReserveLen)
	}
	p.ReservationID = binary.LittleEndian.Uint64(buf[0:])
	p.AccountID = binary.LittleEndian.Uint64(buf[8:])
	p.OrderID = binary.LittleEndian.Uint64(buf[16:])
	p.ConsumerShard = binary.LittleEndian.Uint32(buf[24:])
	p.HostShard = binary.LittleEndian.Uint32(buf[28:])
	p.InstrumentID = binary.LittleEndian.Uint32(buf[32:])
	p.Origin = buf[36]
	p.Amount = int64(binary.LittleEndian.Uint64(buf[37:]))
	p.ExpiresAtNs = binary.LittleEndian.Uint64(buf[45:])
	return p, nil
}

// EncodeWalMarginRelease serializes the 18-byte WAL release payload.
func EncodeWalMarginRelease(p *WalMarginReleasePayload) []byte {
	b := make([]byte, WalMarginReleaseLen)
	binary.LittleEndian.PutUint64(b[0:], p.ReservationID)
	binary.LittleEndian.PutUint64(b[8:], p.AccountID)
	b[16] = p.Origin
	b[17] = p.Reason
	return b
}

// DecodeWalMarginRelease parses an 18-byte WAL release payload.
func DecodeWalMarginRelease(buf []byte) (WalMarginReleasePayload, error) {
	var p WalMarginReleasePayload
	if len(buf) < WalMarginReleaseLen {
		return p, fmt.Errorf("wal margin release: short payload %d < %d", len(buf), WalMarginReleaseLen)
	}
	p.ReservationID = binary.LittleEndian.Uint64(buf[0:])
	p.AccountID = binary.LittleEndian.Uint64(buf[8:])
	p.Origin = buf[16]
	p.Reason = buf[17]
	return p, nil
}

// ---------------------------------------------------------------------------
// Channel naming — the per-shard Aeron IPC aliases. The engine owns a
// CrossShardMarginCoordinator bound to its ctl channel pair; the Go
// risk coordinator (services/cmd/risk) subscribes margin_ctl_in_{shard}
// and publishes margin_ctl_out_{shard}. Mirrors the orders_in/orders_out
// convention (bridge config "%d" alias interpolation).
// ---------------------------------------------------------------------------

// MarginCtlInURI is the coordinator's inbound channel for shard
// (engine → coordinator REQ/RELEASE traffic).
func MarginCtlInURI(shard uint32) string {
	return fmt.Sprintf("aeron:ipc?alias=margin_ctl_in_%d", shard)
}

// MarginCtlOutURI is the coordinator's outbound channel for shard
// (coordinator → engine ACK/NACK/RELEASE traffic).
func MarginCtlOutURI(shard uint32) string {
	return fmt.Sprintf("aeron:ipc?alias=margin_ctl_out_%d", shard)
}

// Margin-ctl Aeron stream ids (orders_in=1001, orders_out=1002 occupy
// the 100x band; the margin control pair takes 1101/1102).
const (
	MarginCtlInStreamID  int32 = 1101
	MarginCtlOutStreamID int32 = 1102
)
