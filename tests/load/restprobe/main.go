// restprobe — Phase-08 Task 8.3.2 REST latency prober.
//
// Hammers one HTTP endpoint at a target rate (open-loop paced) or with
// -workers closed-loop concurrency, recording a 1µs-resolution latency
// histogram, status-code counts and transport errors. The p99 must beat
// 5ms per the Phase-08 §8.7 #5 AC when pointed at a gateway route.
//
//	restprobe -url http://127.0.0.1:8080/api/v1/instruments \
//	    -duration 60s -rate 200 -report restprobe.json
//
//	restprobe -url ... -workers 8            # closed-loop mode
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	latMicroBuckets = 50_000 // 1µs buckets up to 50ms
	latOverflowIdx  = latMicroBuckets
	latArrLen       = latMicroBuckets + 1
)

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

func (h *latHist) percentile(q float64) uint64 {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	want := uint64(float64(total)*q + 0.999)
	var acc uint64
	for i := 0; i < latArrLen; i++ {
		acc += atomic.LoadUint64(&h.buckets[i])
		if acc >= want {
			if i >= latOverflowIdx {
				return h.max.Load()
			}
			return uint64(i)*1_000 + 999
		}
	}
	return h.max.Load()
}

type report struct {
	Started      string            `json:"started"`
	Ended        string            `json:"ended"`
	DurationS    float64           `json:"duration_s"`
	URL          string            `json:"url"`
	Requests     uint64            `json:"requests"`
	Errors       uint64            `json:"errors"`
	StatusCounts map[string]uint64 `json:"status_counts"`
	AchievedRPS  float64           `json:"achieved_rps"`
	LatencyUS    struct {
		P50  float64 `json:"p50"`
		P99  float64 `json:"p99"`
		P999 float64 `json:"p999"`
		Max  float64 `json:"max"`
		Mean float64 `json:"mean"`
	} `json:"latency_us"`
	P99Under5ms bool `json:"p99_under_5ms"`
}

func main() {
	var (
		url         = flag.String("url", "", "endpoint to probe (GET)")
		duration    = flag.Duration("duration", 60*time.Second, "run length")
		rate        = flag.Float64("rate", 200, "open-loop requests/sec")
		workers     = flag.Int("workers", 0, "closed-loop concurrency (0 = open-loop paced)")
		reportPath  = flag.String("report", "restprobe.json", "JSON report path")
		bearer      = flag.String("bearer", "", "optional Authorization bearer token")
		header      = flag.String("header", "", "optional 'K: V' headers (repeat via ;)")
		maxRespBody = flag.Int64("max-body", 1<<20, "response body read cap")
	)
	flag.Parse()
	if *url == "" {
		fmt.Fprintln(os.Stderr, "restprobe: -url is required")
		os.Exit(2)
	}

	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns:          4096,
		MaxIdleConnsPerHost:   4096,
		IdleConnTimeout:       60 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // localhost harness
		ResponseHeaderTimeout: 10 * time.Second,
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	var extra http.Header
	if *header != "" {
		extra = http.Header{}
		for _, kv := range strings.Split(*header, ";") {
			k, v, ok := strings.Cut(kv, ":")
			if ok {
				extra.Add(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
	}

	started := time.Now()
	deadline := started.Add(*duration)
	h := &latHist{}
	var reqs, errs atomic.Uint64
	var statusMu sync.Mutex
	counts := map[int]uint64{}

	one := func() {
		r, err := http.NewRequest(http.MethodGet, *url, nil)
		if err != nil {
			errs.Add(1)
			return
		}
		if *bearer != "" {
			r.Header.Set("Authorization", "Bearer "+*bearer)
		}
		for k, vs := range extra {
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		}
		t0 := time.Now()
		resp, err := client.Do(r)
		if err != nil {
			errs.Add(1)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, *maxRespBody))
		_ = resp.Body.Close()
		h.observe(time.Since(t0).Nanoseconds())
		reqs.Add(1)
		statusMu.Lock()
		counts[resp.StatusCode]++
		statusMu.Unlock()
	}

	if *workers > 0 {
		var wg sync.WaitGroup
		for i := 0; i < *workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(deadline) {
					one()
				}
			}()
		}
		wg.Wait()
	} else {
		interval := time.Duration(float64(time.Second) / *rate)
		next := time.Now()
		sem := make(chan struct{}, 512) // bound in-flight
		var wg sync.WaitGroup
		for time.Now().Before(deadline) {
			next = next.Add(interval)
			for {
				rem := time.Until(next)
				if rem <= 0 {
					break
				}
				time.Sleep(min(rem, time.Millisecond))
			}
			select {
			case sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					one()
				}()
			default: // in-flight saturated; skip rather than burst
			}
		}
		wg.Wait()
	}

	rep := report{
		Started:      started.UTC().Format(time.RFC3339Nano),
		Ended:        time.Now().UTC().Format(time.RFC3339Nano),
		DurationS:    time.Since(started).Seconds(),
		URL:          *url,
		Requests:     reqs.Load(),
		Errors:       errs.Load(),
		StatusCounts: map[string]uint64{},
	}
	if rep.DurationS > 0 {
		rep.AchievedRPS = float64(rep.Requests) / rep.DurationS
	}
	statusMu.Lock()
	keys := make([]int, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		rep.StatusCounts[fmt.Sprintf("%d", k)] = counts[k]
	}
	statusMu.Unlock()
	rep.LatencyUS.P50 = float64(h.percentile(0.50)) / 1e3
	rep.LatencyUS.P99 = float64(h.percentile(0.99)) / 1e3
	rep.LatencyUS.P999 = float64(h.percentile(0.999)) / 1e3
	rep.LatencyUS.Max = float64(h.max.Load()) / 1e3
	if h.count.Load() > 0 {
		rep.LatencyUS.Mean = float64(h.sum.Load()) / float64(h.count.Load()) / 1e3
	}
	rep.P99Under5ms = rep.LatencyUS.P99 > 0 && rep.LatencyUS.P99 <= 5000

	out, _ := json.MarshalIndent(&rep, "", "  ")
	if err := os.WriteFile(*reportPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr,
		"restprobe done: reqs=%d errs=%d p50=%.1fus p99=%.1fus p999=%.1fus rps=%.0f\n",
		rep.Requests, rep.Errors, rep.LatencyUS.P50, rep.LatencyUS.P99,
		rep.LatencyUS.P999, rep.AchievedRPS)
}
