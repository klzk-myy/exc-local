package ohlcv

import (
	"testing"
	"time"
)

func TestCanonicalIntervalSet(t *testing.T) {
	// Spec §24 #264 + §16.2 + Task 6.3.14: 13 intervals, 12 persisted,
	// 1s memory-only.
	want := []string{"1s", "1m", "5m", "15m", "30m", "1h", "2h",
		"4h", "6h", "8h", "1D", "1W", "1M"}
	if len(CanonicalIntervals) != 13 {
		t.Fatalf("CanonicalIntervals=%d, want 13", len(CanonicalIntervals))
	}
	for i, iv := range CanonicalIntervals {
		if iv.String() != want[i] {
			t.Fatalf("interval[%d]=%s, want %s", i, iv, want[i])
		}
		if got, ok := ParseInterval(want[i]); !ok || got != iv {
			t.Fatalf("ParseInterval(%q)=(%v,%v)", want[i], got, ok)
		}
	}
	if len(PersistedIntervals) != 12 {
		t.Fatalf("PersistedIntervals=%d, want 12", len(PersistedIntervals))
	}
	for _, iv := range PersistedIntervals {
		if iv == I1s || !iv.Persisted() {
			t.Fatalf("persisted set contains %s", iv)
		}
	}
	if I1s.Persisted() {
		t.Fatal("1s must be memory-only")
	}
}

func TestIntervalFloorAlignment(t *testing.T) {
	ts := func(y int, M time.Month, d, h, m, s int) time.Time {
		return time.Date(y, M, d, h, m, s, 0, time.UTC)
	}
	cases := []struct {
		iv   Interval
		in   time.Time
		want time.Time
	}{
		// Fixed widths align to UTC day; a timestamp exactly on the edge
		// opens the NEW bucket.
		{I1s, ts(2026, 1, 5, 10, 0, 30), ts(2026, 1, 5, 10, 0, 30)},
		{I1m, ts(2026, 1, 5, 10, 0, 59), ts(2026, 1, 5, 10, 0, 0)},
		{I1m, ts(2026, 1, 5, 10, 1, 0), ts(2026, 1, 5, 10, 1, 0)}, // exact edge → new bucket
		{I5m, ts(2026, 1, 5, 10, 4, 59), ts(2026, 1, 5, 10, 0, 0)},
		{I15m, ts(2026, 1, 5, 10, 44, 0), ts(2026, 1, 5, 10, 30, 0)},
		{I30m, ts(2026, 1, 5, 10, 30, 0), ts(2026, 1, 5, 10, 30, 0)},
		{I1h, ts(2026, 1, 5, 10, 59, 59), ts(2026, 1, 5, 10, 0, 0)},
		{I2h, ts(2026, 1, 5, 11, 0, 0), ts(2026, 1, 5, 10, 0, 0)},
		{I4h, ts(2026, 1, 5, 15, 0, 0), ts(2026, 1, 5, 12, 0, 0)},
		{I6h, ts(2026, 1, 5, 13, 0, 0), ts(2026, 1, 5, 12, 0, 0)},
		// 8h session boundaries: 00/08/16 UTC (Sydney/London/New York).
		{I8h, ts(2026, 1, 5, 7, 59, 59), ts(2026, 1, 5, 0, 0, 0)},
		{I8h, ts(2026, 1, 5, 8, 0, 0), ts(2026, 1, 5, 8, 0, 0)},
		{I8h, ts(2026, 1, 5, 15, 30, 0), ts(2026, 1, 5, 8, 0, 0)},
		{I8h, ts(2026, 1, 5, 16, 0, 0), ts(2026, 1, 5, 16, 0, 0)},
		{I8h, ts(2026, 1, 5, 23, 59, 59), ts(2026, 1, 5, 16, 0, 0)},
		// 1D = 00:00 UTC.
		{I1D, ts(2026, 1, 5, 23, 59, 59), ts(2026, 1, 5, 0, 0, 0)},
		// 1W = Monday 00:00 UTC. 2026-01-05 is a Monday.
		{I1W, ts(2026, 1, 7, 12, 0, 0), ts(2026, 1, 5, 0, 0, 0)},  // Wed
		{I1W, ts(2026, 1, 11, 23, 0, 0), ts(2026, 1, 5, 0, 0, 0)}, // Sun
		{I1W, ts(2026, 1, 5, 0, 0, 0), ts(2026, 1, 5, 0, 0, 0)},   // Mon edge
		{I1W, ts(2026, 1, 4, 18, 0, 0), ts(2025, 12, 29, 0, 0, 0)},
		// 1M = first of month 00:00 UTC.
		{I1M, ts(2026, 1, 31, 23, 59, 59), ts(2026, 1, 1, 0, 0, 0)},
		{I1M, ts(2026, 2, 15, 12, 0, 0), ts(2026, 2, 1, 0, 0, 0)},
		{I1M, ts(2026, 12, 1, 0, 0, 0), ts(2026, 12, 1, 0, 0, 0)},
	}
	for _, c := range cases {
		got := c.iv.Floor(c.in)
		if !got.Equal(c.want) {
			t.Errorf("%s.Floor(%s)=%s, want %s",
				c.iv, c.in.Format(time.RFC3339), got.Format(time.RFC3339), c.want.Format(time.RFC3339))
		}
		if got.Location() != time.UTC {
			t.Errorf("%s.Floor not UTC", c.iv)
		}
	}
}

func TestIntervalNext(t *testing.T) {
	ts := func(y int, M time.Month, d, h, m, s int) time.Time {
		return time.Date(y, M, d, h, m, s, 0, time.UTC)
	}
	cases := []struct {
		iv    Interval
		start time.Time
		want  time.Time
	}{
		{I1s, ts(2026, 1, 5, 10, 0, 30), ts(2026, 1, 5, 10, 0, 31)},
		{I1m, ts(2026, 1, 5, 10, 0, 0), ts(2026, 1, 5, 10, 1, 0)},
		{I8h, ts(2026, 1, 5, 16, 0, 0), ts(2026, 1, 6, 0, 0, 0)},
		{I1D, ts(2026, 1, 31, 0, 0, 0), ts(2026, 2, 1, 0, 0, 0)},
		{I1W, ts(2026, 1, 5, 0, 0, 0), ts(2026, 1, 12, 0, 0, 0)},
		{I1M, ts(2026, 1, 1, 0, 0, 0), ts(2026, 2, 1, 0, 0, 0)},
		{I1M, ts(2026, 12, 1, 0, 0, 0), ts(2027, 1, 1, 0, 0, 0)},
	}
	for _, c := range cases {
		if got := c.iv.Next(c.start); !got.Equal(c.want) {
			t.Errorf("%s.Next(%s)=%s, want %s",
				c.iv, c.start.Format(time.RFC3339), got.Format(time.RFC3339), c.want.Format(time.RFC3339))
		}
	}
	// Floor∘Next round trip: every bucket boundary maps onto itself.
	for _, iv := range CanonicalIntervals {
		base := iv.Floor(ts(2026, 3, 11, 14, 37, 22))
		if n := iv.Next(base); !n.After(base) || !iv.Floor(n).Equal(n) {
			t.Fatalf("%s Next/Floor inconsistent: base=%s next=%s", iv, base, n)
		}
	}
}
