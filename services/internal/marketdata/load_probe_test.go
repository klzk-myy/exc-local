// Gated: skipped unless EXC_LOAD_PROBE=1 (same convention as
// export_pg_test.go's EXC_PG_TEST). No external infrastructure is
// needed — the probe stands up a real marketdata.Server behind a real
// HTTP listener and drives real WebSocket clients in-process; the gate
// exists because the sustained-load leg intentionally runs several
// seconds and must not slow the default suite.
//
// Evidence target (Phase-06 DoD rows):
//   - "No stale data, no gaps under load": per-subscriber sequence
//     continuity under sustained publish load, plus the §10.9
//     gap→resync repair paths (replay-ring resume after a mid-stream
//     drop; resync directive + snapshot for a cursor past the horizon).
//   - "Market data SLA: p99 WS push ≤ 100ms" (spec §24 #99): measured
//     two ways — the server-side Metrics.LatencySnapshot reservoir
//     (Publish→enqueue, ns resolution) and client-observed end-to-end
//     latency (producer t0_ns carried in the payload → client decode,
//     same-host clock).
//
// Run: EXC_LOAD_PROBE=1 go test ./internal/marketdata/ -run 'LoadProbe' -v
package marketdata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// probeChannel is the single public channel under test — an L2-class
// book feed matching the production fanout shape.
const probeChannel = "book@EUR/USD"

// probeEvent mirrors the wire surface the probe consumes: envelope
// fields plus the embedded producer timestamp for end-to-end latency.
type probeEvent struct {
	Type    string `json:"type"`
	Channel string `json:"channel"`
	Seq     uint64 `json:"seq"`
	Data    struct {
		T0 int64 `json:"t0_ns"` // producer UnixNano at Publish entry
		N  int   `json:"n"`
	} `json:"data"`
	Reason  string `json:"reason"`
	FromSeq uint64 `json:"from_seq"`
	ToSeq   uint64 `json:"to_seq"`
	Count   int    `json:"count"`
}

// probeResult is one subscriber's outcome for the load window.
type probeResult struct {
	events   int
	lastSeq  uint64
	gaps     int
	gapFrom  uint64 // expected seq on first discontinuity
	gapTo    uint64 // received seq on first discontinuity
	latNanos []int64
	err      error
}

// probeServer stands up a Server on a real HTTP listener.
func probeServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	if os.Getenv("EXC_LOAD_PROBE") != "1" {
		t.Skip("set EXC_LOAD_PROBE=1 to run the market-data load probe")
	}
	srv := NewServer(cfg)
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	return srv, "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws/v1/marketdata"
}

// probeDial opens one client conn and binds probeChannel.
func probeDial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	b, _ := json.Marshal(map[string]any{
		"action": "subscribe", "channels": []string{probeChannel},
	})
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("subscribe write: %v", err)
	}
	// Consume the subscribed ack before streaming starts.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("subscribe ack read: %v", err)
		}
		var f probeEvent
		if json.Unmarshal(raw, &f) == nil && f.Type == "subscribed" {
			break
		}
	}
	_ = c.SetReadDeadline(time.Time{})
	return c
}

// probeReader drains events until want have been received or the
// deadline passes; seq continuity and per-event latency are recorded.
func probeReader(c *websocket.Conn, want int, deadline time.Time, res *probeResult) {
	for res.events < want {
		_ = c.SetReadDeadline(deadline)
		_, raw, err := c.ReadMessage()
		if err != nil {
			res.err = err
			return
		}
		var f probeEvent
		if err := json.Unmarshal(raw, &f); err != nil || f.Type != "event" ||
			f.Channel != probeChannel {
			continue
		}
		now := time.Now().UnixNano()
		if f.Seq != res.lastSeq+1 {
			if res.gaps == 0 {
				res.gapFrom, res.gapTo = res.lastSeq+1, f.Seq
			}
			res.gaps++
		}
		res.lastSeq = f.Seq
		res.events++
		if f.Data.T0 > 0 {
			if d := now - f.Data.T0; d >= 0 {
				res.latNanos = append(res.latNanos, d)
			}
		}
	}
}

// percentile picks the q-quantile of a sorted copy of the samples.
func percentile(ns []int64, q float64) time.Duration {
	if len(ns) == 0 {
		return 0
	}
	v := append([]int64(nil), ns...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	i := int(float64(len(v)-1) * q)
	return time.Duration(v[i])
}

// probePayload builds a realistic book-delta body carrying the publish
// timestamp for end-to-end latency measurement.
func probePayload(i int) map[string]any {
	return map[string]any{
		"t0_ns": time.Now().UnixNano(), "n": i,
		"bid": "1.08342", "ask": "1.08345", "bid_sz": "1500000",
		"ask_sz": "1200000",
	}
}

// TestLoadProbeSeqContinuityUnderLoad is the sustained-load leg: N real
// WebSocket subscribers on one channel, a producer paced at ≥5k msgs/s,
// and the dual latency measurement. Asserts zero sequence gaps and zero
// server-side slow-consumer drops, and reports the measured latencies.
func TestLoadProbeSeqContinuityUnderLoad(t *testing.T) {
	srv, url := probeServer(t, Config{})

	const subs = 8
	const rate = 6000       // target msgs/s (≥5k required)
	const dur = 7           // seconds of sustained load
	const want = rate * dur // 42000 events per subscriber

	conns := make([]*websocket.Conn, 0, subs)
	for i := 0; i < subs; i++ {
		conns = append(conns, probeDial(t, url))
	}

	results := make([]probeResult, subs)
	var wg sync.WaitGroup
	deadline := time.Now().Add(time.Duration(dur)*time.Second + 15*time.Second)
	for i := range conns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			probeReader(conns[i], want, deadline, &results[i])
		}(i)
	}

	// Paced publisher: absolute-schedule sleeps keep the achieved rate
	// at target even under per-iteration scheduling jitter.
	start := time.Now()
	tick := time.Second / rate
	for i := 1; i <= want; i++ {
		srv.Publish(probeChannel, uint64(i), probePayload(i))
		if d := time.Until(start.Add(time.Duration(i) * tick)); d > 0 {
			time.Sleep(d)
		}
	}
	pubElapsed := time.Since(start)
	wg.Wait()
	totalElapsed := time.Since(start)

	var totalGaps, totalEvents int
	var latAll []int64
	for i := range results {
		r := &results[i]
		if r.err != nil {
			t.Logf("subscriber %d stopped early: events=%d lastSeq=%d err=%v",
				i, r.events, r.lastSeq, r.err)
		}
		totalGaps += r.gaps
		totalEvents += r.events
		latAll = append(latAll, r.latNanos...)
	}

	achieved := float64(want) / pubElapsed.Seconds()
	m := srv.Metrics()
	samples, mean, sP50, sP99, sMax := m.LatencySnapshot()
	cP50 := percentile(latAll, 0.50)
	cP99 := percentile(latAll, 0.99)
	cMax := percentile(latAll, 1.0)

	t.Logf("LOAD: published=%d achieved=%.0f msgs/s over %s (fanout %d subs, %d deliveries)",
		want, achieved, pubElapsed.Round(time.Millisecond), subs, totalEvents)
	t.Logf("GAPS: seq discontinuities across %d subscribers = %d (server FramesDropped=%d)",
		subs, totalGaps, m.FramesDropped.Load())
	t.Logf("LATENCY server-side Publish→enqueue reservoir: samples=%d mean=%s p50=%s p99=%s max=%s",
		samples, mean, sP50, sP99, sMax)
	t.Logf("LATENCY client-observed publish→decode: samples=%d p50=%s p99=%s max=%s",
		len(latAll), cP50, cP99, cMax)
	t.Logf("drain tail after last publish: %s",
		(totalElapsed - pubElapsed).Round(time.Millisecond))

	if achieved < 5000 {
		t.Fatalf("sustained publish rate %.0f msgs/s < 5000 target", achieved)
	}
	if totalGaps != 0 {
		for i := range results {
			if results[i].gaps > 0 {
				t.Logf("subscriber %d: first gap expected seq %d got %d",
					i, results[i].gapFrom, results[i].gapTo)
			}
		}
		t.Fatalf("sequence gaps under load: %d discontinuities", totalGaps)
	}
	for i := range results {
		if results[i].events != want {
			t.Fatalf("subscriber %d received %d/%d events", i, results[i].events, want)
		}
	}
	if m.FramesDropped.Load() != 0 {
		t.Fatalf("server dropped %d frames (slow-consumer saturation)",
			m.FramesDropped.Load())
	}
	// SLA evidence: §24 #99 p99 WS push ≤ 100ms — both lenses must hold.
	if sP99 > 100*time.Millisecond {
		t.Fatalf("server-side push p99 %s exceeds 100ms SLA", sP99)
	}
	if cP99 > 100*time.Millisecond {
		t.Fatalf("client-observed push p99 %s exceeds 100ms SLA", cP99)
	}
}

// TestLoadProbeGapResume is the gap→resync leg: a subscriber dropped
// mid-stream resumes at its last_seq and receives the missed range
// verbatim from the replay ring; a cursor deeper than maxReplayFrames
// and a cursor beyond the ring horizon each take the explicit resync +
// snapshot path (§10.9 item 2, Task 6.3.24).
func TestLoadProbeGapResume(t *testing.T) {
	srv, url := probeServer(t, Config{})

	// Snapshot fallback for the resync-directive legs: the stub reports
	// the channel tail so the client's post-snapshot last_seq resumes
	// the delta chain exactly.
	srv.SetSnapshotSource("book", snapshotFunc(
		func(_ context.Context, _ string) (uint64, any, error) {
			return srv.channelTail(probeChannel),
				map[string]any{"symbol": "EUR/USD", "snapshot": true}, nil
		}))

	// --- Leg 1: drop a subscriber mid-stream, replay-ring resume ------
	c1 := probeDial(t, url)
	const warm = 1000
	for i := 1; i <= warm; i++ {
		srv.Publish(probeChannel, uint64(i), probePayload(i))
	}
	waitSeq(t, c1, warm)

	lastSeen := uint64(warm)
	_ = c1.Close() // mid-stream drop — server keeps publishing into the ring

	const missed = 600
	for i := warm + 1; i <= warm+missed; i++ {
		srv.Publish(probeChannel, uint64(i), probePayload(i))
	}

	c2 := probeDialNoSub(t, url)
	sendJSON(t, c2, map[string]any{
		"action": "resume", "channel": probeChannel, "last_seq": lastSeen,
	})
	var replayed []uint64
	m := readUntil(t, c2, 5*time.Second, func(f map[string]any) bool {
		if f["type"] == "event" {
			replayed = append(replayed, uint64(f["seq"].(float64)))
		}
		return f["type"] == "resumed"
	})
	if len(replayed) != missed {
		t.Fatalf("replayed %d frames, want %d", len(replayed), missed)
	}
	for i, s := range replayed {
		if s != lastSeen+1+uint64(i) {
			t.Fatalf("replay discontinuity at idx %d: seq=%d", i, s)
		}
	}
	if uint64(m["from_seq"].(float64)) != lastSeen+1 ||
		uint64(m["to_seq"].(float64)) != lastSeen+uint64(missed) ||
		int(m["count"].(float64)) != missed {
		t.Fatalf("resumed frame %+v", m)
	}

	// Live stream resumes verbatim after the replay.
	srv.Publish(probeChannel, uint64(warm+missed+1), probePayload(warm+missed+1))
	live := readUntil(t, c2, 3*time.Second, func(f map[string]any) bool {
		return f["type"] == "event"
	})
	if uint64(live["seq"].(float64)) != uint64(warm+missed+1) {
		t.Fatalf("post-resume live seq %v, want %d", live["seq"], warm+missed+1)
	}
	t.Logf("RESYNC leg1: dropped subscriber resumed at seq %d — %d missed frames replayed verbatim, live stream continued",
		lastSeen, missed)

	// --- Leg 2: in-horizon but deep cursor → resync directive ---------
	// Cursor inside the ring but >maxReplayFrames (1000) behind the tail
	// takes the explicit resync + snapshot path rather than flooding.
	tail := srv.channelTail(probeChannel) // = warm+missed+1 = 1601
	sendJSON(t, c2, map[string]any{
		"action": "resync", "channel": probeChannel,
		"last_seq": tail - 1500, // 1501 buffered frames > 1000 replay bound
	})
	rs := readUntil(t, c2, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == "resync"
	})
	if rs["reason"] != "gap_too_large" || rs["channel"] != probeChannel {
		t.Fatalf("resync directive %+v", rs)
	}
	snap := readUntil(t, c2, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == "snapshot"
	})
	if uint64(snap["seq"].(float64)) != tail {
		t.Fatalf("snapshot seq %v, want tail %d", snap["seq"], tail)
	}
	t.Logf("RESYNC leg2: deep cursor (last_seq=%d, tail=%d) → resync directive + snapshot at seq %d",
		tail-1500, tail, uint64(snap["seq"].(float64)))

	// --- Leg 3: cursor beyond the ring horizon → gap_too_large --------
	// Push the ring past its 10,000-message cap so an ancient cursor is
	// genuinely outside the replay horizon (verdict ReplayGapTooLarge).
	const overflow = 10500
	for i := warm + missed + 2; i <= warm+missed+2+overflow; i++ {
		srv.Publish(probeChannel, uint64(i), probePayload(i))
	}
	tail = srv.channelTail(probeChannel)
	sendJSON(t, c2, map[string]any{
		"action": "resync", "channel": probeChannel, "last_seq": 1,
	})
	rs = readUntil(t, c2, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == "resync"
	})
	if rs["reason"] != "gap_too_large" {
		t.Fatalf("horizon resync %+v", rs)
	}
	snap = readUntil(t, c2, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == "snapshot"
	})
	if uint64(snap["seq"].(float64)) != tail {
		t.Fatalf("horizon snapshot seq %v, want tail %d", snap["seq"], tail)
	}
	t.Logf("RESYNC leg3: cursor seq 1 vs ring horizon (tail=%d) → resync gap_too_large + snapshot",
		tail)

	if got := srv.Metrics().ResyncDirectives.Load(); got < 2 {
		t.Fatalf("ResyncDirectives=%d, want ≥2", got)
	}
}

// probeDialNoSub opens a conn without subscribing (resume binds the
// subscription idempotently itself).
func probeDialNoSub(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// waitSeq reads until an event with the given seq arrives.
func waitSeq(t *testing.T, c *websocket.Conn, seq uint64) {
	t.Helper()
	readUntil(t, c, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == "event" && f["seq"] == float64(seq)
	})
}
