// Coverage for Task 9.3.11: traceparent parse/emit round-trip, head+tail
// sampling, span lifecycle (attrs/events/status), Aeron frame
// correlation, OTLP JSON shape, batch export and drop-counting, HTTP
// middleware bridging middleware.TraceContext.
package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"exchange/internal/middleware"
)

// --- traceparent --------------------------------------------------------

func TestParseTraceParent(t *testing.T) {
	sc, ok := ParseTraceParent(
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok || sc.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" ||
		sc.SpanID != "00f067aa0ba902b7" || !sc.Sampled || !sc.Remote {
		t.Fatalf("valid traceparent mis-parsed: %+v ok=%v", sc, ok)
	}
	for _, bad := range []string{
		"", "garbage",
		"00-xyz-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", // uppercase
	} {
		if _, ok := ParseTraceParent(bad); ok {
			t.Fatalf("invalid traceparent %q accepted", bad)
		}
	}
	// Round-trip emit.
	if got := sc.TraceParent(); got !=
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatalf("TraceParent round-trip: %q", got)
	}
	// Unsampled flag renders 00.
	sc.Sampled = false
	if !strings.HasSuffix(sc.TraceParent(), "-00") {
		t.Fatal("unsampled context must emit flags 00")
	}
}

// --- span lifecycle -----------------------------------------------------

type memExporter struct {
	mu    sync.Mutex
	spans []SpanData
	err   error
}

func (m *memExporter) ExportSpans(_ context.Context, ss []SpanData) error {
	if m.err != nil {
		return m.err
	}
	m.mu.Lock()
	m.spans = append(m.spans, ss...)
	m.mu.Unlock()
	return nil
}
func (m *memExporter) Shutdown(context.Context) error { return nil }
func (m *memExporter) all() []SpanData {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SpanData(nil), m.spans...)
}

func TestSpanLifecycle(t *testing.T) {
	exp := &memExporter{}
	tr := NewTracer("test-svc", exp, &Options{SampleAll: true})
	ctx, s := tr.Start(context.Background(), SpanOrderSubmit, KindClient,
		Str("symbol", "EURUSD"), Int("shard", 3))
	s.AddEvent("aeron.offer", Str("stream", "orders"))
	s.SetAttr(Str("order_id", "42"))
	s.SetStatus(StatusOK, "")
	s.Finish()
	s.Finish() // idempotent

	got := exp.all()
	if len(got) != 1 {
		t.Fatalf("expected 1 span, got %d", len(got))
	}
	d := got[0]
	if d.Name != SpanOrderSubmit || d.Kind != KindClient ||
		d.Status != StatusOK || d.ServiceName != "test-svc" {
		t.Fatalf("bad span data: %+v", d)
	}
	if len(d.Events) != 1 || d.Events[0].Name != "aeron.offer" {
		t.Fatalf("events not captured: %+v", d.Events)
	}
	if len(d.Attrs) < 3 {
		t.Fatalf("attrs not captured: %+v", d.Attrs)
	}
	if d.End.Before(d.Start) {
		t.Fatal("end before start")
	}
	// ctx now carries the span context.
	if sc, ok := SpanContextFrom(ctx); !ok || sc.TraceID != d.TraceID {
		t.Fatal("span context not stored")
	}
}

func TestParentChildContinuity(t *testing.T) {
	exp := &memExporter{}
	tr := NewTracer("svc", exp, &Options{SampleAll: true})
	ctx, parent := tr.Start(context.Background(), "p", KindServer)
	_, child := tr.Start(ctx, "c", KindInternal)
	child.Finish()
	parent.Finish()
	got := exp.all()
	if len(got) != 2 || got[0].TraceID != got[1].TraceID ||
		got[0].ParentSpanID != got[1].SpanID {
		t.Fatalf("child must share trace id and parent to parent span: %+v", got)
	}
}

// --- sampling -----------------------------------------------------------

func TestHeadSamplingDeterministic(t *testing.T) {
	exp := &memExporter{}
	tr := NewTracer("svc", exp, &Options{RatioHead: 0.5, SampleAll: false})
	sampled, unsampled := 0, 0
	for i := 0; i < 400; i++ {
		_, s := tr.Start(context.Background(), "op", KindInternal)
		if s.ctx.Sampled {
			sampled++
		} else {
			unsampled++
		}
	}
	if sampled == 0 || unsampled == 0 {
		t.Fatalf("head sampling at 0.5 must split traffic, got %d/%d",
			sampled, unsampled)
	}
	if sampled < 120 || sampled > 280 {
		t.Fatalf("head sampling far from 50%%: %d/400", sampled)
	}
	// Determinism: same trace id → same decision everywhere.
	f := traceIDFraction("4bf92f3577b34da6a3ce929d0e0e4736")
	tr2 := NewTracer("svc", NopExporter{}, &Options{RatioHead: 0.5})
	sc := SpanContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "aa"}
	if tr2.headSample(false, false, sc.TraceID) != (f < 0.5) {
		t.Fatal("head sampling must be deterministic on trace id")
	}
}

func TestParentFlagWins(t *testing.T) {
	tr := NewTracer("svc", NopExporter{}, &Options{RatioHead: 0.0})
	// Parent sampled → child sampled regardless of ratio.
	ctx := ContextWithSpanContext(context.Background(), SpanContext{
		TraceID: NewTraceID(), SpanID: NewSpanID(), Sampled: true})
	_, s := tr.Start(ctx, "op", KindInternal)
	if !s.ctx.Sampled {
		t.Fatal("sampled parent must yield sampled child")
	}
	// Parent unsampled → child unsampled.
	ctx = ContextWithSpanContext(context.Background(), SpanContext{
		TraceID: NewTraceID(), SpanID: NewSpanID(), Sampled: false})
	_, s = tr.Start(ctx, "op", KindInternal)
	if s.ctx.Sampled {
		t.Fatal("unsampled parent must yield unsampled child")
	}
}

func TestTailSampling(t *testing.T) {
	exp := &memExporter{}
	// RatioHead 0 → nothing head-samples; only tail wins.
	tr := NewTracer("svc", exp, &Options{
		RatioHead:     0.0,
		SlowThreshold: 10 * time.Millisecond,
		Clock:         time.Now,
	})
	// Unsampled fast span → dropped.
	_, s := tr.Start(context.Background(), "fast", KindInternal)
	s.Finish()
	if n := len(exp.all()); n != 0 {
		t.Fatalf("fast unsampled span must not export, got %d", n)
	}
	// Error span → tail-sampled.
	_, s = tr.Start(context.Background(), "boom", KindInternal)
	s.RecordError(errors.New("matching rejected"))
	s.Finish()
	if n := len(exp.all()); n != 1 || exp.all()[0].Status != StatusError {
		t.Fatalf("errored span must tail-sample: %+v", exp.all())
	}
	// Slow span → tail-sampled (fixed clock).
	t0 := time.Now()
	var now time.Time
	tr2 := NewTracer("svc", exp, &Options{
		RatioHead: 0.0, SlowThreshold: 5 * time.Millisecond,
		Clock: func() time.Time {
			if now.IsZero() {
				return t0
			}
			return now
		},
	})
	_, s = tr2.Start(context.Background(), "slow", KindInternal)
	now = t0.Add(20 * time.Millisecond) // finish reads now() again
	s.Finish()
	if n := len(exp.all()); n != 2 {
		t.Fatalf("slow span must tail-sample, exported=%d", n)
	}
}

// --- Aeron correlation ---------------------------------------------------

func TestAeronTraceRoundTrip(t *testing.T) {
	tr := NewTracer("svc", NopExporter{}, &Options{SampleAll: true})
	ctx, s := tr.Start(context.Background(), SpanOrderSubmit, KindClient)
	frame := make([]byte, 256)
	InjectAeronTrace(ctx, frame)

	sc, ok := ExtractAeronTrace(frame)
	if !ok || sc.TraceID != s.ctx.TraceID ||
		sc.SpanID != s.ctx.SpanID || !sc.Remote {
		t.Fatalf("aeron round-trip broken: %+v ok=%v", sc, ok)
	}
	// Consumer continues the trace: child of the extracted remote.
	cctx, ok := ContextFromAeron(context.Background(), frame)
	if !ok {
		t.Fatal("ContextFromAeron must store the remote context")
	}
	_, match := tr.Start(cctx, SpanOrderMatch, KindConsumer)
	if match.ctx.TraceID != s.ctx.TraceID ||
		match.ctx.ParentSpanID != s.ctx.SpanID {
		t.Fatalf("consumer span must continue the aeron trace: %+v",
			match.ctx)
	}
	// Untraced frame → ok=false, never a panic.
	if _, ok := ExtractAeronTrace(make([]byte, 256)); ok {
		t.Fatal("zeroed frame must not decode a trace")
	}
	if _, ok := ExtractAeronTrace([]byte("short")); ok {
		t.Fatal("short frame must not decode a trace")
	}
}

// --- OTLP JSON shape ------------------------------------------------------

func TestOTLPEncodingShape(t *testing.T) {
	d := SpanData{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:  "00f067aa0ba902b7", ParentSpanID: "00aabbccddee0011",
		Name: "order.match", Kind: KindConsumer,
		Start:  time.Unix(1700000000, 123),
		End:    time.Unix(1700000000, 223),
		Attrs:  []Attr{Str("symbol", "EURUSD"), Int("shard", 7), Bool("maker", true)},
		Events: []Event{{Name: "match", At: time.Unix(1700000000, 150), Attrs: []Attr{Int("qty", 1000)}}},
		Status: StatusError, StatusMessage: "risk reject",
		ServiceName: "matching-core",
	}
	payload := EncodeOTLPPayload([]SpanData{d})
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("OTLP payload not JSON: %v", err)
	}
	rs := doc["resourceSpans"].([]any)
	if len(rs) != 1 {
		t.Fatalf("one resource expected: %v", doc)
	}
	res := rs[0].(map[string]any)["resource"].(map[string]any)
	attrs := res["attributes"].([]any)
	foundSvc := false
	for _, a := range attrs {
		kv := a.(map[string]any)
		if kv["key"] == "service.name" &&
			kv["value"].(map[string]any)["stringValue"] == "matching-core" {
			foundSvc = true
		}
	}
	if !foundSvc {
		t.Fatal("service.name attribute missing")
	}
	spans := rs[0].(map[string]any)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)
	if len(spans) != 1 {
		t.Fatalf("one span expected: %v", spans)
	}
	sp := spans[0].(map[string]any)
	if sp["traceId"] != d.TraceID || sp["spanId"] != d.SpanID ||
		sp["parentSpanId"] != d.ParentSpanID ||
		sp["name"] != "order.match" || sp["kind"].(float64) != 5 {
		t.Fatalf("span fields wrong: %+v", sp)
	}
	if sp["status"].(map[string]any)["code"].(float64) != 2 {
		t.Fatalf("error status must encode code=2: %+v", sp["status"])
	}
	if sp["endTimeUnixNano"] != "1700000000000000223" {
		t.Fatalf("nanos must be decimal-string encoded: %v",
			sp["endTimeUnixNano"])
	}
}

// --- HTTP middleware -------------------------------------------------------

func TestHTTPMiddlewareContinuity(t *testing.T) {
	exp := &memExporter{}
	tr := NewTracer("gw", exp, &Options{SampleAll: true})
	inbound := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	var saw SpanContext
	h := middleware.Tracing(Middleware(tr)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			sc, _ := SpanContextFrom(r.Context())
			saw = sc
			w.WriteHeader(http.StatusTeapot) // 418 → status OK-ish, not error
		})))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil)
	req.Header.Set(TraceParentHeader, inbound)
	h.ServeHTTP(rec, req)

	if rec.Header().Get("X-Trace-ID") != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatal("middleware.Tracing must continue the inbound trace id")
	}
	if saw.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("server span must continue the inbound trace: %+v", saw)
	}
	got := exp.all()
	if len(got) != 1 || got[0].Kind != KindServer ||
		got[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("server span not exported: %+v", got)
	}
	if got[0].Status == StatusError {
		t.Fatal("418 is not a 5xx — span must not tail-sample as error")
	}
}

func TestHTTPMiddlewareErrorTail(t *testing.T) {
	exp := &memExporter{}
	tr := NewTracer("gw", exp, &Options{RatioHead: 0, SampleAll: false})
	h := Middleware(tr)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	got := exp.all()
	if len(got) != 1 || got[0].Status != StatusError {
		t.Fatalf("5xx must tail-sample with error status: %+v", got)
	}
}

// --- batch exporter -------------------------------------------------------

func TestBatchExporterFlushAndDrop(t *testing.T) {
	exp := &memExporter{}
	b := NewBatchExporter(exp, 4, 10*time.Millisecond, 8)
	defer b.Shutdown(context.Background())
	for i := 0; i < 3; i++ {
		_ = b.ExportSpans(context.Background(),
			[]SpanData{{TraceID: NewTraceID(), SpanID: NewSpanID(),
				Name: "s", Start: time.Now(), End: time.Now()}})
	}
	deadline := time.Now().Add(2 * time.Second)
	for b.Exported() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.Exported() != 3 {
		t.Fatalf("expected 3 exported, got %d", b.Exported())
	}
	// Fill the queue with a stuck inner exporter to observe drops.
	stuck := &blockingExporter{release: make(chan struct{})}
	b2 := NewBatchExporter(stuck, 2, time.Hour, 1)
	for i := 0; i < 10; i++ {
		_ = b2.ExportSpans(context.Background(), []SpanData{{}})
	}
	close(stuck.release)
	_ = b2.Shutdown(context.Background())
	if b2.Dropped() == 0 {
		t.Fatal("backpressure must count drops")
	}
}

type blockingExporter struct {
	release chan struct{}
}

func (e *blockingExporter) ExportSpans(ctx context.Context, _ []SpanData) error {
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *blockingExporter) Shutdown(context.Context) error { return nil }

// --- HTTP exporter (httptest) ----------------------------------------------

func TestOTLPHTTPExporterPosts(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, _ = io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "application/json" {
				w.WriteHeader(400)
			}
			w.WriteHeader(200)
		}))
	defer srv.Close()
	e := &OTLPHTTPExporter{Endpoint: srv.URL, Client: srv.Client()}
	err := e.ExportSpans(context.Background(), []SpanData{{
		TraceID: NewTraceID(), SpanID: NewSpanID(), Name: "x",
		Start: time.Now(), End: time.Now(), ServiceName: "svc",
	}})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		t.Fatal("collector received non-JSON")
	}
	if _, ok := doc["resourceSpans"]; !ok {
		t.Fatal("payload missing resourceSpans")
	}
}
