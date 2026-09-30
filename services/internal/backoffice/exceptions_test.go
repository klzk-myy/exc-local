// Task 24.3.6 unit tests — failed-settlement detection, workflow,
// retry/reverse mechanics, dual-control gating, fail-closed seams.

package backoffice

import (
	"context"
	"fmt"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// In-memory ExceptionStore/ExceptionTx fake
// ---------------------------------------------------------------------------

type soBalance struct {
	avail decimal.Decimal
	lockd decimal.Decimal
}

type soPostedJournal struct {
	entryType   string
	description string
	idemKey     string
	lines       []JournalLine
}

type soExceptionStore struct {
	exc       map[int64]*SettlementException
	legs      map[int64]*ExceptionLeg
	balances  map[string]*soBalance
	movements map[int64]*soMovement // instructionID → movement
	journals  []soPostedJournal
	events    []string
	nextID    int64
	failInTx  error
}

type soMovement struct {
	status      string // PENDING | POSTED | VOID
	compensated bool
}

func newSoExceptionStore() *soExceptionStore {
	return &soExceptionStore{
		exc: map[int64]*SettlementException{}, legs: map[int64]*ExceptionLeg{},
		balances: map[string]*soBalance{}, movements: map[int64]*soMovement{},
	}
}

func (s *soExceptionStore) InTx(ctx context.Context, fn func(ctx context.Context, tx ExceptionTx) error) error {
	if s.failInTx != nil {
		return s.failInTx
	}
	return fn(ctx, s)
}

func (s *soExceptionStore) InsertException(_ context.Context, e SettlementException) (SettlementException, error) {
	s.nextID++
	cp := e
	cp.ID = s.nextID
	s.exc[cp.ID] = &cp
	return cp, nil
}

func (s *soExceptionStore) ExceptionByID(_ context.Context, id int64) (SettlementException, error) {
	if e, ok := s.exc[id]; ok {
		return *e, nil
	}
	return SettlementException{}, nil
}

func (s *soExceptionStore) ListExceptions(_ context.Context, f ExceptionFilter) ([]SettlementException, error) {
	var out []SettlementException
	for _, e := range s.exc {
		if f.OpenOnly && !e.Actionable() {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

func (s *soExceptionStore) InstructionByID(_ context.Context, id int64) (ExceptionLeg, error) {
	if l, ok := s.legs[id]; ok {
		return *l, nil
	}
	return ExceptionLeg{}, fmt.Errorf("not found")
}

func (s *soExceptionStore) LockException(_ context.Context, id int64) (SettlementException, bool, error) {
	if e, ok := s.exc[id]; ok {
		return *e, true, nil
	}
	return SettlementException{}, false, nil
}

func (s *soExceptionStore) ResolveException(_ context.Context, id int64, st ExceptionStatus,
	a ResolutionAction, resolvedBy, approvedBy, dcID int64, notes string, journalID *int64, at time.Time) error {
	e := s.exc[id]
	if !e.Actionable() {
		return fmt.Errorf("conflict")
	}
	e.Status = st
	action := a
	e.Action = &action
	e.ResolvedBy = &resolvedBy
	e.ApprovedBy = &approvedBy
	e.ReversalJournalID = journalID
	e.ResolvedAt = &at
	return nil
}

func (s *soExceptionStore) AssignException(_ context.Context, id, assignee int64, st ExceptionStatus, _ time.Time) error {
	e := s.exc[id]
	e.Status = st
	e.AssignedTo = &assignee
	return nil
}

func (s *soExceptionStore) AppendEvent(_ context.Context, _ int64, _ *int64, action string, _ map[string]any) error {
	s.events = append(s.events, action)
	return nil
}

func (s *soExceptionStore) InstructionForUpdate(_ context.Context, id int64) (ExceptionLeg, bool, error) {
	if l, ok := s.legs[id]; ok {
		return *l, true, nil
	}
	return ExceptionLeg{}, false, nil
}

func (s *soExceptionStore) InstructionsForTradeForUpdate(_ context.Context, tradeID int64) ([]ExceptionLeg, error) {
	var out []ExceptionLeg
	for _, l := range s.legs {
		if l.TradeID == tradeID {
			out = append(out, *l)
		}
	}
	return out, nil
}

func (s *soExceptionStore) FailInstruction(_ context.Context, id int64) error {
	s.legs[id].Status = "FAILED"
	return nil
}

func (s *soExceptionStore) ResetInstructionForRetry(_ context.Context, id int64) error {
	l := s.legs[id]
	if l.Status != "FAILED" && l.Status != "PENDING" {
		return fmt.Errorf("not retryable")
	}
	l.Status = "PENDING"
	return nil
}

func (s *soExceptionStore) VoidInstruction(_ context.Context, id int64) error {
	s.legs[id].Status = "VOID"
	return nil
}

func (s *soExceptionStore) VoidPendingMovements(_ context.Context, instructionID int64) (int64, error) {
	if m, ok := s.movements[instructionID]; ok && m.status == "PENDING" {
		m.status = "VOID"
		return 1, nil
	}
	return 0, nil
}

func (s *soExceptionStore) CompensatePostedMovement(_ context.Context, instructionID int64) (bool, error) {
	if m, ok := s.movements[instructionID]; ok && m.status == "POSTED" {
		m.compensated = true
		return true, nil
	}
	return false, nil
}

func (s *soExceptionStore) AdjustBalance(_ context.Context, accountID int64, currency string, delta decimal.Decimal) error {
	key := fmt.Sprintf("%d:%s", accountID, currency)
	b, ok := s.balances[key]
	if !ok {
		b = &soBalance{}
		s.balances[key] = b
	}
	b.avail = b.avail.Add(delta)
	return nil
}

func (s *soExceptionStore) PostJournal(_ context.Context, entryType, description,
	postedBy, idemKey string, referenceID int64, lines []JournalLine) (int64, error) {
	s.journals = append(s.journals, soPostedJournal{
		entryType: entryType, description: description, idemKey: idemKey, lines: lines})
	return int64(len(s.journals)), nil
}

// ---------------------------------------------------------------------------

type soDualQueue struct{ submits []DualSubmit }

func (q *soDualQueue) Submit(_ context.Context, in DualSubmit) (*DualResult, error) {
	q.submits = append(q.submits, in)
	return &DualResult{ID: int64(len(q.submits)), Operation: in.Operation,
		RequiredRole: in.RequiredRole, Status: "PENDING", ExpiresAt: time.Now().Add(15 * time.Minute)}, nil
}

func soLeg(id, tradeID, acct int64, ccy, amt, dir, status string) *ExceptionLeg {
	return &ExceptionLeg{ID: id, TradeID: tradeID, AccountID: acct, Currency: ccy,
		Amount: decimal.RequireFromString(amt), Direction: dir, Status: status,
		SettlementDate: time.Now().UTC().Truncate(24 * time.Hour)}
}

// ---------------------------------------------------------------------------
// 24.3.6 — detection
// ---------------------------------------------------------------------------

func TestException_DetectFailureFlagsLegAndOpensException(t *testing.T) {
	st := newSoExceptionStore()
	st.legs[10] = soLeg(10, 55, 7, "USD", "1000", "PAY", "PENDING")
	svc := NewExceptionService(st, nil)
	instr := int64(10)
	ex, err := svc.DetectFailure(context.Background(), FailureDetection{
		InstructionID: &instr, Type: ExcInsufficientNostro,
		DetectedBy: "nostro-monitor", Detail: "balance short"})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if st.legs[10].Status != "FAILED" {
		t.Fatalf("leg not FAILED: %s", st.legs[10].Status)
	}
	if ex.Status != ExcStatusOpen || ex.Type != ExcInsufficientNostro {
		t.Fatalf("bad exception %+v", ex)
	}
	if len(st.events) != 1 || st.events[0] != "detect" {
		t.Fatalf("audit event missing: %v", st.events)
	}
	if ex.Amount.String() != "1000" || ex.Currency != "USD" {
		t.Fatalf("amount/currency not inherited: %+v", ex)
	}
}

func TestException_DetectOnSettledLegConflicts(t *testing.T) {
	st := newSoExceptionStore()
	st.legs[10] = soLeg(10, 55, 7, "USD", "1000", "PAY", "SETTLED")
	svc := NewExceptionService(st, nil)
	instr := int64(10)
	_, err := svc.DetectFailure(context.Background(), FailureDetection{
		InstructionID: &instr, Type: ExcSwiftRejection, DetectedBy: "swift"})
	if errCode(err) != CodeExceptionConflict {
		t.Fatalf("want %s, got %v", CodeExceptionConflict, err)
	}
}

func TestException_NilStoreFailsClosed(t *testing.T) {
	svc := NewExceptionService(nil, nil)
	ctx := context.Background()
	for _, err := range []error{
		func() error { _, e := svc.DetectFailure(ctx, FailureDetection{DetectedBy: "x"}); return e }(),
		func() error { _, e := svc.Investigate(ctx, 1, 2); return e }(),
		func() error { _, e := svc.Get(ctx, 1); return e }(),
		svc.ResolveDirect(ctx, 1, ActionManual, 1, 2, ""),
	} {
		if errCode(err) != CodeServiceDegraded {
			t.Fatalf("want SERVICE_DEGRADED, got %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Workflow + resolve
// ---------------------------------------------------------------------------

func TestException_InvestigateAssigns(t *testing.T) {
	st := newSoExceptionStore()
	ex, _ := NewExceptionService(st, nil).DetectFailure(context.Background(),
		FailureDetection{Type: ExcCounterparty, DetectedBy: "cp-watch"})
	svc := NewExceptionService(st, nil)
	got, err := svc.Investigate(context.Background(), ex.ID, 42)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if got.Status != ExcStatusInvestigating || *got.AssignedTo != 42 {
		t.Fatalf("bad investigate result %+v", got)
	}
}

func TestException_ResolveRetryReArmsLeg(t *testing.T) {
	st := newSoExceptionStore()
	st.legs[10] = soLeg(10, 55, 7, "USD", "500", "PAY", "FAILED")
	svc := NewExceptionService(st, nil)
	instr := int64(10)
	ex, _ := svc.DetectFailure(context.Background(),
		FailureDetection{InstructionID: &instr, Type: ExcSwiftRejection, DetectedBy: "swift"})
	if err := svc.ResolveDirect(context.Background(), ex.ID, ActionRetry, 7, 9, "reissue MT202"); err != nil {
		t.Fatalf("resolve retry: %v", err)
	}
	if st.legs[10].Status != "PENDING" {
		t.Fatalf("leg not re-armed: %s", st.legs[10].Status)
	}
	if st.exc[ex.ID].Status != ExcStatusResolvedRetry {
		t.Fatalf("exception not resolved: %s", st.exc[ex.ID].Status)
	}
}

func TestException_ResolveReversalReturnsFunds(t *testing.T) {
	st := newSoExceptionStore()
	st.legs[10] = soLeg(10, 55, 7, "USD", "500", "PAY", "FAILED")
	st.legs[11] = soLeg(11, 55, 7, "EUR", "450", "RECEIVE", "FAILED")
	st.movements[10] = &soMovement{status: "PENDING"}
	st.movements[11] = &soMovement{status: "POSTED"}
	st.balances["7:EUR"] = &soBalance{avail: decimal.RequireFromString("450")}
	svc := NewExceptionService(st, nil)
	instr := int64(10)
	ex, _ := svc.DetectFailure(context.Background(),
		FailureDetection{InstructionID: &instr, Type: ExcCounterparty, DetectedBy: "cp-watch"})
	if err := svc.ResolveDirect(context.Background(), ex.ID, ActionReverse, 7, 9, "cp insolvent"); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if st.legs[10].Status != "VOID" || st.legs[11].Status != "VOID" {
		t.Fatalf("legs not voided: %s %s", st.legs[10].Status, st.legs[11].Status)
	}
	if st.movements[10].status != "VOID" {
		t.Fatalf("pending movement not voided")
	}
	if !st.movements[11].compensated {
		t.Fatalf("posted movement not compensated")
	}
	// PAY leg: +500 USD back to the account; RECEIVE leg: -450 EUR clawback.
	if got := st.balances["7:USD"].avail.String(); got != "500" {
		t.Fatalf("USD avail want 500 got %s", got)
	}
	if got := st.balances["7:EUR"].avail.String(); got != "0" {
		t.Fatalf("EUR avail want 0 got %s", got)
	}
	// Balanced journal posted per currency.
	if len(st.journals) != 1 {
		t.Fatalf("want 1 journal got %d", len(st.journals))
	}
	var dr, cr decimal.Decimal
	for _, l := range st.journals[0].lines {
		dr = dr.Add(l.Debit)
		cr = cr.Add(l.Credit)
	}
	if !dr.Equal(cr) {
		t.Fatalf("journal unbalanced dr=%s cr=%s", dr, cr)
	}
	if st.exc[ex.ID].Status != ExcStatusResolvedReversed || st.exc[ex.ID].ReversalJournalID == nil {
		t.Fatalf("exception not RESOLVED_REVERSED with journal: %+v", st.exc[ex.ID])
	}
}

func TestException_ResolveTerminalConflicts(t *testing.T) {
	st := newSoExceptionStore()
	svc := NewExceptionService(st, nil)
	ex, _ := svc.DetectFailure(context.Background(),
		FailureDetection{Type: ExcOther, DetectedBy: "t"})
	if err := svc.ResolveDirect(context.Background(), ex.ID, ActionManual, 7, 9, "done"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if err := svc.ResolveDirect(context.Background(), ex.ID, ActionManual, 7, 9, "again"); errCode(err) != CodeExceptionConflict {
		t.Fatalf("want %s, got %v", CodeExceptionConflict, err)
	}
}

// ---------------------------------------------------------------------------
// Dual-control resolution path
// ---------------------------------------------------------------------------

func TestException_RequestResolutionRequiresDualQueue(t *testing.T) {
	st := newSoExceptionStore()
	svc := NewExceptionService(st, nil)
	ex, _ := svc.DetectFailure(context.Background(),
		FailureDetection{Type: ExcSwiftRejection, DetectedBy: "x"})
	_, err := svc.RequestResolution(context.Background(), ex.ID, ActionRetry, "n", 7, "ip")
	if errCode(err) != CodeServiceDegraded {
		t.Fatalf("want SERVICE_DEGRADED, got %v", err)
	}
}

func TestException_RequestResolutionSubmitsFinanceOpsDual(t *testing.T) {
	st := newSoExceptionStore()
	st.legs[10] = soLeg(10, 55, 7, "USD", "500", "PAY", "FAILED")
	dual := &soDualQueue{}
	svc := NewExceptionService(st, dual)
	instr := int64(10)
	ex, _ := svc.DetectFailure(context.Background(),
		FailureDetection{InstructionID: &instr, Type: ExcSwiftRejection, DetectedBy: "x"})
	res, err := svc.RequestResolution(context.Background(), ex.ID, ActionReverse, "x", 7, "10.0.0.1")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.RequiredRole != "Finance Ops" || res.Operation != OpSettlementExceptionResolve {
		t.Fatalf("bad dual submit %+v", res)
	}
	if len(dual.submits) != 1 {
		t.Fatalf("no submit recorded")
	}
}

func TestException_RequestResolutionRejectsTerminalAndWriteOff(t *testing.T) {
	st := newSoExceptionStore()
	svc := NewExceptionService(st, &soDualQueue{})
	ex, _ := svc.DetectFailure(context.Background(),
		FailureDetection{Type: ExcOther, DetectedBy: "x"})
	// WRITE_OFF rides the authority matrix — not this endpoint.
	if _, err := svc.RequestResolution(context.Background(), ex.ID, ActionWriteOff, "", 7, ""); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
	// RETRY without an instruction is invalid (external break).
	if _, err := svc.RequestResolution(context.Background(), ex.ID, ActionRetry, "", 7, ""); errCode(err) != "INVALID_REQUEST" {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
	// Resolve then re-request → 409.
	if err := svc.ResolveDirect(context.Background(), ex.ID, ActionManual, 7, 9, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := svc.RequestResolution(context.Background(), ex.ID, ActionManual, "", 7, ""); errCode(err) != CodeExceptionConflict {
		t.Fatalf("want %s, got %v", CodeExceptionConflict, err)
	}
}
