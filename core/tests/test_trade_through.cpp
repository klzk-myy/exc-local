// Tasks 2.3.17 + 2.3.22 — ExecutionCollar / TradeThroughGuard /
// PriceImprovementRecorder coverage (spec §22.2, §6.6b, §23, §24 #277/#400).
//
// Determinism contract under test: every API is a pure function of its
// parameters (timestamps arrive as args; no wall-clock reads), so identical
// WAL input reproduces identical bounds/verdicts/events.
//
// Price scale: 10^8 ticks per unit (Task 2.3.23). Fixture prices center on
// 1.10 == 110'000'000 ticks; multipliers are percent*100 (200 == 2.00%).

#include <gtest/gtest.h>

#include <cstdint>
#include <cstring>
#include <vector>

#include "matching/ExecutionCollar.hpp"
#include "matching/PriceImprovementRecorder.hpp"
#include "matching/TradeThroughGuard.hpp"

using namespace exch;

namespace {

constexpr int64_t kRef = 110'000'000;     // 1.10
constexpr uint64_t kNow = 1'700'000'000'000'000'000ull;  // arbitrary epoch ns

// ---------------------------------------------------------------------------
// Task 2.3.17 — ExecutionCollar (reference-price execution rule)
// ---------------------------------------------------------------------------

TEST(ExecutionCollar, BeginPhaseSnapshotsBoundsForWholePhase) {
    ExecutionCollar c;
    ExecutionCollarRule rule{};
    rule.configured = true;
    rule.buy.has_up = true;
    rule.buy.up_pct100 = 200;     // +2.00% -> 1.122
    rule.buy.has_down = true;
    rule.buy.down_pct100 = 500;   // -5.00% -> 1.045

    const CollarBounds b = c.begin_phase(Side::BUY, rule, kRef, kNow, kNow);
    ASSERT_EQ(b.phase, CollarPhase::ACTIVE);
    EXPECT_TRUE(b.lo_enforced);
    EXPECT_TRUE(b.hi_enforced);
    EXPECT_EQ(b.ref_ticks, kRef);
    EXPECT_EQ(b.hi_ticks, 112'200'000);   // 1.10 * 1.02
    EXPECT_EQ(b.lo_ticks, 104'500'000);   // 1.10 * 0.95

    // Snapshot constancy: a later oracle update must NOT move the bounds an
    // in-flight taker phase is using — the held CollarBounds is a value.
    const CollarBounds later = c.begin_phase(
        Side::BUY, rule, 120'000'000, kNow, kNow);
    EXPECT_EQ(b.hi_ticks, 112'200'000);   // untouched
    EXPECT_EQ(later.hi_ticks, 122'400'000);
    EXPECT_TRUE(ExecutionCollar::price_allowed(b, 112'200'000));
    EXPECT_FALSE(ExecutionCollar::price_allowed(b, 112'200'001));
}

TEST(ExecutionCollar, OutOfRangeMakerStopsMatching) {
    ExecutionCollar c;
    ExecutionCollarRule rule{};
    rule.configured = true;
    rule.buy.has_up = true;
    rule.buy.up_pct100 = 100;  // +1.00% -> 1.111

    const CollarBounds b =
        c.begin_phase(Side::BUY, rule, kRef, kNow, kNow);
    // Maker ladder just below / at / above the collar: the sweep admits the
    // first two and stops BEFORE the first out-of-range price.
    EXPECT_TRUE(ExecutionCollar::price_allowed(b, 110'000'000));
    EXPECT_TRUE(ExecutionCollar::price_allowed(b, 111'100'000));
    EXPECT_FALSE(ExecutionCollar::price_allowed(b, 111'100'001));
    // Lower bound unenforced for a buy collar that only configures "up".
    EXPECT_TRUE(ExecutionCollar::price_allowed(b, 1));

    EXPECT_STREQ(ExecutionCollar::kExpiryReason,
                 "EXECUTION_RULE_PRICE_RANGE_EXCEEDED");
    EXPECT_LE(std::strlen(ExecutionCollar::kExpiryReason), size_t{64});
}

TEST(ExecutionCollar, AbsentRuleOrMultiplierLeavesDirectionUnenforced) {
    ExecutionCollar c;

    // No rule object at all.
    ExecutionCollarRule none{};
    EXPECT_EQ(c.begin_phase(Side::BUY, none, kRef, kNow, kNow).phase,
              CollarPhase::NOT_CONFIGURED);
    EXPECT_TRUE(ExecutionCollar::price_allowed(
        c.begin_phase(Side::BUY, none, kRef, kNow, kNow), INT64_MAX));

    // Rule present but only the sell direction configured -> buy unenforced.
    ExecutionCollarRule sell_only{};
    sell_only.configured = true;
    sell_only.sell.has_down = true;
    sell_only.sell.down_pct100 = 100;
    EXPECT_EQ(c.begin_phase(Side::BUY, sell_only, kRef, kNow, kNow).phase,
              CollarPhase::NOT_CONFIGURED);
    EXPECT_EQ(c.begin_phase(Side::SELL, sell_only, kRef, kNow, kNow).phase,
              CollarPhase::ACTIVE);

    // Side configured but neither multiplier set -> unenforced.
    ExecutionCollarRule empty{};
    empty.configured = true;
    EXPECT_EQ(c.begin_phase(Side::BUY, empty, kRef, kNow, kNow).phase,
              CollarPhase::NOT_CONFIGURED);

    // Missing reference price (0) disables only that direction — NOT a fail.
    const CollarBounds b =
        c.begin_phase(Side::SELL, sell_only, 0, kNow, kNow);
    EXPECT_EQ(b.phase, CollarPhase::NOT_CONFIGURED);
    EXPECT_TRUE(ExecutionCollar::price_allowed(b, 1));
}

TEST(ExecutionCollar, StaleReferenceFailsClosed) {
    ExecutionCollar c;  // built-in gate: 5s
    ExecutionCollarRule rule{};
    rule.configured = true;
    rule.buy.has_up = true;
    rule.buy.up_pct100 = 100;

    // Fresh reference -> ACTIVE.
    EXPECT_EQ(c.begin_phase(Side::BUY, rule, kRef, kNow, kNow).phase,
              CollarPhase::ACTIVE);
    // Boundary: exactly max age is still fresh.
    EXPECT_EQ(c.begin_phase(Side::BUY, rule, kRef,
                            kNow - ExecutionCollar::kDefaultMaxRefAgeNs, kNow)
                  .phase,
              CollarPhase::ACTIVE);
    // 1ns past the gate -> BLOCKED, every maker price disallowed.
    const CollarBounds b = c.begin_phase(
        Side::BUY, rule, kRef,
        kNow - ExecutionCollar::kDefaultMaxRefAgeNs - 1, kNow);
    EXPECT_EQ(b.phase, CollarPhase::BLOCKED);
    EXPECT_FALSE(ExecutionCollar::price_allowed(b, kRef));
    EXPECT_FALSE(ExecutionCollar::price_allowed(b, 1));
    // Untimestamped / future-dated references are untrustworthy -> stale.
    EXPECT_EQ(c.begin_phase(Side::BUY, rule, kRef, 0, kNow).phase,
              CollarPhase::BLOCKED);
    EXPECT_EQ(c.begin_phase(Side::BUY, rule, kRef, kNow + 1, kNow).phase,
              CollarPhase::BLOCKED);
}

struct ProbeCtx {
    bool verdict;
    int calls;
};
bool probe_fn(void* ctx, int64_t ref, uint64_t ref_ts,
              uint64_t now) noexcept {
    auto* p = static_cast<ProbeCtx*>(ctx);
    ++p->calls;
    (void)ref;
    (void)ref_ts;
    (void)now;
    return p->verdict;  // ctx-driven staleness
}

TEST(ExecutionCollar, InjectedStalenessProbeOverrides) {
    ProbeCtx probe{true, 0};
    ExecutionCollar c(0);  // age gate disabled -> probe alone decides
    c.set_staleness_probe(&probe_fn, &probe);

    ExecutionCollarRule rule{};
    rule.configured = true;
    rule.sell.has_down = true;
    rule.sell.down_pct100 = 50;

    const CollarBounds blocked =
        c.begin_phase(Side::SELL, rule, kRef, kNow, kNow);
    EXPECT_EQ(blocked.phase, CollarPhase::BLOCKED);
    EXPECT_GE(probe.calls, 1);

    probe.verdict = false;
    const CollarBounds live =
        c.begin_phase(Side::SELL, rule, kRef, kNow, kNow);
    EXPECT_EQ(live.phase, CollarPhase::ACTIVE);
    EXPECT_EQ(live.lo_ticks, 109'450'000);  // 1.10 * 0.995
    EXPECT_TRUE(ExecutionCollar::price_allowed(live, 109'450'000));
    EXPECT_FALSE(ExecutionCollar::price_allowed(live, 109'449'999));
}

TEST(ExecutionCollar, MisconfiguredBoundsFailClosed) {
    ExecutionCollar c;
    ExecutionCollarRule rule{};
    rule.configured = true;
    rule.buy.has_up = true;
    rule.buy.up_pct100 = -100;  // negative multiplier inverts the bound
    EXPECT_EQ(c.begin_phase(Side::BUY, rule, kRef, kNow, kNow).phase,
              CollarPhase::BLOCKED);

    // down > 100% clamps the lower bound to the positive-price floor rather
    // than admitting price 0.
    rule.buy.up_pct100 = 0;
    rule.buy.has_up = false;
    rule.buy.has_down = true;
    rule.buy.down_pct100 = 20'000;  // -200%
    const CollarBounds b = c.begin_phase(Side::BUY, rule, kRef, kNow, kNow);
    EXPECT_EQ(b.phase, CollarPhase::ACTIVE);
    EXPECT_EQ(b.lo_ticks, 1);
    EXPECT_TRUE(ExecutionCollar::price_allowed(b, 1));
}

// ---------------------------------------------------------------------------
// Task 2.3.22 — TradeThroughGuard
// ---------------------------------------------------------------------------

struct EventLog {
    static void sink(void* ctx, const TradeThroughEvent& e) noexcept {
        static_cast<EventLog*>(ctx)->events.push_back(e);
    }
    std::vector<TradeThroughEvent> events;
};

TtCheckInput make_input(OrderType t, TimeInForce tif, Side side,
                        int64_t limit = 0, int64_t prot = 0, int64_t liq = 0) {
    TtCheckInput in{};
    in.order_id = 42;
    in.side = side;
    in.type = t;
    in.tif = tif;
    in.limit_ticks = limit;
    in.protection_price_ticks = prot;
    in.liquidity_at_or_better_units = liq;
    in.ts_ns = kNow;
    return in;
}

TEST(TradeThroughGuard, ProtectedQuoteCacheTracksBookMutations) {
    TradeThroughGuard g;
    EXPECT_EQ(g.quote().bid_ticks, 0);
    g.on_book_mutation(109'900'000, 110'100'000);
    ProtectedQuote q = g.quote();
    EXPECT_EQ(q.bid_ticks, 109'900'000);
    EXPECT_EQ(q.ask_ticks, 110'100'000);
    // Every mutation republishes atomically — the pair always moves together.
    g.on_book_mutation(109'950'000, 110'050'000);
    q = g.quote();
    EXPECT_EQ(q.bid_ticks, 109'950'000);
    EXPECT_EQ(q.ask_ticks, 110'050'000);
}

TEST(TradeThroughGuard, LimitCrossingQuoteRejected) {
    TradeThroughGuard g;
    EventLog log;
    g.set_event_sink(&EventLog::sink, &log);
    g.on_book_mutation(109'900'000, 110'100'000);

    // BUY limit beyond the protected ask -> TRADE_THROUGH_DETECTED (409).
    const TtVerdict v = g.check(
        make_input(OrderType::LIMIT, TimeInForce::GTC, Side::BUY,
                   110'200'000));
    EXPECT_EQ(v.decision, TtDecision::REJECT);
    ASSERT_NE(v.code, nullptr);
    EXPECT_STREQ(v.code, "TRADE_THROUGH_DETECTED");
    ASSERT_EQ(log.events.size(), size_t{1});
    EXPECT_EQ(log.events[0].kind,
              static_cast<uint8_t>(
                  TtEventKind::LIMIT_TRADE_THROUGH_REJECTED));
    EXPECT_EQ(log.events[0].protected_ask_ticks, 110'100'000);
    EXPECT_EQ(log.events[0].order_id, uint64_t{42});
    EXPECT_EQ(log.events[0].ts_ns, kNow);
    EXPECT_EQ(g.rejections(), uint64_t{1});

    // Symmetric SELL: limit below the protected bid -> reject.
    EXPECT_EQ(g.check(make_input(OrderType::LIMIT, TimeInForce::GTC,
                                 Side::SELL, 109'800'000))
                  .decision,
              TtDecision::REJECT);
}

TEST(TradeThroughGuard, LimitAtOrInsideQuotePasses) {
    TradeThroughGuard g;
    g.on_book_mutation(109'900'000, 110'100'000);
    // At the quote: can only fill at-or-better -> pass.
    EXPECT_EQ(g.check(make_input(OrderType::LIMIT, TimeInForce::GTC,
                                 Side::BUY, 110'100'000))
                  .decision,
              TtDecision::PASS);
    // Inside the quote (non-marketable) -> pass.
    EXPECT_EQ(g.check(make_input(OrderType::LIMIT, TimeInForce::GTC,
                                 Side::BUY, 109'950'000))
                  .decision,
              TtDecision::PASS);
    EXPECT_EQ(g.check(make_input(OrderType::LIMIT, TimeInForce::GTC,
                                 Side::SELL, 109'900'000))
                  .decision,
              TtDecision::PASS);
    // No opposite quote -> nothing to cross (empty side stays with §6.6).
    TradeThroughGuard empty;
    empty.on_book_mutation(0, 0);
    EXPECT_EQ(empty.check(make_input(OrderType::LIMIT, TimeInForce::GTC,
                                     Side::BUY, 500'000'000))
                  .decision,
              TtDecision::PASS);
}

TEST(TradeThroughGuard, MarketClipsToProtectedQuote) {
    TradeThroughGuard g;
    EventLog log;
    g.set_event_sink(&EventLog::sink, &log);
    g.on_book_mutation(109'900'000, 110'100'000);

    // §6.6a protection price (1.105) lies beyond the quote (1.101) -> clip.
    TtVerdict v = g.check(make_input(OrderType::MARKET, TimeInForce::GTC,
                                     Side::BUY, 0, 110'500'000));
    EXPECT_EQ(v.decision, TtDecision::CLIP);
    EXPECT_EQ(v.effective_limit_ticks, 110'100'000);
    ASSERT_NE(v.remainder_code, nullptr);
    EXPECT_STREQ(v.remainder_code, "SLIPPAGE_EXCEEDED");
    ASSERT_EQ(log.events.size(), size_t{1});
    EXPECT_EQ(log.events[0].kind,
              static_cast<uint8_t>(TtEventKind::MARKET_CLIPPED_TO_QUOTE));
    EXPECT_EQ(log.events[0].effective_limit_ticks, 110'100'000);

    // Protection price already tighter than the quote -> bound kept, no
    // prevention event (nothing was clipped away).
    v = g.check(make_input(OrderType::MARKET, TimeInForce::GTC, Side::BUY, 0,
                           110'050'000));
    EXPECT_EQ(v.decision, TtDecision::CLIP);
    EXPECT_EQ(v.effective_limit_ticks, 110'050'000);
    EXPECT_EQ(log.events.size(), size_t{1});  // unchanged

    // No slippage band configured -> the quote itself is the bound (event).
    v = g.check(make_input(OrderType::MARKET, TimeInForce::GTC, Side::BUY));
    EXPECT_EQ(v.decision, TtDecision::CLIP);
    EXPECT_EQ(v.effective_limit_ticks, 110'100'000);
    EXPECT_EQ(log.events.size(), size_t{2});

    // SELL symmetry: protection floor lifted to the protected bid.
    v = g.check(make_input(OrderType::MARKET, TimeInForce::GTC, Side::SELL, 0,
                           109'500'000));
    EXPECT_EQ(v.decision, TtDecision::CLIP);
    EXPECT_EQ(v.effective_limit_ticks, 109'900'000);

    // No opposite quote at all -> nothing to clip to.
    TradeThroughGuard one_sided;
    one_sided.on_book_mutation(109'900'000, 0);
    EXPECT_EQ(one_sided.check(make_input(OrderType::MARKET,
                                         TimeInForce::GTC, Side::BUY))
                  .decision,
              TtDecision::PASS);
}

TEST(TradeThroughGuard, TriggeredStopFormsUseTakerArms) {
    TradeThroughGuard g;
    g.on_book_mutation(109'900'000, 110'100'000);
    // STOP_LIMIT at trigger time is a limit taker — crossing the protected
    // quote rejects with TRADE_THROUGH_DETECTED.
    EXPECT_EQ(g.check(make_input(OrderType::STOP_LIMIT, TimeInForce::GTC,
                                 Side::BUY, 110'200'000))
                  .decision,
              TtDecision::REJECT);
    // STOP at trigger time is a market taker — clipped to the quote.
    const TtVerdict v = g.check(make_input(OrderType::STOP,
                                           TimeInForce::GTC, Side::BUY));
    EXPECT_EQ(v.decision, TtDecision::CLIP);
    EXPECT_EQ(v.effective_limit_ticks, 110'100'000);
}

TEST(TradeThroughGuard, RemainderCancelEmitsSlippageEvent) {
    TradeThroughGuard g;
    EventLog log;
    g.set_event_sink(&EventLog::sink, &log);
    g.on_book_mutation(109'900'000, 110'100'000);
    g.on_remainder_cancelled(42, Side::BUY, 500'000'000, kNow);
    ASSERT_EQ(log.events.size(), size_t{1});
    EXPECT_EQ(log.events[0].kind,
              static_cast<uint8_t>(TtEventKind::MARKET_REMAINDER_SLIPPAGE));
    EXPECT_EQ(log.events[0].remainder_units, 500'000'000);
    EXPECT_EQ(log.events[0].protected_ask_ticks, 110'100'000);
}

TEST(TradeThroughGuard, IocFokRequireLiquidityAtOrBetter) {
    TradeThroughGuard g;
    EventLog log;
    g.set_event_sink(&EventLog::sink, &log);
    g.on_book_mutation(109'900'000, 110'100'000);

    // IOC with zero liquidity at-or-better the quote -> TRADE_THROUGH_DETECTED.
    TtVerdict v = g.check(make_input(OrderType::LIMIT, TimeInForce::IOC,
                                     Side::BUY, 110'100'000, 0, 0));
    EXPECT_EQ(v.decision, TtDecision::REJECT);
    EXPECT_STREQ(v.code, "TRADE_THROUGH_DETECTED");
    ASSERT_EQ(log.events.size(), size_t{1});
    EXPECT_EQ(log.events[0].kind,
              static_cast<uint8_t>(
                  TtEventKind::IOC_FOK_NO_LIQUIDITY_REJECTED));

    // FOK same gate.
    EXPECT_EQ(g.check(make_input(OrderType::MARKET, TimeInForce::FOK,
                                 Side::BUY, 0, 0, 0))
                  .decision,
              TtDecision::REJECT);

    // Liquidity present -> presence gate passes; MARKET FOK then clips.
    v = g.check(make_input(OrderType::MARKET, TimeInForce::FOK, Side::BUY, 0,
                           110'500'000, 1'000'000'000));
    EXPECT_EQ(v.decision, TtDecision::CLIP);

    // LIMIT IOC priced THROUGH the quote with liquidity -> still a
    // trade-through reject (the limit-cross gate runs after the TIF gate).
    v = g.check(make_input(OrderType::LIMIT, TimeInForce::IOC, Side::BUY,
                           110'200'000, 0, 1'000'000'000));
    EXPECT_EQ(v.decision, TtDecision::REJECT);
    EXPECT_STREQ(v.code, "TRADE_THROUGH_DETECTED");
}

TEST(TradeThroughGuard, AuctionBypassesAllChecks) {
    TradeThroughGuard g;
    EventLog log;
    g.set_event_sink(&EventLog::sink, &log);
    g.on_book_mutation(109'900'000, 110'100'000);
    g.set_auction(true);
    ASSERT_TRUE(g.in_auction());

    // Every would-be violation passes untouched during a call auction —
    // the uncross price is the single market-clearing price (§6.6b #4).
    EXPECT_EQ(g.check(make_input(OrderType::LIMIT, TimeInForce::GTC,
                                 Side::BUY, 999'000'000))
                  .decision,
              TtDecision::PASS);
    EXPECT_EQ(g.check(make_input(OrderType::LIMIT, TimeInForce::IOC,
                                 Side::BUY, 110'100'000, 0, 0))
                  .decision,
              TtDecision::PASS);
    EXPECT_EQ(log.events.size(), size_t{0});
    EXPECT_EQ(g.bypassed_checks(), uint64_t{2});
    EXPECT_EQ(g.rejections(), uint64_t{0});

    g.set_auction(false);
    EXPECT_EQ(g.check(make_input(OrderType::LIMIT, TimeInForce::GTC,
                                 Side::BUY, 999'000'000))
                  .decision,
              TtDecision::REJECT);
}

TEST(TradeThroughGuard, EventSinkMayBeAbsent) {
    TradeThroughGuard g;  // no sink registered
    g.on_book_mutation(109'900'000, 110'100'000);
    EXPECT_EQ(g.check(make_input(OrderType::LIMIT, TimeInForce::GTC,
                                 Side::BUY, 110'200'000))
                  .decision,
              TtDecision::REJECT);
    EXPECT_EQ(g.events_emitted(), uint64_t{1});  // counted, not delivered
    EXPECT_EQ(g.checks(), uint64_t{1});
}

// ---------------------------------------------------------------------------
// Task 2.3.22 — PriceImprovementRecorder (spec §6.6b #3)
// ---------------------------------------------------------------------------

struct StampLog {
    static void sink(void* ctx, const ImprovementStamp& s) noexcept {
        static_cast<StampLog*>(ctx)->stamps.push_back(s);
    }
    std::vector<ImprovementStamp> stamps;
};

TEST(PriceImprovementRecorder, DeltaIsLimitMinusExec) {
    // BUY limited at 1.10 lifting a 1.095 offer -> +0.005 improvement.
    ImprovementStamp s = PriceImprovementRecorder::compute(
        Side::BUY, 7, 1001, 110'000'000, 109'500'000, 1'000'000'000);
    EXPECT_EQ(s.delta_ticks, 500'000);
    EXPECT_EQ(s.improved, uint8_t{1});

    // SELL limited at 1.10 hitting a 1.105 bid -> spec formula yields -0.005
    // (signed), improved flag is side-aware positive.
    s = PriceImprovementRecorder::compute(Side::SELL, 8, 1002, 110'000'000,
                                          110'500'000, 500'000'000);
    EXPECT_EQ(s.delta_ticks, -500'000);
    EXPECT_EQ(s.improved, uint8_t{1});

    // Fill AT the limit -> no improvement.
    s = PriceImprovementRecorder::compute(Side::BUY, 9, 1003, 110'000'000,
                                          110'000'000, 1'000'000'000);
    EXPECT_EQ(s.delta_ticks, 0);
    EXPECT_EQ(s.improved, uint8_t{0});

    // Maker-side / no-limit fill (MARKET or maker leg) -> delta 0.
    s = PriceImprovementRecorder::compute(Side::SELL, 10, 1004, 0,
                                          110'000'000, 1);
    EXPECT_EQ(s.delta_ticks, 0);
    EXPECT_EQ(s.improved, uint8_t{0});
}

TEST(PriceImprovementRecorder, RecordAccumulatesTcaCountersAndStamps) {
    PriceImprovementRecorder r;
    StampLog log;
    r.set_stamp_sink(&StampLog::sink, &log);

    r.on_fill(Side::BUY, 7, 1, 110'000'000, 109'500'000, 1'000'000'000);
    r.on_fill(Side::SELL, 8, 2, 110'000'000, 110'500'000, 500'000'000);
    r.on_fill(Side::BUY, 9, 3, 110'000'000, 110'000'000, 1'000'000'000);
    r.on_fill(Side::SELL, 10, 4, 0, 110'100'000, 1'000'000'000);  // market

    EXPECT_EQ(r.fills_seen(), uint64_t{4});
    EXPECT_EQ(r.fills_improved(), uint64_t{2});
    // Side-aware magnitude sum: 500k (buy) + 500k (sell) = 1'000'000 ticks.
    EXPECT_EQ(r.improvement_ticks_total(), 1'000'000);
    ASSERT_EQ(log.stamps.size(), size_t{4});
    EXPECT_EQ(log.stamps[0].order_id, uint64_t{7});
    EXPECT_EQ(log.stamps[0].trade_id, uint64_t{1});
    EXPECT_EQ(log.stamps[0].delta_ticks, 500'000);
    EXPECT_EQ(log.stamps[3].delta_ticks, 0);
    EXPECT_EQ(r.last().order_id, uint64_t{10});
}

TEST(PriceImprovementRecorder, SinklessRecordingStillCounts) {
    PriceImprovementRecorder r;
    r.on_fill(Side::BUY, 1, 1, 110'000'000, 109'000'000, 1);
    EXPECT_EQ(r.fills_seen(), uint64_t{1});
    EXPECT_EQ(r.fills_improved(), uint64_t{1});
}

}  // namespace

// ---------------------------------------------------------------------------
// Engine-level integration (Tasks 2.3.17 + 2.3.22): the standalone components
// above are now wired into MatchingEngine — collar bounds gate each maker
// candidate inside walk_match/fok_feasible, TradeThroughGuard gates intake
// when enabled per instruments.execution_rule, and every fill stamps the
// price-improvement recorder. These cases exercise the real ingress path.
// ---------------------------------------------------------------------------

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "matching/MatchingEngine.hpp"
#include "utils/MemoryPool.hpp"

namespace {

constexpr int64_t kBasePx = 110'000'000;
constexpr int64_t kPip = 10'000;

uint64_t g_eng_ts = 2'000'000'000;

Order* mkeng(MemoryPool<Order>& pool, uint64_t id, Side side, OrderType type,
             int64_t price, int64_t qty, uint64_t acct = 1,
             TimeInForce tif = TimeInForce::GTC, uint8_t flags = 0) {
    Order* o = pool.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = side;
    o->type = type;
    o->tif = tif;
    o->flags = flags;
    o->price_ticks = price;
    o->qty_units = qty;
    o->quantity = Decimal::from_mantissa(qty);
    o->timestamp_ns = ++g_eng_ts;
    o->ingress_seq = g_eng_ts;
    return o;
}

struct EngFixture {
    MemoryPool<Order> pool{512};
    Instrument instr;
    OrderBook book{pool};
    MatchingEngine engine{3, book, pool, nullptr, nullptr};

    EngFixture() {
        instr.instrument_id = 7;
        instr.pip_factor = 10;
        instr.pip_size_ticks = kPip;
        instr.tick_size_ticks = 1'000;
        instr.lot_size_units = 1;
        book.set_instrument(instr);
    }

    void add_makers(Side s, const int64_t (&pxs)[4], int64_t qty) {
        for (int i = 0; i < 4 && pxs[i] != 0; ++i) {
            engine.on_order_received(
                mkeng(pool, 900 + i, s, OrderType::LIMIT, pxs[i], qty, 9));
        }
    }
};

// --- 2.3.22 trade-through ---------------------------------------------------

TEST(EngineTradeThrough, DisabledByDefault_MarketableLimitSweepsNormally) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx, kBasePx + kPip, 0, 0};
    f.add_makers(Side::SELL, asks, 30);
    // Buy limit above the top ask — with TT off this sweeps both levels.
    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT,
              kBasePx + kPip, 50, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 2u);
}

TEST(EngineTradeThrough, LimitBeyondProtectedQuoteRejected) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx, 0, 0, 0};
    f.add_makers(Side::SELL, asks, 30);
    f.engine.set_trade_through_protection(true);

    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT,
              kBasePx + kPip, 10, 2));
    EXPECT_STREQ(f.engine.last_reject(),
                 TradeThroughGuard::kCodeTradeThroughDetected);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(f.book.find_order(1), nullptr);
}

TEST(EngineTradeThrough, LimitExactlyAtQuoteFills) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx, 0, 0, 0};
    f.add_makers(Side::SELL, asks, 30);
    f.engine.set_trade_through_protection(true);
    // limit == protected ask -> at the quote -> allowed, fills at top level.
    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT, kBasePx, 10, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
}

TEST(EngineTradeThrough, FillAtBetterPriceStampsImprovement) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx - kPip, 0, 0, 0};  // better than buy limit
    f.add_makers(Side::SELL, asks, 30);
    // TT off (execution_rule absent): the sweep is unrestricted — the buy
    // fills kPip inside its limit and the delta lands on the recorder.
    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT, kBasePx, 10, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.improvement_recorder().fills_improved(), 1u);
    EXPECT_EQ(f.engine.improvement_recorder().last().delta_ticks, kPip);
    EXPECT_EQ(f.engine.improvement_recorder().last().order_id, 1u);
}

TEST(EngineTradeThrough, MarketClipsToProtectedQuote_RemainderSlippage) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx, kBasePx + kPip, 0, 0};
    f.add_makers(Side::SELL, asks, 30);
    f.engine.set_trade_through_protection(true);
    // Spec §6.6b #7: slippage protection must be wider than the clip or the
    // market arm's own cap would reject first — disable it here.
    f.instr.max_slippage_bps = 10'000;
    f.book.set_instrument(f.instr);

    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::MARKET, 0, 50, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);  // only the top level filled
    EXPECT_STREQ(f.engine.last_reject(),
                 TradeThroughGuard::kCodeSlippageExceeded);
    EXPECT_EQ(f.book.find_order(1), nullptr);
    // Deeper level untouched.
    EXPECT_EQ(f.book.level(Side::SELL, 0)->total_qty_units, 30);
}

TEST(EngineTradeThrough, IocOnEmptyBookRejectedByLiquidityGate) {
    EngFixture f;
    f.engine.set_trade_through_protection(true);
    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT, kBasePx, 10, 2,
              TimeInForce::IOC));
    // §6.6 sparse-book gate (2.3.13) precedes the TT IOC check — on an
    // empty book the canonical code is ORDER_REJECTED_NO_LIQUIDITY.
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectNoLiquidity);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
}

TEST(EngineTradeThrough, AuctionBypassesProtection) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx, 0, 0, 0};
    f.add_makers(Side::SELL, asks, 30);
    f.engine.set_trade_through_protection(true);
    f.engine.set_auction_mode(true);

    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT,
              kBasePx + kPip, 10, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);  // §24 #401 — auction bypass
}

// --- 2.3.17 execution collar ------------------------------------------------

TEST(EngineCollar, OutOfRangeMakerStopsSweep_RemainderExpires) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx, kBasePx + 80 * kPip, 0, 0};
    f.add_makers(Side::SELL, asks, 30);
    f.engine.set_reference_price(kBasePx, /*ref_ts_ns=*/1);
    ExecutionCollarRule rule;
    rule.buy.up_pct100 = 50;      // 0.50% band (~55 pips) — level 2 is out
    rule.buy.has_up = true;
    rule.sell.down_pct100 = 50;
    rule.sell.has_down = true;
    rule.configured = true;
    f.engine.set_execution_rule(rule);
    f.engine.on_time_tick(2);  // logical clock past ref_ts so ref is fresh

    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT,
              kBasePx + 100 * kPip, 50, 2));  // limit admits lvl2; collar bars it
    EXPECT_EQ(f.engine.trades_emitted(), 1u);  // first level only
    EXPECT_STREQ(f.engine.last_reject(),
                 ExecutionCollar::kExpiryReason);
    EXPECT_EQ(f.book.find_order(1), nullptr);  // remainder expired, not rested
    EXPECT_NE(f.book.find_order(901), nullptr);
}

TEST(EngineCollar, AbsentRuleMeansUnenforced) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx, kBasePx + 2 * kPip, 0, 0};
    f.add_makers(Side::SELL, asks, 30);
    // No rule configured -> both levels sweep even with a ref set.
    f.engine.set_reference_price(kBasePx, 1);
    f.engine.on_time_tick(2);
    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT,
              kBasePx + 2 * kPip, 50, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 2u);
}

TEST(EngineCollar, StaleReferenceFailsClosed) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx, 0, 0, 0};
    f.add_makers(Side::SELL, asks, 30);
    f.engine.set_reference_price(kBasePx, /*ref_ts_ns=*/0);  // never priced
    ExecutionCollarRule rule;
    rule.buy.up_pct100 = 50;
    rule.buy.has_up = true;
    rule.configured = true;
    f.engine.set_execution_rule(rule);
    f.engine.on_time_tick(10);

    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT, kBasePx, 10, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);  // fail-closed on stale ref
    EXPECT_STREQ(f.engine.last_reject(),
                 ExecutionCollar::kExpiryReason);
}

TEST(EngineCollar, FokInfeasibleWhenCollarBlocksLevels) {
    EngFixture f;
    const int64_t asks[4] = {kBasePx, kBasePx + 80 * kPip, 0, 0};
    f.add_makers(Side::SELL, asks, 30);
    f.engine.set_reference_price(kBasePx, 1);
    ExecutionCollarRule rule;
    rule.buy.up_pct100 = 50;
    rule.buy.has_up = true;
    rule.configured = true;
    f.engine.set_execution_rule(rule);
    f.engine.on_time_tick(2);

    // FOK needs 50 units but the collar admits only the first 30-level.
    f.engine.on_order_received(
        mkeng(f.pool, 1, Side::BUY, OrderType::LIMIT,
              kBasePx + 100 * kPip, 50, 2, TimeInForce::FOK));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_NE(f.engine.last_reject(), nullptr);
    EXPECT_NE(f.book.find_order(900), nullptr);  // book untouched
}

}  // namespace
