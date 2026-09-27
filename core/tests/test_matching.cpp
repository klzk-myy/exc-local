// Smoke-level coverage; real matching behavior lands with Phase-02 Task 2.3.2.
#include <gtest/gtest.h>

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "matching/MatchingEngine.hpp"
#include "risk/PreTradeChecker.hpp"
#include "utils/MemoryPool.hpp"

using namespace exch;

TEST(MatchingSmoke, ConstructAndCount) {
    MemoryPool<Order> pool(16);
    OrderBook book;
    MatchingEngine engine(3, book, pool);
    EXPECT_EQ(engine.shard_id(), 3u);
    EXPECT_EQ(engine.received_count(), 0u);

    Order* o = pool.alloc();
    ASSERT_NE(o, nullptr);
    o->id = 1;
    engine.on_order_received(o);
    EXPECT_EQ(engine.received_count(), 1u);
    EXPECT_EQ(&engine.book(), &book);
}

TEST(MatchingSmoke, RiskStubAccepts) {
    PreTradeChecker checker;
    Order o{};
    o.type = OrderType::LIMIT;
    EXPECT_EQ(checker.check(o), RiskDecision::ACCEPT);
}
