// Phase-16 advanced order types coverage (spec §6.2/§6.2a/§6.2b;
// Tasks 16.3.3, 16.3.11, 16.3.13, 16.3.15, 16.3.16, 16.3.17, 16.3.22,
// 16.3.25):
//
//   * Trailing stops — PIPS/PERCENTAGE/ABSOLUTE distance units, activation
//     gate, favorable-direction-only anchor ratchet, trigger conversion.
//   * Conditional trigger sources — LAST_PRICE / MARK_PRICE / INDEX_PRICE,
//     stale-oracle fail-closed freeze with per-source isolation.
//   * Pegged orders — PEG_TO_MID / PRIMARY / MARKET references, signed
//     offsets, collars, BBO-following repricing, L2 exclusion,
//     PEGGED_PRICING_UNAVAILABLE admission.
//   * Hidden/dark orders — public-L2 omission, visible-BBO midpoint fills,
//     fail-closed hold when no midpoint exists.
//   * GSLO — admission exposure reserve/cap, exact armed-stop fill at the
//     synthetic venue id, release on cancel/trigger.
//   * MOO/MOC — CALL-phase parking, uncross participation, T-30s freeze
//     cancel refusal, ORDER_INVALID outside a CALL.
//   * WAL — ORDER_NEW_EX encoding, ORDER_TRIGGERED/PEG_REPRICE rows,
//     feedless-replay convergence through RecoveryManager.
//   * EnginePump — extended OrderNew decode (peg/trail/oracle/flags) and
//     malformed-field rejection.

#include <gtest/gtest.h>
#include <unistd.h>

#include <cstdint>
#include <cstring>
#include <deque>
#include <filesystem>
#include <string>
#include <vector>

#include "book/Instrument.hpp"
#include "book/OrderBook.hpp"
#include "exchange_generated.h"
#include "ipc/EnginePump.hpp"
#include "ipc/IpcChannel.hpp"
#include "risk/InstrumentFeed.hpp"
#include "marketdata/BookSerializer.hpp"
#include "matching/IpcPublisher.hpp"
#include "matching/MatchingEngine.hpp"
#include "recovery/RecoveryManager.hpp"
#include "recovery/SnapshotStore.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

namespace {

using namespace exch;

constexpr uint32_t kIid = 3;
constexpr uint16_t kShard = 3;
// 1970-01-01 was a Thursday: day index 6 == Wednesday noon UTC — inside
// the 24/5 FX session so the market-hours gate stays open.
constexpr uint64_t kWedNoon =
    (6ull * 86400ull + 12ull * 3600ull) * 1'000'000'000ull;

// 1e8 tick scale: 100'000'000 ticks == price 1.00000.
constexpr int64_t kP100 = 100'000'000;
constexpr int64_t kP101 = 101'000'000;
constexpr int64_t kP99  =  99'000'000;

Order* mk(MemoryPool<Order>& pool, uint64_t id, Side side, OrderType type,
          int64_t price, int64_t qty, uint64_t acct = 7,
          TimeInForce tif = TimeInForce::GTC, uint8_t flags = 0) {
    Order* o = pool.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = side;
    o->type = type;
    o->tif = tif;
    o->stp_mode = StpMode::CANCEL_NEWEST;
    o->flags = flags;
    o->price_ticks = price;
    o->qty_units = qty;
    return o;
}

Instrument inst() {
    Instrument i{};
    i.instrument_id = kIid;
    std::strncpy(i.symbol, "EUR/USD", sizeof(i.symbol) - 1);
    i.status = InstrumentStatus::ACTIVE;
    i.pip_factor = 10;              // 1 pip = 10'000 ticks (0.0001)
    i.pip_size_ticks = 10'000;
    i.tick_size_ticks = 1'000;
    i.lot_size_units = 1;
    return i;
}

InstrumentFeed::Snapshot snap(
    InstrumentStatus st = InstrumentStatus::ACTIVE,
    bool armed = false, int64_t deadline = 0) {
    InstrumentFeed::Snapshot s;
    s.verifiable = true;
    s.status = st;
    s.market_known = true;
    s.auction_armed = armed;
    s.auction_deadline_ns = deadline;
    return s;
}

struct Fx {
    Instrument ins = inst();  // bound by pointer — must outlive the book
    MemoryPool<Order> pool{1024};
    OrderBook book{pool};
    MatchingEngine engine{kShard, book, pool, nullptr, nullptr};
    InstrumentFeed feed;
    uint64_t next_id = 100;
    Fx() {
        book.set_instrument(ins);
        engine.bind_instrument_feed(&feed);
        feed.apply(snap());
        engine.on_time_tick(kWedNoon);  // open session on the logical clock
    }
    Order* submit(Side side, OrderType type, int64_t price, int64_t qty,
                  const OrderAux& aux, uint64_t acct = 7,
                  TimeInForce tif = TimeInForce::GTC, uint8_t flags = 0) {
        Order* o = mk(pool, next_id++, side, type, price, qty, acct, tif,
                      flags);
        engine.on_order_received(o, aux);
        return o;
    }
    // Print a last-price trade at `px`: a resting bid `qty + rest` deep is
    // swept by a same-size taker sell (different accounts dodge the
    // same-account STP gate). `rest` bids remain for triggered sweeps.
    void print(int64_t px, int64_t qty, int64_t rest = 0) {
        submit(Side::BUY, OrderType::LIMIT, px, qty + rest, OrderAux{},
               /*acct=*/11);
        submit(Side::SELL, OrderType::LIMIT, px, qty, OrderAux{},
               /*acct=*/12);
    }
};

int64_t level_qty(const OrderBook& b, Side s, int64_t price) {
    for (std::size_t d = 0; ; ++d) {
        const PriceLevel* l = b.level(s, d);
        if (l == nullptr) return 0;
        if (l->price_ticks == price) return l->total_qty_units;
    }
}

const StopOrderTrigger::Pending* pending(const Fx& f, uint64_t id) {
    return const_cast<StopOrderTrigger&>(f.engine.stops()).find(id);
}

std::filesystem::path tmp_dir(const char* name) {
    const auto d = std::filesystem::temp_directory_path() /
                   ("exch_phase16_" + std::to_string(::getpid()) + "_" +
                    name);
    std::filesystem::remove_all(d);
    std::filesystem::create_directories(d);
    return d;
}

}  // namespace

// --- trailing stops (Tasks 16.3.3/16.3.15) -----------------------------------

TEST(Phase16Trailing, AbsoluteDistanceArmsAndDerivesStop) {
    Fx f;
    f.print(kP100, 10, /*rest=*/0);              // last = 1.00000
    OrderAux aux{};
    aux.trail_unit = kTrailUnitAbsolute;
    aux.trail_distance = 20'000;                 // 0.00020 raw ticks
    Order* o = f.submit(Side::SELL, OrderType::TRAILING_STOP, 0, 10, aux);
    ASSERT_NE(o, nullptr);
    const auto* p = pending(f, o->id);
    ASSERT_NE(p, nullptr);
    EXPECT_EQ(p->armed, 1u);
    EXPECT_EQ(p->anchor_ticks, kP100);
    EXPECT_EQ(p->stop_price_ticks, kP100 - 20'000);
    EXPECT_EQ(f.engine.stops().trail_live(), 1u);
}

TEST(Phase16Trailing, PipsDistanceConvertsThroughLattice) {
    Fx f;
    f.print(kP100, 10);
    OrderAux aux{};
    aux.trail_unit = kTrailUnitPips;
    aux.trail_distance = 2;                      // 2 pips = 20'000 ticks
    Order* o = f.submit(Side::SELL, OrderType::TRAILING_STOP, 0, 10, aux);
    const auto* p = pending(f, o->id);
    ASSERT_NE(p, nullptr);
    EXPECT_EQ(p->stop_price_ticks, kP100 - 2 * 10'000);
}

TEST(Phase16Trailing, PercentageDistance) {
    Fx f;
    f.print(kP100, 10);
    OrderAux aux{};
    aux.trail_unit = kTrailUnitPercentage;
    aux.trail_distance = 25;                     // 0.25% = 25/10'000
    Order* o = f.submit(Side::SELL, OrderType::TRAILING_STOP, 0, 10, aux);
    const auto* p = pending(f, o->id);
    ASSERT_NE(p, nullptr);
    // 1.00000 * 25/10000 = 0.00025 -> 250'000 ticks
    EXPECT_EQ(p->stop_price_ticks, kP100 - 250'000);
}

TEST(Phase16Trailing, AnchorRatchetsFavorableOnly) {
    Fx f;
    f.print(kP100, 10);
    OrderAux aux{};
    aux.trail_unit = kTrailUnitAbsolute;
    aux.trail_distance = 20'000;
    const uint64_t tid0 = f.next_id;
    f.submit(Side::SELL, OrderType::TRAILING_STOP, 0, 10, aux);
    const auto* p = pending(f, tid0);
    ASSERT_NE(p, nullptr);
    // Rally: SELL anchor ratchets UP, stop follows.
    f.print(kP100 + 10'000, 10);
    EXPECT_EQ(p->anchor_ticks, kP100 + 10'000);
    EXPECT_EQ(p->stop_price_ticks, kP100 - 10'000);
    // Adverse move back down: the armed stop stands — no downward re-anchor.
    f.print(kP100 + 5'000, 10);
    EXPECT_EQ(p->anchor_ticks, kP100 + 10'000);
    EXPECT_EQ(p->stop_price_ticks, kP100 - 10'000);
    // Liquidity below the stop for the triggered market sweep.
    f.submit(Side::BUY, OrderType::LIMIT, kP100 - 50'000, 100, OrderAux{},
             13);
    // Print AT the armed stop: the sell taker consumes its own-price bid,
    // last falls to the stop -> triggers -> sweep fills at the deeper bid.
    const uint64_t trades0 = f.engine.trades_emitted();
    f.print(kP100 - 10'000, 10);
    EXPECT_EQ(pending(f, tid0), nullptr);
    EXPECT_GT(f.engine.trades_emitted(), trades0 + 1);
    EXPECT_EQ(f.engine.last_price_ticks(), kP100 - 50'000);
}

TEST(Phase16Trailing, ActivationGateHoldsDormant) {
    Fx f;
    // MARK-sourced so the reference is driven directly (no prints needed).
    f.engine.set_oracle_snapshot(kP100, kWedNoon, 0, 0, true);
    OrderAux aux{};
    aux.trigger_source = kTriggerSourceMark;
    aux.trail_unit = kTrailUnitAbsolute;
    aux.trail_distance = 20'000;
    aux.activation_price_ticks = kP100 + 50'000;  // SELL arms on a rally
    Order* o = f.submit(Side::SELL, OrderType::TRAILING_STOP, 0, 10, aux);
    const auto* p = pending(f, o->id);
    ASSERT_NE(p, nullptr);
    EXPECT_EQ(p->armed, 0u);
    // An adverse mark move must not wake or trigger a dormant trail.
    f.engine.set_oracle_snapshot(kP99, kWedNoon + 1, 0, 0, true);
    f.engine.on_time_tick(kWedNoon + 1);
    ASSERT_NE(pending(f, o->id), nullptr);
    EXPECT_EQ(pending(f, o->id)->armed, 0u);
    // Rally to the activation gate: arms, anchors at the gate reference.
    f.engine.set_oracle_snapshot(kP100 + 50'000, kWedNoon + 2, 0, 0, true);
    f.engine.on_time_tick(kWedNoon + 2);
    p = pending(f, o->id);
    ASSERT_NE(p, nullptr);
    EXPECT_EQ(p->armed, 1u);
    EXPECT_EQ(p->anchor_ticks, kP100 + 50'000);
    EXPECT_EQ(p->stop_price_ticks, kP100 + 30'000);
}

// --- conditional trigger sources (Task 16.3.17) ------------------------------

TEST(Phase16Triggers, MarkSourceFiresWithoutLastPrint) {
    Fx f;
    // Liquidity for the triggered market sell.
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 1'000, OrderAux{}, 11);
    f.engine.set_oracle_snapshot(kP100, kWedNoon, 0, 0, true);
    OrderAux aux{};
    aux.trigger_source = kTriggerSourceMark;
    aux.stop_price_ticks = kP99 + 50'000;
    Order* o = f.submit(Side::SELL, OrderType::STOP, 0, 10, aux);
    ASSERT_TRUE(f.engine.stops().pending(o->id));
    EXPECT_EQ(f.engine.last_price_ticks(), 0u);   // no prints at all
    // Mark slides to the stop: fires on the oracle alone.
    f.engine.set_oracle_snapshot(kP99 + 50'000, kWedNoon + 1, 0, 0, true);
    f.engine.on_time_tick(kWedNoon + 1);
    EXPECT_FALSE(f.engine.stops().pending(o->id));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.last_price_ticks(), kP99);  // swept the bid
}

TEST(Phase16Triggers, IndexSourceFiresOnIndex) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 1'000, OrderAux{}, 11);
    f.engine.set_oracle_snapshot(0, 0, kP100, kWedNoon, true);
    OrderAux aux{};
    aux.trigger_source = kTriggerSourceIndex;
    aux.stop_price_ticks = kP99 + 50'000;   // index sits above -> not due
    Order* o = f.submit(Side::SELL, OrderType::STOP, 0, 10, aux);
    const uint64_t oid = o->id;
    ASSERT_TRUE(f.engine.stops().pending(oid));
    // Index drops to the sell stop -> due.
    f.engine.set_oracle_snapshot(0, 0, kP99, kWedNoon + 1, true);
    f.engine.on_time_tick(kWedNoon + 1);
    EXPECT_FALSE(f.engine.stops().pending(oid));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
}

TEST(Phase16Triggers, StaleMarkFreezesSourceNotQueue) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99 - 500'000, 2'000,
             OrderAux{}, 11);
    // Stale mark (5s+ old vs kWedNoon): frozen fail-closed.
    f.engine.set_oracle_snapshot(kP100, kWedNoon - 6'000'000'000ll,
                                 0, 0, true);
    // MARK-sourced SELL stop at the HIGHER stop price -> sorts ahead of the
    // LAST-sourced entry in the descending sell chain (frozen head must not
    // shadow a due tail — spec §6.2a).
    OrderAux maux{};
    maux.trigger_source = kTriggerSourceMark;
    maux.stop_price_ticks = kP99 + 900'000;
    Order* mo = f.submit(Side::SELL, OrderType::STOP, 0, 10, maux);
    OrderAux laux{};
    laux.trigger_source = kTriggerSourceLast;
    laux.stop_price_ticks = kP99 + 700'000;
    Order* lo = f.submit(Side::SELL, OrderType::STOP, 0, 10, laux);
    ASSERT_TRUE(f.engine.stops().pending(mo->id));
    ASSERT_TRUE(f.engine.stops().pending(lo->id));
    EXPECT_EQ(f.engine.stops().source_live(kTriggerSourceMark), 1u);
    EXPECT_EQ(f.engine.stops().source_live(kTriggerSourceLast), 1u);
    // Print below BOTH stops: LAST fires, stale-MARK stays parked.
    f.print(kP99 + 600'000, 10);
    EXPECT_FALSE(f.engine.stops().pending(lo->id));
    EXPECT_TRUE(f.engine.stops().pending(mo->id));
}

// --- pegged orders (Tasks 16.3.11/16.3.22) ------------------------------------

TEST(Phase16Peg, MidPegRestsAtVisibleMidpoint) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 11);
    f.submit(Side::SELL, OrderType::LIMIT, kP101, 10, OrderAux{}, 12);
    OrderAux aux{};
    aux.peg_mode = kPegMid;
    // PEG admission frees the scratch node — capture the id up front.
    const uint64_t pid = f.next_id;
    f.submit(Side::BUY, OrderType::PEG, 0, 10, aux);
    const Order* b = f.book.find_order(pid);
    ASSERT_NE(b, nullptr);
    EXPECT_EQ(b->price_ticks, kP100);           // mid(0.99, 1.01) = 1.00
    EXPECT_FALSE(l2_visible(*b));               // pegs are L2-hidden
    EXPECT_EQ(f.engine.last_reject(), nullptr);
}

TEST(Phase16Peg, PrimaryPegOffsetsSameSide) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 11);
    f.submit(Side::SELL, OrderType::LIMIT, kP101, 10, OrderAux{}, 12);
    OrderAux aux{};
    aux.peg_mode = kPegPrimary;
    aux.peg_offset_ticks = 1'000;               // one tick above best bid
    const uint64_t pid = f.next_id;
    f.submit(Side::BUY, OrderType::PEG, 0, 10, aux);
    const Order* b = f.book.find_order(pid);
    ASSERT_NE(b, nullptr);
    EXPECT_EQ(b->price_ticks, kP99 + 1'000);
}

TEST(Phase16Peg, MarketPegOffsetsOppositeSide) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 11);
    f.submit(Side::SELL, OrderType::LIMIT, kP101, 10, OrderAux{}, 12);
    OrderAux aux{};
    aux.peg_mode = kPegMarket;
    aux.peg_offset_ticks = -2'000;              // two ticks under best ask
    const uint64_t pid = f.next_id;
    f.submit(Side::BUY, OrderType::PEG, 0, 10, aux);
    const Order* b = f.book.find_order(pid);
    ASSERT_NE(b, nullptr);
    EXPECT_EQ(b->price_ticks, kP101 - 2'000);
}

TEST(Phase16Peg, RepriceFollowsVisibleBBO) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 11);
    f.submit(Side::SELL, OrderType::LIMIT, kP101, 10, OrderAux{}, 12);
    OrderAux aux{};
    aux.peg_mode = kPegMid;
    const uint64_t pid = f.next_id;
    f.submit(Side::BUY, OrderType::PEG, 0, 10, aux);
    const Order* b = f.book.find_order(pid);
    ASSERT_NE(b, nullptr);
    EXPECT_EQ(b->price_ticks, kP100);
    // Lift the visible bid: mid -> (99.5 + 101)/2 = 100.25 -> reprice.
    f.submit(Side::BUY, OrderType::LIMIT, kP99 + 50'000, 10, OrderAux{}, 13);
    EXPECT_EQ(f.engine.peg_repriced_total(), 1u);
    b = f.book.find_order(pid);
    ASSERT_NE(b, nullptr);
    EXPECT_EQ(b->price_ticks, kP100 + 25'000);
}

TEST(Phase16Peg, CollarClampsReprice) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 11);
    f.submit(Side::SELL, OrderType::LIMIT, kP101, 10, OrderAux{}, 12);
    OrderAux aux{};
    aux.peg_mode = kPegMid;
    aux.peg_limit_ticks = kP100 + 10'000;       // buy collar
    const uint64_t pid = f.next_id;
    f.submit(Side::BUY, OrderType::PEG, 0, 10, aux);
    const Order* b = f.book.find_order(pid);
    ASSERT_NE(b, nullptr);
    EXPECT_EQ(b->price_ticks, kP100);           // mid under the collar
    // Bid lift pushes the mid past the collar -> clamp at the limit.
    f.submit(Side::BUY, OrderType::LIMIT, kP99 + 90'000, 10, OrderAux{}, 13);
    b = f.book.find_order(pid);
    ASSERT_NE(b, nullptr);
    EXPECT_EQ(b->price_ticks, kP100 + 10'000);
}

TEST(Phase16Peg, NoReferenceRejectsUnavailable) {
    Fx f;
    OrderAux aux{};
    aux.peg_mode = kPegMid;
    const uint64_t pid = f.next_id;
    f.submit(Side::BUY, OrderType::PEG, 0, 10, aux);
    EXPECT_EQ(f.book.find_order(pid), nullptr);
    ASSERT_NE(f.engine.last_reject(), nullptr);
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectPeggedPricingUnavailable);
}

TEST(Phase16Peg, NoReferenceWithCollarRestsAtLimit) {
    Fx f;
    OrderAux aux{};
    aux.peg_mode = kPegMid;
    aux.peg_limit_ticks = kP100 + 50'000;
    const uint64_t pid = f.next_id;
    f.submit(Side::BUY, OrderType::PEG, 0, 10, aux);
    const Order* b = f.book.find_order(pid);
    ASSERT_NE(b, nullptr);
    EXPECT_EQ(b->price_ticks, kP100 + 50'000);  // collar fallback
}

TEST(Phase16Peg, L2SnapshotExcludesPeggedLevel) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 11);
    f.submit(Side::SELL, OrderType::LIMIT, kP101, 10, OrderAux{}, 12);
    OrderAux aux{};
    aux.peg_mode = kPegMid;
    f.submit(Side::BUY, OrderType::PEG, 0, 10, aux);   // rests at 1.00
    ASSERT_EQ(f.book.bid_count(), 2u);                 // 0.99 + peg 1.00
    std::vector<uint8_t> buf(4096);
    const std::size_t n =
        serialize_l2_snapshot(f.book, kIid, f.engine.now_ns(), buf.data(),
                              buf.size(), kL2FullDepth);
    ASSERT_GE(n, sizeof(L2SnapshotHeader));
    L2SnapshotHeader hdr{};
    std::memcpy(&hdr, buf.data(), sizeof(hdr));
    EXPECT_EQ(hdr.bid_levels, 1u);   // pegged level contributes nothing
    EXPECT_EQ(hdr.ask_levels, 1u);
    L2LevelRecord rec{};
    std::memcpy(&rec, buf.data() + sizeof(hdr), sizeof(rec));
    EXPECT_EQ(rec.price_ticks, kP99);
    EXPECT_EQ(rec.total_qty_units, 10);
}

// --- hidden / dark orders (Task 16.3.13) --------------------------------------

TEST(Phase16Hidden, L2OmissionButRests) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 11);
    f.submit(Side::SELL, OrderType::LIMIT, kP100 + 50'000, 10, OrderAux{},
             12, TimeInForce::GTC, kOrderFlagHidden);
    f.submit(Side::SELL, OrderType::LIMIT, kP101, 10, OrderAux{}, 12);
    // The hidden level exists inside the book (better ask)...
    ASSERT_GE(f.book.ask_count(), 2u);
    EXPECT_EQ(level_qty(f.book, Side::SELL, kP100 + 50'000), 10);
    // ...but the public feed sees only the visible level.
    std::vector<uint8_t> buf(4096);
    const std::size_t n =
        serialize_l2_snapshot(f.book, kIid, f.engine.now_ns(), buf.data(),
                              buf.size(), kL2FullDepth);
    ASSERT_GE(n, sizeof(L2SnapshotHeader));
    L2SnapshotHeader hdr{};
    std::memcpy(&hdr, buf.data(), sizeof(hdr));
    EXPECT_EQ(hdr.ask_levels, 1u);
    // ask record follows the single bid record.
    L2LevelRecord ask{};
    std::memcpy(&ask,
                buf.data() + sizeof(hdr) + sizeof(L2LevelRecord),
                sizeof(ask));
    EXPECT_EQ(ask.price_ticks, kP101);
    EXPECT_EQ(ask.total_qty_units, 10);
    EXPECT_EQ(ask.order_count, 1u);
}

TEST(Phase16Hidden, MidpointFill) {
    Fx f;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 11);
    f.submit(Side::SELL, OrderType::LIMIT, kP100 + 50'000, 10, OrderAux{},
             12, TimeInForce::GTC, kOrderFlagHidden);
    f.submit(Side::SELL, OrderType::LIMIT, kP101, 10, OrderAux{}, 13);
    // Visible BBO is 0.99 / 1.01 -> mid 1.00. A marketable buy at 1.01
    // sweeps the hidden 1.005 first — fill prints at the MID, not the
    // hidden level price (price improvement, spec §6.2a).
    const uint64_t tid = f.next_id;
    f.submit(Side::BUY, OrderType::LIMIT, kP101, 10, OrderAux{}, 14);
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.last_price_ticks(), kP100);
    // Visible ask untouched; hidden maker filled out; taker fully filled.
    EXPECT_EQ(level_qty(f.book, Side::SELL, kP101), 10);
    EXPECT_EQ(level_qty(f.book, Side::SELL, kP100 + 50'000), 0);
    EXPECT_EQ(f.book.find_order(tid), nullptr);
}

TEST(Phase16Hidden, NoVisibleMidFailsClosed) {
    Fx f;
    // Hidden ask is the ONLY ask-side liquidity -> no visible BBO -> no
    // midpoint -> the taker cannot fill (fail-closed hold, never a fill
    // at the hidden limit itself).
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 11);
    f.submit(Side::SELL, OrderType::LIMIT, kP100 + 50'000, 10, OrderAux{},
             12, TimeInForce::GTC, kOrderFlagHidden);
    Order* t = f.submit(Side::BUY, OrderType::LIMIT, kP101, 10, OrderAux{},
                        14, TimeInForce::IOC);
    EXPECT_NE(t, nullptr);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(f.engine.last_price_ticks(), 0u);
    // IOC remainder cancelled; hidden maker still rests.
    EXPECT_EQ(level_qty(f.book, Side::SELL, kP100 + 50'000), 10);
}

// --- GSLO (Task 16.3.16) -------------------------------------------------------

TEST(Phase16Gslo, ExposureReservedThenReleasedOnCancel) {
    Fx f;
    OrderAux aux{};
    aux.stop_price_ticks = kP100;
    Order* o = f.submit(Side::SELL, OrderType::STOP, 0, 100, aux, 7,
                        TimeInForce::GTC, kOrderFlagGslo);
    ASSERT_TRUE(f.engine.stops().pending(o->id));
    // notional = 100 * 1.00000 = 100 units
    EXPECT_EQ(f.engine.gslo_live_count(), 1u);
    EXPECT_EQ(f.engine.gslo_notional_units(), 100);
    f.engine.on_cancel_received(o->id, 7);
    EXPECT_EQ(f.engine.gslo_notional_units(), 0);
    EXPECT_EQ(f.engine.gslo_live_count(), 0u);
}

TEST(Phase16Gslo, ExactStopFillAtVenueId) {
    Fx f;
    // Liquidity exists far below the stop — the GSLO ignores it entirely
    // and fills the whole quantity at the ARMED stop vs the synthetic
    // venue counterparty.
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 1'000, OrderAux{}, 11);
    OrderAux aux{};
    aux.stop_price_ticks = kP100;
    Order* o = f.submit(Side::SELL, OrderType::STOP, 0, 100, aux, 7,
                        TimeInForce::GTC, kOrderFlagGslo);
    ASSERT_TRUE(f.engine.stops().pending(o->id));
    EXPECT_EQ(f.engine.gslo_notional_units(), 100);
    // Print at 0.99 -> last <= armed stop -> trigger -> guaranteed fill
    // AT the stop (1.00000), never at market.
    f.print(kP99, 10);
    EXPECT_FALSE(f.engine.stops().pending(o->id));
    EXPECT_EQ(f.engine.gslo_fills(), 1u);
    EXPECT_EQ(f.engine.gslo_notional_units(), 0);
    EXPECT_EQ(f.engine.gslo_live_count(), 0u);
    // The GSLO print supersedes the market print on the tape.
    EXPECT_EQ(f.engine.last_price_ticks(), kP100);
    // The resting bid never participated.
    EXPECT_EQ(level_qty(f.book, Side::BUY, kP99), 1'000);
}

TEST(Phase16Gslo, ExposureCapRejectsAdmission) {
    Fx f;
    f.engine.set_gslo_max_exposure_units(50);
    OrderAux aux{};
    aux.stop_price_ticks = kP100;
    Order* o = f.submit(Side::SELL, OrderType::STOP, 0, 100, aux, 7,
                        TimeInForce::GTC, kOrderFlagGslo);
    EXPECT_NE(o, nullptr);
    EXPECT_FALSE(f.engine.stops().pending(o->id));
    ASSERT_NE(f.engine.last_reject(), nullptr);
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectGsloExposureLimit);
    EXPECT_EQ(f.engine.gslo_notional_units(), 0);
    EXPECT_EQ(f.engine.gslo_live_count(), 0u);
}

TEST(Phase16Gslo, FlagOnNonConditionalRejects) {
    Fx f;
    const uint64_t oid = f.next_id;
    f.submit(Side::BUY, OrderType::LIMIT, kP99, 10, OrderAux{}, 7,
             TimeInForce::GTC, kOrderFlagGslo);
    EXPECT_EQ(f.book.find_order(oid), nullptr);
    ASSERT_NE(f.engine.last_reject(), nullptr);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectOrderInvalid);
}

// --- MOO / MOC (Task 16.3.25, spec §6.2b) --------------------------------------

TEST(Phase16Moo, ParksDuringCallThenUncrosses) {
    Fx f;
    const int64_t dl = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl));
    f.engine.on_time_tick(kWedNoon + 1);
    ASSERT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    // Accumulated limit liquidity for the uncross.
    f.submit(Side::SELL, OrderType::LIMIT, kP100, 10, OrderAux{}, 12);
    // MOO parks — never rests, never matches continuously.
    Order* moo = f.submit(Side::BUY, OrderType::MOO, 0, 10, OrderAux{}, 13);
    EXPECT_EQ(f.engine.auction_parked_count(), 1u);
    EXPECT_EQ(f.book.find_order(moo->id), nullptr);
    // Deadline strike -> single-price uncross fills the MOO.
    f.engine.on_time_tick(kWedNoon + 61'000'000'000ull);
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseNone);
    EXPECT_EQ(f.engine.auction_parked_count(), 0u);
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.last_price_ticks(), kP100);
}

TEST(Phase16Moo, FreezeWindowRejectsCancel) {
    Fx f;
    const int64_t dl = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl));
    f.engine.on_time_tick(kWedNoon + 1);
    ASSERT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    Order* moo = f.submit(Side::BUY, OrderType::MOO, 0, 10, OrderAux{}, 13);
    ASSERT_EQ(f.engine.auction_parked_count(), 1u);
    // Outside the T-30s freeze a user cancel still works.
    f.engine.on_cancel_received(moo->id, 13);
    EXPECT_EQ(f.engine.auction_parked_count(), 0u);
    // Re-park and step inside the freeze window (T-10s).
    moo = f.submit(Side::SELL, OrderType::MOC, 0, 10, OrderAux{}, 13);
    ASSERT_EQ(f.engine.auction_parked_count(), 1u);
    f.engine.on_time_tick(kWedNoon + 50'000'000'000ull);
    f.engine.on_cancel_received(moo->id, 13);
    ASSERT_NE(f.engine.last_reject(), nullptr);
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectAmendInAuction);
    EXPECT_EQ(f.engine.auction_parked_count(), 1u);  // committed to uncross
    // Book liquidity accumulated during the CALL gives the strike a
    // clearing candidate; the MOC fills 5 and the remainder drains
    // AUCTION_CANCELLED.
    f.submit(Side::BUY, OrderType::LIMIT, kP100, 5, OrderAux{}, 14);
    f.engine.on_time_tick(kWedNoon + 61'000'000'000ull);
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseNone);
    EXPECT_EQ(f.engine.auction_parked_count(), 0u);
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.last_price_ticks(), kP100);
}

TEST(Phase16Moo, OutsideCallRejectsOrderInvalid) {
    Fx f;  // continuous session — no armed CALL
    Order* o = f.submit(Side::BUY, OrderType::MOO, 0, 10, OrderAux{}, 13);
    EXPECT_NE(o, nullptr);
    EXPECT_EQ(f.engine.auction_parked_count(), 0u);
    ASSERT_NE(f.engine.last_reject(), nullptr);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectOrderInvalid);
}

// --- WAL: ORDER_NEW_EX / ORDER_TRIGGERED / PEG_REPRICE -------------------------

TEST(Phase16Wal, OrderNewExAndAdvancedRowsRoundTrip) {
    const auto dir = tmp_dir("wal_adv");
    const auto wpath = dir / "0.wal";
    Instrument ins1 = inst();
    Instrument ins2 = inst();
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    book.set_instrument(ins1);
    InstrumentFeed feed;
    feed.apply(snap());
    {
        Wal w(wpath.string(), kShard);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        WalWriter ww(&w);
        MatchingEngine e(kShard, book, pool, &ww, nullptr);
        e.bind_instrument_feed(&feed);
        e.on_time_tick(kWedNoon);
        // EnginePump stamping convention (test_recovery.cpp): ingress
        // orders carry (now_ns, wal tail_seq) so the replayed clamp to the
        // WAL envelope reproduces the live priority stamps bit-for-bit.
        const auto sub = [&](uint64_t id, Side side, OrderType type,
                           int64_t px, int64_t qty, uint64_t acct,
                           const OrderAux& aux) {
            Order* o = mk(pool, id, side, type, px, qty, acct);
            o->timestamp_ns = e.now_ns();
            o->ingress_seq = w.tail_seq();
            e.on_order_received(o, aux);
        };
        // Plain LIMIT -> legacy ORDER_NEW.
        sub(1, Side::BUY, OrderType::LIMIT, kP99, 10, 11, OrderAux{});
        sub(2, Side::SELL, OrderType::LIMIT, kP101, 10, 12, OrderAux{});
        // PEG -> ORDER_NEW_EX + PEG_REPRICE rows as the BBO moves.
        OrderAux paux{};
        paux.peg_mode = kPegMid;
        sub(3, Side::BUY, OrderType::PEG, 0, 10, 13, paux);
        sub(4, Side::BUY, OrderType::LIMIT, kP99 + 50'000, 10, 14,
            OrderAux{});
        // MARK-sourced armed trailing stop -> ORDER_NEW_EX; the oracle move
        // fires it -> ORDER_TRIGGERED + TRADE rows.
        e.set_oracle_snapshot(kP100, kWedNoon, 0, 0, true);
        OrderAux taux{};
        taux.trigger_source = kTriggerSourceMark;
        taux.trail_unit = kTrailUnitAbsolute;
        taux.trail_distance = 20'000;
        sub(5, Side::SELL, OrderType::TRAILING_STOP, 0, 10, 15, taux);
        e.set_oracle_snapshot(kP99 + 90'000, kWedNoon + 1, 0, 0, true);
        e.on_time_tick(kWedNoon + 1);   // mark crossed the armed stop
        (void)w.flush();
        w.close();
    }
    // WAL shape: the peg + trail admissions journaled ORDER_NEW_EX, the
    // reprices journaled PEG_REPRICE, the trigger journaled ORDER_TRIGGERED.
    {
        WalReader r(wpath.string());
        ASSERT_TRUE(r.is_open());
        bool saw_ex = false, saw_legacy = false, saw_trigger = false,
             saw_reprice = false;
        WalEntryView v;
        for (;;) {
            const WalScanStep s = r.next(v);
            if (s != WalScanStep::Entry) break;
            if (v.type == WalEventType::ORDER_NEW_EX) {
                EXPECT_EQ(v.payload_len, sizeof(WalOrderNewExPayload));
                saw_ex = true;
            } else if (v.type == WalEventType::ORDER_NEW) {
                EXPECT_EQ(v.payload_len, sizeof(WalOrderNewPayload));
                saw_legacy = true;
            } else if (v.type == WalEventType::ORDER_TRIGGERED) {
                EXPECT_EQ(v.payload_len, sizeof(WalOrderTriggeredPayload));
                saw_trigger = true;
            } else if (v.type == WalEventType::PEG_REPRICE) {
                EXPECT_EQ(v.payload_len, sizeof(WalPegRepricePayload));
                saw_reprice = true;
            }
        }
        EXPECT_TRUE(saw_ex);
        EXPECT_TRUE(saw_legacy);
        EXPECT_TRUE(saw_trigger);
        EXPECT_TRUE(saw_reprice);
    }
    // Feedless replay converges: the peg reprices identically, the MARK
    // trail fires off its authoritative ORDER_TRIGGERED row. The strongest
    // check is whole-book fingerprint parity — same levels, same orders,
    // same fills.
    MemoryPool<Order> pool2{512};
    OrderBook book2{pool2};
    book2.set_instrument(ins2);
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(),
                                          {{kIid, &book2, &pool2}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    std::vector<uint8_t> fp1, fp2;
    WalBookSnapshotHeader h1{}, h2{};
    ASSERT_TRUE(SnapshotStore::serialize_book(book, kIid, /*wal_seq*/ 0,
                                              fp1, h1));
    ASSERT_TRUE(SnapshotStore::serialize_book(book2, kIid, /*wal_seq*/ 0,
                                              fp2, h2));
    EXPECT_EQ(fp1, fp2);
    EXPECT_GT(res.trades_derived + res.trades_applied, 0u);
    EXPECT_GT(res.mutations_applied, 0u);
}

TEST(Phase16Wal, ArmedMarkTrailRejectsLiveButReplays) {
    // Live gate: armed MARK trail with no oracle -> CONDITIONAL_TRIGGER_
    // ORACLE_STALE (the ORDER_NEW_EX row is still committed upstream, so a
    // real WAL captures the admission). Journal-free replay keeps the
    // pending dormant instead — covered above via the triggered path.
    const auto dir = tmp_dir("wal_stale");
    const auto wpath = dir / "0.wal";
    Instrument ins = inst();
    MemoryPool<Order> pool{64};
    OrderBook book{pool};
    book.set_instrument(ins);
    InstrumentFeed feed;
    feed.apply(snap());
    Wal w(wpath.string(), kShard);
    ASSERT_EQ(w.open(), WalStatus::Ok);
    WalWriter ww(&w);
    MatchingEngine e(kShard, book, pool, &ww, nullptr);
    e.bind_instrument_feed(&feed);
    e.on_time_tick(kWedNoon);
    OrderAux aux{};
    aux.trigger_source = kTriggerSourceMark;    // no oracle bound/loaded
    aux.trail_unit = kTrailUnitAbsolute;
    aux.trail_distance = 20'000;
    Order* o = mk(pool, 1, Side::SELL, OrderType::TRAILING_STOP, 0, 10, 15);
    e.on_order_received(o, aux);
    ASSERT_NE(e.last_reject(), nullptr);
    EXPECT_STREQ(e.last_reject(),
                 MatchingEngine::kRejectConditionalOracleStale);
    EXPECT_FALSE(e.stops().pending(1));
    w.close();
}

// --- EnginePump wire decode (Tasks 16.3.3/11/13/16 ingress) -------------------

namespace {

class FakeChannel final : public IpcChannel {
   public:
    bool open() noexcept override { open_ = true; return true; }
    void close() noexcept override { open_ = false; }
    [[nodiscard]] bool is_open() const noexcept override { return open_; }
    bool send(const void* data, uint32_t len) noexcept override {
        sent.emplace_back(static_cast<const uint8_t*>(data),
                          static_cast<const uint8_t*>(data) + len);
        return true;
    }
    int32_t poll(void* buf, uint32_t buf_cap) noexcept override {
        if (inq.empty()) return 0;
        const std::vector<uint8_t>& m = inq.front();
        if (m.size() > buf_cap) return -1;
        std::memcpy(buf, m.data(), m.size());
        const int32_t n = static_cast<int32_t>(m.size());
        inq.pop_front();
        return n;
    }
    bool open_ = false;
    std::deque<std::vector<uint8_t>> inq;
    std::vector<std::vector<uint8_t>> sent;
};

class FakeIngress final : public IEngineIngress {
   public:
    void on_order_received(Order* o) noexcept override {
        orders.push_back(*o);
    }
    void on_order_received_ex(Order* o, const OrderAux& aux) noexcept
        override {
        orders.push_back(*o);
        auxs.push_back(aux);
    }
    void on_cancel_received(uint64_t order_id, uint64_t account_id) noexcept
        override {
        cancels.emplace_back(order_id, account_id);
    }
    void on_time_tick(uint64_t now_ns) noexcept override {
        ticks.push_back(now_ns);
    }
    std::vector<Order> orders;
    std::vector<OrderAux> auxs;
    std::vector<std::pair<uint64_t, uint64_t>> cancels;
    std::vector<uint64_t> ticks;
};

struct ReportLog {
    std::vector<std::pair<std::string, std::string>> entries;
    static void sink(void* ctx, const char* code, const char* detail) {
        auto* self = static_cast<ReportLog*>(ctx);
        self->entries.emplace_back(code != nullptr ? code : "",
                                   detail != nullptr ? detail : "");
    }
    [[nodiscard]] bool has(const char* code) const {
        for (const auto& e : entries)
            if (e.first == code) return true;
        return false;
    }
};

std::vector<uint8_t> wire_order_new(
    uint64_t seq, uint64_t order_id, uint64_t account_id,
    exc::wire::OrderType type, int64_t qty, int64_t price, uint64_t ts,
    uint8_t flags = 0, uint8_t peg_mode = 0, int64_t peg_offset = 0,
    int64_t peg_limit = 0, uint8_t trigger_source = 0,
    int64_t trailing_offset = 0, uint8_t trailing_offset_unit = 0,
    int64_t activation_price = 0, int64_t stop_price = 0) {
    flatbuffers::FlatBufferBuilder b(256);
    const auto coid = b.CreateString("c-" + std::to_string(seq));
    exc::wire::OrderNewBuilder ob(b);
    ob.add_order_id(order_id);
    ob.add_account_id(account_id);
    ob.add_instrument_id(kIid);
    ob.add_side(exc::wire::Side_Sell);
    ob.add_type(type);
    ob.add_qty(qty);
    ob.add_price(price);
    ob.add_tif(exc::wire::TimeInForce_GTC);
    ob.add_client_order_id(coid);
    ob.add_flags(flags);
    ob.add_peg_mode(peg_mode);
    ob.add_peg_offset(peg_offset);
    ob.add_peg_limit(peg_limit);
    ob.add_trigger_source(trigger_source);
    ob.add_trailing_offset(trailing_offset);
    ob.add_trailing_offset_unit(trailing_offset_unit);
    ob.add_activation_price(activation_price);
    ob.add_stop_price(stop_price);
    const auto on = ob.Finish();
    exc::wire::EventBuilder eb(b);
    eb.add_seq(seq);
    eb.add_ts(ts);
    eb.add_type_type(exc::wire::EventType_OrderNew);
    eb.add_type(on.Union());
    b.Finish(eb.Finish());
    return {b.GetBufferPointer(), b.GetBufferPointer() + b.GetSize()};
}

}  // namespace

TEST(Phase16Pump, PegFieldsDecodeToAux) {
    FakeChannel in, out;
    FakeIngress eng;
    MemoryPool<Order> pool(8);
    (void)in.open();
    in.inq.push_back(wire_order_new(
        /*seq=*/1, /*order_id=*/9001, /*account_id=*/77,
        exc::wire::OrderType_Peg, /*qty=*/150'000'000, /*price=*/0,
        /*ts=*/7'777'000, /*flags=*/0, /*peg_mode=*/1,
        /*peg_offset=*/-5'000, /*peg_limit=*/105'000'000,
        /*trigger_source=*/0));
    EnginePump pump(&in, &out, &eng, &pool);
    ASSERT_EQ(pump.run_once(4), 1u);
    ASSERT_EQ(eng.orders.size(), 1u);
    ASSERT_EQ(eng.auxs.size(), 1u);
    EXPECT_EQ(eng.orders[0].type, OrderType::PEG);
    EXPECT_EQ(eng.auxs[0].peg_mode, kPegMid);
    EXPECT_EQ(eng.auxs[0].peg_offset_ticks, -5'000);
    EXPECT_EQ(eng.auxs[0].peg_limit_ticks, 105'000'000);
}

TEST(Phase16Pump, StopMarketPlusTrailUnitDecodesTrailing) {
    FakeChannel in, out;
    FakeIngress eng;
    MemoryPool<Order> pool(8);
    (void)in.open();
    in.inq.push_back(wire_order_new(
        /*seq=*/1, /*order_id=*/9002, /*account_id=*/77,
        exc::wire::OrderType_StopMarket, /*qty=*/10, /*price=*/0,
        /*ts=*/8'000, /*flags=*/0, /*peg_mode=*/0, /*peg_offset=*/0,
        /*peg_limit=*/0, /*trigger_source=*/1,
        /*trailing_offset=*/20'000,
        /*trailing_offset_unit=*/3,  // ABSOLUTE
        /*activation_price=*/99'000'000));
    EnginePump pump(&in, &out, &eng, &pool);
    ASSERT_EQ(pump.run_once(4), 1u);
    ASSERT_EQ(eng.orders.size(), 1u);
    ASSERT_EQ(eng.auxs.size(), 1u);
    EXPECT_EQ(eng.orders[0].type, OrderType::TRAILING_STOP);
    EXPECT_EQ(eng.auxs[0].trigger_source, kTriggerSourceMark);
    EXPECT_EQ(eng.auxs[0].trail_unit, kTrailUnitAbsolute);
    EXPECT_EQ(eng.auxs[0].trail_distance, 20'000);
    EXPECT_EQ(eng.auxs[0].activation_price_ticks, 99'000'000);
}

TEST(Phase16Pump, HiddenAndGsloWireFlagsTranslate) {
    FakeChannel in, out;
    FakeIngress eng;
    MemoryPool<Order> pool(8);
    (void)in.open();
    // wire bit2=hidden, bit3=gslo (bit0 post_only passes through).
    in.inq.push_back(wire_order_new(
        1, 9003, 77, exc::wire::OrderType_Limit, 10, kP100, 9'000,
        /*flags=*/0x0Du));  // post_only | hidden | gslo
    EnginePump pump(&in, &out, &eng, &pool);
    ASSERT_EQ(pump.run_once(4), 1u);
    ASSERT_EQ(eng.orders.size(), 1u);
    const uint8_t f = eng.orders[0].flags;
    EXPECT_EQ(f & kOrderFlagPostOnly, kOrderFlagPostOnly);
    EXPECT_EQ(f & kOrderFlagHidden, kOrderFlagHidden);
    EXPECT_EQ(f & kOrderFlagGslo, kOrderFlagGslo);
    // Wire bits 2/3 must NOT land on the engine's internal markers.
    EXPECT_EQ(f & 0x0Cu, 0u);
}

TEST(Phase16Pump, TrailUnitOnNonStopIsDecodeError) {
    FakeChannel in, out;
    FakeIngress eng;
    MemoryPool<Order> pool(8);
    (void)in.open();
    in.inq.push_back(wire_order_new(
        1, 9004, 77, exc::wire::OrderType_Limit, 10, kP100, 9'500,
        /*flags=*/0, /*peg_mode=*/0, /*peg_offset=*/0, /*peg_limit=*/0,
        /*trigger_source=*/0, /*trailing_offset=*/20'000,
        /*trailing_offset_unit=*/3));
    EnginePump pump(&in, &out, &eng, &pool);
    ReportLog rlog;
    pump.set_report_sink(&ReportLog::sink, &rlog);
    EXPECT_EQ(pump.run_once(4), 1u);
    EXPECT_EQ(pump.decode_errors(), 1u);
    EXPECT_TRUE(eng.orders.empty());
    EXPECT_TRUE(rlog.has("DECODE_ERROR"));
}

TEST(Phase16Pump, OutOfRangeAdvancedEnumsAreDecodeErrors) {
    FakeChannel in, out;
    FakeIngress eng;
    MemoryPool<Order> pool(8);
    (void)in.open();
    in.inq.push_back(wire_order_new(
        1, 9005, 77, exc::wire::OrderType_Limit, 10, kP100, 9'500,
        /*flags=*/0, /*peg_mode=*/4));   // > kPegMarket
    in.inq.push_back(wire_order_new(
        2, 9006, 77, exc::wire::OrderType_Limit, 10, kP100, 9'500,
        /*flags=*/0, /*peg_mode=*/0, /*peg_offset=*/0, /*peg_limit=*/0,
        /*trigger_source=*/3));          // > kTriggerSourceIndex
    in.inq.push_back(wire_order_new(
        3, 9007, 77, exc::wire::OrderType_Limit, 10, kP100, 9'500,
        /*flags=*/0, /*peg_mode=*/0, /*peg_offset=*/0, /*peg_limit=*/0,
        /*trigger_source=*/0, /*trailing_offset=*/0,
        /*trailing_offset_unit=*/4));    // > kTrailUnitAbsolute
    EnginePump pump(&in, &out, &eng, &pool);
    EXPECT_EQ(pump.run_once(8), 3u);
    EXPECT_EQ(pump.decode_errors(), 3u);
    EXPECT_TRUE(eng.orders.empty());
}
