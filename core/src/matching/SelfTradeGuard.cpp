// Task 2.3.2 — SelfTradeGuard diagnostics; the predicate + dispatch live
// inline in the header (hot path, zero call overhead under LTO/inlining).

#include "matching/SelfTradeGuard.hpp"

namespace exch {

const char* SelfTradeGuard::action_name(StpAction a) noexcept {
    switch (a) {
        case StpAction::PROCEED:      return "PROCEED";
        case StpAction::CANCEL_TAKER: return "CANCEL_TAKER";
        case StpAction::CANCEL_MAKER: return "CANCEL_MAKER";
        case StpAction::CANCEL_BOTH:  return "CANCEL_BOTH";
        case StpAction::DECREMENT:    return "DECREMENT";
        case StpAction::TRANSFER:     return "TRANSFER";
    }
    return "?";
}

}  // namespace exch
