package main

import (
	"testing"
	"time"
)

func TestLatHistPercentile(t *testing.T) {
	var h latHist
	// 1000 observations uniformly at 10us and 100 at 1ms.
	for i := 0; i < 1000; i++ {
		h.observe(10_000)
	}
	for i := 0; i < 100; i++ {
		h.observe(1_000_000)
	}
	if got := h.percentile(0.50); got != 10_999 {
		t.Fatalf("p50 = %d, want 10999", got)
	}
	// 1000/1100 = 90.9% at 10us — p99 lands in the 1ms group.
	if got := h.percentile(0.99); got != 1_000_999 {
		t.Fatalf("p99 = %d, want 1000999", got)
	}
	if got := h.percentile(0.999); got != 1_000_999 {
		t.Fatalf("p999 = %d, want 1000999", got)
	}
	if h.count.Load() != 1100 {
		t.Fatalf("count = %d", h.count.Load())
	}
	if h.max.Load() != 1_000_000 {
		t.Fatalf("max = %d", h.max.Load())
	}
}

func TestLatHistOverflow(t *testing.T) {
	var h latHist
	h.observe(0)
	h.observe(25_000_000) // beyond 20ms range -> overflow bucket
	if h.count.Load() != 2 {
		t.Fatalf("count = %d", h.count.Load())
	}
	if got := h.percentile(0.999); got != 25_000_000 {
		t.Fatalf("overflow percentile = %d, want max 25000000", got)
	}
}

func TestLatHistPromCounts(t *testing.T) {
	var h latHist
	for _, ns := range []int64{1_000, 4_999, 6_000, 40_000, 2_000_000} {
		h.observe(ns)
	}
	c := h.promCounts()
	// le=5000: buckets with lo<=5000 are 0..5 -> obs 1000,4999 (6000 is in
	// bucket 6, above the bound) = 2.
	if c[0] != 2 {
		t.Fatalf("le=5000 count = %d, want 2", c[0])
	}
	// le=50000: adds the 40000 obs -> 4
	if c[3] != 4 {
		t.Fatalf("le=50000 count = %d, want 4", c[3])
	}
	// +Inf -> all 5
	if c[len(c)-1] != 5 {
		t.Fatalf("+Inf count = %d, want 5", c[len(c)-1])
	}
	// monotonic non-decreasing
	for i := 1; i < len(c); i++ {
		if c[i] < c[i-1] {
			t.Fatalf("non-monotonic prom counts: %v", c)
		}
	}
}

func TestDupSet(t *testing.T) {
	d := newDupSet()
	if d.seenOrAdd(7) {
		t.Fatal("first sight of 7 reported dup")
	}
	if !d.seenOrAdd(7) {
		t.Fatal("second sight of 7 not reported dup")
	}
	if d.seenOrAdd(8) || d.seenOrAdd(1<<seenBlockShift) {
		t.Fatal("new ids reported dup")
	}
	if d.seen != 3 {
		t.Fatalf("seen = %d, want 3", d.seen)
	}
}

func TestDupSetEviction(t *testing.T) {
	d := newDupSet()
	// Fill seenMaxBlocks+2 distinct blocks.
	for blk := uint64(0); blk < seenMaxBlocks+2; blk++ {
		id := blk << seenBlockShift
		if d.seenOrAdd(id) {
			t.Fatalf("id %d falsely dup", id)
		}
	}
	if len(d.blocks) > seenMaxBlocks {
		t.Fatalf("blocks not bounded: %d", len(d.blocks))
	}
	// id 0 lies below the evicted (tracked) range -> counted dup.
	if !d.seenOrAdd(0) {
		t.Fatal("below-window id not counted dup")
	}
	// An id inside a still-tracked block re-checks against the bitmap.
	last := uint64(seenMaxBlocks+1) << seenBlockShift
	if !d.seenOrAdd(last) {
		t.Fatal("still-tracked id not dup")
	}
}

func TestCorrRing(t *testing.T) {
	c := newCorrRing(1000)
	c.store(1005, 42, false)
	if ts, tkr, ok := c.lookup(1005); !ok || ts != 42 || tkr {
		t.Fatalf("lookup 1005 = %d,%v,%v", ts, tkr, ok)
	}
	c.store(1007, 99, true)
	if ts, tkr, ok := c.lookup(1007); !ok || ts != 99 || !tkr {
		t.Fatalf("taker lookup 1007 = %d,%v,%v", ts, tkr, ok)
	}
	if _, _, ok := c.lookup(999); ok {
		t.Fatal("lookup below base hit")
	}
	if _, _, ok := c.lookup(1006); ok {
		t.Fatal("lookup of unset id hit")
	}
	c.drop(1005)
	if _, _, ok := c.lookup(1005); ok {
		t.Fatal("lookup after drop hit")
	}
	// Overwrite semantics: id evicted by a later id wrapping into same slot.
	c.store(1005, 1, false)
	c.store(1005+(1<<corrRingLog2), 2, true)
	if ts, _, ok := c.lookup(1005); ok {
		t.Fatalf("stale alias lookup hit ts=%d", ts)
	}
	if ts, tkr, ok := c.lookup(1005 + (1 << corrRingLog2)); !ok || ts != 2 || !tkr {
		t.Fatalf("wrapped lookup = %d,%v,%v", ts, tkr, ok)
	}
}

func TestPacerDeadline(t *testing.T) {
	start := time.Unix(0, 0)
	p := newPacer(50_000, start) // 20us interval
	if d := p.deadline(1).Sub(start); d != 20*time.Microsecond {
		t.Fatalf("deadline(1) = %v, want 20us", d)
	}
	if d := p.deadline(50_000).Sub(start); d != time.Second {
		t.Fatalf("deadline(50k) = %v, want 1s", d)
	}
	// Real-time pacing sanity: 10k/s over ~30ms -> ~300 sends, loose bound.
	p = newPacer(10_000, time.Now())
	n := 0
	until := time.Now().Add(30 * time.Millisecond)
	for time.Now().Before(until) {
		p.next()
		n++
	}
	if n < 250 || n > 350 {
		t.Fatalf("paced sends in 30ms = %d, want ~300", n)
	}
}

func TestMetricsTextContains(t *testing.T) {
	st := &stats{}
	st.ordersSent.Store(5)
	st.fills.Store(2)
	st.fillLat.observe(12_000)
	out := string(metricsText(st))
	for _, want := range []string{
		"soak_orders_sent_total 5",
		"soak_fills_total 2",
		"soak_latency_ns_bucket{le=\"50000\"} 1",
		"soak_latency_ns_bucket{le=\"+Inf\"} 1",
		"soak_latency_ns_count 1",
		"soak_orders_per_second 0",
		"soak_ring_drops_total 0",
	} {
		if !contains(out, want) {
			t.Fatalf("metrics output missing %q\n%s", want, out)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
