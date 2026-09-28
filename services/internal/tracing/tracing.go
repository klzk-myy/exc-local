// Package tracing implements Task 9.3.11 — distributed trace_id
// continuity HTTP → Aeron → C++ core → Aeron → Go — with an
// OTLP-compatible model and exporter seam, without the OpenTelemetry SDK
// (the module graph carries no OTel dependency; spec §19.12 requires an
// OTLP wire format to Jaeger/Tempo, not a specific client library).
//
// Wire continuity is W3C `traceparent` end-to-end:
//
//	HTTP edge    middleware.Tracing parses/continues the inbound
//	             traceparent and mints this hop's span id.
//	HTTP server  tracing.Middleware opens a SERVER span from that hop.
//	Aeron        InjectTraceParent writes the same traceparent into the
//	             Aeron application-header region the C++ core echoes
//	             back (see aeron.go for the byte contract).
//	C++ core     reads it as correlation metadata; outbound frames carry
//	             it back to the Go consumers unchanged.
//	Go consumer  ExtractTraceParent reconstructs the remote parent for
//	             the matching/settlement/market-data spans.
//
// Sampling (spec §19.12.3 "head 1% + tail on errors/slow"):
// inbound parent flags are honored; root spans head-sample at the
// configured ratio (deterministic on trace-id so every hop of a sampled
// trace samples); at Finish a span upgrades to sampled when it recorded
// an error or exceeded the slow threshold — tail sampling done in-
// process, which is legal for a single-writer span.
//
// Export: Exporter is the seam. OTLPHTTP posts OTLP/HTTP JSON to a
// collector (deploy/otel/otel-collector.yaml) which fans out to Jaeger
// or Tempo; FileExporter appends the same JSON lines for
// node-local scraping; BatchExporter buffers either behind a bounded
// queue that drops (counted) under backpressure — tracing must never
// backpressure the hot path (spec §2.7).
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// TraceParentHeader is the W3C header carried end-to-end.
const TraceParentHeader = "traceparent"

// SpanContext is the propagated identity of one span.
type SpanContext struct {
	TraceID string // 32-hex
	SpanID  string // 16-hex
	// ParentSpanID is the remote caller's span id — set on spans
	// continued from an inbound traceparent or Aeron frame.
	ParentSpanID string
	// Sampled is the propagation bit plus the local head-sampling
	// decision; tail upgrade happens at Finish (Tracer decides).
	Sampled bool
	// Remote marks a context reconstructed from wire metadata rather
	// than created by this process.
	Remote bool
}

// IsValid reports whether the context carries usable ids.
func (sc SpanContext) IsValid() bool {
	return len(sc.TraceID) == 32 && len(sc.SpanID) == 16 &&
		sc.TraceID != strings.Repeat("0", 32) &&
		sc.SpanID != strings.Repeat("0", 16)
}

// TraceParent renders the W3C header value for this context — the same
// string every hop injects onto the wire.
func (sc SpanContext) TraceParent() string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + sc.TraceID + "-" + sc.SpanID + "-" + flags
}

// ParseTraceParent validates `00-{32hex}-{16hex}-{2hex}` and returns the
// remote SpanContext — SpanID holds the REMOTE span's id (it becomes the
// child's ParentSpanID in Tracer.Start; Remote=true marks provenance).
// Malformed input returns ok=false — tracing never rejects traffic.
func ParseTraceParent(h string) (SpanContext, bool) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 {
		return SpanContext{}, false
	}
	if len(parts[0]) != 2 || len(parts[1]) != 32 ||
		len(parts[2]) != 16 || len(parts[3]) != 2 {
		return SpanContext{}, false
	}
	for _, p := range parts[1:] {
		for _, r := range p {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return SpanContext{}, false
			}
		}
	}
	sc := SpanContext{
		TraceID: parts[1],
		SpanID:  parts[2],
		Sampled: parts[3] == "01",
		Remote:  true,
	}
	if !sc.IsValid() {
		return SpanContext{}, false
	}
	return sc, true
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return make([]byte, n) // all-zero ids fail IsValid — fail visibly
	}
	return b
}

// NewTraceID mints a fresh 32-hex trace id.
func NewTraceID() string { return hex.EncodeToString(randBytes(16)) }

// NewSpanID mints a fresh 16-hex span id.
func NewSpanID() string { return hex.EncodeToString(randBytes(8)) }

// traceIDFraction folds the trace id into [0,1) for deterministic head
// sampling — every hop hashing the same trace id reaches the same
// decision, which is what makes a sampled trace contiguous.
func traceIDFraction(traceID string) float64 {
	if len(traceID) != 32 {
		return 1.0
	}
	b, err := hex.DecodeString(traceID[16:])
	if err != nil || len(b) != 8 {
		return 1.0
	}
	return float64(binary.BigEndian.Uint64(b)) / float64(1<<64)
}

type ctxKeySpan struct{}

// ContextWithSpan stores the active span's context.
func ContextWithSpanContext(ctx context.Context, sc SpanContext) context.Context {
	return context.WithValue(ctx, ctxKeySpan{}, sc)
}

// SpanContextFrom returns the active span context, ok=false when absent.
func SpanContextFrom(ctx context.Context) (SpanContext, bool) {
	sc, ok := ctx.Value(ctxKeySpan{}).(SpanContext)
	return sc, ok && sc.IsValid()
}
