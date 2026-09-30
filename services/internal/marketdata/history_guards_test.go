// Phase-23 Task 23.3.8 — guard, cache and masking unit tests.
package marketdata

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// guardFakeKV is an in-memory HistoryKV.
type guardFakeKV struct {
	m   map[string]string
	ttl map[string]time.Duration
	err error
}

func newGuardFakeKV() *guardFakeKV {
	return &guardFakeKV{m: map[string]string{}, ttl: map[string]time.Duration{}}
}

func (f *guardFakeKV) Get(_ context.Context, key string) *goredis.StringCmd {
	if f.err != nil {
		return goredis.NewStringResult("", f.err)
	}
	v, ok := f.m[key]
	if !ok {
		return goredis.NewStringResult("", goredis.Nil)
	}
	return goredis.NewStringResult(v, nil)
}

func (f *guardFakeKV) Set(_ context.Context, key string, value interface{}, exp time.Duration) *goredis.StatusCmd {
	if f.err != nil {
		return goredis.NewStatusResult("", f.err)
	}
	if b, ok := value.([]byte); ok {
		f.m[key] = string(b)
	} else {
		f.m[key] = "<?>"
	}
	f.ttl[key] = exp
	return goredis.NewStatusResult("OK", nil)
}

// ---------------------------------------------------------------------------
// Timeout
// ---------------------------------------------------------------------------

func TestHistoryQueryGuardTimeout(t *testing.T) {
	ctx, cancel := HistoryQueryGuard{}.QueryContext(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("guard must stamp a deadline")
	}
	if d := time.Until(dl); d <= 9*time.Second || d > 10*time.Second {
		t.Fatalf("default deadline ~10s, got %s", d)
	}
	ctx2, cancel2 := HistoryQueryGuard{Timeout: 250 * time.Millisecond}.
		QueryContext(context.Background())
	defer cancel2()
	dl2, _ := ctx2.Deadline()
	if d := time.Until(dl2); d <= 200*time.Millisecond || d > 250*time.Millisecond {
		t.Fatalf("override deadline ~250ms, got %s", d)
	}
}

// ---------------------------------------------------------------------------
// Cache key + closed interval
// ---------------------------------------------------------------------------

func TestHistoryCacheKeyDeterministic(t *testing.T) {
	q1 := url.Values{"from": {"a"}, "to": {"b"}, "limit": {"100"}}
	q2 := url.Values{"limit": {"100"}, "to": {"b"}, "from": {"a"}}
	k1 := HistoryCacheKey("GET", "/api/v1/history/ticks/EUR%2FUSD", "json", "free", q1)
	k2 := HistoryCacheKey("GET", "/api/v1/history/ticks/EUR%2FUSD", "json", "free", q2)
	if k1 != k2 {
		t.Fatalf("sorted-query hash must be order-insensitive: %q vs %q", k1, k2)
	}
	if !strings.HasPrefix(k1, "histq:") {
		t.Fatalf("key = %q", k1)
	}
	// Tier and format partition the keyspace.
	if HistoryCacheKey("GET", "/p", "json", "premium", q1) == k1 {
		t.Fatal("tier must partition the cache key")
	}
	if HistoryCacheKey("GET", "/p", "csv", "free", q1) == k1 {
		t.Fatal("format must partition the cache key")
	}
	// Different query content → different key.
	if HistoryCacheKey("GET", "/p", "json", "free",
		url.Values{"from": {"z"}}) == k1 {
		t.Fatal("query content must affect the cache key")
	}
}

func TestClosedInterval(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	g := HistoryQueryGuard{Now: func() time.Time { return now }}
	if !g.ClosedInterval(now.Add(-16*time.Minute), 15*time.Minute) {
		t.Fatal("to < now-delay must be closed")
	}
	if g.ClosedInterval(now.Add(-5*time.Minute), 15*time.Minute) {
		t.Fatal("to inside the delay horizon must not be closed")
	}
	if g.ClosedInterval(time.Time{}, 0) {
		t.Fatal("unbounded to must not be closed")
	}
	if !g.ClosedInterval(now.Add(-time.Second), 0) {
		t.Fatal("premium real-time: to < now must be closed")
	}
	if g.ClosedInterval(now.Add(time.Second), 0) {
		t.Fatal("future to is never closed")
	}
}

func TestCacheGetPutBestEffort(t *testing.T) {
	g := HistoryQueryGuard{}
	// Nil store: miss + silent no-op.
	if _, ok := g.CacheGet(context.Background(), nil, "k"); ok {
		t.Fatal("nil store must miss")
	}
	g.CachePut(context.Background(), nil, "k", []byte("x"))

	kv := newGuardFakeKV()
	if _, ok := g.CacheGet(context.Background(), kv, "k"); ok {
		t.Fatal("absent key must miss")
	}
	g.CachePut(context.Background(), kv, "k", []byte("payload"))
	b, ok := g.CacheGet(context.Background(), kv, "k")
	if !ok || string(b) != "payload" {
		t.Fatalf("round-trip = %q,%v", b, ok)
	}
	if kv.ttl["k"] != 60*time.Second {
		t.Fatalf("default TTL = %s, want 60s", kv.ttl["k"])
	}
	// Store outage: never blocks.
	kv.err = errors.New("redis down")
	if _, ok := g.CacheGet(context.Background(), kv, "k"); ok {
		t.Fatal("outage must read as miss")
	}
	g.CachePut(context.Background(), kv, "k2", []byte("y")) // no panic
}

// ---------------------------------------------------------------------------
// Masking
// ---------------------------------------------------------------------------

func maskRow(ts time.Time, maker, taker int64) *HistoryTrade {
	return &HistoryTrade{Ts: ts,
		Participants: ParticipantFields{
			MakerAccountID: maker, TakerAccountID: taker,
			BuyOrderID: 1, SellOrderID: 2, OrderTag: "MMQ",
		}}
}

func TestMaskParticipantFields(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	rows := []*HistoryTrade{maskRow(now, 1, 2), {Ts: now, Symbol: "EUR/USD"}}
	if n := MaskParticipantFields(rows); n != 1 {
		t.Fatalf("masked = %d, want 1 (second row already empty)", n)
	}
	if !rows[0].Participants.Masked() {
		t.Fatal("row 0 must be fully masked")
	}
}

func TestMaskPreOpenWindowed(t *testing.T) {
	open := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	inside := maskRow(open.Add(-10*time.Minute), 1, 2) // [open-15m, open)
	edge := maskRow(open.Add(-15*time.Minute), 1, 2)   // left edge included
	out := maskRow(open.Add(-16*time.Minute), 1, 2)    // before window
	after := maskRow(open.Add(time.Minute), 1, 2)      // post-open
	rows := []*HistoryTrade{inside, edge, out, after}
	if n := MaskPreOpen(rows, open); n != 2 {
		t.Fatalf("masked = %d, want 2", n)
	}
	if !inside.Participants.Masked() || !edge.Participants.Masked() {
		t.Fatal("in-window rows must be masked")
	}
	if out.Participants.Masked() || after.Participants.Masked() {
		t.Fatal("out-of-window rows must NOT be masked — 24/5 venue has " +
			"no per-day open; older sessions would be over-masked")
	}
	// Zero open masks nothing.
	if n := MaskPreOpen(rows, time.Time{}); n != 0 {
		t.Fatalf("zero open masked %d rows", n)
	}
	// Custom window via the windowed form.
	rows2 := []*HistoryTrade{maskRow(open.Add(-time.Hour), 1, 2)}
	if n := MaskPreOpenWindowed(rows2, open, 2*time.Hour); n != 1 {
		t.Fatalf("custom window masked %d, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Venue weekly open (24/5 — Sun 21:00 UTC → Fri 22:00 UTC)
// ---------------------------------------------------------------------------

func TestVenueWeekOpenUTC(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"mid-week", time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC), // Wed
			time.Date(2026, 10, 4, 21, 0, 0, 0, time.UTC)},
		{"sunday in-session", time.Date(2026, 10, 4, 22, 0, 0, 0, time.UTC),
			time.Date(2026, 10, 4, 21, 0, 0, 0, time.UTC)},
		{"sunday early (close)", time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC),
			time.Date(2026, 10, 4, 21, 0, 0, 0, time.UTC)},
		{"saturday close", time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 10, 4, 21, 0, 0, 0, time.UTC)},
		{"friday after close", time.Date(2026, 10, 2, 23, 0, 0, 0, time.UTC),
			time.Date(2026, 10, 4, 21, 0, 0, 0, time.UTC)},
		{"friday in-session", time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := VenueWeekOpenUTC(c.in); !got.Equal(c.want) {
			t.Fatalf("%s: VenueWeekOpenUTC(%s) = %s, want %s",
				c.name, c.in, got, c.want)
		}
	}
}
