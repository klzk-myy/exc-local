// Tasks 2.3.7 + 2.3.19 — EnginePump / EngineLoop / Watchdog coverage:
// FlatBuffers Event decode -> IEngineIngress dispatch, backpressure accounting
// (ENGINE_OVERLOAD), poison-pill quarantine, ring watermarks (shed/halt),
// TIME_TICK WAL stamping, loop beat/metrics, watchdog staleness edges, and a
// SharedMemChannel loopback round-trip.

#include <gtest/gtest.h>
#include <unistd.h>

#include <atomic>
#include <cstdint>
#include <cstring>
#include <deque>
#include <filesystem>
#include <string>
#include <utility>
#include <vector>

#include "exchange_generated.h"
#include "ipc/EnginePump.hpp"
#include "ipc/SharedMemChannel.hpp"
#include "ipc/ShmRing.hpp"
#include "matching/EngineLoop.hpp"
#include "utils/MemoryPool.hpp"
#include "utils/TimeUtils.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

namespace {

// -- fakes / helpers -----------------------------------------------------------

class FakeChannel final : public exch::IpcChannel {
   public:
    bool open() noexcept override {
        open_ = true;
        return true;
    }
    void close() noexcept override { open_ = false; }
    [[nodiscard]] bool is_open() const noexcept override { return open_; }

    bool send(const void* data, uint32_t len) noexcept override {
        if (!send_result) return false;
        sent.emplace_back(static_cast<const uint8_t*>(data),
                          static_cast<const uint8_t*>(data) + len);
        return true;
    }
    int32_t poll(void* buf, uint32_t buf_cap) noexcept override {
        if (inq.empty()) return 0;
        const std::vector<uint8_t>& m = inq.front();
        if (m.size() > buf_cap) return -1;
        std::memcpy(buf, m.data(), m.size());
        const int32_t n = static_cast<int32_t>(m.size());
        inq.pop_front();
        return n;
    }

    bool open_ = false;
    bool send_result = true;
    std::deque<std::vector<uint8_t>> inq;
    std::vector<std::vector<uint8_t>> sent;
};

class FakeIngress final : public exch::IEngineIngress {
   public:
    void on_order_received(exch::Order* o) noexcept override {
        orders.push_back(*o);  // value copy — pool slot may be reused
    }
    void on_cancel_received(uint64_t order_id, uint64_t account_id) noexcept override {
        cancels.emplace_back(order_id, account_id);
    }
    void on_time_tick(uint64_t now_ns) noexcept override { ticks.push_back(now_ns); }

    std::vector<exch::Order> orders;
    std::vector<std::pair<uint64_t, uint64_t>> cancels;
    std::vector<uint64_t> ticks;
};

struct ReportLog {
    std::vector<std::pair<std::string, std::string>> entries;
    static void sink(void* ctx, const char* code, const char* detail) {
        auto* self = static_cast<ReportLog*>(ctx);
        self->entries.emplace_back(code != nullptr ? code : "", detail != nullptr ? detail : "");
    }
    [[nodiscard]] bool has(const char* code) const {
        for (const auto& e : entries)
            if (e.first == code) return true;
        return false;
    }
};

struct PoisonLog {
    std::vector<std::pair<std::vector<uint8_t>, std::string>> entries;
    static void sink(void* ctx, const uint8_t* data, uint32_t len, const char* why) {
        auto* self = static_cast<PoisonLog*>(ctx);
        self->entries.emplace_back(std::vector<uint8_t>(data, data + len),
                                   why != nullptr ? why : "");
    }
};

std::vector<uint8_t> make_order_new(uint64_t seq, uint64_t order_id, uint64_t account_id,
                                    int64_t qty, int64_t price, uint64_t ts) {
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

std::vector<uint8_t> make_order_cancel(uint64_t seq, uint64_t order_id, uint64_t account_id,
                                       uint64_t ts) {
    flatbuffers::FlatBufferBuilder b(128);
    const auto oc = exc::wire::CreateOrderCancel(b, order_id, account_id);
    exc::wire::EventBuilder eb(b);
    eb.add_seq(seq);
    eb.add_ts(ts);
    eb.add_type_type(exc::wire::EventType_OrderCancel);
    eb.add_type(oc.Union());
    b.Finish(eb.Finish());
    return {b.GetBufferPointer(), b.GetBufferPointer() + b.GetSize()};
}

std::vector<uint8_t> make_fill(uint64_t seq, uint64_t trade_id) {
    flatbuffers::FlatBufferBuilder b(128);
    const auto tf = exc::wire::CreateTradeFill(b, trade_id, 1, 2, 100, 200, seq);
    exc::wire::EventBuilder eb(b);
    eb.add_seq(seq);
    eb.add_ts(1);
    eb.add_type_type(exc::wire::EventType_TradeFill);
    eb.add_type(tf.Union());
    b.Finish(eb.Finish());
    return {b.GetBufferPointer(), b.GetBufferPointer() + b.GetSize()};
}

std::string uniq_base(const char* tag) {
    char buf[128];
    std::snprintf(buf, sizeof(buf), "exch_pumptest_%s_%d", tag, static_cast<int>(::getpid()));
    return {buf};
}

void unlink_channel(const std::string& base, uint16_t shard) {
    ::shm_unlink(("/" + exch::SharedMemChannel::in_name(base, shard)).c_str());
    ::shm_unlink(("/" + exch::SharedMemChannel::out_name(base, shard)).c_str());
}

std::filesystem::path tmp_dir(const char* name) {
    const auto d = std::filesystem::temp_directory_path() /
                   ("exch_pump_" + std::to_string(::getpid()) + "_" + name);
    std::filesystem::remove_all(d);
    std::filesystem::create_directories(d);
    return d;
}

// -- EnginePump dispatch --------------------------------------------------------

TEST(EnginePump, OrderNewDecodesAndDispatches) {
    FakeChannel in, out;
    FakeIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    (void)in.open();
    const auto msg = make_order_new(/*seq=*/42, /*order_id=*/9001,
                                    /*account_id=*/77,
                                    /*qty=*/150'000'000, /*price=*/105'000'000,
                                    /*ts=*/7'777'000);
    in.inq.push_back(msg);
    exch::EnginePump pump(&in, &out, &eng, &pool);

    EXPECT_EQ(pump.run_once(16), 1u);
    ASSERT_EQ(eng.orders.size(), 1u);
    const exch::Order& o = eng.orders[0];
    EXPECT_EQ(o.id, 9001u);
    EXPECT_EQ(o.account_id, 77u);
    EXPECT_EQ(o.side, exch::Side::SELL);
    EXPECT_EQ(o.type, exch::OrderType::LIMIT);
    EXPECT_EQ(o.tif, exch::TimeInForce::GTD);
    EXPECT_EQ(o.price_ticks, 105'000'000);
    EXPECT_EQ(o.qty_units, 150'000'000);
    EXPECT_EQ(o.filled_qty_units, 0);
    EXPECT_EQ(o.timestamp_ns, 7'777'000u);          // Event.ts -> fairness stamp
    EXPECT_EQ(o.ingress_seq, 42u);                  // Event.seq -> FIFO tie-break
    EXPECT_EQ(o.quantity.mantissa(), 150'000'000);  // compat mirror
    EXPECT_EQ(pump.msgs_in(), 1u);
    EXPECT_EQ(pump.msgs_dispatched(), 1u);
    EXPECT_EQ(pump.dispatch_latency().total(), 1u);
}

TEST(EnginePump, OrderCancelDecodesAndDispatches) {
    FakeChannel in, out;
    FakeIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    (void)in.open();
    in.inq.push_back(make_order_cancel(/*seq=*/9, /*order_id=*/5555,
                                       /*account_id=*/66, /*ts=*/123));
    exch::EnginePump pump(&in, &out, &eng, &pool);

    EXPECT_EQ(pump.run_once(4), 1u);
    ASSERT_EQ(eng.cancels.size(), 1u);
    EXPECT_EQ(eng.cancels[0].first, 5555u);
    EXPECT_EQ(eng.cancels[0].second, 66u);
    EXPECT_EQ(pool.size(), 0u);  // cancels never allocate
}

TEST(EnginePump, BatchDrainsAndStopsAtEmpty) {
    FakeChannel in, out;
    FakeIngress eng;
    exch::MemoryPool<exch::Order> pool(64);
    (void)in.open();
    for (uint64_t i = 0; i < 10; ++i)
        in.inq.push_back(make_order_new(i, 1000 + i, 5, 1'000'000, 99'000'000, i));
    exch::EnginePump pump(&in, &out, &eng, &pool);

    EXPECT_EQ(pump.run_once(4), 4u);  // bounded by max_batch
    EXPECT_EQ(eng.orders.size(), 4u);
    EXPECT_EQ(pump.run_once(100), 6u);  // drains the rest
    EXPECT_EQ(eng.orders.size(), 10u);
    EXPECT_EQ(pump.run_once(4), 0u);  // drained channel -> 0, no error
    EXPECT_EQ(pump.msgs_in(), 10u);
}

TEST(EnginePump, PoisonPillQuarantinedNotCrashing) {
    FakeChannel in, out;
    FakeIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    (void)in.open();
    in.inq.push_back(std::vector<uint8_t>{0xFF, 0x00, 0x13, 0x37});  // garbage
    in.inq.push_back(make_order_new(1, 1, 1, 1'000'000, 1'000'000, 1));
    exch::EnginePump pump(&in, &out, &eng, &pool);
    PoisonLog plog;
    pump.set_poison_sink(&PoisonLog::sink, &plog);
    // Silence the default stderr reporters — the sink captures everything.
    ReportLog rlog;
    pump.set_report_sink(&ReportLog::sink, &rlog);

    EXPECT_EQ(pump.run_once(8), 2u);
    EXPECT_EQ(pump.poison_pills(), 1u);
    ASSERT_EQ(plog.entries.size(), 1u);
    EXPECT_EQ(plog.entries[0].first.size(), 4u);
    // The good frame behind the pill still dispatched — no loop crash.
    ASSERT_EQ(eng.orders.size(), 1u);
    EXPECT_EQ(eng.orders[0].id, 1u);
}

TEST(EnginePump, BadFieldValuesFailClosed) {
    FakeChannel in, out;
    FakeIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    (void)in.open();
    // qty <= 0 and limit price <= 0 are rejected before touching the pool.
    in.inq.push_back(make_order_new(1, 1, 1, 0, 1'000'000, 1));
    in.inq.push_back(make_order_new(2, 2, 1, 1'000'000, 0, 2));
    exch::EnginePump pump(&in, &out, &eng, &pool);
    ReportLog rlog;
    pump.set_report_sink(&ReportLog::sink, &rlog);

    EXPECT_EQ(pump.run_once(8), 2u);
    EXPECT_EQ(pump.decode_errors(), 2u);
    EXPECT_TRUE(eng.orders.empty());
    EXPECT_EQ(pool.size(), 0u);
    EXPECT_TRUE(rlog.has("DECODE_ERROR"));
}

TEST(EnginePump, PoolExhaustionIsAtomicReject) {
    FakeChannel in, out;
    FakeIngress eng;
    exch::MemoryPool<exch::Order> pool(1);
    (void)in.open();
    in.inq.push_back(make_order_new(1, 1, 1, 1'000'000, 1'000'000, 1));
    in.inq.push_back(make_order_new(2, 2, 1, 1'000'000, 1'000'000, 2));
    exch::EnginePump pump(&in, &out, &eng, &pool);
    ReportLog rlog;
    pump.set_report_sink(&ReportLog::sink, &rlog);

    EXPECT_EQ(pump.run_once(8), 2u);
    ASSERT_EQ(eng.orders.size(), 1u);
    EXPECT_EQ(pump.order_alloc_failures(), 1u);
    EXPECT_TRUE(rlog.has("ORDER_BOOK_CAPACITY_EXCEEDED"));
}

TEST(EnginePump, OutboundBackpressureCountsOverload) {
    FakeChannel in, out;
    FakeIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    exch::EnginePump::Options opts;
    opts.overload_report_every = 4;
    exch::EnginePump pump(&in, &out, &eng, &pool, nullptr, opts);
    ReportLog rlog;
    pump.set_report_sink(&ReportLog::sink, &rlog);
    (void)out.open();
    out.send_result = false;  // ring full — send() declines

    const char* msg = "x";
    for (int i = 0; i < 10; ++i) EXPECT_FALSE(pump.send_outbound(msg, 1));
    EXPECT_EQ(pump.overload_drops(), 10u);
    // First drop reports immediately, then once per 4: drops 1,4,8 -> 3 reports.
    EXPECT_EQ(pump.overload_reports(), 3u);
    EXPECT_TRUE(rlog.has("ENGINE_OVERLOAD"));

    out.send_result = true;
    EXPECT_TRUE(pump.send_outbound(msg, 1));
    EXPECT_EQ(pump.msgs_out(), 1u);
}

TEST(EnginePump, NullEngineDropsCountedNotCrashing) {
    FakeChannel in, out;
    exch::MemoryPool<exch::Order> pool(8);
    (void)in.open();
    in.inq.push_back(make_order_new(1, 1, 1, 1'000'000, 1'000'000, 1));
    in.inq.push_back(make_order_cancel(2, 1, 1, 2));
    exch::EnginePump pump(&in, &out, /*engine=*/nullptr, &pool);
    ReportLog rlog;
    pump.set_report_sink(&ReportLog::sink, &rlog);

    EXPECT_EQ(pump.run_once(8), 2u);
    EXPECT_EQ(pump.unrouted_drops(), 2u);
    EXPECT_TRUE(rlog.has("ENGINE_UNWIRED"));
    pump.tick(1);  // must not crash with null engine
    EXPECT_EQ(pump.ticks(), 1u);
}

TEST(EnginePump, TickStampsWalTimeTick) {
    const auto dir = tmp_dir("tickwal");
    const std::string p = (dir / "0.wal").string();
    exch::Wal wal(p, 0);
    ASSERT_EQ(wal.open(), exch::WalStatus::Ok);

    FakeChannel in, out;
    FakeIngress eng;
    exch::MemoryPool<exch::Order> pool(8);
    exch::EnginePump::Options opts;
    opts.wal_log_ticks = true;  // pump-side stamp (engine stub journals none)
    exch::EnginePump pump(&in, &out, &eng, &pool, &wal, opts);

    pump.tick(1'234'567'890);
    EXPECT_EQ(eng.ticks.size(), 1u);
    EXPECT_EQ(eng.ticks[0], 1'234'567'890u);
    ASSERT_EQ(wal.tail_seq(), 1u);
    (void)wal.flush();

    exch::WalReader r;
    ASSERT_EQ(r.open(p), exch::WalStatus::Ok);
    exch::WalEntryView v;
    ASSERT_EQ(r.next(v), exch::WalScanStep::Entry);
    EXPECT_EQ(v.type, exch::WalEventType::TIME_TICK);
    ASSERT_EQ(v.payload_len, sizeof(exch::WalTimeTickPayload));
    exch::WalTimeTickPayload tp{};
    std::memcpy(&tp, v.payload, sizeof(tp));
    EXPECT_EQ(tp.tick_ns, 1'234'567'890u);
    r.close();
    wal.close();
    std::filesystem::remove_all(dir);
}

// -- EngineLoop -----------------------------------------------------------------

TEST(EngineLoop, SpinDrivesBeatTickAndDispatch) {
    FakeChannel in, out;
    FakeIngress eng;
    exch::MemoryPool<exch::Order> pool(16);
    (void)in.open();
    for (uint64_t i = 0; i < 3; ++i)
        in.inq.push_back(make_order_new(i, 10 + i, 2, 1'000'000, 1'000'000, i));
    exch::EnginePump pump(&in, &out, &eng, &pool);

    exch::EngineLoopConfig cfg;
    cfg.max_batch = 8;
    cfg.tick_interval_ns = 0;  // tick every cycle
    exch::EngineLoop loop(&pump, cfg);
    ReportLog rlog;
    loop.set_report_sink(&ReportLog::sink, &rlog);

    EXPECT_EQ(loop.spin_once(), 3u);
    EXPECT_EQ(eng.orders.size(), 3u);
    EXPECT_EQ(eng.ticks.size(), 1u);
    EXPECT_EQ(loop.loop_beat().load(), 1u);
    EXPECT_GT(loop.last_beat_mono_ns().load(), 0);
    EXPECT_EQ(loop.stats().loop_iters.load(), 1u);
    EXPECT_EQ(loop.stats().cycle_hist.total(), 1u);

    EXPECT_EQ(loop.spin_once(), 0u);  // drained; tick still driven per cycle
    EXPECT_EQ(eng.ticks.size(), 2u);
    EXPECT_EQ(loop.loop_beat().load(), 2u);
}

TEST(EngineLoop, WatermarkShedsNewOrdersButNotCancels) {
    const std::string base = uniq_base("shed");
    const uint16_t shard = 3;
    unlink_channel(base, shard);
    {
        // 64-slot inbound ring: >80% = 52+, >95% = 61+.
        exch::SharedMemChannel core(base, shard, exch::SharedMemChannel::Endpoint::Core, true, 64,
                                    exch::ShmRing::kDefaultSlotPayload);
        exch::SharedMemChannel gw(base, shard, exch::SharedMemChannel::Endpoint::Gateway, false);
        ASSERT_TRUE(core.open());
        ASSERT_TRUE(gw.open());

        FakeIngress eng;
        exch::MemoryPool<exch::Order> pool(128);
        exch::EnginePump pump(&core, &core, &eng, &pool);
        ReportLog pump_log, loop_log;
        pump.set_report_sink(&ReportLog::sink, &pump_log);

        exch::EngineLoopConfig cfg;
        cfg.max_batch = 256;
        exch::EngineLoop loop(&pump, cfg);
        loop.set_report_sink(&ReportLog::sink, &loop_log);

        // Fill to 53/64 = 82.8% — above the shed watermark. Mix in a cancel.
        for (uint64_t i = 0; i < 52; ++i) {
            const auto m = make_order_new(i, 2000 + i, 9, 1'000'000, 1'000'000, i);
            ASSERT_TRUE(gw.send(m.data(), static_cast<uint32_t>(m.size())));
        }
        const auto cx = make_order_cancel(999, 2000, 9, 999);
        ASSERT_TRUE(gw.send(cx.data(), static_cast<uint32_t>(cx.size())));
        ASSERT_EQ(core.occupancy(), 53u);

        EXPECT_EQ(loop.spin_once(), 53u);  // all consumed
        EXPECT_TRUE(loop.shedding());
        EXPECT_EQ(pump.shed_drops(), 52u);  // new orders shed
        EXPECT_TRUE(eng.orders.empty());
        EXPECT_EQ(eng.cancels.size(), 1u);  // cancels never shed
        EXPECT_EQ(core.occupancy(), 0u);
        EXPECT_EQ(loop.stats().shed_cycles.load(), 1u);
    }
    unlink_channel(base, shard);
}

TEST(EngineLoop, WatermarkHaltLeavesFramesQueued) {
    const std::string base = uniq_base("halt");
    const uint16_t shard = 4;
    unlink_channel(base, shard);
    {
        exch::SharedMemChannel core(base, shard, exch::SharedMemChannel::Endpoint::Core, true, 64,
                                    exch::ShmRing::kDefaultSlotPayload);
        exch::SharedMemChannel gw(base, shard, exch::SharedMemChannel::Endpoint::Gateway, false);
        ASSERT_TRUE(core.open());
        ASSERT_TRUE(gw.open());

        FakeIngress eng;
        exch::MemoryPool<exch::Order> pool(128);
        exch::EnginePump pump(&core, &core, &eng, &pool);
        ReportLog pump_log;
        pump.set_report_sink(&ReportLog::sink, &pump_log);
        ReportLog loop_log;
        exch::EngineLoop loop(&pump, {});
        loop.set_report_sink(&ReportLog::sink, &loop_log);

        // 62/64 = 96.9% — above the halt watermark.
        for (uint64_t i = 0; i < 62; ++i) {
            const auto m = make_order_new(i, 3000 + i, 9, 1'000'000, 1'000'000, i);
            ASSERT_TRUE(gw.send(m.data(), static_cast<uint32_t>(m.size())));
        }
        ASSERT_EQ(core.occupancy(), 62u);

        EXPECT_EQ(loop.spin_once(), 0u);  // ingress halted: nothing consumed
        EXPECT_TRUE(loop.ingress_halted());
        EXPECT_EQ(core.occupancy(), 62u);  // frames still queued for retry
        EXPECT_TRUE(eng.orders.empty());
        EXPECT_EQ(loop.stats().critical_bp_events.load(), 1u);
        EXPECT_TRUE(loop_log.has("CRITICAL_BACKPRESSURE"));
        EXPECT_EQ(loop.loop_beat().load(), 1u);  // loop still beats under halt

        // The two free slots still accept writes — halted ingress means the
        // engine stops consuming, so the producer converges on a genuinely
        // full ring and THEN sees sequenced transport backpressure.
        for (uint64_t i = 62; i < 64; ++i) {
            const auto m = make_order_new(i, 4000 + i, 9, 1'000'000, 1'000'000, i);
            ASSERT_TRUE(gw.send(m.data(), static_cast<uint32_t>(m.size())));
        }
        const auto extra = make_order_new(64, 5000, 9, 1'000'000, 1'000'000, 64);
        EXPECT_FALSE(gw.send(extra.data(), static_cast<uint32_t>(extra.size())));
        EXPECT_GE(gw.drops(), 1u);  // sequenced drop accounting at the ring

        // Second halted cycle: still nothing consumed.
        EXPECT_EQ(loop.spin_once(), 0u);
        EXPECT_EQ(core.occupancy(), 64u);
        EXPECT_TRUE(eng.orders.empty());
    }
    unlink_channel(base, shard);
}

// -- Watchdog --------------------------------------------------------------------

TEST(Watchdog, FreshBeatIsOkAndStaleBeatsEscalate) {
    std::atomic<uint64_t> beat{0};
    std::atomic<int64_t> last{0};
    exch::Watchdog::Thresholds t;
    t.warn_ns = 100;
    t.stall_ns = 1'000;

    std::vector<std::pair<exch::Watchdog::Level, int64_t>> events;
    exch::Watchdog wd(
        &beat, &last, t,
        [](void* ctx, exch::Watchdog::Level lvl, int64_t stale, uint64_t /*b*/) noexcept {
            auto* v = static_cast<decltype(&events)>(ctx);
            v->emplace_back(lvl, stale);
        },
        &events);

    // No beat yet — startup is never a false stall.
    EXPECT_EQ(wd.check_once(10'000'000), exch::Watchdog::Level::Ok);

    last.store(1'000'000, std::memory_order_release);
    beat.store(1, std::memory_order_release);

    // Fresh: staleness inside warn window.
    EXPECT_EQ(wd.check_once(1'000'050), exch::Watchdog::Level::Ok);
    EXPECT_TRUE(events.empty());

    // Warn edge fires once.
    EXPECT_EQ(wd.check_once(1'000'500), exch::Watchdog::Level::Warn);
    EXPECT_EQ(wd.check_once(1'000'600), exch::Watchdog::Level::Warn);  // held
    ASSERT_EQ(events.size(), 1u);
    EXPECT_EQ(events[0].first, exch::Watchdog::Level::Warn);
    EXPECT_EQ(events[0].second, 500);
    EXPECT_EQ(wd.warn_samples(), 2u);

    // Stall edge fires on transition.
    EXPECT_EQ(wd.check_once(1'002'500), exch::Watchdog::Level::Stall);
    ASSERT_EQ(events.size(), 2u);
    EXPECT_EQ(events[1].first, exch::Watchdog::Level::Stall);
    EXPECT_EQ(wd.stall_reports(), 1u);

    // Fresh beat re-arms; a new stall episode reports again.
    last.store(5'000'000, std::memory_order_release);
    beat.store(2, std::memory_order_release);
    EXPECT_EQ(wd.check_once(5'000'010), exch::Watchdog::Level::Ok);
    EXPECT_EQ(wd.check_once(5'002'000), exch::Watchdog::Level::Stall);
    ASSERT_EQ(events.size(), 3u);
    EXPECT_EQ(events[2].first, exch::Watchdog::Level::Stall);
    EXPECT_EQ(wd.stall_reports(), 2u);
}

TEST(Watchdog, ThreadedSamplerObservesLiveLoop) {
    std::atomic<uint64_t> beat{0};
    std::atomic<int64_t> last{0};
    exch::Watchdog::Thresholds t;
    t.sample_ns = 50'000;     // 50µs
    t.warn_ns = 200'000'000;  // generous — scheduler jitter on CI
    t.stall_ns = 5'000'000'000;
    std::atomic<int> stalls{0};
    exch::Watchdog wd(
        &beat, &last, t,
        [](void* ctx, exch::Watchdog::Level lvl, int64_t, uint64_t) noexcept {
            if (lvl == exch::Watchdog::Level::Stall) ++(*static_cast<std::atomic<int>*>(ctx));
        },
        &stalls);
    ASSERT_TRUE(wd.start());
    const int64_t deadline = static_cast<int64_t>(exch::steady_ns()) + 200'000'000;
    while (exch::steady_ns() < static_cast<uint64_t>(deadline)) {
        beat.fetch_add(1, std::memory_order_release);
        last.store(static_cast<int64_t>(exch::steady_ns()), std::memory_order_release);
    }
    wd.stop();
    EXPECT_EQ(stalls.load(), 0);
}

// -- SharedMemChannel round trip ---------------------------------------------------

TEST(EnginePump, ShmLoopbackOrderInFillOut) {
    const std::string base = uniq_base("loop");
    const uint16_t shard = 7;
    unlink_channel(base, shard);
    {
        exch::SharedMemChannel core(base, shard, exch::SharedMemChannel::Endpoint::Core, true);
        exch::SharedMemChannel gw(base, shard, exch::SharedMemChannel::Endpoint::Gateway, false);
        ASSERT_TRUE(core.open());
        ASSERT_TRUE(gw.open());

        FakeIngress eng;
        exch::MemoryPool<exch::Order> pool(16);
        exch::EnginePump pump(&core, &core, &eng, &pool);
        ReportLog rlog;
        pump.set_report_sink(&ReportLog::sink, &rlog);

        // Gateway -> core over the real shm ring (zero-copy poll_view path).
        const auto m = make_order_new(77, 4242, 88, 2'000'000, 101'000'000, 55'000'000);
        ASSERT_TRUE(gw.send(m.data(), static_cast<uint32_t>(m.size())));
        EXPECT_EQ(pump.run_once(4), 1u);
        ASSERT_EQ(eng.orders.size(), 1u);
        EXPECT_EQ(eng.orders[0].id, 4242u);
        EXPECT_EQ(eng.orders[0].ingress_seq, 77u);
        EXPECT_EQ(eng.orders[0].timestamp_ns, 55'000'000u);

        // Core -> gateway: pump-mediated outbound send.
        const auto fill = make_fill(77, 9001);
        ASSERT_TRUE(pump.send_outbound(fill.data(), static_cast<uint32_t>(fill.size())));
        uint32_t len = 0;
        const uint8_t* p = nullptr;
        for (int i = 0; i < 10000 && p == nullptr; ++i) p = gw.poll_view(&len);
        ASSERT_NE(p, nullptr);
        const auto* ev = exc::wire::GetEvent(p);
        ASSERT_EQ(ev->type_type(), exc::wire::EventType_TradeFill);
        EXPECT_EQ(ev->type_as_TradeFill()->trade_id(), 9001u);
        gw.consume();
        EXPECT_EQ(pump.msgs_out(), 1u);
        EXPECT_EQ(pump.overload_drops(), 0u);
    }
    unlink_channel(base, shard);
}

}  // namespace
