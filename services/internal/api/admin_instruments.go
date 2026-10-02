// Phase-15 Task 15.3.2 — Admin Instrument API (spec §7.2).
//
// Mounts the instrument-lifecycle surface on the routes registered in
// Phase-05 (gateway/routes_v1.go — this cluster flips the rows live and
// corrects their §7.2 role gates):
//
//	GET  /api/v1/admin/instruments                    full list incl. non-ACTIVE
//	POST /api/v1/admin/instruments                    create → DRAFT — dual control
//	PUT  /api/v1/admin/instruments/{id}               parameter edit — Risk Manager+
//	POST /api/v1/admin/instruments/{id}/activate      DRAFT→ACTIVE — Risk Manager+
//	POST /api/v1/admin/instruments/{id}/restrict      →RESTRICTED — Risk Manager+
//	POST /api/v1/admin/instruments/{id}/cancel-only   →CANCEL_ONLY — RM|Compliance
//	POST /api/v1/admin/instruments/{id}/suspend       →SUSPENDED — Compliance Officer+
//	POST /api/v1/admin/instruments/{id}/halt          →HALTED — Risk Manager+
//	POST /api/v1/admin/instruments/{id}/resume        →ACTIVE (+reopening CALL) — dual control
//	POST /api/v1/admin/instruments/{id}/delist        →DELISTED — dual control
//
// Four-eyes ops (create/resume/delist per §7.2) return 202 with the
// PENDING dual-control request; a second approver's
// POST /api/v1/admin/dual-control/{id}/approve executes the mutation
// inside the approval transaction (executor registered by
// RegisterInstrumentExecutors — approval rolls back on failure and the
// request stays PENDING; engine-feed publication lands via the
// reconciler on the next tick).
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"exchange/internal/admin"
	"exchange/internal/gateway"

	"github.com/jackc/pgx/v5"
)

// decodeJSONBody unmarshals the request body JSON → INVALID_REQUEST on
// error. Bodies are optional for the lifecycle verbs.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		WriteError(w, "INVALID_REQUEST", "invalid JSON body",
			gateway.RequestIDFrom(r.Context()), nil)
		return false
	}
	return true
}

// decodeOptionalJSONBody treats io.EOF (empty body — including chunked
// transfers reporting ContentLength=-1) as a zero-value body.
func decodeOptionalJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(r.Body).Decode(v)
	if err == io.EOF {
		return true
	}
	if err != nil {
		WriteError(w, "INVALID_REQUEST", "invalid JSON body",
			gateway.RequestIDFrom(r.Context()), nil)
		return false
	}
	return true
}

func writePendingDual(w http.ResponseWriter, req *admin.DualControlRequest,
	extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	out := map[string]any{
		"status":            "PENDING",
		"dual_control_id":   req.ID,
		"operation":         req.Operation,
		"required_approver": req.RequiredRole,
		"expires_at":        req.ExpiresAt.Unix(),
	}
	for k, v := range extra {
		out[k] = v
	}
	_ = json.NewEncoder(w).Encode(out)
}

// AdminInstrumentsList — GET /api/v1/admin/instruments (admin-side list
// incl. DRAFT/control states; the public GET /api/v1/instruments stays
// ACTIVE-filtered).
func AdminInstrumentsList(svc *admin.InstrumentService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, true); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		list, err := svc.List(r.Context())
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"instruments": list})
	})
}

// AdminInstrumentCreate — POST /api/v1/admin/instruments. Super Admin
// (route gate) + §7.2 four-eyes: submits OpInstrumentCreate; the second
// approver's approval runs the DRAFT insert inside the approval tx.
func AdminInstrumentCreate(dual *admin.DualControlService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, true)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body admin.InstrumentCreate
		if !decodeJSONBody(w, r, &body) {
			return
		}
		payload := map[string]any{
			"symbol":              body.Symbol,
			"base_currency":       body.BaseCurrency,
			"quote_currency":      body.QuoteCurrency,
			"instrument_type":     body.InstrumentType,
			"tick_size":           body.TickSize,
			"lot_size":            body.LotSize,
			"min_order_qty":       body.MinOrderQty,
			"max_order_qty":       body.MaxOrderQty,
			"price_band_pct_up":   body.PriceBandPctUp,
			"price_band_pct_down": body.PriceBandPctDn,
			"settlement_cycle":    body.SettlementCycle,
			"max_leverage":        body.MaxLeverage,
			"reason":              body.Reason,
			"client_ip":           actor.ClientIP,
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpInstrumentCreate,
			TargetType:   "instrument",
			TargetID:     body.Symbol,
			RequiredRole: admin.RoleSuperAdmin,
			RequestedBy:  actor.UserID,
			Reason:       body.Reason,
			ClientIP:     actor.ClientIP,
			Payload:      payload,
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		writePendingDual(w, req, map[string]any{"instrument": body.Symbol})
	})
}

// AdminInstrumentUpdate — PUT /api/v1/admin/instruments/{id}
// (Task 15.3.2 parameter edits; Task 15.3.8 owns the effective-dated
// maker-checker parameter lifecycle).
func AdminInstrumentUpdate(svc *admin.InstrumentService, trustProxy bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body admin.InstrumentUpdate
		if !decodeJSONBody(w, r, &body) {
			return
		}
		inst, err := svc.Update(r.Context(), actor, id, body)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(inst)
	})
}

// AdminInstrumentTransition — the single-approver lifecycle verbs
// (activate/restrict/cancel-only/suspend/halt per §7.2). Route-mounted
// per-op so each route keeps its own role gate.
func AdminInstrumentTransition(svc *admin.InstrumentService, op string,
	trustProxy bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body admin.TransitionInput
		if !decodeOptionalJSONBody(w, r, &body) {
			return
		}
		inst, err := svc.Transition(r.Context(), actor, id, op, body)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(inst)
	})
}

// AdminInstrumentResume — POST .../resume. §7.2 four-eyes (Risk Manager+
// maker): submits OpInstrumentResume; the approval executes the
// →ACTIVE transition and, when a reopening CALL applies, arms the
// engine-side instrument:auction:{symbol} control key.
func AdminInstrumentResume(dual *admin.DualControlService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, true)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body admin.TransitionInput
		if !decodeOptionalJSONBody(w, r, &body) {
			return
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpInstrumentResume,
			TargetType:   "instrument",
			TargetID:     strconv.FormatInt(id, 10),
			RequiredRole: admin.RoleRiskManager,
			RequestedBy:  actor.UserID,
			Reason:       body.Reason,
			ClientIP:     actor.ClientIP,
			Payload: map[string]any{
				"instrument_id": id,
				"reason":        body.Reason,
				"skip_auction":  body.SkipAuction,
				"auction":       body.Auction,
				"client_ip":     actor.ClientIP,
			},
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		writePendingDual(w, req, map[string]any{"instrument_id": id})
	})
}

// AdminInstrumentUncrossOverride — POST .../uncross-override. Task 15.3.10
// quarantine release: the crossed-book quarantine resolves through the
// dual-controlled resume path with the reopening CALL re-armed — the
// re-armed CALL uncrosses the forensic residue at a single clearing price
// and clears quarantine only on a clean uncross (a residual crossing
// re-quarantines). This surface pins auction:true so the caller cannot
// accidentally resume straight into continuous trading with a crossed
// book.
func AdminInstrumentUncrossOverride(dual *admin.DualControlService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, true)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body admin.TransitionInput
		if !decodeOptionalJSONBody(w, r, &body) {
			return
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpInstrumentResume,
			TargetType:   "instrument",
			TargetID:     strconv.FormatInt(id, 10),
			RequiredRole: admin.RoleRiskManager,
			RequestedBy:  actor.UserID,
			Reason:       body.Reason,
			ClientIP:     actor.ClientIP,
			Payload: map[string]any{
				"instrument_id": id,
				"reason":        body.Reason,
				"skip_auction":  false,
				"auction":       true, // pinned — quarantine release must uncross
				"client_ip":     actor.ClientIP,
			},
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		writePendingDual(w, req, map[string]any{"instrument_id": id})
	})
}

// AdminInstrumentDelist — POST .../delist. §7.2 four-eyes (Super Admin).
func AdminInstrumentDelist(dual *admin.DualControlService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, true)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body admin.TransitionInput
		if !decodeOptionalJSONBody(w, r, &body) {
			return
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpInstrumentDelist,
			TargetType:   "instrument",
			TargetID:     strconv.FormatInt(id, 10),
			RequiredRole: admin.RoleSuperAdmin,
			RequestedBy:  actor.UserID,
			Reason:       body.Reason,
			ClientIP:     actor.ClientIP,
			Payload: map[string]any{
				"instrument_id": id,
				"reason":        body.Reason,
				"client_ip":     actor.ClientIP,
			},
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		writePendingDual(w, req, map[string]any{"instrument_id": id})
	})
}

// RegisterInstrumentExecutors wires the four-eyes executors
// (OpInstrumentCreate/Resume/Delist) — the approval transaction runs
// the mutation; any failure leaves the request PENDING for retry.
func RegisterInstrumentExecutors(dual *admin.DualControlService,
	svc *admin.InstrumentService) {

	dual.RegisterExecutor(admin.OpInstrumentCreate,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var p map[string]any
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return err
			}
			var body admin.InstrumentCreate
			if err := json.Unmarshal(req.Payload, &body); err != nil {
				return err
			}
			actor := admin.AdminActor{UserID: req.RequestedBy}
			if v, _ := p["client_ip"].(string); v != "" {
				actor.ClientIP = v
			}
			if req.ApprovedBy != nil {
				actor.ApproverID = *req.ApprovedBy
			}
			_, err := svc.CreateInTx(ctx, tx, actor, body)
			return err
		})

	register := func(opName, lifecycleOp string) {
		dual.RegisterExecutor(opName,
			func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
				var p struct {
					InstrumentID int64  `json:"instrument_id"`
					Reason       string `json:"reason"`
					SkipAuction  bool   `json:"skip_auction"`
					Auction      bool   `json:"auction"`
					ClientIP     string `json:"client_ip"`
				}
				if err := json.Unmarshal(req.Payload, &p); err != nil {
					return err
				}
				id := p.InstrumentID
				if id <= 0 {
					var err error
					id, err = strconv.ParseInt(req.TargetID, 10, 64)
					if err != nil {
						return err
					}
				}
				actor := admin.AdminActor{
					UserID:   req.RequestedBy,
					ClientIP: p.ClientIP,
				}
				if req.ApprovedBy != nil {
					actor.ApproverID = *req.ApprovedBy
				}
				return svc.TransitionTx(ctx, tx, actor, id, lifecycleOp,
					admin.TransitionInput{
						Reason:      p.Reason,
						SkipAuction: p.SkipAuction,
						Auction:     p.Auction,
					})
			})
	}
	register(admin.OpInstrumentResume, admin.LcOpResume)
	register(admin.OpInstrumentDelist, admin.LcOpDelist)
}
