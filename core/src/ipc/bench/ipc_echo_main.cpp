// Task 1.3.5 — standalone IPC echo/loopback helper used by the Go
// integration tests and deploy/scripts/bench_ipc.sh. NOT a CMake target:
// compiled directly, e.g.
//
//   g++ -std=c++20 -O2 -DEXCH_WITH_AERON=1 \
//       -Icore/include -Icore/proto/gen -Icore/third_party/aeron/include/cpp \
//       core/src/ipc/bench/ipc_echo_main.cpp \
//       core/src/ipc/SharedMemChannel.cpp core/src/ipc/AeronChannel.cpp \
//       core/src/ipc/IpcChannel.cpp \
//       core/third_party/aeron/lib/libaeron_client.a -lpthread -lrt \
//       -o /tmp/ipc_echo
//
// Modes:
//   ipc_echo shm   <base> <shard> <count> <timeout_ms>
//       Attach/create as Endpoint::Core; echo every inbound Event{OrderNew}
//       as Event{TradeFill} on _out, preserving Event.seq and Event.ts.
//   ipc_echo aeron <aeron_dir> <count> <timeout_ms>
//       Same over AeronChannel (sub aeron:ipc?alias=orders_in/1001,
//       pub aeron:ipc?alias=orders_out/1002).
//
// Exits 0 once `count` echoes are published (prints stats to stdout), 1 on
// open failure, 2 on timeout. Build flags matter for fidelity: use -O2.

#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>

#include "exchange_generated.h"
#include "ipc/AeronChannel.hpp"
#include "ipc/SharedMemChannel.hpp"

using namespace exch;

namespace {

// Build the TradeFill echo for an inbound OrderNew event into `builder`.
// The returned span aliases builder memory — publish before next use.
flatbuffers::FlatBufferBuilder g_builder(512);

std::pair<const uint8_t*, uint32_t> echo_fill(const exc::wire::Event* in) {
    const auto* on = in->type_as_OrderNew();
    g_builder.Clear();
    exc::wire::TradeFillBuilder tb(g_builder);
    tb.add_trade_id(9000 + in->seq());
    tb.add_buy_order_id(on != nullptr ? on->order_id() : 0);
    tb.add_sell_order_id(on != nullptr ? on->order_id() + 1 : 0);
    tb.add_price(on != nullptr ? on->price() : 0);
    tb.add_qty(on != nullptr ? on->qty() : 0);
    tb.add_seq(in->seq());
    const auto tf = tb.Finish();
    exc::wire::EventBuilder eb(g_builder);
    eb.add_seq(in->seq());
    eb.add_ts(in->ts());
    eb.add_type_type(exc::wire::EventType_TradeFill);
    eb.add_type(tf.Union());
    g_builder.Finish(eb.Finish());
    return { g_builder.GetBufferPointer(), g_builder.GetSize() };
}

bool is_order(const exc::wire::Event* ev) {
    return ev != nullptr &&
           ev->type_type() == exc::wire::EventType_OrderNew;
}

int run_shm(const char* base, uint16_t shard, uint64_t want, int64_t timeout_ms) {
    SharedMemChannel ch(base, shard, SharedMemChannel::Endpoint::Core, true);
    if (!ch.open()) {
        std::fprintf(stderr, "shm: open failed\n");
        return 1;
    }
    std::fprintf(stderr, "shm: channel open base=%s shard=%u\n", base,
                 static_cast<unsigned>(shard));
    uint64_t echoed = 0;
    const int64_t deadline =
        ShmRing::mono_ns() + timeout_ms * 1'000'000LL;
    while (echoed < want && ShmRing::mono_ns() < deadline) {
        uint32_t len = 0;
        const uint8_t* p = ch.poll_view(&len);  // zero-copy into ring slot
        if (p == nullptr)
            continue;
        const auto* ev = exc::wire::GetEvent(p);
        if (is_order(ev)) {
            auto [out, out_len] = echo_fill(ev);
            // bounded spin on backpressure
            while (!ch.send(out, out_len)) {
                if (ShmRing::mono_ns() > deadline) {
                    std::fprintf(stderr, "shm: send stuck\n");
                    return 2;
                }
            }
            echoed++;
        }
        ch.consume();
    }
    std::printf("shm: echoed=%llu want=%llu occupancy=%llu drops=%llu\n",
                static_cast<unsigned long long>(echoed),
                static_cast<unsigned long long>(want),
                static_cast<unsigned long long>(ch.occupancy()),
                static_cast<unsigned long long>(ch.drops()));
    return echoed == want ? 0 : 2;
}

int run_aeron(const char* dir, uint64_t want, int64_t timeout_ms) {
#if EXCH_WITH_AERON
    AeronChannelConfig cfg;
    cfg.aeron_dir = dir;
    AeronChannel ch(cfg);
    if (!ch.open()) {
        std::fprintf(stderr, "aeron: open failed (dir=%s)\n", dir);
        return 1;
    }
    std::fprintf(stderr, "aeron: channel open dir=%s\n", dir);

    struct Ctx {
        SharedMemChannel* unused;
        AeronChannel* ch;
        uint64_t echoed;
        int64_t deadline;
        bool stuck;
    } ctx { nullptr, &ch, 0, ShmRing::mono_ns() + timeout_ms * 1'000'000LL,
            false };

    while (ctx.echoed < want && ShmRing::mono_ns() < ctx.deadline) {
        ch.poll_each(
            [](void* c, const uint8_t* data, uint32_t len) {
                auto* cc = static_cast<Ctx*>(c);
                flatbuffers::Verifier v(data, len);
                if (!v.VerifyBuffer<exc::wire::Event>())
                    return;
                const auto* ev = exc::wire::GetEvent(data);
                if (!is_order(ev))
                    return;
                auto [out, out_len] = echo_fill(ev);
                while (!cc->ch->send(out, out_len)) {
                    if (ShmRing::mono_ns() > cc->deadline) {
                        cc->stuck = true;
                        return;
                    }
                }
                cc->echoed++;
            },
            &ctx, cfg.poll_fragment_limit);
        if (ctx.stuck)
            return 2;
    }
    std::printf("aeron: echoed=%llu want=%llu\n",
                static_cast<unsigned long long>(ctx.echoed),
                static_cast<unsigned long long>(want));
    return ctx.echoed == want ? 0 : 2;
#else
    (void)dir; (void)want; (void)timeout_ms;
    std::fprintf(stderr, "aeron: built without EXCH_WITH_AERON\n");
    return 1;
#endif
}

}  // namespace

int main(int argc, char** argv) {
    if (argc < 2) {
        std::fprintf(stderr,
                     "usage: %s shm <base> <shard> <count> <timeout_ms>\n"
                     "       %s aeron <aeron_dir> <count> <timeout_ms>\n",
                     argv[0], argv[0]);
        return 64;
    }
    if (std::strcmp(argv[1], "shm") == 0 && argc >= 6) {
        return run_shm(argv[2],
                       static_cast<uint16_t>(std::strtoul(argv[3], nullptr, 10)),
                       std::strtoull(argv[4], nullptr, 10),
                       std::strtoll(argv[5], nullptr, 10));
    }
    if (std::strcmp(argv[1], "aeron") == 0 && argc >= 5) {
        return run_aeron(argv[2], std::strtoull(argv[3], nullptr, 10),
                         std::strtoll(argv[4], nullptr, 10));
    }
    std::fprintf(stderr, "bad args\n");
    return 64;
}
