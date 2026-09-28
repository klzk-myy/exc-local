// Message definitions for the market-data SBE schema (schema ID 1).
//
// All message bodies are fixed-layout little-endian blocks; the SBE message
// header {blockLength, templateId, schemaId, version} governs framing and
// forward-compatible decoding (spec §8.6 SBE row: backward-compatible field
// additions only; breaking changes require a new schema ID).
package sbe

import "encoding/binary"

// Schema identifiers.
const (
	// SchemaIDMarketData is the venue market-data schema carried on the
	// A/B multicast feeds and negotiated REST/WS/private SBE streams.
	SchemaIDMarketData uint16 = 1
	// SchemaVersionCurrent is the latest registered version of schema 1.
	SchemaVersionCurrent uint16 = 1
)

// Template identifiers within SchemaIDMarketData. IDs are never reused or
// renumbered; retired templates stay reserved.
const (
	TemplateHeartbeat          uint16 = 1
	TemplateBookUpdate         uint16 = 2 // incremental L2 level event
	TemplateTrade              uint16 = 3
	TemplateSnapshotMarker     uint16 = 4
	TemplateSecurityStatus     uint16 = 5 // trading-status event
	TemplateSecurityDefinition uint16 = 6 // instrument reference data
)

// Side enumerates book sides. Unknown wire values decode to SideUnknown so
// callers can fail closed instead of misinterpreting a new enum member
// (§24 #284 unknown-enum sentinel check).
type Side uint8

const (
	SideBid     Side = 0
	SideAsk     Side = 1
	SideUnknown Side = 0xFF
)

func sideFromWire(v uint8) Side {
	switch Side(v) {
	case SideBid, SideAsk:
		return Side(v)
	default:
		return SideUnknown
	}
}

// BookAction enumerates incremental book mutations.
type BookAction uint8

const (
	BookActionUpdate  BookAction = 0 // upsert level (price, qty, count)
	BookActionDelete  BookAction = 1 // remove level
	BookActionUnknown BookAction = 0xFF
)

func bookActionFromWire(v uint8) BookAction {
	switch BookAction(v) {
	case BookActionUpdate, BookActionDelete:
		return BookAction(v)
	default:
		return BookActionUnknown
	}
}

// SnapshotMarkerType delimits a snapshot burst.
type SnapshotMarkerType uint8

const (
	SnapshotBegin   SnapshotMarkerType = 1
	SnapshotEnd     SnapshotMarkerType = 2
	SnapshotUnknown SnapshotMarkerType = 0xFF
)

func snapshotMarkerFromWire(v uint8) SnapshotMarkerType {
	switch SnapshotMarkerType(v) {
	case SnapshotBegin, SnapshotEnd:
		return SnapshotMarkerType(v)
	default:
		return SnapshotUnknown
	}
}

// TradingStatus mirrors the instrument lifecycle states of spec §7.1 for the
// wire; values are compact so new lifecycle states append at the end.
type TradingStatus uint8

const (
	StatusActive     TradingStatus = 0
	StatusSuspended  TradingStatus = 1
	StatusHalted     TradingStatus = 2
	StatusDelisted   TradingStatus = 3
	StatusRestricted TradingStatus = 4
	StatusCancelOnly TradingStatus = 5
	StatusAuction    TradingStatus = 6
	StatusUnknown    TradingStatus = 0xFF
)

func tradingStatusFromWire(v uint8) TradingStatus {
	s := TradingStatus(v)
	switch s {
	case StatusActive, StatusSuspended, StatusHalted, StatusDelisted,
		StatusRestricted, StatusCancelOnly, StatusAuction:
		return s
	default:
		return StatusUnknown
	}
}

// Message is any encodable SBE payload. Fixed-layout bodies are encoded by
// the codec; messages never carry their channel sequence — sequencing lives
// in the packet header (framing.go) so A/B arbitration stays byte-level.
type Message interface {
	TemplateID() uint16
	// Header exposes the message header fields used at encode time.
	Version() uint16
}

// Header is the canonical SBE message header.
type Header struct {
	BlockLength uint16
	TemplateID  uint16
	SchemaID    uint16
	Version     uint16
}

const headerSize = 8

func (h Header) marshal() [headerSize]byte {
	var b [headerSize]byte
	binary.LittleEndian.PutUint16(b[0:2], h.BlockLength)
	binary.LittleEndian.PutUint16(b[2:4], h.TemplateID)
	binary.LittleEndian.PutUint16(b[4:6], h.SchemaID)
	binary.LittleEndian.PutUint16(b[6:8], h.Version)
	return b
}

func unmarshalHeader(b []byte) (Header, error) {
	if len(b) < headerSize {
		return Header{}, errTruncated
	}
	return Header{
		BlockLength: binary.LittleEndian.Uint16(b[0:2]),
		TemplateID:  binary.LittleEndian.Uint16(b[2:4]),
		SchemaID:    binary.LittleEndian.Uint16(b[4:6]),
		Version:     binary.LittleEndian.Uint16(b[6:8]),
	}, nil
}

// Heartbeat keeps channel-sequence continuity on quiet periods (MoldUDP64
// heartbeats consume sequence space so gaps remain detectable).
type Heartbeat struct {
	EventTimeNs int64
}

func (Heartbeat) TemplateID() uint16 { return TemplateHeartbeat }
func (Heartbeat) Version() uint16    { return SchemaVersionCurrent }

// BookUpdate is one incremental L2 price-level event (no conflation on the
// institutional feed — spec §10.4).
type BookUpdate struct {
	InstrumentID uint32
	Side         Side
	Action       BookAction
	LevelCount   uint32 // order count at level (0 if venue does not expose)
	PriceTicks   int64  // fixed-point price in instrument tick units
	QtyLots      uint64 // aggregate quantity in lots
	EventTimeNs  int64
}

func (BookUpdate) TemplateID() uint16 { return TemplateBookUpdate }
func (BookUpdate) Version() uint16    { return SchemaVersionCurrent }

// Trade is one execution print.
type Trade struct {
	TradeID       uint64
	InstrumentID  uint32
	AggressorSide Side
	PriceTicks    int64
	QtyLots       uint64
	EventTimeNs   int64
}

func (Trade) TemplateID() uint16 { return TemplateTrade }
func (Trade) Version() uint16    { return SchemaVersionCurrent }

// SnapshotMarker brackets a snapshot burst; the End marker carries LastSeq —
// the channel sequence through which the snapshot is complete.
type SnapshotMarker struct {
	InstrumentID uint32 // 0 = whole-channel snapshot
	Type         SnapshotMarkerType
	LastSeq      uint64
	EventTimeNs  int64
}

func (SnapshotMarker) TemplateID() uint16 { return TemplateSnapshotMarker }
func (SnapshotMarker) Version() uint16    { return SchemaVersionCurrent }

// SecurityStatus broadcasts instrument trading-state changes, versioned and
// recoverable independently of book state (spec §10.4).
type SecurityStatus struct {
	InstrumentID uint32
	Status       TradingStatus
	EventTimeNs  int64
}

func (SecurityStatus) TemplateID() uint16 { return TemplateSecurityStatus }
func (SecurityStatus) Version() uint16    { return SchemaVersionCurrent }

// SecurityDefinition carries fixed instrument reference data. Symbol is a
// fixed 8-byte ASCII field (NUL-padded) — FX pair codes fit ("EURUSD",
// "EURUSD-1W"); longer identifier work is a schema-versioned extension.
type SecurityDefinition struct {
	InstrumentID  uint32
	Symbol        [8]byte
	TickSizeTicks int64
	LotSizeLots   uint64
	Status        TradingStatus
	EventTimeNs   int64
}

func (SecurityDefinition) TemplateID() uint16 { return TemplateSecurityDefinition }
func (SecurityDefinition) Version() uint16    { return SchemaVersionCurrent }

// UnknownMessage is the decode sentinel for template IDs this build does not
// recognize (Task 6.3.18 item 4: unknown message sentinel). The raw block is
// preserved so newer peers' messages can be logged/replayed verbatim.
type UnknownMessage struct {
	Hdr  Header
	Body []byte
}

func (u UnknownMessage) TemplateID() uint16 { return u.Hdr.TemplateID }
func (u UnknownMessage) Version() uint16    { return u.Hdr.Version }
