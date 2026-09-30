package options

import (
	"testing"
	"time"
)

// TestAssignProRata: floor shares by OI plus largest-remainder
// distribution, deterministic across runs and input orderings.
func TestAssignProRata(t *testing.T) {
	writers := []OptionWriter{
		{AccountID: 7, OpenInterest: 50},
		{AccountID: 3, OpenInterest: 30},
		{AccountID: 9, OpenInterest: 20},
	}
	// exercised=7, ΣOI=100 → floors 3,2,1 (rems 50,10,40) → the single
	// leftover goes to acct 7 (largest remainder).
	got, err := AssignWriters(7, writers, 42)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]int64{7: 4, 3: 2, 9: 1}
	var total int64
	for _, a := range got {
		if want[a.AccountID] != a.Quantity {
			t.Fatalf("acct %d got %d, want %d", a.AccountID, a.Quantity, want[a.AccountID])
		}
		if a.Quantity == 0 {
			t.Fatalf("acct %d zero-quantity assignment emitted", a.AccountID)
		}
		total += a.Quantity
	}
	if total != 7 {
		t.Fatalf("assigned %d, want 7", total)
	}
	if got[0].AccountID > got[1].AccountID || got[1].AccountID > got[2].AccountID {
		t.Fatal("assignments not in canonical AccountID order")
	}

	// Determinism: identical call → identical output; shuffled input
	// order → identical output.
	again, err := AssignWriters(7, writers, 42)
	if err != nil {
		t.Fatal(err)
	}
	shuffled, err := AssignWriters(7, []OptionWriter{
		{AccountID: 9, OpenInterest: 20}, {AccountID: 7, OpenInterest: 50},
		{AccountID: 3, OpenInterest: 30},
	}, 42)
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		if got[i] != again[i] || got[i] != shuffled[i] {
			t.Fatalf("assignment %d not deterministic: %+v / %+v / %+v",
				i, got[i], again[i], shuffled[i])
		}
	}
}

// TestAssignTieBreak: equal remainders resolve by the seeded hash —
// deterministic for a fixed seed.
func TestAssignTieBreak(t *testing.T) {
	writers := []OptionWriter{
		{AccountID: 1, OpenInterest: 10},
		{AccountID: 2, OpenInterest: 10},
		{AccountID: 3, OpenInterest: 10},
	}
	// exercised=1, ΣOI=30 → all floors 0, all remainders 10 — pure
	// tie-break territory.
	a, err := AssignWriters(1, writers, 7)
	if err != nil {
		t.Fatal(err)
	}
	b, err := AssignWriters(1, writers, 7)
	if err != nil {
		t.Fatal(err)
	}
	var granted int64
	var winner int64
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("tie-break not deterministic")
		}
		granted += a[i].Quantity
		if a[i].TieBreak {
			winner = a[i].AccountID
		}
	}
	if granted != 1 || winner == 0 {
		t.Fatalf("expected exactly one tie-break winner, got %+v", a)
	}
}

// TestAssignFailures: over-exercise and degenerate input are coded
// failures — assignment never partially allocates.
func TestAssignFailures(t *testing.T) {
	_, err := AssignWriters(10, []OptionWriter{{AccountID: 1, OpenInterest: 5}}, 1)
	requireCode(t, err, CodeOptionAssignmentFailed)
	_, err = AssignWriters(0, []OptionWriter{{AccountID: 1, OpenInterest: 5}}, 1)
	requireCode(t, err, CodeOptionAssignmentFailed)
	_, err = AssignWriters(1, nil, 1)
	requireCode(t, err, CodeOptionAssignmentFailed)
	_, err = AssignWriters(1, []OptionWriter{{AccountID: 1, OpenInterest: 0}}, 1)
	requireCode(t, err, CodeOptionAssignmentFailed)
}

// TestMarkStale: the 5s staleness predicate is symmetric and
// deterministic — and feeding it into MarkTick.Stale makes the
// sibling BarrierMonitor skip (never fabricate) the stale touch.
func TestMarkStale(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	if MarkStale(now, now.Add(-4*time.Second)) {
		t.Fatal("fresh tick flagged stale")
	}
	if !MarkStale(now, now.Add(-6*time.Second)) {
		t.Fatal("stale tick not flagged")
	}
	if !MarkStale(now, now.Add(6*time.Second)) {
		t.Fatal("future-dated tick not flagged")
	}
	if MarkStale(now, now.Add(-MarkStalenessGate)) {
		t.Fatal("boundary tick at exactly the gate flagged stale")
	}
	// Deterministic: same inputs → same verdict.
	if MarkStale(now, now.Add(-10*time.Second)) != MarkStale(now, now.Add(-10*time.Second)) {
		t.Fatal("non-deterministic staleness verdict")
	}

	// Seam check: a mark outside the gate must never evaluate a knock
	// in the sibling BarrierMonitor — the §15.7 item-3 rule. The
	// producer flags the tick via MarkStale(now, tick.At); the
	// monitor's EvaluateBarrierTick re-checks the gate internally.
	mon, err := NewBarrierMonitor(BarrierUpAndOut, 1.25)
	if err != nil {
		t.Fatal(err)
	}
	staleAsOf := now.Add(-10 * time.Second)
	staleTick := MarkTick{
		Price: 9.99, // far through the barrier — would knock if evaluated
		At:    staleAsOf,
		Stale: MarkStale(now, staleAsOf),
	}
	ev, err := mon.Observe(staleTick)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Evaluated || ev.Knocked || mon.Knocked() {
		t.Fatalf("stale mark fabricated a knock: %+v", ev)
	}
	if ev.Reason != BarrierReasonStale {
		t.Fatalf("stale reason %q, want %q", ev.Reason, BarrierReasonStale)
	}
	// The first live post-gap tick then evaluates normally.
	ev, err = mon.Observe(MarkTick{Price: 1.26, At: now})
	if err != nil || !ev.Knocked {
		t.Fatalf("post-gap live touch not registered: %+v err=%v", ev, err)
	}
}

// TestDecideExercise: cutoff enforcement + auto-exercise threshold.
func TestDecideExercise(t *testing.T) {
	expiry := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	at := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)

	// Manual instruction before cutoff → accepted.
	d, err := DecideExercise(before, expiry, false, 1.10, 1.15, InstrExercise)
	if err != nil || d.Action != ExerciseManual {
		t.Fatalf("manual before cutoff: %+v err=%v", d, err)
	}
	// Manual at cutoff → coded rejection.
	_, err = DecideExercise(at, expiry, false, 1.10, 1.15, InstrExercise)
	requireCode(t, err, CodeExerciseCutoffPassed)
	// Opt-out before cutoff honored.
	d, err = DecideExercise(before, expiry, false, 1.10, 1.15, InstrDoNotExercise)
	if err != nil || d.Action != ExerciseOptedOut {
		t.Fatalf("opt-out: %+v err=%v", d, err)
	}
	// No instruction, before cutoff → pending.
	d, err = DecideExercise(before, expiry, false, 1.10, 1.15, InstrNone)
	if err != nil || d.Action != ExercisePending {
		t.Fatalf("pre-cutoff: %+v err=%v", d, err)
	}
	// Call ITM 1.5% at cutoff → auto-exercise.
	d, err = DecideExercise(at, expiry, false, 1.10, 1.10*1.015, InstrNone)
	if err != nil || d.Action != ExerciseAuto || d.Reason != ExerciseReasonAutoITM {
		t.Fatalf("auto ITM: %+v err=%v", d, err)
	}
	// Call ITM 0.4% (<0.5% threshold) → expires OTM.
	d, err = DecideExercise(at, expiry, false, 1.10, 1.10*1.004, InstrNone)
	if err != nil || d.Action != ExerciseExpireOTM {
		t.Fatalf("sub-threshold: %+v err=%v", d, err)
	}
	// Put symmetric: mark 0.6% below strike → auto.
	d, err = DecideExercise(at, expiry, true, 1.10, 1.10*0.994, InstrNone)
	if err != nil || d.Action != ExerciseAuto {
		t.Fatalf("put auto: %+v err=%v", d, err)
	}
	// Determinism.
	d2, err := DecideExercise(at, expiry, false, 1.10, 1.10*1.015, InstrNone)
	if err != nil || d != d2 {
		t.Fatalf("non-deterministic exercise decision: %+v vs %+v", d, d2)
	}
}

// TestDecideRoll: the fixed precedence — spread breach, then expiry
// conflict, then delivery mode — is deterministic.
func TestDecideRoll(t *testing.T) {
	// Spread breach wins over everything.
	d, err := DecideRoll(RollInput{SpreadBps: 12, SpreadToleranceBps: 5,
		SameDayOptionExpiry: true})
	if err != nil || d.Action != RollRejectSpread {
		t.Fatalf("spread breach: %+v err=%v", d, err)
	}
	// Same-day expiry defers the roll.
	d, err = DecideRoll(RollInput{SpreadBps: 3, SpreadToleranceBps: 5,
		SameDayOptionExpiry: true, PhysicalDelivery: true})
	if err != nil || d.Action != RollDeferExpiryConflict {
		t.Fatalf("expiry conflict: %+v err=%v", d, err)
	}
	// Physical instrument → grouped close+open.
	d, err = DecideRoll(RollInput{SpreadBps: 3, SpreadToleranceBps: 5,
		PhysicalDelivery: true})
	if err != nil || d.Action != RollCloseOpen {
		t.Fatalf("close+open: %+v err=%v", d, err)
	}
	// Cash-settled → auto-settle.
	d, err = DecideRoll(RollInput{SpreadBps: 3, SpreadToleranceBps: 5})
	if err != nil || d.Action != RollAutoSettle {
		t.Fatalf("auto-settle: %+v err=%v", d, err)
	}
	// Boundary: spread exactly at tolerance is inside.
	d, err = DecideRoll(RollInput{SpreadBps: 5, SpreadToleranceBps: 5})
	if err != nil || d.Action != RollAutoSettle {
		t.Fatalf("boundary spread: %+v err=%v", d, err)
	}
	// Bad inputs are coded.
	_, err = DecideRoll(RollInput{SpreadBps: 0, SpreadToleranceBps: -1})
	requireCode(t, err, CodeOptionPricingInputInvalid)
}
