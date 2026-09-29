// handlers_market_schedule.go — Phase-15 Task 15.3.4 admin surface for
// the market_schedule_overrides store (migration 221).
//
//	GET    /api/v1/admin/market-schedule                    merged doc + overrides
//	GET    /api/v1/admin/market-schedule/overrides          override list
//	POST   /api/v1/admin/market-schedule/overrides          {date, closed, open?, close?, reason}
//	PUT    /api/v1/admin/market-schedule/overrides/{id}     {closed, open?, close?, reason}
//	DELETE /api/v1/admin/market-schedule/overrides/{id}
//
// Every mutation commits PG + the admin_audit_log/hash-chain row in one
// tx, then republishes the market:hours Redis projection the C++
// PreTradeChecker consumes. Role gate lives in the service
// (Risk Manager / Super Admin write; any venue admin reads) — the route
// registry rows carry the same declarations for the RBAC wrap.
package api

import (
	"encoding/json"
	"net/http"

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// AdminMarketSchedule serves GET /api/v1/admin/market-schedule — the
// merged schedule document (canonical window + effective overrides) as
// it is projected to market:hours.
func AdminMarketSchedule(svc *admin.MarketScheduleService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		doc, err := svc.Current(r.Context(), actor)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, doc)
	}
}

// AdminScheduleOverrideList serves GET .../overrides — the full table
// including expired rows (audit view).
func AdminScheduleOverrideList(svc *admin.MarketScheduleService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		ovs, err := svc.ListOverrides(r.Context(), actor)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"overrides": ovs})
	}
}

// AdminScheduleOverrideCreate serves POST .../overrides.
func AdminScheduleOverrideCreate(svc *admin.MarketScheduleService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var in admin.OverrideInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "malformed JSON body"))
			return
		}
		o, err := svc.CreateOverride(r.Context(), actor, in)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, o)
	}
}

// AdminScheduleOverrideUpdate serves PUT .../overrides/{id} — date is
// immutable; the body carries the new window + mandatory reason.
func AdminScheduleOverrideUpdate(svc *admin.MarketScheduleService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		var in admin.OverrideInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "malformed JSON body"))
			return
		}
		o, err := svc.UpdateOverride(r.Context(), actor, id, in)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, o)
	}
}

// AdminScheduleOverrideDelete serves DELETE .../overrides/{id}.
func AdminScheduleOverrideDelete(svc *admin.MarketScheduleService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		if err := svc.DeleteOverride(r.Context(), actor, id); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
	}
}
