// Task 1.3.12 — error handling primitives: safe_math checked arithmetic,
// MemoryPool high-watermark + capacity sentinel, L0–L3 severity hierarchy,
// and the PTP clock-sync guard (spec §2.7, §3.6, §24 #297).

#include <gtest/gtest.h>

#include <cstdint>
#include <exception>
#include <stdexcept>
#include <type_traits>

#include "book/Order.hpp"
#include "utils/CheckedMath.hpp"
#include "utils/ClockSyncGuard.hpp"
#include "utils/MemoryPool.hpp"
#include "utils/error_severity.hpp"
#include "utils/safe_math.hpp"

using namespace exch;

// --- Checked arithmetic (safe_math.hpp) -----------------------------------

TEST(SafeMath, TryAddDetectsOverflowAndUnderflow) {
    int64_t out = 0;
    EXPECT_TRUE(safe_math::try_add(int64_t{1}, int64_t{2}, out));
    EXPECT_EQ(out, 3);
    EXPECT_TRUE(safe_math::try_add(INT64_MAX, int64_t{0}, out));
    EXPECT_EQ(out, INT64_MAX);

    // Overflow/underflow: false return, `out` left unmodified.
    out = 42;
    EXPECT_FALSE(safe_math::try_add(INT64_MAX, int64_t{1}, out));
    EXPECT_FALSE(safe_math::try_add(INT64_MIN, int64_t{-1}, out));
    EXPECT_EQ(out, 42);
}

TEST(SafeMath, TrySubDetectsUnderflow) {
    int64_t out = 0;
    EXPECT_TRUE(safe_math::try_sub(int64_t{5}, int64_t{7}, out));
    EXPECT_EQ(out, -2);
    EXPECT_FALSE(safe_math::try_sub(INT64_MIN, int64_t{1}, out));
    EXPECT_FALSE(safe_math::try_sub(INT64_MAX, int64_t{-1}, out));
}

TEST(SafeMath, TryMulDetectsOverflow) {
    int64_t out = 0;
    EXPECT_TRUE(safe_math::try_mul(int64_t{1'000'000}, int64_t{1'000'000}, out));
    EXPECT_EQ(out, 1'000'000'000'000);
    EXPECT_FALSE(safe_math::try_mul(INT64_MAX, int64_t{2}, out));
    EXPECT_FALSE(safe_math::try_mul(INT64_MIN, int64_t{-1}, out));
    EXPECT_FALSE(safe_math::try_mul(INT64_MIN, int64_t{2}, out));
}

TEST(SafeMath, TryNegRejectsInt64Min) {
    int64_t out = 0;
    EXPECT_TRUE(safe_math::try_neg(INT64_MAX, out));
    EXPECT_EQ(out, -INT64_MAX);
    EXPECT_TRUE(safe_math::try_neg(INT64_MIN + 1, out));
    EXPECT_EQ(out, INT64_MAX);
    EXPECT_FALSE(safe_math::try_neg(INT64_MIN, out));
    // Unsigned negation underflows for any non-zero operand.
    uint64_t uout = 0;
    EXPECT_TRUE(safe_math::try_neg(uint64_t{0}, uout));
    EXPECT_FALSE(safe_math::try_neg(uint64_t{1}, uout));
}

TEST(SafeMath, UnsignedOps) {
    uint64_t out = 0;
    EXPECT_TRUE(safe_math::try_add(UINT64_MAX - 1, uint64_t{1}, out));
    EXPECT_EQ(out, UINT64_MAX);
    EXPECT_FALSE(safe_math::try_add(UINT64_MAX, uint64_t{1}, out));
    EXPECT_FALSE(safe_math::try_sub(uint64_t{0}, uint64_t{1}, out));
    EXPECT_FALSE(safe_math::try_mul(UINT64_MAX, uint64_t{2}, out));
}

TEST(SafeMath, Int128Ops) {
    // 128-bit intermediates carry Decimal multiply/divide (spec §3.6.2).
    safe_math::int128_t r = 0;
    const safe_math::int128_t i128_max =
        (static_cast<safe_math::int128_t>(INT64_MAX) << 64) |
        static_cast<safe_math::int128_t>(UINT64_MAX);
    const safe_math::int128_t i128_min = -i128_max - 1;

    EXPECT_TRUE(safe_math::try_mul(static_cast<safe_math::int128_t>(INT64_MAX),
                                   static_cast<safe_math::int128_t>(INT64_MAX), r));
    EXPECT_EQ(r, safe_math::mul_wide_i64(INT64_MAX, INT64_MAX));
    EXPECT_FALSE(safe_math::try_add(i128_max, safe_math::int128_t{1}, r));
    EXPECT_FALSE(safe_math::try_sub(i128_min, safe_math::int128_t{1}, r));

    int64_t narrow = 0;
    EXPECT_TRUE(safe_math::try_narrow_i128(safe_math::int128_t{INT64_MAX}, narrow));
    EXPECT_EQ(narrow, INT64_MAX);
    EXPECT_FALSE(safe_math::try_narrow_i128(i128_max, narrow));
    EXPECT_FALSE(safe_math::try_narrow_i128(i128_min, narrow));
    EXPECT_TRUE(safe_math::try_mul_i64(INT64_MAX - 1, int64_t{1}, narrow));
    EXPECT_FALSE(safe_math::try_mul_i64(INT64_MAX, int64_t{2}, narrow));
}

TEST(SafeMath, ThrowingApiCarriesSpecCode) {
    try {
        (void)safe_math::add(INT64_MAX, int64_t{1});
        FAIL() << "expected ArithmeticOverflowError";
    } catch (const safe_math::ArithmeticOverflowError& e) {
        EXPECT_STREQ(e.code(), "ARITHMETIC_OVERFLOW_DETECTED");
        EXPECT_EQ(e.severity(), Severity::L2);
        EXPECT_EQ(e.http_status(), 400);
    }
    // Backward-compatible with the Task 1.3.1 std::overflow_error contract.
    EXPECT_THROW((void)safe_math::mul(INT64_MIN, int64_t{-1}), std::overflow_error);
    EXPECT_THROW((void)safe_math::sub(INT64_MIN, int64_t{1}), std::overflow_error);
    EXPECT_THROW((void)safe_math::neg(INT64_MIN), std::overflow_error);
}

TEST(SafeMath, CheckedMathDelegatesToCanonicalFacade) {
    // exch::detail::checked_* (Task 1.3.1, used by Decimal.cpp) now route
    // through safe_math — same intrinsic implementation, same exception type.
    EXPECT_EQ(detail::checked_add(int64_t{1}, int64_t{2}), 3);
    EXPECT_THROW((void)detail::checked_add(INT64_MAX, int64_t{1}),
                 safe_math::ArithmeticOverflowError);
    EXPECT_THROW((void)detail::checked_mul(INT64_MAX, int64_t{2}),
                 std::overflow_error);
}

// --- L0–L3 severity hierarchy (error_severity.hpp) ------------------------

TEST(ErrorSeverity, TierOrderAndConstants) {
    EXPECT_EQ(SeverityL0, Severity::L0);
    EXPECT_EQ(SeverityL1, Severity::L1);
    EXPECT_EQ(SeverityL2, Severity::L2);
    EXPECT_EQ(SeverityL3, Severity::L3);
    EXPECT_LT(SeverityL0, SeverityL1);
    EXPECT_LT(SeverityL1, SeverityL2);
    EXPECT_LT(SeverityL2, SeverityL3);

    EXPECT_STREQ(severity_name(Severity::L0), "L0");
    EXPECT_STREQ(severity_name(Severity::L3), "L3");
    EXPECT_STREQ(severity_priority(Severity::L0), "P0");
    EXPECT_STREQ(severity_priority(Severity::L3), "P3");
}

TEST(ErrorSeverity, OnlyL0IsFatal) {
    EXPECT_TRUE(is_fatal(Severity::L0));
    EXPECT_FALSE(is_fatal(Severity::L1));
    EXPECT_FALSE(is_fatal(Severity::L2));
    EXPECT_FALSE(is_fatal(Severity::L3));
}

// --- MemoryPool bounds & sentinel (spec §3.6.1) ---------------------------

TEST(MemoryPoolBounds, HighWatermarkTracksPeakUsage) {
    MemoryPool<Order> pool(4);
    EXPECT_EQ(pool.high_watermark(), 0u);

    Order* a = pool.alloc();
    Order* b = pool.alloc();
    EXPECT_EQ(pool.high_watermark(), 2u);

    pool.free(a);
    Order* c = pool.alloc();  // size returns to 2 — watermark must not shrink
    EXPECT_EQ(pool.high_watermark(), 2u);
    pool.free(b);
    pool.free(c);

    Order* x[4];
    for (Order*& p : x) p = pool.alloc();
    EXPECT_EQ(pool.size(), 4u);
    EXPECT_EQ(pool.high_watermark(), 4u);

    pool.reset_high_watermark();
    EXPECT_EQ(pool.high_watermark(), 4u);  // re-based at current live count
    for (Order* p : x) pool.free(p);
    pool.reset_high_watermark();
    EXPECT_EQ(pool.high_watermark(), 0u);
}

TEST(MemoryPoolBounds, AllocOrThrowEmitsCapacityCode) {
    MemoryPool<Order> pool(2);
    EXPECT_NE(pool.alloc_or_throw(), nullptr);
    EXPECT_NE(pool.alloc_or_throw(), nullptr);

    try {
        (void)pool.alloc_or_throw();
        FAIL() << "expected OrderBookCapacityExceeded";
    } catch (const OrderBookCapacityExceeded& e) {
        EXPECT_STREQ(e.code(), "ORDER_BOOK_CAPACITY_EXCEEDED");
        EXPECT_EQ(e.http_status(), 503);
        EXPECT_EQ(e.severity(), Severity::L2);
        EXPECT_STREQ(e.what(), "order book capacity exceeded");
    }
    // noexcept alloc() surface unchanged: nullptr on exhaustion.
    EXPECT_EQ(pool.alloc(), nullptr);
    // Catchable through the base hierarchy.
    EXPECT_THROW((void)pool.alloc_or_throw(), std::exception);
}

TEST(MemoryPoolBounds, SentinelIsAllocationFree) {
    // The exception object stores only literal pointers — static_asserts pin
    // that down so a future edit cannot sneak a std::string member in.
    // (is_trivially_copyable can never hold for a std::exception subclass —
    // the vptr/destructor make it non-trivial on every ABI. Nothrow copy
    // construction is the real "no allocation on throw/copy" guarantee.)
    static_assert(std::is_nothrow_copy_constructible_v<OrderBookCapacityExceeded>);
    static_assert(std::is_nothrow_default_constructible_v<OrderBookCapacityExceeded>);
    static_assert(sizeof(OrderBookCapacityExceeded) <= 48);
    SUCCEED();
}

// --- PTP clock-sync guard (spec §2.7.2 L0, MiFID II RTS 25) ----------------

namespace {

// Injectable probes — plain function pointers, no captures, no allocation.
ClockOffsetSample probe_within_bound() noexcept { return {true, 42'000}; }
ClockOffsetSample probe_at_bound() noexcept { return {true, kMaxClockOffsetNs}; }
ClockOffsetSample probe_over_bound() noexcept { return {true, 100'001}; }
ClockOffsetSample probe_negative_over() noexcept { return {true, -150'000}; }
ClockOffsetSample probe_unsynced() noexcept { return {false, 0}; }

}  // namespace

TEST(ClockSyncGuard, WithinBoundIsOk) {
    EXPECT_EQ(check_clock_sync(probe_within_bound), ClockSyncResult::Ok);
    EXPECT_TRUE(clock_sync_healthy(probe_within_bound));
    EXPECT_NO_THROW(require_clock_sync(probe_within_bound));
}

TEST(ClockSyncGuard, Exactly100usIsOk) {
    // Spec: drift *greater than* 100µs trips the halt; the bound itself passes.
    EXPECT_EQ(check_clock_sync(probe_at_bound), ClockSyncResult::Ok);
}

TEST(ClockSyncGuard, DriftBeyond100usTripsHalt) {
    EXPECT_EQ(check_clock_sync(probe_over_bound), ClockSyncResult::DriftExceeded);
    EXPECT_EQ(check_clock_sync(probe_negative_over), ClockSyncResult::DriftExceeded);
    EXPECT_FALSE(clock_sync_healthy(probe_over_bound));

    try {
        require_clock_sync(probe_over_bound);
        FAIL() << "expected TimeSyncLossHalt";
    } catch (const TimeSyncLossHalt& e) {
        EXPECT_STREQ(e.code(), "TIME_SYNC_LOSS_HALT");
        EXPECT_EQ(e.severity(), Severity::L0);
        EXPECT_TRUE(is_fatal(e.severity()));
        EXPECT_EQ(e.http_status(), 503);
    }
}

TEST(ClockSyncGuard, UnsynchronizedFailsClosed) {
    EXPECT_EQ(check_clock_sync(probe_unsynced), ClockSyncResult::Unsynchronized);
    EXPECT_FALSE(clock_sync_healthy(probe_unsynced));
    EXPECT_THROW(require_clock_sync(probe_unsynced), TimeSyncLossHalt);
    EXPECT_EQ(check_clock_sync(ClockOffsetProbe{nullptr}),
              ClockSyncResult::Unsynchronized);
}

TEST(ClockSyncGuard, KernelProbeRuns) {
    // Environment-dependent: the dev host may or may not run NTP/PTP. The
    // contract under test is that the probe returns a structurally valid
    // sample and never throws — the guard is exercised deterministically via
    // the injected probes above.
    const ClockOffsetSample s = kernel_clock_offset();
    const ClockSyncResult r = check_clock_sync(s);
    EXPECT_TRUE(r == ClockSyncResult::Ok || r == ClockSyncResult::Unsynchronized ||
                r == ClockSyncResult::DriftExceeded);
}
