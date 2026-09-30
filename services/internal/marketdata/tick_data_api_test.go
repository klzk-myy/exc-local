// Phase-23 Task 23.3.4 — tiering, window resolution, negotiation and
// renderer unit tests.
package marketdata

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/ratelimit"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Tier mapping
// ---------------------------------------------------------------------------

func TestHistoryAccessForRateTier(t *testing.T) {
	cases := map[ratelimit.Tier]HistoryAccess{
		ratelimit.TierPublic:        HistoryAccessFree,
		ratelimit.TierBasic:         HistoryAccessFree,
		ratelimit.TierStandard:      HistoryAccessFree, // not granted premium by the task
		ratelimit.TierDemo:          HistoryAccessFree,
		ratelimit.TierProfessional:  HistoryAccessPremium,
		ratelimit.TierInstitutional: HistoryAccessPremium,
		ratelimit.TierAdmin:         HistoryAccessStaff,
		ratelimit.Tier("bogus"):     HistoryAccessFree, // fail-closed
	}
	for tier, want := range cases {
		if got := HistoryAccessForRateTier(tier); got != want {
			t.Fatalf("tier %q → %q, want %q", tier, got, want)
		}
	}
}

func TestRateTierHistoryResolver(t *testing.T) {
	// Nil claims → free.
	r := RateTierHistoryResolver(nil)
	if a, err := r(context.Background(), nil); a != HistoryAccessFree || err != nil {
		t.Fatalf("nil claims → %q,%v", a, err)
	}
	// Resolver maps the claims tier.
	rt := RateTierHistoryResolver(func(context.Context, *auth.Claims) ratelimit.Tier {
		return ratelimit.TierInstitutional
	})
	a, err := rt(context.Background(), &auth.Claims{AccountID: 7})
	if a != HistoryAccessPremium || err != nil {
		t.Fatalf("institutional → %q,%v", a, err)
	}
}

// ---------------------------------------------------------------------------
// Window resolution
// ---------------------------------------------------------------------------

func TestResolveHistoryWindow(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	// Free: from clamps to now-30d, to clamps to now-15min.
	w := ResolveHistoryWindow(HistoryAccessFree,
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), now, now)
	if !w.From.Equal(now.AddDate(0, 0, -30)) || !w.To.Equal(now.Add(-15*time.Minute)) {
		t.Fatalf("free window = [%s, %s)", w.From, w.To)
	}
	// Free: in-window bounds pass through.
	inFrom := now.AddDate(0, 0, -5)
	inTo := now.Add(-time.Hour)
	w = ResolveHistoryWindow(HistoryAccessFree, inFrom, inTo, now)
	if !w.From.Equal(inFrom) || !w.To.Equal(inTo) {
		t.Fatalf("in-window bounds rewritten: [%s, %s)", w.From, w.To)
	}
	// Free: unbounded gets both bounds.
	w = ResolveHistoryWindow(HistoryAccessFree, time.Time{}, time.Time{}, now)
	if w.From.IsZero() || w.To.IsZero() {
		t.Fatalf("free unbounded must get floor+ceiling: %+v", w)
	}
	// Free: from inside the delay horizon → provably empty.
	w = ResolveHistoryWindow(HistoryAccessFree, now.Add(-5*time.Minute), time.Time{}, now)
	if !w.Empty() {
		t.Fatalf("horizon-clamped window must be empty: %+v", w)
	}
	// Premium/staff: verbatim.
	for _, a := range []HistoryAccess{HistoryAccessPremium, HistoryAccessStaff} {
		w = ResolveHistoryWindow(a, inFrom, time.Time{}, now)
		if !w.From.Equal(inFrom) || !w.To.IsZero() {
			t.Fatalf("%s window clamped: %+v", a, w)
		}
	}
}

func TestHistoryDelayFor(t *testing.T) {
	if HistoryDelayFor(HistoryAccessFree) != 15*time.Minute {
		t.Fatal("free must carry the 15-minute publication delay")
	}
	for _, a := range []HistoryAccess{HistoryAccessPremium, HistoryAccessStaff} {
		if HistoryDelayFor(a) != 0 {
			t.Fatalf("%s delay = %s", a, HistoryDelayFor(a))
		}
	}
}

// ---------------------------------------------------------------------------
// Format negotiation
// ---------------------------------------------------------------------------

func TestHistoryFormatFor(t *testing.T) {
	cases := map[string]HistoryFormat{
		"":                                  HistoryFormatJSON,
		"*/*":                               HistoryFormatJSON,
		"application/json":                  HistoryFormatJSON,
		"text/html,application/xhtml+xml":   HistoryFormatJSON, // browser default stays JSON
		"text/csv":                          HistoryFormatCSV,
		"Text/CSV":                          HistoryFormatCSV,
		"application/x-fix":                 HistoryFormatFIX,
		"application/json, text/csv":        HistoryFormatCSV, // explicit beats default
		"text/csv;q=0.5, application/x-fix": HistoryFormatFIX,
	}
	for accept, want := range cases {
		if got := HistoryFormatFor(accept); got != want {
			t.Fatalf("Accept %q → %q, want %q", accept, got, want)
		}
	}
	for f, ct := range map[HistoryFormat]string{
		HistoryFormatJSON: "application/json",
		HistoryFormatCSV:  "text/csv",
		HistoryFormatFIX:  "application/x-fix",
	} {
		if f.ContentType() != ct {
			t.Fatalf("format %q content-type = %q", f, f.ContentType())
		}
	}
}

// ---------------------------------------------------------------------------
// Renderers
// ---------------------------------------------------------------------------

func exportRow(id uint64, seq uint64, ts time.Time) TickExportRow {
	return TickExportRow{
		TradeID: id, EventSeq: seq, ShardID: 2, Symbol: "EUR/USD",
		Price: decimal.MustFromString("1.08522"),
		Qty:   decimal.MustFromString("50000"), Side: "SELL", Ts: ts,
	}
}

func TestWriteTicksCSV(t *testing.T) {
	ts := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := WriteTicksCSV(&buf, []TickExportRow{exportRow(1002, 43, ts)}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	want := "trade_id,event_seq,shard_id,time,price,quantity,side\n" +
		"1002,43,2,2026-10-06T12:00:00.000Z,1.08522000,50000.00000000,SELL\n"
	if out != want {
		t.Fatalf("csv = %q, want %q", out, want)
	}
}

func TestWriteTicksFIXDropCopy(t *testing.T) {
	ts := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := WriteTicksFIXDropCopy(&buf, []TickExportRow{exportRow(1002, 43, ts)}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, tag := range []string{"11=1002", "17=43", "55=EUR/USD",
		"31=1.08522000", "32=50000.00000000", "54=2", "60=20261006-12:00:00.000"} {
		if !strings.Contains(out, tag) {
			t.Fatalf("fix row missing %q: %q", tag, out)
		}
	}
	if !strings.ContainsRune(out, '\x01') {
		t.Fatal("FIX rows must be SOH-delimited")
	}
	// BUY maps to tag 54=1.
	buf.Reset()
	r := exportRow(1, 1, ts)
	r.Side = "BUY"
	_ = WriteTicksFIXDropCopy(&buf, []TickExportRow{r})
	if !strings.Contains(buf.String(), "54=1") {
		t.Fatalf("BUY side = %q", buf.String())
	}
}
