// Phase-3 Task 3.3.1 coverage — AccountStateCache field parser, fail-
// closed getters, the instrument ccy map, heartbeat-TTL liveness, and the
// PreTradeChecker integration (bound-but-unverifiable rejects; verified
// snapshot admits). Live refresher tests are gated on
// EXC_REDIS_TEST_ADDR (same convention as test_sanctions.cpp).

#include <gtest/gtest.h>

#include <cstdint>
#include <cstdio>
#include <memory>
#include <string>

#include "risk/AccountStateCache.hpp"
#include "risk/PreTradeChecker.hpp"
#include "redis/RespClient.hpp"
#include "utils/TimeUtils.hpp"

using namespace exch;

namespace {

// EUR/USD-alike instrument fixture (same shape as test_pretrade.cpp).
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

Order make_order() {
    Order o{};
    o.id = 1;
    o.account_id = 7;
    o.side = Side::BUY;
    o.type = OrderType::LIMIT;
    o.tif = TimeInForce::GTC;
    o.stp_mode = StpMode::CANCEL_NEWEST;
    o.flags = 0;
    o.price_ticks = 110'000'000;
    o.qty_units = 1'000'000'000;  // 10.0 units
    return o;
}

// A verified snapshot: instrument 1 = EUR/USD, account 7 ACTIVE/T2/ECP
// with 3 open positions, 1e7 USD + 1e7 EUR available, net +2.0 on instr 1.
std::shared_ptr<AccountStateCache::Snapshot> verified_snap() {
    auto s = std::make_shared<AccountStateCache::Snapshot>();
    s->heartbeat_unix = 1'700'000'000;
    // Fatal assertions are illegal in non-void helpers; keep the fixture
    // with expect-style checks (all inputs below are well-formed).
    EXPECT_TRUE(account_field_into("i:1", "EUR/USD", s.get()));
    EXPECT_TRUE(account_field_into("7", "ACTIVE,T2,ELIGIBLE_COUNTERPARTY,CANCEL_NEWEST,3",
                                   s.get()));
    EXPECT_TRUE(account_field_into("a:7:USD", "1000000000000000", s.get()));
    EXPECT_TRUE(account_field_into("a:7:EUR", "1000000000000000", s.get()));
    EXPECT_TRUE(account_field_into("p:7:1", "200000000", s.get()));
    return s;
}

}  // namespace

// --- field parser --------------------------------------------------------------

TEST(AccountField, AccountRecord) {
    AccountStateCache::Snapshot s;
    ASSERT_TRUE(account_field_into("42", "SUSPENDED,T1,PROFESSIONAL,NONE,9", &s));
    const auto& r = s.accounts.at(42);
    EXPECT_EQ(r.status, AccountStatus::SUSPENDED);
    EXPECT_EQ(r.kyc, KycTier::T1);
    EXPECT_EQ(r.category, ClientCategory::PROFESSIONAL);
    EXPECT_EQ(r.stp, static_cast<uint8_t>(StpMode::NONE));
    EXPECT_EQ(r.open_positions, 9u);
}

TEST(AccountField, AvailPosInstrHeartbeat) {
    AccountStateCache::Snapshot s;
    ASSERT_TRUE(account_field_into("a:9:EUR", "-500", &s));   // debit allowed?
    EXPECT_EQ(s.avail.at((9ull << 24) |
                        (uint64_t('E') << 16 | 'U' << 8 | 'R')),
              -500);
    ASSERT_TRUE(account_field_into("p:9:12", "-250000000", &s));
    EXPECT_EQ(s.pos.at((9ull << 32) | 12u), -250'000'000);
    ASSERT_TRUE(account_field_into("i:12", "USD/JPY", &s));
    EXPECT_EQ(s.instr.at(12).first, uint32_t('U' << 16 | 'S' << 8 | 'D'));
    EXPECT_EQ(s.instr.at(12).second, uint32_t('J' << 16 | 'P' << 8 | 'Y'));
    ASSERT_TRUE(account_field_into("__hb__", "1700000000", &s));
    EXPECT_EQ(s.heartbeat_unix, 1'700'000'000u);
}

TEST(AccountField, MalformedFieldsRejected) {
    AccountStateCache::Snapshot s;
    EXPECT_FALSE(account_field_into("42", "ACTIVE,T2", &s));          // 2 cols
    EXPECT_FALSE(account_field_into("42", "BOGUS,T2,RETAIL,NONE,0", &s));
    EXPECT_FALSE(account_field_into("a:x:EUR", "1", &s));             // bad id
    EXPECT_FALSE(account_field_into("a:9:EU", "1", &s));              // bad ccy
    EXPECT_FALSE(account_field_into("p:9:x", "1", &s));               // bad instr
    EXPECT_FALSE(account_field_into("i:x", "EUR/USD", &s));           // bad id
    EXPECT_FALSE(account_field_into("i:1", "EURUSD", &s));            // no slash
    EXPECT_FALSE(account_field_into("foo", "bar", &s));               // foreign
    EXPECT_TRUE(s.accounts.empty());
    EXPECT_TRUE(s.avail.empty());
}

// --- cache getters -------------------------------------------------------------

TEST(AccountStateCache, UnboundFailsClosed) {
    AccountStateCache c;
    EXPECT_FALSE(c.ready());
    EXPECT_EQ(c.status(7), AccountStatus::UNKNOWN);
    EXPECT_EQ(c.available_balance(7, 1, BalanceUnit::QUOTE), -1);
    EXPECT_EQ(c.open_position_count(7),
              std::numeric_limits<uint32_t>::max());
    EXPECT_EQ(static_cast<uint8_t>(c.default_stp_mode(7)), kStpModeUnset);
    EXPECT_EQ(c.client_category(7), ClientCategory::RETAIL);
    EXPECT_EQ(c.kyc_tier(7), KycTier::T0);
    EXPECT_EQ(c.net_position_units(7, 1), 0);
}

TEST(AccountStateCache, UnverifiableFailsClosed) {
    AccountStateCache c;
    c.apply(verified_snap());
    c.mark_unverifiable();
    EXPECT_EQ(c.status(7), AccountStatus::UNKNOWN);
    EXPECT_EQ(c.available_balance(7, 1, BalanceUnit::QUOTE), -1);
    EXPECT_EQ(c.net_position_units(7, 1), 0);
}

TEST(AccountStateCache, VerifiedSnapshotReads) {
    AccountStateCache c;
    c.apply(verified_snap());
    EXPECT_TRUE(c.ready());
    EXPECT_EQ(c.status(7), AccountStatus::ACTIVE);
    EXPECT_EQ(c.kyc_tier(7), KycTier::T2);
    EXPECT_EQ(c.client_category(7), ClientCategory::ELIGIBLE_COUNTERPARTY);
    EXPECT_EQ(c.default_stp_mode(7), StpMode::CANCEL_NEWEST);
    EXPECT_EQ(c.open_position_count(7), 3u);
    // BUY debits quote (USD), SELL debits base (EUR) — resolved via i:1.
    EXPECT_EQ(c.available_balance(7, 1, BalanceUnit::QUOTE),
              1'000'000'000'000'000);
    EXPECT_EQ(c.available_balance(7, 1, BalanceUnit::BASE),
              1'000'000'000'000'000);
    EXPECT_EQ(c.net_position_units(7, 1), 200'000'000);
    // Unknown account / instrument / bucket fail closed.
    EXPECT_EQ(c.status(8), AccountStatus::UNKNOWN);
    EXPECT_EQ(c.available_balance(7, 99, BalanceUnit::QUOTE), -1);  // no i:99
    EXPECT_EQ(c.available_balance(7, 1, BalanceUnit::BASE), // a:7:GBP absent
              1'000'000'000'000'000);
    EXPECT_EQ(c.available_balance(8, 1, BalanceUnit::QUOTE), -1);   // a:8:USD
    EXPECT_EQ(c.net_position_units(8, 1), 0);
}

// --- refresher (in-process) ----------------------------------------------------

TEST(AccountStateRefresher, NullClientMarksUnverifiable) {
    AccountStateCache c;
    AccountStateRefresher r(nullptr);
    EXPECT_FALSE(r.refresh(&c));
    EXPECT_TRUE(c.ready());  // bound-but-poisoned
    EXPECT_EQ(c.status(7), AccountStatus::UNKNOWN);
}

// --- checker integration --------------------------------------------------------

TEST(AccountStateCheck, UnverifiableRejectsAll) {
    AccountStateCache c;
    c.mark_unverifiable();
    RiskConfig cfg{};
    cfg.required_kyc_tier = KycTier::T0;
    PreTradeChecker chk{cfg};
    chk.bind_accounts(&c);
    chk.bind_positions(&c);
    Instrument inst = make_instrument();
    Order o = make_order();
    const CheckContext ctx{&inst, now_ns()};
    const RiskVerdict v = chk.check(o, ctx);
    EXPECT_FALSE(v.pass);
    EXPECT_STREQ(v.code, "ACCOUNT_INACTIVE");
}

TEST(AccountStateCheck, VerifiedAdmits) {
    AccountStateCache c;
    c.apply(verified_snap());
    RiskConfig cfg{};
    cfg.required_kyc_tier = KycTier::T0;
    cfg.order_rate_per_sec = 1000;   // don't trip the collar in one test
    cfg.order_rate_burst = 1000;
    PreTradeChecker chk{cfg};
    chk.bind_accounts(&c);
    chk.bind_positions(&c);
    Instrument inst = make_instrument();
    Order o = make_order();
    const CheckContext ctx{&inst, now_ns()};
    const RiskVerdict v = chk.check(o, ctx);
    EXPECT_TRUE(v.pass) << (v.detail != nullptr ? v.detail : "");
}

TEST(AccountStateCheck, SuspendedAndInsufficientReject) {
    AccountStateCache c;
    auto s = verified_snap();
    ASSERT_TRUE(account_field_into("8", "SUSPENDED,T2,RETAIL,CANCEL_NEWEST,0", s.get()));
    ASSERT_TRUE(account_field_into("a:8:USD", "1", s.get()));
    c.apply(s);

    RiskConfig cfg{};
    cfg.order_rate_per_sec = 1000;
    cfg.order_rate_burst = 1000;
    PreTradeChecker chk{cfg};
    chk.bind_accounts(&c);
    chk.bind_positions(&c);
    Instrument inst = make_instrument();
    Order o = make_order();
    const CheckContext ctx{&inst, now_ns()};

    o.account_id = 8;
    RiskVerdict v = chk.check(o, ctx);
    EXPECT_FALSE(v.pass);
    EXPECT_STREQ(v.code, "ACCOUNT_SUSPENDED");

    // Known account, empty balance bucket -> insufficient. Snapshots are
    // immutable once applied, so build the successor first.
    auto s2 = std::make_shared<AccountStateCache::Snapshot>(*s);
    ASSERT_TRUE(account_field_into("9", "ACTIVE,T2,RETAIL,CANCEL_NEWEST,0", s2.get()));
    c.apply(s2);
    o.account_id = 9;
    v = chk.check(o, ctx);
    EXPECT_FALSE(v.pass);
    EXPECT_STREQ(v.code, "INSUFFICIENT_BALANCE");
}

// --- live refresher (gated) -----------------------------------------------------

class AccountStateLive : public ::testing::Test {
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
        (void)client_->execute({"DEL", "account:state"}, &v);
        client_->disconnect();
    }
    exch::RespClientConfig cfg_;
    std::unique_ptr<exch::RespClient> client_;
};

TEST_F(AccountStateLive, RefreshAppliesSnapshot) {
    RespValue v;
    const auto hb = std::to_string(std::time(nullptr));
    ASSERT_TRUE(client_->execute(
        {"HSET", "account:state", "__hb__", hb.c_str(), "7",
         "ACTIVE,T2,RETAIL,-,2", "a:7:USD", "500000000", "i:1",
         "EUR/USD"}, &v));
    AccountStateCache c;
    AccountStateRefresher r(client_.get());
    ASSERT_TRUE(r.refresh(&c));
    EXPECT_EQ(c.status(7), AccountStatus::ACTIVE);
    EXPECT_EQ(c.available_balance(7, 1, BalanceUnit::QUOTE), 500'000'000);
    EXPECT_EQ(c.default_stp_mode(7), static_cast<StpMode>(kStpModeUnset));
}

TEST_F(AccountStateLive, StaleHeartbeatMarksUnverifiable) {
    RespValue v;
    ASSERT_TRUE(client_->execute(
        {"HSET", "account:state", "__hb__", "1", "7",
         "ACTIVE,T2,RETAIL,-,0"}, &v));
    AccountStateCache c;
    AccountStateRefresher r(client_.get(), /*max_hb_age_s=*/30);
    EXPECT_FALSE(r.refresh(&c));
    EXPECT_EQ(c.status(7), AccountStatus::UNKNOWN);
}

// Phase-3 Task 5 — c:{account} credit-party field (spec §3.3b IPartyMap).
TEST(AccountField, CreditPartyField) {
    auto s = std::make_shared<AccountStateCache::Snapshot>();
    // c: arriving BEFORE the account record — HGETALL order is
    // unspecified; the record must not clobber the party index.
    EXPECT_TRUE(account_field_into("c:7", "3", s.get()));
    EXPECT_TRUE(account_field_into(
        "7", "ACTIVE,T2,ELIGIBLE_COUNTERPARTY,CANCEL_NEWEST,0", s.get()));
    EXPECT_TRUE(account_field_into(
        "9", "ACTIVE,T1,RETAIL,CANCEL_NEWEST,0", s.get()));  // no c: field
    EXPECT_FALSE(account_field_into("c:x", "1", s.get()));   // bad id
    EXPECT_FALSE(account_field_into("c:7", "abc", s.get())); // bad index

    AccountStateCache c;
    c.apply(s);
    EXPECT_EQ(c.credit_party_id(7), 3u);                     // screened
    EXPECT_EQ(c.credit_party_id(9), kCreditMaxParties);      // unscreened
    EXPECT_EQ(c.credit_party_id(999), kCreditMaxParties);    // absent
}
