// registry.go — minimal Prometheus metric registry + text exposition
// (format v0.0.4). Task 7.3.4, spec §19.3.
//
// prometheus/client_golang is intentionally not vendored (go.mod carries
// no metrics dependency); this registry implements exactly the exposition
// grammar a standard scrape config consumes:
//
//	# HELP <name> <doc>
//	# TYPE <name> <counter|gauge|histogram>
//	<name>{label="v",...} <value>
//	<name>_bucket{le="<upper>"} <cumcount>   (histogram)
//	<name>_sum <v> / <name>_count <n>        (histogram)
//
// Rendering order is deterministic (families sorted by name, samples by
// label tuple) so scrapes and tests diff cleanly.
package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sampleKey joins a label tuple with a byte that cannot appear in label
// names or values.
type sampleKey string

func keyOf(labels ...string) sampleKey {
	return sampleKey(strings.Join(labels, "\x00"))
}

func (k sampleKey) pairs() []string {
	if k == "" {
		return nil
	}
	return strings.Split(string(k), "\x00")
}

type metricFamily struct {
	name string
	help string
	typ  string // counter | gauge | histogram
}

// atomf is an atomically-updated float64 cell.
type atomf = atomic.Uint64

func fload(c *atomf) float64     { return math.Float64frombits(c.Load()) }
func fstore(c *atomf, v float64) { c.Store(math.Float64bits(v)) }
func fadd(c *atomf, delta float64) {
	for {
		cur := c.Load()
		next := math.Float64bits(math.Float64frombits(cur) + delta)
		if c.CompareAndSwap(cur, next) {
			return
		}
	}
}

// vec stores one atomic cell per label tuple (counter/gauge families).
type vec struct {
	f       metricFamily
	mu      sync.RWMutex
	samples map[sampleKey]*atomf
}

func newVec(name, help, typ string) *vec {
	return &vec{f: metricFamily{name: name, help: help, typ: typ},
		samples: map[sampleKey]*atomf{}}
}

func (v *vec) cell(labels ...string) *atomf {
	if len(labels)%2 != 0 {
		panic("observability: odd label count for " + v.f.name)
	}
	k := keyOf(labels...)
	v.mu.RLock()
	c, ok := v.samples[k]
	v.mu.RUnlock()
	if ok {
		return c
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok = v.samples[k]; ok {
		return c
	}
	c = &atomf{}
	v.samples[k] = c
	return c
}

// CounterVec is a labelled counter family.
type CounterVec struct{ v *vec }

// With returns the counter cell for the label tuple (created on first use).
func (c *CounterVec) With(labels ...string) *Counter {
	return &Counter{c: c.v.cell(labels...)}
}

// Counter is a single monotonically-increasing counter cell.
type Counter struct{ c *atomf }

// Inc adds 1.
func (c *Counter) Inc() { c.Add(1) }

// Add adds a non-negative delta (negative deltas are ignored — counters
// never decrease).
func (c *Counter) Add(delta float64) {
	if delta < 0 {
		return
	}
	fadd(c.c, delta)
}

// GaugeVec is a labelled gauge family.
type GaugeVec struct{ v *vec }

// With returns the gauge cell for the label tuple.
func (g *GaugeVec) With(labels ...string) *Gauge {
	return &Gauge{c: g.v.cell(labels...)}
}

// Gauge is a single settable gauge cell.
type Gauge struct{ c *atomf }

// Set stores the current value.
func (g *Gauge) Set(v float64) { fstore(g.c, v) }

// Add shifts the gauge by delta (negative allowed).
func (g *Gauge) Add(delta float64) { fadd(g.c, delta) }

// HistogramVec is a labelled classic histogram family. Stored counts are
// per-bucket (non-cumulative); the exposition renders the cumulative
// _bucket{le} series plus _sum/_count that Prometheus expects.
type HistogramVec struct {
	f       metricFamily
	buckets []float64 // sorted ascending, +Inf implied
	mu      sync.RWMutex
	cells   map[sampleKey]*histogram
}

type histogram struct {
	counts []atomic.Uint64 // len(buckets)+1; last cell = +Inf overflow
	sum    atomf
	count  atomic.Uint64
}

func newHistogramVec(name, help string, buckets []float64) *HistogramVec {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	dedup := b[:0]
	for i, v := range b {
		if i == 0 || v != b[i-1] {
			dedup = append(dedup, v)
		}
	}
	return &HistogramVec{
		f:       metricFamily{name: name, help: help, typ: "histogram"},
		buckets: dedup,
		cells:   map[sampleKey]*histogram{},
	}
}

// With returns the histogram cell for the label tuple.
func (h *HistogramVec) With(labels ...string) *Histogram {
	k := keyOf(labels...)
	h.mu.RLock()
	c, ok := h.cells[k]
	h.mu.RUnlock()
	if ok {
		return &Histogram{h: c, buckets: h.buckets}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok = h.cells[k]; ok {
		return &Histogram{h: c, buckets: h.buckets}
	}
	c = &histogram{counts: make([]atomic.Uint64, len(h.buckets)+1)}
	h.cells[k] = c
	return &Histogram{h: c, buckets: h.buckets}
}

// Histogram observes float64 values into the fixed bucket set.
type Histogram struct {
	h       *histogram
	buckets []float64
}

// Observe records one observation (negative and NaN values land in the
// first bucket, matching client_golang semantics of counting, not policing).
func (h *Histogram) Observe(v float64) {
	idx := sort.Search(len(h.buckets), func(i int) bool { return v <= h.buckets[i] })
	h.h.counts[idx].Add(1)
	h.h.count.Add(1)
	fadd(&h.h.sum, v)
}

// pullFunc is a pull-based metric for values owned elsewhere (ring
// depths, connection counts, counters maintained as atomics in another
// package) — sampled at render time. typ is "gauge" or "counter".
type pullFunc struct {
	f     metricFamily
	lbls  []string
	fn    func() float64
	vecFn func() []PullSample // nil for single-sample funcs
}

// Registry holds every metric family a service exposes on /metrics.
// Zero value is not usable — build with New().
type Registry struct {
	mu       sync.RWMutex
	counters []*CounterVec
	gauges   []*GaugeVec
	funcs    []*pullFunc
	hists    []*HistogramVec
}

// New returns an empty registry.
func New() *Registry { return &Registry{} }

// Counter registers a counter family.
func (r *Registry) Counter(name, help string) *CounterVec {
	cv := &CounterVec{v: newVec(name, help, "counter")}
	r.mu.Lock()
	r.counters = append(r.counters, cv)
	r.mu.Unlock()
	return cv
}

// Gauge registers a gauge family.
func (r *Registry) Gauge(name, help string) *GaugeVec {
	gv := &GaugeVec{v: newVec(name, help, "gauge")}
	r.mu.Lock()
	r.gauges = append(r.gauges, gv)
	r.mu.Unlock()
	return gv
}

// GaugeFunc registers a pull-based gauge sampled at exposition time.
func (r *Registry) GaugeFunc(name, help string, fn func() float64, labels ...string) {
	if fn == nil {
		panic("observability: nil gauge func for " + name)
	}
	r.mu.Lock()
	r.funcs = append(r.funcs, &pullFunc{
		f:    metricFamily{name: name, help: help, typ: "gauge"},
		lbls: labels, fn: fn,
	})
	r.mu.Unlock()
}

// CounterFunc registers a pull-based counter sampled at exposition time —
// for cumulative counters maintained as atomics in another package
// (e.g. internal/marketdata's Metrics struct, whose fields must not be
// re-implemented here).
func (r *Registry) CounterFunc(name, help string, fn func() float64, labels ...string) {
	if fn == nil {
		panic("observability: nil counter func for " + name)
	}
	r.mu.Lock()
	r.funcs = append(r.funcs, &pullFunc{
		f:    metricFamily{name: name, help: help, typ: "counter"},
		lbls: labels, fn: fn,
	})
	r.mu.Unlock()
}

// PullSample is one dynamically-labelled sample produced by a VecFunc.
type PullSample struct {
	Labels []string // flat k,v pairs
	Value  float64
}

// VecFunc registers a pull-based family whose label set is dynamic —
// fn returns the full sample set at exposition time (e.g. per-tier
// counters out of a sync.Map).
func (r *Registry) VecFunc(name, help, typ string, fn func() []PullSample) {
	if fn == nil {
		panic("observability: nil vec func for " + name)
	}
	r.mu.Lock()
	r.funcs = append(r.funcs, &pullFunc{
		f:     metricFamily{name: name, help: help, typ: typ},
		fn:    func() float64 { return 0 },
		vecFn: fn,
	})
	r.mu.Unlock()
}

// Histogram registers a histogram family with the given bucket bounds
// (seconds or the caller's documented unit).
func (r *Registry) Histogram(name, help string, buckets []float64) *HistogramVec {
	hv := newHistogramVec(name, help, buckets)
	r.mu.Lock()
	r.hists = append(r.hists, hv)
	r.mu.Unlock()
	return hv
}

// ---------------------------------------------------------------------------
// exposition rendering
// ---------------------------------------------------------------------------

func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func writeLabels(w io.Writer, key sampleKey, extra ...string) {
	pairs := key.pairs()
	if len(extra) > 0 {
		pairs = append(pairs, extra...)
	}
	if len(pairs) == 0 {
		return
	}
	io.WriteString(w, "{")
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			io.WriteString(w, ",")
		}
		fmt.Fprintf(w, `%s="%s"`, pairs[i], escapeLabelValue(pairs[i+1]))
	}
	io.WriteString(w, "}")
}

func writeSample(w io.Writer, name string, key sampleKey, v float64, extra ...string) {
	io.WriteString(w, name)
	writeLabels(w, key, extra...)
	fmt.Fprintf(w, " %s\n", strconv.FormatFloat(v, 'g', -1, 64))
}

func writeHeader(w io.Writer, f metricFamily) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
}

// renderVecs renders one or more same-named vecs under a single family
// header; later vecs overwrite duplicate label tuples (last-write-wins,
// matching Prometheus client registration semantics).
func renderVecs(w io.Writer, vs []*vec) {
	vals := map[sampleKey]float64{}
	for _, v := range vs {
		v.mu.RLock()
		for k, c := range v.samples {
			vals[k] = fload(c)
		}
		v.mu.RUnlock()
	}
	keys := make([]sampleKey, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	writeHeader(w, vs[0].f)
	for _, k := range keys {
		writeSample(w, vs[0].f.name, k, vals[k])
	}
}

// writeText renders the full exposition body.
func (r *Registry) writeText(w io.Writer) {
	r.mu.RLock()
	counters := append([]*CounterVec(nil), r.counters...)
	gauges := append([]*GaugeVec(nil), r.gauges...)
	funcs := append([]*pullFunc(nil), r.funcs...)
	hists := append([]*HistogramVec(nil), r.hists...)
	r.mu.RUnlock()

	families := append([]metricFamily(nil),
		func() []metricFamily {
			var fs []metricFamily
			for _, c := range counters {
				fs = append(fs, c.v.f)
			}
			for _, g := range gauges {
				fs = append(fs, g.v.f)
			}
			for _, g := range funcs {
				fs = append(fs, g.f)
			}
			for _, h := range hists {
				fs = append(fs, h.f)
			}
			return fs
		}()...)
	sort.Slice(families, func(i, j int) bool { return families[i].name < families[j].name })

	// index families by name for ordered render; duplicate registrations
	// of one name merge under a single header (re-registering is legal —
	// e.g. subsystems adding samples to a shared family).
	vecsByName := map[string][]*vec{}
	for _, c := range counters {
		vecsByName[c.v.f.name] = append(vecsByName[c.v.f.name], c.v)
	}
	for _, g := range gauges {
		vecsByName[g.v.f.name] = append(vecsByName[g.v.f.name], g.v)
	}
	funcByName := map[string][]*pullFunc{}
	for _, g := range funcs {
		funcByName[g.f.name] = append(funcByName[g.f.name], g)
	}
	histsByName := map[string][]*HistogramVec{}
	for _, h := range hists {
		histsByName[h.f.name] = append(histsByName[h.f.name], h)
	}

	// dedupe the sorted family list by name
	seen := map[string]bool{}
	ordered := families[:0]
	for _, f := range families {
		if !seen[f.name] {
			seen[f.name] = true
			ordered = append(ordered, f)
		}
	}

	writePull := func(gs []*pullFunc) {
		for _, g := range gs {
			if g.vecFn != nil {
				ss := g.vecFn()
				sort.Slice(ss, func(i, j int) bool {
					return strings.Join(ss[i].Labels, "\x00") <
						strings.Join(ss[j].Labels, "\x00")
				})
				for _, s := range ss {
					writeSample(w, g.f.name, keyOf(s.Labels...), s.Value)
				}
				continue
			}
			writeSample(w, g.f.name, keyOf(g.lbls...), g.fn())
		}
	}

	for _, f := range ordered {
		if vs, ok := vecsByName[f.name]; ok {
			renderVecs(w, vs)
			// pull-funcs sharing the name append their samples under the
			// same header rather than emitting a duplicate family.
			writePull(funcByName[f.name])
			continue
		}
		if gs, ok := funcByName[f.name]; ok {
			writeHeader(w, f)
			writePull(gs)
			continue
		}
		if hs, ok := histsByName[f.name]; ok {
			renderHistograms(w, f, hs)
		}
	}
}

func renderHistograms(w io.Writer, f metricFamily, hvs []*HistogramVec) {
	type cellView struct {
		key     sampleKey
		buckets []float64
		counts  []uint64
		sum     float64
		count   uint64
	}
	var views []cellView
	for _, hv := range hvs {
		hv.mu.RLock()
		for k, c := range hv.cells {
			v := cellView{key: k, buckets: hv.buckets,
				counts: make([]uint64, len(c.counts)),
				sum:    fload(&c.sum), count: c.count.Load()}
			for i := range c.counts {
				v.counts[i] = c.counts[i].Load()
			}
			views = append(views, v)
		}
		hv.mu.RUnlock()
	}
	sort.Slice(views, func(i, j int) bool { return views[i].key < views[j].key })

	writeHeader(w, f)
	for _, v := range views {
		var cum uint64
		for i, b := range v.buckets {
			cum += v.counts[i]
			writeSample(w, f.name+"_bucket", v.key, float64(cum),
				"le", strconv.FormatFloat(b, 'g', -1, 64))
		}
		cum += v.counts[len(v.counts)-1]
		writeSample(w, f.name+"_bucket", v.key, float64(cum), "le", "+Inf")
		writeSample(w, f.name+"_sum", v.key, v.sum)
		writeSample(w, f.name+"_count", v.key, float64(v.count))
	}
}

// String renders the exposition body — used by unit tests without an HTTP
// round-trip.
func (r *Registry) String() string {
	var sb strings.Builder
	r.writeText(&sb)
	return sb.String()
}

// ServeMetrics runs a minimal /metrics + /healthz HTTP listener on addr
// until ctx cancels — the Task 7.3.4 surface for scaffold services whose
// real listeners land in later phases (fix/settlement/compliance bind
// their reserved config ports for observability only). Returns nil on
// clean shutdown.
func ServeMetrics(ctx context.Context, addr string, r *Registry, service string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", r.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","service":%q}`, service)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		sdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sdCtx)
		return nil
	}
}

// Handler serves GET /metrics in Prometheus text exposition format
// (content type per exposition spec; scrape interval is the collector's —
// Task 7.3.4 fixes it at 15s in deploy/prometheus/prometheus.yml).
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/metrics" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.writeText(w)
	})
}
