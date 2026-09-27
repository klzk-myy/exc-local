#pragma once

// PHASE-02 STUB (Task 2.3.3) — 14 in-process pre-trade checks per spec §3.3
// (no IPC). SanctionsHook joins in Phase-21 (Task 21.3.10).

#include <cstdint>

#include "book/Order.hpp"

namespace exch {

enum class RiskDecision : uint8_t { ACCEPT, REJECT };

class PreTradeChecker {
public:
    PreTradeChecker() noexcept = default;

    // Stub accepts every order; Task 2.3.3 wires the 14 real checks
    // (checks 8/9/10 themselves start as Phase-2 placeholders per the plan).
    [[nodiscard]] RiskDecision check(const Order& order) const noexcept;
};

}  // namespace exch
