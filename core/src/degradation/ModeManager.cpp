// PHASE-02 STUB (Task 2.3.6) — Redis persistence + broadcast land there.
#include "degradation/ModeManager.hpp"

#include <cstring>

namespace exch {

std::string_view to_string(DegradationMode mode) noexcept {
    switch (mode) {
        case DegradationMode::Normal:         return "Normal";
        case DegradationMode::ReadOnly:       return "ReadOnly";
        case DegradationMode::MarketDataOnly: return "MarketDataOnly";
        case DegradationMode::SpotOnly:       return "SpotOnly";
        case DegradationMode::Throttled:      return "Throttled";
        case DegradationMode::Maintenance:    return "Maintenance";
    }
    return "Normal";
}

int severity(DegradationMode mode) noexcept {
    switch (mode) {
        case DegradationMode::Maintenance:    return 5;
        case DegradationMode::MarketDataOnly: return 4;
        case DegradationMode::SpotOnly:       return 3;
        case DegradationMode::ReadOnly:       return 2;
        case DegradationMode::Throttled:      return 1;
        case DegradationMode::Normal:         return 0;
    }
    return 0;
}

void ModeManager::set_mode(DegradationMode mode, std::string_view reason) noexcept {
    mode_ = mode;
    const std::size_t n = reason.size() < kReasonCap - 1 ? reason.size() : kReasonCap - 1;
    std::memcpy(reason_, reason.data(), n);
    reason_[n] = '\0';
}

}  // namespace exch
