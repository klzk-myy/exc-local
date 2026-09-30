// Phase-09 Task 9.3.29 item 4 — secrets-inventory admin surface tests:
// the SECRET_ROTATION_OVERDUE (503) emission on the read path, role
// gating fail-closed, and the mark-rotated write path.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exchange/internal/auth"
	"exchange/internal/security"
	excerrors "exchange/pkg/errors"
)

// fakeInventorySvc implements inventoryService without PG.
type fakeInventorySvc struct {
	view      *security.InventoryView
	listErr   error
	markErr   error
	upsertErr error
	marked    string
	mark      security.RotationMark
}

func (f *fakeInventorySvc) List(context.Context, int64) (*security.InventoryView, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.view, nil
}

func (f *fakeInventorySvc) UpsertEntry(_ context.Context, _ int64,
	in security.InventoryEntryInput, _ string) (*security.InventoryEntry, error) {
	if f.upsertErr != nil {
		return nil, f.upsertErr
	}
	return &security.InventoryEntry{SecretName: in.SecretName, Class: in.Class}, nil
}

func (f *fakeInventorySvc) MarkRotated(_ context.Context, _ int64, name string,
	m security.RotationMark, _ string) (*security.InventoryEntry, error) {
	f.marked, f.mark = name, m
	if f.markErr != nil {
		return nil, f.markErr
	}
	return &security.InventoryEntry{SecretName: name}, nil
}

func TestSecretsInventoryListEmits503OnOverdue(t *testing.T) {
	svc := &fakeInventorySvc{view: &security.InventoryView{
		Rows: []security.InventoryRow{
			{InventoryEntry: security.InventoryEntry{SecretName: "old-banking-key"},
				State: security.StateOverdue},
		},
		Overdue: []string{"old-banking-key"},
	}}
	req := httptest.NewRequest("GET", "/api/v1/admin/security/secrets-inventory", nil)
	req = req.WithContext(auth.WithClaims(req.Context(), *adminClaims()))
	rec := httptest.NewRecorder()
	AdminSecretsInventoryList(svc)(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("overdue inventory: status=%d want 503", rec.Code)
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Error != "SECRET_ROTATION_OVERDUE" {
		t.Fatalf("code=%s want SECRET_ROTATION_OVERDUE", env.Error)
	}
	// The assessed register travels in details — the responder sees the
	// overdue name without a second call.
	if env.Details["overdue"] == nil {
		t.Fatalf("details missing overdue list: %v", env.Details)
	}
}

func TestSecretsInventoryListHealthy(t *testing.T) {
	svc := &fakeInventorySvc{view: &security.InventoryView{
		Rows: []security.InventoryRow{
			{InventoryEntry: security.InventoryEntry{SecretName: "jwt-hs256-key"},
				State: security.StateOK},
		},
	}}
	req := httptest.NewRequest("GET", "/api/v1/admin/security/secrets-inventory", nil)
	req = req.WithContext(auth.WithClaims(req.Context(), *adminClaims()))
	rec := httptest.NewRecorder()
	AdminSecretsInventoryList(svc)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy inventory: status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestSecretsInventoryAnonRejected(t *testing.T) {
	svc := &fakeInventorySvc{view: &security.InventoryView{}}
	rec := httptest.NewRecorder()
	AdminSecretsInventoryList(svc)(rec,
		httptest.NewRequest("GET", "/api/v1/admin/security/secrets-inventory", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon: status=%d want 401", rec.Code)
	}
}

func TestSecretsInventoryMarkRotated(t *testing.T) {
	svc := &fakeInventorySvc{}
	body := `{"emergency":true,"incident_ref":"INC-2026-0099","break_glass":true,"grant_id":42}`
	req := httptest.NewRequest("POST",
		"/api/v1/admin/security/secrets-inventory/jwt-hs256-key/mark-rotated",
		strings.NewReader(body))
	req.SetPathValue("name", "jwt-hs256-key")
	req = req.WithContext(auth.WithClaims(req.Context(), *adminClaims()))
	rec := httptest.NewRecorder()
	AdminSecretsInventoryMarkRotated(svc, false)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("mark: status=%d body=%s", rec.Code, rec.Body)
	}
	if svc.marked != "jwt-hs256-key" || !svc.mark.Emergency ||
		svc.mark.IncidentRef != "INC-2026-0099" || svc.mark.GrantID != 42 {
		t.Fatalf("mark fields lost: name=%q mark=%+v", svc.marked, svc.mark)
	}
}

func TestSecretsInventoryServiceErrorMapping(t *testing.T) {
	// A coded service error keeps its registry status; an uncoded error
	// degrades to INTERNAL_ERROR with no internals leaked.
	svc := &fakeInventorySvc{listErr: excerrors.New("UNAUTHORIZED_ROLE", "denied")}
	req := httptest.NewRequest("GET", "/api/v1/admin/security/secrets-inventory", nil)
	req = req.WithContext(auth.WithClaims(req.Context(), *adminClaims()))
	rec := httptest.NewRecorder()
	AdminSecretsInventoryList(svc)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("role denial: status=%d want 403", rec.Code)
	}

	svc.listErr = errors.New("pg exploded")
	rec = httptest.NewRecorder()
	AdminSecretsInventoryList(svc)(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("uncoded err: status=%d want 500", rec.Code)
	}
}
