// Task 2.3.20 (spec §6.9, §7.1) — atomic amend/replace + fairness:
//   * single matching-thread replace path; exactly one winner among
//     concurrent amends, losers get STALE_MODIFY, no double-apply
//   * priority rules: price change / qty-up / iceberg display change lose
//     (price, timestamp_ns, ingress_seq) priority; qty-down-only preserves
//   * GTD re-arm on accepted amend; IOC/FOK rejected; state gate
//     (CANCEL_ONLY/SUSPENDED/HALTED/DELISTED) with cancels still allowed
//   * deterministic (price, ts, seq) FIFO — identical under replay

#include <gtest/gtest.h>

#include <cstring>
#include <vector>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "matching/MatchingEngine.hpp"
#include "matching/WalWriter.hpp"
#include "utils/MemoryPool.hpp"

using namespace exch;

namespace {

uint64_t g_ts = 1'000'000'000;

Order* mk(MemoryPool<Order>& pool, uint64_t id, Side side, OrderType type,
          int64_t price, int64_t qty, uint64_t acct = 1,
          TimeInForce tif = TimeInForce::GTC, int64_t display = 0) {
    Order* o = pool.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = side;
    o->type = type;
    o->tif = tif;
    o->price_ticks = price;
    o->qty_units = qty;
    o->quantity = Decimal::from_mantissa(qty);
    o->display_qty_units = display;
    o->timestamp_ns = ++g_ts;
    o->ingress_seq = g_ts;
    return o;
}

struct Fixture {
    MemoryPool<Order> pool{512};
    Instrument instr;
    OrderBook book;
    MatchingEngine engine;

    Fixture()
        : book(pool), engine(3, book, pool, nullptr, nullptr) {
        instr.instrument_id = 7;
        instr.pip_factor = 10;
        instr.pip_size_ticks = 10'000;
        instr.tick_size_ticks = 1'000;
        instr.lot_size_units = 1;
        instr.status = InstrumentStatus::ACTIVE;
        book.set_instrument(instr);
    }
};

int64_t level_qty(const OrderBook& b, Side s, int64_t price) {
    for (std::size_t d = 0;; ++d) {
        const PriceLevel* l = b.level(s, d);
        if (l == nullptr) return 0;
        if (l->price_ticks == price) return l->total_qty_units;
    }
}

// FIFO ids at a price level, head first.
std::vector<uint64_t> level_order(const OrderBook& b, Side s,
                                  int64_t price) {
    std::vector<uint64_t> ids;
    for (std::size_t d = 0;; ++d) {
        const PriceLevel* l = b.level(s, d);
        if (l == nullptr) return ids;
        if (l->price_ticks == price) {
            for (const Order* o = l->head; o != nullptr; o = o->next) {
                ids.push_back(o->id);
            }
            return ids;
        }
    }
}

}  // namespace

// === Concurrent-amend arbitration (§6.9 #1) =====================================

TEST(Amend, ConcurrentAmendExactlyOneWinner) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 50));

    // Winner: first applied amend for the order.
    f.engine.on_amend_received(1, 5010, 0, 0, /*seq=*/100);
    ASSERT_NE(f.book.find_order(1), nullptr);
    EXPECT_EQ(f.book.find_order(1)->price_ticks, 5010);

    // Loser built on the same decision point (same seq) -> STALE_MODIFY,
    // zero double-apply.
    const uint64_t seq0 = f.book.book_seq();
    f.engine.on_amend_received(1, 0, 99, 0, /*seq=*/100);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStaleModify);
    EXPECT_EQ(f.book.book_seq(), seq0);          // loser never mutates
    EXPECT_EQ(f.book.find_order(1)->qty_units, 50);

    // Older seq than the applied winner -> stale too.
    f.engine.on_amend_received(1, 0, 77, 0, /*seq=*/50);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStaleModify);
    EXPECT_EQ(f.book.find_order(1)->qty_units, 50);

    // A strictly advancing seq is a fresh decision -> applies. (last_reject_
    // intentionally retains the last code — assert the mutation itself.)
    f.engine.on_amend_received(1, 0, 60, 0, /*seq=*/200);
    EXPECT_EQ(f.book.find_order(1)->qty_units, 60);
}

TEST(Amend, FenceResetsAfterOrderDeath) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 50));
    f.engine.on_amend_received(1, 0, 40, 0, 100);   // winner at seq 100
    f.engine.on_cancel_received(1, 1);              // order (and fence) die
    ASSERT_EQ(f.book.find_order(1), nullptr);
    // Reused id: a fresh order is a fresh fence epoch — seq <= 100 is legal.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 20));
    f.engine.on_amend_received(1, 0, 10, 0, 5);
    EXPECT_EQ(f.book.find_order(1)->qty_units, 10);
}

TEST(Amend, UnknownOrderRejected) {
    Fixture f;
    f.engine.on_amend_received(4242, 100, 10, 0, 1);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectUnknownOrder);
}

// === Priority rules (§6.9 #5) ===================================================

TEST(Amend, PriceChangeLosesPriority) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 30));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, 5000, 30));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::SELL, OrderType::LIMIT, 5010, 30));

    // Advance the deterministic engine clock past every order stamp — a
    // requeue re-stamps with max(now_ns_, old_ts), so the fresh key only
    // outranks the level when the clock has moved (production: always).
    f.engine.on_time_tick(g_ts + 1'000);

    // #1 reprices into #3's level -> lands BEHIND #3 (tail of that level).
    f.engine.on_amend_received(1, 5010, 0, 0, 100);
    auto ids = level_order(f.book, Side::SELL, 5010);
    ASSERT_EQ(ids.size(), 2u);
    EXPECT_EQ(ids[0], 3u);  // original holder keeps head
    EXPECT_EQ(ids[1], 1u);  // repriced order lost priority
    EXPECT_EQ(level_order(f.book, Side::SELL, 5000)[0], 2u);
}

TEST(Amend, QtyUpLosesPriority) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 30));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, 5000, 30));
    f.engine.on_time_tick(g_ts + 1'000);  // see PriceChangeLosesPriority
    f.engine.on_amend_received(1, 0, 60, 0, 100);  // qty up at same price
    auto ids = level_order(f.book, Side::SELL, 5000);
    ASSERT_EQ(ids.size(), 2u);
    EXPECT_EQ(ids[0], 2u);
    EXPECT_EQ(ids[1], 1u);
    EXPECT_EQ(level_qty(f.book, Side::SELL, 5000), 90);
}

TEST(Amend, QtyDownPreservesPriority) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 50));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, 5000, 50));
    f.engine.on_amend_received(1, 0, 20, 0, 100);  // qty-down only
    auto ids = level_order(f.book, Side::SELL, 5000);
    ASSERT_EQ(ids.size(), 2u);
    EXPECT_EQ(ids[0], 1u);  // still heads the level
    EXPECT_EQ(f.book.find_order(1)->qty_units, 20);
    EXPECT_EQ(level_qty(f.book, Side::SELL, 5000), 70);
}

TEST(Amend, IcebergDisplayChangeLosesPriority) {
    Fixture f;
    // Iceberg total 100, display 10 — live slice is 10.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::ICEBERG, 5000, 100, 1,
           TimeInForce::GTC, /*display=*/10));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, 5000, 50));
    ASSERT_EQ(level_order(f.book, Side::SELL, 5000)[0], 1u);
    f.engine.on_time_tick(g_ts + 1'000);  // see PriceChangeLosesPriority

    // Display change 10 -> 5 (a slice qty-DOWN, which alone would keep
    // priority): the display change itself forces the requeue (§6.9 #5).
    MatchingEngine::AmendRequest req{};
    req.display_qty_units = 5;
    req.ingress_seq = 100;
    f.engine.on_amend_received_ex(1, req);

    auto ids = level_order(f.book, Side::SELL, 5000);
    ASSERT_EQ(ids.size(), 2u);
    EXPECT_EQ(ids[0], 2u);  // iceberg lost its slot
    EXPECT_EQ(ids[1], 1u);
    EXPECT_EQ(f.book.find_order(1)->qty_units, 5);   // live slice resized
    const auto* rec = f.engine.icebergs().find(1);
    ASSERT_NE(rec, nullptr);
    EXPECT_EQ(rec->display_qty_units, 5);
    EXPECT_EQ(rec->total_qty_units, 100);
    EXPECT_EQ(level_qty(f.book, Side::SELL, 5000), 55);
}

TEST(Amend, StopTriggerPriceChangeResortsQueue) {
    Fixture f;
    OrderAux aux{};
    aux.stop_price_ticks = 5100;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::STOP, 0, 10, 1), aux);
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::STOP, 0, 10, 1), aux);
    ASSERT_TRUE(f.engine.stops().pending(1));
    const uint64_t old_ts = f.engine.stops().find(1)->order->timestamp_ns;

    f.engine.on_time_tick(9'999'999);
    // Amend #1's trigger to a later stop -> restamped + resorted.
    f.engine.on_amend_received(1, 0, 0, /*stop=*/5150, /*seq=*/100);
    const auto* p = f.engine.stops().find(1);
    ASSERT_NE(p, nullptr);
    EXPECT_EQ(p->stop_price_ticks, 5150);
    EXPECT_GE(p->order->timestamp_ns, old_ts);
    EXPECT_EQ(p->order->qty_units, 10);

    // Qty amend applies in place without touching the trigger.
    f.engine.on_amend_received(1, 0, 7, 0, /*seq=*/101);
    EXPECT_EQ(f.engine.stops().find(1)->order->qty_units, 7);
    EXPECT_EQ(f.engine.stops().find(1)->stop_price_ticks, 5150);
}

// === GTD re-arm (§6.9 #2) ======================================================

TEST(Amend, AcceptedAmendRearmsGtd) {
    Fixture f;
    OrderAux aux{};
    aux.gtd_expiry_ns = 2'000;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 50, 1,
           TimeInForce::GTD),
        aux);
    f.engine.on_time_tick(1'500);

    // Accepted amend carries a fresh expiry -> timer re-arms to 5000.
    MatchingEngine::AmendRequest req{};
    req.qty_units = 40;
    req.gtd_expiry_ns = 5'000;
    req.ingress_seq = 100;
    f.engine.on_amend_received_ex(1, req);
    EXPECT_EQ(f.book.find_order(1)->qty_units, 40);

    f.engine.on_time_tick(2'000);  // original expiry — must NOT fire
    ASSERT_NE(f.book.find_order(1), nullptr);
    f.engine.on_time_tick(5'000);  // new expiry fires
    EXPECT_EQ(f.book.find_order(1), nullptr);
}

TEST(Amend, AmendWithoutExpiryKeepsTimer) {
    Fixture f;
    OrderAux aux{};
    aux.gtd_expiry_ns = 2'000;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 50, 1,
           TimeInForce::GTD),
        aux);
    // Plain qty amend (no expiry field) — the existing timer stands.
    f.engine.on_amend_received(1, 0, 40, 0, 100);
    EXPECT_EQ(f.book.find_order(1)->qty_units, 40);
    f.engine.on_time_tick(2'000);
    EXPECT_EQ(f.book.find_order(1), nullptr);  // original GTD fired
}

TEST(Amend, RejectedAmendDoesNotRearm) {
    Fixture f;
    OrderAux aux{};
    aux.gtd_expiry_ns = 2'000;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 50, 1,
           TimeInForce::GTD),
        aux);
    MatchingEngine::AmendRequest req{};
    req.qty_units = 40;
    req.gtd_expiry_ns = 9'000;
    req.ingress_seq = 100;
    f.engine.on_amend_received_ex(1, req);         // winner (re-arms to 9000)
    f.engine.on_amend_received_ex(1, req);         // stale loser — no re-arm
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStaleModify);
    f.engine.on_time_tick(2'500);
    ASSERT_NE(f.book.find_order(1), nullptr);      // timer is 9000, not 2000
    f.engine.on_time_tick(9'000);
    EXPECT_EQ(f.book.find_order(1), nullptr);
}

// === IOC/FOK + state gates ======================================================

TEST(Amend, IocFokAmendRejected) {
    Fixture f;
    // A resting IOC is unreachable through the engine, but a PENDING stop
    // can carry IOC — amend it -> ORDER_AMEND_REJECTED.
    OrderAux aux{};
    aux.stop_price_ticks = 5100;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::STOP, 0, 10, 1,
           TimeInForce::IOC),
        aux);
    ASSERT_TRUE(f.engine.stops().pending(1));
    f.engine.on_amend_received(1, 0, 5, 0, 100);
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectAmendRejected);
    EXPECT_EQ(f.engine.stops().find(1)->order->qty_units, 10);  // untouched

    // Book-level guard: an IOC node resting via a direct book add rejects
    // NOT_MODIFIABLE (the engine maps the family to ORDER_AMEND_REJECTED).
    Order* o = f.pool.alloc();
    *o = Order{};
    o->id = 2;
    o->account_id = 1;
    o->side = Side::SELL;
    o->type = OrderType::LIMIT;
    o->tif = TimeInForce::IOC;
    o->price_ticks = 5000;
    o->qty_units = 10;
    o->quantity = Decimal::from_mantissa(10);
    Order* placed = nullptr;
    ASSERT_EQ(f.book.add_order(*o, &placed), BookError::OK);
    EXPECT_EQ(f.book.modify_order(2, 5000, 5, 1, 1),
              BookError::NOT_MODIFIABLE);
}

TEST(Amend, StateGateRejectsAmendButAllowsCancel) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 50));

    const struct {
        InstrumentStatus st;
        const char* code;
    } cases[] = {
        {InstrumentStatus::CANCEL_ONLY,
         MatchingEngine::kRejectInstrumentCancelOnly},
        {InstrumentStatus::SUSPENDED,
         MatchingEngine::kRejectInstrumentSuspended},
        {InstrumentStatus::HALTED, MatchingEngine::kRejectInstrumentHalted},
        {InstrumentStatus::DELISTED,
         MatchingEngine::kRejectInstrumentDelisted},
    };
    for (const auto& c : cases) {
        f.instr.status = c.st;
        const uint64_t seq0 = f.book.book_seq();
        f.engine.on_amend_received(1, 5010, 0, 0, 100);
        EXPECT_STREQ(f.engine.last_reject(), c.code);
        EXPECT_EQ(f.book.book_seq(), seq0);        // no mutation
        EXPECT_EQ(f.book.find_order(1)->price_ticks, 5000);
    }

    // Cancels remain allowed under the same states (§7.1).
    f.instr.status = InstrumentStatus::SUSPENDED;
    f.engine.on_cancel_received(1, 1);
    EXPECT_EQ(f.book.find_order(1), nullptr);

    // Back to ACTIVE: a fresh order amends normally.
    f.instr.status = InstrumentStatus::ACTIVE;
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, 5000, 50));
    f.engine.on_amend_received(2, 0, 40, 0, 100);
    EXPECT_EQ(f.book.find_order(2)->qty_units, 40);
}

// === Deterministic (price, timestamp_ns, ingress_seq) FIFO ======================

TEST(Amend, FifoTieBreakOrdersByTsThenSeq) {
    Fixture f;
    // Insert out of key order: (ts=2000,seq=1) then (ts=1000,seq=99) then
    // (ts=1000,seq=40) — the level chain must sort strictly by (ts,seq).
    Order* a = mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 5000, 10);
    a->timestamp_ns = 2000;
    a->ingress_seq = 1;
    Order* b = mk(f.pool, 2, Side::SELL, OrderType::LIMIT, 5000, 10);
    b->timestamp_ns = 1000;
    b->ingress_seq = 99;
    Order* c = mk(f.pool, 3, Side::SELL, OrderType::LIMIT, 5000, 10);
    c->timestamp_ns = 1000;
    c->ingress_seq = 40;
    f.engine.on_order_received(a);
    f.engine.on_order_received(b);
    f.engine.on_order_received(c);

    auto ids = level_order(f.book, Side::SELL, 5000);
    ASSERT_EQ(ids.size(), 3u);
    EXPECT_EQ(ids[0], 3u);  // (1000,40)
    EXPECT_EQ(ids[1], 2u);  // (1000,99)
    EXPECT_EQ(ids[2], 1u);  // (2000,1)
    EXPECT_TRUE(f.book.validate());
}

TEST(Amend, DeterministicReplaySameWinnerSameOrder) {
    // The same input script replayed produces identical book state — the
    // amend fence and (ts,seq) insertion are pure functions of ingress seq.
    auto run = [](MatchingEngine& e, MemoryPool<Order>& pool) {
        e.on_order_received(
            mk(pool, 1, Side::SELL, OrderType::LIMIT, 5000, 30));
        e.on_order_received(
            mk(pool, 2, Side::SELL, OrderType::LIMIT, 5000, 30));
        e.on_order_received(
            mk(pool, 3, Side::BUY, OrderType::LIMIT, 4900, 30));
        e.on_amend_received(1, 0, 10, 0, 50);   // qty-down: keeps head
        e.on_amend_received(2, 0, 55, 0, 60);   // qty-up: tail behind #1
        e.on_amend_received(1, 5010, 0, 0, 70); // winner: reprice #1
        e.on_amend_received(1, 0, 5, 0, 70);    // stale loser
        e.on_amend_received(2, 5005, 0, 0, 80); // reprice #2 to its own level
    };

    g_ts = 1'000'000'000;
    MemoryPool<Order> p1{512};
    OrderBook b1{p1};
    MatchingEngine e1{3, b1, p1, nullptr, nullptr};
    run(e1, p1);

    g_ts = 1'000'000'000;
    MemoryPool<Order> p2{512};
    OrderBook b2{p2};
    MatchingEngine e2{3, b2, p2, nullptr, nullptr};
    run(e2, p2);

    EXPECT_EQ(e1.book_seq(), e2.book_seq());
    EXPECT_EQ(e1.reject_count(), e2.reject_count());
    EXPECT_EQ(b1.live_orders(), b2.live_orders());
    for (Side s : {Side::BUY, Side::SELL}) {
        for (std::size_t d = 0;; ++d) {
            const PriceLevel* l1 = b1.level(s, d);
            const PriceLevel* l2 = b2.level(s, d);
            ASSERT_EQ(l1 == nullptr, l2 == nullptr);
            if (l1 == nullptr) break;
            EXPECT_EQ(l1->price_ticks, l2->price_ticks);
            EXPECT_EQ(l1->total_qty_units, l2->total_qty_units);
            // Full FIFO identity: same ids in the same order.
            const Order* x = l1->head;
            const Order* y = l2->head;
            while (x != nullptr && y != nullptr) {
                EXPECT_EQ(x->id, y->id);
                EXPECT_EQ(x->qty_units, y->qty_units);
                x = x->next;
                y = y->next;
            }
            EXPECT_EQ(x, nullptr);
            EXPECT_EQ(y, nullptr);
        }
    }
    // And the scripted outcome itself: #1 repriced to 5010 (qty 10),
    // #2 at 5005 (qty 55), #3 untouched.
    const Order* o1 = b1.find_order(1);
    ASSERT_NE(o1, nullptr);
    EXPECT_EQ(o1->price_ticks, 5010);
    EXPECT_EQ(o1->qty_units, 10);
    const Order* o2 = b1.find_order(2);
    ASSERT_NE(o2, nullptr);
    EXPECT_EQ(o2->price_ticks, 5005);
    EXPECT_EQ(o2->qty_units, 55);
    const Order* o3 = b1.find_order(3);
    ASSERT_NE(o3, nullptr);
    EXPECT_EQ(o3->qty_units, 30);
}
