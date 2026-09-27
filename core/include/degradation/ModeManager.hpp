#pragma once

// PHASE-02 STUB (Task 2.3.6) — degradation FSM, spec §2.4. Canonical modes:
// Normal | ReadOnly | MarketDataOnly | SpotOnly | Throttled | Maintenance.
// Redis keys (system:degradation:mode/:entered_at/:reason), Aeron transition
// broadcast and 60s cooldown land in Phase-02; the HTTP X-Degradation-Mode
// header and WS system.status fan-out are Phase-05/06.

#include <cstddef>
#include <cstdint>
#include <string_view>

namespace exch {

enum class DegradationMode : uint8_t {
    Normal,
    ReadOnly,
    MarketDataOnly,
    SpotOnly,
    Throttled,
    Maintenance,
};

[[nodiscard]] std::string_view to_string(DegradationMode mode) noexcept;
// Ordering for trigger arbitration: Maintenance > MarketDataOnly > SpotOnly >
// ReadOnly > Throttled > Normal (spec §2.4).
[[nodiscard]] int severity(DegradationMode mode) noexcept;

class ModeManager {
public:
    ModeManager() noexcept = default;

    [[nodiscard]] DegradationMode mode() const noexcept { return mode_; }
    [[nodiscard]] std::string_view reason() const noexcept { return reason_; }

    // Stub: updates the local cache only.
    void set_mode(DegradationMode mode, std::string_view reason) noexcept;

private:
    static constexpr std::size_t kReasonCap = 64;
    DegradationMode mode_ = DegradationMode::Normal;
    char reason_[kReasonCap] = {};
};

}  // namespace exch
