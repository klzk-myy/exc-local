#pragma once

// Task 2.3.5 — per-shard leader election, epoch-lease fencing model.
//
// CANONICAL CONTRACT (spec §18.6.2, remediation #35 — supersedes the pre-epoch
// "SETNX 10s TTL / EXPIRE every 3s" contract still printed in spec §2.5 and in
// the Task 2.3.5 plan text; the 10s/3s values must NOT be implemented):
//
//   * Lease `engine:leader:{shardId}` in Redis: TTL 2,000ms, refreshed every
//     500ms, value `{"epoch":N,"leader":"<node-id>"}` with a 64-bit
//     monotonically increasing fencing epoch.
//   * Acquisition: one atomic Lua script — absent/expired lease is written
//     {epoch = INCR counter, leader = self} with PX 2000; the granted epoch is
//     returned. (See LeaderLeaseStore.hpp for the epoch-counter rationale.)
//   * Fencing: before every guarded state mutation the engine validates
//     `epoch_local == epoch_current` via validate_epoch()/epoch_current();
//     on `epoch_local < epoch_current` (supersession) the process must
//     self-terminate without WAL/IPC writes — fence_or_die() (SIGTERM,
//     injectable for tests). Demotion also fires the P1 alert sink and the
//     degradation→ReadOnly hook (spec §2.5: "both stop matching, P1 alert,
//     degradation to ReadOnly").
//   * Heartbeat: every 500ms the leader runs the token-checked refresh Lua;
//     the reply carries the stored epoch so supersession is detected
//     immediately. A superseded/absent lease ⇒ fence_or_die. A transport
//     error ⇒ matching freezes at once (may_process_orders() == false — no
//     matching without a verified lease) and, if unrecoverable past the
//     lease-TTL boundary, the process self-terminates: after TTL expiry our
//     claim is dead on any correct store, so lingering is split-brain risk.
//   * Follower: polls the lease key (leader:heartbeat:{shardId}-equivalent
//     state — §4.2's observability mirror refreshed by the same Lua step);
//     on lease absence/expiry it attempts acquire, landing on epoch+1 via the
//     monotonic counter. Startup stabilization: acquire attempts are gated
//     for startup_stabilize_ms (default 5,000ms — plan AC "5s startup
//     stabilization delay", configurable for tests).
//   * Election completes before order processing: elect() runs the
//     acquire-or-follow loop and must return before the matching loop opens;
//     may_process_orders() is the per-iteration local guard.
//   * Redis down ⇒ fail-closed: acquire fails (stay follower), heartbeats
//     freeze matching, no path here ever matches without a live lease.
//
// Hooks are plain function pointers + ctx (no std::function → zero alloc,
// matching utils/ClockSyncGuard.hpp convention).

#include <atomic>
#include <cstdint>
#include <string>
#include <string_view>

#include "election/LeaderLeaseStore.hpp"

namespace exch {

struct LeaderElectionConfig {
    int64_t lease_ttl_ms = kLeaderLeaseTtlMs;         // 2,000 (§18.6.2)
    int64_t refresh_period_ms = kLeaderRefreshMs;     // 500   (§18.6.2)
    int64_t startup_stabilize_ms = kLeaderSettleMs;   // 5,000 (plan AC)
    int64_t follower_poll_ms = 250;                   // follower watch cadence
};

class LeaderElection {
public:
    // Store-less scaffold constructor (main.cpp): a LeaderElection with no
    // lease store can never lead — acquire() fails, elect() returns FOLLOWER,
    // may_process_orders() stays false (fail-closed).
    explicit LeaderElection(uint32_t shard_id) noexcept;
    LeaderElection(uint32_t shard_id, std::string_view node_id,
                   LeaderLeaseStore* store,
                   LeaderElectionConfig cfg = {}) noexcept;
    ~LeaderElection() noexcept;
    LeaderElection(const LeaderElection&) = delete;
    LeaderElection& operator=(const LeaderElection&) = delete;

    // --- injectable hooks ---------------------------------------------------
    // Monotonic milliseconds. Default: steady_clock. Tests inject a fake.
    using clock_fn = int64_t (*)(void* ctx) noexcept;
    void set_clock(clock_fn fn, void* ctx) noexcept;
    // Sleep for `ms` (elect()/run() pacing). Default: std::this_thread::sleep_for.
    using sleep_fn = void (*)(void* ctx, int64_t ms) noexcept;
    void set_sleep_hook(sleep_fn fn, void* ctx) noexcept;
    // Process termination on fencing. Default: kill(getpid(), SIGTERM) — spec
    // §18.6.2 self-terminate without WAL/IPC writes. Tests substitute a flag.
    using terminate_fn = void (*)(void* ctx) noexcept;
    void set_terminate_hook(terminate_fn fn, void* ctx) noexcept;
    // Alerting seam (P1 pages per spec §2.5 split-brain / §2.4 alerting).
    // level: 0 = P0, 1 = P1, 2 = P2 (pager severity). Default: stderr.
    using alert_fn = void (*)(void* ctx, int level, const char* code,
                              const char* detail) noexcept;
    void set_alert_sink(alert_fn fn, void* ctx) noexcept;
    // Degradation seam: invoked exactly once on demotion/fencing to drive the
    // system toward ReadOnly (spec §2.5). Wire to ModeManager::set_mode.
    using degrade_fn = void (*)(void* ctx) noexcept;
    void set_readonly_hook(degrade_fn fn, void* ctx) noexcept;
    // Leadership transition observer (e.g. ModeEventPublisher-style events:
    // "leader_acquired" / "leader_lost"). Purely observational.
    using event_fn = void (*)(void* ctx, const char* kind,
                              uint64_t epoch) noexcept;
    void set_event_sink(event_fn fn, void* ctx) noexcept;

    // --- election ------------------------------------------------------------
    // Single acquire attempt. Gated by startup_stabilize_ms; idempotent for
    // the current leader (re-arms TTL, keeps epoch). false = not leader.
    bool acquire() noexcept;
    // Caller-driven tick: leaders heartbeat on refresh_period_ms cadence,
    // followers poll/acquire on follower_poll_ms cadence. Drive it from the
    // service loop (or run() for a dedicated thread).
    void tick() noexcept;
    // Explicit token-checked refresh NOW (leader only). true = still held;
    // any other outcome runs the fencing path described above.
    bool heartbeat() noexcept;
    // Token-checked release (graceful shutdown). true = our lease deleted.
    bool release() noexcept;
    // Blocking election loop: tries to acquire until budget_ms is exhausted.
    // Returns the settled role — MUST be called and checked before the
    // matching loop opens order ingress (plan AC: "election completes before
    // order processing").
    LeaderRole elect(int64_t budget_ms) noexcept;
    // Dedicated heartbeat/watch thread. Returns false if a loop is already
    // running. stop() is idempotent.
    bool start() noexcept;
    void stop() noexcept;
    [[nodiscard]] bool running() const noexcept;

    // --- fencing --------------------------------------------------------------
    // Re-reads the store and compares epochs. true = our epoch is current.
    // false also means "store unreachable" (unknown ⇒ not current).
    [[nodiscard]] bool epoch_current() noexcept;
    // Pre-mutation fencing validation: epoch_current() plus automatic
    // fence_or_die() on confirmed supersession/absence. The engine calls this
    // before guarded state mutations (once per matching-loop pass or WAL
    // batch — the §18.6.2 "before every state mutation" check, amortized).
    bool validate_epoch() noexcept;
    // Confirmed lease loss ⇒ stop matching immediately, P1 alert, ReadOnly
    // hook, then self-terminate (terminate hook → SIGTERM by default). No WAL
    // or IPC writes happen on this path (spec §18.6.2).
    void fence_or_die() noexcept;
    // Polling split-brain probe: reads the lease and reports a foreign or
    // newer-epoch holder. If it confirms supersession while we still believe
    // we lead, it fences (fail-closed — detection without fencing is a
    // footgun). Returns true when another node owns/owned the lease.
    bool check_split_brain() noexcept;

    // --- state -----------------------------------------------------------------
    // These readers run on the matching thread while tick()/heartbeat()
    // mutate on the election thread — all observable state is atomic.
    [[nodiscard]] uint32_t shard_id() const noexcept { return shard_id_; }
    [[nodiscard]] LeaderRole role() const noexcept {
        return role_.load(std::memory_order_acquire);
    }
    [[nodiscard]] bool is_leader() const noexcept {
        return role() == LeaderRole::LEADER;
    }
    [[nodiscard]] uint64_t epoch() const noexcept {
        return epoch_.load(std::memory_order_acquire);
    }
    [[nodiscard]] bool fenced() const noexcept {
        return fenced_.load(std::memory_order_acquire);
    }
    [[nodiscard]] bool terminated() const noexcept {
        return terminated_.load(std::memory_order_acquire);
    }
    // Local gate the matching loop checks each iteration: leader, not fenced,
    // and not freeze-marked by heartbeat errors. Cheap — no store IO.
    [[nodiscard]] bool may_process_orders() const noexcept {
        return role() == LeaderRole::LEADER && !fenced() &&
               !frozen_.load(std::memory_order_acquire);
    }
    [[nodiscard]] const std::string& node_id() const noexcept {
        return node_id_;
    }

private:
    int64_t now_ms() const noexcept;
    void sleep_ms(int64_t ms) const noexcept;
    void alert(int level, const char* code, const char* detail) noexcept;
    void emit(const char* kind, uint64_t epoch) noexcept;
    [[nodiscard]] bool settle_gate_open(int64_t now) const noexcept;

    static int64_t default_clock(void* ctx) noexcept;
    static void default_sleep(void* ctx, int64_t ms) noexcept;
    static void default_terminate(void* ctx) noexcept;
    static void default_alert(void* ctx, int level, const char* code,
                              const char* detail) noexcept;

    uint32_t shard_id_;
    std::string node_id_;
    LeaderLeaseStore* store_ = nullptr;  // non-owning
    LeaderElectionConfig cfg_;

    clock_fn clock_ = default_clock;
    void* clock_ctx_ = nullptr;
    sleep_fn sleep_ = default_sleep;
    void* sleep_ctx_ = nullptr;
    terminate_fn terminate_ = default_terminate;
    void* term_ctx_ = nullptr;
    alert_fn alert_ = default_alert;
    void* alert_ctx_ = nullptr;
    degrade_fn readonly_ = nullptr;
    void* readonly_ctx_ = nullptr;
    event_fn event_ = nullptr;
    void* event_ctx_ = nullptr;

    // Atomics: written on the election thread (tick/run/elect), read on the
    // matching thread (may_process_orders/is_leader/epoch). Relaxed stores,
    // acquire loads on the reader side — the guards need visibility, not
    // ordering with the store IO.
    std::atomic<LeaderRole> role_{LeaderRole::FOLLOWER};
    std::atomic<uint64_t> epoch_{0};
    std::atomic<bool> fenced_{false};      // confirmed lease loss — terminal
    std::atomic<bool> terminated_{false};  // terminate hook ran
    std::atomic<bool> frozen_{false};      // heartbeat transport error: matching frozen
    // Election-thread-only fields (tick/heartbeat/acquire/elect must all be
    // driven from ONE thread — either the run() loop or manual ticking; do
    // not mix start() with manual tick/acquire calls).
    int64_t started_ms_ = 0;   // construction time (stabilize gate)
    int64_t last_poll_ms_ = 0;
    int64_t last_held_ms_ = 0;  // last successful heartbeat — freeze deadline base

    class Impl;  // thread handle, opaque to keep <thread> out of the header
    Impl* runner_ = nullptr;
};

}  // namespace exch
