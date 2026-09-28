// Task 2.3.10 — ExpiryScheduler tests (spec §5.4 TIF, §6.7 24/5 calendar).
//
// Deterministic contract under test: the scheduler is a pure function of the
// WAL event stream — registrations (on_order_accepted / on_order_done) and
// TIME_TICK stamps (on_time_tick). No system clock is consulted anywhere, so
// replaying the same WAL produces the identical expiry sequence.

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <map>
#include <vector>

#include "matching/ExpiryScheduler.hpp"

using exch::ExpiryScheduler;
using exch::TimeInForce;

namespace {

constexpr uint64_t NS = 1'000'000'000ull;
constexpr uint64_t MIN = 60ull * NS;
constexpr uint64_t HOUR = 60ull * MIN;
constexpr uint64_t DAY = 24ull * HOUR;

// days_from_civil (Howard Hinnant) — deterministic civil-date → day index.
int64_t days_from_civil(int64_t y, unsigned m, unsigned d) noexcept {
    y -= (m <= 2);
    const int64_t era = (y >= 0 ? y : y - 399) / 400;
    const unsigned yoe = static_cast<unsigned>(y - era * 400);
    const unsigned doy = (153 * (m + (m > 2 ? -3 : 9)) + 2) / 5 + d - 1;
    const unsigned doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    return era * 146097 + static_cast<int64_t>(doe) - 719468;
}

uint64_t utc_ns(int64_t y, unsigned mo, unsigned da, unsigned h, unsigned mi = 0,
                unsigned s = 0) noexcept {
    const int64_t secs = days_from_civil(y, mo, da) * 86400 + h * 3600 + mi * 60 + s;
    return static_cast<uint64_t>(secs) * NS;
}

constexpr uint8_t kGTC = static_cast<uint8_t>(TimeInForce::GTC);  // 0
constexpr uint8_t kIOC = static_cast<uint8_t>(TimeInForce::IOC);  // 1
constexpr uint8_t kFOK = static_cast<uint8_t>(TimeInForce::FOK);  // 2
constexpr uint8_t kGTD = static_cast<uint8_t>(TimeInForce::GTD);  // 3
constexpr uint8_t kDAY = static_cast<uint8_t>(TimeInForce::DAY);  // 4

// Week of 2024-01-01 (a Monday) — anchor points for the 24/5 calendar.
const uint64_t kMon1000 = utc_ns(2024, 1, 1, 10, 0);       // Mon 10:00 UTC
const uint64_t kMon2200 = utc_ns(2024, 1, 1, 22, 0);       // Mon 22:00 UTC
const uint64_t kTue2200 = utc_ns(2024, 1, 2, 22, 0);       // Tue 22:00 UTC
const uint64_t kFri2159 = utc_ns(2024, 1, 5, 21, 59);      // Fri 21:59 UTC
const uint64_t kFri2200 = utc_ns(2024, 1, 5, 22, 0);       // Fri 22:00 UTC (close)
const uint64_t kFri2300 = utc_ns(2024, 1, 5, 23, 0);       // Fri 23:00 UTC
const uint64_t kSat1200 = utc_ns(2024, 1, 6, 12, 0);       // Sat 12:00 UTC
const uint64_t kSun1200 = utc_ns(2024, 1, 7, 12, 0);       // Sun 12:00 UTC
const uint64_t kSun2050 = utc_ns(2024, 1, 7, 20, 50);      // Sun pre-open
const uint64_t kSun2130 = utc_ns(2024, 1, 7, 21, 30);      // Sun 21:30 (in-session)
const uint64_t kSun2200 = utc_ns(2024, 1, 7, 22, 0);       // Sun 22:00
const uint64_t kSun2230 = utc_ns(2024, 1, 7, 22, 30);      // Sun 22:30
const uint64_t kNextMon2200 = utc_ns(2024, 1, 8, 22, 0);   // Mon Jan 8 22:00
const uint64_t kNextFri2200 = utc_ns(2024, 1, 12, 22, 0);  // Fri Jan 12 22:00

// Drain all due at tick into a vector (cap large enough).
std::vector<uint64_t> drain(ExpiryScheduler& s, uint64_t tick, uint32_t cap = 1 << 16) {
    std::vector<uint64_t> out(cap);
    const uint32_t n = s.on_time_tick(tick, out.data(), cap);
    out.resize(n);
    return out;
}

}  // namespace

// --- GTD expiry semantics -----------------------------------------------------

TEST(ExpiryGtd, FiresAtExactExpiryNeverBefore) {
    ExpiryScheduler s(64);
    const uint64_t now = kMon1000;
    const uint64_t expiry = now + HOUR;
    s.on_order_accepted(42, kGTD, static_cast<int64_t>(expiry), now);
    EXPECT_EQ(s.size(), 1u);
    EXPECT_EQ(s.next_expiry_ns(), expiry);

    // Tick one ns before expiry: nothing.
    EXPECT_TRUE(drain(s, expiry - 1).empty());
    // Tick exactly at expiry: fires once.
    auto out = drain(s, expiry);
    ASSERT_EQ(out.size(), 1u);
    EXPECT_EQ(out[0], 42u);
    // Once-only: later ticks emit nothing.
    EXPECT_TRUE(drain(s, expiry + 1).empty());
    EXPECT_TRUE(drain(s, expiry + DAY).empty());
    EXPECT_EQ(s.size(), 0u);
    EXPECT_EQ(s.next_expiry_ns(), UINT64_MAX);
}

TEST(ExpiryGtd, PastExpiryFiresOnFirstTick) {
    ExpiryScheduler s(64);
    // Expiry already in the past at registration: fail-closed — the order
    // dies on the next tick rather than resting forever. Drain uses
    // expiry <= tick (inclusive), so a tick at the expiry itself fires.
    s.on_order_accepted(7, kGTD, static_cast<int64_t>(kMon1000), kMon1000 + HOUR);
    EXPECT_TRUE(drain(s, kMon1000 - 1).empty());  // tick before expiry: nothing
    auto out = drain(s, kMon1000);                // tick == expiry: fires
    ASSERT_EQ(out.size(), 1u);
    EXPECT_EQ(out[0], 7u);
}

TEST(ExpiryGtd, NonPositiveExpiryClampedToImmediate) {
    ExpiryScheduler s(64);
    s.on_order_accepted(1, kGTD, 0, kMon1000);   // zero expiry
    s.on_order_accepted(2, kGTD, -5, kMon1000);  // negative expiry
    auto out = drain(s, kMon1000);
    ASSERT_EQ(out.size(), 2u);
    EXPECT_EQ(out[0], 1u);
    EXPECT_EQ(out[1], 2u);
}

TEST(ExpiryTif, NonExpiringTifsAreNoops) {
    ExpiryScheduler s(64);
    s.on_order_accepted(1, kGTC, 0, kMon1000);
    s.on_order_accepted(2, kIOC, 0, kMon1000);
    s.on_order_accepted(3, kFOK, 0, kMon1000);
    EXPECT_EQ(s.size(), 0u);
    EXPECT_EQ(s.next_expiry_ns(), UINT64_MAX);
    EXPECT_TRUE(drain(s, kMon1000 + 30 * DAY).empty());
}

// --- DAY expiry rule ----------------------------------------------------------
// Rule implemented (documented in ExpiryScheduler.hpp, derived from spec §6.7
// "DAY orders: expire at 22:00 UTC Friday (end of trading day)" + the
// intra-week 22:00 UTC New-York-close day boundary):
//   * accepted in-session → next in-session 22:00 UTC boundary > accept time;
//   * accepted in the weekend gap [Fri 22:00, Sun 21:00) incl. pre-open →
//     the upcoming week's Friday 22:00 UTC close.

TEST(ExpiryDay, WeekdayAcceptExpiresSameDayClose) {
    // Mon 10:00 UTC accept → Mon 22:00 UTC expiry (the trading day ends at
    // the New York close, not at midnight).
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kMon1000), kMon2200);
    // Fri 21:59 → Fri 22:00 (one minute later, the weekly close).
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kFri2159), kFri2200);

    ExpiryScheduler s(16);
    s.on_order_accepted(1, kDAY, 0, kMon1000);
    EXPECT_EQ(s.pending_expiry_of(1), kMon2200);
    EXPECT_TRUE(drain(s, kMon2200 - 1).empty());
    auto out = drain(s, kMon2200);
    ASSERT_EQ(out.size(), 1u);
    EXPECT_EQ(out[0], 1u);
}

TEST(ExpiryDay, LateEveningAcceptRollsToNextDayBoundary) {
    // Accept at/after today's 22:00 boundary while the session is open →
    // the order belongs to the next trading day.
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kMon2200), kTue2200);  // == boundary
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kMon2200 + 59 * MIN), kTue2200);
    // Sun in-session accepts use the same rule: 21:30 → Sun 22:00 (same-day),
    // 22:30 → Mon 22:00 (next boundary).
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kSun2130), kSun2200);
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kSun2230), kNextMon2200);
    // Thu 23:00 → Fri 22:00 (the last boundary of the week).
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(utc_ns(2024, 1, 4, 23, 0)), kFri2200);
}

TEST(ExpiryDay, WeekendGapAcceptExpiresNextFridayClose) {
    // Spec §6.7 + Phase-02 AC: "orders spanning weekend close expire Friday
    // 22:00 UTC". An order accepted between the Friday close and the Sunday
    // open (incl. the 20:45–21:00 pre-open) joins the upcoming trading week
    // and expires at that week's Friday 22:00 UTC close.
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kFri2200), kNextFri2200);  // at close
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kFri2300), kNextFri2200);  // Fri 23:00
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kSat1200), kNextFri2200);  // Saturday
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kSun1200), kNextFri2200);  // Sun noon
    EXPECT_EQ(ExpiryScheduler::day_expiry_ns(kSun2050), kNextFri2200);  // pre-open

    // End-to-end: a DAY order accepted Fri 23:00 rests over the weekend and
    // dies at the NEXT Friday close — not at any intervening 22:00 boundary.
    ExpiryScheduler s(16);
    s.on_order_accepted(9, kDAY, 0, kFri2300);
    EXPECT_EQ(s.pending_expiry_of(9), kNextFri2200);
    EXPECT_TRUE(drain(s, kSun2200).empty());      // survives Sunday boundary
    EXPECT_TRUE(drain(s, kNextMon2200).empty());  // survives Monday boundary
    EXPECT_TRUE(drain(s, kNextFri2200 - 1).empty());
    auto out = drain(s, kNextFri2200);
    ASSERT_EQ(out.size(), 1u);
    EXPECT_EQ(out[0], 9u);
}

TEST(ExpiryDay, SessionOpenPredicate) {
    EXPECT_TRUE(ExpiryScheduler::is_session_open(kMon1000));
    EXPECT_TRUE(ExpiryScheduler::is_session_open(kSun2130));
    EXPECT_TRUE(ExpiryScheduler::is_session_open(kFri2200 - 1));
    EXPECT_FALSE(ExpiryScheduler::is_session_open(kFri2200));  // at close
    EXPECT_FALSE(ExpiryScheduler::is_session_open(kSat1200));
    EXPECT_FALSE(ExpiryScheduler::is_session_open(kSun2050));  // pre-open
    EXPECT_FALSE(ExpiryScheduler::is_session_open(kSun1200));
}

// --- Heap ordering / drain semantics -------------------------------------------

TEST(ExpiryDrain, MultipleExpiriesSameTickEmittedSorted) {
    ExpiryScheduler s(64);
    const uint64_t base = kMon1000;
    // Register out of order; emission must be ascending (expiry, order_id).
    s.on_order_accepted(50, kGTD, static_cast<int64_t>(base + 3 * MIN), base);
    s.on_order_accepted(30, kGTD, static_cast<int64_t>(base + 1 * MIN), base);
    s.on_order_accepted(20, kGTD, static_cast<int64_t>(base + 1 * MIN), base);
    s.on_order_accepted(40, kGTD, static_cast<int64_t>(base + 2 * MIN), base);
    s.on_order_accepted(10, kGTD, static_cast<int64_t>(base + 1 * MIN), base);

    auto out = drain(s, base + 3 * MIN);
    // Expected order: expiry then order_id — (t+1m:10,20,30), (t+2m:40), (t+3m:50).
    const std::vector<uint64_t> want{10, 20, 30, 40, 50};
    EXPECT_EQ(out, want);
}

TEST(ExpiryDrain, StaggeredTicksFireInOrder) {
    ExpiryScheduler s(64);
    const uint64_t base = kMon1000;
    s.on_order_accepted(1, kGTD, static_cast<int64_t>(base + 3 * HOUR), base);
    s.on_order_accepted(2, kGTD, static_cast<int64_t>(base + 1 * HOUR), base);
    s.on_order_accepted(3, kGTD, static_cast<int64_t>(base + 2 * HOUR), base);
    EXPECT_EQ(s.next_expiry_ns(), base + 1 * HOUR);

    EXPECT_EQ(drain(s, base + 90 * MIN), std::vector<uint64_t>{2});
    EXPECT_EQ(s.next_expiry_ns(), base + 2 * HOUR);
    EXPECT_EQ(drain(s, base + 150 * MIN), std::vector<uint64_t>{3});
    EXPECT_EQ(s.next_expiry_ns(), base + 3 * HOUR);
    EXPECT_EQ(drain(s, base + 4 * HOUR), std::vector<uint64_t>{1});
}

TEST(ExpiryDrain, OutCapRespectedRemainderNextTick) {
    ExpiryScheduler s(64);
    const uint64_t base = kMon1000;
    s.on_order_accepted(1, kGTD, static_cast<int64_t>(base), base);
    s.on_order_accepted(2, kGTD, static_cast<int64_t>(base), base);
    s.on_order_accepted(3, kGTD, static_cast<int64_t>(base), base);

    uint64_t out[2];
    // cap=2 with 3 due → earliest two now; the third stays pending.
    EXPECT_EQ(s.on_time_tick(base, out, 2), 2u);
    EXPECT_EQ(out[0], 1u);
    EXPECT_EQ(out[1], 2u);
    EXPECT_EQ(s.size(), 1u);
    EXPECT_EQ(s.on_time_tick(base, out, 2), 1u);
    EXPECT_EQ(out[0], 3u);
    EXPECT_EQ(s.size(), 0u);
    // cap=0 / nullptr emit nothing and remove nothing.
    s.on_order_accepted(4, kGTD, static_cast<int64_t>(base), base);
    EXPECT_EQ(s.on_time_tick(base, nullptr, 8), 0u);
    EXPECT_EQ(s.on_time_tick(base, out, 0), 0u);
    EXPECT_EQ(s.size(), 1u);
}

// --- Registration lifecycle ----------------------------------------------------

TEST(ExpiryLifecycle, OrderDoneRemovesPending) {
    ExpiryScheduler s(64);
    const uint64_t base = kMon1000;
    s.on_order_accepted(1, kGTD, static_cast<int64_t>(base + HOUR), base);
    s.on_order_accepted(2, kGTD, static_cast<int64_t>(base + HOUR), base);
    s.on_order_done(1);
    EXPECT_EQ(s.size(), 1u);
    auto out = drain(s, base + 2 * HOUR);
    ASSERT_EQ(out.size(), 1u);
    EXPECT_EQ(out[0], 2u);
    // Done on unknown / already-removed ids is a no-op.
    s.on_order_done(1);
    s.on_order_done(999);
    EXPECT_EQ(s.size(), 0u);
}

TEST(ExpiryLifecycle, ReacceptResetsExpiry) {
    // Amend path (spec §6.6a "GTD timers reset on any accepted amend"):
    // a second on_order_accepted for the same id replaces the old expiry.
    ExpiryScheduler s(64);
    const uint64_t base = kMon1000;
    s.on_order_accepted(5, kGTD, static_cast<int64_t>(base + 1 * HOUR), base);
    s.on_order_accepted(5, kGTD, static_cast<int64_t>(base + 3 * HOUR), base);
    EXPECT_EQ(s.size(), 1u);
    EXPECT_EQ(s.pending_expiry_of(5), base + 3 * HOUR);
    EXPECT_TRUE(drain(s, base + 2 * HOUR).empty());
    EXPECT_EQ(drain(s, base + 3 * HOUR), std::vector<uint64_t>{5});
}

TEST(ExpiryLifecycle, CapacityOverflowCounted) {
    ExpiryScheduler s(2);  // deliberately tiny pending bound
    const uint64_t base = kMon1000;
    s.on_order_accepted(1, kGTD, static_cast<int64_t>(base + HOUR), base);
    s.on_order_accepted(2, kGTD, static_cast<int64_t>(base + HOUR), base);
    s.on_order_accepted(3, kGTD, static_cast<int64_t>(base + HOUR), base);
    EXPECT_EQ(s.size(), 2u);
    EXPECT_EQ(s.register_overflows(), 1u);
    // Registered orders still expire correctly.
    auto out = drain(s, base + HOUR);
    ASSERT_EQ(out.size(), 2u);
}

// --- Snapshot / restore (replay determinism) -------------------------------------

TEST(ExpirySnapshot, RoundTripPreservesPendingSetAndFireOrder) {
    ExpiryScheduler s(128);
    const uint64_t base = kMon1000;
    // Mixed registrations incl. equal expiries + a removal before snapshot.
    s.on_order_accepted(11, kGTD, static_cast<int64_t>(base + 5 * HOUR), base);
    s.on_order_accepted(22, kGTD, static_cast<int64_t>(base + 1 * HOUR), base);
    s.on_order_accepted(33, kGTD, static_cast<int64_t>(base + 1 * HOUR), base);
    s.on_order_accepted(44, kDAY, 0, kMon1000);  // → Mon 22:00
    s.on_order_accepted(55, kDAY, 0, kFri2300);  // → next Fri 22:00
    s.on_order_accepted(66, kGTD, static_cast<int64_t>(base + 9 * HOUR), base);
    s.on_order_done(66);  // removed before snapshot — must not appear

    std::vector<uint8_t> img(s.snapshot_bytes_needed());
    ASSERT_EQ(s.snapshot_pending(img.data(), static_cast<uint32_t>(img.size())),
              static_cast<uint32_t>(img.size()));

    ExpiryScheduler r(128);
    ASSERT_TRUE(r.restore_pending(img.data(), static_cast<uint32_t>(img.size())));
    EXPECT_EQ(r.size(), 5u);
    EXPECT_EQ(r.pending_expiry_of(44), kMon2200);
    EXPECT_EQ(r.pending_expiry_of(55), kNextFri2200);
    EXPECT_EQ(r.pending_expiry_of(66), UINT64_MAX);

    // Identical drain sequence across an identical tick schedule.
    const uint64_t ticks[] = {base + 2 * HOUR, kMon2200, kNextFri2200};
    for (uint64_t t : ticks) {
        EXPECT_EQ(drain(s, t), drain(r, t));
    }
    EXPECT_EQ(s.size(), 0u);
    EXPECT_EQ(r.size(), 0u);

    // Snapshot of the restored state must be byte-identical (canonical form).
    ExpiryScheduler s2(128);
    s2.restore_pending(img.data(), static_cast<uint32_t>(img.size()));
    std::vector<uint8_t> img2(s2.snapshot_bytes_needed());
    s2.snapshot_pending(img2.data(), static_cast<uint32_t>(img2.size()));
    EXPECT_EQ(img, img2);
}

TEST(ExpirySnapshot, RestoreRejectsCorruptInput) {
    ExpiryScheduler s(4);
    const uint64_t base = kMon1000;
    s.on_order_accepted(1, kGTD, static_cast<int64_t>(base + HOUR), base);
    std::vector<uint8_t> img(s.snapshot_bytes_needed());
    ASSERT_GT(s.snapshot_pending(img.data(), static_cast<uint32_t>(img.size())), 0u);

    ExpiryScheduler r(4);
    EXPECT_FALSE(r.restore_pending(nullptr, 0));
    EXPECT_FALSE(r.restore_pending(img.data(), 10));  // truncated header
    EXPECT_FALSE(r.restore_pending(img.data(), static_cast<uint32_t>(img.size() - 1)));
    EXPECT_EQ(r.size(), 0u);  // failed restore leaves an empty scheduler

    img[0] ^= 0xFF;  // bad magic
    EXPECT_FALSE(r.restore_pending(img.data(), static_cast<uint32_t>(img.size())));
}

TEST(ExpirySnapshot, BufferTooSmallWritesNothing) {
    ExpiryScheduler s(4);
    const uint64_t base = kMon1000;
    s.on_order_accepted(1, kGTD, static_cast<int64_t>(base + HOUR), base);
    std::vector<uint8_t> img(s.snapshot_bytes_needed() - 1, 0xAA);
    EXPECT_EQ(s.snapshot_pending(img.data(), static_cast<uint32_t>(img.size())), 0u);
    EXPECT_EQ(s.snapshot_pending(nullptr, 1 << 20), 0u);
}

// --- Deterministic tick-cadence simulation ---------------------------------------

TEST(ExpirySimulation, ThousandOrders100msTicks) {
    // 1000 GTD orders with pseudo-random expiries (deterministic LCG — no
    // <random> dependence), TIME_TICKs every 100 ms. Asserts: every order
    // fires exactly once, no order fires before its expiry, none is missed.
    ExpiryScheduler s(4096);
    const uint64_t t0 = kMon1000;
    const uint64_t tick_step = 100ull * 1'000'000ull;  // 100 ms
    const uint64_t span = 10 * MIN;

    uint64_t rng = 0x243F6A8885A308D3ull;
    auto next_rng = [&rng]() noexcept {
        rng = rng * 6364136223846793005ull + 1442695040888963407ull;
        return rng >> 11;
    };

    constexpr int kOrders = 1000;
    std::vector<uint64_t> expiry(kOrders);
    for (int i = 0; i < kOrders; ++i) {
        expiry[i] = t0 + 1 + (next_rng() % span);
        s.on_order_accepted(static_cast<uint64_t>(i + 1), kGTD, static_cast<int64_t>(expiry[i]),
                            t0);
    }
    EXPECT_EQ(s.size(), static_cast<std::size_t>(kOrders));

    std::vector<uint64_t> emitted_at(kOrders + 1, 0);
    std::vector<int> fired(kOrders + 1, 0);
    uint64_t buf[64];
    for (uint64_t tick = t0; tick <= t0 + span; tick += tick_step) {
        for (;;) {
            const uint32_t n = s.on_time_tick(tick, buf, 64);
            for (uint32_t k = 0; k < n; ++k) {
                const uint64_t id = buf[k];
                ASSERT_GE(id, 1u);
                ASSERT_LE(id, static_cast<uint64_t>(kOrders));
                ASSERT_EQ(fired[id], 0) << "order " << id << " fired twice";
                // Never early: expiry <= tick that emitted it.
                ASSERT_LE(expiry[id - 1], tick) << "order " << id << " fired early";
                fired[id] = 1;
                emitted_at[id] = tick;
            }
            if (n < 64) break;  // drained everything due at this tick
        }
    }

    int total = 0;
    for (int i = 1; i <= kOrders; ++i) total += fired[i];
    EXPECT_EQ(total, kOrders);  // none missed
    EXPECT_EQ(s.size(), 0u);
    EXPECT_EQ(s.next_expiry_ns(), UINT64_MAX);
}

// --- Model check: randomized ops vs std::map reference --------------------------

TEST(ExpiryModel, RandomizedOpsMatchReference) {
    // Interleaved register / done / tick against a reference model
    // (order_id → expiry map). Verifies pending-set equality, drain order
    // (ascending (expiry, order_id)) and exactly-once emission under churn —
    // exercises the open-addressed index's probe chains + heap sift paths.
    ExpiryScheduler s(1024);
    std::map<uint64_t, uint64_t> ref;  // order_id → expiry_ns
    const uint64_t base = kMon1000;
    uint64_t rng = 0xDEADBEEFCAFEF00Dull;
    auto next_rng = [&rng]() noexcept {
        rng ^= rng << 13;
        rng ^= rng >> 7;
        rng ^= rng << 17;
        return rng;
    };

    uint64_t clock = base;
    for (int step = 0; step < 20000; ++step) {
        const uint64_t r = next_rng();
        switch (r % 4) {
            case 0: {  // register (also re-registers: amend-reset path)
                const uint64_t id = 1 + (next_rng() % 300);
                const uint64_t exp = clock + 1 + (next_rng() % (2 * HOUR));
                s.on_order_accepted(id, kGTD, static_cast<int64_t>(exp), clock);
                ref[id] = exp;
                break;
            }
            case 1: {  // done — mix of live and absent ids
                const uint64_t id = 1 + (next_rng() % 300);
                s.on_order_done(id);
                ref.erase(id);
                break;
            }
            default: {                                          // advance clock and drain
                clock += 1 + (next_rng() % (30 * 60ull * NS));  // up to +30min
                uint64_t out[64];
                std::pair<uint64_t, uint64_t> last{0, 0};  // (expiry, id)
                for (;;) {
                    const uint32_t n = s.on_time_tick(clock, out, 64);
                    for (uint32_t k = 0; k < n; ++k) {
                        auto it = ref.find(out[k]);
                        ASSERT_NE(it, ref.end()) << "phantom expiry " << out[k];
                        ASSERT_LE(it->second, clock) << "early expiry";
                        // Emission is ascending (expiry_ns, order_id).
                        const std::pair<uint64_t, uint64_t> key{it->second, it->first};
                        ASSERT_GE(key, last) << "drain order violated";
                        last = key;
                        ref.erase(it);
                    }
                    if (n < 64) break;
                }
                break;
            }
        }
        // Invariant after every op: sizes agree, min-expiry agrees.
        ASSERT_EQ(s.size(), ref.size());
        if (ref.empty()) {
            EXPECT_EQ(s.next_expiry_ns(), UINT64_MAX);
        } else {
            uint64_t mn = UINT64_MAX;
            for (const auto& kv : ref) mn = std::min(mn, kv.second);
            EXPECT_EQ(s.next_expiry_ns(), mn);
        }
    }
    // Final drain: emitted sequence == ascending (expiry, order_id) of the
    // remaining model set.
    std::vector<std::pair<uint64_t, uint64_t>> pending(ref.size());
    std::transform(ref.begin(), ref.end(), pending.begin(),
                   [](const auto& kv) { return std::make_pair(kv.second, kv.first); });
    std::sort(pending.begin(), pending.end());
    auto out = drain(s, clock + 10 * DAY);
    ASSERT_EQ(out.size(), pending.size());
    for (std::size_t i = 0; i < out.size(); ++i) EXPECT_EQ(out[i], pending[i].second);
}

// --- Determinism: identical WAL sequence → identical drain -----------------------

TEST(ExpiryDeterminism, ReplayProducesIdenticalSequence) {
    // Two schedulers fed the same registration + tick sequence must emit
    // byte-identical expiry sequences — the WAL-replay guarantee.
    ExpiryScheduler a(256), b(256);
    const uint64_t base = kFri2300;  // weekend — exercises DAY week math
    for (uint64_t id = 1; id <= 200; ++id) {
        const uint64_t off = (id * 7919) % (7 * DAY);  // spread across the week
        a.on_order_accepted(id, (id % 2) ? kGTD : kDAY, static_cast<int64_t>(base + off), base);
        b.on_order_accepted(id, (id % 2) ? kGTD : kDAY, static_cast<int64_t>(base + off), base);
        if (id % 7 == 0) {
            a.on_order_done(id);
            b.on_order_done(id);
        }
    }
    for (uint64_t tick = base; tick <= base + 8 * DAY; tick += 37 * MIN) {
        uint64_t oa[16], ob[16];
        const uint32_t na = a.on_time_tick(tick, oa, 16);
        const uint32_t nb = b.on_time_tick(tick, ob, 16);
        ASSERT_EQ(na, nb);
        for (uint32_t i = 0; i < na; ++i) EXPECT_EQ(oa[i], ob[i]);
    }
    EXPECT_EQ(a.size(), 0u);
}
