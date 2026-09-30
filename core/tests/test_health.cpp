// Task 2.3.6 — HealthChecker tests: 5s-cadence probe evaluation, FSM
// auto-transitions by spec §2.4 triggers, dwell + cooldown hysteresis,
// auto-recovery, Maintenance manual-hold.

#include <gtest/gtest.h>

#include <atomic>
#include <chrono>
#include <cstdint>
#include <memory>
#include <string>
#include <thread>
#include <vector>

#include "degradation/ModeManager.hpp"
#include "health/HealthChecker.hpp"

namespace {

struct FakeClock {
    int64_t now = 1'000'000;
    static int64_t read(void* ctx) noexcept {
        return static_cast<FakeClock*>(ctx)->now;
    }
    void advance(int64_t ms) { now += ms; }
};

// Scriptable probe state — the checker reads it through fn-pointer probes.
struct Signals {
    int64_t matching_p50_us = 50;   // healthy engine p50
    int64_t redis_p50_us = 500;     // healthy coordination latency
    bool pg_ok = true;
    bool wal_ok = true;
    bool workers_ok = true;
    bool deriv_ok = true;
    int64_t depth = 0;

    static int64_t p50_match(void* ctx) noexcept {
        return static_cast<Signals*>(ctx)->matching_p50_us;
    }
    static int64_t p50_redis(void* ctx) noexcept {
        return static_cast<Signals*>(ctx)->redis_p50_us;
    }
    static bool pg(void* ctx) noexcept {
        return static_cast<Signals*>(ctx)->pg_ok;
    }
    static bool wal(void* ctx) noexcept {
        return static_cast<Signals*>(ctx)->wal_ok;
    }
    static bool workers(void* ctx) noexcept {
        return static_cast<Signals*>(ctx)->workers_ok;
    }
    static bool deriv(void* ctx) noexcept {
        return static_cast<Signals*>(ctx)->deriv_ok;
    }
    static int64_t queue(void* ctx) noexcept {
        return static_cast<Signals*>(ctx)->depth;
    }
};

struct Alerts {
    std::vector<std::pair<int, std::string>> rows;
    static void sink(void* ctx, int level, const char* code,
                     const char* detail) noexcept {
        (void)detail;
        static_cast<Alerts*>(ctx)->rows.emplace_back(level, code);
    }
};

struct Fixture {
    FakeClock clock;
    Signals sig;
    Alerts alerts;
    exch::ModeManager modes;       // local-only store — transitions apply
    exch::HealthChecker checker{modes};

    Fixture() {
        exch::HealthProbes p;
        p.matching_p50_us = &Signals::p50_match;
        p.matching_p50_ctx = &sig;
        p.redis_p50_us = &Signals::p50_redis;
        p.redis_p50_ctx = &sig;
        p.postgres_ok = &Signals::pg;
        p.postgres_ctx = &sig;
        p.wal_ok = &Signals::wal;
        p.wal_ctx = &sig;
        p.workers_ok = &Signals::workers;
        p.workers_ctx = &sig;
        p.derivatives_ok = &Signals::deriv;
        p.derivatives_ctx = &sig;
        p.queue_depth = &Signals::queue;
        p.queue_depth_ctx = &sig;
        checker.set_probes(p);
        checker.set_clock(&FakeClock::read, &clock);
        checker.set_alert_sink(&Alerts::sink, &alerts);
        modes.set_clock(&FakeClock::read, &clock);
    }

    // Run one 5s job: evaluate, then advance time to the next slot.
    exch::HealthReport eval(int64_t step_ms = 5'000) {
        exch::HealthReport r = checker.check_once();
        clock.advance(step_ms);
        return r;
    }
};

using exch::DegradationMode;

TEST(HealthCheckerUnit, AllHealthyStaysNormal) {
    Fixture f;
    const auto r = f.eval();
    EXPECT_EQ(r.target_mode, DegradationMode::Normal);
    EXPECT_FALSE(r.transitioned);
    EXPECT_EQ(f.modes.mode(), DegradationMode::Normal);
    EXPECT_TRUE(r.matching_loop_ok && r.redis_ok && r.postgres_ok &&
                r.wal_ok && r.workers_ok);
}

TEST(HealthCheckerUnit, MatchingLoopP50DrivesCoreSlowReadOnly) {
    Fixture f;
    f.sig.matching_p50_us = 600;  // > 500µs — spec §2.4 "core slow" trigger
    const auto r = f.eval();
    EXPECT_EQ(r.target_mode, DegradationMode::ReadOnly);
    EXPECT_TRUE(r.transitioned);
    EXPECT_EQ(f.modes.mode(), DegradationMode::ReadOnly);
    EXPECT_EQ(f.modes.reason(), "matching_p50_slow");
    // P2 alert per spec §2.4 (ReadOnly/Throttled).
    ASSERT_FALSE(f.alerts.rows.empty());
    EXPECT_EQ(f.alerts.rows.back().first, 2);
}

TEST(HealthCheckerUnit, RedisSlowAndShardUnhealthyFeedReadOnly) {
    {
        Fixture f;
        f.sig.redis_p50_us = 2'000'000;  // §2.7.3: Redis timeout >1s
        const auto r = f.eval();
        EXPECT_EQ(r.target_mode, DegradationMode::ReadOnly);
        EXPECT_EQ(std::string(r.reason), "redis_slow");
    }
    {
        Fixture f;
        f.sig.workers_ok = false;  // "1+ shard unhealthy" (spec §2.4)
        const auto r = f.eval();
        EXPECT_EQ(r.target_mode, DegradationMode::ReadOnly);
        EXPECT_EQ(std::string(r.reason), "shard_unhealthy");
    }
}

TEST(HealthCheckerUnit, PostgresOrWalFailureTriggersMarketDataOnly) {
    {
        Fixture f;
        f.sig.pg_ok = false;
        const auto r = f.eval();
        EXPECT_EQ(r.target_mode, DegradationMode::MarketDataOnly);
        EXPECT_EQ(f.modes.mode(), DegradationMode::MarketDataOnly);
        EXPECT_EQ(std::string(r.reason), "postgres_down");
        ASSERT_FALSE(f.alerts.rows.empty());
        EXPECT_EQ(f.alerts.rows.back().first, 1);  // P1
    }
    {
        Fixture f;
        f.sig.wal_ok = false;
        const auto r = f.eval();
        EXPECT_EQ(r.target_mode, DegradationMode::MarketDataOnly);
        EXPECT_EQ(std::string(r.reason), "wal_corrupt");
    }
}

TEST(HealthCheckerUnit, DerivativesFailureTriggersSpotOnly) {
    Fixture f;
    f.sig.deriv_ok = false;
    const auto r = f.eval();
    EXPECT_EQ(r.target_mode, DegradationMode::SpotOnly);
    EXPECT_EQ(std::string(r.reason), "derivatives_unhealthy");
}

TEST(HealthCheckerUnit, SimultaneousTriggersResolveByPriority) {
    Fixture f;
    // Everything broken at once: highest severity (MarketDataOnly) wins.
    f.sig.matching_p50_us = 900;
    f.sig.pg_ok = false;
    f.sig.deriv_ok = false;
    f.sig.depth = 500;
    const auto r = f.eval();
    EXPECT_EQ(r.target_mode, DegradationMode::MarketDataOnly);
    EXPECT_EQ(f.modes.mode(), DegradationMode::MarketDataOnly);
}

TEST(HealthCheckerUnit, ThrottleRequiresSustainedDepth) {
    Fixture f;
    f.sig.depth = 300;  // > 250 = capacity > 80%
    auto r = f.eval();
    EXPECT_EQ(r.target_mode, DegradationMode::Normal);  // not yet sustained
    EXPECT_FALSE(r.throttle_triggered);

    for (int i = 0; i < 5; ++i) f.eval();  // ~30s total over the mark
    r = f.eval();
    EXPECT_TRUE(r.throttle_triggered);
    EXPECT_EQ(f.modes.mode(), DegradationMode::Throttled);
    EXPECT_EQ(std::string(r.reason), "capacity_over_80pct");
}

TEST(HealthCheckerUnit, AutoRecoveryStepsDownAfterDwellAndCooldown) {
    Fixture f;
    f.sig.matching_p50_us = 900;
    auto r = f.eval();  // t=0: → ReadOnly
    ASSERT_TRUE(r.transitioned);
    ASSERT_EQ(f.modes.mode(), DegradationMode::ReadOnly);

    // Engine recovers (p50 < 100µs sustained) — recovery dwell is 30s AND
    // the 60s cooldown both gate de-escalation.
    f.sig.matching_p50_us = 40;
    for (int i = 0; i < 5; ++i) {  // ~25s of healthy telemetry
        r = f.eval();
        EXPECT_EQ(f.modes.mode(), DegradationMode::ReadOnly);
        EXPECT_TRUE(r.recovery_pending || !r.transitioned);
    }
    // t≈30s: dwell met, cooldown (60s) still blocks.
    r = f.eval();
    EXPECT_EQ(f.modes.mode(), DegradationMode::ReadOnly);
    EXPECT_TRUE(r.recovery_pending);

    while (f.clock.now < 1'000'000 + 61'000) {
        r = f.eval();
    }
    // t≈65s: cooldown elapsed + dwell satisfied → de-escalates to Normal.
    EXPECT_EQ(f.modes.mode(), DegradationMode::Normal);
}

TEST(HealthCheckerUnit, CooldownPreventsFlapping) {
    Fixture f;
    // Alternate slow/healthy fast enough to flap without the cooldown, and
    // record every applied DE-escalation (escalation bypasses cooldown by
    // design — a worse state is never delayed, spec §2.7).
    std::vector<int64_t> deesc_times;
    DegradationMode prev = DegradationMode::Normal;
    for (int i = 0; i < 60; ++i) {
        // 14 healthy evals then one slow eval per 15-cycle — long enough to
        // satisfy the 30s recovery dwell each round, so de-escalation is
        // genuinely attempted every cycle and only the cooldown throttles it.
        f.sig.matching_p50_us = (i % 15 == 14) ? 900 : 40;
        const auto r = f.eval();
        if (r.transitioned &&
            exch::severity(r.target_mode) < exch::severity(prev)) {
            deesc_times.push_back(f.clock.now);
        }
        prev = f.modes.stored_mode();
    }
    ASSERT_GE(deesc_times.size(), 2u) << "test didn't exercise recovery";
    for (size_t i = 1; i < deesc_times.size(); ++i) {
        EXPECT_GE(deesc_times[i] - deesc_times[i - 1], 60'000)
            << "min 60s between de-escalations";
    }
}

// --- Task 8.5.3.3 — degradation hysteresis under error injection ----------
// The plan's legs 2–3: an infrastructure-lag-class trigger must move
// Normal → ReadOnly inside one evaluation (the §2.7.3 "within 500ms"
// bound is trivially met — check_once() commits escalations
// synchronously, cooldown never delays a worse state), and recovery
// must hold ReadOnly until 30 CONSECUTIVE seconds of clear telemetry.

TEST(HealthCheckerUnit, InfraLagSignalEscalatesToReadOnlyImmediately) {
    Fixture f;
    // §2.7.3 L1 lag-family signal: coordination-Redis round-trip p50
    // >1s. The plan's "PostgreSQL replica lag >5s" trigger sits in the
    // same L1 row — a hard PG loss maps even further to MarketDataOnly
    // (asserted in PostgresOrWalFailureTriggersMarketDataOnly), the
    // fail-closed superset.
    f.sig.redis_p50_us = 2'000'000;
    const auto r = f.eval();
    EXPECT_TRUE(r.transitioned);  // same evaluation — no deferred commit
    EXPECT_EQ(f.modes.mode(), DegradationMode::ReadOnly);
    EXPECT_EQ(std::string(r.reason), "redis_slow");
    ASSERT_FALSE(f.alerts.rows.empty());
    EXPECT_EQ(f.alerts.rows.back().first, 2);  // P2 (spec §2.4 Alerting)

    // While the mode holds, a still-firing trigger re-affirms ReadOnly —
    // no transition churn, mode stable.
    const auto r2 = f.eval();
    EXPECT_FALSE(r2.transitioned);
    EXPECT_EQ(f.modes.mode(), DegradationMode::ReadOnly);
}

TEST(HealthCheckerUnit, ReadOnlyRecoveryDwellRequires30ConsecutiveClearSeconds) {
    Fixture f;
    // Isolate the dwell from the 60s ModeManager cooldown: cooldown=0
    // leaves sustained-clear telemetry as the only de-escalation gate.
    exch::HealthCheckerConfig cfg;
    cfg.cooldown_ms = 0;
    f.checker.set_config(cfg);

    f.sig.matching_p50_us = 900;
    ASSERT_TRUE(f.eval().transitioned);           // t=0 → ReadOnly
    ASSERT_EQ(f.modes.mode(), DegradationMode::ReadOnly);

    f.sig.matching_p50_us = 40;                   // telemetry clears
    for (int i = 0; i < 5; ++i) {                 // ~25s clear — dwell not met
        const auto r = f.eval();
        EXPECT_EQ(f.modes.mode(), DegradationMode::ReadOnly);
        EXPECT_TRUE(r.recovery_pending);
        EXPECT_FALSE(r.transitioned);
    }

    // A single unhealthy eval inside the window resets the
    // consecutive-clear clock — "30 consecutive seconds" is literal
    // (spec §2.7.3). The blip re-affirms ReadOnly without transitioning.
    f.sig.matching_p50_us = 900;                  // t≈30s
    const auto blip = f.eval();
    EXPECT_EQ(blip.target_mode, DegradationMode::ReadOnly);
    EXPECT_FALSE(blip.transitioned);
    EXPECT_EQ(f.modes.mode(), DegradationMode::ReadOnly);

    f.sig.matching_p50_us = 40;
    for (int i = 0; i < 6; ++i) {                 // ~30s post-blip, 0..25s
        const auto r = f.eval();                  //   into the new window
        EXPECT_EQ(f.modes.mode(), DegradationMode::ReadOnly)
            << "dwell must restart after the blip — only " << 5 * (i + 1)
            << "s of consecutive clear telemetry have elapsed";
        EXPECT_TRUE(r.recovery_pending);
        EXPECT_FALSE(r.transitioned);
    }
    // t≈65s absolute but only 30s CONSECUTIVE clear: dwell met, cooldown
    // disabled → de-escalation lands on this evaluation.
    const auto r = f.eval();
    EXPECT_TRUE(r.transitioned);
    EXPECT_EQ(f.modes.mode(), DegradationMode::Normal);
}

TEST(HealthCheckerUnit, MaintenanceIsManualHold) {
    Fixture f;
    ASSERT_TRUE(f.modes.set_mode(DegradationMode::Maintenance, "deploy"));
    f.sig.matching_p50_us = 40;
    for (int i = 0; i < 30; ++i) f.eval();  // ~150s healthy — never auto-exits
    EXPECT_EQ(f.modes.mode(), DegradationMode::Maintenance);
}

TEST(HealthCheckerUnit, UnwiredProbesAreHealthyDefaults) {
    exch::ModeManager modes;
    exch::HealthChecker checker{modes};  // scaffold — no probes at all
    const auto r = checker.check_once();
    EXPECT_EQ(r.target_mode, DegradationMode::Normal);
    EXPECT_FALSE(r.transitioned);
}

TEST(HealthCheckerUnit, RunnerThreadTicksAtInterval) {
    Fixture f;
    exch::HealthCheckerConfig cfg;
    cfg.interval_ms = 5;  // fast cadence for the test
    f.checker.set_config(cfg);

    std::atomic<int>* calls = new std::atomic<int>(0);
    exch::HealthProbes p = {};
    struct Ctx {
        std::atomic<int>* n;
    } ctx{calls};
    p.postgres_ok = [](void* c) noexcept -> bool {
        static_cast<Ctx*>(c)->n->fetch_add(1);
        return true;
    };
    p.postgres_ctx = &ctx;
    f.checker.set_probes(p);

    ASSERT_TRUE(f.checker.start());
    std::this_thread::sleep_for(std::chrono::milliseconds(60));
    f.checker.stop();
    EXPECT_GE(calls->load(), 3);  // ~5ms cadence → many evaluations in 60ms
    delete calls;
}

}  // namespace
