// Phase-15 Task 15.3.5 — trade bust / price-adjust REST surface.
//
//	POST /api/v1/admin/trades/{id}/bust   (routes_v1.go, Risk Manager gate)
//
// Body: {"action":"BUST"|"PRICE_ADJUST", "adjusted_price":"1.00",
//
//	     "reason":"...", "approver_id":N, "reference_price":"1.00"}
//
//	- approver_id present → §8.2 synchronous "two principals, one
//	  request" execution (200 with the executed outcome).
//	- approver_id absent  → the §5.29 review row lands PENDING_APPROVAL
//	  (202; a distinct Risk Manager+ decides it through the admin
//	  surface — the queued-review decision endpoint rides the same
//	  service; ApproveBust/RejectBust are the service-level contract).
//
// The instrument-maintenance maker-checker chain (Task 15.3.8) exposes
// no new REST verbs here: the route registry owns endpoint registration
// (Phase-05 Task 5.3.7) and no instrument-change routes are registered.
// The workflow is reachable programmatically through
// admin.InstrumentMaintenanceService; the existing PUT/delist routes
// remain the live instrument surface.
package api

import (
	"encoding/json"
	"net/http"

	"exchange/internal/admin"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
)

// AdminTradeBust — POST /api/v1/admin/trades/{id}/bust.
func AdminTradeBust(svc *admin.TradeBustService) http.Handler {
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
		var body struct {
			Action         string `json:"action"`
			AdjustedPrice  string `json:"adjusted_price"`
			Reason         string `json:"reason"`
			ApproverID     int64  `json:"approver_id"`
			ReferencePrice string `json:"reference_price"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		in := admin.BustRequest{
			TradeID: id, Action: body.Action,
			Reason: body.Reason, ApproverID: body.ApproverID,
		}
		if body.AdjustedPrice != "" {
			d, err := decimal.NewFromString(body.AdjustedPrice)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "adjusted_price must be a decimal",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			in.AdjustedPrice = d
		}
		if body.ReferencePrice != "" {
			d, err := decimal.NewFromString(body.ReferencePrice)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "reference_price must be a decimal",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			in.ReferencePrice = &d
		}
		out, err := svc.RequestBust(r.Context(), actor, in)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if out.Bust.Status == admin.BustStatusPending {
			w.WriteHeader(http.StatusAccepted)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}
