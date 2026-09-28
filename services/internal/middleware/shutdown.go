// Task 9.3.23 — Go service graceful shutdown & rolling update protocol
// (spec §24 #216).
//
// The uniform drain contract every Go service runs on SIGINT/SIGTERM:
//
//  1. stop accepting — DrainFlag flips atomically; readiness gates go
//     unhealthy immediately (ReadyGate), and RejectWhenDraining refuses
//     new requests on keep-alive connections while in-flight work and
//     cancels still complete;
//  2. drain — WebSocket advisory + close (ws.Server.Drain /
//     marketdata.Server.Drain) and HTTP in-flight via http.Server.Shutdown;
//  3. flush — ordered Step hooks (event buffers, audit, telemetry);
//  4. exit — RunSteps returns joined errors so callers log and exit
//     non-zero on a failed drain.
//
// Every step is deadline-bounded; the plan never blocks shutdown
// unboundedly.
package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// DrainFlag is the service-wide "draining" latch — idempotent, atomic,
// set exactly once when the signal arrives.
type DrainFlag struct{ on atomic.Bool }

// Set flips the flag. Safe to call repeatedly.
func (f *DrainFlag) Set() { f.on.Store(true) }

// Draining reports whether the service is draining.
func (f *DrainFlag) Draining() bool { return f.on.Load() }

// ReadyGate wraps a readiness handler so a draining service reports
// unready immediately (K8s pulls the pod while connections drain;
// liveness endpoints must NOT be wrapped — the pod stays alive through
// the drain).
func ReadyGate(f *DrainFlag, emit GateEmitter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f != nil && f.Draining() {
			emit(w, r, "MAINTENANCE_MODE", "service draining for shutdown", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RejectWhenDraining refuses new non-exempt requests once draining —
// http.Server.Shutdown stops listeners but keep-alive connections may
// still offer fresh requests while idle connections linger. Exempt
// traffic mirrors the drain contract: cancels always pass, health stays
// observable.
func RejectWhenDraining(f *DrainFlag, emit GateEmitter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if f != nil && f.Draining() && !drainAllowed(r) {
				emit(w, r, "MAINTENANCE_MODE",
					"service draining — reconnect to a healthy endpoint", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// drainAllowed is the in-drain allowlist: cancels and observability
// survive the drain window so clients can flatten risk; everything else
// gets a clean 503 instead of a half-processed write.
func drainAllowed(r *http.Request) bool {
	p := r.URL.Path
	if ShedExempt(r) { // cancels + health + status feed
		return true
	}
	return r.Method == http.MethodGet &&
		strings.HasPrefix(p, "/api/v1/orders") // order-status reads survive
}

// Step is one bounded drain step of the ordered contract.
type Step struct {
	Name    string
	Timeout time.Duration // bounds Fn; <=0 uses the step default 10s
	Fn      func(ctx context.Context) error
}

// RunSteps executes steps in order, each under its own deadline. A
// failing step is logged and recorded but never blocks the remaining
// steps — shutdown must converge (spec §2.7: no unbounded waits). The
// returned error joins all step failures.
func RunSteps(ctx context.Context, log *slog.Logger, steps []Step) error {
	if log == nil {
		log = slog.Default()
	}
	var errs []error
	for _, st := range steps {
		to := st.Timeout
		if to <= 0 {
			to = 10 * time.Second
		}
		sctx, cancel := context.WithTimeout(ctx, to)
		start := time.Now()
		err := st.Fn(sctx)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("shutdown step failed",
				"step", st.Name, "err", err,
				"dur_ms", time.Since(start).Milliseconds())
			errs = append(errs, fmt.Errorf("%s: %w", st.Name, err))
			continue
		}
		log.Info("shutdown step done",
			"step", st.Name, "dur_ms", time.Since(start).Milliseconds())
	}
	return errors.Join(errs...)
}
