#pragma once

// Phase-21 Task 21.3.10 — in-process sanctions account-flag cache for
// the C++ matching core ("SanctionsHook", spec §14.3 step 2 + §3.3).
//
// The Go control plane (services/internal/compliance/sanctions_flags.go)
// is the authority: screening outcomes land in the audit trail and the
// compliance-hold workflow, then publish coordination-Redis flags:
//
//   exc:sanctions:flagged:{account_id}         — "1" while flagged
//   exc:sanctions:flagged:cleared:{account_id} — compliance-dispositioned
//   exc:sanctions:screened:bloom               — SETBIT per screened acct
//   exc:sanctions:feed:heartbeat               — unix-seconds liveness
//
// A control-path SanctionsRefresher polls that keyspace via RespClient
// on its own thread (zero Redis on the match thread — the same
// discipline as SuspensionRefresher) and swaps a Snapshot into this
// object. PreTradeChecker consults it as check 0c on EVERY order
// admission: shared_ptr load + hash lookups + (optionally) one bitmap
// bit test — zero heap.
//
// Fail closed (spec §2.7):
//   * no snapshot ever applied (refresher never succeeded)      -> reject
//   * last refresh errored (mark_unverifiable)                  -> reject
//   * heartbeat missing/stale past the bound window             -> reject
//   * account in the flagged set                              -> SANCTIONS_HIT
//   * account absent from flagged set                         -> pass
//     (cleared marker / screened bit are audit-grade signal; strict
//     mode via set_require_screened(true) rejects accounts whose
//     screened bit is absent — enabling it before the onboarding
//     screen is wired would block every account, so it is OPT-IN)
//   * a bound-but-null cache is NOT an error — unwired engines keep
//     the legacy behavior (Go admission gates stay authoritative).
//
// Scope note (spec §24 #43 / Task 21.3.23): this cache blocks ORDER
// ENTRY for flagged accounts only — it is not a trading halt; the
// provider-outage scoped degradation lives in the Go funding/KYC
// paths (queued screens + SANCTIONS_SERVICE_UNAVAILABLE), not here.

#include <atomic>
#include <cstdint>
#include <memory>
#include <shared_mutex>
#include <string>
#include <string_view>
#include <unordered_set>

namespace exch {

class SanctionsCache {
public:
    // Snapshot is immutable once published; readers hold it via
    // shared_ptr so the refresher swaps concurrently without locks on
    // the check path.
    struct Snapshot {
        bool unverifiable = false;  // last refresh failed -> fail closed
        uint64_t heartbeat_unix = 0;  // exc:sanctions:feed:heartbeat (s)
        uint64_t applied_ns = 0;      // steady_ns at apply — age bound
        std::unordered_set<uint64_t> flagged;   // flagged:{id}
        std::unordered_set<uint64_t> cleared;   // flagged:cleared:{id}
        std::string screened_bitmap;            // screened:bloom blob
    };

    // Hot-path verdict — an enum, NOT a string: the check path compares
    // it in-register (a const-char* literal pointer would be a cross-TU
    // identity hazard) and zero-allocates.
    enum class Verdict : uint8_t {
        Clear,         // screened or unflagged — proceed
        Hit,           // flagged -> SANCTIONS_HIT
        Unscreened,    // strict mode, no screened bit -> fail closed
        Unverifiable,  // no/unverifiable/stale snapshot -> fail closed
    };

    SanctionsCache() = default;

    // Control path — publish a freshly polled flag set. Passing nullptr
    // (or a snapshot with unverifiable set) engages the fail-closed
    // state. applied_ns is the refresher's steady clock at apply time —
    // the age bound the check path enforces against its own now_ns.
    void apply(std::shared_ptr<const Snapshot> snap) noexcept {
        std::lock_guard<std::shared_mutex> lk(mu_);
        snap_ = std::move(snap);
        ready_.store(true, std::memory_order_release);
    }

    // Mark the flag store unreadable — every subsequent check rejects
    // until a successful apply() lands.
    void mark_unverifiable() noexcept {
        auto s = std::make_shared<Snapshot>();
        s->unverifiable = true;
        apply(std::move(s));
    }

    // True once any snapshot (even an "all clear" one) has landed.
    [[nodiscard]] bool ready() const noexcept {
        return ready_.load(std::memory_order_acquire);
    }

    // OPT-IN strict mode (default off): an account with no screened
    // bitmap bit rejects UNSCREENED even when unflagged. Bind only
    // after the Go onboarding screen is proven live — otherwise every
    // account fails closed.
    void set_require_screened(bool v) noexcept {
        require_screened_.store(v, std::memory_order_release);
    }
    [[nodiscard]] bool require_screened() const noexcept {
        return require_screened_.load(std::memory_order_acquire);
    }

    // Snapshot-freshness bound: a snapshot older than max_age_ns
    // (checked against the checker's own steady now_ns) is treated as
    // unverifiable. Default 60s — the spec §14.3 staleness criterion.
    // The control poll runs at ~1s cadence so 60s tolerates a stalled
    // poll thread without going stale.
    void set_max_age_ns(uint64_t ns) noexcept {
        max_age_ns_.store(ns, std::memory_order_release);
    }
    [[nodiscard]] uint64_t max_age_ns() const noexcept {
        return max_age_ns_.load(std::memory_order_acquire);
    }

    // Hot path — Verdict::Clear / Hit / Unscreened / Unverifiable.
    // now_ns is the checker's steady clock (CheckContext.now_ns).
    [[nodiscard]] Verdict verdict_for(uint64_t account_id,
                                      uint64_t now_ns) const noexcept;

private:
    mutable std::shared_mutex mu_;
    std::shared_ptr<const Snapshot> snap_{};
    std::atomic<bool> ready_{false};
    std::atomic<bool> require_screened_{false};
    std::atomic<uint64_t> max_age_ns_{60'000'000'000ULL};  // 60s
};

// Parse one `exc:sanctions:flagged:*` key into a Snapshot —
// `flagged:{id}` -> flagged set, `flagged:cleared:{id}` -> cleared set.
// Returns false for foreign or malformed keys (ignored, never flag).
[[nodiscard]] bool sanctions_key_into(std::string_view key,
                                      SanctionsCache::Snapshot* out) noexcept;

class RespClient;

// SanctionsRefresher is the control-path poller — one thread polls
// `exc:sanctions:*` via RespClient on its own cadence (main.cpp wires
// ~1s) and swaps the Snapshot into SanctionsCache. The matching thread
// never touches Redis. Any transport/parse failure marks the cache
// unverifiable — check 0c rejects until a clean poll lands.
class SanctionsRefresher {
public:
    // Non-owning: client may be nullptr (refresh then always marks
    // unverifiable — bound-but-unwired still fails closed).
    explicit SanctionsRefresher(RespClient* client) noexcept;
    // One poll round: KEYS flagged:* + GET heartbeat + GET bloom ->
    // snapshot -> apply. Returns false after marking unverifiable on
    // any failure.
    bool refresh(SanctionsCache* cache) noexcept;

private:
    RespClient* client_;
};

}  // namespace exch
