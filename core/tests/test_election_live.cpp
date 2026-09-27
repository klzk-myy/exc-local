// Task 2.3.5 — live integration tests: RespClient + RedisLeaseStore +
// LeaderElection against a real Redis (dev coordination primary).
//
// Gated: skipped unless EXC_REDIS_TEST_ADDR=host:port is set (dev topology
// uses 127.0.0.1:16379 — docker-compose.dev.yml). Each test uses a unique
// high-numbered shard so the keys can't collide with real engine leases, and
// cleans them up on exit.

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <memory>
#include <string>
#include <thread>
#include <unistd.h>

#include "election/LeaderElection.hpp"
#include "election/LeaderLeaseStore.hpp"
#include "redis/RespClient.hpp"

namespace {

std::string redis_addr() {
    const char* env = std::getenv("EXC_REDIS_TEST_ADDR");
    return env != nullptr ? std::string(env) : std::string();
}

struct LiveEnv {
    exch::RespClientConfig cfg;
    std::unique_ptr<exch::RespClient> client;
    uint32_t shard = 0;

    bool up() {
        const std::string addr = redis_addr();
        if (addr.empty()) return false;
        const auto colon = addr.rfind(':');
        cfg.host = addr.substr(0, colon);
        cfg.port = static_cast<uint16_t>(std::atoi(addr.substr(colon + 1).c_str()));
        cfg.connect_timeout_ms = 1'000;
        cfg.io_timeout_ms = 1'000;
        client = std::make_unique<exch::RespClient>(cfg);
        if (!client->connect()) {
            client.reset();
            return false;
        }
        // Unique shard per process: high band + pid — never touches real
        // engine:leader:{0..N} keys.
        shard = 900'000u + static_cast<uint32_t>(::getpid() % 100'000);
        return true;
    }

    void cleanup() {
        if (!client) return;
        char buf[96];
        std::snprintf(buf, sizeof(buf), "engine:leader:%u", shard);
        client->del(buf);
        std::snprintf(buf, sizeof(buf), "engine:leader:epoch:%u", shard);
        client->del(buf);
        std::snprintf(buf, sizeof(buf), "leader:heartbeat:%u", shard);
        client->del(buf);
    }
};

struct TermHook {
    int calls = 0;
    static void fire(void* ctx) noexcept { ++static_cast<TermHook*>(ctx)->calls; }
};

exch::LeaderElectionConfig fast_cfg() {
    exch::LeaderElectionConfig c;
    c.startup_stabilize_ms = 0;  // tests drive timing explicitly
    c.follower_poll_ms = 100;
    return c;
}

TEST(LiveElection, AcquireHeartbeatTtlAndValue) {
    LiveEnv env;
    if (!env.up()) GTEST_SKIP() << "set EXC_REDIS_TEST_ADDR=host:port";
    env.cleanup();

    exch::RedisLeaseStore store_a(env.client.get(), env.shard);
    exch::RespClient client_b(env.cfg);
    ASSERT_TRUE(client_b.connect());
    exch::RedisLeaseStore store_b(&client_b, env.shard);

    exch::LeaderElection a(env.shard, "live-node-a", &store_a, fast_cfg());
    exch::LeaderElection b(env.shard, "live-node-b", &store_b, fast_cfg());

    // First caller wins; second fails on the same shard key.
    ASSERT_TRUE(a.acquire());
    EXPECT_TRUE(a.is_leader());
    EXPECT_FALSE(b.acquire());

    // Canonical lease record: {"epoch":N,"leader":"<id>"}, PX ≈ 2,000ms.
    exch::RespValue v;
    ASSERT_TRUE(env.client->get(store_a.lease_key(), &v));
    std::string_view sv;
    ASSERT_TRUE(v.as_string(&sv));
    uint64_t epoch = 0;
    std::string leader;
    ASSERT_TRUE(exch::parse_lease_value(sv, &epoch, &leader));
    EXPECT_EQ(epoch, a.epoch());
    EXPECT_EQ(leader, "live-node-a");

    const int64_t ttl = env.client->pttl(store_a.lease_key());
    EXPECT_GT(ttl, 0);
    EXPECT_LE(ttl, 2'000);

    // Heartbeat re-arms the TTL (spec §18.6.2: refreshed every 500ms).
    std::this_thread::sleep_for(std::chrono::milliseconds(600));
    ASSERT_TRUE(a.heartbeat());
    const int64_t ttl2 = env.client->pttl(store_a.lease_key());
    EXPECT_GT(ttl2, 1'000);   // re-armed well above the decayed value
    EXPECT_LE(ttl2, 2'000);

    // §4.2 observability mirror refreshed by the same atomic step.
    ASSERT_TRUE(env.client->get("leader:heartbeat:" +
                                    std::to_string(env.shard),
                                &v));
    ASSERT_TRUE(v.as_string(&sv));
    EXPECT_EQ(sv, "live-node-a");

    ASSERT_TRUE(a.release());  // token-checked release
    ASSERT_TRUE(env.client->get(store_a.lease_key(), &v));
    EXPECT_TRUE(v.is_nil());
    env.cleanup();
}

TEST(LiveElection, FollowerTakeoverGetsEpochPlusOne) {
    LiveEnv env;
    if (!env.up()) GTEST_SKIP() << "set EXC_REDIS_TEST_ADDR=host:port";
    env.cleanup();

    exch::RedisLeaseStore store_a(env.client.get(), env.shard);
    exch::RespClient client_b(env.cfg);
    ASSERT_TRUE(client_b.connect());
    exch::RedisLeaseStore store_b(&client_b, env.shard);

    uint64_t first_epoch = 0;
    {
        exch::LeaderElection a(env.shard, "live-node-a", &store_a, fast_cfg());
        ASSERT_TRUE(a.acquire());
        first_epoch = a.epoch();
        // a exits scope WITHOUT release → simulated crash: lease lapses.
    }

    // Follower detects the dead lease only after >2,000ms unrefreshed.
    std::this_thread::sleep_for(std::chrono::milliseconds(2'200));
    exch::LeaderElection b(env.shard, "live-node-b", &store_b, fast_cfg());
    ASSERT_TRUE(b.acquire());
    EXPECT_TRUE(b.is_leader());
    EXPECT_EQ(b.epoch(), first_epoch + 1);  // monotonic fencing epoch

    ASSERT_TRUE(b.release());
    env.cleanup();
}

TEST(LiveElection, TokenCheckedReleaseRejectsNonHolder) {
    LiveEnv env;
    if (!env.up()) GTEST_SKIP() << "set EXC_REDIS_TEST_ADDR=host:port";
    env.cleanup();

    exch::RedisLeaseStore store_a(env.client.get(), env.shard);
    exch::RespClient client_b(env.cfg);
    ASSERT_TRUE(client_b.connect());
    exch::RedisLeaseStore store_b(&client_b, env.shard);

    exch::LeaderElection a(env.shard, "live-node-a", &store_a, fast_cfg());
    ASSERT_TRUE(a.acquire());

    // Wrong token / wrong epoch must not delete A's lease.
    EXPECT_FALSE(store_b.release("live-node-b", a.epoch()));
    EXPECT_FALSE(store_b.release("live-node-a", a.epoch() + 1));
    exch::RespValue v;
    ASSERT_TRUE(env.client->get(store_a.lease_key(), &v));
    EXPECT_FALSE(v.is_nil());

    ASSERT_TRUE(a.release());
    env.cleanup();
}

TEST(LiveElection, StaleLeaderFencesOnHeartbeat) {
    LiveEnv env;
    if (!env.up()) GTEST_SKIP() << "set EXC_REDIS_TEST_ADDR=host:port";
    env.cleanup();

    exch::RedisLeaseStore store_a(env.client.get(), env.shard);
    exch::RespClient client_b(env.cfg);
    ASSERT_TRUE(client_b.connect());
    exch::RedisLeaseStore store_b(&client_b, env.shard);

    exch::LeaderElection a(env.shard, "live-node-a", &store_a, fast_cfg());
    ASSERT_TRUE(a.acquire());
    TermHook term;
    a.set_terminate_hook(&TermHook::fire, &term);

    // Simulate the partition race: A's lease expires, B wins a newer epoch.
    ASSERT_TRUE(store_a.release("live-node-a", a.epoch()));
    exch::LeaderElection b(env.shard, "live-node-b", &store_b, fast_cfg());
    ASSERT_TRUE(b.acquire());
    ASSERT_GT(b.epoch(), a.epoch());

    // A's next heartbeat sees epoch_local < epoch_current → fenced,
    // terminate hook (SIGTERM seam) invoked, matching off.
    EXPECT_FALSE(a.heartbeat());
    EXPECT_TRUE(a.fenced());
    EXPECT_EQ(term.calls, 1);
    EXPECT_FALSE(a.may_process_orders());
    // epoch_current()/validate_epoch() confirm the fencing verdict.
    EXPECT_FALSE(a.epoch_current());

    ASSERT_TRUE(b.release());
    env.cleanup();
}

}  // namespace
