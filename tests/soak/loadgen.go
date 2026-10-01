// Phase-02.5 Task 2.5.3.1 — soak load generator for the C++ matching engine.
//
// Drives a paced stream of FlatBuffers Event{OrderNew} messages into the
// shard's shared-memory IPC ring ({base}_{shard}_in) while a dedicated drain
// goroutine consumes the outbound ring ({base}_{shard}_out): TradeFill events
// feed a send->fill-observed latency histogram and a duplicate-trade_id
// detector; OrderCancel events (the engine's terminal/reject notice) feed a
// parallel send->terminal latency histogram; BookSnapshot / TimeTick events
// are counted.
//
// Metrics are exposed as hand-rolled Prometheus text on -metrics-addr; a JSON
// report lands at -report on SIGINT/SIGTERM, -duration expiry, or engine
// death (unless -reconnect keeps the run alive across restarts).
//
// Order flow model: a synthetic mid-price random walk around a EURUSD-style
// reference (1.08500 at the canonical 10^8 pipette scale, i.e. 108'500'000
// ticks; tick_size_ticks=1'000, pip_size_ticks=10'000 per the engine's bound
// Instrument). Most orders rest passively +-1..20 pips off mid; -cross-pct
// percent are priced through the book to force fills. All prices are emitted
// on the 1'000-tick grid so check-11 (tick quantization) always passes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
)

// ---------------------------------------------------------------------------
// Price model constants (Instrument ticks, 10^8 scale — core/include/book/
// Instrument.hpp; the engine binds tick_size=1'000, pip_size=10'000).
// ---------------------------------------------------------------------------

const (
	tickSizeTicks     = 1_000       // 1 pipette (0.00001) at 10^8 scale
	pipSizeTicks      = 10_000      // 1 pip = 10 ticks
	midStartTicks     = 108_500_000 // 1.08500 EURUSD-style reference
	midMaxWanderTicks = 1_000_000   // keep the walk within +-1% of start
	passiveBandPips   = 20          // resting orders land +-1..20 pips off mid
	crossReachPips    = 25          // crossing orders price through the whole band

	// lotUnitTicks scales "lots" into qty_units (10^8 base units). The bound
	// instrument sets lot_size_units=1, so any positive qty is a valid lot
	// multiple; we model 1 lot = 1e5 base units = 1e13 qty_units.
	lotQtyUnits = 100_000 * 100_000_000
)

// ---------------------------------------------------------------------------
// Latency histogram: 1us-resolution fixed buckets up to 20ms + overflow.
// Percentiles for the report and the coarser Prometheus bucket view are both
// derived from this one array (single-writer drain goroutine; atomic cells so
// the metrics handler can read concurrently).
// ---------------------------------------------------------------------------

const (
	latMicroBuckets = 20_000 // index = ns/1000, clamped
	latOverflowIdx  = latMicroBuckets
	latArrLen       = latMicroBuckets + 1
)

// Prom exposition buckets required by Task 2.5.3.1 (ns).
var promBucketBounds = []uint64{
	5_000, 10_000, 25_000, 50_000, 100_000, 250_000, 500_000, 1_000_000, 5_000_000,
}

type latHist struct {
	buckets [latArrLen]uint64
	count   atomic.Uint64
	sum     atomic.Uint64
	max     atomic.Uint64
}

func (h *latHist) observe(ns int64) {
	if ns < 0 {
		ns = 0
	}
	idx := ns / 1_000
	if idx > latOverflowIdx {
		idx = latOverflowIdx
	}
	atomic.AddUint64(&h.buckets[idx], 1)
	h.count.Add(1)
	h.sum.Add(uint64(ns))
	for {
		m := h.max.Load()
		if uint64(ns) <= m || h.max.CompareAndSwap(m, uint64(ns)) {
			break
		}
	}
}

// percentile returns the bucketed value (ns) at quantile q in [0,1], or 0
// when empty. Resolution is 1us — the histogram's own granularity.
func (h *latHist) percentile(q float64) uint64 {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	want := uint64(float64(total)*q + 0.999) // ceil: first index reaching q
	var acc uint64
	for i := 0; i < latArrLen; i++ {
		acc += atomic.LoadUint64(&h.buckets[i])
		if acc >= want {
			if i >= latOverflowIdx {
				// Beyond the bucketed range — report the observed max.
				return h.max.Load()
			}
			return uint64(i)*1_000 + 999 // top of bucket
		}
	}
	return h.max.Load()
}

// promCounts returns cumulative counts aligned with promBucketBounds plus a
// final +Inf element. Bucket i covers latencies [i*1000, i*1000+999] ns, so
// bound B (ns) covers buckets 0..B/1000 — a <=1us boundary error.
func (h *latHist) promCounts() []uint64 {
	out := make([]uint64, len(promBucketBounds)+1)
	var acc uint64
	bi := 0
	for i := 0; i < latArrLen; i++ {
		v := atomic.LoadUint64(&h.buckets[i])
		acc += v
		for bi < len(promBucketBounds) && int64(i) > int64(promBucketBounds[bi]/1_000) {
			out[bi] = acc - v // buckets 0..i-1 all have lo <= bound
			bi++
		}
	}
	for ; bi < len(out); bi++ {
		out[bi] = acc
	}
	return out
}

// ---------------------------------------------------------------------------
// Order-id -> send-time correlation ring. Order ids are issued sequentially
// from -order-id-base, so a power-of-two ring indexed by (id-base) gives a
// bounded map that self-evicts by overwrite. ids[] is the presence stamp
// (published after ts[]); a stale slot fails the id equality check.
// ---------------------------------------------------------------------------

const corrRingLog2 = 21 // 2M slots * 16B = 32MB ceiling

// ts[i] packs the send stamp with a taker flag in the top bit — a ns-since-
// start stamp can't reach 2^63. Tick-to-trade latency is measured on
// executable (crossing) orders only: a resting GTC fill is delayed by market
// flow, so including it would report order lifetime, not engine latency.
const corrTakerBit = uint64(1) << 63

type corrRing struct {
	ids  []uint64
	ts   []uint64 // sendNs | corrTakerBit
	mask uint64
	base uint64
}

func newCorrRing(base uint64) *corrRing {
	n := uint64(1) << corrRingLog2
	return &corrRing{
		ids:  make([]uint64, n),
		ts:   make([]uint64, n),
		mask: n - 1,
		base: base,
	}
}

func (c *corrRing) store(orderID uint64, sendNs uint64, taker bool) {
	i := (orderID - c.base) & c.mask
	v := sendNs
	if taker {
		v |= corrTakerBit
	}
	atomic.StoreUint64(&c.ts[i], v)
	atomic.StoreUint64(&c.ids[i], orderID)
}

// lookup returns (sendNs, isTaker, ok).
func (c *corrRing) lookup(orderID uint64) (uint64, bool, bool) {
	i := (orderID - c.base) & c.mask
	if atomic.LoadUint64(&c.ids[i]) != orderID {
		return 0, false, false
	}
	v := atomic.LoadUint64(&c.ts[i])
	return v &^ corrTakerBit, v&corrTakerBit != 0, true
}

func (c *corrRing) drop(orderID uint64) {
	i := (orderID - c.base) & c.mask
	atomic.StoreUint64(&c.ids[i], 0)
}

// ---------------------------------------------------------------------------
// Duplicate-id detector: block bitmap keyed by id>>20 (1M ids per 128KB
// bitmap), capped at tradeMaxBlocks blocks with lowest-key eviction. Engine
// trade_ids increase monotonically (recovery re-seeds at max+1), so an id
// arriving below the still-tracked range was necessarily emitted before —
// counted as a duplicate.
// ---------------------------------------------------------------------------

const (
	seenBlockShift = 20
	seenBlockWords = 1 << (seenBlockShift - 6) // 16_384 u64 = 128KiB
	seenMaxBlocks  = 32                        // 4MB, covers last 32M ids
)

type dupSet struct {
	blocks map[uint64]*[seenBlockWords]uint64
	minKey uint64
	seen   uint64 // distinct ids added
}

func newDupSet() *dupSet {
	return &dupSet{blocks: make(map[uint64]*[seenBlockWords]uint64)}
}

// seenOrAdd returns true when id was already recorded (or lies below the
// tracked window and must therefore have been emitted earlier).
func (d *dupSet) seenOrAdd(id uint64) bool {
	key := id >> seenBlockShift
	if len(d.blocks) > 0 && key < d.minKey {
		return true
	}
	b, ok := d.blocks[key]
	if !ok {
		if len(d.blocks) >= seenMaxBlocks {
			delete(d.blocks, d.minKey)
			d.recomputeMin()
		}
		b = new([seenBlockWords]uint64)
		d.blocks[key] = b
		if len(d.blocks) == 1 || key < d.minKey {
			d.minKey = key
		}
	}
	off := id & (1<<seenBlockShift - 1)
	w := off >> 6
	bit := uint64(1) << (off & 63)
	if b[w]&bit != 0 {
		return true
	}
	b[w] |= bit
	d.seen++
	return false
}

func (d *dupSet) recomputeMin() {
	first := true
	var m uint64
	for k := range d.blocks {
		if first || k < m {
			m = k
			first = false
		}
	}
	d.minKey = m
}

// ---------------------------------------------------------------------------
// Counters / gauges shared between send loop, drain loop, monitor, HTTP.
// ---------------------------------------------------------------------------

type stats struct {
	ordersSent     atomic.Uint64
	sendDrops      atomic.Uint64 // Send() returned false past the retry budget
	fills          atomic.Uint64
	cancels        atomic.Uint64 // OrderCancel terminal notices (incl. rejects)
	snapshots      atomic.Uint64
	dbgSlow        atomic.Uint64 // diagnostic: count of >100ms taker samples
	timeTicks      atomic.Uint64
	otherEvents    atomic.Uint64
	decodeErrors   atomic.Uint64
	dupTradeIDs    atomic.Uint64
	myOrdersFilled atomic.Uint64 // fills touching >=1 of our order_ids
	foreignFills   atomic.Uint64 // fills referencing only unknown order_ids

	ordersPerSec  atomic.Uint64 // last completed 1s window send count
	outOccupancy  atomic.Uint64
	ringDrops     atomic.Uint64
	producerAlive atomic.Bool

	fillLat   latHist
	cancelLat latHist
	// engineLat is send->fill-EMITTED latency (engine Event.ts is
	// CLOCK_REALTIME, same domain as the send stamp) — it excludes this
	// process's drain-observation lag, which on a contended host inflates
	// fillLat's tail by tens of ms (Phase-02.5 probe: one ~105ms drain gap
	// produced the whole p99 bucket while engine beat staleness stayed <1ms).
	engineLat latHist

	windowMin   atomic.Uint64 // min 1s sends (first window seeds it)
	windowMax   atomic.Uint64
	windowSum   atomic.Uint64
	windowCount atomic.Uint64
}

func (s *stats) recordWindow(n uint64) {
	s.ordersPerSec.Store(n)
	s.windowSum.Add(n)
	s.windowCount.Add(1)
	for {
		m := s.windowMin.Load()
		if (m != 0 && n >= m) || s.windowMin.CompareAndSwap(m, n) {
			break
		}
	}
	for {
		m := s.windowMax.Load()
		if n <= m || s.windowMax.CompareAndSwap(m, n) {
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Pacer: integer-nanosecond deadline scheduler. Deadline i is
// start + i*interval; sleep while >1ms early, spin for the last stretch.
// ---------------------------------------------------------------------------

type pacer struct {
	intervalNs float64
	start      time.Time
	i          int64
}

func newPacer(rate float64, start time.Time) pacer {
	if rate <= 0 {
		rate = 1
	}
	return pacer{intervalNs: float64(time.Second) / rate, start: start}
}

// deadline returns the scheduled send time for the i-th message.
func (p *pacer) deadline(i int64) time.Time {
	return p.start.Add(time.Duration(float64(i) * p.intervalNs))
}

// next blocks until the next scheduled send instant and returns it.
func (p *pacer) next() time.Time {
	p.i++
	target := p.deadline(p.i)
	for {
		rem := time.Until(target)
		if rem <= 0 {
			return target
		}
		if rem > time.Millisecond {
			time.Sleep(rem - time.Millisecond)
			continue
		}
		runtime.Gosched() // sub-ms spin without burning a runnable goroutine slot
	}
}

// rebase drops the accumulated schedule lag after a stall (backpressure or
// engine outage) so we resume at rate instead of bursting to catch up.
func (p *pacer) rebase(now time.Time) {
	p.i = 0
	p.start = now
}

// ---------------------------------------------------------------------------
// Metrics exposition (hand-rolled Prometheus text; no external dep).
// ---------------------------------------------------------------------------

func metricsText(s *stats) []byte {
	var b []byte
	w := func(format string, a ...any) {
		b = append(b, fmt.Sprintf(format, a...)...)
	}
	w("# HELP soak_orders_sent_total Orders successfully written to the ingress ring.\n")
	w("# TYPE soak_orders_sent_total counter\n")
	w("soak_orders_sent_total %d\n", s.ordersSent.Load())
	w("# HELP soak_order_send_drops_total Order sends dropped after Send() backpressure retries.\n")
	w("# TYPE soak_order_send_drops_total counter\n")
	w("soak_order_send_drops_total %d\n", s.sendDrops.Load())
	w("# HELP soak_fills_total TradeFill events observed on the outbound ring.\n")
	w("# TYPE soak_fills_total counter\n")
	w("soak_fills_total %d\n", s.fills.Load())
	w("# HELP soak_cancels_total OrderCancel terminal notices observed (cancels and engine rejects).\n")
	w("# TYPE soak_cancels_total counter\n")
	w("soak_cancels_total %d\n", s.cancels.Load())
	w("# HELP soak_book_snapshots_total BookSnapshot events observed.\n")
	w("# TYPE soak_book_snapshots_total counter\n")
	w("soak_book_snapshots_total %d\n", s.snapshots.Load())
	w("# HELP soak_time_ticks_total TimeTick events observed.\n")
	w("# TYPE soak_time_ticks_total counter\n")
	w("soak_time_ticks_total %d\n", s.timeTicks.Load())
	w("# HELP soak_other_events_total Outbound events of other/unknown types.\n")
	w("# TYPE soak_other_events_total counter\n")
	w("soak_other_events_total %d\n", s.otherEvents.Load())
	w("# HELP soak_decode_errors_total Outbound frames that failed to decode.\n")
	w("# TYPE soak_decode_errors_total counter\n")
	w("soak_decode_errors_total %d\n", s.decodeErrors.Load())
	w("# HELP soak_duplicate_trade_ids_total TradeFill trade_ids already seen.\n")
	w("# TYPE soak_duplicate_trade_ids_total counter\n")
	w("soak_duplicate_trade_ids_total %d\n", s.dupTradeIDs.Load())
	w("# HELP soak_my_orders_filled_total Fills referencing at least one generated order_id.\n")
	w("# TYPE soak_my_orders_filled_total counter\n")
	w("soak_my_orders_filled_total %d\n", s.myOrdersFilled.Load())
	w("# HELP soak_foreign_fills_total Fills referencing no generated order_id (stale book / replay).\n")
	w("# TYPE soak_foreign_fills_total counter\n")
	w("soak_foreign_fills_total %d\n", s.foreignFills.Load())
	w("# HELP soak_orders_per_second Orders sent in the last completed 1s window.\n")
	w("# TYPE soak_orders_per_second gauge\n")
	w("soak_orders_per_second %d\n", s.ordersPerSec.Load())
	w("# HELP soak_out_queue_occupancy Pending unread messages on the outbound ring.\n")
	w("# TYPE soak_out_queue_occupancy gauge\n")
	w("soak_out_queue_occupancy %d\n", s.outOccupancy.Load())
	w("# HELP soak_ring_drops_total Ingress ring refused writes (ring full, engine-side view).\n")
	w("# TYPE soak_ring_drops_total counter\n")
	w("soak_ring_drops_total %d\n", s.ringDrops.Load())
	w("# HELP soak_producer_alive Whether the outbound ring's producer pid is alive.\n")
	w("# TYPE soak_producer_alive gauge\n")
	pv := 0
	if s.producerAlive.Load() {
		pv = 1
	}
	w("soak_producer_alive %d\n", pv)

	renderHist := func(name, help string, h *latHist) {
		w("# HELP %s %s\n", name, help)
		w("# TYPE %s histogram\n", name)
		counts := h.promCounts()
		for i, bound := range promBucketBounds {
			w("%s_bucket{le=\"%d\"} %d\n", name, bound, counts[i])
		}
		w("%s_bucket{le=\"+Inf\"} %d\n", name, counts[len(counts)-1])
		w("%s_sum %d\n", name, h.sum.Load())
		w("%s_count %d\n", name, h.count.Load())
	}
	renderHist("soak_latency_ns",
		"Send to fill-observed latency in nanoseconds.", &s.fillLat)
	renderHist("soak_engine_latency_ns",
		"Send to fill-emitted latency in nanoseconds (engine Event.ts; excludes drain-observation lag).",
		&s.engineLat)
	renderHist("soak_cancel_latency_ns",
		"Send to OrderCancel-observed latency in nanoseconds (engine terminal rejects/cancels).",
		&s.cancelLat)
	return b
}

// ---------------------------------------------------------------------------
// JSON report.
// ---------------------------------------------------------------------------

type latencyReport struct {
	P50   uint64 `json:"p50"`
	P99   uint64 `json:"p99"`
	P999  uint64 `json:"p999"`
	Max   uint64 `json:"max"`
	Mean  uint64 `json:"mean"`
	Count uint64 `json:"count"`
}

type soakReport struct {
	Started           string        `json:"started"`
	Ended             string        `json:"ended"`
	DurationS         float64       `json:"duration_s"`
	OrdersSent        uint64        `json:"orders_sent"`
	Fills             uint64        `json:"fills"`
	Cancels           uint64        `json:"cancels"`
	Snapshots         uint64        `json:"book_snapshots"`
	TimeTicks         uint64        `json:"time_ticks"`
	OtherEvents       uint64        `json:"other_events"`
	DecodeErrors      uint64        `json:"decode_errors"`
	DuplicateTradeIDs uint64        `json:"duplicate_trade_ids"`
	MyOrdersFilled    uint64        `json:"my_orders_filled"`
	MyDistinctOrders  uint64        `json:"my_distinct_orders_in_fills"`
	ForeignFills      uint64        `json:"foreign_fills"`
	SendDrops         uint64        `json:"send_drops"`
	RingDrops         uint64        `json:"ring_drops"`
	LatencyNS         latencyReport `json:"latency_ns"`
	EngineLatencyNS   latencyReport `json:"engine_latency_ns"`
	CancelLatencyNS   latencyReport `json:"cancel_latency_ns"`
	Throughput        struct {
		Min1s  uint64  `json:"min_1s"`
		Mean1s float64 `json:"mean_1s"`
		Max1s  uint64  `json:"max_1s"`
	} `json:"throughput"`
	TargetRate   float64 `json:"target_rate"`
	StopReason   string  `json:"stop_reason"`
	EngineDied   bool    `json:"engine_died"`
	Shard        uint16  `json:"shard"`
	InstrumentID uint32  `json:"instrument_id"`

	// Flat aliases for tests/soak/monitor.sh's grep-based jget() — it
	// matches `"key": <number>` literally, so percentiles must also exist as
	// MICROSECONDS under these exact key names.
	DupTradeIDsFlat uint64  `json:"dup_trade_ids"`
	AchievedRate    float64 `json:"achieved_rate"`
	P50US           float64 `json:"p50_us"`
	P99US           float64 `json:"p99_us"`
	P999US          float64 `json:"p999_us"`
	// Engine-side aliases: latency measured to the engine's emit stamp —
	// the honest number when drain-observation lag inflates the tail.
	EngineP50US  float64 `json:"engine_p50_us"`
	EngineP99US  float64 `json:"engine_p99_us"`
	EngineP999US float64 `json:"engine_p999_us"`
}

func fillLatencyReport(h *latHist) latencyReport {
	r := latencyReport{
		P50:   h.percentile(0.50),
		P99:   h.percentile(0.99),
		P999:  h.percentile(0.999),
		Max:   h.max.Load(),
		Count: h.count.Load(),
	}
	if r.Count > 0 {
		r.Mean = h.sum.Load() / r.Count
	}
	return r
}

// parseDuration accepts a Go duration string ("72h", "15s") or a bare number
// interpreted as seconds ("3600" -> 1h). "0"/"0s"/empty mean "until signal".
func parseDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("not a duration or seconds: %q", s)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// ---------------------------------------------------------------------------
// Main.
// ---------------------------------------------------------------------------

func main() {
	var (
		base        = flag.String("base", ipc.DefaultShmBase, "shm ring base name")
		shard       = flag.Uint("shard", 0, "shard id")
		instrument  = flag.Uint("instrument", 7, "instrument_id stamped on orders")
		rate        = flag.Float64("rate", 50000, "target orders/sec")
		durationRaw = flag.String("duration", "0",
			"run length: Go duration (72h, 15s) or bare seconds (3600); 0 = until signal")
		metricsAddr = flag.String("metrics-addr", ":9464", "Prometheus metrics listen address")
		reportPath  = flag.String("report", "soak-report.json", "JSON report output path")
		orderIDBase = flag.Uint64("order-id-base", 1, "first order_id; ids increment")
		accounts    = flag.Uint64("accounts", 1000, "round-robin account_id space")
		crossPct    = flag.Float64("cross-pct", 2, "percent of orders priced through the book")
		seed        = flag.Int64("seed", 42, "PRNG seed")
		reconnect   = flag.Bool("reconnect", false,
			"keep running across engine death/restart instead of exiting")
	)
	flag.Parse()

	// -duration accepts both "72h"/"15s" and bare seconds ("3600") — the
	// monitor.sh orchestrator passes seconds, humans tend to pass units.
	duration, err := parseDuration(*durationRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad -duration %q: %v\n", *durationRaw, err)
		os.Exit(2)
	}

	started := time.Now()
	// Latency correlation stamps are absolute UnixNano on both ends:
	// the corr ring stores the send's UnixNano and Event.ts carries the
	// engine's CLOCK_REALTIME emit stamp (same host, same domain).

	st := &stats{}
	st.producerAlive.Store(false)

	// ---- metrics endpoint ---------------------------------------------------
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write(metricsText(st))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("soak loadgen: /metrics\n"))
	})
	srv := &http.Server{Addr: *metricsAddr, Handler: mux}
	ln, err := net.Listen("tcp", *metricsAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "metrics listen %s: %v\n", *metricsAddr, err)
		os.Exit(1)
	}
	go func() { _ = srv.Serve(ln) }()

	// ---- lifecycle plumbing ---------------------------------------------------
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	stopSend := make(chan struct{}) // closed -> send loop exits
	engineDead := make(chan struct{}, 1)
	var stopOnce atomic.Bool
	var stopReason atomic.Value // string
	requestStop := func(why string) {
		if stopOnce.CompareAndSwap(false, true) {
			stopReason.Store(why)
			close(stopSend)
		}
	}

	var durTimer *time.Timer
	if duration > 0 {
		durTimer = time.AfterFunc(duration, func() { requestStop("duration") })
		defer durTimer.Stop()
	}
	go func() {
		s := <-sigCh
		requestStop("signal:" + s.String())
	}()

	// ---- channel attach (retry until the engine's rings exist) ---------------
	var ch *ipc.Channel
	attach := func() error {
		var err error
		ch, err = ipc.OpenChannel(*base, uint16(*shard), ipc.EndpointGateway,
			false, ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload)
		return err
	}
	var lastAttachErr error
	retries := 0
	for {
		if err := attach(); err == nil {
			break
		} else {
			lastAttachErr = err
			if retries%50 == 0 {
				fmt.Fprintf(os.Stderr, "attach %s_%d: %v (retrying every 100ms)\n",
					*base, *shard, err)
			}
			retries++
			select {
			case <-stopSend:
				fmt.Fprintf(os.Stderr, "aborted during attach: %v\n", lastAttachErr)
				os.Exit(2)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	defer func() { _ = ch.Close() }()
	st.producerAlive.Store(ch.ProducerAlive())

	// ---- drain goroutine ------------------------------------------------------
	// Polls _out continuously; never blocks the send loop. On shutdown it
	// exits after a 500ms quiet period (no events observed) — NOT on
	// occupancy==0, because the engine's in-ring backlog can still be
	// producing events when the out ring momentarily reads empty.
	// A slot Peek() refuses (len field out of bounds) would wedge the tail
	// forever; after a 100ms grace a poison slot is skipped via Consume() and
	// counted as a decode error.
	drainDone := make(chan struct{})
	corr := newCorrRing(*orderIDBase)
	tradeIDs := newDupSet()
	myOrderIDs := newDupSet()
	go func() {
		defer close(drainDone)
		buf := make([]byte, 64<<10)
		lastEvent := time.Now()
		var stuckSince time.Time
		for {
			n := ch.Poll(buf)
			if n <= 0 {
				if n < 0 {
					st.decodeErrors.Add(1) // payload bigger than buf — shouldn't happen
				}
				if n == 0 && ch.Occupancy() > 0 {
					// Peek() saw a slot it can't decode — poison frame.
					if stuckSince.IsZero() {
						stuckSince = time.Now()
					}
					if time.Since(stuckSince) > 100*time.Millisecond {
						st.decodeErrors.Add(1)
						ch.Consume()
						stuckSince = time.Time{}
						continue
					}
				} else {
					stuckSince = time.Time{}
				}
				if stopOnce.Load() && time.Since(lastEvent) > 500*time.Millisecond {
					return
				}
				runtime.Gosched()
				continue
			}
			lastEvent = time.Now()
			ev := ipc.DecodeEvent(buf[:n])
			if ev == nil {
				st.decodeErrors.Add(1)
				continue
			}
			switch ev.TypeType() {
			case wire.EventTypeTradeFill:
				tf := ipc.EventTradeFill(ev)
				if tf == nil {
					st.decodeErrors.Add(1)
					continue
				}
				st.fills.Add(1)
				if tradeIDs.seenOrAdd(tf.TradeId()) {
					st.dupTradeIDs.Add(1)
				}
				mine := false
				for _, oid := range [2]uint64{tf.BuyOrderId(), tf.SellOrderId()} {
					if oid >= *orderIDBase {
						myOrderIDs.seenOrAdd(oid)
						mine = true
					}
					if sentNs, taker, ok := corr.lookup(oid); ok {
						if taker {
							// Two clocks, one domain: corr stores the send's
							// UnixNano and Event.ts is the engine's
							// CLOCK_REALTIME emit stamp (same host) — so
							// engineLat excludes this goroutine's drain lag
							// while fillLat keeps the full observed path.
							d := time.Now().UnixNano() - int64(sentNs)
							st.fillLat.observe(d)
							st.engineLat.observe(int64(ev.Ts()) - int64(sentNs))
							if d > 100_000_000 && st.dbgSlow.Add(1) <= 8 {
								fmt.Fprintf(os.Stderr,
									"SLOWFILL oid=%d d=%dms sendUnix=%d obsUnix=%d engTs=%d tid=%d\n",
									oid, d/1e6, sentNs, time.Now().UnixNano(),
									ev.Ts(), tf.TradeId())
							}
						}
						corr.drop(oid) // first fill wins the latency sample
					}
				}
				if mine {
					st.myOrdersFilled.Add(1)
				} else {
					st.foreignFills.Add(1)
				}
			case wire.EventTypeOrderCancel:
				st.cancels.Add(1)
				var t flatbuffers.Table
				if ev.Type(&t) {
					oc := &wire.OrderCancel{}
					oc.Init(t.Bytes, t.Pos)
					if sentNs, _, ok := corr.lookup(oc.OrderId()); ok {
						st.cancelLat.observe(time.Now().UnixNano() - int64(sentNs))
						corr.drop(oc.OrderId())
					}
				}
			case wire.EventTypeBookSnapshot:
				st.snapshots.Add(1)
			case wire.EventTypeTimeTick:
				st.timeTicks.Add(1)
			default:
				st.otherEvents.Add(1)
			}
		}
	}()

	// ---- monitor goroutine ------------------------------------------------------
	// Refreshes occupancy/drops gauges and watches the far-end producer pid.
	// "Engine dead" is only raised after the producer has been seen alive once
	// — a loadgen started before its engine attaches to empty (producer-less)
	// rings must not abort instantly.
	monitorStop := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		everAlive := false
		dead := false
		for {
			select {
			case <-monitorStop:
				return
			case <-t.C:
			}
			st.outOccupancy.Store(ch.Occupancy())
			st.ringDrops.Store(ch.Drops())
			alive := ch.ProducerAlive()
			st.producerAlive.Store(alive)
			if alive {
				everAlive = true
				if dead {
					dead = false
					fmt.Fprintf(os.Stderr, "engine producer alive again; resuming\n")
				}
				continue
			}
			if !everAlive || dead {
				continue
			}
			dead = true
			select {
			case engineDead <- struct{}{}:
			default:
			}
			if !*reconnect {
				requestStop("engine_dead")
			}
		}
	}()

	// Engine-death watcher for -reconnect mode: pause sending while dead.
	if *reconnect {
		go func() {
			for range engineDead {
				fmt.Fprintf(os.Stderr, "engine producer dead; pausing sends\n")
			}
		}()
	}

	// ---- send loop --------------------------------------------------------------
	rnd := rand.New(rand.NewSource(*seed))
	b := flatbuffers.NewBuilder(512)
	p := newPacer(*rate, time.Now())
	pid := os.Getpid()
	mid := int64(midStartTicks)

	windowStart := time.Now()
	var windowSent uint64

	var seq uint64
	// ClientOrderID scratch: fixed prefix once, seq appended per order.
	cidPrefix := []byte("soak-" + strconv.Itoa(pid) + "-")
	sending := true
	for sending {
		select {
		case <-stopSend:
			sending = false
			continue
		default:
		}
		// Pause while the engine is dead in -reconnect mode.
		if *reconnect && !st.producerAlive.Load() {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		target := p.next()

		// 1s window accounting — recorded unconditionally so a stalled
		// second counts as rate 0 rather than disappearing from the mean.
		now := time.Now()
		if now.Sub(windowStart) >= time.Second {
			st.recordWindow(windowSent)
			windowSent = 0
			windowStart = now
		}

		// ---- order synthesis ----
		// Random walk the mid in pipette steps, clamped +-1% of start.
		mid += int64(rnd.Intn(5)-2) * tickSizeTicks
		if mid > midStartTicks+midMaxWanderTicks {
			mid = midStartTicks + midMaxWanderTicks
		} else if mid < midStartTicks-midMaxWanderTicks {
			mid = midStartTicks - midMaxWanderTicks
		}

		seq++
		orderID := *orderIDBase + seq - 1
		accountID := 1 + (seq-1)%*accounts

		cross := rnd.Float64()*100 < *crossPct
		var side wire.Side
		var price int64
		var tif wire.TimeInForce
		var qty int64
		if rnd.Intn(2) == 0 {
			side = wire.SideBuy
		} else {
			side = wire.SideSell
		}
		if cross {
			// Price through the band so a resting opposite-side order fills:
			// buys pay up to mid+25 pips, sells hit down to mid-25 pips. IOC
			// keeps the remainder from resting after a partial sweep.
			reach := int64(1+rnd.Intn(crossReachPips)) * pipSizeTicks
			if side == wire.SideBuy {
				price = mid + reach
			} else {
				price = mid - reach
			}
			tif = wire.TimeInForceIOC
			qty = lotQtyUnits // 1 lot — thin taker, thick book refill
		} else {
			off := int64(1+rnd.Intn(passiveBandPips)) * pipSizeTicks
			if side == wire.SideBuy {
				price = mid - off
			} else {
				price = mid + off
			}
			if rnd.Intn(100) == 0 { // occasional IOC in the passive flow too
				tif = wire.TimeInForceIOC
			} else {
				tif = wire.TimeInForceGTC
			}
			qty = int64(1+rnd.Intn(100)) * lotQtyUnits
		}

		b.Reset()
		sendUnix := uint64(time.Now().UnixNano())
		// strconv scratch beats fmt.Sprintf per order — at 50k+/s the
		// Sprintf reflection+alloc path was ~1us of the ~20us send budget.
		cid := strconv.AppendUint(cidPrefix, seq, 10)
		msg := ipc.EncodeOrderNewEvent(b, seq, sendUnix,
			ipc.OrderNewMsg{
				OrderID:       orderID,
				AccountID:     accountID,
				InstrumentID:  uint32(*instrument),
				Side:          side,
				Type:          wire.OrderTypeLimit,
				Qty:           qty,
				Price:         price,
				TIF:           tif,
				ClientOrderID: string(cid),
			})

		// Bounded retry on ring-full backpressure (~1ms), then drop: a soak
		// generator sheds rather than unbounded-block; drops are reported.
		ok := false
		for tries := 0; tries < 64; tries++ {
			if ch.Send(msg) {
				ok = true
				break
			}
			runtime.Gosched()
		}
		if !ok {
			st.sendDrops.Add(1)
			continue
		}
		st.ordersSent.Add(1)
		windowSent++
		corr.store(orderID, sendUnix, cross)

		// If scheduling fell behind (GC pause, descheduled on a loaded host),
		// rebase so the next deadline is in the future rather than bursting.
		// 1s was too lax: a ~400ms producer stall dumped ~20k catch-up orders
		// flat-out, manufacturing a ~220ms queue tail that read as engine
		// latency in the Phase-02.5 probe. 10ms bounds a catch-up burst to
		// ~500 orders (~6ms of added drain at ~90k/s); stalls still surface
		// honestly via min_1s, orders_sent and achieved_rate.
		if time.Since(target) > 10*time.Millisecond {
			p.rebase(time.Now())
		}
	}
	st.recordWindow(windowSent)

	// ---- drain quiet period -----------------------------------------------------
	// The drain goroutine exits 500ms after the last observed event once
	// stop is requested; bound the wait so a continuously-publishing engine
	// can't hang shutdown.
	select {
	case <-drainDone:
	case <-time.After(8 * time.Second):
	}
	close(monitorStop)
	<-monitorDone
	_ = srv.Shutdown(context.Background())

	// ---- report ------------------------------------------------------------------
	ended := time.Now()
	rep := soakReport{
		Started:           started.UTC().Format(time.RFC3339Nano),
		Ended:             ended.UTC().Format(time.RFC3339Nano),
		DurationS:         ended.Sub(started).Seconds(),
		OrdersSent:        st.ordersSent.Load(),
		Fills:             st.fills.Load(),
		Cancels:           st.cancels.Load(),
		Snapshots:         st.snapshots.Load(),
		TimeTicks:         st.timeTicks.Load(),
		OtherEvents:       st.otherEvents.Load(),
		DecodeErrors:      st.decodeErrors.Load(),
		DuplicateTradeIDs: st.dupTradeIDs.Load(),
		MyOrdersFilled:    st.myOrdersFilled.Load(),
		MyDistinctOrders:  myOrderIDs.seen,
		ForeignFills:      st.foreignFills.Load(),
		SendDrops:         st.sendDrops.Load(),
		RingDrops:         ch.Drops(),
		LatencyNS:         fillLatencyReport(&st.fillLat),
		CancelLatencyNS:   fillLatencyReport(&st.cancelLat),
		TargetRate:        *rate,
		Shard:             uint16(*shard),
		InstrumentID:      uint32(*instrument),
	}
	rep.Throughput.Min1s = st.windowMin.Load()
	rep.Throughput.Max1s = st.windowMax.Load()
	if wc := st.windowCount.Load(); wc > 0 {
		rep.Throughput.Mean1s = float64(st.windowSum.Load()) / float64(wc)
	}
	if sr, ok := stopReason.Load().(string); ok {
		rep.StopReason = sr
	}
	rep.EngineDied = !st.producerAlive.Load() && rep.StopReason == "engine_dead"

	// monitor.sh interop aliases (fill latency, µs).
	rep.DupTradeIDsFlat = rep.DuplicateTradeIDs
	// achieved_rate = sustained send rate over the whole run — the honest
	// number monitor.sh compares against the 45k/s floor.
	if rep.DurationS > 0 {
		rep.AchievedRate = float64(rep.OrdersSent) / rep.DurationS
	} else {
		rep.AchievedRate = rep.Throughput.Mean1s
	}
	rep.EngineLatencyNS = fillLatencyReport(&st.engineLat)
	rep.P50US = float64(rep.LatencyNS.P50) / 1e3
	rep.P99US = float64(rep.LatencyNS.P99) / 1e3
	rep.P999US = float64(rep.LatencyNS.P999) / 1e3
	rep.EngineP50US = float64(rep.EngineLatencyNS.P50) / 1e3
	rep.EngineP99US = float64(rep.EngineLatencyNS.P99) / 1e3
	rep.EngineP999US = float64(rep.EngineLatencyNS.P999) / 1e3

	out, err := json.MarshalIndent(&rep, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal report: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*reportPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write report %s: %v\n", *reportPath, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "stopped (%s): sent=%d fills=%d cancels=%d dup_trade_ids=%d report=%s\n",
		rep.StopReason, rep.OrdersSent, rep.Fills, rep.Cancels,
		rep.DuplicateTradeIDs, *reportPath)
}
