// Phase-14 product-governance endpoints — Task 14.3.13 account product
// profiles, Task 14.3.15 swap-free verification lifecycle, Task 14.3.16
// retail target-market authoring/review.
//
//	POST /api/v1/account/swap-free/request                    — client request
//	                                                             (attestation ref)
//	POST /api/v1/admin/swap-free/{id}/approve                 — Compliance decision
//	POST /api/v1/admin/swap-free/{id}/reject                  — Compliance decision
//	POST /api/v1/admin/swap-free/{id}/revoke                  — revocation +
//	                                                             abuse guard
//	POST /api/v1/admin/product-profiles                       — create (dual-control)
//	PUT  /api/v1/admin/product-profiles                       — update pricing/scope
//	                                                             (dual-control)
//	GET  /api/v1/admin/product-profiles                       — list (?include_retired)
//	POST /api/v1/admin/accounts/{id}/product-profile          — assign profile
//	PUT  /api/v1/admin/product-profiles/{id}/target-market    — upsert target row
//	POST /api/v1/admin/product-target-markets/{id}/review     — APPROVE|NARROW|SUSPEND
//	GET  /api/v1/admin/product-target-markets                 — review queue (?overdue)
//
// Route note: the registered PUT /api/v1/admin/accounts/{id}/product-
// profile route is owned by Task 14.3.7 (client categorization) and is
// already live — profile assignment therefore mounts POST on the same
// path (the Task 14.3.13 plan text names the PUT before 14.3.7 landed;
// same surface, distinct method).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"exchange/internal/accounts"
	"exchange/internal/admin"
	"exchange/internal/gateway"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// POST /api/v1/account/swap-free/request — Task 14.3.15
// ---------------------------------------------------------------------------

type swapFreeRequestBody struct {
	AttestationRef string `json:"attestation_ref"`
}

// AccountSwapFreeRequest opens a PENDING swap-free verification for the
// caller's account. The attestation ref must resolve to a kyc_documents
// row owned by the account (service-enforced).
func AccountSwapFreeRequest(svc *accounts.SwapfreeService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "swap-free service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body swapFreeRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		v, err := svc.Request(r.Context(), accountID, body.AttestationRef)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, v)
	}
}

// AccountSwapFreeStatus is the client-facing mirror view (optional GET
// surface — swapfree_status + latest verification row).
func AccountSwapFreeStatus(svc *accounts.SwapfreeService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "swap-free service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		st, err := svc.Status(r.Context(), accountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, st)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/swap-free/{id}/approve|reject|revoke — Task 14.3.15
// ---------------------------------------------------------------------------

type swapFreeDecisionBody struct {
	Reason string `json:"reason"`
}

// accountsActor adapts the admin claims actor to the accounts package's
// AdminActor (the service resolves roles itself via the wired resolver).
func accountsActor(r *http.Request, trustProxy bool) (accounts.AdminActor, error) {
	actor, err := adminActorFrom(r, trustProxy)
	if err != nil {
		return accounts.AdminActor{}, err
	}
	return accounts.AdminActor{UserID: actor.UserID, ClientIP: actor.ClientIP}, nil
}

// AdminSwapFreeDecide returns the approve/reject/revoke handler for the
// registered swap-free decision routes ("approve"|"reject"|"revoke").
func AdminSwapFreeDecide(svc *accounts.SwapfreeService, decision string, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "swap-free service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, err := accountsActor(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body swapFreeDecisionBody
		// An empty body decodes fine for approve; reject/revoke validate
		// reason inside the service.
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body) // best-effort; service validates
		}
		var v *accounts.SwapfreeVerification
		switch decision {
		case "approve":
			v, err = svc.Approve(r.Context(), actor, id)
		case "reject":
			v, err = svc.Reject(r.Context(), actor, id, body.Reason)
		case "revoke":
			v, err = svc.Revoke(r.Context(), actor, id, body.Reason)
		default:
			err = excerrors.New("INVALID_REQUEST", "unknown decision "+decision)
		}
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, v)
	}
}

// ---------------------------------------------------------------------------
// POST/PUT /api/v1/admin/product-profiles — Task 14.3.13 (dual-control)
// ---------------------------------------------------------------------------

// AdminProductProfileSubmit is the maker side of profile create/update:
// both serialize the change into the dual-control queue; approval runs
// the executor registered by RegisterProductProfileExecutor inside the
// approval transaction.
func AdminProductProfileSubmit(dual *admin.DualControlService,
	action string, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dual == nil {
			WriteError(w, "SERVICE_DEGRADED", "dual-control queue unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			ProfileID int64                 `json:"profile_id"`
			Reason    string                `json:"reason"`
			Input     accounts.ProfileInput `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		targetID := "new"
		if action == "update" {
			if body.ProfileID <= 0 {
				WriteError(w, "INVALID_REQUEST", "profile_id required for update",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			targetID = strconv.FormatInt(body.ProfileID, 10)
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:  admin.OpProductProfileChange,
			TargetType: "account_product_profile",
			TargetID:   targetID,
			Payload: map[string]any{
				"action":     action,
				"profile_id": body.ProfileID,
				"input":      body.Input,
				"client_ip":  actor.ClientIP,
			},
			RequiredRole: admin.RoleComplianceOfficer,
			RequestedBy:  actor.UserID,
			Reason:       "product profile " + action + ": " + body.Reason,
			ClientIP:     actor.ClientIP,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{
			"request": req,
			"message": "dual-control request pending — approval applies the profile change",
		})
	}
}

// RegisterProductProfileExecutor attaches the four-eyes executor: the
// approver's decision decodes {action, profile_id, input, client_ip}
// and applies the profile mutation inside the approval transaction —
// the change, its admin_audit_log row and the four-eyes record commit
// or fail together (a failed apply aborts the approval, leaving the
// request PENDING for retry — fail closed).
func RegisterProductProfileExecutor(dual *admin.DualControlService,
	svc *accounts.ProfileService) {
	dual.RegisterExecutor(admin.OpProductProfileChange,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var p struct {
				Action    string                `json:"action"`
				ProfileID int64                 `json:"profile_id"`
				Input     accounts.ProfileInput `json:"input"`
				ClientIP  string                `json:"client_ip"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return excerrors.New("INVALID_REQUEST",
					"product-profile payload not decodable")
			}
			var err error
			switch p.Action {
			case "create":
				_, err = svc.ApplyCreateTx(ctx, tx, req.RequestedBy,
					p.Input, p.ClientIP)
			case "update":
				_, err = svc.ApplyUpdateTx(ctx, tx, req.RequestedBy,
					p.ProfileID, p.Input, p.ClientIP)
			default:
				err = excerrors.New("INVALID_REQUEST",
					"product-profile action must be create|update")
			}
			return err
		})
}

// AdminProductProfileList serves the admin profile catalogue.
func AdminProductProfileList(svc *accounts.ProfileService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "profile service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		list, err := svc.ListProfiles(r.Context(),
			r.URL.Query().Get("include_retired") == "true")
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"profiles": list})
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/accounts/{id}/product-profile — Task 14.3.13 assign
// ---------------------------------------------------------------------------

type assignProfileBody struct {
	ProfileCode string `json:"profile_code"`
}

// AdminAssignProductProfile is the single-approver assignment route.
// The service enforces: ACTIVE target only (retired profiles
// grandfather existing holders), no open exposure (positions / resting
// orders / pending settlements — the Task 14.3.9 closure read path),
// and zero balances on divisor-changing switches.
func AdminAssignProductProfile(svc *accounts.ProfileService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "profile service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, err := accountsActor(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		accountID, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body assignProfileBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := svc.AssignProfile(r.Context(), actor, accountID, body.ProfileCode)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, p)
	}
}

// ---------------------------------------------------------------------------
// Target-market administration — Task 14.3.16
// ---------------------------------------------------------------------------

// AdminTargetMarketUpsert defines/refreshes the (profile, category)
// target market — Compliance-Officer single approver, audit-logged.
// profile_id binds to the {id} path segment.
func AdminTargetMarketUpsert(svc *accounts.TargetMarketService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "target-market service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, err := accountsActor(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		profileID, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body accounts.TargetMarketInput
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		body.ProfileID = profileID
		tm, err := svc.Upsert(r.Context(), actor, body)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, tm)
	}
}

// AdminTargetMarketReview runs the periodic-review dispositions:
// {action: APPROVE|NARROW|SUSPEND, input?} — NARROW carries the narrowed
// market payload; every action is audit-logged.
func AdminTargetMarketReview(svc *accounts.TargetMarketService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "target-market service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, err := accountsActor(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Action string                      `json:"action"`
			Input  *accounts.TargetMarketInput `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		tm, err := svc.Review(r.Context(), actor, id, body.Action, body.Input)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, tm)
	}
}

// AdminTargetMarketList serves the review queue; ?overdue=true limits
// to rows needing review now.
func AdminTargetMarketList(svc *accounts.TargetMarketService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "target-market service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, err := accountsActor(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		list, err := svc.List(r.Context(), actor,
			r.URL.Query().Get("overdue") == "true")
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"target_markets": list})
	}
}
