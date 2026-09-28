// Task 6.3.9 — ring buffer replay semantics.
package marketdata

import (
	"fmt"
	"testing"
	"time"
)

func TestRingReplayInRange(t *testing.T) {
	r := newRingBuffer(10, time.Minute, time.Now)
	for i := 1; i <= 6; i++ {
		r.append(uint64(i), []byte(fmt.Sprintf(`{"seq":%d}`, i)))
	}
	res := r.replay(2)
	if res.Verdict != ReplayOK {
		t.Fatalf("verdict = %v", res.Verdict)
	}
	if len(res.Msgs) != 4 || string(res.Msgs[0]) != `{"seq":3}` {
		t.Fatalf("msgs = %q", res.Msgs)
	}
	if res.Tail != 6 || res.Oldest != 1 {
		t.Fatalf("tail=%d oldest=%d", res.Tail, res.Oldest)
	}
}

func TestRingReplayUpToDate(t *testing.T) {
	r := newRingBuffer(10, time.Minute, time.Now)
	r.append(7, []byte(`{"seq":7}`))
	if res := r.replay(7); res.Verdict != ReplayUpToDate {
		t.Fatalf("verdict = %v, want ReplayUpToDate", res.Verdict)
	}
}

func TestRingReplayGapTooLarge(t *testing.T) {
	r := newRingBuffer(4, time.Minute, time.Now)
	for i := 1; i <= 9; i++ {
		r.append(uint64(i), []byte(fmt.Sprintf("%d", i)))
	}
	// Ring holds seqs 6..9; cursor 1 → gap.
	res := r.replay(1)
	if res.Verdict != ReplayGapTooLarge {
		t.Fatalf("verdict = %v, want ReplayGapTooLarge", res.Verdict)
	}
	if res.Oldest != 6 || res.Tail != 9 {
		t.Fatalf("oldest=%d tail=%d", res.Oldest, res.Tail)
	}
}

func TestRingReplayInvalidSeq(t *testing.T) {
	r := newRingBuffer(10, time.Minute, time.Now)
	r.append(3, []byte("x"))
	if res := r.replay(9); res.Verdict != ReplayInvalidSeq {
		t.Fatalf("verdict = %v, want ReplayInvalidSeq", res.Verdict)
	}
}

func TestRingReplayEmpty(t *testing.T) {
	r := newRingBuffer(10, time.Minute, time.Now)
	if res := r.replay(0); res.Verdict != ReplayEmpty {
		t.Fatalf("verdict = %v, want ReplayEmpty", res.Verdict)
	}
}

func TestRingAgeEviction(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	r := newRingBuffer(100, 60*time.Second, clock)
	r.append(1, []byte("a"))
	now = now.Add(61 * time.Second)
	r.append(2, []byte("b"))
	// The aged entry evicted on append — only seq 2 remains.
	if r.len() != 1 {
		t.Fatalf("len = %d — aged entry must evict", r.len())
	}
	// Cursor 0 → 1 predates oldest (2): gap.
	res := r.replay(0)
	if res.Verdict != ReplayGapTooLarge || res.Oldest != 2 {
		t.Fatalf("verdict=%v oldest=%d — cursor behind evicted seq is a gap",
			res.Verdict, res.Oldest)
	}
	// Cursor 1 = oldest-1 → still covered: replay seq 2.
	res = r.replay(1)
	if res.Verdict != ReplayOK || len(res.Msgs) != 1 {
		t.Fatalf("verdict=%v msgs=%d", res.Verdict, len(res.Msgs))
	}
}

func TestRingNonMonotonicAppendRejected(t *testing.T) {
	r := newRingBuffer(10, time.Minute, time.Now)
	r.append(5, []byte("five"))
	r.append(5, []byte("dup"))
	r.append(4, []byte("backwards"))
	if r.len() != 1 {
		t.Fatalf("len = %d — non-monotonic appends must drop", r.len())
	}
}
