// Task 2.3.11 + 2.3.16 + 2.3.18 + 2.3.21 — STP suite (spec §6.5,
// §24 #274/#279-280/#368):
//   * all four suppressive modes + NONE gating (Professional/ECP only)
//   * trade-group cross-account prevention, mutual TRANSFER, PREVENTED_MATCH
//     WAL audit records, prevented-qty accounting
//   * account-default resolution order (order -> default -> CANCEL_NEWEST)
//     stamped via the bound PreTradeChecker risk hook
//   * WAL reason=STP on every suppressive outcome + deterministic replay
//   * DECREMENT partial resting + iceberg hidden-portion suppression

#include <gtest/gtest.h>

#include <array>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <memory>
#include <string>
#include <unistd.h>
#include <vector>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "matching/MatchingEngine.hpp"
#include "matching/SelfTradeGuard.hpp"
#include "matching/WalWriter.hpp"
#include "recovery/RecoveryManager.hpp"
#include "risk/EngineRiskAdapter.hpp"
#include "risk/PreTradeChecker.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

using namespace exch;

namespace {

constexpr int64_t P(int64_t v) { return v; }  // int64 10^8 ticks
constexpr uint32_t kIid = 7;
constexpr uint16_t kShard = 3;

uint64_t g_ts = 2'000'000'000;

Order* mk(MemoryPool<Order>& pool, uint64_t id, Side side, OrderType type,
          int64_t price, int64_t qty, uint64_t acct = 1,
          TimeInForce tif = TimeInForce::GTC,
          StpMode stp = StpMode::CANCEL_NEWEST, int64_t display = 0,
          uint8_t flags = 0) {
    Order* o = pool.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = side;
    o->type = type;
    o->tif = tif;
    o->stp_mode = stp;
    o->flags = flags;
    o->price_ticks = price;
    o->qty_units = qty;
    o->quantity = Decimal::from_mantissa(qty);
    o->display_qty_units = display;
    o->timestamp_ns = ++g_ts;
    o->ingress_seq = g_ts;
    return o;
}

int64_t level_qty(const OrderBook& b, Side s, int64_t price) {
    for (std::size_t d = 0; ; ++d) {
        const PriceLevel* l = b.level(s, d);
        if (l == nullptr) return 0;
        if (l->price_ticks == price) return l->total_qty_units;
    }
}

struct Fixture {
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    MatchingEngine engine{kShard, book, pool, nullptr, nullptr};
};

// --- Risk plumbing (Task 2.3.16/2.3.21 admission gate + resolution) ---------

Instrument make_instrument() {
    Instrument i{};
    i.instrument_id = kIid;
    i.status = InstrumentStatus::ACTIVE;
    i.tick_size_ticks = 1;    // every tick multiple passes
    i.lot_size_units = 1;     // every qty is a lot multiple
    i.pip_factor = 10;
    i.pip_size_ticks = 10'000;
    i.min_order_qty_units = 0;
    i.max_order_qty_units = 0;
    i.min_notional_units = 0;
    i.price_band_pct_up = 10'000;    // bands wide open for the unit tests
    i.price_band_pct_down = 10'000;
    i.max_spread_pips = 0;
    i.max_slippage_bps = 0;
    return i;
}

struct MockAccounts final : IAccountState {
    AccountStatus st = AccountStatus::ACTIVE;
    int64_t avail = INT64_MAX / 4;
    uint32_t open_positions = 0;
    StpMode stp_def = static_cast<StpMode>(kStpModeUnset);
    ClientCategory cat = ClientCategory::RETAIL;
    KycTier tier = KycTier::T2;

    AccountStatus status(uint64_t) const noexcept override { return st; }
    int64_t available_balance(uint64_t, uint64_t,
                              BalanceUnit) const noexcept override {
        return avail;
    }
    uint32_t open_position_count(uint64_t) const noexcept override {
        return open_positions;
    }
    StpMode default_stp_mode(uint64_t) const noexcept override {
        return stp_def;
    }
    ClientCategory client_category(uint64_t) const noexcept override {
        return cat;
    }
    KycTier kyc_tier(uint64_t) const noexcept override { return tier; }
};

// Engine wired with the production risk-hook seam: the bound PreTradeChecker
// resolves/stamps stp_mode (Task 2.3.21) and gates NONE by category
// (Task 2.3.16) before the engine's walk ever sees the order.
struct RiskFixture {
    Instrument inst = make_instrument();
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    MockAccounts accounts;
    RiskConfig cfg{};
    PreTradeChecker checker{cfg};
    MatchingEngine engine{kShard, book, pool, nullptr, nullptr};
    EngineRiskBinding binding{&checker, &inst, nullptr};

    RiskFixture() {
        binding.now_ns_source = engine.now_ns_ptr();
        checker.bind_accounts(&accounts);
        // Deliberately unbound positions/market: the checks that consult
        // them skip-or-pass per their documented no-data contracts.
        engine.set_risk_hook(&engine_risk_check, &binding);
    }
};

// --- WAL capture helpers -----------------------------------------------------

struct WalCapture {
    std::filesystem::path dir;
    std::filesystem::path wal_path;
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    std::unique_ptr<Wal> wal;
    std::unique_ptr<WalWriter> writer;
    std::unique_ptr<MatchingEngine> engine;

    explicit WalCapture(const char* name) {
        dir = std::filesystem::temp_directory_path() /
              ("exch_stp_" + std::to_string(::getpid()) + "_" + name);
        std::filesystem::remove_all(dir);
        std::filesystem::create_directories(dir);
        wal_path = dir / "0.wal";
        wal = std::make_unique<Wal>(wal_path.string(), kShard);
        writer = std::make_unique<WalWriter>(wal.get());
        engine =
            std::make_unique<MatchingEngine>(kShard, book, pool, writer.get(),
                                             nullptr);
    }
    bool open() { return wal->open() == WalStatus::Ok; }
};

struct ScannedEntry {
    WalEventType type;
    uint8_t cancel_reason = 0;
    WalPreventedMatchPayload pm{};
    WalOrderModifyPayload mod{};
};

std::vector<ScannedEntry> scan_wal(const std::string& path) {
    std::vector<ScannedEntry> out;
    WalReader r(path);
    WalEntryView ev;
    while (r.next(ev) == WalScanStep::Entry) {
        ScannedEntry e;
        e.type = ev.type;
        if (ev.type == WalEventType::ORDER_CANCEL &&
            ev.payload_len == sizeof(WalOrderCancelPayload)) {
            WalOrderCancelPayload p;
            std::memcpy(&p, ev.payload, sizeof(p));
            e.cancel_reason = p.reason;
        }
        if (ev.type == WalEventType::PREVENTED_MATCH &&
            ev.payload_len == sizeof(WalPreventedMatchPayload)) {
            std::memcpy(&e.pm, ev.payload, sizeof(e.pm));
        }
        if (ev.type == WalEventType::ORDER_MODIFY &&
            ev.payload_len == sizeof(WalOrderModifyPayload)) {
            std::memcpy(&e.mod, ev.payload, sizeof(e.mod));
        }
        out.push_back(e);
    }
    return out;
}

}  // namespace

// --- Task 2.3.11: the four suppressive modes -----------------------------------

TEST(StpModes, CancelNewestRejectsIncoming) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7));
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 30, 7));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(remaining_qty_units(*f.book.find_order(1)), 50);
    EXPECT_EQ(f.book.find_order(2), nullptr);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStpCancelled);
    EXPECT_EQ(f.engine.prevented_qty_total(), 30);
}

TEST(StpModes, CancelOldestCancelsRestingTakerProceeds) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7));
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  P(5100), 20, 8));  // foreign liquidity
    // Same-account CANCEL_OLDEST: maker dies, taker sweeps on to acct 8.
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::LIMIT,
                                  P(5100), 70, 7, TimeInForce::GTC,
                                  StpMode::CANCEL_OLDEST));
    EXPECT_EQ(f.book.find_order(1), nullptr);          // suppressed
    EXPECT_EQ(f.engine.trades_emitted(), 1u);          // filled vs #2
    const Order* t = f.book.find_order(3);
    ASSERT_NE(t, nullptr);                             // remainder rests
    EXPECT_EQ(remaining_qty_units(*t), 50);
    EXPECT_EQ(f.engine.prevented_qty_total(), 50);     // maker's 50 suppressed
}

TEST(StpModes, CancelBothCancelsMakerAndTaker) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7));
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  P(5000), 30, 7, TimeInForce::GTC,
                                  StpMode::CANCEL_BOTH));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(f.book.find_order(1), nullptr);
    EXPECT_EQ(f.book.find_order(2), nullptr);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStpCancelled);
    EXPECT_EQ(f.engine.prevented_qty_total(), 80);     // 50 maker + 30 taker
}

TEST(StpModes, DecrementPartialResting) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7));
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  P(5000), 30, 7, TimeInForce::GTC,
                                  StpMode::DECREMENT));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    const Order* m = f.book.find_order(1);
    ASSERT_NE(m, nullptr);
    EXPECT_EQ(remaining_qty_units(*m), 20);  // 50 - 30
    EXPECT_EQ(f.book.find_order(2), nullptr);  // taker remainder cancelled
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStpCancelled);
    // Prevented qty: 30 suppressed on the surviving maker (order state),
    // 0 taker remainder; totals accumulate both.
    EXPECT_EQ(f.engine.prevented_qty_units(1), 30);
    EXPECT_EQ(f.engine.prevented_qty_total(), 30);
}

TEST(StpModes, DecrementFullyConsumesResting) {
    Fixture f;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 20, 7));
    // Taker 50 > maker 20: maker dies, the taker's 30 remainder is cancelled
    // (spec §6.5 literal — DECREMENT never trades with outsiders mid-step).
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  P(5000), 50, 7, TimeInForce::GTC,
                                  StpMode::DECREMENT));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(f.book.find_order(1), nullptr);
    EXPECT_EQ(f.book.find_order(2), nullptr);
    EXPECT_EQ(f.engine.prevented_qty_total(), 50);  // 20 maker + 30 taker
}

TEST(StpModes, DecrementIcebergHiddenPortion) {
    Fixture f;
    // Iceberg maker 100 total, 10 visible — same account as the taker.
    f.engine.on_order_received(mk(f.pool, 1, Side::SELL, OrderType::ICEBERG,
                                  P(5000), 100, 7, TimeInForce::GTC,
                                  StpMode::CANCEL_NEWEST, /*display=*/10));
    ASSERT_EQ(level_qty(f.book, Side::SELL, P(5000)), 10);
    // Taker 25: suppresses 25 across slice (10) + hidden (15) — the resting
    // order total must land at 75, not 85 (the slice's 10 units would
    // otherwise replenish back into the hidden reserve).
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  P(5000), 25, 7, TimeInForce::GTC,
                                  StpMode::DECREMENT));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    const auto* rec = f.engine.icebergs().find(1);
    ASSERT_NE(rec, nullptr);
    EXPECT_EQ(rec->total_qty_units - rec->filled_total_units, 75);
    // Refreshed slice came back at the level tail.
    EXPECT_EQ(level_qty(f.book, Side::SELL, P(5000)), 10);
    EXPECT_EQ(f.engine.prevented_qty_total(), 25 + 0);  // taker rem was 0
    EXPECT_EQ(f.engine.prevented_qty_units(1), 25);
}

TEST(StpModes, StpDuringTriggerSweep) {
    // Auction-uncross analogue: the triggered-stop sweep re-enters
    // walk_match, so STP applies identically there (the Phase-15 auction
    // uncross drives the same loop).
    Fixture f;
    // Same-account ask rests BEHIND the trigger price at 5200 — the sweep
    // walks asks ascending and reaches it only after firing.
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5200), 40, 7));
    // Pending stop for the SAME account, CANCEL_BOTH: fires on last >= 5100.
    OrderAux aux{};
    aux.stop_price_ticks = P(5100);
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::STOP, 0, 30,
                                  7, TimeInForce::GTC, StpMode::CANCEL_BOTH),
                             aux);
    ASSERT_TRUE(f.engine.stops().pending(2));
    // A foreign ask becomes the top-of-book print level at 5100.
    f.engine.on_order_received(
        mk(f.pool, 3, Side::SELL, OrderType::LIMIT, P(5100), 10, 8));
    // Foreign taker prints last=5100 at top-of-book -> stop fires.
    f.engine.on_order_received(
        mk(f.pool, 4, Side::BUY, OrderType::LIMIT, P(5100), 10, 9));
    // Triggered sweep hits its own account's 5200 ask -> CANCEL_BOTH.
    EXPECT_EQ(f.book.find_order(1), nullptr);
    EXPECT_EQ(f.book.find_order(3), nullptr);  // filled by order 4
    EXPECT_EQ(f.book.live_orders(), 0u);
    EXPECT_EQ(f.engine.trades_emitted(), 1u);  // only the 3x4 foreign fill
    EXPECT_STREQ(f.engine.last_reject(),
                 MatchingEngine::kRejectStpCancelled);
}

// --- Task 2.3.16: NONE gating + SELF_TRADE surveillance -------------------------

TEST(StpNone, RetailRejectedProfessionalProceeds) {
    RiskFixture f;
    // Retail + explicit NONE -> STP_NONE_NOT_PERMITTED pre-trade reject.
    f.accounts.cat = ClientCategory::RETAIL;
    f.engine.on_order_received(mk(f.pool, 1, Side::SELL, OrderType::LIMIT,
                                  P(5000), 50, 7, TimeInForce::GTC,
                                  StpMode::NONE));
    EXPECT_STREQ(f.engine.last_reject(), "STP_NONE_NOT_PERMITTED");
    EXPECT_EQ(f.book.live_orders(), 0u);

    // Professional + NONE -> admitted; self-trade executes.
    f.accounts.cat = ClientCategory::PROFESSIONAL;
    f.engine.on_order_received(mk(f.pool, 2, Side::SELL, OrderType::LIMIT,
                                  P(5000), 50, 7, TimeInForce::GTC,
                                  StpMode::NONE));
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::LIMIT,
                                  P(5000), 20, 7, TimeInForce::GTC,
                                  StpMode::NONE));
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(remaining_qty_units(*f.book.find_order(2)), 30);
}

TEST(StpNone, ResolvedNoneFromAccountDefaultIsGated) {
    RiskFixture f;
    f.accounts.stp_def = StpMode::NONE;
    f.accounts.cat = ClientCategory::RETAIL;
    f.engine.on_order_received(mk(
        f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7,
        TimeInForce::GTC, static_cast<StpMode>(kStpModeUnset)));
    EXPECT_STREQ(f.engine.last_reject(), "STP_NONE_NOT_PERMITTED");
    EXPECT_EQ(f.book.live_orders(), 0u);
}

TEST(StpNone, SelfTradeEmitsSurveillanceEvent) {
    RiskFixture f;
    f.accounts.cat = ClientCategory::ELIGIBLE_COUNTERPARTY;
    std::vector<MatchingEngine::SelfTradeEvent> events;
    f.engine.set_self_trade_sink(
        [](void* ctx, const MatchingEngine::SelfTradeEvent& ev) noexcept {
            static_cast<std::vector<MatchingEngine::SelfTradeEvent>*>(ctx)
                ->push_back(ev);
        },
        &events);
    f.engine.on_order_received(mk(f.pool, 1, Side::SELL, OrderType::LIMIT,
                                  P(5000), 50, 7, TimeInForce::GTC,
                                  StpMode::NONE));
    // Foreign account crossing the same level produces NO event.
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  P(5000), 10, 8, TimeInForce::GTC,
                                  StpMode::NONE));
    EXPECT_TRUE(events.empty());
    // Same-account NONE fill -> exactly one flagged self-trade.
    f.engine.on_order_received(mk(f.pool, 3, Side::BUY, OrderType::LIMIT,
                                  P(5000), 15, 7, TimeInForce::GTC,
                                  StpMode::NONE));
    ASSERT_EQ(events.size(), 1u);
    EXPECT_EQ(events[0].taker_order_id, 3u);
    EXPECT_EQ(events[0].maker_order_id, 1u);
    EXPECT_EQ(events[0].taker_account_id, 7u);
    EXPECT_EQ(events[0].maker_account_id, 7u);
    EXPECT_EQ(events[0].qty_units, 15);
    EXPECT_EQ(events[0].price_ticks, P(5000));
    EXPECT_EQ(f.engine.trades_emitted(), 2u);
}

// --- Task 2.3.18: trade groups, TRANSFER, prevented matches --------------------

TEST(StpGroups, SameGroupDifferentAccountsIsSelfTrade) {
    Fixture f;
    OrderAux maker_aux{};
    maker_aux.trade_group_id = 42;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7), maker_aux);
    OrderAux taker_aux{};
    taker_aux.trade_group_id = 42;
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  P(5000), 30, 9, TimeInForce::GTC,
                                  StpMode::CANCEL_BOTH),
                               taker_aux);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(f.book.find_order(1), nullptr);
    EXPECT_EQ(f.book.find_order(2), nullptr);
}

TEST(StpGroups, DifferentGroupTradesNormally) {
    Fixture f;
    OrderAux aux{};
    aux.trade_group_id = 42;
    f.engine.on_order_received(
        mk(f.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7), aux);
    OrderAux taker_aux{};
    taker_aux.trade_group_id = 77;  // different group — ordinary match
    f.engine.on_order_received(
        mk(f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 30, 9), taker_aux);
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
    EXPECT_EQ(remaining_qty_units(*f.book.find_order(1)), 20);
}

TEST(StpTransfer, RequiresBothSidesToRequest) {
    Fixture f;
    OrderAux aux{};
    aux.trade_group_id = 42;
    // Maker requests TRANSFER but the taker does not -> taker mode governs
    // (CANCEL_NEWEST here: taker dies, no decrement, no PREVENTED_MATCH).
    f.engine.on_order_received(mk(f.pool, 1, Side::SELL, OrderType::LIMIT,
                                  P(5000), 50, 7, TimeInForce::GTC,
                                  StpMode::CANCEL_NEWEST, 0,
                                  kOrderFlagStpTransfer),
                               aux);
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  P(5000), 30, 9, TimeInForce::GTC,
                                  StpMode::CANCEL_NEWEST),
                               aux);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(remaining_qty_units(*f.book.find_order(1)), 50);  // untouched
    EXPECT_EQ(f.book.find_order(2), nullptr);
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStpCancelled);
}

TEST(StpTransfer, SameAccountBehavesAsDecrementWithoutEvent) {
    WalCapture cap("xfer_same_acct");
    ASSERT_TRUE(cap.open());
    std::vector<WalPreventedMatchPayload> events;
    cap.engine->set_prevented_match_sink(
        [](void* ctx, const WalPreventedMatchPayload& p) noexcept {
            static_cast<std::vector<WalPreventedMatchPayload>*>(ctx)
                ->push_back(p);
        },
        &events);
    cap.engine->on_order_received(mk(cap.pool, 1, Side::SELL, OrderType::LIMIT,
                                     P(5000), 50, 7, TimeInForce::GTC,
                                     StpMode::DECREMENT, 0,
                                     kOrderFlagStpTransfer));
    cap.engine->on_order_received(mk(cap.pool, 2, Side::BUY, OrderType::LIMIT,
                                     P(5000), 30, 7, TimeInForce::GTC,
                                     StpMode::DECREMENT, 0,
                                     kOrderFlagStpTransfer));
    EXPECT_EQ(cap.engine->trades_emitted(), 0u);
    EXPECT_EQ(remaining_qty_units(*cap.book.find_order(1)), 20);
    EXPECT_EQ(cap.book.find_order(2), nullptr);
    EXPECT_TRUE(events.empty());  // same account -> plain DECREMENT
    (void)cap.wal->flush();
    const auto entries = scan_wal(cap.wal_path.string());
    for (const auto& e : entries) {
        EXPECT_NE(e.type, WalEventType::PREVENTED_MATCH);
    }
}

TEST(StpTransfer, CrossAccountGroupEmitsPreventedMatch) {
    WalCapture cap("xfer_group");
    ASSERT_TRUE(cap.open());
    std::vector<WalPreventedMatchPayload> events;
    cap.engine->set_prevented_match_sink(
        [](void* ctx, const WalPreventedMatchPayload& p) noexcept {
            static_cast<std::vector<WalPreventedMatchPayload>*>(ctx)
                ->push_back(p);
        },
        &events);
    OrderAux aux{};
    aux.trade_group_id = 42;
    aux.instrument_id = kIid;
    cap.engine->on_order_received(mk(cap.pool, 1, Side::SELL, OrderType::LIMIT,
                                     P(5000), 50, 7, TimeInForce::GTC,
                                     StpMode::DECREMENT, 0,
                                     kOrderFlagStpTransfer),
                                  aux);
    // Mutual TRANSFER across accounts in group 42.
    cap.engine->on_order_received(mk(cap.pool, 2, Side::BUY, OrderType::LIMIT,
                                     P(5000), 30, 9, TimeInForce::GTC,
                                     StpMode::DECREMENT, 0,
                                     kOrderFlagStpTransfer),
                                  aux);
    EXPECT_EQ(cap.engine->trades_emitted(), 0u);       // NO trade emitted
    EXPECT_EQ(remaining_qty_units(*cap.book.find_order(1)), 20);
    EXPECT_EQ(cap.book.find_order(2), nullptr);
    EXPECT_EQ(cap.engine->prevented_qty_units(1), 30);
    EXPECT_EQ(cap.engine->prevented_qty_total(), 30);

    ASSERT_EQ(events.size(), 1u);
    EXPECT_EQ(events[0].maker_order_id, 1u);
    EXPECT_EQ(events[0].taker_order_id, 2u);
    EXPECT_EQ(events[0].maker_account_id, 7u);
    EXPECT_EQ(events[0].taker_account_id, 9u);
    EXPECT_EQ(events[0].trade_group_id, 42u);
    EXPECT_EQ(events[0].mode, static_cast<uint8_t>(StpAction::TRANSFER));
    EXPECT_EQ(events[0].price_ticks, P(5000));
    EXPECT_EQ(events[0].maker_prevented_qty_units, 30);
    EXPECT_EQ(events[0].taker_prevented_qty_units, 0);

    (void)cap.wal->flush();
    const auto entries = scan_wal(cap.wal_path.string());
    int prevented = 0, trades = 0;
    for (const auto& e : entries) {
        if (e.type == WalEventType::PREVENTED_MATCH) {
            ++prevented;
            EXPECT_EQ(e.pm.maker_order_id, 1u);
            EXPECT_EQ(e.pm.taker_order_id, 2u);
        }
        if (e.type == WalEventType::TRADE) ++trades;
    }
    EXPECT_EQ(prevented, 1);
    EXPECT_EQ(trades, 0);  // suppressed matches journal no TRADE
    std::filesystem::remove_all(cap.dir);
}

// --- WAL reason=STP on every suppressive outcome + deterministic replay --------

TEST(StpWal, AllOutcomesJournalReasonStp) {
    // Every suppressive outcome journals ORDER_CANCEL with reason=STP.
    // One isolated WAL per mode: CANCEL_OLDEST lets the taker proceed and
    // rest, which would contaminate later same-price pairs in a shared book.
    auto run = [](const char* name, StpMode m, int64_t maker_qty,
                  int64_t taker_qty) {
        WalCapture cap(name);
        EXPECT_TRUE(cap.open());
        cap.engine->on_order_received(mk(cap.pool, 1, Side::SELL,
                                         OrderType::LIMIT, P(5000), maker_qty,
                                         7));
        cap.engine->on_order_received(mk(cap.pool, 2, Side::BUY,
                                         OrderType::LIMIT, P(5000), taker_qty,
                                         7, TimeInForce::GTC, m));
        (void)cap.wal->flush();
        int stp = 0, other = 0, mods = 0;
        for (const auto& e : scan_wal(cap.wal_path.string())) {
            if (e.type == WalEventType::ORDER_CANCEL) {
                if (e.cancel_reason == kWalCancelReasonStp) ++stp;
                else ++other;
            } else if (e.type == WalEventType::ORDER_MODIFY) {
                ++mods;
            }
        }
        std::filesystem::remove_all(cap.dir);
        return std::array<int, 3>{stp, other, mods};
    };
    // {stp cancels, other cancels, modifies}:
    EXPECT_EQ(run("cn", StpMode::CANCEL_NEWEST, 50, 10),
              (std::array<int, 3>{1, 0, 0}));  // taker only
    EXPECT_EQ(run("co", StpMode::CANCEL_OLDEST, 50, 10),
              (std::array<int, 3>{1, 0, 0}));  // maker only; taker rests
    EXPECT_EQ(run("cb", StpMode::CANCEL_BOTH, 50, 10),
              (std::array<int, 3>{2, 0, 0}));  // maker + taker
    EXPECT_EQ(run("dec", StpMode::DECREMENT, 50, 10),
              (std::array<int, 3>{1, 0, 1}));  // taker cancel + maker modify
}

TEST(StpWal, DeterministicReplayReproducesSuppressedBook) {
    // Replay the post-suppression tail a live engine would have journaled:
    // a surviving maker's NEW+MODIFY, the PREVENTED_MATCH audit record for
    // the group TRANSFER, the suppressed makers' STP cancels, and the
    // surviving CANCEL_OLDEST taker's NEW. RecoveryManager must consume the
    // whole stream — PREVENTED_MATCH replaying as a book-level no-op —
    // and land on exactly the live suppressed state {#4 bid 5100x10}.
    //
    // NOTE (integration seam, RecoveryManager.cpp — outside owned files):
    // the live engine also journals ORDER_NEW for every admitted taker
    // before the walk (MatchingEngine.cpp write_order_new site). Replaying
    // such a NEW for a DEAD taker over a still-resting same-price maker
    // trips OrderBook::add_order's CROSSED guard -> ApplyFailed, so a raw
    // live WAL containing suppressions (or any filled taker) cannot yet be
    // replayed end-to-end; recovery needs a crossed-tolerant insertion for
    // entries that are transient by journal design. This test therefore
    // exercises the deterministic replay of the persistent tail.
    WalCapture cap("replay");
    ASSERT_TRUE(cap.open());
    OrderAux aux{};
    aux.instrument_id = kIid;
    aux.trade_group_id = 42;
    const uint64_t base_ts = 1'700'000'000'000ULL;

    Order* m1 = mk(cap.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7,
                   TimeInForce::GTC, StpMode::DECREMENT, 0,
                   kOrderFlagStpTransfer);
    ASSERT_EQ(cap.writer->write_order_new(*m1, aux, kIid, 50, base_ts),
              WalStatus::Ok);
    ASSERT_EQ(cap.writer->write_order_modify(1, P(5000), 20, 0, base_ts + 1),
              WalStatus::Ok);
    // Mutual group-42 TRANSFER audit record (maker 1 acct7, taker 2 acct9).
    WalPreventedMatchPayload pm{};
    pm.maker_order_id = 1;
    pm.taker_order_id = 2;
    pm.maker_account_id = 7;
    pm.taker_account_id = 9;
    pm.trade_group_id = 42;
    pm.mode = static_cast<uint8_t>(StpAction::TRANSFER);
    pm.price_ticks = P(5000);
    pm.maker_prevented_qty_units = 30;
    pm.taker_prevented_qty_units = 0;
    pm.prevented_notional_units = 30 * P(5000);
    pm.ts_ns = base_ts + 2;
    ASSERT_EQ(cap.writer->write_prevented_match(pm, base_ts + 2),
              WalStatus::Ok);
    ASSERT_EQ(cap.writer->write_order_cancel(2, 9, kWalCancelReasonStp,
                                             base_ts + 3),
              WalStatus::Ok);
    Order* m3 = mk(cap.pool, 3, Side::SELL, OrderType::LIMIT, P(5100), 40, 8);
    ASSERT_EQ(cap.writer->write_order_new(*m3, aux, kIid, 40, base_ts + 4),
              WalStatus::Ok);
    // CANCEL_OLDEST sweep suppressed both same-group makers.
    ASSERT_EQ(cap.writer->write_order_cancel(1, 7, kWalCancelReasonStp,
                                             base_ts + 5),
              WalStatus::Ok);
    ASSERT_EQ(cap.writer->write_order_cancel(3, 8, kWalCancelReasonStp,
                                             base_ts + 6),
              WalStatus::Ok);
    Order* t4 = mk(cap.pool, 4, Side::BUY, OrderType::LIMIT, P(5100), 10, 8,
                   TimeInForce::GTC, StpMode::CANCEL_OLDEST);
    OrderAux aux4{};
    aux4.instrument_id = kIid;
    aux4.trade_group_id = 42;
    ASSERT_EQ(cap.writer->write_order_new(*t4, aux4, kIid, 10, base_ts + 7),
              WalStatus::Ok);
    (void)cap.wal->flush();
    cap.wal->close();

    MemoryPool<Order> pool2{512};
    OrderBook book2{pool2};
    RecoveryManager rm(kShard);
    const RecoveryResult res =
        rm.recover(cap.dir.string(), {{kIid, &book2}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    EXPECT_EQ(book2.live_orders(), 1u);
    const Order* r = book2.find_order(4);
    ASSERT_NE(r, nullptr);
    EXPECT_EQ(remaining_qty_units(*r), 10);
    EXPECT_EQ(r->side, Side::BUY);
    EXPECT_EQ(r->price_ticks, P(5100));
    EXPECT_EQ(book2.find_order(1), nullptr);
    EXPECT_EQ(book2.find_order(3), nullptr);
    EXPECT_EQ(res.books[0].recomputed_book_seq, res.wal_tail);
    std::filesystem::remove_all(cap.dir);
}

// --- Task 2.3.21: account-default resolution order ------------------------------

TEST(StpDefault, ResolutionOrderAndStamping) {
    RiskFixture f;
    f.accounts.stp_def = StpMode::CANCEL_BOTH;
    // Per-order value wins over the account default.
    f.engine.on_order_received(mk(f.pool, 1, Side::SELL, OrderType::LIMIT,
                                  P(5000), 50, 7));
    f.engine.on_order_received(mk(f.pool, 2, Side::BUY, OrderType::LIMIT,
                                  P(5000), 30, 7, TimeInForce::GTC,
                                  StpMode::CANCEL_OLDEST));
    EXPECT_EQ(f.book.find_order(1), nullptr);   // CANCEL_OLDEST won
    EXPECT_NE(f.book.find_order(2), nullptr);   // taker rests after sweep

    // Unset sentinel resolves through the account default (CANCEL_BOTH) —
    // isolated at a fresh level so pair-1 leftovers can't interfere.
    f.engine.on_order_received(mk(f.pool, 3, Side::SELL, OrderType::LIMIT,
                                  P(5100), 50, 7));
    f.engine.on_order_received(mk(
        f.pool, 4, Side::BUY, OrderType::LIMIT, P(5100), 30, 7,
        TimeInForce::GTC, static_cast<StpMode>(kStpModeUnset)));
    EXPECT_EQ(f.book.find_order(3), nullptr);   // CANCEL_BOTH suppressed both
    EXPECT_EQ(f.book.find_order(4), nullptr);
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
}

TEST(StpDefault, NoDefaultFallsToCancelNewest) {
    RiskFixture f;
    f.accounts.stp_def = static_cast<StpMode>(kStpModeUnset);
    f.engine.on_order_received(mk(f.pool, 1, Side::SELL, OrderType::LIMIT,
                                  P(5000), 50, 7));
    f.engine.on_order_received(mk(
        f.pool, 2, Side::BUY, OrderType::LIMIT, P(5000), 30, 7,
        TimeInForce::GTC, static_cast<StpMode>(kStpModeUnset)));
    EXPECT_EQ(f.engine.trades_emitted(), 0u);
    EXPECT_EQ(remaining_qty_units(*f.book.find_order(1)), 50);  // taker died
    EXPECT_STREQ(f.engine.last_reject(), MatchingEngine::kRejectStpCancelled);
}

TEST(StpDefault, StampedModeFlowsToWal) {
    WalCapture cap("stamp");
    ASSERT_TRUE(cap.open());
    Instrument inst = make_instrument();
    MockAccounts accounts;
    accounts.stp_def = StpMode::CANCEL_BOTH;
    PreTradeChecker checker{RiskConfig{}};
    checker.bind_accounts(&accounts);
    EngineRiskBinding binding{&checker, &inst, cap.engine->now_ns_ptr()};
    cap.engine->set_risk_hook(&engine_risk_check, &binding);
    OrderAux aux{};
    aux.instrument_id = kIid;
    cap.engine->on_order_received(mk(
        cap.pool, 1, Side::SELL, OrderType::LIMIT, P(5000), 50, 7,
        TimeInForce::GTC, static_cast<StpMode>(kStpModeUnset)),
                                  aux);
    (void)cap.wal->flush();
    // Direct scan for the stamped resolved mode on the ORDER_NEW record.
    WalReader r(cap.wal_path.string());
    WalEntryView ev;
    ASSERT_EQ(r.next(ev), WalScanStep::Entry);
    ASSERT_EQ(ev.type, WalEventType::ORDER_NEW);
    ASSERT_EQ(ev.payload_len, sizeof(WalOrderNewPayload));
    WalOrderNewPayload p;
    std::memcpy(&p, ev.payload, sizeof(p));
    // Resolved CANCEL_BOTH — not the 0xFF unset sentinel — is journaled.
    EXPECT_EQ(p.stp_mode,
              static_cast<uint32_t>(StpMode::CANCEL_BOTH));
    std::filesystem::remove_all(cap.dir);
}
