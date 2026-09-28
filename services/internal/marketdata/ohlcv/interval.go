package ohlcv

import (
	"fmt"
	"time"
)

// Interval identifies one canonical candle timeframe. Label strings match
// fx_klines.timeframe (migration 173) and marketapi.KlineIntervals.
type Interval int

const (
	I1s  Interval = iota // "1s" — memory-only (Task 6.3.14)
	I1m                  // "1m"
	I5m                  // "5m"
	I15m                 // "15m"
	I30m                 // "30m"
	I1h                  // "1h"
	I2h                  // "2h"
	I4h                  // "4h"
	I6h                  // "6h"
	I8h                  // "8h" — aligns 00/08/16 UTC (FX session boundaries)
	I1D                  // "1D" — 00:00 UTC
	I1W                  // "1W" — Monday 00:00 UTC
	I1M                  // "1M" — first of month 00:00 UTC
)

// CanonicalIntervals is the complete 13-timeframe set in ascending width
// order (spec §24 #264; Phase-06 Tasks 6.3.8/6.3.14; migration 173; §16.2).
var CanonicalIntervals = []Interval{
	I1s, I1m, I5m, I15m, I30m, I1h, I2h, I4h, I6h, I8h, I1D, I1W, I1M,
}

// PersistedIntervals is the §16.2 twelve-interval set written to
// fx_klines and mirrored to ClickHouse — every canonical interval except
// the memory-only 1s.
var PersistedIntervals = []Interval{
	I1m, I5m, I15m, I30m, I1h, I2h, I4h, I6h, I8h, I1D, I1W, I1M,
}

var intervalLabels = map[Interval]string{
	I1s: "1s", I1m: "1m", I5m: "5m", I15m: "15m", I30m: "30m",
	I1h: "1h", I2h: "2h", I4h: "4h", I6h: "6h", I8h: "8h",
	I1D: "1D", I1W: "1W", I1M: "1M",
}

var intervalsByLabel map[string]Interval

func init() {
	intervalsByLabel = make(map[string]Interval, len(intervalLabels))
	for iv, l := range intervalLabels {
		intervalsByLabel[l] = iv
	}
}

// String returns the canonical label ("1m", "1D", ...).
func (iv Interval) String() string {
	if l, ok := intervalLabels[iv]; ok {
		return l
	}
	return fmt.Sprintf("interval(%d)", int(iv))
}

// ParseInterval resolves a canonical label to its Interval.
func ParseInterval(label string) (Interval, bool) {
	iv, ok := intervalsByLabel[label]
	return iv, ok
}

// Persisted reports whether closed bars for this interval are written to
// fx_klines / ClickHouse. 1s bars are memory-only per Task 6.3.14.
func (iv Interval) Persisted() bool { return iv != I1s }

// widthSecs is the fixed bucket width in seconds for every interval that
// has one (all but 1W and 1M). Every fixed width divides 86400 evenly, so
// buckets align to UTC day boundaries.
func (iv Interval) widthSecs() int64 {
	switch iv {
	case I1s:
		return 1
	case I1m:
		return 60
	case I5m:
		return 300
	case I15m:
		return 900
	case I30m:
		return 1800
	case I1h:
		return 3600
	case I2h:
		return 7200
	case I4h:
		return 14400
	case I6h:
		return 21600
	case I8h:
		return 28800
	case I1D:
		return 86400
	default:
		return 0 // 1W / 1M are calendar-aligned
	}
}

// Floor returns the UTC bucket start containing t. A timestamp exactly on
// a boundary belongs to the bucket starting AT that boundary (the newer
// bucket) — floor semantics give this directly.
func (iv Interval) Floor(t time.Time) time.Time {
	u := t.UTC()
	switch iv {
	case I1W:
		day := u.Truncate(24 * time.Hour)
		// Monday = 0 offset. time.Weekday: Sunday=0 ... Saturday=6.
		off := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -off)
	case I1M:
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		s := iv.widthSecs()
		sec := u.Unix()
		return time.Unix(sec-sec%s, 0).UTC()
	}
}

// Next returns the bucket start immediately after the bucket that starts
// at start (i.e. the exclusive bucket end). start must be a Floor result.
func (iv Interval) Next(start time.Time) time.Time {
	switch iv {
	case I1W:
		return start.Add(7 * 24 * time.Hour)
	case I1M:
		return start.AddDate(0, 1, 0)
	default:
		return start.Add(time.Duration(iv.widthSecs()) * time.Second)
	}
}
