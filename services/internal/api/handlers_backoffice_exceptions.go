// Phase-24 backoffice admin surface — Tasks 24.3.6/24.3.7.
//
//   POST /api/v1/admin/settlement-exceptions/{id}/resolve
//       Finance Ops + dual control: submits the resolution onto the §8.2
//       four-eyes queue (the mutation lands in the approval tx via the
//       dual-control executor — never directly from this handler).
//   GET /api/v1/admin/pb-reconciliation?pb_id=&date=
//       Read-Only Auditor: PB give-up reconciliation report.
//
// Route rows already exist in the gateway seed table (both flagged
// DualControl / adminAuth); this file lands the handler constructors the
// cmd/gateway wiring binds via router.Handle.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/internal/backoffice"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Dual-queue adapter + executor registration — the backoffice package keeps
// a narrow seam (backoffice.DualQueue / BackofficeTx helpers) so it never
// imports internal/admin; this file is the structural glue (same role as
// RegisterRoleChangeExecutor for admin role changes).
// ---------------------------------------------------------------------------

// backofficeDualQueue adapts admin.DualControlService to the backoffice
// DualQueue seam — DualSubmit mirrors SubmitInput field-for-field.
type backofficeDualQueue struct{ dual *admin.DualControlService }

// NewBackofficeDualQueue binds the Phase-07 four-eyes queue for the
// settlement-exception / write-off / PB-break ops.
func NewBackofficeDualQueue(dual *admin.DualControlService) backoffice.DualQueue {
	if dual == nil {
		return nil
	}
	return backofficeDualQueue{dual: dual}
}

// Submit implements backoffice.DualQueue.
func (a backofficeDualQueue) Submit(ctx context.Context, in backoffice.DualSubmit) (*backoffice.DualResult, error) {
	req, err := a.dual.Submit(ctx, admin.SubmitInput{
		Operation:    in.Operation,
		TargetType:   in.TargetType,
		TargetID:     in.TargetID,
		Payload:      in.Payload,
		RequiredRole: in.RequiredRole,
		RequestedBy:  in.RequestedBy,
		Reason:       in.Reason,
		ClientIP:     in.ClientIP,
	})
	if err != nil {
		return nil, err
	}
	return &backoffice.DualResult{ID: req.ID, Operation: req.Operation,
		RequiredRole: req.RequiredRole, Status: req.Status, ExpiresAt: req.ExpiresAt}, nil
}

// approverOf resolves the checker principal from a decided request.
func approverOf(req *admin.DualControlRequest) int64 {
	if req.ApprovedBy != nil {
		return *req.ApprovedBy
	}
	return 0
}

// RegisterSettlementExceptionExecutor runs an approved exception
// resolution inside the approval transaction — the leg mutation, balance
// adjustments, reversal journal and the four-eyes record commit or fail
// together (spec §8.2 + Task 24.3.6).
func RegisterSettlementExceptionExecutor(dual *admin.DualControlService,
	svc *backoffice.ExceptionService) {
	dual.RegisterExecutor(backoffice.OpSettlementExceptionResolve,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var p backoffice.ResolutionPayload
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return excerrors.New("INVALID_REQUEST", "dual-control payload corrupt")
			}
			return svc.ApplyResolution(ctx, backoffice.TxFromPgx(tx),
				p.ExceptionID, p.Action, p.RequestedBy, approverOf(req), req.ID, p.Notes)
		})
}

// RegisterSettlementWriteOffExecutor executes an approved write-off inside
// the approval tx — balanced GL journal + exception/break status land
// atomically with the four-eyes record (Task 24.3.19 §17.14.1).
func RegisterSettlementWriteOffExecutor(dual *admin.DualControlService,
	svc *backoffice.OpsService) {
	dual.RegisterExecutor(backoffice.OpSettlementWriteOff,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var w backoffice.WriteOff
			if err := json.Unmarshal(req.Payload, &w); err != nil {
				return excerrors.New("INVALID_REQUEST", "dual-control payload corrupt")
			}
			if w.ID == 0 {
				id, err := strconv.ParseInt(req.TargetID, 10, 64)
				if err != nil {
					return excerrors.New("INVALID_REQUEST", "write-off target corrupt")
				}
				w.ID = id
			}
			return svc.ApplyWriteOff(ctx, backoffice.OpsTxFromPgx(tx),
				w.ID, approverOf(req), req.ID)
		})
}

// RegisterPBBreakResolveExecutor applies an approved PB recon break
// disposition inside the approval tx (Task 24.3.7).
func RegisterPBBreakResolveExecutor(dual *admin.DualControlService,
	svc *backoffice.PBReconService) {
	dual.RegisterExecutor(backoffice.OpPBBreakResolve,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var p backoffice.BreakResolutionPayload
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return excerrors.New("INVALID_REQUEST", "dual-control payload corrupt")
			}
			return svc.ResolveBreak(ctx, backoffice.ReconTxFromPgx(tx),
				p.BreakID, p.Disposition, p.Note, p.ResolverID)
		})
}

// ---------------------------------------------------------------------------
// settlement-exceptions — Task 24.3.6
// ---------------------------------------------------------------------------

// exceptionResolver is the seam (*backoffice.ExceptionService).
type exceptionResolver interface {
	RequestResolution(ctx context.Context, exceptionID int64,
		action backoffice.ResolutionAction, notes string,
		makerID int64, clientIP string) (*backoffice.DualResult, error)
	Get(ctx context.Context, id int64) (*backoffice.SettlementException, error)
}

type exceptionResolveRequest struct {
	Action string `json:"action"` // RETRY | REVERSE | MANUAL
	Notes  string `json:"notes,omitempty"`
}

// AdminSettlementExceptionResolve serves
// POST /api/v1/admin/settlement-exceptions/{id}/resolve — dual-control
// submit only (four-eyes execution rides the approval executor).
func AdminSettlementExceptionResolve(svc exceptionResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "settlement-exception service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		exceptionID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || exceptionID <= 0 {
			WriteError(w, "INVALID_REQUEST", "exception id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req exceptionResolveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		action := backoffice.ResolutionAction(strings.ToUpper(strings.TrimSpace(req.Action)))
		res, err := svc.RequestResolution(r.Context(), exceptionID, action,
			req.Notes, actor, middleware.ClientIP(r, true))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{
			"exception_id":         exceptionID,
			"action":               string(action),
			"dual_control_request": res,
			"status":               res.Status,
		})
	}
}

// AdminSettlementExceptionGet serves the exception read view (same auth
// surface as the resolve route's audit trail).
func AdminSettlementExceptionGet(svc exceptionResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "settlement-exception service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		exceptionID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || exceptionID <= 0 {
			WriteError(w, "INVALID_REQUEST", "exception id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ex, err := svc.Get(r.Context(), exceptionID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"exception": ex})
	}
}

// ---------------------------------------------------------------------------
// pb-reconciliation — Task 24.3.7
// ---------------------------------------------------------------------------

// pbReconReporter is the seam (*backoffice.PBReconService).
type pbReconReporter interface {
	Report(ctx context.Context, pbID int64, day time.Time) (*backoffice.PBReconReport, error)
}

// AdminPBReconciliation serves GET /api/v1/admin/pb-reconciliation —
// query params pb_id (optional; 0 = all PBs) and date (YYYY-MM-DD,
// default today UTC).
func AdminPBReconciliation(svc pbReconReporter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "pb reconciliation service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		q := r.URL.Query()
		var pbID int64
		if raw := strings.TrimSpace(q.Get("pb_id")); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id < 0 {
				WriteError(w, "INVALID_REQUEST", "pb_id must be a non-negative integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			pbID = id
		}
		day := time.Now().UTC().Truncate(24 * time.Hour)
		if raw := strings.TrimSpace(q.Get("date")); raw != "" {
			d, err := time.Parse("2006-01-02", raw)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "date must be YYYY-MM-DD",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			day = d
		}
		rep, err := svc.Report(r.Context(), pbID, day)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, rep)
	}
}
