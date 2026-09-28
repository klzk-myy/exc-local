package marketdata

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestDelayGate_NeverReleasesEarly — the shared gate behind
// liquidations@ and blockTrades@ must hold every item for the FULL
// delay regardless of push order.
func TestDelayGate_NeverReleasesEarly(t *testing.T) {
	var mu sync.Mutex
	var released []int
	g := NewDelayGate[int](50*time.Millisecond, func(v int) {
		mu.Lock()
		released = append(released, v)
		mu.Unlock()
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()

	start := time.Now()
	for i := 0; i < 5; i++ {
		g.Push(i, time.Now())
	}
	time.Sleep(25 * time.Millisecond) // inside the window
	mu.Lock()
	if len(released) != 0 {
		t.Fatalf("released %d items early", len(released))
	}
	mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(released)
		mu.Unlock()
		if n == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/5 released", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("released in %v — delay violated", elapsed)
	}
	if g.Released() != 5 {
		t.Fatalf("Released=%d", g.Released())
	}
}

// TestDelayGate_FIFOOrder — same-anchor items release in push order
// (heap tie-break on insertion seq).
func TestDelayGate_FIFOOrder(t *testing.T) {
	var mu sync.Mutex
	var order []int
	g := NewDelayGate[int](20*time.Millisecond, func(v int) {
		mu.Lock()
		order = append(order, v)
		mu.Unlock()
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	now := time.Now()
	for i := 0; i < 4; i++ {
		g.Push(i, now) // identical anchor → seq breaks the tie
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < 4; i++ {
		if order[i] != i {
			t.Fatalf("release order %v", order)
		}
	}
}

// TestDelayGate_AgedEventReleasesEarlier — an item anchored in the past
// (replay context) releases as soon as its ts+delay passes; the delay
// is measured from the EVENT, not from arrival.
func TestDelayGate_AgedEventReleasesEarlier(t *testing.T) {
	out := make(chan int, 1)
	g := NewDelayGate[int](50*time.Millisecond, func(v int) { out <- v }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	g.Push(9, time.Now().Add(-time.Second)) // ts+delay already elapsed
	select {
	case v := <-out:
		if v != 9 {
			t.Fatalf("v=%d", v)
		}
	case <-time.After(time.Second):
		t.Fatal("aged item should release promptly")
	}
}

// TestDelayGate_CancelDropsPending — cancellation drops the queue; the
// feed is restart-unsafe by design (upstream re-delivers).
func TestDelayGate_CancelDropsPending(t *testing.T) {
	released := make(chan int, 4)
	g := NewDelayGate[int](time.Hour, func(v int) { released <- v }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()

	g.Push(1, time.Now())
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run must return ctx error")
	}
	select {
	case v := <-released:
		t.Fatalf("released %d after cancel", v)
	case <-time.After(50 * time.Millisecond):
	}
}
