#pragma once

// Integration seam (Task 2.3.3 <-> Task 2.3.2): binds PreTradeChecker into
// MatchingEngine's set_risk_hook slot. The hook fn-pointer signature forbids
// captures, so state crosses via this binding struct in ctx.
//
//   EngineRiskBinding b{&checker, &instrument, &engine_now_ns};
//   engine.set_risk_hook(&engine_risk_check, &b);
//
// `now_ns_source` MUST point at the engine's logical clock (MatchingEngine::
// now_ns() — TIME_TICK-driven) so the checker never reads a wall clock and
// replay stays deterministic. `instrument` is the single-book binding; the
// multi-instrument shard model (spec §2.1, one engine hosting many books)
// swaps this for an instrument provider keyed by OrderAux.instrument_id —
// tracked as the book-registry refactor.

#include <cstdint>

#include "book/Instrument.hpp"
#include "risk/PreTradeChecker.hpp"

namespace exch {

struct EngineRiskBinding {
    PreTradeChecker* checker;
    const Instrument* instrument;
    const uint64_t* now_ns_source;  // points at engine's logical clock member
};

// MatchingEngine::risk_check_fn trampoline. nullptr = pass; else the check's
// rejection code, surfaced verbatim as the engine's last_reject().
[[nodiscard]] inline const char* engine_risk_check(void* ctx,
                                                   Order& order) noexcept {
    const auto* b = static_cast<const EngineRiskBinding*>(ctx);
    if (b == nullptr || b->checker == nullptr || b->now_ns_source == nullptr ||
        b->instrument == nullptr) {
        return "RISK_UNBOUND";  // fail closed — never silently skip checks
    }
    CheckContext cc{b->instrument, *b->now_ns_source};
    const RiskVerdict v = b->checker->check(order, cc);
    return v.pass ? nullptr : v.code;
}

}  // namespace exch
