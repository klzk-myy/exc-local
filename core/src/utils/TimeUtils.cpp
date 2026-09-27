#include "utils/TimeUtils.hpp"

#include <ctime>

namespace exch {
namespace {

constexpr uint64_t to_ns(const timespec& ts) noexcept {
    return static_cast<uint64_t>(ts.tv_sec) * 1'000'000'000ull +
           static_cast<uint64_t>(ts.tv_nsec);
}

}  // namespace

uint64_t now_ns() noexcept {
    timespec ts{};
    if (::clock_gettime(CLOCK_REALTIME, &ts) != 0) return 0;
    return to_ns(ts);
}

uint64_t steady_ns() noexcept {
    timespec ts{};
    if (::clock_gettime(CLOCK_MONOTONIC, &ts) != 0) return 0;
    return to_ns(ts);
}

}  // namespace exch
