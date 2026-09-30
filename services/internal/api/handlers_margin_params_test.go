// handlers_margin_params_test.go — Phase-19 Tasks 19.3.21/19.3.24 admin
// surface coverage: maker validation + role gates, the §13.12 approval
// executor (gate refuses without a passing independent run; applies on
// pass), and the entity-policy list read.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"exchange/internal/admin"
	"exchange/internal/risk"
)

// ---------------------------------------------------------------------------
// Fakes (risk.ModelRunStore / risk.ParamChangeStore minimal in-memory)
// ---------------------------------------------------------------------------

type fakeModelRuns struct{ runs map[int64]*risk.ModelRun }

func (f *fakeModelRuns) InsertRun(context.Context, risk.ModelRun) (int64, error) {
	return 0, nil
}
func (f *fakeModelRuns) RunByID(_ context.Context, id int64) (*risk.ModelRun, error) {
	return f.runs[id], nil
}
func (f *fakeModelRuns) LatestRun(context.Context, string) (*risk.ModelRun, error) {
	return nil, nil
}
func (f *fakeModelRuns) SumBreaches(context.Context, string, time.Time) (int, error) {
	return 0, nil
}
func (f *fakeModelRuns) RecordReview(context.Context, int64, int64) error { return nil }
func (f *fakeModelRuns) ListRuns(context.Context, string, time.Time) ([]risk.ModelRun, error) {
	return nil, nil
}

type fakeParamChanges struct {
	rows   map[int64]risk.ParamChange
	status map[int64]string
	next   int64
}

func newFakeParamChanges() *fakeParamChanges {
	return &fakeParamChanges{rows: map[int64]risk.ParamChange{},
		status: map[int64]string{}, next: 1}
}

func (f *fakeParamChanges) InsertChange(_ context.Context, c risk.ParamChange) (int64, error) {
	id := f.next
	f.next++
	c.ChangeID = id
	f.rows[id] = c
	return id, nil
}

func (f *fakeParamChanges) DecideChange(_ context.Context, id int64, status, _ string) error {
	f.status[id] = status
	return nil
}

// passingRun builds a PASS run validated by reviewer 4242 (independent
// of maker 9001) within the freshness window.
func passingRun(param string) *risk.ModelRun {
	reviewer := int64(4242)
	now := time.Now().UTC()
	return &risk.ModelRun{
		RunID: 5, Kind: "STRESS", Status: "PASS",
		ParamChange: &param, ReviewedBy: &reviewer, ReviewedAt: &now,
		CreatedAt: now,
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/margin-param-changes
// ---------------------------------------------------------------------------

func TestAdminMarginParamChangeSubmit_OK(t *testing.T) {
	dual := &fakeDual{}
	rec := serve(t,
		AdminMarginParamChangeSubmit(dual, AdminRoleResolver(riskManagerResolver)),
		marginReq(t, "POST", "/api/v1/admin/margin-param-changes",
			`{"parameter":"auction_floor_pct","proposed_value":{"floor":0.97},"run_id":5,"reason":"recalibrate"}`,
			adminClaims()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if dual.in.Operation != admin.OpMarginParamChange {
		t.Fatalf("op = %q, want %q", dual.in.Operation, admin.OpMarginParamChange)
	}
	if dual.in.RequiredRole != admin.RoleRiskManager {
		t.Fatalf("required_role = %q, want Risk Manager", dual.in.RequiredRole)
	}
	if dual.in.TargetID != "auction_floor_pct" {
		t.Fatalf("target = %q", dual.in.TargetID)
	}
}

func TestAdminMarginParamChangeSubmit_Validation(t *testing.T) {
	h := AdminMarginParamChangeSubmit(&fakeDual{}, AdminRoleResolver(riskManagerResolver))
	for _, body := range []string{
		`{"proposed_value":{},"run_id":5}`,                 // missing parameter
		`{"parameter":"x","run_id":5}`,                     // missing proposed_value
		`{"parameter":"x","proposed_value":{},"run_id":0}`, // missing run link
		`{oops`, // malformed JSON
	} {
		rec := serve(t, h, marginReq(t, "POST",
			"/api/v1/admin/margin-param-changes", body, adminClaims()))
		if rec.Code == http.StatusAccepted {
			t.Fatalf("body %s must not submit: %s", body, rec.Body.String())
		}
	}
}

func TestAdminMarginParamChangeSubmit_RoleGate(t *testing.T) {
	// Read-Only Auditor role — the maker gate refuses before the store.
	rec := serve(t,
		AdminMarginParamChangeSubmit(&fakeDual{}, AdminRoleResolver(auditorResolver)),
		marginReq(t, "POST", "/api/v1/admin/margin-param-changes",
			`{"parameter":"x","proposed_value":{},"run_id":5}`, adminClaims()))
	if rec.Code == http.StatusAccepted {
		t.Fatalf("auditor must not submit margin-param changes: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// applyMarginParamChange — the §13.12 approval executor
// ---------------------------------------------------------------------------

func TestApplyMarginParamChange_UnvalidatedRunRejected(t *testing.T) {
	changes := newFakeParamChanges()
	gate, err := risk.NewParamChangeGate(&fakeModelRuns{
		runs: map[int64]*risk.ModelRun{ // run 5 is FAIL — not a passing run
			5: {RunID: 5, Kind: "STRESS", Status: "FAIL", CreatedAt: time.Now().UTC()},
		}}, changes, 0, nil)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	approver := int64(4242)
	req := &admin.DualControlRequest{
		RequestedBy: 9001, ApprovedBy: &approver,
		Payload: json.RawMessage(
			`{"parameter":"auction_floor_pct","proposed_value":{"floor":0.97},"run_id":5}`),
	}
	err = applyMarginParamChange(context.Background(), gate, changes, req)
	if err == nil {
		t.Fatal("FAIL-status run must refuse the change")
	}
	// The rejected attempt is recorded — §13.12 audit trail.
	if changes.status[1] != risk.ChangeStatusRejected {
		t.Fatalf("change status = %q, want REJECTED", changes.status[1])
	}
}

func TestApplyMarginParamChange_PassingRunApplies(t *testing.T) {
	changes := newFakeParamChanges()
	gate, err := risk.NewParamChangeGate(&fakeModelRuns{
		runs: map[int64]*risk.ModelRun{5: passingRun("auction_floor_pct")},
	}, changes, 0, nil)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	approver := int64(4242)
	req := &admin.DualControlRequest{
		RequestedBy: 9001, ApprovedBy: &approver,
		Payload: json.RawMessage(
			`{"parameter":"auction_floor_pct","proposed_value":{"floor":0.97},"run_id":5}`),
	}
	if err := applyMarginParamChange(context.Background(), gate, changes, req); err != nil {
		t.Fatalf("passing independent run must apply: %v", err)
	}
	if changes.status[1] != risk.ChangeStatusApplied {
		t.Fatalf("change status = %q, want APPLIED", changes.status[1])
	}
	if changes.rows[1].OwnerID != 9001 || changes.rows[1].ApprovedBy != 4242 {
		t.Fatalf("maker/approver not recorded: %+v", changes.rows[1])
	}
}

func TestApplyMarginParamChange_OwnerValidatorConflict(t *testing.T) {
	changes := newFakeParamChanges()
	gate, err := risk.NewParamChangeGate(&fakeModelRuns{
		runs: map[int64]*risk.ModelRun{5: passingRun("auction_floor_pct")},
	}, changes, 0, nil)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	// Approver == requester breaks independence — the gate refuses even
	// though the run passed (belt under the dual-control approver check).
	same := int64(9001)
	req := &admin.DualControlRequest{
		RequestedBy: 9001, ApprovedBy: &same,
		Payload: json.RawMessage(
			`{"parameter":"auction_floor_pct","proposed_value":{"floor":0.97},"run_id":5}`),
	}
	if err := applyMarginParamChange(context.Background(), gate, changes, req); err == nil {
		t.Fatal("owner==approver must be refused (dual control)")
	}
}

// ---------------------------------------------------------------------------
// POST + GET /api/v1/admin/entity-leverage-policy
// ---------------------------------------------------------------------------

func TestAdminEntityLeveragePolicySubmit_OK(t *testing.T) {
	dual := &fakeDual{}
	rec := serve(t,
		AdminEntityLeveragePolicySubmit(dual, AdminRoleResolver(riskManagerResolver)),
		marginReq(t, "POST", "/api/v1/admin/entity-leverage-policy",
			`{"entity_code":"OFFSHORE-1","client_category":"RETAIL","instrument_group":"MINOR","max_leverage":33,"reason":"entity onboarding"}`,
			adminClaims()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if dual.in.Operation != admin.OpEntityLeveragePolicy {
		t.Fatalf("op = %q, want %q", dual.in.Operation, admin.OpEntityLeveragePolicy)
	}
	if dual.in.TargetID != "OFFSHORE-1:RETAIL:MINOR" {
		t.Fatalf("target = %q", dual.in.TargetID)
	}
}

func TestAdminEntityLeveragePolicySubmit_Validation(t *testing.T) {
	h := AdminEntityLeveragePolicySubmit(&fakeDual{}, AdminRoleResolver(riskManagerResolver))
	for _, body := range []string{
		`{"client_category":"RETAIL","instrument_group":"MAJOR","max_leverage":10}`,                    // no entity
		`{"entity_code":"EU","client_category":"VIP","instrument_group":"MAJOR","max_leverage":10}`,    // bad category
		`{"entity_code":"EU","client_category":"RETAIL","instrument_group":"MICRO","max_leverage":10}`, // bad group
		`{"entity_code":"EU","client_category":"RETAIL","instrument_group":"MAJOR","max_leverage":0}`,  // zero cap
		`{oops`,
	} {
		rec := serve(t, h, marginReq(t, "POST",
			"/api/v1/admin/entity-leverage-policy", body, adminClaims()))
		if rec.Code == http.StatusAccepted {
			t.Fatalf("body %s must not submit: %s", body, rec.Body.String())
		}
	}
}

type fakePolicyLister struct {
	rows []risk.EntityPolicyRow
	err  error
}

func (f *fakePolicyLister) ListEntityPolicies(context.Context) ([]risk.EntityPolicyRow, error) {
	return f.rows, f.err
}

func TestAdminEntityLeveragePolicyList_OK(t *testing.T) {
	rec := serve(t,
		AdminEntityLeveragePolicyList(&fakePolicyLister{rows: []risk.EntityPolicyRow{
			{EntityCode: "EU-ESMA", ClientCategory: "RETAIL",
				InstrumentGroup: "MAJOR", MaxLeverage: 30,
				EffectiveFrom: time.Now().UTC()},
		}}, AdminRoleResolver(riskManagerResolver)),
		marginReq(t, "GET", "/api/v1/admin/entity-leverage-policy", "", adminClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Policies []risk.EntityPolicyRow `json:"policies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Policies) != 1 || out.Policies[0].MaxLeverage != 30 {
		t.Fatalf("policies = %+v", out.Policies)
	}
}
