// Task 18.3.18 tests — inbound gap verdicts (incl. the >2,500
// fail-closed Logout), outbound resend planning (admin gap-fill vs
// PossDup replay + archive-hole refusal), reject/message builders, and
// the 50ms cancel-on-disconnect executor (incl. rate-gate interplay and
// the graceful-Logouts-preserve-orders rule).
package fix

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testMsgType(t *testing.T, m *quickfix.Message) string {
	t.Helper()
	mt, err := m.MsgType()
	if err != nil {
		t.Fatalf("msgtype: %v", err)
	}
	return mt
}

func bodyInt(t *testing.T, m *quickfix.Message, tag quickfix.Tag) int {
	t.Helper()
	v, err := m.Body.GetInt(tag)
	if err != nil {
		t.Fatalf("tag %d: %v", int(tag), err)
	}
	return v
}

func bodyStrT(t *testing.T, m *quickfix.Message, tag quickfix.Tag) string {
	t.Helper()
	v, err := m.Body.GetString(tag)
	if err != nil {
		t.Fatalf("tag %d: %v", int(tag), err)
	}
	return v
}

func headerBool(t *testing.T, m *quickfix.Message, tag quickfix.Tag) bool {
	t.Helper()
	v, err := m.Header.GetBool(tag)
	if err != nil {
		t.Fatalf("hdr tag %d: %v", int(tag), err)
	}
	return v
}

// ---------------------------------------------------------------------------
// inbound gap assessment
// ---------------------------------------------------------------------------

func TestAssessInbound(t *testing.T) {
	tests := []struct {
		name     string
		received int64
		expected int64
		possDup  bool
		want     InboundAction
	}{
		{"in order", 5, 5, false, InboundAccept},
		{"small gap", 8, 5, false, InboundResendRequest},
		{"max gap boundary", 2505, 5, false, InboundResendRequest},
		{"excessive gap", 2506, 5, false, InboundLogoutExcessive},
		{"excessive gap possdup still fatal", 9000, 5, true, InboundLogoutExcessive},
		{"low seq possdup dropped", 3, 5, true, InboundDropPossDup},
		{"low seq no possdup fatal", 3, 5, false, InboundTooLow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := AssessInbound(tc.received, tc.expected, tc.possDup)
			if a.Action != tc.want {
				t.Fatalf("action = %v, want %v", a.Action, tc.want)
			}
			switch tc.want {
			case InboundResendRequest:
				if a.BeginSeqNo != tc.expected || a.EndSeqNo != 0 {
					t.Fatalf("resend window = [%d,%d], want [%d,0]",
						a.BeginSeqNo, a.EndSeqNo, tc.expected)
				}
			case InboundLogoutExcessive:
				if a.Logout == nil {
					t.Fatal("missing prebuilt Logout")
				}
				if testMsgType(t, a.Logout) != MsgLogout {
					t.Fatal("excessive-gap message is not 35=5")
				}
				if bodyStrT(t, a.Logout, TagText) != ExcessiveGapLogoutText {
					t.Fatalf("logout text = %q", bodyStrT(t, a.Logout, TagText))
				}
			case InboundTooLow:
				if a.Reject == nil || a.Logout == nil {
					t.Fatal("missing Reject/Logout pair")
				}
				if testMsgType(t, a.Reject) != MsgReject {
					t.Fatal("too-low message is not 35=3")
				}
				if got := bodyInt(t, a.Reject, TagSessionRejectReason); got != SessionRejectValueIncorrect {
					t.Fatalf("session reject reason = %d, want 5", got)
				}
				if got := bodyInt(t, a.Reject, TagRefTagID); got != int(TagMsgSeqNum) {
					t.Fatalf("ref tag = %d, want 34", got)
				}
			}
		})
	}
}

func TestHeartbeatAbnormal(t *testing.T) {
	hbi := 30 * time.Second
	last := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if HeartbeatAbnormal(last, last.Add(2*hbi), hbi) {
		t.Fatal("exactly 2x heartbeat is not yet abnormal")
	}
	if !HeartbeatAbnormal(last, last.Add(2*hbi+time.Millisecond), hbi) {
		t.Fatal("past 2x heartbeat must be abnormal")
	}
	if !HeartbeatAbnormal(last, last.Add(61*time.Second), 0) {
		t.Fatal("default 30s heartBtInt not applied")
	}
}

// ---------------------------------------------------------------------------
// resend planning
// ---------------------------------------------------------------------------

func arch(seq int64, mt string) ArchivedMessage {
	return ArchivedMessage{SeqNum: seq, MsgType: mt,
		SendingTime: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
		Raw:         []byte("wire")}
}

func TestPlanResendMixedAdminAndApp(t *testing.T) {
	// Outbound archive: 5=NewOrderSingle-ish app, 6/7 heartbeats, 8=ExecReport,
	// 9=heartbeat. currentOut=10 → endSeq 0 resolves to 9.
	msgs := []ArchivedMessage{
		arch(5, "8"), arch(6, "0"), arch(7, "1"), arch(8, "8"), arch(9, "0"),
	}
	entries, err := PlanResend(5, 0, 10, msgs)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4 (replay,gapfill,replay,gapfill): %+v", len(entries), entries)
	}
	if entries[0].Replay == nil || entries[0].Replay.SeqNum != 5 {
		t.Fatalf("entry0 = %+v, want replay seq 5", entries[0])
	}
	if entries[1].Replay != nil || entries[1].NewSeqNo != 8 {
		t.Fatalf("entry1 = %+v, want gapfill NewSeqNo=8", entries[1])
	}
	if entries[2].Replay == nil || entries[2].Replay.SeqNum != 8 {
		t.Fatalf("entry2 = %+v, want replay seq 8", entries[2])
	}
	if entries[3].Replay != nil || entries[3].NewSeqNo != 10 {
		t.Fatalf("entry3 = %+v, want trailing gapfill NewSeqNo=10", entries[3])
	}
}

func TestPlanResendArchiveHoleFailsClosed(t *testing.T) {
	msgs := []ArchivedMessage{arch(5, "8"), arch(7, "8")} // seq 6 missing
	_, err := PlanResend(5, 7, 10, msgs)
	if !errors.Is(err, ErrResendHole) {
		t.Fatalf("err = %v, want ErrResendHole", err)
	}
}

func TestPlanResendAllAdminCollapses(t *testing.T) {
	msgs := []ArchivedMessage{arch(5, "0"), arch(6, "1"), arch(7, "0")}
	entries, err := PlanResend(5, 7, 10, msgs)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(entries) != 1 || entries[0].NewSeqNo != 8 {
		t.Fatalf("entries = %+v, want single gapfill NewSeqNo=8", entries)
	}
}

func TestPlanResendEmptyRange(t *testing.T) {
	entries, err := PlanResend(10, 5, 12, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(entries) != 1 || entries[0].NewSeqNo != 12 {
		t.Fatalf("entries = %+v, want gapfill to 12", entries)
	}
}

func TestPlanResendRejectsBadBegin(t *testing.T) {
	if _, err := PlanResend(0, 5, 10, nil); err == nil {
		t.Fatal("begin<1 must error")
	}
}

// ---------------------------------------------------------------------------
// message builders
// ---------------------------------------------------------------------------

func TestMessageBuilders(t *testing.T) {
	m := NewResendRequest(7, 0)
	if testMsgType(t, m) != MsgResendRequest {
		t.Fatal("not 35=2")
	}
	if bodyInt(t, m, TagBeginSeqNo) != 7 || bodyInt(t, m, TagEndSeqNo) != 0 {
		t.Fatal("begin/end wrong")
	}

	m = NewSequenceReset(42, true)
	if testMsgType(t, m) != MsgSequenceReset {
		t.Fatal("not 35=4")
	}
	if bodyInt(t, m, TagNewSeqNo) != 42 {
		t.Fatal("newSeqNo wrong")
	}
	if v, err := m.Body.GetBool(TagGapFillFlag); err != nil || !v {
		t.Fatal("gapfill flag not Y")
	}

	m = NewLogout("EXCESSIVE_SEQUENCE_GAP")
	if testMsgType(t, m) != MsgLogout || bodyStrT(t, m, TagText) != "EXCESSIVE_SEQUENCE_GAP" {
		t.Fatal("logout malformed")
	}

	m = NewReject(11, TagMsgSeqNum, "D", SessionRejectValueIncorrect, "too low")
	if testMsgType(t, m) != MsgReject {
		t.Fatal("not 35=3")
	}
	if bodyInt(t, m, TagRefSeqNum) != 11 || bodyInt(t, m, TagRefTagID) != int(TagMsgSeqNum) {
		t.Fatal("reject refs wrong")
	}
	if bodyStrT(t, m, TagRefMsgType) != "D" {
		t.Fatal("ref msgtype wrong")
	}
	if bodyInt(t, m, TagSessionRejectReason) != 5 {
		t.Fatal("session reject reason wrong")
	}

	m = NewBusinessReject(9, "D", "BR-1", BusinessRejectReasonNotEntitled, "SESSION_NOT_ENTITLED")
	if testMsgType(t, m) != MsgBusinessReject {
		t.Fatal("not 35=j")
	}
	if bodyInt(t, m, TagRefSeqNum) != 9 {
		t.Fatal("missing RefSeqNum(45)")
	}
	if bodyStrT(t, m, TagBusinessRejectRefID) != "BR-1" {
		t.Fatal("business reject ref id wrong")
	}
	if bodyInt(t, m, TagBusinessRejectReason) != BusinessRejectReasonNotEntitled {
		t.Fatal("business reject reason wrong")
	}
	if bodyStrT(t, m, TagText) != "SESSION_NOT_ENTITLED" {
		t.Fatal("text must carry the registry code")
	}

	m = NewNews("Scheduled maintenance — reconnect to fix2.venue:9800")
	if testMsgType(t, m) != MsgNews || !strings.Contains(bodyStrT(t, m, TagHeadline), "Scheduled maintenance") {
		t.Fatal("news malformed")
	}

	orig := time.Date(2026, 9, 29, 11, 58, 0, 0, time.UTC)
	m = NewResendRequest(1, 0)
	MarkReplay(m, orig)
	if !headerBool(t, m, TagPossDupFlag) {
		t.Fatal("PossDupFlag not set")
	}
	if !m.Header.Has(TagOrigSendingTime) {
		t.Fatal("OrigSendingTime not set")
	}
}

func TestMapBusinessRejectReason(t *testing.T) {
	cases := map[string]int{
		"SESSION_NOT_ENTITLED": BusinessRejectReasonNotEntitled,
		"SESSION_THROTTLED":    BusinessRejectReasonThrottled,
		"UNKNOWN_SYMBOL":       BusinessRejectReasonUnknownSecurity,
		"ORDER_NOT_FOUND":      BusinessRejectReasonUnknownID,
		"TRADING_HALTED":       BusinessRejectReasonAppUnavailable,
		"WHATEVER":             BusinessRejectReasonOther,
	}
	for code, want := range cases {
		if got := MapBusinessRejectReason(code); got != want {
			t.Errorf("%s → %d, want %d", code, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// cancel-on-disconnect
// ---------------------------------------------------------------------------

type fakeLifecycleStore struct {
	mu      sync.Mutex
	row     *Session
	status  []string
	heartAt []*time.Time
	getErr  error
	setErr  error
}

func (f *fakeLifecycleStore) SessionByID(_ context.Context, id string) (*Session, error) {
	return f.row, f.getErr
}

func (f *fakeLifecycleStore) SetStatus(_ context.Context, id, status string, hb *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = append(f.status, status)
	f.heartAt = append(f.heartAt, hb)
	return f.setErr
}

type fakeCanceller struct {
	calls   int
	delay   time.Duration
	retN    int
	retErr  error
	lastID  int64
	lastSID string
}

func (f *fakeCanceller) CancelSessionOrders(_ context.Context, accountID int64, sessionID string) (int, error) {
	f.calls++
	f.lastID, f.lastSID = accountID, sessionID
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.retN, f.retErr
}

type fakeGate struct {
	allowed bool
	err     error
}

func (f *fakeGate) Allow(_ context.Context, _ int64, _ time.Duration) (bool, error) {
	return f.allowed, f.err
}

func i64p(v int64) *int64 { return &v }

func codSession(acct *int64, cod bool) *Session {
	return &Session{SessionID: "FIX.4.4:VENUE->CLIENT",
		AccountID: acct, CancelOnDisconnect: cod, Status: "ACTIVE"}
}

func TestCoDPurgesWithinBudget(t *testing.T) {
	store := &fakeLifecycleStore{row: codSession(i64p(77), true)}
	cancel := &fakeCanceller{retN: 3}
	ex := NewCoDExecutor(store, cancel, &fakeGate{allowed: true}, nil, nil, nil)

	ev, err := ex.OnDisconnect(context.Background(), "FIX.4.4:VENUE->CLIENT", "socket_drop", false)
	if err != nil {
		t.Fatalf("cod: %v", err)
	}
	if cancel.calls != 1 || cancel.lastID != 77 {
		t.Fatalf("cancel calls=%d acct=%d", cancel.calls, cancel.lastID)
	}
	if ev.Cancelled != 3 || !ev.WithinBudget {
		t.Fatalf("event = %+v", ev)
	}
	if len(store.status) != 1 || store.status[0] != "DISCONNECTED" {
		t.Fatalf("status writes = %v", store.status)
	}
}

func TestCoDGracefulLogoutPreservesOrders(t *testing.T) {
	store := &fakeLifecycleStore{row: codSession(i64p(77), true)}
	cancel := &fakeCanceller{retN: 3}
	ex := NewCoDExecutor(store, cancel, nil, nil, nil, nil)

	ev, err := ex.OnDisconnect(context.Background(), "FIX.4.4:VENUE->CLIENT", "peer_logout", true)
	if err != nil {
		t.Fatalf("cod: %v", err)
	}
	if cancel.calls != 0 {
		t.Fatal("graceful logout must NOT mass-cancel (AC #30)")
	}
	if !ev.Graceful || store.status[0] != "LOGGED_OUT" {
		t.Fatalf("event=%+v status=%v", ev, store.status)
	}
}

func TestCoDDisabledAndDropCopy(t *testing.T) {
	// cancel_on_disconnect=false → mark DISCONNECTED, no purge.
	store := &fakeLifecycleStore{row: codSession(i64p(77), false)}
	cancel := &fakeCanceller{}
	ex := NewCoDExecutor(store, cancel, nil, nil, nil, nil)
	ev, err := ex.OnDisconnect(context.Background(), "FIX.4.4:VENUE->CLIENT", "socket_drop", false)
	if err != nil || !ev.CoDDisabled || cancel.calls != 0 {
		t.Fatalf("disabled: ev=%+v calls=%d err=%v", ev, cancel.calls, err)
	}

	// account_id NULL (drop-copy) → read-only, nothing to purge.
	store.row = codSession(nil, true)
	ev, err = ex.OnDisconnect(context.Background(), "FIX.4.4:VENUE->CLIENT", "socket_drop", false)
	if err != nil || !ev.DropCopyOnly || cancel.calls != 0 {
		t.Fatalf("dropcopy: ev=%+v calls=%d err=%v", ev, cancel.calls, err)
	}
}

func TestCoDRateGate(t *testing.T) {
	// §24 #245 — second purge inside the 5s window is gated, not dropped silently.
	store := &fakeLifecycleStore{row: codSession(i64p(77), true)}
	cancel := &fakeCanceller{retN: 1}
	ex := NewCoDExecutor(store, cancel, &fakeGate{allowed: false}, nil, nil, nil)
	ev, err := ex.OnDisconnect(context.Background(), "FIX.4.4:VENUE->CLIENT", "socket_drop", false)
	if err != nil || !ev.RateLimited || cancel.calls != 0 {
		t.Fatalf("gated: ev=%+v calls=%d err=%v", ev, cancel.calls, err)
	}

	// A broken gate must not strand live orders on a dead session —
	// fail-closed pessimism proceeds with the purge.
	cancel = &fakeCanceller{retN: 2}
	ex = NewCoDExecutor(store, cancel, &fakeGate{err: errors.New("redis down")}, nil, nil, nil)
	ev, err = ex.OnDisconnect(context.Background(), "FIX.4.4:VENUE->CLIENT", "socket_drop", false)
	if err != nil || cancel.calls != 1 || ev.Cancelled != 2 {
		t.Fatalf("gate error: ev=%+v calls=%d err=%v", ev, cancel.calls, err)
	}
}

func TestCoDBudgetBreachSurfaced(t *testing.T) {
	// 50ms budget is measured: a slow cancel still completes but the
	// event flags the breach for alerting.
	store := &fakeLifecycleStore{row: codSession(i64p(77), true)}
	cancel := &fakeCanceller{retN: 5, delay: 60 * time.Millisecond}
	ex := NewCoDExecutor(store, cancel, nil, nil, nil, nil)
	ev, err := ex.OnDisconnect(context.Background(), "FIX.4.4:VENUE->CLIENT", "heartbeat_timeout", false)
	if err != nil {
		t.Fatalf("cod: %v", err)
	}
	if ev.WithinBudget {
		t.Fatal("60ms cancel must breach the 50ms budget")
	}
	if ev.Cancelled != 5 || ev.Elapsed < 60*time.Millisecond {
		t.Fatalf("event = %+v", ev)
	}
}

func TestCoDUnknownSessionFailsClosed(t *testing.T) {
	store := &fakeLifecycleStore{} // row nil → session not found
	ex := NewCoDExecutor(store, &fakeCanceller{}, nil, nil, nil, nil)
	_, err := ex.OnDisconnect(context.Background(), "FIX.4.4:VENUE->GHOST", "socket_drop", false)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestCoDUnwiredCancellerFailsClosed(t *testing.T) {
	store := &fakeLifecycleStore{row: codSession(i64p(77), true)}
	ex := NewCoDExecutor(store, nil, nil, nil, nil, nil)
	_, err := ex.OnDisconnect(context.Background(), "FIX.4.4:VENUE->CLIENT", "socket_drop", false)
	if err == nil {
		t.Fatal("unwired canceller must error, never pretend")
	}
}
