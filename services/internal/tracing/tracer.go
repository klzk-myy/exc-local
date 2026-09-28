// tracer.go — span creation and the head+tail sampling policy
// (spec §19.12.3: "head 1% + tail on errors/slow").
//
// Head: the inbound parent's sampled flag wins (continuity); root spans
// deterministically sample RatioHead of trace ids via traceIDFraction —
// every hop of a sampled trace id samples, so a head-sampled trace is
// contiguous across HTTP → Aeron → C++ → Aeron → Go.
//
// Tail: at Finish, an unsampled span that recorded an error or ran
// longer than SlowThreshold upgrades to sampled — errors and slow
// operations always reach the backend. Tail-in-process is sound here
// because each span has exactly one writer (the process that created it);
// downstream spans still see the parent's propagation bit and head-sample
// consistently.
package tracing

import (
	"context"
	"time"
)

// Options configure a Tracer.
type Options struct {
	// RatioHead is the root-span head-sampling fraction [0,1].
	// Default 0.01 (spec §19.12.3 head 1%).
	RatioHead float64
	// SlowThreshold is the tail-sampling duration bound — finished spans
	// slower than this export regardless of head sampling.
	// Default 5ms (the §19 order-path latency budget boundary).
	SlowThreshold time.Duration
	// Sampled disables sampling entirely when true (dev/staging).
	SampleAll bool
	// Clock overrides time.Now (tests).
	Clock func() time.Time
}

// Tracer mints spans and routes finished spans to the exporter.
type Tracer struct {
	service string
	exp     Exporter
	opts    Options
	now     func() time.Time
	// dropped counts spans discarded by the exporter queue — exported as
	// a metric by the caller (observability registry seam stays in the
	// wiring layer, not here).
}

// NewTracer wires a tracer for service onto exporter exp (nil → Nop).
func NewTracer(service string, exp Exporter, opts *Options) *Tracer {
	o := Options{RatioHead: 0.01, SlowThreshold: 5 * time.Millisecond, Clock: time.Now}
	if opts != nil {
		if opts.RatioHead != 0 {
			o.RatioHead = opts.RatioHead
		}
		if opts.SlowThreshold != 0 {
			o.SlowThreshold = opts.SlowThreshold
		}
		o.SampleAll = opts.SampleAll
		if opts.Clock != nil {
			o.Clock = opts.Clock
		}
	}
	if exp == nil {
		exp = NopExporter{}
	}
	return &Tracer{service: service, exp: exp, opts: o, now: o.Clock}
}

// Adopt begins a span whose identity was already minted upstream — e.g.
// the HTTP server span adopts the hop id middleware.Tracing minted so the
// X-Trace-ID response, the exported span, and the traceparent injected
// into Aeron are the same identity. sc.Sampled carries the inbound flag
// and is honored verbatim (continuity beats local policy).
func (t *Tracer) Adopt(ctx context.Context, sc SpanContext, name string, kind Kind, attrs ...Attr) (context.Context, *Span) {
	if !sc.IsValid() {
		return t.Start(ctx, name, kind, attrs...)
	}
	s := &Span{
		ctx:    sc,
		tracer: t,
		name:   name,
		kind:   kind,
		start:  t.now(),
		clock:  t.now,
		attrs:  attrs,
	}
	return ContextWithSpanContext(ctx, sc), s
}

// Start begins a span. The parent context comes from an active span in
// ctx, else from a SpanContext previously stored in ctx (e.g. by
// ContextFromAeron or an HTTP middleware that parsed traceparent). With
// no parent the span starts a new trace.
func (t *Tracer) Start(ctx context.Context, name string, kind Kind, attrs ...Attr) (context.Context, *Span) {
	sc := SpanContext{SpanID: NewSpanID()}
	var parentSampled bool
	hasParent := false
	if p, ok := SpanContextFrom(ctx); ok {
		sc.TraceID = p.TraceID
		sc.ParentSpanID = p.SpanID
		parentSampled = p.Sampled
		hasParent = true
	} else {
		sc.TraceID = NewTraceID()
	}
	sc.Sampled = t.headSample(hasParent, parentSampled, sc.TraceID)

	s := &Span{
		ctx:    sc,
		tracer: t,
		name:   name,
		kind:   kind,
		start:  t.now(),
		clock:  t.now,
		attrs:  attrs,
	}
	return ContextWithSpanContext(ctx, sc), s
}

// headSample decides the propagation/head bit. Parent wins — a trace
// continues sampled or unsampled as the edge decided — so the decision
// is stable across process hops.
func (t *Tracer) headSample(hasParent, parentSampled bool, traceID string) bool {
	if t.opts.SampleAll {
		return true
	}
	if hasParent {
		return parentSampled
	}
	return traceIDFraction(traceID) < t.opts.RatioHead
}

// finish applies tail sampling then exports the snapshot.
func (t *Tracer) finish(s *Span) {
	end := t.now()
	d := s.snapshot(end) // captures status/attrs under lock
	sampled := s.ctx.Sampled ||
		d.Status == StatusError ||
		d.End.Sub(d.Start) >= t.opts.SlowThreshold
	if !sampled {
		return
	}
	// Export is fire-and-forget: BatchExporter drops (counted) under
	// backpressure rather than blocking the hot path.
	_ = t.exp.ExportSpans(context.Background(), []SpanData{d})
}

// StartNamed is the span vocabulary for the order lifecycle
// (Task 9.3.11 AC "spans for order submission, matching, settlement,
// market data") — fixed names keep the collector backend queryable.
const (
	SpanOrderSubmit = "order.submit"    // gateway → Aeron offer
	SpanOrderMatch  = "order.match"     // C++ matching span (remote parent)
	SpanSettlement  = "settlement.t+1"  // settlement service leg
	SpanMarketData  = "marketdata.tick" // MD publish hop
)
