// Task 2.3.2 — MatchingEngine coverage (spec §3.2/§3.7/§6.5):
// price-time priority, MARKET sweeps, FOK all-or-nothing, IOC remainder,
// STP default/actions, iceberg slice refresh, stop triggers, idempotent
// cancels, WAL-journal-per-mutation, amend, GTD expiry, determinism.

#include <gtest/gtest.h>

#include <atomic>
#include <cstdio>
#include <string>
#include <unordered_map>
#include <unistd.h>

#include <sys/mman.h>  // shm_unlink

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "ipc/IpcChannel.hpp"
#include "matching/IpcPublisher.hpp"
#include "matching/MatchingEngine.hpp"
#include "matching/WalWriter.hpp"
#include "risk/BilateralCreditMatrix.h"
#include "risk/RiskInterfaces.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"

using namespace exch;

namespace {

constexpr int64_t P(int64_t v) { return v; }  // ticks, 10^8 scale

// Deterministic ingress stamps — tests bump a shared counter.
uint64_t g_ts = 1'000'000'000;

Order* mk(MemoryPool<Order>& pool, uint64_t id, Side side, OrderType type,
          int64_t price, int64_t qty, uint64_t acct = 1,
          TimeInForce tif = TimeInForce::GTC,
          StpMode stp = StpMode::CANCEL_NEWEST, int64_t display = 0) {
    Order* o = pool.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = side;
    o->type = type;
    o->tif = tif;
    o->stp_mode = stp;
    o->price_ticks = price;
    o->qty_units = qty;
    o->quantity = Decimal::from_mantissa(qty);
    o->display_qty_units = display;
    o->timestamp_ns = ++g_ts;
    o->ingress_seq = g_ts;
    return o;
}

// Counting IPC channel — every send lands here.
struct CountingChannel final : IpcChannel {
    bool open() noexcept override { return true; }
    void close() noexcept override {}
    bool is_open() const noexcept override { return true; }
    bool send(const void*, uint32_t n) noexcept override {
        ++sends;
        bytes += n;
        return !fail_sends;
    }
    int32_t poll(void*, uint32_t) noexcept override { return 0; }
    uint64_t sends = 0;
    uint64_t bytes = 0;
    bool fail_sends = false;
};

int64_t level_qty(const OrderBook& b, Side s, int64_t price) {
    for (std::size_t d = 0; ; ++d) {
        const PriceLevel* l = b.level(s, d);
        if (l == nullptr) return 0;
        if (l->price_ticks == price) return l->total_qty_units;
    }
}

int level_count(const OrderBook& b, Side s) {
    int n = 0;
    while (b.level(s, n) != nullptr) ++n;
    return n;
}

struct Fixture {
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    MatchingEngine engine{3, book, pool, nullptr, nullptr};
};

}  // namespace

// --- Core matching ------------------------------------------------------------

TEST(MatchingEngine, ConstructDefaults) {
    Fixture f;
    EXPECT_EQ(f.engine.shard_id(), 3u);
    EXPECT_EQ(f.engine.received_count(), 0u);
    EXPECT_EQ(&f.engine.book(), &f.book);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(f.engine.last_reject(), nullptr);
}

TEST(MatchingEngine, LimitRestsWhenNotMarketable) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 100));
    const Order* o = f.book.find_order(1);
    ASSERT_NE(o, nullptr);
    EXPECT_EQ(remaining_qty_units(*o), 100);
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5000)), 100);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
}

TEST(MatchingEngine, LimitMatchPriceTimePriority) {
    Fixture f;
    // Two asks at the same price — FIFO: #1 (earlier ts) fills before #2.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 30));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, P(5000), 30));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::LIMIT, P(5000), 50, /*acct=*/2));

    EXPECT_EQ(f.engine.trades_emitted(), 2u);
    EXPECT_EQ(f.book.find_order(1), nullptr);          // #1 fully filled
    const Order* o2 = f.book.find_order(2);
    ASSERT_NE(o2, nullptr);
    EXPECT_EQ(o2->filled_qty_units, 20);               // #2 partially filled
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5000)), 10);
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(P(5000)));
}

TEST(MatchingEngine, MakerPriceWinsOverTakerPrice) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 100));
    // Aggressor pays 5100 — must still trade at the maker's 5000.
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5100), 40, /*acct=*/2));
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(P(5000)));
    EXPECT_EQ(remaining_qty_units(*f.book.find_order(1)), 60);
}

TEST(MatchingEngine, MarketWalksLevelsThenStops) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 10));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, P(5100), 10));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::SELL, OrderType::LIMIT, P(5200), 10));

    f.engine.on_order_received(
        mk(f.pool, 4, Side::BUY, OrderType::MARKET, 0, 25, /*acct=*/2));
    EXPECT_EQ(f.engine.trades_emitted(), 3u);
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5000)), 0);
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5100)), 0);
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5200)), 5);
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(P(5200)));

    // Oversized market order stops at empty book (remainder cancelled, no rest).
    f.engine.on_order_received(
        mk(f.pool, 5, Side::BUY, OrderType::MARKET, 0, 100, /*acct=*/2));
    EXPECT_EQ(level_count(f.book, Side::SELL), 0);
    EXPECT_EQ(f.book.find_order(5), nullptr);  // never rests
}

TEST(MatchingEngine, IocPartialFillCancelsRemainder) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 30));
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  P(5000), 80, 2, TimeInForce::IOC));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    // IOC remainder must not rest.
    EXPECT_EQ(f.book.find_order(2), nullptr);
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5000)), 0);
    EXPECT_EQ(f.engine.last_reject(), nullptr);  // IOC remainder is not a reject
}

TEST(MatchingEngine, FokAllOrNothing) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 40));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, P(5100), 40));

    // Book cannot cover 100 -> zero mutation (the asks are untouched).
    const uint64_t seq0 = f.book.book_seq();
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::LIMIT,
                                  P(5200), 100, 2, TimeInForce::FOK));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(f.book.book_seq(), seq0);
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5000)), 40);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectFokUnfilled);

    // Covered -> fills atomically across both levels.
    f.engine.on_order_received(mk(f.pool, 4, Side::BUY, OrderType::LIMIT,
                                  P(5100), 80, 2, TimeInForce::FOK));
    EXPECT_EQ(f.engine.trades_emitted(), 2u);
    EXPECT_EQ(level_count(f.book, Side::SELL), 0);
}

TEST(MatchingEngine, FokRejectedByLimitPrice) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 40));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, P(5100), 40));
    // Liquidity exists at 5000+5100 but the FOK cap is 5050.
    const uint64_t seq0 = f.book.book_seq();
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::LIMIT,
                                  P(5050), 80, 2, TimeInForce::FOK));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(f.book.book_seq(), seq0);
}

// --- STP -----------------------------------------------------------------------

TEST(MatchingEngine, StpDefaultCancelNewest) {
    Fixture f;
    f.engine.on_order_received(mk(f.pool, 1, Side::SELL, OrderType::LIMIT,
                                  P(5000), 50, /*acct=*/7));
    // Same account, default stp_mode (CANCEL_NEWEST) -> taker dies.
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 50, /*acct=*/7));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(remaining_qty_units(*f.book.find_order(1)), 50);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStpCancelled);
}

TEST(MatchingEngine, StpCancelOldest) {
    Fixture f;
    f.engine.on_order_received(mk(f.pool, 1, Side::SELL, OrderType::LIMIT,
                                  P(5000), 50, /*acct=*/7));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 20, /*acct=*/7,
           TimeInForce::GTC, StpMode::CANCEL_OLDEST));
    // Maker cancelled; taker proceeds — nothing left to cross, so it rests.
    EXPECT_EQ(f.book.find_order(1), nullptr);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    const Order* taker = f.book.find_order(2);
    ASSERT_NE(taker, nullptr);
    EXPECT_EQ(remaining_qty_units(*taker), 20);
}

TEST(MatchingEngine, StpTradeGroupMatch) {
    Fixture f;
    // Maker rests under group 42.
    OrderAux aux{};
    aux.trade_group_id = 42;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7), aux);
    // Different account but same nonzero group -> still a self-trade.
    OrderAux taker_aux{};
    taker_aux.trade_group_id = 42;
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 50, 9), taker_aux);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(remaining_qty_units(*f.book.find_order(1)), 50);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStpCancelled);
}

// --- Iceberg --------------------------------------------------------------------

TEST(MatchingEngine, IcebergVisibleSliceAndReplenish) {
    Fixture f;
    // 100-unit iceberg, 10-unit display slice.
    Order* ib = mk(f.pool, 1, Side::SELL, OrderType::ICEBERG, P(5000), 100, 1,
                   TimeInForce::GTC, StpMode::CANCEL_NEWEST, /*display=*/10);
    f.engine.on_order_received(ib);
    ASSERT_EQ(level_qty(f.book, Side::SELL, P(5000)), 10);  // only the slice

    // Taker eats 35 units: 10 (slice) + 10 + 10 + 5 — the last fill is a
    // PARTIAL slice fill, so the live slice shows 5 remaining (refresh only
    // fires on a full slice death).
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 35, /*acct=*/2));
    EXPECT_EQ(f.engine.trades_emitted(), 4u);
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5000)), 5);

    // Drain the rest (65 left): fills to exactly the original quantity.
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::LIMIT, P(5000), 100, /*acct=*/2));
    EXPECT_EQ(f.engine.trades_emitted(), 4u + 7u);
    EXPECT_EQ(level_count(f.book, Side::SELL), 0);
    EXPECT_EQ(f.engine.icebergs().size(), 0u);
    EXPECT_FALSE(f.engine.icebergs().contains(1));
}

TEST(MatchingEngine, IcebergCancelReleasesHidden) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::ICEBERG, P(5000), 100, 1,
           TimeInForce::GTC, StpMode::CANCEL_NEWEST, 10));
    f.engine.on_cancel_received(1, 1);
    EXPECT_EQ(f.book.find_order(1), nullptr);
    EXPECT_EQ(f.engine.icebergs().size(), 0u);
    EXPECT_EQ(level_count(f.book, Side::SELL), 0);
}

// --- Stop orders -----------------------------------------------------------------

TEST(MatchingEngine, StopTriggersWhenLastPriceCrosses) {
    Fixture f;
    // Resting asks the triggered stop will consume.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5100), 40));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, P(5200), 40));

    // Buy stop at 110 (in raw ticks 5100*1.02... keep integer): stop=5150.
    OrderAux stop_aux{};
    stop_aux.stop_price_ticks = P(5150);
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::STOP, 0, 50, 2), stop_aux);
    EXPECT_TRUE(f.engine.stops().pending(3));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);

    // A trade at 5100 (maker price) — below the stop — does not trigger.
    f.engine.on_order_received(
        mk(f.pool, 4, Side::BUY, OrderType::LIMIT, P(5140), 30, 3));
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(P(5100)));
    EXPECT_TRUE(f.engine.stops().pending(3));

    // Push last price to 5160 >= 5150: a 20-unit buy sweeps the remaining
    // 10@5100 then 10@5160 -> the stop fires as a market order mid-ingress.
    f.engine.on_order_received(
        mk(f.pool, 5, Side::SELL, OrderType::LIMIT, P(5160), 10));
    f.engine.on_order_received(
        mk(f.pool, 6, Side::BUY, OrderType::LIMIT, P(5160), 20, 3));
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(P(5200)));
    EXPECT_FALSE(f.engine.stops().pending(3));

    // The stop's 50 units consumed the whole ask side (40@5200 remainders
    // covered by the stop after its own trigger sweep).
    const Order* a2 = f.book.find_order(2);
    EXPECT_EQ(a2, nullptr);  // 5200 fully consumed by the triggered stop
    EXPECT_EQ(level_count(f.book, Side::SELL), 0);
}

TEST(MatchingEngine, StopLimitRestsAfterTrigger) {
    Fixture f;
    // Thin ask side: only 10 at 5000 — a triggered 50-qty stop-limit at 5050
    // fills 10 and rests the remainder as a 5050 bid.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 10));
    OrderAux aux{};
    aux.stop_price_ticks = P(4990);  // sell stop triggers on last <= 4990
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::STOP_LIMIT, P(4950), 50, 2), aux);
    EXPECT_TRUE(f.engine.stops().pending(2));

    // Maker at 4990: a buy lifts it -> last=4990 -> sell stop-limit triggers
    // -> sells down to 4950... no bids below -> rests as ask @4950.
    f.engine.on_order_received(
        mk(f.pool, 3, Side::SELL, OrderType::LIMIT, P(4990), 10));
    f.engine.on_order_received(
        mk(f.pool, 4, Side::BUY, OrderType::LIMIT, P(4990), 10, 3));
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(P(4990)));
    EXPECT_FALSE(f.engine.stops().pending(2));

    const Order* sl = f.book.find_order(2);
    ASSERT_NE(sl, nullptr);
    EXPECT_EQ(sl->price_ticks, P(4950));
    EXPECT_EQ(remaining_qty_units(*sl), 50);  // no bids to take
    // And it is now ahead of the old 5000 ask in price-time order.
    EXPECT_EQ(f.book.best_ask()->price_ticks, P(4950));
}

TEST(MatchingEngine, StopCancelWhilePending) {
    Fixture f;
    OrderAux aux{};
    aux.stop_price_ticks = P(5100);
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::STOP, 0, 10, 2), aux);
    ASSERT_TRUE(f.engine.stops().pending(1));
    const uint64_t seq0 = f.book.book_seq();
    f.engine.on_cancel_received(1, 2);
    EXPECT_FALSE(f.engine.stops().pending(1));
    EXPECT_EQ(f.book.book_seq(), seq0);  // pending stops never touch the book
}

// --- Cancel / amend / expiry ------------------------------------------------------

TEST(MatchingEngine, DoubleCancelIsIdempotent) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50));
    f.engine.on_cancel_received(1, 1);
    EXPECT_EQ(f.book.find_order(1), nullptr);
    const uint64_t seq_after = f.book.book_seq();
    const uint64_t rejects = f.engine.reject_count();
    // Second cancel: no mutation, just UNKNOWN_ORDER bookkeeping.
    f.engine.on_cancel_received(1, 1);
    f.engine.on_cancel_received(1, 1);
    EXPECT_EQ(f.book.book_seq(), seq_after);
    EXPECT_EQ(f.engine.reject_count(), rejects + 2);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectUnknownOrder);
    // Pool slot released once — alloc/free accounting stays sane.
    EXPECT_EQ(f.pool.remaining(), f.pool.capacity());
}

TEST(MatchingEngine, CancelForeignAccountRejected) {
    Fixture f;
    f.engine.on_order_received(mk(f.pool, 1, Side::SELL, OrderType::LIMIT,
                                  P(5000), 50, /*acct=*/7));
    f.engine.on_cancel_received(1, /*account=*/8);
    ASSERT_NE(f.book.find_order(1), nullptr);  // still resting
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectUnknownOrder);
}

TEST(MatchingEngine, AmendQtyDownKeepsPriorityAndFields) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, P(5000), 50));
    // Amend #2 down to 20 — keeps level FIFO position (book contract).
    f.engine.on_amend_received(2, 0, 20, 0, 999);
    const Order* o2 = f.book.find_order(2);
    ASSERT_NE(o2, nullptr);
    EXPECT_EQ(o2->qty_units, 20);
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5000)), 70);
    // #1 still heads the level.
    EXPECT_EQ(f.book.level(Side::SELL, 0)->head->id, 1u);
}

TEST(MatchingEngine, AmendRepriceMovesLevel) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50));
    f.engine.on_amend_received(1, P(5050), 0, 0, 999);
    const Order* o = f.book.find_order(1);
    ASSERT_NE(o, nullptr);
    EXPECT_EQ(o->price_ticks, P(5050));
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5000)), 0);
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5050)), 50);
    // Crossing amend rejected without mutation.
    f.engine.on_order_received(
        mk(f.pool, 9, Side::BUY, OrderType::LIMIT, P(4950), 10));
    const uint64_t seq1 = f.book.book_seq();
    f.engine.on_amend_received(9, P(5100), 0, 0, 1000);
    EXPECT_EQ(f.book.find_order(9)->price_ticks, P(4950));
    EXPECT_EQ(f.book.book_seq(), seq1);  // rejected — no mutation
}

TEST(MatchingEngine, GtdExpiresOnTimeTick) {
    Fixture f;
    OrderAux aux{};
    aux.gtd_expiry_ns = 2'000;  // expires at logical t=2000
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 1,
           TimeInForce::GTD),
        aux);
    ASSERT_NE(f.book.find_order(1), nullptr);

    f.engine.on_time_tick(1'999);
    ASSERT_NE(f.book.find_order(1), nullptr);
    f.engine.on_time_tick(2'000);
    EXPECT_EQ(f.book.find_order(1), nullptr);  // expired at t=2000
    EXPECT_EQ(level_count(f.book, Side::SELL), 0);
    EXPECT_EQ(f.engine.now_ns(), 2'000u);
}

TEST(MatchingEngine, TimeTickIsMonotone) {
    Fixture f;
    f.engine.on_time_tick(5000);
    f.engine.on_time_tick(4000);  // stale — clock must not rewind
    EXPECT_EQ(f.engine.now_ns(), 5000u);
}

// --- Determinism ------------------------------------------------------------------

namespace {
void scripted_book(MemoryPool<Order>& pool, MatchingEngine& e) {
    OrderAux a{};
    e.on_order_received(mk(pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 60));
    e.on_order_received(mk(pool, 2, Side::SELL, OrderType::LIMIT, P(5100), 60));
    e.on_order_received(mk(pool, 3, Side::BUY, OrderType::LIMIT, P(4900), 60));
    a.stop_price_ticks = P(5050);
    e.on_order_received(mk(pool, 4, Side::BUY, OrderType::STOP, 0, 25, 2), a);
    e.on_order_received(mk(pool, 5, Side::BUY, OrderType::LIMIT, P(5000), 40,
                           3));
    // Sweeps remaining 20@5000 + 10@5100 -> last=5100 fires the stop.
    e.on_order_received(mk(pool, 6, Side::BUY, OrderType::LIMIT, P(5100), 30,
                           3));
    e.on_cancel_received(2, 1);            // partially-filled ask cancelled
    e.on_amend_received(3, P(4950), 30, 0, 42);
    e.on_time_tick(9'000'000);
}
}  // namespace

TEST(MatchingEngine, DeterministicSameInputSameOutput) {
    g_ts = 1'000'000'000;
    MemoryPool<Order> p1{512};
    OrderBook b1{p1};
    MatchingEngine e1{3, b1, p1, nullptr, nullptr};
    scripted_book(p1, e1);

    g_ts = 1'000'000'000;
    MemoryPool<Order> p2{512};
    OrderBook b2{p2};
    MatchingEngine e2{3, b2, p2, nullptr, nullptr};
    scripted_book(p2, e2);

    EXPECT_EQ(e1.book_seq(), e2.book_seq());
    EXPECT_EQ(e1.trades_emitted(), e2.trades_emitted());
    EXPECT_EQ(e1.last_price_ticks(), e2.last_price_ticks());
    EXPECT_EQ(e1.received_count(), e2.received_count());
    EXPECT_EQ(b1.live_orders(), b2.live_orders());
    for (Side s : {Side::BUY, Side::SELL}) {
        for (std::size_t d = 0;; ++d) {
            const PriceLevel* l1 = b1.level(s, d);
            const PriceLevel* l2 = b2.level(s, d);
            ASSERT_EQ(l1 == nullptr, l2 == nullptr);
            if (l1 == nullptr) break;
            EXPECT_EQ(l1->price_ticks, l2->price_ticks);
            EXPECT_EQ(l1->total_qty_units, l2->total_qty_units);
            EXPECT_EQ(l1->order_count, l2->order_count);
        }
    }
}

// --- WAL + IPC wiring --------------------------------------------------------------

TEST(MatchingEngine, WalJournalsEveryMutation) {
    const std::string path =
        "/tmp/exc_me_wal_" + std::to_string(::getpid()) + ".wal";
    std::remove(path.c_str());
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    Wal wal(path, /*shard=*/3);
    ASSERT_EQ(wal.open(), WalStatus::Ok);
    WalWriter ww(&wal);
    MatchingEngine e(3, book, pool, &ww, nullptr);

    const uint64_t a0 = wal.appends();
    e.on_order_received(mk(pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 60));
    EXPECT_EQ(wal.appends(), a0 + 1);                       // ORDER_NEW

    e.on_order_received(mk(pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 40, 2));
    EXPECT_EQ(wal.appends(), a0 + 3);                       // NEW + TRADE

    e.on_cancel_received(1, 1);
    EXPECT_EQ(wal.appends(), a0 + 4);                       // ORDER_CANCEL

    e.on_cancel_received(1, 1);                             // idempotent: no log
    EXPECT_EQ(wal.appends(), a0 + 4);

    e.on_time_tick(7'777);
    EXPECT_EQ(wal.appends(), a0 + 5);                       // TIME_TICK
    wal.close();
    std::remove(path.c_str());
}

TEST(MatchingEngine, SeedTradeIdContinuesSequence) {
    Fixture f;
    f.engine.seed_trade_id(1000);
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 30));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 30, 2));
    EXPECT_EQ(f.engine.next_trade_id(), 1001u);
}

TEST(MatchingEngine, IpcPublishesFillsAndCancels) {
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    CountingChannel chan;
    IpcPublisher pub(&chan);
    MatchingEngine e(3, book, pool, nullptr, &pub);

    e.on_order_received(mk(pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 60));
    const uint64_t s0 = chan.sends;
    e.on_order_received(mk(pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 40, 2));
    // Trade fill publishes immediately; the depth snapshot is conflated —
    // it lands on the first on_time_tick past the coalesce window
    // (spec §10.2 conflation).
    EXPECT_GE(chan.sends, s0 + 1);
    e.on_time_tick(100'000'000);  // 100ms — beyond the 10ms coalesce window
    EXPECT_GE(chan.sends, s0 + 2);
    EXPECT_EQ(pub.published(), chan.sends);
    EXPECT_EQ(pub.drops(), 0u);
}

TEST(MatchingEngine, InvalidOrderRejectedBeforeBook) {
    Fixture f;
    Order* bad = mk(f.pool, 0, Side::BUY, OrderType::LIMIT, P(5000), 10);
    f.engine.on_order_received(bad);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectOrderInvalid);
    EXPECT_EQ(f.book.live_orders(), 0u);
    EXPECT_EQ(f.pool.remaining(), f.pool.capacity());
}

TEST(MatchingEngine, RiskHookRejectsBeforeBook) {
    Fixture f;
    f.engine.set_risk_hook(
        [](void*, Order&) noexcept -> const char* { return "RISK_REJECTED"; },
        nullptr);
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 10));
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectRiskRejected);
    EXPECT_EQ(f.book.live_orders(), 0u);
}

TEST(MatchingEngine, RiskHookPropagatesCode) {
    Fixture f;
    f.engine.set_risk_hook(
        [](void*, Order&) noexcept -> const char* { return "PRICE_OUT_OF_BAND"; },
        nullptr);
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 10));
    EXPECT_STREQ(f.engine.last_reject(), "PRICE_OUT_OF_BAND");
    EXPECT_EQ(f.book.live_orders(), 0u);
}

// --- Phase-3 Task 5 — bilateral credit consume_or_skip in the walk ----------
// Spec §3.3b / §24 #403: when a matrix + party map are bound and the taker
// maps to a party, every screened maker pair must debit mutual headroom;
// Skip preserves price-time priority and unscreened pairs proceed ungated.

namespace {

struct CreditMapStub final : IPartyMap {
    std::unordered_map<uint64_t, uint32_t> by_acct;
    [[nodiscard]] uint32_t credit_party_id(uint64_t a) const
        noexcept override {
        const auto it = by_acct.find(a);
        return it == by_acct.end() ? kCreditMaxParties : it->second;
    }
};

std::atomic<int> g_credit_shm_seq{0};

// Fresh shm matrix per test (unique name, unlinked on scope exit).
struct CreditFixture {
    std::string name;
    BilateralCreditMatrix matrix;
    CreditMapStub pmap;

    CreditFixture() : name(unique()) {
        std::string path = "/" + name;
        ::shm_unlink(path.c_str());
        EXPECT_TRUE(matrix.open(name, /*create=*/true));
    }
    ~CreditFixture() {
        matrix.close();
        ::shm_unlink(("/" + name).c_str());
    }
    static std::string unique() {
        char buf[96];
        std::snprintf(buf, sizeof(buf), "exch_mcm_%d_%d",
                      static_cast<int>(::getpid()),
                      g_credit_shm_seq.fetch_add(1));
        return buf;
    }
    // Mutual headroom between two parties.
    void grant(uint32_t a, uint32_t b, uint64_t ticks) {
        EXPECT_TRUE(matrix.apply_update(a, b, ticks));
        EXPECT_TRUE(matrix.apply_update(b, a, ticks));
    }
};

// Order args sized so qty*price/1e8 lands in the thousands: qty=1000
// units @ 1e9 ticks -> 10'000 quote notional.
constexpr int64_t kCreditPx = 1'000'000'000;

}  // namespace

TEST(MatchingEngineCredit, ScreenedPairFillsWithMutualCredit) {
    Fixture f;
    CreditFixture cm;
    cm.pmap.by_acct[1] = 1;
    cm.pmap.by_acct[2] = 2;
    cm.grant(1, 2, 20'000);
    f.engine.bind_credit(&cm.matrix, &cm.pmap);

    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kCreditPx, 1000));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, kCreditPx, 500,
           /*acct=*/2));

    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    // notional = 500 * 1e9 / 1e8 = 5'000 debited from BOTH directions.
    EXPECT_EQ(cm.matrix.credit_limit(1, 2), 15'000u);
    EXPECT_EQ(cm.matrix.credit_limit(2, 1), 15'000u);
    const Order* maker = f.book.find_order(1);
    ASSERT_NE(maker, nullptr);
    EXPECT_EQ(remaining_qty_units(*maker), 500);
    EXPECT_EQ(f.engine.last_reject(), nullptr);
}

TEST(MatchingEngineCredit, ExhaustedRemainderRejected) {
    Fixture f;
    CreditFixture cm;
    cm.pmap.by_acct[1] = 1;
    cm.pmap.by_acct[3] = 3;
    // No cells -> zero mutual credit between parties 1 and 3.
    f.engine.bind_credit(&cm.matrix, &cm.pmap);

    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kCreditPx, 1000));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, kCreditPx, 500,
           /*acct=*/3));

    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_STREQ(f.engine.last_reject(), "BILATERAL_CREDIT_EXHAUSTED");
    // Skip NEVER removes or reorders the resting order.
    const Order* maker = f.book.find_order(1);
    ASSERT_NE(maker, nullptr);
    EXPECT_EQ(remaining_qty_units(*maker), 1000);
    // The taker remainder died — nothing rested on the bid side.
    EXPECT_EQ(f.book.find_order(2), nullptr);
}

TEST(MatchingEngineCredit, SkipPreservesPriorityAndDescends) {
    Fixture f;
    CreditFixture cm;
    cm.pmap.by_acct[1] = 1;
    cm.pmap.by_acct[2] = 2;
    cm.pmap.by_acct[3] = 3;
    // Taker (party 3) has no credit with party 1, full credit with party 2.
    cm.grant(2, 3, 50'000);
    f.engine.bind_credit(&cm.matrix, &cm.pmap);

    // Better price level held by an uncredited maker; deeper level by a
    // credited one — the taker must skip #1 and fill vs #2 at ITS price.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kCreditPx, 1000));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, kCreditPx + 10'000,
           1000, /*acct=*/2));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::LIMIT, kCreditPx + 10'000, 500,
           /*acct=*/3));

    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    const Order* skipped = f.book.find_order(1);
    ASSERT_NE(skipped, nullptr);
    EXPECT_EQ(remaining_qty_units(*skipped), 1000);
    EXPECT_EQ(f.engine.last_price_ticks(),
              static_cast<uint64_t>(kCreditPx + 10'000));
    EXPECT_EQ(cm.matrix.credit_limit(2, 3), 45'000u);
}

TEST(MatchingEngineCredit, UnscreenedTakerUngated) {
    Fixture f;
    CreditFixture cm;
    cm.pmap.by_acct[1] = 1;  // maker screened; taker acct 9 unmapped.
    f.engine.bind_credit(&cm.matrix, &cm.pmap);

    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kCreditPx, 1000));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, kCreditPx, 500,
           /*acct=*/9));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.last_reject(), nullptr);
}

TEST(MatchingEngineCredit, UnscreenedMakerProceeds) {
    Fixture f;
    CreditFixture cm;
    cm.pmap.by_acct[3] = 3;  // taker screened; maker acct 9 unmapped.
    f.engine.bind_credit(&cm.matrix, &cm.pmap);

    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kCreditPx, 1000,
           /*acct=*/9));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, kCreditPx, 500,
           /*acct=*/3));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.last_reject(), nullptr);
}

TEST(MatchingEngineCredit, DebitDrainsThenExhausts) {
    Fixture f;
    CreditFixture cm;
    cm.pmap.by_acct[1] = 1;
    cm.pmap.by_acct[3] = 3;
    // One fill's worth of mutual credit only (5'000): the first 500-unit
    // print drains both cells; the second maker then skips and the taker
    // remainder must die — never partially rest against uncredited flow.
    cm.grant(1, 3, 5'000);
    f.engine.bind_credit(&cm.matrix, &cm.pmap);

    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kCreditPx, 500));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, kCreditPx, 500));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::LIMIT, kCreditPx, 1000,
           /*acct=*/3));

    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_STREQ(f.engine.last_reject(), "BILATERAL_CREDIT_EXHAUSTED");
    EXPECT_EQ(f.book.find_order(1), nullptr);  // filled away
    const Order* second = f.book.find_order(2);
    ASSERT_NE(second, nullptr);
    EXPECT_EQ(remaining_qty_units(*second), 500);  // skipped, intact
    EXPECT_EQ(cm.matrix.credit_limit(1, 3), 0u);
    EXPECT_EQ(cm.matrix.credit_limit(3, 1), 0u);
}

TEST(MatchingEngineCredit, CreditSkippedHiddenDoesNotLeakMidpoint) {
    // Regression (2026-10-02): a HIDDEN maker that passes the midpoint
    // gate sets eff_px=mid, then fails consume_or_skip — the NEXT visible
    // member of the level must still fill at the level price, not the
    // stale mid the hidden member left behind.
    Fixture f;
    CreditFixture cm;
    cm.pmap.by_acct[1] = 1;  // hidden maker -> party 1
    cm.pmap.by_acct[2] = 2;  // visible maker -> party 2
    cm.pmap.by_acct[3] = 3;  // taker        -> party 3
    cm.grant(2, 3, 50'000);  // taker has credit with party 2 only
    f.engine.bind_credit(&cm.matrix, &cm.pmap);

    // Visible bid establishes the midpoint against the visible ask:
    // mid = ((kCreditPx-20000) + kCreditPx)/2 = kCreditPx-10000.
    f.engine.on_order_received(
        mk(f.pool, 90, Side::BUY, OrderType::LIMIT, kCreditPx - 20'000,
           100, /*acct=*/9));
    Order* hid =
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kCreditPx, 1000);
    ASSERT_NE(hid, nullptr);
    hid->flags |= kOrderFlagHidden;
    f.engine.on_order_received(hid);  // party 1 — uncredited vs taker
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, kCreditPx, 1000,
           /*acct=*/2));  // same level, behind the hidden member

    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::LIMIT, kCreditPx, 500,
           /*acct=*/3));

    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    // Fill prints at the maker LEVEL, never the stale hidden midpoint.
    EXPECT_EQ(f.engine.last_price_ticks(),
              static_cast<uint64_t>(kCreditPx));
    const Order* skipped = f.book.find_order(1);
    ASSERT_NE(skipped, nullptr);
    EXPECT_EQ(remaining_qty_units(*skipped), 1000);  // hidden intact
    const Order* filled = f.book.find_order(2);
    ASSERT_NE(filled, nullptr);
    EXPECT_EQ(remaining_qty_units(*filled), 500);
}
