// Task 18.3.3 tests — MarketDataRequest handling, subscription
// management, snapshot/incremental encoding, rejection paths.
package fix

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/marketdata"
)

// captureSender records outbound messages per session id string.
type captureSender struct {
	mu   sync.Mutex
	msgs map[string][]*quickfix.Message
	err  error
}

func newCaptureSender() *captureSender {
	return &captureSender{msgs: map[string][]*quickfix.Message{}}
}

func (c *captureSender) SendTo(m *quickfix.Message, id quickfix.SessionID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.msgs[id.String()] = append(c.msgs[id.String()], m)
	return nil
}

func (c *captureSender) got(id quickfix.SessionID) []*quickfix.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*quickfix.Message(nil), c.msgs[id.String()]...)
}

func mdataSessionID() quickfix.SessionID {
	return quickfix.SessionID{
		BeginString:  "FIX.4.4",
		SenderCompID: "EXC",
		TargetCompID: "CLIENT1",
	}
}

func lvl(px, qty int64) marketdata.Level {
	return marketdata.Level{Price: px, Qty: qty, Count: 1}
}

func newMDRequest(reqID string, subType int, symbols ...string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgMarketDataRequest)
	m.Body.SetString(TagMDReqID, reqID)
	m.Body.SetInt(TagSubscriptionRequestType, subType)
	grp := quickfix.NewRepeatingGroup(TagNoRelatedSym, quickfix.GroupTemplate{
		quickfix.GroupElement(TagSymbol),
	})
	for _, s := range symbols {
		g := grp.Add()
		g.SetString(TagSymbol, s)
	}
	m.Body.SetGroup(grp)
	return m
}

type mdEnt struct {
	action, etype, symbol, px, qty string
	pos                            int
}

// readEntries parses the NoMDEntries group of a W or X message — the
// group delimiter differs per message type (269 on W, 279 on X).
func readEntries(t *testing.T, m *quickfix.Message) []mdEnt {
	t.Helper()
	mt := mustMsgType(t, m)
	var tmpl quickfix.GroupTemplate
	if mt == "X" {
		tmpl = quickfix.GroupTemplate{
			quickfix.GroupElement(TagMDUpdateAction),
			quickfix.GroupElement(TagMDEntryType),
			quickfix.GroupElement(TagSymbol),
			quickfix.GroupElement(TagMDEntryPx),
			quickfix.GroupElement(TagMDEntrySize),
			quickfix.GroupElement(TagMDEntryPositionNo),
		}
	} else {
		tmpl = quickfix.GroupTemplate{
			quickfix.GroupElement(TagMDEntryType),
			quickfix.GroupElement(TagMDEntryPx),
			quickfix.GroupElement(TagMDEntrySize),
			quickfix.GroupElement(TagMDEntryPositionNo),
		}
	}
	grp := quickfix.NewRepeatingGroup(TagNoMDEntries, tmpl)
	if err := m.Body.GetGroup(grp); err != nil {
		t.Fatalf("NoMDEntries group: %v", err)
	}
	out := make([]mdEnt, 0, grp.Len())
	for i := 0; i < grp.Len(); i++ {
		g := grp.Get(i)
		e := mdEnt{action: "-"}
		if g.Has(TagMDUpdateAction) {
			if v, err := g.GetInt(TagMDUpdateAction); err == nil {
				e.action = string(rune('0' + v))
			}
		}
		if v, err := g.GetString(TagMDEntryType); err == nil {
			e.etype = v
		}
		if v, err := g.GetString(TagSymbol); err == nil {
			e.symbol = v
		}
		if v, err := g.GetString(TagMDEntryPx); err == nil {
			e.px = v
		}
		if v, err := g.GetString(TagMDEntrySize); err == nil {
			e.qty = v
		}
		if v, err := g.GetInt(TagMDEntryPositionNo); err == nil {
			e.pos = v
		}
		out = append(out, e)
	}
	return out
}

func mustMsgType(t *testing.T, m *quickfix.Message) string {
	t.Helper()
	mt, err := m.MsgType()
	if err != nil {
		t.Fatalf("MsgType: %v", err)
	}
	return mt
}

func newMDService(t *testing.T, sender *captureSender, known map[string]bool) *MarketDataService {
	t.Helper()
	svc, err := NewMarketDataService(MarketDataDeps{
		Sender: sender,
		Known:  func(s string) bool { return known[s] },
		Depth:  20,
	})
	if err != nil {
		t.Fatalf("NewMarketDataService: %v", err)
	}
	return svc
}

func TestSubscribeSnapshotThenIncremental(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()

	svc.PushDelta(marketdata.BookDelta{
		Symbol: "EUR/USD",
		Bids:   []marketdata.Level{lvl(110000000, 500000000)}, // 1.10 @ 5.0
		Asks:   []marketdata.Level{lvl(110010000, 300000000)},
	})

	if rej := svc.HandleMarketDataRequest(id,
		newMDRequest("md-1", SubTypeSnapshotUpdates, "EUR/USD")); rej != nil {
		t.Fatalf("subscribe rejected: %v", rej)
	}
	got := sender.got(id)
	if len(got) != 1 || mustMsgType(t, got[0]) != "W" {
		t.Fatalf("expected one W snapshot, got %v", got)
	}
	if v, _ := got[0].Body.GetString(TagMDReqID); v != "md-1" {
		t.Fatalf("MDReqID = %q", v)
	}
	if v, _ := got[0].Body.GetString(TagSymbol); v != "EUR/USD" {
		t.Fatalf("Symbol = %q", v)
	}
	entries := readEntries(t, got[0])
	if len(entries) != 2 {
		t.Fatalf("snapshot entries = %d, want 2", len(entries))
	}
	if entries[0].etype != "0" || entries[0].px != "1.1" || entries[0].qty != "5" {
		t.Fatalf("bid entry = %+v", entries[0])
	}
	if entries[1].etype != "1" {
		t.Fatalf("offer entry = %+v", entries[1])
	}

	// Book change: bid qty changes at pos 1 (Change), ask price moves
	// (New), a second ask level appears (New).
	svc.PushDelta(marketdata.BookDelta{
		Symbol: "EUR/USD",
		Bids:   []marketdata.Level{lvl(110000000, 700000000)},
		Asks:   []marketdata.Level{lvl(110020000, 300000000), lvl(110030000, 900000000)},
	})
	got = sender.got(id)
	if len(got) != 2 || mustMsgType(t, got[1]) != "X" {
		t.Fatalf("expected X incremental, got %v", got)
	}
	if v, _ := got[1].Body.GetString(TagMDReqID); v != "md-1" {
		t.Fatalf("incremental MDReqID = %q", v)
	}
	entries = readEntries(t, got[1])
	if len(entries) != 3 {
		t.Fatalf("incremental entries = %d, want 3: %+v", len(entries), entries)
	}
	if entries[0].action != "1" || entries[0].etype != "0" || entries[0].pos != 1 {
		t.Fatalf("bid change entry = %+v", entries[0])
	}
	if entries[1].action != "0" || entries[1].etype != "1" || entries[1].pos != 1 {
		t.Fatalf("offer new entry = %+v", entries[1])
	}
	if entries[2].action != "0" || entries[2].etype != "1" || entries[2].pos != 2 {
		t.Fatalf("offer pos2 entry = %+v", entries[2])
	}
}

func TestIncrementalBookShrinkEmitsDeletes(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()

	svc.PushDelta(marketdata.BookDelta{
		Symbol: "EUR/USD",
		Bids:   []marketdata.Level{lvl(1, 1), lvl(2, 1)},
		Asks:   []marketdata.Level{lvl(3, 1), lvl(4, 1)},
	})
	_ = svc.HandleMarketDataRequest(id, newMDRequest("md-1", SubTypeSnapshotUpdates, "EUR/USD"))

	svc.PushDelta(marketdata.BookDelta{
		Symbol: "EUR/USD",
		Bids:   []marketdata.Level{lvl(1, 1)},
		Asks:   []marketdata.Level{},
	})
	got := sender.got(id)
	if len(got) != 2 || mustMsgType(t, got[1]) != "X" {
		t.Fatalf("expected X after shrink, got %v", got)
	}
	entries := readEntries(t, got[1])
	var deletes int
	for _, e := range entries {
		if e.action == "2" {
			deletes++
		}
	}
	// pos2 bid deletes; both ask positions delete.
	if deletes != 3 {
		t.Fatalf("deletes = %d, want 3 (%+v)", deletes, entries)
	}
	if entries[0].action != "2" || entries[0].etype != "0" || entries[0].pos != 2 {
		t.Fatalf("bid delete = %+v", entries[0])
	}
}

func TestUnknownSymbolRejected(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()

	if rej := svc.HandleMarketDataRequest(id,
		newMDRequest("md-9", SubTypeSnapshotUpdates, "ZZZ/XXX")); rej != nil {
		t.Fatalf("session reject unexpected: %v", rej)
	}
	got := sender.got(id)
	if len(got) != 1 || mustMsgType(t, got[0]) != "Y" {
		t.Fatalf("expected 35=Y reject, got %v", got)
	}
	if v, _ := got[0].Body.GetInt(TagMDReqRejReason); v != MDRejUnknownSymbol {
		t.Fatalf("rej reason = %d, want %d", v, MDRejUnknownSymbol)
	}
	if txt, _ := got[0].Body.GetString(TagText); !strings.Contains(txt, "ZZZ/XXX") {
		t.Fatalf("reject text missing symbol: %q", txt)
	}
	if svc.SubscriptionCount(id) != 0 {
		t.Fatal("no subscription should register on reject")
	}
}

func TestDuplicateMDReqIDRejected(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()

	_ = svc.HandleMarketDataRequest(id, newMDRequest("dup", SubTypeSnapshotUpdates, "EUR/USD"))
	_ = svc.HandleMarketDataRequest(id, newMDRequest("dup", SubTypeSnapshotUpdates, "EUR/USD"))
	got := sender.got(id)
	if len(got) != 2 || mustMsgType(t, got[1]) != "Y" {
		t.Fatalf("expected 35=Y dup reject, got %v", got)
	}
	if v, _ := got[1].Body.GetInt(TagMDReqRejReason); v != MDRejDuplicateMDReqID {
		t.Fatalf("rej reason = %d", v)
	}
	if svc.SubscriptionCount(id) != 1 {
		t.Fatalf("subs = %d, want 1", svc.SubscriptionCount(id))
	}
}

func TestMaxSubscriptionsHonored(t *testing.T) {
	sender := newCaptureSender()
	svc, err := NewMarketDataService(MarketDataDeps{
		Sender:           sender,
		Known:            func(string) bool { return true },
		MaxSubscriptions: 1,
		Depth:            20,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := mdataSessionID()
	_ = svc.HandleMarketDataRequest(id, newMDRequest("a", SubTypeSnapshotUpdates, "EUR/USD"))
	_ = svc.HandleMarketDataRequest(id, newMDRequest("b", SubTypeSnapshotUpdates, "GBP/USD"))
	got := sender.got(id)
	if len(got) != 2 || mustMsgType(t, got[1]) != "Y" {
		t.Fatalf("expected 35=Y cap reject, got %v", got)
	}
	if txt, _ := got[1].Body.GetString(TagText); !strings.Contains(txt, "MAX_SUBSCRIPTIONS") {
		t.Fatalf("cap reject text = %q", txt)
	}
}

func TestUnsubscribeRemoves(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()

	svc.PushDelta(marketdata.BookDelta{Symbol: "EUR/USD", Bids: []marketdata.Level{lvl(1, 1)}})
	_ = svc.HandleMarketDataRequest(id, newMDRequest("md-1", SubTypeSnapshotUpdates, "EUR/USD"))
	// Unknown-reqID unsubscribe is an idempotent no-op.
	_ = svc.HandleMarketDataRequest(id, newMDRequest("ghost", SubTypeDisable, "EUR/USD"))
	if svc.SubscriptionCount(id) != 1 {
		t.Fatal("idempotent unsubscribe must not remove live sub")
	}
	_ = svc.HandleMarketDataRequest(id, newMDRequest("md-1", SubTypeDisable, "EUR/USD"))
	if svc.SubscriptionCount(id) != 0 {
		t.Fatal("unsubscribe must remove the subscription")
	}
	svc.PushDelta(marketdata.BookDelta{Symbol: "EUR/USD", Bids: []marketdata.Level{lvl(1, 2)}})
	if n := len(sender.got(id)); n != 1 {
		t.Fatalf("no frames after unsubscribe; got %d", n)
	}
}

func TestSnapshotOnlyLeavesNoSubscription(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()
	svc.PushDelta(marketdata.BookDelta{Symbol: "EUR/USD", Bids: []marketdata.Level{lvl(1, 1)}})

	if rej := svc.HandleMarketDataRequest(id,
		newMDRequest("snap", SubTypeSnapshot, "EUR/USD")); rej != nil {
		t.Fatal(rej)
	}
	got := sender.got(id)
	if len(got) != 1 || mustMsgType(t, got[0]) != "W" {
		t.Fatalf("expected W snapshot, got %v", got)
	}
	if svc.SubscriptionCount(id) != 0 {
		t.Fatal("snapshot request must not register a subscription")
	}
	svc.PushDelta(marketdata.BookDelta{Symbol: "EUR/USD", Bids: []marketdata.Level{lvl(1, 2)}})
	if n := len(sender.got(id)); n != 1 {
		t.Fatalf("snapshot-only client received incremental; total %d", n)
	}
}

func TestEntitlementDenied(t *testing.T) {
	sender := newCaptureSender()
	svc, err := NewMarketDataService(MarketDataDeps{
		Sender:   sender,
		Known:    func(string) bool { return true },
		Entitled: func(_ string, sym string) bool { return sym != "USD/JPY" },
		Depth:    20,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := mdataSessionID()
	_ = svc.HandleMarketDataRequest(id, newMDRequest("r", SubTypeSnapshotUpdates, "USD/JPY"))
	got := sender.got(id)
	if len(got) != 1 || mustMsgType(t, got[0]) != "Y" {
		t.Fatalf("expected 35=Y entitlement reject, got %v", got)
	}
	if v, _ := got[0].Body.GetInt(TagMDReqRejReason); v != MDRejInsufficientPermission {
		t.Fatalf("rej reason = %d", v)
	}
}

func TestFullRefreshUpdateTypeEmitsW(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()
	svc.PushDelta(marketdata.BookDelta{Symbol: "EUR/USD", Bids: []marketdata.Level{lvl(1, 1)}})

	m := newMDRequest("md-fr", SubTypeSnapshotUpdates, "EUR/USD")
	m.Body.SetInt(TagMDUpdateType, MDUpdateFullRefresh)
	_ = svc.HandleMarketDataRequest(id, m)

	svc.PushDelta(marketdata.BookDelta{Symbol: "EUR/USD", Bids: []marketdata.Level{lvl(1, 2)}})
	got := sender.got(id)
	if len(got) != 2 || mustMsgType(t, got[1]) != "W" {
		t.Fatalf("265=0 must emit full W per change; got %v", got)
	}
}

func TestMalformedRequestsSessionReject(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()

	// Missing MDReqID(262) → session-layer reject.
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgMarketDataRequest)
	m.Body.SetInt(TagSubscriptionRequestType, SubTypeSnapshotUpdates)
	if rej := svc.HandleMarketDataRequest(id, m); rej == nil {
		t.Fatal("missing MDReqID must return MessageRejectError")
	}

	// Missing NoRelatedSym(146) → session-layer reject.
	m2 := quickfix.NewMessage()
	m2.Header.SetString(TagMsgType, MsgMarketDataRequest)
	m2.Body.SetString(TagMDReqID, "x")
	m2.Body.SetInt(TagSubscriptionRequestType, SubTypeSnapshotUpdates)
	if rej := svc.HandleMarketDataRequest(id, m2); rej == nil {
		t.Fatal("missing NoRelatedSym must return MessageRejectError")
	}
}

func TestUnsupportedMDUpdateTypeRejected(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()

	m := newMDRequest("md-u", SubTypeSnapshotUpdates, "EUR/USD")
	m.Body.SetInt(TagMDUpdateType, 2) // FIX 4.4 defines 0/1 only
	if rej := svc.HandleMarketDataRequest(id, m); rej != nil {
		t.Fatal(rej)
	}
	got := sender.got(id)
	if len(got) != 1 || mustMsgType(t, got[0]) != "Y" {
		t.Fatalf("expected 35=Y, got %v", got)
	}
	if v, _ := got[0].Body.GetInt(TagMDReqRejReason); v != MDRejUnsupportedMDUpdate {
		t.Fatalf("rej reason = %d", v)
	}
}

func TestEntryTypeFilter(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()
	svc.PushDelta(marketdata.BookDelta{
		Symbol: "EUR/USD",
		Bids:   []marketdata.Level{lvl(1, 1)},
		Asks:   []marketdata.Level{lvl(2, 1)},
	})
	m := newMDRequest("md-et", SubTypeSnapshotUpdates, "EUR/USD")
	et := quickfix.NewRepeatingGroup(TagNoMDEntryTypes, quickfix.GroupTemplate{
		quickfix.GroupElement(TagMDEntryType),
	})
	g := et.Add()
	g.SetString(TagMDEntryType, "1") // offers only
	m.Body.SetGroup(et)
	_ = svc.HandleMarketDataRequest(id, m)

	got := sender.got(id)
	if len(got) != 1 {
		t.Fatalf("expected W, got %v", got)
	}
	entries := readEntries(t, got[0])
	if len(entries) != 1 || entries[0].etype != "1" {
		t.Fatalf("expected offers-only entries, got %+v", entries)
	}
}

func TestDropSessionPurgesSubscriptions(t *testing.T) {
	sender := newCaptureSender()
	svc := newMDService(t, sender, map[string]bool{"EUR/USD": true})
	id := mdataSessionID()
	svc.PushDelta(marketdata.BookDelta{Symbol: "EUR/USD", Bids: []marketdata.Level{lvl(1, 1)}})
	_ = svc.HandleMarketDataRequest(id, newMDRequest("md-1", SubTypeSnapshotUpdates, "EUR/USD"))
	svc.DropSession(id)
	if svc.SubscriptionCount(id) != 0 {
		t.Fatal("DropSession must purge")
	}
	svc.PushDelta(marketdata.BookDelta{Symbol: "EUR/USD", Bids: []marketdata.Level{lvl(1, 2)}})
	if n := len(sender.got(id)); n != 1 {
		t.Fatalf("dropped session still receiving; total %d", n)
	}
}

func TestRunConsumesDeltaSource(t *testing.T) {
	sender := newCaptureSender()
	src := marketdata.DeltaSourceFunc(func(ctx context.Context) (<-chan marketdata.BookDelta, error) {
		ch := make(chan marketdata.BookDelta, 2)
		go func() {
			defer close(ch)
			ch <- marketdata.BookDelta{Symbol: "EUR/USD", Bids: []marketdata.Level{lvl(1, 1)}}
			<-ctx.Done()
		}()
		return ch, nil
	})
	svc, err := NewMarketDataService(MarketDataDeps{
		Sender:      sender,
		DeltaSource: src,
		Known:       func(s string) bool { return s == "EUR/USD" },
		Depth:       20,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = svc.Run(ctx) }()
	id := mdataSessionID()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = svc.HandleMarketDataRequest(id, newMDRequest("md-r", SubTypeSnapshotUpdates, "EUR/USD"))
		if n := len(sender.got(id)); n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(sender.got(id)); n < 1 {
		t.Fatal("source-driven snapshot never arrived")
	}
}
