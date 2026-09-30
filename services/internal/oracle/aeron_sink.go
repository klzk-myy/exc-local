package oracle

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"exchange/pkg/decimal"
)

// Aeron publication contract (spec §26, Task 19.5.3.2): mark/index
// rounds fan out to the C++ core on channel 224.0.1.1:40456 in
// addition to the Redis keyspace the refresher polls. The frame is a
// fixed-layout little-endian record — the same discipline as
// ipc/margin_ctl.go:
//
//	[0]   uint8  message type (1 = mark/index round)
//	[1]   uint8  symbol byte length (≤255)
//	[2:]  symbol bytes (e.g. "EUR/USD")
//	[+]   int64  mark ticks, 1e8 scale
//	[+]   int64  index ticks, 1e8 scale (0 = absent)
//	[+]   int64  unix-nanoseconds stamp
const aeronFrameMarkIndex byte = 1

// Oracle Aeron channel — §26 contract (multicast 224.0.1.1:40456).
// Stream id 1201 keeps the 100x/110x bands (orders, margin_ctl) free.
const (
	// AeronOracleURI is the multicast channel the C++ core subscribes
	// to for mark/index rounds (spec §26: 224.0.1.1:40456).
	AeronOracleURI = "aeron:udp?endpoint=224.0.1.1:40456"
	// AeronOracleStreamID is the stream id on the oracle channel.
	AeronOracleStreamID int32 = 1201
)

// Offerer is the minimal Aeron publication surface —
// *ipcaeron.Publication satisfies it; tests inject a recorder.
type Offerer interface {
	Offer(buf []byte) int64
}

// AeronMarkSink implements AeronSink over an Aeron publication. An
// Offer result ≤ 0 (not-connected / back-pressured / admin action) is
// treated as a publication failure — the publisher logs it and Redis
// still carries the authoritative mark (fail-soft on the side channel,
// fail-closed on the data path).
type AeronMarkSink struct {
	Pub Offerer
}

// NewAeronMarkSink wraps a raw Aeron publication.
func NewAeronMarkSink(pub Offerer) *AeronMarkSink {
	return &AeronMarkSink{Pub: pub}
}

// PublishMarkIndex encodes and offers one mark/index round.
func (s *AeronMarkSink) PublishMarkIndex(_ context.Context, symbol string,
	mark, index decimal.Decimal, at time.Time) error {
	if s == nil || s.Pub == nil {
		return fmt.Errorf("oracle aeron: publication not bound")
	}
	if len(symbol) > 255 {
		return fmt.Errorf("oracle aeron: symbol %q exceeds 255-byte frame", symbol)
	}
	buf := make([]byte, 2+len(symbol)+24)
	buf[0] = aeronFrameMarkIndex
	buf[1] = byte(len(symbol))
	off := 2 + copy(buf[2:], symbol)
	binary.LittleEndian.PutUint64(buf[off:], uint64(ticks1e8(mark)))
	binary.LittleEndian.PutUint64(buf[off+8:], uint64(ticks1e8(index)))
	binary.LittleEndian.PutUint64(buf[off+16:], uint64(at.UnixNano()))
	if rc := s.Pub.Offer(buf); rc < 0 {
		return fmt.Errorf("oracle aeron offer rc=%d", rc)
	}
	return nil
}

// ticks1e8 converts a decimal to the 1e8-scaled tick contract shared
// with the C++ PriceOracleFeed parser.
func ticks1e8(d decimal.Decimal) int64 {
	if !d.IsPositive() {
		return 0
	}
	return d.Mul(decimal.NewFromInt(100_000_000)).Round(0).IntPart()
}
