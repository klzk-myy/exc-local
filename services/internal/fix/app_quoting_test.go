// app_quoting_test.go — wire-half coverage for Task 18.3.7's session
// framing + Task 11.3.12's SCOPE_LP gate through ToApp (spec §9.4).
package fix

import (
	"strings"
	"testing"

	"exchange/internal/marketmaking"
	"exchange/internal/orders"

	"github.com/quickfixgo/quickfix"
)

// massQuoteMsg builds one 35=i carrying entries of {QuoteEntryID, Symbol,
// BidPx, BidSize, OfferPx, OfferSize}.
func massQuoteMsg(setID string, entries [][6]string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgMassQuote))
	m.Body.SetString(TagQuoteSetID, setID)
	grp := quickfix.NewRepeatingGroup(TagNoQuoteEntries, quickfix.GroupTemplate{
		quickfix.GroupElement(TagQuoteEntryID),
		quickfix.GroupElement(TagSymbol),
		quickfix.GroupElement(TagBidPx),
		quickfix.GroupElement(TagBidSize),
		quickfix.GroupElement(TagOfferPx),
		quickfix.GroupElement(TagOfferSize),
	})
	for _, e := range entries {
		ge := grp.Add()
		ge.SetString(TagQuoteEntryID, e[0])
		ge.SetString(TagSymbol, e[1])
		if e[2] != "" {
			ge.SetString(TagBidPx, e[2])
		}
		if e[3] != "" {
			ge.SetString(TagBidSize, e[3])
		}
		if e[4] != "" {
			ge.SetString(TagOfferPx, e[4])
		}
		if e[5] != "" {
			ge.SetString(TagOfferSize, e[5])
		}
	}
	m.Body.SetGroup(grp)
	return m
}

func quoteCancelMsg(setID string, cancelType int, symbol string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgQuoteCancel))
	if setID != "" {
		m.Body.SetString(TagQuoteSetID, setID)
	}
	m.Body.SetInt(TagQuoteCancelType, cancelType)
	if symbol != "" {
		m.Body.SetString(TagSymbol, symbol)
	}
	return m
}

// quoteSvcFor binds the service stack over the quoting_test fakes: pipe
// with account 11 (the app session's bound account) and the EURUSD
// instrument entitlement.
func quoteSvcFor(t *testing.T, pipe *fakePipeline) (*QuoteService, *fakeEntitlement) {
	t.Helper()
	pipe.acct = &orders.Account{ID: 11}
	ent := &fakeEntitlement{programs: map[string]*marketmaking.Program{
		"11|42": activeProgram(42),
	}}
	svc, err := NewQuoteService(pipe, ent, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return svc, ent
}

func ackEntryStatuses(t *testing.T, m *quickfix.Message) map[string]int {
	t.Helper()
	out := map[string]int{}
	if !m.Body.Has(TagNoQuoteEntries) {
		return out
	}
	grp := quickfix.NewRepeatingGroup(TagNoQuoteEntries, quickfix.GroupTemplate{
		quickfix.GroupElement(TagQuoteEntryID),
		quickfix.GroupElement(TagQuoteAckStatus),
		quickfix.GroupElement(TagQuoteRejectReason),
		quickfix.GroupElement(TagSymbol),
		quickfix.GroupElement(TagText),
	})
	if err := m.Body.GetGroup(grp); err != nil {
		t.Fatalf("ack group: %v", err)
	}
	for i := 0; i < grp.Len(); i++ {
		ge := grp.Get(i)
		id, _ := ge.GetString(TagQuoteEntryID)
		st, _ := ge.GetInt(TagQuoteAckStatus)
		out[id] = st
	}
	return out
}

func TestFromApp_MassQuoteRoutedAndAcked(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession() // EUR/USD only
	flow := &memFlow{acct: nil}
	pipe := newFakePipeline()
	qs, _ := quoteSvcFor(t, pipe)
	app, captured := newTestApp(st, flow, nil)
	app.opt.QS = qs

	app.FromApp(massQuoteMsg("set-1", [][6]string{
		{"e1", "EUR/USD", "1.1000", "1000", "1.1002", "2000"}, // entitled
		{"e2", "GBP/USD", "1.2500", "500", "1.2502", "500"},   // out of scope
	}), testSessionID())

	if len(*captured) != 1 {
		t.Fatalf("emits: %d", len(*captured))
	}
	m := (*captured)[0].Msg
	if mt, _ := m.MsgType(); mt != MsgMassQuoteAck {
		t.Fatalf("want 35=b, got %v", mt)
	}
	sts := ackEntryStatuses(t, m)
	if sts["e1"] != QuoteStatusAccepted {
		t.Fatalf("e1 should accept: %+v (text %q)", sts, bodyText(t, m))
	}
	if sts["e2"] != QuoteStatusRejected {
		t.Fatalf("e2 must reject out-of-scope: %+v", sts)
	}
	if got, _ := m.Body.GetInt(TagQuoteAckStatus); got != QuoteStatusRejected {
		t.Fatalf("set-level status must be 5 on partial reject, got %d", got)
	}
	// EUR/USD two-sided → two quote-sourced submits.
	if len(pipe.orders) != 2 {
		t.Fatalf("submits: %d", len(pipe.orders))
	}
}

func TestFromApp_MassQuoteNilQSUnsupported(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	flow := &memFlow{}
	app, captured := newTestApp(st, flow, nil)

	app.FromApp(massQuoteMsg("s", [][6]string{{"e", "EUR/USD", "1", "1", "", ""}}),
		testSessionID())
	if len(*captured) != 1 {
		t.Fatalf("emits: %d", len(*captured))
	}
	if mt, _ := (*captured)[0].Msg.MsgType(); mt != MsgBusinessReject {
		t.Fatalf("want 35=j, got %v", mt)
	}
	if !strings.Contains(bodyText(t, (*captured)[0].Msg), "MSGTYPE_UNSUPPORTED") {
		t.Fatalf("text: %q", bodyText(t, (*captured)[0].Msg))
	}
}

func TestFromApp_MassQuoteDropCopyRejected(t *testing.T) {
	st := newMemStore()
	row := entitledSession()
	row.AccountID = nil // drop-copy session
	st.sessions[testSessionID().String()] = row
	flow := &memFlow{}
	pipe := newFakePipeline()
	qs, _ := quoteSvcFor(t, pipe)
	app, captured := newTestApp(st, flow, nil)
	app.opt.QS = qs

	app.FromApp(massQuoteMsg("s", [][6]string{{"e", "EUR/USD", "1", "1", "", ""}}),
		testSessionID())
	if len(*captured) != 1 {
		t.Fatalf("emits: %d", len(*captured))
	}
	if mt, _ := (*captured)[0].Msg.MsgType(); mt != MsgBusinessReject {
		t.Fatalf("want 35=j, got %v", mt)
	}
	if len(pipe.orders) != 0 {
		t.Fatal("drop-copy session must not reach the order pipeline")
	}
}

func TestFromApp_MassQuoteLPSuspendedHaltsQuotesCLOBContinues(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	flow := &memFlow{acct: &orders.Account{ID: 11}}
	pipe := newFakePipeline()
	qs, ent := quoteSvcFor(t, pipe)
	_ = ent
	qs.WithLPGate(&fakeLPResolver{lp: map[int64]int64{11: 5}},
		&fakeLPGuard{suspended: map[string]string{"5": "halted by risk"}})
	app, captured := newTestApp(st, flow, nil)
	app.opt.QS = qs

	app.FromApp(massQuoteMsg("s1", [][6]string{
		{"e1", "EUR/USD", "1.1", "10", "1.1002", "10"},
		{"e2", "EUR/USD", "1.1001", "10", "1.1003", "10"},
	}), testSessionID())

	if len(*captured) != 1 {
		t.Fatalf("emits: %d", len(*captured))
	}
	sts := ackEntryStatuses(t, (*captured)[0].Msg)
	if sts["e1"] != QuoteStatusRejected || sts["e2"] != QuoteStatusRejected {
		t.Fatalf("suspended LP set must reject every entry: %+v", sts)
	}
	if len(pipe.orders) != 0 {
		t.Fatal("suspended LP must post no quote legs")
	}
	// Firm CLOB continues: the same session's 35=D still flows through
	// the canonical pipeline (LP scope lives outside the order lattice).
	if len(*captured) != 1 {
		t.Fatalf("emits after quote: %d", len(*captured))
	}
	app.FromApp(newOrderMsg(), testSessionID())
	if len(flow.submitted) != 1 {
		t.Fatal("CLOB order entry must continue under SCOPE_LP")
	}
}

func TestFromApp_QuoteCancelCancelsSet(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	flow := &memFlow{}
	pipe := newFakePipeline()
	qs, _ := quoteSvcFor(t, pipe)
	app, captured := newTestApp(st, flow, nil)
	app.opt.QS = qs

	app.FromApp(massQuoteMsg("set-9", [][6]string{
		{"e1", "EUR/USD", "1.1", "10", "1.1002", "10"},
	}), testSessionID())
	app.FromApp(quoteCancelMsg("set-9", QuoteCancelAllQuotes, ""), testSessionID())

	if len(*captured) != 2 {
		t.Fatalf("emits: %d", len(*captured))
	}
	m := (*captured)[1].Msg
	if mt, _ := m.MsgType(); mt != MsgMassQuoteAck {
		t.Fatalf("want 35=b cancel ack, got %v", mt)
	}
	if got, _ := m.Body.GetInt(TagQuoteAckStatus); got != QuoteStatusAccepted {
		t.Fatalf("cancel ack status: %d (text %q)", got, bodyText(t, m))
	}
	if len(pipe.cancelled) == 0 {
		t.Fatal("cancel must reach the order pipeline")
	}
}

