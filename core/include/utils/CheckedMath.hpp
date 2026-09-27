#pragma once

// Checked-arithmetic helpers (Task 1.3.1). Task 1.3.12 consolidated the
// intrinsic implementations into the canonical facade utils/safe_math.hpp
// (spec §2.7, §3.6) — the exch::detail::checked_* names remain as thin
// wrappers for existing call sites; do not add new ones here.
// Contract: every helper throws safe_math::ArithmeticOverflowError — a
// std::overflow_error subclass whose code() == "ARITHMETIC_OVERFLOW_DETECTED"
// (spec §3.6.2) — on overflow; the caller aborts the mutation and maps the
// code to the structured error envelope.

#include <cstdint>
#include <stdexcept>

#include "utils/safe_math.hpp"

namespace exch::detail {

[[noreturn]] inline void throw_overflow(const char* op) {
    throw safe_math::ArithmeticOverflowError(op);
}

inline int64_t checked_add(int64_t a, int64_t b) {
    return safe_math::add(a, b);
}

inline int64_t checked_sub(int64_t a, int64_t b) {
    return safe_math::sub(a, b);
}

inline int64_t checked_mul(int64_t a, int64_t b) {
    return safe_math::mul(a, b);
}

inline int64_t checked_neg(int64_t a) {
    return safe_math::neg(a);
}

}  // namespace exch::detail
