// Task 5.3.41 step 2 — gateway-to-core timeout guard.
//
// Wraps order-submission handlers that block on Aeron IPC acknowledgement:
// if the wrapped handler has not produced a response within 500ms
// (EngineTimeoutBudget, spec §8.7 item 4), the client gets
// GATEWAY_TIMEOUT_MATCHING_ENGINE (HTTP 504) instead of a hanging
// connection. The handler keeps running in the background — Aeron
// submissions are inherently async — but its writes are discarded into a
// buffer that is only flushed if it finished inside the budget.
//
// Mount this only on routes that synchronously await a core ack
// (order place/cancel/replace); streaming and stub routes must not be
// wrapped.
package gateway

import (
	"bytes"
	"net/http"
	"time"
)

// EngineTimeoutBudget is the spec §8.7/§2.7 hard ceiling on an
// unacknowledged Aeron IPC submission on the REST surface.
const EngineTimeoutBudget = 500 * time.Millisecond

// bufferedResponse captures a handler's output so the timeout path can
// discard or flush it atomically — two writers never race on the wire.
type bufferedResponse struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: make(http.Header)}
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}

// flush copies the captured response to w. Called once, by the winner of
// the timeout race.
func (b *bufferedResponse) flush(w http.ResponseWriter) {
	for k, vs := range b.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.buf.Bytes())
}

// Timeout wraps next with a hard d deadline. If next finishes in time its
// buffered response is flushed verbatim; on expiry the client receives the
// envelope for timeoutCode (default GATEWAY_TIMEOUT_MATCHING_ENGINE, 504).
// The late handler's output is dropped — the client already has its
// terminal answer.
func (r *Router) Timeout(d time.Duration, timeoutCode string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		resp := newBufferedResponse()
		done := make(chan struct{})
		go func() {
			next.ServeHTTP(resp, req)
			close(done)
		}()

		timer := time.NewTimer(d)
		defer timer.Stop()

		select {
		case <-done:
			resp.flush(w)
		case <-timer.C:
			r.WriteError(w, req, timeoutCode,
				"upstream matching engine did not acknowledge within "+d.String(),
				map[string]any{"timeout_ms": d.Milliseconds()})
		case <-req.Context().Done():
			// Client disconnected first — nothing to write; the handler
			// still finishes into the discard buffer.
		}
	})
}

// EngineTimeout is Timeout with the spec-mandated 500ms budget and
// GATEWAY_TIMEOUT_MATCHING_ENGINE code.
func (r *Router) EngineTimeout(next http.Handler) http.Handler {
	return r.Timeout(EngineTimeoutBudget, "GATEWAY_TIMEOUT_MATCHING_ENGINE", next)
}
