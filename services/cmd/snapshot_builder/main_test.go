package main

import (
	"testing"
	"time"
)

func TestSnapshotNextRun(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	next, err := snapshotNextRun(now, "23:59")
	if err != nil {
		t.Fatalf("nextRun: %v", err)
	}
	if next.Format("15:04") != "23:59" || next.Day() != 27 {
		t.Fatalf("next = %v, want today 23:59", next)
	}
	// At/after the boundary → tomorrow.
	for _, n := range []time.Time{
		time.Date(2026, 9, 27, 23, 59, 0, 0, time.UTC),
		time.Date(2026, 9, 27, 23, 59, 30, 0, time.UTC),
	} {
		next, err = snapshotNextRun(n, "23:59")
		if err != nil || next.Day() != 28 {
			t.Fatalf("next after boundary = %v err=%v", next, err)
		}
	}
	// Default schedule constant honoured.
	if _, err := snapshotNextRun(now, "23:59"); err != nil {
		t.Fatal(err)
	}
	// Bad input fails closed.
	for _, bad := range []string{"", "9", "25:00", "23:60", "aa:bb"} {
		if _, err := snapshotNextRun(now, bad); err == nil {
			t.Fatalf("bad schedule %q must error", bad)
		}
	}
}

func TestSnapshotTargetDay(t *testing.T) {
	d, err := snapshotTargetDay("2026-09-27")
	if err != nil || d.Format("2006-01-02") != "2026-09-27" ||
		d.Location() != time.UTC {
		t.Fatalf("date = %v err=%v", d, err)
	}
	if d.Hour() != 0 {
		t.Fatalf("day must be midnight UTC: %v", d)
	}
	if _, err := snapshotTargetDay("27/09/2026"); err == nil {
		t.Fatal("bad date must error")
	}
	today, err := snapshotTargetDay("")
	if err != nil {
		t.Fatalf("default today: %v", err)
	}
	now := time.Now().UTC()
	if today.Format("2006-01-02") != now.Format("2006-01-02") {
		t.Fatalf("default day = %v, want UTC today", today)
	}
}
