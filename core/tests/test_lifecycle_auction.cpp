// Phase-15 coverage — Tasks 15.3.3/15.3.4/15.3.6/15.3.10 (spec §7.1/§7.3):
// lifecycle status matrix, 24/5 market-hours gate, reopening CALL auction
// (accumulate → indicative → uncross → EXTEND → withdraw → quarantine) and
// WAL-journaled recovery convergence for the auction path.

#include <gtest/gtest.h>

#include <cstdio>
#include <cstring>
#include <filesystem>
#include <string>
#include <unistd.h>
#include <vector>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "matching/MatchingEngine.hpp"
#include "matching/WalWriter.hpp"
#include "recovery/RecoveryManager.hpp"
#include "recovery/SnapshotStore.hpp"
#include "risk/InstrumentFeed.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"

using namespace exch;

namespace {

constexpr uint32_t kIid = 7;
constexpr uint16_t kShard = 3;
constexpr uint64_t kDayNs = 86'400ull * 1'000'000'000ull;
// 1970-01-01 was a Thursday: day index 6 == Wednesday, 2 == Saturday,
// 3 == Sunday, 1 == Friday.
constexpr uint64_t kWedNoon = (6ull * 86400ull + 12ull * 3600ull) *
                              1'000'000'000ull;
// Saturday must come AFTER the fixture's Wednesday tick on the monotone
// logical clock — use 1970-01-10 (day index 9 -> dow (9+4)%7 == 6).
constexpr uint64_t kSatNoon = (9ull * 86400ull + 12ull * 3600ull) *
                              1'000'000'000ull;
// Wednesday AFTER the Saturday tick — 1970-01-14 (day index 13 -> dow 3).
constexpr uint64_t kWedNoon2 = (13ull * 86400ull + 12ull * 3600ull) *
                               1'000'000'000ull;
constexpr uint64_t kSun2044 = (3ull * 86400ull + 20ull * 3600ull +
                               44ull * 60ull) * 1'000'000'000ull;
constexpr uint64_t kSun2045 = (3ull * 86400ull + 20ull * 3600ull +
                               45ull * 60ull) * 1'000'000'000ull;
constexpr uint64_t kFri2159 = (1ull * 86400ull + 21ull * 3600ull +
                               59ull * 60ull) * 1'000'000'000ull;
constexpr uint64_t kFri2200 = (1ull * 86400ull + 22ull * 3600ull) *
                              1'000'000'000ull;

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
    i.tick_size_ticks = 1;
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
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    MatchingEngine engine{3, book, pool, nullptr, nullptr};
    InstrumentFeed feed;
    Fx() {
        book.set_instrument(ins);
        engine.bind_instrument_feed(&feed);
        feed.apply(snap());
        engine.on_time_tick(kWedNoon);  // open session on the logical clock
    }
};

int64_t level_qty(const OrderBook& b, Side s, int64_t price) {
    for (std::size_t d = 0; ; ++d) {
        const PriceLevel* l = b.level(s, d);
        if (l == nullptr) return 0;
        if (l->price_ticks == price) return l->total_qty_units;
    }
}

std::vector<uint8_t> fingerprint(const OrderBook& book) {
    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    EXPECT_TRUE(SnapshotStore::serialize_book(book, kIid, /*wal_seq*/ 0,
                                              blob, hdr));
    return blob;
}

std::filesystem::path tmp_dir(const char* name) {
    const auto d = std::filesystem::temp_directory_path() /
                   ("exch_lifecycle_" + std::to_string(::getpid()) + "_" +
                    name);
    std::filesystem::remove_all(d);
    std::filesystem::create_directories(d);
    return d;
}

}  // namespace

// --- control-plane parsers (pure) ---------------------------------------------

TEST(Lifecycle, ParseInstrumentStatus) {
    InstrumentStatus st{};
    EXPECT_TRUE(parse_instrument_status("ACTIVE", &st));
    EXPECT_EQ(st, InstrumentStatus::ACTIVE);
    EXPECT_TRUE(parse_instrument_status("CANCEL_ONLY", &st));
    EXPECT_EQ(st, InstrumentStatus::CANCEL_ONLY);
    EXPECT_TRUE(parse_instrument_status("SUSPENDED", &st));
    EXPECT_TRUE(parse_instrument_status("HALTED", &st));
    EXPECT_TRUE(parse_instrument_status("RESTRICTED", &st));
    EXPECT_TRUE(parse_instrument_status("DELISTED", &st));
    EXPECT_TRUE(parse_instrument_status("DRAFT", &st));
    EXPECT_FALSE(parse_instrument_status("active", &st));
    EXPECT_FALSE(parse_instrument_status("", &st));
    EXPECT_FALSE(parse_instrument_status("TRADING", &st));
}

TEST(Lifecycle, ParseAuctionCall) {
    int64_t dl = 0;
    EXPECT_TRUE(parse_auction_call("CALL:12345", &dl));
    EXPECT_EQ(dl, 12345);
    EXPECT_TRUE(parse_auction_call("EXTEND:777", &dl));
    EXPECT_EQ(dl, 777);
    EXPECT_FALSE(parse_auction_call("CALL:", &dl));
    EXPECT_FALSE(parse_auction_call("CALL:abc", &dl));
    EXPECT_FALSE(parse_auction_call("OPEN:5", &dl));
    EXPECT_FALSE(parse_auction_call("", &dl));
}

TEST(Lifecycle, ParseMarketHours) {
    MarketHours h{};
    // Empty object keeps canonical 24/5 defaults.
    EXPECT_TRUE(parse_market_hours_json("{}", &h));
    EXPECT_EQ(h.pre_open_sow, 20 * 3600 + 45 * 60);
    EXPECT_EQ(h.close_sow, 5 * 86400 + 22 * 3600);
    // Custom fields + a full-day-closed override.
    const std::string j =
        R"({"open_utc":"SUN 21:00","close_utc":"FRI 21:30",)"
        R"("pre_open_utc":"SUN 20:30",)"
        R"("overrides":[{"date":"1970-01-03","closed":true}]})";
    EXPECT_TRUE(parse_market_hours_json(j, &h));
    EXPECT_EQ(h.pre_open_sow, 20 * 3600 + 30 * 60);
    EXPECT_EQ(h.close_sow, 5 * 86400 + 21 * 3600 + 30 * 60);
    ASSERT_EQ(h.override_count, 1u);
    EXPECT_TRUE(h.overrides[0].closed);
    // A present-but-unparseable field value fails closed (an absent field
    // legitimately keeps the default — the malformed JSON must carry a
    // parseable string with an invalid SOW value to exercise the gate).
    EXPECT_FALSE(parse_market_hours_json(
        R"({"open_utc":"XYZ 99:99"})", &h));
    EXPECT_FALSE(parse_market_hours_json(
        R"({"overrides":[{"date":"1970-01-03"}]})", &h));  // neither
                                                          // closed nor
                                                          // open/close
}

TEST(Lifecycle, MarketEntryWeek) {
    const MarketHours h{};
    // Sunday pre-open boundary: 20:44:59 closed, 20:45 open.
    EXPECT_FALSE(market_entry_allowed(h, kSun2044));
    EXPECT_TRUE(market_entry_allowed(h, kSun2045));
    // Mid-week open.
    EXPECT_TRUE(market_entry_allowed(h, kWedNoon));
    // Friday close boundary: 21:59 open, exactly 22:00 closed.
    EXPECT_TRUE(market_entry_allowed(h, kFri2159));
    EXPECT_FALSE(market_entry_allowed(h, kFri2200));
    // Weekend closed.
    EXPECT_FALSE(market_entry_allowed(h, kSatNoon));
}

// --- lifecycle gates (engine, feed-bound) -------------------------------------

TEST(Lifecycle, UnverifiableFeedFailsClosed) {
    Fx f;
    InstrumentFeed::Snapshot s;  // default: verifiable=false
    f.feed.apply(s);
    Order* o = mk(f.pool, 1, Side::BUY, OrderType::LIMIT, 100, 10);
    f.engine.on_order_received(o, OrderAux{});
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectInstrumentSuspended);
    EXPECT_EQ(f.book.live_orders(), 0u);
}

TEST(Lifecycle, StatusMatrixNewOrders) {
    struct Row {
        InstrumentStatus st;
        const char* code;   // nullptr == admitted
    };
    const Row rows[] = {
        {InstrumentStatus::ACTIVE, nullptr},
        {InstrumentStatus::CANCEL_ONLY,
         MatchingEngine::kRejectInstrumentCancelOnly},
        {InstrumentStatus::SUSPENDED,
         MatchingEngine::kRejectInstrumentSuspended},
        {InstrumentStatus::HALTED,
         MatchingEngine::kRejectInstrumentHalted},
        {InstrumentStatus::DELISTED,
         MatchingEngine::kRejectInstrumentDelisted},
        {InstrumentStatus::DRAFT,
         MatchingEngine::kRejectInstrumentSuspended},
    };
    uint64_t id = 1;
    for (const Row& r : rows) {
        Fx f;
        f.feed.apply(snap(r.st));
        Order* o = mk(f.pool, id++, Side::BUY, OrderType::LIMIT, 100, 10);
        f.engine.on_order_received(o, OrderAux{});
        if (r.code == nullptr) {
            EXPECT_EQ(f.book.live_orders(), 1u)
                << static_cast<int>(r.st);
        } else {
            EXPECT_STREQ(f.engine.last_reject(), r.code);
            EXPECT_EQ(f.book.live_orders(), 0u);
        }
    }
}

TEST(Lifecycle, RestrictedIsLimitOnly) {
    Fx f;
    f.feed.apply(snap(InstrumentStatus::RESTRICTED));
    Order* lim = mk(f.pool, 1, Side::BUY, OrderType::LIMIT, 100, 10);
    f.engine.on_order_received(lim, OrderAux{});
    EXPECT_EQ(f.book.live_orders(), 1u);
    Order* mkt = mk(f.pool, 2, Side::SELL, OrderType::MARKET, 0, 5);
    f.engine.on_order_received(mkt, OrderAux{});
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectInstrumentRestricted);
    EXPECT_EQ(f.book.live_orders(), 1u);
}

TEST(Lifecycle, DelistedReduceOnlyWindow) {
    Fx f;
    f.feed.apply(snap(InstrumentStatus::DELISTED));
    Order* plain = mk(f.pool, 1, Side::SELL, OrderType::LIMIT, 100, 10);
    f.engine.on_order_received(plain, OrderAux{});
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectInstrumentDelisted);
    Order* ro = mk(f.pool, 2, Side::SELL, OrderType::LIMIT, 100, 10, 1,
                   TimeInForce::GTC, kOrderFlagReduceOnly);
    f.engine.on_order_received(ro, OrderAux{});
    EXPECT_EQ(f.book.live_orders(), 1u);
}

TEST(Lifecycle, CancelsStayOpenAmendsGated) {
    // Rest an order while ACTIVE, then suspend the instrument.
    Fx f;
    Order* o = mk(f.pool, 1, Side::BUY, OrderType::LIMIT, 100, 10);
    f.engine.on_order_received(o, OrderAux{});
    ASSERT_EQ(f.book.live_orders(), 1u);
    f.feed.apply(snap(InstrumentStatus::SUSPENDED));
    // Amend is gated by the status matrix.
    f.engine.on_amend_received(1, 105, 0, 0, 99);
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectInstrumentSuspended);
    // Cancel still lands.
    f.engine.on_cancel_received(1, 1);
    EXPECT_EQ(f.book.live_orders(), 0u);
}

TEST(Lifecycle, MarketHoursGate) {
    Fx f;
    // Rest an order while the session is open (fixture is at kWedNoon).
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    ASSERT_EQ(f.book.live_orders(), 1u);
    // Weekend on the (monotone) logical clock — new entry rejects,
    // cancels stay session-independent.
    f.engine.on_time_tick(kSatNoon);
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectMarketClosed);
    EXPECT_EQ(f.book.live_orders(), 1u);
    f.engine.on_cancel_received(2, 1);
    EXPECT_EQ(f.book.live_orders(), 0u);
    // Missing market:hours fails closed even when the feed verified —
    // the NEXT Wednesday keeps the clock monotone.
    InstrumentFeed::Snapshot s = snap();
    s.market_known = false;
    f.feed.apply(s);
    f.engine.on_time_tick(kWedNoon2);
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectMarketClosed);
}

// --- reopening CALL auction ----------------------------------------------------

TEST(Auction, CallAccumulatesCrossedBook) {
    Fx f;
    const int64_t dl = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, /*armed=*/true, dl));
    // Next event observes the armed key and enters CALL.
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::LIMIT,
                                  105, 10), OrderAux{});
    ASSERT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    EXPECT_EQ(f.engine.auction_id(), dl);
    // Crossing sell rests instead of matching — the book is allowed to
    // accumulate crossed interest during CALL.
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_TRUE(f.book.crossed());
    EXPECT_EQ(f.book.live_orders(), 2u);
}

TEST(Auction, MarketAndIocFokAreParked) {
    Fx f;
    const int64_t dl = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl));
    f.engine.on_time_tick(kWedNoon + 1);  // enter CALL
    ASSERT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::MARKET,
                                  0, 5), OrderAux{});
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  100, 5, 1, TimeInForce::IOC), OrderAux{});
    f.engine.on_order_received(mk(f.pool, 3, Side::SELL, OrderType::LIMIT,
                                  100, 5, 1, TimeInForce::FOK), OrderAux{});
    EXPECT_EQ(f.engine.auction_parked_count(), 3u);
    EXPECT_EQ(f.book.live_orders(), 0u);  // parked orders are off-book
    // Stop orders accumulate untriggered (not parked, not booked).
    OrderAux sa{};
    sa.stop_price_ticks = 110;
    f.engine.on_order_received(mk(f.pool, 4, Side::BUY, OrderType::STOP,
                                  0, 5), sa);
    EXPECT_EQ(f.engine.auction_parked_count(), 3u);
    EXPECT_EQ(f.book.live_orders(), 0u);
}

TEST(Auction, AmendRejectedCancelAllowed) {
    Fx f;
    const int64_t dl = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl));
    f.engine.on_time_tick(kWedNoon + 1);
    ASSERT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::LIMIT,
                                  105, 10), OrderAux{});
    f.engine.on_amend_received(1, 106, 0, 0, 1);
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectAmendInAuction);
    f.engine.on_cancel_received(1, 1);
    EXPECT_EQ(f.book.live_orders(), 0u);
    // Parked orders cancel too.
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::MARKET,
                                  0, 5), OrderAux{});
    ASSERT_EQ(f.engine.auction_parked_count(), 1u);
    f.engine.on_cancel_received(2, 1);
    EXPECT_EQ(f.engine.auction_parked_count(), 0u);
}

TEST(Auction, UncrossSinglePriceAndResume) {
    Fx f;
    const int64_t dl = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl));
    f.engine.on_time_tick(kWedNoon + 1);
    // Bid 10@105, ask 10@100 — clearing price 100 (lowest max-volume
    // candidate), volume 10.
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::LIMIT,
                                  105, 10), OrderAux{});
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::MARKET,
                                  0, 5), OrderAux{});
    ASSERT_EQ(f.engine.auction_parked_count(), 1u);
    // Strike the deadline on the logical clock.
    f.engine.on_time_tick(static_cast<uint64_t>(dl));
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseNone);
    EXPECT_EQ(f.engine.last_completed_auction_id(), dl);
    EXPECT_EQ(f.engine.auction_parked_count(), 0u);
    // Trades at the single clearing price 100: the parked market buy
    // (5) takes priority, then the limit buy clears the remaining 5.
    EXPECT_EQ(f.engine.trades_emitted(), 2u);
    EXPECT_EQ(level_qty(f.book, Side::BUY, 105), 5);  // residual 5@105
    EXPECT_EQ(f.engine.last_price_ticks(), 100);
    EXPECT_FALSE(f.book.crossed());
    // CLEARED result request surfaced to the control thread.
    EXPECT_EQ(f.feed.auction_result_pending(), dl);
    EXPECT_TRUE(f.feed.auction_result_cleared());
    // Continuous trading resumes: a fresh limit executes against the
    // residual buy. Priced at 101 — inside the 2% last-price band (the
    // uncross set last price = 100) and still crossing the resting 105
    // bid; account 2 avoids the same-account STP gate.
    f.engine.on_order_received(mk(f.pool, 4, Side::SELL, OrderType::LIMIT,
                                  101, 3, /*acct=*/2), OrderAux{});
    EXPECT_EQ(f.engine.trades_emitted(), 3u);  // 3@105 vs residual buy
}

TEST(Auction, ExtendMovesDeadline) {
    Fx f;
    const int64_t dl1 = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    const int64_t dl2 = dl1 + 30'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl1));
    f.engine.on_time_tick(kWedNoon + 1);
    ASSERT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::LIMIT,
                                  105, 10), OrderAux{});
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    // Go ladder pushes EXTEND:{dl2}; the next event observes it.
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl2));
    f.engine.on_time_tick(static_cast<uint64_t>(dl1));
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    EXPECT_EQ(f.engine.auction_extensions(), 1);
    EXPECT_EQ(f.engine.auction_deadline_ns(), dl2);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);  // old deadline did not fire
    f.engine.on_time_tick(static_cast<uint64_t>(dl2));
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseNone);
    EXPECT_EQ(f.engine.last_completed_auction_id(), dl2);
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
}

TEST(Auction, WithdrawnKeyQuarantinesCrossedBook) {
    Fx f;
    const int64_t dl = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl));
    f.engine.on_time_tick(kWedNoon + 1);
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::LIMIT,
                                  105, 10), OrderAux{});
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    ASSERT_TRUE(f.book.crossed());
    // Control plane deletes the key (skip_auction resume) while the book
    // is crossed — the CALL cannot complete; the engine quarantines the
    // forensic state instead of leaving an unexplained crossed book.
    f.feed.apply(snap(InstrumentStatus::ACTIVE, false, 0));
    f.engine.on_time_tick(kWedNoon + 2);
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseNone);
    ASSERT_TRUE(f.engine.quarantined());
    EXPECT_STREQ(f.engine.quarantine_code(),
                 MatchingEngine::kRejectInstrumentHalted);
    EXPECT_TRUE(f.book.crossed());        // forensic residue preserved
    EXPECT_TRUE(f.book.allow_crossed());  // legitimated for the audit
    // New orders + amends reject; cancels still land.
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectInstrumentHalted);
    f.engine.on_amend_received(1, 106, 0, 0, 2);
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectInstrumentHalted);
    f.engine.on_cancel_received(1, 1);
    EXPECT_TRUE(f.book.find_order(1) == nullptr);
}

TEST(Auction, StrikeFailAwaitsExtensionThenClears) {
    Fx f;
    const int64_t dl1 = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    const int64_t dl2 = dl1 + 30'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl1));
    f.engine.on_time_tick(kWedNoon + 1);
    // A lone parked market buy has no clearing-price anchor.
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::MARKET,
                                  0, 5), OrderAux{});
    f.engine.on_time_tick(static_cast<uint64_t>(dl1));
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    EXPECT_TRUE(f.engine.auction_awaiting());
    // FAILED surfaced to the control thread for this strike.
    EXPECT_EQ(f.feed.auction_result_pending(), dl1);
    EXPECT_FALSE(f.feed.auction_result_cleared());
    f.feed.clear_auction_result_request();
    // The Go ladder extends; interest arrives; the next strike clears.
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl2));
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  100, 5), OrderAux{});
    EXPECT_EQ(f.engine.auction_extensions(), 1);
    f.engine.on_time_tick(static_cast<uint64_t>(dl2));
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseNone);
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(f.feed.auction_result_pending(), dl2);
    EXPECT_TRUE(f.feed.auction_result_cleared());
}

TEST(Auction, CompletedDeadlineDoesNotReenter) {
    Fx f;
    const int64_t dl = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl));
    f.engine.on_time_tick(kWedNoon + 1);
    // A completed CALL needs a clean uncross — empty-book strikes only
    // arm the awaiting-extension state (Go ladder owns extend/suspend).
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::LIMIT,
                                  105, 10), OrderAux{});
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    f.engine.on_time_tick(static_cast<uint64_t>(dl));
    ASSERT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseNone);
    // A stale snapshot republishing the consumed deadline must not
    // re-enter CALL (same auction_id == last_completed).
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl));
    f.engine.on_time_tick(static_cast<uint64_t>(dl) + 1);
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseNone);
    // A genuinely new arm (different deadline) re-enters.
    const int64_t dl2 = dl + 90'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl2));
    f.engine.on_time_tick(static_cast<uint64_t>(dl) + 2);
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    EXPECT_EQ(f.engine.auction_id(), dl2);
}

TEST(Auction, ReArmClearsQuarantineByUncrossing) {
    Fx f;
    const int64_t dl = static_cast<int64_t>(kWedNoon) + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl));
    f.engine.on_time_tick(kWedNoon + 1);
    f.engine.on_order_received(mk(f.pool, 1, Side::BUY, OrderType::LIMIT,
                                  105, 10), OrderAux{});
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  100, 10), OrderAux{});
    f.feed.apply(snap(InstrumentStatus::ACTIVE, false, 0));
    f.engine.on_time_tick(kWedNoon + 2);
    ASSERT_TRUE(f.engine.quarantined());
    // Re-arming a fresh CALL resolves the crossed residue by uncross.
    const int64_t dl2 = dl + 60'000'000'000ll;
    f.feed.apply(snap(InstrumentStatus::ACTIVE, true, dl2));
    f.engine.on_time_tick(kWedNoon + 3);
    ASSERT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseCall);
    f.engine.on_time_tick(static_cast<uint64_t>(dl2));
    EXPECT_EQ(f.engine.auction_phase(), MatchingEngine::kAuctionPhaseNone);
    EXPECT_FALSE(f.engine.quarantined());
    EXPECT_FALSE(f.book.crossed());
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
}

// --- WAL + recovery -------------------------------------------------------------

TEST(Auction, WalReplayConverges) {
    const auto dir = tmp_dir("auction_wal");
    const auto wpath = dir / "0.wal";
    Instrument ins1 = inst();   // bound by pointer — outlives the book
    Instrument ins2 = inst();
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    book.set_instrument(ins1);
    InstrumentFeed feed;
    feed.apply(snap(InstrumentStatus::ACTIVE, true,
                    static_cast<int64_t>(kWedNoon) + 60'000'000'000ll));
    {
        Wal w(wpath.string(), kShard);
        ASSERT_EQ(w.open(), WalStatus::Ok);
        WalWriter ww(&w);
        MatchingEngine e(kShard, book, pool, &ww, nullptr);
        e.bind_instrument_feed(&feed);
        e.on_time_tick(kWedNoon);          // journaled TIME_TICK
        e.on_time_tick(kWedNoon + 1);      // armed-key observed -> CALL
        e.on_order_received(mk(pool, 1, Side::BUY, OrderType::LIMIT,
                               105, 10), OrderAux{});
        e.on_order_received(mk(pool, 2, Side::SELL, OrderType::LIMIT,
                               100, 10), OrderAux{});
        e.on_order_received(mk(pool, 3, Side::BUY, OrderType::MARKET,
                               0, 5), OrderAux{});
        // Strike — UNCROSS row + TRADE rows + parked drain cancels.
        e.on_time_tick(kWedNoon + 61'000'000'000ull);
        ASSERT_EQ(e.auction_phase(), MatchingEngine::kAuctionPhaseNone);
        EXPECT_EQ(e.trades_emitted(), 2u);
        // Post-auction continuous trade + cancel (101 stays inside the 2%
        // last-price band around the 100 clearing print; acct 2 avoids
        // the same-account STP gate).
        e.on_order_received(mk(pool, 4, Side::SELL, OrderType::LIMIT,
                               101, 3, /*acct=*/2), OrderAux{});
        e.on_cancel_received(1, 1);
        (void)w.flush();
        w.close();
    }
    const std::vector<uint8_t> live_fp = fingerprint(book);

    MemoryPool<Order> pool2{512};
    OrderBook book2{pool2};
    book2.set_instrument(ins2);
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(),
                                          {{kIid, &book2, &pool2}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(fingerprint(book2), live_fp);
    // Recovered auction state is surfaced for live-engine adoption.
    const RecoveryManager::RecoveredAuctionState* as =
        rm.recovered_auction_state(kIid);
    ASSERT_NE(as, nullptr);
    EXPECT_EQ(as->phase, MatchingEngine::kAuctionPhaseNone);
    EXPECT_EQ(as->last_completed,
              static_cast<int64_t>(kWedNoon) + 60'000'000'000ll);
    EXPECT_FALSE(as->quarantined);
}
