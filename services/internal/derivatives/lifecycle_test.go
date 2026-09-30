// lifecycle_test.go — pure unit coverage for Task 22.3.10: the 15:00 UTC
// exercise cutoff, the ±0.5% ITM boundary, do-not-exercise semantics,
// pro-rata writer assignment (deterministic seed), and the expiry tick.
package derivatives

import (
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// ITM math — spec §15.4 / §24 #158: ≥0.5% ITM auto-exercises, boundary
// inclusive.
// ---------------------------------------------------------------------------

func TestITMBpsBoundaries(t *testing.T) {
	strike := decimal.RequireFromString("1.10")

	// CALL: mark 1.1055 → (1.1055-1.10)/1.10 = 0.005 = exactly +50bps.
	bps, err := ITMBps("CALL", strike, decimal.RequireFromString("1.1055"))
	if err != nil {
		t.Fatalf("itm: %v", err)
	}
	if !bps.GreaterThanOrEqual(decimal.NewFromInt(AutoExerciseITMThresholdBps)) {
		t.Fatalf("exactly-0.5%% ITM must qualify: %s bps", bps)
	}
	// mark 1.1054 → ~49.998bps → below threshold.
	bps, _ = ITMBps("CALL", strike, decimal.RequireFromString("1.1054"))
	if bps.GreaterThanOrEqual(decimal.NewFromInt(AutoExerciseITMThresholdBps)) {
		t.Fatalf("just-under-0.5%% must not qualify: %s bps", bps)
	}
	// PUT: mark 1.0945 → (1.10-1.0945)/1.0945 ≈ +50.25bps.
	bps, _ = ITMBps("PUT", strike, decimal.RequireFromString("1.0945"))
	if bps.LessThan(decimal.NewFromInt(AutoExerciseITMThresholdBps)) {
		t.Fatalf("PUT ITM mismeasured: %s bps", bps)
	}
	// PUT OTM.
	bps, _ = ITMBps("PUT", strike, decimal.RequireFromString("1.20"))
	if bps.IsPositive() {
		t.Fatalf("PUT above strike must be OTM: %s", bps)
	}
	// Fail-closed inputs.
	for _, tc := range []struct {
		typ          string
		strike, mark decimal.Decimal
	}{
		{"CALL", decimal.Zero, decimal.One},
		{"CALL", decimal.One, decimal.Zero},
		{"PUT", decimal.NewFromInt(-1), decimal.One},
		{"WUT", decimal.One, decimal.One},
	} {
		if _, err := ITMBps(tc.typ, tc.strike, tc.mark); err == nil {
			t.Fatalf("%+v must reject", tc)
		}
	}
}

// ---------------------------------------------------------------------------
// Exercise cutoff — 15:00 UTC on the adjusted expiry day.
// ---------------------------------------------------------------------------

func TestExerciseCutoff(t *testing.T) {
	// Friday expiry → same-day 15:00 UTC.
	fri := day(2026, 2, 6) // Friday
	cut := ExerciseCutoff(nil, fri, "EUR", "USD")
	if cut.Format("2006-01-02 15:04") != "2026-02-06 15:00" || cut.Location() != time.UTC {
		t.Fatalf("cutoff %s", cut)
	}
	// Saturday expiry → nudged to Monday 15:00 (weekday calendar).
	sat := day(2026, 2, 7)
	cut = ExerciseCutoff(nil, sat, "EUR", "USD")
	if cut.Format("2006-01-02 15:04") != "2026-02-09 15:00" {
		t.Fatalf("weekend expiry must shift forward, got %s", cut)
	}
}

func TestCheckExerciseWindow(t *testing.T) {
	expiry := day(2026, 2, 6).Add(10 * time.Hour) // Friday
	opt := &optionRow{
		ID: 1, OptionType: "CALL", ExerciseStyle: "EUROPEAN",
		ExpiryAt: expiry, BaseCcy: "EUR", QuoteCcy: "USD",
		Status: OptStatusOpen, Side: "LONG",
	}
	// EUROPEAN before expiry day → not exercisable.
	assertCode(t, checkExerciseWindow(opt, day(2026, 2, 5).Add(10*time.Hour)),
		CodeOptionNotExercisable, "european early")
	// Expiry day pre-cutoff → allowed.
	if err := checkExerciseWindow(opt, expiry); err != nil {
		t.Fatalf("expiry-day pre-cutoff must pass: %v", err)
	}
	// At exactly 15:00:00 → closed.
	assertCode(t, checkExerciseWindow(opt, day(2026, 2, 6).Add(15*time.Hour)),
		CodeExerciseCutoffPassed, "at cutoff")
	// 14:59:59 → open.
	if err := checkExerciseWindow(opt, day(2026, 2, 6).Add(15*time.Hour-time.Second)); err != nil {
		t.Fatalf("14:59:59 must pass: %v", err)
	}
	// AMERICAN any time before cutoff.
	opt.ExerciseStyle = "AMERICAN"
	if err := checkExerciseWindow(opt, day(2026, 2, 2)); err != nil {
		t.Fatalf("american early must pass: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Pro-rata assignment — sum-exact, capacity-bounded, seed-deterministic.
// ---------------------------------------------------------------------------

func TestAssignProRata(t *testing.T) {
	mk := func(q string) *optionRow {
		return &optionRow{Quantity: decimal.RequireFromString(q)}
	}
	writers := []*optionRow{mk("60"), mk("30"), mk("10")}
	qty := decimal.RequireFromString("50")
	s1 := assignProRata(writers, qty, 42)
	var sum decimal.Decimal
	for i, s := range s1 {
		sum = sum.Add(s)
		if s.GreaterThan(writers[i].Quantity) {
			t.Fatalf("slice %d exceeds OI", i)
		}
	}
	if !sum.Equal(qty) {
		t.Fatalf("slices %v sum %s != %s", s1, sum, qty)
	}
	// 50 across OI 100 at 60/30/10 → exact 30/15/5.
	if !(s1[0].Equal(decimal.RequireFromString("30")) &&
		s1[1].Equal(decimal.RequireFromString("15")) &&
		s1[2].Equal(decimal.RequireFromString("5"))) {
		t.Fatalf("pro-rata split wrong: %v", s1)
	}
	// Deterministic: same seed → same split.
	s2 := assignProRata(writers, qty, 42)
	for i := range s1 {
		if !s1[i].Equal(s2[i]) {
			t.Fatalf("seeded split not deterministic: %v vs %v", s1, s2)
		}
	}
	// Fractional remainder — still sums exactly.
	writers2 := []*optionRow{mk("1"), mk("1"), mk("1")}
	s3 := assignProRata(writers2, decimal.RequireFromString("2"), 7)
	sum = decimal.Zero
	for _, s := range s3 {
		sum = sum.Add(s)
	}
	if !sum.Equal(decimal.RequireFromString("2")) {
		t.Fatalf("remainder split sum %s", sum)
	}
	// Capacity cap — slices never exceed per-writer OI even on the cap path.
	writers3 := []*optionRow{mk("1"), mk("100")}
	s4 := assignProRata(writers3, decimal.RequireFromString("80"), 3)
	if s4[0].GreaterThan(decimal.RequireFromString("1")) {
		t.Fatalf("cap violated: %v", s4)
	}
	if !s4[0].Add(s4[1]).Equal(decimal.RequireFromString("80")) {
		t.Fatalf("cap split sum wrong: %v", s4)
	}
}

func TestAssignmentSeedDeterministic(t *testing.T) {
	exp := day(2026, 2, 6)
	a := assignmentSeed(5, decimal.RequireFromString("10"), exp)
	b := assignmentSeed(5, decimal.RequireFromString("10"), exp)
	c := assignmentSeed(6, decimal.RequireFromString("10"), exp)
	if a != b || a == c || a < 0 {
		t.Fatalf("seed unstable: %d %d %d", a, b, c)
	}
}

// ---------------------------------------------------------------------------
// Expiry daemon tick — weekday 15:00 UTC.
// ---------------------------------------------------------------------------

func TestNextExpiryTick(t *testing.T) {
	// Friday 10:00 → Friday 15:00.
	next := nextExpiryTick(day(2026, 2, 6).Add(10 * time.Hour))
	if next.Format("2006-01-02 15:04") != "2026-02-06 15:00" {
		t.Fatalf("intra-day tick %s", next)
	}
	// Friday 16:00 → Monday 15:00 (weekends have no expiries).
	next = nextExpiryTick(day(2026, 2, 6).Add(16 * time.Hour))
	if next.Format("2006-01-02 15:04") != "2026-02-09 15:00" {
		t.Fatalf("weekend tick %s", next)
	}
}
