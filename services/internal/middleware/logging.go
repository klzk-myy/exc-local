// Package middleware holds net/http middleware shared by HTTP services.
// Auth and rate limiting land in Phase-05; request logging ships now.
package middleware

import (
	"bufio"
	stderrors "errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// statusWriter captures the response status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the real writer to http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack forwards the connection takeover — the gorilla WS upgrader
// asserts http.Hijacker on the writer it receives, so the logging
// wrapper must pass it through verbatim.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, stderrors.New("underlying writer is not a Hijacker")
	}
	return h.Hijack()
}

// Logging emits one slog record per completed HTTP request.
func Logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"remote", r.RemoteAddr,
			"dur_ms", time.Since(start).Milliseconds(),
		)
	})
}
