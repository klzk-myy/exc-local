// Handler tests — Phase-11 rails+returns cluster (Tasks 11.3.1/11.3.11).
// httptest + fake service seams; money paths live in the funding
// package's unit + integration tests.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/funding"
	excerrors "exchange/pkg/errors"
)

// --- fakes ---------------------------------------------------------------

type fakeRailMatrix struct{ caps []funding.RailCapability }

func (f fakeRailMatrix) Capabilities() []funding.RailCapability { return f.caps }

type fakeRailSelector struct {
	sel *funding.Selection
	err error
	got funding.SelectionRequest
}

func (f *fakeRailSelector) Select(_ context.Context, req funding.SelectionRequest) (*funding.Selection, error) {
	f.got = req
	return f.sel, f.err
}

type fakeScreener struct {
	res *funding.ScreenResult
	err error
	got funding.InboundWire

	resolveRes *funding.ResolveOutcome
	resolveErr error
	gotResolve struct {
		id           int64
		action       string
		investigator int64
		notes        string
	}
}

func (f *fakeScreener) ScreenInbound(_ context.Context, w funding.InboundWire) (*funding.ScreenResult, error) {
	f.got = w
	return f.res, f.err
}

func (f *fakeScreener) ResolveSuspense(_ context.Context, id int64,
	action string, investigatorID int64, notes string) (*funding.ResolveOutcome, error) {
	f.gotResolve.id, f.gotResolve.action = id, action
	f.gotResolve.investigator, f.gotResolve.notes = investigatorID, notes
	return f.resolveRes, f.resolveErr
}

type fakeSuspenseLister struct {
	rows  []funding.SuspenseRow
	total int64
	err   error
	got   funding.SuspenseFilter
}

func (f *fakeSuspenseLister) ListSuspense(_ context.Context, fl funding.SuspenseFilter) ([]funding.SuspenseRow, int64, error) {
	f.got = fl
	return f.rows, f.total, f.err
}

type fakeReturnApplier struct {
	out *funding.ReturnOutcome
	err error
	got struct {
		e2e, code, reason string
	}
}

func (f *fakeReturnApplier) ApplyReturn(_ context.Context, e2e, code, reason string) (*funding.ReturnOutcome, error) {
	f.got.e2e, f.got.code, f.got.reason = e2e, code, reason
	return f.out, f.err
}

func newRec() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// --- FundingRails ---------------------------------------------------------

func TestP11RailsMatrix(t *testing.T) {
	cut := 15 * 60
	src := fakeRailMatrix{caps: []funding.RailCapability{
		{Rail: "SWIFT", Name: "SWIFT", AllCurrencies: true,
			CutOffUTC: &cut, CutOffLabel: "15:00 UTC", SettlementLag: "T+1"},
	}}
	h := FundingRails(src)

	// Unauthenticated → 401.
	rec := newRec()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/funding/rails", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth: %d", rec.Code)
	}

	// Authenticated → 200 + rails payload.
	rec = newRec()
	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/v1/funding/rails", nil), 7)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rails: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Rails []funding.RailCapability `json:"rails"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rails) != 1 || body.Rails[0].Rail != "SWIFT" {
		t.Fatalf("payload %+v", body.Rails)
	}
}

// --- FundingRailSelection ---------------------------------------------------

func TestP11RailSelection(t *testing.T) {
	sel := &fakeRailSelector{sel: &funding.Selection{
		Rail: "FEDNOW", ValueDate: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
	}}
	h := FundingRailSelection(sel)

	// Happy path — 200 and the caller's account id flows into selection.
	rec := newRec()
	req := userCtx(httptest.NewRequest(http.MethodPost, "/api/v1/funding/rail-selection",
		strings.NewReader(`{"currency":"USD","amount":"1000"}`)), 42)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body)
	}
	if sel.got.AccountID != 42 || sel.got.Amount.String() != "1000" {
		t.Fatalf("request mapping %+v", sel.got)
	}

	// Malformed body → INVALID_REQUEST.
	rec = newRec()
	req = userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{not json`)), 42)
	h.ServeHTTP(rec, req)
	env := decodeErr(t, rec)
	if env.Error != "INVALID_REQUEST" || rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d %s", rec.Code, env.Error)
	}

	// Non-positive amount → INVALID_REQUEST.
	rec = newRec()
	req = userCtx(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"currency":"USD","amount":"-5"}`)), 42)
	h.ServeHTTP(rec, req)
	env = decodeErr(t, rec)
	if env.Error != "INVALID_REQUEST" {
		t.Fatalf("neg amount: %s", env.Error)
	}
}

func TestP11RailSelectionServiceErrs(t *testing.T) {
	sel := &fakeRailSelector{err: excerrors.New("BANKING_RAIL_UNAVAILABLE", "no rail")}
	h := FundingRailSelection(sel)
	mkReq := func() *http.Request {
		return userCtx(httptest.NewRequest(http.MethodPost, "/x",
			strings.NewReader(`{"currency":"USD","amount":"10"}`)), 42)
	}
	// BANKING_RAIL_UNAVAILABLE is registered 503.
	rec := newRec()
	h.ServeHTTP(rec, mkReq())
	env := decodeErr(t, rec)
	if env.Error != "BANKING_RAIL_UNAVAILABLE" || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("rail unavailable: %d %s", rec.Code, env.Error)
	}

	// RAIL_CUTOFF_EXCEEDED keeps its registered status (422).
	sel.err = excerrors.New("RAIL_CUTOFF_EXCEEDED", "past cut-off")
	rec = newRec()
	h.ServeHTTP(rec, mkReq())
	env = decodeErr(t, rec)
	if env.Error != "RAIL_CUTOFF_EXCEEDED" || env.Status != rec.Code {
		t.Fatalf("cutoff: %d %s", rec.Code, env.Error)
	}
}

// --- AdminInboundWire -------------------------------------------------------

func TestP11InboundWire(t *testing.T) {
	scr := &fakeScreener{res: &funding.ScreenResult{
		Disposition: funding.DispositionAccepted,
	}}
	h := AdminInboundWire(scr)

	// Unauthenticated → 401.
	rec := newRec()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth: %d", rec.Code)
	}

	// Accepted → 200, wire fields mapped.
	mkReq := func() *http.Request {
		return adminCtxB(httptest.NewRequest(http.MethodPost, "/x",
			strings.NewReader(`{"bank_tx_id":"B1","rail":"SEPA","currency":"EUR",
				"amount":"100","originator_name":"Jane","originator_account":"DE1",
				"reference":"EXC00000007-EUR"}`)))
	}
	rec = newRec()
	h.ServeHTTP(rec, mkReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	if scr.got.BankTxID != "B1" || scr.got.Amount.String() != "100" || scr.got.Rail != "SEPA" {
		t.Fatalf("wire mapping %+v", scr.got)
	}

	// Quarantined (unattributable) → 202, not an error envelope.
	scr.res = &funding.ScreenResult{Disposition: funding.DispositionQuarantined}
	rec = newRec()
	h.ServeHTTP(rec, mkReq())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("quarantined should be 202: %d", rec.Code)
	}

	// Name-mismatch → the registered 422 coded rejection.
	scr.res = nil
	scr.err = excerrors.New("THIRD_PARTY_DEPOSIT_REJECTED", "mismatch")
	rec = newRec()
	h.ServeHTTP(rec, mkReq())
	env := decodeErr(t, rec)
	if env.Error != "THIRD_PARTY_DEPOSIT_REJECTED" || rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("third-party: %d %s", rec.Code, env.Error)
	}

	// Bad amount → INVALID_REQUEST.
	rec = newRec()
	badReq := adminCtxB(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"bank_tx_id":"B2","currency":"EUR","amount":"abc"}`)))
	h.ServeHTTP(rec, badReq)
	env = decodeErr(t, rec)
	if env.Error != "INVALID_REQUEST" {
		t.Fatalf("bad amount: %s", env.Error)
	}
}

// --- AdminQuarantineList ----------------------------------------------------

func TestP11QuarantineList(t *testing.T) {
	rows := make([]funding.SuspenseRow, 3)
	for i := range rows {
		rows[i] = funding.SuspenseRow{ID: int64(i + 1), BankTxID: "b",
			QuarantinedAt: time.Now().UTC()}
	}
	l := &fakeSuspenseLister{rows: rows, total: 3}
	h := AdminQuarantineList(l)

	// Default page → 200, limit 50 passed through, items returned.
	rec := newRec()
	req := adminCtxB(httptest.NewRequest(http.MethodGet, "/x?status=QUARANTINED", nil))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if l.got.Status != "QUARANTINED" || l.got.Limit != 50 {
		t.Fatalf("filter %+v", l.got)
	}

	// limit > 200 rejected.
	rec = newRec()
	req = adminCtxB(httptest.NewRequest(http.MethodGet, "/x?limit=999", nil))
	h.ServeHTTP(rec, req)
	env := decodeErr(t, rec)
	if env.Error != "INVALID_REQUEST" {
		t.Fatalf("limit: %s", env.Error)
	}

	// Bad account_id rejected.
	rec = newRec()
	req = adminCtxB(httptest.NewRequest(http.MethodGet, "/x?account_id=-1", nil))
	h.ServeHTTP(rec, req)
	env = decodeErr(t, rec)
	if env.Error != "INVALID_REQUEST" {
		t.Fatalf("account_id: %s", env.Error)
	}
}

// --- AdminQuarantineResolve ---------------------------------------------------

func TestP11QuarantineResolveDualControl(t *testing.T) {
	scr := &fakeScreener{resolveRes: &funding.ResolveOutcome{
		SuspenseID: 9, Action: "RELEASE_TO_CLIENT", Status: "RESOLVED",
	}}
	h := AdminQuarantineResolve(scr)
	mkReq := func(body string) *http.Request {
		r := adminCtxB(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)))
		r.SetPathValue("id", "9")
		return r
	}

	// Missing approver → DUAL_CONTROL_REQUIRED.
	rec := newRec()
	h.ServeHTTP(rec, mkReq(`{"action":"RELEASE_TO_CLIENT"}`))
	env := decodeErr(t, rec)
	if env.Error != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("no approver: %s", env.Error)
	}

	// approver == actor (adminCtxB subject 500) → DUAL_CONTROL_VIOLATION.
	rec = newRec()
	h.ServeHTTP(rec, mkReq(`{"action":"RELEASE_TO_CLIENT","approver_id":500}`))
	env = decodeErr(t, rec)
	if env.Error != "DUAL_CONTROL_VIOLATION" {
		t.Fatalf("self-approve: %s", env.Error)
	}

	// Valid four-eyes resolve → 200, investigator = acting admin.
	rec = newRec()
	h.ServeHTTP(rec, mkReq(`{"action":"RELEASE_TO_CLIENT","approver_id":600,"notes":"ok"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	if scr.gotResolve.id != 9 || scr.gotResolve.investigator != 500 ||
		scr.gotResolve.action != "RELEASE_TO_CLIENT" {
		t.Fatalf("resolve mapping %+v", scr.gotResolve)
	}

	// Bad path id → INVALID_REQUEST.
	rec = newRec()
	req := adminCtxB(httptest.NewRequest(http.MethodPost, "/x",
		strings.NewReader(`{"action":"RETURN_TO_SOURCE","approver_id":600}`)))
	req.SetPathValue("id", "abc")
	h.ServeHTTP(rec, req)
	env = decodeErr(t, rec)
	if env.Error != "INVALID_REQUEST" {
		t.Fatalf("bad id: %s", env.Error)
	}
}

// --- AdminRailReturn ----------------------------------------------------------

func TestP11RailReturn(t *testing.T) {
	ap := &fakeReturnApplier{out: &funding.ReturnOutcome{
		FundingStatus: funding.FundingFailed,
	}}
	h := AdminRailReturn(ap)

	// Missing fields → INVALID_REQUEST.
	rec := newRec()
	req := adminCtxB(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"end_to_end_id":"E1"}`)))
	h.ServeHTTP(rec, req)
	env := decodeErr(t, rec)
	if env.Error != "INVALID_REQUEST" {
		t.Fatalf("missing code: %s", env.Error)
	}

	// Happy path → 200, args mapped.
	mkReq := func() *http.Request {
		return adminCtxB(httptest.NewRequest(http.MethodPost, "/x",
			strings.NewReader(`{"end_to_end_id":"E2E-9","return_code":"AC01","reason":"bad acct"}`)))
	}
	rec = newRec()
	h.ServeHTTP(rec, mkReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("return: %d %s", rec.Code, rec.Body)
	}
	if ap.got.e2e != "E2E-9" || ap.got.code != "AC01" {
		t.Fatalf("return mapping %+v", ap.got)
	}

	// Unknown instruction → NOT_FOUND propagates.
	ap.out = nil
	ap.err = excerrors.New("NOT_FOUND", "rail payment not found")
	rec = newRec()
	h.ServeHTTP(rec, mkReq())
	env = decodeErr(t, rec)
	if env.Error != "NOT_FOUND" {
		t.Fatalf("not found: %s", env.Error)
	}
}
