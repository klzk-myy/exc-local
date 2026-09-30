// Task 9.3.11 — C++ half of trace_id continuity
// HTTP -> Aeron -> C++ -> Aeron -> Go (spec §19.12 "Aeron trace_id header
// format", §24 #112): the 64-byte EXCTRACE block at frame offset 0 is
// consumed by EnginePump, echoed verbatim on every outbound frame the
// engine emits inside the dispatch window (IpcPublisher/L3Publisher), and
// mints an `order.match` span remote-parented on the inbound traceparent.

#include <gtest/gtest.h>
#include <unistd.h>

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <deque>
#include <filesystem>
#include <string>
#include <vector>

#include "exchange_generated.h"
#include "ipc/EnginePump.hpp"
#include "ipc/IpcChannel.hpp"
#include "ipc/TraceContext.hpp"
#include "matching/IpcPublisher.hpp"
#include "utils/MemoryPool.hpp"

namespace {

constexpr char kTp[] =
    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";

class FakeChannel final : public exch::IpcChannel {
   public:
    bool open() noexcept override { open_ = true; return true; }
    void close() noexcept override { open_ = false; }
    [[nodiscard]] bool is_open() const noexcept override { return open_; }
    bool send(const void* data, uint32_t len) noexcept override {
        sent.emplace_back(static_cast<const uint8_t*>(data),
                          static_cast<const uint8_t*>(data) + len);
        return true;
    }
    int32_t poll(void* buf, uint32_t cap) noexcept override {
        if (inq.empty()) return 0;
        const auto& m = inq.front();
        if (m.size() > cap) return -1;
        std::memcpy(buf, m.data(), m.size());
        const auto n = static_cast<int32_t>(m.size());
        inq.pop_front();
        return n;
    }
    bool open_ = false;
    std::deque<std::vector<uint8_t>> inq;
    std::vector<std::vector<uint8_t>> sent;
};

// Ingress that emits a TradeFill while dispatching — the engine's
// "response frame" the contract echoes the trace block on.
class EchoingIngress final : public exch::IEngineIngress {
   public:
    void on_order_received(exch::Order* o) noexcept override {
        orders.push_back(*o);
        if (pub != nullptr) {
            (void)pub->publish_trade(/*trade_id=*/7, o->id, 2,
                                     /*price_ticks=*/105'000'000,
                                     /*qty_units=*/150'000'000,
                                     /*engine_seq=*/9,
                                     /*ts_ns=*/123456789);
        }
    }
    void on_cancel_received(uint64_t order_id,
                            uint64_t account_id) noexcept override {
        cancels.emplace_back(order_id, account_id);
    }
    void on_time_tick(uint64_t now_ns) noexcept override {
        ticks.push_back(now_ns);
    }
    exch::IpcPublisher* pub = nullptr;
    std::vector<exch::Order> orders;
    std::vector<std::pair<uint64_t, uint64_t>> cancels;
    std::vector<uint64_t> ticks;
};

struct SpanLog {
    std::vector<exch::CoreSpan> spans;
    static void sink(void* ctx, const exch::CoreSpan& s) {
        static_cast<SpanLog*>(ctx)->spans.push_back(s);
    }
};

// Build the 64B EXCTRACE block exactly like Go's InjectAeronTrace:
// zeroed block, magic at [0:8], traceparent left-justified at [8:63).
std::vector<uint8_t> make_block(const char* traceparent) {
    std::vector<uint8_t> b(exch::kTraceBlockLen, 0);
    std::memcpy(b.data(), exch::kTraceMagic, 8);
    if (traceparent != nullptr) {
        std::memcpy(b.data() + 8, traceparent, std::strlen(traceparent));
    }
    return b;
}

std::vector<uint8_t> make_order_new(uint64_t seq, uint64_t order_id,
                                    uint64_t account_id, int64_t qty,
                                    int64_t price, uint64_t ts) {
    flatbuffers::FlatBufferBuilder b(256);
    const auto coid = b.CreateString("c-" + std::to_string(seq));
    exc::wire::OrderNewBuilder ob(b);
    ob.add_order_id(order_id);
    ob.add_account_id(account_id);
    ob.add_instrument_id(3);
    ob.add_side(exc::wire::Side_Sell);
    ob.add_type(exc::wire::OrderType_Limit);
    ob.add_qty(qty);
    ob.add_price(price);
    ob.add_tif(exc::wire::TimeInForce_GTD);
    ob.add_client_order_id(coid);
    const auto on = ob.Finish();
    exc::wire::EventBuilder eb(b);
    eb.add_seq(seq);
    eb.add_ts(ts);
    eb.add_type_type(exc::wire::EventType_OrderNew);
    eb.add_type(on.Union());
    b.Finish(eb.Finish());
    return {b.GetBufferPointer(), b.GetBufferPointer() + b.GetSize()};
}

std::vector<uint8_t> framed(const std::vector<uint8_t>& block,
                            const std::vector<uint8_t>& payload) {
    std::vector<uint8_t> f = block;
    f.insert(f.end(), payload.begin(), payload.end());
    return f;
}

// Decode the Event inside a (possibly traced) wire frame.
const exc::wire::Event* decode_frame(const std::vector<uint8_t>& f) {
    if (exch::has_trace_block(f.data(), static_cast<uint32_t>(f.size()))) {
        return exc::wire::GetEvent(f.data() + exch::kTraceBlockLen);
    }
    return exc::wire::GetEvent(f.data());
}

// --- Codec ------------------------------------------------------------------

TEST(TraceCodec, ValidBlockParses) {
    const auto blk = make_block(kTp);
    exch::CoreSpanContext c{};
    ASSERT_TRUE(exch::parse_trace_context(blk.data(),
                                        exch::kTraceBlockLen, c));
    EXPECT_STREQ(c.trace_id, "4bf92f3577b34da6a3ce929d0e0e4736");
    EXPECT_STREQ(c.parent_span_id, "00f067aa0ba902b7");
    EXPECT_TRUE(c.sampled);
    // Verbatim copy for echo — byte-identical block.
    EXPECT_EQ(0, std::memcmp(c.block, blk.data(), exch::kTraceBlockLen));
}

TEST(TraceCodec, RejectsAbsentShortAndMalformed) {
    EXPECT_FALSE(exch::has_trace_block(nullptr, 128));
    EXPECT_FALSE(exch::has_trace_block(
        reinterpret_cast<const uint8_t*>("EXCTRACE"), 8));  // too short
    auto blk = make_block(kTp);
    blk[0] = 'X';
    exch::CoreSpanContext c{};
    EXPECT_FALSE(
        exch::parse_trace_context(blk.data(), exch::kTraceBlockLen, c));

    // Magic present but malformed parent variants — all untraced.
    blk = make_block("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b");  // short
    EXPECT_FALSE(
        exch::parse_trace_context(blk.data(), exch::kTraceBlockLen, c));
    blk = make_block("00-4bf92f3577b34da6a3ce929d0e0e473G-00f067aa0ba902b7-01");  // bad hex
    EXPECT_FALSE(
        exch::parse_trace_context(blk.data(), exch::kTraceBlockLen, c));
    blk = make_block("00-00000000000000000000000000000000-00f067aa0ba902b7-01");  // zero trace id
    EXPECT_FALSE(
        exch::parse_trace_context(blk.data(), exch::kTraceBlockLen, c));
    blk = make_block("00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01");  // zero span id
    EXPECT_FALSE(
        exch::parse_trace_context(blk.data(), exch::kTraceBlockLen, c));
}

// --- EnginePump path ---------------------------------------------------------

TEST(TraceDispatch, TracedOrderNewDecodesAndEchoesOnResponse) {
    FakeChannel in, out;
    EchoingIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    exch::IpcPublisher pub(&out);
    eng.pub = &pub;
    (void)in.open();
    (void)out.open();

    exch::EnginePump pump(&in, &out, &eng, &pool);
    pub.set_trace_slot(pump.trace_slot());
    SpanLog slog;
    pump.set_span_sink(&SpanLog::sink, &slog);

    const auto block = make_block(kTp);
    in.inq.push_back(
        framed(block, make_order_new(/*seq=*/42, /*order_id=*/9001,
                                     /*account_id=*/77, /*qty=*/150'000'000,
                                     /*price=*/105'000'000, /*ts=*/7'777'000)));

    EXPECT_EQ(pump.run_once(4), 1u);
    ASSERT_EQ(eng.orders.size(), 1u);
    EXPECT_EQ(eng.orders[0].id, 9001u);
    EXPECT_EQ(pump.traced_frames_in(), 1u);

    // The `order.match` span, remote-parented on the frame's span id.
    ASSERT_EQ(slog.spans.size(), 1u);
    const exch::CoreSpan& s = slog.spans[0];
    EXPECT_STREQ(s.name, "order.match");
    EXPECT_STREQ(s.trace_id, "4bf92f3577b34da6a3ce929d0e0e4736");
    EXPECT_STREQ(s.parent_span_id, "00f067aa0ba902b7");
    EXPECT_EQ(std::strlen(s.span_id), 16u);
    EXPECT_EQ(s.kind, exch::kSpanKindConsumer);
    EXPECT_EQ(s.event_type,
              static_cast<uint8_t>(exc::wire::EventType_OrderNew));
    EXPECT_GE(s.end_unix_ns, s.start_unix_ns);
    EXPECT_EQ(pump.spans_emitted(), 1u);

    // The response frame carries the block verbatim + the Event at +64.
    ASSERT_EQ(out.sent.size(), 1u);
    const auto& f = out.sent[0];
    ASSERT_EQ(0, std::memcmp(f.data(), block.data(), exch::kTraceBlockLen));
    const auto* ev = decode_frame(f);
    ASSERT_NE(ev, nullptr);
    EXPECT_EQ(ev->type_type(), exc::wire::EventType_TradeFill);
    const auto* tf = ev->type_as_TradeFill();
    ASSERT_NE(tf, nullptr);
    EXPECT_EQ(tf->trade_id(), 7u);
    EXPECT_EQ(tf->buy_order_id(), 9001u);
}

TEST(TraceDispatch, UntracedFrameKeepsLegacyWireShape) {
    FakeChannel in, out;
    EchoingIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    exch::IpcPublisher pub(&out);
    eng.pub = &pub;
    (void)in.open();
    (void)out.open();
    exch::EnginePump pump(&in, &out, &eng, &pool);
    pub.set_trace_slot(pump.trace_slot());
    SpanLog slog;
    pump.set_span_sink(&SpanLog::sink, &slog);

    in.inq.push_back(make_order_new(1, 100, 5, 1'000'000, 99'000'000, 1));
    EXPECT_EQ(pump.run_once(4), 1u);
    EXPECT_EQ(pump.traced_frames_in(), 0u);
    EXPECT_TRUE(slog.spans.empty());

    // No prefix: the response decodes at offset 0 — legacy consumers
    // (gateway/shm readers) see an unchanged wire shape.
    ASSERT_EQ(out.sent.size(), 1u);
    EXPECT_FALSE(exch::has_trace_block(out.sent[0].data(),
                                       static_cast<uint32_t>(out.sent[0].size())));
    const auto* ev = exc::wire::GetEvent(out.sent[0].data());
    ASSERT_NE(ev, nullptr);
    EXPECT_EQ(ev->type_type(), exc::wire::EventType_TradeFill);
}

TEST(TraceDispatch, MalformedBlockStillDecodesUntraced) {
    FakeChannel in, out;
    EchoingIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    exch::IpcPublisher pub(&out);
    eng.pub = &pub;
    (void)in.open();
    (void)out.open();
    exch::EnginePump pump(&in, &out, &eng, &pool);
    pub.set_trace_slot(pump.trace_slot());

    // Magic'd header with a garbage traceparent — the block is still the
    // reserved 64B header, so the payload decodes at +64 untraced
    // (contract: invalid block = untraced frame, never a fault).
    auto blk = make_block("totally-not-a-traceparent.....................");
    in.inq.push_back(
        framed(blk, make_order_new(2, 200, 5, 1'000'000, 99'000'000, 2)));

    EXPECT_EQ(pump.run_once(4), 1u);
    ASSERT_EQ(eng.orders.size(), 1u);
    EXPECT_EQ(eng.orders[0].id, 200u);
    EXPECT_EQ(pump.traced_frames_in(), 0u);
    ASSERT_EQ(out.sent.size(), 1u);
    EXPECT_FALSE(exch::has_trace_block(
        out.sent[0].data(), static_cast<uint32_t>(out.sent[0].size())));
}

TEST(TraceDispatch, TraceScopeClearsBetweenFrames) {
    FakeChannel in, out;
    EchoingIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    exch::IpcPublisher pub(&out);
    eng.pub = &pub;
    (void)in.open();
    (void)out.open();
    exch::EnginePump pump(&in, &out, &eng, &pool);
    pub.set_trace_slot(pump.trace_slot());

    constexpr char kTp2[] =
        "00-aaaa1111bbbb2222cccc3333dddd4444-1111222233334444-00";
    in.inq.push_back(framed(make_block(kTp),
                            make_order_new(1, 300, 5, 1'000'000, 9'000'000, 1)));
    in.inq.push_back(make_order_new(2, 301, 5, 1'000'000, 9'000'000, 2));
    in.inq.push_back(framed(make_block(kTp2),
                            make_order_new(3, 302, 5, 1'000'000, 9'000'000, 3)));

    EXPECT_EQ(pump.run_once(8), 3u);
    EXPECT_EQ(pump.traced_frames_in(), 2u);
    ASSERT_EQ(out.sent.size(), 3u);
    // Frame 1 traced with kTp, frame 2 untraced (slot disarmed), frame 3
    // traced with kTp2 — no leakage of the previous command's context.
    EXPECT_TRUE(exch::has_trace_block(
        out.sent[0].data(), static_cast<uint32_t>(out.sent[0].size())));
    EXPECT_EQ(0, std::memcmp(out.sent[0].data() + 8, kTp, std::strlen(kTp)));
    EXPECT_FALSE(exch::has_trace_block(
        out.sent[1].data(), static_cast<uint32_t>(out.sent[1].size())));
    EXPECT_TRUE(exch::has_trace_block(
        out.sent[2].data(), static_cast<uint32_t>(out.sent[2].size())));
    EXPECT_EQ(0, std::memcmp(out.sent[2].data() + 8, kTp2, std::strlen(kTp2)));
}

// --- OTLP JSONL sink ---------------------------------------------------------

TEST(TraceSink, FileSinkWritesOtlpJsonLine) {
    const auto dir = std::filesystem::temp_directory_path() /
                     ("exch_trace_" + std::to_string(::getpid()));
    std::filesystem::create_directories(dir);
    const std::string path = (dir / "spans.jsonl").string();
    std::FILE* f = std::fopen(path.c_str(), "w");
    ASSERT_NE(f, nullptr);

    exch::CoreSpan s{};
    std::snprintf(s.name, sizeof(s.name), "order.match");
    std::snprintf(s.trace_id, sizeof(s.trace_id), "%s",
                  "4bf92f3577b34da6a3ce929d0e0e4736");
    std::snprintf(s.parent_span_id, sizeof(s.parent_span_id), "%s",
                  "00f067aa0ba902b7");
    exch::render_hex16(0xdeadbeefcafe1234ull, s.span_id);
    s.kind = exch::kSpanKindConsumer;
    s.start_unix_ns = 1'000;
    s.end_unix_ns = 2'000;
    s.event_type = 0;
    s.sampled = true;
    exch::file_span_sink(f, s);
    std::fclose(f);

    std::FILE* r = std::fopen(path.c_str(), "r");
    ASSERT_NE(r, nullptr);
    char buf[4096];
    const auto n = std::fread(buf, 1, sizeof(buf) - 1, r);
    std::fclose(r);
    buf[n] = '\0';
    const std::string line(buf);
    EXPECT_NE(line.find("\"service.name\",\"value\":{\"stringValue\":\"matching-engine\""),
              std::string::npos);
    EXPECT_NE(line.find("\"traceId\":\"4bf92f3577b34da6a3ce929d0e0e4736\""),
              std::string::npos);
    EXPECT_NE(line.find("\"parentSpanId\":\"00f067aa0ba902b7\""),
              std::string::npos);
    EXPECT_NE(line.find("\"spanId\":\"deadbeefcafe1234\""), std::string::npos);
    EXPECT_NE(line.find("\"name\":\"order.match\""), std::string::npos);
    EXPECT_EQ(line.back(), '\n');
    std::filesystem::remove_all(dir);
}

}  // namespace
