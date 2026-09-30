// Phase-24 Tasks 24.3.10 / 24.3.15 — bunched-order + block-trade
// allocation surface (spec §5.31, §17.8, §24 #172/#237).
//
// Client intake:
//
//	POST /api/v1/allocations                        block-trade split (Engine.SubmitBlock)
//
// Admin lifecycle (role gates ride the route registry):
//
//	GET  /api/v1/admin/allocations/groups/{id}      group evidence bundle (Read-Only Auditor+)
//	POST /api/v1/admin/allocations/groups           register group + eligibility (Finance Ops+)
//	POST /api/v1/admin/allocations/groups/{id}/fills      attach fills (Finance Ops+)
//	POST /api/v1/admin/allocations/groups/{id}/allocate   run the stored method (Finance Ops+)
//	POST /api/v1/admin/allocations/groups/{id}/eligibility flip eligible/ineligible (Finance Ops+)
//	POST /api/v1/admin/allocations/groups/{id}/submit     settlement lock (Finance Ops+)
//	POST /api/v1/admin/allocations/{id}/claim       fund-ops confirm → legs + GL + 35=AK (Finance Ops+)
//	POST /api/v1/admin/allocations/{id}/reject      fund-ops reject (Finance Ops+)
//	POST /api/v1/admin/allocations/{id}/cancel      pre-settlement cancel (Finance Ops+)
//	POST /api/v1/admin/allocations/{id}/correct     pre-lock: direct; settlement-locked: dual-control queue
//	POST /api/v1/admin/allocations/escalate         T+0 EOD unallocated sweep (Finance Ops+)
//
// Post-submission corrections are four-eyes: on a settlement-locked
// group the handler submits a dual-control request (operation
// "allocation-correct", RequiredRole=Finance Ops); the registered
// executor applies the offset/replacement pair inside the approval
// flow's error path (the correction itself runs in its own store tx —
// a failed correction aborts approval so no approval records a
// correction that never landed). On unlocked groups the synchronous
// path applies immediately.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/internal/backoffice"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// OpAllocationCorrect is the dual-control operation name for
// settlement-locked allocation corrections (the admin.Op* constants live
// upstream; this op name rides the same queue via RegisterExecutor —
// same convention as backoffice's OpSettlementExceptionResolve).
const OpAllocationCorrect = "allocation-correct"

// allocationBackend is the engine seam — *backoffice.Engine satisfies it.
type allocationBackend interface {
	SubmitBlock(ctx context.Context, actorID int64, in backoffice.BlockSubmitInput) (*backoffice.BlockResult, error)
	ConfirmFund(ctx context.Context, allocID int64, confirmer string) (*backoffice.ConfirmResult, error)
	RejectFund(ctx context.Context, allocID int64, confirmer, reason string) (*backoffice.Allocation, error)
	AmendAllocation(ctx context.Context, actor backoffice.Actor, allocID int64, in backoffice.CorrectInput) (*backoffice.Correction, error)
	EscalateUnallocated(ctx context.Context, cutoff time.Time) (*backoffice.EscalateResult, error)
	CreateGroup(ctx context.Context, actorID int64, in backoffice.CreateGroupInput) (*backoffice.Group, error)
	AttachFills(ctx context.Context, groupID int64, tradeIDs []int64) (*backoffice.Group, error)
	Allocate(ctx context.Context, groupID int64, legs []backoffice.LegRequest) ([]backoffice.Allocation, error)
	SubmitToSettlement(ctx context.Context, groupID int64, actor string) (*backoffice.Group, error)
	Cancel(ctx context.Context, allocID int64, actor, reason string) (*backoffice.Allocation, error)
	Correct(ctx context.Context, actor backoffice.Actor, allocID int64, in backoffice.CorrectInput) (*backoffice.Correction, error)
	SetEligibility(ctx context.Context, groupID, accountID int64, eligible bool, actor string) (*backoffice.Group, error)
	Detail(ctx context.Context, groupID int64) (*backoffice.GroupDetail, error)
}

// AllocationDeps wires the allocation surface.
type AllocationDeps struct {
	Backend    allocationBackend
	Dual       dualSubmitter // *admin.DualControlService — required for locked corrections
	TrustProxy bool
}

func (d AllocationDeps) ok(w http.ResponseWriter, r *http.Request) bool {
	if d.Backend == nil {
		WriteError(w, "SERVICE_DEGRADED", "allocation service unavailable",
			gateway.RequestIDFrom(r.Context()), nil)
		return false
	}
	return true
}

func allocPathID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	raw := r.PathValue(key)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, "INVALID_REQUEST", key+" path parameter must be a positive integer",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// Client intake — POST /api/v1/allocations
// ---------------------------------------------------------------------------

// allocationSubmitRequest is the REST intake body. Quantities cross the
// wire as decimal strings (never float64).
type allocationSubmitRequest struct {
	TradeID  int64  `json:"trade_id"`
	Side     string `json:"side"` // "BUY"|"SELL"|"1"|"2"
	Method   string `json:"allocation_method"`
	Capacity string `json:"capacity,omitempty"`
	GroupRef string `json:"group_ref,omitempty"`
	Legs     []struct {
		AccountID int64  `json:"fund_account_id"`
		Quantity  string `json:"quantity,omitempty"`
		Weight    string `json:"weight,omitempty"`
		Ref       string `json:"alloc_account,omitempty"`
		PartyID   string `json:"party_id,omitempty"`
		PartyLEI  string `json:"party_lei,omitempty"`
	} `json:"legs"`
}

func parseSideByte(s string) (byte, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "BUY", "1":
		return '1', true
	case "SELL", "2":
		return '2', true
	}
	return 0, false
}

func decField(name, raw string, required bool) (decimal.Decimal, error) {
	if raw == "" {
		if required {
			return decimal.Zero, excerrors.New("INVALID_REQUEST", name+" is required")
		}
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, excerrors.New("INVALID_REQUEST", name+": invalid decimal "+strconv.Quote(raw))
	}
	return d, nil
}

// AllocationsCreate serves POST /api/v1/allocations — the account
// manager splits a block trade into fund sub-accounts under the named
// deterministic method (MANUAL quantities, RULE_BASED percentages,
// PRO_RATA weights, EQUAL_SPLIT).
func AllocationsCreate(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var req allocationSubmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		side, ok := parseSideByte(req.Side)
		if !ok {
			WriteError(w, "ALLOCATION_INVALID", "side must be BUY or SELL",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		in := backoffice.BlockSubmitInput{
			TradeID:  req.TradeID,
			Side:     side,
			Method:   backoffice.Method(strings.ToUpper(strings.TrimSpace(req.Method))),
			Capacity: backoffice.Capacity(strings.ToUpper(strings.TrimSpace(req.Capacity))),
			GroupRef: strings.TrimSpace(req.GroupRef),
		}
		for i, l := range req.Legs {
			qty, err := decField("legs.quantity", l.Quantity, false)
			if err != nil {
				WriteError(w, "INVALID_REQUEST",
					"leg "+strconv.Itoa(i+1)+": quantity must be a decimal string",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			wt, err := decField("legs.weight", l.Weight, false)
			if err != nil {
				WriteError(w, "INVALID_REQUEST",
					"leg "+strconv.Itoa(i+1)+": weight must be a decimal string",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			in.Legs = append(in.Legs, backoffice.LegRequest{
				AccountID: l.AccountID, Quantity: qty, Weight: wt,
				Ref: l.Ref, PartyID: l.PartyID, PartyLEI: strings.ToUpper(l.PartyLEI),
			})
		}
		res, err := deps.Backend.SubmitBlock(r.Context(), accountID, in)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, res)
	}
}

// ---------------------------------------------------------------------------
// Admin — groups.
// ---------------------------------------------------------------------------

// allocGroupRequest is the POST /api/v1/admin/allocations/groups body.
type allocGroupRequest struct {
	GroupRef         string `json:"group_ref"`
	ManagerAccountID int64  `json:"manager_account_id"`
	InstrumentID     int64  `json:"instrument_id"`
	Side             string `json:"side"`
	Capacity         string `json:"capacity"`
	Method           string `json:"allocation_method"`
	Eligible         []struct {
		AccountID int64  `json:"account_id"`
		Weight    string `json:"weight,omitempty"`
		PartyID   string `json:"party_id,omitempty"`
		PartyLEI  string `json:"party_lei,omitempty"`
	} `json:"eligible_accounts"`
}

// AdminAllocationGroupCreate serves POST /api/v1/admin/allocations/groups —
// registers the bunched-order group BEFORE order entry with its eligible
// beneficial accounts and declared capacity.
func AdminAllocationGroupCreate(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		var req allocGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		side, ok := parseSideByte(req.Side)
		if !ok {
			WriteError(w, "ALLOCATION_INVALID", "side must be BUY or SELL",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		in := backoffice.CreateGroupInput{
			GroupRef:         strings.TrimSpace(req.GroupRef),
			ManagerAccountID: req.ManagerAccountID,
			InstrumentID:     req.InstrumentID,
			Side:             side,
			Capacity:         backoffice.Capacity(strings.ToUpper(req.Capacity)),
			Method:           backoffice.Method(strings.ToUpper(req.Method)),
		}
		for i, e := range req.Eligible {
			wt, err := decField("eligible.weight", e.Weight, false)
			if err != nil {
				WriteError(w, "INVALID_REQUEST",
					"eligible account "+strconv.Itoa(i+1)+": invalid weight",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			in.Eligible = append(in.Eligible, backoffice.EligibleAccount{
				AccountID: e.AccountID, Weight: wt,
				PartyID: e.PartyID, PartyLEI: strings.ToUpper(e.PartyLEI),
			})
		}
		// Capacity defaults on the eligibility rows to the group's.
		for i := range in.Eligible {
			in.Eligible[i].Capacity = in.Capacity
		}
		g, err := deps.Backend.CreateGroup(r.Context(), actor, in)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"group": g})
	}
}

// AdminAllocationGroupGet serves GET /api/v1/admin/allocations/groups/{id}
// — the full evidence bundle (group, fills, allocations, registry, audit
// chain).
func AdminAllocationGroupGet(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, ok := allocPathID(w, r, "id")
		if !ok {
			return
		}
		d, err := deps.Backend.Detail(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if d == nil {
			WriteError(w, "ALLOCATION_NOT_FOUND", "group not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, d)
	}
}

// attachFillsRequest is the .../fills body.
type attachFillsRequest struct {
	TradeIDs []int64 `json:"trade_ids"`
}

// AdminAllocationAttachFills serves POST .../groups/{id}/fills.
func AdminAllocationAttachFills(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, ok := allocPathID(w, r, "id")
		if !ok {
			return
		}
		var req attachFillsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		g, err := deps.Backend.AttachFills(r.Context(), id, req.TradeIDs)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"group": g})
	}
}

// allocateRequest is the .../allocate body.
type allocateRequest struct {
	Legs []struct {
		AccountID int64  `json:"account_id"`
		Quantity  string `json:"quantity,omitempty"`
		Weight    string `json:"weight,omitempty"`
		Ref       string `json:"alloc_account,omitempty"`
		PartyID   string `json:"party_id,omitempty"`
		PartyLEI  string `json:"party_lei,omitempty"`
	} `json:"legs"`
}

// AdminAllocationAllocate serves POST .../groups/{id}/allocate — runs the
// group's pre-declared deterministic method.
func AdminAllocationAllocate(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		id, ok := allocPathID(w, r, "id")
		if !ok {
			return
		}
		var req allocateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var legs []backoffice.LegRequest
		for i, l := range req.Legs {
			qty, err := decField("quantity", l.Quantity, false)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "leg "+strconv.Itoa(i+1)+": invalid quantity",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			wt, err := decField("weight", l.Weight, false)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "leg "+strconv.Itoa(i+1)+": invalid weight",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			legs = append(legs, backoffice.LegRequest{
				AccountID: l.AccountID, Quantity: qty, Weight: wt,
				Ref: l.Ref, PartyID: l.PartyID, PartyLEI: strings.ToUpper(l.PartyLEI),
			})
		}
		rows, err := deps.Backend.Allocate(r.Context(), id, legs)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"allocations": rows})
	}
}

// eligibilityRequest is the .../eligibility body.
type eligibilityRequest struct {
	AccountID int64 `json:"account_id"`
	Eligible  bool  `json:"eligible"`
}

// AdminAllocationEligibility serves POST .../groups/{id}/eligibility —
// flips a beneficiary's eligibility after execution.
func AdminAllocationEligibility(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, ok := allocPathID(w, r, "id")
		if !ok {
			return
		}
		var req eligibilityRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		g, err := deps.Backend.SetEligibility(r.Context(), id, req.AccountID, req.Eligible, fmt.Sprint(actor))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"group": g})
	}
}

// AdminAllocationSubmit serves POST .../groups/{id}/submit — locks the
// group for settlement.
func AdminAllocationSubmit(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, ok := allocPathID(w, r, "id")
		if !ok {
			return
		}
		g, err := deps.Backend.SubmitToSettlement(r.Context(), id, fmt.Sprint(actor))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"group": g})
	}
}

// ---------------------------------------------------------------------------
// Admin — allocation legs (fund-ops claim/reject/cancel/correct).
// ---------------------------------------------------------------------------

type reasonRequest struct {
	Reason string `json:"reason"`
}

// AdminAllocationClaim serves POST /api/v1/admin/allocations/{id}/claim —
// fund-ops confirmation: child settlement instructions + 2090 GL rebook +
// per-fund 35=AK report, atomic.
func AdminAllocationClaim(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, ok := allocPathID(w, r, "id")
		if !ok {
			return
		}
		res, err := deps.Backend.ConfirmFund(r.Context(), id, "admin:"+fmt.Sprint(actor))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// AdminAllocationReject serves POST .../{id}/reject.
func AdminAllocationReject(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, ok := allocPathID(w, r, "id")
		if !ok {
			return
		}
		var req reasonRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		a, err := deps.Backend.RejectFund(r.Context(), id, "admin:"+fmt.Sprint(actor), req.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"allocation": a})
	}
}

// AdminAllocationCancel serves POST .../{id}/cancel (pre-settlement).
func AdminAllocationCancel(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, ok := allocPathID(w, r, "id")
		if !ok {
			return
		}
		var req reasonRequest
		// Reason optional on cancel but recommended — decode is tolerant.
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				WriteError(w, "INVALID_REQUEST", "malformed JSON body",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		a, err := deps.Backend.Cancel(r.Context(), id, "admin:"+fmt.Sprint(actor), req.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"allocation": a})
	}
}

// correctRequest is the .../correct body.
type correctRequest struct {
	BeneficiaryAccountID int64  `json:"beneficiary_account_id,omitempty"`
	Quantity             string `json:"quantity,omitempty"`
	Reason               string `json:"reason"`
	// Locked only: the synchronous four-eyes call may carry the second
	// principal inline; the queued path goes through dual-control submit.
	ApproverID int64 `json:"approver_id,omitempty"`
}

// AdminAllocationCorrect serves POST .../{id}/correct — the
// offset/replacement correction. On settlement-locked groups the request
// either carries a distinct approver_id (synchronous four-eyes, still
// enforced by the service's role check) or is queued through the
// dual-control service (RequiredRole "Finance Ops", 15-minute window) —
// never a solo mutation.
func AdminAllocationCorrect(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, ok := allocPathID(w, r, "id")
		if !ok {
			return
		}
		var req correctRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		qty, err := decField("quantity", req.Quantity, false)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "quantity must be a decimal string",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		in := backoffice.CorrectInput{
			BeneficiaryAccountID: req.BeneficiaryAccountID,
			Quantity:             qty,
			Reason:               strings.TrimSpace(req.Reason),
		}
		if in.Reason == "" {
			WriteError(w, "INVALID_REQUEST", "correction requires a reason",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if req.ApproverID > 0 {
			// Inline four-eyes: the service enforces distinctness + role.
			c, err := deps.Backend.Correct(r.Context(), backoffice.Actor{
				UserID: actor, ApproverID: req.ApproverID,
				ClientIP: middleware.ClientIP(r, deps.TrustProxy),
			}, id, in)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{"correction": c})
			return
		}
		if deps.Dual == nil {
			WriteError(w, "DUAL_CONTROL_REQUIRED",
				"correction requires a second approver (approver_id) or the dual-control queue",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		dreq, err := deps.Dual.Submit(r.Context(), admin.SubmitInput{
			Operation:  OpAllocationCorrect,
			TargetType: "trade_allocation",
			TargetID:   strconv.FormatInt(id, 10),
			Payload: map[string]any{
				"allocation_id":          id,
				"beneficiary_account_id": req.BeneficiaryAccountID,
				"quantity":               qty.String(),
				"reason":                 in.Reason,
			},
			RequiredRole: admin.RoleFinanceOps,
			RequestedBy:  actor,
			Reason:       in.Reason,
			ClientIP:     middleware.ClientIP(r, deps.TrustProxy),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{
			"dual_control": "required", "request": dreq,
		})
	}
}

// RegisterAllocationCorrectExecutor attaches the four-eyes executor: on
// approval it applies the offset/replacement pair with both principals
// recorded on the immutable audit event. A failed correction aborts the
// approval transaction — the request stays PENDING rather than recording
// a correction that never landed (fail closed).
func RegisterAllocationCorrectExecutor(dual *admin.DualControlService, be allocationBackend) {
	if dual == nil || be == nil {
		return
	}
	dual.RegisterExecutor(OpAllocationCorrect,
		func(ctx context.Context, _ pgx.Tx, req *admin.DualControlRequest) error {
			var p struct {
				AllocationID         int64  `json:"allocation_id"`
				BeneficiaryAccountID int64  `json:"beneficiary_account_id"`
				Quantity             string `json:"quantity"`
				Reason               string `json:"reason"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return excerrors.New("INVALID_REQUEST",
					"allocation correction payload not decodable")
			}
			var approver int64
			if req.ApprovedBy != nil {
				approver = *req.ApprovedBy
			}
			qty, _ := decimal.NewFromString(p.Quantity)
			_, err := be.Correct(ctx, backoffice.Actor{
				UserID: req.RequestedBy, ApproverID: approver,
			}, p.AllocationID, backoffice.CorrectInput{
				BeneficiaryAccountID: p.BeneficiaryAccountID,
				Quantity:             qty,
				Reason:               p.Reason,
			})
			return err
		})
}

// AdminAllocationEscalate serves POST /api/v1/admin/allocations/escalate —
// the T+0 EOD sweep: groups still carrying unallocated filled quantity
// land durable P2 compliance alerts.
func AdminAllocationEscalate(deps AllocationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.ok(w, r) {
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		res, err := deps.Backend.EscalateUnallocated(r.Context(), time.Time{})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}
