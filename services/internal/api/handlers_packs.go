// Tasks 7.3.13/7.3.14 — governance pack admin REST surface.
//
//	GET  /api/v1/admin/governance-packs?kind=&limit=
//	GET  /api/v1/admin/governance-packs/{id}
//	POST /api/v1/admin/governance-packs/generate
//	POST /api/v1/admin/governance-packs/{id}/release   (dual control)
//
// Guarantees:
//   - Reads resolve the caller's role through the AdminRoleResolver seam;
//     auditor roles see RELEASED board packs plus CEO roll-ups only
//     (spec §7.6 item 3).
//   - generate is the on-demand rebuild path (same hash rule as the
//     06:00 job); gated Super Admin.
//   - release is maker-checker: the request carries a distinct
//     approver_id who must themselves resolve to an eligible role; the
//     released row is then immutable (schema trigger).
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/admin"
	"exchange/internal/gateway"

	excerrors "exchange/pkg/errors"
)

// PackGenerateRequest is the generate body. Period shapes by kind:
// CEO_DAILY {date:"YYYY-MM-DD"} (empty date → prior UTC day);
// BOARD_QUARTERLY {year, quarter}; BOARD_ADHOC {label}.
type PackGenerateRequest struct {
	Kind    string `json:"kind"`
	Date    string `json:"date"`
	Year    int    `json:"year"`
	Quarter int    `json:"quarter"`
	Label   string `json:"label"`
}

// PackReleaseRequest is the maker-checker release body.
type PackReleaseRequest struct {
	ApproverID json.RawMessage `json:"approver_id"`
	Reason     string          `json:"reason"`
}

// AdminPackList serves GET /api/v1/admin/governance-packs?kind=&limit=.
func AdminPackList(svc *admin.GovernancePackService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		limit := 0
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if v, perr := strconv.Atoi(raw); perr == nil {
				limit = v
			}
		}
		packs, err := svc.List(r.Context(), actor, r.URL.Query().Get("kind"), limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"governance_packs": packs})
	}
}

// AdminPackGet serves GET /api/v1/admin/governance-packs/{id}; the
// response includes hash_ok = VerifyHash(pack) so callers can confirm the
// stored snapshot is unmodified.
func AdminPackGet(svc *admin.GovernancePackService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		packID, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		p, err := svc.Get(r.Context(), actor, packID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"pack": p, "hash_ok": admin.VerifyHash(p),
		})
	}
}

// AdminPackGenerate serves POST /api/v1/admin/governance-packs/generate.
func AdminPackGenerate(svc *admin.GovernancePackService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var req PackGenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		period, err := packPeriodFromRequest(req)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		p, err := svc.Generate(r.Context(), actor, strings.ToUpper(req.Kind), period)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, p)
	}
}

// AdminPackRelease serves POST /api/v1/admin/governance-packs/{id}/release
// — maker-checker: approver_id must be a distinct, eligible admin.
func AdminPackRelease(svc *admin.GovernancePackService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		packID, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var req PackReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		approver, present, err := parseFlexID(req.ApproverID, "approver_id")
		if err != nil {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", err.Error()))
			return
		}
		if present {
			actor.ApproverID = approver
		}
		p, err := svc.Release(r.Context(), actor, packID, req.Reason)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, p)
	}
}

// packPeriodFromRequest maps the kind-specific request fields to a
// PackPeriod.
func packPeriodFromRequest(req PackGenerateRequest) (admin.PackPeriod, error) {
	switch strings.ToUpper(req.Kind) {
	case admin.PackKindCEODaily:
		if req.Date == "" {
			return admin.DailyPeriod(time.Now().UTC().AddDate(0, 0, -1)), nil
		}
		d, err := time.Parse("2006-01-02", req.Date)
		if err != nil {
			return admin.PackPeriod{}, excerrors.New("INVALID_REQUEST",
				"date must be YYYY-MM-DD")
		}
		return admin.DailyPeriod(d), nil
	case admin.PackKindBoardQuarterly:
		return admin.QuarterlyPeriod(req.Year, req.Quarter)
	case admin.PackKindBoardAdhoc:
		return admin.AdhocPeriod(req.Label)
	default:
		return admin.PackPeriod{}, excerrors.New("INVALID_REQUEST",
			"kind must be CEO_DAILY|BOARD_QUARTERLY|BOARD_ADHOC")
	}
}
