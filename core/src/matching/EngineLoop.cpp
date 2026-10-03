// Task 2.3.19 — matching-loop driver, watermark backpressure, watchdog.

#include "matching/EngineLoop.hpp"

#include <chrono>
#include <cstdio>

#include "utils/TimeUtils.hpp"

namespace exch {

namespace {

void stderr_loop_report(void* /*ctx*/, const char* code, const char* detail) {
    std::fprintf(stderr, "[P1] %s: %s\n", code != nullptr ? code : "?",
                 detail != nullptr ? detail : "");
}

void sleep_ns(int64_t ns) noexcept {
    if (ns <= 0) return;
    std::this_thread::sleep_for(std::chrono::nanoseconds(ns));
}

}  // namespace

// --- Watchdog ------------------------------------------------------------------

Watchdog::Watchdog(const std::atomic<uint64_t>* beat, const std::atomic<int64_t>* last_beat_ns,
                   Thresholds t, report_fn fn, void* ctx,
                   const std::atomic<bool>* parked) noexcept
    : beat_(beat), last_beat_ns_(last_beat_ns), t_(t), fn_(fn), ctx_(ctx),
      parked_(parked) {}

const char* Watchdog::level_name(Level l) noexcept {
    switch (l) {
        case Level::Ok:
            return "OK";
        case Level::Warn:
            return "WARN";
        case Level::Stall:
            return "STALL";
    }
    return "?";
}

Watchdog::Level Watchdog::check_once(int64_t now_mono_ns) noexcept {
    const int64_t last = last_beat_ns_->load(std::memory_order_acquire);
    Level lvl = Level::Ok;
    // last==0 means the loop has not beaten yet — no staleness signal exists
    // before the first beat, so startup never false-alarms.
    if (last > 0) {
        const int64_t stale = now_mono_ns - last;
        if (stale > t_.stall_ns)
            lvl = Level::Stall;
        else if (stale > t_.warn_ns)
            lvl = Level::Warn;
        if (lvl == Level::Stall)
            ++stall_samples_;
        else if (lvl == Level::Warn) {
            // Deliberate idle park suppresses WARN — the scheduler owns the
            // sleep duration (nanosleep overshoot on a loaded host is not a
            // slow cycle). STALL above was already resolved, so a park that
            // never returns still escalates.
            if (parked_ != nullptr && parked_->load(std::memory_order_acquire)) {
                ++warn_suppressed_;
                lvl = Level::Ok;
            } else {
                ++warn_samples_;
            }
        }
    }
    if (lvl != armed_) {
        armed_ = lvl;
        if (fn_ != nullptr && lvl != Level::Ok) {
            // WARN reports are edge-triggered AND rate-capped at 1/sec: a
            // threshold-adjacent flap re-arms on every Ok sample and would
            // otherwise emit a line per sample (the same class of flood the
            // CRITICAL_BACKPRESSURE cap bounds — 25.6GB in the 8h soak).
            // STALL is not capped — it is rare and must not be lost.
            if (lvl == Level::Warn && last_warn_report_ns_ != 0 &&
                now_mono_ns - last_warn_report_ns_ < 1'000'000'000) {
                return lvl;
            }
            if (lvl == Level::Warn) last_warn_report_ns_ = now_mono_ns;
            const uint64_t beat = beat_ != nullptr ? beat_->load(std::memory_order_acquire) : 0;
            if (lvl == Level::Stall) ++stall_reports_;
            try {
                fn_(ctx_, lvl, last > 0 ? now_mono_ns - last : 0, beat);
            } catch (...) {
            }
        }
    }
    return lvl;
}

bool Watchdog::start() {
    if (th_.joinable()) return false;
    th_ = std::jthread([this](std::stop_token st) {
        while (!st.stop_requested()) {
            check_once(static_cast<int64_t>(steady_ns()));
            sleep_ns(t_.sample_ns);
        }
    });
    return true;
}

void Watchdog::stop() noexcept {
    if (th_.joinable()) {
        th_.request_stop();
        th_.join();
    }
}

// --- EngineLoop -----------------------------------------------------------------

EngineLoop::EngineLoop(EnginePump* pump, EngineLoopConfig cfg) noexcept
    : pump_(pump), cfg_(cfg),
      watchdog_(&loop_beat_, &last_beat_ns_, cfg.watchdog, nullptr, nullptr,
                &parked_) {}

EngineLoop::~EngineLoop() {
    stop();
    stop_watchdog();
}

uint32_t EngineLoop::spin_once() noexcept {
    const int64_t t0 = static_cast<int64_t>(steady_ns());
    uint32_t drained = 0;

    if (pump_ != nullptr) {
        // §2.7.3 watermark policy on the inbound ring (transports without
        // occupancy introspection report 0/0 -> policy inert, always drain).
        const uint64_t occ = pump_->inbound_occupancy();
        const uint64_t cap = pump_->inbound_capacity();
        stats_.queue_depth.store(occ, std::memory_order_relaxed);

        const bool over_halt = cap != 0 && occ * 100 >= cap * cfg_.halt_watermark_pct;
        if (over_halt && !halted_) {
            // >95%: pause ingress for THIS cycle — beats/ticks still run so
            // the watchdog sees a live loop — then drain regardless next
            // cycle. run_once is the ONLY consumer of the inbound ring, so
            // a halt that persists while occ >= 95% can never unwind: the
            // ring stays full, every producer write drops, ingress wedges
            // permanently (observed Phase-02.5 probe: orders_sent froze,
            // WAL stalled, engine spun halt cycles at ~84% CPU). Bounded
            // alternation is the spec §2.7.3 semantic ("pausing … to flush",
            // "without dropping frames") — a transient pause, not a stop.
            halted_ = true;
            const uint64_t ev =
                stats_.critical_bp_events.fetch_add(1, std::memory_order_relaxed) + 1;
            // Edge (first event) + at most 1/sec while the halt persists —
            // the count-stride alone let a busy-spin loop emit 25.6GB of
            // log in the Phase-02.5 8h soak.
            if (ev == 1 || t0 - last_crit_report_ns_ >= 1'000'000'000) {
                last_crit_report_ns_ = t0;
                report("CRITICAL_BACKPRESSURE", "inbound ring >95% — ingress paused this cycle");
            }
            pump_->set_shed_new_orders(true);
        } else {
            halted_ = false;
            shedding_ = cap != 0 && occ * 100 >= cap * cfg_.shed_watermark_pct;
            // At >=95% the drain cycle still sheds OrderNew — the >80% tier
            // remains in force through the pause (sequenced CAPACITY_EXCEEDED
            // rejects; cancels always dispatch).
            pump_->set_shed_new_orders(shedding_ || over_halt);
            if (shedding_ || over_halt) stats_.shed_cycles.fetch_add(1, std::memory_order_relaxed);
            drained = pump_->run_once(cfg_.max_batch);
        }

        // Deterministic engine clock: tick every tick_interval_ns of loop
        // cadence (TIME_TICK WAL stamping lives in EnginePump::tick — the
        // matching thread owns the WAL, Task 2.3.10 remediation #4).
        if (t0 - last_tick_mono_ >= cfg_.tick_interval_ns) {
            last_tick_mono_ = t0;
            pump_->tick(now_ns());
        }
    }

    // Cycle-cost accounting only when the cycle did work — at 50k+ orders/s
    // the loop iterates ~20M times/s and the vast majority drain nothing;
    // a histogram record + second clock read + three atomic ops on every
    // empty spin was ~20% of the matching core in profiling (Phase-02.5).
    // Empty cycles can't stall (they take ~100ns), so skipping their stats
    // changes nothing the watchdog or STALL detection observes — both key
    // off beat staleness, which still stamps every iteration.
    int64_t beat_ns = t0;
    if (drained > 0) {
        const int64_t t1 = static_cast<int64_t>(steady_ns());
        const int64_t cyc = t1 - t0;
        beat_ns = t1;
        stats_.last_cycle_ns.store(cyc, std::memory_order_relaxed);
        int64_t mx = stats_.max_cycle_ns.load(std::memory_order_relaxed);
        while (cyc > mx &&
               !stats_.max_cycle_ns.compare_exchange_weak(mx, cyc, std::memory_order_relaxed));
        stats_.cycle_hist.record(static_cast<uint64_t>(cyc < 0 ? 0 : cyc));
    }
    stats_.loop_iters.fetch_add(1, std::memory_order_relaxed);
    // The liveness beat: stamped at END of cycle so a hung dispatch stretches
    // staleness exactly as long as the loop is actually stuck.
    loop_beat_.fetch_add(1, std::memory_order_release);
    last_beat_ns_.store(beat_ns, std::memory_order_release);

    if (drained == 0 && cfg_.idle_sleep_ns > 0) {
        // Deliberate idle park: the mark covers the WHOLE idle stretch, not
        // just the nanosleep — parked time is loop policy, not a slow cycle,
        // and idle-period housekeeping (cross-shard drain, tick work between
        // park exit and the next park entry) routinely exceeds warn_ns when
        // the loop only runs a few times per ms (observed: ~600µs un-parked
        // windows at idle_sleep_ns=100µs -> ~1.4k ENGINE_WARN/s per engine,
        // ~16MB/s of json-file log writes across 8 shards). STALL still
        // fires if a hang stretches staleness past stall_ns — beats stop
        // stamping entirely, which no parked flag can mask.
        parked_.store(true, std::memory_order_release);
        last_beat_ns_.store(static_cast<int64_t>(steady_ns()), std::memory_order_release);
        sleep_ns(cfg_.idle_sleep_ns);
        last_beat_ns_.store(static_cast<int64_t>(steady_ns()), std::memory_order_release);
    } else if (drained > 0) {
        // First cycle with real work ends the idle stretch. WARN coverage is
        // only lost for this one transition cycle; the next hung dispatch
        // still reports normally.
        parked_.store(false, std::memory_order_release);
    }
    return drained;
}

void EngineLoop::run(const std::atomic<bool>* stop) noexcept {
    while (!stop->load(std::memory_order_acquire)) spin_once();
}

bool EngineLoop::start() {
    if (th_.joinable()) return false;
    th_ = std::jthread([this](std::stop_token st) {
        while (!st.stop_requested()) spin_once();
    });
    return true;
}

void EngineLoop::stop() noexcept {
    if (th_.joinable()) {
        th_.request_stop();
        th_.join();
    }
}

bool EngineLoop::start_watchdog(Watchdog::report_fn fn, void* ctx) {
    watchdog_.set_report_sink(fn, ctx);
    return watchdog_.start();
}

void EngineLoop::stop_watchdog() noexcept { watchdog_.stop(); }

void EngineLoop::report(const char* code, const char* detail) noexcept {
    try {
        if (report_fn_ != nullptr)
            report_fn_(report_ctx_, code, detail);
        else
            stderr_loop_report(nullptr, code, detail);
    } catch (...) {
    }
}

}  // namespace exch
