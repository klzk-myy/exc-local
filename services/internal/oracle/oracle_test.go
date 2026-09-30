package oracle

import (
	"context"
	"sync"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// fakeFeed is the scripted test feed — same contract as feeds.SimFeed
// without importing the adapter package (unit tests exercise the
// service, not the transport).
type fakeFeed struct {
	name   string
	quotes map[string]Quote
	err    error
}

func (f *fakeFeed) Name() string { return f.name }
func (f *fakeFeed) Poll(_ context.Context, syms []string) ([]Quote, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Quote, 0, len(syms))
	for _, s := range syms {
		if q, ok := f.quotes[s]; ok {
			out = append(out, q)
		}
	}
	return out, nil
}

func fq(feed, sym, px string, ts time.Time) Quote {
	return Quote{Symbol: sym, Mid: decimal.RequireFromString(px),
		Weight: decimal.NewFromInt(1), Ts: ts, Feed: feed}
}

// fakePub captures every published mark/health/fallback — the
// Publisher seam keeps the service unit-testable without Redis.
type fakePub struct {
	mu        sync.Mutex
	marks     []MarkResult
	health    map[string]HealthState
	fallbacks []*StaleReference
	markErr   error
}

func newFakePub() *fakePub {
	return &fakePub{health: map[string]HealthState{}}
}
func (p *fakePub) PublishMark(_ context.Context, r MarkResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.markErr != nil {
		return p.markErr
	}
	p.marks = append(p.marks, r)
	return nil
}
func (p *fakePub) PublishHealth(_ context.Context, sym string, st HealthState, _ float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health[sym] = st
	return nil
}
func (p *fakePub) PublishFallback(_ context.Context, ref *StaleReference) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fallbacks = append(p.fallbacks, ref)
	return nil
}

func newTestService(t *testing.T, fd []Feed, syms []string,
	pub Publisher, now func() time.Time) *Service {
	t.Helper()
	s, err := NewService(Options{Feeds: fd, Symbols: syms, Publisher: pub, Now: now})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func TestServiceRequiresTwoFeeds(t *testing.T) {
	pub := newFakePub()
	if _, err := NewService(Options{
		Feeds: []Feed{&fakeFeed{name: "a"}}, Symbols: []string{"EUR/USD"},
		Publisher: pub,
	}); err == nil {
		t.Fatal("single-feed oracle must refuse to start (fail closed)")
	}
	if _, err := NewService(Options{
		Feeds:   []Feed{&fakeFeed{name: "a"}, &fakeFeed{name: "a"}},
		Symbols: []string{"EUR/USD"}, Publisher: pub,
	}); err == nil {
		t.Fatal("duplicate feed names must be rejected")
	}
	if _, err := NewService(Options{
		Feeds:   []Feed{&fakeFeed{name: "a"}, &fakeFeed{name: "b"}},
		Symbols: []string{"EUR/USD"},
	}); err == nil {
		t.Fatal("nil publisher must refuse — no silent marks")
	}
}

func TestMedianOfTwoFeeds(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	pub := newFakePub()
	s := newTestService(t, []Feed{
		&fakeFeed{name: "a", quotes: map[string]Quote{"EUR/USD": fq("a", "EUR/USD", "1.1000", now)}},
		&fakeFeed{name: "b", quotes: map[string]Quote{"EUR/USD": fq("b", "EUR/USD", "1.1002", now)}},
	}, []string{"EUR/USD"}, pub, clock)

	res := s.TickOnce(context.Background())
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("expected 1 OK result, got %+v", res)
	}
	want := decimal.RequireFromString("1.1001") // median of even cohort = midpoint
	if !res[0].Mark.Equal(want) {
		t.Fatalf("mark %s, want median %s", res[0].Mark, want)
	}
	if !res[0].Index.Equal(want) {
		t.Fatalf("equal-weight index %s, want %s", res[0].Index, want)
	}
	if len(pub.marks) != 1 {
		t.Fatalf("expected 1 published mark, got %d", len(pub.marks))
	}
	if pub.health["EUR/USD"] != HealthOK {
		t.Fatalf("health %s, want OK", pub.health["EUR/USD"])
	}
}

func TestIndexVolumeWeighted(t *testing.T) {
	now := time.Now()
	pub := newFakePub()
	q1 := fq("a", "EUR/USD", "1.1000", now)
	q1.Weight = decimal.NewFromInt(3)
	q2 := fq("b", "EUR/USD", "1.1004", now)
	q2.Weight = decimal.NewFromInt(1)
	s := newTestService(t, []Feed{
		&fakeFeed{name: "a", quotes: map[string]Quote{"EUR/USD": q1}},
		&fakeFeed{name: "b", quotes: map[string]Quote{"EUR/USD": q2}},
	}, []string{"EUR/USD"}, pub, func() time.Time { return now })

	res := s.TickOnce(context.Background())
	// VWAP = (1.1000*3 + 1.1004*1) / 4 = 1.1001
	want := decimal.RequireFromString("1.1001")
	if !res[0].Index.Equal(want) {
		t.Fatalf("index %s, want VWAP %s", res[0].Index, want)
	}
	if !res[0].Mark.Equal(decimal.RequireFromString("1.1002")) {
		t.Fatalf("mark %s, want median midpoint 1.1002", res[0].Mark)
	}
}

func TestStalenessGate(t *testing.T) {
	now := time.Now()
	pub := newFakePub()
	stale := now.Add(-StaleAfter - time.Second)
	s := newTestService(t, []Feed{
		&fakeFeed{name: "a", quotes: map[string]Quote{"EUR/USD": fq("a", "EUR/USD", "1.1000", stale)}},
		&fakeFeed{name: "b", quotes: map[string]Quote{"EUR/USD": fq("b", "EUR/USD", "1.1002", now)}},
	}, []string{"EUR/USD"}, pub, func() time.Time { return now })

	res := s.TickOnce(context.Background())
	if res[0].OK {
		t.Fatal("1 fresh feed must not produce a mark (fail closed)")
	}
	if len(res[0].Stale) != 1 || res[0].Stale[0] != "a" {
		t.Fatalf("stale feed 'a' not attributed: %+v", res[0].Stale)
	}
	if len(pub.marks) != 0 {
		t.Fatal("no mark may publish with <2 fresh feeds")
	}
	if pub.health["EUR/USD"] != HealthDegraded {
		t.Fatalf("health %s, want DEGRADED (1 fresh feed)", pub.health["EUR/USD"])
	}
	if got := s.Health(context.Background()); got != HealthDegraded {
		t.Fatalf("service health %s, want DEGRADED", got)
	}
}

func TestAllStaleUnavailable(t *testing.T) {
	now := time.Now()
	pub := newFakePub()
	stale := now.Add(-2 * StaleAfter)
	s := newTestService(t, []Feed{
		&fakeFeed{name: "a", quotes: map[string]Quote{"EUR/USD": fq("a", "EUR/USD", "1.1000", stale)}},
		&fakeFeed{name: "b", quotes: map[string]Quote{"EUR/USD": fq("b", "EUR/USD", "1.1002", stale)}},
	}, []string{"EUR/USD"}, pub, func() time.Time { return now })

	res := s.TickOnce(context.Background())
	if res[0].OK || len(pub.marks) != 0 {
		t.Fatal("all-stale cohort must publish nothing")
	}
	if pub.health["EUR/USD"] != HealthUnavailable {
		t.Fatalf("health %s, want UNAVAILABLE", pub.health["EUR/USD"])
	}
	if s.Health(context.Background()) != HealthUnavailable {
		t.Fatal("service health must read UNAVAILABLE")
	}
}

func TestDivergenceExclusion(t *testing.T) {
	now := time.Now()
	pub := newFakePub()
	s := newTestService(t, []Feed{
		&fakeFeed{name: "a", quotes: map[string]Quote{"EUR/USD": fq("a", "EUR/USD", "1.1000", now)}},
		&fakeFeed{name: "b", quotes: map[string]Quote{"EUR/USD": fq("b", "EUR/USD", "1.1001", now)}},
		// c diverges +50bps — dropped, mark computes from a+b.
		&fakeFeed{name: "c", quotes: map[string]Quote{"EUR/USD": fq("c", "EUR/USD", "1.1055", now)}},
	}, []string{"EUR/USD"}, pub, func() time.Time { return now })

	res := s.TickOnce(context.Background())
	if !res[0].OK {
		t.Fatal("coherent a+b cohort must still produce a mark")
	}
	if len(res[0].Divergent) != 1 || res[0].Divergent[0] != "c" {
		t.Fatalf("divergent feed 'c' not attributed: %+v", res[0].Divergent)
	}
	if !res[0].Mark.Equal(decimal.RequireFromString("1.10005")) {
		t.Fatalf("mark %s should exclude the divergent quote", res[0].Mark)
	}
	if pub.health["EUR/USD"] != HealthDegraded {
		t.Fatalf("divergent exclusion must degrade health, got %s", pub.health["EUR/USD"])
	}
}

func TestNonPositiveQuoteRejected(t *testing.T) {
	now := time.Now()
	pub := newFakePub()
	bad := fq("b", "EUR/USD", "0", now)
	s := newTestService(t, []Feed{
		&fakeFeed{name: "a", quotes: map[string]Quote{"EUR/USD": fq("a", "EUR/USD", "1.1000", now)}},
		&fakeFeed{name: "b", quotes: map[string]Quote{"EUR/USD": bad}},
	}, []string{"EUR/USD"}, pub, func() time.Time { return now })

	res := s.TickOnce(context.Background())
	if res[0].OK {
		t.Fatal("zero-price quote must not count toward the ≥2 floor")
	}
}

func TestPerSymbolFreshness(t *testing.T) {
	now := time.Now()
	pub := newFakePub()
	// feed a covers EUR/USD + USD/TRY; feed b covers EUR/USD only —
	// USD/TRY has ONE fresh source and must fail closed.
	s := newTestService(t, []Feed{
		&fakeFeed{name: "a", quotes: map[string]Quote{
			"EUR/USD": fq("a", "EUR/USD", "1.1000", now),
			"USD/TRY": fq("a", "USD/TRY", "34.0", now),
		}},
		&fakeFeed{name: "b", quotes: map[string]Quote{
			"EUR/USD": fq("b", "EUR/USD", "1.1002", now),
		}},
	}, []string{"EUR/USD", "USD/TRY"}, pub, func() time.Time { return now })

	res := s.TickOnce(context.Background())
	var eur, try *MarkResult
	for i := range res {
		switch res[i].Symbol {
		case "EUR/USD":
			eur = &res[i]
		case "USD/TRY":
			try = &res[i]
		}
	}
	if eur == nil || !eur.OK {
		t.Fatal("EUR/USD must mark (2 fresh feeds)")
	}
	if try == nil || try.OK {
		t.Fatal("USD/TRY has 1 fresh feed — must fail closed")
	}
	if got := s.Health(context.Background()); got != HealthDegraded {
		t.Fatalf("service health %s, want DEGRADED", got)
	}
}

func TestFeedErrorCounted(t *testing.T) {
	now := time.Now()
	pub := newFakePub()
	s := newTestService(t, []Feed{
		&fakeFeed{name: "a", err: context.DeadlineExceeded},
		&fakeFeed{name: "b", quotes: map[string]Quote{"EUR/USD": fq("b", "EUR/USD", "1.1", now)}},
	}, []string{"EUR/USD"}, pub, func() time.Time { return now })
	res := s.TickOnce(context.Background())
	if res[0].OK {
		t.Fatal("errored feed must not count")
	}
	for _, h := range s.FeedHealth() {
		if h.Name == "a" && h.Errors == 0 {
			t.Fatal("feed error not recorded")
		}
	}
}
