// Task 6.3.2 — conflation, sequencing and checksum tests.
package marketdata

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// emitSink captures Publish calls (channel, seq, data) for assertions.
type emitSink struct {
	mu   sync.Mutex
	rows []emitRow
	sig  chan struct{}
}

type emitRow struct {
	channel string
	seq     uint64
	data    any
}

func newEmitSink() *emitSink {
	return &emitSink{sig: make(chan struct{}, 512)}
}

func (s *emitSink) emit(channel string, seq uint64, data any) {
	s.mu.Lock()
	s.rows = append(s.rows, emitRow{channel, seq, data})
	s.mu.Unlock()
	s.sig <- struct{}{}
}

// waitFor blocks until n rows captured or times out.
func (s *emitSink) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		have := len(s.rows)
		s.mu.Unlock()
		if have >= n {
			return
		}
		select {
		case <-s.sig:
		case <-deadline:
			t.Fatalf("timed out waiting for %d emits (have %d)", n, have)
		}
	}
}

func (s *emitSink) get(i int) emitRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[i]
}

func mkDelta(sym string, bidPx int64) BookDelta {
	return BookDelta{
		Symbol: sym,
		Bids:   []Level{{Price: bidPx, Qty: 1_000_000_000, Count: 2}},
		Asks:   []Level{{Price: bidPx + 5000, Qty: 2_000_000_000, Count: 1}},
		Ts:     time.Now(),
	}
}

func runConflator(t *testing.T, cfg ConflatorConfig, seq SeqStore) (*Conflator, *emitSink) {
	t.Helper()
	sink := newEmitSink()
	c := NewConflator(cfg, nil, seq, sink.emit, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && err != context.Canceled {
			t.Errorf("conflator run: %v", err)
		}
	})
	return c, sink
}

func decodeUpdate(t *testing.T, row emitRow) depthUpdate {
	t.Helper()
	b, err := json.Marshal(row.data)
	if err != nil {
		t.Fatalf("marshal emit data: %v", err)
	}
	var u depthUpdate
	if err := json.Unmarshal(b, &u); err != nil {
		t.Fatalf("unmarshal depthUpdate: %v", err)
	}
	return u
}

// TestConflationWindowFlush — deltas coalesce inside the 100ms window;
// one frame per symbol per window carrying first_seq..last_seq.
func TestConflationWindowFlush(t *testing.T) {
	c, sink := runConflator(t, ConflatorConfig{
		Window:    40 * time.Millisecond,
		MaxEvents: 1000,
		Depth:     20,
	}, NewMemSeqStore())

	c.Push(mkDelta("EUR/USD", 108_000_000))
	c.Push(mkDelta("EUR/USD", 108_010_000))
	c.Push(mkDelta("EUR/USD", 108_020_000))

	sink.waitFor(t, 2) // book@ + depth@ emit per flush
	row := sink.get(0)
	if row.channel != "book@EUR/USD" {
		t.Fatalf("first emit channel = %q", row.channel)
	}
	u := decodeUpdate(t, row)
	if u.Event != "depthUpdate" || u.Symbol != "EUR/USD" {
		t.Fatalf("bad update envelope: %+v", u)
	}
	if u.Coalesced != 3 {
		t.Fatalf("coalesced = %d, want 3", u.Coalesced)
	}
	if u.FirstSeq != 1 || u.LastSeq != 3 || u.PrevLastSeq != 0 || u.Seq != 3 {
		t.Fatalf("seq envelope %+v — want first=1 last=3 prev=0", u)
	}
	if len(u.Bids) != 1 || u.Bids[0][0] != "1.0802" {
		t.Fatalf("bids = %v — want latest price 1.0802", u.Bids)
	}
	if u.CRC32 == 0 {
		t.Fatal("crc32 not populated")
	}
}

// TestConflationCountFlush — 100 (test: 5) coalesced deltas flush
// immediately, without waiting for the window.
func TestConflationCountFlush(t *testing.T) {
	c, sink := runConflator(t, ConflatorConfig{
		Window:    time.Hour, // window must NOT fire in the test
		MaxEvents: 5,
		Depth:     20,
	}, NewMemSeqStore())

	for i := 0; i < 5; i++ {
		c.Push(mkDelta("EUR/USD", int64(108_000_000+i)))
	}
	// Flush is count-triggered: emit arrives without the window expiring.
	sink.waitFor(t, 2)
	u := decodeUpdate(t, sink.get(0))
	if u.Coalesced != 5 || u.FirstSeq != 1 || u.LastSeq != 5 {
		t.Fatalf("count-flush update %+v — want coalesced=5 seq 1..5", u)
	}
	if c.Metrics().FlushesByCount.Load() != 1 {
		t.Fatalf("FlushesByCount = %d", c.Metrics().FlushesByCount.Load())
	}
}

// TestSeqOrderingPerSymbol — every delta consumes one seq; sequences are
// strictly monotonic per symbol and independent across symbols.
func TestSeqOrderingPerSymbol(t *testing.T) {
	c, sink := runConflator(t, ConflatorConfig{
		Window:    20 * time.Millisecond,
		MaxEvents: 2,
		Depth:     20,
	}, NewMemSeqStore())

	c.Push(mkDelta("EUR/USD", 1))
	c.Push(mkDelta("EUR/USD", 2)) // flush 1..2
	c.Push(mkDelta("GBP/USD", 1)) // own domain
	c.Push(mkDelta("EUR/USD", 3))
	c.Push(mkDelta("EUR/USD", 4)) // flush 3..4

	sink.waitFor(t, 6) // 3 flushes × (book@ + depth@)

	var eur, gbp []uint64
	sink.mu.Lock()
	for _, r := range sink.rows {
		u := depthUpdate{}
		b, _ := json.Marshal(r.data)
		_ = json.Unmarshal(b, &u)
		switch u.Symbol {
		case "EUR/USD":
			eur = append(eur, u.Seq)
		case "GBP/USD":
			gbp = append(gbp, u.Seq)
		}
	}
	sink.mu.Unlock()

	// EUR/USD emits twice (seq 2, seq 4) on both channels; GBP once (3).
	wantEur := []uint64{2, 2, 4, 4}
	if len(eur) != 4 {
		t.Fatalf("EUR/USD seqs %v, want %v", eur, wantEur)
	}
	for i := range wantEur {
		if eur[i] != wantEur[i] {
			t.Fatalf("EUR/USD seqs %v, want %v", eur, wantEur)
		}
	}
	if len(gbp) != 2 || gbp[0] != 1 {
		t.Fatalf("GBP/USD seqs %v, want [1 1]", gbp)
	}
}

// TestSeqPrevChain — prev_last_seq of flush N equals last_seq of flush
// N-1 (the §10.9 contiguity envelope clients validate).
func TestSeqPrevChain(t *testing.T) {
	c, sink := runConflator(t, ConflatorConfig{
		Window:    20 * time.Millisecond,
		MaxEvents: 2,
		Depth:     20,
	}, NewMemSeqStore())

	c.Push(mkDelta("EUR/USD", 1))
	c.Push(mkDelta("EUR/USD", 2))
	c.Push(mkDelta("EUR/USD", 3))
	c.Push(mkDelta("EUR/USD", 4))
	sink.waitFor(t, 4)

	first := decodeUpdate(t, sink.get(0))
	second := decodeUpdate(t, sink.get(2))
	if first.PrevLastSeq != 0 || first.LastSeq != 2 {
		t.Fatalf("first frame %+v", first)
	}
	if second.PrevLastSeq != 2 || second.LastSeq != 4 {
		t.Fatalf("second frame %+v — prev must chain to 2", second)
	}
}

// TestDepthCRC32Stable — the checksum is deterministic over level content.
func TestDepthCRC32Stable(t *testing.T) {
	bids := []Level{{Price: 100, Qty: 5, Count: 1}, {Price: 99, Qty: 3, Count: 2}}
	asks := []Level{{Price: 101, Qty: 7, Count: 1}}
	a := depthChecksum(bids, asks, 20)
	b := depthChecksum(bids, asks, 20)
	if a != b {
		t.Fatal("crc32 not deterministic")
	}
	c := depthChecksum(append([]Level{}, bids...), append([]Level{}, asks...), 20)
	if a != c {
		t.Fatal("crc32 unstable across copies")
	}
	diff := depthChecksum([]Level{{Price: 102, Qty: 5, Count: 1}, bids[1]}, asks, 20)
	if diff == a {
		t.Fatal("crc32 did not change with level content")
	}
	trunc := depthChecksum(append(bids, Level{Price: 1, Qty: 1}), asks, 2)
	if trunc != a {
		t.Fatal("depth cap not applied to checksum")
	}
}

// TestConflatorSnapshot — SnapshotSource returns latest book with
// is_snapshot and the channel cursor seq.
func TestConflatorSnapshot(t *testing.T) {
	c, sink := runConflator(t, ConflatorConfig{
		Window:    time.Hour,
		MaxEvents: 2,
		Depth:     20,
	}, NewMemSeqStore())
	c.Push(mkDelta("EUR/USD", 108_000_000))
	c.Push(mkDelta("EUR/USD", 108_010_000))
	sink.waitFor(t, 2)

	seq, data, err := c.Snapshot(context.Background(), "book@EUR/USD")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if seq != 2 {
		t.Fatalf("snapshot seq = %d, want 2", seq)
	}
	u, ok := data.(depthUpdate)
	if !ok {
		t.Fatalf("snapshot type %T", data)
	}
	if !u.Snapshot || u.Symbol != "EUR/USD" || len(u.Bids) != 1 {
		t.Fatalf("snapshot payload %+v", u)
	}
	if _, _, err := c.Snapshot(context.Background(), "trades@EUR/USD"); err == nil {
		t.Fatal("snapshot accepted non-book channel type")
	}
	if _, _, err := c.Snapshot(context.Background(), "book@USD/MXN"); err == nil {
		t.Fatal("snapshot for unseen symbol accepted")
	}
}

// TestSeqMirrorStore — emitted seqs land in the SeqStore.
func TestSeqMirrorStore(t *testing.T) {
	store := NewMemSeqStore()
	c, sink := runConflator(t, ConflatorConfig{
		Window:    10 * time.Millisecond,
		MaxEvents: 2,
		Depth:     20,
	}, store)
	c.Push(mkDelta("EUR/USD", 1))
	c.Push(mkDelta("EUR/USD", 2))
	sink.waitFor(t, 2)

	deadline := time.Now().Add(2 * time.Second)
	for {
		v, _ := store.Load(context.Background(), "EUR/USD")
		if v == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("md:seq mirror = %d, want 2", v)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
