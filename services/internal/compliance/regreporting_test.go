// Unit tests for the Phase-21 reporting cluster wiring (Tasks 21.3.4/.5/
// .9/.16): verdict parsing, fail-closed client construction, dispatcher
// lifecycle (ACK/NACK/transport-error/unconfigured endpoint), and the
// regime reporter façades over reporting.Service.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/compliance/reporting"
	"exchange/internal/compliance/reporting/reporttest"
	"exchange/internal/oracle/rates"
)

var regTestNow = time.Date(2024, 6, 3, 10, 0, 0, 0, time.UTC)

// regTestLEI mints a checksum-valid ISO 17442 LEI for tests.
func regTestLEI(t *testing.T, body18 string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range body18 + "00" {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "%d", r-'A'+10)
		}
	}
	rem := 0
	for _, r := range b.String() {
		rem = (rem*10 + int(r-'0')) % 97
	}
	return fmt.Sprintf("%s%02d", body18, 98-rem)
}

func regSvc(t *testing.T, fs *reporttest.FakeStore) *reporting.Service {
	t.Helper()
	svc, err := reporting.NewService(fs, reporting.Config{
		VenueLEI: regTestLEI(t, "TESTTESTTESTTEST00"), VenueMIC: "XEXC",
		USINamespace: "EXC", DualSided: true,
		RepairSLA: 2 * time.Hour, StaleAfter: 24 * time.Hour,
		Now: func() time.Time { return regTestNow },
		DerivativeEnrich: func(_ context.Context, tc *reporting.TradeContext) (
			string, json.RawMessage, json.RawMessage, error) {
			return "", json.RawMessage(`{"mark_price":"1.0850"}`),
				json.RawMessage(`{"collateralisation":"UNCOLLATERALISED"}`), nil
		},
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc
}

func regFwdTrade(tradeID int64, buyerJur, sellerJur string) *reporting.TradeContext {
	return &reporting.TradeContext{
		TradeID: tradeID, InstrumentID: 1, InstrumentCode: "EUR/USD",
		InstrumentType: "FORWARD", BaseCurrency: "EUR", QuoteCurrency: "USD",
		BuyerAccountID: 11, SellerAccountID: 22,
		BuyerUserID: 501, SellerUserID: 502,
		BuyerJurisdiction: buyerJur, SellerJurisdiction: sellerJur,
		Price: "1.0850", Quantity: "1000000", ExecutedAt: regTestNow,
	}
}

// ---------------------------------------------------------------------------
// Verdict parsing (sync ACK + async NACK seam)
// ---------------------------------------------------------------------------

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string // "ack"|"nack"|"async"|"err"
		code   string
	}{
		{"json_ack", 200, `{"status":"ACK","ref":"R1"}`, "ack", ""},
		{"json_nack", 200, `{"status":"NACK","code":"E12","message":"bad"}`, "nack", "E12"},
		{"xml_ack", 200, `<Ack><Status>ACK</Status><Ref>X9</Ref></Ack>`, "ack", ""},
		{"async_202", 202, ``, "async", ""},
		{"async_empty", 200, ``, "async", ""},
		{"http_4xx_nack", 422, `unprocessable`, "nack", "HTTP_422"},
		{"http_5xx_err", 503, `down`, "err", ""},
		{"unknown_status", 200, `{"status":"MAYBE"}`, "nack", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := parseVerdict(c.status, []byte(c.body))
			switch c.want {
			case "err":
				if err == nil {
					t.Fatal("expected transport error")
				}
			case "async":
				if err != nil || v.Status != nil {
					t.Fatalf("expected async, got %+v err=%v", v, err)
				}
			case "ack":
				if err != nil || v.Status == nil || *v.Status != reporting.AckAccept {
					t.Fatalf("expected ACK, got %+v err=%v", v, err)
				}
			case "nack":
				if err != nil || v.Status == nil || *v.Status != reporting.AckReject {
					t.Fatalf("expected NACK, got %+v err=%v", v, err)
				}
				if c.code != "" && v.Code != c.code {
					t.Fatalf("code: %s", v.Code)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Fail-closed client construction + wire shape
// ---------------------------------------------------------------------------

func TestVendorClients_FailClosed(t *testing.T) {
	if _, err := NewAPAClient("", vendorHTTPOpts{}); err == nil {
		t.Fatal("APA without endpoint must fail")
	}
	if _, err := NewARMClient("", vendorHTTPOpts{}); err == nil {
		t.Fatal("ARM without endpoint must fail")
	}
	if _, err := NewTRClient("", vendorHTTPOpts{}); err == nil {
		t.Fatal("TR without endpoint must fail")
	}
	if _, err := NewSDRClient("", vendorHTTPOpts{}); err == nil {
		t.Fatal("SDR without endpoint must fail")
	}
}

func TestAPAClient_Submit(t *testing.T) {
	var gotAuth, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ACK","ref":"APA-77"}`)
	}))
	defer srv.Close()

	c, err := NewAPAClient(srv.URL, vendorHTTPOpts{AuthToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.Submit(context.Background(), &reporting.Submission{
		ReportSubmissionID: 9, Payload: []byte(`{"price":"1.0850"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status == nil || *v.Status != reporting.AckAccept || v.Ref != "APA-77" {
		t.Fatalf("verdict: %+v", v)
	}
	if gotAuth != "Bearer tok" || gotCT != "application/json" {
		t.Fatalf("headers: %q %q", gotAuth, gotCT)
	}
	if !strings.Contains(gotBody, "1.0850") {
		t.Fatalf("body: %s", gotBody)
	}
}

func TestARMClient_SubmitXML(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusAccepted) // async
	}))
	defer srv.Close()

	c, err := NewARMClient(srv.URL, vendorHTTPOpts{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.Submit(context.Background(), &reporting.Submission{
		ReportSubmissionID: 5, PayloadXML: `<Document/>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != nil {
		t.Fatal("202 must leave verdict pending (async)")
	}
	if gotBody != "<Document/>" {
		t.Fatalf("wire body: %s", gotBody)
	}
}

// ---------------------------------------------------------------------------
// Dispatcher lifecycle
// ---------------------------------------------------------------------------

type fakeTransportLedger struct {
	rows       []*TransportRow
	seq        int64
	responded  map[int64]int // transport id → http status logged
	lastFailed *TransportRow
}

func (l *fakeTransportLedger) Insert(_ context.Context, r *TransportRow) error {
	l.seq++
	cp := *r
	cp.ID = l.seq
	l.rows = append(l.rows, &cp)
	r.ID = cp.ID
	return nil
}
func (l *fakeTransportLedger) LatestFor(_ context.Context, subID int64) (*TransportRow, error) {
	var best *TransportRow
	for _, r := range l.rows {
		if r.ReportSubmissionID == subID && (best == nil || r.Attempt > best.Attempt) {
			best = r
		}
	}
	return best, nil
}
func (l *fakeTransportLedger) AppendResponse(_ context.Context, id int64, httpStatus int,
	_ []byte, _ string) error {
	l.responded[id] = httpStatus
	return nil
}
func (l *fakeTransportLedger) SetVerdict(_ context.Context, id int64, ackStatus, code, detail,
	ref string, at time.Time) error {
	for _, r := range l.rows {
		if r.ID == id {
			r.AckStatus = ackStatus
			r.ErrorCode, r.ErrorDetail, r.ExternalRef = code, detail, ref
			r.ResolvedAt = &at
			return nil
		}
	}
	return fmt.Errorf("transport %d not found", id)
}
func (l *fakeTransportLedger) FailAttempt(_ context.Context, id int64, code, detail string,
	next, at time.Time) error {
	for _, r := range l.rows {
		if r.ID == id {
			r.AckStatus = TkFailed
			r.ErrorCode, r.ErrorDetail = code, detail
			r.NextAttemptAt, r.LastErrorAt = &next, &at
			l.lastFailed = r
			return nil
		}
	}
	return fmt.Errorf("transport %d not found", id)
}

func newDispatcherTest(t *testing.T) (*reporting.Service, *reporttest.FakeStore,
	*fakeTransportLedger, *reporting.Submission, *reporting.Event) {
	fs := reporttest.NewFakeStore(regTestNow)
	fs.SeedSchemas()
	svc := regSvc(t, fs)
	// One spot execution → MIFID2 event + ARM + APA pending artifacts.
	evs, err := svc.RecordExecution(context.Background(), &reporting.TradeContext{
		TradeID: 77, InstrumentID: 1, InstrumentCode: "EUR/USD",
		InstrumentType: "SPOT", BaseCurrency: "EUR", QuoteCurrency: "USD",
		BuyerAccountID: 11, SellerAccountID: 22, BuyerUserID: 501,
		SellerUserID: 502, Price: "1.0850", Quantity: "1000000",
		ExecutedAt: regTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Status != reporting.EventValidated {
		t.Fatalf("setup event %s", evs[0].Status)
	}
	subs, _ := fs.SubmissionsForEvent(context.Background(), evs[0].EventID)
	var arm *reporting.Submission
	for i := range subs {
		if subs[i].Destination == reporting.DestinationARM {
			arm = &subs[i]
		}
	}
	if arm == nil {
		t.Fatal("setup: ARM artifact missing")
	}
	return svc, fs, &fakeTransportLedger{responded: map[int64]int{}}, arm, evs[0]
}

func TestDispatcher_ACK(t *testing.T) {
	svc, fs, led, arm, ev := newDispatcherTest(t)
	mock := &MockVendorClient{Dest: reporting.DestinationARM}
	d := &SubmissionDispatcher{Svc: svc, Ledger: led,
		Clients: map[reporting.Destination]VendorClient{reporting.DestinationARM: mock}}
	n, err := d.DispatchOnce(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("dispatched %d", n)
	}
	// Artifact ACKed, event ACCEPTED, transport row ACK + wire IN logged.
	sub, _ := fs.SubmissionByID(context.Background(), arm.ReportSubmissionID)
	if sub.Status != reporting.SubAcked {
		t.Fatalf("sub status %s", sub.Status)
	}
	row, _ := led.LatestFor(context.Background(), arm.ReportSubmissionID)
	if row.AckStatus != TkAcked || row.ExternalRef == "" {
		t.Fatalf("transport: %+v", row)
	}
	if led.responded[row.ID] != 200 {
		t.Fatal("wire IN log missing")
	}
	got, _ := fs.EventByID(context.Background(), ev.EventID)
	if got.Status != reporting.EventAccepted {
		t.Fatalf("event %s", got.Status)
	}
}

func TestDispatcher_NACKOpensRepair(t *testing.T) {
	svc, fs, led, arm, _ := newDispatcherTest(t)
	nack := reporting.AckReject
	mock := &MockVendorClient{Dest: reporting.DestinationARM,
		Fn: func(_ context.Context, _ *reporting.Submission) (*VendorVerdict, error) {
			return &VendorVerdict{Status: &nack, Code: "E500", Text: "schema"}, nil
		}}
	d := &SubmissionDispatcher{Svc: svc, Ledger: led,
		Clients: map[reporting.Destination]VendorClient{reporting.DestinationARM: mock}}
	if _, err := d.DispatchOnce(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	row, _ := led.LatestFor(context.Background(), arm.ReportSubmissionID)
	if row.AckStatus != TkNacked {
		t.Fatalf("transport: %s", row.AckStatus)
	}
	if !fs.OpenBreakTypes()[reporting.BreakNackRepair] {
		t.Fatal("NACK_REPAIR break missing")
	}
	if fs.Events[0].Status != reporting.EventRejected {
		t.Fatalf("event %s", fs.Events[0].Status)
	}
}

func TestDispatcher_TransportError_Backoff(t *testing.T) {
	svc, fs, led, arm, _ := newDispatcherTest(t)
	mock := &MockVendorClient{Dest: reporting.DestinationARM,
		Fn: func(_ context.Context, _ *reporting.Submission) (*VendorVerdict, error) {
			return nil, fmt.Errorf("connection refused")
		}}
	d := &SubmissionDispatcher{Svc: svc, Ledger: led,
		Clients: map[reporting.Destination]VendorClient{reporting.DestinationARM: mock},
		Now:     func() time.Time { return regTestNow }}
	if _, err := d.DispatchOnce(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	row, _ := led.LatestFor(context.Background(), arm.ReportSubmissionID)
	if row.AckStatus != TkFailed || row.ErrorCode != "TRANSPORT_ERROR" {
		t.Fatalf("transport: %+v", row)
	}
	if row.NextAttemptAt == nil || !row.NextAttemptAt.After(regTestNow) {
		t.Fatal("retry backoff not scheduled")
	}
	// The artifact stays PENDING (store-and-forward) — never silently lost.
	sub, _ := fs.SubmissionByID(context.Background(), arm.ReportSubmissionID)
	if sub.Status != reporting.SubPending {
		t.Fatalf("sub status %s", sub.Status)
	}
}

func TestDispatcher_UnconfiguredEndpoint_FailClosed(t *testing.T) {
	svc, _, led, arm, _ := newDispatcherTest(t)
	var alerts []string
	d := &SubmissionDispatcher{Svc: svc, Ledger: led,
		Clients: map[reporting.Destination]VendorClient{},
		Alert: func(_ context.Context, sev, code, summary string) error {
			alerts = append(alerts, sev+":"+code)
			return nil
		}}
	n, err := d.DispatchOnce(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("nothing may be marked dispatched without a client")
	}
	row, _ := led.LatestFor(context.Background(), arm.ReportSubmissionID)
	if row == nil || row.ErrorCode != "ENDPOINT_UNCONFIGURED" {
		t.Fatalf("transport: %+v", row)
	}
	if len(alerts) == 0 || !strings.Contains(alerts[0], "P1") {
		t.Fatalf("P1 alert missing: %v", alerts)
	}
	// Second sweep must not re-ledger spam for the same artifact's defect
	// (both ARM+APA rows land once each — dedupe is per artifact).
	if _, err := d.DispatchOnce(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var fails int
	for _, r := range led.rows {
		if r.ErrorCode == "ENDPOINT_UNCONFIGURED" &&
			r.ReportSubmissionID == arm.ReportSubmissionID {
			fails++
		}
	}
	if fails != 1 {
		t.Fatalf("endpoint defect ledgered %d times for artifact", fails)
	}
}

func TestDispatcher_AsyncPending(t *testing.T) {
	svc, fs, led, arm, _ := newDispatcherTest(t)
	mock := &MockVendorClient{Dest: reporting.DestinationARM,
		Fn: func(_ context.Context, _ *reporting.Submission) (*VendorVerdict, error) {
			return &VendorVerdict{HTTPStatus: 202, Ref: "ARM-1"}, nil // async
		}}
	d := &SubmissionDispatcher{Svc: svc, Ledger: led,
		Clients: map[reporting.Destination]VendorClient{reporting.DestinationARM: mock}}
	if _, err := d.DispatchOnce(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	row, _ := led.LatestFor(context.Background(), arm.ReportSubmissionID)
	if row.AckStatus != TkSubmitted || row.ExternalRef != "ARM-1" {
		t.Fatalf("async row: %+v", row)
	}
	sub, _ := fs.SubmissionByID(context.Background(), arm.ReportSubmissionID)
	if sub.Status != reporting.SubSubmitted {
		t.Fatalf("sub %s", sub.Status)
	}
	// Async NACK arrives on the callback seam (spec §14.5).
	if _, err := svc.IngestAck(context.Background(), reporting.Ack{
		ReportSubmissionID: sub.ReportSubmissionID, EventID: sub.EventID,
		AckStatus: reporting.AckReject, AckCode: "E9", ExternalRef: "ARM-1",
	}); err != nil {
		t.Fatal(err)
	}
	if !fs.OpenBreakTypes()[reporting.BreakNackRepair] {
		t.Fatal("async NACK must open repair break")
	}
}

// ---------------------------------------------------------------------------
// Regime reporter façades
// ---------------------------------------------------------------------------

func TestEMIRReporter(t *testing.T) {
	fs := reporttest.NewFakeStore(regTestNow)
	fs.SeedSchemas()
	svc := regSvc(t, fs)

	rep, err := NewEMIRReporter(svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Spot rejected — non-derivative.
	spot := regFwdTrade(1, "DE", "GB")
	spot.InstrumentType = "SPOT"
	if _, err := rep.ReportDerivative(context.Background(), spot); err == nil {
		t.Fatal("spot must be rejected")
	}
	// FORWARD → EMIR_REFIT event, dual-sided on.
	e, err := rep.ReportDerivative(context.Background(), regFwdTrade(2, "DE", "GB"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Regime != reporting.RegimeEMIRREFIT || !e.DualSided {
		t.Fatalf("emir event: %+v", e)
	}
	// Lifecycle: TERM then ERRO.
	if _, err := rep.Cancel(context.Background(), e.UTI, regTestNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := rep.ReportError(context.Background(), e.UTI, regTestNow.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	latest, _ := fs.LatestEvent(context.Background(), e.UTI, reporting.RegimeEMIRREFIT)
	if latest.ReportSeq != 3 || latest.Action != reporting.ActionError {
		t.Fatalf("chain: seq=%d action=%s", latest.ReportSeq, latest.Action)
	}
}

func TestEMIRReporter_EnrichNEWT(t *testing.T) {
	fs := reporttest.NewFakeStore(regTestNow)
	fs.SeedSchemas()
	svc := regSvc(t, fs)

	// Oracle healthy → FWD_POINTS_ORACLE valuation block.
	rep, _ := NewEMIRReporter(svc, func(_ context.Context, pair string) (rates.SwapPoint, error) {
		if pair != "EUR/USD" {
			return rates.SwapPoint{}, fmt.Errorf("no pair %s", pair)
		}
		return rates.SwapPoint{Pair: pair, Long: decimal.NewFromFloat(0.0002),
			Short: decimal.NewFromFloat(-0.0003), AsOf: regTestNow,
			Source: "Refinitiv"}, nil
	})
	notional, val, mrg, err := rep.EnrichNEWT(context.Background(), regFwdTrade(3, "DE", "GB"))
	if err != nil {
		t.Fatal(err)
	}
	if notional != "" { // enricher defers notional to trade arithmetic
		t.Fatalf("notional: %q", notional)
	}
	if !strings.Contains(string(val), "FWD_POINTS_ORACLE") ||
		!strings.Contains(string(val), "0.0002") {
		t.Fatalf("valuation: %s", val)
	}
	if !strings.Contains(string(mrg), "UNCOLLATERALISED") {
		t.Fatalf("margin: %s", mrg)
	}
	// Oracle down → fail closed (error, no fabricated valuation).
	rep2, _ := NewEMIRReporter(svc, func(_ context.Context, _ string) (rates.SwapPoint, error) {
		return rates.SwapPoint{}, fmt.Errorf("stale")
	})
	if _, _, _, err := rep2.EnrichNEWT(context.Background(), regFwdTrade(4, "DE", "GB")); err == nil {
		t.Fatal("stale oracle must fail closed")
	}
}

func TestDoddFrankReporter(t *testing.T) {
	fs := reporttest.NewFakeStore(regTestNow)
	fs.SeedSchemas()
	svc := regSvc(t, fs)
	rep, err := NewDoddFrankReporter(svc)
	if err != nil {
		t.Fatal(err)
	}
	// No US nexus → no CFTC events.
	p43, p45, err := rep.ReportSwap(context.Background(), regFwdTrade(5, "DE", "GB"))
	if err != nil {
		t.Fatal(err)
	}
	if p43 != nil || p45 != nil {
		t.Fatal("non-US trade must not file CFTC")
	}
	// US buyer → both parts.
	p43, p45, err = rep.ReportSwap(context.Background(), regFwdTrade(6, "US", "GB"))
	if err != nil {
		t.Fatal(err)
	}
	if p43 == nil || p45 == nil || p43.USI == "" || p45.UTI == "" {
		t.Fatalf("cftc events: %+v %+v", p43, p45)
	}
	// Lifecycle: MODI is P45-only; TERM hits both (P43 public termination).
	pp43, pp45, err := rep.ReportLifecycle(context.Background(), p45.UTI,
		reporting.LifecycleInput{Action: reporting.ActionModify, EventTS: regTestNow.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if pp43 != nil || pp45 == nil {
		t.Fatal("MODI must be P45-only")
	}
	pp43, pp45, err = rep.ReportLifecycle(context.Background(), p45.UTI,
		reporting.LifecycleInput{Action: reporting.ActionTerminate,
			EventType: reporting.EventTypeTermination, EventTS: regTestNow.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if pp43 == nil || pp45 == nil {
		t.Fatal("TERM must hit both parts")
	}
	// Large-trader: below threshold → nil; above → LTR row.
	instID := int64(1)
	fs.Limits = append(fs.Limits, &reporting.CFTCLimit{
		InstrumentID: &instID, InstrumentType: "FORWARD",
		LargeTraderThreshold: "500", Currency: "USD",
		EffectiveFrom: regTestNow.Add(-time.Hour),
	})
	fs.Positions = []reporting.PositionSnapshot{{
		AccountID: 11, InstrumentID: 1, Quantity: "400", UpdatedAt: regTestNow,
	}}
	if ev, err := rep.CheckLimits(context.Background(), 11, 1, "FORWARD", "EUR/USD"); err != nil || ev != nil {
		t.Fatalf("below threshold: %v %v", ev, err)
	}
	fs.Positions[0].Quantity = "600"
	ev, err := rep.CheckLimits(context.Background(), 11, 1, "FORWARD", "EUR/USD")
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.Action != reporting.ActionLargeTrader {
		t.Fatalf("LTR: %+v", ev)
	}
}

func TestMiFIDReporter(t *testing.T) {
	fs := reporttest.NewFakeStore(regTestNow)
	fs.SeedSchemas()
	svc := regSvc(t, fs)
	rep, err := NewMiFIDReporter(svc)
	if err != nil {
		t.Fatal(err)
	}
	tc := regFwdTrade(8, "DE", "GB")
	tc.BuyerAlgoID = "ALGO-9"
	e, err := rep.ReportExecution(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	if e.Regime != reporting.RegimeMIFID2 || e.DecisionMakerID != "ALGO-9" ||
		e.DecisionMakerType != "ALGO" {
		t.Fatalf("decomposition: %+v", e)
	}
	if e.DisseminationDueAt == nil {
		t.Fatal("RTS 22 T+1 deadline missing")
	}
	if !rep.RTS22Deadline(regTestNow).Equal(*e.DisseminationDueAt) {
		t.Fatal("deadline helper must match event")
	}
}
