package margin

import (
	"testing"
	"time"

	"exchange/pkg/decimal"
)

var expiryA = time.Date(2026, 10, 15, 15, 0, 0, 0, time.UTC)
var expiryB = time.Date(2026, 11, 15, 15, 0, 0, 0, time.UTC)

func leg(id int64, typ, side string, strike string, expiry time.Time, qty, naked string) OptionLeg {
	return OptionLeg{
		PositionID: id, AccountID: 7, InstrumentID: 500, UnderlyingID: 9,
		OptionType: typ, Side: side,
		Quantity:       decimal.RequireFromString(qty),
		Strike:         decimal.RequireFromString(strike),
		ExpiryAt:       expiry,
		NakedMarginUSD: decimal.RequireFromString(naked),
		ContractSize:   decimal.NewFromInt(1000),
		QuoteToUSD:     decimal.NewFromInt(1),
	}
}

func findSpread(det *SpreadDetection, kind string) *RecognizedSpread {
	for i := range det.Spreads {
		if det.Spreads[i].Kind == kind {
			return &det.Spreads[i]
		}
	}
	return nil
}

func TestDetectVerticalSpread_MaxLossBound(t *testing.T) {
	// Long 1.10 call + short 1.15 call, same expiry → bound = width·qty·size.
	det := DetectSpreads([]OptionLeg{
		leg(1, "CALL", "LONG", "1.10", expiryA, "10", "300"),
		leg(2, "CALL", "SHORT", "1.15", expiryA, "10", "700"),
	}, DefaultSpreadParams())
	sp := findSpread(det, SpreadVerticalCall)
	if sp == nil {
		t.Fatalf("no vertical detected; naked=%d", len(det.NakedLegs))
	}
	want := decimal.RequireFromString("500") // 0.05 × 10 × 1000
	if !sp.BoundMarginUSD.Equal(want) {
		t.Fatalf("bound %s want %s", sp.BoundMarginUSD, want)
	}
	if !sp.OffsetUSD.Equal(decimal.RequireFromString("500")) { // 1000 − 500
		t.Fatalf("offset %s", sp.OffsetUSD)
	}
	if sp.LongLegID != 1 || sp.ShortLegID != 2 {
		t.Fatalf("leg ids %+v", sp)
	}
}

func TestDetectVerticalSpread_PutAndPartialQty(t *testing.T) {
	// Put vertical, unequal qty → matched = min; offset never exceeds naked.
	det := DetectSpreads([]OptionLeg{
		leg(1, "PUT", "SHORT", "1.20", expiryA, "5", "400"),
		leg(2, "PUT", "LONG", "1.10", expiryA, "10", "200"),
	}, DefaultSpreadParams())
	sp := findSpread(det, SpreadVerticalPut)
	if sp == nil {
		t.Fatal("no put vertical")
	}
	want := decimal.RequireFromString("500") // 0.10 × 5 × 1000
	if !sp.BoundMarginUSD.Equal(want) || !sp.MatchedQty.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("bound %s qty %s", sp.BoundMarginUSD, sp.MatchedQty)
	}
	if sp.OffsetUSD.GreaterThan(decimal.RequireFromString("600")) {
		t.Fatal("offset exceeds naked sum")
	}
}

func TestDetectStraddle_ShortBothSides(t *testing.T) {
	// Short call + short put, same strike+expiry → bound = max + 0.5·min.
	det := DetectSpreads([]OptionLeg{
		leg(1, "CALL", "SHORT", "1.10", expiryA, "10", "600"),
		leg(2, "PUT", "SHORT", "1.10", expiryA, "10", "400"),
	}, DefaultSpreadParams())
	sp := findSpread(det, SpreadStraddle)
	if sp == nil {
		t.Fatal("no straddle")
	}
	if !sp.BoundMarginUSD.Equal(decimal.RequireFromString("800")) { // 600 + 200
		t.Fatalf("bound %s want 800", sp.BoundMarginUSD)
	}
	if !sp.OffsetBps.Equal(decimal.NewFromInt(5000)) {
		t.Fatalf("bps %s", sp.OffsetBps)
	}
}

func TestDetectStrangle_DifferentStrikes(t *testing.T) {
	det := DetectSpreads([]OptionLeg{
		leg(1, "CALL", "SHORT", "1.15", expiryA, "10", "300"),
		leg(2, "PUT", "SHORT", "1.05", expiryA, "10", "300"),
	}, DefaultSpreadParams())
	sp := findSpread(det, SpreadStrangle)
	if sp == nil {
		t.Fatal("no strangle")
	}
	if !sp.BoundMarginUSD.Equal(decimal.RequireFromString("450")) { // 300 + 150
		t.Fatalf("bound %s", sp.BoundMarginUSD)
	}
}

func TestDetectStraddle_LongBothSides_ZeroBound(t *testing.T) {
	det := DetectSpreads([]OptionLeg{
		leg(1, "CALL", "LONG", "1.10", expiryA, "10", "100"),
		leg(2, "PUT", "LONG", "1.10", expiryA, "10", "100"),
	}, DefaultSpreadParams())
	sp := findSpread(det, SpreadStraddle)
	if sp == nil {
		t.Fatal("no long straddle")
	}
	if !sp.BoundMarginUSD.IsZero() {
		t.Fatalf("long combo bound should be 0, got %s", sp.BoundMarginUSD)
	}
}

func TestDetectCalendar_ShortNearLongFar(t *testing.T) {
	det := DetectSpreads([]OptionLeg{
		leg(1, "CALL", "SHORT", "1.10", expiryA, "10", "500"),
		leg(2, "CALL", "LONG", "1.10", expiryB, "10", "300"),
	}, DefaultSpreadParams())
	sp := findSpread(det, SpreadCalendar)
	if sp == nil {
		t.Fatal("no calendar")
	}
	if !sp.BoundMarginUSD.Equal(decimal.RequireFromString("250")) { // 50% of short naked
		t.Fatalf("bound %s", sp.BoundMarginUSD)
	}
}

func TestDetectCalendar_ReverseNotRecognized(t *testing.T) {
	// Long near + short far is a reverse calendar — not an offset pair.
	det := DetectSpreads([]OptionLeg{
		leg(1, "CALL", "LONG", "1.10", expiryA, "10", "100"),
		leg(2, "CALL", "SHORT", "1.10", expiryB, "10", "500"),
	}, DefaultSpreadParams())
	if findSpread(det, SpreadCalendar) != nil {
		t.Fatal("reverse calendar recognized")
	}
	if len(det.NakedLegs) != 2 {
		t.Fatalf("naked %d", len(det.NakedLegs))
	}
}

func TestDetect_NonQualifyingLegs(t *testing.T) {
	det := DetectSpreads([]OptionLeg{
		leg(1, "CALL", "LONG", "1.10", expiryA, "10", "100"), // lone leg
		{PositionID: 2, AccountID: 7, UnderlyingID: 9, OptionType: "BINARY",
			Side: "LONG", Quantity: decimal.NewFromInt(1),
			Strike: decimal.RequireFromString("1"), ExpiryAt: expiryA},
		leg(3, "CALL", "LONG", "1.10", expiryA, "10", "100"), // same side — no pair
	}, DefaultSpreadParams())
	if len(det.Spreads) != 0 {
		t.Fatalf("spreads detected: %+v", det.Spreads)
	}
}

func TestDetect_SingleLegCannotPairTwice(t *testing.T) {
	// One long call could pair with the short call as a vertical OR a
	// calendar against the later short — it must commit to exactly one.
	det := DetectSpreads([]OptionLeg{
		leg(1, "CALL", "LONG", "1.10", expiryA, "10", "100"),
		leg(2, "CALL", "SHORT", "1.15", expiryA, "10", "500"),
		leg(3, "CALL", "SHORT", "1.10", expiryB, "10", "300"),
	}, DefaultSpreadParams())
	if len(det.Spreads) != 1 {
		t.Fatalf("expected exactly 1 spread, got %d", len(det.Spreads))
	}
	// Vertical has priority over calendar (fixed ordering).
	if det.Spreads[0].Kind != SpreadVerticalCall {
		t.Fatalf("kind %s", det.Spreads[0].Kind)
	}
	if len(det.NakedLegs) != 1 {
		t.Fatalf("residual naked %d", len(det.NakedLegs))
	}
}

func TestDetect_DeterministicOrdering(t *testing.T) {
	legs := []OptionLeg{
		leg(3, "PUT", "SHORT", "1.05", expiryA, "10", "300"),
		leg(1, "CALL", "SHORT", "1.15", expiryA, "10", "300"),
		leg(2, "CALL", "LONG", "1.10", expiryA, "10", "200"),
		leg(4, "PUT", "SHORT", "1.10", expiryA, "10", "300"),
	}
	a := DetectSpreads(legs, DefaultSpreadParams())
	b := DetectSpreads(legs, DefaultSpreadParams())
	if len(a.Spreads) != len(b.Spreads) {
		t.Fatal("nondeterministic count")
	}
	for i := range a.Spreads {
		if a.Spreads[i].ID != b.Spreads[i].ID {
			t.Fatalf("nondeterministic spread ids")
		}
	}
}

func TestDetect_MultiAccountIsolation(t *testing.T) {
	// Legs across different accounts must never pair.
	l1 := leg(1, "CALL", "LONG", "1.10", expiryA, "10", "100")
	l1.AccountID = 7
	l2 := leg(2, "CALL", "SHORT", "1.15", expiryA, "10", "500")
	l2.AccountID = 8
	det := DetectSpreads([]OptionLeg{l1, l2}, DefaultSpreadParams())
	if len(det.Spreads) != 0 {
		t.Fatal("cross-account pair detected")
	}
}

func TestSpreadParams_CustomOffset(t *testing.T) {
	p := SpreadParams{OffsetsBps: map[string]decimal.Decimal{
		SpreadStraddle: decimal.NewFromInt(8000)}} // 80% relief on smaller leg
	det := DetectSpreads([]OptionLeg{
		leg(1, "CALL", "SHORT", "1.10", expiryA, "10", "600"),
		leg(2, "PUT", "SHORT", "1.10", expiryA, "10", "400"),
	}, p)
	sp := findSpread(det, SpreadStraddle)
	if sp == nil {
		t.Fatal("no straddle")
	}
	// bound = 600 + 400·(1−0.8) = 680
	if !sp.BoundMarginUSD.Equal(decimal.RequireFromString("680")) {
		t.Fatalf("bound %s want 680", sp.BoundMarginUSD)
	}
}
