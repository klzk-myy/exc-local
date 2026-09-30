// Phase-22 Task 22.3.12 — multi-leg implied matching (spec §6.3/§24).
// Covers: registration validation, implied-in (swap from outright legs),
// implied-out (far outright from swap+near), multi-level quantity
// limiting + repricing, deterministic link ordering, post_only probe,
// WAL-before-mutation ordering, wal-fault halt, resting-order implied
// fills via on_book_changed, self-account prefix exclusion, and the
// no-phantom-resting-orders invariant.
//
// Not in CMakeLists.txt's test table (that file is outside this task's
// change scope) — compile directly; see the task report for the
// registration seam.

#include <gtest/gtest.h>

#include <cstdint>
#include <cstdio>
#include <unistd.h>

#include "book/OrderBook.hpp"
#include "matching/ImpliedMatcher.hpp"
#include "matching/WalWriter.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"

using namespace exch;

namespace {

constexpr int64_t P(int64_t v) { return v; }  // ticks, 10^8 scale
uint64_t g_ts = 1'000'000'000;

Order* mk(MemoryPool<Order>& pool, uint64_t id, Side side,
          int64_t price, int64_t qty, uint64_t acct = 1,
          TimeInForce tif = TimeInForce::GTC) {
    Order* o = pool.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = side;
    o->type = OrderType::LIMIT;
    o->tif = tif;
    o->stp_mode = StpMode::NONE;
    o->price_ticks = price;
    o->qty_units = qty;
    o->quantity = Decimal::from_mantissa(qty);
    o->timestamp_ns = ++g_ts;
    o->ingress_seq = g_ts;
    return o;
}

// New taker node — mid-ingress shape: pooled but never added to a book.
Order* taker(MemoryPool<Order>& pool, uint64_t id, Side side,
             int64_t price, int64_t qty, uint64_t acct = 9,
             uint8_t flags = 0, OrderType type = OrderType::LIMIT) {
    Order* o = mk(pool, id, side, price, qty, acct);
    o->flags = flags;
    o->type = type;
    return o;
}

// rest inserts a resting order directly into a book (pre-seeded state —
// the implied matcher is exercised below the engine dispatch loop).
Order* rest(OrderBook& book, MemoryPool<Order>& pool, uint64_t id,
            Side side, int64_t price, int64_t qty, uint64_t acct = 1) {
    Order* o = mk(pool, id, side, price, qty, acct);
    Order* out = nullptr;
    EXPECT_EQ(book.add_order(*o, &out), BookError::OK);
    return out;
}

ImpliedLink link(uint64_t id, uint32_t out, uint32_t i0, int8_t s0,
                 uint32_t r0, uint32_t i1, int8_t s1, uint32_t r1) {
    ImpliedLink l{};
    l.link_id = id;
    l.out_instrument = out;
    l.legs[0] = ImpliedLeg{i0, s0, r0};
    l.legs[1] = ImpliedLeg{i1, s1, r1};
    return l;
}

constexpr uint32_t kNear = 1;   // spot/near outright
constexpr uint32_t kFar = 2;    // far outright
constexpr uint32_t kSwap = 3;   // swap-points book: far − near

// Three-book curve: near (1) + far (2) + swap (3).
struct Curve {
    MemoryPool<Order> p_near{256}, p_far{256}, p_swap{256}, p_t{256};
    OrderBook near_b{p_near}, far_b{p_far}, swap_b{p_swap};
    ImpliedMatcher m;
    uint64_t tid = 1;

    Curve() {
        EXPECT_TRUE(m.register_book(kNear, near_b, nullptr, nullptr));
        EXPECT_TRUE(m.register_book(kFar, far_b, nullptr, nullptr));
        EXPECT_TRUE(m.register_book(kSwap, swap_b, nullptr, nullptr));
    }
    // IMPLIED_IN: swap = far − near.
    void implied_in(uint64_t id = 10) {
        EXPECT_TRUE(m.register_link(
            link(id, kSwap, kFar, +1, 1, kNear, -1, 1)));
    }
    // IMPLIED_OUT: far = swap + near.
    void implied_out(uint64_t id = 11) {
        EXPECT_TRUE(m.register_link(
            link(id, kFar, kSwap, +1, 1, kNear, +1, 1)));
    }
};

}  // namespace

// --- registration -----------------------------------------------------------

TEST(ImpliedMatcher, RegistrationValidation) {
    Curve c;
    EXPECT_FALSE(c.m.register_book(kNear, c.near_b, nullptr, nullptr));
    EXPECT_FALSE(c.m.register_book(0, c.near_b, nullptr, nullptr));
    // Unregistered leg / out.
    EXPECT_FALSE(c.m.register_link(link(1, 99, kFar, +1, 1, kNear, -1, 1)));
    EXPECT_FALSE(c.m.register_link(link(2, kSwap, 99, +1, 1, kNear, -1, 1)));
    // Bad sign / ratio / self-reference / duplicate id.
    EXPECT_FALSE(c.m.register_link(link(3, kSwap, kFar, 0, 1, kNear, -1, 1)));
    EXPECT_FALSE(c.m.register_link(link(4, kSwap, kFar, +1, 0, kNear, -1, 1)));
    EXPECT_FALSE(c.m.register_link(link(5, kSwap, kSwap, +1, 1, kNear, -1, 1)));
    c.implied_in(6);
    EXPECT_FALSE(c.m.register_link(link(6, kSwap, kFar, +1, 1, kNear, -1, 1)));
    EXPECT_TRUE(c.m.unregister_link(6));
    EXPECT_FALSE(c.m.unregister_link(6));
    c.implied_in(6);  // re-register after removal
}

// --- implied-in --------------------------------------------------------------

TEST(ImpliedMatcher, ImpliedInSwapFromOutrights) {
    Curve c;
    c.implied_in();
    // far ask 1.10500e8, near bid 1.10000e8 → implied swap ask = 0.00500e8.
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 100);
    rest(c.near_b, c.p_near, 2, Side::BUY, P(110000), 100);

    Order* t = taker(c.p_t, 900, Side::BUY, P(600), 40);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'000, c.tid);
    EXPECT_EQ(r.filled_units, 40);
    EXPECT_EQ(r.combos, 1u);
    EXPECT_EQ(r.fills, 2u);   // one maker fill per leg
    EXPECT_EQ(t->filled_qty_units, 40);
    EXPECT_EQ(remaining_qty_units(*t), 0);
    // Legs consumed at their own prices.
    EXPECT_EQ(c.far_b.level(Side::SELL, 0)->total_qty_units, 60);
    EXPECT_EQ(c.near_b.level(Side::BUY, 0)->total_qty_units, 60);
    // No phantom resting order materialized in the swap book.
    EXPECT_EQ(c.swap_b.live_orders(), 0u);
}

TEST(ImpliedMatcher, ImpliedInSellConsumesBidSideCombo) {
    Curve c;
    c.implied_in();
    // implied swap bid = far_bid − near_ask = 110400 − 110050 = 350.
    rest(c.far_b, c.p_far, 1, Side::BUY, P(110400), 50);
    rest(c.near_b, c.p_near, 2, Side::SELL, P(110050), 50);

    Order* t = taker(c.p_t, 900, Side::SELL, P(300), 30);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'001, c.tid);
    EXPECT_EQ(r.filled_units, 30);
    EXPECT_EQ(remaining_qty_units(*t), 0);
    EXPECT_EQ(c.swap_b.live_orders(), 0u);
}

// --- implied-out --------------------------------------------------------------

TEST(ImpliedMatcher, ImpliedOutFarFromSwapPlusNear) {
    Curve c;
    c.implied_out();
    // implied far ask = swap_ask + near_ask = 500 + 110050 = 110550.
    rest(c.swap_b, c.p_swap, 1, Side::SELL, P(500), 80);
    rest(c.near_b, c.p_near, 2, Side::SELL, P(110050), 80);

    Order* t = taker(c.p_t, 900, Side::BUY, P(110600), 50);
    const auto r = c.m.match_incoming(kFar, *t, 1'700'000'002, c.tid);
    EXPECT_EQ(r.filled_units, 50);
    EXPECT_EQ(remaining_qty_units(*t), 0);
    EXPECT_EQ(c.swap_b.level(Side::SELL, 0)->total_qty_units, 30);
    EXPECT_EQ(c.near_b.level(Side::SELL, 0)->total_qty_units, 30);
    EXPECT_EQ(c.far_b.live_orders(), 0u);  // nothing rested
}

// --- multi-level quantity limiting + repricing --------------------------------

TEST(ImpliedMatcher, MultiLevelComboWalk) {
    Curve c;
    c.implied_in();
    // far asks: 110500 x20, then 110600 x100; near bid 110000 x100.
    // implied ask levels: 500 x20, then 600.
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 20);
    rest(c.far_b, c.p_far, 2, Side::SELL, P(110600), 100, /*acct=*/2);
    rest(c.near_b, c.p_near, 3, Side::BUY, P(110000), 100, /*acct=*/3);

    Order* t = taker(c.p_t, 900, Side::BUY, P(700), 50);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'003, c.tid);
    EXPECT_EQ(r.filled_units, 50);         // 20 @ implied 500 + 30 @ implied 600
    EXPECT_EQ(r.combos, 2u);               // repriced after level depletion
    EXPECT_EQ(r.fills, 4u);                // 2 makers per combo step
    EXPECT_EQ(remaining_qty_units(*t), 0);
    EXPECT_EQ(c.far_b.level(Side::SELL, 0)->price_ticks, P(110600));
    EXPECT_EQ(c.far_b.level(Side::SELL, 0)->total_qty_units, 70);
}

TEST(ImpliedMatcher, SmallestLegLimitsCombo) {
    Curve c;
    c.implied_in();
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 15);  // thin leg
    rest(c.near_b, c.p_near, 2, Side::BUY, P(110000), 100);

    Order* t = taker(c.p_t, 900, Side::BUY, P(600), 50);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'004, c.tid);
    EXPECT_EQ(r.filled_units, 15);  // capped by the thin far leg
    EXPECT_EQ(remaining_qty_units(*t), 35);  // remainder stays with the host
    EXPECT_EQ(c.far_b.live_orders(), 0u);
    EXPECT_EQ(c.near_b.level(Side::BUY, 0)->total_qty_units, 85);
}

// --- limits / no-cross ---------------------------------------------------------

TEST(ImpliedMatcher, LimitNotCrossedNoFill) {
    Curve c;
    c.implied_in();
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 100);
    rest(c.near_b, c.p_near, 2, Side::BUY, P(110000), 100);
    // implied ask 500; taker limit 400 → no fill, nothing mutates.
    Order* t = taker(c.p_t, 900, Side::BUY, P(400), 40);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'005, c.tid);
    EXPECT_EQ(r.filled_units, 0);
    EXPECT_EQ(r.combos, 0u);
    EXPECT_EQ(c.far_b.level(Side::SELL, 0)->total_qty_units, 100);
    EXPECT_FALSE(c.m.wal_fault());
}

TEST(ImpliedMatcher, MarketTakerUnbounded) {
    Curve c;
    c.implied_in();
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 100);
    rest(c.near_b, c.p_near, 2, Side::BUY, P(110000), 100);
    Order* t = taker(c.p_t, 900, Side::BUY, /*price*/0, 40,
                     /*acct=*/9, /*flags=*/0, OrderType::MARKET);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'006, c.tid);
    EXPECT_EQ(r.filled_units, 40);
}

// --- deterministic link ordering ------------------------------------------------

TEST(ImpliedMatcher, BetterLinkWinsThenTiebreakLinkId) {
    Curve c;
    c.implied_in(/*id=*/20);
    // A second implied-in link over the same legs (a venue could carry
    // e.g. a ratio-2 variant or an alternate tenor pairing — here the
    // duplicated shape proves link_id order, not registration order).
    c.implied_in(/*id=*/10);
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 100);
    rest(c.near_b, c.p_near, 2, Side::BUY, P(110000), 100);
    Order* t = taker(c.p_t, 900, Side::BUY, P(600), 30);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'007, c.tid);
    EXPECT_EQ(r.filled_units, 30);
    EXPECT_EQ(c.swap_b.live_orders(), 0u);
}

// --- post_only probe -------------------------------------------------------------

TEST(ImpliedMatcher, PostOnlyProbesWithoutMutation) {
    Curve c;
    c.implied_in();
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 100);
    rest(c.near_b, c.p_near, 2, Side::BUY, P(110000), 100);
    Order* t = taker(c.p_t, 900, Side::BUY, P(600), 40, /*acct=*/9,
                     kOrderFlagPostOnly);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'008, c.tid);
    EXPECT_TRUE(r.would_match);
    EXPECT_EQ(r.filled_units, 0);          // nothing executed
    EXPECT_EQ(t->filled_qty_units, 0);
    EXPECT_EQ(c.far_b.level(Side::SELL, 0)->total_qty_units, 100);
    // A post_only priced off-market reports false.
    Order* t2 = taker(c.p_t, 901, Side::BUY, P(100), 40, /*acct=*/9,
                      kOrderFlagPostOnly);
    const auto r2 = c.m.match_incoming(kSwap, *t2, 1'700'000'009, c.tid);
    EXPECT_FALSE(r2.would_match);
}

// --- evaluation (FOK/post_only dry-run path) ---------------------------------------

TEST(ImpliedMatcher, EvaluateIncomingFullDepth) {
    Curve c;
    c.implied_in();
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 20);
    rest(c.far_b, c.p_far, 2, Side::SELL, P(110600), 50, /*acct=*/2);
    rest(c.near_b, c.p_near, 3, Side::BUY, P(110000), 100, /*acct=*/3);
    // FOK feasibility: implied depth fills 70 of 80 inside limit 650 —
    // 20 @ implied 500 (far level 1), then all 50 @ implied 600
    // (far level 2 ≤ limit); the 10-unit shortfall stays unfillable.
    Order* t = taker(c.p_t, 900, Side::BUY, P(650), 80,
                     /*acct=*/9, /*flags=*/0, OrderType::LIMIT);
    t->tif = TimeInForce::FOK;
    const auto ev = c.m.evaluate_incoming(kSwap, *t);
    EXPECT_TRUE(ev.crossed);
    EXPECT_EQ(ev.best_price_ticks, P(500));
    EXPECT_EQ(ev.fillable_qty_units, 70);  // 20 @ 500 + 50 @ 600
    // Nothing mutated by the dry run.
    EXPECT_EQ(c.far_b.level(Side::SELL, 0)->total_qty_units, 20);
    EXPECT_EQ(t->filled_qty_units, 0);
}

// --- WAL discipline ---------------------------------------------------------------

TEST(ImpliedMatcher, WalJournalsBeforeEachFill) {
    Curve c;
    c.implied_in();
    const std::string base = "/tmp/exc_impl_" + std::to_string(::getpid());
    Wal w_far(base + "_far.wal", 2), w_near(base + "_near.wal", 1),
        w_swap(base + "_swap.wal", 3);
    ASSERT_EQ(w_far.open(), WalStatus::Ok);
    ASSERT_EQ(w_near.open(), WalStatus::Ok);
    ASSERT_EQ(w_swap.open(), WalStatus::Ok);
    WalWriter ww_far(&w_far), ww_near(&w_near), ww_swap(&w_swap);

    ImpliedMatcher m;
    MemoryPool<Order> p2{256};
    OrderBook far2{p2}, near2{p2}, swap2{p2};
    MemoryPool<Order> pt{256};
    ASSERT_TRUE(m.register_book(kFar, far2, &ww_far, nullptr));
    ASSERT_TRUE(m.register_book(kNear, near2, &ww_near, nullptr));
    ASSERT_TRUE(m.register_book(kSwap, swap2, &ww_swap, nullptr));
    ASSERT_TRUE(m.register_link(link(10, kSwap, kFar, +1, 1, kNear, -1, 1)));

    rest(far2, p2, 1, Side::SELL, P(110500), 100);
    rest(near2, p2, 2, Side::BUY, P(110000), 100);

    const uint64_t a_far = w_far.appends();
    const uint64_t a_near = w_near.appends();
    const uint64_t a_swap = w_swap.appends();
    uint64_t tid = 1;
    Order* t = taker(pt, 900, Side::BUY, P(600), 40);
    const auto r = m.match_incoming(kSwap, *t, 1'700'000'010, tid);
    EXPECT_EQ(r.filled_units, 40);
    // One TRADE per leg fill plus the out-book presentation record.
    EXPECT_EQ(w_far.appends(), a_far + 1);
    EXPECT_EQ(w_near.appends(), a_near + 1);
    EXPECT_EQ(w_swap.appends(), a_swap + 1);
    EXPECT_FALSE(m.wal_fault());
    w_far.close(); w_near.close(); w_swap.close();
    std::remove((base + "_far.wal").c_str());
    std::remove((base + "_near.wal").c_str());
    std::remove((base + "_swap.wal").c_str());
}

TEST(ImpliedMatcher, WalFaultHaltsCombo) {
    Curve c;
    c.implied_in();
    // Closed WAL → every write_trade fails → first leg fill aborts and
    // the sweep halts; nothing after the fault mutates.
    const std::string path = "/tmp/exc_impl_fault_" +
        std::to_string(::getpid()) + ".wal";
    std::remove(path.c_str());
    Wal wal(path, 2);
    WalWriter ww(&wal);  // never opened → append returns NotOpen
    ImpliedMatcher m;
    MemoryPool<Order> p2{256}, pt{256};
    OrderBook far2{p2}, near2{p2}, swap2{p2};
    ASSERT_TRUE(m.register_book(kFar, far2, &ww, nullptr));
    ASSERT_TRUE(m.register_book(kNear, near2, nullptr, nullptr));
    ASSERT_TRUE(m.register_book(kSwap, swap2, nullptr, nullptr));
    ASSERT_TRUE(m.register_link(link(10, kSwap, kFar, +1, 1, kNear, -1, 1)));
    rest(far2, p2, 1, Side::SELL, P(110500), 100);
    rest(near2, p2, 2, Side::BUY, P(110000), 100);
    uint64_t tid = 1;
    Order* t = taker(pt, 900, Side::BUY, P(600), 40);
    const auto r = m.match_incoming(kSwap, *t, 1'700'000'011, tid);
    EXPECT_TRUE(r.wal_fault);
    EXPECT_TRUE(m.wal_fault());
    EXPECT_EQ(r.filled_units, 0);
    // The fault halts before ANY mutation — legs untouched.
    EXPECT_EQ(far2.level(Side::SELL, 0)->total_qty_units, 100);
    EXPECT_EQ(near2.level(Side::BUY, 0)->total_qty_units, 100);
    EXPECT_EQ(t->filled_qty_units, 0);
}

// --- resting-order fills (book-change notification) --------------------------------

TEST(ImpliedMatcher, RestingSwapBidFillsAgainstImpliedAsk) {
    Curve c;
    c.implied_in();
    // Resting swap bid 550 waits; a far ask + near bid arrive producing
    // implied ask 500 ≤ 550 → the resting order fills at the implied
    // price (price improvement flows to the maker).
    rest(c.swap_b, c.p_swap, 1, Side::BUY, P(550), 40);
    rest(c.far_b, c.p_far, 2, Side::SELL, P(110500), 100, /*acct=*/2);
    rest(c.near_b, c.p_near, 3, Side::BUY, P(110000), 100, /*acct=*/3);

    const auto r = c.m.on_book_changed(kNear, 1'700'000'012, c.tid);
    EXPECT_EQ(r.filled_units, 40);
    EXPECT_EQ(c.swap_b.live_orders(), 0u);  // resting bid fully filled
    EXPECT_EQ(c.far_b.level(Side::SELL, 0)->total_qty_units, 60);
    EXPECT_EQ(c.near_b.level(Side::BUY, 0)->total_qty_units, 60);
}

TEST(ImpliedMatcher, RestingOutBidImpliedOut) {
    Curve c;
    c.implied_out();
    // implied far ask = swap_ask + near_ask = 500 + 110050 = 110550.
    rest(c.far_b, c.p_far, 1, Side::BUY, P(110600), 30);
    rest(c.swap_b, c.p_swap, 2, Side::SELL, P(500), 80, /*acct=*/2);
    rest(c.near_b, c.p_near, 3, Side::SELL, P(110050), 80, /*acct=*/3);
    const auto r = c.m.on_book_changed(kSwap, 1'700'000'013, c.tid);
    EXPECT_EQ(r.filled_units, 30);
    EXPECT_EQ(c.far_b.live_orders(), 0u);
}

TEST(ImpliedMatcher, NoRestingFillWhenNotCrossed) {
    Curve c;
    c.implied_in();
    rest(c.swap_b, c.p_swap, 1, Side::BUY, P(400), 40);
    rest(c.far_b, c.p_far, 2, Side::SELL, P(110500), 100, /*acct=*/2);
    rest(c.near_b, c.p_near, 3, Side::BUY, P(110000), 100, /*acct=*/3);
    const auto r = c.m.on_book_changed(kFar, 1'700'000'014, c.tid);
    EXPECT_EQ(r.filled_units, 0);
    EXPECT_EQ(c.swap_b.live_orders(), 1u);
}

// --- self-trade prefix exclusion -----------------------------------------------------

TEST(ImpliedMatcher, SelfAccountMakerTruncatesCapacity) {
    Curve c;
    c.implied_in();
    // FIFO prefix rule: the first same-account maker ends usable
    // capacity — orders behind it are unreachable for this aggressor.
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 30, /*acct=*/2);
    rest(c.far_b, c.p_far, 2, Side::SELL, P(110500), 70, /*acct=*/9);  // self
    rest(c.far_b, c.p_far, 3, Side::SELL, P(110500), 50, /*acct=*/3);
    rest(c.near_b, c.p_near, 4, Side::BUY, P(110000), 200, /*acct=*/4);
    Order* t = taker(c.p_t, 900, Side::BUY, P(600), 100, /*acct=*/9);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'015, c.tid);
    EXPECT_EQ(r.filled_units, 30);  // only the prefix before the self-maker
    EXPECT_EQ(remaining_qty_units(*t), 70);
    EXPECT_EQ(c.far_b.level(Side::SELL, 0)->total_qty_units, 120);
}

// --- feature-flag mirror ----------------------------------------------------------------

TEST(ImpliedMatcher, DisabledGateIsNoOp) {
    Curve c;
    c.implied_in();
    c.m.set_enabled(false);
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 100);
    rest(c.near_b, c.p_near, 2, Side::BUY, P(110000), 100);
    Order* t = taker(c.p_t, 900, Side::BUY, P(600), 40);
    const auto r = c.m.match_incoming(kSwap, *t, 1'700'000'016, c.tid);
    EXPECT_EQ(r.filled_units, 0);
    EXPECT_STREQ(r.reject, "IMPLIED_DISABLED");
    EXPECT_EQ(c.far_b.level(Side::SELL, 0)->total_qty_units, 100);
    EXPECT_FALSE(c.m.evaluate_incoming(kSwap, *t).crossed);
}

// --- no phantom resting orders anywhere -------------------------------------------------

TEST(ImpliedMatcher, NoPhantomRestingOrders) {
    Curve c;
    c.implied_in();
    c.implied_out();
    rest(c.far_b, c.p_far, 1, Side::SELL, P(110500), 100);
    rest(c.near_b, c.p_near, 2, Side::BUY, P(110000), 100);
    rest(c.swap_b, c.p_swap, 3, Side::SELL, P(400), 50, /*acct=*/2);
    rest(c.near_b, c.p_near, 4, Side::SELL, P(110050), 80, /*acct=*/3);
    Order* t = taker(c.p_t, 900, Side::BUY, P(600), 30);
    (void)c.m.match_incoming(kSwap, *t, 1'700'000'017, c.tid);
    // The implied mechanism consumes makers only — it never inserts.
    EXPECT_EQ(c.swap_b.live_orders(), 1u);   // the real resting sell stays
    EXPECT_EQ(c.near_b.live_orders(), 2u);
    EXPECT_EQ(c.far_b.live_orders(), 1u);
}
