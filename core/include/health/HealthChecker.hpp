#pragma once

// Task 2.3.6 — HealthChecker: the 5s probe job that drives ModeManager
// auto-transitions (spec §2.4 triggers + §2.7.3 hysteresis).
//
// Probe set (all injectable as function pointers + ctx — zero std::function
// alloc; an unwired probe is "signal unavailable in this build", treated as
// healthy and skipped — the integrator wires what exists):
//
//   * matching_p50_us — engine rolling matching-thread p50, the §2.4
//     ReadOnly "core slow" self-probe (remediation #8): > 500µs triggers,
//     < 100µs sustained 30s recovers;
//   * redis_p50_us   — coordination-Redis round-trip p50; §2.7.3 "Redis
//     timeout >1s" feeds ReadOnly;
//   * postgres_ok    — connectivity probe (PG MCP ping behind the seam);
//   * wal_ok         — WAL integrity probe;
//   * workers_ok     — shard worker heartbeat rollup;
//   * derivatives_ok — derivatives-engine probe (Phase-22 wiring; default
//     healthy here);
//   * queue_depth    — capacity gauge; >250 sustained 30s ⇒ Throttled
//     (spec §2.4 "capacity >80%"), <187 (60%) sustained 5min recovers.
//
// Arbitration each evaluation: the highest-severity firing trigger wins
// (Maintenance > MarketDataOnly > SpotOnly > ReadOnly > Throttled > Normal —
// simultaneous triggers resolve by spec §2.4 priority). Escalation applies
// immediately; de-escalation requires (a) the current mode's recovery dwell
// — sustained clear telemetry — and (b) the ModeManager cooldown (60s) so
// flapping can't oscillate. Stored Maintenance is never auto-exited.
// A throwing probe is a failed probe: its signal reads unhealthy
// (fail-closed).

#include <atomic>
#include <cstdint>
#include <thread>

#include "degradation/ModeManager.hpp"

namespace exch {

struct HealthProbes {
    int64_t (*matching_p50_us)(void* ctx) = nullptr;
    void* matching_p50_ctx = nullptr;
    int64_t (*redis_p50_us)(void* ctx) = nullptr;
    void* redis_p50_ctx = nullptr;
    bool (*postgres_ok)(void* ctx) = nullptr;
    void* postgres_ctx = nullptr;
    bool (*wal_ok)(void* ctx) = nullptr;
    void* wal_ctx = nullptr;
    bool (*workers_ok)(void* ctx) = nullptr;
    void* workers_ctx = nullptr;
    bool (*derivatives_ok)(void* ctx) = nullptr;
    void* derivatives_ctx = nullptr;
    int64_t (*queue_depth)(void* ctx) = nullptr;
    void* queue_depth_ctx = nullptr;
};

struct HealthCheckerConfig {
    int64_t interval_ms = 5'000;       // §5s evaluation cadence (plan DoD)
    int64_t cooldown_ms = 60'000;      // kept in sync into ModeManager
    // ReadOnly ("core slow") — spec §2.4.
    int64_t matching_slow_us = 500;    // trigger: matching-loop p50 above
    int64_t matching_recover_us = 100; // recovery: p50 below, sustained
    int64_t read_only_recover_ms = 30'000;
    // Redis latency — spec §2.7.3 "Redis timeout >1s" feeds ReadOnly.
    int64_t redis_slow_us = 1'000'000;
    // Throttled — spec §2.4: depth > 250 for 30s; recover < 187 (60% of the
    // implied 312-slot watermark) for 5min.
    int64_t throttle_depth = 250;
    int64_t throttle_recover_depth = 187;
    int64_t throttle_trigger_ms = 30'000;
    int64_t throttle_recover_ms = 300'000;
    // MarketDataOnly / SpotOnly recovery dwell — spec §2.7.3 "30 consecutive
    // seconds of verified healthy telemetry".
    int64_t store_recover_ms = 30'000;
    int64_t deriv_recover_ms = 30'000;
};

struct HealthReport {
    // Probe booleans (kept from the Phase-02 stub; extended below).
    bool matching_loop_ok = true;
    bool redis_ok = true;
    bool postgres_ok = true;
    bool wal_ok = true;
    bool workers_ok = true;
    uint32_t queue_depth = 0;
    // Extension fields.
    int64_t matching_p50_us = 0;
    int64_t redis_p50_us = 0;
    bool derivatives_ok = true;
    bool throttle_triggered = false;
    DegradationMode current_mode = DegradationMode::Normal;
    DegradationMode target_mode = DegradationMode::Normal;
    bool transitioned = false;       // a transition was applied this check
    bool recovery_pending = false;   // de-escalation wanted but dwell/cooldown gated
    const char* reason = "";         // winning trigger (static storage)
};

class HealthChecker {
public:
    explicit HealthChecker(ModeManager& modes) noexcept;
    ~HealthChecker() noexcept;

    HealthChecker(const HealthChecker&) = delete;
    HealthChecker& operator=(const HealthChecker&) = delete;

    void set_config(const HealthCheckerConfig& cfg) noexcept;
    void set_probes(const HealthProbes& probes) noexcept { probes_ = probes; }
    // Monotonic milliseconds for dwell/cooldown bookkeeping. Default:
    // steady_clock. Tests inject a fake.
    using clock_fn = int64_t (*)(void* ctx) noexcept;
    void set_clock(clock_fn fn, void* ctx) noexcept;
    // Alert seam: P1 for MarketDataOnly/SpotOnly, P2 for ReadOnly/Throttled
    // (spec §2.4 Alerting), level 0 = info (recovery).
    using alert_fn = void (*)(void* ctx, int level, const char* code,
                              const char* detail) noexcept;
    void set_alert_sink(alert_fn fn, void* ctx) noexcept;

    // One evaluation: refresh the mode cache, run all probes, arbitrate,
    // apply/decline the transition. Called every interval_ms by run(), or
    // directly in tests.
    HealthReport check_once() noexcept;
    // Dedicated probe thread: check_once() every cfg.interval_ms.
    bool start();
    void stop() noexcept;
    [[nodiscard]] bool running() const noexcept { return runner_ != nullptr; }
    [[nodiscard]] HealthReport last_report() const noexcept {
        return last_report_;
    }
    [[nodiscard]] const HealthCheckerConfig& config() const noexcept {
        return cfg_;
    }

private:
    int64_t now_ms() const noexcept;
    void alert(int level, const char* code, const char* detail) noexcept;
    // Sustained-clear dwell required before leaving `mode` via the auto path.
    int64_t recovery_dwell_ms(DegradationMode mode) const noexcept;
    int pager_level(DegradationMode mode) const noexcept;

    static int64_t default_clock(void* ctx) noexcept;

    ModeManager& modes_;
    HealthCheckerConfig cfg_;
    HealthProbes probes_;
    clock_fn clock_ = default_clock;
    void* clock_ctx_ = nullptr;
    alert_fn alert_ = nullptr;
    void* alert_ctx_ = nullptr;

    // Dwell bookkeeping (mono ms; <0 = condition not currently satisfied —
    // 0 is a legal clock reading at process start, so the sentinel is -1).
    int64_t throttle_since_ = -1;     // depth continuously > trigger
    int64_t ro_clear_since_ = -1;     // ReadOnly conditions continuously clear
    int64_t thr_clear_since_ = -1;    // depth continuously <= recover
    int64_t store_clear_since_ = -1;  // pg && wal continuously healthy
    int64_t deriv_clear_since_ = -1;  // derivatives continuously healthy

    HealthReport last_report_{};

    // Runner thread (started via start(); null until then).
    struct Runner {
        std::atomic<bool> stop{false};
        std::thread th;
    };
    Runner* runner_ = nullptr;
};

}  // namespace exch
