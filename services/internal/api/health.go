// Package api holds HTTP handlers shared by services.
// Deeper routes are registered in Phase-05 (Task 5.3.7).
package api

import (
	"net/http"
)

// Health is the stub liveness endpoint. It returns HTTP 200 with the exact
// body {"status":"ok"} required by the Task 1.3.2 acceptance criteria.
func Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
