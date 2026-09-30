#pragma once

// Task 9.3.11 — the C++ half of trace_id continuity
// HTTP -> Aeron -> C++ -> Aeron -> Go (spec §19.12 "Aeron trace_id header
// format", §24 #112 T09-003; deploy/otel/README.md holds the pipeline doc).
//
// Byte contract — identical to services/internal/tracing/aeron.go:
//
//   An order-path frame MAY carry a fixed 64-byte `trace_parent` block at
//   offset 0; the FlatBuffers Event payload follows at +kTraceBlockLen:
//
//     offset 0   8B  magic "EXCTRACE" (0x4558435452414345 little-endian)
//     offset 8   55B W3C traceparent ASCII "00-{32hex}-{16hex}-{2hex}",
//                    left-justified, NUL-padded
//     offset 63  1B  reserved (0)
//
//   The magic doubles as the presence flag: frames without it keep the
//   legacy layout (payload at offset 0) — producers that never inject
//   (pre-9.3.11 gateways, control tools) are unaffected. A magic'd block
//   with a malformed traceparent still means the header is occupied:
//   the payload decodes at +64 untraced — the field is metadata; the hot
//   path never fails on it (spec §2.7: tracing never rejects traffic).
//
//   The engine's obligations:
//     1. Consume: decode the payload at +kTraceBlockLen when the block is
//        present (EnginePump::dispatch).
//     2. Echo: copy the 64B block VERBATIM into every frame emitted in
//        response to that command — the Go consumers continue the trace
//        with tracing.ExtractAeronTrace (match report, market-data tick
//        family, L3 order events, settlement trigger).
//     3. Span: emit one `order.match` span per traced command, remote-
//        parented on the frame's span id, through the span sink seam
//        (OTLP/HTTP JSONL compatible with the collector pipeline — the
//        in-repo model carries no OTel SDK dependency, same as Go).
//
// Echo transport: EnginePump owns a TraceSlot it arms for the duration of
// each dispatch; IpcPublisher/L3Publisher bind the same slot and compose
// [block][Event] at send. Single-writer = the matching thread; no atomics
// on the slot itself.

#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstring>

namespace exch {

// --- Wire constants ---------------------------------------------------------

inline constexpr uint32_t kTraceBlockLen = 64;
inline constexpr uint32_t kTraceParentLen = 55;      // 2+1+32+1+16+1+2
inline constexpr char kTraceMagic[9] = "EXCTRACE";   // 8 bytes on the wire

// OTLP SpanKind value for CONSUMER (mirrors services/internal/tracing).
inline constexpr int32_t kSpanKindConsumer = 5;

// --- Inbound decode ---------------------------------------------------------

// has_trace_block: the magic check that also serves as the presence flag.
// A FlatBuffers Event can never start with these bytes — its first 4 bytes
// are a uoffset into the buffer and 0x54435845 ("EXCT") is far past any
// frame that fits the transport, so there is no ambiguity with the legacy
// layout.
[[nodiscard]] inline bool has_trace_block(const uint8_t* frame,
                                          uint32_t len) noexcept {
    return frame != nullptr && len >= kTraceBlockLen &&
           std::memcmp(frame, kTraceMagic, 8) == 0;
}

// Parsed remote context — trace identity for the engine's `order.match`
// span plus the verbatim block for outbound echo.
struct CoreSpanContext {
    char trace_id[33];        // 32-hex + NUL
    char parent_span_id[17];  // 16-hex + NUL (remote caller's span id)
    bool sampled;
    uint8_t block[kTraceBlockLen];  // verbatim copy — echoed on responses
};

namespace detail {

[[nodiscard]] inline bool is_hex(char c) noexcept {
    return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
}

[[nodiscard]] inline bool all_hex(const char* p, uint32_t n) noexcept {
    for (uint32_t i = 0; i < n; ++i) {
        if (!is_hex(p[i])) return false;
    }
    return true;
}

[[nodiscard]] inline bool all_zero(const char* p, uint32_t n) noexcept {
    for (uint32_t i = 0; i < n; ++i) {
        if (p[i] != '0') return false;
    }
    return true;
}

}  // namespace detail

// parse_trace_context validates the W3C traceparent inside a magic'd
// block — the same acceptance the Go side applies in
// tracing.ParseTraceParent: "VV-{32hex}-{16hex}-{2hex}", ids non-zero.
// Returns false when the parent is malformed — caller treats the frame as
// untraced (payload STILL at +64; the block was reserved). Requires
// has_trace_block(frame,len) to have passed.
[[nodiscard]] inline bool parse_trace_context(const uint8_t* frame,
                                              uint32_t len,
                                              CoreSpanContext& out) noexcept {
    if (!has_trace_block(frame, len)) return false;
    const char* tp = reinterpret_cast<const char*>(frame + 8);
    // Find the effective length: left-justified, NUL-padded in 55 bytes —
    // same scan as Go's ExtractAeronTrace.
    uint32_t n = 0;
    while (n < kTraceParentLen && tp[n] != '\0') ++n;
    if (n != kTraceParentLen) return false;
    if (tp[2] != '-' || tp[35] != '-' || tp[52] != '-') return false;
    if (!detail::all_hex(tp + 3, 32) || !detail::all_hex(tp + 36, 16) ||
        !detail::all_hex(tp + 53, 2)) {
        return false;
    }
    if (detail::all_zero(tp + 3, 32) || detail::all_zero(tp + 36, 16)) {
        return false;  // W3C zero ids are invalid — mirror Go's IsValid
    }
    std::memcpy(out.trace_id, tp + 3, 32);
    out.trace_id[32] = '\0';
    std::memcpy(out.parent_span_id, tp + 36, 16);
    out.parent_span_id[16] = '\0';
    out.sampled = (tp[53] == '0' && tp[54] == '1');
    std::memcpy(out.block, frame, kTraceBlockLen);
    return true;
}

// --- Outbound echo ----------------------------------------------------------

// TraceSlot is the per-dispatch trace binding: EnginePump arms it before
// dispatching a traced frame and disarms when dispatch returns. Publishers
// read it at send time — frames emitted inside the dispatch window are the
// responses the contract echoes the block on. Single writer, no sync.
class TraceSlot {
   public:
    void arm(const uint8_t* block64) noexcept {
        std::memcpy(block_, block64, kTraceBlockLen);
        armed_ = true;
    }
    void disarm() noexcept { armed_ = false; }
    [[nodiscard]] bool armed() const noexcept { return armed_; }
    [[nodiscard]] const uint8_t* block() const noexcept { return block_; }

   private:
    uint8_t block_[kTraceBlockLen] = {};
    bool armed_ = false;
};

// compose_traced_frame writes [64B block][payload] into dst. Returns
// false when dst_cap is too small — callers then send the payload
// untraced rather than dropping the message (the field is metadata;
// send correctness outranks trace fidelity).
[[nodiscard]] inline bool compose_traced_frame(const TraceSlot& slot,
                                               const uint8_t* payload,
                                               uint32_t len, uint8_t* dst,
                                               uint32_t dst_cap) noexcept {
    if (dst == nullptr ||
        static_cast<uint64_t>(len) + kTraceBlockLen > dst_cap) {
        return false;
    }
    std::memcpy(dst, slot.block(), kTraceBlockLen);
    if (len > 0) std::memcpy(dst + kTraceBlockLen, payload, len);
    return true;
}

// --- Span emission ----------------------------------------------------------

// mint_span_id renders a fresh 16-hex span id — splitmix64 over a
// per-pump counter XOR'd with a time salt. Span ids must be unique within
// a trace; a monotonically-mixed process counter guarantees that.
[[nodiscard]] inline uint64_t mint_span_id(uint64_t salt) noexcept {
    uint64_t z = salt + 0x9E3779B97F4A7C15ull;
    z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ull;
    z = (z ^ (z >> 27)) * 0x94D049BB133111EBull;
    return z ^ (z >> 31);
}

inline void render_hex16(uint64_t v, char out[17]) noexcept {
    static constexpr char kHex[] = "0123456789abcdef";
    for (int i = 15; i >= 0; --i) {
        out[i] = kHex[v & 0xF];
        v >>= 4;
    }
    out[16] = '\0';
}

// CoreSpan is the engine's completed-span record handed to the span sink.
// Field names mirror tracing.SpanData so the OTLP formatter below emits
// the identical JSON the Go FileExporter writes.
struct CoreSpan {
    char name[32];           // "order.match"
    char trace_id[33];
    char parent_span_id[17];  // "" for a root (never — remote parented here)
    char span_id[17];
    int32_t kind;             // kSpanKindConsumer
    uint64_t start_unix_ns;
    uint64_t end_unix_ns;
    uint8_t event_type;       // exc::wire::EventType ordinal (0xFF = unknown)
    bool sampled;
};

// format_span_jsonl renders ONE OTLP/HTTP ExportTraceServiceRequest line
// — byte-compatible with Go's EncodeOTLPPayload so the node-local
// collector tail (deploy/otel/) ingests engine spans unchanged.
// Returns bytes written, 0 when dst is too small (span dropped — tracing
// never faults the hot path).
inline uint32_t format_span_jsonl(const CoreSpan& s, const char* service,
                                  char* dst, uint32_t dst_cap) noexcept {
    if (dst == nullptr || dst_cap == 0) return 0;
    const int n = std::snprintf(
        dst, dst_cap,
        "{\"resourceSpans\":[{\"resource\":{\"attributes\":["
        "{\"key\":\"service.name\",\"value\":{\"stringValue\":\"%s\"}},"
        "{\"key\":\"telemetry.sdk.name\",\"value\":{\"stringValue\":\"exchange-lite-otlp\"}},"
        "{\"key\":\"telemetry.sdk.language\",\"value\":{\"stringValue\":\"c++\"}}]},"
        "\"scopeSpans\":[{\"scope\":{\"name\":\"exchange.tracing\"},\"spans\":[{"
        "\"traceId\":\"%s\",\"spanId\":\"%s\",\"parentSpanId\":\"%s\","
        "\"name\":\"%s\",\"kind\":%d,"
        "\"startTimeUnixNano\":\"%llu\",\"endTimeUnixNano\":\"%llu\","
        "\"attributes\":[{\"key\":\"wire.event_type\",\"value\":{\"intValue\":\"%u\"}}],"
        "\"events\":[],\"status\":{\"code\":%d,\"message\":\"\"}}]}]}]}\n",
        service != nullptr ? service : "matching-engine", s.trace_id,
        s.span_id, s.parent_span_id, s.name, s.kind,
        static_cast<unsigned long long>(s.start_unix_ns),
        static_cast<unsigned long long>(s.end_unix_ns),
        static_cast<unsigned>(s.event_type),
        s.sampled ? 1 : 0);
    if (n <= 0 || static_cast<uint32_t>(n) >= dst_cap) return 0;
    return static_cast<uint32_t>(n);
}

// file_span_sink is a ready-to-use EnginePump::span_sink_fn writing one
// OTLP JSONL line per span to a std::FILE* opened in append mode — the
// node-local scrape path (EXC_TRACE_FILE in main.cpp). ctx is the FILE*.
inline void file_span_sink(void* ctx, const CoreSpan& span) noexcept {
    auto* f = static_cast<std::FILE*>(ctx);
    if (f == nullptr) return;
    char line[2048];
    const uint32_t n =
        format_span_jsonl(span, "matching-engine", line, sizeof(line));
    if (n == 0) return;
    const uint32_t wrote =
        static_cast<uint32_t>(std::fwrite(line, 1, n, f));
    (void)wrote;  // tracing never blocks on the sink — a short write drops
    std::fflush(f);  // spans are diagnostics; flush keeps tails alive
}

}  // namespace exch
