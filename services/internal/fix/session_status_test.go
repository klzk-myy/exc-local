// Task 18.3.15 tests — TradingSessionStatusRequest handling, TradSesStatus
// mapping, broadcast targeting, and the ≤50ms emit contract (§24 #243).
package fix

import (
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
)

func newTSSRequest(reqID string, subType int, symbol string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgTradingSessionStatusRequest)
	m.Body.SetString(TagTradSesReqID, reqID)
	m.Body.SetInt(TagSubscriptionRequestType, subType)
	if symbol != "" {
		m.Body.SetString(TagSymbol, symbol)
	}
	return m
}

func newTSSService(t *testing.T, sender *captureSender) *SessionStatusService {
	t.Helper()
	svc, err := NewSessionStatusService(SessionStatusDeps{
		Sender: sender,
		Known:  func(s string) bool { return s == "EUR/USD" || s == "GBP/USD" },
	})
	if err != nil {
		t.Fatalf("NewSessionStatusService: %v", err)
	}
	return svc
}

func TestInstrumentStatusMapping(t *testing.T) {
	cases := map[string]int{
		"ACTIVE":     2,
		"HALTED":     3,
		"RESTRICTED": 4,
		"SUSPENDED":  5,
		"DELISTED":   6,
	}
	for st, want := range cases {
		got, ok := mapInstrumentStatus(st)
		if !ok || got != want {
			t.Fatalf("%s → (%d,%v), want (%d,true)", st, got, ok, want)
		}
	}
	for _, st := range []string{"DRAFT", "CANCEL_ONLY", "", "BOGUS"} {
		if _, ok := mapInstrumentStatus(st); ok {
			t.Fatalf("%s must not map to a TradSesStatus", st)
		}
	}
}

func TestSessionStateMapping(t *testing.T) {
	cases := map[string]int{
		"OPEN": 2, "CLOSED": 3, "HALT": 3,
		"PRE_OPEN": 4, "AUCTION": 4, "PRE_CLOSE": 5,
	}
	for st, want := range cases {
		got, ok := mapSessionState(st)
		if !ok || got != want {
			t.Fatalf("%s → (%d,%v), want (%d,true)", st, got, ok, want)
		}
	}
}

func TestSubscribeThenBroadcast(t *testing.T) {
	sender := newCaptureSender()
	svc := newTSSService(t, sender)
	id := testSessionID()

	// Subscribe all scopes (no Symbol); a venue state must be visible so
	// the request's state reply resolves via last-observed events.
	svc.PublishSessionEvent(SessionLifecycleEvent{Event: "session.open", State: "OPEN"})
	if rej := svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("tsr-1", SubTypeSnapshotUpdates, "")); rej != nil {
		t.Fatal(rej)
	}
	got := sender.got(id)
	if len(got) != 1 || mustMsgType(t, got[0]) != "h" {
		t.Fatalf("expected 35=h state reply, got %v", got)
	}
	if v, _ := got[0].Body.GetString(TagTradSesReqID); v != "tsr-1" {
		t.Fatalf("TradSesReqID = %q", v)
	}
	if v, _ := got[0].Body.GetInt(TagTradSesStatus); v != 2 {
		t.Fatalf("TradSesStatus = %d, want 2 (Open)", v)
	}
	if v, _ := got[0].Body.GetString(TagTradingSessionID); v != "FX" {
		t.Fatalf("TradingSessionID = %q", v)
	}

	// Instrument HALTED → broadcast to the subscriber.
	svc.PublishInstrumentStatus(SecurityStatusEvent{
		Event: "SECURITY_STATUS", Symbol: "EUR/USD", Status: "HALTED", ChangeID: 7,
	})
	got = sender.got(id)
	if len(got) != 2 || mustMsgType(t, got[1]) != "h" {
		t.Fatalf("expected broadcast 35=h, got %v", got)
	}
	m := got[1]
	if v, _ := m.Body.GetInt(TagTradSesStatus); v != 3 {
		t.Fatalf("TradSesStatus = %d, want 3 (HALTED→Closed)", v)
	}
	if v, _ := m.Body.GetString(TagSymbol); v != "EUR/USD" {
		t.Fatalf("Symbol = %q", v)
	}
	if v, _ := m.Body.GetString(TagUnsolicitedIndicator); v != "Y" {
		t.Fatalf("UnsolicitedIndicator = %q", v)
	}
	if !m.Body.Has(TagTransactTime) {
		t.Fatal("TransactTime missing")
	}
}

func TestSymbolScopedSubscription(t *testing.T) {
	sender := newCaptureSender()
	svc := newTSSService(t, sender)
	id := testSessionID()

	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "ACTIVE"})
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("tsr-s", SubTypeSnapshotUpdates, "EUR/USD"))

	// Event on an unrelated symbol → no broadcast to this session.
	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "GBP/USD", Status: "HALTED"})
	if n := len(sender.got(id)); n != 1 {
		t.Fatalf("scoped session received unrelated broadcast; total %d", n)
	}
	// Matching symbol → broadcast.
	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "SUSPENDED"})
	got := sender.got(id)
	if len(got) != 2 {
		t.Fatalf("missing scoped broadcast, total %d", len(got))
	}
	if v, _ := got[1].Body.GetInt(TagTradSesStatus); v != 5 {
		t.Fatalf("SUSPENDED → %d, want 5 (Pre-Close)", v)
	}
	// Venue session event reaches all subscribers regardless of filter.
	svc.PublishSessionEvent(SessionLifecycleEvent{Event: "session.closed", State: "CLOSED"})
	got = sender.got(id)
	if len(got) != 3 {
		t.Fatalf("venue close broadcast missing, total %d", len(got))
	}
	if v, _ := got[2].Body.GetInt(TagTradSesStatus); v != 3 {
		t.Fatalf("CLOSED → %d, want 3", v)
	}
	if got[2].Body.Has(TagSymbol) {
		t.Fatal("venue-level broadcast must not carry Symbol")
	}
}

func TestSnapshotRequestNoRegistration(t *testing.T) {
	sender := newCaptureSender()
	svc := newTSSService(t, sender)
	id := testSessionID()

	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "ACTIVE"})
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("snap-1", SubTypeSnapshot, "EUR/USD"))
	if svc.SubscriptionCount(id) != 0 {
		t.Fatal("snapshot request must not register")
	}
	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "HALTED"})
	if n := len(sender.got(id)); n != 1 {
		t.Fatalf("snapshot-only client got broadcasts; total %d", n)
	}
	got := sender.got(id)
	if v, _ := got[0].Body.GetInt(TagTradSesStatus); v != 2 {
		t.Fatalf("snapshot reply TradSesStatus = %d, want 2", v)
	}
}

func TestUnsubscribeStopsBroadcasts(t *testing.T) {
	sender := newCaptureSender()
	svc := newTSSService(t, sender)
	id := testSessionID()

	svc.PublishSessionEvent(SessionLifecycleEvent{State: "OPEN"})
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("tsr-1", SubTypeSnapshotUpdates, ""))
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("tsr-1", SubTypeDisable, ""))
	if svc.SubscriptionCount(id) != 0 {
		t.Fatal("unsubscribe must remove the subscription")
	}
	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "HALTED"})
	if n := len(sender.got(id)); n != 1 {
		t.Fatalf("broadcast after unsubscribe; total %d", n)
	}
}

func TestRequestRejections(t *testing.T) {
	sender := newCaptureSender()
	svc := newTSSService(t, sender)
	id := testSessionID()

	// Unknown symbol.
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("bad-sym", SubTypeSnapshotUpdates, "XXX/YYY"))
	// Bad TradingSessionID.
	bad := newTSSRequest("bad-ts", SubTypeSnapshotUpdates, "")
	bad.Body.SetString(TagTradingSessionID, "OTHER")
	_ = svc.HandleTradingSessionStatusRequest(id, bad)
	// Unsupported SubscriptionRequestType.
	badSub := newTSSRequest("bad-sub", 9, "")
	_ = svc.HandleTradingSessionStatusRequest(id, badSub)

	got := sender.got(id)
	if len(got) != 3 {
		t.Fatalf("expected 3 rejection replies, got %d", len(got))
	}
	for i, m := range got {
		if mustMsgType(t, m) != "h" {
			t.Fatalf("reject %d msgtype = %s", i, mustMsgType(t, m))
		}
		if v, _ := m.Body.GetInt(TagTradSesStatus); v != TradSesRequestRejected {
			t.Fatalf("reject %d status = %d, want 6", i, v)
		}
	}
	if txt, _ := got[0].Body.GetString(TagText); !strings.Contains(txt, "UNKNOWN_SYMBOL") {
		t.Fatalf("unknown-symbol reject text = %q", txt)
	}
	if v, _ := got[1].Body.GetInt(TagTradSesStatusRejReas); v != TSRejReasonUnknownSessionOrID {
		t.Fatalf("bad-ts 567 = %d, want 1", v)
	}
}

func TestUnmappedStatesDoNotBroadcast(t *testing.T) {
	sender := newCaptureSender()
	svc := newTSSService(t, sender)
	id := testSessionID()
	svc.PublishSessionEvent(SessionLifecycleEvent{State: "OPEN"})
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("s", SubTypeSnapshotUpdates, ""))
	base := len(sender.got(id))

	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "CANCEL_ONLY"})
	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "DRAFT"})
	// Parameter-change event (Field set, Status empty) also skips.
	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Field: "tick_size"})
	if n := len(sender.got(id)); n != base {
		t.Fatalf("unmapped states broadcast; +%d frames", n-base)
	}
	if got := svc.Stats().Skipped; got < 3 {
		t.Fatalf("skipped = %d, want ≥3", got)
	}
}

func TestSnapshotStateUnavailableHonestReject(t *testing.T) {
	sender := newCaptureSender()
	svc := newTSSService(t, sender) // no events seen, no lookup seams
	id := testSessionID()
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("cold", SubTypeSnapshot, "EUR/USD"))
	got := sender.got(id)
	if len(got) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(got))
	}
	if v, _ := got[0].Body.GetInt(TagTradSesStatus); v != TradSesRequestRejected {
		t.Fatalf("cold snapshot must reject, not fabricate — got %d", v)
	}
	if txt, _ := got[0].Body.GetString(TagText); !strings.Contains(txt, "STATE_UNAVAILABLE") {
		t.Fatalf("text = %q", txt)
	}
}

func TestBroadcastVenueOrchestratorSeam(t *testing.T) {
	sender := newCaptureSender()
	svc := newTSSService(t, sender)
	id := testSessionID()
	svc.PublishSessionEvent(SessionLifecycleEvent{State: "OPEN"})
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("s", SubTypeSnapshotUpdates, ""))
	base := len(sender.got(id))

	if n := svc.BroadcastVenue("HALT"); n != 1 {
		t.Fatalf("BroadcastVenue messaged %d sessions, want 1", n)
	}
	got := sender.got(id)
	if len(got) != base+1 {
		t.Fatalf("no venue broadcast delivered, total %d", len(got))
	}
	if v, _ := got[len(got)-1].Body.GetInt(TagTradSesStatus); v != 3 {
		t.Fatalf("HALT → %d, want 3", v)
	}
}

func TestBroadcastLatencyUnder50ms(t *testing.T) {
	sender := newCaptureSender()
	// Fake clock steps 1ms per call: handler entry, TransactTime build,
	// and the final latency read each consume a step — the total must
	// still land well under the 50ms contract.
	var step int64
	now := func() time.Time {
		step++
		return time.Unix(1_700_000_000, 0).Add(time.Duration(step) * time.Millisecond)
	}
	var lastLatency time.Duration
	svc, err := NewSessionStatusService(SessionStatusDeps{
		Sender:  sender,
		Known:   func(string) bool { return true },
		Now:     now,
		Latency: func(_ string, d time.Duration) { lastLatency = d },
	})
	if err != nil {
		t.Fatal(err)
	}
	id := testSessionID()
	svc.PublishSessionEvent(SessionLifecycleEvent{State: "OPEN"})
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("s", SubTypeSnapshotUpdates, ""))

	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "HALTED"})
	if lastLatency <= 0 {
		t.Fatal("latency hook never fired")
	}
	if lastLatency >= 50*time.Millisecond {
		t.Fatalf("broadcast latency %v exceeds 50ms", lastLatency)
	}
	if svc.Stats().LastLatencyNs != lastLatency.Nanoseconds() {
		t.Fatal("LastLatencyNs stat diverged from hook")
	}
}

func TestRapidStateFlapOrderedBroadcasts(t *testing.T) {
	sender := newCaptureSender()
	svc := newTSSService(t, sender)
	id := testSessionID()
	svc.PublishSessionEvent(SessionLifecycleEvent{State: "OPEN"})
	_ = svc.HandleTradingSessionStatusRequest(id,
		newTSSRequest("s", SubTypeSnapshotUpdates, "EUR/USD"))
	base := len(sender.got(id))

	// ACTIVE→HALTED→ACTIVE inside one ingest burst — every transition
	// broadcasts in order (clients see both edges).
	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "HALTED"})
	svc.PublishInstrumentStatus(SecurityStatusEvent{Symbol: "EUR/USD", Status: "ACTIVE"})
	got := sender.got(id)
	if len(got) != base+2 {
		t.Fatalf("flap broadcasts = %d new frames, want 2", len(got)-base)
	}
	if v, _ := got[base].Body.GetInt(TagTradSesStatus); v != 3 {
		t.Fatalf("first flap = %d, want 3", v)
	}
	if v, _ := got[base+1].Body.GetInt(TagTradSesStatus); v != 2 {
		t.Fatalf("second flap = %d, want 2", v)
	}
}
