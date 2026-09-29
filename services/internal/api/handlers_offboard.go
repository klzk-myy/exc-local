// Phase-14 account lifecycle endpoints — Task 14.3.9 closure &
// offboarding, Task 14.3.10 compliance holds, Task 14.3.11 cooling-off,
// Task 14.3.12 webhook dead-letter admin.
//
//	POST /api/v1/account/close                              — client-initiated
//	                                                          closure (RequireTwoFactor-
//	                                                          wrapped at mount)
//	POST /api/v1/account/cooling-off                        — activate irrevocable
//	                                                          self-exclusion
//	POST /api/v1/admin/accounts/{id}/close                  — forced closure maker
//	                                                          (dual-control queue;
//	                                                          approval via the existing
//	                                                          /admin/dual-control/* routes)
//	POST /api/v1/admin/compliance/holds                     — place hold (manual trigger)
//	GET  /api/v1/admin/compliance/holds                     — officer review list
//	POST /api/v1/admin/compliance/holds/{id}/release        — release (4-eyes approver_id)
//	POST /api/v1/admin/compliance/holds/{id}/escalate       — disposition sar|closure
//	GET  /api/v1/admin/webhooks/dead-letters                — dead-letter review
//	POST /api/v1/admin/webhooks/dead-letters/{id}/retransmit — manual retransmit
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"exchange/internal/accounts"
	"exchange/internal/admin"
	"exchange/internal/compliance"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/webhooks"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// POST /api/v1/account/close — Task 14.3.9 client path
// ---------------------------------------------------------------------------

// AccountClose serves the client-initiated closure. The mount wraps it
// in auth.RequireTwoFactor() — the session-AMR elevation IS the 2FA
// gate the task requires; the service re-checks ownership and every
// blocking precondition.
func AccountClose(svc *accounts.ClosureService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		userID, err := parseSubjectID(claims.Subject)
		if err != nil || userID <= 0 {
			WriteError(w, "UNAUTHORIZED", "user identity unresolvable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Reason       string            `json:"reason"`
			Destinations map[string]string `json:"destinations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Close(r.Context(), accounts.ClosureRequest{
			AccountID:    accountID,
			UserID:       userID,
			Reason:       body.Reason,
			Destinations: body.Destinations,
			Actor:        userID,
			IP:           middleware.ClientIP(r, trustProxy),
			RequestID:    gateway.RequestIDFrom(r.Context()),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/accounts/{id}/close — Task 14.3.9 forced path maker
// ---------------------------------------------------------------------------

// AdminAccountClose submits the forced-closure dual-control request.
// The approver confirms through the existing /admin/dual-control/{id}/
// approve route; the executor registered at startup runs the same
// offboarding pipeline inside the approval transaction.
func AdminAccountClose(dual *admin.DualControlService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		accountID, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.Reason == "" {
			WriteError(w, "INVALID_REQUEST",
				"mandatory reason required for forced closure",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpAccountClosure,
			TargetType:   "account",
			TargetID:     strconv.FormatInt(accountID, 10),
			Payload:      map[string]any{"account_id": accountID, "reason": body.Reason},
			RequiredRole: admin.RoleComplianceOfficer,
			RequestedBy:  actor.UserID,
			Reason:       "forced account closure: " + body.Reason,
			ClientIP:     actor.ClientIP,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{
			"request": req,
			"message": "dual-control request pending — approval executes the closure",
		})
	}
}

// RegisterAccountClosureExecutor attaches the four-eyes executor for
// forced closures: the second approver's approval decodes
// {account_id, reason} and runs the full offboarding pipeline inside
// the approval transaction — status flip, durable closure row, audit
// record and the approval commit atomically. A pipeline failure
// (residual positions the dispatcher could not flatten, a failed
// sweep) aborts the approval so the request stays PENDING for retry
// rather than recording a closure that never completed (fail closed).
func RegisterAccountClosureExecutor(dual *admin.DualControlService,
	svc *accounts.ClosureService) {
	dual.RegisterExecutor(admin.OpAccountClosure,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var p struct {
				AccountID int64  `json:"account_id"`
				Reason    string `json:"reason"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil || p.AccountID == 0 {
				return excerrors.New("INVALID_REQUEST",
					"account-closure payload not decodable")
			}
			var approver int64
			if req.ApprovedBy != nil {
				approver = *req.ApprovedBy
			}
			_, err := svc.ForcedClose(ctx, tx, accounts.ClosureRequest{
				AccountID:  p.AccountID,
				Reason:     p.Reason,
				Forced:     true,
				Actor:      req.RequestedBy,
				ApproverID: approver,
				RequestRef: req.ID,
			})
			return err
		})
}

// ---------------------------------------------------------------------------
// POST /api/v1/account/cooling-off — Task 14.3.11
// ---------------------------------------------------------------------------

// AccountCoolingOff activates the irrevocable self-exclusion window.
// duration ∈ {1d,3d,7d,30d} and acknowledged:true are mandatory —
// anything else is INVALID_REQUEST. The response carries the period
// plus the de-risking saga result (cancels/closes/partial_failure).
func AccountCoolingOff(svc *accounts.CoolingOffService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		userID, err := parseSubjectID(claims.Subject)
		if err != nil || userID <= 0 {
			WriteError(w, "UNAUTHORIZED", "user identity unresolvable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Duration     string `json:"duration"`
			Acknowledged bool   `json:"acknowledged"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		period, res, err := svc.Activate(r.Context(), accounts.CoolingOffRequest{
			AccountID:    accountID,
			Duration:     body.Duration,
			Acknowledged: body.Acknowledged,
			Actor:        claims.Subject,
			IP:           middleware.ClientIP(r, trustProxy),
			RequestID:    gateway.RequestIDFrom(r.Context()),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{
			"period": period,
			"result": res,
			"notice": "cooling-off is irrevocable — it cannot be cancelled or shortened",
		})
	}
}

// ---------------------------------------------------------------------------
// Compliance holds — Task 14.3.10
// ---------------------------------------------------------------------------

// AdminHoldPlace serves POST /api/v1/admin/compliance/holds — the
// manual Compliance Officer placement. (Phase-21 sanctions/PEP callers
// use compliance.HoldService.PlaceHold directly — the same seam.)
func AdminHoldPlace(svc *compliance.HoldService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			AccountID      int64  `json:"account_id"`
			Trigger        string `json:"trigger"`
			Reason         string `json:"reason"`
			EvidenceRef    string `json:"evidence_ref"`
			SLAHours       int    `json:"sla_hours"`
			HighConfidence bool   `json:"high_confidence"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		trigger := body.Trigger
		if trigger == "" {
			trigger = compliance.HoldTriggerManual
		}
		h, err := svc.PlaceHold(r.Context(), compliance.PlaceHoldRequest{
			AccountID:      body.AccountID,
			Trigger:        trigger,
			Reason:         body.Reason,
			EvidenceRef:    body.EvidenceRef,
			SLAHours:       body.SLAHours,
			HighConfidence: body.HighConfidence,
			PlacedBy:       actor.UserID,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"hold": h})
	}
}

// AdminHoldList serves GET /api/v1/admin/compliance/holds — the frozen-
// account officer review view with reason/evidence/SLA timeline.
func AdminHoldList(svc *compliance.HoldService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		holds, err := svc.ListHolds(r.Context(),
			r.URL.Query().Get("status"), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if holds == nil {
			holds = []compliance.Hold{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"holds": holds})
	}
}

// AdminHoldRelease serves POST /api/v1/admin/compliance/holds/{id}/
// release — clears a false-positive hold (four-eyes: distinct
// approver_id mandatory).
func AdminHoldRelease(svc *compliance.HoldService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			ApproverID int64  `json:"approver_id"`
			Reason     string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		h, err := svc.Release(r.Context(), r.PathValue("id"),
			actor.UserID, body.ApproverID, body.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"hold": h})
	}
}

// AdminHoldEscalate serves POST /api/v1/admin/compliance/holds/{id}/
// escalate — disposition "sar" records the escalation (Phase-21 Task
// 21.3.3 files the actual SAR); "closure" submits the forced-closure
// dual-control request.
func AdminHoldEscalate(svc *compliance.HoldService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Disposition string `json:"disposition"` // "sar" | "closure"
			Reason      string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		switch body.Disposition {
		case "sar":
			h, err := svc.EscalateSAR(r.Context(), r.PathValue("id"),
				actor.UserID, body.Reason)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{"hold": h})
		case "closure":
			h, reqID, err := svc.EscalateToClosure(r.Context(),
				r.PathValue("id"), actor.UserID, body.Reason)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{
				"hold":                    h,
				"dual_control_request_id": reqID,
				"message":                 "closure request pending — approval executes the forced-closure pipeline",
			})
		default:
			WriteError(w, "INVALID_REQUEST",
				"disposition must be 'sar' or 'closure'",
				gateway.RequestIDFrom(r.Context()), nil)
		}
	}
}

// ---------------------------------------------------------------------------
// Webhook dead-letters — Task 14.3.12 admin review + manual retransmit
// ---------------------------------------------------------------------------

// AdminWebhookDeadLetters serves GET /api/v1/admin/webhooks/dead-letters.
func AdminWebhookDeadLetters(store *webhooks.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		ds, err := store.ListDeadLetters(r.Context(), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if ds == nil {
			ds = []webhooks.Delivery{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"dead_letters": ds})
	}
}

// AdminWebhookRetransmit serves POST /api/v1/admin/webhooks/
// dead-letters/{id}/retransmit — the delivery requeues PENDING with a
// fresh attempt budget; the store writes admin_audit_log in the same
// transaction.
func AdminWebhookRetransmit(store *webhooks.Store, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		d, err := store.Retransmit(r.Context(), r.PathValue("id"), actor.UserID)
		if err != nil {
			switch {
			case errors.Is(err, webhooks.ErrDeliveryNotFound):
				WriteError(w, "NOT_FOUND", "dead-letter delivery not found",
					gateway.RequestIDFrom(r.Context()), nil)
			default:
				writeServiceErr(w, r, err)
			}
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"delivery": d})
	}
}
