// Phase-21 Task 21.3.10 coverage — the in-process sanctions account-flag
// cache + the PreTradeChecker check-0c integration:
//   * sanctions_key_into parses exc:sanctions:flagged:{id} and
//     flagged:cleared:{id}; foreign/malformed keys are ignored
//   * verdict_for: no snapshot / unverifiable / missing heartbeat / stale
//     snapshot -> UNVERIFIABLE; flagged -> FLAGGED; cleared marker
//     overrides a stale flag; unflagged -> clear; strict-mode
//     require_screened -> UNSCREENED without the bitmap bit
//   * PreTradeChecker: bound cache flagged -> SANCTIONS_HIT before the
//     account-status check; unverifiable -> SANCTIONS_SERVICE_UNAVAILABLE;
//     unbound seam keeps the legacy accept behavior
//   * SanctionsRefresher round trip against live Redis (gated on
//     EXC_REDIS_TEST_ADDR, mirroring test_election_live.cpp).

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <memory>
#include <string>
#include <thread>

#include <cstdio>

#include "redis/RespClient.hpp"
#include "risk/PreTradeChecker.hpp"
#include "risk/SanctionsCache.hpp"
#include "utils/TimeUtils.hpp"

using namespace exch;

namespace {

Instrument make_instrument() {
    Instrument i{};
    i.instrument_id = 1;
    std::snprintf(i.symbol, sizeof(i.symbol), "%s", "EUR/USD");
    i.type = InstrumentType::SPOT;
    i.status = InstrumentStatus::ACTIVE;
    i.pip_factor = 10;
    i.pip_size_ticks = 10'000;
    i.tick_size_ticks = 1'000;
    i.lot_size_units = 1'000'000;
    i.min_order_qty_units = 1'000'000;
    i.max_order_qty_units = 1'000'000'000'000;
    i.min_notional_units = 10'000'000;
    i.price_band_pct_up = 200;
    i.price_band_pct_down = 500;
    i.last_price_ticks = 110'000'000;
    return i;
}

Order make_order(uint64_t account_id) {
    Order o{};
    o.id = 1;
    o.account_id = account_id;
    o.side = Side::BUY;
    o.type = OrderType::LIMIT;
    o.tif = TimeInForce::GTC;
    o.stp_mode = StpMode::CANCEL_NEWEST;
    o.flags = 0;
    o.price_ticks = 110'000'000;
    o.qty_units = 1'000'000'000;
    return o;
}

struct OkAccounts final : IAccountState {
    AccountStatus status(uint64_t) const noexcept override {
        return AccountStatus::ACTIVE;
    }
    int64_t available_balance(uint64_t, uint64_t,
                              BalanceUnit) const noexcept override {
        return 1'000'000'000'000'000;
    }
    uint32_t open_position_count(uint64_t) const noexcept override {
        return 0;
    }
    StpMode default_stp_mode(uint64_t) const noexcept override {
        return static_cast<StpMode>(kStpModeUnset);
    }
    ClientCategory client_category(uint64_t) const noexcept override {
        return ClientCategory::PROFESSIONAL;
    }
    KycTier kyc_tier(uint64_t) const noexcept override {
        return KycTier::T2;
    }
};

struct FlatPositions final : IPositionState {
    int64_t net_position_units(uint64_t, uint64_t) const noexcept override {
        return 0;
    }
};

struct FlatMarket final : IMarketState {
    int64_t last_price_ticks(uint64_t) const noexcept override { return 0; }
    int64_t price_ticks_ago(uint64_t, uint64_t,
                            uint64_t) const noexcept override { return 0; }
    int64_t best_bid_ticks(uint64_t) const noexcept override { return 0; }
    int64_t best_ask_ticks(uint64_t) const noexcept override { return 0; }
};

// Verified snapshot: heartbeat present + fresh applied_ns.
std::shared_ptr<SanctionsCache::Snapshot> verified(uint64_t now_ns) {
    auto s = std::make_shared<SanctionsCache::Snapshot>();
    s->heartbeat_unix = 1'700'000'000;
    s->applied_ns = now_ns;
    return s;
}

}  // namespace

// --- key parsing ---------------------------------------------------------------

TEST(SanctionsKey, ParsesFlaggedAndCleared) {
    SanctionsCache::Snapshot s;
    EXPECT_TRUE(sanctions_key_into("exc:sanctions:flagged:42", &s));
    EXPECT_EQ(s.flagged.count(42), 1u);
    EXPECT_TRUE(
        sanctions_key_into("exc:sanctions:flagged:cleared:77", &s));
    EXPECT_EQ(s.cleared.count(77), 1u);
}

TEST(SanctionsKey, RejectsForeignAndMalformed) {
    SanctionsCache::Snapshot s;
    EXPECT_FALSE(sanctions_key_into("halt:account:7", &s));
    EXPECT_FALSE(sanctions_key_into("exc:sanctions:flagged:", &s));
    EXPECT_FALSE(sanctions_key_into("exc:sanctions:flagged:abc", &s));
    EXPECT_FALSE(sanctions_key_into("exc:sanctions:flagged:12x", &s));
    EXPECT_FALSE(sanctions_key_into("exc:sanctions:flagged:set", &s));
    EXPECT_FALSE(sanctions_key_into("exc:sanctions:feed:heartbeat", &s));
    EXPECT_FALSE(sanctions_key_into("exc:sanctions:flagged:cleared:", &s));
    EXPECT_TRUE(s.flagged.empty() && s.cleared.empty());
    EXPECT_FALSE(sanctions_key_into("exc:sanctions:flagged:1", nullptr));
}

// --- verdict_for ---------------------------------------------------------------

TEST(SanctionsVerdict, NoSnapshotIsUnverifiable) {
    SanctionsCache c;
    EXPECT_EQ(c.verdict_for(7, steady_ns()), SanctionsCache::Verdict::Unverifiable);
}

TEST(SanctionsVerdict, MarkedUnverifiableRejects) {
    SanctionsCache c;
    c.mark_unverifiable();
    EXPECT_EQ(c.verdict_for(7, steady_ns()), SanctionsCache::Verdict::Unverifiable);
}

TEST(SanctionsVerdict, MissingHeartbeatIsUnverifiable) {
    SanctionsCache c;
    auto s = std::make_shared<SanctionsCache::Snapshot>();
    s->applied_ns = steady_ns();  // heartbeat_unix = 0 — never published
    c.apply(s);
    EXPECT_EQ(c.verdict_for(7, steady_ns()), SanctionsCache::Verdict::Unverifiable);
}

TEST(SanctionsVerdict, FlaggedAccountHits) {
    SanctionsCache c;
    const uint64_t now = steady_ns();
    auto s = verified(now);
    s->flagged.insert(7);
    c.apply(s);
    EXPECT_EQ(c.verdict_for(7, now), SanctionsCache::Verdict::Hit);
    EXPECT_EQ(c.verdict_for(8, now), SanctionsCache::Verdict::Clear);
}

TEST(SanctionsVerdict, ClearedOverridesFlagged) {
    SanctionsCache c;
    const uint64_t now = steady_ns();
    auto s = verified(now);
    s->flagged.insert(7);
    s->cleared.insert(7);
    c.apply(s);
    EXPECT_EQ(c.verdict_for(7, now), SanctionsCache::Verdict::Clear);
}

TEST(SanctionsVerdict, StaleSnapshotIsUnverifiable) {
    SanctionsCache c;
    c.set_max_age_ns(1'000'000);  // 1ms
    auto s = std::make_shared<SanctionsCache::Snapshot>();
    s->heartbeat_unix = 1;
    s->applied_ns = 1'000'000'000;  // applied 1s ago
    c.apply(s);
    EXPECT_EQ(c.verdict_for(7, 2'000'000'000ULL),
                 SanctionsCache::Verdict::Unverifiable);
}

TEST(SanctionsVerdict, StrictModeUnscreenedRejects) {
    SanctionsCache c;
    c.set_require_screened(true);
    const uint64_t now = steady_ns();
    auto s = verified(now);
    // screened bit set for account 9 only (byte 1, bit 1).
    s->screened_bitmap = std::string("\x00\x02", 2);
    c.apply(s);
    EXPECT_EQ(c.verdict_for(7, now), SanctionsCache::Verdict::Unscreened);
    EXPECT_EQ(c.verdict_for(9, now), SanctionsCache::Verdict::Clear);
    // Flagged still wins over screened status.
    auto s2 = verified(now);
    s2->flagged.insert(9);
    c.apply(s2);
    EXPECT_EQ(c.verdict_for(9, now), SanctionsCache::Verdict::Hit);
}

// --- PreTradeChecker integration (check 0c) -------------------------------------

TEST(SanctionsPreTrade, FlaggedRejectsSanctionsHit) {
    Instrument inst = make_instrument();
    OkAccounts accounts;
    FlatPositions positions;
    FlatMarket market;
    PreTradeChecker checker{RiskConfig{}};
    checker.bind_accounts(&accounts);
    checker.bind_positions(&positions);
    checker.bind_market(&market);
    SanctionsCache cache;
    const uint64_t now = steady_ns();
    auto s = verified(now);
    s->flagged.insert(7);
    cache.apply(s);
    checker.bind_sanctions(&cache);
    EXPECT_EQ(cache.verdict_for(7, now), SanctionsCache::Verdict::Hit);
    Order o = make_order(7);
    const CheckContext ctx{&inst, now};
    const RiskVerdict v = checker.check(o, ctx);
    ASSERT_FALSE(v.pass);
    ASSERT_NE(v.code, nullptr);
    EXPECT_STREQ(v.code, "SANCTIONS_HIT");
}

TEST(SanctionsPreTrade, UnverifiableRejectsUnavailable) {
    Instrument inst = make_instrument();
    OkAccounts accounts;
    PreTradeChecker checker{RiskConfig{}};
    checker.bind_accounts(&accounts);
    SanctionsCache cache;  // bound, never refreshed
    checker.bind_sanctions(&cache);
    Order o = make_order(7);
    const CheckContext ctx{&inst, steady_ns()};
    const RiskVerdict v = checker.check(o, ctx);
    ASSERT_FALSE(v.pass);
    EXPECT_STREQ(v.code, "SANCTIONS_SERVICE_UNAVAILABLE");
}

TEST(SanctionsPreTrade, ClearVerdictPassesPipeline) {
    Instrument inst = make_instrument();
    OkAccounts accounts;
    FlatPositions positions;
    FlatMarket market;
    PreTradeChecker checker{RiskConfig{}};
    checker.bind_accounts(&accounts);
    checker.bind_positions(&positions);
    checker.bind_market(&market);
    SanctionsCache cache;
    const uint64_t now = steady_ns();
    cache.apply(verified(now));  // verified, unflagged
    checker.bind_sanctions(&cache);
    Order o = make_order(7);
    const CheckContext ctx{&inst, now};
    const RiskVerdict v = checker.check(o, ctx);
    EXPECT_TRUE(v.pass) << (v.code ? v.code : "") << " "
                        << (v.detail ? v.detail : "");
}

TEST(SanctionsPreTrade, UnboundKeepsLegacyPath) {
    // No sanctions binding at all -> detached legacy behavior; the Go
    // admission gates own enforcement in that topology.
    PreTradeChecker checker{RiskConfig{}};
    Order o = make_order(7);
    EXPECT_EQ(checker.check(o), RiskDecision::ACCEPT);
}

TEST(SanctionsPreTrade, BoundCacheGatesDetachedPath) {
    SanctionsCache cache;  // bound but never refreshed -> fail closed
    PreTradeChecker checker{RiskConfig{}};
    checker.bind_sanctions(&cache);
    Order o = make_order(7);
    EXPECT_EQ(checker.check(o), RiskDecision::REJECT);
}

// --- live refresher (gated) ----------------------------------------------------

class SanctionsLive : public ::testing::Test {
protected:
    void SetUp() override {
        const char* env = std::getenv("EXC_REDIS_TEST_ADDR");
        if (env == nullptr) GTEST_SKIP() << "set EXC_REDIS_TEST_ADDR";
        const std::string addr(env);
        const auto colon = addr.rfind(':');
        cfg_.host = addr.substr(0, colon);
        cfg_.port = static_cast<uint16_t>(
            std::atoi(addr.substr(colon + 1).c_str()));
        cfg_.connect_timeout_ms = 1000;
        cfg_.io_timeout_ms = 1000;
        client_ = std::make_unique<exch::RespClient>(cfg_);
        ASSERT_TRUE(client_->connect()) << "redis unreachable";
    }
    void TearDown() override {
        if (!client_) return;
        RespValue v;
        (void)client_->execute(
            {"DEL", "exc:sanctions:flagged:42",
             "exc:sanctions:feed:heartbeat",
             "exc:sanctions:screened:bloom"},
            &v);
        client_->disconnect();
    }
    exch::RespClientConfig cfg_;
    std::unique_ptr<exch::RespClient> client_;
};

TEST_F(SanctionsLive, RefreshPublishesSnapshot) {
    SanctionsCache cache;
    SanctionsRefresher refresher(client_.get());
    RespValue v;
    ASSERT_TRUE(client_->execute(
        {"SET", "exc:sanctions:feed:heartbeat", "1700000000"}, &v));
    ASSERT_TRUE(client_->execute(
        {"SET", "exc:sanctions:flagged:42", "1"}, &v));
    ASSERT_TRUE(refresher.refresh(&cache));
    ASSERT_TRUE(cache.ready());
    EXPECT_EQ(cache.verdict_for(42, steady_ns()), SanctionsCache::Verdict::Hit);
    EXPECT_EQ(cache.verdict_for(7, steady_ns()), SanctionsCache::Verdict::Clear);
}

TEST_F(SanctionsLive, UnreachableClientMarksUnverifiable) {
    SanctionsCache cache;
    SanctionsRefresher refresher(nullptr);
    EXPECT_FALSE(refresher.refresh(&cache));
    EXPECT_EQ(cache.verdict_for(42, steady_ns()),
                 SanctionsCache::Verdict::Unverifiable);
}
