// message.go — message definitions for the order-entry SBE schema
// (schema ID 2, version 1 — see schema/order_entry.xml).
//
// All bodies are fixed-layout little-endian blocks mirroring the XML;
// fields are only ever appended at the end of a block within a schema
// version so decoders honor Header.BlockLength and skip unknown tails
// (forward compatibility, same rule as the Phase-06 market-data codec
// and §24 #284).
package fixsbe

import "encoding/binary"

// Schema identifiers — 1 is the market-data schema (internal/sbe);
// the order-entry schema is ID 2.
const (
	SchemaIDOrderEntry     uint16 = 2
	SchemaVersionCurrent   uint16 = 1
)

// Template identifiers within SchemaIDOrderEntry. IDs are never reused
// or renumbered; retired templates stay reserved.
const (
	TemplateNewOrder      uint16 = 1 // client → venue
	TemplateCancelOrder   uint16 = 2
	TemplateReplaceOrder  uint16 = 3
	TemplateNegotiate     uint16 = 5 // session handshake (Task 18.3.17)

	TemplateExecutionReport      uint16 = 101 // venue → client
	TemplateBusinessReject       uint16 = 102
	TemplateNews                 uint16 = 103
	TemplateNegotiationResponse  uint16 = 104
)

// Wire enums (fix SBE-style numeric vocabulary).
const (
	SideBuy  uint8 = 1
	SideSell uint8 = 2
)

const (
	OrdTypeMarket    uint8 = 1
	OrdTypeLimit     uint8 = 2
	OrdTypeStop      uint8 = 3
	OrdTypeStopLimit uint8 = 4
)

const (
	TIFDay uint8 = 0
	TIFGTC uint8 = 1
	TIFIOC uint8 = 3
	TIFFOK uint8 = 4
	TIFGTD uint8 = 6
)

const (
	OrdStatusNew             uint8 = 0
	OrdStatusPartiallyFilled uint8 = 1
	OrdStatusFilled          uint8 = 2
	OrdStatusCancelled       uint8 = 4
	OrdStatusRejected        uint8 = 8
	OrdStatusExpired         uint8 = 12
)

const (
	ExecTypeNew       uint8 = 0
	ExecTypeCancelled uint8 = 4
	ExecTypeReplaced  uint8 = 5
	ExecTypeRejected  uint8 = 8
	ExecTypeExpired   uint8 = 12
	ExecTypeTrade     uint8 = 15
)

const (
	NewsUrgencyNormal     uint8 = 0
	NewsUrgencyFlash      uint8 = 1
	NewsUrgencyBackground uint8 = 2
)

const (
	NegotiateOK         uint8 = 0
	NegotiateDeprecated uint8 = 1
	NegotiateRejected   uint8 = 2
)

// ResponseCodec selects the outbound encoding a session asked for at
// Negotiate — the tag-value/SBE transport combinations of Task 18.3.17.
type ResponseCodec uint8

const (
	CodecSBE      ResponseCodec = 0
	CodecTagValue ResponseCodec = 1
)

// NewOrder flag bits.
const (
	FlagPostOnly   uint8 = 1 << 0
	FlagReduceOnly uint8 = 1 << 1
	FlagHidden     uint8 = 1 << 2
)

// ClOrdID is the fixed-width wire identifier field.
type ClOrdID [32]byte

// Str returns the NUL-trimmed ASCII view.
func (c ClOrdID) Str() string {
	n := 0
	for n < len(c) && c[n] != 0 {
		n++
	}
	return string(c[:n])
}

// SetClOrdID writes s into the field; overlong values truncate (callers
// bound lengths at admission).
func SetClOrdID(s string) ClOrdID {
	var c ClOrdID
	copy(c[:], s)
	return c
}

// Header is the SBE message header — identical wire layout to the
// market-data schema (internal/sbe) so one framing layer serves both.
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

// ---------------------------------------------------------------------------
// Inbound messages
// ---------------------------------------------------------------------------

// NewOrder mirrors the NewOrder template (blockLength 96).
type NewOrder struct {
	ClOrdID        ClOrdID
	AccountID      uint64
	InstrumentID   uint32
	Side           uint8
	OrdType        uint8
	TimeInForce    uint8
	Flags          uint8
	Price          int64 // mantissa ×10^-8; 0 = absent
	Qty            int64
	StopPrice      int64
	DisplayQty     int64
	ExpireTimeNs   uint64 // GTD expiry; 0 = none
	TransactTimeNs uint64
}

// CancelOrder mirrors the CancelOrder template (blockLength 96).
type CancelOrder struct {
	ClOrdID        ClOrdID
	OrigClOrdID    ClOrdID
	OrderID        uint64 // 0 = resolve via OrigClOrdID
	AccountID      uint64
	InstrumentID   uint32
	Side           uint8
	TransactTimeNs uint64
}

// ReplaceOrder mirrors the ReplaceOrder template (blockLength 136).
type ReplaceOrder struct {
	ClOrdID        ClOrdID
	OrigClOrdID    ClOrdID
	OrderID        uint64
	AccountID      uint64
	InstrumentID   uint32
	Side           uint8
	TimeInForce    uint8
	Flags          uint8
	Price          int64 // 0 = unchanged
	Qty            int64
	StopPrice      int64
	DisplayQty     int64
	ExpireTimeNs   uint64
	TransactTimeNs uint64
}

// Negotiate is the session handshake (template 5): schema negotiation +
// Ed25519 session-key proof bound to the TLS SNI hostname.
type Negotiate struct {
	SchemaID      uint16
	SchemaVersion uint16
	ResponseCodec ResponseCodec
	SessionPubKey [32]byte // Ed25519 public key
	Nonce         [16]byte // server-issued challenge
	Signature     [64]byte // Ed25519 over ChallengeMessage()
}

// ---------------------------------------------------------------------------
// Outbound messages
// ---------------------------------------------------------------------------

// ExecutionReport mirrors the ExecutionReport template (blockLength 112).
type ExecutionReport struct {
	OrderID        uint64
	ExecID         uint64
	ClOrdID        ClOrdID
	InstrumentID   uint32
	OrdStatus      uint8
	ExecType       uint8
	Side           uint8
	Price          int64
	LastPrice      int64
	LastQty        int64
	LeavesQty      int64
	CumQty         int64
	RejectCode     uint32
	TransactTimeNs uint64
}

// BusinessReject mirrors the BusinessReject template (blockLength 48).
type BusinessReject struct {
	ClOrdID        ClOrdID
	RefTemplateID  uint16
	RejectReason   uint16
	RejectCode     uint32
	TransactTimeNs uint64
}

// News is the administrative advisory (template 103) — the maintenance
// drain broadcast of Task 18.3.17 / spec §24 #289.
type News struct {
	Headline    [64]byte
	Body        [192]byte
	Urgency     uint8
	NewsID      uint32
	EventTimeNs uint64
}

// NegotiationResponse mirrors the NegotiationResponse template
// (blockLength 40).
type NegotiationResponse struct {
	SchemaID      uint16
	SchemaVersion uint16
	Status        uint8
	ServerNonce   [16]byte
	SunsetTimeNs  uint64
	ErrorCode     uint32
}

// Message is any encodable SBE payload.
type Message interface {
	TemplateID() uint16
}

func (NewOrder) TemplateID() uint16            { return TemplateNewOrder }
func (CancelOrder) TemplateID() uint16         { return TemplateCancelOrder }
func (ReplaceOrder) TemplateID() uint16        { return TemplateReplaceOrder }
func (Negotiate) TemplateID() uint16           { return TemplateNegotiate }
func (ExecutionReport) TemplateID() uint16     { return TemplateExecutionReport }
func (BusinessReject) TemplateID() uint16      { return TemplateBusinessReject }
func (News) TemplateID() uint16                { return TemplateNews }
func (NegotiationResponse) TemplateID() uint16 { return TemplateNegotiationResponse }

// Block lengths for schema version 1.
const (
	blockLenNewOrder             = 96
	blockLenCancelOrder          = 96
	blockLenReplaceOrder         = 136
	blockLenNegotiate            = 120
	blockLenExecutionReport      = 112
	blockLenBusinessReject       = 48
	blockLenNews                 = 272
	blockLenNegotiationResponse  = 40
)
