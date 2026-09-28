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
// Mode dispatch is table-driven. Fail-closed default: any unhandled/unknown
// mode resolves to CANCEL_TAKER — prevention is never silently disabled.
//
// StpMode::NONE (Task 2.3.16, spec §6.5/§24 #274): maps to PROCEED — the
// "no prevention" mode is legal only for PROFESSIONAL/ELIGIBLE_COUNTERPARTY
// clients, gated at admission by PreTradeChecker check 14
// (STP_NONE_NOT_PERMITTED). The engine trusts the admission-stamped mode;
// an unstamped NONE (no risk hook bound) still proceeds by design — hook
// binding is the deployment contract — while every resulting self-fill is
// flagged to the SELF_TRADE surveillance sink for Phase-17 wash-trading
// monitoring.
//
// TRANSFER (Task 2.3.18, spec §6.5 remediation note): deliberately NOT an
// StpMode value — it is a behavioral mode implemented atop DECREMENT plus a
// both-sides request flag (kOrderFlagStpTransfer). The mutual flag is the
// single exception to taker-mode authority: when BOTH sides request
// TRANSFER the pair resolves to StpAction::TRANSFER regardless of the
// taker's otherwise-authoritative mode; a taker-only request falls back to
// the taker's resolved mode verbatim. The engine executes TRANSFER as
// DECREMENT and, for cross-account group matches only, additionally
// journals a PREVENTED_MATCH audit record (WalPreventedMatchPayload) — the
// Phase-03 GL service owns the balanced posting, the engine never writes
// PostgreSQL.

#include <cstdint>

#include "book/Order.hpp"

namespace exch {

// Order::flags bit3 — "TRANSFER requested" marker (Task 2.3.18, spec §6.5).
// Rides the existing wire `flags` ubyte (proto/exchange.fbs OrderNew.flags
// passes through verbatim in EnginePump); bits 0/1 are
// kOrderFlagPostOnly/kOrderFlagReduceOnly (book/Order.hpp), bit2 is the
// engine-internal MARKET_WITH_PROTECTION stamp (matching/MatchingEngine.hpp).
inline constexpr uint8_t kOrderFlagStpTransfer = 1u << 3;

// What the engine must do when a self-match is detected.
enum class StpAction : uint8_t {
    PROCEED = 0,    // no prevention (Task 2.3.16 NONE, Professional/ECP only)
    CANCEL_TAKER,   // CANCEL_NEWEST / CANCEL_SELF — kill incoming remainder
    CANCEL_MAKER,   // CANCEL_OLDEST — cancel resting order, taker proceeds
    CANCEL_BOTH,    // cancel maker + taker remainder
    DECREMENT,      // reduce resting by min(maker_rem, taker_rem); taker
                    // remainder is then cancelled (spec §6.5 literal)
    TRANSFER,       // Task 2.3.18 — DECREMENT semantics + PREVENTED_MATCH
                    // journal when maker/taker are distinct accounts in one
                    // trade group; same-account resolves to plain DECREMENT
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
    // CANCEL_TAKER (== CANCEL_NEWEST default, spec §24 #4). NONE -> PROCEED
    // is safe here precisely because check 14 rejects retail NONE upstream
    // (kStpModeUnset/unreachable values never arrive as NONE).
    [[nodiscard]] static constexpr StpAction action(StpMode mode) noexcept {
        // Index order matches the spec §5.4 StpMode enum verbatim.
        constexpr StpAction dispatch[] = {
            /* CANCEL_NEWEST = 0 */ StpAction::CANCEL_TAKER,
            /* CANCEL_OLDEST = 1 */ StpAction::CANCEL_MAKER,
            /* CANCEL_BOTH   = 2 */ StpAction::CANCEL_BOTH,
            /* DECREMENT     = 3 */ StpAction::DECREMENT,
            /* NONE          = 4 */ StpAction::PROCEED,
        };
        const auto idx = static_cast<uint8_t>(mode);
        return idx < sizeof(dispatch) ? dispatch[idx] : StpAction::CANCEL_TAKER;
    }

    // Task 2.3.18 dispatch: TRANSFER is selected only when BOTH sides carry
    // kOrderFlagStpTransfer — the one exception to taker-mode authority
    // (spec §6.5: TRANSFER "requires both maker and taker to request
    // TRANSFER"). A taker-only request falls back to the taker's resolved
    // mode verbatim.
    [[nodiscard]] static constexpr StpAction action(
        StpMode mode, bool taker_transfer, bool maker_transfer) noexcept {
        if (taker_transfer && maker_transfer) return StpAction::TRANSFER;
        return action(mode);
    }

    [[nodiscard]] static const char* action_name(StpAction a) noexcept;
};

}  // namespace exch
