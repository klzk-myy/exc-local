// Phase-11 Task 11.3.4 step 4 / Task 11.3.12 step 2 — control-path
// Redis poll that keeps the C++ core's SuspensionFlags snapshot current.
//
// The Go control plane owns `halt:*` writes; this refresher reads the
// keyspace back with `KEYS halt:*` on a control cadence (the matching
// thread NEVER blocks on Redis — it reads the last applied snapshot).
// `KEYS` is safe here: the `halt:*` namespace is operator-sized (a
// handful of flags), not user-sized.
//
// Failure semantics (spec §2.7 strict fail-closed): any transport or
// parse error marks the flags unverifiable — every subsequent order
// admission rejects TRADING_HALTED until a clean poll lands. A halted
// venue that cannot see its own flags must not keep matching.

#include "risk/SuspensionRefresher.hpp"

namespace exch {

SuspensionRefresher::SuspensionRefresher(RespClient* client) noexcept
    : client_(client) {}

bool SuspensionRefresher::refresh(SuspensionFlags* flags) noexcept {
    if (flags == nullptr) return false;
    auto snap = std::make_shared<SuspensionFlags::Snapshot>();
    if (client_ == nullptr) {
        flags->mark_unverifiable();
        return false;
    }
    RespValue v;
    if (!client_->execute({"KEYS", "halt:*"}, &v) || v.is_error() ||
        v.type != RespValue::Type::Array) {
        flags->mark_unverifiable();
        return false;
    }
    for (const RespValue& item : v.items) {
        std::string_view key;
        if (!item.as_string(&key)) continue;
        // Unknown/malformed keys are ignored inside suspension_key_into —
        // they never suspend; a transport-level failure above already
        // marked the set unverifiable.
        (void)suspension_key_into(key, snap.get());
    }
    // Phase-13 Task 13.3.6 — MiFID II RTS 9 OTR breach flags. The Go
    // OtrMonitor owns `otr:breach:{account}` writes; the same
    // operator-sized keyspace discipline applies (a breached account is
    // one flag). A failed poll is treated exactly like a halt-side
    // failure: unverifiable => every admission rejects until a clean
    // refresh — the engine must not keep matching for an account whose
    // ratio state it cannot see (spec §2.7).
    RespValue ov;
    if (!client_->execute({"KEYS", "otr:breach:*"}, &ov) || ov.is_error() ||
        ov.type != RespValue::Type::Array) {
        flags->mark_unverifiable();
        return false;
    }
    for (const RespValue& item : ov.items) {
        std::string_view key;
        if (!item.as_string(&key)) continue;
        // `otr:breach:index` and any other non-`{id}` target are ignored
        // inside otr_key_into — bookkeeping keys never suspend.
        (void)otr_key_into(key, snap.get());
    }
    flags->apply(std::move(snap));
    return true;
}

}  // namespace exch
