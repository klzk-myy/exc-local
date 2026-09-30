// Handler tests — Phase-21 regulatory reporting admin surface
// (Tasks 21.3.4/.5/.9/.14/.16): regime-pinned event export, event
// detail, break resolution, corrected resubmission, async ACK ingest,
// party-identifier upsert (LEI checksum gate), and the reconcile seam.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/compliance/reporting"
	"exchange/internal/compliance/reporting/reporttest"
)

// checksum-valid test LEI (ISO 17442 mod-97-2 verified).
const regTestLEI = "TESTTESTTESTTEST0038"

var regNow = time.Date(2024, 6, 3, 10, 0, 0, 0, time.UTC)

func regDeps(t *testing.T) (RegReportingDeps, *reporttest.FakeStore) {
	t.Helper()
	fs := reporttest.NewFakeStore(regNow)
	fs.SeedSchemas()
	svc, err := reporting.NewService(fs, reporting.Config{
		VenueLEI: regTestLEI, VenueMIC: "XEXC", USINamespace: "EXC",
		Now: func() time.Time { return regNow },
		DerivativeEnrich: func(_ context.Context, tc *reporting.TradeContext) (string, json.RawMessage, json.RawMessage, error) {
			return "", json.RawMessage(`{"mark_price":"1.0850"}`),
				json.RawMessage(`{"collateralisation":"UNCOLLATERALISED"}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return RegReportingDeps{Svc: svc, TrustProxy: true}, fs
}

func regAdminReq(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(auth.WithClaims(req.Context(),
		auth.Claims{Subject: "500", AccountID: 9, Scopes: []string{"admin"}}))
}

func regAnonReq(method, path, body string) *http.Request {
	return httptest.NewRequest(method, path, strings.NewReader(body))
}

func seedMiFIDEvent(t *testing.T, svc *reporting.Service) *reporting.Event {
	t.Helper()
	evs, err := svc.RecordExecution(context.Background(), &reporting.TradeContext{
		TradeID: 42, InstrumentID: 1, InstrumentCode: "EUR/USD",
		InstrumentType: "SPOT", BaseCurrency: "EUR", QuoteCurrency: "USD",
		BuyerAccountID: 11, SellerAccountID: 22, BuyerUserID: 501,
		SellerUserID: 502, Price: "1.0850", Quantity: "1000000",
		ExecutedAt: regNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return evs[0]
}

func TestAdminRegEvents_ForceRegime(t *testing.T) {
	d, _ := regDeps(t)
	seedMiFIDEvent(t, d.Svc)
	d.ForceRegime = reporting.RegimeMIFID2

	// Even a caller asking for a different regime gets the pinned feed.
	rec := httptest.NewRecorder()
	AdminRegEvents(d)(rec, regAnonReq(http.MethodGet,
		"/api/v1/admin/emir-report?regime=EMIR_REFIT", ""))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Events []reporting.Event `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 || out.Events[0].Regime != reporting.RegimeMIFID2 {
		t.Fatalf("forced regime broken: %+v", out.Events)
	}
}

func TestAdminRegEvents_RequiresRegime(t *testing.T) {
	d, _ := regDeps(t)
	rec := httptest.NewRecorder()
	AdminRegEvents(d)(rec, regAnonReq(http.MethodGet, "/api/v1/admin/regreporting/events", ""))
	if rec.Code == 200 {
		t.Fatal("regime-less query must fail")
	}
	if env := decodeErr(t, rec); env.Error != "INVALID_REQUEST" {
		t.Fatalf("code: %+v", env.Error)
	}
}

func TestAdminRegEventDetail(t *testing.T) {
	d, _ := regDeps(t)
	e := seedMiFIDEvent(t, d.Svc)

	rec := httptest.NewRecorder()
	req := regAnonReq(http.MethodGet, "/api/v1/admin/regreporting/events/", "")
	req.SetPathValue("id", "42")
	AdminRegEventDetail(d)(rec, req)
	if rec.Code == 200 {
		t.Fatal("unknown event must 404")
	}

	rec = httptest.NewRecorder()
	req = regAnonReq(http.MethodGet, "/api/v1/admin/regreporting/events/", "")
	req.SetPathValue("id", "1")
	AdminRegEventDetail(d)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Event       reporting.Event        `json:"event"`
		Submissions []reporting.Submission `json:"submissions"`
		Acks        []reporting.Ack        `json:"acks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Event.EventID != e.EventID || len(out.Submissions) != 2 {
		t.Fatalf("detail: %+v", out)
	}
}

func TestAdminRegBreakResolve(t *testing.T) {
	d, fs := regDeps(t)
	// Seed a quarantined event → VALIDATION break exists.
	bad := &reporting.TradeContext{
		TradeID: 7, InstrumentID: 1, InstrumentCode: "EUR/USD",
		InstrumentType: "SPOT", QuoteCurrency: "", // missing → quarantine
		Price: "1.0", Quantity: "1", ExecutedAt: regNow,
		BuyerAccountID: 11, SellerAccountID: 22,
	}
	if _, err := d.Svc.RecordExecution(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	if len(fs.Breaks) == 0 {
		t.Fatal("expected break")
	}

	// Anonymous → UNAUTHORIZED.
	rec := httptest.NewRecorder()
	req := regAnonReq(http.MethodPost, "/", `{"resolution":"RESOLVED"}`)
	req.SetPathValue("id", "1")
	AdminRegBreakResolve(d)(rec, req)
	if rec.Code == 200 {
		t.Fatal("anonymous must fail closed")
	}

	// Admin RESOLVED.
	rec = httptest.NewRecorder()
	req = regAdminReq(t, http.MethodPost, "/",
		`{"resolution":"RESOLVED","notes":"fixed upstream"}`)
	req.SetPathValue("id", "1")
	AdminRegBreakResolve(d)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if fs.Breaks[0].Status != reporting.BreakResolved || fs.Breaks[0].ResolvedBy != 500 {
		t.Fatalf("break: %+v", fs.Breaks[0])
	}
	// Idempotent — already resolved reports applied=false.
	rec = httptest.NewRecorder()
	req = regAdminReq(t, http.MethodPost, "/", `{"resolution":"RESOLVED"}`)
	req.SetPathValue("id", "1")
	AdminRegBreakResolve(d)(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["applied"] != false {
		t.Fatalf("re-resolve must be a no-op: %v", out)
	}
	// Bogus resolution → 400.
	rec = httptest.NewRecorder()
	req = regAdminReq(t, http.MethodPost, "/", `{"resolution":"MAYBE"}`)
	req.SetPathValue("id", "2")
	AdminRegBreakResolve(d)(rec, req)
	if rec.Code == 200 {
		t.Fatal("bad resolution must fail")
	}
}

func TestAdminRegResubmit(t *testing.T) {
	d, fs := regDeps(t)
	e := seedMiFIDEvent(t, d.Svc)
	subs, _ := fs.SubmissionsForEvent(context.Background(), e.EventID)
	// NACK the ARM artifact first.
	if _, err := d.Svc.IngestAck(context.Background(), reporting.Ack{
		ReportSubmissionID: subs[0].ReportSubmissionID,
		EventID:            e.EventID, AckStatus: reporting.AckReject,
	}); err != nil {
		t.Fatal(err)
	}
	// Corrected resubmission → 201 + corrected artifact.
	rec := httptest.NewRecorder()
	req := regAdminReq(t, http.MethodPost, "/", `{"corrections":{"price":"1.0855"}}`)
	req.SetPathValue("id", "1")
	AdminRegResubmit(d)(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// Non-repairable field → 4xx envelope.
	rec = httptest.NewRecorder()
	req = regAdminReq(t, http.MethodPost, "/", `{"corrections":{"account_id":1}}`)
	req.SetPathValue("id", "1")
	AdminRegResubmit(d)(rec, req)
	if rec.Code == http.StatusCreated {
		t.Fatal("non-whitelisted field must be rejected")
	}
	// Missing body → 400.
	rec = httptest.NewRecorder()
	req = regAdminReq(t, http.MethodPost, "/", `{}`)
	req.SetPathValue("id", "1")
	AdminRegResubmit(d)(rec, req)
	if rec.Code == 200 {
		t.Fatal("empty corrections must fail")
	}
}

func TestAdminRegAckIngest(t *testing.T) {
	d, fs := regDeps(t)
	e := seedMiFIDEvent(t, d.Svc)
	subs, _ := fs.SubmissionsForEvent(context.Background(), e.EventID)

	// NACK → break surfaced in the response.
	rec := httptest.NewRecorder()
	req := regAdminReq(t, http.MethodPost, "/api/v1/admin/regreporting/acks",
		`{"report_submission_id":`+itoa64(subs[0].ReportSubmissionID)+
			`,"status":"NACK","code":"E55","text":"bad field"}`)
	AdminRegAckIngest(d)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Break *reporting.Break `json:"break"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Break == nil || out.Break.BreakType != reporting.BreakNackRepair {
		t.Fatalf("break: %+v", out.Break)
	}
	// Bad status → 400.
	rec = httptest.NewRecorder()
	req = regAdminReq(t, http.MethodPost, "/", `{"report_submission_id":1,"status":"MEH"}`)
	AdminRegAckIngest(d)(rec, req)
	if rec.Code == 200 {
		t.Fatal("bad status must fail")
	}
	// Unknown artifact → 404.
	rec = httptest.NewRecorder()
	req = regAdminReq(t, http.MethodPost, "/", `{"report_submission_id":999,"status":"ACK"}`)
	AdminRegAckIngest(d)(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestAdminRegPartyUpsert(t *testing.T) {
	d, fs := regDeps(t)
	// Bad LEI checksum → 400.
	rec := httptest.NewRecorder()
	req := regAdminReq(t, http.MethodPost, "/",
		`{"account_id":11,"lei":"TESTTESTTESTTEST0099"}`)
	AdminRegPartyUpsert(d)(rec, req)
	if rec.Code == 200 {
		t.Fatal("bad LEI must fail")
	}
	// Valid LEI → stored.
	rec = httptest.NewRecorder()
	req = regAdminReq(t, http.MethodPost, "/",
		`{"account_id":11,"lei":"`+regTestLEI+`","decision_maker_id":"DM-1"}`)
	AdminRegPartyUpsert(d)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if fs.Parties[11] == nil || fs.Parties[11].LEI != regTestLEI {
		t.Fatal("party not stored")
	}
}

func TestAdminRegReconcile(t *testing.T) {
	d, _ := regDeps(t)
	rec := httptest.NewRecorder()
	req := regAdminReq(t, http.MethodPost, "/", `{"regime":"EMIR_REFIT"}`)
	AdminRegReconcile(d)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// Unsupported regime → error envelope.
	rec = httptest.NewRecorder()
	req = regAdminReq(t, http.MethodPost, "/", `{"regime":"MIFID2"}`)
	AdminRegReconcile(d)(rec, req)
	if rec.Code == 200 {
		t.Fatal("MIFID2 reconcile must fail (derivative regimes only)")
	}
}
