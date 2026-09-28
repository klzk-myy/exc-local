// Wire framing for the A/B multicast feeds — MoldUDP64-style datagram
// framing carrying a 64-bit session ID and a 64-bit monotonic channel
// sequence plus a counted list of SBE messages (spec §27.2 "SBE Multicast
// (A/B)": "MoldUDP64 framing with 64-bit monotonic sequences").
//
// Datagram layout (little-endian):
//
//	u16 magic        = 0x5350 ("SP")
//	u16 channelID    = feed channel (identical on feeds A and B)
//	u16 msgCount     = number of SBE messages in this packet
//	u16 flags        = reserved (0)
//	u64 sessionID    = publisher epoch — changes on restart; consumers treat
//	                   a new session as a channel reset (resync via snapshot)
//	u64 seq          = channel sequence — monotonic within a session,
//	                   gap-checked by consumers
//	per message:     u16 msgLen || SBE message (header + block)
package sbe

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	packetMagic      uint16 = 0x5350 // "SP"
	packetHeaderSize        = 24
	// MaxPacketDatagram bounds one datagram — a UDP packet must fit the
	// path MTU; batches larger than this are refused by the publisher.
	MaxPacketDatagram = 1400
	// maxPacketMessages is a sanity bound on decode (allocations).
	maxPacketMessages = 4096
)

var (
	errPacketMagic     = errors.New("sbe: bad packet magic")
	errPacketTruncated = errors.New("sbe: truncated packet")
	errPacketMsgCount  = errors.New("sbe: packet message count exceeds bound")
	errPacketOversize  = errors.New("sbe: datagram exceeds MaxPacketDatagram")
)

// Packet is one decoded datagram. Raw keeps the exact received bytes so the
// A/B assembler can byte-compare duplicate deliveries (SBE_FEED_A_DESYNC
// detection) without re-encoding.
type Packet struct {
	ChannelID uint16
	SessionID uint64
	Seq       uint64
	Flags     uint16
	Messages  [][]byte // encoded SBE messages, in order
	Raw       []byte   // original datagram bytes
}

// EncodePacket frames msgs into one datagram stamped with
// channelID/sessionID/seq. msgs are pre-encoded SBE messages (see
// MarshalMessage). The same returned bytes are sent verbatim on feeds A and
// B — byte equivalence is therefore structural, not negotiated.
func EncodePacket(channelID uint16, sessionID, seq uint64, msgs [][]byte) ([]byte, error) {
	if len(msgs) > maxPacketMessages {
		return nil, errPacketMsgCount
	}
	n := packetHeaderSize
	for _, m := range msgs {
		if len(m) > 0xFFFF {
			return nil, fmt.Errorf("sbe: message too large (%d bytes)", len(m))
		}
		n += 2 + len(m)
	}
	if n > MaxPacketDatagram {
		return nil, errPacketOversize
	}
	out := make([]byte, 0, n)
	out = binary.LittleEndian.AppendUint16(out, packetMagic)
	out = binary.LittleEndian.AppendUint16(out, channelID)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(msgs)))
	out = binary.LittleEndian.AppendUint16(out, 0 /* flags */)
	out = binary.LittleEndian.AppendUint64(out, sessionID)
	out = binary.LittleEndian.AppendUint64(out, seq)
	for _, m := range msgs {
		out = binary.LittleEndian.AppendUint16(out, uint16(len(m)))
		out = append(out, m...)
	}
	return out, nil
}

// PeekPacketSeq returns the channel sequence of a raw datagram without full
// decode; ok=false for non-packet bytes (used by fault-injection filters).
func PeekPacketSeq(dg []byte) (seq uint64, ok bool) {
	if len(dg) < packetHeaderSize {
		return 0, false
	}
	if binary.LittleEndian.Uint16(dg[0:2]) != packetMagic {
		return 0, false
	}
	return binary.LittleEndian.Uint64(dg[16:24]), true
}

// DecodePacket validates and decodes one datagram.
func DecodePacket(dg []byte) (*Packet, error) {
	if len(dg) < packetHeaderSize {
		return nil, errPacketTruncated
	}
	if binary.LittleEndian.Uint16(dg[0:2]) != packetMagic {
		return nil, errPacketMagic
	}
	p := &Packet{
		ChannelID: binary.LittleEndian.Uint16(dg[2:4]),
		SessionID: binary.LittleEndian.Uint64(dg[8:16]),
		Seq:       binary.LittleEndian.Uint64(dg[16:24]),
		Flags:     binary.LittleEndian.Uint16(dg[6:8]),
		Raw:       dg,
	}
	count := int(binary.LittleEndian.Uint16(dg[4:6]))
	if count > maxPacketMessages {
		return nil, errPacketMsgCount
	}
	off := packetHeaderSize
	p.Messages = make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if off+2 > len(dg) {
			return nil, errPacketTruncated
		}
		ml := int(binary.LittleEndian.Uint16(dg[off : off+2]))
		off += 2
		if off+ml > len(dg) {
			return nil, errPacketTruncated
		}
		p.Messages = append(p.Messages, dg[off:off+ml])
		off += ml
	}
	if off != len(dg) {
		return nil, errPacketTruncated // trailing garbage
	}
	return p, nil
}

// DecodeMessages decodes each SBE message in the packet.
func (p *Packet) DecodeMessages() ([]Message, error) {
	out := make([]Message, 0, len(p.Messages))
	for i, raw := range p.Messages {
		m, n, err := DecodeMessage(raw)
		if err != nil {
			return nil, fmt.Errorf("sbe: packet seq=%d msg %d: %w", p.Seq, i, err)
		}
		if n != len(raw) {
			return nil, fmt.Errorf("sbe: packet seq=%d msg %d: %d trailing bytes", p.Seq, i, len(raw)-n)
		}
		out = append(out, m)
	}
	return out, nil
}
