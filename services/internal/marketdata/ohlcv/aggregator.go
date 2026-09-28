package ohlcv

import (
	"math"
	"time"

	"exchange/pkg/decimal"
)

// ring1sCap is the 60-second in-memory ring buffer for memory-only 1s
// bars (Task 6.3.14).
const ring1sCap = 60

// seriesKey identifies one (symbol, interval) candle series.
type seriesKey struct {
	symbol string
	iv     Interval
}

// series is the mutable aggregation state for one (symbol, interval).
//
// open holds candles whose bucket close_time+grace is still ahead of the
// watermark — with a sub-width grace that is at most 2 buckets (the live
// bucket plus the just-elapsed one still inside grace). Finalized buckets
// are removed and never reopened: lastFinalized is the immutability
// frontier.
type series struct {
	symbol string
	iv     Interval

	open      map[int64]*Candle // bucket start (unix sec) → accumulating bar
	watermark time.Time         // max event/advance time seen

	lastFinalized int64 // unix sec of the newest finalized bucket
	hasFinalized  bool

	lastClose   decimal.Decimal // close of the newest finalized real bar
	hasClose    bool            // false until the first real bar closes
	lastCloseID int64           // instrument id carried into gap candles

	ring []Candle // 1s only: last ring1sCap closed bars
}

// seriesUpdate reports how one interval absorbed (or rejected) a trade.
type seriesUpdate struct {
	Interval   Interval
	Candle     *Candle   // still-open candle the trade updated (nil when Late)
	Late       bool      // bucket already finalized → reconciliation
	LateBucket time.Time // the finalized bucket the trade targeted
}

// aggResult is the pure outcome of one handle/advance pass: the engine
// performs the side effects (persist/emit/metrics) afterwards.
type aggResult struct {
	updates     []seriesUpdate
	closed      []Candle
	gapOverflow int // buckets finalized without emission past maxGap
}

// aggregator holds all series. It is deterministic: wall-clock movement
// enters only through Advance, event-time movement through Handle.
type aggregator struct {
	intervals []Interval
	grace     time.Duration
	maxGap    int // gap-candle emission cap per finalize pass (flood guard)

	series map[seriesKey]*series
}

func newAggregator(intervals []Interval, grace time.Duration, maxGap int) *aggregator {
	return &aggregator{
		intervals: intervals,
		grace:     grace,
		maxGap:    maxGap,
		series:    make(map[seriesKey]*series),
	}
}

func (a *aggregator) seriesFor(symbol string, iv Interval) *series {
	k := seriesKey{symbol, iv}
	s, ok := a.series[k]
	if !ok {
		s = &series{symbol: symbol, iv: iv, open: make(map[int64]*Candle)}
		a.series[k] = s
	}
	return s
}

// handle folds one trade into every configured interval. The watermark
// moves to the event time first — finalizing any buckets whose grace has
// fully elapsed — then the trade lands in its (still-open or new) bucket.
func (a *aggregator) handle(ev TradeEvent) aggResult {
	res := aggResult{updates: make([]seriesUpdate, 0, len(a.intervals))}
	for _, iv := range a.intervals {
		s := a.seriesFor(ev.Symbol, iv)
		up, closed, gap := s.handle(ev, a.grace, a.maxGap)
		res.updates = append(res.updates, up)
		res.closed = append(res.closed, closed...)
		res.gapOverflow += gap
	}
	return res
}

// advance pushes every series watermark to now (monotonic) and returns
// the candles finalized by the move — including carry-forward gap bars.
func (a *aggregator) advance(now time.Time) aggResult {
	res := aggResult{}
	for _, s := range a.series {
		if now.After(s.watermark) {
			s.watermark = now.UTC()
		}
		closed, gap := s.finalizeReady(a.grace, a.maxGap)
		res.closed = append(res.closed, closed...)
		res.gapOverflow += gap
	}
	return res
}

// openCandle returns the live bar for (symbol, iv) at bucket start b.
func (a *aggregator) openCandle(symbol string, iv Interval, b time.Time) *Candle {
	s, ok := a.series[seriesKey{symbol, iv}]
	if !ok {
		return nil
	}
	return s.open[b.Unix()]
}

// ring1s returns the retained closed 1s bars for symbol (oldest first).
func (a *aggregator) ring1s(symbol string) []Candle {
	s, ok := a.series[seriesKey{symbol, I1s}]
	if !ok {
		return nil
	}
	out := make([]Candle, len(s.ring))
	copy(out, s.ring)
	return out
}

// handle implements the bucket-placement contract:
//   - bucket still in the open map      → accumulate (late-but-in-grace
//     trades for the just-elapsed bucket land here too);
//   - bucket at or before lastFinalized → late: reconcile, never reopen;
//   - anything newer                    → open a fresh candle.
func (s *series) handle(ev TradeEvent, grace time.Duration, maxGap int) (seriesUpdate, []Candle, int) {
	var closed []Candle
	var gap int
	if ev.Ts.After(s.watermark) {
		s.watermark = ev.Ts.UTC()
		closed, gap = s.finalizeReady(grace, maxGap)
	}
	b := s.iv.Floor(ev.Ts)
	bu := b.Unix()
	if c, ok := s.open[bu]; ok {
		c.addTrade(ev)
		return seriesUpdate{Interval: s.iv, Candle: c}, closed, gap
	}
	if s.hasFinalized && bu <= s.lastFinalized {
		return seriesUpdate{Interval: s.iv, Late: true, LateBucket: b}, closed, gap
	}
	c := newCandle(ev, s.iv, b)
	s.open[bu] = c
	c.addTrade(ev)
	return seriesUpdate{Interval: s.iv, Candle: c}, closed, gap
}

// finalizeReady finalizes every bucket whose close_time+grace is at or
// before the watermark. Buckets without trades emit a carry-forward bar
// once a real close exists; buckets before the first ever close finalize
// silently (no previous close to carry — fabricating a price is worse
// than an honest gap). maxGap bounds gap-candle emission per pass so a
// corrupt far-future timestamp cannot materialize an unbounded run of
// synthesized bars.
func (s *series) finalizeReady(grace time.Duration, maxGap int) ([]Candle, int) {
	var start time.Time
	if s.hasFinalized {
		start = s.iv.Next(time.Unix(s.lastFinalized, 0).UTC())
	} else {
		if len(s.open) == 0 {
			return nil, 0
		}
		min := int64(math.MaxInt64)
		for k := range s.open {
			if k < min {
				min = k
			}
		}
		start = time.Unix(min, 0).UTC()
	}
	var out []Candle
	overflow := 0
	gaps := 0
	for b := start; ; b = s.iv.Next(b) {
		closeAt := s.iv.Next(b).Add(grace)
		if closeAt.After(s.watermark) {
			break
		}
		if c, ok := s.open[b.Unix()]; ok {
			c.Closed = true
			out = append(out, *c)
			s.lastClose, s.hasClose = c.Close, true
			s.lastCloseID = c.InstrumentID
			delete(s.open, b.Unix())
			s.pushRing(*c)
		} else if s.hasClose && s.iv != I1s {
			// Gap synthesis is skipped for the memory-only 1s tier: a
			// tick-resolution stream produces no useful carry-forward
			// filler and the 60-bar ring must hold real bars, not
			// synthesized quiet seconds (see doc.go).
			if gaps < maxGap {
				gc := gapCandle(s.symbol, s.lastCloseID, s.iv, b, s.lastClose)
				out = append(out, gc)
				s.pushRing(gc)
				gaps++
			} else {
				overflow++
			}
		}
		s.lastFinalized = b.Unix()
		s.hasFinalized = true
	}
	return out, overflow
}

// pushRing appends a closed bar to the 1s memory ring (no-op on other
// intervals — they persist instead).
func (s *series) pushRing(c Candle) {
	if s.iv != I1s {
		return
	}
	if len(s.ring) == ring1sCap {
		copy(s.ring, s.ring[1:])
		s.ring = s.ring[:ring1sCap-1]
	}
	s.ring = append(s.ring, c)
}
