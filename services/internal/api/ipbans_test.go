// Task 5.3.34 — admin ban-surface tests over MemBackend.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/ratelimit"
)

func adminReq(t *testing.T, method, path, ip string, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ip != "" {
		req.SetPathValue("ip", ip)
	}
	return req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "op-9", AccountID: 1, Scopes: []string{"admin"}}))
}

func TestAdminBanListAndAudit(t *testing.T) {
	mem := ratelimit.NewMemBackend()
	admin := &ratelimit.BanAdmin{B: mem}
	list, get, put, del, audit := AdminIPBans(mem, admin)

	// unauthenticated → 401
	rec := httptest.NewRecorder()
	list(rec, httptest.NewRequest("GET", "/api/v1/admin/ip-bans", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon list status=%d", rec.Code)
	}
	// non-admin scope → 403
	req := httptest.NewRequest("GET", "/api/v1/admin/ip-bans", nil)
	req = req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "u", AccountID: 2, Scopes: []string{"read"}}))
	rec = httptest.NewRecorder()
	list(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin list status=%d", rec.Code)
	}

	// PUT a manual ban.
	rec = httptest.NewRecorder()
	put(rec, adminReq(t, "PUT", "/api/v1/admin/ip-bans/198.51.100.7",
		"198.51.100.7", `{"duration_s":300,"reason":"abuse"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rec.Code, rec.Body.String())
	}

	// GET the record.
	rec = httptest.NewRecorder()
	get(rec, adminReq(t, "GET", "/api/v1/admin/ip-bans/198.51.100.7",
		"198.51.100.7", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status=%d", rec.Code)
	}
	var ban ratelimit.Ban
	if err := json.Unmarshal(rec.Body.Bytes(), &ban); err != nil {
		t.Fatal(err)
	}
	if ban.IP != "198.51.100.7" || ban.Actor != "op-9" {
		t.Fatalf("ban=%+v", ban)
	}

	// DELETE (unban, keep strikes).
	rec = httptest.NewRecorder()
	del(rec, adminReq(t, "DELETE", "/api/v1/admin/ip-bans/198.51.100.7",
		"198.51.100.7", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("del status=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	get(rec, adminReq(t, "GET", "/api/v1/admin/ip-bans/198.51.100.7",
		"198.51.100.7", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("post-unban get status=%d, want 404", rec.Code)
	}

	// Audit must carry ban + unban entries with the admin actor.
	rec = httptest.NewRecorder()
	audit(rec, adminReq(t, "GET", "/api/v1/admin/ip-bans/audit", "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status=%d", rec.Code)
	}
	var body struct {
		Audit []json.RawMessage `json:"audit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Audit) < 2 {
		t.Fatalf("audit entries=%d, want ≥2", len(body.Audit))
	}
	var ev ratelimit.AuditEvent
	if err := json.Unmarshal(body.Audit[0], &ev); err != nil || ev.Actor != "op-9" {
		t.Fatalf("audit record=%s err=%v", body.Audit[0], err)
	}
}

func TestAdminAllowlist(t *testing.T) {
	mem := ratelimit.NewMemBackend()
	admin := &ratelimit.BanAdmin{B: mem}
	put, del := AdminIPAllowlist(admin)

	rec := httptest.NewRecorder()
	put(rec, adminReq(t, "PUT", "/api/v1/admin/ip-allowlist/10.0.0.1",
		"10.0.0.1", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("allowlist put status=%d", rec.Code)
	}
	ok, err := mem.IsAllowlisted(context.Background(), "10.0.0.1")
	if err != nil || !ok {
		t.Fatalf("IsAllowlisted=%v err=%v", ok, err)
	}
	rec = httptest.NewRecorder()
	del(rec, adminReq(t, "DELETE", "/api/v1/admin/ip-allowlist/10.0.0.1",
		"10.0.0.1", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("allowlist del status=%d", rec.Code)
	}
	ok, _ = mem.IsAllowlisted(context.Background(), "10.0.0.1")
	if ok {
		t.Fatal("still allowlisted after delete")
	}
}

func TestAdminBanRejectsBadIP(t *testing.T) {
	mem := ratelimit.NewMemBackend()
	admin := &ratelimit.BanAdmin{B: mem}
	_, _, put, _, _ := AdminIPBans(mem, admin)
	rec := httptest.NewRecorder()
	put(rec, adminReq(t, "PUT", "/api/v1/admin/ip-bans/not-an-ip",
		"not-an-ip", `{"duration_s":60}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestHealthFullSchema(t *testing.T) {
	h := HealthFull(func(context.Context) (string, error) {
		return "Throttled", nil
	}, []ShardHealth{{ID: 0, Status: "ok", Leader: true}}, "1.2.3")
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/health/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var body HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "degraded" || body.Mode != "Throttled" {
		t.Fatalf("body=%+v", body)
	}
	if body.Version != "1.2.3" || body.Timestamp == "" {
		t.Fatalf("body=%+v", body)
	}
	// mode read failure → down+Maintenance (fail-closed).
	h = HealthFull(func(context.Context) (string, error) {
		return "", context.DeadlineExceeded
	}, nil, "1.0.0")
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/health/ready", nil))
	body = HealthResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Status != "down" || body.Mode != "Maintenance" {
		t.Fatalf("fail-closed body=%+v", body)
	}
}

var _ = time.Now // keep time import honest if fixtures change
