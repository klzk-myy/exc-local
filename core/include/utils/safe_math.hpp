#pragma once

// Canonical checked-arithmetic facade (Task 1.3.12, spec §3.6.2). Wraps the
// compiler intrinsics (__builtin_add_overflow / __builtin_sub_overflow /
// __builtin_mul_overflow) for all 64-bit and 128-bit fixed-point operations.
// Two surfaces sharing one implementation:
//
//   try_*  — noexcept, non-allocating, hot-path safe. Returns false on
//            overflow/underflow and leaves `out` unmodified; the caller
//            aborts the mutation and returns ARITHMETIC_OVERFLOW_DETECTED
//            (HTTP 400) per spec §3.6.2.
//
//   add/sub/mul/neg — throwing surface for cold/boundary code. Throws
//            ArithmeticOverflowError, a std::overflow_error subclass
//            carrying code() == ARITHMETIC_OVERFLOW_DETECTED and
//            severity() == Severity::L2, so existing catchers of
//            std::overflow_error keep working (CheckedMath.hpp delegates
//            here — intrinsics must not be duplicated elsewhere).

#include <concepts>
#include <cstdint>
#include <stdexcept>
#include <type_traits>

#include "utils/error_severity.hpp"

namespace exch::safe_math {

// Widening intermediates: __int128 is a GNU extension (ISO C++ has no i128;
// std::integral does not cover it under -std=c++20 strict mode).
#pragma GCC diagnostic push
#pragma GCC diagnostic ignored "-Wpedantic"

using int128_t = __int128;
using uint128_t = unsigned __int128;

// CheckedInt covers the fixed-point mantissa types used for prices,
// quantities and cumulative notionals: all standard integer types
// (int64_t/uint64_t wire values) plus the __int128 intermediates of Decimal
// multiply/divide — std::integral excludes __int128 under strict -std=c++20.
template <typename T>
concept CheckedInt = std::integral<T> || std::is_same_v<T, int128_t> ||
                     std::is_same_v<T, uint128_t>;

// --- Non-throwing surface (hot path) --------------------------------------

// Note: the __builtin_*_overflow intrinsics store the wrapped result into
// the destination even on overflow, so each try_* computes into a scratch
// and only writes `out` on success — a caller that ignores the return value
// can never observe a wrapped value (fail-closed, spec §2.7.1).

template <CheckedInt T>
[[nodiscard]] inline bool try_add(T a, T b, T& out) noexcept {
    T r;
    if (__builtin_add_overflow(a, b, &r)) return false;
    out = r;
    return true;
}

template <CheckedInt T>
[[nodiscard]] inline bool try_sub(T a, T b, T& out) noexcept {
    T r;
    if (__builtin_sub_overflow(a, b, &r)) return false;
    out = r;
    return true;
}

template <CheckedInt T>
[[nodiscard]] inline bool try_mul(T a, T b, T& out) noexcept {
    T r;
    if (__builtin_mul_overflow(a, b, &r)) return false;
    out = r;
    return true;
}

// Checked negation — rejects INT64_MIN (and any unsigned non-zero, whose
// negation is unrepresentable → underflow per spec §3.6.2).
template <CheckedInt T>
[[nodiscard]] inline bool try_neg(T a, T& out) noexcept {
    T r;
    if (__builtin_sub_overflow(T{0}, a, &r)) return false;
    out = r;
    return true;
}

// Widening signed multiply: computes a*b in 128 bits and narrows to int64.
// Equivalent to try_mul<int64_t> but makes the widening intent explicit for
// cumulative-notional call sites (qty * price) per spec §3.6.2.
[[nodiscard]] inline bool try_mul_i64(int64_t a, int64_t b, int64_t& out) noexcept {
    return try_mul(a, b, out);
}

// Widening signed multiply keeping the 128-bit product (no narrowing).
[[nodiscard]] inline int128_t mul_wide_i64(int64_t a, int64_t b) noexcept {
    return static_cast<int128_t>(a) * static_cast<int128_t>(b);
}

// Checked narrowing of a 128-bit intermediate to int64.
[[nodiscard]] inline bool try_narrow_i128(int128_t v, int64_t& out) noexcept {
    if (v > static_cast<int128_t>(INT64_MAX) ||
        v < static_cast<int128_t>(INT64_MIN)) {
        return false;
    }
    out = static_cast<int64_t>(v);
    return true;
}

#pragma GCC diagnostic pop

// --- Throwing surface (cold/boundary path) --------------------------------

// Overflow/underflow sentinel carrying the §23 code. Derives from
// std::overflow_error for backward compatibility with the Task 1.3.1
// CheckedMath.hpp contract and Decimal.cpp catch sites.
class ArithmeticOverflowError : public std::overflow_error {
public:
    explicit ArithmeticOverflowError(const char* op)
        : std::overflow_error(op) {}

    [[nodiscard]] const char* code() const noexcept {
        return kCodeArithmeticOverflowDetected;
    }
    [[nodiscard]] Severity severity() const noexcept { return Severity::L2; }
    [[nodiscard]] int http_status() const noexcept { return 400; }
};

template <CheckedInt T>
[[nodiscard]] inline T add(T a, T b) {
    T r;
    if (__builtin_add_overflow(a, b, &r))
        throw ArithmeticOverflowError("safe_math::add");
    return r;
}

template <CheckedInt T>
[[nodiscard]] inline T sub(T a, T b) {
    T r;
    if (__builtin_sub_overflow(a, b, &r))
        throw ArithmeticOverflowError("safe_math::sub");
    return r;
}

template <CheckedInt T>
[[nodiscard]] inline T mul(T a, T b) {
    T r;
    if (__builtin_mul_overflow(a, b, &r))
        throw ArithmeticOverflowError("safe_math::mul");
    return r;
}

template <CheckedInt T>
[[nodiscard]] inline T neg(T a) {
    T r;
    if (__builtin_sub_overflow(T{0}, a, &r))
        throw ArithmeticOverflowError("safe_math::neg");
    return r;
}

}  // namespace exch::safe_math
