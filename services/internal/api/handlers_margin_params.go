// handlers_margin_params.go — Phase-19 Tasks 19.3.21/19.3.24 admin
// surfaces:
//
//	POST /api/v1/admin/margin-param-changes     (Risk Manager maker →
//	  dual-control PENDING; approval runs the §13.12 ParamChangeGate —
//	  no passing linked validation run ⇒ MARGIN_MODEL_UNVALIDATED and
//	  the request never leaves PENDING)
//	POST /api/v1/admin/entity-leverage-policy   (Risk Manager maker →
//	  dual-control PENDING; approval upserts the effective-dated cell
//	  inside the approval tx — spec §13.14 "dual-controlled CRUD")
//	GET  /api/v1/admin/entity-leverage-policy   (Risk Manager/Read-Only
//	  Auditor read of the full matrix)
//
// Both mutations ride the §8.2 four-eyes queue — the maker endpoint
// only creates the request; a second principal's approval executes.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"exchange/internal/admin"
	"exchange/internal/gateway"
	"exchange/internal/risk"
	excerrors "exchange/pkg/errors"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Margin-model parameter changes (Task 19.3.21, spec §13.12)
// ---------------------------------------------------------------------------

// marginParamChangeRequest is the maker payload: the parameter name
// (e.g. "auction_floor_pct", "leverage_tier:US-CFTC:MAJOR"), the
// proposed value as raw JSON, and the linked validation run the
// §13.12 gate must find passing + independent.
type marginParamChangeRequest struct {
	Parameter     string          `json:"parameter"`
	ProposedValue json.RawMessage `json:"proposed_value"`
	RunID         int64           `json:"run_id"`
	Reason        string          `json:"reason"`
}

// AdminMarginParamChangeSubmit — POST /api/v1/admin/margin-param-changes.
// Creates the dual-control request; the approval-time executor owns the
// gate evaluation (validation can land between proposal and approval —
// gating at approve-time, not submit-time, is the fail-closed choice).
func AdminMarginParamChangeSubmit(dual dualSubmitter,
	resolver AdminRoleResolver) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {
		makerID, ok := requireRiskManagerRole(w, r, resolver)
		if !ok {
			return
		}
		if dual == nil {
			WriteError(w, "SERVICE_DEGRADED", "dual-control service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var in marginParamChangeRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "malformed JSON body"))
			return
		}
		if in.Parameter == "" {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "parameter is required"))
			return
		}
		if len(in.ProposedValue) == 0 {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "proposed_value is required"))
			return
		}
		if in.RunID <= 0 {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST",
				"run_id must link a margin_model_runs row"))
			return
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpMarginParamChange,
			TargetType:   "margin_parameter",
			TargetID:     in.Parameter,
			RequiredRole: admin.RoleRiskManager,
			RequestedBy:  makerID,
			Reason:       in.Reason,
			Payload: map[string]any{
				"parameter":      in.Parameter,
				"proposed_value": json.RawMessage(in.ProposedValue),
				"run_id":         in.RunID,
			},
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		writePendingDual(w, req, map[string]any{"parameter": in.Parameter})
	}
}

// RegisterMarginParamChangeExecutor binds the §13.12 gate to the
// dual-control approval path: approval evaluates the linked run
// (dual-control check + PASS + freshness + validator independence +
// parameter scope) and records the margin_model_param_changes audit
// row — VALIDATED→APPLIED on success, REJECTED with reason on refusal.
// A refusal fails the executor, so the request stays PENDING and the
// audit row preserves the rejected attempt (§13.12 requires the
// attempt itself be auditable).
func RegisterMarginParamChangeExecutor(dual *admin.DualControlService,
	gate *risk.ParamChangeGate, changes risk.ParamChangeStore) {

	dual.RegisterExecutor(admin.OpMarginParamChange,
		func(ctx context.Context, _ pgx.Tx, req *admin.DualControlRequest) error {
			return applyMarginParamChange(ctx, gate, changes, req)
		})
}

// applyMarginParamChange is the §13.12 approval path, factored out for
// direct testing: decode the payload → GateAndRecord (PENDING →
// VALIDATED|REJECTED audit row) → on pass, stamp APPLIED.
func applyMarginParamChange(ctx context.Context, gate *risk.ParamChangeGate,
	changes risk.ParamChangeStore, req *admin.DualControlRequest) error {

	var p struct {
		Parameter     string          `json:"parameter"`
		ProposedValue json.RawMessage `json:"proposed_value"`
		RunID         int64           `json:"run_id"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		return excerrors.New("INVALID_REQUEST", "margin-param-change payload malformed")
	}
	var approver int64
	if req.ApprovedBy != nil {
		approver = *req.ApprovedBy
	}
	id, err := gate.GateAndRecord(ctx, risk.ParamChange{
		Parameter:  p.Parameter,
		Proposed:   p.ProposedValue,
		OwnerID:    req.RequestedBy,
		ApprovedBy: approver,
		RunID:      p.RunID,
	})
	if err != nil {
		return err // gate codes: DUAL_CONTROL_REQUIRED / MARGIN_MODEL_UNVALIDATED
	}
	if changes != nil && id > 0 {
		if derr := changes.DecideChange(ctx, id, risk.ChangeStatusApplied, ""); derr != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "param change applied stamp", derr)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Entity leverage policy matrix (Task 19.3.24, spec §13.14)
// ---------------------------------------------------------------------------

// entityLeveragePolicyRequest is the maker payload for one
// effective-dated policy cell.
type entityLeveragePolicyRequest struct {
	EntityCode      string    `json:"entity_code"`
	ClientCategory  string    `json:"client_category"`
	InstrumentGroup string    `json:"instrument_group"`
	MaxLeverage     int       `json:"max_leverage"`
	EffectiveFrom   time.Time `json:"effective_from"`
	Reason          string    `json:"reason"`
}

var entityLeverageCategories = map[string]bool{
	"RETAIL": true, "PROFESSIONAL": true, "ELIGIBLE_COUNTERPARTY": true,
}

var entityLeverageGroups = map[string]bool{
	"MAJOR": true, "MINOR": true, "EXOTIC": true,
}

// EntityPolicyLister is the read seam — *risk.PgLeverageStore
// satisfies it.
type EntityPolicyLister interface {
	ListEntityPolicies(ctx context.Context) ([]risk.EntityPolicyRow, error)
}

// AdminEntityLeveragePolicySubmit — POST /api/v1/admin/entity-leverage-policy.
// Maker submits the cell upsert as a dual-control request; approval
// executes the insert/update inside the approval transaction.
func AdminEntityLeveragePolicySubmit(dual dualSubmitter,
	resolver AdminRoleResolver) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {
		makerID, ok := requireRiskManagerRole(w, r, resolver)
		if !ok {
			return
		}
		if dual == nil {
			WriteError(w, "SERVICE_DEGRADED", "dual-control service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var in entityLeveragePolicyRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "malformed JSON body"))
			return
		}
		if in.EntityCode == "" || len(in.EntityCode) > 32 {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "entity_code required (≤32 chars)"))
			return
		}
		if !entityLeverageCategories[in.ClientCategory] {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST",
				"client_category must be RETAIL|PROFESSIONAL|ELIGIBLE_COUNTERPARTY"))
			return
		}
		if !entityLeverageGroups[in.InstrumentGroup] {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST",
				"instrument_group must be MAJOR|MINOR|EXOTIC"))
			return
		}
		if in.MaxLeverage <= 0 {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "max_leverage must be > 0"))
			return
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpEntityLeveragePolicy,
			TargetType:   "entity_leverage_policy",
			TargetID:     in.EntityCode + ":" + in.ClientCategory + ":" + in.InstrumentGroup,
			RequiredRole: admin.RoleRiskManager,
			RequestedBy:  makerID,
			Reason:       in.Reason,
			Payload: map[string]any{
				"entity_code":      in.EntityCode,
				"client_category":  in.ClientCategory,
				"instrument_group": in.InstrumentGroup,
				"max_leverage":     in.MaxLeverage,
				"effective_from":   in.EffectiveFrom,
			},
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		writePendingDual(w, req, map[string]any{"entity_code": in.EntityCode})
	}
}

// AdminEntityLeveragePolicyList — GET /api/v1/admin/entity-leverage-policy.
// Risk Manager or Read-Only Auditor reads the full effective-dated
// matrix (any admin binding satisfying the route role gate reaches the
// handler; the resolver enforces the role names §8.2 defines).
func AdminEntityLeveragePolicyList(lister EntityPolicyLister,
	resolver AdminRoleResolver) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireRiskManagerRole(w, r, resolver); !ok {
			return
		}
		if lister == nil {
			WriteError(w, "SERVICE_DEGRADED", "leverage policy store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := lister.ListEntityPolicies(r.Context())
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"policies": rows})
	}
}

// RegisterEntityLeveragePolicyExecutor binds the cell upsert to the
// approval transaction — the mutation and the four-eyes record commit
// or roll back together; created_by/updated_by carry maker/approver.
func RegisterEntityLeveragePolicyExecutor(dual *admin.DualControlService) {
	dual.RegisterExecutor(admin.OpEntityLeveragePolicy,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var p entityLeveragePolicyRequest
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return excerrors.New("INVALID_REQUEST", "entity-leverage-policy payload malformed")
			}
			if p.EffectiveFrom.IsZero() {
				p.EffectiveFrom = time.Now().UTC()
			}
			var approver int64
			if req.ApprovedBy != nil {
				approver = *req.ApprovedBy
			}
			return risk.UpsertEntityPolicyTx(ctx, tx, risk.EntityPolicyRow{
				EntityCode:      p.EntityCode,
				ClientCategory:  p.ClientCategory,
				InstrumentGroup: p.InstrumentGroup,
				MaxLeverage:     p.MaxLeverage,
				EffectiveFrom:   p.EffectiveFrom,
			}, req.RequestedBy, approver)
		})
}
