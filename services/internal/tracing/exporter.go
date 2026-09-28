// exporter.go — the Exporter seam plus OTLP/HTTP JSON and JSONL file
// implementations (Task 9.3.11 "export to Jaeger/Tempo").
//
// OTLP/HTTP JSON layout (opentelemetry-proto ExportTraceServiceRequest):
//
//	{"resourceSpans":[{ "resource":{"attributes":[…service.name…]},
//	  "scopeSpans":[{"scope":{"name":"exchange"},
//	    "spans":[{"traceId":"…","spanId":"…","parentSpanId":"…",
//	      "name":"…","kind":2,"startTimeUnixNano":"…","endTimeUnixNano":"…",
//	      "attributes":[{"key":"k","value":{"stringValue":"v"}}],
//	      "events":[{"timeUnixNano":"…","name":"…","attributes":[…]}],
//	      "status":{"code":2,"message":"…"}}]}]}]}
//
// A collector (deploy/otel/otel-collector.yaml) terminates this on
// :4318/v1/traces and fans out to Jaeger (:14250 OTLP gRPC) or Tempo —
// services speak plain OTLP/HTTP, no agent SDK required.
package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Exporter is the span sink seam.
type Exporter interface {
	// ExportSpans ships completed spans. Implementations must not block
	// the caller meaningfully — the hot path cannot wait on a collector.
	ExportSpans(ctx context.Context, spans []SpanData) error
	// Shutdown drains and closes.
	Shutdown(ctx context.Context) error
}

// NopExporter discards spans (default when no backend is configured).
type NopExporter struct{}

func (NopExporter) ExportSpans(context.Context, []SpanData) error { return nil }
func (NopExporter) Shutdown(context.Context) error                { return nil }

// ---------------------------------------------------------------------------
// OTLP/HTTP JSON encoding
// ---------------------------------------------------------------------------

func otlpValue(v any) map[string]any {
	switch t := v.(type) {
	case string:
		return map[string]any{"stringValue": t}
	case int64:
		return map[string]any{"intValue": strconv.FormatInt(t, 10)}
	case int:
		return map[string]any{"intValue": strconv.Itoa(t)}
	case float64:
		return map[string]any{"doubleValue": t}
	case bool:
		return map[string]any{"boolValue": t}
	default:
		return map[string]any{"stringValue": fmt.Sprint(t)}
	}
}

func otlpAttrs(attrs []Attr) []map[string]any {
	out := make([]map[string]any, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, map[string]any{"key": a.Key, "value": otlpValue(a.Value)})
	}
	return out
}

// EncodeOTLPPayload renders spans as an OTLP/HTTP JSON
// ExportTraceServiceRequest grouped by service name (one resource per
// service — spans in a batch may span services).
func EncodeOTLPPayload(spans []SpanData) []byte {
	byService := map[string][]SpanData{}
	order := []string{}
	for _, s := range spans {
		svc := s.ServiceName
		if svc == "" {
			svc = "unknown-service"
		}
		if _, seen := byService[svc]; !seen {
			order = append(order, svc)
		}
		byService[svc] = append(byService[svc], s)
	}
	resourceSpans := make([]map[string]any, 0, len(order))
	for _, svc := range order {
		ss := byService[svc]
		spansJSON := make([]map[string]any, 0, len(ss))
		for _, s := range ss {
			spansJSON = append(spansJSON, encodeSpan(s))
		}
		resourceSpans = append(resourceSpans, map[string]any{
			"resource": map[string]any{
				"attributes": otlpAttrs([]Attr{
					Str("service.name", svc),
					Str("telemetry.sdk.name", "exchange-lite-otlp"),
					Str("telemetry.sdk.language", "go"),
				}),
			},
			"scopeSpans": []map[string]any{{
				"scope": map[string]any{"name": "exchange.tracing"},
				"spans": spansJSON,
			}},
		})
	}
	payload := map[string]any{"resourceSpans": resourceSpans}
	b, _ := json.Marshal(payload)
	return b
}

func encodeSpan(s SpanData) map[string]any {
	ev := make([]map[string]any, 0, len(s.Events))
	for _, e := range s.Events {
		ev = append(ev, map[string]any{
			"timeUnixNano": strconv.FormatInt(e.At.UnixNano(), 10),
			"name":         e.Name,
			"attributes":   otlpAttrs(e.Attrs),
		})
	}
	out := map[string]any{
		"traceId":           s.TraceID,
		"spanId":            s.SpanID,
		"name":              s.Name,
		"kind":              int32(s.Kind),
		"startTimeUnixNano": strconv.FormatInt(s.Start.UnixNano(), 10),
		"endTimeUnixNano":   strconv.FormatInt(s.End.UnixNano(), 10),
		"attributes":        otlpAttrs(s.Attrs),
		"events":            ev,
	}
	if s.ParentSpanID != "" {
		out["parentSpanId"] = s.ParentSpanID
	}
	out["status"] = map[string]any{
		"code":    int32(s.Status),
		"message": s.StatusMessage,
	}
	return out
}

// ---------------------------------------------------------------------------
// OTLPHTTP — POST to a collector endpoint (default :4318/v1/traces).
// ---------------------------------------------------------------------------

// OTLPHTTPExporter ships spans to an OTLP/HTTP collector.
type OTLPHTTPExporter struct {
	Endpoint string            // e.g. http://otel-collector:4318/v1/traces
	Headers  map[string]string // optional auth/tenant headers
	Client   *http.Client      // nil → 2s-timeout default
}

// ExportSpans POSTs the JSON payload. Errors are returned (the batching
// layer decides retry/drop); ExportSpans itself is synchronous and
// short-timeout.
func (e *OTLPHTTPExporter) ExportSpans(ctx context.Context, spans []SpanData) error {
	if len(spans) == 0 {
		return nil
	}
	endpoint := e.Endpoint
	if endpoint == "" {
		endpoint = "http://127.0.0.1:4318/v1/traces"
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint, bytes.NewReader(EncodeOTLPPayload(spans)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("otlp export: collector status %d", resp.StatusCode)
	}
	return nil
}

// Shutdown is a no-op for the stateless HTTP exporter.
func (e *OTLPHTTPExporter) Shutdown(context.Context) error { return nil }

// ---------------------------------------------------------------------------
// FileExporter — append OTLP JSON lines (one batch per line). The
// node-local collector tails the file (deploy/otel/), or a dev just
// cats it. Same payload as OTLPHTTPExporter.
// ---------------------------------------------------------------------------

type FileExporter struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// NewFileExporter opens (creating/truncating=false: append) path.
func NewFileExporter(path string) (*FileExporter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	return &FileExporter{f: f, path: path}, nil
}

// ExportSpans appends one OTLP JSON line per call.
func (e *FileExporter) ExportSpans(_ context.Context, spans []SpanData) error {
	if len(spans) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.f.Write(append(EncodeOTLPPayload(spans), '\n'))
	return err
}

// Shutdown flushes and closes the file.
func (e *FileExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.f.Close()
}

// ---------------------------------------------------------------------------
// BatchExporter — bounded async queue in front of a synchronous exporter.
// Full queue drops newest with a counted drop; tracing must never
// backpressure the hot path (spec §2.7).
// ---------------------------------------------------------------------------

// BatchExporter batches spans behind a queue and flushes on a timer.
type BatchExporter struct {
	inner      Exporter
	queue      chan SpanData
	batchMax   int
	flushEvery time.Duration
	done       chan struct{}
	stopped    chan struct{}
	closeOnce  sync.Once
	mu         sync.Mutex
	dropped    int64
	exported   int64
	lastErr    error
}

// NewBatchExporter wraps inner with a queue of queueSize and a flush
// interval. batchMax bounds the per-flush payload.
func NewBatchExporter(inner Exporter, queueSize int, flushEvery time.Duration, batchMax int) *BatchExporter {
	if queueSize <= 0 {
		queueSize = 2048
	}
	if flushEvery <= 0 {
		flushEvery = 500 * time.Millisecond
	}
	if batchMax <= 0 {
		batchMax = 256
	}
	b := &BatchExporter{
		inner: inner, queue: make(chan SpanData, queueSize),
		batchMax: batchMax, flushEvery: flushEvery,
		done: make(chan struct{}), stopped: make(chan struct{}),
	}
	go b.loop()
	return b
}

// ExportSpans enqueues — drops (counted) when full.
func (b *BatchExporter) ExportSpans(_ context.Context, spans []SpanData) error {
	for _, s := range spans {
		select {
		case b.queue <- s:
		default:
			b.mu.Lock()
			b.dropped++
			b.mu.Unlock()
		}
	}
	return nil
}

// Dropped reports spans lost to backpressure — wire to the
// observability registry as `tracing_spans_dropped`.
func (b *BatchExporter) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Exported reports spans successfully flushed.
func (b *BatchExporter) Exported() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exported
}

func (b *BatchExporter) loop() {
	tick := time.NewTicker(b.flushEvery)
	defer func() { tick.Stop(); close(b.stopped) }()
	for {
		select {
		case <-tick.C:
			b.flush(false)
		case <-b.done:
			b.flush(true)
			return
		}
	}
}

func (b *BatchExporter) flush(drainAll bool) {
	batch := make([]SpanData, 0, b.batchMax)
	for {
		select {
		case s := <-b.queue:
			batch = append(batch, s)
			if len(batch) >= b.batchMax && !drainAll {
				b.send(batch)
				batch = batch[:0]
			}
		default:
			if len(batch) > 0 {
				b.send(batch)
			}
			return
		}
	}
}

func (b *BatchExporter) send(batch []SpanData) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := b.inner.ExportSpans(ctx, batch)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.dropped += int64(len(batch))
		b.lastErr = err
		return
	}
	b.exported += int64(len(batch))
}

// LastError reports the most recent inner-exporter failure (observability
// label seam).
func (b *BatchExporter) LastError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr
}

// Shutdown stops the loop, waits for the final drain (bounded by ctx),
// then shuts down inner.
func (b *BatchExporter) Shutdown(ctx context.Context) error {
	var err error
	b.closeOnce.Do(func() {
		close(b.done)
		select {
		case <-b.stopped:
		case <-ctx.Done():
			err = ctx.Err()
			return
		}
		err = b.inner.Shutdown(ctx)
	})
	return err
}
