// PHASE-02 STUB (Task 2.3.3) — 14-check pipeline lands there.
#include "risk/PreTradeChecker.hpp"

namespace exch {

RiskDecision PreTradeChecker::check(const Order& /*order*/) const noexcept {
    return RiskDecision::ACCEPT;
}

}  // namespace exch
