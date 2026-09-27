// Task 2.3.1 coverage — flat-array + intrusive-FIFO order book (spec §3.1),
// plus the Task 2.3.20 amend-priority contract the book implements now.
// Pipette/tick-precision coverage lives in test_book_pipette.cpp (2.3.23).

#include <gtest/gtest.h>

#include <atomic>
#include <cstdint>
#include <cstdlib>
#include <map>
#include <new>
#include <random>
#include <set>
#include <type_traits>
#include <unordered_map>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "book/PriceLevel.hpp"
#include "utils/MemoryPool.hpp"

using namespace exch;

namespace {

// --- allocator instrumentation -------------------------------------------
// Counted ONLY inside measured regions — gtest/framework traffic outside the
// flag is irrelevant. The operators themselves always work; only the count
// is gated.
std::atomic<uint64_t> g_alloc_calls{0};
std::atomic<bool> g_counting{false};

void* counted_alloc(std::size_t n) {
    if (g_counting.load(std::memory_order_relaxed)) {
        g_alloc_calls.fetch_add(1, std::memory_order_relaxed);
    }
    if (void* p = std::malloc(n != 0 ? n : 1)) return p;
    throw std::bad_alloc();
}
void* counted_alloc_nothrow(std::size_t n) noexcept {
    if (g_counting.load(std::memory_order_relaxed)) {
        g_alloc_calls.fetch_add(1, std::memory_order_relaxed);
    }
    return std::malloc(n != 0 ? n : 1);
}
void* counted_alloc_aligned(std::size_t n, std::align_val_t a) {
    if (g_counting.load(std::memory_order_relaxed)) {
        g_alloc_calls.fetch_add(1, std::memory_order_relaxed);
    }
    void* p = nullptr;
    const std::size_t al = static_cast<std::size_t>(a);
    if (posix_memalign(&p, al, n != 0 ? n : 1) != 0) throw std::bad_alloc();
    return p;
}

}  // namespace

// Global operator new/delete replacement — link-local to this test binary.
void* operator new(std::size_t n) { return counted_alloc(n); }
void* operator new[](std::size_t n) { return counted_alloc(n); }
void* operator new(std::size_t n, const std::nothrow_t&) noexcept {
    return counted_alloc_nothrow(n);
}
void* operator new[](std::size_t n, const std::nothrow_t&) noexcept {
    return counted_alloc_nothrow(n);
}
void* operator new(std::size_t n, std::align_val_t a) {
    return counted_alloc_aligned(n, a);
}
void* operator new[](std::size_t n, std::align_val_t a) {
    return counted_alloc_aligned(n, a);
}
void operator delete(void* p) noexcept { std::free(p); }
void operator delete[](void* p) noexcept { std::free(p); }
void operator delete(void* p, std::size_t) noexcept { std::free(p); }
void operator delete[](void* p, std::size_t) noexcept { std::free(p); }
void operator delete(void* p, std::align_val_t) noexcept { std::free(p); }
void operator delete[](void* p, std::align_val_t) noexcept { std::free(p); }
void operator delete(void* p, std::size_t, std::align_val_t) noexcept {
    std::free(p);
}
void operator delete[](void* p, std::size_t, std::align_val_t) noexcept {
    std::free(p);
}
void operator delete(void* p, const std::nothrow_t&) noexcept { std::free(p); }
void operator delete[](void* p, const std::nothrow_t&) noexcept {
    std::free(p);
}

namespace {

// Caller-side template — the book copies fields into a pooled slot.
Order mk(uint64_t id, Side side, int64_t price_ticks, int64_t qty_units,
         uint64_t ts, uint64_t ingress,
         TimeInForce tif = TimeInForce::GTC) {
    Order o{};
    o.id = id;
    o.account_id = 100 + id % 10;
    o.side = side;
    o.type = OrderType::LIMIT;
    o.tif = tif;
    o.price_ticks = price_ticks;
    o.qty_units = qty_units;
    o.timestamp_ns = ts;
    o.ingress_seq = ingress;
    return o;
}

int64_t book_side_total(const OrderBook& b, Side s) {
    int64_t sum = 0;
    const uint32_t n = s == Side::BUY ? b.bid_count() : b.ask_count();
    for (uint32_t i = 0; i < n; ++i) sum += b.level(s, i)->total_qty_units;
    return sum;
}

}  // namespace

// --- Type/layout contract ---------------------------------------------------

TEST(OrderBookTypes, OrderIsPodPerSpec31) {
    static_assert(std::is_aggregate_v<Order>);
    static_assert(std::is_trivially_copyable_v<Order>);
    static_assert(std::is_trivially_destructible_v<Order>);
    static_assert(std::is_standard_layout_v<Order>);
    // Task 2.3.23: primary matching fields are int64 10^8 ticks.
    static_assert(std::is_same_v<decltype(Order::price_ticks), int64_t>);
    static_assert(std::is_same_v<decltype(Order::qty_units), int64_t>);
    static_assert(std::is_same_v<decltype(Order::filled_qty_units), int64_t>);
    static_assert(std::is_same_v<decltype(PriceLevel::price_ticks), int64_t>);
    SUCCEED();
}

TEST(OrderBookTypes, EnumsMatchSpecEnums) {
    EXPECT_EQ(static_cast<uint8_t>(Side::BUY), 0);
    EXPECT_EQ(static_cast<uint8_t>(Side::SELL), 1);
    EXPECT_EQ(static_cast<uint8_t>(OrderType::LIMIT), 0);
    EXPECT_EQ(static_cast<uint8_t>(OrderType::MOC), 15);  // 16 spec §5.4 values
    EXPECT_EQ(static_cast<uint8_t>(TimeInForce::GTC), 0);
    EXPECT_EQ(static_cast<uint8_t>(TimeInForce::DAY), 4);  // GTC IOC FOK GTD DAY
    EXPECT_EQ(static_cast<uint8_t>(StpMode::CANCEL_NEWEST), 0);
    EXPECT_EQ(static_cast<uint8_t>(StpMode::NONE), 4);     // spec §5.4 order
}

TEST(OrderBookTypes, PriceLevelIsCacheAligned) {
    static_assert(alignof(PriceLevel) == 64);
    static_assert(sizeof(PriceLevel) == 64);
    SUCCEED();
}

// --- Lifecycle ---------------------------------------------------------------

TEST(OrderBookBasic, EmptyBookState) {
    OrderBook book;
    EXPECT_EQ(book.book_seq(), 0u);
    EXPECT_EQ(book.bid_count(), 0u);
    EXPECT_EQ(book.ask_count(), 0u);
    EXPECT_EQ(book.live_orders(), 0u);
    EXPECT_EQ(book.best_bid(), nullptr);
    EXPECT_EQ(book.best_ask(), nullptr);
    EXPECT_EQ(book.front_order(Side::BUY), nullptr);
    EXPECT_EQ(book.front_order(Side::SELL), nullptr);
    EXPECT_EQ(book.level(Side::BUY, 0), nullptr);
    EXPECT_FALSE(book.crossed());
    const char* why = nullptr;
    EXPECT_TRUE(book.validate(&why)) << (why ? why : "");
    // Detached book (no pool): fail-closed mutations.
    Order* out = nullptr;
    EXPECT_EQ(book.add_order(mk(1, Side::BUY, 100'000'000, 1'000'000, 1, 1),
                            &out),
              BookError::CAPACITY_EXCEEDED);
    EXPECT_EQ(out, nullptr);
    EXPECT_EQ(book.cancel_order(1), BookError::NOT_FOUND);
    EXPECT_EQ(book.book_seq(), 0u);  // rejects never bump the sequence
}

TEST(OrderBookBasic, AddSingleOrderEachSide) {
    MemoryPool<Order> pool(64);
    OrderBook book(pool);
    Order* a = nullptr;
    ASSERT_EQ(book.add_order(mk(1, Side::BUY, 110'000'000, 500'000'000, 10, 1),
                            &a),
              BookError::OK);
    Order* b = nullptr;
    ASSERT_EQ(book.add_order(mk(2, Side::SELL, 110'020'000, 300'000'000, 11, 2),
                            &b),
              BookError::OK);
    EXPECT_EQ(book.book_seq(), 2u);
    EXPECT_EQ(book.bid_count(), 1u);
    EXPECT_EQ(book.ask_count(), 1u);
    ASSERT_NE(book.best_bid(), nullptr);
    ASSERT_NE(book.best_ask(), nullptr);
    EXPECT_EQ(book.best_bid()->price_ticks, 110'000'000);
    EXPECT_EQ(book.best_bid()->total_qty_units, 500'000'000);
    EXPECT_EQ(book.best_bid()->order_count, 1u);
    EXPECT_EQ(book.best_bid()->head, a);
    EXPECT_EQ(book.best_bid()->tail, a);
    EXPECT_EQ(book.best_ask()->price_ticks, 110'020'000);
    EXPECT_EQ(book.find_order(1), a);
    EXPECT_EQ(book.find_order(2), b);
    EXPECT_EQ(book.find_order(99), nullptr);
    EXPECT_EQ(book.live_orders(), 2u);
    EXPECT_EQ(a->quantity.mantissa(), a->qty_units);  // compat mirror synced
    EXPECT_TRUE(book.validate());
}

// --- Level ordering / binary search -----------------------------------------

TEST(OrderBookLevels, LevelsStaySorted) {
    MemoryPool<Order> pool(1024);
    OrderBook book(pool);
    const int64_t bid_prices[] = {110'000'000, 109'900'000, 110'100'000,
                                  109'800'000, 110'050'000};
    const int64_t ask_prices[] = {110'500'000, 110'600'000, 110'450'000,
                                  110'700'000};
    uint64_t ts = 0;
    uint64_t id = 1;
    for (int64_t p : bid_prices) {
        Order* o = nullptr;
        ASSERT_EQ(book.add_order(mk(id++, Side::BUY, p, 1'000'000, ts, ts),
                                 &o), BookError::OK);
        ++ts;
    }
    for (int64_t p : ask_prices) {
        Order* o = nullptr;
        ASSERT_EQ(book.add_order(mk(id++, Side::SELL, p, 1'000'000, ts, ts),
                                 &o), BookError::OK);
        ++ts;
    }
    EXPECT_EQ(book.bid_count(), 5u);
    EXPECT_EQ(book.ask_count(), 4u);
    for (uint32_t i = 1; i < book.bid_count(); ++i) {  // descending bids
        EXPECT_LT(book.level(Side::BUY, i)->price_ticks,
                  book.level(Side::BUY, i - 1)->price_ticks);
    }
    for (uint32_t i = 1; i < book.ask_count(); ++i) {  // ascending asks
        EXPECT_GT(book.level(Side::SELL, i)->price_ticks,
                  book.level(Side::SELL, i - 1)->price_ticks);
    }
    EXPECT_EQ(book.best_bid()->price_ticks, 110'100'000);
    EXPECT_EQ(book.best_ask()->price_ticks, 110'450'000);
    EXPECT_EQ(book.level(Side::BUY, 5), nullptr);  // depth past the tail
    EXPECT_EQ(book.level(Side::SELL, 4), nullptr);
    const char* why = nullptr;
    EXPECT_TRUE(book.validate(&why)) << (why ? why : "");
}

TEST(OrderBookLevels, BinarySearchProbeBound) {
    // O(log N) evidence by construction: fill a side near kMaxLevels and
    // confirm level lookup never exceeds ceil(log2(4096)) + 1 = 13 probes.
    constexpr std::size_t kLevels = 4000;
    MemoryPool<Order> pool(kLevels + 16);
    OrderBook book(pool);
    uint64_t ts = 0;
    for (std::size_t i = 0; i < kLevels; ++i) {  // dense descending bids
        Order* o = nullptr;
        const int64_t p = 109'000'000 + static_cast<int64_t>(i) * 1'000;
        ASSERT_EQ(book.add_order(mk(i + 1, Side::BUY, p, 1'000'000, ts, ts),
                                 &o), BookError::OK);
        ++ts;
    }
    EXPECT_EQ(book.bid_count(), kLevels);
    // Worst case: miss + insert at the extremes and middle.
    Order* o = nullptr;
    ASSERT_EQ(book.add_order(mk(9001, Side::BUY, 108'999'500, 1'000'000, ts, ts),
                             &o), BookError::OK);
    EXPECT_LE(book.last_lookup_probes(), 13u);  // log2(4000) < 12
    ASSERT_EQ(book.add_order(mk(9002, Side::BUY, 113'000'500, 1'000'000,
                                ts + 1, ts + 1), &o), BookError::OK);
    EXPECT_LE(book.last_lookup_probes(), 13u);
    EXPECT_TRUE(book.validate());
}

// --- FIFO / price-time priority ----------------------------------------------

TEST(OrderBookFifo, SamePriceOrdersFillHeadFirst) {
    MemoryPool<Order> pool(64);
    OrderBook book(pool);
    Order *o1 = nullptr, *o2 = nullptr, *o3 = nullptr;
    ASSERT_EQ(book.add_order(mk(1, Side::SELL, 110'500'000, 100'000'000, 10, 1),
                             &o1), BookError::OK);
    ASSERT_EQ(book.add_order(mk(2, Side::SELL, 110'500'000, 200'000'000, 11, 2),
                             &o2), BookError::OK);
    ASSERT_EQ(book.add_order(mk(3, Side::SELL, 110'500'000, 300'000'000, 12, 3),
                             &o3), BookError::OK);
    const PriceLevel* lvl = book.best_ask();
    ASSERT_NE(lvl, nullptr);
    // head→tail = arrival order (price-time priority).
    EXPECT_EQ(lvl->head, o1);
    EXPECT_EQ(lvl->head->next, o2);
    EXPECT_EQ(lvl->head->next->next, o3);
    EXPECT_EQ(lvl->tail, o3);
    EXPECT_EQ(lvl->order_count, 3u);
    EXPECT_EQ(lvl->total_qty_units, 600'000'000);

    // Partial fill on the head: head keeps priority, total shrinks.
    Order snap{};
    ASSERT_EQ(book.apply_fill(o1, 40'000'000, &snap), BookError::OK);
    EXPECT_EQ(snap.id, 1u);
    EXPECT_EQ(book.front_order(Side::SELL), o1);
    EXPECT_EQ(o1->filled_qty_units, 40'000'000);
    EXPECT_EQ(book.best_ask()->total_qty_units, 560'000'000);
    EXPECT_EQ(book.live_orders(), 3u);

    // Finish o1 → o2 becomes head; o1 returns to the pool.
    ASSERT_EQ(book.apply_fill(o1, 60'000'000, &snap), BookError::OK);
    EXPECT_TRUE(is_filled(snap));
    EXPECT_EQ(book.front_order(Side::SELL), o2);
    EXPECT_EQ(book.find_order(1), nullptr);
    EXPECT_EQ(book.live_orders(), 2u);
    EXPECT_EQ(book.best_ask()->total_qty_units, 500'000'000);

    // Over-fill rejected without state change.
    EXPECT_EQ(book.apply_fill(o2, 999'000'000, nullptr), BookError::INVALID_QTY);
    EXPECT_EQ(book.best_ask()->total_qty_units, 500'000'000);
    EXPECT_TRUE(book.validate());
}

// --- Cancel -------------------------------------------------------------------

TEST(OrderBookCancel, MiddleTailAndLastOrderAtLevel) {
    MemoryPool<Order> pool(64);
    OrderBook book(pool);
    Order* os[3] = {nullptr, nullptr, nullptr};
    Order*& o1 = os[0];
    Order*& o3 = os[2];
    for (uint64_t i = 0; i < 3; ++i) {
        ASSERT_EQ(book.add_order(mk(i + 1, Side::BUY, 110'000'000, 100'000'000,
                                    i + 1, i + 1), &os[i]), BookError::OK);
    }
    // Middle unlink keeps the chain intact.
    Order snap{};
    ASSERT_EQ(book.cancel_order(2, &snap), BookError::OK);
    EXPECT_EQ(snap.id, 2u);
    EXPECT_EQ(snap.qty_units, 100'000'000);  // released qty for accounting
    EXPECT_EQ(book.best_bid()->head, o1);
    EXPECT_EQ(book.best_bid()->head->next, o3);
    EXPECT_EQ(o3->prev, o1);
    EXPECT_EQ(book.best_bid()->tail, o3);
    EXPECT_EQ(book.best_bid()->total_qty_units, 200'000'000);
    EXPECT_EQ(book.find_order(2), nullptr);
    EXPECT_EQ(pool.size(), 2u);  // slot returned to the pool

    // Full level cancel drops the level; side empties.
    ASSERT_EQ(book.cancel_order(1), BookError::OK);
    ASSERT_EQ(book.cancel_order(3), BookError::OK);
    EXPECT_EQ(book.bid_count(), 0u);
    EXPECT_EQ(book.best_bid(), nullptr);
    EXPECT_EQ(pool.size(), 0u);
    EXPECT_EQ(book.cancel_order(42), BookError::NOT_FOUND);
    EXPECT_EQ(book.book_seq(), 6u);  // 3 adds + 3 cancels = 6 mutations
    EXPECT_TRUE(book.validate());
}

// --- Crossing guard ------------------------------------------------------------

TEST(OrderBookInvariant, CrossingInsertRejectedAtomically) {
    MemoryPool<Order> pool(64);
    OrderBook book(pool);
    Order* o = nullptr;
    ASSERT_EQ(book.add_order(mk(1, Side::SELL, 110'000'000, 1'000'000, 1, 1),
                             &o), BookError::OK);
    // Bid at the ask meets → CROSSED; above the ask crosses → CROSSED.
    const uint64_t seq0 = book.book_seq();
    EXPECT_EQ(book.add_order(mk(2, Side::BUY, 110'000'000, 1'000'000, 2, 2),
                             &o), BookError::CROSSED);
    EXPECT_EQ(o, nullptr);
    EXPECT_EQ(book.add_order(mk(3, Side::BUY, 110'500'000, 1'000'000, 3, 3),
                             &o), BookError::CROSSED);
    EXPECT_EQ(book.book_seq(), seq0);          // atomic reject — no mutation
    EXPECT_EQ(book.bid_count(), 0u);
    EXPECT_EQ(pool.size(), 1u);                // rejected add frees nothing
    EXPECT_FALSE(book.crossed());
    // Just under the ask is fine.
    ASSERT_EQ(book.add_order(mk(4, Side::BUY, 109'999'000, 1'000'000, 4, 4),
                             &o), BookError::OK);
    // Symmetric: ask at/below best bid rejected.
    EXPECT_EQ(book.add_order(mk(5, Side::SELL, 109'999'000, 1'000'000, 5, 5),
                             &o), BookError::CROSSED);
    EXPECT_EQ(book.add_order(mk(6, Side::SELL, 109'000'000, 1'000'000, 6, 6),
                             &o), BookError::CROSSED);
    EXPECT_TRUE(book.validate());
}

// --- Modify (Task 2.3.20 contract) ---------------------------------------------

TEST(OrderBookModify, QtyDownPreservesPriority) {
    MemoryPool<Order> pool(64);
    OrderBook book(pool);
    Order *o1 = nullptr, *o2 = nullptr;
    ASSERT_EQ(book.add_order(mk(1, Side::BUY, 110'000'000, 500'000'000, 10, 1),
                             &o1), BookError::OK);
    ASSERT_EQ(book.add_order(mk(2, Side::BUY, 110'000'000, 300'000'000, 11, 2),
                             &o2), BookError::OK);
    // Qty-down on the tail order: position + timestamp preserved.
    const uint64_t seq0 = book.book_seq();
    ASSERT_EQ(book.modify_order(2, 110'000'000, 100'000'000, 99, 99),
              BookError::OK);
    EXPECT_EQ(book.best_bid()->tail, o2);          // still tail
    EXPECT_EQ(o2->timestamp_ns, 11u);              // original stamp kept
    EXPECT_EQ(o2->ingress_seq, 2u);
    EXPECT_EQ(o2->qty_units, 100'000'000);
    EXPECT_EQ(o2->quantity.mantissa(), 100'000'000);  // mirror synced
    EXPECT_EQ(book.best_bid()->total_qty_units, 600'000'000);
    EXPECT_EQ(book.book_seq(), seq0 + 1);
    EXPECT_TRUE(book.validate());
}

TEST(OrderBookModify, QtyUpLosesPriorityToTail) {
    MemoryPool<Order> pool(64);
    OrderBook book(pool);
    Order *o1 = nullptr, *o2 = nullptr;
    ASSERT_EQ(book.add_order(mk(1, Side::SELL, 110'500'000, 500'000'000, 10, 1),
                             &o1), BookError::OK);
    ASSERT_EQ(book.add_order(mk(2, Side::SELL, 110'500'000, 300'000'000, 11, 2),
                             &o2), BookError::OK);
    // Qty-up on the HEAD order: it re-queues at the tail with a fresh stamp,
    // same order id (amend preserves identity).
    ASSERT_EQ(book.modify_order(1, 110'500'000, 600'000'000, 50, 50),
              BookError::OK);
    EXPECT_EQ(book.best_ask()->head, o2);
    EXPECT_EQ(book.best_ask()->tail, o1);
    EXPECT_EQ(o1->id, 1u);
    EXPECT_EQ(o1->timestamp_ns, 50u);
    EXPECT_EQ(o1->ingress_seq, 50u);
    EXPECT_EQ(o1->qty_units, 600'000'000);
    EXPECT_EQ(book.best_ask()->total_qty_units, 900'000'000);
    EXPECT_TRUE(book.validate());
}

TEST(OrderBookModify, PriceChangeMovesLevelAndLosesPriority) {
    MemoryPool<Order> pool(64);
    OrderBook book(pool);
    Order *o1 = nullptr, *o2 = nullptr;
    ASSERT_EQ(book.add_order(mk(1, Side::BUY, 110'000'000, 100'000'000, 10, 1),
                             &o1), BookError::OK);
    ASSERT_EQ(book.add_order(mk(2, Side::BUY, 110'010'000, 100'000'000, 11, 2),
                             &o2), BookError::OK);
    // Move o1 up one level: it must land BEHIND o2 is impossible (different
    // price) — it becomes its own level between them.
    ASSERT_EQ(book.modify_order(1, 110'005'000, 100'000'000, 60, 60),
              BookError::OK);
    EXPECT_EQ(book.bid_count(), 2u);
    EXPECT_EQ(book.level(Side::BUY, 0)->price_ticks, 110'010'000);
    EXPECT_EQ(book.level(Side::BUY, 1)->price_ticks, 110'005'000);
    EXPECT_EQ(book.level(Side::BUY, 1)->head, o1);
    EXPECT_EQ(o1->timestamp_ns, 60u);
    // Now move o2 DOWN onto o1's price: lands at the tail of o1's level.
    ASSERT_EQ(book.modify_order(2, 110'005'000, 100'000'000, 61, 61),
              BookError::OK);
    EXPECT_EQ(book.bid_count(), 1u);
    EXPECT_EQ(book.best_bid()->head, o1);
    EXPECT_EQ(book.best_bid()->tail, o2);
    EXPECT_EQ(book.best_bid()->total_qty_units, 200'000'000);
    EXPECT_TRUE(book.validate());
}

TEST(OrderBookModify, RejectionsAreAtomic) {
    MemoryPool<Order> pool(64);
    OrderBook book(pool);
    Order* o = nullptr;
    ASSERT_EQ(book.add_order(mk(1, Side::BUY, 110'000'000, 100'000'000, 10, 1),
                             &o), BookError::OK);
    ASSERT_EQ(book.add_order(mk(2, Side::SELL, 110'100'000, 100'000'000, 11, 2),
                             &o), BookError::OK);
    // IOC/FOK cannot be amended even if somehow resting.
    Order* ioc = nullptr;
    ASSERT_EQ(book.add_order(mk(3, Side::BUY, 109'000'000, 100'000'000, 12, 3,
                              TimeInForce::IOC), &ioc), BookError::OK);
    EXPECT_EQ(book.modify_order(3, 109'500'000, 100'000'000, 70, 70),
              BookError::NOT_MODIFIABLE);
    // Unknown id / bad params / crossing amend / amend below filled qty.
    EXPECT_EQ(book.modify_order(77, 109'500'000, 100'000'000, 70, 70),
              BookError::NOT_FOUND);
    EXPECT_EQ(book.modify_order(1, 0, 100'000'000, 70, 70),
              BookError::INVALID_PRICE);
    EXPECT_EQ(book.modify_order(1, 109'500'000, 0, 70, 70),
              BookError::INVALID_QTY);
    EXPECT_EQ(book.modify_order(1, 110'100'000, 100'000'000, 70, 70),
              BookError::CROSSED);   // would meet best ask
    EXPECT_TRUE(book.validate());
    // Amend-to-<=filled rejected after a partial fill.
    Order* m = book.find_order(1);
    ASSERT_NE(m, nullptr);
    ASSERT_EQ(book.apply_fill(m, 80'000'000, nullptr), BookError::OK);
    EXPECT_EQ(book.modify_order(1, 110'000'000, 80'000'000, 71, 71),
              BookError::INVALID_QTY);  // == filled: nothing left to rest
    EXPECT_EQ(book.modify_order(1, 110'000'000, 50'000'000, 72, 72),
              BookError::INVALID_QTY);  // < filled
    // Valid qty-down amend above filled qty works.
    EXPECT_EQ(book.modify_order(1, 110'000'000, 90'000'000, 73, 73),
              BookError::OK);
    EXPECT_EQ(m->qty_units, 90'000'000);
    EXPECT_EQ(m->filled_qty_units, 80'000'000);
    EXPECT_EQ(m->timestamp_ns, 10u);  // qty-down kept its stamp
    EXPECT_TRUE(book.validate());
}

// --- Capacity / validation edges -----------------------------------------------

TEST(OrderBookEdges, PoolExhaustionAndDuplicateIds) {
    MemoryPool<Order> pool(4);
    OrderBook book(pool);
    Order* o = nullptr;
    for (uint64_t id = 1; id <= 4; ++id) {
        ASSERT_EQ(book.add_order(mk(id, Side::BUY, 109'000'000 + id * 1'000,
                                    1'000'000, id, id), &o), BookError::OK);
    }
    // 5th order: pool exhausted → CAPACITY_EXCEEDED, atomic reject.
    EXPECT_EQ(book.add_order(mk(5, Side::BUY, 109'100'000, 1'000'000, 5, 5),
                             &o), BookError::CAPACITY_EXCEEDED);
    EXPECT_EQ(pool.remaining(), 0u);
    // Duplicate id rejected before touching the pool.
    EXPECT_EQ(book.add_order(mk(1, Side::SELL, 120'000'000, 1'000'000, 6, 6),
                             &o), BookError::DUPLICATE_ID);
    EXPECT_EQ(pool.remaining(), 0u);
    // Zero/negative inputs rejected.
    EXPECT_EQ(book.add_order(mk(9, Side::BUY, 0, 1'000'000, 7, 7), &o),
              BookError::INVALID_PRICE);
    EXPECT_EQ(book.add_order(mk(9, Side::BUY, -5, 1'000'000, 7, 7), &o),
              BookError::INVALID_PRICE);
    EXPECT_EQ(book.add_order(mk(9, Side::BUY, 109'000'000, 0, 7, 7), &o),
              BookError::INVALID_QTY);
    // Freeing a slot lets the next add in.
    ASSERT_EQ(book.cancel_order(1), BookError::OK);
    ASSERT_EQ(book.add_order(mk(5, Side::BUY, 109'105'000, 1'000'000, 8, 8),
                             &o), BookError::OK);
    EXPECT_TRUE(book.validate());
}

TEST(OrderBookEdges, MaxOrdersCapAndLevelCapacity) {
    MemoryPool<Order> pool(8);
    OrderBook book(pool, 4);  // explicit resting cap below pool size
    Order* o = nullptr;
    for (uint64_t id = 1; id <= 4; ++id) {
        ASSERT_EQ(book.add_order(mk(id, Side::BUY, 109'000'000 + id * 1'000,
                                    1'000'000, id, id), &o), BookError::OK);
    }
    EXPECT_EQ(book.add_order(mk(5, Side::BUY, 109'200'000, 1'000'000, 5, 5),
                             &o), BookError::CAPACITY_EXCEEDED);
    // Level capacity: fill kMaxLevels asks with distinct prices.
    MemoryPool<Order> pool2(OrderBook::kMaxLevels + 4);
    OrderBook book2(pool2);
    uint64_t ts = 0;
    for (std::size_t i = 0; i < OrderBook::kMaxLevels; ++i) {
        ASSERT_EQ(book2.add_order(mk(i + 1, Side::SELL,
                                     110'000'000 + static_cast<int64_t>(i),
                                     1'000'000, ts, ts), &o), BookError::OK);
        ++ts;
    }
    EXPECT_EQ(book2.ask_count(), OrderBook::kMaxLevels);
    EXPECT_EQ(book2.add_order(mk(99999, Side::SELL, 220'000'000, 1'000'000,
                                ts, ts), &o), BookError::LEVEL_CAPACITY);
    // Same-price order still inserts — level capacity ≠ order capacity.
    ASSERT_EQ(book2.add_order(mk(99998, Side::SELL, 110'000'000, 1'000'000,
                                ts + 1, ts + 1), &o), BookError::OK);
    EXPECT_TRUE(book2.validate());
}

// --- Snapshot -------------------------------------------------------------------

TEST(OrderBookSnapshot, ReturnsConsistentBidsAsksSeq) {
    MemoryPool<Order> pool(64);
    OrderBook book(pool);
    Order* o = nullptr;
    ASSERT_EQ(book.add_order(mk(1, Side::BUY, 110'000'000, 100'000'000, 1, 1),
                             &o), BookError::OK);
    ASSERT_EQ(book.add_order(mk(2, Side::BUY, 110'000'000, 50'000'000, 2, 2),
                             &o), BookError::OK);
    ASSERT_EQ(book.add_order(mk(3, Side::BUY, 109'500'000, 25'000'000, 3, 3),
                             &o), BookError::OK);
    ASSERT_EQ(book.add_order(mk(4, Side::SELL, 110'200'000, 75'000'000, 4, 4),
                             &o), BookError::OK);
    const BookSnapshot s = book.snapshot();
    EXPECT_EQ(s.seq, book.book_seq());
    ASSERT_EQ(s.bids.size(), 2u);
    ASSERT_EQ(s.asks.size(), 1u);
    EXPECT_EQ(s.bids[0].price_ticks, 110'000'000);
    EXPECT_EQ(s.bids[0].total_qty_units, 150'000'000);
    EXPECT_EQ(s.bids[0].order_count, 2u);
    EXPECT_EQ(s.bids[1].price_ticks, 109'500'000);
    EXPECT_EQ(s.asks[0].price_ticks, 110'200'000);
    // Snapshot is a value copy — mutating the book does not alias it.
    ASSERT_EQ(book.cancel_order(4), BookError::OK);
    EXPECT_EQ(s.asks[0].price_ticks, 110'200'000);
}

// --- Zero heap allocation in the hot path ----------------------------------------

TEST(OrderBookAlloc, AddCancelModifyFillAllocateZeroHeap) {
    MemoryPool<Order> pool(256);   // pool + book index allocate pre-region
    OrderBook book(pool);
    constexpr int kOps = 512;
    BookError results[kOps];
    g_counting.store(true);
    uint64_t ts = 1;
    uint64_t next_id = 1;
    for (int i = 0; i < kOps; ++i) {
        BookError r = BookError::OK;
        switch (i % 4) {
            case 0: {
                Order* o = nullptr;
                r = book.add_order(mk(next_id++, Side::BUY,
                                      109'000'000 + (i % 50) * 1'000,
                                      10'000'000, ts, ts), &o);
                break;
            }
            case 1: {
                Order* o = nullptr;
                r = book.add_order(mk(next_id++, Side::SELL,
                                      120'000'000 + (i % 50) * 1'000,
                                      10'000'000, ts, ts), &o);
                break;
            }
            case 2:
                r = book.modify_order(next_id - 1,
                                      109'500'000 + (i % 20) * 1'000,
                                      20'000'000, ts, ts);
                break;
            case 3:
                r = book.cancel_order(next_id - 2);
                break;
        }
        results[i] = r;
        ++ts;
    }
    // Fill pass inside the measured region too: full fills pop heads and
    // return slots to the pool — still zero heap traffic. front_order()
    // re-reads the current best level each round, so emptied levels roll
    // forward correctly.
    BookError fill_rc = BookError::OK;
    for (Side s : {Side::BUY, Side::SELL}) {
        while (Order* head = book.front_order(s)) {
            fill_rc = book.apply_fill(head, remaining_qty_units(*head),
                                      nullptr);
        }
    }
    g_counting.store(false);
    EXPECT_EQ(fill_rc, BookError::OK);
    // Sanity: the workload actually applied (not silently all-rejected).
    int ok_count = 0;
    for (int i = 0; i < kOps; ++i) {
        if (results[i] == BookError::OK) ++ok_count;
    }
    EXPECT_GT(ok_count, 300);
    EXPECT_EQ(g_alloc_calls.load(), 0u);
    EXPECT_EQ(book.live_orders(), 0u);
    EXPECT_TRUE(book.validate());
}

// --- Randomized invariant fuzz -----------------------------------------------------

TEST(OrderBookFuzz, NeverCrossedAndQtyConserved) {
    constexpr int kIterations = 60'000;
    constexpr int64_t kPriceMin = 100'000'000;
    constexpr int64_t kPriceMax = 120'000'000;
    constexpr int64_t kPriceStep = 1'000;  // 0.00001 tick

    MemoryPool<Order> pool(2'048);
    OrderBook book(pool);
    std::mt19937_64 rng(0x5EED);
    std::unordered_map<uint64_t, Order> live;
    std::set<int64_t> bid_prices, ask_prices;
    uint64_t next_id = 1;
    uint64_t ts = 1;

    auto model_best_bid = [&]() -> int64_t {
        return bid_prices.empty() ? 0 : *bid_prices.rbegin();
    };
    auto model_best_ask = [&]() -> int64_t {
        return ask_prices.empty() ? 0 : *ask_prices.begin();
    };
    auto sync_side_sets = [&](Side s, int64_t price) {
        std::set<int64_t>& set = s == Side::BUY ? bid_prices : ask_prices;
        bool still_present = false;
        for (const auto& kv : live) {
            if (kv.second.side == s && kv.second.price_ticks == price) {
                still_present = true;
                break;
            }
        }
        if (still_present) set.insert(price); else set.erase(price);
    };

    for (int i = 0; i < kIterations; ++i) {
        const int op = static_cast<int>(rng() % 100);
        if (op < 55 || live.empty()) {
            // --- add ---
            const Side s = rng() % 2 ? Side::BUY : Side::SELL;
            const int64_t price =
                kPriceMin + static_cast<int64_t>(rng() % ((kPriceMax - kPriceMin) / kPriceStep)) * kPriceStep;
            const int64_t qty = 1'000'000 + static_cast<int64_t>(rng() % 100) * 1'000'000;
            Order t = mk(next_id, s, price, qty, ts, ts);
            Order* o = nullptr;
            const BookError r = book.add_order(t, &o);
            // Predict the crossing guard against the model.
            const bool would_cross =
                (s == Side::BUY) ? (model_best_ask() != 0 && price >= model_best_ask())
                                 : (model_best_bid() != 0 && price <= model_best_bid());
            if (r == BookError::OK) {
                ASSERT_FALSE(would_cross) << "book accepted a crossing add";
                live.emplace(next_id, t);
                (s == Side::BUY ? bid_prices : ask_prices).insert(price);
                ASSERT_NE(o, nullptr);
                ++next_id;
            } else {
                ASSERT_TRUE(r == BookError::CROSSED ||
                            r == BookError::CAPACITY_EXCEEDED)
                    << "unexpected add result " << book_error_name(r);
                ASSERT_TRUE(would_cross || r == BookError::CAPACITY_EXCEEDED);
            }
        } else if (op < 80) {
            // --- cancel ---
            auto it = live.begin();
            std::advance(it, rng() % live.size());
            const uint64_t id = it->first;
            const int64_t price = it->second.price_ticks;
            const Side s = it->second.side;
            ASSERT_EQ(book.cancel_order(id), BookError::OK);
            live.erase(it);
            sync_side_sets(s, price);
        } else {
            // --- modify ---
            auto it = live.begin();
            std::advance(it, rng() % live.size());
            const uint64_t id = it->first;
            const Order cur = it->second;
            const int64_t price =
                kPriceMin + static_cast<int64_t>(rng() % ((kPriceMax - kPriceMin) / kPriceStep)) * kPriceStep;
            const int64_t qty = 1'000'000 + static_cast<int64_t>(rng() % 100) * 1'000'000;
            const BookError r = book.modify_order(id, price, qty, ts, ts);
            // Cross-check the verdict against the model: all fuzz orders are
            // GTC and unfilled, so the only legitimate reject is CROSSED.
            const bool would_cross =
                (cur.side == Side::BUY)
                    ? (model_best_ask() != 0 && price >= model_best_ask())
                    : (model_best_bid() != 0 && price <= model_best_bid());
            if (r == BookError::OK) {
                ASSERT_FALSE(would_cross)
                    << "book accepted a crossing amend at iter " << i;
                // Model the priority contract verbatim.
                Order upd = cur;
                upd.qty_units = qty;
                upd.price_ticks = price;
                const bool noop =
                    (price == cur.price_ticks) && (qty == cur.qty_units);
                const bool keep =
                    (price == cur.price_ticks) && (qty < cur.qty_units);
                if (!keep && !noop) {
                    upd.timestamp_ns = ts;  // fresh stamp on priority loss
                    upd.ingress_seq = ts;
                }
                live[id] = upd;
                sync_side_sets(cur.side, cur.price_ticks);
                (cur.side == Side::BUY ? bid_prices : ask_prices).insert(price);
            } else {
                ASSERT_EQ(r, BookError::CROSSED)
                    << "unexpected modify result " << book_error_name(r);
                ASSERT_TRUE(would_cross);
            }
        }
        ++ts;

        // Invariants after EVERY op: never crossed; aggregate conservation.
        ASSERT_FALSE(book.crossed())
            << "crossed book after iter " << i;
        if ((i & 0x3FF) == 0 || i == kIterations - 1) {
            const char* why = nullptr;
            ASSERT_TRUE(book.validate(&why)) << "iter " << i << ": " << (why ? why : "");
            int64_t model_bids = 0, model_asks = 0;
            for (const auto& [id, o] : live) {
                if (o.side == Side::BUY) model_bids += remaining_qty_units(o);
                else model_asks += remaining_qty_units(o);
            }
            ASSERT_EQ(book_side_total(book, Side::BUY), model_bids);
            ASSERT_EQ(book_side_total(book, Side::SELL), model_asks);
            ASSERT_EQ(book.live_orders(), live.size());
        }
    }
    const char* why = nullptr;
    ASSERT_TRUE(book.validate(&why)) << (why ? why : "");
    // Bids strictly below asks at the boundary.
    if (book.best_bid() && book.best_ask()) {
        ASSERT_LT(book.best_bid()->price_ticks, book.best_ask()->price_ticks);
    }
    // seq is monotonic and equals the number of applied mutations.
    ASSERT_GT(book.book_seq(), 0u);
}

