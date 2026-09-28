#pragma once

// Task 2.3.19 — matching-loop driver, ring-buffer watermark backpressure,
// in-process watchdog, and loop metrics (spec §2.7, §3.6, §24 #299).
//
// EngineLoop owns the per-cycle cadence on top of EnginePump (Task 2.3.7):
// each cycle samples inbound occupancy, applies the §2.7.3 watermark policy —
//   >80%  shed: consume-but-reject OrderNew (cancels always pass; they
//         release capacity), reported as CAPACITY_EXCEEDED;
//   >95%  halt: ingress stops entirely — messages stay queued so the SPSC
//         producer blocks on a full transport with sequenced drop
//         accounting, instead of the engine consuming-then-dropping
//         unsequenced commands — reported as CRITICAL_BACKPRESSURE —
// drains a bounded batch, drives the deterministic expiry/stop tick
// (on_time_tick), and stamps the monotonic loop_beat the Watchdog samples.
//
// Watchdog (Task 2.3.19 DoD): a dedicated jthread samples the beat at 100µs.
// Staleness > warn_ns (default 500µs — a slow cycle) fires a WARN report;
// staleness > stall_ns fires STALL once per episode (edge-triggered, re-arms
// on a fresh beat). The stall ACTION is deliberately a callback: Task 2.3.19's
// L0 response (dirty-WAL flush -> leader-lease release -> core halt for
// hot-standby promotion) must run where WAL/thread ownership is legal — the
// embedder wires it in main.cpp. Default stall_ns is 2s: the plan's 2ms
// halt threshold is achievable via config but false-positive halts under
// scheduler jitter are worse than a slower true-positive detection on this
// scaffold (note for production tuning: set stall_ns = 2ms on pinned cores).
// sd_notify("WATCHDOG=1") supervisor pings are wired at the main.cpp sink.

#include <atomic>
#include <cstdint>
#include <thread>

#include "ipc/EnginePump.hpp"

namespace exch {

// --- Watchdog -----------------------------------------------------------------

// Samples a monotonic beat counter + last-beat timestamp published by the
// loop; reports level transitions through a function-pointer sink (no
// std::function alloc, noexcept call sites).
class Watchdog {
   public:
    enum class Level : uint8_t { Ok = 0, Warn = 1, Stall = 2 };

    struct Thresholds {
        int64_t sample_ns = 100'000;       // 100µs sampling cadence
        int64_t warn_ns = 500'000;         // >500µs stale -> WARN
        int64_t stall_ns = 2'000'000'000;  // >2s stale -> STALL (see header)
    };

    // level, observed staleness ns, last beat value. Called from the watchdog
    // thread (or the test's thread) on every transition into Warn/Stall.
    // A throwing sink is swallowed at the call site.
    using report_fn = void (*)(void* ctx, Level level, int64_t stale_ns, uint64_t beat);

    // beat/last_beat_ns must outlive the Watchdog (EngineLoop member layout
    // guarantees this — the atomics are declared before the watchdog member).
    // `parked` (nullable) suppresses WARN while the loop is in a deliberate
    // idle_sleep park — parked time is loop policy, not a slow cycle. STALL
    // still fires: a park that never returns (hang inside sleep is
    // impossible, so staleness then means the wake path wedged) is a real
    // stall. Suppressed warns accumulate in warn_suppressed_.
    Watchdog(const std::atomic<uint64_t>* beat, const std::atomic<int64_t>* last_beat_ns,
             Thresholds t, report_fn fn = nullptr, void* ctx = nullptr,
             const std::atomic<bool>* parked = nullptr) noexcept;

    // One staleness probe with an explicit monotonic clock reading — the
    // testable core. Returns the observed level; fires the sink on a
    // transition into Warn/Stall (edge-triggered).
    Level check_once(int64_t now_mono_ns) noexcept;

    void set_report_sink(report_fn fn, void* ctx) noexcept {
        fn_ = fn;
        ctx_ = ctx;
    }

    // Dedicated sampling thread: check_once(steady_ns()) every sample_ns.
    bool start();
    void stop() noexcept;
    [[nodiscard]] bool running() const noexcept { return th_.joinable(); }

    [[nodiscard]] uint64_t warn_samples() const noexcept { return warn_samples_; }
    [[nodiscard]] uint64_t stall_samples() const noexcept { return stall_samples_; }
    [[nodiscard]] uint64_t stall_reports() const noexcept { return stall_reports_; }
    // WARNs suppressed because the loop was in a deliberate idle park.
    [[nodiscard]] uint64_t warn_suppressed() const noexcept { return warn_suppressed_; }

    static const char* level_name(Level l) noexcept;

   private:
    const std::atomic<uint64_t>* beat_;
    const std::atomic<int64_t>* last_beat_ns_;
    Thresholds t_;
    report_fn fn_ = nullptr;
    void* ctx_ = nullptr;

    Level armed_ = Level::Ok;  // edge detection — re-arms on a fresh beat
    const std::atomic<bool>* parked_ = nullptr;  // nullable — see ctor doc
    uint64_t warn_samples_ = 0;
    uint64_t stall_samples_ = 0;
    uint64_t stall_reports_ = 0;
    uint64_t warn_suppressed_ = 0;

    std::jthread th_;
};

// --- EngineLoop -----------------------------------------------------------------

struct EngineLoopConfig {
    uint32_t max_batch = 256;              // inbound frames drained per cycle
    int64_t tick_interval_ns = 1'000'000;  // on_time_tick cadence (1ms)
    // Pause when a cycle drains nothing. 0 = hot spin (matching-core default).
    // Must stay << watchdog_warn_ns or an idle loop reads as a stalled one.
    int64_t idle_sleep_ns = 0;
    // §2.7.3 watermarks (percent of inbound ring capacity).
    uint64_t shed_watermark_pct = 80;        // >80%: shed new orders
    uint64_t halt_watermark_pct = 95;        // >95%: halt ingress this cycle
    uint64_t critical_report_stride = 1024;  // CRITICAL_BACKPRESSURE stride
    Watchdog::Thresholds watchdog{};
};

// One-line metrics surface for monitor threads (all relaxed atomics; the
// single writer is the loop thread running spin_once).
struct EngineLoopStats {
    std::atomic<uint64_t> loop_iters{0};
    std::atomic<uint64_t> queue_depth{0};         // last inbound occupancy sample
    std::atomic<uint64_t> shed_cycles{0};         // cycles run under the shed wm
    std::atomic<uint64_t> critical_bp_events{0};  // cycles under the halt wm
    std::atomic<int64_t> last_cycle_ns{0};
    std::atomic<int64_t> max_cycle_ns{0};
    LatencyHistogram cycle_hist;  // per-cycle duration, ns
};

class EngineLoop {
   public:
    // pump must outlive the loop. Watchdog thresholds come from cfg.
    EngineLoop(EnginePump* pump, EngineLoopConfig cfg = {}) noexcept;
    ~EngineLoop();

    EngineLoop(const EngineLoop&) = delete;
    EngineLoop& operator=(const EngineLoop&) = delete;

    // One loop iteration (watermark policy -> drain batch -> tick -> beat).
    // Returns frames consumed. Directly callable — tests drive it inline.
    uint32_t spin_once() noexcept;

    // Blocking loop on the calling thread until *stop (must be non-null).
    void run(const std::atomic<bool>* stop) noexcept;
    // Owned jthread variant. stop() is idempotent and joins.
    bool start();
    void stop() noexcept;

    // Wire + start the watchdog's sampling thread (fn gets WARN/STALL edges).
    bool start_watchdog(Watchdog::report_fn fn, void* ctx);
    void stop_watchdog() noexcept;
    [[nodiscard]] Watchdog& watchdog() noexcept { return watchdog_; }

    // Alert sink for CRITICAL_BACKPRESSURE (same signature as EnginePump's).
    void set_report_sink(EnginePump::report_fn fn, void* ctx) noexcept {
        report_fn_ = fn;
        report_ctx_ = ctx;
    }

    // Liveness surface the Watchdog samples.
    [[nodiscard]] const std::atomic<uint64_t>& loop_beat() const noexcept { return loop_beat_; }
    [[nodiscard]] const std::atomic<int64_t>& last_beat_mono_ns() const noexcept {
        return last_beat_ns_;
    }

    [[nodiscard]] const EngineLoopStats& stats() const noexcept { return stats_; }
    [[nodiscard]] EnginePump* pump() const noexcept { return pump_; }
    [[nodiscard]] bool ingress_halted() const noexcept { return halted_; }
    [[nodiscard]] bool shedding() const noexcept { return shedding_; }

   private:
    void report(const char* code, const char* detail) noexcept;

    EnginePump* pump_;
    EngineLoopConfig cfg_;

    // Written by the loop thread, read by the watchdog/monitor threads.
    std::atomic<uint64_t> loop_beat_{0};
    std::atomic<int64_t> last_beat_ns_{0};
    // True while inside a deliberate idle_sleep park — the watchdog suppresses
    // WARN (parked time is policy, not work) but never STALL.
    std::atomic<bool> parked_{false};

    int64_t last_tick_mono_ = 0;
    bool halted_ = false;
    bool shedding_ = false;

    EnginePump::report_fn report_fn_ = nullptr;
    void* report_ctx_ = nullptr;

    EngineLoopStats stats_;

    // Declared last: it stores pointers to loop_beat_/last_beat_ns_, so the
    // atomics must already exist at construction (member declaration order).
    Watchdog watchdog_;

    std::jthread th_;
};

}  // namespace exch
