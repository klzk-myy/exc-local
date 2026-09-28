// http.go — HTTP server-span middleware (Task 9.3.11).
//
// Stacks after middleware.Tracing: that hop already parsed/continued the
// inbound traceparent and minted this hop's span id (TraceContext);
// Middleware adopts it as the SERVER span's context — the trace id on
// X-Trace-ID, the span the exporter ships, and the traceparent we inject
// into Aeron are the same identity, which is the continuity claim.
package tracing

import (
	"context"
	"net/http"
	"strconv"

	"exchange/internal/middleware"
)

// statusRecorder captures the response status for the span's http.status
// attribute and error tail-sampling.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status, r.written = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.status, r.written = http.StatusOK, true
	}
	return r.ResponseWriter.Write(b)
}

// Flush preserves streaming endpoints (SSE/WebSocket upgrade path).
func (r *statusRecorder) Flush() {
	if !r.written {
		r.status, r.written = http.StatusOK, true
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware returns an http middleware that opens a SERVER span per
// request on tracer t and finishes it when the handler returns. Stack
// order: middleware.Tracing BEFORE this (it provides TraceContext);
// this middleware also parses traceparent directly when middleware.
// Tracing did not run (e.g. a service that mounts it standalone).
func Middleware(t *Tracer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			// The remote caller's span id (for tree linkage).
			remote, hasRemote := ParseTraceParent(
				r.Header.Get(TraceParentHeader))
			var span *Span
			name := routeName(r)
			attrs := []Attr{
				Str("http.request.method", r.Method),
				Str("url.path", r.URL.Path),
				Str("http.route", name),
				Str("user_agent.original", r.UserAgent()),
				Str("network.peer.address", clientIPForSpan(r)),
			}
			if tc := middleware.TraceIDFrom(ctx); tc != nil {
				// Adopt the hop identity middleware.Tracing minted — the
				// exported server span's id IS what the Aeron leg will
				// reference as its parent.
				sc := SpanContext{
					TraceID: tc.TraceID,
					SpanID:  tc.SpanID,
					Sampled: tc.Sampled,
				}
				if hasRemote {
					sc.ParentSpanID = remote.SpanID
				}
				ctx, span = t.Adopt(ctx, sc, name, KindServer, attrs...)
			} else {
				if hasRemote {
					ctx = ContextWithSpanContext(ctx, remote)
				}
				ctx, span = t.Start(ctx, name, KindServer, attrs...)
			}
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))
			span.SetAttr(Int("http.response.status_code",
				int64(rec.status)))
			switch {
			case rec.status >= 500:
				span.SetStatus(StatusError,
					"HTTP "+strconv.Itoa(rec.status))
			default:
				span.SetStatus(StatusOK, "")
			}
			span.Finish()
		})
	}
}

// routeName prefers the ServeMux pattern (low cardinality) over the raw
// path so per-order spans aggregate in the backend.
func routeName(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.Method + " " + r.URL.Path
}

// clientIPForSpan is a low-cardinality best-effort peer label — the
// precise value lives in middleware.ClientIP (rate limiter); here we
// only record whether the request arrived proxied.
func clientIPForSpan(r *http.Request) string {
	if r.RemoteAddr == "" {
		return ""
	}
	// strip port without importing net.SplitHostPort failure paths
	if i := len(r.RemoteAddr) - 1; i >= 0 {
		for ; i >= 0; i-- {
			if r.RemoteAddr[i] == ':' {
				return r.RemoteAddr[:i]
			}
		}
	}
	return r.RemoteAddr
}

// AnnotateFromTraceContext is the bridge helper for code paths that hold
// only a middleware.TraceContext (e.g. a NATS consumer reconstructing a
// request's trace) — it stores tc's identity as the REMOTE parent so the
// next Start() parents to it.
func AnnotateFromTraceContext(ctx context.Context,
	tc *middleware.TraceContext) context.Context {
	if tc == nil || tc.TraceID == "" {
		return ctx
	}
	return ContextWithSpanContext(ctx, SpanContext{
		TraceID: tc.TraceID,
		SpanID:  tc.SpanID,
		Sampled: tc.Sampled,
		Remote:  true,
	})
}
