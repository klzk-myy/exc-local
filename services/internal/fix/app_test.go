// Application-level tests: logon authentication (unknown pair / bad
// credential / bound-account mismatch), entitlement rejects,
// SESSION_THROTTLED, orderly-vs-abnormal disconnect semantics and the
// 35=BE dead-man command. Emitted wire messages are captured through
// ReportBus taps — SendToTarget returns errUnknownSession in-process
// but taps still observe every frame, which is exactly what the
// assertions need.
package fix

import (
	"context"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/accounts"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
)

// ---- fakes ------------------------------------------------------------------

type memStore struct {
	sessions map[string]*Session
	msgs     map[string]map[int64][]byte
	status   map[string]string
	verify   func(keyID int64, keyName, secret string) (int64, bool, error)
}

func newMemStore() *memStore {
	return &memStore{sessions: map[string]*Session{},
		msgs: map[string]map[int64][]byte{}, status: map[string]string{}}
}

func (m *memStore) SessionByID(_ context.Context, id string) (*Session, error) {
	return m.sessions[id], nil
}
func (m *memStore) CreateSession(_ context.Context, s *Session) (*Session, error) {
	cp := *s
	m.sessions[cp.SessionID] = &cp
	return &cp, nil
}
func (m *memStore) SetStatus(_ context.Context, id, st string, _ *time.Time) error {
	m.status[id] = st
	return nil
}
func (m *memStore) SetSeq(_ context.Context, id string, s, t int64) error {
	if row := m.sessions[id]; row != nil {
		row.SenderSeqNum, row.TargetSeqNum = s, t
	}
	return nil
}
func (m *memStore) SeqState(_ context.Context, id string) (int64, int64, bool, error) {
	if row := m.sessions[id]; row != nil {
		return row.SenderSeqNum, row.TargetSeqNum, true, nil
	}
	return 0, 0, false, nil
}
func (m *memStore) RefreshSession(context.Context, string) error { return nil }
func (m *memStore) SaveMessage(_ context.Context, id string, seq int64, msg []byte) error {
	if m.msgs[id] == nil {
		m.msgs[id] = map[int64][]byte{}
	}
	m.msgs[id][seq] = msg
	return nil
}
func (m *memStore) Messages(_ context.Context, id string, b, e int64) ([][]byte, error) {
	var out [][]byte
	for seq := b; seq <= e; seq++ {
		if m.msgs[id][seq] != nil {
			out = append(out, m.msgs[id][seq])
		}
	}
	return out, nil
}
func (m *memStore) PurgeMessages(_ context.Context, id string) error {
	delete(m.msgs, id)
	return nil
}
func (m *memStore) VerifyAPIKey(ctx context.Context, keyID int64, keyName, secret string) (int64, bool, error) {
	return m.verify(keyID, keyName, secret)
}
func (m *memStore) UpdateEntitlement(_ context.Context, id string, u EntitlementUpdate) (*Session, error) {
	row := m.sessions[id]
	if row == nil {
		return nil, nil
	}
	if u.ClearAccount {
		row.AccountID = nil
	} else if u.AccountID != nil {
		row.AccountID = u.AccountID
	}
	if u.ClearAPIKey {
		row.APIKeyID = nil
	} else if u.APIKeyID != nil {
		row.APIKeyID = u.APIKeyID
	}
	if u.SetAllInstruments {
		row.AllowedInstruments = ""
	} else if u.AllowedInstruments != nil {
		row.AllowedInstruments = *u.AllowedInstruments
	}
	if u.CancelOnDisconnect != nil {
		row.CancelOnDisconnect = *u.CancelOnDisconnect
	}
	if u.MaxMsgsPerSec != nil {
		row.MaxMsgsPerSec = *u.MaxMsgsPerSec
	}
	return row, nil
}

type memFlow struct {
	acct      *orders.Account
	submitted []*orders.SubmitRequest
	cancelled []int64
	replaced  []int64
	codCalls  []struct {
		acct int64
		sess string
	}
	submitErr error
}

func (f *memFlow) AccountByID(context.Context, int64) (*orders.Account, error) {
	return f.acct, nil
}
func (f *memFlow) Submit(_ context.Context, _ *orders.Account, req *orders.SubmitRequest) (*orders.Ack, error) {
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	f.submitted = append(f.submitted, req)
	return &orders.Ack{OrderID: 42, ClientOrderID: req.ClientOrderID, Status: "ACTIVE"}, nil
}
func (f *memFlow) Cancel(_ context.Context, _ *orders.Account, orderID int64, _, _, _ string) (*orders.Ack, error) {
	f.cancelled = append(f.cancelled, orderID)
	return &orders.Ack{OrderID: orderID, Status: "CANCELLED"}, nil
}
func (f *memFlow) CancelReplace(_ context.Context, _ *orders.Account, orderID int64,
	_ *orders.CancelReplaceRequest, _, _, _ string) (*orders.Order, error) {
	f.replaced = append(f.replaced, orderID)
	return &orders.Order{ID: orderID, Status: "ACTIVE"}, nil
}
func (f *memFlow) CancelOnDisconnect(_ context.Context, accountID int64, sessionID string) (*orders.MassCancelResult, error) {
	f.codCalls = append(f.codCalls, struct {
		acct int64
		sess string
	}{accountID, sessionID})
	return &orders.MassCancelResult{Cancelled: 2}, nil
}
func (f *memFlow) GetOrder(_ context.Context, _ *orders.Account, orderID int64) (*orders.Order, error) {
	return &orders.Order{ID: orderID, Status: "ACTIVE"}, nil
}

type memRead struct{ orders map[int64]*orders.Order }

func (r *memRead) GetOrder(_ context.Context, id int64) (*orders.Order, error) {
	return r.orders[id], nil
}
func (r *memRead) DedupLookup(context.Context, int64, string) (*orders.DedupRow, error) {
	return nil, nil
}
func (r *memRead) ApplyCancel(context.Context, int64) error { return nil }
func (r *memRead) ApplyFill(context.Context, int64, decimal.Decimal, decimal.Decimal) error {
	return nil
}

type memDeadMan struct {
	set      []int64
	disabled []int64
	err      error
}

func (d *memDeadMan) Set(_ context.Context, _ int64, ms int64, _ bool) (*accounts.CountdownAck, error) {
	if d.err != nil {
		return nil, d.err
	}
	d.set = append(d.set, ms)
	return &accounts.CountdownAck{CountdownExpiry: time.Now().UnixMilli() + ms}, nil
}
func (d *memDeadMan) Disable(_ context.Context, _ int64) (*accounts.CountdownAck, error) {
	d.disabled = append(d.disabled, 0)
	return &accounts.CountdownAck{}, nil
}
func (d *memDeadMan) Status(_ context.Context, _ int64) (*accounts.CountdownAck, error) {
	return &accounts.CountdownAck{}, nil
}

// ---- harness ----------------------------------------------------------------

func i64(v int64) *int64 { return &v }

func testSessionID() quickfix.SessionID {
	return quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "EXC", TargetCompID: "CLI"}
}

func newTestApp(st *memStore, flow *memFlow, dm *memDeadMan) (*App, *[]ReportEvent) {
	app := NewApp(Options{
		Store: st, Orders: flow, OrderRead: &memRead{},
		DeadMan: dm, Log: slogNop{},
	})
	var captured []ReportEvent
	app.Report().WithTap(func(ev ReportEvent) { captured = append(captured, ev) })
	return app, &captured
}

type slogNop struct{}

func (slogNop) Info(string, ...any)  {}
func (slogNop) Warn(string, ...any)  {}
func (slogNop) Error(string, ...any) {}

func logonMsg(user, pass string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgLogon))
	m.Body.SetString(TagUsername, user)
	m.Body.SetString(TagPassword, pass)
	return m
}

func bodyText(t *testing.T, m *quickfix.Message) string {
	t.Helper()
	v, err := m.Body.GetString(TagText)
	if err != nil {
		return ""
	}
	return v
}

func bodyStr(m *quickfix.Message, tag quickfix.Tag) string {
	v, err := m.Body.GetString(tag)
	if err != nil {
		return ""
	}
	return v
}

// ---- logon authentication ----------------------------------------------------

func TestFromAdmin_UnknownSessionRejected(t *testing.T) {
	st := newMemStore()
	app, _ := newTestApp(st, &memFlow{}, nil)
	sid := testSessionID()
	err := app.FromAdmin(logonMsg("k", "s"), sid)
	if err == nil {
		t.Fatal("unprovisioned session must be rejected")
	}
	if rl, ok := err.(quickfix.RejectLogon); !ok || rl.Text != "SESSION_NOT_PROVISIONED" {
		t.Fatalf("reject: %#v", err)
	}
}

func TestFromAdmin_BadCredentialRejected(t *testing.T) {
	st := newMemStore()
	key := int64(7)
	st.sessions[testSessionID().String()] = &Session{
		SessionID: testSessionID().String(), APIKeyID: &key, AccountID: i64(11),
	}
	st.verify = func(id int64, name, secret string) (int64, bool, error) {
		return 11, false, nil // credential mismatch
	}
	app, _ := newTestApp(st, &memFlow{}, nil)
	err := app.FromAdmin(logonMsg("sub_x", "wrong"), testSessionID())
	if rl, ok := err.(quickfix.RejectLogon); !ok || rl.Text != "SESSION_INVALID_CREDENTIALS" {
		t.Fatalf("reject: %#v", err)
	}
}

func TestFromAdmin_CredentialAccountMismatch(t *testing.T) {
	// A valid API key belonging to a DIFFERENT account must not open a
	// session bound to another account.
	st := newMemStore()
	key := int64(7)
	st.sessions[testSessionID().String()] = &Session{
		SessionID: testSessionID().String(), APIKeyID: &key, AccountID: i64(11),
	}
	st.verify = func(id int64, name, secret string) (int64, bool, error) {
		return 99, true, nil // key verifies, but account is 99 not 11
	}
	app, _ := newTestApp(st, &memFlow{}, nil)
	err := app.FromAdmin(logonMsg("sub_x", "secret"), testSessionID())
	if rl, ok := err.(quickfix.RejectLogon); !ok || rl.Text != "SESSION_NOT_ENTITLED" {
		t.Fatalf("reject: %#v", err)
	}
}

func TestFromAdmin_ValidLogon(t *testing.T) {
	st := newMemStore()
	key := int64(7)
	st.sessions[testSessionID().String()] = &Session{
		SessionID: testSessionID().String(), APIKeyID: &key, AccountID: i64(11),
	}
	st.verify = func(id int64, name, secret string) (int64, bool, error) {
		return 11, true, nil
	}
	app, _ := newTestApp(st, &memFlow{}, nil)
	if err := app.FromAdmin(logonMsg("sub_x", "secret"), testSessionID()); err != nil {
		t.Fatalf("valid logon must pass: %v", err)
	}
}

// ---- entitlement + throttle --------------------------------------------------

func entitledSession() *Session {
	return &Session{
		SessionID:          testSessionID().String(),
		AccountID:          i64(11),
		AllowedInstruments: "EUR/USD",
		CancelOnDisconnect: true,
		MaxMsgsPerSec:      100,
	}
}

func TestFromApp_DropCopyCannotSubmit(t *testing.T) {
	st := newMemStore()
	row := entitledSession()
	row.AccountID = nil // drop copy
	st.sessions[testSessionID().String()] = row
	app, captured := newTestApp(st, &memFlow{acct: &orders.Account{ID: 11}}, nil)
	app.FromApp(newOrderMsg(), testSessionID())
	if len(*captured) != 1 {
		t.Fatalf("emits: %d", len(*captured))
	}
	m := (*captured)[0].Msg
	if mt, _ := m.MsgType(); mt != MsgBusinessReject {
		t.Fatalf("want 35=j, got %v", mt)
	}
	if bodyText(t, m) != "SESSION_NOT_ENTITLED" {
		t.Fatalf("text: %q", bodyText(t, m))
	}
	if v, _ := m.Body.GetInt(TagBusinessRejectReason); v != BusinessRejectReasonNotEntitled {
		t.Fatalf("reason: %d", v)
	}
}

func TestFromApp_UntitledInstrumentRejected(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	flow := &memFlow{acct: &orders.Account{ID: 11}}
	app, captured := newTestApp(st, flow, nil)
	m := newOrderMsg()
	m.Body.SetString(TagSymbol, "GBP/JPY") // not in allowlist
	app.FromApp(m, testSessionID())
	if len(flow.submitted) != 0 {
		t.Fatal("non-entitled order must never reach Submit")
	}
	if len(*captured) != 1 || bodyText(t, (*captured)[0].Msg) != "SESSION_NOT_ENTITLED" {
		t.Fatalf("emits: %+v", *captured)
	}
}

func TestFromApp_ThrottledNeverSilentlyDropped(t *testing.T) {
	st := newMemStore()
	row := entitledSession()
	row.MaxMsgsPerSec = 2
	st.sessions[testSessionID().String()] = row
	flow := &memFlow{acct: &orders.Account{ID: 11}}
	app, captured := newTestApp(st, flow, nil)

	for i := 0; i < 5; i++ {
		app.FromApp(newOrderMsg(), testSessionID())
	}
	throttled, accepted := 0, 0
	for _, ev := range *captured {
		mt, _ := ev.Msg.MsgType()
		switch {
		case mt == MsgBusinessReject && bodyText(t, ev.Msg) == "SESSION_THROTTLED":
			throttled++
			if v, _ := ev.Msg.Body.GetInt(TagBusinessRejectReason); v != BusinessRejectReasonThrottled {
				t.Fatalf("throttle reason: %d", v)
			}
		case mt == MsgExecutionReport:
			accepted++
		default:
			t.Fatalf("unexpected emit %v", mt)
		}
	}
	if throttled != 3 || accepted != 2 {
		t.Fatalf("want 2 accepted + 3 throttled, got %d/%d", accepted, throttled)
	}
	if len(flow.submitted) != 2 {
		t.Fatalf("submits: %d", len(flow.submitted))
	}
}

func TestFromApp_NewOrderSingleAccepted(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	flow := &memFlow{acct: &orders.Account{ID: 11}}
	app, captured := newTestApp(st, flow, nil)
	app.FromApp(newOrderMsg(), testSessionID())
	if len(flow.submitted) != 1 {
		t.Fatalf("submits: %d", len(flow.submitted))
	}
	req := flow.submitted[0]
	if req.Symbol != "EUR/USD" || req.SessionID != testSessionID().String() {
		t.Fatalf("req: %+v", req)
	}
	if len(*captured) != 1 {
		t.Fatalf("emits: %d", len(*captured))
	}
	m := (*captured)[0].Msg
	if mt, _ := m.MsgType(); mt != MsgExecutionReport {
		t.Fatalf("want 35=8, got %v", mt)
	}
	if bodyStr(m, TagExecType) != ExecTypeNew {
		t.Fatalf("exectype: %q", bodyStr(m, TagExecType))
	}
}

// ---- disconnect semantics ----------------------------------------------------

func TestOnLogout_AbnormalRunsCoD_OrderlyPreserves(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	flow := &memFlow{acct: &orders.Account{ID: 11}}
	app, _ := newTestApp(st, flow, nil)
	sid := testSessionID()

	// Abnormal drop (no orderly Logout seen): CoD mass-cancels.
	app.OnLogout(sid)
	if len(flow.codCalls) != 1 || flow.codCalls[0].acct != 11 ||
		flow.codCalls[0].sess != sid.String() {
		t.Fatalf("cod calls: %+v", flow.codCalls)
	}

	// Orderly Logout (35=5 inbound marks the session) preserves orders.
	app.FromAdmin(logoutMsg(), sid)
	app.OnLogout(sid)
	if len(flow.codCalls) != 1 {
		t.Fatal("orderly logout must NOT mass-cancel")
	}
}

func TestOnLogout_CoDDisabledPreserves(t *testing.T) {
	st := newMemStore()
	row := entitledSession()
	row.CancelOnDisconnect = false
	st.sessions[testSessionID().String()] = row
	flow := &memFlow{}
	app, _ := newTestApp(st, flow, nil)
	app.OnLogout(testSessionID())
	if len(flow.codCalls) != 0 {
		t.Fatal("cancel_on_disconnect=false must preserve orders")
	}
}

func logoutMsg() *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgLogout))
	return m
}

// ---- dead-man (35=BE) --------------------------------------------------------

func userReq(ms string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgUserRequest))
	m.Body.SetString(TagUserRequestID, "ur-1")
	m.Body.SetString(TagUserRequestType, UserRequestTypeCountdown)
	m.Body.SetString(TagCountdownMs, ms)
	return m
}

func TestDeadMan_ArmAndDisable(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	dm := &memDeadMan{}
	app, captured := newTestApp(st, &memFlow{acct: &orders.Account{ID: 11}}, dm)

	app.FromApp(userReq("30000"), testSessionID())
	if len(dm.set) != 1 || dm.set[0] != 30000 {
		t.Fatalf("set calls: %v", dm.set)
	}
	last := (*captured)[len(*captured)-1].Msg
	if mt, _ := last.MsgType(); mt != MsgUserResponse {
		t.Fatalf("want 35=BF, got %v", mt)
	}
	if bodyStr(last, TagUserStatus) != "1" || bodyStr(last, TagCountdownMs) != "30000" {
		t.Fatalf("resp: %q %q", bodyStr(last, TagUserStatus), bodyStr(last, TagCountdownMs))
	}

	app.FromApp(userReq("0"), testSessionID())
	if len(dm.disabled) != 1 {
		t.Fatalf("disable calls: %v", dm.disabled)
	}
}

func TestDeadMan_OutOfRange(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	dm := &memDeadMan{}
	app, captured := newTestApp(st, &memFlow{acct: &orders.Account{ID: 11}}, dm)
	for _, ms := range []string{"500", "60001", "-1", "abc"} {
		app.FromApp(userReq(ms), testSessionID())
	}
	if len(dm.set) != 0 {
		t.Fatalf("out-of-range must never reach the timer: %v", dm.set)
	}
	for _, ev := range *captured {
		if bodyStr(ev.Msg, TagUserStatusText) != "INVALID_TIMEOUT" {
			t.Fatalf("resp text: %q", bodyStr(ev.Msg, TagUserStatusText))
		}
	}
}

func TestDeadMan_DropCopyRejected(t *testing.T) {
	st := newMemStore()
	row := entitledSession()
	row.AccountID = nil
	st.sessions[testSessionID().String()] = row
	dm := &memDeadMan{}
	app, captured := newTestApp(st, &memFlow{}, dm)
	app.FromApp(userReq("30000"), testSessionID())
	if len(dm.set) != 0 {
		t.Fatal("drop-copy session must not arm countdowns")
	}
	if bodyStr((*captured)[0].Msg, TagUserStatusText) != "SESSION_NOT_ENTITLED" {
		t.Fatalf("resp: %+v", (*captured)[0].Msg)
	}
}

func TestDeadMan_UnsupportedRequestType(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	dm := &memDeadMan{}
	app, captured := newTestApp(st, &memFlow{}, dm)
	m := userReq("30000")
	m.Body.SetString(TagUserRequestType, "9")
	app.FromApp(m, testSessionID())
	if len(dm.set) != 0 || bodyStr((*captured)[0].Msg, TagUserStatusText) != "USERREQUEST_UNSUPPORTED" {
		t.Fatalf("resp: %+v", (*captured)[0].Msg)
	}
}

// ---- Phase-3 Task 3 wiring: 35=J dispatch + drop-copy binding -----------------

type memDropBindings struct{ rows map[string][]int64 }

func (b memDropBindings) Bindings(_ context.Context, s string) ([]int64, error) {
	return b.rows[s], nil
}
func (b memDropBindings) Save(_ context.Context, s string, a []int64) error {
	b.rows[s] = a
	return nil
}

// 35=J must reach the allocation service — the report it emits (35=AK on
// accept, 35=P on reject) proves dispatch; MSGTYPE_UNSUPPORTED is the
// pre-wiring failure mode this test locks out.
func TestFromApp_AllocationInstructionDispatches(t *testing.T) {
	st := newMemStore()
	row := entitledSession()
	row.AccountID = i64(900) // allocation legs resolve under master 900
	st.sessions[testSessionID().String()] = row
	svc, _ := testSvc(newFakeAllocStore())
	app := NewApp(Options{
		Store: st, Orders: &memFlow{}, OrderRead: &memRead{},
		Log: slogNop{}, Allocations: svc,
	})
	var captured []ReportEvent
	app.Report().WithTap(func(ev ReportEvent) { captured = append(captured, ev) })

	m := newAllocMsg("A1", 0, "MANUAL")
	m.Body.SetString(TagQuantity, "100")
	addExecGroup(m, "E1")
	addAllocLegs(m, [2]string{"SUB1", "100"})
	app.FromApp(m, testSessionID())

	if len(captured) == 0 {
		t.Fatal("35=J produced no report")
	}
	mt, _ := captured[0].Msg.MsgType()
	if mt == MsgBusinessReject && bodyText(t, captured[0].Msg) == "MSGTYPE_UNSUPPORTED" {
		t.Fatal("35=J fell through to MSGTYPE_UNSUPPORTED — not wired")
	}
	if mt != MsgAllocationReport && mt != MsgAllocationInstructionAck {
		t.Fatalf("want 35=AK/35=P, got %v", mt)
	}
}

// A drop-copy session (account_id NULL) may never instruct allocations.
func TestFromApp_AllocationDropCopyRejected(t *testing.T) {
	st := newMemStore()
	row := entitledSession()
	row.AccountID = nil
	st.sessions[testSessionID().String()] = row
	svc, _ := testSvc(newFakeAllocStore())
	app := NewApp(Options{
		Store: st, Orders: &memFlow{}, OrderRead: &memRead{},
		Log: slogNop{}, Allocations: svc,
	})
	var captured []ReportEvent
	app.Report().WithTap(func(ev ReportEvent) { captured = append(captured, ev) })

	m := newAllocMsg("A1", 0, "MANUAL")
	addAllocLegs(m, [2]string{"SUB1", "1"})
	app.FromApp(m, testSessionID())

	if len(captured) != 1 {
		t.Fatalf("emits: %d", len(captured))
	}
	if mt, _ := captured[0].Msg.MsgType(); mt != MsgBusinessReject {
		t.Fatalf("want 35=j, got %v", mt)
	}
	if bodyText(t, captured[0].Msg) != "SESSION_NOT_ENTITLED" {
		t.Fatalf("text: %q", bodyText(t, captured[0].Msg))
	}
}

// Unwired (nil Allocations) sessions still fail closed.
func TestFromApp_AllocationUnwiredFailsClosed(t *testing.T) {
	st := newMemStore()
	st.sessions[testSessionID().String()] = entitledSession()
	app, captured := newTestApp(st, &memFlow{}, nil)
	m := newAllocMsg("A1", 0, "MANUAL")
	addAllocLegs(m, [2]string{"SUB1", "1"})
	app.FromApp(m, testSessionID())
	if len(*captured) != 1 || bodyText(t, (*captured)[0].Msg) != "MSGTYPE_UNSUPPORTED" {
		t.Fatalf("emits: %+v", *captured)
	}
}

// A drop-copy logon binds its provisioned account set into the router;
// a report for a bound account then attempts the fan-out send (which
// fails over the dead transport in tests — the attempt itself is the
// wiring proof, observed via OnError).
func TestOnLogon_BindsDropCopyTarget(t *testing.T) {
	st := newMemStore()
	dc := &Session{
		SessionID:     "FIX.4.4:EXC->COPY",
		MaxMsgsPerSec: 100,
		// AccountID nil → KindDropCopy
	}
	st.sessions[dc.SessionID] = dc
	router := NewDropCopyRouter()
	var sendErrs []error
	router.OnError = func(e error) { sendErrs = append(sendErrs, e) }
	router.OrderAccount = func(_ context.Context, orderID int64) (int64, error) {
		return 11, nil // the report's owner
	}
	app := NewApp(Options{
		Store: st, Orders: &memFlow{}, OrderRead: &memRead{},
		Log: slogNop{}, DropCopy: router,
		DropCopyBindings: memDropBindings{rows: map[string][]int64{
			dc.SessionID: {11},
		}},
	})
	app.Report().WithTap(router.Tap())

	dcSID := quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "EXC", TargetCompID: "COPY"}
	app.OnLogon(dcSID)

	// A report for bound account 11 reaches the bound copy target — the
	// send attempt errors (no live transport in tests), proving the tap
	// chain executed rather than silently no-oping.
	app.Report().Emit(ReportEvent{
		SessionID: testSessionID(), OrderID: 5, Msg: newReport(),
	})
	if len(sendErrs) == 0 {
		t.Fatal("bound drop-copy target never attempted the copy send")
	}

	// Unbound accounts copy nothing.
	sendErrs = nil
	router.OrderAccount = func(context.Context, int64) (int64, error) { return 77, nil }
	app.Report().Emit(ReportEvent{
		SessionID: testSessionID(), OrderID: 6, Msg: newReport(),
	})
	if len(sendErrs) != 0 {
		t.Fatal("account 77 is not bound — must never be copied")
	}

	// Logout releases the target — subsequent reports send nowhere.
	app.OnLogout(dcSID)
	sendErrs = nil
	router.OrderAccount = func(context.Context, int64) (int64, error) { return 11, nil }
	app.Report().Emit(ReportEvent{
		SessionID: testSessionID(), OrderID: 7, Msg: newReport(),
	})
	if len(sendErrs) != 0 {
		t.Fatal("logged-out drop-copy session still bound")
	}
}
