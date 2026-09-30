// errblast CLI — Phase-08.5 Task 8.5.3.3.
//
//	errblast -addr http://127.0.0.1:8080 -rate 10000 -duration 60s \
//	    -jwt-secret-b64 <b64> -report errblast.json
//
// Under the plan's staging parameters (50k TPS background load from
// tests/soak or tests/load/bookpump running alongside), this supplies
// the 10k invalid-orders/sec injection leg; in this repo checkout the
// same binary validates the rejection path at whatever rate the dev
// gateway sustains.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	var (
		addr        = flag.String("addr", "http://127.0.0.1:8080", "gateway base URL")
		ordersPath  = flag.String("orders-path", "/api/v1/orders", "order placement endpoint")
		healthPath  = flag.String("health-path", "/health/live", "liveness control-probe endpoint")
		rate        = flag.Float64("rate", 10000, "invalid orders/sec")
		durationRaw = flag.String("duration", "60s",
			"run length: Go duration (60s, 5m) or bare seconds; 0 = until signal")
		workers     = flag.Int("workers", 64, "concurrent sender goroutines")
		controlRate = flag.Float64("control-rate", 5, "liveness probes/sec")
		token       = flag.String("token", "", "static Bearer JWT for authed defect classes")
		jwtKey      = flag.String("jwt-secret-b64", "",
			"HS256 key (base64) — mint rotating-account tokens instead of a static -token")
		jwtAccounts  = flag.Int("jwt-accounts", 512, "account_id rotation space for minted tokens")
		reqTimeout   = flag.Duration("req-timeout", 5*time.Second, "per-request deadline")
		startupGrace = flag.Duration("startup-grace", 5*time.Second,
			"warmup window — transport failures inside it are not a crash")
		max5xx = flag.Float64("max-5xx-pct", 1.0,
			"allowed 5xx share of blast responses (percent) before FAIL")
		xffPool = flag.Int("xff-pool", 0,
			"rotate X-Forwarded-For over N fake client IPs (gateway must run EXC_TRUST_PROXY=1)")
		oversizeKB = flag.Int("oversize-kb", 1024, "oversized-payload class body size (KiB)")
		seed       = flag.Int64("seed", 42, "PRNG seed")
		reportPath = flag.String("report", "", "JSON report output path (default stderr summary only)")
	)
	flag.Parse()

	duration, err := parseDuration(*durationRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad -duration %q: %v\n", *durationRaw, err)
		os.Exit(64) // EX_USAGE
	}

	cfg := Config{
		Addr:         *addr,
		OrdersPath:   *ordersPath,
		HealthPath:   *healthPath,
		Token:        *token,
		JWTKeyB64:    *jwtKey,
		JWTAccounts:  *jwtAccounts,
		Rate:         *rate,
		Duration:     duration,
		Workers:      *workers,
		ControlRate:  *controlRate,
		ReqTimeout:   *reqTimeout,
		StartupGrace: *startupGrace,
		Max5xxPct:    *max5xx,
		XFFPool:      *xffPool,
		OversizeKB:   *oversizeKB,
		Seed:         *seed,
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rep, code := Run(ctx, cfg)

	if *reportPath != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*reportPath, b, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		}
	}

	fmt.Fprintf(os.Stderr,
		"errblast: sent=%d 4xx=%d 429=%d 5xx=%d timeout=%d transport=%d "+
			"| control ok=%d failed=%d crashed=%v | p50=%dµs p99=%dµs | %s\n",
		rep.Totals.Sent, rep.Totals.R4xx, rep.Totals.R429, rep.Totals.R5xx,
		rep.Totals.Timeouts, rep.Totals.Transport,
		rep.Control.OK, rep.Control.Failed+rep.Control.NonOK, rep.Control.Crashed,
		rep.LatencyUS.P50, rep.LatencyUS.P99, rep.Verdict)
	for _, why := range rep.Reasons {
		fmt.Fprintf(os.Stderr, "  - %s\n", why)
	}
	os.Exit(code)
}

// parseDuration accepts a Go duration string ("60s", "5m") or a bare
// number interpreted as seconds ("90" -> 90s). "0"/empty = until signal.
func parseDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("not a duration or seconds: %q", s)
	}
	return time.Duration(secs * float64(time.Second)), nil
}
