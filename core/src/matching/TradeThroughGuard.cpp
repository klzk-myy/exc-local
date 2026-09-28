// Task 2.3.22 — TradeThroughGuard cold-path definitions (names only).
// All hot-path logic is inline in matching/TradeThroughGuard.hpp — the
// component is value/atomic state plus POD verdicts; the only out-of-line
// members are diagnostic name strings used by tests, logs and the Phase-07
// observability surface.

#include "matching/TradeThroughGuard.hpp"

namespace exch {

const char* TradeThroughGuard::decision_name(TtDecision d) noexcept {
    switch (d) {
        case TtDecision::PASS:   return "PASS";
        case TtDecision::REJECT: return "REJECT";
        case TtDecision::CLIP:   return "CLIP";
    }
    return "UNKNOWN";
}

const char* TradeThroughGuard::event_kind_name(TtEventKind k) noexcept {
    switch (k) {
        case TtEventKind::LIMIT_TRADE_THROUGH_REJECTED:
            return "LIMIT_TRADE_THROUGH_REJECTED";
        case TtEventKind::IOC_FOK_NO_LIQUIDITY_REJECTED:
            return "IOC_FOK_NO_LIQUIDITY_REJECTED";
        case TtEventKind::MARKET_CLIPPED_TO_QUOTE:
            return "MARKET_CLIPPED_TO_QUOTE";
        case TtEventKind::MARKET_REMAINDER_SLIPPAGE:
            return "MARKET_REMAINDER_SLIPPAGE";
    }
    return "UNKNOWN";
}

}  // namespace exch
