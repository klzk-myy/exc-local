// Phase-12 Tasks 12.3.4 + 12.3.13 — KYC intake surface.
//
//	POST /api/v1/kyc/submit              — multipart document submission
//	                                       (fields: requested_tier, jurisdiction;
//	                                       file parts named doc_<TYPE>, e.g.
//	                                       doc_PASSPORT, doc_UTILITY_BILL)
//	GET  /api/v1/kyc/status              — tier, limits, latest submission, docs
//	GET  /api/v1/kyc/requirements        — ops-matrix query
//	                                       (?tier=&jurisdiction=&document_type=&vendor=)
//	POST /api/v1/kyc/self-certification  — W-8BEN/W-8BEN-E/W-9 intake (T1+)
//	GET  /api/v1/kyc/self-certification  — own certification history
//
// Boundary: submission/intake only. accounts.kyc_tier stays T0 until a
// reviewer approves — Phase-14 Task 14.3.4 owns that lifecycle.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"exchange/internal/auth"
	"exchange/internal/compliance"
	"exchange/internal/gateway"
)

// kycAccount resolves the authenticated account id or writes the
// UNAUTHORIZED envelope and returns false.
func kycAccount(w http.ResponseWriter, r *http.Request) (int64, bool) {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil || claims.AccountID == 0 {
		WriteError(w, "UNAUTHORIZED", "authentication required",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return claims.AccountID, true
}

// kycErr maps service failures onto registered codes (spec §8.7):
// ErrValidation/ErrInfected are client faults (400 INVALID_REQUEST),
// ErrTierTooLow is the canonical KYC_REQUIRED 403, everything else is a
// service fault (503 SERVICE_DEGRADED — fail-closed).
func kycErr(w http.ResponseWriter, r *http.Request, err error) {
	rid := gateway.RequestIDFrom(r.Context())
	switch {
	case errors.Is(err, compliance.ErrInfected):
		WriteError(w, "INVALID_REQUEST",
			"document failed security screening", rid, nil)
	case errors.Is(err, compliance.ErrTierTooLow):
		WriteError(w, "KYC_REQUIRED", err.Error(), rid, nil)
	case errors.Is(err, compliance.ErrValidation):
		WriteError(w, "INVALID_REQUEST", err.Error(), rid, nil)
	case errors.Is(err, compliance.ErrObjectsUnset):
		WriteError(w, "SERVICE_DEGRADED",
			"document storage unavailable", rid, nil)
	default:
		WriteError(w, "SERVICE_DEGRADED",
			"KYC service unavailable", rid, nil)
	}
}

// KYCSubmit handles the multipart intake. File parts are named
// doc_<DOCUMENT_TYPE> (matrix document_type values — e.g. doc_PASSPORT,
// doc_UTILITY_BILL, doc_LIVENESS_SELFIE, doc_SOURCE_OF_FUNDS,
// doc_CERTIFICATE_OF_INCORPORATION). Per-file cap 10 MiB, total 32 MiB.
func KYCSubmit(svc *compliance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, ok := kycAccount(w, r)
		if !ok {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, compliance.MaxTotalBytes+(1<<20))
		if err := r.ParseMultipartForm(compliance.MaxTotalBytes); err != nil {
			WriteError(w, "INVALID_REQUEST",
				"multipart body required (doc_<TYPE> file parts)",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		in := compliance.SubmitInput{
			AccountID:     acct,
			RequestedTier: r.FormValue("requested_tier"),
			Jurisdiction:  r.FormValue("jurisdiction"),
		}
		if r.MultipartForm != nil {
			for field, headers := range r.MultipartForm.File {
				if !strings.HasPrefix(field, "doc_") {
					continue
				}
				docType := strings.ToUpper(strings.TrimPrefix(field, "doc_"))
				for _, fh := range headers {
					data, err := readPart(fh)
					if err != nil {
						WriteError(w, "INVALID_REQUEST",
							fmt.Sprintf("%s: %v", docType, err),
							gateway.RequestIDFrom(r.Context()), nil)
						return
					}
					in.Documents = append(in.Documents, compliance.UploadDoc{
						Type:        docType,
						Filename:    fh.Filename,
						ContentType: fh.Header.Get("Content-Type"),
						Data:        data,
					})
				}
			}
		}
		res, err := svc.Submit(r.Context(), in)
		if err != nil {
			kycErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, res)
	}
}

// readPart buffers one multipart file part (cap enforced by the caller's
// MaxBytesReader + the service's per-doc check).
func readPart(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, compliance.MaxDocBytes+1))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// KYCStatus serves the account's current KYC view — tier, §14.2 limits,
// latest submission and document list (object keys never leave the
// service).
func KYCStatus(svc *compliance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, ok := kycAccount(w, r)
		if !ok {
			return
		}
		view, err := svc.StatusView(r.Context(), acct)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED", "KYC service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, view)
	}
}

// KYCRequirements is the Task-12.3.13 query API: ?tier=&jurisdiction=
// (&document_type=&vendor= filters) → tier policy + merged matrix rows
// ('*' defaults + exact-jurisdiction overlays).
func KYCRequirements(svc *compliance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := kycAccount(w, r); !ok {
			return
		}
		q := r.URL.Query()
		req, err := svc.Requirements(r.Context(),
			q.Get("tier"), q.Get("jurisdiction"))
		if err != nil {
			kycErr(w, r, err)
			return
		}
		dt := strings.ToUpper(strings.TrimSpace(q.Get("document_type")))
		vendor := strings.ToUpper(strings.TrimSpace(q.Get("vendor")))
		if dt != "" || vendor != "" {
			filtered := req.Documents[:0]
			for _, row := range req.Documents {
				if dt != "" && row.DocumentType != dt {
					continue
				}
				if vendor != "" && strings.ToUpper(row.Vendor) != vendor {
					continue
				}
				filtered = append(filtered, row)
			}
			req.Documents = filtered
		}
		WriteJSON(w, http.StatusOK, req)
	}
}

// selfCertBody is the POST /api/v1/kyc/self-certification JSON contract.
type selfCertBody struct {
	FormType   string          `json:"form_type"`
	TIN        string          `json:"tin"`
	TINCountry string          `json:"tin_country"`
	TINKind    string          `json:"tin_kind"` // optional SSN|EIN|ITIN hint
	Fields     json.RawMessage `json:"fields"`   // JSON object; legal_name required
}

// KYCSelfCertSubmit records one W-8BEN/W-8BEN-E/W-9 — T1+ accounts only.
func KYCSelfCertSubmit(svc *compliance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, ok := kycAccount(w, r)
		if !ok {
			return
		}
		var body selfCertBody
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "invalid JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		cert, err := svc.SubmitSelfCert(r.Context(), compliance.SelfCertInput{
			AccountID:  acct,
			FormType:   body.FormType,
			TIN:        body.TIN,
			TINCountry: body.TINCountry,
			TINKind:    body.TINKind,
			Fields:     body.Fields,
		})
		if err != nil {
			kycErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, cert)
	}
}

// KYCSelfCertList returns the account's self-certification history.
func KYCSelfCertList(svc *compliance.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acct, ok := kycAccount(w, r)
		if !ok {
			return
		}
		certs, err := svc.ListSelfCerts(r.Context(), acct)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED", "KYC service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"certifications": certs})
	}
}
