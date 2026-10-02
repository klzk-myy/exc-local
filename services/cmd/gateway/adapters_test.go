package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Task-7: the five legacy WS alias paths must answer the spec-mandated
// 410 ENDPOINT_GONE envelope (spec: "legacy WS aliases remain registered
// stubs returning ENDPOINT_GONE-style responses") — not a bare 501 shim
// and not an HTTP redirect a WS client cannot follow mid-upgrade.
func TestWSAliasGone410(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ws/market", nil)
	wsAliasGone().ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Type    string `json:"type"`
		Error   string `json:"error"`
		Status  int    `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body=%s err=%v", rec.Body.String(), err)
	}
	if env.Error != "ENDPOINT_GONE" || env.Status != http.StatusGone {
		t.Fatalf("env=%+v", env)
	}
	if rec.Header().Get("Link") == "" {
		t.Fatal("successor-version Link header missing")
	}
}
