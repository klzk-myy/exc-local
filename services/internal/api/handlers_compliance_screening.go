// Phase-21 Task 21.3.1/21.3.11/21.3.23 — compliance screening admin
// surface:
//
//	GET  /api/v1/admin/sanctions/status                    — list
//	      provenance + provider-gate + pending-queue depth
//	POST /api/v1/admin/sanctions/refresh                   — force the
//	      vendor-list refresh pass (Compliance Officer)
//	POST /api/v1/admin/sanctions/queue/replay              — manual
//	      pending-screen drain after provider recovery
//	POST /api/v1/admin/compliance/screening/accounts/{id}  — on-demand
//	      sanctions+PEP screen of one account
//	POST /api/v1/admin/compliance/screening/adverse-media  — manual
//	      adverse-media intake (vendor feed is the automated path)
//
// All mutating surfaces resolve the admin identity from Bearer claims
// via adminActorFrom and write the §8.7 error envelope. Service errors
// map through writeServiceErr (SANCTIONS_SERVICE_UNAVAILABLE → 503).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/compliance"
	"exchange/internal/gateway"

	excerrors "exchange/pkg/errors"
)

// ScreeningAdminDeps bundles the Phase-21 screening surfaces — wiring
// fills only what is bound; nil deps surface 503 on their endpoints
// (operator-visible, never a silent stub).
type ScreeningAdminDeps struct {
	Gate      *compliance.ProviderGate
	Screener  *compliance.ListScreener
	Refresher *compliance.VendorRefresher
	Replayer  *compliance.QueueReplayer
	Queue     compliance.ScreenQueue
	Screening *compliance.ScreeningService
	// SubjectFor materializes the ScreeningSubject for an account —
	// wiring binds the account/KYC-profile projection.
	SubjectFor func(ctx context.Context, accountID int64) (compliance.ScreeningSubject, error)
}

// AdminSanctionsStatus serves GET /api/v1/admin/sanctions/status —
// provenance inventory, provider-gate state, queue depth and the last
// refresh report.
func AdminSanctionsStatus(d ScreeningAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{}
		if d.Screener != nil {
			lists, entries, loadedAt := d.Screener.Stats()
			resp["list_files"] = lists
			resp["entries"] = entries
			resp["loaded_at"] = loadedAt
			resp["provenance"] = d.Screener.Provenance()
			resp["pep_entries"] = d.Screener.EntryCount(compliance.EntryKindPEP)
		}
		if d.Gate != nil {
			resp["provider_gate"] = d.Gate.Status()
		}
		if d.Queue != nil {
			if depth, err := d.Queue.Depth(r.Context()); err == nil {
				resp["pending_screens"] = depth
			}
		}
		if d.Refresher != nil {
			resp["last_refresh"] = d.Refresher.Status()
		}
		WriteJSON(w, http.StatusOK, resp)
	}
}

// AdminSanctionsRefresh serves POST /api/v1/admin/sanctions/refresh —
// forces one vendor pull+reload pass. Not dual-controlled: the
// operation is idempotent and fail-closed (a bad feed retains last
// good), so the safety property is the pipeline's, not the caller's.
func AdminSanctionsRefresh(d ScreeningAdminDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if d.Refresher == nil {
			writeServiceErr(w, r, excerrors.New("SANCTIONS_SERVICE_UNAVAILABLE",
				"vendor refresher not configured"))
			return
		}
		rep, err := d.Refresher.RefreshOnce(r.Context())
		// Report the pass even when a feed failed — the operator needs
		// per-feed outcomes; the error code surfaces the failure class.
		status := http.StatusOK
		code := ""
		if err != nil {
			status = http.StatusBadGateway
			code = "SANCTIONS_REFRESH_FAILED"
		}
		WriteJSON(w, status, map[string]any{
			"report": rep, "error_code": code})
	}
}

// AdminSanctionsQueueReplay serves POST /api/v1/admin/sanctions/queue/
// replay — drains the pending-screen queue now (normally invoked by the
// gate's recovery hook; this is the officer's manual trip).
func AdminSanctionsQueueReplay(d ScreeningAdminDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if d.Replayer == nil {
			writeServiceErr(w, r, excerrors.New("SANCTIONS_SERVICE_UNAVAILABLE",
				"screen queue not configured"))
			return
		}
		rep, err := d.Replayer.Replay(r.Context())
		if err != nil {
			// Partial progress is still reported — items left in the
			// queue are visible in the report's backlog field.
			WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
				"report":     rep,
				"error":      map[string]any{"code": "SANCTIONS_SERVICE_UNAVAILABLE", "message": err.Error()},
				"request_id": gateway.RequestIDFrom(r.Context())})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"report": rep})
	}
}

// AdminScreenAccount serves POST /api/v1/admin/compliance/screening/
// accounts/{id} — on-demand sanctions+PEP screen of one account (the
// rescreen/backfill officer surface).
func AdminScreenAccount(d ScreeningAdminDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		accountID, err := lpPathID(r)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if d.Screening == nil || d.SubjectFor == nil {
			writeServiceErr(w, r, excerrors.New("SANCTIONS_SERVICE_UNAVAILABLE",
				"screening service not configured"))
			return
		}
		sub, err := d.SubjectFor(r.Context(), accountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		out, err := d.Screening.ScreenOnboarding(r.Context(), sub)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		_ = actor
		WriteJSON(w, http.StatusOK, map[string]any{"outcome": out})
	}
}

// AdminAdverseMediaIntake serves POST /api/v1/admin/compliance/
// screening/adverse-media — the manual flag surface (vendor feed is
// the automated leg through ScreeningService.ScreenAdverseMedia).
func AdminAdverseMediaIntake(d ScreeningAdminDeps, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var body struct {
			AccountID   int64  `json:"account_id"`
			Source      string `json:"source"`
			Headline    string `json:"headline"`
			URL         string `json:"url"`
			Severity    string `json:"severity"`
			PublishedAt string `json:"published_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if body.AccountID <= 0 || body.Headline == "" {
			writeServiceErr(w, r, excerrors.New("INVALID_REQUEST",
				"account_id and headline are required"))
			return
		}
		if d.Screening == nil || d.SubjectFor == nil {
			writeServiceErr(w, r, excerrors.New("SANCTIONS_SERVICE_UNAVAILABLE",
				"screening service not configured"))
			return
		}
		sub, err := d.SubjectFor(r.Context(), body.AccountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		item := compliance.AdverseMediaItem{
			AccountID: body.AccountID,
			Source:    body.Source,
			Headline:  body.Headline,
			URL:       body.URL,
			Severity:  body.Severity,
		}
		if item.Source == "" {
			item.Source = "manual:" + strconv.FormatInt(actor.UserID, 10)
		}
		if body.PublishedAt != "" {
			if t, terr := time.Parse(time.RFC3339, body.PublishedAt); terr == nil {
				item.PublishedAt = &t
			}
		}
		if err := d.Screening.ReportAdverseMedia(r.Context(), sub, item); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"recorded": true})
	}
}
