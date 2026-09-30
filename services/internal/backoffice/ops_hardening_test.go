// Task 24.3.19 unit tests — break aging, suspense SLA, write-off
// authority matrix, nostro funding thresholds, rail cutoffs, CLS pay-in
// ladder, FX close-out economics, rail failover, Herstatt caps, LP
// default playbook.

package backoffice

import (
	"context"
	"fmt"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// In-memory OpsStore/OpsTx fake
// ---------------------------------------------------------------------------

type soOpsStore struct {
	exceptions map[int64]*SettlementException
	breaks     map[int64]*PBReconBreak
	suspense   []SuspenseItem
	writeOffs  map[int64]*WriteOff
	thresholds map[int64]*FundingThreshold
	outflows   map[int64]decimal.Decimal // nostro id → projected outflow
	nostroBal  map[int64]decimal.Decimal
	cutoff     map[string]*CutoffRule // "rail:ccy"
	payins     map[int64]*CLSPayIn
	fails      map[int64]*SettlementFail
	tradePrice map[int64]decimal.Decimal
	closeouts  map[int64]*FXFailCloseout // by failID
	payments   map[int64]*FailoverPayment
	herstatt   map[int64]*HerstattExposure
	lpEvents   map[int64][]LPDefaultEvent // by lp
	journals   []soPostedJournal
	excEvents  []string
	nextID     int64
}

func newSoOpsStore() *soOpsStore {
	return &soOpsStore{
		exceptions: map[int64]*SettlementException{},
		breaks:     map[int64]*PBReconBreak{},
		writeOffs:  map[int64]*WriteOff{}, thresholds: map[int64]*FundingThreshold{},
		outflows: map[int64]decimal.Decimal{}, nostroBal: map[int64]decimal.Decimal{},
		cutoff: map[string]*CutoffRule{}, payins: map[int64]*CLSPayIn{},
		fails: map[int64]*SettlementFail{}, tradePrice: map[int64]decimal.Decimal{},
		closeouts: map[int64]*FXFailCloseout{}, payments: map[int64]*FailoverPayment{},
		herstatt: map[int64]*HerstattExposure{}, lpEvents: map[int64][]LPDefaultEvent{},
	}
}

func (s *soOpsStore) InTx(ctx context.Context, fn func(context.Context, OpsTx) error) error {
	return fn(ctx, s)
}

// --- aging ---
func (s *soOpsStore) StaleExceptions(_ context.Context, olderThan time.Time) ([]SettlementException, error) {
	var out []SettlementException
	for _, e := range s.exceptions {
		if e.Actionable() && e.DetectedAt.Before(olderThan) {
			out = append(out, *e)
		}
	}
	return out, nil
}

func (s *soOpsStore) StalePBBreaks(_ context.Context, olderThan time.Time) ([]PBReconBreak, error) {
	var out []PBReconBreak
	for _, b := range s.breaks {
		if (b.Status == PBBreakOpen || b.Status == PBBreakInvestigating) &&
			b.CreatedAt.Before(olderThan) {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (s *soOpsStore) MarkExceptionStatus(_ context.Context, id int64, st ExceptionStatus, _ time.Time) error {
	s.exceptions[id].Status = st
	return nil
}

func (s *soOpsStore) EscalatePBBreak(_ context.Context, id int64, at time.Time) error {
	b := s.breaks[id]
	if b.Status == PBBreakOpen {
		b.Status = PBBreakInvestigating
	}
	b.EscalatedAt = &at
	return nil
}

func (s *soOpsStore) AppendExceptionEvent(_ context.Context, _ int64, _ *int64, action string, _ map[string]any) error {
	s.excEvents = append(s.excEvents, action)
	return nil
}

func (s *soOpsStore) AppendPBBreakEvent(_ context.Context, _ *int64, _ *int64, action string, _ map[string]any) error {
	s.excEvents = append(s.excEvents, action)
	return nil
}

func (s *soOpsStore) SuspenseBreaches(_ context.Context, at time.Time) ([]SuspenseItem, error) {
	var out []SuspenseItem
	for _, i := range s.suspense {
		if i.QuarantineState != "CLEARED" && at.After(i.SLAExpiresAt) {
			out = append(out, i)
		}
	}
	return out, nil
}

// --- write-offs ---
func (s *soOpsStore) InsertWriteOff(_ context.Context, w WriteOff) (WriteOff, error) {
	s.nextID++
	cp := w
	cp.ID = s.nextID
	s.writeOffs[cp.ID] = &cp
	return cp, nil
}

func (s *soOpsStore) WriteOffByID(_ context.Context, id int64) (WriteOff, bool, error) {
	if w, ok := s.writeOffs[id]; ok {
		return *w, true, nil
	}
	return WriteOff{}, false, nil
}

func (s *soOpsStore) LockWriteOff(_ context.Context, id int64) (WriteOff, bool, error) {
	return s.WriteOffByID(context.Background(), id)
}

func (s *soOpsStore) ExecuteWriteOff(_ context.Context, id, jid, approver, dc int64, _ time.Time) error {
	w := s.writeOffs[id]
	if w.Status != "PENDING" {
		return fmt.Errorf("conflict")
	}
	w.Status = "EXECUTED"
	w.GLJournalID = &jid
	w.ApprovedBy = &approver
	w.DualControlID = &dc
	return nil
}

func (s *soOpsStore) MarkExceptionWrittenOff(_ context.Context, id int64, _ time.Time) error {
	s.exceptions[id].Status = ExcStatusWrittenOff
	return nil
}

func (s *soOpsStore) MarkPBBreakWrittenOff(_ context.Context, id int64, _ time.Time) error {
	s.breaks[id].Status = PBBreakWrittenOff
	return nil
}

func (s *soOpsStore) PostJournal(_ context.Context, entryType, description,
	postedBy, idemKey string, _ int64, lines []JournalLine) (int64, error) {
	s.journals = append(s.journals, soPostedJournal{
		entryType: entryType, description: description, idemKey: idemKey, lines: lines})
	return int64(len(s.journals)), nil
}

// --- funding thresholds ---
func (s *soOpsStore) Thresholds(_ context.Context) ([]FundingThreshold, error) {
	var out []FundingThreshold
	for _, t := range s.thresholds {
		out = append(out, *t)
	}
	return out, nil
}

func (s *soOpsStore) ThresholdRowsForUpdate(ctx context.Context) ([]FundingThreshold, error) {
	return s.Thresholds(ctx)
}

func (s *soOpsStore) ProjectedOutflow(_ context.Context, nostro int64, _ string, days int) (decimal.Decimal, error) {
	return s.outflows[nostro].Mul(decimal.NewFromInt(int64(days))), nil
}

func (s *soOpsStore) NostroBalance(_ context.Context, nostro int64) (decimal.Decimal, error) {
	return s.nostroBal[nostro], nil
}

func (s *soOpsStore) UpdateThreshold(_ context.Context, id int64, computed decimal.Decimal, breach bool, _ time.Time) error {
	t := s.thresholds[id]
	t.ComputedThreshold = computed
	t.Breach = breach
	return nil
}

// --- cutoffs ---
func (s *soOpsStore) CutoffRule(_ context.Context, rail, ccy string) (CutoffRule, bool, error) {
	r, ok := s.cutoff[rail+":"+ccy]
	if !ok {
		return CutoffRule{}, false, nil
	}
	return *r, true, nil
}

// --- CLS pay-ins ---
func (s *soOpsStore) UpsertCLSPayIn(_ context.Context, p CLSPayIn) (CLSPayIn, error) {
	for _, e := range s.payins {
		if e.Currency == p.Currency && e.ValueDate.Equal(p.ValueDate) &&
			e.NostroAccountID == p.NostroAccountID {
			return *e, nil
		}
	}
	s.nextID++
	cp := p
	cp.ID = s.nextID
	s.payins[cp.ID] = &cp
	return cp, nil
}

func (s *soOpsStore) LockCLSPayIn(_ context.Context, id int64) (CLSPayIn, bool, error) {
	if p, ok := s.payins[id]; ok {
		return *p, true, nil
	}
	return CLSPayIn{}, false, nil
}

func (s *soOpsStore) UpdateCLSPayIn(_ context.Context, p CLSPayIn) error {
	cp := p
	s.payins[p.ID] = &cp
	return nil
}

func (s *soOpsStore) DueCLSPayIns(_ context.Context, _ time.Time) ([]CLSPayIn, error) {
	var out []CLSPayIn
	for _, p := range s.payins {
		out = append(out, *p)
	}
	return out, nil
}

// --- FX close-out ---
func (s *soOpsStore) FailByIDForUpdate(_ context.Context, id int64) (SettlementFail, bool, error) {
	if f, ok := s.fails[id]; ok {
		return *f, true, nil
	}
	return SettlementFail{}, false, nil
}

func (s *soOpsStore) TradePrice(_ context.Context, id int64) (decimal.Decimal, bool, error) {
	p, ok := s.tradePrice[id]
	return p, ok, nil
}

func (s *soOpsStore) InsertFXCloseout(_ context.Context, c FXFailCloseout) (FXFailCloseout, bool, error) {
	if e, ok := s.closeouts[c.FailID]; ok {
		return *e, false, nil
	}
	s.nextID++
	cp := c
	cp.ID = s.nextID
	s.closeouts[c.FailID] = &cp
	return cp, true, nil
}

func (s *soOpsStore) SetFailStatus(_ context.Context, id int64, st FailStatus, at time.Time) error {
	f := s.fails[id]
	if f.Status == st {
		return nil
	}
	f.Status = st
	if st == FailClosedOut {
		f.ClosedOutAt = &at
	}
	return nil
}

// --- rail failover ---
func (s *soOpsStore) EnqueuePayment(_ context.Context, p FailoverPayment) (FailoverPayment, bool, error) {
	for _, e := range s.payments {
		if e.PaymentRef == p.PaymentRef {
			return *e, false, nil
		}
	}
	s.nextID++
	cp := p
	cp.ID = s.nextID
	s.payments[cp.ID] = &cp
	return cp, true, nil
}

func (s *soOpsStore) DueFailoverPayments(_ context.Context, at time.Time) ([]FailoverPayment, error) {
	var out []FailoverPayment
	for _, p := range s.payments {
		if (p.Status == "QUEUED" || p.Status == "RETRYING") && !p.NextAttemptAt.After(at) {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *soOpsStore) LockFailoverPayment(_ context.Context, id int64) (FailoverPayment, bool, error) {
	if p, ok := s.payments[id]; ok {
		return *p, true, nil
	}
	return FailoverPayment{}, false, nil
}

func (s *soOpsStore) UpdateFailoverPayment(_ context.Context, p FailoverPayment) error {
	cp := p
	s.payments[p.ID] = &cp
	return nil
}

// --- Herstatt ---
func (s *soOpsStore) OpenHerstatt(_ context.Context) ([]HerstattExposure, error) {
	var out []HerstattExposure
	for _, h := range s.herstatt {
		if h.SettledAt == nil {
			out = append(out, *h)
		}
	}
	return out, nil
}

func (s *soOpsStore) OpenHerstattForUpdate(ctx context.Context) ([]HerstattExposure, error) {
	return s.OpenHerstatt(ctx)
}

func (s *soOpsStore) InsertHerstatt(_ context.Context, h HerstattExposure) (HerstattExposure, error) {
	s.nextID++
	cp := h
	cp.ID = s.nextID
	s.herstatt[cp.ID] = &cp
	return cp, nil
}

func (s *soOpsStore) SettleHerstatt(_ context.Context, id int64, at time.Time) error {
	h := s.herstatt[id]
	if h.SettledAt != nil {
		return fmt.Errorf("conflict")
	}
	h.SettledAt = &at
	return nil
}

func (s *soOpsStore) MarkHerstattBreached(_ context.Context, id int64) error {
	s.herstatt[id].Breached = true
	return nil
}

// --- LP default ---
func (s *soOpsStore) LPDefaultLatest(_ context.Context, lpID int64) (LPDefaultEvent, bool, error) {
	return s.LPDefaultLatestTx(context.Background(), lpID)
}

func (s *soOpsStore) LPDefaultLatestTx(_ context.Context, lpID int64) (LPDefaultEvent, bool, error) {
	evs := s.lpEvents[lpID]
	if len(evs) == 0 {
		return LPDefaultEvent{}, false, nil
	}
	return evs[len(evs)-1], true, nil
}

func (s *soOpsStore) InsertLPDefault(_ context.Context, e LPDefaultEvent) (LPDefaultEvent, error) {
	s.nextID++
	cp := e
	cp.ID = s.nextID
	s.lpEvents[e.LPID] = append(s.lpEvents[e.LPID], cp)
	return cp, nil
}

func (s *soOpsStore) RestitutionsUndelivered(_ context.Context) ([]Restitution, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Shared fakes
// ---------------------------------------------------------------------------

type soAlertSink struct{ alerts []string }

func (a *soAlertSink) Raise(_ context.Context, _, code, _ string, _ map[string]string) error {
	a.alerts = append(a.alerts, code)
	return nil
}

type soCutoffs struct{ dec CutoffDecisionView }

func (c *soCutoffs) Evaluate(_, _ string, _ time.Time) (CutoffDecisionView, error) {
	return c.dec, nil
}

type soDispatcher struct{ fail map[string]bool }

func (d *soDispatcher) Dispatch(_ context.Context, rail string, _ map[string]any) error {
	if d.fail[rail] {
		return fmt.Errorf("rail %s down", rail)
	}
	return nil
}

func soOpsSvc(st *soOpsStore) (*OpsService, *soAlertSink) {
	al := &soAlertSink{}
	return NewOpsService(st).WithAlerter(al), al
}

// ---------------------------------------------------------------------------
// 1. Aging + suspense SLA
// ---------------------------------------------------------------------------

func TestOps_SweepAgingBuckets(t *testing.T) {
	st := newSoOpsStore()
	now := time.Now().UTC()
	st.exceptions[1] = &SettlementException{ID: 1, Status: ExcStatusOpen,
		DetectedAt: now.Add(-26 * time.Hour)} // T+1 → investigate
	st.exceptions[2] = &SettlementException{ID: 2, Status: ExcStatusOpen,
		DetectedAt: now.Add(-72 * time.Hour)} // T+2 → investigate + escalate
	st.exceptions[3] = &SettlementException{ID: 3, Status: ExcStatusInvestigating,
		DetectedAt: now.Add(-6 * 24 * time.Hour)} // T+5 → write-off review
	st.breaks[1] = &PBReconBreak{ID: 1, Status: PBBreakOpen,
		CreatedAt: now.Add(-26 * time.Hour)} // → investigating/escalated
	st.suspense = []SuspenseItem{
		{ID: 1, SLAExpiresAt: now.Add(-1 * time.Hour)}, // breached
		{ID: 2, SLAExpiresAt: now.Add(24 * time.Hour)}, // in SLA
	}
	svc, al := soOpsSvc(st)
	res, err := svc.SweepAging(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Investigated != 3 { // exceptions 1,2 + break 1
		t.Fatalf("investigated %d", res.Investigated)
	}
	if res.Escalated != 1 || res.WriteOffReview != 1 || res.SuspenseBreaches != 1 {
		t.Fatalf("buckets %+v", res)
	}
	if st.exceptions[1].Status != ExcStatusInvestigating ||
		st.breaks[1].Status != PBBreakInvestigating {
		t.Fatalf("aging transitions missing")
	}
	var sawWO, sawSLA bool
	for _, a := range al.alerts {
		if a == "WRITE_OFF_REVIEW_DUE" {
			sawWO = true
		}
		if a == CodeSuspenseSLABreach {
			sawSLA = true
		}
	}
	if !sawWO || !sawSLA {
		t.Fatalf("alerts missing: %v", al.alerts)
	}
}

// ---------------------------------------------------------------------------
// 2. Write-off authority matrix + dual control + balanced journal
// ---------------------------------------------------------------------------

func TestOps_WriteOffAuthorityMatrix(t *testing.T) {
	cases := []struct {
		amt  string
		tier string
		role string
	}{
		{"0", "T1", "Finance Ops"}, {"1000", "T1", "Finance Ops"},
		{"1000.01", "T2", "Risk Manager"}, {"100000", "T2", "Risk Manager"},
		{"100000.01", "T3", "Super Admin"}, {"999999999", "T3", "Super Admin"},
	}
	for _, c := range cases {
		tier, role, err := WriteOffTierFor(decimal.RequireFromString(c.amt))
		if err != nil || tier != c.tier || role != c.role {
			t.Fatalf("amt %s → %s/%s err %v", c.amt, tier, role, err)
		}
	}
}

func TestOps_RequestWriteOffValidationAndDual(t *testing.T) {
	st := newSoOpsStore()
	svc, _ := soOpsSvc(st)
	exc := int64(7)
	// No dual queue → fail closed.
	if _, _, err := svc.RequestWriteOff(context.Background(), &exc, nil,
		"USD", decimal.NewFromInt(10), "r", 9); errCode(err) != CodeServiceDegraded {
		t.Fatalf("want SERVICE_DEGRADED, got %v", err)
	}
	dual := &soDualQueue{}
	svc.WithDual(dual)
	// No target → invalid.
	if _, _, err := svc.RequestWriteOff(context.Background(), nil, nil,
		"USD", decimal.NewFromInt(10), "r", 9); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
	// Negative amount / missing reason → invalid.
	if _, _, err := svc.RequestWriteOff(context.Background(), &exc, nil,
		"USD", decimal.NewFromInt(-10), "r", 9); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
	if _, _, err := svc.RequestWriteOff(context.Background(), &exc, nil,
		"USD", decimal.NewFromInt(10), "", 9); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
	// Tier routes the approval role.
	w, dc, err := svc.RequestWriteOff(context.Background(), &exc, nil,
		"USD", decimal.NewFromInt(50_000), "unrecoverable break", 9)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if w.Status != "PENDING" || w.Tier != "T2" || w.RequiredRole != "Risk Manager" {
		t.Fatalf("bad write-off %+v", w)
	}
	if dc.RequiredRole != "Risk Manager" || dc.Operation != OpSettlementWriteOff {
		t.Fatalf("dual submit %+v", dc)
	}
	if w.DualControlID == nil || *w.DualControlID != dc.ID {
		t.Fatalf("dual id not stamped")
	}
}

func TestOps_ApplyWriteOffJournalsAndClosesTarget(t *testing.T) {
	st := newSoOpsStore()
	st.exceptions[7] = &SettlementException{ID: 7, Status: ExcStatusInvestigating}
	svc, _ := soOpsSvc(st)
	svc.WithDual(&soDualQueue{})
	exc := int64(7)
	w, _, err := svc.RequestWriteOff(context.Background(), &exc, nil,
		"USD", decimal.NewFromInt(500), "write-off", 9)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	err = st.InTx(context.Background(), func(ctx context.Context, tx OpsTx) error {
		return svc.ApplyWriteOff(ctx, tx, w.ID, 10, 55)
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if st.writeOffs[w.ID].Status != "EXECUTED" || st.writeOffs[w.ID].GLJournalID == nil {
		t.Fatalf("write-off not executed: %+v", st.writeOffs[w.ID])
	}
	if len(st.journals) != 1 {
		t.Fatalf("journal missing")
	}
	var dr, cr decimal.Decimal
	for _, l := range st.journals[0].lines {
		dr = dr.Add(l.Debit)
		cr = cr.Add(l.Credit)
	}
	if !dr.Equal(decimal.NewFromInt(500)) || !dr.Equal(cr) {
		t.Fatalf("unbalanced journal dr=%s cr=%s", dr, cr)
	}
	if st.exceptions[7].Status != ExcStatusWrittenOff {
		t.Fatalf("exception not WRITTEN_OFF")
	}
	// Double-apply → conflict.
	err = st.InTx(context.Background(), func(ctx context.Context, tx OpsTx) error {
		return svc.ApplyWriteOff(ctx, tx, w.ID, 10, 55)
	})
	if errCode(err) != CodeExceptionConflict {
		t.Fatalf("want %s, got %v", CodeExceptionConflict, err)
	}
}

// ---------------------------------------------------------------------------
// 3. Nostro funding thresholds
// ---------------------------------------------------------------------------

func TestOps_NostroThresholdBreach(t *testing.T) {
	st := newSoOpsStore()
	st.thresholds[1] = &FundingThreshold{ID: 1, NostroAccountID: 50,
		Currency: "USD", MinOutflowDays: 3,
		CLSPayInCover: decimal.RequireFromString("100000")}
	st.outflows[50] = decimal.RequireFromString("200000") // /day → 600k over 3d
	st.nostroBal[50] = decimal.RequireFromString("650000")
	st.thresholds[2] = &FundingThreshold{ID: 2, NostroAccountID: 51,
		Currency: "EUR", MinOutflowDays: 0, // default 3d
		CLSPayInCover: decimal.RequireFromString("0")}
	st.outflows[51] = decimal.RequireFromString("100000")
	st.nostroBal[51] = decimal.RequireFromString("500000")
	svc, al := soOpsSvc(st)
	n, err := svc.EvaluateNostroThresholds(context.Background())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 breach, got %d", n)
	}
	if !st.thresholds[1].Breach || st.thresholds[2].Breach {
		t.Fatalf("breach flags wrong: %+v %+v", st.thresholds[1], st.thresholds[2])
	}
	// cover = 3×200k + 100k CLS = 700k > 650k balance.
	if st.thresholds[1].ComputedThreshold.String() != "700000" {
		t.Fatalf("threshold %s", st.thresholds[1].ComputedThreshold)
	}
	var saw bool
	for _, a := range al.alerts {
		if a == CodeNostroFundingBreach {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("funding breach alert missing")
	}
}

// ---------------------------------------------------------------------------
// 4. Rail cutoff — late payment fee + value-date roll
// ---------------------------------------------------------------------------

func TestOps_CheckCutoffLatePayment(t *testing.T) {
	st := newSoOpsStore()
	st.cutoff["SWIFT:USD"] = &CutoffRule{Rail: "SWIFT", Currency: "USD",
		CutoffUTC: "17:00", Roll: true, LateFee: decimal.RequireFromString("25")}
	svc, _ := soOpsSvc(st)
	svc.WithCutoffs(&soCutoffs{dec: CutoffDecisionView{
		CutoffPassed: true, QueuedForNextCycle: true,
		ValueDate: time.Now().Add(24 * time.Hour)}})
	ev, err := svc.CheckCutoff(context.Background(), "SWIFT", "USD", time.Now())
	if err != nil {
		t.Fatalf("cutoff: %v", err)
	}
	if !ev.Late || !ev.RollValueDate || ev.LateFee.String() != "25" {
		t.Fatalf("late verdict %+v", ev)
	}
	// Unknown rail → NOT_FOUND; missing evaluator → degraded.
	if _, err := svc.CheckCutoff(context.Background(), "SEPA", "EUR", time.Now()); errCode(err) != "NOT_FOUND" {
		t.Fatalf("want NOT_FOUND, got %v", err)
	}
	svc2, _ := soOpsSvc(st) // no evaluator
	if _, err := svc2.CheckCutoff(context.Background(), "SWIFT", "USD", time.Now()); errCode(err) != CodeServiceDegraded {
		t.Fatalf("want SERVICE_DEGRADED, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. CLS pay-in operations
// ---------------------------------------------------------------------------

func TestOps_CLSPrefundDeadlineIsTMinus1(t *testing.T) {
	vd := time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC)
	dl := CLSPrefundDeadline(vd)
	if dl.Year() != 2025 || dl.Month() != 3 || dl.Day() != 9 ||
		dl.Hour() != 22 || dl.Minute() != 0 {
		t.Fatalf("deadline %v", dl)
	}
}

func TestOps_CLSPayInFundingAndLadder(t *testing.T) {
	st := newSoOpsStore()
	vd := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	svc, _ := soOpsSvc(st)
	p, err := svc.ScheduleCLSPayIn(context.Background(), "USD", vd, 50,
		decimal.RequireFromString("1000000"))
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if !p.PrefundDeadline.Equal(CLSPrefundDeadline(vd)) || p.Status != "SCHEDULED" {
		t.Fatalf("bad schedule %+v", p)
	}
	// Partial funding → SHORT.
	p2, err := svc.RecordCLSFunding(context.Background(), p.ID, decimal.RequireFromString("400000"))
	if err != nil || p2.Status != "SHORT" {
		t.Fatalf("partial funding %+v err %v", p2, err)
	}
	// Full cover → FUNDED.
	p3, err := svc.RecordCLSFunding(context.Background(), p.ID, decimal.RequireFromString("600000"))
	if err != nil || p3.Status != "FUNDED" {
		t.Fatalf("full funding %+v err %v", p3, err)
	}
}

func TestOps_CLSPayInLadderEscalatesToFailed(t *testing.T) {
	st := newSoOpsStore()
	svc, _ := soOpsSvc(st)
	// Underfunded past deadline → ladder advances one rung per monitor pass.
	vd := time.Now().UTC().Add(-24 * time.Hour) // value date passed
	p, _ := svc.ScheduleCLSPayIn(context.Background(), "USD", vd, 50,
		decimal.RequireFromString("1000000"))
	for i := 1; i <= 4; i++ {
		n, err := svc.CLSPayInMonitor(context.Background())
		if err != nil {
			t.Fatalf("monitor %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("pass %d: want 1 advance, got %d", i, n)
		}
	}
	got := st.payins[p.ID]
	if got.LadderStep != 4 || got.Status != "FAILED" || got.Fallback != "CONTROLLED_GROSS" {
		t.Fatalf("ladder end state %+v", got)
	}
	// Steady state: FAILED is terminal for the monitor.
	if n, _ := svc.CLSPayInMonitor(context.Background()); n != 0 {
		t.Fatalf("terminal pay-in still advancing")
	}
}

func TestOps_CLSMemberOutageArmsFallback(t *testing.T) {
	st := newSoOpsStore()
	svc, _ := soOpsSvc(st)
	vd := time.Now().UTC().Add(24 * time.Hour)
	p, _ := svc.ScheduleCLSPayIn(context.Background(), "USD", vd, 50,
		decimal.RequireFromString("100"))
	// Funded but member outage → NETTING fallback armed, no ladder.
	st.payins[p.ID].MemberOutage = true
	st.payins[p.ID].Funded = st.payins[p.ID].Required
	if n, err := svc.CLSPayInMonitor(context.Background()); err != nil || n != 1 {
		t.Fatalf("monitor: %d %v", n, err)
	}
	if st.payins[p.ID].Fallback != "NETTING" || st.payins[p.ID].LadderStep != 0 {
		t.Fatalf("member-outage fallback not armed: %+v", st.payins[p.ID])
	}
}

// ---------------------------------------------------------------------------
// 6. FX fail close-out economics (§17.14.5 — supersedes CSDR for FX)
// ---------------------------------------------------------------------------

func TestOps_FXCloseoutEconomics(t *testing.T) {
	st := newSoOpsStore()
	now := time.Now().UTC().Truncate(24 * time.Hour)
	isd := now.Add(-72 * time.Hour) // ISD 3 days ago → accrual ISD+1..now = 3d
	st.fails[1] = &SettlementFail{ID: 1, InstructionID: 10, TradeID: 55,
		AccountID: 7, Currency: "EUR", Amount: decimal.RequireFromString("1000000"),
		ISD: isd, Regime: RegimeFXCloseout, Status: FailOpen}
	st.tradePrice[55] = decimal.RequireFromString("1.1000")
	svc, _ := soOpsSvc(st)
	svc.SetClockForTest(func() time.Time { return now.Add(12 * time.Hour) })

	co, err := svc.CloseOutFXFail(context.Background(), 1, FXCloseoutInput{
		PolicyRateBP: decimal.RequireFromString("300"),
		MarkRate:     decimal.RequireFromString("1.1200")})
	if err != nil {
		t.Fatalf("closeout: %v", err)
	}
	// Replacement cost: |1.12−1.10| × €1,000,000 = 20,000.
	if co.ReplacementCost.String() != "20000" {
		t.Fatalf("replacement %s", co.ReplacementCost)
	}
	// Fail interest: 1M × (300+100)bp/10000 × 3d/360 = 40000×3/360 = 333.33...
	want := decimal.RequireFromString("40000").
		Mul(decimal.NewFromInt(3)).Div(decimal.NewFromInt(360))
	if !co.FailInterest.Equal(want) || co.AccrualDays != 3 {
		t.Fatalf("interest %s days %d want %s/3", co.FailInterest, co.AccrualDays, want)
	}
	if co.Treatment != "ISDA_CLOSEOUT" {
		t.Fatalf("treatment %s", co.Treatment)
	}
	if st.fails[1].Status != FailClosedOut || st.fails[1].ClosedOutAt == nil {
		t.Fatalf("fail not CLOSED_OUT")
	}
	// GL: replacement claim + fail-interest accrual, balanced.
	if len(st.journals) != 1 {
		t.Fatalf("journal missing")
	}
	var dr, cr decimal.Decimal
	for _, l := range st.journals[0].lines {
		dr = dr.Add(l.Debit)
		cr = cr.Add(l.Credit)
	}
	if !dr.Equal(cr) {
		t.Fatalf("unbalanced journal dr=%s cr=%s", dr, cr)
	}
}

func TestOps_FXCloseoutGuards(t *testing.T) {
	st := newSoOpsStore()
	st.fails[1] = &SettlementFail{ID: 1, TradeID: 55, Regime: RegimeCSDR, Status: FailOpen}
	st.fails[2] = &SettlementFail{ID: 2, TradeID: 55, Regime: RegimeFXCloseout, Status: FailClosedOut}
	st.tradePrice[55] = decimal.RequireFromString("1.1")
	svc, _ := soOpsSvc(st)
	// CSDR regime rejects FX close-out.
	if _, err := svc.CloseOutFXFail(context.Background(), 1, FXCloseoutInput{
		MarkRate: decimal.RequireFromString("1.2")}); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
	// Already closed-out → conflict.
	if _, err := svc.CloseOutFXFail(context.Background(), 2, FXCloseoutInput{
		MarkRate: decimal.RequireFromString("1.2")}); errCode(err) != CodeExceptionConflict {
		t.Fatalf("want %s, got %v", CodeExceptionConflict, err)
	}
	// No mark + no pricer → fail closed.
	st.fails[3] = &SettlementFail{ID: 3, TradeID: 55, Regime: RegimeFXCloseout, Status: FailOpen}
	if _, err := svc.CloseOutFXFail(context.Background(), 3, FXCloseoutInput{}); errCode(err) != CodeServiceDegraded {
		t.Fatalf("want SERVICE_DEGRADED, got %v", err)
	}
	// With pricer seam.
	svc.WithPricer(soPricer{marks: map[int64]decimal.Decimal{55: decimal.RequireFromString("1.15")}})
	if _, err := svc.CloseOutFXFail(context.Background(), 3, FXCloseoutInput{
		PolicyRateBP: decimal.RequireFromString("100")}); err != nil {
		t.Fatalf("pricer closeout: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 7. Rail failover — retry ladder, fallback rail, duplicate guard
// ---------------------------------------------------------------------------

func TestOps_QueuePaymentDuplicateGuard(t *testing.T) {
	st := newSoOpsStore()
	svc, _ := soOpsSvc(st)
	p1, created, err := svc.QueuePayment(context.Background(), FailoverPayment{
		PaymentRef: "REF-1", Rail: "FEDNOW", Currency: "USD",
		Amount: decimal.RequireFromString("5000")})
	if err != nil || !created {
		t.Fatalf("enqueue: %v created %v", err, created)
	}
	if p1.FallbackRail != "ACH" || p1.Status != "QUEUED" {
		t.Fatalf("fallback not armed: %+v", p1)
	}
	p2, created, err := svc.QueuePayment(context.Background(), FailoverPayment{
		PaymentRef: "REF-1", Rail: "FEDNOW", Currency: "USD",
		Amount: decimal.RequireFromString("5000")})
	if err != nil || created || p2.ID != p1.ID {
		t.Fatalf("duplicate enqueue: %+v created %v", p2, created)
	}
}

func TestOps_RailFailoverLadderAndExhaustion(t *testing.T) {
	st := newSoOpsStore()
	disp := &soDispatcher{fail: map[string]bool{"SWIFT": true, "CORRESPONDENT_BACKUP": true}}
	svc, al := soOpsSvc(st)
	svc.WithDispatcher(disp)
	p, _, err := svc.QueuePayment(context.Background(), FailoverPayment{
		PaymentRef: "REF-9", Rail: "SWIFT", Currency: "USD",
		Amount: decimal.RequireFromString("100")})
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	now := time.Now()
	svc.SetClockForTest(func() time.Time { return now })

	// Attempts 1,2 → RETRYING on SWIFT; attempt 3 → fails over to backup.
	for i := 1; i <= 3; i++ {
		if _, err := svc.ProcessFailover(context.Background()); err != nil {
			t.Fatalf("failover pass %d: %v", i, err)
		}
		got := st.payments[p.ID]
		if i < 3 {
			if got.Status != "RETRYING" || got.Attempts != i || got.Rail != "SWIFT" {
				t.Fatalf("pass %d state %+v", i, got)
			}
		} else if got.Rail != "CORRESPONDENT_BACKUP" || got.Attempts != 0 || got.Status != "RETRYING" {
			t.Fatalf("failover did not switch rails: %+v", got)
		}
		now = now.Add(5 * time.Hour) // jump past any retry delay
	}
	// Backup rail exhausts after 3 more failures → EXHAUSTED + P0.
	for i := 1; i <= 3; i++ {
		if _, err := svc.ProcessFailover(context.Background()); err != nil {
			t.Fatalf("backup pass %d: %v", i, err)
		}
		now = now.Add(5 * time.Hour)
	}
	got := st.payments[p.ID]
	if got.Status != "EXHAUSTED" {
		t.Fatalf("not exhausted: %+v", got)
	}
	var p0 bool
	for _, a := range al.alerts {
		if a == CodeRailFailoverExhausted {
			p0 = true
		}
	}
	if !p0 {
		t.Fatalf("exhausted alert missing: %v", al.alerts)
	}
	// Terminal rows are never re-dispatched.
	if n, err := svc.ProcessFailover(context.Background()); err != nil || n != 0 {
		t.Fatalf("terminal payment redispatched: %d %v", n, err)
	}
}

func TestOps_RailFailoverDispatchSuccess(t *testing.T) {
	st := newSoOpsStore()
	svc, _ := soOpsSvc(st)
	svc.WithDispatcher(&soDispatcher{fail: map[string]bool{}})
	p, _, _ := svc.QueuePayment(context.Background(), FailoverPayment{
		PaymentRef: "REF-OK", Rail: "FEDNOW", Currency: "USD",
		Amount: decimal.RequireFromString("10")})
	if n, err := svc.ProcessFailover(context.Background()); err != nil || n != 1 {
		t.Fatalf("dispatch: %d %v", n, err)
	}
	if st.payments[p.ID].Status != "DISPATCHED" {
		t.Fatalf("not dispatched: %+v", st.payments[p.ID])
	}
	// DISPATCHED latch — never re-sent (duplicate-payment guard).
	if n, _ := svc.ProcessFailover(context.Background()); n != 0 {
		t.Fatalf("dispatched payment resent")
	}
}

// ---------------------------------------------------------------------------
// 8. Herstatt exposure
// ---------------------------------------------------------------------------

func TestOps_HerstattBreachAndSettle(t *testing.T) {
	st := newSoOpsStore()
	svc, al := soOpsSvc(st)
	h, err := svc.OpenHerstatt(context.Background(), 77, "USD",
		decimal.RequireFromString("1000000"), decimal.RequireFromString("1000000"), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if h.CapMinutes != HerstattDefaultCapMinutes {
		t.Fatalf("cap %d", h.CapMinutes)
	}
	// Within cap → no breach.
	if n, err := svc.MonitorHerstatt(context.Background()); err != nil || n != 0 {
		t.Fatalf("early monitor: %d %v", n, err)
	}
	// Push past cap.
	st.herstatt[h.ID].WindowOpenedAt = time.Now().Add(-3 * time.Hour)
	if n, err := svc.MonitorHerstatt(context.Background()); err != nil || n != 1 {
		t.Fatalf("breach monitor: %d %v", n, err)
	}
	var saw bool
	for _, a := range al.alerts {
		if a == CodeHerstattLimitBreach {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("Herstatt alert missing")
	}
	// Breached flag dedupes the page.
	if n, _ := svc.MonitorHerstatt(context.Background()); n != 0 {
		t.Fatalf("double-breached")
	}
	// Counter-leg arrives → window closes.
	if err := svc.SettleHerstatt(context.Background(), h.ID); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if st.herstatt[h.ID].SettledAt == nil {
		t.Fatalf("not settled")
	}
}

// ---------------------------------------------------------------------------
// 9. LP-default playbook
// ---------------------------------------------------------------------------

func TestOps_LPDefaultLadder(t *testing.T) {
	st := newSoOpsStore()
	svc, _ := soOpsSvc(st)
	ctx := context.Background()
	// Unknown stage.
	if _, err := svc.RecordLPDefault(ctx, 5, "NOPE", nil); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
	e1, err := svc.RecordLPDefault(ctx, 5, "QUOTE_WITHDRAWN", nil)
	if err != nil || e1.Stage != "QUOTE_WITHDRAWN" {
		t.Fatalf("stage1: %v %+v", err, e1)
	}
	// Regression is refused (returns latest, no new row).
	e, err := svc.RecordLPDefault(ctx, 5, "FLOORS_WIDENED", nil)
	if err != nil || e.Stage != "FLOORS_WIDENED" {
		t.Fatalf("stage2: %v %+v", err, e)
	}
	e, err = svc.RecordLPDefault(ctx, 5, "QUOTE_WITHDRAWN", nil)
	if err != nil || e.Stage != "FLOORS_WIDENED" {
		t.Fatalf("regression should return latest FLOORS_WIDENED, got %+v", e)
	}
	if len(st.lpEvents[5]) != 2 {
		t.Fatalf("regression inserted a row")
	}
	// Terminal: ADL quintile 5.
	e, err = svc.RecordLPDefault(ctx, 5, "ADL_Q5", nil)
	if err != nil || e.Stage != "ADL_Q5" {
		t.Fatalf("adl: %v %+v", err, e)
	}
}

func TestOps_NilStoreFailsClosed(t *testing.T) {
	svc := NewOpsService(nil)
	ctx := context.Background()
	if _, err := svc.SweepAging(ctx); errCode(err) != CodeServiceDegraded {
		t.Fatalf("sweep: %v", err)
	}
	if _, _, err := svc.QueuePayment(ctx, FailoverPayment{PaymentRef: "x", Rail: "SWIFT"}); errCode(err) != CodeServiceDegraded {
		t.Fatalf("queue: %v", err)
	}
	if _, err := svc.ProcessFailover(ctx); errCode(err) != CodeServiceDegraded {
		t.Fatalf("failover: %v", err)
	}
	if _, err := svc.MonitorHerstatt(ctx); errCode(err) != CodeServiceDegraded {
		t.Fatalf("herstatt: %v", err)
	}
	if _, err := svc.RecordLPDefault(ctx, 1, "QUOTE_WITHDRAWN", nil); errCode(err) != CodeServiceDegraded {
		t.Fatalf("lp: %v", err)
	}
}
