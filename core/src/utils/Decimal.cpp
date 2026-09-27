#include "utils/Decimal.hpp"

#include <cctype>
#include <cstdio>
#include <stdexcept>

#include "utils/CheckedMath.hpp"

namespace exch {

namespace {
constexpr int64_t kPow10[Decimal::SCALE_DIGITS + 1] = {
    1, 10, 100, 1'000, 10'000, 100'000, 1'000'000, 10'000'000, 100'000'000};

[[noreturn]] void throw_parse() {
    throw std::invalid_argument("Decimal::from_string");
}
}  // namespace

Decimal Decimal::from_int(int64_t whole) {
    return from_mantissa(detail::checked_mul(whole, SCALE));
}

Decimal Decimal::from_string(std::string_view text) {
    if (text.empty()) throw_parse();

    std::size_t i = 0;
    const bool neg = (text[0] == '-');
    if (text[0] == '-' || text[0] == '+') ++i;

    int64_t whole = 0;
    int64_t frac = 0;
    int frac_digits = 0;
    bool any = false;

    for (; i < text.size() && text[i] != '.'; ++i) {
        if (!std::isdigit(static_cast<unsigned char>(text[i]))) throw_parse();
        whole = detail::checked_add(detail::checked_mul(whole, 10), text[i] - '0');
        any = true;
    }
    if (i < text.size() && text[i] == '.') {
        ++i;
        for (; i < text.size(); ++i) {
            if (!std::isdigit(static_cast<unsigned char>(text[i]))) throw_parse();
            if (++frac_digits > SCALE_DIGITS) throw_parse();
            frac = detail::checked_add(detail::checked_mul(frac, 10), text[i] - '0');
            any = true;
        }
    }
    if (!any) throw_parse();

    int64_t mantissa = detail::checked_add(
        detail::checked_mul(whole, SCALE),
        detail::checked_mul(frac, kPow10[SCALE_DIGITS - frac_digits]));
    if (neg) mantissa = detail::checked_neg(mantissa);
    return from_mantissa(mantissa);
}

std::string Decimal::to_string() const {
    const bool neg = mantissa_ < 0;
    const uint64_t um =
        neg ? uint64_t{0} - static_cast<uint64_t>(mantissa_) : static_cast<uint64_t>(mantissa_);
    const uint64_t whole = um / static_cast<uint64_t>(SCALE);
    uint64_t frac = um % static_cast<uint64_t>(SCALE);

    char buf[40];
    char* p = buf;
    if (neg) *p++ = '-';
    p += std::snprintf(p, sizeof(buf) - static_cast<std::size_t>(p - buf), "%llu",
                       static_cast<unsigned long long>(whole));
    if (frac != 0) {
        char fbuf[SCALE_DIGITS + 1];
        std::snprintf(fbuf, sizeof(fbuf), "%08llu", static_cast<unsigned long long>(frac));
        int last = SCALE_DIGITS - 1;
        while (last > 0 && fbuf[last] == '0') --last;
        fbuf[last + 1] = '\0';
        *p++ = '.';
        for (int k = 0; k <= last; ++k) *p++ = fbuf[k];
    }
    *p = '\0';
    return std::string(buf, static_cast<std::size_t>(p - buf));
}

Decimal Decimal::abs() const {
    return mantissa_ < 0 ? from_mantissa(detail::checked_neg(mantissa_)) : *this;
}

Decimal Decimal::operator-() const {
    return from_mantissa(detail::checked_neg(mantissa_));
}

Decimal Decimal::operator+(Decimal rhs) const {
    return from_mantissa(detail::checked_add(mantissa_, rhs.mantissa_));
}

Decimal Decimal::operator-(Decimal rhs) const {
    return from_mantissa(detail::checked_sub(mantissa_, rhs.mantissa_));
}

// Widening intermediates: __int128 is a GNU extension (ISO C++ has no i128).
#pragma GCC diagnostic push
#pragma GCC diagnostic ignored "-Wpedantic"

Decimal Decimal::operator*(Decimal rhs) const {
    const __int128 product = static_cast<__int128>(mantissa_) * rhs.mantissa_;
    const __int128 q = product / SCALE;
    if (q > INT64_MAX || q < INT64_MIN) detail::throw_overflow("Decimal::operator*");
    return from_mantissa(static_cast<int64_t>(q));
}

Decimal Decimal::operator/(Decimal rhs) const {
    if (rhs.mantissa_ == 0) throw std::domain_error("Decimal: division by zero");
    const __int128 dividend = static_cast<__int128>(mantissa_) * SCALE;
    const __int128 q = dividend / rhs.mantissa_;
    if (q > INT64_MAX || q < INT64_MIN) detail::throw_overflow("Decimal::operator/");
    return from_mantissa(static_cast<int64_t>(q));
}

#pragma GCC diagnostic pop

Decimal& Decimal::operator+=(Decimal rhs) { return *this = *this + rhs; }
Decimal& Decimal::operator-=(Decimal rhs) { return *this = *this - rhs; }
Decimal& Decimal::operator*=(Decimal rhs) { return *this = *this * rhs; }
Decimal& Decimal::operator/=(Decimal rhs) { return *this = *this / rhs; }

}  // namespace exch
