#include <gtest/gtest.h>

#include <cstdint>
#include <stdexcept>

#include "utils/Decimal.hpp"

using exch::Decimal;

TEST(Decimal, ScaleIsPipette) {
    EXPECT_EQ(Decimal::SCALE, 100'000'000);
    EXPECT_EQ(Decimal::SCALE_DIGITS, 8);
    EXPECT_EQ(Decimal::from_mantissa(1).mantissa(), 1);          // 1e-8
    EXPECT_EQ(Decimal::one().mantissa(), Decimal::SCALE);
    EXPECT_TRUE(Decimal::zero().is_zero());
}

TEST(Decimal, FromInt) {
    EXPECT_EQ(Decimal::from_int(0).mantissa(), 0);
    EXPECT_EQ(Decimal::from_int(5).mantissa(), 500'000'000);
    EXPECT_EQ(Decimal::from_int(-3).mantissa(), -300'000'000);
    EXPECT_THROW((void)Decimal::from_int(INT64_MAX), std::overflow_error);
}

TEST(Decimal, FromString) {
    EXPECT_EQ(Decimal::from_string("0").mantissa(), 0);
    EXPECT_EQ(Decimal::from_string("1").mantissa(), Decimal::SCALE);
    EXPECT_EQ(Decimal::from_string("1.5").mantissa(), 150'000'000);
    EXPECT_EQ(Decimal::from_string("-1.5").mantissa(), -150'000'000);
    EXPECT_EQ(Decimal::from_string("+2.25").mantissa(), 225'000'000);
    EXPECT_EQ(Decimal::from_string(".5").mantissa(), 50'000'000);
    EXPECT_EQ(Decimal::from_string("0.00000001").mantissa(), 1);   // one pipette
    EXPECT_EQ(Decimal::from_string("123.45678901").mantissa(), 12'345'678'901);
    EXPECT_EQ(Decimal::from_string("1.").mantissa(), Decimal::SCALE);
    EXPECT_EQ(Decimal::from_string("-0").mantissa(), 0);
}

TEST(Decimal, FromStringRejects) {
    EXPECT_THROW((void)Decimal::from_string(""), std::invalid_argument);
    EXPECT_THROW((void)Decimal::from_string("abc"), std::invalid_argument);
    EXPECT_THROW((void)Decimal::from_string("1.2.3"), std::invalid_argument);
    EXPECT_THROW((void)Decimal::from_string("1x"), std::invalid_argument);
    EXPECT_THROW((void)Decimal::from_string("-"), std::invalid_argument);
    EXPECT_THROW((void)Decimal::from_string("."), std::invalid_argument);
    EXPECT_THROW((void)Decimal::from_string("1.000000001"), std::invalid_argument);  // >8 dp
    EXPECT_THROW((void)Decimal::from_string("1,5"), std::invalid_argument);
    EXPECT_THROW((void)Decimal::from_string(" 1"), std::invalid_argument);
    EXPECT_THROW((void)Decimal::from_string("1 "), std::invalid_argument);
}

TEST(Decimal, FromStringOverflow) {
    EXPECT_THROW((void)Decimal::from_string("99999999999"), std::overflow_error);
    EXPECT_THROW((void)Decimal::from_string("9223372036854775807"), std::overflow_error);
    EXPECT_THROW((void)Decimal::from_string("-92233720368547758080"), std::overflow_error);
}

TEST(Decimal, AddSub) {
    EXPECT_EQ(Decimal::from_string("1.25") + Decimal::from_string("2.5"),
              Decimal::from_string("3.75"));
    EXPECT_EQ(Decimal::from_string("2.5") - Decimal::from_string("1.25"),
              Decimal::from_string("1.25"));
    EXPECT_EQ(Decimal::from_string("0.1") + Decimal::from_string("0.2"),
              Decimal::from_string("0.3"));  // exact — no float error
    EXPECT_TRUE((Decimal::one() - Decimal::one()).is_zero());
    // pipette precision carries through
    EXPECT_EQ(Decimal::from_mantissa(1) + Decimal::from_mantissa(1),
              Decimal::from_mantissa(2));
    Decimal d = Decimal::from_string("1.0");
    d += Decimal::from_string("0.5");
    d -= Decimal::from_string("0.25");
    EXPECT_EQ(d, Decimal::from_string("1.25"));
}

TEST(Decimal, Mul) {
    EXPECT_EQ(Decimal::from_string("1.5") * Decimal::from_int(2),
              Decimal::from_int(3));
    EXPECT_EQ(Decimal::from_string("2.5") * Decimal::from_string("0.4"),
              Decimal::from_int(1));
    EXPECT_EQ(Decimal::from_string("-1.5") * Decimal::from_int(2),
              Decimal::from_int(-3));
    // truncation toward zero below pipette precision
    EXPECT_EQ(Decimal::from_string("0.33333333") * Decimal::from_int(3),
              Decimal::from_string("0.99999999"));
    EXPECT_EQ(Decimal::from_mantissa(1) * Decimal::from_mantissa(1),
              Decimal::zero());  // 1e-8 * 1e-8 = 1e-16 -> truncates to 0
    Decimal d = Decimal::from_string("1.08400");  // EURUSD-ish
    d *= Decimal::from_int(100'000);
    EXPECT_EQ(d, Decimal::from_int(108'400));
}

TEST(Decimal, Div) {
    EXPECT_EQ(Decimal::from_int(3) / Decimal::from_int(2),
              Decimal::from_string("1.5"));
    EXPECT_EQ(Decimal::from_int(1) / Decimal::from_int(3),
              Decimal::from_string("0.33333333"));  // trunc toward zero
    EXPECT_EQ(Decimal::from_int(-1) / Decimal::from_int(3),
              Decimal::from_string("-0.33333333"));
    EXPECT_EQ(Decimal::from_string("1.5") / Decimal::from_string("0.5"),
              Decimal::from_int(3));
    Decimal d = Decimal::from_int(10);
    d /= Decimal::from_int(4);
    EXPECT_EQ(d, Decimal::from_string("2.5"));
}

TEST(Decimal, DivByZero) {
    EXPECT_THROW((void)(Decimal::one() / Decimal::zero()), std::domain_error);
    EXPECT_THROW((void)(Decimal::zero() / Decimal::zero()), std::domain_error);
}

TEST(Decimal, OverflowAborts) {
    const Decimal max = Decimal::from_mantissa(INT64_MAX);
    const Decimal min = Decimal::from_mantissa(INT64_MIN);
    EXPECT_THROW((void)(max + Decimal::one()), std::overflow_error);
    EXPECT_THROW((void)(min - Decimal::one()), std::overflow_error);
    EXPECT_THROW((void)(max * Decimal::from_int(2)), std::overflow_error);
    EXPECT_THROW((void)(min * Decimal::from_int(2)), std::overflow_error);
    EXPECT_THROW((void)(min / Decimal::from_mantissa(-1)), std::overflow_error);
    EXPECT_THROW((void)(-min), std::overflow_error);
    EXPECT_THROW((void)min.abs(), std::overflow_error);
    // boundaries that must NOT throw
    EXPECT_NO_THROW((void)(max + Decimal::zero()));
    EXPECT_NO_THROW((void)(max * Decimal::one()));
    EXPECT_EQ(max * Decimal::zero(), Decimal::zero());
}

TEST(Decimal, Compare) {
    const Decimal a = Decimal::from_string("1.25");
    const Decimal b = Decimal::from_string("1.5");
    const Decimal c = Decimal::from_string("1.25");
    EXPECT_TRUE(a < b);
    EXPECT_TRUE(b > a);
    EXPECT_TRUE(a <= c && a >= c);
    EXPECT_TRUE(a == c);
    EXPECT_TRUE(a != b);
    EXPECT_TRUE(Decimal::from_int(-1) < Decimal::zero());
    EXPECT_TRUE(Decimal::from_string("0.00000001") > Decimal::zero());
}

TEST(Decimal, ToStringRoundTrip) {
    for (const char* s : {"0", "1", "-1", "1.5", "-1.5", "0.00000001",
                          "-123.45678901", "1000000", "0.1", "3.14159265"}) {
        EXPECT_STREQ(Decimal::from_string(s).to_string().c_str(), s);
    }
    // INT64_MIN mantissa must not explode the magnitude path
    EXPECT_EQ(Decimal::from_mantissa(INT64_MIN).to_string(),
              "-92233720368.54775808");
    EXPECT_EQ(Decimal::from_mantissa(INT64_MAX).to_string(),
              "92233720368.54775807");
}
