// Task 2.3.26 — DiscretionaryExecutor coverage (spec §6.11, §24 #405;
// migration 103 orders.discretionary_offset_pips):
//   * band math per side (limit +/- offset_pips * pip_size_ticks)
//   * fail-closed rejects: negative/sentinel offsets, cap breach,
//     degenerate instrument, checked-arithmetic overflow
//   * eligibility gate: LIMIT + {GTC,GTD,DAY} only, IOC/FOK/POST_ONLY out
//   * remainder-at-limit semantics on a real OrderBook + MatchingEngine
//     pair (read-only use): sweep inside the hidden band, unfilled
//     balance rests at the nominal limit, public depth shows limit only.
//
// The engine-side wiring is the documented seam in
// matching/DiscretionaryExecutor.hpp (engine owner). The tests below
// emulate that seam faithfully: the ingress order is submitted to the real
// engine as a GTC LIMIT priced at ev.aggressive_price_ticks (the taker
// walk bound — fills land at maker prices exactly as walk_match would),
// then the surviving remainder is re-priced to the nominal limit via
// modify_order — identical end-state to the seam's rest_remainder path
// (same order id, filled_qty_units preserved, rests at limit).

#include <gtest/gtest.h>

#include <cstdint>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "matching/DiscretionaryExecutor.hpp"
#include "matching/MatchingEngine.hpp"
#include "utils/MemoryPool.hpp"

using namespace exch;

namespace {

constexpr int64_t kPip = 10'000;          // 5-dec pair: pip_size_ticks
constexpr int64_t kLimit = 110'000'000;   // EUR/USD 1.10000 at 10^8 scale

uint64_t g_ts = 1'000'000'000;            // deterministic ingress stamps

// Internally-consistent 5-decimal instrument (pip_factor*1000 ==
// pip_size_ticks, pip_size % tick == 0 — the is_valid() invariant chain).
// max_spread_pips = 100 => discretionary cap = 200 pips (§6.11.3).
Instrument make_instrument() {
    Instrument i{};
    i.instrument_id = 7;
    i.decimal_places = 5;
    i.pip_factor = 10;
    i.pip_size_ticks = ticks_per_pip(i);  // 10'000
    i.tick_size_ticks = 1'000;
    i.lot_size_units = 1;
    i.max_spread_pips = 100;
    return i;
}

Order* mk(MemoryPool<Order>& pool, uint64_t id, Side side, OrderType type,
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
    o->quantity = Decimal::from_mantissa(qty);  // compat mirror
    o->timestamp_ns = ++g_ts;
    o->ingress_seq = g_ts;
    return o;
}

struct Fixture {
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    MatchingEngine engine{3, book, pool, nullptr, nullptr};
};

int64_t side_depth(const OrderBook& b, Side s) {
    int64_t q = 0;
    for (std::size_t d = 0;; ++d) {
        const PriceLevel* l = b.level(s, d);
        if (l == nullptr) break;
        q += l->total_qty_units;
    }
    return q;
}

// Seam emulation (see DiscretionaryExecutor.hpp "ENGINE SEAM" §2/§3):
//   eval() -> walk at ev.aggressive_price_ticks -> remainder at limit.
// Walk is real: the ingress node is a GTC LIMIT priced at the band edge, so
// walk_match sweeps every contra level inside the band at maker prices and
// rests the surplus. The surplus is then re-priced to the nominal limit —
// same observable end-state as rest_remainder() on a node whose
// price_ticks never left the limit (same id, fills preserved).
void submit_discretionary(MatchingEngine& eng, MemoryPool<Order>& pool,
                          uint64_t id, Side side, int64_t limit, int64_t qty,
                          int64_t offset_pips, const Instrument& inst,
                          uint64_t acct = 2) {
    const DiscretionaryEval ev =
        DiscretionaryExecutor::eval(side, limit, offset_pips, inst);
    ASSERT_EQ(ev.reject_code, nullptr);
    eng.on_order_received(mk(pool, id, side, OrderType::LIMIT,
                             ev.aggressive_price_ticks, qty, acct,
                             TimeInForce::GTC));
    if (eng.book().find_order(id) != nullptr && ev.has_band) {
        ++g_ts;
        ASSERT_EQ(eng.book().modify_order(id, limit, qty, g_ts, g_ts),
                  BookError::OK);
    }
}

}  // namespace

// --- Pure band math -------------------------------------------------------

TEST(DiscretionaryEval, BuyBandIsLimitPlusOffsetTimesPipSize) {
    const Instrument i = make_instrument();
    const DiscretionaryEval ev =
        DiscretionaryExecutor::eval(Side::BUY, kLimit, 5, i);
    ASSERT_EQ(ev.reject_code, nullptr);
    EXPECT_TRUE(ev.has_band);
    EXPECT_EQ(ev.aggressive_price_ticks, kLimit + 5 * kPip);
}

TEST(DiscretionaryEval, SellBandIsLimitMinusOffsetTimesPipSize) {
    const Instrument i = make_instrument();
    const DiscretionaryEval ev =
        DiscretionaryExecutor::eval(Side::SELL, kLimit, 5, i);
    ASSERT_EQ(ev.reject_code, nullptr);
    EXPECT_TRUE(ev.has_band);
    EXPECT_EQ(ev.aggressive_price_ticks, kLimit - 5 * kPip);
}

TEST(DiscretionaryEval, ZeroOffsetIsNoBandPassthrough) {
    const Instrument i = make_instrument();
    const DiscretionaryEval ev =
        DiscretionaryExecutor::eval(Side::BUY, kLimit, 0, i);
    ASSERT_EQ(ev.reject_code, nullptr);
    EXPECT_FALSE(ev.has_band);
    EXPECT_EQ(ev.aggressive_price_ticks, kLimit);
}

TEST(DiscretionaryEval, NegativeOffsetsRejectFailClosed) {
    const Instrument i = make_instrument();
    for (const int64_t off : {int64_t{-1}, int64_t{-7}, INT64_MIN}) {
        const DiscretionaryEval ev =
            DiscretionaryExecutor::eval(Side::BUY, kLimit, off, i);
        EXPECT_STREQ(ev.reject_code,
                     DiscretionaryExecutor::kRejectOffsetInvalid);
        EXPECT_FALSE(ev.has_band);
        EXPECT_EQ(ev.aggressive_price_ticks, kLimit);  // safe verdict
    }
}

TEST(DiscretionaryEval, OffsetAboveMaxSpreadTimesTwoRejects) {
    Instrument i = make_instrument();
    i.max_spread_pips = 10;  // cap = 20 pips (spec §6.11.3 literal)
    EXPECT_EQ(DiscretionaryExecutor::eval(Side::BUY, kLimit, 20, i)
                  .reject_code,
              nullptr);
    EXPECT_STREQ(DiscretionaryExecutor::eval(Side::BUY, kLimit, 21, i)
                     .reject_code,
                 DiscretionaryExecutor::kRejectOffsetInvalid);
}

TEST(DiscretionaryEval, UnconfiguredSpreadCapRejectsEveryBand) {
    Instrument i = make_instrument();
    i.max_spread_pips = 0;  // fail-closed: bound 0 => no band is admissible
    EXPECT_STREQ(DiscretionaryExecutor::eval(Side::SELL, kLimit, 1, i)
                     .reject_code,
                 DiscretionaryExecutor::kRejectOffsetInvalid);
    EXPECT_EQ(DiscretionaryExecutor::eval(Side::SELL, kLimit, 0, i)
                  .reject_code,
              nullptr);  // offset 0 stays a passthrough
}

TEST(DiscretionaryEval, DegeneratePipSizeRejects) {
    Instrument i = make_instrument();
    i.pip_size_ticks = 0;  // band inexpressible — never silently zero
    EXPECT_STREQ(DiscretionaryExecutor::eval(Side::BUY, kLimit, 5, i)
                     .reject_code,
                 DiscretionaryExecutor::kRejectOffsetInvalid);
}

TEST(DiscretionaryEval, BuyBandOverflowRejects) {
    const Instrument i = make_instrument();
    const DiscretionaryEval ev = DiscretionaryExecutor::eval(
        Side::BUY, INT64_MAX - 3 * kPip, 5, i);  // +50'000 overflows
    EXPECT_STREQ(ev.reject_code,
                 DiscretionaryExecutor::kRejectOffsetInvalid);
    EXPECT_EQ(ev.aggressive_price_ticks, INT64_MAX - 3 * kPip);
}

TEST(DiscretionaryEval, SellBandClampsAtTickFloor) {
    const Instrument i = make_instrument();
    // limit 2 ticks, offset 5 pips -> raw edge far below 0. Non-positive
    // prices cannot rest on the book; the band clamps at tick_size_ticks
    // ("take any positive bid") rather than rejecting valid intent.
    const DiscretionaryEval ev =
        DiscretionaryExecutor::eval(Side::SELL, 2 * i.tick_size_ticks, 5, i);
    ASSERT_EQ(ev.reject_code, nullptr);
    EXPECT_TRUE(ev.has_band);
    EXPECT_EQ(ev.aggressive_price_ticks, i.tick_size_ticks);
}

// --- Eligibility gate (spec §6.11.3) ---------------------------------------

TEST(DiscretionaryValidate, LimitGtcGtdDayPermitted) {
    const Instrument i = make_instrument();
    for (const TimeInForce t :
         {TimeInForce::GTC, TimeInForce::GTD, TimeInForce::DAY}) {
        Order o{};
        o.type = OrderType::LIMIT;
        o.tif = t;
        o.price_ticks = kLimit;
        EXPECT_EQ(DiscretionaryExecutor::validate(o, 5, i), nullptr)
            << "tif " << static_cast<int>(t);
    }
}

TEST(DiscretionaryValidate, IocFokPostOnlyNonLimitRejected) {
    const Instrument i = make_instrument();
    Order o{};
    o.type = OrderType::LIMIT;
    o.price_ticks = kLimit;

    o.tif = TimeInForce::IOC;
    EXPECT_STREQ(DiscretionaryExecutor::validate(o, 5, i),
                 DiscretionaryExecutor::kRejectOffsetInvalid);
    o.tif = TimeInForce::FOK;
    EXPECT_STREQ(DiscretionaryExecutor::validate(o, 5, i),
                 DiscretionaryExecutor::kRejectOffsetInvalid);

    o.tif = TimeInForce::GTC;
    o.flags = kOrderFlagPostOnly;
    EXPECT_STREQ(DiscretionaryExecutor::validate(o, 5, i),
                 DiscretionaryExecutor::kRejectOffsetInvalid);
    o.flags = 0;

    for (const OrderType t : {OrderType::MARKET, OrderType::STOP,
                              OrderType::STOP_LIMIT, OrderType::ICEBERG}) {
        o.type = t;
        EXPECT_STREQ(DiscretionaryExecutor::validate(o, 5, i),
                     DiscretionaryExecutor::kRejectOffsetInvalid)
            << "type " << static_cast<int>(t);
    }
}

TEST(DiscretionaryValidate, ZeroOffsetAlwaysEligible) {
    const Instrument i = make_instrument();
    Order o{};
    o.type = OrderType::MARKET;      // would reject with a nonzero offset
    o.tif = TimeInForce::IOC;
    EXPECT_EQ(DiscretionaryExecutor::validate(o, 0, i), nullptr);
}

TEST(DiscretionaryValidate, DelegatesNumericGatesToEval) {
    const Instrument i = make_instrument();
    Order o{};
    o.type = OrderType::LIMIT;
    o.tif = TimeInForce::GTC;
    o.price_ticks = kLimit;
    EXPECT_STREQ(DiscretionaryExecutor::validate(o, -1, i),
                 DiscretionaryExecutor::kRejectOffsetInvalid);
    EXPECT_STREQ(DiscretionaryExecutor::validate(o, 201, i),  // cap = 200
                 DiscretionaryExecutor::kRejectOffsetInvalid);
}

// --- Real engine: sweep inside band, remainder rests at limit --------------

TEST(DiscretionaryEngine, BuySweepsWithinBandThenRestsAtLimit) {
    Fixture f;
    const Instrument i = make_instrument();
    // Resting asks: 30 @ +1pip, 40 @ +3pips inside the 5-pip band;
    // 50 @ +6pips beyond the band edge — must survive untouched.
    f.engine.on_order_received(mk(f.pool, 11, Side::SELL, OrderType::LIMIT,
                                  kLimit + 1 * kPip, 30, /*acct=*/9));
    f.engine.on_order_received(mk(f.pool, 12, Side::SELL, OrderType::LIMIT,
                                  kLimit + 3 * kPip, 40, /*acct=*/9));
    f.engine.on_order_received(mk(f.pool, 13, Side::SELL, OrderType::LIMIT,
                                  kLimit + 6 * kPip, 50, /*acct=*/9));

    submit_discretionary(f.engine, f.pool, /*id=*/21, Side::BUY, kLimit,
                         /*qty=*/100, /*offset_pips=*/5, i);

    // Took 30 + 40 inside the band at maker prices (price improvement —
    // last fill prints the +3pip ask, not the +5pip band edge).
    EXPECT_EQ(f.engine.trades_emitted(), 2u);
    EXPECT_EQ(f.engine.last_price_ticks(),
              static_cast<uint64_t>(kLimit + 3 * kPip));

    // Remainder 30 rests at the nominal limit under the same order id.
    const Order* resting = f.book.find_order(21);
    ASSERT_NE(resting, nullptr);
    EXPECT_EQ(resting->price_ticks, kLimit);
    EXPECT_EQ(resting->filled_qty_units, 70);
    EXPECT_EQ(remaining_qty_units(*resting), 30);
    EXPECT_EQ(f.book.bid_count(), 1u);
    EXPECT_EQ(f.book.ask_count(), 1u);          // only the out-of-band ask
    const PriceLevel* ask = f.book.level(Side::SELL, 0);
    ASSERT_NE(ask, nullptr);
    EXPECT_EQ(ask->price_ticks, kLimit + 6 * kPip);
    EXPECT_EQ(ask->total_qty_units, 50);

    // Public depth (L2 snapshot = BookSerializer input) exposes only the
    // nominal limit — the band edge 110'050'000 appears nowhere.
    const BookSnapshot snap = f.book.snapshot();
    ASSERT_EQ(snap.bids.size(), 1u);
    EXPECT_EQ(snap.bids[0].price_ticks, kLimit);
    EXPECT_EQ(snap.bids[0].total_qty_units, 30);
    for (const SnapshotLevel& l : snap.bids) {
        EXPECT_NE(l.price_ticks, kLimit + 5 * kPip);
    }
    EXPECT_TRUE(f.book.validate());
    EXPECT_FALSE(f.book.crossed());
}

TEST(DiscretionaryEngine, SellSweepsWithinBandThenRestsAtLimit) {
    Fixture f;
    const Instrument i = make_instrument();
    // Resting bids: 30 @ -1pip, 40 @ -3pips inside; 50 @ -6pips outside.
    f.engine.on_order_received(mk(f.pool, 11, Side::BUY, OrderType::LIMIT,
                                  kLimit - 1 * kPip, 30, /*acct=*/9));
    f.engine.on_order_received(mk(f.pool, 12, Side::BUY, OrderType::LIMIT,
                                  kLimit - 3 * kPip, 40, /*acct=*/9));
    f.engine.on_order_received(mk(f.pool, 13, Side::BUY, OrderType::LIMIT,
                                  kLimit - 6 * kPip, 50, /*acct=*/9));

    submit_discretionary(f.engine, f.pool, /*id=*/21, Side::SELL, kLimit,
                         /*qty=*/100, /*offset_pips=*/5, i);

    EXPECT_EQ(f.engine.trades_emitted(), 2u);
    EXPECT_EQ(f.engine.last_price_ticks(),
              static_cast<uint64_t>(kLimit - 3 * kPip));

    const Order* resting = f.book.find_order(21);
    ASSERT_NE(resting, nullptr);
    EXPECT_EQ(resting->price_ticks, kLimit);   // nominal limit, not band edge
    EXPECT_EQ(remaining_qty_units(*resting), 30);
    const BookSnapshot snap = f.book.snapshot();
    ASSERT_EQ(snap.asks.size(), 1u);
    EXPECT_EQ(snap.asks[0].price_ticks, kLimit);
    ASSERT_EQ(snap.bids.size(), 1u);
    EXPECT_EQ(snap.bids[0].price_ticks, kLimit - 6 * kPip);
    EXPECT_EQ(snap.bids[0].total_qty_units, 50);
    EXPECT_TRUE(f.book.validate());
}

TEST(DiscretionaryEngine, FullFillInsideBandLeavesNoResidual) {
    Fixture f;
    const Instrument i = make_instrument();
    f.engine.on_order_received(mk(f.pool, 11, Side::SELL, OrderType::LIMIT,
                                  kLimit + 1 * kPip, 30, /*acct=*/9));
    f.engine.on_order_received(mk(f.pool, 12, Side::SELL, OrderType::LIMIT,
                                  kLimit + 3 * kPip, 40, /*acct=*/9));

    submit_discretionary(f.engine, f.pool, /*id=*/21, Side::BUY, kLimit,
                         /*qty=*/60, /*offset_pips=*/5, i);

    EXPECT_EQ(f.engine.trades_emitted(), 2u);
    EXPECT_EQ(f.book.find_order(21), nullptr);  // nothing rests
    EXPECT_EQ(side_depth(f.book, Side::BUY), 0);
    // Second ask took the residual 30; 10 remains at +3pips.
    const PriceLevel* ask = f.book.level(Side::SELL, 0);
    ASSERT_NE(ask, nullptr);
    EXPECT_EQ(ask->price_ticks, kLimit + 3 * kPip);
    EXPECT_EQ(ask->total_qty_units, 10);
    EXPECT_TRUE(f.book.validate());
}

TEST(DiscretionaryEngine, ZeroOffsetRestsLikePlainLimit) {
    Fixture f;
    const Instrument i = make_instrument();
    f.engine.on_order_received(mk(f.pool, 11, Side::SELL, OrderType::LIMIT,
                                  kLimit + 6 * kPip, 50, /*acct=*/9));

    submit_discretionary(f.engine, f.pool, /*id=*/21, Side::BUY, kLimit,
                         /*qty=*/30, /*offset_pips=*/0, i);

    EXPECT_EQ(f.engine.trades_emitted(), 0u);   // +6pips beyond limit — no fill
    const Order* resting = f.book.find_order(21);
    ASSERT_NE(resting, nullptr);
    EXPECT_EQ(resting->price_ticks, kLimit);
    EXPECT_EQ(remaining_qty_units(*resting), 30);
    EXPECT_TRUE(f.book.validate());
}

// ---------------------------------------------------------------------------
// Engine-intake integration (Task 2.3.26): the offset now rides OrderAux
// end-to-end — on_order_received(order, aux) evaluates the band inside the
// LIMIT arm, sweeps to the aggressive bound, and rests the remainder at the
// nominal limit. These cases exercise the REAL ingress path.
// ---------------------------------------------------------------------------

namespace {

OrderAux aux_with_offset(int64_t offset_pips) {
    OrderAux a{};
    a.discretionary_offset_pips = offset_pips;
    return a;
}

}  // namespace

TEST(DiscretionaryEngineIntake, BandSweepThenRestAtNominalLimit) {
    Fixture g;
    const Instrument i = make_instrument();
    g.book.set_instrument(i);
    // Asks at limit+1pip, +3pips, +6pips — offset 5pips reaches the first two.
    g.engine.on_order_received(mk(g.pool, 11, Side::SELL, OrderType::LIMIT,
                                  kLimit + 1 * kPip, 30, 9));
    g.engine.on_order_received(mk(g.pool, 12, Side::SELL, OrderType::LIMIT,
                                  kLimit + 3 * kPip, 30, 9));
    g.engine.on_order_received(mk(g.pool, 13, Side::SELL, OrderType::LIMIT,
                                  kLimit + 6 * kPip, 30, 9));

    Order* t = mk(g.pool, 21, Side::BUY, OrderType::LIMIT, kLimit, 50, 2);
    g.engine.on_order_received(t, aux_with_offset(/*offset_pips=*/5));

    // Band reaches +5pips: fills at +1 and +3 (60 units at maker prices),
    // 50 < 60 -> fully filled, nothing rests.
    EXPECT_EQ(g.engine.trades_emitted(), 2u);
    EXPECT_EQ(g.book.find_order(21), nullptr);
    EXPECT_TRUE(g.book.validate());

    // Deeper asks survive untouched.
    EXPECT_NE(g.book.find_order(13), nullptr);
}

TEST(DiscretionaryEngineIntake, PartialBandSweepRestsRemainderAtLimit) {
    Fixture f;
    const Instrument i = make_instrument();
    f.book.set_instrument(i);
    f.engine.on_order_received(mk(f.pool, 11, Side::SELL, OrderType::LIMIT,
                                  kLimit + 1 * kPip, 30, 9));
    f.engine.on_order_received(mk(f.pool, 12, Side::SELL, OrderType::LIMIT,
                                  kLimit + 6 * kPip, 30, 9));

    // qty 80 vs 30 within band -> 50 rest at the NOMINAL limit, not the band.
    Order* t = mk(f.pool, 21, Side::BUY, OrderType::LIMIT, kLimit, 80, 2);
    f.engine.on_order_received(t, aux_with_offset(5));

    EXPECT_EQ(f.engine.trades_emitted(), 1u);  // 30 @ +1pip
    const Order* resting = f.book.find_order(21);
    ASSERT_NE(resting, nullptr);
    EXPECT_EQ(resting->price_ticks, kLimit);           // nominal limit only
    EXPECT_EQ(remaining_qty_units(*resting), 50);
    EXPECT_EQ(resting->filled_qty_units, 30);
    // The +6pip ask (outside the +5 band) is untouched.
    EXPECT_NE(f.book.find_order(12), nullptr);
    EXPECT_TRUE(f.book.validate());
}

TEST(DiscretionaryEngineIntake, NegativeAndOverCapOffsetsRejectBeforeWal) {
    Fixture f;
    const Instrument i = make_instrument();  // max_spread_pips=100 -> cap 200
    f.book.set_instrument(i);

    Order* neg = mk(f.pool, 21, Side::BUY, OrderType::LIMIT, kLimit, 10, 2);
    f.engine.on_order_received(neg, aux_with_offset(-1));
    EXPECT_STREQ(f.engine.last_reject(),
                 DiscretionaryExecutor::kRejectOffsetInvalid);
    EXPECT_EQ(f.book.find_order(21), nullptr);

    Order* cap = mk(f.pool, 22, Side::BUY, OrderType::LIMIT, kLimit, 10, 2);
    f.engine.on_order_received(cap, aux_with_offset(201));
    EXPECT_STREQ(f.engine.last_reject(),
                 DiscretionaryExecutor::kRejectOffsetInvalid);
    EXPECT_EQ(f.book.find_order(22), nullptr);
    EXPECT_TRUE(f.book.validate());
}

TEST(DiscretionaryEngineIntake, IocAndPostOnlyOffsetsRejected) {
    Fixture f;
    f.book.set_instrument(make_instrument());

    Order* ioc = mk(f.pool, 21, Side::BUY, OrderType::LIMIT, kLimit, 10, 2,
                    TimeInForce::IOC);
    f.engine.on_order_received(ioc, aux_with_offset(5));
    EXPECT_STREQ(f.engine.last_reject(),
                 DiscretionaryExecutor::kRejectOffsetInvalid);

    Order* po = mk(f.pool, 22, Side::BUY, OrderType::LIMIT, kLimit, 10, 2,
                   TimeInForce::GTC, kOrderFlagPostOnly);
    f.engine.on_order_received(po, aux_with_offset(5));
    EXPECT_STREQ(f.engine.last_reject(),
                 DiscretionaryExecutor::kRejectOffsetInvalid);
    EXPECT_TRUE(f.book.validate());
}

TEST(DiscretionaryEngineIntake, SellBandMirror) {
    Fixture f;
    const Instrument i = make_instrument();
    f.book.set_instrument(i);
    f.engine.on_order_received(mk(f.pool, 11, Side::BUY, OrderType::LIMIT,
                                  kLimit - 1 * kPip, 30, 9));
    f.engine.on_order_received(mk(f.pool, 12, Side::BUY, OrderType::LIMIT,
                                  kLimit - 6 * kPip, 30, 9));

    Order* t = mk(f.pool, 21, Side::SELL, OrderType::LIMIT, kLimit, 80, 2);
    f.engine.on_order_received(t, aux_with_offset(5));

    EXPECT_EQ(f.engine.trades_emitted(), 1u);  // only -1pip bid reachable
    const Order* resting = f.book.find_order(21);
    ASSERT_NE(resting, nullptr);
    EXPECT_EQ(resting->price_ticks, kLimit);
    EXPECT_EQ(remaining_qty_units(*resting), 50);
    EXPECT_TRUE(f.book.validate());
}
