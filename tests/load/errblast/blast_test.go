// errblast harness tests — drive Run() against a real HTTP surface
// (httptest) that emulates the gateway's rejection taxonomy, plus
// verdict-policy unit cases.
package main

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubGateway answers POST /orders with the status each defect class
// deserves (mirroring the gateway's rejection envelope) and serves
// /health/live 200 while alive.
func stubGateway(t *testing.T, force500 *atomic.Bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		if force500.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body [4096]byte
		n, _ := r.Body.Read(body[:]) // only need the head to classify
		b := string(body[:n])
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "Bearer eyJ") && strings.Contains(r.RemoteAddr, "x"):
			w.WriteHeader(http.StatusUnauthorized) // unreachable guard
		case auth != "" && strings.Contains(auth, "bad"):
			w.WriteHeader(http.StatusUnauthorized)
		case r.Header.Get("Content-Type") == "text/plain":
			w.WriteHeader(http.StatusUnsupportedMediaType)
		case r.ContentLength > 64*1024:
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		case !strings.HasPrefix(strings.TrimSpace(b), "{"):
			w.WriteHeader(http.StatusBadRequest)
		case strings.Contains(b, `"quantity":"-`):
			w.WriteHeader(http.StatusUnprocessableEntity)
		case strings.Contains(b, `"price":"-`):
			w.WriteHeader(http.StatusUnprocessableEntity)
		case strings.Contains(b, "ZZZ"):
			w.WriteHeader(http.StatusBadRequest)
		case strings.Contains(b, "SIDEWAYS"):
			w.WriteHeader(http.StatusBadRequest)
		case !json.Valid(body[:n]) && r.ContentLength < 64*1024:
			w.WriteHeader(http.StatusBadRequest)
		case strings.Contains(b, `"symbol":"EUR/USD"}`) && !strings.Contains(b, "side"):
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusUnauthorized) // no/unknown auth
		}
	})
	return httptest.NewServer(mux)
}

func baseCfg(addr string) Config {
	return Config{
		Addr:         addr,
		Rate:         2_000,
		Duration:     400 * time.Millisecond,
		Workers:      16,
		ControlRate:  25,
		ReqTimeout:   2 * time.Second,
		StartupGrace: 150 * time.Millisecond,
		Max5xxPct:    1.0,
		Seed:         7,
	}
}

func TestBlastRejectionStormStaysHealthy(t *testing.T) {
	var force500 atomic.Bool
	srv := stubGateway(t, &force500)
	defer srv.Close()

	rep, code := Run(context.Background(), baseCfg(srv.URL))
	if code != 0 {
		t.Fatalf("verdict = %d, reasons %v", code, rep.Reasons)
	}
	if rep.Totals.Sent < 200 {
		t.Fatalf("too few requests sent: %d", rep.Totals.Sent)
	}
	if rep.Totals.R5xx != 0 {
		t.Fatalf("unexpected 5xx: %d", rep.Totals.R5xx)
	}
	// Every defect class must have fired — a silent class would mask a
	// dispatch-path regression.
	for name, cr := range rep.PerClass {
		if cr.Sent == 0 {
			t.Fatalf("defect class %q never sent", name)
		}
		rejects := cr.R4xx + cr.R429
		if rejects == 0 {
			t.Fatalf("defect class %q produced no rejections: %+v", name, cr)
		}
	}
	if rep.Control.OK == 0 || rep.Control.Crashed {
		t.Fatalf("control stream unhealthy: %+v", rep.Control)
	}
}

func TestBlast5xxBreachFails(t *testing.T) {
	var force500 atomic.Bool
	srv := stubGateway(t, &force500)
	defer srv.Close()
	force500.Store(true) // every blast request 500s — dispatch path broken

	rep, code := Run(context.Background(), baseCfg(srv.URL))
	if code != 3 {
		t.Fatalf("verdict = %d, want 3 (5xx breach); reasons %v", code, rep.Reasons)
	}
	if rep.Totals.R5xx == 0 {
		t.Fatal("5xx counter not populated")
	}
}

func TestBlastDetectsCrash(t *testing.T) {
	var force500 atomic.Bool
	srv := stubGateway(t, &force500)

	cfg := baseCfg(srv.URL)
	cfg.Duration = 10 * time.Second // would run on if not aborted
	cfg.StartupGrace = 100 * time.Millisecond

	done := make(chan int, 1)
	var rep *Report
	go func() {
		r, c := Run(context.Background(), cfg)
		rep = r
		done <- c
	}()

	time.Sleep(400 * time.Millisecond) // let it get going
	srv.Close()                        // simulate the gateway dying mid-blast

	select {
	case code := <-done:
		if code != 2 {
			t.Fatalf("verdict = %d, want 2 (crash detected)", code)
		}
		if !rep.Control.Crashed {
			t.Fatal("crash flag not set")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("blast did not abort after target death")
	}
}

func TestDefectClassShapes(t *testing.T) {
	cfg := Config{OversizeKB: 128}
	cfg.defaults()
	r := newTestRand()
	seen := map[string]bool{}
	for _, c := range defectClasses() {
		seen[c.name] = true
		spec := c.build(r, 1, &cfg)
		if len(spec.body) == 0 {
			t.Fatalf("class %s produced empty body", c.name)
		}
		switch c.name {
		case "invalid_signature":
			if !spec.noAuth {
				t.Fatal("invalid_signature must carry its own credentials")
			}
			if !strings.HasPrefix(spec.headers["Authorization"], "Bearer eyJ") {
				t.Fatal("invalid_signature must present a JWT-shaped token")
			}
		case "invalid_hmac_headers":
			if spec.headers["X-SIGNATURE"] == "" || spec.headers["X-API-KEY"] == "" {
				t.Fatal("invalid_hmac_headers missing signature headers")
			}
		case "malformed_json":
			if json.Valid(spec.body) {
				t.Fatal("malformed_json body must not parse")
			}
		case "oversized_payload":
			if len(spec.body) < 100*1024 {
				t.Fatalf("oversized body too small: %d", len(spec.body))
			}
		case "negative_quantity":
			if !strings.Contains(string(spec.body), `"quantity":"-`) {
				t.Fatal("negative_quantity body has no negative qty")
			}
		case "wrong_content_type":
			if spec.contentType != "text/plain" {
				t.Fatal("wrong_content_type must not be application/json")
			}
		}
		if !spec.noAuth && spec.headers["Idempotency-Key"] == "" {
			t.Fatalf("class %s missing Idempotency-Key", c.name)
		}
	}
	for _, want := range []string{
		"negative_quantity", "negative_price", "malformed_json",
		"invalid_signature", "invalid_hmac_headers", "unknown_instrument",
		"enum_violation", "missing_fields", "oversized_payload",
		"wrong_content_type",
	} {
		if !seen[want] {
			t.Fatalf("defect class %q missing", want)
		}
	}
}

func TestVerdictPolicy(t *testing.T) {
	cfg := baseCfg("http://unused")
	// Healthy run.
	rep := &Report{Totals: ClassReport{Sent: 1000, R4xx: 900, R429: 95, R5xx: 5}}
	if code, _ := verdict(rep, &cfg); code != 0 {
		t.Fatalf("healthy run verdict = %d", code)
	}
	// 5xx over threshold (1% of 1000 = 10; 50 > 10).
	rep.Totals.R5xx = 50
	if code, _ := verdict(rep, &cfg); code != 3 {
		t.Fatalf("5xx breach verdict = %d, want 3", code)
	}
	// Crash dominates.
	rep.Control.Crashed = true
	if code, _ := verdict(rep, &cfg); code != 2 {
		t.Fatalf("crash verdict = %d, want 2", code)
	}
	// Transport-only (nothing answered) without the flag — still crash class.
	rep = &Report{Totals: ClassReport{Sent: 100, Transport: 100}}
	if code, _ := verdict(rep, &cfg); code != 2 {
		t.Fatalf("transport-only verdict = %d, want 2", code)
	}
}

func TestMintJWTRoundTrip(t *testing.T) {
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 32 zero bytes
	tok, err := mintJWT(key, "errblast", 7, []string{"trade"})
	if err != nil {
		t.Fatalf("mintJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "eyJ") {
		t.Fatalf("token shape wrong: %q", tok)
	}
}

func newTestRand() *rand.Rand { return rand.New(rand.NewSource(1)) }
