// Package rates implements Task 19.5.3.5 — per-currency discount
// curves and Tom-Next swap points inside the PriceOracle.
//
// Canonical contract (spec §15.3, §24 #134):
//   - ≥7 tenors per currency: ON, T/N, 1W, 1M, 3M, 6M, 12M — off-tenor
//     dates interpolate log-linear between bracketing pillars.
//   - Day-count: ACT/360 for USD EUR CHF JPY; ACT/365 for GBP AUD NZD
//     CAD SGD HKD (spec §15.3 per-currency convention — supersedes the
//     prior hardcoded d/360).
//   - Publication: Redis curve:{ccy} JSON + forward points under
//     fwd_points:{pair}; 5s staleness gate identical to spot feeds —
//     a stale curve fails closed (YIELD_CURVE_UNAVAILABLE).
//   - Archive: ClickHouse curve snapshots daily (the ArchiveSink seam).
//
// Consumers: Phase-3 Task 3.3.7 Tom-Next rollover (swap points),
// Phase-22 Task 22.3.1 forward pricing (discount factors + day-count),
// Phase-22 Task 22.3.3 NDF fixing.
package rates

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Tenors & day-count conventions (spec §15.3)
// ---------------------------------------------------------------------------

// Tenor is a curve pillar. ON and TN are overnight / tomorrow-next;
// the rest are standard money-market pillars.
type Tenor string

const (
	TenorON  Tenor = "ON"
	TenorTN  Tenor = "TN"
	Tenor1W  Tenor = "1W"
	Tenor1M  Tenor = "1M"
	Tenor3M  Tenor = "3M"
	Tenor6M  Tenor = "6M"
	Tenor12M Tenor = "12M"
)

// CanonicalTenors is the ≥7-pillar contract in order.
var CanonicalTenors = []Tenor{TenorON, TenorTN, Tenor1W, Tenor1M, Tenor3M, Tenor6M, Tenor12M}

// tenorDays maps each pillar to its nominal day offset — the
// interpolation axis. T/N is 1 day; ON anchors t=0 with a 1-day rate.
var tenorDays = map[Tenor]int{
	TenorON: 0, TenorTN: 1, Tenor1W: 7,
	Tenor1M: 30, Tenor3M: 91, Tenor6M: 182, Tenor12M: 365,
}

// DayBasis is the per-currency accrual denominator.
type DayBasis int

const (
	Basis360 DayBasis = 360
	Basis365 DayBasis = 365
)

// basisByCcy is the spec §15.3 split — ACT/360: USD EUR CHF JPY;
// ACT/365: GBP AUD NZD CAD SGD HKD. Unlisted currencies default to
// ACT/360 (the dollar/money-market majority convention).
var basisByCcy = map[string]DayBasis{
	"GBP": Basis365, "AUD": Basis365, "NZD": Basis365,
	"CAD": Basis365, "SGD": Basis365, "HKD": Basis365,
}

// DayCount returns the currency's spec-§15.3 accrual basis.
func DayCount(ccy string) DayBasis {
	if b, ok := basisByCcy[strings.ToUpper(ccy)]; ok {
		return b
	}
	return Basis360
}

// YearFraction converts days to a year fraction under the currency's
// convention — the forward-pricing accrual term.
func YearFraction(ccy string, days int) float64 {
	return float64(days) / float64(DayCount(ccy))
}

// ---------------------------------------------------------------------------
// Curve model
// ---------------------------------------------------------------------------

// Curve is one currency's discount curve — pillar rates (decimal
// fractions, e.g. 0.0525 = 5.25%) plus a staleness anchor.
type Curve struct {
	Currency string
	// Rates maps tenor → annualized simple rate (decimal fraction).
	Rates map[Tenor]decimal.Decimal
	AsOf  time.Time
	// SourceFeeds names the contributors (≥2 per the >=2-source rule).
	SourceFeeds []string
	Stale       bool
}

// Complete reports whether every canonical pillar is present and
// positive — a partial curve never publishes.
func (c Curve) Complete() bool {
	for _, t := range CanonicalTenors {
		if r, ok := c.Rates[t]; !ok || !r.IsPositive() {
			return false
		}
	}
	return true
}

// Rate interpolates the continuously-compounded rate at dayOffset —
// log-linear between bracketing pillars (spec Task 19.5.3.5 step 2);
// flat extrapolation outside the curve's ends.
func (c Curve) Rate(dayOffset int) decimal.Decimal {
	points := make([][2]float64, 0, len(c.Rates))
	for t, r := range c.Rates {
		if d, ok := tenorDays[t]; ok && r.IsPositive() {
			points = append(points, [2]float64{float64(d), r.InexactFloat64()})
		}
	}
	if len(points) == 0 {
		return decimal.Zero
	}
	sort.Slice(points, func(i, j int) bool { return points[i][0] < points[j][0] })
	x := float64(dayOffset)
	if x <= points[0][0] {
		return decimal.NewFromFloat(points[0][1])
	}
	if x >= points[len(points)-1][0] {
		return decimal.NewFromFloat(points[len(points)-1][1])
	}
	for i := 0; i+1 < len(points); i++ {
		x0, y0 := points[i][0], points[i][1]
		x1, y1 := points[i+1][0], points[i+1][1]
		if x >= x0 && x <= x1 {
			// Log-linear on the discount factor: DF = exp(-r·t);
			// interpolating ln DF between pillars is the standard
			// money-market convention (no arbitrage at knots).
			b0 := math.Max(float64(DayCount(c.Currency)), 1)
			lf0 := -y0 * x0 / b0
			lf1 := -y1 * x1 / b0
			lf := lf0 + (lf1-lf0)*(x-x0)/(x1-x0)
			r := -lf * b0 / x
			return decimal.NewFromFloat(r)
		}
	}
	return decimal.NewFromFloat(points[len(points)-1][1])
}

// DiscountFactor returns exp(-r·t) at dayOffset — Phase-22 forward
// pricing consumes this directly.
func (c Curve) DiscountFactor(dayOffset int) float64 {
	r := c.Rate(dayOffset)
	t := YearFraction(c.Currency, dayOffset)
	return math.Exp(-r.InexactFloat64() * t)
}

// ---------------------------------------------------------------------------
// Tom-Next swap points
// ---------------------------------------------------------------------------

// SwapPoint is a per-pair T/N point quote — the Phase-3 Task 3.3.7
// rollover input (§15.3). Points are signed and in the pair's pip
// scale: positive accrues TO the position holder, negative charges
// (same convention as settlement.SwapRate).
type SwapPoint struct {
	Pair  string // "EUR/USD"
	Long  decimal.Decimal
	Short decimal.Decimal
	AsOf  time.Time
	// Source names the contributor feed — Refinitiv T/N points,
	// Bloomberg BVSW, or the derived rate-differential fallback.
	Source string
}

// SwapPointsKey is the Redis contract for per-pair forward points.
func SwapPointsKey(pair string) string { return "fwd_points:" + pair }

// CurveKey is the per-currency Redis publication contract.
func CurveKey(ccy string) string { return "curve:" + strings.ToUpper(ccy) }

// ---------------------------------------------------------------------------
// Store — Redis publication + ClickHouse archive seam
// ---------------------------------------------------------------------------

// ArchiveSink is the daily ClickHouse snapshot writer (Task 19.5.3.5
// step 5 — audit/back-testing tier).
type ArchiveSink interface {
	ArchiveCurve(ctx context.Context, c Curve) error
}

// Store publishes curves and swap points to Redis and archives them.
type Store struct {
	C          *goredis.Client
	Archive    ArchiveSink
	StaleAfter time.Duration // default 5s — the same gate as spot feeds
	now        func() time.Time
}

// NewStore wires the publication path.
func NewStore(c *goredis.Client) *Store {
	return &Store{C: c, StaleAfter: 5 * time.Second, now: time.Now}
}

// PublishCurve writes curve:{ccy} atomically with its staleness stamp.
// Incomplete or stale curves refuse to publish — consumers reading a
// missing/expired curve fail closed on YIELD_CURVE_UNAVAILABLE.
func (s *Store) PublishCurve(ctx context.Context, c Curve) error {
	if !c.Complete() {
		return fmt.Errorf("rates: %s curve incomplete — refusing publish", c.Currency)
	}
	if s.now().Sub(c.AsOf) > s.StaleAfter {
		return fmt.Errorf("rates: %s curve stale (%s) — refusing publish",
			c.Currency, s.now().Sub(c.AsOf))
	}
	doc, err := jsonMarshalCurve(c)
	if err != nil {
		return err
	}
	pipe := s.C.TxPipeline()
	pipe.Set(ctx, CurveKey(c.Currency), doc, 0)
	pipe.Set(ctx, CurveKey(c.Currency)+":ts",
		fmt.Sprint(c.AsOf.UnixNano()), 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("rates: publish curve %s: %w", c.Currency, err)
	}
	if s.Archive != nil {
		if err := s.Archive.ArchiveCurve(ctx, c); err != nil {
			return fmt.Errorf("rates: archive curve %s: %w", c.Currency, err)
		}
	}
	return nil
}

// PublishSwapPoint writes fwd_points:{pair} for T/N rollover + forward
// pricing — the wire doc carries both legs + the staleness stamp.
func (s *Store) PublishSwapPoint(ctx context.Context, sp SwapPoint) error {
	doc, err := json.Marshal(struct {
		Long   string `json:"long"`
		Short  string `json:"short"`
		AsOfNs int64  `json:"as_of_ns"`
		Source string `json:"source,omitempty"`
	}{sp.Long.String(), sp.Short.String(), sp.AsOf.UnixNano(), sp.Source})
	if err != nil {
		return err
	}
	pipe := s.C.TxPipeline()
	pipe.Set(ctx, SwapPointsKey(sp.Pair), doc, 0)
	pipe.Set(ctx, SwapPointsKey(sp.Pair)+":ts",
		fmt.Sprint(sp.AsOf.UnixNano()), 0)
	_, err = pipe.Exec(ctx)
	return err
}

// SwapPointFor reads fwd_points:{pair} and enforces the staleness
// gate — a stale row returns STALE_FORWARD_POINTS semantics (the
// caller maps through the registered code).
func (s *Store) SwapPointFor(ctx context.Context, pair string) (SwapPoint, error) {
	vals, err := s.C.MGet(ctx, SwapPointsKey(pair), SwapPointsKey(pair)+":ts").Result()
	if err != nil {
		return SwapPoint{}, fmt.Errorf("rates: fwd points read %s: %w", pair, err)
	}
	doc, ok := vals[0].(string)
	if !ok || doc == "" {
		return SwapPoint{}, fmt.Errorf("%w: %s", ErrStaleForwardPoints, pair)
	}
	var j struct {
		Long   string `json:"long"`
		Short  string `json:"short"`
		AsOfNs int64  `json:"as_of_ns"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(doc), &j); err != nil {
		return SwapPoint{}, fmt.Errorf("rates: fwd points %s malformed: %w", pair, err)
	}
	sp := SwapPoint{Pair: pair, Source: j.Source, AsOf: time.Unix(0, j.AsOfNs)}
	if sp.Long, err = decimal.NewFromString(j.Long); err != nil {
		return SwapPoint{}, fmt.Errorf("rates: fwd points %s long malformed: %w", pair, err)
	}
	if sp.Short, err = decimal.NewFromString(j.Short); err != nil {
		return SwapPoint{}, fmt.Errorf("rates: fwd points %s short malformed: %w", pair, err)
	}
	if s.now().Sub(sp.AsOf) > s.StaleAfter {
		return sp, fmt.Errorf("%w: %s stale %s",
			ErrStaleForwardPoints, pair, s.now().Sub(sp.AsOf))
	}
	return sp, nil
}

// ErrStaleForwardPoints mirrors the registered code STALE_FORWARD_POINTS
// — callers map it through the errs registry.
var ErrStaleForwardPoints = fmt.Errorf("stale forward points")

// ---------------------------------------------------------------------------
// Reader — the consumer-side seam (Phase-3/22 lookups)
// ---------------------------------------------------------------------------

// ErrCurveUnavailable mirrors the registered code YIELD_CURVE_UNAVAILABLE
// — callers map it through the errs registry.
var ErrCurveUnavailable = fmt.Errorf("yield curve unavailable")

// GetCurve reads curve:{ccy} and enforces the staleness gate.
func (s *Store) GetCurve(ctx context.Context, ccy string) (Curve, error) {
	vals, err := s.C.MGet(ctx, CurveKey(ccy), CurveKey(ccy)+":ts").Result()
	if err != nil {
		return Curve{}, fmt.Errorf("rates: read %s: %w", ccy, err)
	}
	doc, ok := vals[0].(string)
	if !ok || doc == "" {
		return Curve{}, fmt.Errorf("%w: %s", ErrCurveUnavailable, ccy)
	}
	c, err := jsonUnmarshalCurve(doc)
	if err != nil {
		return Curve{}, err
	}
	if ts, ok := vals[1].(string); ok {
		var ns int64
		if _, err := fmt.Sscan(ts, &ns); err == nil {
			c.AsOf = time.Unix(0, ns)
		}
	}
	c.Stale = s.now().Sub(c.AsOf) > s.StaleAfter
	if c.Stale {
		return c, fmt.Errorf("%w: %s stale %s",
			ErrCurveUnavailable, ccy, s.now().Sub(c.AsOf))
	}
	return c, nil
}
