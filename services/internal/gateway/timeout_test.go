package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exchange/internal/errs"
)

func TestTimeoutFastHandlerPassesThrough(t *testing.T) {
	r := NewRouter(http.NewServeMux(), errs.New())
	h := r.Timeout(50*time.Millisecond, "GATEWAY_TIMEOUT_MATCHING_ENGINE",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Inner", "1")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))
	if rec.Code != http.StatusCreated || rec.Header().Get("X-Inner") != "1" {
		t.Fatalf("fast path mangled: %d %q body=%s", rec.Code, rec.Header().Get("X-Inner"), rec.Body.String())
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestTimeoutSlowHandlerReturns504(t *testing.T) {
	r := NewRouter(http.NewServeMux(), errs.New())
	h := r.Timeout(30*time.Millisecond, "GATEWAY_TIMEOUT_MATCHING_ENGINE",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond) // exceeds the budget
			w.WriteHeader(200)
		}))
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d, want 504", rec.Code)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("timeout path took %v — client must not hang", elapsed)
	}
	var env Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != "GATEWAY_TIMEOUT_MATCHING_ENGINE" {
		t.Fatalf("envelope code %q, want GATEWAY_TIMEOUT_MATCHING_ENGINE", env.Error)
	}
}

func TestEngineTimeoutBudgetIsSpecValue(t *testing.T) {
	if EngineTimeoutBudget != 500*time.Millisecond {
		t.Fatalf("engine timeout budget %v, want 500ms (spec §8.7)", EngineTimeoutBudget)
	}
}
