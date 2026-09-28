// Task 5.3.29 item 2 — request tracing without an OTel dependency (the
// module graph has none; adding one for header propagation is overkill).
//
// The middleware honours the W3C `traceparent` header: an inbound valid
// trace id is continued (same trace-id, fresh span-id), otherwise a new
// trace id is minted. The resolved trace id lands in the request context
// (TraceIDFrom) and on the response as X-Trace-ID so access logs,
// gateway request_ids, and the future OTel SDK all join on one value.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// TraceHeader is the W3C header consumed/emitted.
const TraceHeader = "traceparent"

// TraceIDHeader is the plain trace-id response header.
const TraceIDHeader = "X-Trace-ID"

type ctxKeyTrace struct{}

// TraceContext is the propagated request trace identity.
type TraceContext struct {
	TraceID string // 32-hex W3C trace id
	SpanID  string // 16-hex span id minted for this hop
	Sampled bool   // inbound traceparent flags bit 0
}

// TraceIDFrom returns the trace context or nil.
func TraceIDFrom(ctx context.Context) *TraceContext {
	tc, _ := ctx.Value(ctxKeyTrace{}).(*TraceContext)
	return tc
}

// parseTraceparent validates `00-{32hex}-{16hex}-{2hex}` and returns the
// trace id — malformed headers are ignored (a new trace starts), never
// rejected: tracing must not fail requests.
func parseTraceparent(h string) (traceID string, sampled bool, ok bool) {
	parts := strings.Split(h, "-")
	if len(parts) != 4 {
		return "", false, false
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return "", false, false
	}
	for _, p := range parts[1:] {
		for _, r := range p {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return "", false, false
			}
		}
	}
	if parts[1] == "00000000000000000000000000000000" {
		return "", false, false
	}
	return parts[1], parts[3] == "01", true
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}

// Tracing injects/continues the W3C trace identity.
func Tracing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc := &TraceContext{SpanID: randomHex(8)}
		if tid, sampled, ok := parseTraceparent(r.Header.Get(TraceHeader)); ok {
			tc.TraceID, tc.Sampled = tid, sampled
		} else {
			tc.TraceID = randomHex(16)
		}
		w.Header().Set(TraceIDHeader, tc.TraceID)
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), ctxKeyTrace{}, tc)))
	})
}
