// Task 2.3.5 — per-shard leader election, epoch-lease fencing model.
// Canonical mechanics: spec §18.6.2 (remediation #35) — TTL 2,000ms, refresh
// 500ms, 64-bit monotonic fencing epoch, token-checked Lua everywhere. The
// pre-epoch SETNX 10s/EXPIRE 3s contract in spec §2.5 and the Task 2.3.5 plan
// text is SUPERSEDED and deliberately not implemented here.

#include "election/LeaderElection.hpp"

#include <atomic>
#include <chrono>
#include <cstdio>
#include <csignal>
#include <thread>
#include <unistd.h>

namespace exch {

struct LeaderElection::Impl {
    std::atomic<bool> stop{false};
    std::thread th;
};

// --- statics ------------------------------------------------------------------

int64_t LeaderElection::default_clock(void*) noexcept {
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::steady_clock::now().time_since_epoch())
        .count();
}

void LeaderElection::default_sleep(void*, int64_t ms) noexcept {
    if (ms > 0) {
        std::this_thread::sleep_for(std::chrono::milliseconds(ms));
    }
}

void LeaderElection::default_terminate(void*) noexcept {
    // Spec §18.6.2: "the partitioned primary immediately self-terminates
    // (SIGTERM) without writing to disk or publishing IPC events". We raise
    // rather than exit() so the systemd/supervisor path sees a signal death
    // and the WAL's crash-flush handler (§18.6.3 step 1) still runs.
    ::kill(::getpid(), SIGTERM);
}

void LeaderElection::default_alert(void*, int level, const char* code,
                                   const char* detail) noexcept {
    std::fprintf(stderr, "[election] P%d %s: %s\n", level, code,
                 detail != nullptr ? detail : "");
}

// --- lifecycle ----------------------------------------------------------------

LeaderElection::LeaderElection(uint32_t shard_id) noexcept
    : shard_id_(shard_id) {
    started_ms_ = now_ms();
}

LeaderElection::LeaderElection(uint32_t shard_id, std::string_view node_id,
                               LeaderLeaseStore* store,
                               LeaderElectionConfig cfg) noexcept
    : shard_id_(shard_id),
      node_id_(node_id),
      store_(store),
      cfg_(cfg) {
    started_ms_ = now_ms();
    if (!valid_node_id(node_id_)) {
        // Misconfiguration is fail-closed: an unusable node id can never
        // hold a lease, so this instance stays a permanent follower.
        alert(1, "ELECTION_BAD_NODE_ID", "node id rejected; stays follower");
        node_id_.clear();
    }
}

LeaderElection::~LeaderElection() noexcept { stop(); }

void LeaderElection::set_clock(clock_fn fn, void* ctx) noexcept {
    clock_ = fn != nullptr ? fn : default_clock;
    clock_ctx_ = ctx;
    started_ms_ = now_ms();  // re-base the settle gate on the new clock
}

void LeaderElection::set_sleep_hook(sleep_fn fn, void* ctx) noexcept {
    sleep_ = fn != nullptr ? fn : default_sleep;
    sleep_ctx_ = ctx;
}

void LeaderElection::set_terminate_hook(terminate_fn fn, void* ctx) noexcept {
    terminate_ = fn != nullptr ? fn : default_terminate;
    term_ctx_ = ctx;
}

void LeaderElection::set_alert_sink(alert_fn fn, void* ctx) noexcept {
    alert_ = fn != nullptr ? fn : default_alert;
    alert_ctx_ = ctx;
}

void LeaderElection::set_readonly_hook(degrade_fn fn, void* ctx) noexcept {
    readonly_ = fn;
    readonly_ctx_ = ctx;
}

void LeaderElection::set_event_sink(event_fn fn, void* ctx) noexcept {
    event_ = fn;
    event_ctx_ = ctx;
}

// --- helpers ------------------------------------------------------------------

int64_t LeaderElection::now_ms() const noexcept {
    return clock_ != nullptr ? clock_(clock_ctx_) : default_clock(nullptr);
}

void LeaderElection::sleep_ms(int64_t ms) const noexcept {
    if (sleep_ != nullptr) sleep_(sleep_ctx_, ms);
}

void LeaderElection::alert(int level, const char* code,
                           const char* detail) noexcept {
    if (alert_ != nullptr) alert_(alert_ctx_, level, code, detail);
}

void LeaderElection::emit(const char* kind, uint64_t epoch) noexcept {
    if (event_ != nullptr) event_(event_ctx_, kind, epoch);
}

bool LeaderElection::settle_gate_open(int64_t now) const noexcept {
    return now - started_ms_ >= cfg_.startup_stabilize_ms;
}

// --- election ------------------------------------------------------------------

bool LeaderElection::acquire() noexcept {
    if (fenced_ || store_ == nullptr || node_id_.empty()) return false;
    const int64_t now = now_ms();
    if (!settle_gate_open(now)) return false;  // startup stabilization

    bool granted = false;
    uint64_t epoch = 0;
    if (!store_->try_acquire(node_id_, cfg_.lease_ttl_ms, &granted, &epoch)) {
        // Transport error: state unknown → stay follower (fail-closed: never
        // match without a lease).
        return false;
    }
    if (!granted) {
        // Another holder carries the stored epoch. If it somehow regressed
        // below ours while we believed we led, the store is inconsistent —
        // the heartbeat path will fence us on the next refresh; nothing to
        // do here but stay follower.
        return false;
    }
    const bool reacquire = (role_ == LeaderRole::LEADER);
    // Epoch first, then publish LEADER: a matching-thread reader that sees
    // the leader role must already see the fencing epoch that granted it.
    epoch_ = epoch;
    role_ = LeaderRole::LEADER;
    last_held_ms_ = now;
    frozen_ = false;
    if (!reacquire) emit("leader_acquired", epoch_);
    return true;
}

bool LeaderElection::heartbeat() noexcept {
    if (role_ != LeaderRole::LEADER || fenced_) return false;
    uint64_t cur = 0;
    const LeaseCheck chk =
        store_->heartbeat(node_id_, epoch_, cfg_.lease_ttl_ms, &cur);
    const int64_t now = now_ms();
    switch (chk) {
        case LeaseCheck::Held:
            last_held_ms_ = now;
            frozen_ = false;
            return true;
        case LeaseCheck::Superseded:
            // epoch_local < epoch_current — spec §18.6.2 mandates SIGTERM.
            fence_or_die();
            return false;
        case LeaseCheck::Absent:
            // Our lease is gone (expired under a stalled heartbeat, or
            // revoked). A key could already have been re-acquired and
            // released by a successor inside a missed-refresh window —
            // absence is unprovable, so it is fatal (fail-closed).
            fence_or_die();
            return false;
        case LeaseCheck::Error:
        default:
            // Unknown state: freeze matching immediately (do NOT match
            // without a verified lease) and give the transport until the
            // lease-TTL boundary. Past it, our claim is provably dead on any
            // correct store — a partitioned ex-leader lingering past its
            // lease is exactly the split-brain case §18.6.2 exists to kill.
            frozen_ = true;
            if (now - last_held_ms_ >= cfg_.lease_ttl_ms) {
                alert(1, "LEASE_UNVERIFIABLE",
                      "heartbeat transport down past lease TTL boundary");
                fence_or_die();
                return false;
            }
            alert(2, "LEASE_REFRESH_FAILED",
                  "leader heartbeat transport error; matching frozen");
            return false;
    }
}

bool LeaderElection::release() noexcept {
    if (role_ != LeaderRole::LEADER || store_ == nullptr) return false;
    const uint64_t e = epoch_.load(std::memory_order_relaxed);
    role_ = LeaderRole::FOLLOWER;
    epoch_ = 0;
    const bool ok = store_->release(node_id_, e);
    if (ok) emit("leader_released", e);
    return ok;
}

void LeaderElection::tick() noexcept {
    if (fenced_ || store_ == nullptr) return;
    const int64_t now = now_ms();
    if (role_ == LeaderRole::LEADER) {
        if (now - last_held_ms_ >= cfg_.refresh_period_ms || frozen_) {
            heartbeat();
        }
        return;
    }
    // Follower: watch the lease (leader:heartbeat:{shard}-equivalent — the
    // lease key itself is the authoritative state) and take over on expiry.
    if (!settle_gate_open(now)) return;
    if (now - last_poll_ms_ < cfg_.follower_poll_ms) return;
    last_poll_ms_ = now;
    acquire();
}

LeaderRole LeaderElection::elect(int64_t budget_ms) noexcept {
    const int64_t deadline = now_ms() + (budget_ms > 0 ? budget_ms : 0);
    for (;;) {
        if (fenced_) return LeaderRole::FOLLOWER;
        if (acquire()) return LeaderRole::LEADER;
        const int64_t now = now_ms();
        if (now >= deadline) return role_;
        int64_t nap = cfg_.follower_poll_ms;
        // Sleep past the settle gate in one go when that's the only blocker.
        const int64_t gate_left = (started_ms_ + cfg_.startup_stabilize_ms) - now;
        if (gate_left > nap) nap = gate_left;
        const int64_t left = deadline - now;
        if (nap > left) nap = left;
        sleep_ms(nap);
    }
}

bool LeaderElection::start() noexcept {
    if (runner_ != nullptr) return false;
    runner_ = new Impl();
    runner_->th = std::thread([this] {
        while (!runner_->stop.load(std::memory_order_relaxed)) {
            tick();
            // Cadence: the finer of refresh/poll so neither is late.
            const int64_t period = cfg_.refresh_period_ms < cfg_.follower_poll_ms
                                       ? cfg_.refresh_period_ms
                                       : cfg_.follower_poll_ms;
            int64_t slept = 0;
            while (slept < period &&
                   !runner_->stop.load(std::memory_order_relaxed)) {
                const int64_t chunk = (period - slept) < 20 ? (period - slept) : 20;
                sleep_ms(chunk);
                slept += chunk;
            }
        }
    });
    return true;
}

void LeaderElection::stop() noexcept {
    if (runner_ == nullptr) return;
    runner_->stop.store(true, std::memory_order_relaxed);
    if (runner_->th.joinable()) runner_->th.join();
    delete runner_;
    runner_ = nullptr;
}

bool LeaderElection::running() const noexcept {
    return runner_ != nullptr;
}

// --- fencing -------------------------------------------------------------------

bool LeaderElection::epoch_current() noexcept {
    if (role_ != LeaderRole::LEADER || fenced_ || store_ == nullptr) {
        return false;
    }
    LeaseRecord rec;
    if (!store_->read(&rec)) return false;  // unknown ⇒ not current
    return rec.present && rec.epoch == epoch_ && rec.leader == node_id_;
}

bool LeaderElection::validate_epoch() noexcept {
    if (role_ != LeaderRole::LEADER || fenced_) return false;
    LeaseRecord rec;
    if (store_ == nullptr || !store_->read(&rec)) {
        // Unverifiable: freeze matching but do not die on a single read
        // failure — heartbeat's TTL boundary is the kill switch.
        frozen_ = true;
        return false;
    }
    if (!rec.present || rec.epoch != epoch_ || rec.leader != node_id_) {
        // Confirmed supersession/revocation (incl. epoch_local <
        // epoch_current): spec §18.6.2 self-termination path.
        fence_or_die();
        return false;
    }
    return true;
}

void LeaderElection::fence_or_die() noexcept {
    if (fenced_) return;
    // Stop matching FIRST — no further guarded mutation may proceed from
    // this process even while the alert/degrade hooks run.
    fenced_ = true;
    frozen_ = true;
    const uint64_t e = epoch_;
    role_ = LeaderRole::FOLLOWER;
    emit("leader_lost", e);
    alert(1, "SPLIT_BRAIN_FENCED",
          "lease superseded or unverifiable; self-terminating per §18.6.2");
    if (readonly_ != nullptr) readonly_(readonly_ctx_);  // degrade → ReadOnly
    terminated_ = true;
    if (terminate_ != nullptr) terminate_(term_ctx_);
}

bool LeaderElection::check_split_brain() noexcept {
    if (store_ == nullptr) return false;
    LeaseRecord rec;
    if (!store_->read(&rec)) return false;  // unknown ≠ proof of split brain
    if (!rec.present) return false;
    const bool foreign = rec.leader != node_id_ || rec.epoch != epoch_;
    if (foreign && role_ == LeaderRole::LEADER && !fenced_) {
        // We think we lead but the store disagrees — confirmed split brain.
        fence_or_die();
    }
    return foreign;
}

}  // namespace exch
