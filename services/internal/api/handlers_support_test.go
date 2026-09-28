// Handler unit tests — Phase-07 Tasks 7.3.7 (support tickets) and
// 7.3.3 (audit query role gate). Fake service seams; no Postgres.
package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/support"

	excerrors "exchange/pkg/errors"
)

// fakeTickets implements the ticketService seam.
type fakeTickets struct {
	created *support.Ticket
	listed  []support.Ticket
	err     error
	gotReq  support.CreateRequest
	gotUpd  support.AdminUpdate
	gotIP   string
}

func (f *fakeTickets) Create(_ context.Context, req support.CreateRequest) (*support.Ticket, error) {
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.created, nil
}
func (f *fakeTickets) ListMine(_ context.Context, id int64, _ *struct {
	Time time.Time
	ID   int64
}, _ int) ([]support.Ticket, error) {
	return f.listed, f.err
}
func (f *fakeTickets) GetMine(_ context.Context, a, t int64) (*support.Ticket, []support.Note, error) {
	return f.created, nil, f.err
}
func (f *fakeTickets) List(_ context.Context, _ int64, _ support.AdminFilter, _ *struct {
	Time time.Time
	ID   int64
}, _ int) ([]support.Ticket, error) {
	return f.listed, f.err
}
func (f *fakeTickets) ComplaintRegister(_ context.Context, _ int64, _ *struct {
	Time time.Time
	ID   int64
}, _ int) ([]support.Ticket, error) {
	return f.listed, f.err
}
func (f *fakeTickets) Get(_ context.Context, _, t int64) (*support.Ticket, []support.Note, error) {
	return f.created, nil, f.err
}
func (f *fakeTickets) Update(_ context.Context, id int64, u support.AdminUpdate, ip string) (*support.Ticket, error) {
	f.gotUpd, f.gotIP = u, ip
	return f.created, f.err
}
func (f *fakeTickets) AddNote(_ context.Context, _, t int64, b string, internal bool, _ string) (*support.Note, error) {
	return &support.Note{NoteID: 7, TicketID: t, Body: b, Internal: internal}, f.err
}

func ticket(t *testing.T, code int, h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, r)
	if rec.Code != code {
		t.Fatalf("status %d want %d — body %s", rec.Code, code, rec.Body.String())
	}
	return rec
}

func TestSupportTicketCreate_Happy(t *testing.T) {
	svc := &fakeTickets{created: &support.Ticket{
		ID: 42, AccountID: 7, Category: "KYC", Status: "OPEN"}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/support/tickets",
		strings.NewReader(`{"category":"KYC","subject":"verify me"}`))
	rec := ticket(t, http.StatusCreated, SupportTicketCreate(svc), userCtx(r, 7))
	if !strings.Contains(rec.Body.String(), `"ticket_id":42`) {
		t.Fatalf("body: %s", rec.Body.String())
	}
	if svc.gotReq.AccountID != 7 || svc.gotReq.Category != "KYC" {
		t.Fatalf("request mapping: %+v", svc.gotReq)
	}
}

func TestSupportTicketCreate_Unauthenticated(t *testing.T) {
	svc := &fakeTickets{}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/support/tickets",
		strings.NewReader(`{"category":"KYC","subject":"x"}`))
	rec := ticket(t, http.StatusUnauthorized, SupportTicketCreate(svc), r)
	if env := decodeErr(t, rec); env.Error != "UNAUTHORIZED" {
		t.Fatalf("code: %s", env.Error)
	}
}

func TestSupportTicketCreate_MalformedBody(t *testing.T) {
	svc := &fakeTickets{}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/support/tickets",
		strings.NewReader(`{nope`))
	rec := ticket(t, http.StatusBadRequest, SupportTicketCreate(svc), userCtx(r, 7))
	if env := decodeErr(t, rec); env.Error != "INVALID_REQUEST" {
		t.Fatalf("code: %s", env.Error)
	}
}

func TestSupportTicketCreate_ServiceErrorMaps(t *testing.T) {
	svc := &fakeTickets{err: excerrors.New("INVALID_REQUEST", "bad category")}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/support/tickets",
		strings.NewReader(`{"category":"BOGUS","subject":"x"}`))
	rec := ticket(t, http.StatusBadRequest, SupportTicketCreate(svc), userCtx(r, 7))
	if env := decodeErr(t, rec); env.Error != "INVALID_REQUEST" {
		t.Fatalf("code: %s", env.Error)
	}
}

func TestSupportTicketList_Happy(t *testing.T) {
	svc := &fakeTickets{listed: []support.Ticket{
		{ID: 1, CreatedAt: time.Now().UTC()}}}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/support/tickets?limit=10", nil)
	rec := ticket(t, http.StatusOK, SupportTicketList(svc), userCtx(r, 7))
	if !strings.Contains(rec.Body.String(), `"data"`) ||
		!strings.Contains(rec.Body.String(), `"next_cursor"`) {
		t.Fatalf("envelope missing: %s", rec.Body.String())
	}
}

func TestSupportTicketGet_BadID(t *testing.T) {
	svc := &fakeTickets{}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/support/tickets/abc", nil)
	r.SetPathValue("id", "abc")
	ticket(t, http.StatusBadRequest, SupportTicketGet(svc), userCtx(r, 7))
}

func TestSupportTicketGet_NotFound404(t *testing.T) {
	svc := &fakeTickets{err: excerrors.New("TICKET_NOT_FOUND", "ticket 9 not found")}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/support/tickets/9", nil)
	r.SetPathValue("id", "9")
	rec := ticket(t, http.StatusNotFound, SupportTicketGet(svc), userCtx(r, 7))
	if env := decodeErr(t, rec); env.Error != "TICKET_NOT_FOUND" {
		t.Fatalf("code: %s", env.Error)
	}
}

func TestAdminSupportTicketUpdate_MappingAndIP(t *testing.T) {
	svc := &fakeTickets{created: &support.Ticket{ID: 5, Status: "IN_PROGRESS"}}
	r := httptest.NewRequest(http.MethodPut, "/api/v1/admin/support/tickets",
		strings.NewReader(`{"ticket_id":5,"status":"IN_PROGRESS","assign_to_me":true}`))
	r.RemoteAddr = "198.51.100.4:1234"
	rec := ticket(t, http.StatusOK, AdminSupportTicketUpdate(svc, false),
		adminCtxB(r))
	if svc.gotUpd.TicketID != 5 || !svc.gotUpd.AssignToMe ||
		svc.gotUpd.Status != "IN_PROGRESS" {
		t.Fatalf("update mapping: %+v", svc.gotUpd)
	}
	if svc.gotIP != "198.51.100.4" {
		t.Fatalf("client ip not propagated for the audit row: %q", svc.gotIP)
	}
	_ = rec
}

func TestAdminSupportTicketUpdate_NoAdminClaims(t *testing.T) {
	svc := &fakeTickets{}
	r := httptest.NewRequest(http.MethodPut, "/api/v1/admin/support/tickets",
		strings.NewReader(`{"ticket_id":5,"status":"RESOLVED"}`))
	// Claims with a non-numeric subject — admin id unresolvable.
	r = r.WithContext(auth.WithClaims(r.Context(),
		auth.Claims{Subject: "not-a-number"}))
	rec := ticket(t, http.StatusUnauthorized, AdminSupportTicketUpdate(svc, false), r)
	if env := decodeErr(t, rec); env.Error != "UNAUTHORIZED" {
		t.Fatalf("code: %s", env.Error)
	}
}

func TestAdminSupportTicketNote_DefaultInternal(t *testing.T) {
	svc := &fakeTickets{}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/support/tickets/5/notes",
		strings.NewReader(`{"body":"called the client"}`))
	r.SetPathValue("id", "5")
	rec := ticket(t, http.StatusCreated, AdminSupportTicketNote(svc, false), adminCtxB(r))
	if !strings.Contains(rec.Body.String(), `"internal":true`) {
		t.Fatalf("notes must default to internal: %s", rec.Body.String())
	}
}

func TestAdminSupportTicketList_Filters(t *testing.T) {
	svc := &fakeTickets{}
	r := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/support/tickets?status=OPEN&queue=COMPLIANCE&breached=1", nil)
	ticket(t, http.StatusOK, AdminSupportTicketList(svc), adminCtxB(r))
}

func TestAdminAuditLog_RoleGate(t *testing.T) {
	// nil resolver → fail closed UNAUTHORIZED_ROLE.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit-log", nil)
	rec := ticket(t, http.StatusForbidden, AdminAuditLog(nil, nil), adminCtxB(r))
	if env := decodeErr(t, rec); env.Error != "UNAUTHORIZED_ROLE" {
		t.Fatalf("code: %s", env.Error)
	}
	// Resolver returning a non-auditor role → also refused.
	resolver := func(context.Context, int64) (string, error) { return "Support Agent", nil }
	rec = ticket(t, http.StatusForbidden, AdminAuditLog(nil, resolver), adminCtxB(r))
	if env := decodeErr(t, rec); env.Error != "UNAUTHORIZED_ROLE" {
		t.Fatalf("non-auditor must be refused, got %s", env.Error)
	}
	// Resolver error → INTERNAL_ERROR, never a silent allow.
	badResolver := func(context.Context, int64) (string, error) { return "", errors.New("db gone") }
	rec = ticket(t, http.StatusInternalServerError, AdminAuditLog(nil, badResolver), adminCtxB(r))
	if env := decodeErr(t, rec); env.Error != "INTERNAL_ERROR" {
		t.Fatalf("resolver error must be INTERNAL_ERROR, got %s", env.Error)
	}
}

func TestAdminSupportView_RoleGate(t *testing.T) {
	// nil resolver → fail closed.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/support/accounts/9", nil)
	r.SetPathValue("id", "9")
	ticket(t, http.StatusForbidden, AdminSupportView(nil, nil, false), adminCtxB(r))

	supportRole := func(context.Context, int64) (string, error) { return "Support Agent", nil }
	financeRole := func(context.Context, int64) (string, error) { return "Finance Ops", nil }
	view := &fakeViewer{v: nil}
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/support/accounts/9", nil)
	r2.SetPathValue("id", "9")
	// Finance Ops is not a support-view role.
	ticket(t, http.StatusForbidden, AdminSupportView(view, financeRole, false), adminCtxB(r2))
	r3 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/support/accounts/9", nil)
	r3.SetPathValue("id", "9")
	rec := ticket(t, http.StatusOK, AdminSupportView(view, supportRole, false), adminCtxB(r3))
	_ = rec
}

type fakeViewer struct {
	v *admin.SupportView
}

func (f *fakeViewer) View(_ context.Context, _, id int64, _ string) (*admin.SupportView, error) {
	if f.v != nil {
		return f.v, nil
	}
	return &admin.SupportView{AccountID: id}, nil
}
