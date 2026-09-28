// Task 2.3.13 (spec §6.6) + Task 2.3.15 (spec §6.6a) — sparse-book
// protections and market-order slippage protection:
//   * side-aware empty-book rejection (ORDER_REJECTED_NO_LIQUIDITY)
//   * wide-spread market rejection (MARKET_ORDER_REJECTED_WIDE_SPREAD)
//   * safe L2 serialization — exact populated-level counts, no padding
//   * synthetic-limit conversion at best ± max_slippage_bps, remainder
//     rejected SLIPPAGE_EXCEEDED, MARKET_WITH_PROTECTION journaled via
//     Order::flags bit2, MarketOrderProtectionTriggered on the sink seam.

#include <gtest/gtest.h>

#include <cstdio>
#include <cstring>
#include <string>
#include <unistd.h>
#include <vector>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "marketdata/BookSerializer.hpp"
#include "matching/MatchingEngine.hpp"
#include "matching/WalWriter.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

using namespace exch;

namespace {

// 10^8-tick prices: 1.10000 == 110'000'000 ticks. pip_factor=10 -> a pip is
// 10'000 ticks, so P(pips) expresses prices in pip units around 1.10000.
constexpr int64_t kBase = 110'000'000;
constexpr int64_t pip(int64_t n) { return n * 10'000; }

uint64_t g_ts = 1'000'000'000;

Order* mk(MemoryPool<Order>& pool, uint64_t id, Side side, OrderType type,
          int64_t price, int64_t qty, uint64_t acct = 1,
          TimeInForce tif = TimeInForce::GTC) {
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
    o->timestamp_ns = ++g_ts;
    o->ingress_seq = g_ts;
    return o;
}

// Instrument bound to the book: 5-decimal major by default (pip_factor=10,
// settlement T+1). Tests mutate fields before constructing the book/engine
// or via set_instrument-safe early binding.
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
        instr.settlement_cycle = 1;  // T+1 major
        book.set_instrument(instr);
    }
};

int level_count(const OrderBook& b, Side s) {
    int n = 0;
    while (b.level(s, n) != nullptr) ++n;
    return n;
}

int64_t level_qty(const OrderBook& b, Side s, int64_t price) {
    for (std::size_t d = 0;; ++d) {
        const PriceLevel* l = b.level(s, d);
        if (l == nullptr) return 0;
        if (l->price_ticks == price) return l->total_qty_units;
    }
}

// --- protection-event capture -------------------------------------------------
struct ProtEvents {
    std::vector<MatchingEngine::MarketOrderProtectionEvent> evs;
    static void sink(void* ctx,
                     const MatchingEngine::MarketOrderProtectionEvent& e) noexcept {
        static_cast<ProtEvents*>(ctx)->evs.push_back(e);
    }
};

}  // namespace

// === Task 2.3.13 — side-aware empty-book protection ============================

TEST(BookProtection, MarketBuyRejectedWhenAskSideEmpty) {
    Fixture f;
    // Bids only — a BUY market has nothing to take.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, kBase, 10));
    ASSERT_EQ(level_count(f.book, Side::SELL), 0);

    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::MARKET, 0, 10, 2));
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectNoLiquidity);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);

    // Side-aware: a SELL market in the same book faces populated bids and
    // must trade normally (the empty ASK side is irrelevant to it).
    f.engine.on_order_received(
        mk(f.pool, 3, Side::SELL, OrderType::MARKET, 0, 5, 3));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(kBase));
}

TEST(BookProtection, MarketSellRejectedWhenBidSideEmpty) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kBase, 10));
    ASSERT_EQ(level_count(f.book, Side::BUY), 0);

    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::MARKET, 0, 10, 2));
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectNoLiquidity);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);

    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::MARKET, 0, 5, 3));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
}

TEST(BookProtection, IocAndFokRejectedOnEmptyOppositeSide) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, kBase, 10));

    // IOC buy with empty asks -> NO_LIQUIDITY (not a silent remainder drop).
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  kBase + pip(10), 10, 2, TimeInForce::IOC));
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectNoLiquidity);

    // FOK buy, same situation.
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::LIMIT,
                                  kBase + pip(10), 10, 2, TimeInForce::FOK));
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectNoLiquidity);

    // FOK sell with bids present is unaffected by the empty ask side and
    // proceeds to feasibility (which passes against the bid).
    f.engine.on_order_received(mk(f.pool, 4, Side::SELL, OrderType::LIMIT,
                                  kBase, 5, 3, TimeInForce::FOK));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
}

TEST(BookProtection, LimitOrdersUnaffectedByEmptySide) {
    Fixture f;
    // GTC limit into a completely empty book rests — never NO_LIQUIDITY.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, kBase, 10));
    EXPECT_EQ(f.engine.last_reject(), nullptr);
    ASSERT_NE(f.book.find_order(1), nullptr);

    // Pending stop is exempt too (it does not take liquidity now).
    OrderAux aux{};
    aux.stop_price_ticks = kBase - pip(5);
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::STOP, 0, 10, 2), aux);
    EXPECT_TRUE(f.engine.stops().pending(2));
}

// === Task 2.3.13 #1 — wide-spread market rejection ==============================

TEST(BookProtection, WideSpreadRejectsMarketOrders) {
    Fixture f;
    f.instr.max_spread_pips = 5;
    // 10-pip spread: bid 1.10000 / ask 1.10100.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, kBase, 10));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, kBase + pip(10), 10));

    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::MARKET, 0, 5, 2));
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectWideSpread);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(level_qty(f.book, Side::SELL, kBase + pip(10)), 10);

    f.engine.on_order_received(
        mk(f.pool, 4, Side::SELL, OrderType::MARKET, 0, 5, 3));
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectWideSpread);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
}

TEST(BookProtection, LimitsAcceptedInsideAndOutsideSpread) {
    Fixture f;
    f.instr.max_spread_pips = 5;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, kBase, 10));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, kBase + pip(10), 10));

    // LIMIT inside the spread rests mid-book.
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::LIMIT,
                                  kBase + pip(4), 5, 2));
    EXPECT_EQ(f.engine.last_reject(), nullptr);
    ASSERT_NE(f.book.find_order(3), nullptr);

    // LIMIT crossing the spread trades normally — protection never touches
    // priced orders.
    f.engine.on_order_received(mk(f.pool, 4, Side::BUY, OrderType::LIMIT,
                                  kBase + pip(10), 5, 3));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(kBase + pip(10)));
}

TEST(BookProtection, MarketFillsWhenSpreadWithinBand) {
    Fixture f;
    f.instr.max_spread_pips = 5;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, kBase, 10));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, kBase + pip(3), 10));
    // 3-pip spread <= 5 -> market order proceeds (subject to slippage band).
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::MARKET, 0, 5, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.engine.last_reject(), nullptr);
}

// === Task 2.3.13 #4 — safe L2 serialization =====================================

TEST(BookProtection, L2EmptyBookEmitsExactZeroCounts) {
    Fixture f;
    uint8_t buf[4096];
    const std::size_t n =
        serialize_l2_snapshot(f.book, 7, 12345, buf, sizeof(buf));
    ASSERT_EQ(n, sizeof(L2SnapshotHeader));
    L2SnapshotHeader hdr{};
    std::memcpy(&hdr, buf, sizeof(hdr));
    EXPECT_EQ(hdr.instrument_id, 7u);
    EXPECT_EQ(hdr.bid_levels, 0u);
    EXPECT_EQ(hdr.ask_levels, 0u);
    EXPECT_EQ(hdr.seq, f.book.book_seq());
    EXPECT_EQ(hdr.ts_ns, 12345u);
}

TEST(BookProtection, L2OneAndPartialDepthEmitExactCounts) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, kBase, 10));

    uint8_t buf[4096];
    std::size_t n = serialize_l2_snapshot(f.book, 7, 0, buf, sizeof(buf));
    ASSERT_EQ(n, l2_wire_size(1, 0));
    L2SnapshotHeader hdr{};
    std::memcpy(&hdr, buf, sizeof(hdr));
    EXPECT_EQ(hdr.bid_levels, 1u);
    EXPECT_EQ(hdr.ask_levels, 0u);
    L2LevelRecord rec{};
    std::memcpy(&rec, buf + sizeof(hdr), sizeof(rec));
    EXPECT_EQ(rec.price_ticks, kBase);
    EXPECT_EQ(rec.total_qty_units, 10);
    EXPECT_EQ(rec.order_count, 1u);

    // Partial depth: 3 bids + 2 asks — both under the 20-level feed depth.
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, kBase - pip(1), 20));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::LIMIT, kBase - pip(2), 30));
    f.engine.on_order_received(
        mk(f.pool, 4, Side::SELL, OrderType::LIMIT, kBase + pip(2), 40));
    f.engine.on_order_received(
        mk(f.pool, 5, Side::SELL, OrderType::LIMIT, kBase + pip(3), 50));

    n = serialize_l2_snapshot(f.book, 7, 0, buf, sizeof(buf));
    ASSERT_EQ(n, l2_wire_size(3, 2));
    std::memcpy(&hdr, buf, sizeof(hdr));
    EXPECT_EQ(hdr.bid_levels, 3u);
    EXPECT_EQ(hdr.ask_levels, 2u);
    // No synthesized padding: every emitted record carries a real price.
    const uint8_t* p = buf + sizeof(hdr);
    for (uint32_t i = 0; i < hdr.bid_levels + hdr.ask_levels; ++i) {
        std::memcpy(&rec, p, sizeof(rec));
        EXPECT_GT(rec.price_ticks, 0);
        EXPECT_GT(rec.total_qty_units, 0);
        EXPECT_GE(rec.order_count, 1u);
        p += sizeof(rec);
    }
    // Bids descending, asks ascending on the wire.
    std::memcpy(&rec, buf + sizeof(hdr), sizeof(rec));
    EXPECT_EQ(rec.price_ticks, kBase);          // best bid first
    std::memcpy(&rec, buf + sizeof(hdr) + 3 * sizeof(L2LevelRecord),
                sizeof(rec));
    EXPECT_EQ(rec.price_ticks, kBase + pip(2));  // best ask first
}

TEST(BookProtection, L2DeepBookCapsAtFeedDepth) {
    Fixture f;
    // 25 bid levels — deeper than the 20-level feed convention.
    for (uint64_t i = 0; i < 25; ++i) {
        f.engine.on_order_received(
            mk(f.pool, 100 + i, Side::BUY, OrderType::LIMIT,
               kBase - pip(static_cast<int64_t>(i + 1)), 10));
    }
    ASSERT_EQ(level_count(f.book, Side::BUY), 25);

    uint8_t buf[4096];
    const std::size_t n =
        serialize_l2_snapshot(f.book, 7, 0, buf, sizeof(buf));
    ASSERT_EQ(n, l2_wire_size(kL2FeedDepth, 0));
    L2SnapshotHeader hdr{};
    std::memcpy(&hdr, buf, sizeof(hdr));
    EXPECT_EQ(hdr.bid_levels, kL2FeedDepth);

    // Full-depth escape emits every populated level.
    const std::size_t full =
        serialize_l2_snapshot(f.book, 7, 0, buf, sizeof(buf), kL2FullDepth);
    EXPECT_EQ(full, l2_wire_size(25, 0));
}

TEST(BookProtection, L2FailsClosedOnShortBuffer) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, kBase, 10));
    uint8_t buf[64];
    EXPECT_EQ(serialize_l2_snapshot(f.book, 7, 0, buf, 10), 0u);
    EXPECT_EQ(serialize_l2_snapshot(f.book, 7, 0, nullptr, sizeof(buf)), 0u);
}

// === Task 2.3.15 — market-order slippage protection =============================

TEST(BookProtection, SlippageBuyCollarAndRemainderReject) {
    Fixture f;
    f.instr.max_slippage_bps = 100;  // 1.00% band -> collar 111'100'000
    // Asks at 1.10000, 1.10050 (inside), 1.11200 (beyond the collar).
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kBase, 10));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, kBase + pip(5), 10));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::SELL, OrderType::LIMIT, kBase + pip(120), 10));

    // Protection price = 110'000'000 + 110'000'000*100/10'000 = 111'100'000.
    f.engine.on_order_received(
        mk(f.pool, 4, Side::BUY, OrderType::MARKET, 0, 25, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 2u);  // 10@base + 10@base+5p
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectSlippageExceeded);
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(kBase + pip(5)));
    // The beyond-collar level is untouched; the market order never rests.
    EXPECT_EQ(level_qty(f.book, Side::SELL, kBase + pip(120)), 10);
    EXPECT_EQ(f.book.find_order(4), nullptr);
}

TEST(BookProtection, SlippageSellCollarSymmetric) {
    Fixture f;
    f.instr.max_slippage_bps = 100;  // collar = 108'900'000
    // Bids at 1.10000, 1.09950 (inside), 1.08800 (beyond the collar).
    f.engine.on_order_received(
        mk(f.pool, 1, Side::BUY, OrderType::LIMIT, kBase, 10));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, kBase - pip(5), 10));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::LIMIT, kBase - pip(120), 10));

    f.engine.on_order_received(
        mk(f.pool, 4, Side::SELL, OrderType::MARKET, 0, 25, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 2u);
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectSlippageExceeded);
    EXPECT_EQ(f.engine.last_price_ticks(), uint64_t(kBase - pip(5)));
    EXPECT_EQ(level_qty(f.book, Side::BUY, kBase - pip(120)), 10);
}

TEST(BookProtection, SlippageFullFillInsideCollarNoReject) {
    Fixture f;
    f.instr.max_slippage_bps = 100;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kBase, 10));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::SELL, OrderType::LIMIT, kBase + pip(5), 10));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::MARKET, 0, 20, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 2u);
    EXPECT_EQ(f.engine.last_reject(), nullptr);  // fully filled inside band
}

TEST(BookProtection, SlippageDefaultsBySettlementCycle) {
    // max_slippage_bps == 0 sentinel: T+1 major -> 100 bps collar
    // (111'100'000), T+2 exotic -> 200 bps (112'200'000). A level at +150
    // pips (~136 bps) splits the two defaults.
    {
        Fixture f;  // settlement_cycle = 1 -> major 100bps collar
        f.engine.on_order_received(
            mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kBase, 10));
        f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                      kBase + pip(150), 10));
        f.engine.on_order_received(
            mk(f.pool, 3, Side::BUY, OrderType::MARKET, 0, 20, 2));
        EXPECT_EQ(f.engine.trades_emitted(), 1u);
        EXPECT_STREQ(f.engine.last_reject(),
                     MatchingEngine::kRejectSlippageExceeded);
    }
    {
        Fixture f;
        f.instr.settlement_cycle = 2;  // -> exotic 200bps collar
        f.engine.on_order_received(
            mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kBase, 10));
        f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                      kBase + pip(150), 10));
        f.engine.on_order_received(
            mk(f.pool, 3, Side::BUY, OrderType::MARKET, 0, 20, 2));
        EXPECT_EQ(f.engine.trades_emitted(), 2u);
        EXPECT_EQ(f.engine.last_reject(), nullptr);
    }
}

TEST(BookProtection, SlippageUnboundedBandDisablesProtection) {
    Fixture f;
    f.instr.max_slippage_bps = 10'000;  // documented "protection off" escape
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kBase, 10));
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  kBase + pip(500), 10));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::MARKET, 0, 15, 2));
    EXPECT_EQ(f.engine.trades_emitted(), 2u);
    EXPECT_NE(f.engine.last_reject(),
              MatchingEngine::kRejectSlippageExceeded);
}

TEST(BookProtection, ProtectionEventEmittedOncePerConversion) {
    Fixture f;
    f.instr.max_slippage_bps = 100;
    ProtEvents sink;
    f.engine.set_protection_event_sink(&ProtEvents::sink, &sink);

    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, kBase, 10));
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  kBase + pip(120), 10));
    f.engine.on_order_received(
        mk(f.pool, 3, Side::BUY, OrderType::MARKET, 0, 15, 2));

    ASSERT_EQ(sink.evs.size(), 1u);
    const auto& e = sink.evs[0];
    EXPECT_EQ(e.order_id, 3u);
    EXPECT_EQ(e.account_id, 2u);
    EXPECT_EQ(e.instrument_id, 7u);
    EXPECT_EQ(e.side, Side::BUY);
    EXPECT_EQ(e.best_price_ticks, kBase);
    EXPECT_EQ(e.protection_price_ticks, kBase + 1'100'000);  // +100bps
    EXPECT_EQ(e.slippage_bps, 100);
    EXPECT_EQ(e.clipped_qty_units, 5);  // 15 - 10 filled

    // A plain LIMIT order never fires the event.
    f.engine.on_order_received(
        mk(f.pool, 4, Side::BUY, OrderType::LIMIT, kBase - pip(1), 5, 2));
    EXPECT_EQ(sink.evs.size(), 1u);
}

TEST(BookProtection, WalCarriesMarketWithProtectionFlag) {
    const std::string path =
        "/tmp/exc_bp_wal_" + std::to_string(::getpid()) + ".wal";
    std::remove(path.c_str());
    {
        MemoryPool<Order> pool{512};
        Instrument instr{};
        instr.instrument_id = 7;
        instr.pip_factor = 10;
        instr.pip_size_ticks = 10'000;
        instr.tick_size_ticks = 1'000;
        instr.lot_size_units = 1;
        instr.settlement_cycle = 1;
        instr.max_slippage_bps = 100;
        OrderBook book{pool};
        book.set_instrument(instr);
        Wal wal(path, /*shard=*/3);
        ASSERT_EQ(wal.open(), WalStatus::Ok);
        WalWriter ww(&wal);
        MatchingEngine e(3, book, pool, &ww, nullptr);

        e.on_order_received(
            mk(pool, 1, Side::SELL, OrderType::LIMIT, kBase, 10));
        e.on_order_received(
            mk(pool, 2, Side::BUY, OrderType::MARKET, 0, 5, 2));
        EXPECT_EQ(e.trades_emitted(), 1u);
        ASSERT_EQ(wal.flush(), WalStatus::Ok);
        wal.close();
    }

    // Scan the segment for order 2's ORDER_NEW and check flags bit2.
    WalReader reader;
    ASSERT_EQ(reader.open(path), WalStatus::Ok);
    bool seen_market = false;
    for (;;) {
        WalEntryView v{};
        const WalScanStep s = reader.next(v);
        if (s != WalScanStep::Entry) break;
        if (v.type != WalEventType::ORDER_NEW ||
            v.payload_len != sizeof(WalOrderNewPayload)) {
            continue;
        }
        WalOrderNewPayload p{};
        std::memcpy(&p, v.payload, sizeof(p));
        if (p.order_id == 2) {
            seen_market = true;
            EXPECT_EQ(p.type, 0u);  // wire::OrderType_Market
            EXPECT_TRUE(
                (p.flags & MatchingEngine::kOrderFlagMarketWithProtection) !=
                0);
        } else {
            EXPECT_FALSE(
                (p.flags & MatchingEngine::kOrderFlagMarketWithProtection) !=
                0);
        }
    }
    EXPECT_TRUE(seen_market);
    std::remove(path.c_str());
}

TEST(BookProtection, UnboundInstrumentRunsUnprotected) {
    // No instrument bound -> reference-data-free unit path still sweeps to
    // empty (pre-protection behavior).
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    MatchingEngine e(3, book, pool, nullptr, nullptr);
    e.on_order_received(
        mk(pool, 1, Side::SELL, OrderType::LIMIT, kBase, 10));
    e.on_order_received(
        mk(pool, 2, Side::SELL, OrderType::LIMIT, kBase + pip(50), 10));
    e.on_order_received(
        mk(pool, 3, Side::BUY, OrderType::MARKET, 0, 15, 2));
    EXPECT_EQ(e.trades_emitted(), 2u);
    EXPECT_EQ(e.last_reject(), nullptr);
}
