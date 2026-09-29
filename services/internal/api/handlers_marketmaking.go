// Task 18.3.10 — Market-Maker Program admin REST surface.
//
//	GET    /api/v1/admin/mm-programs                      list (?account_id=&status=)
//	POST   /api/v1/admin/mm-programs                      enroll (ACTIVE)
//	GET    /api/v1/admin/mm-programs/{id}                 detail
//	PUT    /api/v1/admin/mm-programs/{id}                 update obligations/MMP/rebate/OTR
//	POST   /api/v1/admin/mm-programs/{id}/suspend         {reason} — Risk-Manager review state
//	POST   /api/v1/admin/mm-programs/{id}/resume          reactivate
//	POST   /api/v1/admin/mm-programs/{id}/mmp-reset       clear MMP lockout (§24 #139 explicit reset)
//	GET    /api/v1/admin/mm-programs/{id}/compliance      daily rollup (?from=&to= YYYY-MM-DD)
//	GET    /api/v1/admin/mm-programs/{id}/rebates         accrual rows (?limit=)
//	POST   /api/v1/admin/mm-programs/rebates/post         sweep unposted accruals → GL (Finance Ops)
//
// Decimal fields ride JSON strings — never float money (spec §5.3).
// Role gates are the route-registry rows: enrollment/status under
// RoleRiskManager, the rebate GL sweep under RoleFinanceOps.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/gateway"
	"exchange/internal/marketmaking"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// mmProgramRequest is the enroll/update body. instrument_id null =
// program-wide row; otr_allowance null leaves venue/scoped ratios.
type mmProgramRequest struct {
	AccountID    int64   `json:"account_id"`
	InstrumentID *int64  `json:"instrument_id"`
	MinQuoteSize string  `json:"min_quote_size"`
	MaxSpreadBps string  `json:"max_spread_bps"`
	PresencePct  string  `json:"presence_pct"`
	MMPMaxFills  int     `json:"mmp_max_fills"`
	MMPWindowMs  int     `json:"mmp_window_ms"`
	RebateBps    string  `json:"rebate_bps"`
	OtrAllowance *string `json:"otr_allowance"`
}

func (r *mmProgramRequest) program() (*marketmaking.Program, error) {
	dec := func(field, raw string) (decimal.Decimal, error) {
		if raw == "" {
			return decimal.Zero, excerrors.New("INVALID_REQUEST", field+" required")
		}
		d, err := decimal.NewFromString(raw)
		if err != nil {
			return decimal.Zero, excerrors.New("INVALID_REQUEST",
				field+" must be a decimal string")
		}
		return d, nil
	}
	var err error
	p := &marketmaking.Program{
		AccountID:    r.AccountID,
		InstrumentID: r.InstrumentID,
		MMPMaxFills:  r.MMPMaxFills,
		MMPWindowMs:  r.MMPWindowMs,
	}
	if p.MinQuoteSize, err = dec("min_quote_size", r.MinQuoteSize); err != nil {
		return nil, err
	}
	if p.MaxSpreadBps, err = dec("max_spread_bps", r.MaxSpreadBps); err != nil {
		return nil, err
	}
	if p.PresencePct, err = dec("presence_pct", r.PresencePct); err != nil {
		return nil, err
	}
	if p.RebateBps, err = dec("rebate_bps", r.RebateBps); err != nil {
		return nil, err
	}
	if r.OtrAllowance != nil {
		v, err := dec("otr_allowance", *r.OtrAllowance)
		if err != nil {
			return nil, err
		}
		p.OtrAllowance = &v
	}
	return p, nil
}

func mmPathID(r *http.Request) (int64, error) {
	v, ok := parsePathID(r.PathValue("id"))
	if !ok {
		return 0, excerrors.New("INVALID_REQUEST", "id must be a positive integer")
	}
	return v, nil
}

// AdminMMProgramList serves GET /api/v1/admin/mm-programs.
func AdminMMProgramList(svc *marketmaking.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var accountID int64
		if raw := r.URL.Query().Get("account_id"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				writeSvcErr(w, r, excerrors.New("INVALID_REQUEST",
					"account_id must be a positive integer"))
				return
			}
			accountID = id
		}
		programs, err := svc.List(r.Context(), accountID,
			marketmaking.Status(r.URL.Query().Get("status")))
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"mm_programs": programs})
	}
}

// AdminMMProgramEnroll serves POST /api/v1/admin/mm-programs.
func AdminMMProgramEnroll(svc *marketmaking.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var req mmProgramRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := req.program()
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		if err := svc.Enroll(r.Context(), p); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, p)
	}
}

// AdminMMProgramGet serves GET /api/v1/admin/mm-programs/{id}.
func AdminMMProgramGet(svc *marketmaking.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := mmPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		p, err := svc.Get(r.Context(), id)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, p)
	}
}

// AdminMMProgramUpdate serves PUT /api/v1/admin/mm-programs/{id} —
// rewrites obligations/MMP/rebate/OTR parameters; status transitions go
// through the suspend/resume actions.
func AdminMMProgramUpdate(svc *marketmaking.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := mmPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var req mmProgramRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := req.program()
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		p.ID = id
		if err := svc.Update(r.Context(), p); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, p)
	}
}

// AdminMMProgramSuspend serves POST …/{id}/suspend {reason}.
func AdminMMProgramSuspend(svc *marketmaking.Service, trustProxy bool) http.HandlerFunc {
	return mmStatusAction(svc, trustProxy, true)
}

// AdminMMProgramResume serves POST …/{id}/resume.
func AdminMMProgramResume(svc *marketmaking.Service, trustProxy bool) http.HandlerFunc {
	return mmStatusAction(svc, trustProxy, false)
}

func mmStatusAction(svc *marketmaking.Service, trustProxy bool, suspend bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := mmPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // optional payload
		if suspend {
			if body.Reason == "" {
				body.Reason = "admin suspend"
			}
			err = svc.Suspend(r.Context(), id, body.Reason)
		} else {
			err = svc.Resume(r.Context(), id)
		}
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		p, err := svc.Get(r.Context(), id)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, p)
	}
}

// AdminMMProgramMMPReset serves POST …/{id}/mmp-reset — the §24 #139
// explicit reset clearing the MMP lockout + fill window. programID
// carries the program row so the tracker needs no account/instrument
// resolution.
func AdminMMProgramMMPReset(tracker *marketmaking.MMPTracker,
	trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		if tracker == nil {
			WriteError(w, "SERVICE_DEGRADED", "mmp tracker not wired",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, err := mmPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		tracker.ResetMMPProgram(id)
		WriteJSON(w, http.StatusOK, map[string]any{
			"program_id": id, "mmp_lockout": "cleared",
		})
	}
}

// AdminMMProgramCompliance serves GET …/{id}/compliance?from=&to= —
// daily rollup rows; defaults to the trailing 7 UTC days.
func AdminMMProgramCompliance(svc *marketmaking.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := mmPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		q := r.URL.Query()
		now := time.Now().UTC()
		to := now.Truncate(24 * time.Hour)
		from := to.AddDate(0, 0, -7)
		dayFmt := "2006-01-02"
		if raw := q.Get("from"); raw != "" {
			if from, err = time.ParseInLocation(dayFmt, raw, time.UTC); err != nil {
				writeSvcErr(w, r, excerrors.New("INVALID_REQUEST",
					"from must be YYYY-MM-DD"))
				return
			}
		}
		if raw := q.Get("to"); raw != "" {
			if to, err = time.ParseInLocation(dayFmt, raw, time.UTC); err != nil {
				writeSvcErr(w, r, excerrors.New("INVALID_REQUEST",
					"to must be YYYY-MM-DD"))
				return
			}
		}
		if to.Before(from) {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "to before from"))
			return
		}
		rows, err := svc.Compliance(r.Context(), id, from, to)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"compliance": rows})
	}
}

// AdminMMProgramRebates serves GET …/{id}/rebates?limit=.
func AdminMMProgramRebates(svc *marketmaking.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := mmPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		limit := 200
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, perr := strconv.Atoi(raw); perr == nil {
				limit = n
			}
		}
		rows, err := svc.Rebates(r.Context(), id, limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"rebates": rows})
	}
}

// AdminMMRebatePost serves POST /api/v1/admin/mm-programs/rebates/post —
// the monthly GL sweep (Finance Ops): one balanced journal per
// (account, currency) posted through the ledger path.
func AdminMMRebatePost(svc *marketmaking.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := adminActorFrom(r, trustProxy); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		posted, err := svc.PostAccruedRebates(r.Context())
		if err != nil && len(posted) == 0 {
			writeSvcErr(w, r, err)
			return
		}
		resp := map[string]any{"posted_journals": posted}
		if err != nil {
			// Partial sweep: report what landed plus the first failure.
			resp["partial_error"] = err.Error()
		}
		WriteJSON(w, http.StatusOK, resp)
	}
}
