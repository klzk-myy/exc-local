// phase24_tasks_test.go — Phase-24 Tasks 24.3.8 (CLS PvP), 24.3.9
// (SSI + bilateral netting), 24.3.12 (statement ingestion), 24.3.20
// (rail cut-off) and 24.3.21 (suspense routing) unit coverage over
// in-memory seam fakes.
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

func mustDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return d
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return ts
}

// ===========================================================================
// Task 24.3.20 — RailCutoffService
// ===========================================================================

type fakeScheduleStore struct {
	rows []RailScheduleRow
	err  error
}

func (s *fakeScheduleStore) RailSchedules(context.Context) ([]RailScheduleRow, error) {
	return s.rows, s.err
}

func newCutoffSvc(t *testing.T, rows []RailScheduleRow, now time.Time) *RailCutoffService {
	t.Helper()
	cal := testCalendar(t)
	svc, err := NewRailCutoffService(&fakeScheduleStore{rows: rows}, cal, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewRailCutoffService: %v", err)
	}
	if err := svc.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return svc
}

var defaultSchedules = []RailScheduleRow{
	{RailName: "FEDWIRE", Currency: "USD", Timezone: "America/New_York", CutoffLocal: "18:30"},
	{RailName: "TARGET2", Currency: "EUR", Timezone: "Europe/Berlin", CutoffLocal: "18:00"},
	{RailName: "CHAPS", Currency: "GBP", Timezone: "Europe/London", CutoffLocal: "17:00"},
	{RailName: "CLS_PVP", Currency: "*", Timezone: "Europe/Berlin", CutoffLocal: "06:30"},
}

// Fedwire cut-off is evaluated in America/New_York, not UTC.
func TestRailCutoffTimezoneAware(t *testing.T) {
	// 2025-06-10 is a Tuesday. 22:00 UTC = 18:00 ET (EDT, UTC-4) — before 18:30.
	svc := newCutoffSvc(t, defaultSchedules, mustTime(t, "2025-06-10T22:00:00Z"))
	d, err := svc.Evaluate("FEDWIRE", "USD", mustTime(t, "2025-06-10T22:00:00Z"))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if d.CutoffPassed || d.QueuedForNextCycle {
		t.Fatalf("18:00 ET must be inside the 18:30 window: %+v", d)
	}
	// 23:00 UTC = 19:00 ET — past the cut-off.
	d, err = svc.Evaluate("FEDWIRE", "USD", mustTime(t, "2025-06-10T23:00:00Z"))
	if err != nil {
		t.Fatalf("Evaluate past: %v", err)
	}
	if !d.CutoffPassed || !d.QueuedForNextCycle {
		t.Fatalf("19:00 ET must be past the window: %+v", d)
	}
	if !d.ValueDate.Equal(day(t, "2025-06-11")) {
		t.Fatalf("value date must roll to next business day, got %v", d.ValueDate)
	}
}

func TestRailCutoffWeekendRoll(t *testing.T) {
	// Friday 2025-06-13 23:00 UTC → next business day Monday 2025-06-16.
	svc := newCutoffSvc(t, defaultSchedules, mustTime(t, "2025-06-13T23:00:00Z"))
	d, err := svc.Evaluate("CHAPS", "GBP", mustTime(t, "2025-06-13T23:00:00Z"))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !d.CutoffPassed || !d.ValueDate.Equal(day(t, "2025-06-16")) {
		t.Fatalf("weekend roll expected Monday 2025-06-16, got %v", d.ValueDate)
	}
}

func TestRailCutoffUnscheduledFailsClosed(t *testing.T) {
	svc := newCutoffSvc(t, defaultSchedules, mustTime(t, "2025-06-10T12:00:00Z"))
	_, err := svc.Evaluate("WISE", "USD", mustTime(t, "2025-06-10T12:00:00Z"))
	assertCode(t, err, "BANKING_RAIL_UNAVAILABLE")
}

func TestRailCutoffMalformedRowFailsReload(t *testing.T) {
	cal := testCalendar(t)
	svc, err := NewRailCutoffService(&fakeScheduleStore{rows: []RailScheduleRow{
		{RailName: "FEDWIRE", Currency: "USD", Timezone: "Not/AZone", CutoffLocal: "18:30"},
	}}, cal, time.Now)
	if err != nil {
		t.Fatalf("NewRailCutoffService: %v", err)
	}
	if err := svc.Reload(context.Background()); err == nil {
		t.Fatal("malformed timezone must fail the reload")
	}
}

func TestRailCutoffEnforceSameDay(t *testing.T) {
	svc := newCutoffSvc(t, defaultSchedules, mustTime(t, "2025-06-10T23:00:00Z"))
	if err := svc.EnforceSameDay("FEDWIRE", "USD", mustTime(t, "2025-06-10T23:00:00Z")); err == nil {
		t.Fatal("same-day request past Fedwire cut-off must be refused")
	} else {
		assertCode(t, err, "RAIL_CUTOFF_EXCEEDED")
	}
	if err := svc.EnforceSameDay("FEDWIRE", "USD", mustTime(t, "2025-06-10T12:00:00Z")); err != nil {
		t.Fatalf("inside window must pass: %v", err)
	}
}

// T+0 settlement legs generated past the rail cut-off land
// QUEUED_FOR_NEXT_CYCLE with a rolled value date.
func TestGenerateInstructionsQueuedPastCutoff(t *testing.T) {
	store := newFakeSettlementStore()
	store.addInstrument(1, "EUR/USD", "EUR", "USD", 0) // same-day
	store.addNostro(10, "EUR", "ECB", "COBADEFF", "1", "")
	store.addNostro(11, "USD", "FRB", "FRNYUS33", "2", "")
	// Tuesday 2025-06-10 23:00 UTC = 19:00 ET → past Fedwire 18:30 ET.
	now := mustTime(t, "2025-06-10T23:00:00Z")
	cutoff := newCutoffSvc(t, defaultSchedules, now)
	svc, err := NewSettlementService(store, testCalendar(t), SettlementOptions{
		SenderBIC: "EXCHGB2L", Cutoff: cutoff, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewSettlementService: %v", err)
	}
	res, err := svc.GenerateInstructions(context.Background(), SettlementFill{
		TradeID: 100, InstrumentID: 1, BuyerAccountID: 7, SellerAccountID: 8,
		Quantity: mustDec(t, "1000"), Price: mustDec(t, "1.10"),
		TradeDate: now,
	})
	if err != nil {
		t.Fatalf("GenerateInstructions: %v", err)
	}
	if len(res.Legs) != 4 {
		t.Fatalf("want 4 legs, got %d", len(res.Legs))
	}
	for _, l := range res.Legs {
		if l.Status != SettleQueuedNextCycle {
			t.Fatalf("leg must be QUEUED_FOR_NEXT_CYCLE, got %s", l.Status)
		}
		if !normalizeDay(l.SettlementDate).Equal(day(t, "2025-06-11")) {
			t.Fatalf("value date must roll to 2025-06-11, got %v", l.SettlementDate)
		}
	}
	// Nothing dispatches today.
	rep, err := svc.DispatchDue(context.Background(), now)
	if err != nil {
		t.Fatalf("DispatchDue: %v", err)
	}
	if rep.Dispatched != 0 {
		t.Fatalf("queued legs must not dispatch same-day, got %d", rep.Dispatched)
	}
	// Next business day inside the window: legs release + dispatch.
	next := mustTime(t, "2025-06-11T10:00:00Z")
	cutoff2 := newCutoffSvc(t, defaultSchedules, next)
	svc2, err := NewSettlementService(store, testCalendar(t), SettlementOptions{
		SenderBIC: "EXCHGB2L", Cutoff: cutoff2, Clock: func() time.Time { return next },
		Dispatcher: &NullDispatcher{},
	})
	if err != nil {
		t.Fatalf("NewSettlementService 2: %v", err)
	}
	rep, err = svc2.DispatchDue(context.Background(), next)
	if err != nil {
		t.Fatalf("DispatchDue next day: %v", err)
	}
	if rep.Dispatched != 4 {
		t.Fatalf("4 legs must dispatch on the rolled date, got %d (errs %v)", rep.Dispatched, rep.Errors)
	}
}

// ===========================================================================
// Task 24.3.8 — CLS PvP
// ===========================================================================

type fakeClsStore struct {
	ref        *ClsRefData
	refFound   bool
	agreement  bool
	nextID     int64
	rows       map[int64]*ClsInstruction
	byRef      map[string]*ClsInstruction
	events     []ClsStatusEvent
	exceptions []SettlementException
}

func newFakeClsStore() *fakeClsStore {
	return &fakeClsStore{
		ref: &ClsRefData{
			VersionID: 1, Version: "CLS-REF-TEST",
			Currencies: map[string]bool{"USD": true, "EUR": true, "GBP": true},
			Products:   map[string]bool{"SPOT": true, "FORWARD": true},
			Members:    map[string]bool{"MEMBBANKXXX": true, "MEMBBANK": true},
			Cutoffs:    map[string]ClsCutoff{},
			AltPvP:     map[string]bool{"GBP/USD": true},
			Limits:     map[string]ClsPrincipalLimit{"USD": {Currency: "USD", MaxPrincipal: mustDec2("1000000"), MaxDurationH: 4}},
		},
		refFound: true,
		nextID:   1,
		rows:     map[int64]*ClsInstruction{},
		byRef:    map[string]*ClsInstruction{},
	}
}

func mustDec2(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func (s *fakeClsStore) LoadReference(context.Context) (*ClsRefData, bool, error) {
	return s.ref, s.refFound, nil
}
func (s *fakeClsStore) NettingAgreementExists(context.Context, int64) (bool, error) {
	return s.agreement, nil
}
func (s *fakeClsStore) InTx(ctx context.Context, fn func(context.Context, ClsTx) error) error {
	return fn(ctx, fakeClsTx{s})
}

type fakeClsTx struct{ s *fakeClsStore }

func (t fakeClsTx) InsertInstruction(_ context.Context, in ClsInstruction) (int64, error) {
	id := t.s.nextID
	t.s.nextID++
	cp := in
	cp.ID = id
	cp.CreatedAt = time.Now().UTC()
	t.s.rows[id] = &cp
	t.s.byRef[in.InstructionRef] = &cp
	return id, nil
}
func (t fakeClsTx) LockInstruction(_ context.Context, ref string, id int64) (*ClsInstruction, bool, error) {
	if ref != "" {
		if r, ok := t.s.byRef[ref]; ok {
			return r, true, nil
		}
		return nil, false, nil
	}
	if r, ok := t.s.rows[id]; ok {
		return r, true, nil
	}
	return nil, false, nil
}
func (t fakeClsTx) Transition(_ context.Context, id int64, from, to ClsStatus, e ClsStatusEvent) error {
	r, ok := t.s.rows[id]
	if !ok {
		return fmt.Errorf("instruction %d not found", id)
	}
	if from != to && r.Status != from {
		return excerrors.New(CodeClsStateConflict, "status drift")
	}
	r.Status = to
	e.InstructionID = id
	e.From, e.To = from, to
	t.s.events = append(t.s.events, e)
	return nil
}
func (t fakeClsTx) UpdatePayload(_ context.Context, id int64, payload string, version int, uetr *string) error {
	t.s.rows[id].MessagePayload = &payload
	t.s.rows[id].PayloadVersion = version
	if uetr != nil {
		t.s.rows[id].UETR = uetr
	}
	return nil
}
func (t fakeClsTx) SetMemberIDs(_ context.Context, id int64, mid, ack string) error {
	t.s.rows[id].MemberInstructionID = &mid
	t.s.rows[id].MemberAckRef = &ack
	return nil
}
func (t fakeClsTx) SetSettled(_ context.Context, id int64, at time.Time, jid int64) error {
	t.s.rows[id].SettledAt = &at
	t.s.rows[id].GLJournalID = &jid
	return nil
}
func (t fakeClsTx) SetRoute(_ context.Context, id int64, r ClsRoute) error {
	t.s.rows[id].SettlementRoute = r
	return nil
}
func (t fakeClsTx) InsertException(_ context.Context, e SettlementException) error {
	t.s.exceptions = append(t.s.exceptions, e)
	return nil
}

type fakeMember struct {
	ack  *ClsMemberAck
	err  error
	sent []ClsMessage
}

func (m *fakeMember) Send(_ context.Context, msg ClsMessage) (*ClsMemberAck, error) {
	m.sent = append(m.sent, msg)
	if m.err != nil {
		return nil, m.err
	}
	if m.ack == nil {
		return &ClsMemberAck{MemberInstructionID: "M123", AckRef: "ACK1", Matched: true}, nil
	}
	return m.ack, nil
}

func clsNew() ClsNewInstruction {
	return ClsNewInstruction{
		InstructionRef:        "CLSREF001",
		CounterpartyAccountID: 42,
		MemberBIC:             "MEMBBANK",
		Product:               "SPOT",
		BuyCurrency:           "EUR",
		BuyAmount:             mustDec2("500000"),
		SellCurrency:          "USD",
		SellAmount:            mustDec2("550000"),
		ValueDate:             dayNil("2025-06-12"),
	}
}

func dayNil(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

func newClsSvc(t *testing.T, st *fakeClsStore, member ClsMemberAdapter, poster JournalPoster) *ClsPvpService {
	t.Helper()
	svc, err := NewClsPvpService(st, ClsPvpOptions{Member: member, Poster: poster})
	if err != nil {
		t.Fatalf("NewClsPvpService: %v", err)
	}
	return svc
}

func TestClsSubmitDispatchMatched(t *testing.T) {
	st := newFakeClsStore()
	member := &fakeMember{}
	svc := newClsSvc(t, st, member, nil)

	in, err := svc.SubmitInstruction(context.Background(), clsNew())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if in.Status != ClsValidated {
		t.Fatalf("want VALIDATED, got %s", in.Status)
	}
	out, err := svc.Dispatch(context.Background(), "CLSREF001")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if out.Status != ClsEligible {
		t.Fatalf("member match must land ELIGIBLE, got %s", out.Status)
	}
	if len(member.sent) != 1 || !strings.Contains(member.sent[0].XML, "pacs.009") {
		t.Fatalf("ISO 20022 payload expected, got %+v", member.sent)
	}
	if member.sent[0].UETR == "" {
		t.Fatal("UETR must be assigned")
	}
}

func TestClsNilMemberFailsClosed(t *testing.T) {
	st := newFakeClsStore()
	svc := newClsSvc(t, st, nil, nil)
	in, err := svc.SubmitInstruction(context.Background(), clsNew())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	_, err = svc.Dispatch(context.Background(), "CLSREF001")
	assertCode(t, err, CodeClsMemberUnavailable)
	if st.rows[in.ID].Status != ClsValidated {
		t.Fatalf("instruction must stay VALIDATED, got %s", st.rows[in.ID].Status)
	}
}

func TestClsMissingRefDataFailsClosed(t *testing.T) {
	st := newFakeClsStore()
	st.refFound = false
	svc := newClsSvc(t, st, &fakeMember{}, nil)
	_, err := svc.SubmitInstruction(context.Background(), clsNew())
	assertCode(t, err, CodeClsRefDataMissing)
}

func TestClsIneligibleWaterfall(t *testing.T) {
	// GBP/USD pair with GBP ineligible → INELIGIBLE; AltPvP covers
	// GBP/USD → route ALT_PVP.
	st := newFakeClsStore()
	delete(st.ref.Currencies, "GBP")
	svc := newClsSvc(t, st, &fakeMember{}, nil)
	in := clsNew()
	in.BuyCurrency = "GBP"
	out, err := svc.SubmitInstruction(context.Background(), in)
	assertCode(t, err, CodeClsNotEligible)
	if out.SettlementRoute != ClsRouteAltPvp {
		t.Fatalf("want ALT_PVP route, got %s", out.SettlementRoute)
	}
}

func TestClsControlledGrossBreachAlerts(t *testing.T) {
	st := newFakeClsStore()
	delete(st.ref.Currencies, "GBP")
	delete(st.ref.AltPvP, "GBP/USD")
	st.agreement = false // no ISDA → controlled gross
	st.ref.Limits["USD"] = ClsPrincipalLimit{Currency: "USD", MaxPrincipal: mustDec2("100"), MaxDurationH: 4}
	al := &fakeAlerter{}
	svc, err := NewClsPvpService(st, ClsPvpOptions{Member: &fakeMember{}, Alerter: al})
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	in := clsNew()
	in.BuyCurrency = "GBP"
	in.SellAmount = mustDec2("550000") // exceeds the 100 USD limit
	_, err = svc.SubmitInstruction(context.Background(), in)
	assertCode(t, err, CodeClsNotEligible)
	if len(st.exceptions) == 0 || st.exceptions[0].Code != "OTHER" {
		t.Fatalf("principal-risk break expected, got %+v", st.exceptions)
	}
	if len(al.alerts) == 0 || al.alerts[0].Severity != "P1" {
		t.Fatalf("P1 alert expected, got %+v", al.alerts)
	}
}

func TestClsFinalityRequiresAuthentication(t *testing.T) {
	st := newFakeClsStore()
	poster := &fakePoster{}
	svc := newClsSvc(t, st, &fakeMember{}, poster)
	if _, err := svc.SubmitInstruction(context.Background(), clsNew()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := svc.Dispatch(context.Background(), "CLSREF001"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, err := svc.MarkPayIn(context.Background(), "CLSREF001"); err != nil {
		t.Fatalf("PayIn: %v", err)
	}
	// Unauthenticated finality is refused outright.
	_, err := svc.RecordFinality(context.Background(), "CLSREF001", "MREF1", false)
	assertCode(t, err, CodeClsStateConflict)
	if len(poster.journals) != 0 {
		t.Fatal("unauthenticated finality must not post GL")
	}
	// Authenticated finality posts the paired journal + lands SETTLED.
	out, err := svc.RecordFinality(context.Background(), "CLSREF001", "MREF1", true)
	if err != nil {
		t.Fatalf("RecordFinality: %v", err)
	}
	if out.Status != ClsSettled || out.GLJournalID == nil {
		t.Fatalf("want SETTLED with journal, got %+v", out)
	}
	if len(poster.journals) != 1 || len(poster.journals[0].Lines) != 4 {
		t.Fatalf("finality journal must carry 4 balanced legs, got %+v", poster.journals)
	}
	// Idempotent replay.
	out2, err := svc.RecordFinality(context.Background(), "CLSREF001", "MREF1", true)
	if err != nil || out2.Status != ClsSettled {
		t.Fatalf("replay: %v %+v", err, out2)
	}
	if len(poster.journals) != 1 {
		t.Fatal("replayed finality must not re-post")
	}
}

func TestClsMemberStatusUnmatchedOpensBreak(t *testing.T) {
	st := newFakeClsStore()
	svc := newClsSvc(t, st, &fakeMember{}, nil)
	if _, err := svc.SubmitInstruction(context.Background(), clsNew()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	_, err := svc.ApplyMemberStatus(context.Background(), "CLSREF001", ClsRejected, "MREF", "pair rejected")
	if err != nil {
		t.Fatalf("ApplyMemberStatus: %v", err)
	}
	if len(st.exceptions) == 0 || st.exceptions[0].Code != stmtExceptionClsRejected {
		t.Fatalf("CLS_MISMATCH break expected, got %+v", st.exceptions)
	}
}

// ===========================================================================
// Task 24.3.9 — SSI + bilateral netting
// ===========================================================================

type fakeSsiStore struct {
	verified map[int64]*VerifiedBeneficiary
	nextID   int64
	rows     map[int64]*StandingSettlementInstruction
}

func newFakeSsiStore() *fakeSsiStore {
	return &fakeSsiStore{verified: map[int64]*VerifiedBeneficiary{}, nextID: 1, rows: map[int64]*StandingSettlementInstruction{}}
}

func (s *fakeSsiStore) VerifiedBankAccount(_ context.Context, accountID, bankAccountID int64) (*VerifiedBeneficiary, bool, error) {
	b, ok := s.verified[bankAccountID]
	if !ok || b.AccountID != accountID {
		return nil, false, nil
	}
	return b, true, nil
}
func (s *fakeSsiStore) InsertSSI(_ context.Context, r StandingSettlementInstruction) (int64, error) {
	id := s.nextID
	s.nextID++
	cp := r
	cp.ID = id
	s.rows[id] = &cp
	return id, nil
}
func (s *fakeSsiStore) SSI(_ context.Context, id int64) (*StandingSettlementInstruction, bool, error) {
	r, ok := s.rows[id]
	return r, ok, nil
}
func (s *fakeSsiStore) RevokeSSI(_ context.Context, id int64, at time.Time) error {
	if r, ok := s.rows[id]; ok {
		r.Status = SsiRevoked
		r.RevokedAt = &at
	}
	return nil
}
func (s *fakeSsiStore) ActiveSSIFor(_ context.Context, accountID int64, ccy string) (*StandingSettlementInstruction, bool, error) {
	for _, r := range s.rows {
		if r.AccountID == accountID && r.Currency == ccy && r.Status == SsiActive {
			return r, true, nil
		}
	}
	return nil, false, nil
}
func (s *fakeSsiStore) ListSSIs(_ context.Context, accountID int64) ([]StandingSettlementInstruction, error) {
	var out []StandingSettlementInstruction
	for _, r := range s.rows {
		if r.AccountID == accountID {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (s *fakeSsiStore) ClearDefault(_ context.Context, accountID int64, ccy string) error {
	for _, r := range s.rows {
		if r.AccountID == accountID && r.Currency == ccy && r.IsDefault {
			r.IsDefault = false
		}
	}
	return nil
}

func TestSsiRegisterVerified(t *testing.T) {
	store := newFakeSsiStore()
	store.verified[55] = &VerifiedBeneficiary{
		BankAccountID: 55, AccountID: 7, Currency: "USD",
		IBAN: "US12EXAMPLE", SwiftBIC: "CHASUS33", BankName: "Chase",
	}
	svc, err := NewSsiService(store, nil)
	if err != nil {
		t.Fatalf("NewSsiService: %v", err)
	}
	ssi, err := svc.RegisterSSI(context.Background(), 7, 55, "USD", "", "", true)
	if err != nil {
		t.Fatalf("RegisterSSI: %v", err)
	}
	if ssi.NostroOrBeneficiaryRef != "US12EXAMPLE" || ssi.BIC != "CHASUS33" || !ssi.IsDefault {
		t.Fatalf("registry row must supply ref/BIC defaults: %+v", ssi)
	}
	if ssi.VerifiedAt == nil || ssi.Status != SsiActive {
		t.Fatalf("SSI must be ACTIVE + verified: %+v", ssi)
	}
}

func TestSsiRejectsUnverifiedBeneficiary(t *testing.T) {
	store := newFakeSsiStore()
	svc, _ := NewSsiService(store, nil)
	_, err := svc.RegisterSSI(context.Background(), 7, 999, "USD", "X", "", false)
	assertCode(t, err, CodeSsiNotVerified)
}

func TestSsiRevokeIdempotent(t *testing.T) {
	store := newFakeSsiStore()
	store.verified[55] = &VerifiedBeneficiary{BankAccountID: 55, AccountID: 7, Currency: "EUR", IBAN: "DE123"}
	svc, _ := NewSsiService(store, nil)
	ssi, _ := svc.RegisterSSI(context.Background(), 7, 55, "EUR", "", "", false)
	out, err := svc.RevokeSSI(context.Background(), ssi.ID)
	if err != nil || out.Status != SsiRevoked {
		t.Fatalf("revoke: %v %+v", err, out)
	}
	out2, err := svc.RevokeSSI(context.Background(), ssi.ID)
	if err != nil || out2.Status != SsiRevoked {
		t.Fatalf("idempotent revoke: %v %+v", err, out2)
	}
}

// --- netting fakes ---------------------------------------------------------

type fakeNettingStore struct {
	agreement   bool
	obligations []NettingObligation
	batches     map[int64]*NettingBatch
	lines       map[int64][]*NettingLine
	nextBatch   int64
	nextLine    int64
	claimed     []int64
}

func newFakeNettingStore() *fakeNettingStore {
	return &fakeNettingStore{agreement: true, batches: map[int64]*NettingBatch{},
		lines: map[int64][]*NettingLine{}, nextBatch: 1, nextLine: 1}
}

func (s *fakeNettingStore) NettingAgreementExists(context.Context, int64) (bool, error) {
	return s.agreement, nil
}
func (s *fakeNettingStore) NettableObligations(_ context.Context, cp int64, ccy string, vd time.Time) ([]NettingObligation, error) {
	var out []NettingObligation
	for _, o := range s.obligations {
		if o.AccountID == cp && o.Currency == ccy && normalizeDay(o.ValueDate).Equal(normalizeDay(vd)) {
			out = append(out, o)
		}
	}
	return out, nil
}
func (s *fakeNettingStore) Batch(_ context.Context, id int64) (*NettingBatch, bool, error) {
	b, ok := s.batches[id]
	return b, ok, nil
}
func (s *fakeNettingStore) BatchLines(_ context.Context, id int64) ([]NettingLine, error) {
	return s.linesOf(id), nil
}
func (s *fakeNettingStore) linesOf(id int64) []NettingLine {
	var out []NettingLine
	for _, l := range s.lines[id] {
		out = append(out, *l)
	}
	return out
}
func (s *fakeNettingStore) ListBatches(_ context.Context, status string, _ int) ([]NettingBatch, error) {
	var out []NettingBatch
	for _, b := range s.batches {
		if status == "" || string(b.Status) == status {
			out = append(out, *b)
		}
	}
	return out, nil
}
func (s *fakeNettingStore) InTx(ctx context.Context, fn func(context.Context, NettingTx) error) error {
	return fn(ctx, fakeNettingTx{s})
}

type fakeNettingTx struct{ s *fakeNettingStore }

func (t fakeNettingTx) LockObligations(ctx context.Context, cp int64, ccy string, vd time.Time) ([]NettingObligation, error) {
	return t.s.NettableObligations(ctx, cp, ccy, vd)
}
func (t fakeNettingTx) InsertBatch(_ context.Context, b NettingBatch) (int64, error) {
	id := t.s.nextBatch
	t.s.nextBatch++
	cp := b
	cp.ID = id
	t.s.batches[id] = &cp
	return id, nil
}
func (t fakeNettingTx) InsertLines(_ context.Context, lines []NettingLine) error {
	for _, l := range lines {
		l.ID = t.s.nextLine
		t.s.nextLine++
		cp := l
		t.s.lines[l.BatchID] = append(t.s.lines[l.BatchID], &cp)
	}
	return nil
}
func (t fakeNettingTx) LockBatch(_ context.Context, id int64) (*NettingBatch, bool, error) {
	b, ok := t.s.batches[id]
	return b, ok, nil
}
func (t fakeNettingTx) ReleaseLines(_ context.Context, batchID int64, ids []int64, reason string) (int, error) {
	n := 0
	in := map[int64]bool{}
	for _, id := range ids {
		in[id] = true
	}
	for _, l := range t.s.lines[batchID] {
		if in[l.SettlementInstructionID] && l.Status == "NETTED" {
			l.Status = "RELEASED"
			l.ReleasedReason = reason
			n++
		}
	}
	return n, nil
}
func (t fakeNettingTx) Lines(_ context.Context, batchID int64) ([]NettingLine, error) {
	return t.s.linesOf(batchID), nil
}
func (t fakeNettingTx) UpdateBatch(_ context.Context, b NettingBatch) error {
	if r, ok := t.s.batches[b.ID]; ok {
		*r = b
		return nil
	}
	return fmt.Errorf("batch %d not found", b.ID)
}
func (t fakeNettingTx) ClaimNettedLegs(_ context.Context, ids []int64, _ string, _ time.Time) (int, error) {
	t.s.claimed = append(t.s.claimed, ids...)
	return len(ids), nil
}

type fakeClsChecker struct{ eligible bool }

func (f fakeClsChecker) ClsEligible(context.Context, string, string, string) (bool, error) {
	return f.eligible, nil
}

type fakeCutoff struct{ passed bool }

func (f fakeCutoff) CutoffPassed(context.Context, string, time.Time) (bool, string, error) {
	return f.passed, "FEDWIRE", nil
}

func nettingSvc(t *testing.T, store *fakeNettingStore, ssi SsiStore, cls ClsEligibilityChecker, cut RailCutoffChecker, clock ...time.Time) *NettingService {
	t.Helper()
	ck := func() time.Time { return time.Now() }
	if len(clock) == 1 {
		fixed := clock[0]
		ck = func() time.Time { return fixed }
	}
	svc, err := NewNettingService(store, NettingServiceOptions{
		SSI: ssi, Cls: cls, Cutoff: cut, Calendar: testCalendar(t), Clock: ck,
	})
	if err != nil {
		t.Fatalf("NewNettingService: %v", err)
	}
	return svc
}

func TestNettingRunAggregates(t *testing.T) {
	st := newFakeNettingStore()
	vd := dayNil("2025-06-12")
	st.obligations = []NettingObligation{
		{InstructionID: 1, TradeID: 10, AccountID: 42, Currency: "USD", Amount: mustDec2("100"), Direction: DirectionPay, ValueDate: vd, BaseCurrency: "EUR", QuoteCurrency: "USD"},
		{InstructionID: 2, TradeID: 11, AccountID: 42, Currency: "USD", Amount: mustDec2("60"), Direction: DirectionPay, ValueDate: vd},
		{InstructionID: 3, TradeID: 12, AccountID: 42, Currency: "USD", Amount: mustDec2("90"), Direction: DirectionReceive, ValueDate: vd},
	}
	svc := nettingSvc(t, st, newFakeSsiStore(), fakeClsChecker{eligible: false}, fakeCutoff{})
	rep, err := svc.RunNetting(context.Background(), NettingScope{
		CounterpartyAccountID: 42, Currency: "USD", ValueDate: vd,
	})
	if err != nil {
		t.Fatalf("RunNetting: %v", err)
	}
	b := rep.Batch
	if b == nil || b.Status != BatchNetted {
		t.Fatalf("batch must land NETTED, got %+v", b)
	}
	// ΣPAY 160 vs ΣRECEIVE 90 → net 70 PAY.
	if !b.NetObligation.Equal(mustDec2("70")) || !b.GrossObligation.Equal(mustDec2("250")) {
		t.Fatalf("net 70/gross 250 expected, got net %s gross %s", b.NetObligation, b.GrossObligation)
	}
	if rep.Lines != 3 {
		t.Fatalf("3 lines expected, got %d", rep.Lines)
	}
}

func TestNettingClsExclusion(t *testing.T) {
	st := newFakeNettingStore()
	vd := dayNil("2025-06-12")
	st.obligations = []NettingObligation{
		{InstructionID: 1, TradeID: 10, AccountID: 42, Currency: "USD", Amount: mustDec2("100"), Direction: DirectionPay, ValueDate: vd},
	}
	svc := nettingSvc(t, st, newFakeSsiStore(), fakeClsChecker{eligible: true}, fakeCutoff{})
	rep, err := svc.RunNetting(context.Background(), NettingScope{
		CounterpartyAccountID: 42, Currency: "USD", ValueDate: vd,
	})
	if err != nil {
		t.Fatalf("RunNetting: %v", err)
	}
	if !rep.Empty || rep.ClsExcluded != 1 {
		t.Fatalf("CLS-eligible leg must be excluded, got %+v", rep)
	}
}

func TestNettingAgreementRequired(t *testing.T) {
	st := newFakeNettingStore()
	st.agreement = false
	vd := dayNil("2025-06-12")
	st.obligations = []NettingObligation{
		{InstructionID: 1, TradeID: 10, AccountID: 42, Currency: "USD", Amount: mustDec2("100"), Direction: DirectionPay, ValueDate: vd},
	}
	svc := nettingSvc(t, st, newFakeSsiStore(), fakeClsChecker{}, fakeCutoff{})
	_, err := svc.RunNetting(context.Background(), NettingScope{
		CounterpartyAccountID: 42, Currency: "USD", ValueDate: vd,
	})
	assertCode(t, err, CodeNettingAgreementMissing)
}

func TestNettingDispatchCutoffGate(t *testing.T) {
	st := newFakeNettingStore()
	vd := dayNil("2025-06-12")
	st.obligations = []NettingObligation{
		{InstructionID: 1, TradeID: 10, AccountID: 42, Currency: "USD", Amount: mustDec2("100"), Direction: DirectionPay, ValueDate: vd},
	}
	ssiStore := newFakeSsiStore()
	ssiStore.verified[5] = &VerifiedBeneficiary{BankAccountID: 5, AccountID: 42, Currency: "USD", IBAN: "USX"}
	ssiSvc, _ := NewSsiService(ssiStore, nil)
	if _, err := ssiSvc.RegisterSSI(context.Background(), 42, 5, "USD", "", "", true); err != nil {
		t.Fatalf("ssi: %v", err)
	}

	svc := nettingSvc(t, st, ssiStore, fakeClsChecker{}, fakeCutoff{passed: false}, vd)
	rep, err := svc.RunNetting(context.Background(), NettingScope{CounterpartyAccountID: 42, Currency: "USD", ValueDate: vd})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out, err := svc.DispatchBatch(context.Background(), rep.Batch.ID, "FEDWIRE")
	if err != nil {
		t.Fatalf("DispatchBatch: %v", err)
	}
	if out.Status != BatchDispatched || len(out.SSISnapshot) == 0 {
		t.Fatalf("dispatch must snapshot the SSI, got %+v", out)
	}
	// Post-cutoff dispatch is refused.
	svcPast := nettingSvc(t, st, ssiStore, fakeClsChecker{}, fakeCutoff{passed: true}, vd)
	rep2, err := svcPast.RunNetting(context.Background(), NettingScope{CounterpartyAccountID: 42, Currency: "USD", ValueDate: vd})
	if err == nil && rep2.Batch != nil {
		_, err = svcPast.DispatchBatch(context.Background(), rep2.Batch.ID, "FEDWIRE")
		assertCode(t, err, "RAIL_CUTOFF_EXCEEDED")
	}
}

func TestNettingBustReopens(t *testing.T) {
	st := newFakeNettingStore()
	vd := dayNil("2025-06-12")
	st.obligations = []NettingObligation{
		{InstructionID: 1, TradeID: 10, AccountID: 42, Currency: "USD", Amount: mustDec2("100"), Direction: DirectionPay, ValueDate: vd},
		{InstructionID: 2, TradeID: 11, AccountID: 42, Currency: "USD", Amount: mustDec2("40"), Direction: DirectionReceive, ValueDate: vd},
	}
	svc := nettingSvc(t, st, newFakeSsiStore(), fakeClsChecker{}, fakeCutoff{})
	rep, err := svc.RunNetting(context.Background(), NettingScope{CounterpartyAccountID: 42, Currency: "USD", ValueDate: vd})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := svc.ReopenBatch(context.Background(), rep.Batch.ID, []int64{2}, "TRADE_BUST")
	if err != nil {
		t.Fatalf("ReopenBatch: %v", err)
	}
	if b.Status != BatchOpen && b.Status != BatchNetted {
		t.Fatalf("batch must stay OPEN/NETTED after partial bust, got %s", b.Status)
	}
	// Net recompute: 100 PAY remains → 100 net.
	if !b.NetObligation.Equal(mustDec2("100")) {
		t.Fatalf("net must recompute to 100, got %s", b.NetObligation)
	}
	if b.ReopenCount != 1 {
		t.Fatalf("reopen counter must bump, got %d", b.ReopenCount)
	}
}

// ===========================================================================
// Task 24.3.12 — statement ingestion
// ===========================================================================

type fakeStatementStore struct {
	nostro     *NostroAccount
	statements map[int64]*BankStatement
	byChecksum map[string]*BankStatement
	entries    map[int64][]StatementEntryRow
	exceptions []SettlementException
	missing    []MissingPayment
	uetrHit    *EntryMatch
	refHit     *EntryMatch
	amtHit     *EntryMatch
	nextID     int64
}

func newFakeStatementStore() *fakeStatementStore {
	return &fakeStatementStore{
		nostro:     &NostroAccount{ID: 1, Currency: "USD", BankName: "FRB", BankCode: "FRNYUS33", IBAN: "US99TEST"},
		statements: map[int64]*BankStatement{}, byChecksum: map[string]*BankStatement{},
		entries: map[int64][]StatementEntryRow{}, nextID: 1,
	}
}

func (s *fakeStatementStore) NostroByID(_ context.Context, id int64) (*NostroAccount, bool, error) {
	if s.nostro != nil && s.nostro.ID == id {
		return s.nostro, true, nil
	}
	return nil, false, nil
}
func (s *fakeStatementStore) StatementByChecksum(_ context.Context, sha string) (*BankStatement, bool, error) {
	b, ok := s.byChecksum[sha]
	return b, ok, nil
}
func (s *fakeStatementStore) LatestStatement(_ context.Context, nostroID int64, stmtNo string) (*BankStatement, bool, error) {
	var best *BankStatement
	for _, b := range s.statements {
		if b.NostroAccountID == nostroID && b.StatementNumber == stmtNo {
			if best == nil || (b.SequenceNumber != nil && (best.SequenceNumber == nil || *b.SequenceNumber > *best.SequenceNumber)) {
				best = b
			}
		}
	}
	return best, best != nil, nil
}
func (s *fakeStatementStore) OpenExceptionFor(context.Context, string, int64, int64) (bool, error) {
	return false, nil
}
func (s *fakeStatementStore) ListStatements(context.Context, int64, int) ([]BankStatement, error) {
	var out []BankStatement
	for _, b := range s.statements {
		out = append(out, *b)
	}
	return out, nil
}
func (s *fakeStatementStore) ListEntries(_ context.Context, id int64) ([]StatementEntryRow, error) {
	return s.entries[id], nil
}
func (s *fakeStatementStore) MissingPayments(context.Context, int64, string, time.Time) ([]MissingPayment, error) {
	return s.missing, nil
}
func (s *fakeStatementStore) InTx(ctx context.Context, fn func(context.Context, StatementTx) error) error {
	return fn(ctx, fakeStatementTx{s})
}

type fakeStatementTx struct{ s *fakeStatementStore }

func (t fakeStatementTx) InsertStatement(_ context.Context, b BankStatement) (int64, error) {
	id := t.s.nextID
	t.s.nextID++
	cp := b
	cp.ID = id
	t.s.statements[id] = &cp
	t.s.byChecksum[b.ChecksumSHA256] = &cp
	return id, nil
}
func (t fakeStatementTx) InsertEntry(_ context.Context, stmtID int64, e ParsedEntry) (int64, bool, error) {
	for _, r := range t.s.entries[stmtID] {
		if r.Entry.EntryRef == e.EntryRef && r.Entry.UETR == e.UETR &&
			r.Entry.Amount.Equal(e.Amount) && r.Entry.Credit == e.Credit {
			return r.ID, true, nil
		}
	}
	id := t.s.nextID
	t.s.nextID++
	t.s.entries[stmtID] = append(t.s.entries[stmtID], StatementEntryRow{ID: id, Entry: e, Status: "UNMATCHED"})
	return id, false, nil
}
func (t fakeStatementTx) SetStatementStatus(_ context.Context, id int64, status string, matched, total int) error {
	t.s.statements[id].Status = status
	t.s.statements[id].MatchedCount = matched
	t.s.statements[id].EntryCount = total
	return nil
}
func (t fakeStatementTx) MatchByUETR(_ context.Context, uetr, ccy string) (*EntryMatch, bool, error) {
	if t.s.uetrHit != nil {
		return t.s.uetrHit, true, nil
	}
	return nil, false, nil
}
func (t fakeStatementTx) MatchByRef(_ context.Context, ref, ccy string) (*EntryMatch, bool, error) {
	if t.s.refHit != nil {
		return t.s.refHit, true, nil
	}
	return nil, false, nil
}
func (t fakeStatementTx) MatchByAmount(_ context.Context, ccy string, a decimal.Decimal, vd time.Time, credit bool) (*EntryMatch, bool, error) {
	if t.s.amtHit != nil {
		return t.s.amtHit, true, nil
	}
	return nil, false, nil
}
func (t fakeStatementTx) SetEntryMatch(_ context.Context, entryID int64, m EntryMatch) error {
	for k, rows := range t.s.entries {
		for i := range rows {
			if rows[i].ID == entryID {
				t.s.entries[k][i].Status = "MATCHED"
				t.s.entries[k][i].MatchKey = m.Key
			}
		}
	}
	return nil
}
func (t fakeStatementTx) MarkEntryStatus(_ context.Context, entryID int64, status string) error {
	for k, rows := range t.s.entries {
		for i := range rows {
			if rows[i].ID == entryID {
				t.s.entries[k][i].Status = status
			}
		}
	}
	return nil
}
func (t fakeStatementTx) InsertException(_ context.Context, e SettlementException) (int64, error) {
	t.s.exceptions = append(t.s.exceptions, e)
	return int64(len(t.s.exceptions)), nil
}

type fakeParser struct {
	stmt *ParsedStatement
	err  error
}

func (p *fakeParser) Parse(context.Context, string, []byte) (*ParsedStatement, error) {
	return p.stmt, p.err
}

type fakeSuspense struct {
	calls []ParsedEntry
	sid   int64
}

func (f *fakeSuspense) RouteUnmatchedCredit(_ context.Context, e ParsedEntry, _ string) (int64, error) {
	f.calls = append(f.calls, e)
	if f.sid == 0 {
		f.sid = 900
	}
	return f.sid, nil
}

func TestStatementIngestChecksumDedup(t *testing.T) {
	st := newFakeStatementStore()
	p := &fakeParser{stmt: &ParsedStatement{
		Format: "MT940", StatementNumber: "1", Currency: "USD", IBAN: "US99TEST", BIC: "FRNYUS33",
		StatementDate: dayNil("2025-06-10"),
	}}
	svc, err := NewStatementIngestionService(st, StatementIngestionOptions{Parser: p})
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	raw := []byte("statement bytes")
	res, err := svc.Ingest(context.Background(), IngestRequest{NostroAccountID: 1, Format: "MT940", Raw: raw})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	res2, err := svc.Ingest(context.Background(), IngestRequest{NostroAccountID: 1, Format: "MT940", Raw: raw})
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if !res2.Duplicate || res2.StatementID != res.StatementID {
		t.Fatalf("checksum replay must dedupe, got %+v", res2)
	}
}

func TestStatementIngestChecksumMismatch(t *testing.T) {
	st := newFakeStatementStore()
	svc, _ := NewStatementIngestionService(st, StatementIngestionOptions{Parser: &fakeParser{stmt: &ParsedStatement{Currency: "USD"}}})
	_, err := svc.Ingest(context.Background(), IngestRequest{
		NostroAccountID: 1, Format: "MT940", Raw: []byte("x"), ExpectedChecksum: "deadbeef",
	})
	assertCode(t, err, CodeStatementMalformed)
}

func TestStatementIngestAccountMismatch(t *testing.T) {
	st := newFakeStatementStore()
	svc, _ := NewStatementIngestionService(st, StatementIngestionOptions{Parser: &fakeParser{stmt: &ParsedStatement{
		Format: "MT940", StatementNumber: "1", Currency: "EUR", IBAN: "OTHER",
		StatementDate: dayNil("2025-06-10"),
	}}})
	_, err := svc.Ingest(context.Background(), IngestRequest{NostroAccountID: 1, Format: "MT940", Raw: []byte("x")})
	assertCode(t, err, CodeStatementAccountMismatch)
}

func TestStatementMatchPriorities(t *testing.T) {
	st := newFakeStatementStore()
	iid := int64(55)
	exp := mustDec2("100.00")
	st.uetrHit = &EntryMatch{InstructionID: &iid, Key: "UETR", ExpectedAmount: &exp}
	p := &fakeParser{stmt: &ParsedStatement{
		Format: "CAMT053", StatementNumber: "7", Currency: "USD", IBAN: "US99TEST",
		StatementDate: dayNil("2025-06-10"),
		Entries: []ParsedEntry{{
			UETR: "eb6305c9-1f7f-49de-aed0-16487c27b42d", EntryRef: "R1",
			Amount: mustDec2("100.00"), Currency: "USD", Credit: true,
			ValueDate: dayNil("2025-06-10"),
		}},
	}}
	svc, _ := NewStatementIngestionService(st, StatementIngestionOptions{Parser: p})
	res, err := svc.Ingest(context.Background(), IngestRequest{NostroAccountID: 1, Format: "CAMT053", Raw: []byte("x")})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Matched != 1 || res.Entries[0].MatchKey != "UETR" {
		t.Fatalf("UETR match expected, got %+v", res)
	}
}

func TestStatementUnmatchedCreditRoutesSuspense(t *testing.T) {
	st := newFakeStatementStore()
	susp := &fakeSuspense{}
	p := &fakeParser{stmt: &ParsedStatement{
		Format: "MT940", StatementNumber: "9", Currency: "USD", IBAN: "US99TEST",
		StatementDate: dayNil("2025-06-10"),
		Entries: []ParsedEntry{{
			EntryRef: "UNKNWN", BankRef: "BANKREF1", Amount: mustDec2("25.50"),
			Currency: "USD", Credit: true, ValueDate: dayNil("2025-06-10"),
			RemitterName: "Mystery Remitter",
		}},
	}}
	svc, _ := NewStatementIngestionService(st, StatementIngestionOptions{Parser: p, Suspense: susp})
	res, err := svc.Ingest(context.Background(), IngestRequest{NostroAccountID: 1, Format: "MT940", Raw: []byte("x")})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Unmatched != 1 || res.BreaksOpened == 0 {
		t.Fatalf("unmatched credit must open a break, got %+v", res)
	}
	if len(st.exceptions) == 0 || st.exceptions[0].Code != StmtExcUnexpectedCredit {
		t.Fatalf("UNEXPECTED_CREDIT expected, got %+v", st.exceptions)
	}
	if len(susp.calls) != 1 || susp.calls[0].RemitterName != "Mystery Remitter" {
		t.Fatalf("suspense router must receive the credit, got %+v", susp.calls)
	}
	if st.exceptions[0].SuspenseMappingID == nil {
		t.Fatal("break must link the suspense mapping")
	}
}

func TestStatementMissingPaymentBreak(t *testing.T) {
	st := newFakeStatementStore()
	st.missing = []MissingPayment{{
		InstructionID: 77, TradeID: 70, Currency: "USD", Amount: mustDec2("100"),
		SettlementDate: dayNil("2025-06-09"), Reference: "SWF-77",
	}}
	p := &fakeParser{stmt: &ParsedStatement{
		Format: "MT940", StatementNumber: "10", Currency: "USD", IBAN: "US99TEST",
		StatementDate: dayNil("2025-06-10"),
	}}
	svc, _ := NewStatementIngestionService(st, StatementIngestionOptions{Parser: p})
	res, err := svc.Ingest(context.Background(), IngestRequest{NostroAccountID: 1, Format: "MT940", Raw: []byte("x")})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	found := false
	for _, e := range st.exceptions {
		if e.Code == StmtExcMissingPayment {
			found = true
		}
	}
	if !found {
		t.Fatalf("MISSING_PAYMENT break expected, got %+v (res %+v)", st.exceptions, res)
	}
}

// ===========================================================================
// Task 24.3.21 — suspense router
// ===========================================================================

func TestSuspenseNilGuardFailsClosed(t *testing.T) {
	if _, err := NewSuspenseService(nil); err == nil {
		t.Fatal("nil guard must refuse construction")
	}
}

func TestSuspenseRouteUnmatchedCredit(t *testing.T) {
	svc, err := NewSuspenseService(&fakeScreener{sid: 77})
	if err != nil {
		t.Fatalf("NewSuspenseService: %v", err)
	}
	sid, err := svc.RouteUnmatchedCredit(context.Background(), ParsedEntry{
		BankRef: "BR1", Currency: "USD", Amount: mustDec2("10"),
		ValueDate: dayNil("2025-06-10"), RemitterName: "X",
	}, "BR1")
	if err != nil || sid != 77 {
		t.Fatalf("route: %v sid=%d", err, sid)
	}
}

type fakeScreener struct{ sid int64 }

func (f *fakeScreener) ScreenInbound(context.Context, SuspenseInbound) (*SuspenseScreenResult, error) {
	return &SuspenseScreenResult{Disposition: "QUARANTINED", SuspenseID: f.sid}, nil
}
func (f *fakeScreener) ResolveSuspense(_ context.Context, id int64, action string, inv int64, notes string) (*SuspenseResolveOutcome, error) {
	return &SuspenseResolveOutcome{SuspenseID: id, Action: action, Status: "RESOLVED"}, nil
}

func TestSuspenseResolvePassthrough(t *testing.T) {
	svc, _ := NewSuspenseService(&fakeScreener{sid: 5})
	out, err := svc.Resolve(context.Background(), 5, "RELEASE_TO_CLIENT", 9, "verified KYC match")
	if err != nil || out.Action != "RELEASE_TO_CLIENT" {
		t.Fatalf("resolve: %v %+v", err, out)
	}
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected coded error %s, got nil", want)
	}
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != want {
		t.Fatalf("expected code %s, got %v", want, err)
	}
}
