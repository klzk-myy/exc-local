// Smoke-level coverage; real book behavior lands with Phase-02 Task 2.3.1.
#include <gtest/gtest.h>

#include <cstdint>
#include <type_traits>

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "book/PriceLevel.hpp"
#include "utils/MemoryPool.hpp"

using namespace exch;

TEST(OrderBookSmoke, EnumsMatchSpecEnums) {
    EXPECT_EQ(static_cast<uint8_t>(Side::BUY), 0);
    EXPECT_EQ(static_cast<uint8_t>(Side::SELL), 1);
    EXPECT_EQ(static_cast<uint8_t>(OrderType::LIMIT), 0);
    EXPECT_EQ(static_cast<uint8_t>(OrderType::MOC), 15);  // 16 spec §5.4 values
    EXPECT_EQ(static_cast<uint8_t>(TimeInForce::GTC), 0);
    EXPECT_EQ(static_cast<uint8_t>(TimeInForce::DAY), 4);  // GTC IOC FOK GTD DAY
}

TEST(OrderBookSmoke, OrderIsPodPerSpec31) {
    static_assert(std::is_aggregate_v<Order>);
    static_assert(std::is_trivially_copyable_v<Order>);
    static_assert(std::is_trivially_destructible_v<Order>);
    static_assert(std::is_standard_layout_v<Order>);
    SUCCEED();
}

TEST(OrderBookSmoke, PriceLevelIsCacheAligned) {
    static_assert(alignof(PriceLevel) == 64);
    static_assert(sizeof(PriceLevel) == 64);
    SUCCEED();
}

TEST(OrderBookSmoke, EmptyBookState) {
    OrderBook book;
    EXPECT_EQ(book.book_seq(), 0u);
    EXPECT_EQ(book.bid_count(), 0u);
    EXPECT_EQ(book.ask_count(), 0u);
    EXPECT_EQ(book.best_bid(), nullptr);
    EXPECT_EQ(book.best_ask(), nullptr);
}

TEST(OrderBookSmoke, PoolIntegration) {
    MemoryPool<Order> pool(kOrderPoolCapacity);
    Order* o = pool.alloc();
    ASSERT_NE(o, nullptr);
    o->id = 7;
    o->side = Side::BUY;
    o->type = OrderType::LIMIT;
    o->tif = TimeInForce::GTC;
    o->quantity = Decimal::from_string("1.5");
    o->price = Decimal::from_string("1.08425");
    EXPECT_FALSE(is_filled(*o));
    EXPECT_EQ(remaining_qty(*o), Decimal::from_string("1.5"));
    o->filled_qty = o->quantity;
    EXPECT_TRUE(is_filled(*o));
    pool.free(o);
}
