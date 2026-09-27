// Task 1.3.5 — IPC transport tests: shm SPSC ring, SharedMemChannel,
// AeronChannel (driver-gated), FlatBuffers wire schema.

#include <gtest/gtest.h>

#include <atomic>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <thread>
#include <unistd.h>
#include <vector>

#include "exchange_generated.h"
#include "ipc/AeronChannel.hpp"
#include "ipc/SharedMemChannel.hpp"
#include "ipc/ShmRing.hpp"

namespace {

// -- helpers ----------------------------------------------------------------

std::string uniq_base(const char* tag) {
    char buf[128];
    std::snprintf(buf, sizeof(buf), "exch_test_%s_%d", tag,
                  static_cast<int>(::getpid()));
    return { buf };
}

void unlink_ring(const std::string& name) {
    ::shm_unlink(("/" + name).c_str());
}

void unlink_channel(const std::string& base, uint16_t shard) {
    unlink_ring(exch::SharedMemChannel::in_name(base, shard));
    unlink_ring(exch::SharedMemChannel::out_name(base, shard));
}

std::vector<uint8_t> make_order_new_event(uint64_t seq, uint64_t order_id) {
    flatbuffers::FlatBufferBuilder b(256);
    const auto coid = b.CreateString("cli-" + std::to_string(seq));
    exc::wire::OrderNewBuilder ob(b);
    ob.add_order_id(order_id);
    ob.add_account_id(1000 + seq);
    ob.add_instrument_id(3);
    ob.add_side(exc::wire::Side_Buy);
    ob.add_type(exc::wire::OrderType_Limit);
    ob.add_qty(100000000 + static_cast<int64_t>(seq));
    ob.add_price(105000000);
    ob.add_tif(exc::wire::TimeInForce_GTC);
    ob.add_client_order_id(coid);
    const auto on = ob.Finish();
    exc::wire::EventBuilder eb(b);
    eb.add_seq(seq);
    eb.add_ts(static_cast<uint64_t>(exch::ShmRing::mono_ns()));
    eb.add_type_type(exc::wire::EventType_OrderNew);
    eb.add_type(on.Union());
    b.Finish(eb.Finish());
    return { b.GetBufferPointer(), b.GetBufferPointer() + b.GetSize() };
}

// Expected OrderNew fields for a message built by make_order_new_event.
bool check_order_new(const uint8_t* data, uint32_t len, uint64_t want_seq) {
    flatbuffers::Verifier v(data, len);
    if (!v.VerifyBuffer<exc::wire::Event>())
        return false;
    const auto* ev = exc::wire::GetEvent(data);
    if (ev->seq() != want_seq ||
        ev->type_type() != exc::wire::EventType_OrderNew)
        return false;
    const auto* on = ev->type_as_OrderNew();
    return on != nullptr && on->instrument_id() == 3 &&
           on->price() == 105000000 &&
           on->qty() == 100000000 + static_cast<int64_t>(want_seq);
}

std::vector<uint8_t> make_trade_fill_event(uint64_t seq, uint64_t order_id) {
    flatbuffers::FlatBufferBuilder b(256);
    exc::wire::TradeFillBuilder tb(b);
    tb.add_trade_id(9000 + seq);
    tb.add_buy_order_id(order_id);
    tb.add_sell_order_id(order_id + 1);
    tb.add_price(105000000);
    tb.add_qty(100000000);
    tb.add_seq(seq);
    const auto tf = tb.Finish();
    exc::wire::EventBuilder eb(b);
    eb.add_seq(seq);
    eb.add_ts(static_cast<uint64_t>(exch::ShmRing::mono_ns()));
    eb.add_type_type(exc::wire::EventType_TradeFill);
    eb.add_type(tf.Union());
    b.Finish(eb.Finish());
    return { b.GetBufferPointer(), b.GetBufferPointer() + b.GetSize() };
}

bool aeron_driver_up(std::string* dir_out) {
    const char* env = std::getenv("EXCH_IPC_AERON_DIR");
    std::vector<std::string> candidates;
    if (env != nullptr && *env != '\0')
        candidates.emplace_back(env);
    if (const char* user = std::getenv("USER"))
        candidates.push_back(std::string("/dev/shm/aeron-") + user);
    candidates.emplace_back("/tmp/aeron-exc");
    for (const auto& d : candidates) {
        struct stat st {};
        if (::stat((d + "/cnc.dat").c_str(), &st) == 0) {
            *dir_out = d;
            return true;
        }
    }
    return false;
}

// -- ShmRing ----------------------------------------------------------------

TEST(ShmRing, LayoutConstants) {
    using R = exch::ShmRing;
    EXPECT_EQ(R::kOffHead, 0u);
    EXPECT_EQ(R::kOffTail, 64u);
    EXPECT_EQ(R::kOffHeartbeat, 128u);
    EXPECT_EQ(R::kOffPid, 192u);
    EXPECT_EQ(R::kOffMagic, 256u);
    EXPECT_EQ(R::kOffCapacity, 264u);
    EXPECT_EQ(R::kOffSlotPayload, 272u);
    EXPECT_EQ(R::kOffDrops, 280u);
    EXPECT_EQ(R::kOffSlots, 320u);
    EXPECT_EQ(R::kSlotHeaderBytes, 64u);
    EXPECT_EQ(R::kOffSlots % 64u, 0u);  // slots stay cache-line aligned
}

TEST(ShmRing, ZeroLoss1000InProcess) {
    const std::string base = uniq_base("shm1k");
    const std::string name = base + "_ring";
    unlink_ring(name);
    {
        exch::ShmRing prod(name, exch::ShmRing::Role::Producer, true);
        exch::ShmRing cons(name, exch::ShmRing::Role::Consumer, false);
        ASSERT_TRUE(prod.is_open());
        ASSERT_TRUE(cons.is_open());

        std::atomic<uint64_t> got{0};
        std::atomic<bool> bad{false};
        std::thread consumer([&] {
            uint64_t want = 0;
            const int64_t deadline =
                exch::ShmRing::mono_ns() + 10'000'000'000LL;
            while (want < 1000 && exch::ShmRing::mono_ns() < deadline) {
                uint32_t len = 0;
                const uint8_t* p = cons.peek(&len);
                if (p == nullptr)
                    continue;
                if (!check_order_new(p, len, want))
                    bad = true;
                cons.consume();
                want++;
            }
            got = want;
        });

        for (uint64_t i = 0; i < 1000; i++) {
            const auto msg = make_order_new_event(i, 5000 + i);
            ASSERT_TRUE(prod.write_wait(msg.data(),
                                        static_cast<uint32_t>(msg.size()),
                                        5'000'000'000LL));
        }
        consumer.join();
        EXPECT_EQ(got.load(), 1000u);
        EXPECT_FALSE(bad.load());
        EXPECT_EQ(prod.drops(), 0u);
    }
    unlink_ring(name);
}

// Cross-process proof: forked child consumes what the parent produces.
TEST(ShmRing, ZeroLoss1000AcrossProcess) {
    const std::string base = uniq_base("shmfork");
    const std::string name = base + "_ring";
    unlink_ring(name);

    exch::ShmRing prod(name, exch::ShmRing::Role::Producer, true);
    ASSERT_TRUE(prod.is_open());

    const pid_t pid = ::fork();
    ASSERT_NE(pid, -1);
    if (pid == 0) {
        // child — consumer endpoint over its own mapping of the same shm obj
        exch::ShmRing cons(name, exch::ShmRing::Role::Consumer, false);
        if (!cons.is_open())
            _exit(2);
        uint64_t want = 0;
        const int64_t deadline =
            exch::ShmRing::mono_ns() + 10'000'000'000LL;
        while (want < 1000 && exch::ShmRing::mono_ns() < deadline) {
            uint32_t len = 0;
            const uint8_t* p = cons.peek(&len);
            if (p == nullptr)
                continue;
            if (!check_order_new(p, len, want))
                _exit(3);
            cons.consume();
            want++;
        }
        _exit(want == 1000 ? 0 : 4);
    }

    for (uint64_t i = 0; i < 1000; i++) {
        const auto msg = make_order_new_event(i, 6000 + i);
        ASSERT_TRUE(prod.write_wait(msg.data(),
                                    static_cast<uint32_t>(msg.size()),
                                    10'000'000'000LL));
    }
    int status = 0;
    ASSERT_EQ(::waitpid(pid, &status, 0), pid);
    EXPECT_TRUE(WIFEXITED(status));
    EXPECT_EQ(WEXITSTATUS(status), 0);
    prod.close();
    unlink_ring(name);
}

TEST(ShmRing, BackpressureWhenFull) {
    const std::string name = uniq_base("shmfull") + "_ring";
    unlink_ring(name);
    {
        const uint32_t cap = 8;
        exch::ShmRing prod(name, exch::ShmRing::Role::Producer, true,
                           cap, exch::ShmRing::kDefaultSlotPayload);
        exch::ShmRing cons(name, exch::ShmRing::Role::Consumer, false);
        ASSERT_TRUE(prod.is_open() && cons.is_open());

        const auto msg = make_order_new_event(0, 1);
        for (uint32_t i = 0; i < cap; i++)
            ASSERT_TRUE(prod.try_write(msg.data(),
                                       static_cast<uint32_t>(msg.size())));
        // Ring full: producer declines and counts the drop.
        EXPECT_FALSE(prod.try_write(msg.data(),
                                    static_cast<uint32_t>(msg.size())));
        EXPECT_GE(prod.drops(), 1u);
        EXPECT_EQ(prod.occupancy(), cap);

        // Drain one slot: producer can write again.
        uint32_t len = 0;
        ASSERT_NE(cons.peek(&len), nullptr);
        cons.consume();
        EXPECT_TRUE(prod.try_write(msg.data(),
                                   static_cast<uint32_t>(msg.size())));
        EXPECT_EQ(prod.occupancy(), cap);
    }
    unlink_ring(name);
}

// Producer-death detection: pid stamped in the header, kill(pid,0) on the
// consumer side flips once the writer exits.
TEST(ShmRing, ProducerDeathDetection) {
    const std::string name = uniq_base("shmdie") + "_ring";
    unlink_ring(name);
    exch::ShmRing cons(name, exch::ShmRing::Role::Consumer, true);
    ASSERT_TRUE(cons.is_open());
    EXPECT_FALSE(cons.producer_alive());  // no producer yet

    const pid_t pid = ::fork();
    ASSERT_NE(pid, -1);
    if (pid == 0) {
        exch::ShmRing prod(name, exch::ShmRing::Role::Producer, false);
        if (!prod.is_open())
            _exit(2);
        const auto msg = make_order_new_event(0, 1);
        (void)prod.try_write(msg.data(), static_cast<uint32_t>(msg.size()));
        _exit(0);  // crash-exit without close()
    }
    // wait for the child to stamp its pid then die
    int status = 0;
    ASSERT_EQ(::waitpid(pid, &status, 0), pid);
    for (int i = 0; i < 500 && cons.producer_pid() == 0; i++)
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
    ASSERT_EQ(cons.producer_pid(), static_cast<uint64_t>(pid));
    EXPECT_FALSE(cons.producer_alive());   // dead pid -> false
    EXPECT_GT(cons.producer_heartbeat_ns(), 0u);

    uint32_t len = 0;
    const uint8_t* p = cons.peek(&len);
    ASSERT_NE(p, nullptr);  // its last message is still deliverable
    EXPECT_TRUE(check_order_new(p, len, 0));
    cons.consume();
    cons.close();
    unlink_ring(name);
}

TEST(ShmRing, RejectsBadGeometry) {
    exch::ShmRing r;
    EXPECT_FALSE(r.open("exch_bad_np2", exch::ShmRing::Role::Producer, true,
                        100 /*not pow2*/, 1024));
    EXPECT_FALSE(r.open("exch_bad_stride", exch::ShmRing::Role::Producer, true,
                        64, 100 /*payload not 64B multiple*/));
    unlink_ring("exch_bad_np2");
    unlink_ring("exch_bad_stride");
}

// -- SharedMemChannel --------------------------------------------------------

TEST(SharedMemChannel, RoundTripBothDirections) {
    const std::string base = uniq_base("chan");
    const uint16_t shard = 0;
    unlink_channel(base, shard);
    {
        exch::SharedMemChannel core(base, shard,
                                    exch::SharedMemChannel::Endpoint::Core,
                                    true);
        exch::SharedMemChannel gw(base, shard,
                                  exch::SharedMemChannel::Endpoint::Gateway,
                                  false);
        ASSERT_TRUE(core.open());
        ASSERT_TRUE(gw.open());

        // Gateway -> core: OrderNew over _in
        const auto order = make_order_new_event(7, 4242);
        ASSERT_TRUE(gw.send(order.data(), static_cast<uint32_t>(order.size())));

        uint32_t len = 0;
        const uint8_t* p = nullptr;
        for (int i = 0; i < 10000 && p == nullptr; i++)
            p = core.poll_view(&len);
        ASSERT_NE(p, nullptr);
        EXPECT_TRUE(check_order_new(p, len, 7));
        core.consume();

        // Core -> gateway: TradeFill echo over _out (zero-copy write path is
        // send(); FlatBuffers event is produced inline then handed off).
        const auto fill = make_trade_fill_event(7, 4242);
        ASSERT_TRUE(core.send(fill.data(), static_cast<uint32_t>(fill.size())));

        p = nullptr;
        for (int i = 0; i < 10000 && p == nullptr; i++)
            p = gw.poll_view(&len);
        ASSERT_NE(p, nullptr);
        const auto* ev = exc::wire::GetEvent(p);
        ASSERT_EQ(ev->type_type(), exc::wire::EventType_TradeFill);
        const auto* tf = ev->type_as_TradeFill();
        ASSERT_NE(tf, nullptr);
        EXPECT_EQ(tf->trade_id(), 9007u);
        EXPECT_EQ(tf->buy_order_id(), 4242u);
        gw.consume();

        EXPECT_EQ(core.occupancy(), 0u);
        EXPECT_EQ(gw.drops(), 0u);
    }
    unlink_channel(base, shard);
}

// -- AeronChannel --------------------------------------------------------------

TEST(AeronChannel, StubFailsClosedWithoutDriver) {
    // With no configured driver dir and no driver on any candidate path,
    // open() must fail closed rather than crash.
    std::string dir;
    if (aeron_driver_up(&dir))
        GTEST_SKIP() << "driver present; covered by AeronChannel.Loopback";
    exch::AeronChannelConfig cfg;
    cfg.aeron_dir = "/nonexistent/aeron/dir";
    cfg.connect_timeout_ms = 200;
    exch::AeronChannel ch(cfg);
    EXPECT_FALSE(ch.open());
    EXPECT_FALSE(ch.is_open());
    EXPECT_FALSE(ch.send("x", 1));
    EXPECT_EQ(ch.poll(nullptr, 0), 0);
}

TEST(AeronChannel, LoopbackEcho) {
    std::string dir;
    if (!aeron_driver_up(&dir))
        GTEST_SKIP() << "no media driver (set EXCH_IPC_AERON_DIR / start aeronmd)";

    exch::AeronChannelConfig core_cfg;
    core_cfg.aeron_dir = dir;
    core_cfg.in_uri = "aeron:ipc?alias=orders_in";    // core subscribes inbound
    core_cfg.in_stream_id = 1001;
    core_cfg.out_uri = "aeron:ipc?alias=orders_out";  // core publishes outbound
    core_cfg.out_stream_id = 1002;
    exch::AeronChannel core(core_cfg);
    ASSERT_TRUE(core.open());

    exch::AeronChannelConfig gw_cfg;
    gw_cfg.aeron_dir = dir;
    gw_cfg.in_uri = "aeron:ipc?alias=orders_out";   // gateway reads core out
    gw_cfg.in_stream_id = 1002;
    gw_cfg.out_uri = "aeron:ipc?alias=orders_in";   // gateway writes core in
    gw_cfg.out_stream_id = 1001;
    exch::AeronChannel gw(gw_cfg);
    ASSERT_TRUE(gw.open());

    // Wait for the IPC images to cross-connect.
    for (int i = 0;
         i < 2000 && !(core.subscriber_connected() && gw.subscriber_connected());
         i++) {
        core.poll_each([](void*, const uint8_t*, uint32_t) {}, nullptr, 0);
        gw.poll_each([](void*, const uint8_t*, uint32_t) {}, nullptr, 0);
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
    ASSERT_TRUE(core.subscriber_connected());
    ASSERT_TRUE(gw.subscriber_connected());

    // Gateway -> core: OrderNew; core -> gateway: TradeFill echo.
    const auto order = make_order_new_event(11, 777);
    bool sent = false;
    for (int i = 0; i < 1000 && !sent; i++)
        sent = gw.send(order.data(), static_cast<uint32_t>(order.size()));
    ASSERT_TRUE(sent);

    bool got_order = false;
    struct Ctx { bool* flag; } ctx { &got_order };
    const int64_t deadline = exch::ShmRing::mono_ns() + 5'000'000'000LL;
    while (!got_order && exch::ShmRing::mono_ns() < deadline) {
        core.poll_each(
            [](void* c, const uint8_t* data, uint32_t len) {
                auto* cc = static_cast<Ctx*>(c);
                *cc->flag = check_order_new(data, len, 11);
            },
            &ctx, 4);
    }
    ASSERT_TRUE(got_order);

    const auto fill = make_trade_fill_event(11, 777);
    sent = false;
    for (int i = 0; i < 1000 && !sent; i++)
        sent = core.send(fill.data(), static_cast<uint32_t>(fill.size()));
    ASSERT_TRUE(sent);

    bool got_fill = false;
    Ctx fctx { &got_fill };
    const int64_t d2 = exch::ShmRing::mono_ns() + 5'000'000'000LL;
    while (!got_fill && exch::ShmRing::mono_ns() < d2) {
        gw.poll_each(
            [](void* c, const uint8_t* data, uint32_t) {
                const auto* ev = exc::wire::GetEvent(data);
                auto* cc = static_cast<Ctx*>(c);
                *cc->flag =
                    ev->type_type() == exc::wire::EventType_TradeFill &&
                    ev->type_as_TradeFill()->buy_order_id() == 777;
            },
            &fctx, 4);
    }
    EXPECT_TRUE(got_fill);
}

}  // namespace
