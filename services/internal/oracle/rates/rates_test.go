package rates

import (
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// Task 19.5.3.5 — curve completeness, interpolation, day-count,
// staleness gate.

func fullCurve() Curve {
	return Curve{
		Currency: "USD", AsOf: time.Now(),
		Rates: map[Tenor]decimal.Decimal{
			TenorON:  decimal.RequireFromString("0.050"),
			TenorTN:  decimal.RequireFromString("0.050"),
			Tenor1W:  decimal.RequireFromString("0.051"),
			Tenor1M:  decimal.RequireFromString("0.052"),
			Tenor3M:  decimal.RequireFromString("0.053"),
			Tenor6M:  decimal.RequireFromString("0.054"),
			Tenor12M: decimal.RequireFromString("0.055"),
		},
		SourceFeeds: []string{"refinitiv", "bfix"},
	}
}

func TestDayCountConventions(t *testing.T) {
	// §15.3: ACT/360 USD EUR CHF JPY; ACT/365 GBP AUD NZD CAD SGD HKD.
	for _, c := range []string{"USD", "EUR", "CHF", "JPY"} {
		if DayCount(c) != Basis360 {
			t.Fatalf("%s must be ACT/360", c)
		}
	}
	for _, c := range []string{"GBP", "AUD", "NZD", "CAD", "SGD", "HKD"} {
		if DayCount(c) != Basis365 {
			t.Fatalf("%s must be ACT/365", c)
		}
	}
	// Unlisted → ACT/360 default (money-market majority).
	if DayCount("TRY") != Basis360 {
		t.Fatal("unlisted ccy must default to ACT/360")
	}
}

func TestCurveCompleteness(t *testing.T) {
	c := fullCurve()
	if !c.Complete() {
		t.Fatal("7-pillar curve must be complete")
	}
	delete(c.Rates, Tenor3M)
	if c.Complete() {
		t.Fatal("missing pillar must fail completeness")
	}
	c = fullCurve()
	c.Rates[TenorON] = decimal.Zero
	if c.Complete() {
		t.Fatal("non-positive pillar must fail completeness")
	}
}

func TestCurvePillarAndInterpolation(t *testing.T) {
	c := fullCurve()
	// Pillar hits return the pillar rate.
	if r := c.Rate(30); !r.Equal(decimal.RequireFromString("0.052")) {
		t.Fatalf("1M pillar rate %s, want 0.052", r)
	}
	// Interpolation between 1M(30d,0.052) and 3M(91d,0.053): at 60d
	// the log-linear rate sits strictly between.
	mid := c.Rate(60)
	if !mid.GreaterThan(decimal.RequireFromString("0.052")) ||
		!mid.LessThan(decimal.RequireFromString("0.053")) {
		t.Fatalf("interpolated 60d rate %s outside pillar band", mid)
	}
	// Flat extrapolation past the wings.
	if r := c.Rate(400); !r.Equal(decimal.RequireFromString("0.055")) {
		t.Fatalf("past-12M extrapolation %s, want flat 0.055", r)
	}
	// Discount factor is monotonically decreasing in tenor.
	if c.DiscountFactor(365) >= c.DiscountFactor(30) {
		t.Fatal("DF must decrease with tenor")
	}
}

func TestCurvePublishRefusesStaleAndIncomplete(t *testing.T) {
	// Store.PublishCurve's guards run before any Redis write — exercise
	// them without a client by observing the guard path only needs the
	// complete/stale checks ahead of the C call. Build a store with a
	// nil client; the guards must error BEFORE touching Redis.
	s := &Store{StaleAfter: 5 * time.Second, now: time.Now}

	bad := fullCurve()
	delete(bad.Rates, Tenor6M)
	if err := s.PublishCurve(t.Context(), bad); err == nil {
		t.Fatal("incomplete curve must refuse to publish")
	}
	stale := fullCurve()
	stale.AsOf = time.Now().Add(-time.Minute)
	if err := s.PublishCurve(t.Context(), stale); err == nil {
		t.Fatal("stale curve must refuse to publish")
	}
}
