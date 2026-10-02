package api

// Phase-09 Task 9.3.15 (item 4) — DORA Art. 28 ICT third-party register
// admin surface (spec §19.5, §24 #171; migration 285). RBAC: the route
// registry stamps adminAuth(RoleComplianceOfficer); handlers decode +
// delegate to operations/dora.VendorService.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/gateway"
	"exchange/internal/operations/dora"
	excerrors "exchange/pkg/errors"
)

// AdminICTList — GET /api/v1/admin/ict-providers?status=.
func AdminICTList(svc *dora.VendorService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := svc.List(r.Context(), r.URL.Query().Get("status"))
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ict_providers": out})
	}
}

type ictProviderBody struct {
	Name                    string `json:"name"`
	ICTService              string `json:"ict_service"`
	FunctionsSupported      string `json:"functions_supported"`
	LocationsSubcontractors string `json:"locations_subcontractors"`
	Concentration           string `json:"concentration"`
	ContractTerms           string `json:"contract_terms"`
	TerminationNoticeDays   *int   `json:"termination_notice_days"`
	ExitStrategy            string `json:"exit_strategy"`
	SubstitutionPlan        string `json:"substitution_plan"`
	RenewalAt               string `json:"renewal_at"`
	NextReviewAt            string `json:"next_review_at"`
	Owner                   string `json:"owner"`
	Status                  string `json:"status"`
	Notes                   string `json:"notes"`
}

func ictProviderFrom(b *ictProviderBody) (*dora.Provider, error) {
	p := &dora.Provider{
		Name: b.Name, ICTService: b.ICTService,
		FunctionsSupported:      b.FunctionsSupported,
		LocationsSubcontractors: b.LocationsSubcontractors,
		Concentration:           b.Concentration,
		ContractTerms:           b.ContractTerms,
		TerminationNoticeDays:   b.TerminationNoticeDays,
		ExitStrategy:            b.ExitStrategy,
		SubstitutionPlan:        b.SubstitutionPlan,
		Owner:                   b.Owner,
		Status:                  b.Status,
		Notes:                   b.Notes,
	}
	if b.RenewalAt != "" {
		t, perr := time.Parse(time.RFC3339, b.RenewalAt)
		if perr != nil {
			return nil, excerrors.New("INVALID_REQUEST",
				"renewal_at must be RFC3339")
		}
		p.RenewalAt = &t
	}
	if b.NextReviewAt != "" {
		t, perr := time.Parse(time.RFC3339, b.NextReviewAt)
		if perr != nil {
			return nil, excerrors.New("INVALID_REQUEST",
				"next_review_at must be RFC3339")
		}
		p.NextReviewAt = &t
	}
	return p, nil
}

// AdminICTCreate — POST /api/v1/admin/ict-providers.
func AdminICTCreate(svc *dora.VendorService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b ictProviderBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := ictProviderFrom(&b)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		out, err := svc.Create(r.Context(), p)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ict_provider": out})
	}
}

// AdminICTUpdate — PUT /api/v1/admin/ict-providers/{id} (full-row PUT).
func AdminICTUpdate(svc *dora.VendorService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var b ictProviderBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := ictProviderFrom(&b)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		p.ID = id
		out, err := svc.Update(r.Context(), p)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ict_provider": out})
	}
}

// AdminICTRetire — DELETE /api/v1/admin/ict-providers/{id}.
func AdminICTRetire(svc *dora.VendorService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err := svc.Retire(r.Context(), id, actor.UserID); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"status": "RETIRED"})
	}
}

// AdminICTReview — POST /api/v1/admin/ict-providers/{id}/reviews.
func AdminICTReview(svc *dora.VendorService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var b struct {
			Kind        string `json:"kind"`
			Outcome     string `json:"outcome"`
			EvidenceRef string `json:"evidence_ref"`
			Notes       string `json:"notes"`
			ReviewedAt  string `json:"reviewed_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rev := &dora.Review{
			ProviderID: id, Kind: b.Kind, Outcome: b.Outcome,
			EvidenceRef: b.EvidenceRef, Notes: b.Notes,
			Actor: &actor.UserID,
		}
		if b.ReviewedAt != "" {
			t, perr := time.Parse(time.RFC3339, b.ReviewedAt)
			if perr != nil {
				WriteError(w, "INVALID_REQUEST", "reviewed_at must be RFC3339",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			rev.ReviewedAt = t
		}
		out, err := svc.RecordReview(r.Context(), rev)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"review": out})
	}
}

// AdminICTReviews — GET /api/v1/admin/ict-providers/{id}/reviews.
func AdminICTReviews(svc *dora.VendorService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		out, err := svc.Reviews(r.Context(), id)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reviews": out})
	}
}

// AdminICTDue — GET /api/v1/admin/ict-providers/due — the live sweep:
// overdue reviews, approaching renewals, stale exit-plan tests.
func AdminICTDue(svc *dora.VendorService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := svc.Sweep(r.Context())
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"alerts": out})
	}
}
