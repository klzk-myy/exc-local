package api

// Phase-14 Task 14.3.1 — POST /api/v1/orders/oco handler surface tests:
// auth gate + the §23 OCO_SIBLING_CANCEL_RACE → HTTP 409 mapping.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	excerrors "exchange/pkg/errors"
)

// Unauthenticated requests reject before the pair parser runs.
func TestOrderSubmitOCO_RequiresAuth(t *testing.T) {
	h := OrderSubmitOCO(&OrderDeps{})
	body := []byte(`{"symbol":"EURUSD","legs":[{},{}]}`)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/v1/orders/oco",
		bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body.String())
	}
}

// The sibling-race surface (spec §6.5/§23): a service-side
// OCO_SIBLING_CANCEL_RACE must write the registry row — HTTP 409.
func TestWriteServiceErr_OcoRaceMaps409(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders/oco", nil)
	writeServiceErr(rec, req,
		excerrors.New("OCO_SIBLING_CANCEL_RACE", "sibling leg already filled"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if env["error"] != "OCO_SIBLING_CANCEL_RACE" {
		t.Fatalf("body code = %v", env["error"])
	}
}
