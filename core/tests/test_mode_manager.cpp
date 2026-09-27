// Task 2.3.6 — ModeManager / ModeStore / ModeEventPublisher tests.
//
// Deterministic coverage uses an in-memory ModeStore fake + fake clock;
// the Redis binding is exercised live at the bottom (gated on
// EXC_REDIS_TEST_ADDR).

#include <gtest/gtest.h>

#include <cstdint>
#include <cstdlib>
#include <memory>
#include <string>
#include <unistd.h>
#include <vector>

#include "degradation/ModeEventPublisher.hpp"
#include "degradation/ModeManager.hpp"
#include "degradation/RedisModeStore.hpp"
#include "ipc/IpcChannel.hpp"
#include "redis/RespClient.hpp"

namespace {

struct FakeClock {
    int64_t now = 1'700'000'000'000;  // plausible epoch ms
    static int64_t read(void* ctx) noexcept {
        return static_cast<FakeClock*>(ctx)->now;
    }
    void advance(int64_t ms) { now += ms; }
};

class FakeModeStore final : public exch::ModeStore {
public:
    bool write(const exch::ModeRecord& rec) noexcept override {
        ++writes;
        if (fail_) return false;
        rec_ = rec;
        present_ = true;
        return true;
    }
    bool read(exch::ModeRecord* out) noexcept override {
        ++reads;
        if (fail_) return false;
        if (!present_) {
            *out = exch::ModeRecord{};  // absent ⇒ Normal (spec §2.4)
            return true;
        }
        *out = rec_;
        return true;
    }
    void fail(bool on) { fail_ = on; }

    int writes = 0;
    int reads = 0;
    bool present_ = false;
    exch::ModeRecord rec_;

private:
    bool fail_ = false;
};

struct Transitions {
    struct Row {
        exch::DegradationMode from;
        exch::DegradationMode to;
        std::string reason;
        int64_t entered_at_ms;
    };
    std::vector<Row> rows;
    static void sink(void* ctx, exch::DegradationMode from,
                     exch::DegradationMode to, std::string_view reason,
                     int64_t entered_at_ms) noexcept {
        static_cast<Transitions*>(ctx)->rows.push_back(
            {from, to, std::string(reason), entered_at_ms});
    }
};

struct Fixture {
    FakeClock clock;
    FakeModeStore store;
    Transitions transitions;

    std::unique_ptr<exch::ModeManager> make() {
        auto m = std::make_unique<exch::ModeManager>(&store);
        m->set_clock(&FakeClock::read, &clock);
        m->set_transition_sink(&Transitions::sink, &transitions);
        return m;
    }
};

// --- mode vocabulary ------------------------------------------------------------

TEST(ModeManagerUnit, SixModesDefinedAndSeverityOrdered) {
    using exch::DegradationMode;
    EXPECT_EQ(exch::to_string(DegradationMode::Normal), "Normal");
    EXPECT_EQ(exch::to_string(DegradationMode::ReadOnly), "ReadOnly");
    EXPECT_EQ(exch::to_string(DegradationMode::MarketDataOnly),
              "MarketDataOnly");
    EXPECT_EQ(exch::to_string(DegradationMode::SpotOnly), "SpotOnly");
    EXPECT_EQ(exch::to_string(DegradationMode::Throttled), "Throttled");
    EXPECT_EQ(exch::to_string(DegradationMode::Maintenance), "Maintenance");

    // Priority: Maintenance > MarketDataOnly > SpotOnly > ReadOnly >
    // Throttled > Normal.
    EXPECT_LT(exch::severity(DegradationMode::Normal),
              exch::severity(DegradationMode::Throttled));
    EXPECT_LT(exch::severity(DegradationMode::Throttled),
              exch::severity(DegradationMode::ReadOnly));
    EXPECT_LT(exch::severity(DegradationMode::ReadOnly),
              exch::severity(DegradationMode::SpotOnly));
    EXPECT_LT(exch::severity(DegradationMode::SpotOnly),
              exch::severity(DegradationMode::MarketDataOnly));
    EXPECT_LT(exch::severity(DegradationMode::MarketDataOnly),
              exch::severity(DegradationMode::Maintenance));

    DegradationMode m;
    for (auto v : {"Normal", "ReadOnly", "MarketDataOnly", "SpotOnly",
                   "Throttled", "Maintenance"}) {
        ASSERT_TRUE(exch::mode_from_string(v, &m));
        EXPECT_EQ(exch::to_string(m), v);
    }
    EXPECT_FALSE(exch::mode_from_string("readonly", &m));  // exact casing
    EXPECT_FALSE(exch::mode_from_string("", &m));
}

TEST(ModeManagerUnit, SetModePersistsAtomicallyAndBroadcasts) {
    Fixture f;
    auto m = f.make();

    ASSERT_TRUE(m->set_mode(exch::DegradationMode::ReadOnly, "redis_slow"));
    EXPECT_EQ(m->mode(), exch::DegradationMode::ReadOnly);
    EXPECT_EQ(m->reason(), "redis_slow");
    EXPECT_EQ(m->entered_at_ms(), f.clock.now);

    // One atomic record carrying mode+entered_at+reason (MSET in Redis).
    ASSERT_EQ(f.store.writes, 1);
    EXPECT_EQ(f.store.rec_.mode, exch::DegradationMode::ReadOnly);
    EXPECT_EQ(f.store.rec_.reason, "redis_slow");
    EXPECT_EQ(f.store.rec_.entered_at_ms, f.clock.now);

    // WS system.status transition broadcast went out on the seam.
    ASSERT_EQ(f.transitions.rows.size(), 1u);
    EXPECT_EQ(f.transitions.rows[0].from, exch::DegradationMode::Normal);
    EXPECT_EQ(f.transitions.rows[0].to, exch::DegradationMode::ReadOnly);
    EXPECT_EQ(f.transitions.rows[0].reason, "redis_slow");
}

TEST(ModeManagerUnit, RefreshReadsAuthoritativeStore) {
    Fixture f;
    auto m = f.make();

    // Someone else (admin tool / watchdogd) wrote the record.
    f.store.rec_ = {exch::DegradationMode::Throttled, 424242, "ops"};
    f.store.present_ = true;

    EXPECT_EQ(m->get_mode(), exch::DegradationMode::Throttled);
    EXPECT_EQ(m->reason(), "ops");
    EXPECT_EQ(m->entered_at_ms(), 424242);
    EXPECT_FALSE(m->mode_stale());
}

TEST(ModeManagerUnit, AbsentKeysReadAsNormal) {
    Fixture f;
    auto m = f.make();
    EXPECT_EQ(m->get_mode(), exch::DegradationMode::Normal);
    EXPECT_FALSE(m->mode_stale());
}

TEST(ModeManagerUnit, RedisDownFailsClosedToMaintenanceAfterTwoFailures) {
    Fixture f;
    auto m = f.make();
    ASSERT_TRUE(m->set_mode(exch::DegradationMode::ReadOnly, "seed"));

    f.store.fail(true);
    EXPECT_FALSE(m->refresh());  // failure 1 — last-known mode still served
    EXPECT_TRUE(m->mode_stale());
    EXPECT_EQ(m->mode(), exch::DegradationMode::ReadOnly);
    EXPECT_FALSE(m->store_fail_override());

    EXPECT_FALSE(m->refresh());  // failure 2 — fail-closed override
    EXPECT_TRUE(m->store_fail_override());
    EXPECT_EQ(m->mode(), exch::DegradationMode::Maintenance);
    // The stored record itself is untouched (overlay is not a transition).
    EXPECT_EQ(m->stored_mode(), exch::DegradationMode::ReadOnly);
    EXPECT_EQ(f.store.writes, 1);

    f.store.fail(false);
    ASSERT_TRUE(m->refresh());  // recovery clears the overlay
    EXPECT_EQ(m->mode(), exch::DegradationMode::ReadOnly);
    EXPECT_FALSE(m->mode_stale());
}

TEST(ModeManagerUnit, CooldownGatesDeescalationNotEscalation) {
    Fixture f;
    auto m = f.make();

    ASSERT_TRUE(m->set_mode(exch::DegradationMode::ReadOnly, "core_slow"));

    // Recovery within 60s is refused — cooldown prevents flapping.
    f.clock.advance(10'000);
    EXPECT_FALSE(
        m->request_transition(exch::DegradationMode::Normal, "healthy"));
    EXPECT_EQ(m->mode(), exch::DegradationMode::ReadOnly);

    // Escalation bypasses cooldown entirely (fail-closed: a worse state is
    // never delayed).
    EXPECT_TRUE(m->request_transition(exch::DegradationMode::MarketDataOnly,
                                      "postgres_down"));
    EXPECT_EQ(m->mode(), exch::DegradationMode::MarketDataOnly);

    // A de-escalation inside the fresh 60s window is refused; after it,
    // allowed.
    f.clock.advance(10'000);
    EXPECT_FALSE(m->request_transition(exch::DegradationMode::Normal, "ok"));
    f.clock.advance(51'000);  // >60s since the last applied transition
    EXPECT_TRUE(m->request_transition(exch::DegradationMode::Normal, "ok"));
    EXPECT_EQ(m->mode(), exch::DegradationMode::Normal);
}

TEST(ModeManagerUnit, AutoPathNeverLeavesMaintenance) {
    Fixture f;
    auto m = f.make();

    ASSERT_TRUE(m->set_mode(exch::DegradationMode::Maintenance, "deploy"));
    f.clock.advance(120'000);  // well past cooldown — still refused
    EXPECT_FALSE(
        m->request_transition(exch::DegradationMode::Normal, "healthy"));
    EXPECT_EQ(m->mode(), exch::DegradationMode::Maintenance);

    // Manual path clears it (spec §2.4: admin re-enables).
    ASSERT_TRUE(m->set_mode(exch::DegradationMode::Normal, "admin_clear"));
    EXPECT_EQ(m->mode(), exch::DegradationMode::Normal);
}

TEST(ModeManagerUnit, NoStoreLocalOnlyScaffold) {
    // main.cpp scaffold: no store wired — transitions still apply locally.
    exch::ModeManager m;
    EXPECT_TRUE(m.refresh());  // trivially healthy without a store
    EXPECT_TRUE(m.set_mode(exch::DegradationMode::SpotOnly, "deriv"));
    EXPECT_EQ(m.mode(), exch::DegradationMode::SpotOnly);
}

// --- ModeEventPublisher ----------------------------------------------------------

class FakeChannel final : public exch::IpcChannel {
public:
    bool open() noexcept override {
        open_ = true;
        return true;
    }
    void close() noexcept override { open_ = false; }
    bool is_open() const noexcept override { return open_; }
    bool send(const void* data, uint32_t len) noexcept override {
        if (!open_ || fail_send_) return false;
        sent.emplace_back(static_cast<const char*>(data), len);
        return true;
    }
    int32_t poll(void*, uint32_t) noexcept override { return 0; }

    bool open_ = false;
    bool fail_send_ = false;
    std::vector<std::string> sent;
};

TEST(ModeEventPublisherTest, FrameContractAndSinkAdapter) {
    FakeChannel ch;
    ASSERT_TRUE(ch.open());
    exch::ModeEventPublisher pub(&ch);

    const std::string frame = exch::ModeEventPublisher::encode(
        exch::DegradationMode::Normal, exch::DegradationMode::ReadOnly,
        "redis_slow", 1'700'000'000'000);
    EXPECT_EQ(frame,
              "{\"type\":\"system.status\",\"event\":\"degradation_mode\","
              "\"mode\":\"ReadOnly\",\"prev\":\"Normal\","
              "\"reason\":\"redis_slow\",\"ts_ms\":1700000000000}");

    // Wired as the ModeManager transition sink: a set_mode publishes a frame.
    FakeModeStore store;
    exch::ModeManager m(&store);
    m.set_transition_sink(&exch::ModeEventPublisher::sink, &pub);
    ASSERT_TRUE(m.set_mode(exch::DegradationMode::Throttled, "capacity"));
    ASSERT_EQ(ch.sent.size(), 1u);
    EXPECT_NE(ch.sent[0].find("\"mode\":\"Throttled\""), std::string::npos);
    EXPECT_NE(ch.sent[0].find("\"prev\":\"Normal\""), std::string::npos);

    // Closed/absent channel: publish fails but the transition still applied.
    ch.close();
    ASSERT_TRUE(m.set_mode(exch::DegradationMode::Normal, "ok"));
    EXPECT_EQ(ch.sent.size(), 1u);
}

// --- live Redis binding ----------------------------------------------------------

class LiveMode : public ::testing::Test {
protected:
    void SetUp() override {
        const char* env = std::getenv("EXC_REDIS_TEST_ADDR");
        if (env == nullptr) GTEST_SKIP() << "set EXC_REDIS_TEST_ADDR=host:port";
        const std::string addr = env;
        const auto colon = addr.rfind(':');
        cfg_.host = addr.substr(0, colon);
        cfg_.port =
            static_cast<uint16_t>(std::atoi(addr.substr(colon + 1).c_str()));
        client_ = std::make_unique<exch::RespClient>(cfg_);
        ASSERT_TRUE(client_->connect()) << "redis unreachable";
    }
    void TearDown() override {
        if (client_) {
            client_->del("system:degradation:mode");
            client_->del("system:degradation:entered_at");
            client_->del("system:degradation:reason");
            client_->disconnect();
        }
    }
    exch::RespClientConfig cfg_;
    std::unique_ptr<exch::RespClient> client_;
};

TEST_F(LiveMode, AtomicKeysWrittenAndReadBack) {
    exch::RedisModeStore store(client_.get());
    exch::ModeManager m(&store);

    ASSERT_TRUE(m.set_mode(exch::DegradationMode::ReadOnly, "redis_slow"));

    // All three spec §4.2 keys are set by the single atomic write.
    exch::RespValue v;
    ASSERT_TRUE(client_->get("system:degradation:mode", &v));
    std::string_view sv;
    ASSERT_TRUE(v.as_string(&sv));
    EXPECT_EQ(sv, "ReadOnly");
    ASSERT_TRUE(client_->get("system:degradation:reason", &v));
    ASSERT_TRUE(v.as_string(&sv));
    EXPECT_EQ(sv, "redis_slow");
    ASSERT_TRUE(client_->get("system:degradation:entered_at", &v));
    ASSERT_TRUE(v.as_string(&sv));
    EXPECT_GT(std::strtoll(std::string(sv).c_str(), nullptr, 10), 0);

    // getMode() round-trips through the store.
    exch::ModeManager m2(&store);  // fresh cache — proves the read path
    EXPECT_EQ(m2.get_mode(), exch::DegradationMode::ReadOnly);
    EXPECT_EQ(m2.reason(), "redis_slow");
}

TEST_F(LiveMode, MissingKeysAreNormal) {
    client_->del("system:degradation:mode");
    exch::RedisModeStore store(client_.get());
    exch::ModeManager m(&store);
    EXPECT_EQ(m.get_mode(), exch::DegradationMode::Normal);
}

TEST_F(LiveMode, UnreachableStoreOverridesToMaintenance) {
    exch::RedisModeStore store(client_.get());
    exch::ModeManager m(&store);
    ASSERT_TRUE(m.refresh());

    client_->disconnect();  // simulate Redis loss
    EXPECT_FALSE(m.refresh());
    EXPECT_FALSE(m.refresh());
    EXPECT_EQ(m.mode(), exch::DegradationMode::Maintenance);  // fail-closed
    EXPECT_TRUE(m.mode_stale());
}

}  // namespace
