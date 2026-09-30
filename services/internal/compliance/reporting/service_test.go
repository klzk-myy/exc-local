package reporting

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2024, 6, 3, 10, 0, 0, 0, time.UTC)

func testVenueLEI(t *testing.T) string {
	return leiWithChecksum(t, "TESTTESTTESTTEST00")
}

// enrichStub supplies deterministic valuation/margin/notional so EMIR
// NEWTs satisfy the pinned required_fields (production wires the
// Phase-19.5 forward-points oracle — compliance.EMIRReporter.EnrichNEWT).
func enrichStub(_ context.Context, tc *TradeContext) (string, json.RawMessage, json.RawMessage, error) {
	val := json.RawMessage(fmt.Sprintf(
		`{"mark_price":%q,"currency":%q,"basis":"FWD_POINTS_ORACLE"}`,
		tc.Price, tc.QuoteCurrency))
	mrg := json.RawMessage(fmt.Sprintf(
		`{"collateralisation":"UNCOLLATERALISED","currency":%q}`, tc.QuoteCurrency))
	return "", val, mrg, nil
}

func newTestService(t *testing.T, reportAllCFTC bool) (*Service, *fakeStore) {
	t.Helper()
	fs := newFakeStore(testNow)
	fs.seedSchemas()
	svc, err := NewService(fs, Config{
		VenueLEI: testVenueLEI(t), VenueMIC: "XEXC", USINamespace: "EXC",
		DualSided: true, ReportAllToCFTC: reportAllCFTC,
		RepairSLA: 2 * time.Hour, StaleAfter: 24 * time.Hour,
		DerivativeEnrich: enrichStub,
		Now:              func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc, fs
}

func spotTrade(tradeID int64) *TradeContext {
	return &TradeContext{
		TradeID: tradeID, InstrumentID: 1, InstrumentCode: "EUR/USD",
		InstrumentType: "SPOT", BaseCurrency: "EUR", QuoteCurrency: "USD",
		BuyOrderID: 100, SellOrderID: 200,
		BuyerAccountID: 11, SellerAccountID: 22,
		BuyerUserID: 501, SellerUserID: 502,
		Price: "1.0850", Quantity: "1000000",
		ExecutedAt: testNow,
	}
}

func fwdTrade(tradeID int64, buyerJur, sellerJur string) *TradeContext {
	tc := spotTrade(tradeID)
	tc.InstrumentType = "FORWARD"
	tc.BuyerJurisdiction = buyerJur
	tc.SellerJurisdiction = sellerJur
	return tc
}

func TestRecordExecution_Spot_MiFIDOnly(t *testing.T) {
	svc, fs := newTestService(t, false)
	evs, err := svc.RecordExecution(context.Background(), spotTrade(7))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Regime != RegimeMIFID2 {
		t.Fatalf("expected one MIFID2 event, got %v", evs)
	}
	e := evs[0]
	if e.Status != EventValidated {
		t.Fatalf("status %s, errors %s", e.Status, e.ValidationErrors)
	}
	if e.InstrumentCode != "EUR/USD" {
		t.Fatal("venue instrument code required (not ISIN)")
	}
	// RTS 22 decomposition: INTC fallbacks when no party rows exist.
	if e.BuyerID != "INTC501" || e.SellerID != "INTC502" {
		t.Fatalf("buyer/seller ids: %s/%s", e.BuyerID, e.SellerID)
	}
	// Artifacts: ARM (RTS 22) + APA (RTS 1) — MiFID fans out to both.
	subs, _ := fs.SubmissionsForEvent(context.Background(), e.EventID)
	if len(subs) != 2 {
		t.Fatalf("expected ARM+APA artifacts, got %d", len(subs))
	}
	dests := map[Destination]bool{}
	for _, s := range subs {
		dests[s.Destination] = true
	}
	if !dests[DestinationARM] || !dests[DestinationAPA] {
		t.Fatalf("destinations: %v", dests)
	}
	// T+1 23:59 CET deadline rides the event.
	if e.DisseminationDueAt == nil ||
		e.DisseminationDueAt.UTC().Format("2006-01-02 15:04") != "2024-06-04 21:59" {
		t.Fatalf("rts22 deadline: %v", e.DisseminationDueAt)
	}
}

func TestRecordExecution_Derivative_RegimeFanout(t *testing.T) {
	svc, _ := newTestService(t, false)
	// EU-only parties → MiFID + EMIR, no CFTC.
	evs, err := svc.RecordExecution(context.Background(), fwdTrade(9, "DE", "GB"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[Regime]*Event{}
	for _, e := range evs {
		got[e.Regime] = e
	}
	if got[RegimeMIFID2] == nil || got[RegimeEMIRREFIT] == nil {
		t.Fatalf("expected MIFID2+EMIR, got %v", got)
	}
	if got[RegimeCFTCP43] != nil || got[RegimeCFTCP45] != nil {
		t.Fatal("no US nexus — CFTC regimes must not fire")
	}
	// EMIR NEWT: enriched valuation/margin/notional satisfy the pinned
	// ruleset — the event validates, not quarantines.
	if emir := got[RegimeEMIRREFIT]; emir.Status != EventValidated {
		t.Fatalf("emir status %s, errors %s", emir.Status, emir.ValidationErrors)
	}
	if got[RegimeEMIRREFIT].Notional != "1085000" {
		t.Fatalf("notional: %s", got[RegimeEMIRREFIT].Notional)
	}
	// US buyer → all four regimes.
	evs, err = svc.RecordExecution(context.Background(), fwdTrade(10, "US", "DE"))
	if err != nil {
		t.Fatal(err)
	}
	got = map[Regime]*Event{}
	for _, e := range evs {
		got[e.Regime] = e
	}
	if got[RegimeCFTCP43] == nil || got[RegimeCFTCP45] == nil {
		t.Fatalf("US nexus must add CFTC 43/45, got %v", got)
	}
	for _, e := range evs {
		if e.Regime == RegimeEMIRREFIT && !e.DualSided {
			t.Fatal("dual-sided flag must ride the venue config")
		}
		if (e.Regime == RegimeCFTCP43 || e.Regime == RegimeCFTCP45) && e.USI == "" {
			t.Fatal("CFTC events need USI")
		}
	}
	// Part 43 carries the 15-minute real-time dissemination SLA.
	if d := got[RegimeCFTCP43].DisseminationDueAt; d == nil ||
		!d.Equal(testNow.Add(15*time.Minute)) {
		t.Fatalf("part43 dissemination deadline: %v", d)
	}
}

func TestRecordExecution_EnrichErrorFailsClosed(t *testing.T) {
	fs := newFakeStore(testNow)
	fs.seedSchemas()
	svc, err := NewService(fs, Config{
		VenueLEI: testVenueLEI(t), VenueMIC: "XEXC", USINamespace: "EXC",
		Now: func() time.Time { return testNow },
		DerivativeEnrich: func(_ context.Context, _ *TradeContext) (string, json.RawMessage, json.RawMessage, error) {
			return "", nil, nil, fmt.Errorf("stale forward points")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Oracle failure → error propagates (consumer NAKs); the EMIR event
	// is never persisted, and no valuation is fabricated.
	_, err = svc.RecordExecution(context.Background(), fwdTrade(15, "DE", "GB"))
	if err == nil {
		t.Fatal("enrich error must propagate")
	}
	for _, e := range fs.events {
		if e.Regime == RegimeEMIRREFIT {
			t.Fatal("EMIR event must not persist on enrich failure")
		}
	}
}

func TestRecordExecution_NoEnricherQuarantinesDerivative(t *testing.T) {
	fs := newFakeStore(testNow)
	fs.seedSchemas()
	svc, err := NewService(fs, Config{
		VenueLEI: testVenueLEI(t), VenueMIC: "XEXC", USINamespace: "EXC",
		Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := svc.RecordExecution(context.Background(), fwdTrade(16, "DE", "GB"))
	if err != nil {
		t.Fatal(err)
	}
	var emir *Event
	for _, e := range evs {
		if e.Regime == RegimeEMIRREFIT {
			emir = e
		}
	}
	if emir == nil || emir.Status != EventQuarantined {
		t.Fatalf("missing enricher must quarantine, got %+v", emir)
	}
	subs, _ := fs.SubmissionsForEvent(context.Background(), emir.EventID)
	if len(subs) != 0 {
		t.Fatal("quarantined event leaked artifacts")
	}
}

func TestRecordExecution_Idempotent(t *testing.T) {
	svc, fs := newTestService(t, false)
	evs1, err := svc.RecordExecution(context.Background(), spotTrade(11))
	if err != nil {
		t.Fatal(err)
	}
	evs2, err := svc.RecordExecution(context.Background(), spotTrade(11))
	if err != nil {
		t.Fatal(err)
	}
	if evs1[0].EventID != evs2[0].EventID {
		t.Fatal("replay must return the stored event")
	}
	if len(fs.events) != 1 {
		t.Fatalf("replay must not duplicate, got %d", len(fs.events))
	}
}

func TestRecordExecution_UTICollision(t *testing.T) {
	svc, fs := newTestService(t, false)
	// Seed: a different trade already owns the UTI that trade 6 would
	// mint — recorded under EMIR so LatestEvent(uti, MIFID2) misses and
	// the UTIOwner collision path fires.
	fs.events = append(fs.events, &Event{
		UTI: UTIFor(svc.Cfg.VenueLEI, "TRADE", 6), TradeID: 999,
		Regime: RegimeEMIRREFIT, Action: ActionNew, EventType: EventTypeTrade,
		ReportSeq: 1, Status: EventAccepted, EventTS: testNow,
		InstrumentCode: "EUR/USD",
	})
	_, err := svc.RecordExecution(context.Background(), spotTrade(6))
	if err == nil {
		t.Fatal("UTI bound to another trade must reject")
	}
	var found bool
	for _, b := range fs.breaks {
		if b.BreakType == BreakIDCollision {
			found = true
		}
	}
	if !found {
		t.Fatal("expected ID_COLLISION break")
	}
}

func TestRecordExecution_Quarantine(t *testing.T) {
	svc, fs := newTestService(t, false)
	tc := spotTrade(20)
	tc.QuoteCurrency = "" // required field missing → quarantine
	evs, err := svc.RecordExecution(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Status != EventQuarantined {
		t.Fatalf("expected QUARANTINED, got %s", evs[0].Status)
	}
	subs, _ := fs.SubmissionsForEvent(context.Background(), evs[0].EventID)
	if len(subs) != 0 {
		t.Fatalf("quarantined event leaked %d artifacts", len(subs))
	}
	var valBreak bool
	for _, b := range fs.breaks {
		if b.BreakType == BreakValidation {
			valBreak = true
		}
	}
	if !valBreak {
		t.Fatal("expected VALIDATION break")
	}
}

func TestRecordLifecycle_SeqAndSupersedes(t *testing.T) {
	svc, _ := newTestService(t, false)
	evs, err := svc.RecordExecution(context.Background(), fwdTrade(30, "DE", "GB"))
	if err != nil {
		t.Fatal(err)
	}
	var emir *Event
	for _, e := range evs {
		if e.Regime == RegimeEMIRREFIT {
			emir = e
		}
	}
	// VALU continuation.
	v, err := svc.RecordLifecycle(context.Background(), LifecycleInput{
		UTI: emir.UTI, Regime: RegimeEMIRREFIT,
		Action: ActionValuation, EventType: EventTypeValuation,
		EventTS:   testNow.Add(time.Hour),
		Valuation: []byte(`{"mtm":"1000.00","currency":"USD"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.ReportSeq != 2 {
		t.Fatalf("seq: %d", v.ReportSeq)
	}
	// TERM.
	term, err := svc.RecordLifecycle(context.Background(), LifecycleInput{
		UTI: emir.UTI, Regime: RegimeEMIRREFIT,
		Action: ActionTerminate, EventType: EventTypeTermination,
		EventTS: testNow.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if term.ReportSeq != 3 {
		t.Fatalf("seq: %d", term.ReportSeq)
	}
	// Missing NEWT must fail closed.
	_, err = svc.RecordLifecycle(context.Background(), LifecycleInput{
		UTI: "NOPE", Regime: RegimeEMIRREFIT, Action: ActionTerminate,
		EventTS: testNow,
	})
	if err == nil {
		t.Fatal("lifecycle without NEWT must fail")
	}
}

func TestIngestAck_NACKOpensRepairBreak(t *testing.T) {
	svc, fs := newTestService(t, false)
	evs, err := svc.RecordExecution(context.Background(), spotTrade(40))
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := fs.SubmissionsForEvent(context.Background(), evs[0].EventID)
	br, err := svc.IngestAck(context.Background(), Ack{
		ReportSubmissionID: subs[0].ReportSubmissionID,
		EventID:            evs[0].EventID,
		AckStatus:          AckReject, AckCode: "E1234", AckText: "bad field",
	})
	if err != nil {
		t.Fatal(err)
	}
	if br == nil || br.BreakType != BreakNackRepair {
		t.Fatalf("expected NACK_REPAIR break, got %+v", br)
	}
	if fs.events[0].Status != EventRejected {
		t.Fatalf("stored event status: %s", fs.events[0].Status)
	}
	after, _ := fs.SubmissionByID(context.Background(), subs[0].ReportSubmissionID)
	if after.Status != SubNacked {
		t.Fatalf("submission status: %s", after.Status)
	}
}

func TestResubmit_CorrectedAttempt(t *testing.T) {
	svc, fs := newTestService(t, false)
	evs, err := svc.RecordExecution(context.Background(), spotTrade(50))
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := fs.SubmissionsForEvent(context.Background(), evs[0].EventID)
	var arm *Submission
	for i := range subs {
		if subs[i].Destination == DestinationARM {
			arm = &subs[i]
		}
	}
	if arm == nil {
		t.Fatal("ARM artifact missing")
	}
	// Reject, then resubmit with a correction.
	if _, err := svc.IngestAck(context.Background(), Ack{
		ReportSubmissionID: arm.ReportSubmissionID,
		EventID:            evs[0].EventID,
		AckStatus:          AckReject, AckCode: "E1", AckText: "bad price",
	}); err != nil {
		t.Fatal(err)
	}
	fresh, err := svc.Resubmit(context.Background(), arm.ReportSubmissionID,
		map[string]any{"price": "1.0851"}, 900)
	if err != nil {
		t.Fatal(err)
	}
	// Rejected event → CORR lifecycle row (supersedes chain) whose own
	// ARM artifact carries the corrected price — one corrected report
	// per seq, immutable history preserved on the superseded row.
	corr, err := fs.LatestEvent(context.Background(), evs[0].UTI, RegimeMIFID2)
	if err != nil || corr == nil {
		t.Fatal("corr event missing")
	}
	if corr.Action != ActionCorrect || corr.SupersedesEventID == nil ||
		*corr.SupersedesEventID != evs[0].EventID {
		t.Fatalf("corr chain broken: %+v", corr)
	}
	if fresh.EventID != corr.EventID || fresh.Destination != DestinationARM {
		t.Fatalf("corrected artifact: %+v", fresh)
	}
	if !json.Valid(fresh.Payload) ||
		!strings.Contains(string(fresh.Payload), "1.0851") {
		t.Fatalf("corrected payload: %s", fresh.Payload)
	}
	if corr.Price != "1.0851" {
		t.Fatalf("corr event field: %s", corr.Price)
	}
	// The superseded row keeps its original value (append-only).
	if fs.events[0].Price != "1.0850" {
		t.Fatal("original event row must be untouched")
	}
	// Non-whitelisted field must fail.
	_, err = svc.Resubmit(context.Background(), arm.ReportSubmissionID,
		map[string]any{"account_id": 1}, 900)
	if err == nil {
		t.Fatal("non-repairable field must be rejected")
	}
}

func TestReconcile_MissingAndDuplicate(t *testing.T) {
	svc, fs := newTestService(t, false)
	// Internal open position with no repo event → MISSING.
	fs.positions = []PositionSnapshot{{
		PositionID: 1, AccountID: 11, InstrumentID: 99,
		Quantity: "1000", UpdatedAt: testNow,
	}}
	rep, err := svc.Reconcile(context.Background(), RegimeEMIRREFIT)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.BreaksOpened) == 0 {
		t.Fatal("expected breaks")
	}
	var missing bool
	for _, b := range fs.breaks {
		if b.BreakType == BreakMissing {
			missing = true
		}
	}
	if !missing {
		t.Fatal("expected MISSING break")
	}
	// Duplicate NEWT for one UTI → DUPLICATE.
	uti := UTIFor(svc.Cfg.VenueLEI, "TRADE", 77)
	for i := 0; i < 2; i++ {
		fs.events = append(fs.events, &Event{
			EventID: 900 + int64(i), UTI: uti, Regime: RegimeEMIRREFIT,
			Action: ActionNew, EventType: EventTypeTrade, ReportSeq: i + 1,
			Status: EventAccepted, EventTS: testNow,
			InstrumentCode: "EUR/USD", AccountID: 11, InstrumentID: 77,
		})
	}
	if _, err := svc.Reconcile(context.Background(), RegimeEMIRREFIT); err != nil {
		t.Fatal(err)
	}
	var dup bool
	for _, b := range fs.breaks {
		if b.BreakType == BreakDuplicate {
			dup = true
		}
	}
	if !dup {
		t.Fatal("expected DUPLICATE break")
	}
}
