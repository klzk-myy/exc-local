// Task 2.3.6 — degradation ModeManager, spec §2.4. See header for the
// transition policy (escalation bypasses cooldown, de-escalation requires it,
// stored Maintenance is a manual hold) and the fail-closed read semantics.

#include "degradation/ModeManager.hpp"

#include <chrono>
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

bool mode_from_string(std::string_view s, DegradationMode* out) noexcept {
    static constexpr std::pair<std::string_view, DegradationMode> kModes[] = {
        {"Normal", DegradationMode::Normal},
        {"ReadOnly", DegradationMode::ReadOnly},
        {"MarketDataOnly", DegradationMode::MarketDataOnly},
        {"SpotOnly", DegradationMode::SpotOnly},
        {"Throttled", DegradationMode::Throttled},
        {"Maintenance", DegradationMode::Maintenance},
    };
    for (const auto& [name, mode] : kModes) {
        if (s == name) {
            if (out != nullptr) *out = mode;
            return true;
        }
    }
    return false;
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

ModeManager::ModeManager(ModeStore* store) noexcept : store_(store) {}

int64_t ModeManager::default_clock(void*) noexcept {
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::system_clock::now().time_since_epoch())
        .count();
}

void ModeManager::set_clock(clock_fn fn, void* ctx) noexcept {
    clock_ = fn != nullptr ? fn : default_clock;
    clock_ctx_ = ctx;
}

void ModeManager::set_transition_sink(transition_fn fn, void* ctx) noexcept {
    sink_ = fn;
    sink_ctx_ = ctx;
}

int64_t ModeManager::now_ms() const noexcept {
    return clock_ != nullptr ? clock_(clock_ctx_) : default_clock(nullptr);
}

void ModeManager::apply_local(DegradationMode mode, std::string_view reason,
                              int64_t entered_at_ms) noexcept {
    stored_mode_ = mode;
    entered_at_ms_ = entered_at_ms;
    last_transition_ms_ = entered_at_ms;
    const size_t n = reason.size() < kReasonCap - 1 ? reason.size()
                                                    : kReasonCap - 1;
    std::memcpy(reason_, reason.data(), n);
    reason_[n] = '\0';
}

bool ModeManager::set_mode(DegradationMode mode,
                           std::string_view reason) noexcept {
    const DegradationMode from = stored_mode_;
    const int64_t entered = now_ms();
    bool persisted = true;
    if (store_ != nullptr) {
        ModeRecord rec;
        rec.mode = mode;
        rec.entered_at_ms = entered;
        rec.reason.assign(reason);
        persisted = store_->write(rec);  // atomic MSET in the Redis binding
    }
    // Local state advances even when persistence fails — the effective mode
    // must never be *less* degraded than an accepted request (fail toward
    // severity); the next successful refresh() re-syncs to the store.
    apply_local(mode, reason, entered);
    if (!persisted) consecutive_store_failures_ = read_fail_threshold_;
    if (sink_ != nullptr) {
        sink_(sink_ctx_, from, mode, reason, entered);
    }
    return persisted;
}

bool ModeManager::request_transition(DegradationMode mode,
                                     std::string_view reason) noexcept {
    const DegradationMode current = stored_mode_;
    if (mode == current) return true;  // already there — nothing to do
    // Spec §2.4: Maintenance exits only via the admin path
    // (/admin/maintenance/disable) — the auto FSM never leaves it.
    if (current == DegradationMode::Maintenance) return false;
    if (severity(mode) < severity(current)) {
        // De-escalation is the flapping axis — cooldown gates it. Escalation
        // is always allowed: delaying a worse degradation would violate
        // fail-closed pessimism (spec §2.7).
        if (now_ms() - last_transition_ms_ < cooldown_ms_) return false;
    }
    return set_mode(mode, reason);
}

bool ModeManager::refresh() noexcept {
    if (store_ == nullptr) return true;  // local-only scaffold (main.cpp)
    ModeRecord rec;
    if (!store_->read(&rec)) {
        ++consecutive_store_failures_;
        return false;
    }
    consecutive_store_failures_ = 0;
    // The store is authoritative: adopt its record (without touching
    // last_transition_ms_ — refresh is observation, not a transition).
    stored_mode_ = rec.mode;
    entered_at_ms_ = rec.entered_at_ms;
    const size_t n = rec.reason.size() < kReasonCap - 1 ? rec.reason.size()
                                                        : kReasonCap - 1;
    std::memcpy(reason_, rec.reason.data(), n);
    reason_[n] = '\0';
    return true;
}

}  // namespace exch
