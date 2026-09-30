// Phase-21 Task 21.3.15 + 21.3.19 admin/public surface — regulated-
// venue governance (member/DEA register, rulebook/product versioning,
// market-control record, cases, conflicts, self-assessment/CCO report,
// launch gate) and the public MiFID II RTS 27/28 best-execution
// publications.
//
// Mutations re-check Compliance Officer / Super Admin inside the
// services; admin reads are auditor-open per the route registry. The
// public best-execution surface is unauthenticated and serves
// PUBLISHED artifacts only — DRAFT rows are invisible there.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/compliance"
	"exchange/internal/compliance/venue"
	"exchange/internal/gateway"
)

// VenueGovDeps carries the Task 21.3.15/19 services; each may be nil —
// handlers fail closed SERVICE_DEGRADED rather than panic.
type VenueGovDeps struct {
	Venue *venue.Service
	RTS27 *compliance.RTS27Service
	RTS28 *compliance.RTS28Service
}

func venueSvc(w http.ResponseWriter, r *http.Request, d VenueGovDeps) (*venue.Service, bool) {
	if d.Venue == nil {
		WriteError(w, "SERVICE_DEGRADED", "venue governance unavailable",
			gateway.RequestIDFrom(r.Context()), nil)
		return nil, false
	}
	return d.Venue, true
}

func rts27Svc(w http.ResponseWriter, r *http.Request, d VenueGovDeps) (*compliance.RTS27Service, bool) {
	if d.RTS27 == nil {
		WriteError(w, "SERVICE_DEGRADED", "RTS 27 reporting unavailable",
			gateway.RequestIDFrom(r.Context()), nil)
		return nil, false
	}
	return d.RTS27, true
}

func rts28Svc(w http.ResponseWriter, r *http.Request, d VenueGovDeps) (*compliance.RTS28Service, bool) {
	if d.RTS28 == nil {
		WriteError(w, "SERVICE_DEGRADED", "RTS 28 reporting unavailable",
			gateway.RequestIDFrom(r.Context()), nil)
		return nil, false
	}
	return d.RTS28, true
}

func venueID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, "INVALID_REQUEST", "bad id",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return id, true
}

func parseDayParam(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", raw)
	return t, err == nil
}

// ---------------------------------------------------------------------------
// Member register — Task 21.3.15 item 1
// ---------------------------------------------------------------------------

// AdminVenueMemberList serves GET /api/v1/admin/venue/members
// (?access_model=MEMBER|DEA|SPONSORED&limit=).
func AdminVenueMemberList(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		ms, err := svc.ListMembers(r.Context(),
			strings.ToUpper(r.URL.Query().Get("access_model")),
			parseLimitQuery(r.URL.Query().Get("limit"), 200, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if ms == nil {
			ms = []venue.Member{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"members": ms})
	}
}

// AdminVenueMemberRegister serves POST /api/v1/admin/venue/members —
// files a member/DEA/sponsored application {legal_name, lei,
// access_model, regulatory_status?, jurisdiction?}. Idempotent on LEI.
func AdminVenueMemberRegister(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			LegalName        string `json:"legal_name"`
			LEI              string `json:"lei"`
			AccessModel      string `json:"access_model"`
			RegulatoryStatus string `json:"regulatory_status"`
			Jurisdiction     string `json:"jurisdiction"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		m, created, err := svc.RegisterMember(r.Context(), venue.MemberInput{
			LegalName: body.LegalName, LEI: body.LEI,
			AccessModel:      body.AccessModel,
			RegulatoryStatus: body.RegulatoryStatus,
			Jurisdiction:     body.Jurisdiction,
		}, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		code := http.StatusCreated
		if !created {
			code = http.StatusOK
		}
		WriteJSON(w, code, map[string]any{"member": m, "created": created})
	}
}

// AdminVenueMemberGet serves GET /api/v1/admin/venue/members/{id} —
// register row + immutable lifecycle ledger + review history.
func AdminVenueMemberGet(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		m, err := svc.GetMember(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		events, err := svc.MemberEvents(r.Context(), id, 500)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		reviews, err := svc.MemberReviews(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if events == nil {
			events = []venue.MemberEvent{}
		}
		if reviews == nil {
			reviews = []venue.MemberReview{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"member": m, "events": events, "reviews": reviews})
	}
}

// AdminVenueMemberDueDiligence serves POST
// /api/v1/admin/venue/members/{id}/due-diligence {status, evidence?}.
func AdminVenueMemberDueDiligence(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Status   string          `json:"status"`
			Evidence json.RawMessage `json:"evidence"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		m, err := svc.RecordDueDiligence(r.Context(), id, body.Status,
			body.Evidence, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"member": m})
	}
}

// AdminVenueMemberAgreement serves POST
// /api/v1/admin/venue/members/{id}/agreements {kind, ref}.
func AdminVenueMemberAgreement(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Kind string `json:"kind"`
			Ref  string `json:"ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		m, err := svc.AttachAgreement(r.Context(), id, body.Kind, body.Ref,
			actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"member": m})
	}
}

// AdminVenueMemberProducts serves POST
// /api/v1/admin/venue/members/{id}/products {products[], ports[]}.
func AdminVenueMemberProducts(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Products []string `json:"products"`
			Ports    []string `json:"ports"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		m, err := svc.ApproveProducts(r.Context(), id, body.Products,
			body.Ports, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"member": m})
	}
}

// AdminVenueMemberDecision serves POST
// /api/v1/admin/venue/members/{id}/decision {approve, reason?,
// annual_review_due?}. APPROVED is refused before due diligence
// COMPLETED + ≥1 agreement (fail closed).
func AdminVenueMemberDecision(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Approve         bool   `json:"approve"`
			Reason          string `json:"reason"`
			AnnualReviewDue string `json:"annual_review_due"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var due time.Time
		if body.AnnualReviewDue != "" {
			var perr error
			due, perr = time.Parse("2006-01-02", body.AnnualReviewDue)
			if perr != nil {
				WriteError(w, "INVALID_REQUEST",
					"annual_review_due must be YYYY-MM-DD",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		m, err := svc.Decide(r.Context(), id, body.Approve, body.Reason,
			due, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"member": m})
	}
}

// AdminVenueMemberSuspend serves POST
// /api/v1/admin/venue/members/{id}/suspend {reason} — trading stops at
// the next gate read.
func AdminVenueMemberSuspend(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return venueMemberAction(d, trustProxy, func(s *venue.Service,
		ctx context.Context, id int64, reason string, actor int64) (*venue.Member, error) {
		return s.Suspend(ctx, id, reason, actor)
	})
}

// AdminVenueMemberReinstate serves POST
// /api/v1/admin/venue/members/{id}/reinstate {reason}.
func AdminVenueMemberReinstate(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return venueMemberAction(d, trustProxy, func(s *venue.Service,
		ctx context.Context, id int64, reason string, actor int64) (*venue.Member, error) {
		return s.Reinstate(ctx, id, reason, actor)
	})
}

// AdminVenueMemberTerminate serves POST
// /api/v1/admin/venue/members/{id}/terminate {reason} — ends
// membership; the row is retained for record-keeping.
func AdminVenueMemberTerminate(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return venueMemberAction(d, trustProxy, func(s *venue.Service,
		ctx context.Context, id int64, reason string, actor int64) (*venue.Member, error) {
		return s.Terminate(ctx, id, reason, actor)
	})
}

func venueMemberAction(d VenueGovDeps, trustProxy bool,
	fn func(*venue.Service, context.Context, int64, string, int64) (*venue.Member, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
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
		m, err := fn(svc, r.Context(), id, body.Reason, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"member": m})
	}
}

// AdminVenueMemberAppeal serves POST
// /api/v1/admin/venue/members/{id}/appeals {grounds} — the appeal is a
// ledger row, never an edit.
func AdminVenueMemberAppeal(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Grounds string `json:"grounds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ev, err := svc.Appeal(r.Context(), id, body.Grounds, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"appeal": ev})
	}
}

// AdminVenueMemberAppealDecision serves POST
// /api/v1/admin/venue/members/{id}/appeal-decision {uphold, rationale}
// — UPHELD reinstates a suspended member (never a terminated one).
func AdminVenueMemberAppealDecision(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Uphold    bool   `json:"uphold"`
			Rationale string `json:"rationale"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		m, err := svc.DecideAppeal(r.Context(), id, body.Uphold,
			body.Rationale, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"member": m})
	}
}

// AdminVenueMemberReview serves POST
// /api/v1/admin/venue/members/{id}/reviews {review_type?, outcome,
// findings?, next_review_due} — a FAIL outcome suspends trading.
func AdminVenueMemberReview(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			ReviewType    string          `json:"review_type"`
			Outcome       string          `json:"outcome"`
			Findings      json.RawMessage `json:"findings"`
			NextReviewDue string          `json:"next_review_due"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		nextDue, ok := parseDayParam(body.NextReviewDue)
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				"next_review_due must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rv, err := svc.Review(r.Context(), id, body.ReviewType,
			body.Outcome, body.Findings, nextDue, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"review": rv})
	}
}

// ---------------------------------------------------------------------------
// Rulebook / product governance — Task 21.3.15 item 2
// ---------------------------------------------------------------------------

// AdminVenueRulebookList serves GET /api/v1/admin/venue/rulebooks
// (?kind=RULEBOOK|PRODUCT_TERMS&status=).
func AdminVenueRulebookList(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		rbs, err := svc.ListRulebooks(r.Context(),
			strings.ToUpper(r.URL.Query().Get("kind")),
			strings.ToUpper(r.URL.Query().Get("status")),
			parseLimitQuery(r.URL.Query().Get("limit"), 200, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rbs == nil {
			rbs = []venue.Rulebook{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"rulebooks": rbs})
	}
}

// AdminVenueRulebookDraft serves POST /api/v1/admin/venue/rulebooks —
// files a new versioned rulebook/product-terms draft. Idempotent on
// (kind, scope_key, version).
func AdminVenueRulebookDraft(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Kind              string `json:"kind"`
			ScopeKey          string `json:"scope_key"`
			Version           string `json:"version"`
			BodyRef           string `json:"body_ref"`
			RequiresRegulator bool   `json:"requires_regulator_approval"`
			NoticePeriodDays  int    `json:"notice_period_days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rb, created, err := svc.DraftRulebook(r.Context(),
			strings.ToUpper(body.Kind), body.ScopeKey, body.Version,
			body.BodyRef, body.RequiresRegulator, body.NoticePeriodDays,
			actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		code := http.StatusCreated
		if !created {
			code = http.StatusOK
		}
		WriteJSON(w, code, map[string]any{"rulebook": rb, "created": created})
	}
}

// AdminVenueRulebookGet serves GET /api/v1/admin/venue/rulebooks/{id}
// — version row + notices + acknowledgement evidence.
func AdminVenueRulebookGet(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		rb, err := svc.GetRulebook(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		notices, err := svc.ListNotices(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		acks, err := svc.ListAcks(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if notices == nil {
			notices = []venue.RuleNotice{}
		}
		if acks == nil {
			acks = []venue.RuleAck{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"rulebook": rb, "notices": notices, "acknowledgements": acks})
	}
}

// AdminVenueRulebookFile serves POST
// /api/v1/admin/venue/rulebooks/{id}/file {filing_ref} — records the
// regulator filing (DRAFT → FILED).
func AdminVenueRulebookFile(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			FilingRef string `json:"filing_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rb, err := svc.FileToRegulator(r.Context(), id, body.FilingRef,
			actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"rulebook": rb})
	}
}

// AdminVenueRulebookRegDecision serves POST
// /api/v1/admin/venue/rulebooks/{id}/regulator-decision {approved,
// notes?} — ingests the regulator verdict.
func AdminVenueRulebookRegDecision(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Approved bool   `json:"approved"`
			Notes    string `json:"notes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rb, err := svc.RecordRegulatorDecision(r.Context(), id,
			body.Approved, body.Notes, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"rulebook": rb})
	}
}

// AdminVenueRulebookApprove serves POST
// /api/v1/admin/venue/rulebooks/{id}/approve — venue/CCO approval.
func AdminVenueRulebookApprove(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		rb, err := svc.Approve(r.Context(), id, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"rulebook": rb})
	}
}

// AdminVenueRulebookActivate serves POST
// /api/v1/admin/venue/rulebooks/{id}/activate {effective_from?,
// emergency?, emergency_reason?} — refused with
// VENUE_RULEBOOK_NOT_APPROVED before the required approvals stand; a
// non-emergency change also requires a participant notice.
func AdminVenueRulebookActivate(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			EffectiveFrom   string `json:"effective_from"`
			Emergency       bool   `json:"emergency"`
			EmergencyReason string `json:"emergency_reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var from time.Time
		if body.EffectiveFrom != "" {
			var perr error
			from, perr = time.Parse(time.RFC3339, body.EffectiveFrom)
			if perr != nil {
				if d, derr := time.Parse("2006-01-02", body.EffectiveFrom); derr == nil {
					from = d
				} else {
					WriteError(w, "INVALID_REQUEST",
						"effective_from must be RFC3339 or YYYY-MM-DD",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
			}
		}
		rb, err := svc.Activate(r.Context(), id, from, body.Emergency,
			body.EmergencyReason, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"rulebook": rb})
	}
}

// AdminVenueNoticeIssue serves POST
// /api/v1/admin/venue/rulebooks/{id}/notices {subject, body_ref?,
// member_id?} — member_id omitted broadcasts to all participants.
func AdminVenueNoticeIssue(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Subject  string `json:"subject"`
			BodyRef  string `json:"body_ref"`
			MemberID *int64 `json:"member_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		n, err := svc.IssueNotice(r.Context(), id, body.MemberID,
			body.Subject, body.BodyRef, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"notice": n})
	}
}

// AdminVenueAck serves POST /api/v1/admin/venue/rulebooks/{id}/acks
// {member_id, notice_id?, acknowledged_by, evidence?} — member
// acknowledgement evidence; idempotent on (rulebook, member, notice).
func AdminVenueAck(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			MemberID       int64           `json:"member_id"`
			NoticeID       *int64          `json:"notice_id"`
			AcknowledgedBy string          `json:"acknowledged_by"`
			Evidence       json.RawMessage `json:"evidence"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ack, created, err := svc.RecordAck(r.Context(), id, body.MemberID,
			body.NoticeID, body.AcknowledgedBy, body.Evidence, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		code := http.StatusCreated
		if !created {
			code = http.StatusOK
		}
		WriteJSON(w, code, map[string]any{"ack": ack, "created": created})
	}
}

// ---------------------------------------------------------------------------
// Interventions / cases / conflicts — Task 21.3.15 item 3
// ---------------------------------------------------------------------------

// AdminVenueInterventionList serves GET /api/v1/admin/venue/interventions
// (?status=ACTIVE|LIFTED|CLOSED&kind=).
func AdminVenueInterventionList(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		vs, err := svc.ListInterventions(r.Context(),
			strings.ToUpper(r.URL.Query().Get("status")),
			strings.ToUpper(r.URL.Query().Get("kind")),
			parseLimitQuery(r.URL.Query().Get("limit"), 200, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if vs == nil {
			vs = []venue.Intervention{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"interventions": vs})
	}
}

// AdminVenueInterventionRecord serves POST
// /api/v1/admin/venue/interventions {kind, instrument_id?, member_id?,
// account_id?, reason, detail?} — the market-control/emergency record.
func AdminVenueInterventionRecord(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Kind         string          `json:"kind"`
			InstrumentID *int64          `json:"instrument_id"`
			MemberID     *int64          `json:"member_id"`
			AccountID    *int64          `json:"account_id"`
			Reason       string          `json:"reason"`
			Detail       json.RawMessage `json:"detail"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		v, err := svc.RecordIntervention(r.Context(),
			strings.ToUpper(body.Kind), body.InstrumentID, body.MemberID,
			body.AccountID, body.Reason, body.Detail, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"intervention": v})
	}
}

// AdminVenueInterventionLift serves POST
// /api/v1/admin/venue/interventions/{id}/lift {note?}.
func AdminVenueInterventionLift(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // note optional
		v, err := svc.LiftIntervention(r.Context(), id, body.Note, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"intervention": v})
	}
}

// AdminVenueCaseList serves GET /api/v1/admin/venue/cases
// (?status=&kind=INVESTIGATION|DISCIPLINARY).
func AdminVenueCaseList(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		cs, err := svc.ListCases(r.Context(),
			strings.ToUpper(r.URL.Query().Get("status")),
			strings.ToUpper(r.URL.Query().Get("kind")),
			parseLimitQuery(r.URL.Query().Get("limit"), 200, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if cs == nil {
			cs = []venue.Case{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"cases": cs})
	}
}

// AdminVenueCaseOpen serves POST /api/v1/admin/venue/cases {kind,
// member_id?, account_id?, subject, detail?}.
func AdminVenueCaseOpen(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Kind      string          `json:"kind"`
			MemberID  *int64          `json:"member_id"`
			AccountID *int64          `json:"account_id"`
			Subject   string          `json:"subject"`
			Detail    json.RawMessage `json:"detail"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, created, err := svc.OpenCase(r.Context(),
			strings.ToUpper(body.Kind), body.MemberID, body.AccountID,
			body.Subject, body.Detail, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		code := http.StatusCreated
		if !created {
			code = http.StatusOK
		}
		WriteJSON(w, code, map[string]any{"case": c, "created": created})
	}
}

// AdminVenueCaseGet serves GET /api/v1/admin/venue/cases/{id} — case +
// immutable evidence attachments.
func AdminVenueCaseGet(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		c, err := svc.GetCase(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		ev, err := svc.ListCaseEvidence(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if ev == nil {
			ev = []venue.CaseEvidence{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"case": c, "evidence": ev})
	}
}

// AdminVenueCaseEvidence serves POST
// /api/v1/admin/venue/cases/{id}/evidence {evidence_ref, sha256?,
// note?} — append-only.
func AdminVenueCaseEvidence(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			EvidenceRef string `json:"evidence_ref"`
			SHA256      string `json:"sha256"`
			Note        string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ev, err := svc.AttachEvidence(r.Context(), id, body.EvidenceRef,
			body.SHA256, body.Note, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"evidence": ev})
	}
}

// AdminVenueCaseTransition serves POST
// /api/v1/admin/venue/cases/{id}/transition {status, outcome?} —
// OPEN → INVESTIGATING → CHARGED → SANCTIONED|DISMISSED|CLOSED;
// terminal transitions require an outcome.
func AdminVenueCaseTransition(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Status  string `json:"status"`
			Outcome string `json:"outcome"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, err := svc.TransitionCase(r.Context(), id,
			strings.ToUpper(body.Status), body.Outcome, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"case": c})
	}
}

// AdminVenueConflictList serves GET /api/v1/admin/venue/conflicts
// (?status=DECLARED|MITIGATED|RECUSED|CLOSED).
func AdminVenueConflictList(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		cs, err := svc.ListConflicts(r.Context(),
			strings.ToUpper(r.URL.Query().Get("status")),
			parseLimitQuery(r.URL.Query().Get("limit"), 200, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if cs == nil {
			cs = []venue.Conflict{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"conflicts": cs})
	}
}

// AdminVenueConflictDeclare serves POST /api/v1/admin/venue/conflicts
// {member_id?, officer_user_id?, subject, nature}.
func AdminVenueConflictDeclare(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			MemberID      *int64 `json:"member_id"`
			OfficerUserID *int64 `json:"officer_user_id"`
			Subject       string `json:"subject"`
			Nature        string `json:"nature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, err := svc.DeclareConflict(r.Context(), body.MemberID,
			body.OfficerUserID, body.Subject, body.Nature, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"conflict": c})
	}
}

// AdminVenueConflictResolve serves POST
// /api/v1/admin/venue/conflicts/{id}/resolve {status:MITIGATED|RECUSED|
// CLOSED, mitigation} — RECUSED is the recusal edge case.
func AdminVenueConflictResolve(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			Status     string `json:"status"`
			Mitigation string `json:"mitigation"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, err := svc.ResolveConflict(r.Context(), id,
			strings.ToUpper(body.Status), body.Mitigation, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"conflict": c})
	}
}

// ---------------------------------------------------------------------------
// Self-assessment + CCO report — Task 21.3.15 item 4
// ---------------------------------------------------------------------------

// AdminVenueAssessmentList serves GET
// /api/v1/admin/venue/self-assessments.
func AdminVenueAssessmentList(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		as, err := svc.ListSelfAssessments(r.Context(),
			parseLimitQuery(r.URL.Query().Get("limit"), 20, 100))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if as == nil {
			as = []venue.SelfAssessment{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"assessments": as})
	}
}

// AdminVenueAssessmentFile serves POST
// /api/v1/admin/venue/self-assessments {period_year, exceptions?,
// financial_attestation?, remediation?} — the control-evidence
// snapshot is assembled service-side at filing time.
func AdminVenueAssessmentFile(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			PeriodYear           int             `json:"period_year"`
			Exceptions           json.RawMessage `json:"exceptions"`
			FinancialAttestation json.RawMessage `json:"financial_attestation"`
			Remediation          json.RawMessage `json:"remediation"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		a, err := svc.FileSelfAssessment(r.Context(), body.PeriodYear,
			body.Exceptions, body.FinancialAttestation, body.Remediation,
			actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"assessment": a})
	}
}

// AdminVenueAssessmentComplete serves POST
// /api/v1/admin/venue/self-assessments/{id}/complete — assessor sign-off.
func AdminVenueAssessmentComplete(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		a, err := svc.CompleteSelfAssessment(r.Context(), id, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"assessment": a})
	}
}

// AdminVenueCCOList serves GET /api/v1/admin/venue/cco-reports.
func AdminVenueCCOList(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		rs, err := svc.ListCCOReports(r.Context(),
			parseLimitQuery(r.URL.Query().Get("limit"), 20, 100))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rs == nil {
			rs = []venue.CCOReport{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reports": rs})
	}
}

// AdminVenueCCOGenerate serves POST /api/v1/admin/venue/cco-reports
// {period_start, period_end, unresolved_remediation?} — assembles the
// annual CCO report with the control-evidence snapshot.
func AdminVenueCCOGenerate(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			PeriodStart           string          `json:"period_start"`
			PeriodEnd             string          `json:"period_end"`
			UnresolvedRemediation json.RawMessage `json:"unresolved_remediation"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		start, ok1 := parseDayParam(body.PeriodStart)
		end, ok2 := parseDayParam(body.PeriodEnd)
		if !ok1 || !ok2 {
			WriteError(w, "INVALID_REQUEST",
				"period_start/period_end must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rep, err := svc.GenerateCCOReport(r.Context(), start, end,
			body.UnresolvedRemediation, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"report": rep})
	}
}

// AdminVenueCCOSign serves POST /api/v1/admin/venue/cco-reports/{id}/sign
// — board sign-off (DRAFT → SIGNED).
func AdminVenueCCOSign(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		rep, err := svc.SignCCOReport(r.Context(), id, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// AdminVenueCCOFile serves POST /api/v1/admin/venue/cco-reports/{id}/file
// {filing_ref} — records the regulator filing (SIGNED → FILED).
func AdminVenueCCOFile(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			FilingRef string `json:"filing_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rep, err := svc.FileCCOReport(r.Context(), id, body.FilingRef,
			actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// ---------------------------------------------------------------------------
// Launch prerequisites + gate — Task 21.3.15 item 5
// ---------------------------------------------------------------------------

// AdminVenuePrereqList serves GET
// /api/v1/admin/venue/launch-prerequisites — the full checklist.
func AdminVenuePrereqList(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		ps, err := svc.ListPrerequisites(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if ps == nil {
			ps = []venue.Prerequisite{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"prerequisites": ps})
	}
}

// AdminVenuePrereqEvidence serves POST
// /api/v1/admin/venue/launch-prerequisites {kind, scope?, description?,
// evidence_ref, expires_at?} — upserts the evidence row.
func AdminVenuePrereqEvidence(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Kind        string `json:"kind"`
			Scope       string `json:"scope"`
			Description string `json:"description"`
			EvidenceRef string `json:"evidence_ref"`
			ExpiresAt   string `json:"expires_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var exp *time.Time
		if body.ExpiresAt != "" {
			t, perr := time.Parse(time.RFC3339, body.ExpiresAt)
			if perr != nil {
				WriteError(w, "INVALID_REQUEST",
					"expires_at must be RFC3339",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			exp = &t
		}
		p, err := svc.EvidencePrerequisite(r.Context(),
			strings.ToUpper(body.Kind), body.Scope, body.Description,
			body.EvidenceRef, exp, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"prerequisite": p})
	}
}

// AdminVenuePrereqExpire serves POST
// /api/v1/admin/venue/launch-prerequisites/{id}/expire — the
// license-lapse edge case; member trading re-checks on next read.
func AdminVenuePrereqExpire(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		p, err := svc.MarkPrereqExpired(r.Context(), id, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"prerequisite": p})
	}
}

// AdminVenueLaunchGate serves GET /api/v1/admin/venue/launch-gate —
// evaluates the production launch gate; ready=false lists the missing
// required prerequisites.
func AdminVenueLaunchGate(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := venueSvc(w, r, d)
		if !ok {
			return
		}
		rep, err := svc.LaunchGate(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		code := http.StatusOK
		if !rep.Ready {
			// 200 with ready=false — the gate answer is the payload; the
			// launch pipeline consumes `ready`, not a transport failure.
			code = http.StatusOK
		}
		WriteJSON(w, code, map[string]any{"launch_gate": rep})
	}
}

// ---------------------------------------------------------------------------
// RTS 27/28 admin surface — Task 21.3.19
// ---------------------------------------------------------------------------

// AdminMiFIDReport serves GET /api/v1/admin/mifid-report — the
// combined best-execution register view: latest RTS 27 and RTS 28
// report rows (both statuses; ?status= filters).
func AdminMiFIDReport(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r27, ok := rts27Svc(w, r, d)
		if !ok {
			return
		}
		r28, ok := rts28Svc(w, r, d)
		if !ok {
			return
		}
		status := strings.ToUpper(r.URL.Query().Get("status"))
		limit := parseLimitQuery(r.URL.Query().Get("limit"), 50, 200)
		r27s, err := r27.ListReports(r.Context(), status, limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		r28s, err := r28.ListReports(r.Context(), status, 0, limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if r27s == nil {
			r27s = []compliance.RTS27Report{}
		}
		if r28s == nil {
			r28s = []compliance.RTS28Report{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"rts27_reports": r27s, "rts28_reports": r28s})
	}
}

// AdminRTS27Materialize serves POST
// /api/v1/admin/bestexec/rts27/materialize {day?} — recomputes the
// day's daily_stats rows from ClickHouse (idempotent upsert). Day
// defaults to yesterday UTC.
func AdminRTS27Materialize(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts27Svc(w, r, d)
		if !ok {
			return
		}
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Day string `json:"day"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil &&
			r.ContentLength > 0 {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		day := time.Now().UTC().Add(-24 * time.Hour)
		if body.Day != "" {
			d, ok := parseDayParam(body.Day)
			if !ok {
				WriteError(w, "INVALID_REQUEST", "day must be YYYY-MM-DD",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			day = d
		}
		n, err := svc.MaterializeDay(r.Context(), day)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"day": day.Format("2006-01-02"), "rows_materialized": n})
	}
}

// AdminRTS27Generate serves POST /api/v1/admin/bestexec/rts27/generate
// {quarter?} — files a DRAFT report per instrument class for the
// quarter (default: the quarter containing today).
func AdminRTS27Generate(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts27Svc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Quarter string `json:"quarter"` // any YYYY-MM-DD inside the quarter
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil &&
			r.ContentLength > 0 {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := time.Now().UTC()
		if body.Quarter != "" {
			d, ok := parseDayParam(body.Quarter)
			if !ok {
				WriteError(w, "INVALID_REQUEST", "quarter must be YYYY-MM-DD",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			q = d
		}
		reps, err := svc.GenerateQuarter(r.Context(), q, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if reps == nil {
			reps = []compliance.RTS27Report{}
		}
		WriteJSON(w, http.StatusCreated,
			map[string]any{"reports": reps})
	}
}

// AdminRTS27List serves GET /api/v1/admin/bestexec/rts27 (?status=).
func AdminRTS27List(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts27Svc(w, r, d)
		if !ok {
			return
		}
		reps, err := svc.ListReports(r.Context(),
			strings.ToUpper(r.URL.Query().Get("status")),
			parseLimitQuery(r.URL.Query().Get("limit"), 100, 200))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if reps == nil {
			reps = []compliance.RTS27Report{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reports": reps})
	}
}

// AdminRTS27Get serves GET /api/v1/admin/bestexec/rts27/{id}.
func AdminRTS27Get(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts27Svc(w, r, d)
		if !ok {
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		rep, err := svc.GetReport(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// AdminRTS27Publish serves POST
// /api/v1/admin/bestexec/rts27/{id}/publish — DRAFT → PUBLISHED.
func AdminRTS27Publish(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts27Svc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		rep, err := svc.Publish(r.Context(), id, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// AdminRTS28Generate serves POST /api/v1/admin/bestexec/rts28/generate
// {year} — files a DRAFT per instrument class for the year.
func AdminRTS28Generate(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts28Svc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			Year int `json:"year"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		reps, err := svc.GenerateYear(r.Context(), body.Year, actor.UserID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if reps == nil {
			reps = []compliance.RTS28Report{}
		}
		WriteJSON(w, http.StatusCreated,
			map[string]any{"reports": reps})
	}
}

// AdminRTS28List serves GET /api/v1/admin/bestexec/rts28 (?status=&year=).
func AdminRTS28List(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts28Svc(w, r, d)
		if !ok {
			return
		}
		year := 0
		if q := r.URL.Query().Get("year"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				year = n
			}
		}
		reps, err := svc.ListReports(r.Context(),
			strings.ToUpper(r.URL.Query().Get("status")), year,
			parseLimitQuery(r.URL.Query().Get("limit"), 100, 200))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if reps == nil {
			reps = []compliance.RTS28Report{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reports": reps})
	}
}

// AdminRTS28Get serves GET /api/v1/admin/bestexec/rts28/{id}.
func AdminRTS28Get(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts28Svc(w, r, d)
		if !ok {
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		rep, err := svc.GetReport(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// AdminRTS28Publish serves POST
// /api/v1/admin/bestexec/rts28/{id}/publish {qualitative_assessment} —
// the narrative is mandatory (RTS 28 table 3).
func AdminRTS28Publish(d VenueGovDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts28Svc(w, r, d)
		if !ok {
			return
		}
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		var body struct {
			QualitativeAssessment string `json:"qualitative_assessment"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rep, err := svc.Publish(r.Context(), id, actor.UserID,
			body.QualitativeAssessment)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// ---------------------------------------------------------------------------
// RTS 27/28 public surface — PUBLISHED artifacts only
// ---------------------------------------------------------------------------

// PublicRTS27List serves GET /api/v1/venue/best-execution/rts27 —
// unauthenticated; PUBLISHED rows only (csv stripped).
func PublicRTS27List(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts27Svc(w, r, d)
		if !ok {
			return
		}
		reps, err := svc.ListPublished(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if reps == nil {
			reps = []compliance.RTS27Report{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reports": reps})
	}
}

// PublicRTS27Get serves GET /api/v1/venue/best-execution/rts27/{id} —
// PUBLISHED only (DRAFT answers NOT_FOUND).
func PublicRTS27Get(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts27Svc(w, r, d)
		if !ok {
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		rep, err := svc.GetPublished(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		rep.CSV = "" // artifact via /csv
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// PublicRTS27CSV serves GET /api/v1/venue/best-execution/rts27/{id}/csv.
func PublicRTS27CSV(d VenueGovDeps) http.HandlerFunc {
	return csvDownload(d, func(d2 VenueGovDeps,
		ctx context.Context, id int64) (string, error) {
		rep, err := d2.RTS27.GetPublished(ctx, id)
		if err != nil {
			return "", err
		}
		return rep.CSV, nil
	}, "rts27")
}

// PublicRTS28List serves GET /api/v1/venue/best-execution/rts28.
func PublicRTS28List(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts28Svc(w, r, d)
		if !ok {
			return
		}
		reps, err := svc.ListPublished(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if reps == nil {
			reps = []compliance.RTS28Report{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"reports": reps})
	}
}

// PublicRTS28Get serves GET /api/v1/venue/best-execution/rts28/{id}.
func PublicRTS28Get(d VenueGovDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := rts28Svc(w, r, d)
		if !ok {
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		rep, err := svc.GetPublished(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		rep.CSV = ""
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// PublicRTS28CSV serves GET /api/v1/venue/best-execution/rts28/{id}/csv.
func PublicRTS28CSV(d VenueGovDeps) http.HandlerFunc {
	return csvDownload(d, func(d2 VenueGovDeps,
		ctx context.Context, id int64) (string, error) {
		rep, err := d2.RTS28.GetPublished(ctx, id)
		if err != nil {
			return "", err
		}
		return rep.CSV, nil
	}, "rts28")
}

// csvDownload serves a published report's CSV artifact — text/csv with
// a Content-Disposition filename.
func csvDownload(d VenueGovDeps,
	fetch func(VenueGovDeps, context.Context, int64) (string, error),
	prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if (prefix == "rts27" && d.RTS27 == nil) ||
			(prefix == "rts28" && d.RTS28 == nil) {
			WriteError(w, "SERVICE_DEGRADED", "best-execution reporting unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, ok := venueID(w, r)
		if !ok {
			return
		}
		csv, err := fetch(d, r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition",
			`attachment; filename="`+prefix+`_`+strconv.FormatInt(id, 10)+`.csv"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(csv))
	}
}
