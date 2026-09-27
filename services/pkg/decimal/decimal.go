// Package decimal is the exchange-wide fixed-point decimal facade.
//
// Spec §5.3 names decimal.Decimal (shopspring/decimal) as the Go-side
// precision standard, so Decimal is a type ALIAS — call sites interoperate
// with shopspring natively and there is exactly one decimal type in the
// codebase. No float64 may be used for financial quantities.
package decimal

import "github.com/shopspring/decimal"

// Decimal is an arbitrary-precision decimal number.
type Decimal = decimal.Decimal

// Scale is the core engine's fixed-point scale: C++ order/price wire values
// are int64 quantities of 10^-8 "pipette" units (spec §5.3). Conversions
// between Go Decimals and core wire values use ScaleFactor.
const (
	Scale       int   = 8
	ScaleFactor int64 = 100_000_000 // 10^Scale
)

var (
	// Zero and One are the common constants.
	Zero = decimal.Zero
	One  = decimal.NewFromInt(1)
)

// NewFromString parses s (e.g. "1.23456789") into a Decimal.
func NewFromString(s string) (Decimal, error) { return decimal.NewFromString(s) }

// MustFromString parses s and panics on failure — for tests and constants only.
func MustFromString(s string) Decimal { return decimal.RequireFromString(s) }

// NewFromInt returns v as a Decimal.
func NewFromInt(v int64) Decimal { return decimal.NewFromInt(v) }

// NewFromScaled converts a core wire value (integer count of 10^-8 units)
// into a Decimal.
func NewFromScaled(scaled int64) Decimal {
	return decimal.New(scaled, -int32(Scale))
}

// Scaled converts d to the core wire representation: an int64 count of
// 10^-8 units, truncated toward zero beyond 8 decimal places.
func Scaled(d Decimal) int64 {
	return d.Shift(int32(Scale)).Truncate(0).IntPart()
}
