// Unit tests for the marketapi domain layer (Tasks 5.3.5/5.3.35/5.3.44).
package marketapi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testInstrument() Instrument {
	minP, maxP, spread := "0.00001", "", "50"
	_ = maxP
	open, algo := int64(200), int64(50)
	return Instrument{
		ID: 1, Symbol: "EUR/USD", BaseCurrency: "EUR", QuoteCurrency: "USD",
		InstrumentType: "SPOT", Status: "ACTIVE",
		TickSize: "0.00001", LotSize: "1000",
		MinOrderQty: "1000", MaxOrderQty: "100000000", MinNotional: "10",
		MinPrice: &minP, MaxPrice: nil,
		PriceBandPctUp: "2.00", PriceBandPctDown: "5.00",
		MaxSpreadPips: &spread, MaxOpenOrders: &open, MaxAlgoOrders: &algo,
		MaxLeverage: 30, SettlementCycle: 1,
		UpdatedAt: time.UnixMilli(1700000000000),
	}
}

func TestInstrumentFiltersEmitAllSix(t *testing.T) {
	fs := testInstrument().Filters()
	want := []string{"PRICE_FILTER", "LOT_SIZE", "MIN_NOTIONAL",
		"PRICE_BAND", "MAX_ORDERS", "SPREAD_PROTECTION"}
	if len(fs) != len(want) {
		t.Fatalf("filters=%d, want %d", len(fs), len(want))
	}
	for i, w := range want {
		if fs[i].FilterType != w {
			t.Fatalf("filter[%d]=%s, want %s", i, fs[i].FilterType, w)
		}
	}
	if *fs[0].TickSize != "0.00001" || fs[0].MaxPrice != nil {
		t.Fatalf("PRICE_FILTER=%+v", fs[0])
	}
	if *fs[1].MinQty != "1000" || *fs[1].StepSize != "1000" {
		t.Fatalf("LOT_SIZE=%+v", fs[1])
	}
	if *fs[4].MaxOpenOrders != 200 || *fs[4].MaxAlgoOrders != 50 {
		t.Fatalf("MAX_ORDERS=%+v", fs[4])
	}
	if *fs[5].MaxSpreadPips != "50" {
		t.Fatalf("SPREAD_PROTECTION=%+v", fs[5])
	}
	// Every filter serializes all of its members (null for unconfigured)
	// — clients must not infer "absent key = unbounded".
	body, _ := json.Marshal(fs)
	s := string(body)
	for _, key := range []string{
		"min_price", "max_price", "tick_size", "min_qty", "max_qty",
		"step_size", "min_notional", "price_band_pct_up",
		"price_band_pct_down", "max_open_orders", "max_algo_orders",
		"max_spread_pips"} {
		if !strings.Contains(s, `"`+key+`"`) {
			t.Fatalf("filters JSON missing member %q: %s", key, s)
		}
	}
}

func TestOrderTypesAndPermissions(t *testing.T) {
	spot := testInstrument()
	if got := len(spot.OrderTypes()); got != 16 {
		t.Fatalf("spot order types=%d, want 16", got)
	}
	fwd := spot
	fwd.InstrumentType = "FORWARD"
	if got := len(fwd.OrderTypes()); got != 4 {
		t.Fatalf("forward order types=%d, want 4: %v", got, fwd.OrderTypes())
	}
	opt := spot
	opt.InstrumentType = "OPTION"
	if got := len(opt.OrderTypes()); got != 2 {
		t.Fatalf("option order types=%d, want 2", got)
	}
	restricted := spot
	restricted.Status = "RESTRICTED"
	if restricted.MarketOrdersAllowed() || !restricted.NewOrdersAllowed() {
		t.Fatal("RESTRICTED must take limit orders only")
	}
	draft := spot
	draft.Status = "DRAFT"
	if draft.NewOrdersAllowed() || draft.MarketOrdersAllowed() {
		t.Fatal("DRAFT must not admit orders")
	}
}

func TestSettlementLabel(t *testing.T) {
	i := testInstrument()
	for cyc, want := range map[int]string{0: "SAME_DAY", 1: "T+1", 2: "T+2", 5: "T+5"} {
		i.SettlementCycle = cyc
		if got := i.SettlementLabel(); got != want {
			t.Fatalf("cycle %d → %s, want %s", cyc, got, want)
		}
	}
}

func TestVenueETagStableAndSensitive(t *testing.T) {
	inst := testInstrument()
	now := time.UnixMilli(1700000100000)
	v1 := BuildVenueInfo([]Instrument{inst}, now)
	e1, err := VenueETag(v1)
	if err != nil {
		t.Fatal(err)
	}
	// Clock tick must not change the tag.
	v2 := BuildVenueInfo([]Instrument{inst}, now.Add(5*time.Second))
	e2, _ := VenueETag(v2)
	if e1 != e2 {
		t.Fatalf("etag unstable across server_time tick: %s vs %s", e1, e2)
	}
	// An instrument mutation bumps the tag.
	inst.UpdatedAt = inst.UpdatedAt.Add(time.Minute)
	v3 := BuildVenueInfo([]Instrument{inst}, now)
	e3, _ := VenueETag(v3)
	if e3 == e1 {
		t.Fatal("etag did not change on instrument update")
	}
}

func TestVenueDocShape(t *testing.T) {
	doc := BuildVenueInfo([]Instrument{testInstrument()}, time.UnixMilli(1700000100000))
	if doc.Timezone != "UTC" || doc.ServerTimeMs != 1700000100000 {
		t.Fatalf("doc head=%+v", doc)
	}
	if len(doc.RateLimits) != 6 || len(doc.RouteWeights) == 0 {
		t.Fatalf("rate tables=%d/%d", len(doc.RateLimits), len(doc.RouteWeights))
	}
	if doc.RateLimits[0].Tier != "public" || doc.RateLimits[0].KeyedBy != "ip" {
		t.Fatalf("rate_limits[0]=%+v", doc.RateLimits[0])
	}
	if doc.TradingHours.Type != "24/5" {
		t.Fatalf("trading_hours=%+v", doc.TradingHours)
	}
	if len(doc.Symbols) != 1 || doc.Symbols[0].Symbol != "EUR/USD" ||
		len(doc.Symbols[0].Filters) != 6 {
		t.Fatalf("symbols=%+v", doc.Symbols)
	}
}

func TestCacheTTLAndErrorPassthrough(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewCache(func() time.Time { return now })
	calls := 0
	fn := func() (any, error) { calls++; return "v", nil }
	for i := 0; i < 3; i++ {
		if v, err := c.Do("k", time.Second, fn); err != nil || v != "v" {
			t.Fatalf("Do=%v,%v", v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 (cache hit)", calls)
	}
	now = now.Add(2 * time.Second)
	if _, err := c.Do("k", time.Second, fn); err != nil || calls != 2 {
		t.Fatalf("expired refetch: calls=%d", calls)
	}
	// Errors never cache.
	boom := errors.New("store down")
	_, err := c.Do("e", time.Second, func() (any, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	_, err = c.Do("e", time.Second, func() (any, error) { return "ok", nil })
	if err != nil {
		t.Fatal("error was cached — must refetch")
	}
}

func TestAnnouncementLive(t *testing.T) {
	now := time.Now()
	a := Announcement{Status: "PUBLISHED", PublishAt: now.Add(-time.Minute)}
	if !a.Live(now) {
		t.Fatal("published+started should be live")
	}
	a.Status = "DRAFT"
	if a.Live(now) {
		t.Fatal("draft must not be live")
	}
	a.Status = "PUBLISHED"
	exp := now.Add(-time.Second)
	a.ExpiresAt = &exp
	if a.Live(now) {
		t.Fatal("expired announcement must not be live")
	}
	fut := now.Add(time.Hour)
	a.ExpiresAt = &fut
	a.PublishAt = now.Add(time.Minute)
	if a.Live(now) {
		t.Fatal("future publish_at must not be live")
	}
}
