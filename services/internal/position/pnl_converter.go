// Package position — Task 3.3.9: multi-currency P&L conversion.
//
// Raw position P&L is computed in the instrument's QUOTE currency
// (spec §13.1/§16.4: EUR/GBP P&L is GBP; USD/JPY P&L is JPY). For equity,
// margin utilization and base-ledger settlement it is converted to the
// account's base_currency via the PriceOracle mark mid-rate (Phase-19.5
// owns the oracle; FXRateProvider is the seam).
//
// Conversion resolution order (fail-closed, spec §2.7):
//  1. identity (from == to)
//  2. direct pair from/to            → rate = mid(from,to)
//  3. inverted pair to/from          → rate = 1 / mid(to,from)
//  4. cross via USD numeraire        → rate = rate(from,USD) * rate(USD,to)
//
// Any non-positive mark aborts with MARK_PRICE_OUT_OF_BOUNDS; when no path
// resolves the conversion aborts with PRICE_ORACLE_UNAVAILABLE. Rates are
// never guessed or defaulted.
package position

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Error codes emitted here; canonical HTTP-status registration lands in
// the Phase-05 Task 5.3.21 registry (spec §23 names reused).
const (
	CodeMarkPriceOutOfBounds = "MARK_PRICE_OUT_OF_BOUNDS" // HTTP 400, L2
	CodeOracleUnavailable    = "PRICE_ORACLE_UNAVAILABLE" // HTTP 503, L1
)

// ErrPairNotFound is returned by FXRateProvider when the ordered pair has
// no instrument/oracle coverage.
var ErrPairNotFound = errors.New("fx pair not found")

// PivotCurrency is the cross-conversion numeraire (spec §13.1 USD
// numeraire normalization).
const PivotCurrency = "USD"

// Pair is an ordered currency pair: MidRate returns units of Quote per
// 1 unit of Base.
type Pair struct {
	Base  string
	Quote string
}

func (p Pair) String() string { return p.Base + "/" + p.Quote }

// FXRateProvider supplies mark mid-rates — the Phase-19.5 PriceOracle seam.
// rate = quote currency units per 1 base unit. Implementations must return
// ErrPairNotFound for unknown pairs and a non-positive rate is treated as
// out-of-bounds by the Converter (fail-closed).
type FXRateProvider interface {
	MidRate(ctx context.Context, pair Pair) (decimal.Decimal, error)
}

// BatchRateProvider is an optional provider extension for O(N) batch
// loading (spec §13.1: no N+1 queries across mark prices). ConvertAll uses
// it when the concrete provider implements it.
type BatchRateProvider interface {
	MidRates(ctx context.Context, pairs []Pair) (map[Pair]decimal.Decimal, error)
}

// Conversion records one executed conversion for the audit/journal layer.
// NOTE: ToAmount and Rate carry full decimal precision (inverse/cross
// rates can be non-terminating). The ledger layer rounds ToAmount to the
// schema's DECIMAL(28,8) at posting; Rate persists at DECIMAL(28,12).
type Conversion struct {
	FromCurrency string
	ToCurrency   string
	FromAmount   decimal.Decimal
	ToAmount     decimal.Decimal
	Rate         decimal.Decimal // effective ToCurrency per 1 FromCurrency
	// Path is the human-readable hop list, e.g. "EUR/USD" or
	// "USD/JPY^-1,GBP/USD" (suffixed ^-1 = inverted rate used).
	Path string
}

// Converter resolves FX rates between arbitrary currencies.
type Converter struct {
	rates FXRateProvider
	pivot string
}

// NewConverter builds a Converter. pivot == "" defaults to USD.
func NewConverter(rates FXRateProvider, pivot string) *Converter {
	if pivot == "" {
		pivot = PivotCurrency
	}
	return &Converter{rates: rates, pivot: pivot}
}

// Convert converts amount from one currency to another at the current
// mark mid-rate. Amount may be negative (P&L losses convert with sign).
func (c *Converter) Convert(ctx context.Context, amount decimal.Decimal, from, to string) (Conversion, error) {
	from, to = strings.ToUpper(strings.TrimSpace(from)), strings.ToUpper(strings.TrimSpace(to))
	if len(from) != 3 || len(to) != 3 {
		return Conversion{}, excerrors.New("INVALID_CURRENCY",
			fmt.Sprintf("bad ISO currency pair %q→%q", from, to))
	}
	rate, path, err := c.rate(ctx, from, to)
	if err != nil {
		return Conversion{}, err
	}
	return Conversion{
		FromCurrency: from,
		ToCurrency:   to,
		FromAmount:   amount,
		ToAmount:     amount.Mul(rate),
		Rate:         rate,
		Path:         path,
	}, nil
}

// Rate exposes the effective from→to mid-rate without converting an
// amount (used by reporting/equity paths).
func (c *Converter) Rate(ctx context.Context, from, to string) (decimal.Decimal, string, error) {
	return c.rate(ctx, strings.ToUpper(from), strings.ToUpper(to))
}

// ConvertAll converts each amount to `to` in O(N) oracle lookups: when the
// provider implements BatchRateProvider all required pairs are resolved in
// one batch; otherwise results are cached per-pair inside the call.
func (c *Converter) ConvertAll(ctx context.Context, amounts []decimal.Decimal, from, to string) ([]Conversion, error) {
	cache := &cachedRates{inner: c.rates, memo: map[Pair]decimal.Decimal{}}
	if bp, ok := c.rates.(BatchRateProvider); ok {
		m, err := bp.MidRates(ctx, c.requiredPairs(from, to))
		if err != nil {
			return nil, fmt.Errorf("batch mid-rate prefetch: %w", err)
		}
		for p, r := range m {
			cache.memo[p] = r
		}
	}
	sub := &Converter{rates: cache, pivot: c.pivot}
	out := make([]Conversion, len(amounts))
	for i, a := range amounts {
		cv, err := sub.Convert(ctx, a, from, to)
		if err != nil {
			return nil, err
		}
		out[i] = cv
	}
	return out, nil
}

// rate resolves the effective from→to rate. All oracle errors except
// ErrPairNotFound abort immediately (fail-closed — a flaky oracle must not
// silently fall back to a different price path).
func (c *Converter) rate(ctx context.Context, from, to string) (decimal.Decimal, string, error) {
	if from == to {
		return decimal.One, "identity", nil
	}
	// direct
	if r, err := c.mid(ctx, Pair{from, to}); err == nil {
		return r, Pair{from, to}.String(), nil
	} else if !errors.Is(err, ErrPairNotFound) {
		return decimal.Zero, "", err
	}
	// inverted
	if r, err := c.mid(ctx, Pair{to, from}); err == nil {
		return decimal.One.Div(r), Pair{to, from}.String() + "^-1", nil
	} else if !errors.Is(err, ErrPairNotFound) {
		return decimal.Zero, "", err
	}
	// cross via pivot numeraire
	if from != c.pivot && to != c.pivot {
		r1, p1, err1 := c.leg(ctx, from, c.pivot)
		r2, p2, err2 := c.leg(ctx, c.pivot, to)
		if err1 == nil && err2 == nil {
			return r1.Mul(r2), p1 + "," + p2, nil
		}
		for _, e := range []error{err1, err2} {
			if e != nil && !errors.Is(e, ErrPairNotFound) {
				return decimal.Zero, "", e
			}
		}
	}
	return decimal.Zero, "", excerrors.New(CodeOracleUnavailable,
		fmt.Sprintf("no FX conversion path %s→%s (direct, inverse, %s-cross)", from, to, c.pivot))
}

// leg resolves one direct-or-inverted hop (no recursion).
func (c *Converter) leg(ctx context.Context, base, quote string) (decimal.Decimal, string, error) {
	if base == quote {
		return decimal.One, "identity", nil
	}
	if r, err := c.mid(ctx, Pair{base, quote}); err == nil {
		return r, Pair{base, quote}.String(), nil
	} else if !errors.Is(err, ErrPairNotFound) {
		return decimal.Zero, "", err
	}
	if r, err := c.mid(ctx, Pair{quote, base}); err == nil {
		return decimal.One.Div(r), Pair{quote, base}.String() + "^-1", nil
	} else if !errors.Is(err, ErrPairNotFound) {
		return decimal.Zero, "", err
	}
	return decimal.Zero, "", ErrPairNotFound
}

// mid fetches and validates a mid-rate: non-positive marks are
// out-of-bounds and abort fail-closed.
func (c *Converter) mid(ctx context.Context, p Pair) (decimal.Decimal, error) {
	r, err := c.rates.MidRate(ctx, p)
	if err != nil {
		return decimal.Zero, err
	}
	if !r.IsPositive() {
		return decimal.Zero, excerrors.New(CodeMarkPriceOutOfBounds,
			fmt.Sprintf("non-positive mark mid-rate %s for %s", r, p))
	}
	return r, nil
}

// requiredPairs lists the oracle lookups a from→to conversion may need
// (direct, inverse, and both pivot legs) so batch providers can prefetch.
func (c *Converter) requiredPairs(from, to string) []Pair {
	pairs := []Pair{{from, to}, {to, from}}
	if from != c.pivot && to != c.pivot {
		pairs = append(pairs,
			Pair{from, c.pivot}, Pair{c.pivot, from},
			Pair{c.pivot, to}, Pair{to, c.pivot})
	}
	return pairs
}

// cachedRates memoizes MidRate within one batch conversion (no N+1).
type cachedRates struct {
	inner FXRateProvider
	memo  map[Pair]decimal.Decimal
}

func (c *cachedRates) MidRate(ctx context.Context, p Pair) (decimal.Decimal, error) {
	if v, ok := c.memo[p]; ok {
		return v, nil
	}
	v, err := c.inner.MidRate(ctx, p)
	if err != nil {
		return decimal.Zero, err
	}
	c.memo[p] = v
	return v, nil
}
