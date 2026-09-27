#pragma once

// Minimal checked-arithmetic helpers (Task 1.3.1). Task 1.3.12 formalizes this
// into safe_math.hpp with the L0-L3 severity hierarchy (spec §2.7, §3.6).
// Contract: every helper throws std::overflow_error on overflow — the caller
// aborts the mutation and maps it to ARITHMETIC_OVERFLOW_DETECTED (spec §3.6.2).

#include <cstdint>
#include <stdexcept>

namespace exch::detail {

[[noreturn]] inline void throw_overflow(const char* op) {
    throw std::overflow_error(op);
}

inline int64_t checked_add(int64_t a, int64_t b) {
    int64_t r;
    if (__builtin_add_overflow(a, b, &r)) throw_overflow("checked_add");
    return r;
}

inline int64_t checked_sub(int64_t a, int64_t b) {
    int64_t r;
    if (__builtin_sub_overflow(a, b, &r)) throw_overflow("checked_sub");
    return r;
}

inline int64_t checked_mul(int64_t a, int64_t b) {
    int64_t r;
    if (__builtin_mul_overflow(a, b, &r)) throw_overflow("checked_mul");
    return r;
}

inline int64_t checked_neg(int64_t a) {
    int64_t r;
    if (__builtin_sub_overflow(0, a, &r)) throw_overflow("checked_neg");
    return r;
}

}  // namespace exch::detail
