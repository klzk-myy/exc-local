// Task 23.3.3 tests — premium feed channels, entitlement gate, producers,
// monthly billing.
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"exchange/internal/ledger"
	"exchange/internal/ratelimit"
	"exchange/internal/ws"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Channel registration
// ---------------------------------------------------------------------------

func TestPremiumChannelsRegistered(t *testing.T) {
	cases := []struct {
		raw   string
		typ   string
		class ChannelClass
	}{
		{"premium_l3@EUR/USD", FeedPremiumL3, ClassL3},
		{"depth_full@EUR/USD", FeedDepthFull, ClassL2},
		{"auction@EUR/USD", FeedAuction, ClassNone},
		{"auction@all", FeedAuction, ClassNone},
		{"greeks@EUR/USD", FeedGreeks, ClassNone},
	}
	for _, tc := range cases {
		ch, err := ParseChannel(tc.raw)
		if err != nil {
			t.Fatalf("ParseChannel(%q): %v", tc.raw, err)
		}
		if ch.Type != tc.typ || ch.Class != tc.class {
			t.Fatalf("ParseChannel(%q) = %+v, want type=%s class=%s",
				tc.raw, ch, tc.typ, tc.class)
		}
	}
}

// ---------------------------------------------------------------------------
// Entitlement gate
// ---------------------------------------------------------------------------

type fakeFeedSubStore struct {
	mu     sync.Mutex
	subs   map[string]FeedSubscription // "acct|feed"
	err    error
	marked []markCall
}

type markCall struct {
	id        int64
	renewsAt  time.Time
	journalID int64
}

func (s *fakeFeedSubStore) ActiveSubscription(_ context.Context,
	accountID int64, feed string) (FeedSubscription, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return FeedSubscription{}, false, s.err
	}
	sub, ok := s.subs[fmt.Sprintf("%d|%s", accountID, feed)]
	if ok && sub.Status != FeedStatusActive {
		return FeedSubscription{}, false, nil
	}
	return sub, ok, nil
}

func (s *fakeFeedSubStore) DueSubscriptions(_ context.Context,
	asOf time.Time, limit int) ([]FeedSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FeedSubscription
	for _, sub := range s.subs {
		if sub.Status == FeedStatusActive && !sub.RenewsAt.After(asOf) {
			out = append(out, sub)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeFeedSubStore) MarkBilled(_ context.Context, id int64,
	billedAt, renewsAt time.Time, journalID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked = append(s.marked, markCall{id: id, renewsAt: renewsAt, journalID: journalID})
	return nil
}

func premiumSess(tier string) *ws.Session {
	return &ws.Session{
		Authenticated: true, ViaAPIKey: true, AccountID: 7,
		Tier: tier, Scopes: []string{"read"},
	}
}

func premiumChannel(raw string) Channel {
	ch, err := ParseChannel(raw)
	if err != nil {
		panic(err)
	}
	return ch
}

func TestFeedEntitlement_Gates(t *testing.T) {
	store := &fakeFeedSubStore{subs: map[string]FeedSubscription{
		"7|" + FeedGreeks: {ID: 1, AccountID: 7, Feed: FeedGreeks,
			Status: FeedStatusActive},
	}}
	e := &FeedEntitlements{Store: store}
	ch := premiumChannel("greeks@EUR/USD")

	// Anonymous → deny.
	if ok, _ := e.Check(&ws.Session{}, ch); ok {
		t.Fatal("anonymous session admitted to greeks@")
	}
	// Basic tier → deny (subscription present is not enough).
	if ok, why := e.Check(premiumSess("basic"), ch); ok {
		t.Fatal("basic tier admitted to greeks@")
	} else if why == "" {
		t.Fatal("denial without reason")
	}
	// Standard tier → deny (floor is Professional).
	if ok, _ := e.Check(premiumSess("standard"), ch); ok {
		t.Fatal("standard tier admitted to greeks@")
	}
	// Professional + ACTIVE subscription → admit.
	if ok, why := e.Check(premiumSess("professional"), ch); !ok {
		t.Fatalf("professional+sub denied: %s", why)
	}
	// Institutional + ACTIVE subscription → admit.
	if ok, why := e.Check(premiumSess("institutional"), ch); !ok {
		t.Fatalf("institutional+sub denied: %s", why)
	}
	// Professional tier but NO subscription for the feed → deny.
	if ok, _ := e.Check(premiumSess("professional"),
		premiumChannel("premium_l3@EUR/USD")); ok {
		t.Fatal("no-subscription premium_l3 bind admitted")
	}
	// Cancelled subscription is invisible to ActiveSubscription → deny.
	e2 := &FeedEntitlements{Store: &fakeFeedSubStore{subs: map[string]FeedSubscription{
		"7|" + FeedGreeks: {ID: 2, AccountID: 7, Feed: FeedGreeks,
			Status: FeedStatusCancelled},
	}}}
	if ok, _ := e2.Check(premiumSess("professional"), ch); ok {
		t.Fatal("cancelled/absent subscription admitted")
	}
	// Non-premium channel passes through unharmed.
	if ok, _ := e.Check(&ws.Session{}, premiumChannel("book@EUR/USD")); !ok {
		t.Fatal("public channel gated by FeedEntitlements")
	}
}

func TestFeedEntitlement_FailsClosed(t *testing.T) {
	ch := premiumChannel("depth_full@EUR/USD")
	sess := premiumSess("professional")

	// Nil store → deny (deployment does not sell feeds).
	e := &FeedEntitlements{}
	if ok, _ := e.Check(sess, ch); ok {
		t.Fatal("nil store admitted premium bind")
	}
	// Store error → deny, and the error path is never cached.
	store := &fakeFeedSubStore{err: errors.New("pg down")}
	e = &FeedEntitlements{Store: store}
	if ok, _ := e.Check(sess, ch); ok {
		t.Fatal("store error admitted premium bind")
	}
	// Recovery: store heals → admit (errors are not cached).
	store.mu.Lock()
	store.err = nil
	store.subs = map[string]FeedSubscription{
		"7|" + FeedDepthFull: {ID: 3, AccountID: 7, Feed: FeedDepthFull,
			Status: FeedStatusActive},
	}
	store.mu.Unlock()
	if ok, why := e.Check(sess, ch); !ok {
		t.Fatalf("healed store still denying: %s", why)
	}
}

func TestFeedEntitlement_TierResolver(t *testing.T) {
	store := &fakeFeedSubStore{subs: map[string]FeedSubscription{
		"7|" + FeedAuction: {ID: 4, AccountID: 7, Feed: FeedAuction,
			Status: FeedStatusActive},
	}}
	// JWT session carries no tier → public → deny; a TierResolver
	// override (deployment role mapping) admits.
	sess := &ws.Session{Authenticated: true, AccountID: 7}
	e := &FeedEntitlements{Store: store}
	if ok, _ := e.Check(sess, premiumChannel("auction@EUR/USD")); ok {
		t.Fatal("tier-less JWT session admitted to auction@")
	}
	e.TierResolver = func(*ws.Session) ratelimit.Tier {
		return ratelimit.TierInstitutional
	}
	if ok, why := e.Check(sess, premiumChannel("auction@EUR/USD")); !ok {
		t.Fatalf("resolved institutional denied: %s", why)
	}
}

func TestChainEntitlements_FirstDenyWins(t *testing.T) {
	deny := EntitlementFunc(func(*ws.Session, Channel) (bool, string) {
		return false, "denied by test"
	})
	admit := EntitlementFunc(func(*ws.Session, Channel) (bool, string) {
		return true, ""
	})
	ch := premiumChannel("book@EUR/USD")
	if ok, _ := ChainEntitlements(admit, deny).Check(&ws.Session{}, ch); ok {
		t.Fatal("chain ignored a denial")
	}
	if ok, _ := ChainEntitlements(admit, admit).Check(&ws.Session{}, ch); !ok {
		t.Fatal("all-admit chain denied")
	}
}

// ---------------------------------------------------------------------------
// Producers
// ---------------------------------------------------------------------------

func TestPremiumL3Producer_PublishesWithL3Seq(t *testing.T) {
	em := &emitter{}
	p := NewPremiumL3Producer(nil, em.fn(), nil)
	ev := L3Event{
		Symbol: testSym, OrderID: 42, AccountHash: 7,
		Kind: L3Add, Side: SideBuy,
		Price:    decimal.RequireFromString("1.0850"),
		Quantity: decimal.RequireFromString("1000000"),
		QtyDelta: decimal.RequireFromString("1000000"),
		Seq:      1234,
		Ts:       time.UnixMilli(1000),
		WalSeq:   99,
	}
	p.Publish(ev)
	got := em.all()
	if len(got) != 1 || got[0].Channel != "premium_l3@"+testSym {
		t.Fatalf("emitted %+v", got)
	}
	if got[0].Seq != 1234 {
		t.Fatalf("channel seq = %d, want l3_seq 1234", got[0].Seq)
	}
	m := payload(t, got[0])
	if m["event"] != "ORDER_ADD" || m["l3_seq"] != float64(1234) ||
		m["price"] != "1.085" {
		t.Fatalf("payload wrong: %v", m)
	}
	// Unrouted events never reach an untyped channel.
	p.Publish(L3Event{Seq: 5})
	if len(em.all()) != 1 {
		t.Fatal("symbol-less event published")
	}
}

func TestFullDepthProducer_AllLevels(t *testing.T) {
	em := &emitter{}
	p := NewFullDepthProducer(nil, em.fn(), nil, nil)
	var bids, asks []Level
	for i := 0; i < 25; i++ {
		bids = append(bids, Level{Price: int64(108500000 - i*10), Qty: 1e8, Count: 2})
		asks = append(asks, Level{Price: int64(108510000 + i*10), Qty: 1e8, Count: 3})
	}
	p.Push(BookDelta{Symbol: testSym, Bids: bids, Asks: asks, EngineSeq: 9})
	got := em.all()
	if len(got) != 1 || got[0].Channel != "depth_full@"+testSym {
		t.Fatalf("emitted %+v", got)
	}
	m := payload(t, got[0])
	if n := len(m["bids"].([]any)); n != 25 {
		t.Fatalf("bids levels = %d, want 25 (full depth, no top-N cap)", n)
	}
	if n := len(m["asks"].([]any)); n != 25 {
		t.Fatalf("asks levels = %d, want 25", n)
	}
	if m["engine_seq"] != float64(9) {
		t.Fatalf("engine_seq missing: %v", m)
	}
	// Second delta chains prev_last_seq per §10.9.
	p.Push(BookDelta{Symbol: testSym, Bids: bids[:5], Asks: asks[:5], EngineSeq: 10})
	got = em.all()
	m2 := payload(t, got[1])
	if m2["prev_last_seq"] != float64(got[0].Seq) {
		t.Fatalf("prev_last_seq = %v, want %d", m2["prev_last_seq"], got[0].Seq)
	}
}

func TestAuctionsProducer_FiltersAndGates(t *testing.T) {
	em := &emitter{}
	p := NewAuctionsProducer(LiquidationsProducerConfig{}, nil, em.fn())
	// The §24 #263 floor is enforced — a premium feed never loosens it.
	if p.gate.delay != LiquidationDelay {
		t.Fatalf("auction delay = %v, floor %v must apply",
			p.gate.delay, LiquidationDelay)
	}
	// publish() is the post-gate emit path: non-auction events drop.
	p.publish(LiquidationEvent{Symbol: testSym, Side: "SELL",
		Price: "1.1", Qty: "5", IsAuction: false, Ts: time.Now()})
	if len(em.all()) != 0 {
		t.Fatal("non-auction event leaked to the auction feed")
	}
	p.publish(LiquidationEvent{Symbol: testSym, Side: "SELL",
		Price: "1.1", Qty: "5", IsAuction: true, Ts: time.Now()})
	got := em.all()
	if len(got) != 2 {
		t.Fatalf("emitted %d, want 2 (auction@{sym} + auction@all)", len(got))
	}
	sym := forChannel(got, "auction@"+testSym)
	all := forChannel(got, "auction@all")
	if len(sym) != 1 || len(all) != 1 {
		t.Fatalf("sym=%d all=%d", len(sym), len(all))
	}
	m := payload(t, sym[0])
	if m["event"] != "auction" || m["side"] != "SELL" || m["price"] != "1.1" {
		t.Fatalf("payload wrong: %v", m)
	}
}

// ---------------------------------------------------------------------------
// Billing
// ---------------------------------------------------------------------------

type fakePoster struct {
	mu       sync.Mutex
	journals []ledger.Journal
	err      error
}

func (p *fakePoster) Post(_ context.Context, j ledger.Journal) (ledger.PostResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return ledger.PostResult{}, p.err
	}
	p.journals = append(p.journals, j)
	return ledger.PostResult{JournalID: int64(100 + len(p.journals)), Committed: true}, nil
}

func TestPremiumBiller_PostsBalancedFeeJournal(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	renew := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeFeedSubStore{subs: map[string]FeedSubscription{
		"7|" + FeedGreeks: {ID: 11, AccountID: 7, Feed: FeedGreeks,
			Status: FeedStatusActive, BilledMonthlyMinor: 25000,
			Currency: "USD", RenewsAt: renew},
	}}
	poster := &fakePoster{}
	var events []PremiumFeedBillingEvent
	b := NewPremiumFeedBiller(store, poster,
		BillingEventSinkFunc(func(_ context.Context, ev PremiumFeedBillingEvent) error {
			events = append(events, ev)
			return nil
		}), func() time.Time { return now }, nil)

	n, err := b.BillDue(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("BillDue = %d, %v — want 1, nil", n, err)
	}
	if len(poster.journals) != 1 {
		t.Fatalf("posted %d journals, want 1", len(poster.journals))
	}
	j := poster.journals[0]
	if j.EntryType != ledger.EntryFee {
		t.Fatalf("entry type %q, want FEE", j.EntryType)
	}
	if j.IdempotencyKey != "premium-feed:11:2026-10" {
		t.Fatalf("idempotency key %q", j.IdempotencyKey)
	}
	if len(j.Lines) != 2 || !j.Lines[0].Debit.Equal(decimal.NewFromInt(250)) ||
		!j.Lines[1].Credit.Equal(decimal.NewFromInt(250)) {
		t.Fatalf("unbalanced/wrong lines: %+v", j.Lines)
	}
	if j.Lines[0].AccountCode != ledger.CustomerLiability("USD") ||
		j.Lines[1].AccountCode != ledger.CommissionRevenue("USD") {
		t.Fatalf("wrong GL accounts: %+v", j.Lines)
	}
	if len(j.Effects) != 1 || j.Effects[0].AccountID != 7 ||
		!j.Effects[0].AvailableDelta.Equal(decimal.NewFromInt(-250)) {
		t.Fatalf("wallet effect wrong: %+v", j.Effects)
	}
	// renews_at rolls one month from the boundary (no drift), journal linked.
	if len(store.marked) != 1 || !store.marked[0].renewsAt.Equal(
		renew.AddDate(0, 1, 0)) || store.marked[0].journalID != 101 {
		t.Fatalf("MarkBilled = %+v", store.marked)
	}
	if len(events) != 1 || events[0].Event != "premium_feed_billing" ||
		events[0].Amount != "250" || events[0].JournalID != 101 {
		t.Fatalf("billing event wrong: %+v", events)
	}
}

func TestPremiumBiller_FailsClosedWithoutPoster(t *testing.T) {
	b := NewPremiumFeedBiller(&fakeFeedSubStore{}, nil, nil, nil, nil)
	if _, err := b.BillDue(context.Background()); err == nil {
		t.Fatal("nil poster billed")
	}
}

func TestPremiumBiller_PostFailureKeepsRowUnbilled(t *testing.T) {
	renew := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeFeedSubStore{subs: map[string]FeedSubscription{
		"7|" + FeedAuction: {ID: 12, AccountID: 7, Feed: FeedAuction,
			Status: FeedStatusActive, BilledMonthlyMinor: 9900,
			Currency: "USD", RenewsAt: renew},
	}}
	poster := &fakePoster{err: errors.New("INSUFFICIENT_BALANCE")}
	b := NewPremiumFeedBiller(store, poster, nil, nil, nil)
	n, err := b.BillDue(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("BillDue = %d, %v — want 0, nil (per-row failure)", n, err)
	}
	if len(store.marked) != 0 {
		t.Fatal("failed charge marked billed")
	}
}

func TestCcyExponent(t *testing.T) {
	if ccyExponent("JPY") != 0 || ccyExponent("BHD") != 3 ||
		ccyExponent("USD") != 2 || ccyExponent("eur") != 2 {
		t.Fatal("minor-unit exponents wrong")
	}
}
