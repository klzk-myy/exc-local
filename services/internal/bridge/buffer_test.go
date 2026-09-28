package bridge

import (
	"fmt"
	"testing"
)

func TestBufferFIFO(t *testing.T) {
	b := newEventBuffer(4)
	for i := 0; i < 4; i++ {
		if ev := b.Push(bufferedEvent{seq: uint64(i), subjects: []string{"s"}}); ev {
			t.Fatalf("push %d unexpectedly evicted", i)
		}
	}
	if b.Len() != 4 {
		t.Fatalf("len=%d want 4", b.Len())
	}
	for i := 0; i < 4; i++ {
		e, _, ok := b.Head()
		if !ok || e.seq != uint64(i) {
			t.Fatalf("head %d: got %+v ok=%v", i, e, ok)
		}
		b.Pop()
	}
	if _, _, ok := b.Head(); ok || b.Len() != 0 {
		t.Fatal("buffer should be drained")
	}
}

func TestBufferBoundedDropOldest(t *testing.T) {
	const cap = 100
	b := newEventBuffer(cap)
	evicted := 0
	for i := 0; i < cap+10; i++ {
		if b.Push(bufferedEvent{seq: uint64(i)}) {
			evicted++
		}
	}
	if b.Len() != cap {
		t.Fatalf("len=%d want %d (bound violated)", b.Len(), cap)
	}
	if evicted != 10 {
		t.Fatalf("evicted=%d want 10", evicted)
	}
	// Oldest surviving event must be seq 10 (seqs 0-9 evicted), and order
	// is preserved for replay after reconnect.
	for i := 10; i < cap+10; i++ {
		e, _, ok := b.Head()
		if !ok || e.seq != uint64(i) {
			t.Fatalf("replay order broken: got seq %d want %d", e.seq, i)
		}
		b.Pop()
	}
}

// When the head is evicted mid-fanout (buffer full during a publish
// stall), the generation check lets the drain loop detect it and restart
// on the new head instead of publishing a stale event's leftovers.
func TestBufferGenDetectsHeadEviction(t *testing.T) {
	b := newEventBuffer(2)
	b.Push(bufferedEvent{seq: 1, subjects: []string{"a", "b"}})
	b.Push(bufferedEvent{seq: 2})

	e, gen, ok := b.Head()
	if !ok || e.seq != 1 {
		t.Fatal("head should be seq 1")
	}
	// Simulate: drain published subject "a", stalls on "b"; meanwhile the
	// buffer fills and evicts the head.
	b.Push(bufferedEvent{seq: 3}) // evicts seq 1
	if b.HeadGen() == gen {
		t.Fatal("gen must change on head eviction")
	}
	if b.PopIfGen(gen) {
		t.Fatal("PopIfGen must refuse a stale generation")
	}
	e2, _, ok := b.Head()
	if !ok || e2.seq != 2 {
		t.Fatalf("new head seq=%d want 2", e2.seq)
	}
	// Current generation pops fine.
	_, gen2, _ := b.Head()
	if !b.PopIfGen(gen2) {
		t.Fatal("PopIfGen must pop on matching generation")
	}
}

func TestBufferInterleavedPushPop(t *testing.T) {
	b := newEventBuffer(8)
	for i := 0; i < 1000; i++ {
		b.Push(bufferedEvent{seq: uint64(i)})
		e, _, ok := b.Head()
		if !ok || e.seq != uint64(i) {
			t.Fatalf("i=%d head seq=%d ok=%v", i, e.seq, ok)
		}
		b.Pop()
		if b.Len() != 0 {
			t.Fatal("drain must empty buffer")
		}
	}
}

func BenchmarkBufferPush(b *testing.B) {
	buf := newEventBuffer(100_000)
	payload := []byte(fmt.Sprintf("%0128d", 0))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf.Push(bufferedEvent{seq: uint64(i), payload: payload,
			subjects: []string{"trades.0.EUR-USD", "settlements.0.EUR-USD"}})
		buf.Pop()
	}
}
