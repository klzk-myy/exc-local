// Task 16.3.21 — recurring conversion / rebalancing / strategy
// marketplace REST handlers.
//
//	POST   /api/v1/strategies                          create strategy
//	GET    /api/v1/strategies                          list account strategies
//	GET    /api/v1/strategies/{id}                     detail + recent runs
//	POST   /api/v1/strategies/{id}/pause               pause scheduling
//	POST   /api/v1/strategies/{id}/resume              resume scheduling
//	DELETE /api/v1/strategies/{id}                     cancel + unwind open run
//	GET    /api/v1/strategy-templates                  approved marketplace list
//	POST   /api/v1/strategy-templates                  publish (config only)
//	POST   /api/v1/strategy-templates/{id}/instantiate copy config → strategy
//	GET    /api/v1/admin/strategy-templates            review queue (all states)
//	POST   /api/v1/admin/strategy-templates/{id}/approve
//	POST   /api/v1/admin/strategy-templates/{id}/reject
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/strategies"
)

// StrategyDeps bundles the service with the order-auth seam.
type StrategyDeps struct {
	SVC        *strategies.Service
	Orders     *OrderDeps
	TrustProxy bool
}

func (d *StrategyDeps) tradeAuth(w http.ResponseWriter, r *http.Request,
	body []byte) (int64, bool) {

	_, acct, ok := d.Orders.orderAuth(w, r, body, "trade")
	if !ok {
		return 0, false
	}
	return acct.ID, true
}

func (d *StrategyDeps) readAuth(w http.ResponseWriter, r *http.Request) (int64, bool) {
	_, acct, ok := d.Orders.orderAuth(w, r, nil, "read")
	if !ok {
		return 0, false
	}
	return acct.ID, true
}

// StrategyCreate serves POST /api/v1/strategies.
func StrategyCreate(d *StrategyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body too large",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		acctID, ok := d.tradeAuth(w, r, body)
		if !ok {
			return
		}
		var in strategies.CreateInput
		if err := json.Unmarshal(body, &in); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		st, err := d.SVC.Create(r.Context(), acctID, in)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, st)
	}
}

// StrategyList serves GET /api/v1/strategies.
func StrategyList(d *StrategyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acctID, ok := d.readAuth(w, r)
		if !ok {
			return
		}
		list, err := d.SVC.List(r.Context(), acctID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"strategies": list})
	}
}

// StrategyGet serves GET /api/v1/strategies/{id}.
func StrategyGet(d *StrategyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acctID, ok := d.readAuth(w, r)
		if !ok {
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		det, err := d.SVC.Detail(r.Context(), acctID, id)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, det)
	}
}

// StrategyPause serves POST /api/v1/strategies/{id}/pause.
func StrategyPause(d *StrategyDeps) http.HandlerFunc {
	return d.transition(func(svc *strategies.Service, ctx context.Context,
		acctID, id int64) (*strategies.Strategy, error) {
		return svc.Pause(ctx, acctID, id)
	})
}

// StrategyResume serves POST /api/v1/strategies/{id}/resume.
func StrategyResume(d *StrategyDeps) http.HandlerFunc {
	return d.transition(func(svc *strategies.Service, ctx context.Context,
		acctID, id int64) (*strategies.Strategy, error) {
		return svc.Resume(ctx, acctID, id)
	})
}

// StrategyCancel serves DELETE /api/v1/strategies/{id}.
func StrategyCancel(d *StrategyDeps) http.HandlerFunc {
	return d.transition(func(svc *strategies.Service, ctx context.Context,
		acctID, id int64) (*strategies.Strategy, error) {
		return svc.Cancel(ctx, acctID, id)
	})
}

func (d *StrategyDeps) transition(
	fn func(*strategies.Service, context.Context, int64, int64) (*strategies.Strategy, error),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acctID, ok := d.tradeAuth(w, r, nil)
		if !ok {
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		st, err := fn(d.SVC, r.Context(), acctID, id)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, st)
	}
}

// StrategyTemplates serves GET /api/v1/strategy-templates — the approved
// marketplace catalog only.
func StrategyTemplates(d *StrategyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := d.readAuth(w, r); !ok {
			return
		}
		list, err := d.SVC.ListTemplates(r.Context(), strategies.TemplateApproved)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"templates": list})
	}
}

// StrategyTemplatePublish serves POST /api/v1/strategy-templates —
// publishes a PENDING_APPROVAL template (configuration JSONB only).
func StrategyTemplatePublish(d *StrategyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "request body too large",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		acctID, ok := d.tradeAuth(w, r, body)
		if !ok {
			return
		}
		var in strategies.TemplatePublishInput
		if err := json.Unmarshal(body, &in); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, err := d.SVC.PublishTemplate(r.Context(), acctID, in)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, t)
	}
}

// StrategyTemplateInstantiate serves
// POST /api/v1/strategy-templates/{id}/instantiate — copies an APPROVED
// template's config into a new account-owned strategy.
func StrategyTemplateInstantiate(d *StrategyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acctID, ok := d.tradeAuth(w, r, nil)
		if !ok {
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		st, err := d.SVC.Instantiate(r.Context(), acctID, id)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, st)
	}
}

// AdminStrategyTemplates serves GET /api/v1/admin/strategy-templates —
// the review queue across all states.
func AdminStrategyTemplates(d *StrategyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, d.TrustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		list, err := d.SVC.ListTemplates(r.Context(),
			r.URL.Query().Get("status"))
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"templates": list})
	}
}

// AdminStrategyTemplateDecide serves approve/reject — Compliance Officer
// route gate is enforced by the route registry; the decision writes its
// admin_audit_log row inside the mutation transaction.
func AdminStrategyTemplateDecide(d *StrategyDeps, approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, d.TrustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // optional
		t, err := d.SVC.DecideTemplate(r.Context(), id, approve,
			actor.UserID, middleware.ClientIP(r, d.TrustProxy), body.Reason)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, t)
	}
}
