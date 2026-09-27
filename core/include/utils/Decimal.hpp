#pragma once

// Fixed-point decimal: int64 mantissa scaled by 10^8 (pipette units, spec §3,
// Task 1.3.1). Integer arithmetic only — no floating point in financial math.
// Multiplication/division use __int128 intermediates; all ops throw
// std::overflow_error on overflow (spec §3.6.2 checked-arithmetic invariant).

#include <compare>
#include <cstdint>
#include <string>
#include <string_view>
#include <type_traits>

namespace exch {

class Decimal {
public:
    static constexpr int64_t SCALE = 100'000'000;  // 10^8 pipette units
    static constexpr int SCALE_DIGITS = 8;

    constexpr Decimal() noexcept = default;

    [[nodiscard]] static constexpr Decimal from_mantissa(int64_t mantissa) noexcept {
        return Decimal{mantissa};
    }
    [[nodiscard]] static Decimal from_int(int64_t whole);                // whole * SCALE, checked
    [[nodiscard]] static Decimal from_string(std::string_view text);     // strict; throws on bad input

    [[nodiscard]] constexpr int64_t mantissa() const noexcept { return mantissa_; }
    [[nodiscard]] constexpr bool is_zero() const noexcept { return mantissa_ == 0; }
    [[nodiscard]] constexpr int sign() const noexcept { return (mantissa_ > 0) - (mantissa_ < 0); }

    [[nodiscard]] Decimal abs() const;
    [[nodiscard]] Decimal operator-() const;  // checked negation (INT64_MIN throws)
    [[nodiscard]] std::string to_string() const;

    [[nodiscard]] Decimal operator+(Decimal rhs) const;
    [[nodiscard]] Decimal operator-(Decimal rhs) const;
    [[nodiscard]] Decimal operator*(Decimal rhs) const;  // truncates toward zero
    [[nodiscard]] Decimal operator/(Decimal rhs) const;  // truncates toward zero; throws on /0
    Decimal& operator+=(Decimal rhs);
    Decimal& operator-=(Decimal rhs);
    Decimal& operator*=(Decimal rhs);
    Decimal& operator/=(Decimal rhs);

    constexpr bool operator==(const Decimal&) const = default;
    constexpr auto operator<=>(const Decimal&) const = default;

    [[nodiscard]] static constexpr Decimal zero() noexcept { return from_mantissa(0); }
    [[nodiscard]] static constexpr Decimal one() noexcept { return from_mantissa(SCALE); }

private:
    explicit constexpr Decimal(int64_t mantissa) noexcept : mantissa_(mantissa) {}
    int64_t mantissa_ = 0;
};

static_assert(std::is_trivially_copyable_v<Decimal>);
static_assert(std::is_standard_layout_v<Decimal>);
static_assert(sizeof(Decimal) == sizeof(int64_t));

}  // namespace exch
