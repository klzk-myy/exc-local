// Package middleware holds net/http middleware shared by HTTP services.
// Auth and rate limiting land in Phase-05; request logging ships now.
package middleware

import (
	"log/slog"
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
