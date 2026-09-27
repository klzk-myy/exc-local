// Task 2.3.6 — HealthChecker: 5s probe job driving ModeManager
// auto-transitions. Trigger/recovery semantics are spec §2.4 + §2.7.3; see
// header for the arbitration rules.

#include "health/HealthChecker.hpp"

#include <chrono>
#include <cstdio>

namespace exch {
namespace {

// A probe that throws is a failed probe (fail-closed): its signal reads
// unhealthy. Probes are function pointers so noexcept-ness is a convention,
// not a guarantee — catch defensively.
template <typename F, typename T>
T probe_or(F* fn, void* ctx, T fallback) noexcept {
    if (fn == nullptr) return fallback;
    try {
        return fn(ctx);
    } catch (...) {
        return fallback;
    }
}

}  // namespace

HealthChecker::HealthChecker(ModeManager& modes) noexcept : modes_(modes) {}

HealthChecker::~HealthChecker() noexcept {
    try {
        stop();
    } catch (...) {
    }
}

int64_t HealthChecker::default_clock(void*) noexcept {
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::steady_clock::now().time_since_epoch())
        .count();
}

void HealthChecker::set_config(const HealthCheckerConfig& cfg) noexcept {
    cfg_ = cfg;
    // Cooldown lives in ModeManager (single transition gate for manual and
    // auto paths); keep the two views consistent.
    modes_.set_cooldown_ms(cfg.cooldown_ms);
}

void HealthChecker::set_clock(clock_fn fn, void* ctx) noexcept {
    clock_ = fn != nullptr ? fn : default_clock;
    clock_ctx_ = ctx;
}

void HealthChecker::set_alert_sink(alert_fn fn, void* ctx) noexcept {
    alert_ = fn;
    alert_ctx_ = ctx;
}

int64_t HealthChecker::now_ms() const noexcept {
    return clock_ != nullptr ? clock_(clock_ctx_) : default_clock(nullptr);
}

void HealthChecker::alert(int level, const char* code,
                          const char* detail) noexcept {
    if (alert_ != nullptr) alert_(alert_ctx_, level, code, detail);
}

int64_t HealthChecker::recovery_dwell_ms(DegradationMode mode) const noexcept {
    switch (mode) {
        case DegradationMode::ReadOnly:       return cfg_.read_only_recover_ms;
        case DegradationMode::Throttled:      return cfg_.throttle_recover_ms;
        case DegradationMode::MarketDataOnly: return cfg_.store_recover_ms;
        case DegradationMode::SpotOnly:       return cfg_.deriv_recover_ms;
        default:                              return 0;
    }
}

int HealthChecker::pager_level(DegradationMode mode) const noexcept {
    switch (mode) {
        case DegradationMode::MarketDataOnly:
        case DegradationMode::SpotOnly:
            return 1;  // P1 (spec §2.4 Alerting)
        case DegradationMode::ReadOnly:
        case DegradationMode::Throttled:
            return 2;  // P2
        default:
            return 0;
    }
}

HealthReport HealthChecker::check_once() noexcept {
    const int64_t now = now_ms();
    HealthReport rep;

    // Refresh the local view of the authoritative record first — a store
    // failure trips the ModeManager fail-closed overlay (mode() ==
    // Maintenance after the threshold) without a committed transition.
    modes_.refresh();

    // --- probes (unwired ⇒ signal unavailable ⇒ treated healthy) -----------
    rep.matching_p50_us =
        probe_or(probes_.matching_p50_us, probes_.matching_p50_ctx,
                 int64_t{0});
    rep.redis_p50_us =
        probe_or(probes_.redis_p50_us, probes_.redis_p50_ctx, int64_t{0});
    rep.postgres_ok =
        probe_or(probes_.postgres_ok, probes_.postgres_ctx, true);
    rep.wal_ok = probe_or(probes_.wal_ok, probes_.wal_ctx, true);
    rep.workers_ok = probe_or(probes_.workers_ok, probes_.workers_ctx, true);
    rep.derivatives_ok =
        probe_or(probes_.derivatives_ok, probes_.derivatives_ctx, true);
    const int64_t depth =
        probe_or(probes_.queue_depth, probes_.queue_depth_ctx, int64_t{0});
    rep.queue_depth =
        static_cast<uint32_t>(depth < 0 ? 0 : depth);

    // Derived health booleans.
    rep.matching_loop_ok =
        rep.matching_p50_us >= 0 &&
        rep.matching_p50_us <= cfg_.matching_slow_us;
    rep.redis_ok =
        rep.redis_p50_us >= 0 && rep.redis_p50_us <= cfg_.redis_slow_us;

    // --- dwell bookkeeping --------------------------------------------------
    // Trigger side: Throttled needs depth > trigger sustained for
    // throttle_trigger_ms ("capacity >80% (queue depth >250 for 30s)", §2.4).
    if (depth > cfg_.throttle_depth) {
        if (throttle_since_ < 0) throttle_since_ = now;
    } else {
        throttle_since_ = -1;
    }
    rep.throttle_triggered =
        throttle_since_ >= 0 &&
        now - throttle_since_ >= cfg_.throttle_trigger_ms;

    // Recovery side: per-mode "conditions continuously clear since" stamps.
    const bool ro_clear = rep.matching_p50_us >= 0 &&
                          rep.matching_p50_us < cfg_.matching_recover_us &&
                          rep.workers_ok && rep.redis_ok;
    ro_clear_since_ =
        ro_clear ? (ro_clear_since_ >= 0 ? ro_clear_since_ : now) : -1;
    const bool thr_clear = depth <= cfg_.throttle_recover_depth;
    thr_clear_since_ =
        thr_clear ? (thr_clear_since_ >= 0 ? thr_clear_since_ : now) : -1;
    const bool store_clear = rep.postgres_ok && rep.wal_ok;
    store_clear_since_ =
        store_clear ? (store_clear_since_ >= 0 ? store_clear_since_ : now) : -1;
    deriv_clear_since_ =
        rep.derivatives_ok
            ? (deriv_clear_since_ >= 0 ? deriv_clear_since_ : now)
            : -1;

    // --- arbitration: highest-severity firing trigger wins ------------------
    DegradationMode target = DegradationMode::Normal;
    const char* reason = "";
    if (rep.throttle_triggered) {
        target = DegradationMode::Throttled;
        reason = "capacity_over_80pct";
    }
    if (!rep.matching_loop_ok || !rep.redis_ok || !rep.workers_ok) {
        target = DegradationMode::ReadOnly;
        reason = !rep.matching_loop_ok
                     ? "matching_p50_slow"
                     : (!rep.redis_ok ? "redis_slow" : "shard_unhealthy");
    }
    if (!rep.derivatives_ok) {
        target = DegradationMode::SpotOnly;
        reason = "derivatives_unhealthy";
    }
    if (!rep.postgres_ok || !rep.wal_ok) {
        target = DegradationMode::MarketDataOnly;
        reason = !rep.postgres_ok ? "postgres_down" : "wal_corrupt";
    }
    rep.target_mode = target;
    rep.reason = reason;

    // --- apply / defer -------------------------------------------------------
    const DegradationMode current = modes_.stored_mode();
    rep.current_mode = current;

    if (target == current) {
        rep.transitioned = false;
    } else if (severity(target) > severity(current)) {
        // Escalation: apply immediately — cooldown must never delay a worse
        // degradation (fail-closed, spec §2.7).
        rep.transitioned = modes_.request_transition(target, reason);
        if (rep.transitioned) {
            alert(pager_level(target), "DEGRADATION_TRANSITION", reason);
        }
    } else {
        // De-escalation: require the current mode's sustained-clear dwell
        // (§2.4 recovery column / §2.7.3 30s healthy telemetry). The 60s
        // cooldown itself is enforced inside ModeManager::request_transition.
        int64_t clear_since = -1;
        switch (current) {
            case DegradationMode::ReadOnly:       clear_since = ro_clear_since_; break;
            case DegradationMode::Throttled:      clear_since = thr_clear_since_; break;
            case DegradationMode::MarketDataOnly: clear_since = store_clear_since_; break;
            case DegradationMode::SpotOnly:       clear_since = deriv_clear_since_; break;
            default:                              clear_since = -1; break;
        }
        const bool dwell_met =
            clear_since >= 0 && now - clear_since >= recovery_dwell_ms(current);
        if (dwell_met) {
            rep.transitioned = modes_.request_transition(target, reason);
            rep.recovery_pending = !rep.transitioned;  // cooldown still gating
            if (rep.transitioned) {
                alert(0, "DEGRADATION_RECOVERED", reason);
            }
        } else {
            rep.recovery_pending = true;
        }
    }

    last_report_ = rep;
    return rep;
}

bool HealthChecker::start() {
    if (runner_ != nullptr) return false;
    auto* r = new (std::nothrow) Runner();
    if (r == nullptr) return false;
    try {
        r->th = std::thread([this, r] {
            while (!r->stop.load(std::memory_order_relaxed)) {
                check_once();
                // Sleep in small chunks so stop() is responsive even with the
                // production 5s interval.
                int64_t slept = 0;
                while (slept < cfg_.interval_ms &&
                       !r->stop.load(std::memory_order_relaxed)) {
                    const int64_t chunk =
                        (cfg_.interval_ms - slept) < 20
                            ? (cfg_.interval_ms - slept)
                            : 20;
                    std::this_thread::sleep_for(
                        std::chrono::milliseconds(chunk));
                    slept += chunk;
                }
            }
        });
    } catch (...) {
        delete r;
        return false;
    }
    runner_ = r;
    return true;
}

void HealthChecker::stop() noexcept {
    if (runner_ == nullptr) return;
    runner_->stop.store(true, std::memory_order_relaxed);
    if (runner_->th.joinable()) {
        try {
            runner_->th.join();
        } catch (...) {
        }
    }
    delete runner_;
    runner_ = nullptr;
}

}  // namespace exch
