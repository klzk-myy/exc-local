#pragma once

// Nanosecond clocks (Task 1.3.1, spec §3.1).
// now_ns():    CLOCK_REALTIME — wall clock for order timestamps; PTP-disciplined
//              at the OS level (hardware PTP lands in Phase-09 Task 9.3.12).
// steady_ns(): CLOCK_MONOTONIC — interval/deadline measurement only.

#include <cstdint>

namespace exch {

[[nodiscard]] uint64_t now_ns() noexcept;
[[nodiscard]] uint64_t steady_ns() noexcept;

}  // namespace exch
