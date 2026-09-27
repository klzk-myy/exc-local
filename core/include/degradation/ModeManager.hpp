#pragma once

// Task 2.3.6 — degradation ModeManager, spec §2.4.
//
// Canonical mode set (PascalCase, exact): Normal | ReadOnly | MarketDataOnly
// | SpotOnly | Throttled | Maintenance. Persistence lives in the coordination
// Redis under `system:degradation:{mode,entered_at,reason}` (spec §4.2), one
// atomic MSET per transition; a transition is also published on the injected
// transition sink — the IPC seam Phase-06 wires to the WS `system.status`
// fan-out (ModeEventPublisher adapts an IpcChannel to the sink signature).
//
// Transition policy (request_transition — the automatic path HealthChecker
// uses):
//   * escalation (higher severity) always applies — cooldown must never
//     delay a worse degradation (fail-closed, spec §2.7);
//   * de-escalation/recovery requires cooldown_ms (default 60,000 — plan DoD
//     "min 60s between transitions") since the last applied transition;
//   * stored Maintenance is a manual hold: the auto path never leaves it
//     (spec §2.4: recovery is "admin re-enables via /admin/maintenance/
//     disable"); set_mode() — the manual path — always applies.
//
// Read path (refresh/mode): the store is authoritative. A failed refresh is
// NOT silently treated as Normal — after `read_fail_threshold` (default 2)
// consecutive read failures the effective mode() reports Maintenance
// (fail-closed: an unverifiable degradation state must never read as
// healthy). stored_mode()/mode_stale() expose the last-known truth for
// observability; the overlay is not persisted.

#include <cstdint>
#include <string>
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
[[nodiscard]] bool mode_from_string(std::string_view s,
                                    DegradationMode* out) noexcept;
// Ordering for trigger arbitration: Maintenance > MarketDataOnly > SpotOnly >
// ReadOnly > Throttled > Normal (spec §2.4).
[[nodiscard]] int severity(DegradationMode mode) noexcept;

// What a store persists per transition. entered_at_ms is epoch millis
// (spec §4.2 "Epoch millis").
struct ModeRecord {
    DegradationMode mode = DegradationMode::Normal;
    int64_t entered_at_ms = 0;
    std::string reason;
};

// Persistence seam — production binding is RedisModeStore
// (src/degradation/RedisModeStore.cpp, atomic MSET); tests substitute an
// in-memory fake. false return = transport/store failure (fail-closed
// semantics decided by the caller).
class ModeStore {
public:
    virtual ~ModeStore() = default;
    virtual bool write(const ModeRecord& rec) noexcept = 0;
    // Absent keys MUST read as a Normal record (spec §2.4 default), not an
    // error; false is reserved for store failures.
    virtual bool read(ModeRecord* out) noexcept = 0;
};

class ModeManager {
public:
    ModeManager() noexcept = default;
    // Store-backed instance. `store` is non-owning.
    explicit ModeManager(ModeStore* store) noexcept;

    // Wall-clock milliseconds for entered_at/transition bookkeeping.
    // Default: system_clock. Tests inject a fake.
    using clock_fn = int64_t (*)(void* ctx) noexcept;
    void set_clock(clock_fn fn, void* ctx) noexcept;

    // Transition broadcast seam — WS `system.status` carrier. Signature
    // matches ModeEventPublisher::sink so an IPC channel plugs in directly.
    using transition_fn = void (*)(void* ctx, DegradationMode from,
                                   DegradationMode to, std::string_view reason,
                                   int64_t entered_at_ms) noexcept;
    void set_transition_sink(transition_fn fn, void* ctx) noexcept;

    void set_cooldown_ms(int64_t ms) noexcept { cooldown_ms_ = ms; }
    void set_read_fail_threshold(int n) noexcept {
        read_fail_threshold_ = n > 0 ? n : 1;
    }

    // Manual/direct transition (admin path): always applies — persists via
    // the store (when wired) and broadcasts on the sink. Returns the store
    // write result; local state is updated regardless (fail toward the
    // requested severity until the next authoritative refresh).
    bool set_mode(DegradationMode mode, std::string_view reason) noexcept;

    // Automatic transition (HealthChecker path): applies the arbitration
    // policy documented at the top — escalation bypasses cooldown,
    // de-escalation requires it, stored Maintenance is never left
    // automatically. true = the transition was applied.
    bool request_transition(DegradationMode mode,
                            std::string_view reason) noexcept;

    // Pull the authoritative record from the store into the local cache.
    // No store wired ⇒ trivially true (local-only scaffold). On failure the
    // consecutive-failure counter advances; see mode().
    bool refresh() noexcept;

    // Effective mode: the last-known stored mode, overridden to Maintenance
    // once read_fail_threshold consecutive store failures have occurred —
    // failed refresh()es and failed set_mode() writes both count
    // (fail-closed: an unverifiable degradation state must never imply
    // Normal, and a store that just ate a write is unverifiable).
    [[nodiscard]] DegradationMode mode() const noexcept {
        return store_fail_override() ? DegradationMode::Maintenance
                                     : stored_mode_;
    }
    // Last successfully known/written mode, ignoring the failure overlay.
    [[nodiscard]] DegradationMode stored_mode() const noexcept {
        return stored_mode_;
    }
    // true while at least one consecutive store operation has failed.
    [[nodiscard]] bool mode_stale() const noexcept {
        return consecutive_store_failures_ > 0;
    }
    [[nodiscard]] bool store_fail_override() const noexcept {
        return consecutive_store_failures_ >= read_fail_threshold_;
    }
    [[nodiscard]] int consecutive_store_failures() const noexcept {
        return consecutive_store_failures_;
    }
    [[nodiscard]] std::string_view reason() const noexcept { return reason_; }
    [[nodiscard]] int64_t entered_at_ms() const noexcept {
        return entered_at_ms_;
    }
    // Wall-ms of the last applied transition (drives the auto cooldown).
    [[nodiscard]] int64_t last_transition_ms() const noexcept {
        return last_transition_ms_;
    }
    [[nodiscard]] int64_t cooldown_ms() const noexcept { return cooldown_ms_; }

    // getMode() convenience: refresh() then effective mode() — the per-job
    // read the plan describes.
    DegradationMode get_mode() noexcept {
        refresh();
        return mode();
    }

private:
    int64_t now_ms() const noexcept;
    static int64_t default_clock(void* ctx) noexcept;
    void apply_local(DegradationMode mode, std::string_view reason,
                     int64_t entered_at_ms) noexcept;

    ModeStore* store_ = nullptr;
    clock_fn clock_ = default_clock;
    void* clock_ctx_ = nullptr;
    transition_fn sink_ = nullptr;
    void* sink_ctx_ = nullptr;
    int64_t cooldown_ms_ = 60'000;  // plan DoD: min 60s between transitions
    int read_fail_threshold_ = 2;   // fail-closed override after 2 failures

    DegradationMode stored_mode_ = DegradationMode::Normal;
    int64_t entered_at_ms_ = 0;
    int64_t last_transition_ms_ = 0;
    int consecutive_store_failures_ = 0;
    static constexpr size_t kReasonCap = 128;
    char reason_[kReasonCap] = {};
};

}  // namespace exch
