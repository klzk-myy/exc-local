// Phase-13.5 Task 13.5.3.8/13.5.3.9 unit tests — no PG required.
package security

import (
	"testing"
	"time"
)

func TestSeverityFromCVSS(t *testing.T) {
	cases := []struct {
		score float64
		want  string
		ok    bool
	}{
		{0.0, "", false},
		{0.1, SeverityLow, true},
		{3.9, SeverityLow, true},
		{4.0, SeverityMedium, true},
		{6.9, SeverityMedium, true},
		{7.0, SeverityHigh, true},
		{8.9, SeverityHigh, true},
		{9.0, SeverityCritical, true},
		{10.0, SeverityCritical, true},
		{10.1, "", false},
		{-1, "", false},
	}
	for _, c := range cases {
		got, ok := SeverityFromCVSS(c.score)
		if got != c.want || ok != c.ok {
			t.Errorf("SeverityFromCVSS(%v) = %q,%v want %q,%v",
				c.score, got, ok, c.want, c.ok)
		}
	}
}

func TestFixETAContract(t *testing.T) {
	// Task 13.5.3.9 severity contract: Critical 7d, High 30d, Medium 90d.
	table := map[string]time.Duration{
		SeverityCritical: 7 * 24 * time.Hour,
		SeverityHigh:     30 * 24 * time.Hour,
		SeverityMedium:   90 * 24 * time.Hour,
		SeverityLow:      180 * 24 * time.Hour,
	}
	for sev, want := range table {
		got, ok := FixETA(sev)
		if !ok || got != want {
			t.Errorf("FixETA(%s) = %v,%v want %v", sev, got, ok, want)
		}
	}
	if _, ok := FixETA("BOGUS"); ok {
		t.Error("FixETA(BOGUS) must fail closed")
	}
}

func TestTransitions(t *testing.T) {
	allowed := [][2]string{
		{StatusTriaged, StatusInProgress},
		{StatusTriaged, StatusDisputed},
		{StatusTriaged, StatusRejected},
		{StatusInProgress, StatusFixed},
		{StatusInProgress, StatusDisputed},
		{StatusInProgress, StatusRejected},
		// Disputed is real — every exit lands somewhere meaningful.
		{StatusDisputed, StatusInProgress},
		{StatusDisputed, StatusFixed},
		{StatusDisputed, StatusRejected},
		// Researcher rebuttal lane.
		{StatusRejected, StatusDisputed},
	}
	for _, tr := range allowed {
		if !ValidTransition(tr[0], tr[1]) {
			t.Errorf("transition %s → %s must be allowed", tr[0], tr[1])
		}
	}
	denied := [][2]string{
		{StatusFixed, StatusInProgress},
		{StatusFixed, StatusDisputed},
		{StatusFixed, StatusRejected},
		{StatusTriaged, StatusFixed}, // no skipping in-progress work
		{StatusDisputed, StatusTriaged},
		{"INTAKED", StatusTriaged}, // superseded enum — not a state
		{"", StatusTriaged},
	}
	for _, tr := range denied {
		if ValidTransition(tr[0], tr[1]) {
			t.Errorf("transition %s → %s must be denied", tr[0], tr[1])
		}
	}
}

func TestBusinessDays(t *testing.T) {
	// Friday 2026-09-25 → +5 business days lands Friday 2026-10-02
	// (skips the weekend).
	fri := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	got := businessDays(fri, 5)
	want := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("businessDays(fri,5) = %v want %v", got, want)
	}
	// Monday +5 → next Monday.
	mon := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	if got := businessDays(mon, 5); got.Weekday() != time.Monday ||
		got.Day() != 5 {
		t.Fatalf("businessDays(mon,5) = %v want Monday Oct 5", got)
	}
	// Saturday input counts forward the same (submission on a weekend
	// does not grant free SLA days beyond the weekend itself).
	sat := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	gotSat := businessDays(sat, 5)
	if gotSat.Weekday() != time.Friday || gotSat.Day() != 2 {
		t.Fatalf("businessDays(sat,5) = %v want Friday Oct 2", gotSat)
	}
}
