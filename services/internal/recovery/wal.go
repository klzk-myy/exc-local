// Package recovery implements the Task 4.3.2 WAL-to-S3 archive service and
// the Task 4.3.3 replay-from-archive engine.
//
// wal.go is a Go port of the WAL scanner in core/src/wal/WalEntry.cpp —
// same wire format (spec §3.4), same pad/torn-tail semantics, same CRC32C:
//
//	frame:   WalFileHeader{magic u32, version u16, shard u16} (8 bytes)
//	entry:   seq u64 | ts_ns u64 | event_type u8 | payload_len u32 |
//	         payload[payload_len] | crc32c u32   (overhead 25 bytes)
//	pad:     >=8 bytes: kWalPadSeq u64 sentinel then zeros to next 4KB
//	         file-offset boundary; 1..7 bytes: bare zeros
//	tail:    zeros to EOF = clean end (preallocated segment space)
//
// Payloads are the packed structs from core/include/wal/WalEntry.hpp
// (little-endian, fixed offsets — NOT FlatBuffers; FlatBuffers are only on
// the Aeron IPC path).
package recovery

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	WalMagic           uint32 = 0x57414C00
	WalVersion         uint16 = 1
	WalFileHeaderSize         = 8
	WalEntryHeaderSize        = 21
	WalEntryOverhead          = WalEntryHeaderSize + 4
	WalBlockSize       uint64 = 4096
	WalPadSeq          uint64 = ^uint64(0)
	WalMaxPayload      uint64 = 64 << 20
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// CRC32C computes the CRC32C (Castagnoli) checksum used by the WAL.
func CRC32C(data []byte) uint32 { return crc32.Checksum(data, crcTable) }

// EventType mirrors exch::WalEventType (append-only enum).
type EventType uint8

const (
	EvOrderNew       EventType = 0
	EvOrderCancel    EventType = 1
	EvOrderModify    EventType = 2
	EvTrade          EventType = 3
	EvTimeTick       EventType = 4
	EvBookSnapshot   EventType = 5
	EvMarginReserve  EventType = 6
	EvMarginRelease  EventType = 7
	EvPreventedMatch EventType = 8
)

func (t EventType) String() string {
	switch t {
	case EvOrderNew:
		return "ORDER_NEW"
	case EvOrderCancel:
		return "ORDER_CANCEL"
	case EvOrderModify:
		return "ORDER_MODIFY"
	case EvTrade:
		return "TRADE"
	case EvTimeTick:
		return "TIME_TICK"
	case EvBookSnapshot:
		return "BOOK_SNAPSHOT"
	case EvMarginReserve:
		return "MARGIN_RESERVE"
	case EvMarginRelease:
		return "MARGIN_RELEASE"
	case EvPreventedMatch:
		return "PREVENTED_MATCH"
	}
	return fmt.Sprintf("UNKNOWN(%d)", uint8(t))
}

// Entry is one decoded WAL record. Payload aliases the scanned image.
type Entry struct {
	Seq         uint64
	TimestampNs uint64
	Type        EventType
	Payload     []byte
	Offset      uint64 // byte offset of the record inside the segment
	RecordBytes uint64 // WalEntryOverhead + payload_len
}

// ScanResult mirrors WalScanResult.
type ScanResult struct {
	HeaderOK      bool
	Version       uint16
	Shard         uint16
	Entries       uint64
	LastSeq       uint64
	Pads          uint64
	EntryBytes    uint64
	PadBytes      uint64
	ValidEnd      uint64 // offset after the last valid record or pad
	Corrupt       bool
	CorruptOffset uint64
}

func alignUp4K(p uint64) uint64 { return (p + WalBlockSize - 1) &^ (WalBlockSize - 1) }

func zeroRun(data []byte, p uint64, avail uint64) uint64 {
	var z uint64
	for z < avail && data[p+z] == 0 {
		z++
	}
	return z
}

var (
	ErrBadHeader = errors.New("wal: bad file header (magic/version)")
	ErrShortFile = errors.New("wal: file shorter than frame header")
)

// ScanSegment walks a whole segment image with the same ladder as
// wal_scan(): header check, then Entry/Pad/End/Corrupt steps. Returned
// entries alias data — copy before retaining across calls.
func ScanSegment(data []byte) (ScanResult, []Entry, error) {
	res := ScanResult{ValidEnd: WalFileHeaderSize}
	if uint64(len(data)) < WalFileHeaderSize {
		return res, nil, ErrShortFile
	}
	if binary.LittleEndian.Uint32(data[0:4]) != WalMagic ||
		binary.LittleEndian.Uint16(data[4:6]) != WalVersion {
		return res, nil, ErrBadHeader
	}
	res.HeaderOK = true
	res.Version = binary.LittleEndian.Uint16(data[4:6])
	res.Shard = binary.LittleEndian.Uint16(data[6:8])

	size := uint64(len(data))
	pos := uint64(WalFileHeaderSize)
	var entries []Entry
	for {
		avail := size - pos
		if avail == 0 {
			break
		}
		p := pos

		// Strict pad form: sentinel u64 + zeros to next 4KB boundary.
		if avail >= 8 && binary.LittleEndian.Uint64(data[p:p+8]) == WalPadSeq {
			end := alignUp4K(p)
			if end > size || zeroRun(data, p+8, end-p-8) != end-p-8 {
				res.Corrupt, res.CorruptOffset = true, p
				break
			}
			res.Pads++
			res.PadBytes += end - p
			pos = end
			res.ValidEnd = pos
			continue
		}

		// Entry form.
		if avail >= WalEntryHeaderSize {
			payloadLen := uint64(binary.LittleEndian.Uint32(data[p+17 : p+21]))
			rec := uint64(WalEntryHeaderSize) + payloadLen + 4
			if payloadLen <= WalMaxPayload && rec <= avail {
				stored := binary.LittleEndian.Uint32(
					data[p+WalEntryHeaderSize+payloadLen : p+rec])
				if CRC32C(data[p:p+WalEntryHeaderSize+payloadLen]) == stored {
					e := Entry{
						Seq:         binary.LittleEndian.Uint64(data[p : p+8]),
						TimestampNs: binary.LittleEndian.Uint64(data[p+8 : p+16]),
						Type:        EventType(data[p+16]),
						Payload:     data[p+WalEntryHeaderSize : p+WalEntryHeaderSize+payloadLen],
						Offset:      p,
						RecordBytes: rec,
					}
					entries = append(entries, e)
					res.Entries++
					res.LastSeq = e.Seq
					res.EntryBytes += rec
					pos = p + rec
					res.ValidEnd = pos
					continue
				}
			}
		}

		// Zero tail / short pad form / corrupt.
		z := zeroRun(data, p, avail)
		if z == avail {
			break // clean end of log
		}
		if toB := alignUp4K(p) - p; toB > 0 && z == toB {
			res.Pads++
			res.PadBytes += z
			pos = p + z
			res.ValidEnd = pos
			continue
		}
		res.Corrupt, res.CorruptOffset = true, p
		break
	}
	return res, entries, nil
}

// --- payload decoders (packed little-endian structs, WalEntry.hpp) ------

// OrderNewPayload — sizeof 80.
type OrderNewPayload struct {
	OrderID         uint64
	AccountID       uint64
	InstrumentID    uint32
	Side            uint8 // 0=Buy 1=Sell
	Type            uint8
	TIF             uint8
	Flags           uint8
	PriceTicks      int64
	QtyUnits        int64
	VisibleQtyUnits int64
	StopPriceTicks  int64
	StpMode         uint32
	TradeGroupID    uint32
	GtdExpiryNs     int64
}

func DecodeOrderNew(p []byte) (OrderNewPayload, error) {
	var o OrderNewPayload
	if len(p) < 80 {
		return o, fmt.Errorf("wal: ORDER_NEW payload %d bytes, want >=80", len(p))
	}
	o.OrderID = binary.LittleEndian.Uint64(p[0:8])
	o.AccountID = binary.LittleEndian.Uint64(p[8:16])
	o.InstrumentID = binary.LittleEndian.Uint32(p[16:20])
	o.Side, o.Type, o.TIF, o.Flags = p[20], p[21], p[22], p[23]
	o.PriceTicks = int64(binary.LittleEndian.Uint64(p[24:32]))
	o.QtyUnits = int64(binary.LittleEndian.Uint64(p[32:40]))
	o.VisibleQtyUnits = int64(binary.LittleEndian.Uint64(p[40:48]))
	o.StopPriceTicks = int64(binary.LittleEndian.Uint64(p[48:56]))
	o.StpMode = binary.LittleEndian.Uint32(p[56:60])
	o.TradeGroupID = binary.LittleEndian.Uint32(p[60:64])
	o.GtdExpiryNs = int64(binary.LittleEndian.Uint64(p[64:72]))
	return o, nil
}

// OrderCancelPayload — sizeof 24.
type OrderCancelPayload struct {
	OrderID   uint64
	AccountID uint64
	Reason    uint8 // 0=user 1=expired 2=STP 3=FOK_unfilled 4=IOC_remainder
}

func DecodeOrderCancel(p []byte) (OrderCancelPayload, error) {
	var o OrderCancelPayload
	if len(p) < 24 {
		return o, fmt.Errorf("wal: ORDER_CANCEL payload %d bytes, want >=24", len(p))
	}
	o.OrderID = binary.LittleEndian.Uint64(p[0:8])
	o.AccountID = binary.LittleEndian.Uint64(p[8:16])
	o.Reason = p[16]
	return o, nil
}

// OrderModifyPayload — sizeof 40.
type OrderModifyPayload struct {
	OrderID           uint64
	NewPriceTicks     int64
	NewQtyUnits       int64
	NewStopPriceTicks int64
}

func DecodeOrderModify(p []byte) (OrderModifyPayload, error) {
	var o OrderModifyPayload
	if len(p) < 40 {
		return o, fmt.Errorf("wal: ORDER_MODIFY payload %d bytes, want >=40", len(p))
	}
	o.OrderID = binary.LittleEndian.Uint64(p[0:8])
	o.NewPriceTicks = int64(binary.LittleEndian.Uint64(p[8:16]))
	o.NewQtyUnits = int64(binary.LittleEndian.Uint64(p[16:24]))
	o.NewStopPriceTicks = int64(binary.LittleEndian.Uint64(p[24:32]))
	return o, nil
}

// TradePayload — sizeof 48.
type TradePayload struct {
	TradeID      uint64
	BuyOrderID   uint64
	SellOrderID  uint64
	InstrumentID uint32
	PriceTicks   int64
	QtyUnits     int64
}

func DecodeTrade(p []byte) (TradePayload, error) {
	var t TradePayload
	if len(p) < 48 {
		return t, fmt.Errorf("wal: TRADE payload %d bytes, want >=48", len(p))
	}
	t.TradeID = binary.LittleEndian.Uint64(p[0:8])
	t.BuyOrderID = binary.LittleEndian.Uint64(p[8:16])
	t.SellOrderID = binary.LittleEndian.Uint64(p[16:24])
	t.InstrumentID = binary.LittleEndian.Uint32(p[24:28])
	t.PriceTicks = int64(binary.LittleEndian.Uint64(p[32:40]))
	t.QtyUnits = int64(binary.LittleEndian.Uint64(p[40:48]))
	return t, nil
}

// TimeTickPayload — sizeof 16.
type TimeTickPayload struct {
	TickNs uint64
}

func DecodeTimeTick(p []byte) (TimeTickPayload, error) {
	var t TimeTickPayload
	if len(p) < 16 {
		return t, fmt.Errorf("wal: TIME_TICK payload %d bytes, want >=16", len(p))
	}
	t.TickNs = binary.LittleEndian.Uint64(p[0:8])
	return t, nil
}

// BookSnapshot — header (24) + levels (16 each) + orders (48 each).
type SnapshotLevel struct {
	PriceTicks int64
	Side       uint8 // 0=bid 1=ask
}

type SnapshotOrder struct {
	OrderID         uint64
	AccountID       uint64
	QtyUnits        int64
	VisibleQtyUnits int64
	StopPriceTicks  int64
	StpMode         uint32
	TIF             uint8
}

// SnapshotOrderExt — appended restore extension record (60 B), one per
// pinned SnapshotOrder in stream order. level_index resolves the order's
// price+side via Levels[level_index] — the pinned region alone cannot
// rebuild a book (SnapshotStore.hpp documents this).
type SnapshotOrderExt struct {
	OrderID     uint64
	QtyUnits    int64 // original total (pinned record carries remaining)
	FilledQty   int64
	PriceTicks  int64
	TimestampNs uint64
	IngressSeq  uint64
	LevelIndex  uint32
	Type        uint8
	Side        uint8
	Flags       uint8
}

const (
	SnapExtMagic     uint32 = 0x31455853 // 'SXE1'
	SnapExtVersion   uint16 = 1
	snapExtHdrSize          = 16
	snapExtOrderSize        = 60
)

type BookSnapshotPayload struct {
	InstrumentID uint32
	Levels       []SnapshotLevel
	Orders       []SnapshotOrder
	OrderExt     []SnapshotOrderExt // nil when the ext trailer is absent
	BookSeq      uint64
	ExtOK        bool
}

func DecodeBookSnapshot(p []byte) (BookSnapshotPayload, error) {
	var s BookSnapshotPayload
	if len(p) < 24 {
		return s, fmt.Errorf("wal: BOOK_SNAPSHOT payload %d bytes, want >=24", len(p))
	}
	s.InstrumentID = binary.LittleEndian.Uint32(p[0:4])
	levelCount := binary.LittleEndian.Uint32(p[4:8])
	orderCount := binary.LittleEndian.Uint64(p[8:16])
	s.BookSeq = binary.LittleEndian.Uint64(p[16:24])
	need := uint64(24) + uint64(levelCount)*16 + orderCount*48
	if uint64(len(p)) < need {
		return s, fmt.Errorf("wal: BOOK_SNAPSHOT payload %d bytes, want >=%d", len(p), need)
	}
	off := 24
	for i := uint32(0); i < levelCount; i++ {
		s.Levels = append(s.Levels, SnapshotLevel{
			PriceTicks: int64(binary.LittleEndian.Uint64(p[off : off+8])),
			Side:       p[off+8],
		})
		off += 16
	}
	for i := uint64(0); i < orderCount; i++ {
		s.Orders = append(s.Orders, SnapshotOrder{
			OrderID:         binary.LittleEndian.Uint64(p[off : off+8]),
			AccountID:       binary.LittleEndian.Uint64(p[off+8 : off+16]),
			QtyUnits:        int64(binary.LittleEndian.Uint64(p[off+16 : off+24])),
			VisibleQtyUnits: int64(binary.LittleEndian.Uint64(p[off+24 : off+32])),
			StopPriceTicks:  int64(binary.LittleEndian.Uint64(p[off+32 : off+40])),
			StpMode:         binary.LittleEndian.Uint32(p[off+40 : off+44]),
			TIF:             p[off+44],
		})
		off += 48
	}

	// Optional extension trailer: ext header then orderCount × 60B.
	// RecoveryManager requires it; the Go replayer uses it when present and
	// degrades to pinned-only reconstruction (orders→accounts, levels sans
	// per-order attribution) when absent.
	rest := p[off:]
	if uint64(len(rest)) >= snapExtHdrSize {
		xmagic := binary.LittleEndian.Uint32(rest[0:4])
		xver := binary.LittleEndian.Uint16(rest[4:6])
		xcount := binary.LittleEndian.Uint64(rest[8:16])
		if xmagic == SnapExtMagic && xver == SnapExtVersion &&
			uint64(len(rest)) == snapExtHdrSize+xcount*snapExtOrderSize &&
			xcount == orderCount {
			xo := snapExtHdrSize
			for i := uint64(0); i < xcount; i++ {
				s.OrderExt = append(s.OrderExt, SnapshotOrderExt{
					OrderID:     binary.LittleEndian.Uint64(rest[xo : xo+8]),
					QtyUnits:    int64(binary.LittleEndian.Uint64(rest[xo+8 : xo+16])),
					FilledQty:   int64(binary.LittleEndian.Uint64(rest[xo+16 : xo+24])),
					PriceTicks:  int64(binary.LittleEndian.Uint64(rest[xo+24 : xo+32])),
					TimestampNs: binary.LittleEndian.Uint64(rest[xo+32 : xo+40]),
					IngressSeq:  binary.LittleEndian.Uint64(rest[xo+40 : xo+48]),
					LevelIndex:  binary.LittleEndian.Uint32(rest[xo+48 : xo+52]),
					Type:        rest[xo+52],
					Side:        rest[xo+53],
					Flags:       rest[xo+54],
				})
				xo += snapExtOrderSize
			}
			s.ExtOK = true
		}
	}
	return s, nil
}

// PreventedMatchPayload — sizeof 80.
type PreventedMatchPayload struct {
	MakerOrderID           uint64
	TakerOrderID           uint64
	MakerAccountID         uint64
	TakerAccountID         uint64
	PriceTicks             int64
	MakerPreventedQtyUnits int64
	TakerPreventedQtyUnits int64
	PreventedNotionalUnits int64
	TradeGroupID           uint32
	Mode                   uint8
	TsNs                   uint64
}

func DecodePreventedMatch(p []byte) (PreventedMatchPayload, error) {
	var m PreventedMatchPayload
	if len(p) < 80 {
		return m, fmt.Errorf("wal: PREVENTED_MATCH payload %d bytes, want >=80", len(p))
	}
	m.MakerOrderID = binary.LittleEndian.Uint64(p[0:8])
	m.TakerOrderID = binary.LittleEndian.Uint64(p[8:16])
	m.MakerAccountID = binary.LittleEndian.Uint64(p[16:24])
	m.TakerAccountID = binary.LittleEndian.Uint64(p[24:32])
	m.PriceTicks = int64(binary.LittleEndian.Uint64(p[32:40]))
	m.MakerPreventedQtyUnits = int64(binary.LittleEndian.Uint64(p[40:48]))
	m.TakerPreventedQtyUnits = int64(binary.LittleEndian.Uint64(p[48:56]))
	m.PreventedNotionalUnits = int64(binary.LittleEndian.Uint64(p[56:64]))
	m.TradeGroupID = binary.LittleEndian.Uint32(p[64:68])
	m.Mode = p[68]
	m.TsNs = binary.LittleEndian.Uint64(p[72:80])
	return m, nil
}

// --- test/dev encoder ---------------------------------------------------
//
// Prod WAL bytes are written by the C++ engine (WalWriter). EncodeEntry +
// FileHeader exist so Go tests and dev tooling can build spec-conformant
// segments (and so archive round-trip tests exercise the real format).

// FileHeader serializes the 8-byte segment frame header.
func FileHeader(shard uint16) []byte {
	b := make([]byte, WalFileHeaderSize)
	binary.LittleEndian.PutUint32(b[0:4], WalMagic)
	binary.LittleEndian.PutUint16(b[4:6], WalVersion)
	binary.LittleEndian.PutUint16(b[6:8], shard)
	return b
}

// EncodeEntry serializes one entry record (header+payload+crc32c).
func EncodeEntry(seq, tsNs uint64, typ EventType, payload []byte) []byte {
	b := make([]byte, WalEntryOverhead+len(payload))
	binary.LittleEndian.PutUint64(b[0:8], seq)
	binary.LittleEndian.PutUint64(b[8:16], tsNs)
	b[16] = uint8(typ)
	binary.LittleEndian.PutUint32(b[17:21], uint32(len(payload)))
	copy(b[WalEntryHeaderSize:], payload)
	binary.LittleEndian.PutUint32(b[WalEntryHeaderSize+len(payload):],
		CRC32C(b[:WalEntryHeaderSize+len(payload)]))
	return b
}

// EncodePad emits a pad region like wal_emit_pad (sentinel when n >= 8).
func EncodePad(n int) []byte {
	b := make([]byte, n)
	if n >= 8 {
		binary.LittleEndian.PutUint64(b[0:8], WalPadSeq)
	}
	return b
}
