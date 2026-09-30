// aeron.go — the Aeron leg of trace_id continuity (Task 9.3.11,
// spec §19.12 "Aeron trace_id header format", §24 #112 T09-003).
//
// Byte contract (shared with the C++ core — Phase-01 SBE/IPC schema):
//
//	Every order-path Aeron frame reserves a fixed 64-byte application
//	header field `trace_parent` — ASCII, left-justified, NUL-padded —
//	carrying the W3C traceparent value the sender started with.
//
//	  offset 0   8B  magic "EXCTRACE" (0x4558435452414345)
//	  offset 8   55B traceparent ASCII "00-{32}-{16}-{2}"
//	  offset 63  1B  reserved (0)
//
//	The Go side writes it via InjectAeronTrace before offer(); the C++
//	matching core copies the field verbatim into every frame it emits in
//	response to that command (match report, market-data tick family it
//	derives, settlement trigger). Consumers read it with
//	ExtractAeronTrace and continue the trace with a remote parent.
//
//	Frames whose trace block is absent/invalid are simply untraced —
//	the field is metadata; the hot path never fails on it.
package tracing

import (
	"context"
	"encoding/binary"
)

// AeronTraceHeaderLen is the fixed reserved size of the trace block.
const AeronTraceHeaderLen = 64

// aeronTraceMagic is the 8-byte sentinel "EXCTRACE" (little-endian uint64
// as it lands in the frame: bytes spell the ASCII string).
const aeronTraceMagic = "EXCTRACE"

// traceParentLen is the exact length of a W3C traceparent value.
const traceParentLen = 55 // 2+1+32+1+16+1+2

// InjectAeronTrace writes the active span's traceparent into the frame's
// reserved header region. buf must be at least AeronTraceHeaderLen and
// the block occupies buf[0:AeronTraceHeaderLen]. No-op without an active
// span context — the frame simply ships untraced.
func InjectAeronTrace(ctx context.Context, buf []byte) {
	sc, ok := SpanContextFrom(ctx)
	if !ok || len(buf) < AeronTraceHeaderLen {
		return
	}
	for i := 0; i < AeronTraceHeaderLen; i++ {
		buf[i] = 0
	}
	copy(buf[0:8], aeronTraceMagic)
	copy(buf[8:8+traceParentLen], sc.TraceParent())
}

// ExtractAeronTrace parses the trace block of a received frame. Returns
// the remote SpanContext and ok=false when the block is absent or
// malformed — callers then start a root span instead of failing.
func ExtractAeronTrace(buf []byte) (SpanContext, bool) {
	if len(buf) < AeronTraceHeaderLen ||
		binary.LittleEndian.Uint64(buf[0:8]) !=
			binary.LittleEndian.Uint64([]byte(aeronTraceMagic)) {
		return SpanContext{}, false
	}
	end := 8 + traceParentLen
	for i := 8; i < end; i++ {
		if buf[i] == 0 {
			end = i
			break
		}
	}
	return ParseTraceParent(string(buf[8:end]))
}

// ContextFromAeron combines extract+store: returns ctx carrying the
// remote SpanContext (or ctx unchanged when untraced), so a consumer is
// one call from starting its child span.
func ContextFromAeron(ctx context.Context, frame []byte) (context.Context, bool) {
	sc, ok := ExtractAeronTrace(frame)
	if !ok {
		return ctx, false
	}
	return ContextWithSpanContext(ctx, sc), true
}

// StripAeronTrace peels the optional 64B block off a received frame:
// body aliases payload at the event offset. ok reports the block was
// present — consumers that continue the trace use sc; every decode
// site must call this before touching the FlatBuffers root, since the
// engine echoes the block verbatim on frames answering a traced
// command (Task 9.3.11 wire contract — block-at-0, Event at +64).
func StripAeronTrace(payload []byte) (body []byte, sc SpanContext, ok bool) {
	if sc, ok = ExtractAeronTrace(payload); ok {
		return payload[AeronTraceHeaderLen:], sc, true
	}
	return payload, SpanContext{}, false
}
