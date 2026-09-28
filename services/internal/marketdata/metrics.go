// Task 6.3.2 SLA instrumentation (spec §24 #99: p99 WS push ≤ 100ms).
//
// The fanout path cannot measure a client's socket read — it measures
// the server-side portion: delta received → marshaled event frame
// enqueued to every subscriber. A bounded reservoir keeps the last N
// send durations for p99 computation without unbounded memory.
package marketdata

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics is the package's observability surface (counters are atomic;
// the latency reservoir is mutex-guarded).
type Metrics struct {
	// Connections / frames.
	ConnsOpened     atomic.Int64
	FramesPublished atomic.Int64
	FramesDropped   atomic.Int64 // slow-consumer saturation drops (Task 6.3.21)
	// L2 conflator counters.
	DeltasReceived  atomic.Int64
	DeltasDropped   atomic.Int64 // input-queue saturation (fail-loud gap)
	FlushesByWindow atomic.Int64 // 100ms-window flushes
	FlushesByCount  atomic.Int64 // 100-event flushes
	ConflationEmits atomic.Int64
	SeqMirrorErrors atomic.Int64
	// Task 6.3.22 / 6.3.24 / 6.3.17 counters.
	ResyncDirectives      atomic.Int64 // resync frames emitted (gap repair)
	EntitlementRejections atomic.Int64 // ENTITLEMENT_REQUIRED rejects (§10.7)
	PrivateFrames         atomic.Int64 // private:* fanout frames
	RefPriceStaleEmits    atomic.Int64 // referencePrice stale-flagged emits
	DepthVariantEmits     atomic.Int64 // parameterized depth@ emits (6.3.15)

	// framesByTier is the §10.7 per-tier fair-use counter: frames
	// delivered to sessions of each tier label (quote-count/bandwidth
	// observability). Keyed by tier string ("public", "basic", ...).
	framesByTier sync.Map // map[string]*atomic.Int64

	resMu   sync.Mutex
	latency []time.Duration // ring of recent send durations
	latIdx  int
	latFull bool
	latSum  atomic.Int64 // total nanoseconds observed
	latMax  atomic.Int64 // max nanoseconds observed
}

// latencyReservoir is the sample window for the p99 estimate — 8k
// samples is enough resolution at FX feed rates while staying bounded.
const latencyReservoir = 8192

// NewMetrics builds the metrics sink.
func NewMetrics() *Metrics {
	return &Metrics{latency: make([]time.Duration, latencyReservoir)}
}

// ObserveTierDelivery counts one delivered frame under the session's
// tier label — the §10.7 per-tier quote-count/bandwidth fair-use
// counters (Task 6.3.22 item 3). Anonymous conns count under "public".
func (m *Metrics) ObserveTierDelivery(tier string) {
	if tier == "" {
		tier = "public"
	}
	v, ok := m.framesByTier.Load(tier)
	if !ok {
		v, _ = m.framesByTier.LoadOrStore(tier, &atomic.Int64{})
	}
	v.(*atomic.Int64).Add(1)
}

// FramesByTier snapshots the fair-use counters for /health and tests.
func (m *Metrics) FramesByTier() map[string]int64 {
	out := map[string]int64{}
	m.framesByTier.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// ObserveSend records one Publish→enqueue duration.
func (m *Metrics) ObserveSend(d time.Duration) {
	if d < 0 {
		return
	}
	m.resMu.Lock()
	m.latency[m.latIdx] = d
	m.latIdx++
	if m.latIdx >= latencyReservoir {
		m.latIdx = 0
		m.latFull = true
	}
	m.resMu.Unlock()
	m.latSum.Add(d.Nanoseconds())
	for {
		cur := m.latMax.Load()
		if d.Nanoseconds() <= cur || m.latMax.CompareAndSwap(cur, d.Nanoseconds()) {
			break
		}
	}
}

// LatencySnapshot summarizes the reservoir: sample count, mean and the
// p50/p99 estimates over the retained window.
func (m *Metrics) LatencySnapshot() (samples int, mean, p50, p99, max time.Duration) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	n := m.latIdx
	if m.latFull {
		n = latencyReservoir
	}
	if n == 0 {
		return 0, 0, 0, 0, 0
	}
	vals := make([]time.Duration, n)
	copy(vals, m.latency[:n])
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	var sum int64
	for _, v := range vals {
		sum += int64(v)
	}
	p50i := n / 2
	p99i := n * 99 / 100
	if p99i >= n {
		p99i = n - 1
	}
	return n, time.Duration(sum / int64(n)), vals[p50i], vals[p99i], vals[n-1]
}
