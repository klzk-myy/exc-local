// Task 6.3.15 — parameterized depth@{symbol}:{levels}:{cadence} tests
// (spec §24 #265 supported-combination table {5,10,20}×{100,250,1000}).
package marketdata

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestParseChannelDepthVariants — the §24 #265 table: all nine
// level×cadence combinations plus the bare and partial forms bind;
// everything outside the table rejects at parse time.
func TestParseChannelDepthVariants(t *testing.T) {
	for _, lv := range []int{5, 10, 20} {
		for _, cad := range []int{100, 250, 1000} {
			raw := "depth@EUR/USD:" + itoa(lv) + ":" + itoa(cad)
			ch, err := ParseChannel(raw)
			if err != nil {
				t.Errorf("ParseChannel(%q) rejected supported variant: %v", raw, err)
				continue
			}
			if ch.Depth.Levels != lv || ch.Depth.CadenceMs != cad {
				t.Errorf("ParseChannel(%q) = %+v", raw, ch.Depth)
			}
			if ch.Raw != raw {
				t.Errorf("ParseChannel(%q) raw = %q (verbatim token must route)", raw, ch.Raw)
			}
		}
	}
	// Bare + partial forms resolve the defaults.
	ch, err := ParseChannel("depth@EUR/USD")
	if err != nil || ch.Depth != DefaultDepthVariant {
		t.Fatalf("bare depth: %+v err=%v", ch.Depth, err)
	}
	ch, err = ParseChannel("depth@EUR/USD:10")
	if err != nil || ch.Depth.Levels != 10 || ch.Depth.CadenceMs != 100 {
		t.Fatalf("partial depth: %+v err=%v", ch.Depth, err)
	}

	bad := []string{
		"depth@EUR/USD:7:100",   // unsupported levels
		"depth@EUR/USD:5:50",    // unsupported cadence
		"depth@EUR/USD:0:100",   // zero levels
		"depth@EUR/USD:20:2000", // unsupported cadence
		"depth@EUR/USD:x:100",   // non-numeric levels
		"depth@EUR/USD:5:x",     // non-numeric cadence
		"depth@EUR/USD:5:100:9", // too many parts
		"depth@EUR/USD:",        // empty params
		"depth@EUR/USD:5:",      // trailing empty cadence
		"book@EUR/USD:5:100",    // params on a non-depth channel
	}
	for _, raw := range bad {
		if _, err := ParseChannel(raw); err == nil {
			t.Errorf("ParseChannel(%q) accepted unsupported form", raw)
		}
	}
}

func itoa(v int) string { return strconv.Itoa(v) }

// mkBook builds a delta with n populated levels per side so slicing
// assertions are meaningful.
func mkBook(sym string, n int) BookDelta {
	d := BookDelta{Symbol: sym, Ts: time.Now()}
	for i := 0; i < n; i++ {
		px := int64(108_000_000 - i*10_000)
		d.Bids = append(d.Bids, Level{Price: px, Qty: 1_000_000_000, Count: 1})
		d.Asks = append(d.Asks, Level{Price: px + 60_000, Qty: 1_000_000_000, Count: 1})
	}
	return d
}

// variantSource returns a static VariantSource map for tests.
func variantSource(m map[string][]DepthVariant) func() map[string][]DepthVariant {
	return func() map[string][]DepthVariant { return m }
}

// TestDepthVariantSlicedEmit — a depth@{sym}:5:100 subscriber receives
// the SAME book state sliced to 5 levels, not a mirrored top-20 frame;
// the emitted CRC covers the emitted slice (§24 #83).
func TestDepthVariantSlicedEmit(t *testing.T) {
	c, sink := runConflator(t, ConflatorConfig{
		Window:       15 * time.Millisecond,
		MaxEvents:    1000,
		Depth:        20,
		VariantSweep: 5 * time.Millisecond,
		VariantSource: variantSource(map[string][]DepthVariant{
			"EUR/USD": {{Levels: 5, CadenceMs: 100}},
		}),
	}, NewMemSeqStore())

	c.Push(mkBook("EUR/USD", 20))
	// Wait for: master book@ + depth@ + the variant frame.
	sink.waitFor(t, 3)

	var variant emitRow
	found := false
	sink.mu.Lock()
	for _, r := range sink.rows {
		if r.channel == "depth@EUR/USD:5:100" {
			variant = r
			found = true
		}
	}
	sink.mu.Unlock()
	if !found {
		t.Fatal("no depth@EUR/USD:5:100 emit")
	}
	u := decodeUpdate(t, variant)
	if len(u.Bids) != 5 || len(u.Asks) != 5 {
		t.Fatalf("variant levels = %d bids/%d asks, want 5/5", len(u.Bids), len(u.Asks))
	}
	if u.Levels != 5 || u.CadenceMs != 100 {
		t.Fatalf("variant echo fields = %d/%d", u.Levels, u.CadenceMs)
	}
	// CRC must cover the EMITTED slice, not the full 20-level book.
	book := mkBook("EUR/USD", 20)
	if want := depthChecksum(book.Bids, book.Asks, 5); u.CRC32 != want {
		t.Fatalf("variant crc = %d, want %d (5-level slice)", u.CRC32, want)
	}
	if u.Seq == 0 || u.LastSeq == 0 {
		t.Fatalf("variant seq envelope empty: %+v", u)
	}
}

// TestDepthVariantCadenceGate — a 1000ms-cadence variant emits the first
// frame promptly, then suppresses fresher state until its interval
// elapses, while the master book@ channel keeps its own cadence.
func TestDepthVariantCadenceGate(t *testing.T) {
	c, sink := runConflator(t, ConflatorConfig{
		Window:       10 * time.Millisecond,
		MaxEvents:    1000,
		Depth:        20,
		VariantSweep: 3 * time.Millisecond,
		VariantSource: variantSource(map[string][]DepthVariant{
			"EUR/USD": {{Levels: 20, CadenceMs: 1000}},
		}),
	}, NewMemSeqStore())

	c.Push(mkBook("EUR/USD", 20))
	waitForChannel(t, sink, "depth@EUR/USD:20:1000")
	// Let the first window flush land so the second delta is a NEW
	// cursor advance, not a coalesced extension of the first.
	waitForChannel(t, sink, "book@EUR/USD")

	// Fresh state arrives immediately after — cadence must suppress it.
	c.Push(mkDelta("EUR/USD", 109_000_000))
	time.Sleep(150 * time.Millisecond) // ≫ sweep, ≪ cadence
	if n := countChannel(sink, "depth@EUR/USD:20:1000"); n != 1 {
		t.Fatalf("variant emits before cadence elapsed = %d, want 1", n)
	}
	// Master channel emitted both deltas meanwhile (proves suppression
	// is the variant's own cadence, not feed starvation).
	if n := countChannel(sink, "book@EUR/USD"); n < 2 {
		t.Fatalf("master emits = %d, want ≥2", n)
	}
}

// TestDepthVariantPrevChain — the variant channel's prev_last_seq chain
// is its OWN, not the master's: a slow-cadence variant's frames chain
// prev → its prior emit even though master frames landed in between.
func TestDepthVariantPrevChain(t *testing.T) {
	c, sink := runConflator(t, ConflatorConfig{
		Window:       10 * time.Millisecond,
		MaxEvents:    1000,
		Depth:        20,
		VariantSweep: 3 * time.Millisecond,
		VariantSource: variantSource(map[string][]DepthVariant{
			"EUR/USD": {{Levels: 10, CadenceMs: 100}},
		}),
	}, NewMemSeqStore())

	c.Push(mkBook("EUR/USD", 20))
	waitForChannel(t, sink, "depth@EUR/USD:10:100")
	time.Sleep(120 * time.Millisecond) // cadence elapses
	c.Push(mkDelta("EUR/USD", 109_000_000))
	// Wait for the second variant emit.
	deadline := time.Now().Add(2 * time.Second)
	for countChannel(sink, "depth@EUR/USD:10:100") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	var frames []depthUpdate
	sink.mu.Lock()
	for _, r := range sink.rows {
		if r.channel == "depth@EUR/USD:10:100" {
			frames = append(frames, decodeUpdate(t, r))
		}
	}
	sink.mu.Unlock()
	if len(frames) < 2 {
		t.Fatalf("variant emits = %d, want ≥2", len(frames))
	}
	if frames[0].PrevLastSeq != 0 {
		t.Fatalf("first variant frame prev = %d, want 0", frames[0].PrevLastSeq)
	}
	if frames[1].PrevLastSeq != frames[0].LastSeq {
		t.Fatalf("variant prev chain broken: prev=%d, prior last=%d",
			frames[1].PrevLastSeq, frames[0].LastSeq)
	}
	if frames[1].LastSeq <= frames[0].LastSeq {
		t.Fatal("variant last_seq did not advance")
	}
}

// TestSnapshotDepthVariant — resync on a depth:5 channel returns a
// 5-level snapshot with the variant echo fields (not a top-20 frame).
func TestSnapshotDepthVariant(t *testing.T) {
	c, sink := runConflator(t, ConflatorConfig{
		Window: 20 * time.Millisecond, MaxEvents: 1000, Depth: 20,
	}, NewMemSeqStore())
	c.Push(mkBook("EUR/USD", 20))
	sink.waitFor(t, 2)

	seq, data, err := c.Snapshot(context.Background(), "depth@EUR/USD:5:250")
	if err != nil {
		t.Fatalf("variant snapshot: %v", err)
	}
	u, ok := data.(depthUpdate)
	if !ok {
		t.Fatalf("snapshot type %T", data)
	}
	if seq == 0 || len(u.Bids) != 5 || len(u.Asks) != 5 {
		t.Fatalf("variant snapshot seq=%d bids=%d asks=%d", seq, len(u.Bids), len(u.Asks))
	}
	if u.Levels != 5 || u.CadenceMs != 250 || !u.Snapshot {
		t.Fatalf("variant snapshot fields %+v", u)
	}
	book := mkBook("EUR/USD", 20)
	if want := depthChecksum(book.Bids, book.Asks, 5); u.CRC32 != want {
		t.Fatalf("snapshot crc covers %d levels, want 5-level slice", 20)
	}
	if _, _, err := c.Snapshot(context.Background(), "depth@EUR/USD:7:100"); err == nil {
		t.Fatal("unsupported variant snapshot accepted")
	}
}

// TestDepthVariantWireEndToEnd — a real WS client subscribing
// depth@{sym}:5:100 receives frames on the verbatim channel token while
// the bare depth@{sym} subscriber gets the master stream.
func TestDepthVariantWireEndToEnd(t *testing.T) {
	srv, c := dial(t, Config{})
	sendJSON(t, c, map[string]any{"action": "subscribe",
		"channels": []string{"depth@EUR/USD:5:100", "depth@EUR/USD"}})
	m := readUntil(t, c, 2*time.Second, func(m map[string]any) bool {
		return m["type"] == "subscribed"
	})
	if int(m["total"].(float64)) != 2 {
		t.Fatalf("subs = %v", m["total"])
	}
	vars := srv.ActiveDepthVariants()
	if len(vars["EUR/USD"]) != 1 || vars["EUR/USD"][0].Levels != 5 {
		t.Fatalf("ActiveDepthVariants = %+v", vars)
	}

	srv.Publish("depth@EUR/USD:5:100", 7, map[string]any{"variant": true})
	srv.Publish("depth@EUR/USD", 8, map[string]any{"variant": false})
	got := map[string]bool{}
	deadline := time.Now().Add(2 * time.Second)
	for len(got) < 2 {
		m := readFrame(t, c, time.Until(deadline))
		if m["type"] == "event" {
			got[m["channel"].(string)] = true
		}
	}
	if !got["depth@EUR/USD:5:100"] || !got["depth@EUR/USD"] {
		t.Fatalf("events routed = %v", got)
	}
}

func waitForChannel(t *testing.T, sink *emitSink, channel string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countChannel(sink, channel) > 0 {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("no emit on %s", channel)
}

func countChannel(sink *emitSink, channel string) int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	n := 0
	for _, r := range sink.rows {
		if r.channel == channel {
			n++
		}
	}
	return n
}
