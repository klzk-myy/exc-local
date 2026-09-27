#pragma once

// PTP/NTP clock-sync guard (Task 1.3.12). Spec §2.7.2 lists "PTP clock skew
// >100µs" as an L0 Critical/Fatal trigger and MiFID II RTS 25 requires
// demonstrable clock synchronization; the §23 code for a violation is
// TIME_SYNC_LOSS_HALT (HTTP 503). Two surfaces:
//
//   Startup gate  — require_clock_sync() throws TimeSyncLossHalt (L0) when
//                   the clock is unsynchronized or drift exceeds the bound;
//                   check_clock_sync() returns the tri-state for callers
//                   that map failures themselves.
//   Daemon health — clock_sync_healthy() is a noexcept bool the periodic
//                   health/watchdog probe (Phase-02 HealthChecker, Phase-09
//                   exchange-watchdogd) calls each cycle.
//
// The offset probe is injectable as a plain function pointer (no
// std::function → no allocation), so tests never depend on host NTP/PTP
// state. The default kernel_clock_offset() reads the kernel clock
// discipline via ntp_adjtime(2) — the same PLL that chrony/ntpd and
// phc2sys steer once hardware PTP lands (Phase-09 Task 9.3.12) — and
// reports STA_UNSYNC or a failing syscall as unsynchronized (fail-closed,
// spec §2.7.1).

#include <cstdint>

#include "utils/error_severity.hpp"

namespace exch {

// Hard drift bound per spec §2.7.2 / MiFID II RTS 25: |offset| > 100µs
// trips TIME_SYNC_LOSS_HALT.
inline constexpr int64_t kMaxClockOffsetNs = 100'000;  // 100µs

struct ClockOffsetSample {
    bool synced;        // clock is disciplined by a sync source
    int64_t offset_ns;  // signed offset of system clock vs sync reference, ns
};

// Probe signature: returns the current offset sample. Must be a plain
// function pointer (capture-less lambdas convert) — noexcept by convention.
using ClockOffsetProbe = ClockOffsetSample (*)() noexcept;

// Default probe: kernel clock discipline via ntp_adjtime(2). Never throws;
// a failed syscall or STA_UNSYNC yields synced=false.
[[nodiscard]] ClockOffsetSample kernel_clock_offset() noexcept;

enum class ClockSyncResult : uint8_t {
    Ok = 0,           // disciplined and |offset| ≤ kMaxClockOffsetNs
    Unsynchronized,   // no sync source discipline / probe failure
    DriftExceeded,    // |offset| > kMaxClockOffsetNs
};

[[nodiscard]] const char* clock_sync_result_name(ClockSyncResult r) noexcept;

// Pure policy check on a sample (and on a probe result).
[[nodiscard]] ClockSyncResult check_clock_sync(ClockOffsetSample s) noexcept;
[[nodiscard]] ClockSyncResult check_clock_sync(ClockOffsetProbe probe) noexcept;
[[nodiscard]] ClockSyncResult check_clock_sync() noexcept;  // kernel probe

// Startup gate: throws TimeSyncLossHalt (L0, TIME_SYNC_LOSS_HALT) unless the
// clock is synchronized within the bound. Call before the matching loop
// opens ingress; a thrown value must abort startup (spec §2.7.2 L0).
void require_clock_sync(ClockOffsetProbe probe);
void require_clock_sync();

// Daemon health-check surface: true only when synchronized within bound.
[[nodiscard]] bool clock_sync_healthy(ClockOffsetProbe probe) noexcept;
[[nodiscard]] bool clock_sync_healthy() noexcept;

}  // namespace exch
