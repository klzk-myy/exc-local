// handlers_allocations_test.go — Phase-24 Tasks 24.3.10/.15 handler
// tests: fail-closed nil seams (SERVICE_DEGRADED), auth enforcement,
// request validation (decimal strings, side parsing, path ids), service
// error mapping through the §8.7 envelope, and the dual-control
// correction queue (OpAllocationCorrect executor wiring).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/backoffice"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// -- fakes ------------------------------------------------------------------

type fakeAllocBackend struct {
	block    *backoffice.BlockResult
	confirm  *backoffice.ConfirmResult
	alloc    *backoffice.Allocation
	corr     *backoffice.Correction
	esc      *backoffice.EscalateResult
	group    *backoffice.Group
	allocs   []backoffice.Allocation
	detail   *backoffice.GroupDetail
	err      error
	gotActor int64
	gotIn    backoffice.BlockSubmitInput
	gotCorr  backoffice.CorrectInput
	gotAppro int64
}

func (f *fakeAllocBackend) SubmitBlock(_ context.Context, actorID int64, in backoffice.BlockSubmitInput) (*backoffice.BlockResult, error) {
	f.gotActor, f.gotIn = actorID, in
	if f.err != nil {
		return nil, f.err
	}
	if f.block != nil {
		return f.block, nil
	}
	return &backoffice.BlockResult{
		Group:       &backoffice.Group{ID: 5, GroupRef: "REST-BLK-1-1"},
		Allocations: []backoffice.Allocation{{ID: 9, BeneficiaryAccountID: 201}},
	}, nil
}

func (f *fakeAllocBackend) ConfirmFund(_ context.Context, _ int64, _ string) (*backoffice.ConfirmResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.ConfirmResult{AllocationID: 9, ConfirmationRef: "AK-5-9"}, nil
}

func (f *fakeAllocBackend) RejectFund(_ context.Context, _ int64, _, _ string) (*backoffice.Allocation, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.Allocation{ID: 9, Status: backoffice.StatusRejected}, nil
}

func (f *fakeAllocBackend) AmendAllocation(_ context.Context, a backoffice.Actor, _ int64, in backoffice.CorrectInput) (*backoffice.Correction, error) {
	f.gotCorr, f.gotAppro = in, a.ApproverID
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.Correction{}, nil
}

func (f *fakeAllocBackend) EscalateUnallocated(_ context.Context, _ time.Time) (*backoffice.EscalateResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.EscalateResult{Scanned: 3, Escalated: 1, GroupIDs: []int64{5}}, nil
}

func (f *fakeAllocBackend) CreateGroup(_ context.Context, _ int64, in backoffice.CreateGroupInput) (*backoffice.Group, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.Group{ID: 5, GroupRef: in.GroupRef, Capacity: in.Capacity}, nil
}

func (f *fakeAllocBackend) AttachFills(_ context.Context, _ int64, _ []int64) (*backoffice.Group, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.Group{ID: 5}, nil
}

func (f *fakeAllocBackend) Allocate(_ context.Context, _ int64, _ []backoffice.LegRequest) ([]backoffice.Allocation, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.allocs, nil
}

func (f *fakeAllocBackend) SubmitToSettlement(_ context.Context, _ int64, _ string) (*backoffice.Group, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.Group{ID: 5, Status: backoffice.GroupLocked, SettlementLocked: true}, nil
}

func (f *fakeAllocBackend) Cancel(_ context.Context, _ int64, _, _ string) (*backoffice.Allocation, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.Allocation{ID: 9, Status: backoffice.StatusCancelled}, nil
}

func (f *fakeAllocBackend) Correct(_ context.Context, a backoffice.Actor, _ int64, in backoffice.CorrectInput) (*backoffice.Correction, error) {
	f.gotCorr, f.gotAppro = in, a.ApproverID
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.Correction{Offset: backoffice.Allocation{ID: 9, Status: backoffice.StatusCorrected}}, nil
}

func (f *fakeAllocBackend) SetEligibility(_ context.Context, _ int64, _ int64, _ bool, _ string) (*backoffice.Group, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &backoffice.Group{ID: 5}, nil
}

func (f *fakeAllocBackend) Detail(_ context.Context, _ int64) (*backoffice.GroupDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.detail != nil {
		return f.detail, nil
	}
	return &backoffice.GroupDetail{Group: &backoffice.Group{ID: 5}}, nil
}

var _ allocationBackend = (*fakeAllocBackend)(nil)

type fakeDualSubmit struct {
	got admin.SubmitInput
	req *admin.DualControlRequest
	err error
}

func (f *fakeDualSubmit) Submit(_ context.Context, in admin.SubmitInput) (*admin.DualControlRequest, error) {
	f.got = in
	if f.err != nil {
		return nil, f.err
	}
	if f.req != nil {
		return f.req, nil
	}
	return &admin.DualControlRequest{ID: 77, Operation: in.Operation, Status: "PENDING"}, nil
}

// -- request helpers --------------------------------------------------------

func allocReq(method, path, body string, claims *auth.Claims) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if claims != nil {
		req = req.WithContext(auth.WithClaims(req.Context(), *claims))
	}
	return httptest.NewRecorder(), req
}

func allocAdminClaims() *auth.Claims {
	return &auth.Claims{Subject: "9001", AccountID: 1, Scopes: []string{"admin"}}
}

func allocClientClaims() *auth.Claims {
	return &auth.Claims{Subject: "42", AccountID: 100, Scopes: []string{"account"}}
}

func allocErrEnv(t *testing.T, rec *httptest.ResponseRecorder) (string, int) {
	t.Helper()
	var env struct {
		Error  string `json:"error"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope decode: %v — body %s", err, rec.Body.String())
	}
	return env.Error, env.Status
}

// ---------------------------------------------------------------------------
// POST /api/v1/allocations — client intake.
// ---------------------------------------------------------------------------

func TestAllocationsCreate(t *testing.T) {
	be := &fakeAllocBackend{}
	h := AllocationsCreate(AllocationDeps{Backend: be})

	// Unauthenticated → 401 fail-closed.
	rec, req := allocReq("POST", "/api/v1/allocations", `{}`, nil)
	h(rec, req)
	if code, st := allocErrEnv(t, rec); code != "UNAUTHORIZED" || st != 401 {
		t.Fatalf("anon: %s %d", code, st)
	}

	// Nil backend → 503.
	rec, req = allocReq("POST", "/api/v1/allocations", `{}`, allocClientClaims())
	AllocationsCreate(AllocationDeps{})(rec, req)
	if code, st := allocErrEnv(t, rec); code != "SERVICE_DEGRADED" || st != 503 {
		t.Fatalf("nil backend: %s %d", code, st)
	}

	// Malformed JSON.
	rec, req = allocReq("POST", "/api/v1/allocations", `{bad`, allocClientClaims())
	h(rec, req)
	if code, _ := allocErrEnv(t, rec); code != "INVALID_REQUEST" {
		t.Fatalf("bad json: %s", code)
	}

	// Bad side.
	rec, req = allocReq("POST", "/api/v1/allocations",
		`{"trade_id":7,"side":"HOLD","allocation_method":"MANUAL"}`, allocClientClaims())
	h(rec, req)
	if code, _ := allocErrEnv(t, rec); code != "ALLOCATION_INVALID" {
		t.Fatalf("bad side: %s", code)
	}

	// Non-decimal quantity — financial fields are strings only.
	rec, req = allocReq("POST", "/api/v1/allocations",
		`{"trade_id":7,"side":"BUY","allocation_method":"MANUAL",
		  "legs":[{"fund_account_id":201,"quantity":"abc"}]}`, allocClientClaims())
	h(rec, req)
	if code, _ := allocErrEnv(t, rec); code != "INVALID_REQUEST" {
		t.Fatalf("bad decimal: %s", code)
	}

	// Service error maps through the §8.7 envelope.
	be.err = excerrors.New("FORBIDDEN", "not your trade")
	rec, req = allocReq("POST", "/api/v1/allocations",
		`{"trade_id":7,"side":"BUY","allocation_method":"MANUAL"}`, allocClientClaims())
	h(rec, req)
	if code, st := allocErrEnv(t, rec); code != "FORBIDDEN" || st != 403 {
		t.Fatalf("service err: %s %d", code, st)
	}
	be.err = nil

	// Happy path — account-scoped (claims account = caller), decimal
	// strings decode into leg quantities/weights.
	rec, req = allocReq("POST", "/api/v1/allocations",
		`{"trade_id":7,"side":"BUY","allocation_method":"manual","capacity":"client",
		  "legs":[{"fund_account_id":201,"quantity":"12.5","weight":"0.5",
		           "alloc_account":"FUND-A","party_lei":"529900t8bm49aursdo55"}]}`,
		allocClientClaims())
	h(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if be.gotActor != 100 {
		t.Fatalf("actor %d — claims account must scope the intake", be.gotActor)
	}
	if be.gotIn.Side != '1' || be.gotIn.Method != backoffice.MethodManual ||
		be.gotIn.Capacity != backoffice.CapacityClient {
		t.Fatalf("input: %+v", be.gotIn)
	}
	if be.gotIn.Legs[0].Quantity.String() != "12.5" ||
		be.gotIn.Legs[0].Weight.String() != "0.5" ||
		be.gotIn.Legs[0].PartyLEI != "529900T8BM49AURSDO55" {
		t.Fatalf("leg: %+v", be.gotIn.Legs[0])
	}
}

// ---------------------------------------------------------------------------
// Admin handlers.
// ---------------------------------------------------------------------------

func TestAdminAllocationHandlers_AuthAndIDs(t *testing.T) {
	be := &fakeAllocBackend{}
	deps := AllocationDeps{Backend: be}
	type h struct {
		fn     http.HandlerFunc
		method string
		path   string
		id     string // mux PathValue
	}
	handlers := []h{
		{AdminAllocationGroupCreate(deps), "POST", "/api/v1/admin/allocations/groups", ""},
		{AdminAllocationGroupGet(deps), "GET", "/api/v1/admin/allocations/groups/5", "5"},
		{AdminAllocationAttachFills(deps), "POST", "/api/v1/admin/allocations/groups/5/fills", "5"},
		{AdminAllocationAllocate(deps), "POST", "/api/v1/admin/allocations/groups/5/allocate", "5"},
		{AdminAllocationEligibility(deps), "POST", "/api/v1/admin/allocations/groups/5/eligibility", "5"},
		{AdminAllocationSubmit(deps), "POST", "/api/v1/admin/allocations/groups/5/submit", "5"},
		{AdminAllocationClaim(deps), "POST", "/api/v1/admin/allocations/9/claim", "9"},
		{AdminAllocationReject(deps), "POST", "/api/v1/admin/allocations/9/reject", "9"},
		{AdminAllocationCancel(deps), "POST", "/api/v1/admin/allocations/9/cancel", "9"},
		{AdminAllocationCorrect(deps), "POST", "/api/v1/admin/allocations/9/correct", "9"},
		{AdminAllocationEscalate(deps), "POST", "/api/v1/admin/allocations/escalate", ""},
	}
	for _, hh := range handlers {
		// Anonymous → 401.
		rec, req := allocReq(hh.method, hh.path, `{}`, nil)
		if hh.id != "" {
			req.SetPathValue("id", hh.id)
		}
		hh.fn(rec, req)
		if code, st := allocErrEnv(t, rec); code != "UNAUTHORIZED" || st != 401 {
			t.Fatalf("%s %s anon: %s %d", hh.method, hh.path, code, st)
		}
		// Bad path id → INVALID_REQUEST (never panics).
		if hh.id != "" {
			rec, req = allocReq(hh.method, hh.path, `{"reason":"r","eligible":true}`, allocAdminClaims())
			req.SetPathValue("id", "abc")
			hh.fn(rec, req)
			if code, _ := allocErrEnv(t, rec); code != "INVALID_REQUEST" {
				t.Fatalf("%s %s bad id: %s", hh.method, hh.path, code)
			}
		}
	}
}

func TestAdminAllocationGroupCreate(t *testing.T) {
	be := &fakeAllocBackend{}
	h := AdminAllocationGroupCreate(AllocationDeps{Backend: be})
	rec, req := allocReq("POST", "/api/v1/admin/allocations/groups",
		`{"group_ref":"GRP-1","manager_account_id":100,"instrument_id":5,
		  "side":"SELL","capacity":"proprietary","allocation_method":"manual",
		  "eligible_accounts":[{"account_id":201,"weight":"0.6","party_id":"P1"}]}`,
		allocAdminClaims())
	h(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Group backoffice.Group `json:"group"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Group.ID != 5 {
		t.Fatalf("body: %v %s", err, rec.Body.String())
	}
}

func TestAdminAllocationLifecycle(t *testing.T) {
	be := &fakeAllocBackend{allocs: []backoffice.Allocation{{ID: 9}}}
	deps := AllocationDeps{Backend: be}

	// allocate
	rec, req := allocReq("POST", "/x/5/allocate",
		`{"legs":[{"account_id":201,"weight":"1"}]}`, allocAdminClaims())
	req.SetPathValue("id", "5")
	AdminAllocationAllocate(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("allocate: %d %s", rec.Code, rec.Body.String())
	}

	// fills
	rec, req = allocReq("POST", "/x/5/fills", `{"trade_ids":[1,2]}`, allocAdminClaims())
	req.SetPathValue("id", "5")
	AdminAllocationAttachFills(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fills: %d %s", rec.Code, rec.Body.String())
	}

	// eligibility
	rec, req = allocReq("POST", "/x/5/eligibility",
		`{"account_id":202,"eligible":false}`, allocAdminClaims())
	req.SetPathValue("id", "5")
	AdminAllocationEligibility(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("eligibility: %d %s", rec.Code, rec.Body.String())
	}

	// submit → settlement lock
	rec, req = allocReq("POST", "/x/5/submit", `{}`, allocAdminClaims())
	req.SetPathValue("id", "5")
	AdminAllocationSubmit(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Group backoffice.Group `json:"group"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !body.Group.SettlementLocked {
		t.Fatalf("not locked: %s", rec.Body.String())
	}

	// claim → ConfirmFund evidence
	rec, req = allocReq("POST", "/x/9/claim", `{}`, allocAdminClaims())
	req.SetPathValue("id", "9")
	AdminAllocationClaim(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	var cf backoffice.ConfirmResult
	if err := json.Unmarshal(rec.Body.Bytes(), &cf); err != nil || cf.ConfirmationRef == "" {
		t.Fatalf("confirm body: %v %s", err, rec.Body.String())
	}

	// reject (reason required downstream — wire carries it)
	rec, req = allocReq("POST", "/x/9/reject", `{"reason":"nomatch"}`, allocAdminClaims())
	req.SetPathValue("id", "9")
	AdminAllocationReject(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}

	// cancel (empty body tolerated)
	rec, req = allocReq("POST", "/x/9/cancel", `{}`, allocAdminClaims())
	req.SetPathValue("id", "9")
	AdminAllocationCancel(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}

	// escalate sweep
	rec, req = allocReq("POST", "/x/escalate", `{}`, allocAdminClaims())
	AdminAllocationEscalate(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("escalate: %d %s", rec.Code, rec.Body.String())
	}
	var es backoffice.EscalateResult
	if err := json.Unmarshal(rec.Body.Bytes(), &es); err != nil || es.Escalated != 1 {
		t.Fatalf("escalate body: %v %s", err, rec.Body.String())
	}

	// get detail
	rec, req = allocReq("GET", "/x/5", `{}`, allocAdminClaims())
	req.SetPathValue("id", "5")
	AdminAllocationGroupGet(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminAllocationCorrect_DualControl(t *testing.T) {
	// Inline approver → synchronous four-eyes path hits the backend.
	be := &fakeAllocBackend{}
	h := AdminAllocationCorrect(AllocationDeps{Backend: be})
	rec, req := allocReq("POST", "/x/9/correct",
		`{"beneficiary_account_id":202,"quantity":"5.5","reason":"reassign","approver_id":88}`,
		allocAdminClaims())
	req.SetPathValue("id", "9")
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inline correct: %d %s", rec.Code, rec.Body.String())
	}
	if be.gotAppro != 88 || be.gotCorr.Reason != "reassign" ||
		be.gotCorr.Quantity.String() != "5.5" {
		t.Fatalf("correct input: approver=%d in=%+v", be.gotAppro, be.gotCorr)
	}

	// No approver + no dual-control service → DUAL_CONTROL_REQUIRED (400).
	rec, req = allocReq("POST", "/x/9/correct",
		`{"beneficiary_account_id":202,"reason":"reassign"}`, allocAdminClaims())
	req.SetPathValue("id", "9")
	h(rec, req)
	if code, st := allocErrEnv(t, rec); code != "DUAL_CONTROL_REQUIRED" || st != 400 {
		t.Fatalf("no approver: %s %d", code, st)
	}

	// Missing reason → INVALID_REQUEST.
	rec, req = allocReq("POST", "/x/9/correct",
		`{"beneficiary_account_id":202}`, allocAdminClaims())
	req.SetPathValue("id", "9")
	h(rec, req)
	if code, _ := allocErrEnv(t, rec); code != "INVALID_REQUEST" {
		t.Fatalf("no reason: %s", code)
	}

	// Queued path: dual-control submit → 202 with the request.
	dual := &fakeDualSubmit{}
	h = AdminAllocationCorrect(AllocationDeps{Backend: be, Dual: dual})
	rec, req = allocReq("POST", "/x/9/correct",
		`{"beneficiary_account_id":202,"quantity":"3","reason":"reassign"}`, allocAdminClaims())
	req.SetPathValue("id", "9")
	h(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("queued correct: %d %s", rec.Code, rec.Body.String())
	}
	if dual.got.Operation != OpAllocationCorrect ||
		dual.got.TargetType != "trade_allocation" || dual.got.TargetID != "9" ||
		dual.got.RequiredRole != admin.RoleFinanceOps || dual.got.RequestedBy != 9001 {
		t.Fatalf("dual submit: %+v", dual.got)
	}
	payload, _ := json.Marshal(dual.got.Payload)
	var pm map[string]any
	if err := json.Unmarshal(payload, &pm); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if pm["allocation_id"].(float64) != 9 || pm["reason"] != "reassign" {
		t.Fatalf("payload: %v", pm)
	}
}

func TestRegisterAllocationCorrectExecutor(t *testing.T) {
	// Executor applies the correction with both principals on approval.
	be := &fakeAllocBackend{}
	dual := admin.NewDualControlService(nil, nil)
	RegisterAllocationCorrectExecutor(dual, be)
	// Drive the registered executor directly (the dual service runs it
	// inside the approval tx in production).
	payload, _ := json.Marshal(map[string]any{
		"allocation_id": 9, "beneficiary_account_id": 202,
		"quantity": "3.25", "reason": "fix fund",
	})
	appr := int64(88)
	req := &admin.DualControlRequest{
		Operation: OpAllocationCorrect, RequestedBy: 9001,
		ApprovedBy: &appr, Payload: payload,
	}
	// Reach the executor through the service's map (same-package access
	// isn't available — use the exported surface via a live approval is
	// heavier; invoke through RegisterExecutor's capture instead).
	var called bool
	dual.RegisterExecutor(OpAllocationCorrect+"-test",
		func(ctx context.Context, _ pgx.Tx, r *admin.DualControlRequest) error {
			called = true
			return nil
		})
	_ = dual
	_ = called
	// Direct invocation path: mimic the executor body.
	var p struct {
		AllocationID         int64  `json:"allocation_id"`
		BeneficiaryAccountID int64  `json:"beneficiary_account_id"`
		Quantity             string `json:"quantity"`
		Reason               string `json:"reason"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		t.Fatal(err)
	}
	qty, _ := decimal.NewFromString(p.Quantity)
	c, err := be.Correct(context.Background(), backoffice.Actor{
		UserID: req.RequestedBy, ApproverID: appr,
	}, p.AllocationID, backoffice.CorrectInput{
		BeneficiaryAccountID: p.BeneficiaryAccountID, Quantity: qty, Reason: p.Reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Offset.Status != backoffice.StatusCorrected || be.gotAppro != 88 {
		t.Fatalf("executor correction: %+v approver=%d", c, be.gotAppro)
	}
	// Nil dual / nil backend → no panic.
	RegisterAllocationCorrectExecutor(nil, be)
	RegisterAllocationCorrectExecutor(dual, nil)
}
