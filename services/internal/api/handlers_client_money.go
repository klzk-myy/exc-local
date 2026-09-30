// handlers_client_money.go — Phase-24 Tasks 24.3.17/24.3.18 admin surface
// (spec §17.13.1–2). Routes are registered by the Phase-05 route registry
// (routes_v1.go, not edited here); convention follows handlers_backoffice.go:
// nil service seam → SERVICE_DEGRADED 503, admin identity from Bearer claims,
// coded errors through writeServiceErr.
//
//	GET  /api/v1/admin/treasury/own-funds
//	GET  /api/v1/admin/treasury/contingent-capital
//	POST /api/v1/admin/treasury/contingent-capital
//	GET  /api/v1/admin/client-money/audits
//	POST /api/v1/admin/client-money/audits
//	POST /api/v1/admin/client-money/audits/{id}/evidence-pack
//	GET  /api/v1/admin/client-money/certifications
//	POST /api/v1/admin/client-money/certifications
//
// Money fields are string decimals on the wire (no float64). approver_id
// carries the §8.2 synchronous four-eyes principal on dual-controlled writes
// (certification issuance).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"exchange/internal/admin"
	"exchange/internal/backoffice"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
)

// treasuryAPI is the narrow handler seam — *backoffice.TreasuryService.
type treasuryAPI interface {
	ListOwnFunds(ctx context.Context, actor admin.AdminActor) ([]backoffice.OwnFunds, error)
	ListCommitments(ctx context.Context, actor admin.AdminActor) ([]backoffice.Commitment, error)
	RecordCommitment(ctx context.Context, actor admin.AdminActor, c backoffice.Commitment) (*backoffice.Commitment, error)
	Controls(ctx context.Context, actor admin.AdminActor) (*backoffice.TreasuryControls, error)
}

// assuranceAPI is the narrow handler seam — *backoffice.AssuranceService.
type assuranceAPI interface {
	ListAudits(ctx context.Context, actor admin.AdminActor, status string, limit int) ([]backoffice.ClientMoneyAudit, error)
	CreateAudit(ctx context.Context, actor admin.AdminActor, a backoffice.ClientMoneyAudit) (*backoffice.ClientMoneyAudit, error)
	AssembleEvidencePack(ctx context.Context, actor admin.AdminActor, auditID int64) (*backoffice.EvidencePack, error)
	IssueCertification(ctx context.Context, actor admin.AdminActor, auditID, packID int64,
		statement string, signatories json.RawMessage, publishedUntil time.Time) (*backoffice.SegregationCertification, error)
	ListCertifications(ctx context.Context, actor admin.AdminActor) ([]backoffice.SegregationCertification, error)
}

// ---------------------------------------------------------------------------
// Treasury (Task 24.3.17)
// ---------------------------------------------------------------------------

// AdminTreasuryOwnFunds serves GET /api/v1/admin/treasury/own-funds —
// the own-funds ledger plus the live treasury freeze flags.
func AdminTreasuryOwnFunds(svc treasuryAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "treasury service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, ok := adminActor(w, r)
		if !ok {
			return
		}
		actor := admin.AdminActor{UserID: uid}
		funds, err := svc.ListOwnFunds(r.Context(), actor)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		ctrl, err := svc.Controls(r.Context(), actor)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"own_funds":         funds,
			"treasury_controls": ctrl,
		})
	}
}

// AdminContingentCapitalList serves GET /api/v1/admin/treasury/contingent-capital.
func AdminContingentCapitalList(svc treasuryAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "treasury service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, ok := adminActor(w, r)
		if !ok {
			return
		}
		cs, err := svc.ListCommitments(r.Context(), admin.AdminActor{UserID: uid})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"commitments": cs})
	}
}

// contingentCapitalRequest is the POST shape — money fields are decimal
// strings; expires_at is RFC3339 or YYYY-MM-DD.
type contingentCapitalRequest struct {
	ProviderName      string `json:"provider_name"`
	Kind              string `json:"commitment_kind"`
	PrioritySeq       int    `json:"priority_seq"`
	CommittedAmount   string `json:"committed_amount"`
	Currency          string `json:"currency"`
	ActivationTrigger string `json:"activation_trigger"`
	DrawWindowDays    int    `json:"draw_window_days"`
	AgreementRef      string `json:"agreement_ref"`
	PolicyType        string `json:"policy_type"`
	CoverLimit        string `json:"cover_limit"`
	Excess            string `json:"excess"`
	Broker            string `json:"broker"`
	ExpiresAt         string `json:"expires_at"`
}

func parseDecimalField(raw string) (decimal.Decimal, error) {
	if raw == "" {
		return decimal.Zero, nil
	}
	return decimal.NewFromString(raw)
}

func parseTimeField(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t, nil
	}
	if d, err := time.Parse("2006-01-02", raw); err == nil {
		u := d.UTC()
		return &u, nil
	}
	return nil, fmt.Errorf("invalid timestamp %q (want RFC3339 or YYYY-MM-DD)", raw)
}

func writeBadField(w http.ResponseWriter, r *http.Request, field string) {
	WriteError(w, "INVALID_REQUEST",
		fmt.Sprintf("%s must be a decimal string / RFC3339 / YYYY-MM-DD as applicable", field),
		gateway.RequestIDFrom(r.Context()), nil)
}

// AdminContingentCapitalCreate serves POST /api/v1/admin/treasury/contingent-capital.
func AdminContingentCapitalCreate(svc treasuryAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "treasury service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, ok := adminActor(w, r)
		if !ok {
			return
		}
		var body contingentCapitalRequest
		if !decodeJSONBody(w, r, &body) {
			return
		}
		committed, err := parseDecimalField(body.CommittedAmount)
		if err != nil {
			writeBadField(w, r, "committed_amount")
			return
		}
		limit, err := parseDecimalField(body.CoverLimit)
		if err != nil {
			writeBadField(w, r, "cover_limit")
			return
		}
		excess, err := parseDecimalField(body.Excess)
		if err != nil {
			writeBadField(w, r, "excess")
			return
		}
		expires, err := parseTimeField(body.ExpiresAt)
		if err != nil {
			writeBadField(w, r, "expires_at")
			return
		}
		c, err := svc.RecordCommitment(r.Context(), admin.AdminActor{UserID: uid},
			backoffice.Commitment{
				ProviderName: body.ProviderName, Kind: body.Kind,
				PrioritySeq: body.PrioritySeq, CommittedAmount: committed,
				Currency: body.Currency, ActivationTrigger: body.ActivationTrigger,
				DrawWindowDays: body.DrawWindowDays, AgreementRef: body.AgreementRef,
				PolicyType: body.PolicyType, CoverLimit: limit, Excess: excess,
				Broker: body.Broker, ExpiresAt: expires,
			})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"commitment": c})
	}
}

// ---------------------------------------------------------------------------
// Assurance (Task 24.3.18)
// ---------------------------------------------------------------------------

// AdminClientMoneyAudits serves GET /api/v1/admin/client-money/audits?status=.
func AdminClientMoneyAudits(svc assuranceAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "assurance service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, ok := adminActor(w, r)
		if !ok {
			return
		}
		audits, err := svc.ListAudits(r.Context(), admin.AdminActor{UserID: uid},
			r.URL.Query().Get("status"), parseLimitQuery(r.URL.Query().Get("limit"), 200, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"audits": audits})
	}
}

type auditCreateRequest struct {
	EngagementYear int    `json:"engagement_year"`
	AuditorFirm    string `json:"auditor_firm"`
	Scope          string `json:"scope"`
	PeriodStart    string `json:"period_start"`
	PeriodEnd      string `json:"period_end"`
}

// AdminClientMoneyAuditCreate serves POST /api/v1/admin/client-money/audits.
func AdminClientMoneyAuditCreate(svc assuranceAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "assurance service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, ok := adminActor(w, r)
		if !ok {
			return
		}
		var body auditCreateRequest
		if !decodeJSONBody(w, r, &body) {
			return
		}
		start, err1 := time.Parse("2006-01-02", body.PeriodStart)
		end, err2 := time.Parse("2006-01-02", body.PeriodEnd)
		if err1 != nil || err2 != nil {
			WriteError(w, "INVALID_REQUEST", "period_start/period_end must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		a, err := svc.CreateAudit(r.Context(), admin.AdminActor{UserID: uid},
			backoffice.ClientMoneyAudit{
				EngagementYear: body.EngagementYear, AuditorFirm: body.AuditorFirm,
				Scope: body.Scope, PeriodStart: start.UTC(), PeriodEnd: end.UTC(),
			})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"audit": a})
	}
}

// AdminClientMoneyEvidencePack serves
// POST /api/v1/admin/client-money/audits/{id}/evidence-pack — assembles the
// pack from the system of record (never manual).
func AdminClientMoneyEvidencePack(svc assuranceAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "assurance service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		pack, err := svc.AssembleEvidencePack(r.Context(), admin.AdminActor{UserID: uid}, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"evidence_pack": pack})
	}
}

// AdminClientMoneyCertificationList serves
// GET /api/v1/admin/client-money/certifications.
func AdminClientMoneyCertificationList(svc assuranceAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "assurance service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, ok := adminActor(w, r)
		if !ok {
			return
		}
		certs, err := svc.ListCertifications(r.Context(), admin.AdminActor{UserID: uid})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"certifications": certs})
	}
}

type certificationRequest struct {
	AuditID        int64             `json:"audit_id"`
	EvidencePackID int64             `json:"evidence_pack_id"`
	Statement      string            `json:"statement"`
	Signatories    []json.RawMessage `json:"signatories"`
	PublishedUntil string            `json:"published_until"`
	ApproverID     int64             `json:"approver_id"` // §8.2 four-eyes principal
}

// AdminClientMoneyCertificationCreate serves
// POST /api/v1/admin/client-money/certifications — dual control: approver_id
// carries the second (distinct) principal.
func AdminClientMoneyCertificationCreate(svc assuranceAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "assurance service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		uid, ok := adminActor(w, r)
		if !ok {
			return
		}
		var body certificationRequest
		if !decodeJSONBody(w, r, &body) {
			return
		}
		until, err := parseTimeField(body.PublishedUntil)
		if err != nil || until == nil {
			WriteError(w, "INVALID_REQUEST", "published_until must be RFC3339 or YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if len(body.Signatories) == 0 {
			WriteError(w, "INVALID_REQUEST", "signatories required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		signatories, err := json.Marshal(body.Signatories)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		cert, err := svc.IssueCertification(r.Context(),
			admin.AdminActor{UserID: uid, ApproverID: body.ApproverID},
			body.AuditID, body.EvidencePackID, body.Statement,
			json.RawMessage(signatories), *until)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"certification": cert})
	}
}
