// Phase-15 Tasks 15.3.12/15.3.13 — the listing/delisting + auction-
// calendar handler cluster (spec §7.5/§7.1, §24 #352/#401). Mounts:
//
//	GET  /api/v1/admin/listing-proposals                     list (?status=)
//	POST /api/v1/admin/listing-proposals                     propose
//	POST /api/v1/admin/listing-proposals/{id}/review         REVIEW|APPROVE|REJECT
//	GET  /api/v1/admin/ops-board                             consolidated board
//	GET  /api/v1/admin/instruments/{symbol}/auction-calendar calendar rows
//	PUT  /api/v1/admin/instruments/{symbol}/auction-calendar replace — dual control
//
// The APPROVE review leg and the calendar PUT are four-eyes surfaces:
// the handler files the dual-control request and returns 202; the second
// approver's POST /api/v1/admin/dual-control/{id}/approve executes the
// registered executor (DRAFT create + SCHEDULED / calendar full-replace)
// inside the approval transaction.
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"exchange/internal/admin"
	"exchange/internal/instruments"

	excerrors "exchange/pkg/errors"
)

// AdminListingProposalsList — GET /api/v1/admin/listing-proposals
// (?status=PROPOSED|IN_REVIEW|APPROVED|REJECTED|SCHEDULED).
func AdminListingProposalsList(svc *instruments.ListingService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, true); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		list, err := svc.List(r.Context(), r.URL.Query().Get("status"))
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"proposals": list})
	}
}

// AdminListingProposalCreate — POST /api/v1/admin/listing-proposals.
// The proposer submits the full reference row + oracle feed set +
// per-symbol risk defaults; auto-checks run at insert and are stored
// verbatim on the row.
func AdminListingProposalCreate(svc *instruments.ListingService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var in instruments.ProposalInput
		if !decodeJSONBody(w, r, &in) {
			return
		}
		p, err := svc.Propose(r.Context(), actor, in)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(p)
	}
}

// AdminListingProposalReview — POST .../listing-proposals/{id}/review.
// Body {action: REVIEW|APPROVE|REJECT, note?, activate_at?}. APPROVE
// files the OpInstrumentListing four-eyes request → 202; REVIEW/REJECT
// apply single-approver → 200.
func AdminListingProposalReview(svc *instruments.ListingService,
	trustProxy bool) http.HandlerFunc {
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
		var body struct {
			Action     string     `json:"action"`
			Note       string     `json:"note"`
			ActivateAt *time.Time `json:"activate_at"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		p, dc, err := svc.Review(r.Context(), actor, id, body.Action,
			body.Note, body.ActivateAt)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		if dc != nil {
			writePendingDual(w, dc, map[string]any{
				"proposal_id": p.ID, "symbol": p.Symbol,
				"proposal_status": p.Status,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(p)
	}
}

// AdminOpsBoard — GET /api/v1/admin/ops-board. Read-only consolidated
// board: non-ACTIVE instruments + grace deadlines, pending proposals,
// pending instrument-surface approvals, upcoming auctions, today's
// fixings and operational warnings.
func AdminOpsBoard(svc *instruments.OpsBoardService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, true); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		board, err := svc.Board(r.Context())
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(board)
	}
}

// AdminAuctionCalendarGet — GET /api/v1/admin/instruments/{symbol}/
// auction-calendar. Returns every row for the instrument (enabled and
// disabled — the ops view needs the full set).
func AdminAuctionCalendarGet(store *instruments.CalendarStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, true); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		symbol := r.PathValue("symbol")
		if symbol == "" {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "symbol required"))
			return
		}
		rows, err := store.CalendarFor(r.Context(), symbol)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"symbol": strings.ToUpper(symbol), "entries": rows})
	}
}

// AdminAuctionCalendarPut — PUT .../auction-calendar. Four-eyes surface
// (spec §7.5/Task 15.3.13): validates the full-replace body, then files
// OpInstrumentCalendar — the approval runs the replace + audit inside
// the dual-control transaction.
func AdminAuctionCalendarPut(dual *admin.DualControlService,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		symbol := strings.ToUpper(strings.TrimSpace(r.PathValue("symbol")))
		if symbol == "" {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "symbol required"))
			return
		}
		var body struct {
			Entries []instruments.CalendarEntry `json:"entries"`
			Reason  string                      `json:"reason"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		if len(body.Entries) == 0 {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST",
				"entries is required — an empty calendar requires an explicit [] body"))
			return
		}
		// Validate up front so a malformed calendar fails at request
		// time rather than inside the approval transaction.
		for i := range body.Entries {
			body.Entries[i].Symbol = symbol
			if err := body.Entries[i].Validate(); err != nil {
				writeSvcErr(w, r, err)
				return
			}
		}
		req, err := dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpInstrumentCalendar,
			TargetType:   "instrument",
			TargetID:     symbol,
			RequiredRole: admin.RoleRiskManager,
			RequestedBy:  actor.UserID,
			Reason:       body.Reason,
			ClientIP:     actor.ClientIP,
			Payload: map[string]any{
				"symbol":    symbol,
				"entries":   body.Entries,
				"client_ip": actor.ClientIP,
			},
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		writePendingDual(w, req, map[string]any{"symbol": symbol})
	}
}
