#pragma once

// Phase-11 Tasks 11.3.4 / 11.3.8 / 11.3.12 — in-process kill-switch
// suspension lattice for the C++ matching core.
//
// The Go control plane (services/internal/admin/killswitch_service.go)
// is the authority: mutations land in `trading_suspensions`
// (PostgreSQL) + `admin_audit_log`, then raise Redis `halt:*` flags —
// `halt:global`, `halt:account:{id}`, `halt:counterparty:{id}`,
// `halt:instrument:{SYMBOL}`, `halt:instrumentclass:{CLASS}`,
// `halt:fixsession:{id}`, `halt:lp:{id}`, `halt:rail:{RAIL}`,
// `halt:desk:{id}`, `halt:region:{R}`, `halt:env:{E}`.
//
// A control-path SuspensionRefresher (SuspensionRefresher.cpp) polls
// those keys via RespClient and swaps a Snapshot into this object.
// PreTradeChecker consults the snapshot on EVERY order admission —
// the check itself is a shared_ptr load + hash lookups, zero heap.
//
// Fail closed (spec §2.7):
//   * no snapshot ever applied (refresher never succeeded)  -> reject
//   * the refresher's last poll errored (mark_unverifiable)  -> reject
//   * a bound-but-null SuspensionFlags is NOT an error: engines built
//     without Redis wiring (unit tests, the detached stub path) keep the
//     pre-Task-11.3.4 behavior — enforcement then lives entirely on the
//     Go admission gates, which fail closed.
//
// Precedence (first match wins, mirroring the Go resolver):
//   GLOBAL handled as the envelope — checked first because the flag
//   list is unordered; then ACCOUNT -> COUNTERPARTY -> FIX_SESSION ->
//   INSTRUMENT -> INSTRUMENT_CLASS -> DESK -> REGION -> ENV.

#include <atomic>
#include <cstdint>
#include <memory>
#include <shared_mutex>
#include <string>
#include <unordered_set>

namespace exch {

class SuspensionFlags {
public:
    // Snapshot is immutable once published; readers hold it via
    // shared_ptr so the refresher can swap concurrently without locks
    // on the check path.
    struct Snapshot {
        bool unverifiable = false;  // last refresh failed -> fail closed
        bool global = false;
        std::unordered_set<uint64_t> accounts;       // halt:account:{id}
        std::unordered_set<uint64_t> counterparties; // halt:counterparty:{id}
        std::unordered_set<std::string> fix_sessions;  // halt:fixsession:{id}
        std::unordered_set<std::string> instruments;   // halt:instrument:{SYM}
        std::unordered_set<std::string> classes;       // halt:instrumentclass:{T}
        std::unordered_set<std::string> desks;         // halt:desk:{id}
        std::unordered_set<std::string> regions;       // halt:region:{R}
        std::unordered_set<std::string> envs;          // halt:env:{E}
    };

    SuspensionFlags() = default;

    // Control path — publish a freshly polled flag set. Passing nullptr
    // (or a snapshot with unverifiable set) puts the checker into the
    // fail-closed state. The shared_mutex only guards the pointer swap;
    // the hot-path read is a single uncontended shared lock (~tens of
    // ns, well inside the checker's <10µs budget).
    void apply(std::shared_ptr<const Snapshot> snap) noexcept {
        std::lock_guard<std::shared_mutex> lk(mu_);
        snap_ = std::move(snap);
        ready_.store(true, std::memory_order_release);
    }

    // Mark the flag store unreadable — every subsequent check rejects
    // until a successful apply() lands (strictest-safe, spec §2.7).
    void mark_unverifiable() noexcept {
        auto s = std::make_shared<Snapshot>();
        s->unverifiable = true;
        apply(std::move(s));
    }

    // True once any snapshot (even an "all clear" one) has landed.
    [[nodiscard]] bool ready() const noexcept {
        return ready_.load(std::memory_order_acquire);
    }

    // Hot path — returns the winning scope's static label, or nullptr
    // when trading is open for this order. `symbol`/`cls` may be empty
    // strings (dimension skipped). Session is not evaluated here (the
    // core's Order carries no session id — FIX-session kills are
    // enforced at the gateway per Task 11.3.8 step 2).
    [[nodiscard]] const char* scope_for(uint64_t account_id,
                                        const char* symbol,
                                        const char* cls) const noexcept;

private:
    mutable std::shared_mutex mu_;
    std::shared_ptr<const Snapshot> snap_{};
    std::atomic<bool> ready_{false};
};

// Parse one `halt:*` key into a Snapshot. Returns false for keys that
// do not belong to the kill-switch namespace or carry a malformed
// target (they are ignored — a foreign key must never suspend trading).
[[nodiscard]] bool suspension_key_into(std::string_view key,
                                       SuspensionFlags::Snapshot* out) noexcept;

}  // namespace exch
