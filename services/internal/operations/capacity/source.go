// Metrics source seam. The generator talks to MetricsSource; the
// production implementation is PrometheusSource hitting the HTTP
// query_range/query APIs, tests substitute scripted series.
package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Point is one (timestamp, value) sample.
type Point struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// MetricsSource supplies historical and instant metrics. Range returns
// the expression's samples collapsed to one series — when a query
// resolves to multiple Prometheus series the implementation sums values
// sharing a timestamp (spec expressions already aggregate via sum/max).
type MetricsSource interface {
	Range(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Point, error)
	Instant(ctx context.Context, expr string, at time.Time) (float64, error)
}

// PrometheusSource implements MetricsSource over the Prometheus HTTP
// API (api/v1/query_range + api/v1/query).
type PrometheusSource struct {
	Base   string // e.g. "http://prometheus:9090"
	Client *http.Client
}

// NewPrometheusSource wires the source; nil client → a 10s-default http.Client.
func NewPrometheusSource(base string, c *http.Client) *PrometheusSource {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &PrometheusSource{Base: base, Client: c}
}

type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Values [][]any `json:"values"` // matrix: [[ts, "v"], ...]
			Value  []any   `json:"value"`  // vector: [ts, "v"]
		} `json:"result"`
	} `json:"data"`
}

func (p *PrometheusSource) get(ctx context.Context, path string, q url.Values) (*promResponse, error) {
	u := p.Base + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(body), 200))
	}
	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("prometheus decode: %w", err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: %s", pr.Error)
	}
	return &pr, nil
}

func promPair(pair []any) (time.Time, float64, bool) {
	if len(pair) != 2 {
		return time.Time{}, 0, false
	}
	ts, ok := pair[0].(float64)
	if !ok {
		return time.Time{}, 0, false
	}
	s, ok := pair[1].(string)
	if !ok {
		return time.Time{}, 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	return time.Unix(int64(ts), int64((ts-float64(int64(ts)))*1e9)).UTC(), v, true
}

// Range queries /api/v1/query_range and collapses matrix series by
// summing same-timestamp values.
func (p *PrometheusSource) Range(ctx context.Context, expr string,
	start, end time.Time, step time.Duration) ([]Point, error) {
	q := url.Values{
		"query": {expr},
		"start": {strconv.FormatFloat(float64(start.Unix()), 'f', 0, 64)},
		"end":   {strconv.FormatFloat(float64(end.Unix()), 'f', 0, 64)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', 0, 64)},
	}
	pr, err := p.get(ctx, "/api/v1/query_range", q)
	if err != nil {
		return nil, err
	}
	acc := map[int64]float64{}
	for _, series := range pr.Data.Result {
		for _, pair := range series.Values {
			ts, v, ok := promPair(pair)
			if !ok {
				continue
			}
			acc[ts.Unix()] += v
		}
	}
	keys := make([]int64, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		out = append(out, Point{At: time.Unix(k, 0).UTC(), Value: acc[k]})
	}
	return out, nil
}

// Instant queries /api/v1/query and sums the vector result.
func (p *PrometheusSource) Instant(ctx context.Context, expr string, at time.Time) (float64, error) {
	q := url.Values{
		"query": {expr},
		"time":  {strconv.FormatFloat(float64(at.Unix()), 'f', 0, 64)},
	}
	pr, err := p.get(ctx, "/api/v1/query", q)
	if err != nil {
		return 0, err
	}
	var sum float64
	var found bool
	for _, r := range pr.Data.Result {
		if _, v, ok := promPair(r.Value); ok {
			sum += v
			found = true
		}
	}
	if !found {
		return 0, fmt.Errorf("prometheus: empty instant result for %q", truncate(expr, 60))
	}
	return sum, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
