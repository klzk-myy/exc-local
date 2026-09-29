// Phase-14 Task 14.3.1 — OCO (one-cancels-other) coverage (spec §6.2/§6.5,
// §24 #47): link install + validation, atomic sibling cancel on terminal
// fill, the doomed-leg OCO_SIBLING_CANCEL_RACE gate (deterministic race
// resolution by monotonic ring sequence), pair dissolve on non-fill
// terminals, WAL OCO_LINK journaling, and recovery replay convergence.

#include <gtest/gtest.h>

#include <cstdio>
#include <filesystem>
#include <string>
#include <unistd.h>
#include <vector>

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "ipc/IpcChannel.hpp"
#include "matching/IpcPublisher.hpp"
#include "matching/MatchingEngine.hpp"
#include "matching/WalWriter.hpp"
#include "recovery/RecoveryManager.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

using namespace exch;

namespace {

constexpr uint32_t kIid = 7;
constexpr uint32_t kShard = 3;
uint64_t g_ts = 1'000'000'000;

Order* mk(MemoryPool<Order>& pool, uint64_t id, Side side, int64_t price,
          int64_t qty, uint64_t acct = 1,
          TimeInForce tif = TimeInForce::GTC) {
    Order* o = pool.alloc();
    if (o == nullptr) return nullptr;
    *o = Order{};
    o->id = id;
    o->account_id = acct;
    o->side = side;
    o->type = OrderType::LIMIT;
    o->tif = tif;
    o->stp_mode = StpMode::CANCEL_NEWEST;
    o->price_ticks = price;
    o->qty_units = qty;
    o->quantity = Decimal::from_mantissa(qty);
    o->timestamp_ns = ++g_ts;
    o->ingress_seq = g_ts;
    return o;
}

struct Fixture {
    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    MatchingEngine engine{3, book, pool, nullptr, nullptr};

    void link(uint64_t link, uint64_t a, uint64_t b, uint64_t acct = 1) {
        engine.on_oco_link_received(link, a, b, acct, kIid);
    }
};

std::filesystem::path tmp_dir(const char* name) {
    const auto d = std::filesystem::temp_directory_path() /
                   ("exch_oco_" + std::to_string(::getpid()) + "_" + name);
    std::filesystem::remove_all(d);
    std::filesystem::create_directories(d);
    return d;
}

}  // namespace

// --- Link install + validation ----------------------------------------------

TEST(OcoEngine, LinkInstallsPair) {
    Fixture f;
    f.link(100, 10, 20);
    EXPECT_EQ(f.engine.oco_member_count(), 2u);
    EXPECT_EQ(f.engine.oco_link_count(), 1u);
    EXPECT_EQ(f.engine.oco_member_state(10), 0);  // armed
    EXPECT_EQ(f.engine.oco_member_state(20), 0);
    EXPECT_EQ(f.engine.oco_member_state(99), -1);  // unlinked
}

TEST(OcoEngine, LinkValidation) {
    Fixture f;
    f.link(0, 10, 20);   // zero link id
    EXPECT_STREQ(f.engine.last_reject(), "OCO_LINK_INVALID");
    f.link(1, 0, 20);    // zero leg id
    EXPECT_STREQ(f.engine.last_reject(), "OCO_LINK_INVALID");
    f.link(1, 5, 5);     // self-pair
    EXPECT_STREQ(f.engine.last_reject(), "OCO_LINK_INVALID");
    EXPECT_EQ(f.engine.oco_member_count(), 0u);
    EXPECT_EQ(f.engine.reject_count(), 3u);
}

TEST(OcoEngine, LinkConflictAndIdempotentResend) {
    Fixture f;
    f.link(1, 10, 20);
    const uint64_t rej0 = f.engine.reject_count();
    f.link(1, 10, 20);   // identical re-send — idempotent no-op
    EXPECT_EQ(f.engine.reject_count(), rej0);
    EXPECT_EQ(f.engine.oco_member_count(), 2u);
    f.link(2, 10, 30);   // 10 already linked — conflict
    EXPECT_STREQ(f.engine.last_reject(), "OCO_LINK_CONFLICT");
    EXPECT_EQ(f.engine.oco_link_count(), 1u);
    f.link(3, 40, 10);   // conflict from the other leg too
    EXPECT_STREQ(f.engine.last_reject(), "OCO_LINK_CONFLICT");
}

// --- Fill-triggered sibling cancel -------------------------------------------

TEST(OcoEngine, FillCancelsSiblingAtomically) {
    Fixture f;
    f.link(1, 10, 20);
    OrderAux aux{};
    aux.instrument_id = kIid;
    f.engine.on_order_received(
        mk(f.pool, 10, Side::BUY, 5000, 50), aux);    // leg A rests bid
    f.engine.on_order_received(
        mk(f.pool, 20, Side::SELL, 5100, 30), aux);   // leg B rests ask
    ASSERT_NE(f.book.find_order(10), nullptr);
    ASSERT_NE(f.book.find_order(20), nullptr);

    // Aggressor lifts leg A fully — leg B must die in the same dispatch.
    f.engine.on_order_received(
        mk(f.pool, 30, Side::SELL, 5000, 50, /*acct=*/2), aux);
    EXPECT_EQ(f.book.find_order(10), nullptr);        // filled
    EXPECT_EQ(f.book.find_order(20), nullptr);        // OCO sibling cancelled
    EXPECT_EQ(f.engine.oco_member_count(), 0u);       // link fully released
    EXPECT_EQ(f.engine.oco_link_count(), 0u);
}

TEST(OcoEngine, PartialFillKeepsSibling) {
    Fixture f;
    f.link(1, 10, 20);
    OrderAux aux{};
    aux.instrument_id = kIid;
    f.engine.on_order_received(mk(f.pool, 10, Side::BUY, 5000, 50), aux);
    f.engine.on_order_received(mk(f.pool, 20, Side::SELL, 5100, 30), aux);

    f.engine.on_order_received(
        mk(f.pool, 30, Side::SELL, 5000, 20, /*acct=*/2), aux);
    const Order* a = f.book.find_order(10);
    ASSERT_NE(a, nullptr);
    EXPECT_EQ(remaining_qty_units(*a), 30);   // partial — leg still live
    EXPECT_NE(f.book.find_order(20), nullptr);
    EXPECT_EQ(f.engine.oco_member_count(), 2u);  // pair still armed
}

TEST(OcoEngine, FillingSiblingCancelsTheWinnerSide) {
    // Symmetric arm: fill leg B (the sell leg) — buy leg A must die.
    Fixture f;
    f.link(1, 10, 20);
    OrderAux aux{};
    aux.instrument_id = kIid;
    f.engine.on_order_received(mk(f.pool, 10, Side::BUY, 5000, 50), aux);
    f.engine.on_order_received(mk(f.pool, 20, Side::SELL, 5100, 30), aux);
    // Third-party maker so the taker fully consumes leg B.
    f.engine.on_order_received(
        mk(f.pool, 40, Side::BUY, 5100, 30, /*acct=*/2), aux);
    EXPECT_EQ(f.book.find_order(20), nullptr);
    EXPECT_EQ(f.book.find_order(10), nullptr);
    EXPECT_EQ(f.engine.oco_member_count(), 0u);
}

TEST(OcoEngine, FillCancelsPendingStopSibling) {
    // Leg A is a pending STOP — the sibling cancel must reach the pending
    // stop queue, not just the book.
    Fixture f;
    f.link(1, 10, 20);
    OrderAux aux{};
    aux.instrument_id = kIid;
    aux.stop_price_ticks = 5200;
    f.engine.on_order_received(
        mk(f.pool, 20, Side::SELL, 5100, 30), aux);  // resting ask leg
    Order* stop = mk(f.pool, 10, Side::BUY, 0, 50);
    stop->type = OrderType::STOP;
    f.engine.on_order_received(stop, aux);           // pending stop leg
    ASSERT_TRUE(f.engine.stops().pending(10));

    f.engine.on_order_received(
        mk(f.pool, 30, Side::BUY, 5100, 30, /*acct=*/2), aux);
    EXPECT_EQ(f.book.find_order(20), nullptr);       // filled
    EXPECT_FALSE(f.engine.stops().pending(10));      // stop sibling cancelled
    EXPECT_EQ(f.engine.oco_member_count(), 0u);
}

// --- Deterministic race: the doomed leg --------------------------------------

TEST(OcoEngine, DoomedLegRejectsWithOcoSiblingCancelRace) {
    // Wire reality: the gateway sequences OcoLink then both legs' OrderNew
    // on the ring. If leg A fills while leg B's OrderNew is still in flight
    // on the same ring, B arrives doomed — monotonic sequence decides, the
    // trailing leg is rejected OCO_SIBLING_CANCEL_RACE (spec §6.5/§6.8).
    Fixture f;
    f.link(1, 10, 20);
    OrderAux aux{};
    aux.instrument_id = kIid;
    f.engine.on_order_received(mk(f.pool, 10, Side::BUY, 5000, 50), aux);
    f.engine.on_order_received(
        mk(f.pool, 30, Side::SELL, 5000, 50, /*acct=*/2), aux);
    ASSERT_EQ(f.book.find_order(10), nullptr);       // A filled; B not yet sent
    ASSERT_EQ(f.engine.oco_member_state(20), 1);     // doomed mark

    f.engine.on_order_received(mk(f.pool, 20, Side::SELL, 5100, 30), aux);
    EXPECT_STREQ(f.engine.last_reject(), "OCO_SIBLING_CANCEL_RACE");
    EXPECT_EQ(f.book.find_order(20), nullptr);       // never admitted
    EXPECT_EQ(f.engine.oco_member_count(), 0u);      // doomed entry released
}

// --- Non-fill terminals dissolve the pair ------------------------------------

TEST(OcoEngine, UserCancelDissolvesPairSiblingSurvives) {
    Fixture f;
    f.link(1, 10, 20);
    OrderAux aux{};
    aux.instrument_id = kIid;
    f.engine.on_order_received(mk(f.pool, 10, Side::BUY, 5000, 50), aux);
    f.engine.on_order_received(mk(f.pool, 20, Side::SELL, 5100, 30), aux);

    f.engine.on_cancel_received(10, 1);              // cancel leg A
    EXPECT_EQ(f.book.find_order(10), nullptr);
    EXPECT_NE(f.book.find_order(20), nullptr);       // sibling survives
    EXPECT_EQ(f.engine.oco_member_count(), 0u);      // … standalone
    // And it fills normally afterwards — no stale link.
    f.engine.on_order_received(
        mk(f.pool, 30, Side::BUY, 5100, 30, /*acct=*/2), aux);
    EXPECT_EQ(f.book.find_order(20), nullptr);
    EXPECT_EQ(f.engine.trades_emitted(), 1u);
}

TEST(OcoEngine, ExpiredLegDissolvesPair) {
    Fixture f;
    f.link(1, 10, 20);
    OrderAux aux{};
    aux.instrument_id = kIid;
    aux.gtd_expiry_ns = 2'000'000'000;               // expires at ts 2e9
    f.engine.on_order_received(mk(f.pool, 10, Side::BUY, 5000, 50), aux);
    aux.gtd_expiry_ns = 0;
    f.engine.on_order_received(mk(f.pool, 20, Side::SELL, 5100, 30), aux);

    f.engine.on_time_tick(2'000'000'001);            // sweep expires leg A
    EXPECT_EQ(f.book.find_order(10), nullptr);
    EXPECT_NE(f.book.find_order(20), nullptr);
    EXPECT_EQ(f.engine.oco_member_count(), 0u);
}

// --- WAL + recovery ------------------------------------------------------------

TEST(OcoEngine, WalJournalsLinkAndSiblingCancel) {
    const auto dir = tmp_dir("wal_journal");
    const auto wpath = dir / "0.wal";
    {
        MemoryPool<Order> pool{512};
        OrderBook book{pool};
        Wal wal(wpath.string(), kShard);
        ASSERT_EQ(wal.open(), WalStatus::Ok);
        WalWriter ww(&wal);
        MatchingEngine eng(kShard, book, pool, &ww, nullptr);
        OrderAux aux{};
        aux.instrument_id = kIid;
        eng.on_oco_link_received(1, 10, 20, 1, kIid);
        eng.on_order_received(mk(pool, 10, Side::BUY, 5000, 50), aux);
        eng.on_order_received(mk(pool, 20, Side::SELL, 5100, 30), aux);
        eng.on_order_received(mk(pool, 30, Side::SELL, 5000, 50, 2), aux);
        wal.close();
    }
    // Scan the segment: expect OCO_LINK and a reason-7 ORDER_CANCEL for 20.
    WalReader r(wpath.string());
    ASSERT_EQ(r.open(wpath.string()), WalStatus::Ok);
    bool saw_link = false;
    bool saw_oco_cancel = false;
    WalEntryView ev{};
    while (r.next(ev) == WalScanStep::Entry) {
        if (ev.type == WalEventType::OCO_LINK) {
            WalOcoLinkPayload p;
            ASSERT_EQ(ev.payload_len, sizeof(p));
            std::memcpy(&p, ev.payload, sizeof(p));
            EXPECT_EQ(p.link_id, 1u);
            EXPECT_EQ(p.order_id_a, 10u);
            EXPECT_EQ(p.order_id_b, 20u);
            EXPECT_EQ(p.instrument_id, kIid);
            saw_link = true;
        }
        if (ev.type == WalEventType::ORDER_CANCEL) {
            WalOrderCancelPayload p;
            ASSERT_EQ(ev.payload_len, sizeof(p));
            std::memcpy(&p, ev.payload, sizeof(p));
            if (p.order_id == 20 &&
                p.reason == kWalCancelReasonOcoLink) {
                saw_oco_cancel = true;
            }
        }
    }
    EXPECT_TRUE(saw_link);
    EXPECT_TRUE(saw_oco_cancel);
    EXPECT_FALSE(r.corrupt_seen());
    r.close();
}

TEST(OcoEngine, RecoveryReplaysLinkAndSiblingCancel) {
    // Live engine journals the OCO flow; a cold RecoveryManager replay must
    // reproduce the terminal state — link restored, winner filled, sibling
    // cancelled — deterministically.
    const auto dir = tmp_dir("replay");
    {
        MemoryPool<Order> pool{512};
        OrderBook book{pool};
        const auto wpath = dir / "0.wal";
        Wal wal(wpath.string(), kShard);
        ASSERT_EQ(wal.open(), WalStatus::Ok);
        WalWriter ww(&wal);
        MatchingEngine eng(kShard, book, pool, &ww, nullptr);
        OrderAux aux{};
        aux.instrument_id = kIid;
        eng.on_oco_link_received(1, 10, 20, 1, kIid);
        eng.on_order_received(mk(pool, 10, Side::BUY, 5000, 50), aux);
        eng.on_order_received(mk(pool, 20, Side::SELL, 5100, 30), aux);
        eng.on_order_received(mk(pool, 30, Side::SELL, 5000, 50, 2), aux);
        wal.close();
    }

    MemoryPool<Order> pool{512};
    OrderBook book{pool};
    RecoveryManager rm(kShard);
    const RecoveryResult res = rm.recover(dir.string(), {{kIid, &book, &pool}});
    ASSERT_EQ(res.status, RecoveryStatus::Ok) << res.detail;
    // Order 10 filled; order 20 sibling-cancelled — both absent post-replay,
    // aggressor 30 rested nothing (fully consumed).
    EXPECT_EQ(book.find_order(10), nullptr);
    EXPECT_EQ(book.find_order(20), nullptr);
    EXPECT_EQ(book.find_order(30), nullptr);
    EXPECT_EQ(book.live_orders(), 0u);
    EXPECT_TRUE(res.books[0].book_seq_verified);
}
