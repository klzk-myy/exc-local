#include <gtest/gtest.h>

#include <cstdint>
#include <ctime>

#include "utils/TimeUtils.hpp"

TEST(TimeUtils, NowNsIsWallClock) {
    const uint64_t now = exch::now_ns();
    ASSERT_NE(now, 0u);
    // Within 60s of gettimeofday-equivalent wall time.
    const uint64_t wall = static_cast<uint64_t>(std::time(nullptr)) * 1'000'000'000ull;
    const uint64_t skew = now > wall ? now - wall : wall - now;
    EXPECT_LT(skew, 60ull * 1'000'000'000ull);
}

TEST(TimeUtils, SteadyNsMonotonic1M) {
    uint64_t prev = exch::steady_ns();
    ASSERT_NE(prev, 0u);
    for (int i = 0; i < 1'000'000; ++i) {
        const uint64_t t = exch::steady_ns();
        ASSERT_GE(t, prev) << "non-monotonic at i=" << i;
        prev = t;
    }
}

TEST(TimeUtils, NowNsNonDecreasing1M) {
    uint64_t prev = exch::now_ns();
    for (int i = 0; i < 1'000'000; ++i) {
        const uint64_t t = exch::now_ns();
        ASSERT_GE(t, prev) << "realtime regressed at i=" << i;
        prev = t;
    }
}

TEST(TimeUtils, SteadyAdvances) {
    const uint64_t a = exch::steady_ns();
    const timespec req{0, 50'000};  // 50us
    ::nanosleep(&req, nullptr);
    const uint64_t b = exch::steady_ns();
    EXPECT_GT(b, a);
}
