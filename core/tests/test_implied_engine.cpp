// Phase-22 Task 22.3.12 — production wiring coverage for multi-leg implied
// matching. Exercises the MatchingEngine ↔ ImpliedMatcher seam end-to-end
// through on_order_received: implied-in/out taker fills, tail-rescan fills
// of resting out-book orders, POST_ONLY probing, FOK's outright-only gate,
// OCO sibling cancellation, LAST-sourced stop triggering via the dirty-drain
// fixpoint, and the no-phantom-resting-order invariant — all with engine
// bookkeeping (meta/OCO/L3/last_price) kept consistent by the owner hook.

#include <gtest/gtest.h>

#include <cstdint>
#include <cstdio>
#include <unistd.h>

#include "book/OrderBook.hpp"
#include "ipc/EnginePump.hpp"
#include "matching/ImpliedMatcher.hpp"
#include "matching/MatchingEngine.hpp"
#include "utils/MemoryPool.hpp"

using namespace exch;

namespace {

uint64_t g_ts = 1'000'000'000;

Instrument ins(uint32_t id) {
    Instrument i{};
    i.instrument_id = id;
    std::snprintf(i.symbol, sizeof(i.symbol), "T%u", id);
    i.pip_factor = 10;
    i.pip_size_ticks = 10'000;
    i.tick_size_ticks = 1;    // raw tick arithmetic in these tests
    i.lot_size_units = 1;
    i.price_band_pct_up = 50'000;   // 500% — keep the band inert
    i.price_band_pct_down = 9'900;  // 99%
    return i;
}

constexpr uint32_t kNear = 1, kFar = 2, kSwap = 3;

struct Curve {
    // OrderBook binds instruments by pointer — they must outlive the book.
    Instrument i_near = ins(kNear), i_far = ins(kFar), i_swap = ins(kSwap);
    MemoryPool<Order> p_near{256}, p_far{256}, p_swap{256};
    OrderBook b_near{p_near}, b_far{p_far}, b_swap{p_swap};
    MatchingEngine e_near, e_far, e_swap;
    ImpliedMatcher m;
    uint64_t next_id = 100;

    Curve()
        : e_near(kNear, b_near, p_near),
          e_far(kFar, b_far, p_far),
          e_swap(kSwap, b_swap, p_swap) {
        b_near.set_instrument(i_near);
        b_far.set_instrument(i_far);
        b_swap.set_instrument(i_swap);
        e_near.bind_implied(&m);
        e_far.bind_implied(&m);
        e_swap.bind_implied(&m);
        // IMPLIED_IN: swap = far − near.
        ImpliedLink in{};
        in.link_id = 10;
        in.out_instrument = kSwap;
        in.legs[0] = ImpliedLeg{kFar, +1, 1};
        in.legs[1] = ImpliedLeg{kNear, -1, 1};
        EXPECT_TRUE(m.register_link(in));
        // IMPLIED_OUT: far = swap + near.
        ImpliedLink out{};
        out.link_id = 11;
        out.out_instrument = kFar;
        out.legs[0] = ImpliedLeg{kSwap, +1, 1};
        out.legs[1] = ImpliedLeg{kNear, +1, 1};
        EXPECT_TRUE(m.register_link(out));
    }

    // Host drain: every dirty engine settles its implied fills once.
    void drain() {
        for (MatchingEngine* e : {&e_near, &e_far, &e_swap}) {
            if (e->take_implied_dirty()) e->implied_sync();
        }
    }
};

Order* mk(MemoryPool<Order>& p, uint64_t id, Side s, int64_t px, int64_t qty,
          uint64_t acct = 1, TimeInForce tif = TimeInForce::GTC,
          uint8_t flags = 0, OrderType type = OrderType::LIMIT) {
    Order* o = p.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = s;
    o->type = type;
    o->tif = tif;
    o->price_ticks = px;
    o->qty_units = qty;
    o->quantity = Decimal::from_mantissa(qty);
    o->flags = flags;
    o->timestamp_ns = ++g_ts;
    o->ingress_seq = g_ts;
    return o;
}

void send(MatchingEngine& e, MemoryPool<Order>& p, uint64_t id, Side s,
          int64_t px, int64_t qty, uint64_t acct = 1,
          TimeInForce tif = TimeInForce::GTC, uint8_t flags = 0,
          OrderType type = OrderType::LIMIT, int64_t stop_px = 0) {
    Order* o = mk(p, id, s, px, qty, acct, tif, flags, type);
    ASSERT_NE(o, nullptr);
    OrderAux aux{};
    aux.instrument_id = e.book().instrument()->instrument_id;
    aux.stop_price_ticks = stop_px;
    e.on_order_received(o, aux);
}

// Wire-level send through the curve ingress — instrument routes, drain is
// automatic inside the adapter.
void send_in(CurveIngress& in, MemoryPool<Order>& p, uint64_t id,
             uint32_t instr, Side s, int64_t px, int64_t qty,
             uint64_t acct = 1, TimeInForce tif = TimeInForce::GTC) {
    Order* o = mk(p, id, s, px, qty, acct, tif);
    ASSERT_NE(o, nullptr);
    OrderAux aux{};
    aux.instrument_id = instr;
    in.on_order_received_ex(o, aux);
}

}  // namespace

// --- implied-in through the production ingress path ------------------------

TEST(ImpliedEngine, ImpliedInFillThroughIngress) {
    Curve c;
    send(c.e_far, c.p_far, 1, Side::SELL, 110500, 100);
    send(c.e_near, c.p_near, 2, Side::BUY, 110000, 100);
    // Implied swap ask = 110500 − 110000 = 500.
    send(c.e_swap, c.p_swap, 3, Side::BUY, 600, 40, /*acct=*/9);
    c.drain();

    // Taker consumed by implied liquidity, never rested.
    EXPECT_EQ(c.b_swap.find_order(3), nullptr);
    EXPECT_EQ(c.b_swap.live_orders(), 0u);
    // Leg makers reduced by the filled quantity.
    const Order* far_maker = c.b_far.find_order(1);
    const Order* near_maker = c.b_near.find_order(2);
    ASSERT_NE(far_maker, nullptr);
    ASSERT_NE(near_maker, nullptr);
    EXPECT_EQ(remaining_qty_units(*far_maker), 60);
    EXPECT_EQ(remaining_qty_units(*near_maker), 60);
    // Owner hook ran engine bookkeeping: trade counts on all three books.
    EXPECT_EQ(c.e_swap.trades_emitted(), 1u);   // out-book presentation fill
    EXPECT_EQ(c.e_far.trades_emitted(), 1u);
    EXPECT_EQ(c.e_near.trades_emitted(), 1u);
    EXPECT_EQ(c.e_swap.last_price_ticks(), 500);
}

// --- implied-out through ingress --------------------------------------------

TEST(ImpliedEngine, ImpliedOutFillThroughIngress) {
    Curve c;
    send(c.e_swap, c.p_swap, 1, Side::SELL, 500, 100);
    send(c.e_near, c.p_near, 2, Side::SELL, 110050, 100);
    // Implied far ask = 500 + 110050 = 110550.
    send(c.e_far, c.p_far, 3, Side::BUY, 110600, 30, /*acct=*/9);
    c.drain();

    EXPECT_EQ(c.b_far.find_order(3), nullptr);
    EXPECT_EQ(c.b_far.live_orders(), 0u);
    EXPECT_EQ(remaining_qty_units(*c.b_swap.find_order(1)), 70);
    EXPECT_EQ(remaining_qty_units(*c.b_near.find_order(2)), 70);
    EXPECT_EQ(c.e_far.last_price_ticks(), 110550);
}

// --- resting out-book order filled when leg liquidity arrives ---------------

TEST(ImpliedEngine, RestingOutFillsOnLegArrival) {
    Curve c;
    // Resting swap BUY first — no legs yet, so it rests (out-book change
    // on swap also rescans, but there is no implied quote to cross).
    // Distinct account: same-account leg makers would (correctly)
    // truncate implied capacity under the FIFO-prefix exclusion.
    send(c.e_swap, c.p_swap, 1, Side::BUY, 500, 50, /*acct=*/2);
    ASSERT_NE(c.b_swap.find_order(1), nullptr);
    // Far ask alone can't form a two-leg quote.
    send(c.e_far, c.p_far, 2, Side::SELL, 110500, 80);
    c.drain();
    EXPECT_NE(c.b_swap.find_order(1), nullptr);
    // Near bid completes the implied ask = 110500 − 110000 = 500; the near
    // engine's commit-tail rescan fills the resting swap order.
    send(c.e_near, c.p_near, 3, Side::BUY, 110000, 80);
    c.drain();
    EXPECT_EQ(c.b_swap.find_order(1), nullptr);
    EXPECT_EQ(remaining_qty_units(*c.b_far.find_order(2)), 30);
    EXPECT_EQ(remaining_qty_units(*c.b_near.find_order(3)), 30);
    EXPECT_EQ(c.e_swap.last_price_ticks(), 500);
}

// --- POST_ONLY probes implied liquidity and rejects ---------------------------

TEST(ImpliedEngine, PostOnlyRejectsImpliedCross) {
    Curve c;
    send(c.e_far, c.p_far, 1, Side::SELL, 110500, 100);
    send(c.e_near, c.p_near, 2, Side::BUY, 110000, 100);
    // Would cross the implied ask of 500 — outright swap book is empty, so
    // this exercises the implied probe specifically.
    send(c.e_swap, c.p_swap, 3, Side::BUY, 600, 40, /*acct=*/9,
         TimeInForce::GTC, kOrderFlagPostOnly);
    c.drain();
    EXPECT_STREQ(c.e_swap.last_reject(),
                 MatchingEngine::kRejectPostOnlyViolation);
    EXPECT_EQ(c.b_swap.live_orders(), 0u);
    // Legs untouched — the probe never mutates.
    EXPECT_EQ(remaining_qty_units(*c.b_far.find_order(1)), 100);
    EXPECT_EQ(remaining_qty_units(*c.b_near.find_order(2)), 100);
}

// --- FOK stays outright-only (no combined-liquidity feasibility proof) -------

TEST(ImpliedEngine, FokStaysOutrightOnly) {
    Curve c;
    send(c.e_far, c.p_far, 1, Side::SELL, 110500, 100);
    send(c.e_near, c.p_near, 2, Side::BUY, 110000, 100);
    send(c.e_swap, c.p_swap, 3, Side::BUY, 600, 40, /*acct=*/9,
         TimeInForce::FOK);
    c.drain();
    // The empty-book admission gate fires before fok_feasible — FOK is
    // deliberately not implied-exempt (outright-only, §27 ruling), so the
    // observable code is the sparse-book NO_LIQUIDITY, not FOK_UNFILLED.
    EXPECT_STREQ(c.e_swap.last_reject(),
                 MatchingEngine::kRejectNoLiquidity);
    EXPECT_EQ(remaining_qty_units(*c.b_far.find_order(1)), 100);
    EXPECT_EQ(remaining_qty_units(*c.b_near.find_order(2)), 100);
}

// --- IOC partial implied fill + remainder cancel -------------------------------

TEST(ImpliedEngine, IocPartialImpliedThenCancels) {
    Curve c;
    send(c.e_far, c.p_far, 1, Side::SELL, 110500, 30);
    send(c.e_near, c.p_near, 2, Side::BUY, 110000, 30);
    // Capacity 30; taker asks 50 → fills 30 implied, remainder cancels.
    send(c.e_swap, c.p_swap, 3, Side::BUY, 600, 50, /*acct=*/9,
         TimeInForce::IOC);
    c.drain();
    EXPECT_EQ(c.b_swap.live_orders(), 0u);
    EXPECT_EQ(c.b_far.find_order(1), nullptr);
    EXPECT_EQ(c.b_near.find_order(2), nullptr);
    EXPECT_EQ(c.e_swap.trades_emitted(), 1u);
}

// --- OCO sibling cancel through the owner hook --------------------------------

TEST(ImpliedEngine, OcoSiblingCancelsOnImpliedDeath) {
    Curve c;
    // OCO pair resting on the FAR book: leg A is the implied-in maker.
    c.e_far.on_oco_link_received(7, 10, 11, /*account=*/1, kFar);
    send(c.e_far, c.p_far, 10, Side::SELL, 110500, 50);
    send(c.e_far, c.p_far, 11, Side::SELL, 110700, 50);
    ASSERT_EQ(c.e_far.oco_member_count(), 2u);
    send(c.e_near, c.p_near, 12, Side::BUY, 110000, 100);
    // Swap taker fully fills far leg A via implied liquidity → sibling
    // leg B must be cancelled by the owner hook's OCO arm.
    send(c.e_swap, c.p_swap, 13, Side::BUY, 600, 50, /*acct=*/9);
    c.drain();
    EXPECT_EQ(c.b_far.find_order(10), nullptr);   // winner dead
    EXPECT_EQ(c.b_far.find_order(11), nullptr);   // sibling cancelled
    EXPECT_EQ(c.e_far.oco_member_count(), 0u);
    EXPECT_EQ(remaining_qty_units(*c.b_near.find_order(12)), 50);
}

// --- LAST-sourced stop fires off an implied print via the dirty drain ---------

TEST(ImpliedEngine, StopTriggersOnImpliedLastPrice) {
    Curve c;
    // Outright swap bid for the triggered stop to sweep.
    send(c.e_swap, c.p_swap, 1, Side::BUY, 480, 10);
    // Pending STOP SELL on swap, trigger 500 on the LAST source.
    send(c.e_swap, c.p_swap, 2, Side::SELL, 0, 10, /*acct=*/2,
         TimeInForce::GTC, 0, OrderType::STOP, /*stop_px=*/500);
    // Legs → implied ask 500; the implied print at 500 must pop the stop.
    send(c.e_far, c.p_far, 3, Side::SELL, 110500, 100);
    send(c.e_near, c.p_near, 4, Side::BUY, 110000, 100);
    send(c.e_swap, c.p_swap, 5, Side::BUY, 600, 40, /*acct=*/9);
    c.drain();  // dirty-flagged swap engine settles → LAST print fires stop
    // Stop converted to market and swept the outright bid.
    EXPECT_EQ(c.b_swap.find_order(2), nullptr);
    EXPECT_EQ(c.b_swap.find_order(1), nullptr);
    EXPECT_EQ(c.e_swap.last_price_ticks(), 480);
}

// --- CurveIngress: shard routing + automatic dirty drain ---------------------

TEST(ImpliedEngine, CurveIngressRoutesAndDrains) {
    Curve c;
    // Shared trade-id stream — implied fills in sibling books draw from
    // the same monotonic counter (one journal space).
    uint64_t shared_tid = 1;
    c.e_near.bind_trade_id_stream(&shared_tid);
    c.e_far.bind_trade_id_stream(&shared_tid);
    c.e_swap.bind_trade_id_stream(&shared_tid);

    CurveIngress in(&c.p_near);  // unrouted nodes free here
    EXPECT_TRUE(in.add_engine(&c.e_near, kNear));
    EXPECT_TRUE(in.add_engine(&c.e_far, kFar));
    EXPECT_TRUE(in.add_engine(&c.e_swap, kSwap));
    EXPECT_FALSE(in.add_engine(&c.e_swap, kSwap));  // dup instrument
    EXPECT_FALSE(in.add_engine(&c.e_swap, 0));      // zero id

    // Legs + taker routed purely by aux.instrument_id — no manual drain.
    send_in(in, c.p_far, 1, kFar, Side::SELL, 110500, 100);
    send_in(in, c.p_near, 2, kNear, Side::BUY, 110000, 100);
    send_in(in, c.p_swap, 3, kSwap, Side::BUY, 600, 40, /*acct=*/9);
    EXPECT_EQ(remaining_qty_units(*c.b_far.find_order(1)), 60);
    EXPECT_EQ(remaining_qty_units(*c.b_near.find_order(2)), 60);
    // Trade ids came from the shared stream: 2 leg fills + 1 presentation.
    EXPECT_EQ(shared_tid, 4u);

    // Cancel routing by ownership probe — the wire carries no instrument.
    in.on_cancel_received(1, /*acct=*/1);
    EXPECT_EQ(c.b_far.find_order(1), nullptr);
    EXPECT_EQ(in.unrouted_cancels(), 0u);
    in.on_cancel_received(999, /*acct=*/1);  // absent on every engine
    EXPECT_EQ(in.unrouted_cancels(), 1u);

    // Unrouted instrument drops + returns the node to the pool.
    send_in(in, c.p_near, 50, /*instr=*/99, Side::BUY, 1, 1);
    EXPECT_EQ(in.unrouted_orders(), 1u);
}
