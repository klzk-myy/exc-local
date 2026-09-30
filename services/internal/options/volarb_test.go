package options

import (
	"testing"
)

// TestSurfaceClean: an arbitrage-free surface produces zero violations
// and ValidateSurface passes.
func TestSurfaceClean(t *testing.T) {
	s, err := BuildIVSurface(SurfaceBuildInput{Tenors: cleanTenors()})
	if err != nil {
		t.Fatal(err)
	}
	if v := SurfaceViolations(s, DefaultArbTolerance()); len(v) != 0 {
		t.Fatalf("clean surface produced violations: %v", v)
	}
	if err := ValidateSurface(s); err != nil {
		t.Fatal(err)
	}
}

// TestCalendarArbRejection: a longer tenor carrying total variance
// below the shorter tenor violates calendar no-arb → coded rejection.
func TestCalendarArbRejection(t *testing.T) {
	tenors := cleanTenors()
	// Crush the long tenor's variance: vol 0.115 → 0.04 makes
	// w = 0.04²·1.0 = 0.0016 < short-tenor w at matching k.
	for i := range tenors[2].Points {
		tenors[2].Points[i].Vol = 0.04
	}
	s, err := BuildIVSurface(SurfaceBuildInput{Tenors: tenors})
	if err != nil {
		t.Fatal(err)
	}
	violations := SurfaceViolations(s, DefaultArbTolerance())
	var cal int
	for _, v := range violations {
		if v.Kind == ViolationCalendar {
			cal++
		}
	}
	if cal == 0 {
		t.Fatal("calendar arbitrage not detected")
	}
	requireCode(t, ValidateSurface(s), CodeVolatilitySurfaceArbitrage)

	// And BuildIVSurface's Validate gate rejects the same input.
	if _, err := BuildIVSurface(SurfaceBuildInput{Tenors: tenors, Validate: true}); err == nil {
		t.Fatal("build accepted arbitraged surface")
	} else {
		requireCode(t, err, CodeVolatilitySurfaceArbitrage)
	}
}

// TestButterflyArbRejection: a vol spike at the middle strike lifts
// the middle call above the chord joining its neighbors — a negative
// butterfly price → coded rejection.
func TestButterflyArbRejection(t *testing.T) {
	tenors := cleanTenors()[:1]
	tenors[0].Points[1].Vol = 0.60 // vol spike at K=1.10
	s, err := BuildIVSurface(SurfaceBuildInput{Tenors: tenors})
	if err != nil {
		t.Fatal(err)
	}
	violations := SurfaceViolations(s, DefaultArbTolerance())
	var fly int
	for _, v := range violations {
		if v.Kind == ViolationButterfly {
			fly++
		}
	}
	if fly == 0 {
		t.Fatal("butterfly arbitrage not detected")
	}
	requireCode(t, ValidateSurface(s), CodeVolatilitySurfaceArbitrage)
}

// TestViolationDeterminism: the violation scan is stable across runs.
func TestViolationDeterminism(t *testing.T) {
	tenors := cleanTenors()
	for i := range tenors[2].Points {
		tenors[2].Points[i].Vol = 0.04
	}
	tenors[0].Points[1].Vol = 0.005
	s, err := BuildIVSurface(SurfaceBuildInput{Tenors: tenors})
	if err != nil {
		t.Fatal(err)
	}
	a := SurfaceViolations(s, DefaultArbTolerance())
	b := SurfaceViolations(s, DefaultArbTolerance())
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("violation count unstable: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("violation %d diverged: %+v vs %+v", i, a[i], b[i])
		}
	}
	// Calendar violations precede butterfly in the canonical order.
	lastCal, firstFly := -1, len(a)
	for i, v := range a {
		if v.Kind == ViolationCalendar {
			lastCal = i
		}
		if v.Kind == ViolationButterfly && i < firstFly {
			firstFly = i
		}
	}
	if lastCal >= firstFly {
		t.Fatalf("violation ordering not canonical: lastCal=%d firstFly=%d", lastCal, firstFly)
	}
}
