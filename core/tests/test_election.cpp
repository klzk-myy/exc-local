// Task 2.3.5 — LeaderElection unit tests over a deterministic in-memory
// LeaderLeaseStore fake (same contract as the Redis+Lua binding; live
// Redis integration is test_election_live.cpp, gated on EXC_REDIS_TEST_ADDR).
//
// Canonical mechanics verified here: spec §18.6.2 epoch lease — TTL 2,000ms /
// refresh 500ms / monotonic fencing epoch / token-checked release /
// fence_or_die on supersession (the §2.5 SETNX 10s/3s text is superseded and
// must not be implemented).

#include <gtest/gtest.h>

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "election/LeaderElection.hpp"
#include "election/LeaderLeaseStore.hpp"

namespace {

// --- deterministic clock --------------------------------------------------------

struct FakeClock {
    int64_t now = 1'000'000;  // ms — non-zero so "since" sentinels stay honest
    static int64_t read(void* ctx) noexcept {
        return static_cast<FakeClock*>(ctx)->now;
    }
    // Sleep advances the fake clock — elect()/tick() stay deterministic.
    static void sleep(void* ctx, int64_t ms) noexcept {
        if (ms > 0) static_cast<FakeClock*>(ctx)->now += ms;
    }
    void advance(int64_t ms) { now += ms; }
};

// --- in-memory lease store implementing the same semantics as the Lua ---------

class FakeLeaseStore final : public exch::LeaderLeaseStore {
public:
    explicit FakeLeaseStore(FakeClock* clock) : clock_(clock) {}

    bool try_acquire(std::string_view leader, int64_t ttl_ms, bool* granted,
                     uint64_t* epoch_out) noexcept override {
        ++acquire_calls;
        if (fail_all_) return false;
        const int64_t now = clock_->now;
        if (present_ && now < expires_at_) {
            *epoch_out = epoch_;
            *granted = (leader_ == leader);  // holder re-acquire = refresh
            return true;
        }
        epoch_ = ++counter_;  // INCR epoch counter — monotonic across expiry
        leader_.assign(leader);
        expires_at_ = now + ttl_ms;
        present_ = true;
        *epoch_out = epoch_;
        *granted = true;
        return true;
    }

    exch::LeaseCheck heartbeat(std::string_view leader, uint64_t epoch,
                               int64_t ttl_ms,
                               uint64_t* cur_epoch_out) noexcept override {
        ++heartbeat_calls;
        if (fail_all_) return exch::LeaseCheck::Error;
        const int64_t now = clock_->now;
        if (!present_ || now >= expires_at_) {
            if (now >= expires_at_) present_ = false;
            return exch::LeaseCheck::Absent;
        }
        *cur_epoch_out = epoch_;
        if (leader_ == leader && epoch_ == epoch) {
            expires_at_ = now + ttl_ms;   // token-checked TTL re-arm
            ++hb_key_writes;              // leader:heartbeat:{shard} mirror
            return exch::LeaseCheck::Held;
        }
        return exch::LeaseCheck::Superseded;
    }

    bool release(std::string_view leader, uint64_t epoch) noexcept override {
        ++release_calls;
        if (fail_all_) return false;
        const int64_t now = clock_->now;
        if (!present_ || now >= expires_at_) return false;
        if (leader_ != leader || epoch_ != epoch) return false;
        present_ = false;
        return true;
    }

    bool read(exch::LeaseRecord* out) noexcept override {
        ++read_calls;
        if (fail_all_) return false;
        const int64_t now = clock_->now;
        if (present_ && now >= expires_at_) present_ = false;  // lazy expiry
        out->present = present_;
        out->epoch = epoch_;
        out->leader = leader_;
        out->ttl_ms = present_ ? expires_at_ - now : -2;
        return true;
    }

    // test controls
    void fail_all(bool on) { fail_all_ = on; }
    void force_expire() { expires_at_ = clock_->now - 1; }
    // Write a foreign lease directly (simulates a competitor's acquire).
    void foreign_write(std::string_view leader, uint64_t epoch,
                       int64_t ttl_ms) {
        leader_.assign(leader);
        epoch_ = epoch;
        counter_ = epoch > counter_ ? epoch : counter_;
        expires_at_ = clock_->now + ttl_ms;
        present_ = true;
    }

    int acquire_calls = 0;
    int heartbeat_calls = 0;
    int release_calls = 0;
    int read_calls = 0;
    int hb_key_writes = 0;

private:
    FakeClock* clock_;
    bool present_ = false;
    uint64_t epoch_ = 0;
    std::string leader_;
    int64_t expires_at_ = 0;
    uint64_t counter_ = 0;
    bool fail_all_ = false;
};

// --- test fixture ---------------------------------------------------------------

struct Hooks {
    int terminate_calls = 0;
    int readonly_calls = 0;
    int leader_acquired = 0;
    int leader_lost = 0;
    std::vector<std::pair<int, std::string>> alerts;

    static void on_terminate(void* ctx) noexcept {
        ++static_cast<Hooks*>(ctx)->terminate_calls;
    }
    static void on_readonly(void* ctx) noexcept {
        ++static_cast<Hooks*>(ctx)->readonly_calls;
    }
    static void on_alert(void* ctx, int level, const char* code,
                         const char* detail) noexcept {
        (void)detail;
        static_cast<Hooks*>(ctx)->alerts.emplace_back(level, code);
    }
    static void on_event(void* ctx, const char* kind, uint64_t epoch) noexcept {
        (void)epoch;
        auto* h = static_cast<Hooks*>(ctx);
        if (std::string(kind) == "leader_acquired") ++h->leader_acquired;
        if (std::string(kind) == "leader_lost") ++h->leader_lost;
    }
};

struct Fixture {
    FakeClock clock;
    FakeLeaseStore store{&clock};
    Hooks hooks;

    exch::LeaderElectionConfig cfg() const {
        exch::LeaderElectionConfig c;
        c.startup_stabilize_ms = 0;   // tests control time explicitly
        return c;
    }

    std::unique_ptr<exch::LeaderElection> make(uint32_t shard,
                                               const char* node) {
        auto e = std::make_unique<exch::LeaderElection>(shard, node, &store,
                                                        cfg());
        e->set_clock(&FakeClock::read, &clock);
        e->set_sleep_hook(&FakeClock::sleep, &clock);
        e->set_terminate_hook(&Hooks::on_terminate, &hooks);
        e->set_alert_sink(&Hooks::on_alert, &hooks);
        e->set_readonly_hook(&Hooks::on_readonly, &hooks);
        e->set_event_sink(&Hooks::on_event, &hooks);
        return e;
    }
};

// --- tests ------------------------------------------------------------------------

TEST(LeaderElectionUnit, LeaseValueFormatRoundTrip) {
    // Canonical {"epoch":N,"leader":"<id>"} encoding.
    const std::string v = exch::format_lease_value(7, "core-node-1");
    EXPECT_EQ(v, "{\"epoch\":7,\"leader\":\"core-node-1\"}");
    uint64_t e = 0;
    std::string leader;
    ASSERT_TRUE(exch::parse_lease_value(v, &e, &leader));
    EXPECT_EQ(e, 7u);
    EXPECT_EQ(leader, "core-node-1");
    // Malformed / foreign values never parse (fail-closed).
    EXPECT_FALSE(exch::parse_lease_value("", &e, &leader));
    EXPECT_FALSE(exch::parse_lease_value("garbage", &e, &leader));
    EXPECT_FALSE(exch::parse_lease_value("{\"epoch\":7}", &e, &leader));
    EXPECT_FALSE(
        exch::parse_lease_value("{\"epoch\":,\"leader\":\"x\"}", &e, &leader));
    EXPECT_FALSE(exch::parse_lease_value("{\"epoch\":1,\"leader\":\"\"}",
                                         &e, &leader));
    // node-id charset guard
    EXPECT_TRUE(exch::valid_node_id("core-node-1.b_2"));
    EXPECT_FALSE(exch::valid_node_id(""));
    EXPECT_FALSE(exch::valid_node_id("bad\"id"));
    EXPECT_FALSE(exch::valid_node_id("a b"));
}

TEST(LeaderElectionUnit, AcquireFirstWinsSecondFails) {
    Fixture f;
    auto a = f.make(0, "node-a");
    auto b = f.make(0, "node-b");  // same shard lease

    ASSERT_TRUE(a->acquire());
    EXPECT_TRUE(a->is_leader());
    EXPECT_EQ(a->epoch(), 1u);
    EXPECT_TRUE(a->may_process_orders());

    EXPECT_FALSE(b->acquire());
    EXPECT_FALSE(b->is_leader());
    EXPECT_FALSE(b->may_process_orders());
    // Loser observed the winner's epoch.
    f.store.foreign_write("node-a", a->epoch(), 60'000);
    EXPECT_TRUE(b->check_split_brain());   // foreign holder visible
    EXPECT_FALSE(a->check_split_brain());  // leader sees its own lease
}

TEST(LeaderElectionUnit, HeartbeatRenewsLeaseOnCadence) {
    Fixture f;
    auto a = f.make(0, "node-a");
    ASSERT_TRUE(a->acquire());

    // Refresh cadence: tick() heartbeats only every refresh_period_ms.
    const int before = f.store.heartbeat_calls;
    a->tick();  // now == acquire time — no refresh due
    EXPECT_EQ(f.store.heartbeat_calls, before);

    f.clock.advance(600);  // past the 500ms refresh cadence
    a->tick();
    EXPECT_EQ(f.store.heartbeat_calls, before + 1);
    EXPECT_EQ(f.store.hb_key_writes, 1);  // leader:heartbeat mirror updated

    exch::LeaseRecord rec;
    ASSERT_TRUE(f.store.read(&rec));
    EXPECT_TRUE(rec.present);
    EXPECT_EQ(rec.epoch, 1u);
    EXPECT_EQ(rec.leader, "node-a");
    EXPECT_EQ(rec.ttl_ms, 2000);  // TTL re-armed to the full 2,000ms
}

TEST(LeaderElectionUnit, FollowerTakesOverAfterExpiryWithEpochPlusOne) {
    Fixture f;
    auto a = f.make(0, "node-a");
    auto b = f.make(0, "node-b");
    ASSERT_TRUE(a->acquire());
    ASSERT_EQ(a->epoch(), 1u);

    // A stalls (crashed): its 2,000ms lease lapses while B keeps polling.
    f.clock.advance(250);  // B's poll cadence
    b->tick();
    EXPECT_FALSE(b->is_leader());  // lease still fresh — no takeover yet
    f.clock.advance(2'000);        // now > 2,000ms unrefreshed
    for (int i = 0; i < 40 && !b->is_leader(); ++i) {
        f.clock.advance(250);
        b->tick();
    }
    ASSERT_TRUE(b->is_leader());
    EXPECT_EQ(b->epoch(), 2u);  // monotonic fencing epoch: old+1

    // The dead leader's next heartbeat detects supersession → fenced.
    a->heartbeat();
    EXPECT_TRUE(a->fenced());
    EXPECT_TRUE(a->terminated());
    EXPECT_FALSE(a->may_process_orders());
    EXPECT_EQ(f.hooks.terminate_calls, 1);
    EXPECT_EQ(f.hooks.readonly_calls, 1);   // degradation → ReadOnly hook ran
    ASSERT_FALSE(f.hooks.alerts.empty());
    EXPECT_EQ(f.hooks.alerts.back().first, 1);  // P1
    EXPECT_EQ(f.hooks.alerts.back().second, "SPLIT_BRAIN_FENCED");
    // Both acquisitions share this fixture's hook sink: A's initial grant
    // and B's takeover after lease expiry each emitted leader_acquired.
    EXPECT_EQ(f.hooks.leader_acquired, 2);
    EXPECT_EQ(f.hooks.leader_lost, 1);          // A's loss observed
}

TEST(LeaderElectionUnit, EpochIsMonotonicAcrossReleaseAndReacquire) {
    Fixture f;
    auto a = f.make(0, "node-a");
    ASSERT_TRUE(a->acquire());
    EXPECT_EQ(a->epoch(), 1u);
    ASSERT_TRUE(a->release());
    EXPECT_FALSE(a->is_leader());

    auto b = f.make(0, "node-b");
    ASSERT_TRUE(b->acquire());
    EXPECT_EQ(b->epoch(), 2u);  // strictly increasing, no reuse
}

TEST(LeaderElectionUnit, ReleaseIsTokenChecked) {
    Fixture f;
    auto a = f.make(0, "node-a");
    auto b = f.make(0, "node-b");
    ASSERT_TRUE(a->acquire());

    // Non-holder release attempt: wrong token must not delete A's lease.
    EXPECT_FALSE(f.store.release("node-b", a->epoch()));
    EXPECT_FALSE(b->release());  // B is not leader — nothing to release
    exch::LeaseRecord rec;
    ASSERT_TRUE(f.store.read(&rec));
    EXPECT_TRUE(rec.present);

    ASSERT_TRUE(a->release());  // holder releases cleanly
    ASSERT_TRUE(f.store.read(&rec));
    EXPECT_FALSE(rec.present);
    ASSERT_TRUE(b->acquire());  // immediate takeover possible post-release
    EXPECT_EQ(b->epoch(), 2u);
}

TEST(LeaderElectionUnit, StartupStabilizationGatesAcquire) {
    Fixture f;
    exch::LeaderElectionConfig cfg;
    cfg.startup_stabilize_ms = 5'000;  // plan AC: 5s follower settle
    exch::LeaderElection e(0, "node-a", &f.store, cfg);
    e.set_clock(&FakeClock::read, &f.clock);

    EXPECT_FALSE(e.acquire());   // inside the settle window
    EXPECT_EQ(f.store.acquire_calls, 0);  // no store touch at all

    f.clock.advance(5'001);
    EXPECT_TRUE(e.acquire());
    EXPECT_TRUE(e.is_leader());
}

TEST(LeaderElectionUnit, ElectCompletesBeforeOrderProcessing) {
    Fixture f;
    // Winner: elect() resolves LEADER inside its budget.
    {
        auto a = f.make(0, "node-a");
        EXPECT_EQ(a->elect(0), exch::LeaderRole::LEADER);
        EXPECT_TRUE(a->may_process_orders());
    }
    // Contended shard: follower elect() spends the budget then reports
    // FOLLOWER — it must never report leadership it doesn't hold.
    {
        Fixture g;
        auto a = g.make(0, "node-a");
        ASSERT_TRUE(a->acquire());
        auto b = g.make(0, "node-b");
        EXPECT_EQ(b->elect(500), exch::LeaderRole::FOLLOWER);
        EXPECT_FALSE(b->may_process_orders());
    }
}

TEST(LeaderElectionUnit, RedisDownFailsClosed) {
    Fixture f;
    auto a = f.make(0, "node-a");
    f.store.fail_all(true);

    // Acquire fails closed: follower, no matching without a lease.
    EXPECT_FALSE(a->acquire());
    EXPECT_FALSE(a->is_leader());
    EXPECT_FALSE(a->may_process_orders());
    EXPECT_FALSE(a->epoch_current());
    EXPECT_FALSE(a->validate_epoch());

    // Recovery is possible once the store returns — a fresh epoch is granted.
    f.store.fail_all(false);
    ASSERT_TRUE(a->acquire());
    EXPECT_TRUE(a->is_leader());
    EXPECT_TRUE(a->may_process_orders());
}

TEST(LeaderElectionUnit, HeartbeatTransportErrorFreezesThenFences) {
    Fixture f;
    auto a = f.make(0, "node-a");
    ASSERT_TRUE(a->acquire());
    ASSERT_TRUE(a->may_process_orders());

    f.store.fail_all(true);
    f.clock.advance(600);
    a->tick();  // heartbeat errors → frozen immediately (fail-closed)
    EXPECT_FALSE(a->may_process_orders());  // no matching without lease proof
    EXPECT_FALSE(a->fenced());              // still inside TTL grace
    EXPECT_TRUE(a->is_leader());            // not yet demoted

    // Still failing past the 2,000ms lease boundary → claim is provably
    // dead on any correct store → self-terminate.
    f.clock.advance(2'000);
    a->tick();
    EXPECT_TRUE(a->fenced());
    EXPECT_TRUE(a->terminated());
    EXPECT_EQ(f.hooks.terminate_calls, 1);
    EXPECT_EQ(f.hooks.readonly_calls, 1);
}

TEST(LeaderElectionUnit, HeartbeatAbsentLeaseIsFatal) {
    Fixture f;
    auto a = f.make(0, "node-a");
    ASSERT_TRUE(a->acquire());

    // Lease vanishes (expired under a stall or revoked) before the next
    // heartbeat — absence is unprovable ⇒ fail-closed fence.
    f.store.force_expire();
    EXPECT_FALSE(a->heartbeat());
    EXPECT_TRUE(a->fenced());
    EXPECT_EQ(f.hooks.terminate_calls, 1);
}

TEST(LeaderElectionUnit, EpochCurrentValidatesAgainstStore) {
    Fixture f;
    auto a = f.make(0, "node-a");
    ASSERT_TRUE(a->acquire());
    EXPECT_TRUE(a->epoch_current());
    EXPECT_TRUE(a->validate_epoch());

    // A competitor lands a newer epoch behind our back.
    f.store.foreign_write("node-b", 5, 60'000);
    EXPECT_FALSE(a->epoch_current());
    EXPECT_FALSE(a->validate_epoch());  // auto-fences on confirmation
    EXPECT_TRUE(a->fenced());
    EXPECT_TRUE(a->terminated());
}

TEST(LeaderElectionUnit, CheckSplitBrainDetectsForeignHolder) {
    Fixture f;
    auto a = f.make(0, "node-a");
    ASSERT_TRUE(a->acquire());

    f.store.foreign_write("node-b", a->epoch() + 1, 60'000);
    EXPECT_TRUE(a->check_split_brain());  // detection…
    EXPECT_TRUE(a->fenced());             // …and fail-closed fencing

    // Clean state: sole holder ⇒ no split brain.
    Fixture g;
    auto b = g.make(0, "node-b");
    ASSERT_TRUE(b->acquire());
    EXPECT_FALSE(b->check_split_brain());
}

TEST(LeaderElectionUnit, StoreLessScaffoldNeverLeads) {
    // main.cpp's one-arg scaffold: no lease store ⇒ permanent follower
    // (fail-closed — election can't conclude ⇒ matching stays shut).
    exch::LeaderElection e(3);
    EXPECT_FALSE(e.acquire());
    EXPECT_EQ(e.elect(0), exch::LeaderRole::FOLLOWER);
    EXPECT_FALSE(e.may_process_orders());
    EXPECT_FALSE(e.is_leader());
}

TEST(LeaderElectionUnit, InvalidNodeIdNeverLeads) {
    Fixture f;
    // Rejected charset ⇒ the election object refuses leadership entirely.
    auto bad = f.make(0, "bad node\"");
    EXPECT_FALSE(bad->acquire());
    EXPECT_EQ(bad->elect(0), exch::LeaderRole::FOLLOWER);
}

TEST(LeaderElectionUnit, LeaderIdempotentReacquireKeepsEpoch) {
    Fixture f;
    auto a = f.make(0, "node-a");
    ASSERT_TRUE(a->acquire());
    const uint64_t e = a->epoch();
    // Re-acquire while we hold: same epoch, TTL re-armed — not a new grant.
    ASSERT_TRUE(a->acquire());
    EXPECT_EQ(a->epoch(), e);
}

}  // namespace
