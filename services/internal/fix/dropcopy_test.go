// Unit tests for Task 18.3.4 — drop-copy session classification, read-only
// rejection, bound-account fan-out, Parties stamping (spec §9.2).
package fix

import (
	"context"
	"strings"
	"testing"

	"github.com/quickfixgo/quickfix"
)

func TestDropCopySessionKind(t *testing.T) {
	trading := &Session{AccountID: i64(42)}
	dc := &Session{AccountID: nil}
	if DropCopySessionKind(trading) != KindTrading {
		t.Fatal("bound-account session must be trading")
	}
	if DropCopySessionKind(dc) != KindDropCopy {
		t.Fatal("nil account_id must classify drop copy")
	}
	if DropCopySessionKind(nil) != KindTrading {
		t.Fatal("nil session must not classify as drop copy")
	}
}

func TestOrderEntryRejector(t *testing.T) {
	rej := NewOrderEntryRejector()
	if m := rej.Reject(&Session{AccountID: i64(1)}, MsgNewOrderSingle, "X1"); m != nil {
		t.Fatal("trading session must not be rejected")
	}
	m := rej.Reject(&Session{}, MsgNewOrderSingle, "X1")
	if m == nil {
		t.Fatal("drop-copy order entry must be rejected")
	}
	if mt, _ := m.Header.GetString(TagMsgType); mt != MsgBusinessReject {
		t.Fatalf("want 35=j business reject, got %s", mt)
	}
	txt, _ := m.Body.GetString(TagText)
	if !strings.Contains(txt, "drop copy is read-only") {
		t.Fatalf("Text must carry read-only reason, got %q", txt)
	}
}

func TestIsOrderEntry(t *testing.T) {
	for _, mt := range []string{MsgNewOrderSingle, MsgOrderCancelRequest,
		MsgOrderCancelReplace, MsgAllocationInstruction} {
		if !IsOrderEntry(mt) {
			t.Fatalf("%s must be order-entry", mt)
		}
	}
	if IsOrderEntry(MsgHeartbeat) || IsOrderEntry(MsgTestRequest) {
		t.Fatal("admin msgtypes are not order entry")
	}
}

// dcSender records outbound copies per session.
type dcSender struct{ msgs []*quickfix.Message }

func (c *dcSender) SendTo(m *quickfix.Message, _ quickfix.SessionID) error {
	c.msgs = append(c.msgs, m)
	return nil
}

func execReport(orderID int64) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgExecutionReport)
	m.Body.SetString(TagExecID, "EX1")
	m.Body.SetString(TagClOrdID, "C1")
	m.Body.SetString(TagSide, "1")
	return m
}

func TestDropCopyRouter_FanOutBoundAccounts(t *testing.T) {
	r := NewDropCopyRouter()
	bound := &dcSender{}
	unbound := &dcSender{}
	dst := quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "EX", TargetCompID: "DC1"}
	if err := r.Bind(DropCopyTarget{ID: "dc1", SessionID: dst, Sender: bound,
		Accounts: map[int64]bool{900: true}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := r.Bind(DropCopyTarget{ID: "dc2", Sender: unbound,
		Accounts: map[int64]bool{555: true}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := r.Bind(DropCopyTarget{ID: "nope", Sender: bound}); err != ErrDropCopyNotBound {
		t.Fatalf("unbound target must fail, got %v", err)
	}
	r.OrderAccount = func(_ context.Context, orderID int64) (int64, error) { return 900, nil }

	ev := ReportEvent{SessionID: dst, OrderID: 7, Msg: execReport(7)}
	if err := r.OnReport(context.Background(), ev); err != nil {
		t.Fatalf("onreport: %v", err)
	}
	if len(bound.msgs) != 1 {
		t.Fatalf("bound target must receive 1 copy, got %d", len(bound.msgs))
	}
	if len(unbound.msgs) != 0 {
		t.Fatalf("out-of-scope account leaked %d copies", len(unbound.msgs))
	}
	// The copy is a distinct instance — mutating it can't touch the origin.
	if bound.msgs[0] == ev.Msg {
		t.Fatal("copy must be a cloned message, not the shared instance")
	}
}

func TestDropCopyRouter_UnresolvableAccountFailsClosed(t *testing.T) {
	r := NewDropCopyRouter()
	s := &dcSender{}
	_ = r.Bind(DropCopyTarget{ID: "dc", Sender: s, Accounts: map[int64]bool{1: true}})
	// No OrderAccount resolver and no Tag 1 → fail closed.
	err := r.OnReport(context.Background(), ReportEvent{Msg: execReport(0)})
	if err == nil {
		t.Fatal("unresolvable account must surface an error")
	}
	if len(s.msgs) != 0 {
		t.Fatal("unscoped report must not be copied")
	}
}

func TestDropCopyRouter_Tag1Fallback(t *testing.T) {
	r := NewDropCopyRouter()
	s := &dcSender{}
	_ = r.Bind(DropCopyTarget{ID: "dc", Sender: s, Accounts: map[int64]bool{42: true}})
	m := execReport(9)
	m.Body.SetString(TagAccount, "42")
	if err := r.OnReport(context.Background(), ReportEvent{Msg: m}); err != nil {
		t.Fatalf("onreport: %v", err)
	}
	if len(s.msgs) != 1 {
		t.Fatal("Tag 1 fallback should scope the copy")
	}
}

func TestCloneMessage_Independence(t *testing.T) {
	m := execReport(1)
	m.Body.SetString(TagText, "orig")
	cp := CloneMessage(m)
	if cp == m {
		t.Fatal("clone must be a different instance")
	}
	cp.Body.SetString(TagText, "copy")
	if v, _ := m.Body.GetString(TagText); v != "orig" {
		t.Fatal("mutating clone must not touch the original")
	}
}

func TestSetParties_Roundtrip(t *testing.T) {
	m := execReport(1)
	SetParties(m, []Party{
		{ID: "EXSESS", Source: PartyIDSourceProprietary, Role: PartyRoleExecutingFirm},
		{ID: "BICPB1XX", Source: PartyIDSourceBIC, Role: PartyRolePrimeBroker},
	})
	grp := quickfix.NewRepeatingGroup(TagNoPartyIDs, quickfix.GroupTemplate{
		quickfix.GroupElement(TagPartyID),
		quickfix.GroupElement(TagPartyIDSource),
		quickfix.GroupElement(TagPartyRole),
	})
	if err := m.Body.GetGroup(grp); err != nil {
		t.Fatalf("read parties: %v", err)
	}
	if grp.Len() != 2 {
		t.Fatalf("want 2 parties, got %d", grp.Len())
	}
	pid, _ := grp.Get(1).GetString(TagPartyID)
	role, _ := grp.Get(1).GetString(TagPartyRole)
	if pid != "BICPB1XX" || role != PartyRolePrimeBroker {
		t.Fatalf("party fields: %s/%s", pid, role)
	}
}

func TestSetFXParties_Roundtrip(t *testing.T) {
	m := execReport(1)
	SetFXParties(m, []Party{{ID: "PB1", Source: "BIC"}})
	SetFXSettlement(m, "T+1", "2026-10-20")
	grp := quickfix.NewRepeatingGroup(TagFXNoPartyIDs, quickfix.GroupTemplate{
		quickfix.GroupElement(TagFXPartyID),
		quickfix.GroupElement(TagFXPartyIDSource),
	})
	if err := m.Body.GetGroup(grp); err != nil || grp.Len() != 1 {
		t.Fatalf("fx parties: %v len=%d", err, grp.Len())
	}
	if v, _ := m.Body.GetString(TagFXSettlementType); v != "T+1" {
		t.Fatalf("9501 = %q", v)
	}
}

func TestDropCopyRouter_AllocationReportFanOut(t *testing.T) {
	r := NewDropCopyRouter()
	s := &dcSender{}
	_ = r.Bind(DropCopyTarget{ID: "dc", Sender: s, Accounts: map[int64]bool{900: true}})
	rep := BuildAllocationReport(Allocation{ID: 9, AllocID: "A1", MasterAccountID: 900,
		Status: AllocStatusAccepted, Symbol: "EURUSD", Side: '1', ExecQty: decV("10")}, nil)
	ev := AllocationChange{Type: "COMMITTED", Report: rep,
		Allocation: Allocation{MasterAccountID: 900}}
	if err := r.OnAllocationChange(context.Background(), ev); err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if len(s.msgs) != 1 {
		t.Fatal("bound tap must receive the 35=AK copy")
	}
}
