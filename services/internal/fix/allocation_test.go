// Unit tests for Task 18.3.13 — AllocationInstruction (35=J) parse,
// validation, correction transitions and propagation seams. No Postgres
// (allocationPersister is faked in-memory); PG-backed coverage lives in
// allocation_pg_test.go behind EXC_PG_TEST=1.
package fix

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/quickfixgo/quickfix"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// --- test doubles -----------------------------------------------------------

type fakeAllocStore struct {
	allocs   map[string]*Allocation
	nextID   int64
	events   []string
	failNext error
}

func newFakeAllocStore() *fakeAllocStore { return &fakeAllocStore{allocs: map[string]*Allocation{}} }

func (f *fakeAllocStore) SaveNew(_ context.Context, a *Allocation, legs []AllocationLeg) error {
	if f.failNext != nil {
		return f.failNext
	}
	f.nextID++
	a.ID = f.nextID
	a.Legs = legs
	for i := range a.Legs {
		a.Legs[i].ChildExecID = fmt.Sprintf("ALLOC-%d-%d", a.ID, a.Legs[i].LegNo)
	}
	cp := *a
	f.allocs[a.AllocID] = &cp
	f.events = append(f.events, "SAVE:"+string(a.Status))
	return nil
}

func (f *fakeAllocStore) Supersede(_ context.Context, ref string, repl *Allocation, legs []AllocationLeg) error {
	prev, ok := f.allocs[ref]
	if !ok {
		return allocErr(CodeAllocationInvalid, "allocation %q not found", ref)
	}
	if prev.SettlementLocked {
		return allocErr(CodeAllocationInvalid, "allocation %q claimed by settlement", ref)
	}
	if prev.Status != AllocStatusAccepted {
		return allocErr(CodeAllocationInvalid, "allocation %q in status %s is not amendable", ref, prev.Status)
	}
	prev.Status = AllocStatusReplaced
	f.events = append(f.events, "SUPERSEDE:"+ref)
	return f.SaveNew(context.Background(), repl, legs)
}

func (f *fakeAllocStore) Cancel(_ context.Context, allocID, _ string) (*Allocation, error) {
	a, ok := f.allocs[allocID]
	if !ok {
		return nil, allocErr(CodeAllocationInvalid, "allocation %q not found", allocID)
	}
	if a.SettlementLocked || a.Status != AllocStatusAccepted {
		return nil, allocErr(CodeAllocationInvalid, "allocation %q not amendable", allocID)
	}
	a.Status = AllocStatusCancelled
	for i := range a.Legs {
		a.Legs[i].Status = LegCancelled
	}
	f.events = append(f.events, "CANCEL:"+allocID)
	return a, nil
}

func (f *fakeAllocStore) AppendEvent(_ context.Context, _ int64, t string, _ map[string]any, _ string) error {
	f.events = append(f.events, "EVENT:"+t)
	return nil
}

type fakeExecResolver struct {
	execs  map[string]ExecRef
	orders map[string]ExecRef
}

func (f fakeExecResolver) Exec(_ context.Context, id string) (ExecRef, error) {
	if r, ok := f.execs[id]; ok {
		return r, nil
	}
	return ExecRef{}, fmt.Errorf("unknown exec %s", id)
}

func (f fakeExecResolver) OrderCum(_ context.Context, id string) (ExecRef, error) {
	if r, ok := f.orders[id]; ok {
		return r, nil
	}
	return ExecRef{}, fmt.Errorf("unknown order %s", id)
}

type fakeAccounts map[int64]map[string]int64 // masterID -> allocAccount -> acctID

func (f fakeAccounts) ResolveAllocAccount(_ context.Context, master int64, acct string) (int64, error) {
	if m, ok := f[master]; ok {
		if id, ok := m[acct]; ok {
			return id, nil
		}
	}
	return 0, fmt.Errorf("account %s not under master %d", acct, master)
}

type sinkRecorder struct{ evs []AllocationChange }

func (s *sinkRecorder) OnAllocationChange(_ context.Context, ev AllocationChange) error {
	s.evs = append(s.evs, ev)
	return nil
}

// --- message builders -------------------------------------------------------

func decV(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func newAllocMsg(allocID string, transType int, method string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgAllocationInstruction)
	m.Body.SetString(TagAllocID, allocID)
	m.Body.SetInt(TagAllocTransType, transType)
	if method != "" {
		m.Body.SetString(TagAllocType, method)
	}
	m.Body.SetString(TagSymbol, "EURUSD")
	m.Body.SetString(TagSide, "1")
	return m
}

func addExecGroup(m *quickfix.Message, execIDs ...string) {
	grp := quickfix.NewRepeatingGroup(TagNoExecs, quickfix.GroupTemplate{quickfix.GroupElement(TagExecID)})
	for _, id := range execIDs {
		g := grp.Add()
		g.SetString(TagExecID, id)
	}
	m.Body.SetGroup(grp)
}

func addAllocLegs(m *quickfix.Message, legs ...[2]string) {
	grp := quickfix.NewRepeatingGroup(TagNoAllocs, quickfix.GroupTemplate{
		quickfix.GroupElement(TagAllocAccount),
		quickfix.GroupElement(TagAllocQty),
		quickfix.GroupElement(TagAllocPrice),
	})
	for _, l := range legs {
		g := grp.Add()
		g.SetString(TagAllocAccount, l[0])
		g.SetString(TagAllocQty, l[1])
	}
	m.Body.SetGroup(grp)
}

func decEq(t *testing.T, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(decV(want)) {
		t.Fatalf("decimal %s != %s", got, want)
	}
}

func testSvc(store *fakeAllocStore) (*AllocationService, *sinkRecorder) {
	rec := &sinkRecorder{}
	exec := fakeExecResolver{
		execs:  map[string]ExecRef{"E1": {Qty: decV("100"), AvgPx: decV("1.10"), Symbol: "EURUSD", Side: '1', AccountID: 900}},
		orders: map[string]ExecRef{},
	}
	accts := fakeAccounts{900: {"SUB1": 901, "SUB2": 902, "SUB3": 903}}
	svc := NewAllocationService(store, exec, accts)
	svc.AddSink(rec)
	return svc, rec
}

// --- parse tests ------------------------------------------------------------

func TestParseAllocationInstruction(t *testing.T) {
	m := newAllocMsg("A1", 0, "MANUAL")
	m.Body.SetString(TagQuantity, "100")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"SUB1", "60"}, [2]string{"SUB2", "40"})
	instr, err := ParseAllocationInstruction(m)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instr.AllocID != "A1" || instr.TransType != AllocTransNew || instr.Method != AllocMethodManual {
		t.Fatalf("header mismatch: %+v", instr)
	}
	if len(instr.ExecRefs) != 1 || instr.ExecRefs[0] != "E1" {
		t.Fatalf("exec refs: %v", instr.ExecRefs)
	}
	if len(instr.Legs) != 2 || instr.Legs[0].AllocAccount != "SUB1" {
		t.Fatalf("legs: %+v", instr.Legs)
	}
	decEq(t, instr.Legs[1].AllocQty, "40")
	decEq(t, instr.DeclaredQty, "100")
}

func TestParseAllocationInstruction_MissingAllocID(t *testing.T) {
	m := quickfix.NewMessage()
	m.Body.SetInt(TagAllocTransType, 0)
	if _, err := ParseAllocationInstruction(m); err == nil {
		t.Fatal("expected missing AllocID error")
	}
}

func TestParseAllocationInstruction_NoLegs(t *testing.T) {
	m := newAllocMsg("A1", 0, "")
	if _, err := ParseAllocationInstruction(m); err == nil {
		t.Fatal("expected no-legs error")
	}
}

// --- NEW / validation --------------------------------------------------------

func TestAllocationNew_ManualAccepted(t *testing.T) {
	store := newFakeAllocStore()
	svc, rec := testSvc(store)
	m := newAllocMsg("A1", 0, "MANUAL")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"SUB1", "60"}, [2]string{"SUB2", "40"})
	out, err := svc.Handle(context.Background(), "FIX.4.4:EX->MM", 900, m)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.Ack != nil || out.Report == nil {
		t.Fatal("expected 35=AK report, no ack")
	}
	if mt, _ := out.Report.Header.GetString(TagMsgType); mt != MsgAllocationReport {
		t.Fatalf("report msgtype %s", mt)
	}
	a := store.allocs["A1"]
	if a.Status != AllocStatusAccepted || len(a.Legs) != 2 {
		t.Fatalf("stored: %+v", a)
	}
	if a.Legs[0].AccountID != 901 || a.Legs[1].AccountID != 902 {
		t.Fatalf("leg accounts: %+v", a.Legs)
	}
	if a.Legs[0].ChildExecID == "" {
		t.Fatal("child exec id not stamped")
	}
	if len(rec.evs) != 1 || rec.evs[0].Type != "COMMITTED" {
		t.Fatalf("sink events: %+v", rec.evs)
	}
}

func TestAllocationNew_OverAllocationRejected(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	m := newAllocMsg("A2", 0, "MANUAL")
	addExecGroup(m, "E1") // exec qty 100
	addAllocLegs(m, [2]string{"SUB1", "60"}, [2]string{"SUB2", "41"})
	out, err := svc.Handle(context.Background(), "S", 900, m)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.Report != nil || out.Ack == nil {
		t.Fatal("expected 35=P reject ack")
	}
	if code, _ := out.Ack.Body.GetInt(TagAllocRejCode); code != AllocRejCodeSumMismatch {
		t.Fatalf("AllocRejCode %d, want 4", code)
	}
	if txt, _ := out.Ack.Body.GetString(TagText); txt != CodeAllocationSumMismatch {
		t.Fatalf("Text %q", txt)
	}
	if a := store.allocs["A2"]; a == nil || a.Status != AllocStatusRejected {
		t.Fatalf("rejected instruction must be persisted for audit: %+v", a)
	}
}

func TestAllocationNew_UnderAllocationRejected(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	m := newAllocMsg("A3", 0, "MANUAL")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"SUB1", "60"}, [2]string{"SUB2", "30"})
	out, _ := svc.Handle(context.Background(), "S", 900, m)
	if code, _ := out.Ack.Body.GetInt(TagAllocRejCode); code != AllocRejCodeSumMismatch {
		t.Fatalf("under-allocation must reject code 4, got %d", code)
	}
}

func TestAllocationNew_DeclaredQtyMismatch(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	m := newAllocMsg("A4", 0, "MANUAL")
	m.Body.SetString(TagQuantity, "99")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"SUB1", "99"})
	out, _ := svc.Handle(context.Background(), "S", 900, m)
	if txt, _ := out.Ack.Body.GetString(TagText); txt != CodeAllocationSumMismatch {
		t.Fatalf("declared-qty mismatch must be ALLOCATION_SUM_MISMATCH, got %q", txt)
	}
}

func TestAllocationNew_UnknownAccount(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	m := newAllocMsg("A5", 0, "MANUAL")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"OTHER_ACCT", "100"})
	out, _ := svc.Handle(context.Background(), "S", 900, m)
	if txt, _ := out.Ack.Body.GetString(TagText); txt != CodeAllocationInvalid {
		t.Fatalf("want ALLOCATION_INVALID, got %q", txt)
	}
	if st, _ := out.Ack.Body.GetInt(TagAllocStatus); st != AllocStatusAccountLevelReject {
		t.Fatalf("AllocStatus %d, want 2", st)
	}
}

func TestAllocationNew_UnknownExecRef(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	m := newAllocMsg("A6", 0, "MANUAL")
	addExecGroup(m, "NOPE")
	addAllocLegs(m, [2]string{"SUB1", "100"})
	out, _ := svc.Handle(context.Background(), "S", 900, m)
	if txt, _ := out.Ack.Body.GetString(TagText); txt != CodeAllocationInvalid {
		t.Fatalf("want ALLOCATION_INVALID, got %q", txt)
	}
}

func TestAllocationNew_NoRefs(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	m := newAllocMsg("A7", 0, "MANUAL")
	addAllocLegs(m, [2]string{"SUB1", "100"})
	out, _ := svc.Handle(context.Background(), "S", 900, m)
	if txt, _ := out.Ack.Body.GetString(TagText); txt != CodeAllocationInvalid {
		t.Fatalf("want ALLOCATION_INVALID, got %q", txt)
	}
}

// --- PRO_RATA / STEP_OUT -----------------------------------------------------

func TestAllocationProRata_ExactConservation(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	m := newAllocMsg("A8", 0, "PRO_RATA")
	addExecGroup(m, "E1") // 100 across three equal weights
	addAllocLegs(m, [2]string{"SUB1", "1"}, [2]string{"SUB2", "1"}, [2]string{"SUB3", "1"})
	out, err := svc.Handle(context.Background(), "S", 900, m)
	if err != nil || out.Report == nil {
		t.Fatalf("handle: %v", err)
	}
	a := store.allocs["A8"]
	var sum decimal.Decimal
	for _, l := range a.Legs {
		sum = sum.Add(l.AllocQty)
	}
	decEq(t, sum, "100") // quantum conservation, residuals distributed
}

func TestAllocationStepOut(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	m := newAllocMsg("A9", 0, "STEP_OUT")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"EXTBROKER1", "70"}, [2]string{"EXTBROKER2", "30"})
	out, err := svc.Handle(context.Background(), "S", 900, m)
	if err != nil || out.Report == nil {
		t.Fatalf("handle: %v", err)
	}
	a := store.allocs["A9"]
	for _, l := range a.Legs {
		if l.Status != LegExternalBooked || l.StepOutBroker == "" || l.AccountID != 0 {
			t.Fatalf("step-out leg not external-booked: %+v", l)
		}
	}
}

// --- REPLACE / CANCEL --------------------------------------------------------

func seedAccepted(t *testing.T, svc *AllocationService, allocID string) {
	t.Helper()
	m := newAllocMsg(allocID, 0, "MANUAL")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"SUB1", "60"}, [2]string{"SUB2", "40"})
	out, err := svc.Handle(context.Background(), "S", 900, m)
	if err != nil || out.Report == nil {
		t.Fatalf("seed %s: %v", allocID, err)
	}
}

func TestAllocationReplace(t *testing.T) {
	store := newFakeAllocStore()
	svc, rec := testSvc(store)
	seedAccepted(t, svc, "A10")
	m := newAllocMsg("A10B", 1, "MANUAL")
	m.Body.SetString(TagRefAllocID, "A10")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"SUB1", "50"}, [2]string{"SUB2", "50"})
	out, err := svc.Handle(context.Background(), "S", 900, m)
	if err != nil || out.Report == nil {
		t.Fatalf("replace: %v", err)
	}
	if store.allocs["A10"].Status != AllocStatusReplaced {
		t.Fatalf("prev status %s", store.allocs["A10"].Status)
	}
	if store.allocs["A10B"].Status != AllocStatusAccepted {
		t.Fatal("replacement not accepted")
	}
	if len(rec.evs) != 2 || rec.evs[1].Type != "REPLACED" || rec.evs[1].PrevAllocID != "A10" {
		t.Fatalf("propagation: %+v", rec.evs)
	}
}

func TestAllocationCancel(t *testing.T) {
	store := newFakeAllocStore()
	svc, rec := testSvc(store)
	seedAccepted(t, svc, "A11")
	m := newAllocMsg("A11", 2, "")
	m.Body.SetString(TagRefAllocID, "A11")
	out, err := svc.Handle(context.Background(), "S", 900, m)
	if err != nil || out.Report == nil {
		t.Fatalf("cancel: %v", err)
	}
	if store.allocs["A11"].Status != AllocStatusCancelled {
		t.Fatalf("status %s", store.allocs["A11"].Status)
	}
	for _, l := range store.allocs["A11"].Legs {
		if l.Status != LegCancelled {
			t.Fatalf("leg not cancelled: %+v", l)
		}
	}
	if rec.evs[len(rec.evs)-1].Type != "CANCELLED" {
		t.Fatalf("last event: %+v", rec.evs[len(rec.evs)-1])
	}
}

func TestAllocationReplaceNotAccepted(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	seedAccepted(t, svc, "A12")
	// cancel it first → REPLACE on CANCELLED must reject
	cm := newAllocMsg("A12", 2, "")
	cm.Body.SetString(TagRefAllocID, "A12")
	if _, err := svc.Handle(context.Background(), "S", 900, cm); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rm := newAllocMsg("A12B", 1, "MANUAL")
	rm.Body.SetString(TagRefAllocID, "A12")
	addExecGroup(rm, "E1")
	addAllocLegs(rm, [2]string{"SUB1", "100"})
	out, err := svc.Handle(context.Background(), "S", 900, rm)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.Ack == nil {
		t.Fatal("REPLACE on CANCELLED must reject")
	}
	if txt, _ := out.Ack.Body.GetString(TagText); txt != CodeAllocationInvalid {
		t.Fatalf("want ALLOCATION_INVALID, got %q", txt)
	}
}

func TestAllocationReplaceMissingRef(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	m := newAllocMsg("A13", 1, "MANUAL")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"SUB1", "100"})
	out, _ := svc.Handle(context.Background(), "S", 900, m)
	if out.Ack == nil {
		t.Fatal("REPLACE without RefAllocID must reject")
	}
}

func TestAllocationCancelLocked(t *testing.T) {
	store := newFakeAllocStore()
	svc, _ := testSvc(store)
	seedAccepted(t, svc, "A14")
	store.allocs["A14"].SettlementLocked = true
	cm := newAllocMsg("A14", 2, "")
	cm.Body.SetString(TagRefAllocID, "A14")
	out, err := svc.Handle(context.Background(), "S", 900, cm)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.Ack == nil {
		t.Fatal("settlement-locked cancel must reject")
	}
}

func TestAllocationValidationErrorsAreCoded(t *testing.T) {
	var ce *excerrors.Error
	err := allocErr(CodeAllocationSumMismatch, "x")
	if !errors.As(err, &ce) || ce.Code != CodeAllocationSumMismatch {
		t.Fatal("allocErr must produce coded errors")
	}
}
