// registry_test.go — exposition-format round-trip tests: render with the
// registry, parse with an independent minimal parser, assert semantics.
package observability

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// parsedSample is one exposition line broken into name/labels/value.
type parsedSample struct {
	name   string
	labels map[string]string
	value  float64
}

// parseExposition is the test-side parser — intentionally independent of
// the writer so round-trip failures are real.
func parseExposition(t *testing.T, body string) (
	types map[string]string, samples []parsedSample,
) {
	t.Helper()
	types = map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			parts := strings.SplitN(line, " ", 4)
			if strings.HasPrefix(line, "# TYPE ") {
				types[parts[2]] = parts[3]
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		s := parsedSample{labels: map[string]string{}}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			t.Fatalf("bad sample line %q", line)
		}
		v, err := strconv.ParseFloat(line[sp+1:], 64)
		if err != nil {
			t.Fatalf("bad sample value %q: %v", line, err)
		}
		s.value = v
		head := line[:sp]
		if i := strings.IndexByte(head, '{'); i >= 0 {
			s.name = head[:i]
			lbls := head[i+1:]
			if !strings.HasSuffix(lbls, "}") {
				t.Fatalf("bad labels %q", head)
			}
			lbls = lbls[:len(lbls)-1]
			for _, p := range splitLabelPairs(lbls) {
				kv := strings.SplitN(p, "=", 2)
				if len(kv) != 2 {
					t.Fatalf("bad label pair %q", p)
				}
				s.labels[kv[0]] = strings.Trim(kv[1], `"`)
			}
		} else {
			s.name = head
		}
		samples = append(samples, s)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return types, samples
}

// splitLabelPairs splits on commas that are NOT inside a quoted value.
func splitLabelPairs(s string) []string {
	var out []string
	depth := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			depth = !depth
		case '\\':
			i++ // skip escaped char
		case ',':
			if !depth {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func findSample(samples []parsedSample, name string,
	match map[string]string) (parsedSample, bool) {
	for _, s := range samples {
		if s.name != name {
			continue
		}
		ok := true
		for k, v := range match {
			if s.labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return s, true
		}
	}
	return parsedSample{}, false
}

func TestRegistryCounterGaugeRoundTrip(t *testing.T) {
	reg := New()
	c := reg.Counter("test_events_total", "Test counter.")
	c.With("kind", "a").Inc()
	c.With("kind", "a").Add(2)
	c.With("kind", "b").Inc()
	g := reg.Gauge("test_depth", "Test gauge.")
	g.With("shard", "0").Set(42)
	g.With("shard", "0").Add(-2)

	types, samples := parseExposition(t, reg.String())
	if types["test_events_total"] != "counter" {
		t.Fatalf("counter type = %q", types["test_events_total"])
	}
	if types["test_depth"] != "gauge" {
		t.Fatalf("gauge type = %q", types["test_depth"])
	}
	if s, ok := findSample(samples, "test_events_total", map[string]string{"kind": "a"}); !ok || s.value != 3 {
		t.Fatalf("counter a = %v (found=%v)", s.value, ok)
	}
	if s, ok := findSample(samples, "test_depth", map[string]string{"shard": "0"}); !ok || s.value != 40 {
		t.Fatalf("gauge = %v (found=%v)", s.value, ok)
	}
}

func TestRegistryHistogramExposition(t *testing.T) {
	reg := New()
	h := reg.Histogram("test_latency_seconds", "Latency.", []float64{0.1, 0.5, 1.0})
	h.With("route", "/x").Observe(0.05)
	h.With("route", "/x").Observe(0.4)
	h.With("route", "/x").Observe(0.9)
	h.With("route", "/x").Observe(2.0) // +Inf overflow

	types, samples := parseExposition(t, reg.String())
	if types["test_latency_seconds"] != "histogram" {
		t.Fatalf("histogram type = %q", types["test_latency_seconds"])
	}
	want := map[string]float64{"0.1": 1, "0.5": 2, "1": 3, "+Inf": 4}
	for le, v := range want {
		s, ok := findSample(samples, "test_latency_seconds_bucket",
			map[string]string{"route": "/x", "le": le})
		if !ok || s.value != v {
			t.Fatalf("bucket le=%s = %v want %v (found=%v)", le, s.value, v, ok)
		}
	}
	s, ok := findSample(samples, "test_latency_seconds_sum", map[string]string{"route": "/x"})
	if !ok || s.value < 3.34 || s.value > 3.36 {
		t.Fatalf("sum = %v", s.value)
	}
	s, ok = findSample(samples, "test_latency_seconds_count", map[string]string{"route": "/x"})
	if !ok || s.value != 4 {
		t.Fatalf("count = %v", s.value)
	}
}

func TestRegistryLabelEscaping(t *testing.T) {
	reg := New()
	c := reg.Counter("esc_total", "Escape test.")
	c.With("label", `a"b\nc`).Inc()
	types, samples := parseExposition(t, reg.String())
	_ = types
	// The parser is deliberately dumb about unescaping — match the raw
	// escaped form the exposition must contain.
	s, ok := findSample(samples, "esc_total", map[string]string{"label": `a\"b\\nc`})
	if !ok {
		t.Fatalf("escaped label sample missing: %s", reg.String())
	}
	if s.value != 1 {
		t.Fatalf("value = %v", s.value)
	}
	// The raw output must contain the escaped forms.
	out := reg.String()
	if !strings.Contains(out, `"a\"b\\nc"`) {
		t.Fatalf("exposition lacks escaped label: %q", out)
	}
}

func TestRegistryHandler(t *testing.T) {
	reg := New()
	reg.Counter("x_total", "X.").With().Add(7)
	srv := httptest.NewServer(reg.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content-type %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "x_total 7") {
		t.Fatalf("body missing counter: %s", body)
	}

	resp2, err := http.Get(srv.URL + "/other")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("/other status = %d", resp2.StatusCode)
	}
}

func TestRegistryDeterministicOrder(t *testing.T) {
	reg := New()
	reg.Counter("b_total", "B.").With().Inc()
	reg.Counter("a_total", "A.").With().Inc()
	reg.Gauge("a_gauge", "G.").With("z", "2").Set(2)
	reg.Gauge("a_gauge", "G.").With("z", "1").Set(1)
	first := reg.String()
	for i := 0; i < 20; i++ {
		if reg.String() != first {
			t.Fatalf("nondeterministic render at iter %d", i)
		}
	}
	// sorted families: a_gauge, a_total, b_total
	if !(strings.Index(first, "# TYPE a_gauge") < strings.Index(first, "# TYPE a_total") &&
		strings.Index(first, "# TYPE a_total") < strings.Index(first, "# TYPE b_total")) {
		t.Fatalf("families not sorted: %s", first)
	}
	// sorted label tuples inside a_gauge: z=1 before z=2
	if strings.Index(first, `z="1"`) > strings.Index(first, `z="2"`) {
		t.Fatalf("samples not sorted: %s", first)
	}
}

func TestGaugeFunc(t *testing.T) {
	reg := New()
	var v float64
	reg.GaugeFunc("pull_gauge", "Pulled.", func() float64 { return v })
	v = 12.5
	_, samples := parseExposition(t, reg.String())
	s, ok := findSample(samples, "pull_gauge", nil)
	if !ok || s.value != 12.5 {
		t.Fatalf("gauge func = %v (found=%v)", s.value, ok)
	}
}

func TestCounterFuncAndVecFunc(t *testing.T) {
	reg := New()
	var n int64 = 41
	reg.CounterFunc("pull_total", "Pulled counter.", func() float64 {
		return float64(n)
	})
	reg.VecFunc("pull_per_tier", "Dynamic-label counter.", "counter",
		func() []PullSample {
			return []PullSample{
				{Labels: []string{"tier", "basic"}, Value: 3},
				{Labels: []string{"tier", "public"}, Value: 7},
			}
		})
	types, samples := parseExposition(t, reg.String())
	if types["pull_total"] != "counter" || types["pull_per_tier"] != "counter" {
		t.Fatalf("types = %v", types)
	}
	if s, ok := findSample(samples, "pull_total", nil); !ok || s.value != 41 {
		t.Fatalf("pull_total = %v", s.value)
	}
	if s, ok := findSample(samples, "pull_per_tier",
		map[string]string{"tier": "public"}); !ok || s.value != 7 {
		t.Fatalf("pull_per_tier public = %v", s.value)
	}
	// Deterministic order across renders despite map-iteration in fn.
	if reg.String() != reg.String() {
		t.Fatal("nondeterministic vec-func render")
	}
}

func ExampleRegistry() {
	reg := New()
	reg.Counter("orders_total", "Orders submitted.").With("shard", "0").Add(3)
	fmt.Print(reg.String())
	// Output:
	// # HELP orders_total Orders submitted.
	// # TYPE orders_total counter
	// orders_total{shard="0"} 3
}
