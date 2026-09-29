// Phase-11 Task 11.3.4/11.3.8/11.3.12 — see header for the contract.

#include "risk/SuspensionFlags.hpp"

#include <charconv>

namespace exch {

namespace {

// Rejection detail strings — static storage, safe to hand to the verdict
// detail slot (the code is always TRADING_HALTED; detail names the scope).
constexpr char kScopeGlobal[] = "GLOBAL kill-switch active";
constexpr char kScopeAccount[] = "ACCOUNT kill-switch active";
constexpr char kScopeCounterparty[] = "COUNTERPARTY kill-switch active";
constexpr char kScopeInstrument[] = "INSTRUMENT kill-switch active";
constexpr char kScopeClass[] = "INSTRUMENT_CLASS kill-switch active";
constexpr char kScopeDesk[] = "DESK kill-switch active";
constexpr char kScopeRegion[] = "REGION kill-switch active";
constexpr char kScopeEnv[] = "ENV kill-switch active";
constexpr char kScopeUnverifiable[] =
    "kill-switch state unverifiable — fail closed";

[[nodiscard]] bool parse_u64(std::string_view s, uint64_t* out) noexcept {
    if (s.empty() || out == nullptr) return false;
    uint64_t v = 0;
    const char* b = s.data();
    const char* e = b + s.size();
    const auto r = std::from_chars(b, e, v);
    if (r.ec != std::errc{} || r.ptr != e || v == 0) return false;
    *out = v;
    return true;
}

[[nodiscard]] std::string_view key_body(std::string_view key) noexcept {
    // "halt:<scope>:<target>" -> "<scope>:<target>"; "halt:global" -> "global".
    constexpr std::string_view kPrefix = "halt:";
    if (key.size() <= kPrefix.size() || key.substr(0, kPrefix.size()) != kPrefix)
        return {};
    return key.substr(kPrefix.size());
}

[[nodiscard]] std::pair<std::string_view, std::string_view> split_scope(
    std::string_view body) noexcept {
    const std::size_t pos = body.find(':');
    if (pos == std::string_view::npos) return {body, {}};
    return {body.substr(0, pos), body.substr(pos + 1)};
}

}  // namespace

bool suspension_key_into(std::string_view key,
                         SuspensionFlags::Snapshot* out) noexcept {
    if (out == nullptr) return false;
    const std::string_view body = key_body(key);
    if (body.empty()) return false;
    const auto [scope, target] = split_scope(body);

    if (scope == "global") {
        out->global = true;
        return true;
    }
    if (target.empty()) return false;

    uint64_t id = 0;
    if (scope == "account") {
        if (!parse_u64(target, &id)) return false;
        out->accounts.insert(id);
        return true;
    }
    if (scope == "counterparty") {
        // Task 11.3.12: the COUNTERPARTY target is an institutional
        // account/user id — same axis as ACCOUNT on the order path.
        if (!parse_u64(target, &id)) return false;
        out->counterparties.insert(id);
        return true;
    }
    if (scope == "fix_session") {
        out->fix_sessions.emplace(target);
        return true;
    }
    if (scope == "instrument") {
        out->instruments.emplace(target);
        return true;
    }
    if (scope == "instrument_class") {
        out->classes.emplace(target);
        return true;
    }
    if (scope == "desk") {
        out->desks.emplace(target);
        return true;
    }
    if (scope == "region") {
        out->regions.emplace(target);
        return true;
    }
    if (scope == "env") {
        out->envs.emplace(target);
        return true;
    }
    // LP / RAIL are not order-path dimensions — the LP gate lives at the
    // FIX quote-ingress layer and the rail gate in the funding service.
    return false;
}

const char* SuspensionFlags::scope_for(uint64_t account_id,
                                       const char* symbol,
                                       const char* cls) const noexcept {
    std::shared_lock<std::shared_mutex> lk(mu_);
    const std::shared_ptr<const Snapshot> s = snap_;
    if (s == nullptr) {
        // Bound but never refreshed — the halt state is unverifiable,
        // so admission fails closed until the first successful poll.
        return kScopeUnverifiable;
    }
    if (s->unverifiable) return kScopeUnverifiable;
    if (s->global) return kScopeGlobal;
    if (account_id != 0) {
        if (s->accounts.find(account_id) != s->accounts.end())
            return kScopeAccount;
        if (s->counterparties.find(account_id) != s->counterparties.end())
            return kScopeCounterparty;
    }
    if (symbol != nullptr && symbol[0] != '\0' &&
        s->instruments.find(symbol) != s->instruments.end()) {
        return kScopeInstrument;
    }
    if (cls != nullptr && cls[0] != '\0' &&
        s->classes.find(cls) != s->classes.end()) {
        return kScopeClass;
    }
    return nullptr;
}

}  // namespace exch
