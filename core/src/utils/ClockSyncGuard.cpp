#include "utils/ClockSyncGuard.hpp"

#include <sys/timex.h>

#include "utils/safe_math.hpp"

namespace exch {

ClockOffsetSample kernel_clock_offset() noexcept {
    timex tx{};
    if (::ntp_adjtime(&tx) != 0) {
        // Fail-closed: an unreadable discipline state counts as unsynced.
        return {.synced = false, .offset_ns = 0};
    }
    // timex.status STA_UNSYNC means no source is disciplining the clock;
    // timex.offset is nanoseconds when STA_NANO is set, microseconds
    // otherwise (adjtimex(2)).
    const bool synced = (tx.status & STA_UNSYNC) == 0;
    int64_t off = tx.offset;
    if ((tx.status & STA_NANO) == 0 &&
        !safe_math::try_mul(off, int64_t{1000}, off)) {
        off = INT64_MAX;  // an unrepresentable offset is out of bounds
    }
    return {.synced = synced, .offset_ns = off};
}

const char* clock_sync_result_name(ClockSyncResult r) noexcept {
    switch (r) {
        case ClockSyncResult::Ok: return "Ok";
        case ClockSyncResult::Unsynchronized: return "Unsynchronized";
        case ClockSyncResult::DriftExceeded: return "DriftExceeded";
    }
    return "?";
}

ClockSyncResult check_clock_sync(ClockOffsetSample s) noexcept {
    if (!s.synced) return ClockSyncResult::Unsynchronized;
    // Compare without abs() so INT64_MIN cannot UB.
    if (s.offset_ns > kMaxClockOffsetNs || s.offset_ns < -kMaxClockOffsetNs) {
        return ClockSyncResult::DriftExceeded;
    }
    return ClockSyncResult::Ok;
}

ClockSyncResult check_clock_sync(ClockOffsetProbe probe) noexcept {
    if (probe == nullptr) return ClockSyncResult::Unsynchronized;
    return check_clock_sync(probe());
}

ClockSyncResult check_clock_sync() noexcept {
    return check_clock_sync(kernel_clock_offset);
}

void require_clock_sync(ClockOffsetProbe probe) {
    if (check_clock_sync(probe) != ClockSyncResult::Ok) {
        throw TimeSyncLossHalt{};
    }
}

void require_clock_sync() {
    require_clock_sync(kernel_clock_offset);
}

bool clock_sync_healthy(ClockOffsetProbe probe) noexcept {
    return check_clock_sync(probe) == ClockSyncResult::Ok;
}

bool clock_sync_healthy() noexcept {
    return clock_sync_healthy(kernel_clock_offset);
}

}  // namespace exch
