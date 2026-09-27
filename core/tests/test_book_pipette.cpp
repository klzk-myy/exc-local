// Task 2.3.23 coverage — pipette fixed-point scaling: all matching-engine
// arithmetic on int64 10^8 ticks, instrument-aware pip factors, zero
// floating-point math in the book hot path (spec §3.3a, §24 #402).

#include <gtest/gtest.h>

#include <cstdint>
#include <cstring>
#include <type_traits>
#include <vector>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "utils/MemoryPool.hpp"

// FlatBuffers wire schema (proto/exchange.fbs) encodes raw int64 ticks; when
// the generated header is on the include path we prove integer roundtrip.
#if __has_include("exchange_generated.h") && __has_include(<flatbuffers/flatbuffers.h>)
#define EXCH_TEST_FLATBUFFERS 1
#include "exchange_generated.h"
#else
#define EXCH_TEST_FLATBUFFERS 0
#endif

using namespace exch;

namespace {

// EUR/USD — 5-decimal major: pip_factor 10^1, tick 0.00001.
Instrument eurusd() {
    Instrument i{};
    i.instrument_id = 1;
    std::strncpy(i.symbol, "EUR/USD", sizeof(i.symbol) - 1);
    i.type = InstrumentType::SPOT;
    i.decimal_places = 5;
    i.pip_factor = 10;                       // 10^1 — spec Task 2.3.23
    i.pip_size_ticks = 10'000;               // 1 pip = 0.0001 = 10^4 ticks
    i.tick_size_ticks = 1'000;               // 0.00001 = 10^3 ticks
    i.lot_size_units = 1'000'000;            // 0.01 lot step (10^8 units)
    i.min_order_qty_units = 100'000'000;     // 1.0 unit min
    i.max_order_qty_units = 0;               // unbounded in this test
    i.min_notional_units = 0;
    i.price_band_pct_up = 200;               // 2.00%
    i.price_band_pct_down = 500;             // 5.00%
    i.max_spread_pips = 50;
    i.max_slippage_bps = 100;
    i.display_ratio = 1000;                  // 10.00% iceberg visible
    i.last_price_ticks = 110'505'000;        // 1.10505
    return i;
}

// USD/JPY — 3-decimal JPY pair: pip_factor 10^3, tick 0.001.
Instrument usdjpy() {
    Instrument i = eurusd();
    i.instrument_id = 2;
    std::strncpy(i.symbol, "USD/JPY", sizeof(i.symbol) - 1);
    i.decimal_places = 3;
    i.pip_factor = 1'000;                    // 10^3 — spec Task 2.3.23
    i.pip_size_ticks = 1'000'000;            // 1 pip = 0.01 = 10^6 ticks
    i.tick_size_ticks = 100'000;             // 0.001 = 10^5 ticks
    i.last_price_ticks = 14'950'500'000;     // 149.505
    return i;
}

Order mk_order(uint64_t id, Side side, int64_t price_ticks, int64_t qty_units,
               uint64_t ts) {
    Order o{};
    o.id = id;
    o.account_id = 7;
    o.side = side;
    o.type = OrderType::LIMIT;
    o.tif = TimeInForce::GTC;
    o.price_ticks = price_ticks;
    o.qty_units = qty_units;
    o.timestamp_ns = ts;
    o.ingress_seq = ts;
    return o;
}

}  // namespace

// --- Fixed-point representation ---------------------------------------------

TEST(PipetteTypes, OrderFieldsAreInt64Ticks) {
    // §24 #402: the matching engine order book operates strictly on int64.
    static_assert(std::is_same_v<decltype(Order::price_ticks), int64_t>);
    static_assert(std::is_same_v<decltype(Order::qty_units), int64_t>);
    static_assert(std::is_same_v<decltype(Order::filled_qty_units), int64_t>);
    static_assert(std::is_same_v<decltype(Order::display_qty_units), int64_t>);
    static_assert(std::is_same_v<decltype(PriceLevel::price_ticks), int64_t>);
    static_assert(std::is_same_v<decltype(PriceLevel::total_qty_units), int64_t>);
    static_assert(std::is_same_v<decltype(SnapshotLevel::price_ticks), int64_t>);
    static_assert(std::is_same_v<decltype(SnapshotLevel::total_qty_units), int64_t>);
    SUCCEED();
}

TEST(PipetteTypes, InstrumentIsPodWithPipFactor) {
    static_assert(std::is_aggregate_v<Instrument>);
    static_assert(std::is_trivially_copyable_v<Instrument>);
    const Instrument e = eurusd();
    const Instrument j = usdjpy();
    EXPECT_TRUE(is_valid(e));
    EXPECT_TRUE(is_valid(j));
    EXPECT_EQ(e.pip_factor, 10);        // 5-decimal pair → 10^1
    EXPECT_EQ(j.pip_factor, 1'000);     // 3-decimal JPY pair → 10^3
}

// --- Exact pipette arithmetic -------------------------------------------------
// 1 pipette = 0.1 pip. Exactness here is binary integer equality — no
// representational rounding exists at the 10^8 scale.

TEST(PipetteMath, EurUsdFiveDecimalExact) {
    const Instrument i = eurusd();
    EXPECT_EQ(ticks_per_pip(i), 10'000);      // 0.0001 in ticks
    EXPECT_EQ(ticks_per_pipette(i), 1'000);   // 0.00001 in ticks — the 5th decimal
    // 1.10505 EUR/USD = 110'505'000 ticks at 10^8.
    constexpr int64_t p0 = 110'505'000;
    // +0.1 pip (one pipette) == +1'000 ticks exactly.
    EXPECT_EQ(p0 + ticks_per_pipette(i), 110'506'000);  // 1.10506
    // +1 pip == +10'000 ticks == 1.10605.
    EXPECT_EQ(p0 + ticks_per_pip(i), 110'515'000);
    // Bid 1.10505 / ask 1.10510 → 5 pipettes = 0.5 pip spread.
    const int64_t ask = 110'510'000;
    EXPECT_EQ(ask - p0, 5'000);
    EXPECT_EQ(spread_pipettes(ask, p0, i), 5);
    EXPECT_EQ(spread_pips(ask, p0, i), 0);    // whole pips truncate (spec formula)
    // 3.0-pip spread: bid 110'500'000 / ask 110'530'000.
    EXPECT_EQ(spread_pips(110'530'000, 110'500'000, i), 3);
    // Conversion round-trips at integer precision.
    EXPECT_EQ(pips_to_ticks(3, i), 30'000);
    EXPECT_EQ(pipettes_to_ticks(5, i), 5'000);
}

TEST(PipetteMath, UsdJpyThreeDecimalExact) {
    const Instrument i = usdjpy();
    EXPECT_EQ(ticks_per_pip(i), 1'000'000);      // 0.01 in ticks
    EXPECT_EQ(ticks_per_pipette(i), 100'000);    // 0.001 in ticks — the 3rd decimal
    // 149.505 USD/JPY = 14'950'500'000 ticks.
    constexpr int64_t p0 = 14'950'500'000;
    // +0.1 pip (pipette) == +100'000 ticks → 149.506.
    EXPECT_EQ(p0 + ticks_per_pipette(i), 14'950'600'000);
    // +1 pip → 149.515.
    EXPECT_EQ(p0 + ticks_per_pip(i), 14'951'500'000);
    // 0.5-pip spread (5 pipettes) between 149.505 and 149.510.
    const int64_t ask = 14'951'000'000;
    EXPECT_EQ(ask - p0, 500'000);
    EXPECT_EQ(spread_pipettes(ask, p0, i), 5);
    EXPECT_EQ(spread_pips(ask, p0, i), 0);
    // 1.00 JPY move == 100 pips exactly.
    EXPECT_EQ(spread_pips(15'050'500'000, p0, i), 100);
    // Sub-pipette residuals truncate deterministically (integer division).
    EXPECT_EQ(spread_pips(14'950'599'999, p0, i), 0);   // 0.9999 pipette
    EXPECT_EQ(spread_pipettes(14'950'599'999, p0, i), 0);
}

TEST(PipetteMath, UnconfiguredInstrumentFailsClosed) {
    Instrument bad{};
    bad.pip_factor = 0;  // reference data missing → never report "tight" spread
    EXPECT_EQ(spread_pips(200, 100, bad), INT64_MAX);
    EXPECT_EQ(spread_pipettes(200, 100, bad), INT64_MAX);
    EXPECT_FALSE(is_valid(bad));
}

// --- Instrument integer checks (spec §3.3 #11/#12/#6 inputs) -------------------

TEST(PipetteChecks, TickAndLotQuantization) {
    const Instrument e = eurusd();
    EXPECT_TRUE(is_tick_multiple(110'505'000, e));   // 1.10505 = 110505 ticks*1000
    EXPECT_FALSE(is_tick_multiple(110'505'001, e));  // sub-tick
    EXPECT_TRUE(is_lot_multiple(500'000'000, e));    // 5.0 == 500 * lot step
    EXPECT_FALSE(is_lot_multiple(500'500'000, e));   // off-step
    const Instrument j = usdjpy();
    EXPECT_TRUE(is_tick_multiple(14'950'500'000, j));
    EXPECT_FALSE(is_tick_multiple(14'950'500'001, j));
}

TEST(PipetteChecks, NotionalPriceBandIcebergAreIntegerExact) {
    const Instrument e = eurusd();
    int64_t out = 0;
    // 100'000 EUR at 1.10505 → 110'505.00 USD notional (in 10^8 units).
    ASSERT_TRUE(notional_units(10'000'000'000'000, 110'505'000, out));
    EXPECT_EQ(out, 11'050'500'000'000);
    // Overflow narrows fail-closed.
    EXPECT_FALSE(notional_units(INT64_MAX, INT64_MAX, out));
    // Price band around last_price 1.10505: +2.00% / −5.00%, exact integers.
    int64_t lo = 0, hi = 0;
    ASSERT_TRUE(price_band_bounds(e, e.last_price_ticks, lo, hi));
    EXPECT_EQ(hi, 112'715'100);   // 110'505'000 * 1.0200 — exact
    EXPECT_EQ(lo, 104'979'750);   // 110'505'000 * 0.9500 — exact
    // ICEBERG display ratio: 10.00% of 100'000 units → 10'000 units.
    ASSERT_TRUE(iceberg_visible_units(e, 10'000'000'000'000, out));
    EXPECT_EQ(out, 1'000'000'000'000);
}

// --- Book integration: integer spread over live state -------------------------

TEST(PipetteBook, SpreadPipsFromLiveBook) {
    MemoryPool<Order> pool(64);
    const Instrument e = eurusd();
    OrderBook book(pool, e);
    int64_t pips = -1;
    EXPECT_FALSE(book.spread_pips(pips));          // empty → undefined
    Order* o = nullptr;
    ASSERT_EQ(book.add_order(mk_order(1, Side::BUY, 110'500'000, 1'000'000, 1),
                             &o), BookError::OK);
    EXPECT_FALSE(book.spread_pips(pips));          // one side only
    ASSERT_EQ(book.add_order(mk_order(2, Side::SELL, 110'530'000, 1'000'000, 2),
                             &o), BookError::OK);
    ASSERT_TRUE(book.spread_pips(pips));
    EXPECT_EQ(pips, 3);                            // 30'000 ticks / 10'000
    // Same check on a JPY book.
    MemoryPool<Order> pool2(64);
    const Instrument j = usdjpy();
    OrderBook book2(pool2, j);
    ASSERT_EQ(book2.add_order(mk_order(1, Side::BUY, 14'950'500'000,
                                     1'000'000, 1), &o), BookError::OK);
    ASSERT_EQ(book2.add_order(mk_order(2, Side::SELL, 14'952'500'000,
                                     1'000'000, 2), &o), BookError::OK);
    ASSERT_TRUE(book2.spread_pips(pips));
    EXPECT_EQ(pips, 2);   // 0.020 JPY / 0.01 = 2 pips — exact
}

// --- Wire encoding: raw integer ticks, no float/string conversion -------------

// Minimal SBE-style fixed-layout encode: a POD memcpy — the exact byte
// pattern the SBE codec will emit for price/qty (Task 18.3.8 owns the full
// codec; the book's contract is that ticks are plain int64).
TEST(PipetteWire, RawTickEncodingRoundTrips) {
    struct PackedOrder {  // SBE-style packed layout
        uint64_t id;
        int64_t price_ticks;
        int64_t qty_units;
    };
    const PackedOrder src{42, 14'950'500'000, 10'000'000'000'000};
    uint8_t buf[sizeof(PackedOrder)];
    std::memcpy(buf, &src, sizeof(buf));
    PackedOrder dst{};
    std::memcpy(&dst, buf, sizeof(buf));
    EXPECT_EQ(dst.id, 42u);
    EXPECT_EQ(dst.price_ticks, 14'950'500'000);   // 149.505 USD/JPY exact
    EXPECT_EQ(dst.qty_units, 10'000'000'000'000); // 100'000 units exact
}

#if EXCH_TEST_FLATBUFFERS
TEST(PipetteWire, FlatBuffersEncodesIntegerTicks) {
    // proto/exchange.fbs carries int64 price/qty — no FP fields in the flow.
    flatbuffers::FlatBufferBuilder b(512);
    exc::wire::PriceLevelBuilder lb(b);
    lb.add_price(110'530'000);
    lb.add_qty(500'000'000);
    lb.add_count(2);
    const auto lvl = lb.Finish();
    std::vector<flatbuffers::Offset<exc::wire::PriceLevel>> asks{lvl};
    const auto asks_off = b.CreateVector(asks);
    exc::wire::BookSnapshotBuilder sb(b);
    sb.add_instrument_id(1);
    sb.add_seq(77);
    sb.add_asks(asks_off);
    const auto snap = sb.Finish();
    b.Finish(snap);
    // Decode and verify the integers came through untouched.
    const auto* got =
        flatbuffers::GetRoot<exc::wire::BookSnapshot>(b.GetBufferPointer());
    ASSERT_EQ(got->asks()->size(), 1u);
    EXPECT_EQ(got->asks()->Get(0)->price(), 110'530'000);   // 1.10530 — exact
    EXPECT_EQ(got->asks()->Get(0)->qty(), 500'000'000);     // 5.0 units — exact
    EXPECT_EQ(got->asks()->Get(0)->count(), 2u);
    EXPECT_EQ(got->seq(), 77u);
}
#endif
