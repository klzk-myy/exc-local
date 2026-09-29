// Task 2.3.3 + 2.3.9 coverage — all 14 in-process pre-trade checks
// (spec §3.3): per-check pass/fail with exact codes, cheapest-first
// short-circuit ordering proof, per-account collar burst/refill, band edge
// cases, execution-flag semantics, STP resolution/gating, the optional §3.3b
// credit screen, and the <10µs-total p99 budget.

#include <gtest/gtest.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <string>
#include <vector>

#include <fcntl.h>
#include <sys/mman.h>
#include <unistd.h>

#include "risk/PreTradeChecker.hpp"
#include "utils/TimeUtils.hpp"

using namespace exch;

namespace {

// --- Instrument fixture: EUR/USD-alike --------------------------------------
// tick 0.00001 (1'000 ticks), lot 0.01 units (1e6), qty floor 0.01,
// qty cap 10'000.0 (1e12), notional floor 0.1 quote unit (1e7), bands
// +2.00%/-5.00%, last price 1.10 -> 110'000'000 ticks.
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
    i.price_band_pct_up = 200;    // 2.00%
    i.price_band_pct_down = 500;  // 5.00%
    i.last_price_ticks = 110'000'000;  // 1.10
    return i;
}

// Valid order: LIMIT BUY 10.0 units @1.10 (notional 11.0 quote units).
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
    o.qty_units = 1'000'000'000;  // 10.0 units at 1e8 scale
    return o;
}

// --- Mocks (instrumented for the ordering proof) -----------------------------
// Provider calls append their check number to `log`; a rejected order must
// leave the log without entries from later checks.

struct MockAccounts final : IAccountState {
    AccountStatus st = AccountStatus::ACTIVE;
    int64_t quote_avail = 1'000'000'000'000'000;  // 1e7 units
    int64_t base_avail = 1'000'000'000'000'000;
    uint32_t open_positions = 0;
    StpMode stp_def = static_cast<StpMode>(kStpModeUnset);
    ClientCategory cat = ClientCategory::RETAIL;
    KycTier tier = KycTier::T2;
    std::vector<int>* log = nullptr;

    AccountStatus status(uint64_t) const noexcept override {
        if (log) log->push_back(1);
        return st;
    }
    int64_t available_balance(uint64_t, uint64_t,
                              BalanceUnit u) const noexcept override {
        if (log) log->push_back(u == BalanceUnit::QUOTE ? 3 : -3);
        return u == BalanceUnit::QUOTE ? quote_avail : base_avail;
    }
    uint32_t open_position_count(uint64_t) const noexcept override {
        if (log) log->push_back(4);
        return open_positions;
    }
    StpMode default_stp_mode(uint64_t) const noexcept override {
        if (log) log->push_back(14);
        return stp_def;
    }
    ClientCategory client_category(uint64_t) const noexcept override {
        if (log) log->push_back(114);
        return cat;
    }
    KycTier kyc_tier(uint64_t) const noexcept override {
        if (log) log->push_back(10);
        return tier;
    }
};

struct MockPositions final : IPositionState {
    int64_t pos = 0;
    std::vector<int>* log = nullptr;
    int64_t net_position_units(uint64_t, uint64_t) const noexcept override {
        if (log) log->push_back(13);
        return pos;
    }
};

struct MockMarket final : IMarketState {
    int64_t last = 0;   // 0 -> checker falls back to instrument.last_price
    int64_t ago = 0;
    int64_t bid = 0;
    int64_t ask = 0;
    std::vector<int>* log = nullptr;
    int64_t last_price_ticks(uint64_t) const noexcept override {
        if (log) log->push_back(6);
        return last;
    }
    int64_t price_ticks_ago(uint64_t, uint64_t,
                            uint64_t) const noexcept override {
        if (log) log->push_back(9);
        return ago;
    }
    int64_t best_bid_ticks(uint64_t) const noexcept override {
        if (log) log->push_back(-13);
        return bid;
    }
    int64_t best_ask_ticks(uint64_t) const noexcept override {
        if (log) log->push_back(-13);
        return ask;
    }
};

struct MockMargin final : IMarginCheck {
    bool result = true;
    mutable uint32_t calls = 0;
    bool passes(const Order&, const Instrument&, const IAccountState&,
                int64_t) const noexcept override {
        ++calls;
        return result;
    }
};

struct MockPartyMap final : IPartyMap {
    uint32_t party = kCreditMaxParties;  // unmapped by default
    uint32_t credit_party_id(uint64_t) const noexcept override {
        return party;
    }
};

struct Fixture {
    Instrument inst = make_instrument();
    MockAccounts accounts;
    MockPositions positions;
    MockMarket market;
    PreTradeChecker checker{RiskConfig{}};
    Order order = make_order();
    CheckContext ctx{&inst, 1'700'000'000'000'000'000ULL};

    Fixture() {
        checker.bind_accounts(&accounts);
        checker.bind_positions(&positions);
        checker.bind_market(&market);
    }
};

void expect_reject(const RiskVerdict& v, const char* code) {
    ASSERT_FALSE(v.pass);
    ASSERT_NE(v.code, nullptr);
    EXPECT_STREQ(v.code, code);
    EXPECT_NE(v.detail, nullptr);
}

}  // namespace

// --- happy path + legacy shim ------------------------------------------------

TEST(PreTrade, HappyPathPasses) {
    Fixture f;
    const RiskVerdict v = f.checker.check(f.order, f.ctx);
    EXPECT_TRUE(v.pass) << (v.code ? v.code : "") << " "
                        << (v.detail ? v.detail : "");
    EXPECT_EQ(v.code, nullptr);
    // stp_mode arrived explicit — unchanged.
    EXPECT_EQ(f.order.stp_mode, StpMode::CANCEL_NEWEST);
}

TEST(PreTrade, DetachedLegacyShimAccepts) {
    // Stub-era contract kept for test_matching/main.cpp callers.
    PreTradeChecker detached;
    Order o{};
    o.type = OrderType::LIMIT;
    EXPECT_EQ(detached.check(o), RiskDecision::ACCEPT);
    EXPECT_FALSE(detached.configured());
}

TEST(PreTrade, LegacyShimRunsPipelineWhenBound) {
    Fixture f;
    Instrument inst = make_instrument();
    f.checker.bind_instrument(&inst);
    f.accounts.st = AccountStatus::SUSPENDED;
    Order o = make_order();
    EXPECT_EQ(f.checker.check(o), RiskDecision::REJECT);
}

// ---- 1. account status --------------------------------------------------------

TEST(PreTrade, AccountStatusRejects) {
    Fixture f;
    f.accounts.st = AccountStatus::SUSPENDED;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeAccountSuspended);
    f.accounts.st = AccountStatus::FROZEN;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeAccountFrozen);
    f.accounts.st = AccountStatus::CLOSED;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeAccountInactive);
    f.accounts.st = AccountStatus::UNKNOWN;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeAccountInactive);
    // account_id 0 is never queried — forced inactive.
    f.accounts.st = AccountStatus::ACTIVE;
    f.order.account_id = 0;
    std::vector<int> log;
    f.accounts.log = &log;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeAccountInactive);
    EXPECT_TRUE(log.empty());
}

// Ordering proof: a check-1 failure must short-circuit before ANY later
// provider call — the mock logs every touch.
TEST(PreTrade, Check1ShortCircuitsCheck14) {
    Fixture f;
    std::vector<int> log;
    f.accounts.log = &log;
    f.positions.log = &log;
    f.market.log = &log;
    f.accounts.st = AccountStatus::SUSPENDED;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeAccountSuspended);
    ASSERT_EQ(log.size(), 1u);
    EXPECT_EQ(log[0], 1);  // only status() ran — no balance/collar/STP
}

// And the positive-ordering proof: a passing order consults providers in
// contract order (1 status -> 3 balance -> 4 positions -> 10 kyc -> 14 stp).
TEST(PreTrade, ProviderOrderOnPass) {
    Fixture f;
    std::vector<int> log;
    f.accounts.log = &log;
    f.positions.log = &log;
    f.market.log = &log;
    ASSERT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // Extract the account-side ordering marks in call order.
    std::vector<int> marks;
    for (int c : log) {
        if (c == 1 || c == 3 || c == 4 || c == 10 || c == 14 || c == 114)
            marks.push_back(c);
    }
    // status, balance(quote), open_positions, [margin balance], kyc,
    // default_stp — in that order. (client_category is deliberately absent:
    // it is only consulted when the resolved STP mode is NONE.)
    ASSERT_GE(marks.size(), 6u);
    EXPECT_EQ(marks.front(), 1);
    EXPECT_EQ(marks[1], 3);
    EXPECT_EQ(marks[2], 4);
    const auto it10 = std::find(marks.begin(), marks.end(), 10);
    const auto it14 = std::find(marks.begin(), marks.end(), 14);
    ASSERT_TRUE(it10 != marks.end() && it14 != marks.end());
    EXPECT_LT(it10, it14);   // kyc (10) before stp (14)
    EXPECT_EQ(marks.back(), 14);  // stp resolution is the last provider read
}

// ---- 2. instrument status ------------------------------------------------------

TEST(PreTrade, InstrumentStatusRejects) {
    Fixture f;
    auto with_status = [&](InstrumentStatus s) {
        Instrument i = make_instrument();
        i.status = s;
        CheckContext c{&i, f.ctx.now_ns};
        return f.checker.check(f.order, c);
    };
    EXPECT_TRUE(with_status(InstrumentStatus::ACTIVE).pass);
    expect_reject(with_status(InstrumentStatus::SUSPENDED),
                  kCodeInstrumentSuspended);
    expect_reject(with_status(InstrumentStatus::HALTED),
                  kCodeInstrumentHalted);
    expect_reject(with_status(InstrumentStatus::CANCEL_ONLY),
                  kCodeInstrumentCancelOnly);
    expect_reject(with_status(InstrumentStatus::DELISTED),
                  kCodeInstrumentDelisted);
    expect_reject(with_status(InstrumentStatus::DRAFT),
                  kCodeInstrumentInactive);

    // RESTRICTED (§7.1): limit orders pass, market orders reject.
    {
        Instrument i = make_instrument();
        i.status = InstrumentStatus::RESTRICTED;
        CheckContext c{&i, f.ctx.now_ns};
        EXPECT_TRUE(f.checker.check(f.order, c).pass);
        Order mkt = make_order();
        mkt.type = OrderType::MARKET;
        mkt.price_ticks = 0;
        expect_reject(f.checker.check(mkt, c), kCodeInstrumentRestricted);
    }
    // DELISTED (§7.1 + remediation #35): reduce_only still admitted so the
    // 30d close-only window can drain positions.
    {
        Instrument i = make_instrument();
        i.status = InstrumentStatus::DELISTED;
        CheckContext c{&i, f.ctx.now_ns};
        Order ro = make_order();
        ro.flags = kOrderFlagReduceOnly;
        ro.side = Side::SELL;
        f.positions.pos = 2'000'000'000;  // long 20 units to reduce
        EXPECT_TRUE(f.checker.check(ro, c).pass);
    }
    // No instrument bound -> fail closed.
    {
        CheckContext c{nullptr, f.ctx.now_ns};
        expect_reject(f.checker.check(f.order, c), kCodeInstrumentInactive);
    }
}

// ---- 3. balance -----------------------------------------------------------------

TEST(PreTrade, BalanceCheck) {
    Fixture f;
    // Order needs notional = 10 * 1.10 = 11.0 quote units = 1.1e9.
    f.accounts.quote_avail = 1'100'000'000;   // exactly enough
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    f.accounts.quote_avail = 1'099'999'999;   // one tick short
    expect_reject(f.checker.check(f.order, f.ctx), kCodeInsufficientBalance);

    // SELL debits BASE units, not quote.
    f.accounts.quote_avail = 1'000'000'000'000'000;
    f.accounts.base_avail = 500'000'000;  // 5 units < 10 required
    f.order.side = Side::SELL;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeInsufficientBalance);
    f.accounts.base_avail = 1'000'000'000;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);

    // Provider "unknown" (<0) fails closed.
    f.accounts.base_avail = -1;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeInsufficientBalance);
}

// ---- 4. position limit ------------------------------------------------------------

TEST(PreTrade, PositionLimit) {
    Fixture f;
    f.accounts.open_positions = 500;  // cfg default max
    expect_reject(f.checker.check(f.order, f.ctx), kCodePositionLimitExceeded);
    f.accounts.open_positions = 499;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // reduce_only bypasses the cap (it only shrinks exposure).
    f.accounts.open_positions = 500;
    f.order.side = Side::SELL;
    f.order.flags = kOrderFlagReduceOnly;
    f.positions.pos = 2'000'000'000;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
}

// ---- 5. per-account collar ---------------------------------------------------------

TEST(PreTrade, CollarRejects51stAndRefills) {
    Fixture f;
    const uint64_t t0 = 1'700'000'000'000'000'000ULL;
    for (int i = 0; i < 50; ++i) {
        f.ctx.now_ns = t0 + i;  // same instant — no refill
        ASSERT_TRUE(f.checker.check(f.order, f.ctx).pass) << "order " << i;
    }
    f.ctx.now_ns = t0 + 50;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeRateLimitExceeded);
    // One token refills every 20ms at 50/s. +10ms: ~0.5 token — still closed.
    f.ctx.now_ns = t0 + 50 + 10'000'000ULL;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeRateLimitExceeded);
    // +20ms since the insert stamp: exactly 1 token accrued — one pass.
    f.ctx.now_ns = t0 + 50 + 20'000'000ULL;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    f.ctx.now_ns = t0 + 50 + 20'000'001ULL;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeRateLimitExceeded);
    // A quiet minute refills to the 50-token cap — full burst again.
    f.ctx.now_ns = t0 + 61'000'000'000ULL;
    for (int i = 0; i < 50; ++i) {
        ASSERT_TRUE(f.checker.check(f.order, f.ctx).pass) << "refill " << i;
    }
    expect_reject(f.checker.check(f.order, f.ctx), kCodeRateLimitExceeded);
}

TEST(PreTrade, CollarIsPerAccount) {
    Fixture f;
    const uint64_t t0 = 1'700'000'000'000'000'000ULL;
    for (int i = 0; i < 50; ++i) {
        f.ctx.now_ns = t0 + i;
        ASSERT_TRUE(f.checker.check(f.order, f.ctx).pass);
    }
    Order other = make_order();
    other.account_id = 8;
    EXPECT_TRUE(f.checker.check(other, f.ctx).pass);
    // And the original account is still capped.
    expect_reject(f.checker.check(f.order, f.ctx), kCodeRateLimitExceeded);
}

TEST(PreTrade, CollarResetEvicts) {
    Fixture f;
    const uint64_t t0 = 1'700'000'000'000'000'000ULL;
    for (int i = 0; i < 50; ++i) {
        f.ctx.now_ns = t0 + i;
        ASSERT_TRUE(f.checker.check(f.order, f.ctx).pass);
    }
    f.checker.reset_account_collar(7);
    // Evicted -> fresh full burst for the same account.
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
}

// ---- 6. price band -------------------------------------------------------------------

TEST(PreTrade, PriceBand) {
    Fixture f;
    // last = 1.10 (110'000'000); band +2% => hi 112'200'000, -5% => lo
    // 104'500'000 (exact: 110e6*10200/10000, 110e6*9500/10000).
    f.order.price_ticks = 112'200'000;      // exactly at +2% boundary
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    f.order.price_ticks = 112'200'001;      // one tick above band
    expect_reject(f.checker.check(f.order, f.ctx), kCodePriceOutOfBand);
    f.order.price_ticks = 104'500'000;      // exactly at -5% boundary
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    f.order.price_ticks = 104'499'000;      // just below (tick-aligned)
    expect_reject(f.checker.check(f.order, f.ctx), kCodePriceOutOfBand);

    // First trade / no last price anywhere -> band cannot fire, order passes.
    f.market.last = 0;
    Instrument bare = make_instrument();
    bare.last_price_ticks = 0;
    CheckContext c{&bare, f.ctx.now_ns};
    f.order.price_ticks = 200'000'000;
    EXPECT_TRUE(f.checker.check(f.order, c).pass);

    // Provider-supplied last price wins over the instrument field.
    f.market.last = 100'000'000;  // 1.00 -> band [95e6, 102e6]
    c.instrument = &f.inst;
    f.order.price_ticks = 110'000'000;
    expect_reject(f.checker.check(f.order, c), kCodePriceOutOfBand);

    // Market-type order (price_ticks == 0) skips the band entirely.
    f.order.type = OrderType::MARKET;
    f.order.price_ticks = 0;
    EXPECT_TRUE(f.checker.check(f.order, c).pass);
}

// ---- 7. qty bounds ---------------------------------------------------------------------

TEST(PreTrade, QtyBounds) {
    Fixture f;
    f.order.qty_units = 1'000'000'000'001;  // over 10'000-unit cap
    expect_reject(f.checker.check(f.order, f.ctx), kCodeOrderQtyExceedsMax);
    f.order.qty_units = 500'000;            // below 0.01-unit floor
    expect_reject(f.checker.check(f.order, f.ctx), kCodeOrderQtyBelowMin);
    f.order.qty_units = 0;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeOrderQtyBelowMin);
    f.order.qty_units = 1'000'000'000;      // restore
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
}

// ---- 8. margin stub + Phase-19 seam ------------------------------------------------------

TEST(PreTrade, MarginStub) {
    Fixture f;
    // A BUY can never fail the stub after check 3 passed (full notional >
    // 3.34% requirement by construction) — the placeholder only bites for
    // SELLs, where check 3 debits BASE but margin still reads QUOTE.
    f.order.side = Side::SELL;
    f.accounts.base_avail = 1'000'000'000;      // covers the 10-unit sale
    // Stub: quote balance >= notional * 3.34% = 1.1e9 * 0.0334 = 36'740'000.
    f.accounts.quote_avail = 36'740'000;        // exactly at requirement
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    f.accounts.quote_avail = 36'739'999;        // one tick short
    expect_reject(f.checker.check(f.order, f.ctx), kCodeMarginCheckFailed);
    // Unknown balance fails closed.
    f.accounts.quote_avail = -1;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeMarginCheckFailed);
}

TEST(PreTrade, MarginHookSeamReplacesStub) {
    Fixture f;
    f.order.side = Side::SELL;
    f.accounts.base_avail = 1'000'000'000;
    f.accounts.quote_avail = 0;  // stub math would fail; hook overrides
    MockMargin hook;
    f.checker.bind_margin(&hook);
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    EXPECT_EQ(hook.calls, 1u);
    hook.result = false;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeMarginCheckFailed);
}

// ---- 9. circuit-breaker placeholder --------------------------------------------------------

TEST(PreTrade, CircuitBreakerStub) {
    Fixture f;
    // Order price must stay inside the check-6 band around `last` so the
    // breaker rejection is the one observed — pin price == last each time.
    f.market.last = 104'000'000;
    f.order.price_ticks = 104'000'000;
    // +5.77% move over the 60s window -> OPEN.
    f.market.ago = 98'000'000;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeCircuitBreakerOpen);
    // Exactly +5.00% -> at the threshold, passes (rejects only beyond).
    f.market.last = 105'000'000;
    f.order.price_ticks = 105'000'000;
    f.market.ago = 100'000'000;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // No history -> placeholder can't fire.
    f.market.ago = 0;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // Downward move symmetric: 100 -> 94 is -6%.
    f.market.last = 94'000'000;
    f.order.price_ticks = 94'000'000;
    f.market.ago = 100'000'000;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeCircuitBreakerOpen);
}

// ---- 10. KYC tier stub ------------------------------------------------------------------------

TEST(PreTrade, KycTierStub) {
    Fixture f;
    // Default required=T0 -> stub gates nothing (all accounts pass).
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // Raise the bar: T1 required, account T0 -> reject; T2 -> pass.
    RiskConfig cfg;
    cfg.required_kyc_tier = KycTier::T1;
    PreTradeChecker strict(cfg);
    strict.bind_accounts(&f.accounts);
    strict.bind_positions(&f.positions);
    strict.bind_market(&f.market);
    f.accounts.tier = KycTier::T0;
    expect_reject(strict.check(f.order, f.ctx), kCodeKycTierInsufficient);
    f.accounts.tier = KycTier::T2;
    EXPECT_TRUE(strict.check(f.order, f.ctx).pass);
}

// ---- 11. tick/lot --------------------------------------------------------------------------------

TEST(PreTrade, TickLot) {
    Fixture f;
    f.order.price_ticks = 110'000'500;  // not a 1'000-tick multiple
    expect_reject(f.checker.check(f.order, f.ctx), kCodeTickSizeViolation);
    f.order.price_ticks = 110'000'000;
    f.order.qty_units = 1'000'000'500;  // not a 1e6-unit multiple
    expect_reject(f.checker.check(f.order, f.ctx), kCodeLotSizeViolation);
    f.order.qty_units = 1'000'000'000;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // Negative price is malformed outright.
    f.order.price_ticks = -110'000'000;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeInvalidRequest);
}

// ---- 12. min notional --------------------------------------------------------------------------------

TEST(PreTrade, MinNotional) {
    Fixture f;
    // min_notional = 1e7 (0.1 quote unit). qty 0.05 units @1.10 = 5.5e6.
    f.order.qty_units = 5'000'000;   // 0.05 (lot-aligned: 5 * 1e6)
    expect_reject(f.checker.check(f.order, f.ctx), kCodeMinNotionalViolation);
    f.order.qty_units = 10'000'000;  // 0.1 -> notional 1.1e7 >= 1e7
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // min_notional 0 = no floor.
    Instrument wide = make_instrument();
    wide.min_notional_units = 0;
    CheckContext c{&wide, f.ctx.now_ns};
    f.order.qty_units = 1'000'000;
    EXPECT_TRUE(f.checker.check(f.order, c).pass);
}

// ---- 13. execution flags ---------------------------------------------------------------------------------

TEST(PreTrade, PostOnlyMarketable) {
    Fixture f;
    f.market.ask = 110'000'000;
    f.order.flags = kOrderFlagPostOnly;
    // BUY at best ask -> would cross -> reject.
    expect_reject(f.checker.check(f.order, f.ctx), kCodePostOnlyViolation);
    // One tick below the ask -> rests -> pass.
    f.order.price_ticks = 109'999'000;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // SELL at best bid -> crosses.
    f.market.bid = 110'000'000;
    f.order.side = Side::SELL;
    f.order.price_ticks = 110'000'000;
    expect_reject(f.checker.check(f.order, f.ctx), kCodePostOnlyViolation);
    // SELL above bid -> rests.
    f.order.price_ticks = 110'001'000;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // post_only MARKET can never rest -> always marketable.
    Order mkt = make_order();
    mkt.type = OrderType::MARKET;
    mkt.price_ticks = 0;
    mkt.flags = kOrderFlagPostOnly;
    expect_reject(f.checker.check(mkt, f.ctx), kCodePostOnlyViolation);
    // Empty opposite side (ask == 0) -> nothing to cross -> pass.
    f.market.ask = 0;
    Order o2 = make_order();
    o2.flags = kOrderFlagPostOnly;
    EXPECT_TRUE(f.checker.check(o2, f.ctx).pass);
}

TEST(PreTrade, ReduceOnlyNeedsOppositePosition) {
    Fixture f;
    f.order.flags = kOrderFlagReduceOnly;
    f.order.side = Side::SELL;
    f.positions.pos = 0;  // flat -> reject
    expect_reject(f.checker.check(f.order, f.ctx), kCodeReduceOnlyViolation);
    // SELL reduce_only into a LONG position -> pass.
    f.positions.pos = 2'000'000'000;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // SELL reduce_only while SHORT would increase exposure -> reject.
    f.positions.pos = -2'000'000'000;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeReduceOnlyViolation);
    // BUY reduce_only while SHORT (pos is still -2e9) -> pass; while LONG
    // it would increase exposure -> reject.
    f.order.side = Side::BUY;
    f.order.price_ticks = 108'000'000;  // inside the +/-band around 110e6
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    f.positions.pos = 2'000'000'000;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeReduceOnlyViolation);
    f.positions.pos = -2'000'000'000;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // No position provider bound -> fail closed (treated as flat).
    Fixture bare;
    bare.positions.pos = 0;
    PreTradeChecker c2;
    c2.bind_accounts(&bare.accounts);
    c2.bind_market(&bare.market);  // positions deliberately unbound
    Order ro = make_order();
    ro.flags = kOrderFlagReduceOnly;
    expect_reject(c2.check(ro, bare.ctx), kCodeReduceOnlyViolation);
}

// ---- 14. STP validation + resolution + NONE gating ------------------------------------------------------

TEST(PreTrade, StpModeResolution) {
    // Free-function contract (Task 2.3.21): order -> account -> CANCEL_NEWEST.
    EXPECT_EQ(resolve_stp_mode(StpMode::CANCEL_BOTH, StpMode::DECREMENT),
              StpMode::CANCEL_BOTH);
    EXPECT_EQ(resolve_stp_mode(static_cast<StpMode>(kStpModeUnset),
                               StpMode::DECREMENT),
              StpMode::DECREMENT);
    EXPECT_EQ(resolve_stp_mode(static_cast<StpMode>(kStpModeUnset),
                               static_cast<StpMode>(kStpModeUnset)),
              StpMode::CANCEL_NEWEST);
    // Malformed value (not the sentinel, not in the enum) -> CANCEL_NEWEST
    // from the resolver, but the checker rejects it before it can resolve.
    EXPECT_EQ(resolve_stp_mode(static_cast<StpMode>(7),
                               StpMode::DECREMENT),
              StpMode::CANCEL_NEWEST);
}

TEST(PreTrade, StpUnsetResolvesAccountDefaultAndStamps) {
    Fixture f;
    f.accounts.stp_def = StpMode::DECREMENT;
    f.order.stp_mode = static_cast<StpMode>(kStpModeUnset);
    ASSERT_TRUE(f.checker.check(f.order, f.ctx).pass);
    EXPECT_EQ(f.order.stp_mode, StpMode::DECREMENT);  // stamped
    // Explicit order value always wins.
    f.order.stp_mode = StpMode::CANCEL_BOTH;
    ASSERT_TRUE(f.checker.check(f.order, f.ctx).pass);
    EXPECT_EQ(f.order.stp_mode, StpMode::CANCEL_BOTH);
    // Account default malformed/unset -> CANCEL_NEWEST.
    f.accounts.stp_def = static_cast<StpMode>(kStpModeUnset);
    f.order.stp_mode = static_cast<StpMode>(kStpModeUnset);
    ASSERT_TRUE(f.checker.check(f.order, f.ctx).pass);
    EXPECT_EQ(f.order.stp_mode, StpMode::CANCEL_NEWEST);
    // resolved_stp() exposes the same rule for the engine.
    f.order.stp_mode = static_cast<StpMode>(kStpModeUnset);
    f.accounts.stp_def = StpMode::CANCEL_OLDEST;
    EXPECT_EQ(f.checker.resolved_stp(f.order), StpMode::CANCEL_OLDEST);
}

TEST(PreTrade, StpUnknownValueRejected) {
    Fixture f;
    f.order.stp_mode = static_cast<StpMode>(7);  // outside enum, not sentinel
    expect_reject(f.checker.check(f.order, f.ctx), kCodeInvalidRequest);
}

TEST(PreTrade, StpNoneGatedToProfessionalEcp) {
    Fixture f;
    f.order.stp_mode = StpMode::NONE;
    f.accounts.cat = ClientCategory::RETAIL;
    expect_reject(f.checker.check(f.order, f.ctx), kCodeStpNoneNotPermitted);
    f.accounts.cat = ClientCategory::PROFESSIONAL;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    f.accounts.cat = ClientCategory::ELIGIBLE_COUNTERPARTY;
    EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);
    // NONE arriving via account default is gated identically.
    f.accounts.cat = ClientCategory::RETAIL;
    f.accounts.stp_def = StpMode::NONE;
    f.order.stp_mode = static_cast<StpMode>(kStpModeUnset);
    expect_reject(f.checker.check(f.order, f.ctx), kCodeStpNoneNotPermitted);
}

// ---- optional bilateral-credit admission screen (spec §3.3b) -------------------------------------------

TEST(PreTrade, CreditScreenOptional) {
    Fixture f;
    const std::string name =
        "exch_pretrade_credit_" + std::to_string(::getpid());
    const std::string path = "/" + name;
    ::shm_unlink(path.c_str());
    {
        BilateralCreditMatrix m;
        ASSERT_TRUE(m.open(name, /*create=*/true));
        MockPartyMap pmap;
        pmap.party = 3;
        f.checker.bind_credit(&m, &pmap);

        // Zero-initialized matrix: no counterparty has mutual headroom with
        // party 3 -> fail fast at admission.
        expect_reject(f.checker.check(f.order, f.ctx),
                      kCodeBilateralCreditExhausted);

        // Grant mutual headroom party 3 <-> party 9 covering the notional
        // (1.1e9) -> screen passes. cell(a,b) is directed: credit_limit(a,b)
        // is a->b remaining headroom; can_match needs both directions.
        ASSERT_TRUE(m.apply_update(3, 9, 2'000'000'000));
        ASSERT_TRUE(m.apply_update(9, 3, 2'000'000'000));
        EXPECT_TRUE(f.checker.check(f.order, f.ctx).pass);

        // Unmapped account (anonymous retail flow) skips the screen even
        // with the matrix bound.
        m.close();
    }
    {
        // Re-open zeroed matrix, unmapped party -> pass.
        BilateralCreditMatrix m2;
        ASSERT_TRUE(m2.open(name, /*create=*/true));
        Fixture f2;
        MockPartyMap pmap2;  // unmapped
        f2.checker.bind_credit(&m2, &pmap2);
        EXPECT_TRUE(f2.checker.check(f2.order, f2.ctx).pass);
        m2.close();
    }
    ::shm_unlink(path.c_str());
}

// ---- latency budget: all 14 checks, <10µs total ---------------------------------------------------------

TEST(PreTrade, LatencyP99Under10us) {
    Fixture f;
    RiskConfig cfg;
    cfg.order_rate_per_sec = 10'000'000;  // collar can't bite mid-benchmark
    cfg.order_rate_burst = 10'000'000;
    PreTradeChecker fast(cfg);
    fast.bind_accounts(&f.accounts);
    fast.bind_positions(&f.positions);
    fast.bind_market(&f.market);

    const int kIters = 1000;
    std::vector<uint64_t> ns(static_cast<size_t>(kIters));
    Order o = make_order();
    CheckContext c{&f.inst, 0};
    // Warm the table + caches.
    for (int i = 0; i < 100; ++i) {
        o.account_id = static_cast<uint64_t>(i + 1);
        (void)fast.check(o, c);
    }
    for (int i = 0; i < kIters; ++i) {
        o.account_id = static_cast<uint64_t>(i + 1);  // fresh bucket per iter
        const auto a = std::chrono::steady_clock::now();
        const RiskVerdict v = fast.check(o, c);
        const auto b = std::chrono::steady_clock::now();
        ASSERT_TRUE(v.pass) << (v.code ? v.code : "?");
        ns[static_cast<size_t>(i)] =
            static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::nanoseconds>(b - a)
                    .count());
    }
    std::sort(ns.begin(), ns.end());
    const uint64_t p50 = ns[static_cast<size_t>(kIters / 2)];
    const uint64_t p99 = ns[static_cast<size_t>(kIters * 99 / 100)];
    const uint64_t worst = ns.back();
    std::printf("[pretrade] p50=%lluns p99=%lluns worst=%lluns\n",
                static_cast<unsigned long long>(p50),
                static_cast<unsigned long long>(p99),
                static_cast<unsigned long long>(worst));
    EXPECT_LT(p99, 10'000u);  // spec §3.3: all 14 checks < 10µs total
}

// --- Phase-13 Task 13.3.6: MiFID II RTS 9 OTR breach gate (check 0b) ----------

TEST(PreTrade, OtrKeyParsesAccountFlagOnly) {
    SuspensionFlags::Snapshot snap;
    EXPECT_TRUE(otr_key_into("otr:breach:7", &snap));
    EXPECT_TRUE(snap.otr_breached.count(7) == 1);
    // Bookkeeping + malformed + foreign keys never land a flag.
    EXPECT_FALSE(otr_key_into("otr:breach:index", &snap));   // sweep index
    EXPECT_FALSE(otr_key_into("otr:breach:", &snap));
    EXPECT_FALSE(otr_key_into("otr:breach:0", &snap));       // id 0 never valid
    EXPECT_FALSE(otr_key_into("otr:breach:abc", &snap));
    EXPECT_FALSE(otr_key_into("otr:events:7:EUR/USD", &snap));
    EXPECT_FALSE(otr_key_into("halt:account:7", &snap));     // wrong namespace
    EXPECT_EQ(snap.otr_breached.size(), 1u);
}

TEST(PreTrade, OtrBreachRejectsNewOrder) {
    Fixture f;
    SuspensionFlags flags;
    f.checker.bind_suspensions(&flags);
    auto snap = std::make_shared<SuspensionFlags::Snapshot>();
    snap->otr_breached.insert(7);  // make_order()'s account_id
    flags.apply(snap);
    const RiskVerdict v = f.checker.check(f.order, f.ctx);
    expect_reject(v, "OTR_LIMIT_EXCEEDED");
}

TEST(PreTrade, OtrBreachIsPerAccount) {
    Fixture f;
    SuspensionFlags flags;
    f.checker.bind_suspensions(&flags);
    auto snap = std::make_shared<SuspensionFlags::Snapshot>();
    snap->otr_breached.insert(9);  // a different account
    flags.apply(snap);
    const RiskVerdict v = f.checker.check(f.order, f.ctx);
    EXPECT_TRUE(v.pass) << (v.code ? v.code : "?");
}

TEST(PreTrade, OtrFlagClearReadmits) {
    Fixture f;
    SuspensionFlags flags;
    f.checker.bind_suspensions(&flags);
    auto breached = std::make_shared<SuspensionFlags::Snapshot>();
    breached->otr_breached.insert(7);
    flags.apply(breached);
    EXPECT_FALSE(f.checker.check(f.order, f.ctx).pass);
    // The Go monitor clears otr:breach:7 -> next poll publishes without it.
    flags.apply(std::make_shared<SuspensionFlags::Snapshot>());
    const RiskVerdict v = f.checker.check(f.order, f.ctx);
    EXPECT_TRUE(v.pass) << (v.code ? v.code : "?");
}

TEST(PreTrade, OtrBreachLegacyShimRejects) {
    // The detached check(order) path honors the same gate.
    SuspensionFlags flags;
    auto snap = std::make_shared<SuspensionFlags::Snapshot>();
    snap->otr_breached.insert(7);
    flags.apply(snap);
    PreTradeChecker detached;
    detached.bind_suspensions(&flags);
    Order o = make_order();
    EXPECT_EQ(detached.check(o), RiskDecision::REJECT);
    o.account_id = 8;
    EXPECT_EQ(detached.check(o), RiskDecision::ACCEPT);
}
