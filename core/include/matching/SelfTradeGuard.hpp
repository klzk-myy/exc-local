#pragma once

// Task 2.3.2 — self-trade prevention evaluator (spec §6.5, §3.7.3,
// migration 072). Pure/stateless: the guard classifies a (taker, maker)
// pair and maps the resolved stp_mode to an action; the MatchingEngine owns
// every resulting mutation (WAL cancel/modify, book updates, publish).
//
// Self-trade condition: maker.account_id == taker.account_id, OR both
// trade_group_id values are non-zero and equal (accounts.trade_group_id,
// migration 072 — same group == self-trade).
//
// Mode dispatch is table-driven so Task 2.3.11's full behavior set drops in
// without touching call sites. Fail-closed default: any unhandled/unknown
// mode resolves to CANCEL_NEWEST — prevention is never silently disabled.
// (StpMode::NONE is mapped to CANCEL_TAKER here on purpose: the
// Professional/ECP gating that makes NONE legal is Task 2.3.16's; until it
// lands, "no prevention" must not be reachable from the wire.)

#include <cstdint>

#include "book/Order.hpp"

namespace exch {

// What the engine must do when a self-match is detected.
enum class StpAction : uint8_t {
    PROCEED = 0,    // no prevention (reserved for Task 2.3.16 NONE gating)
    CANCEL_TAKER,   // CANCEL_NEWEST / CANCEL_SELF — kill incoming remainder
    CANCEL_MAKER,   // CANCEL_OLDEST — cancel resting order, taker proceeds
    CANCEL_BOTH,    // cancel maker + taker remainder
    DECREMENT,      // reduce resting by min(maker_rem, taker_rem); taker
                    // remainder is then cancelled (spec §6.5 literal)
};

class SelfTradeGuard {
public:
    // Self-match predicate: same account, or same nonzero trade group.
    [[nodiscard]] static constexpr bool
    is_self_match(uint64_t taker_account_id, uint64_t maker_account_id,
                  uint32_t taker_group_id, uint32_t maker_group_id) noexcept {
        return taker_account_id == maker_account_id ||
               (taker_group_id != 0 && taker_group_id == maker_group_id);
    }

    // Convenience overload for resting makers whose group the engine
    // resolved through its order-meta index.
    [[nodiscard]] static constexpr bool
    is_self_match(const Order& taker, const Order& maker,
                  uint32_t taker_group_id, uint32_t maker_group_id) noexcept {
        return is_self_match(taker.account_id, maker.account_id,
                             taker_group_id, maker_group_id);
    }

    // Table-driven mode -> action. Out-of-enum values fail closed to
    // CANCEL_TAKER (== CANCEL_NEWEST default, spec §24 #4).
    [[nodiscard]] static constexpr StpAction action(StpMode mode) noexcept {
        // Index order matches the spec §5.4 StpMode enum verbatim.
        constexpr StpAction dispatch[] = {
            /* CANCEL_NEWEST = 0 */ StpAction::CANCEL_TAKER,
            /* CANCEL_OLDEST = 1 */ StpAction::CANCEL_MAKER,
            /* CANCEL_BOTH   = 2 */ StpAction::CANCEL_BOTH,
            /* DECREMENT     = 3 */ StpAction::DECREMENT,
            /* NONE          = 4 */ StpAction::CANCEL_TAKER,
        };
        const auto idx = static_cast<uint8_t>(mode);
        return idx < sizeof(dispatch) ? dispatch[idx] : StpAction::CANCEL_TAKER;
    }

    [[nodiscard]] static const char* action_name(StpAction a) noexcept;
};

}  // namespace exch
