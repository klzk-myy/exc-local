// Task 2.3.2 — smoke + seam coverage for the matching engine stack.
// Exhaustive behavior lives in test_matching_engine.cpp; this file keeps the
// legacy smoke checks (adapted to the 5-arg contract) plus thin checks over
// the decomposed components (WalWriter / IpcPublisher / SelfTradeGuard /
// StopOrderTrigger / IcebergManager) that the engine wires together.

#include <gtest/gtest.h>

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "ipc/IpcChannel.hpp"
#include "matching/IcebergManager.hpp"
#include "matching/IpcPublisher.hpp"
#include "matching/MatchingEngine.hpp"
#include "matching/SelfTradeGuard.hpp"
#include "matching/StopOrderTrigger.hpp"
#include "matching/WalWriter.hpp"
#include "risk/PreTradeChecker.hpp"
#include "utils/MemoryPool.hpp"

using namespace exch;

namespace {

uint64_t g_smoke_ts = 42'000'000'000;

Order* mk_order(MemoryPool<Order>& pool, uint64_t id, Side side,
                OrderType type, int64_t price, int64_t qty,
                uint64_t acct = 1) {
    Order* o = pool.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = side;
    o->type = type;
    o->tif = TimeInForce::GTC;
    o->price_ticks = price;
    o->qty_units = qty;
    o->quantity = Decimal::from_mantissa(qty);
    o->timestamp_ns = ++g_smoke_ts;
    o->ingress_seq = g_smoke_ts;
    return o;
}

}  // namespace

TEST(MatchingSmoke, ConstructAndCount) {
    MemoryPool<Order> pool(16);
    OrderBook book(pool);  // pool-bound book — resting orders can exist
    MatchingEngine engine(3, book, pool);  // 5-arg ctor defaults: no WAL/IPC
    EXPECT_EQ(engine.shard_id(), 3u);
    EXPECT_EQ(engine.received_count(), 0u);

    Order* o = mk_order(pool, 1, Side::SELL, OrderType::LIMIT, 5000, 10);
    ASSERT_NE(o, nullptr);
    engine.on_order_received(o);
    EXPECT_EQ(engine.received_count(), 1u);
    EXPECT_EQ(&engine.book(), &book);
    EXPECT_NE(book.find_order(1), nullptr);  // unmarketable -> rested
}

TEST(MatchingSmoke, RiskStubAccepts) {
    PreTradeChecker checker;
    Order o{};
    o.type = OrderType::LIMIT;
    EXPECT_EQ(checker.check(o), RiskDecision::ACCEPT);
}

// --- Decomposed component seams ------------------------------------------------

TEST(MatchingComponents, SelfTradeGuardTable) {
    EXPECT_EQ(SelfTradeGuard::action(StpMode::CANCEL_NEWEST),
              StpAction::CANCEL_TAKER);
    EXPECT_EQ(SelfTradeGuard::action(StpMode::CANCEL_OLDEST),
              StpAction::CANCEL_MAKER);
    EXPECT_EQ(SelfTradeGuard::action(StpMode::CANCEL_BOTH),
              StpAction::CANCEL_BOTH);
    EXPECT_EQ(SelfTradeGuard::action(StpMode::DECREMENT),
              StpAction::DECREMENT);
    // NONE -> PROCEED (Task 2.3.16): legal only after check-14's
    // Professional/ECP admission gate rejects retail NONE upstream.
    EXPECT_EQ(SelfTradeGuard::action(StpMode::NONE), StpAction::PROCEED);
    // Task 2.3.18 mutual-flag TRANSFER dispatch (the two-flag overload is
    // the one exception to taker-mode authority).
    EXPECT_EQ(SelfTradeGuard::action(StpMode::CANCEL_NEWEST,
                                     /*taker_xfer=*/true,
                                     /*maker_xfer=*/true),
              StpAction::TRANSFER);
    EXPECT_EQ(SelfTradeGuard::action(StpMode::DECREMENT,
                                     /*taker_xfer=*/true,
                                     /*maker_xfer=*/false),
              StpAction::DECREMENT);  // taker-only request -> mode verbatim
    EXPECT_EQ(SelfTradeGuard::action(static_cast<StpMode>(99)),
              StpAction::CANCEL_TAKER);  // out-of-enum -> fail closed

    EXPECT_TRUE(SelfTradeGuard::is_self_match(7, 7, 0, 0));   // same account
    EXPECT_TRUE(SelfTradeGuard::is_self_match(1, 2, 9, 9));   // same group
    EXPECT_FALSE(SelfTradeGuard::is_self_match(1, 2, 0, 0));  // unrelated
    EXPECT_FALSE(SelfTradeGuard::is_self_match(1, 2, 9, 8));  // diff groups
}

TEST(MatchingComponents, WalWriterNullWalDegrades) {
    WalWriter w(nullptr);  // journal-free mode — no-op appends
    Order o{};
    OrderAux aux{};
    EXPECT_EQ(w.write_order_new(o, aux, 1, 100, 0), WalStatus::NotOpen);
    EXPECT_EQ(w.write_order_cancel(1, 1, 0, 0), WalStatus::NotOpen);
    EXPECT_EQ(w.write_order_modify(1, 5, 5, 0, 0), WalStatus::NotOpen);
    EXPECT_EQ(w.write_trade(1, 1, 2, 1, 100, 5, 0), WalStatus::NotOpen);
    EXPECT_EQ(w.write_time_tick(1), WalStatus::NotOpen);
    EXPECT_EQ(w.flush(), WalStatus::NotOpen);
    EXPECT_EQ(w.failures(), 0u);  // never reached a WAL — nothing failed
}

TEST(MatchingComponents, StopTriggerQueueOrder) {
    MemoryPool<Order> pool(64);
    StopOrderTrigger q(64);
    // Buys trigger on last >= stop (ascending); sells on last <= stop
    // (descending).
    Order* b1 = mk_order(pool, 1, Side::BUY, OrderType::STOP, 0, 10);
    Order* b2 = mk_order(pool, 2, Side::BUY, OrderType::STOP, 0, 10);
    Order* s1 = mk_order(pool, 3, Side::SELL, OrderType::STOP, 0, 10);
    ASSERT_TRUE(q.enqueue(b1, 5100));
    ASSERT_TRUE(q.enqueue(b2, 5050));
    ASSERT_TRUE(q.enqueue(s1, 4900));
    EXPECT_EQ(q.size(), 3u);

    // last=5050: only the 5050 buy is due (5100 buy not; 4900 sell needs
    // last <= 4900). At 5000 nothing fires.
    EXPECT_EQ(q.pop_triggered(5000), nullptr);
    Order* c = q.pop_triggered(5050);
    ASSERT_NE(c, nullptr);
    EXPECT_EQ(c->id, 2u);
    EXPECT_EQ(c->next, nullptr);
    EXPECT_EQ(q.size(), 2u);

    // Push price through both remaining triggers.
    c = q.pop_triggered(5100);
    ASSERT_NE(c, nullptr);
    EXPECT_EQ(c->id, 1u);  // 5100 buy
    c = q.pop_triggered(4800);
    ASSERT_NE(c, nullptr);
    EXPECT_EQ(c->id, 3u);  // 4900 sell
    EXPECT_TRUE(q.empty());

    // Idempotent remove on an absent id.
    EXPECT_EQ(q.remove(9), nullptr);
    EXPECT_EQ(q.remove(1), nullptr);  // already popped
}

TEST(MatchingComponents, IcebergSliceMath) {
    IcebergManager mgr(16);
    Order tmpl{};
    tmpl.id = 7;
    tmpl.qty_units = 100;
    // Default slice = 10% when no display/instrument hint.
    EXPECT_EQ(IcebergManager::visible_slice(100, 0, nullptr), 10);
    // Explicit display wins.
    EXPECT_EQ(IcebergManager::visible_slice(100, 25, nullptr), 25);
    // Clamps to the order size.
    EXPECT_EQ(IcebergManager::visible_slice(100, 500, nullptr), 100);

    IcebergManager::Record* r = mgr.register_order(tmpl, /*filled=*/0, 10);
    ASSERT_NE(r, nullptr);
    EXPECT_EQ(IcebergManager::next_slice(*r), 10);
    r->filled_total_units = 95;
    EXPECT_EQ(IcebergManager::next_slice(*r), 5);   // tail slice
    r->filled_total_units = 100;
    EXPECT_EQ(IcebergManager::next_slice(*r), 0);   // done
    mgr.erase(7);
    EXPECT_FALSE(mgr.contains(7));
}
