package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func testEnv() *Env {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &Env{Now: func() time.Time { return now }}
}

// The correctness gate: a unit producing fewer keys than MinKeys is an
// error — never a fresh marker.
func TestMinKeysGate(t *testing.T) {
	u := Unit{Name: "u", Priority: P1, MinKeys: 3,
		Warm: func(context.Context, *Env) (int, error) { return 1, nil }}
	w := New(testEnv(), []Unit{u})
	rep := w.Run(context.Background(), "test")
	if rep.OK() {
		t.Fatal("below-min-keys warm reported ok")
	}
	if rep.Units[0].Status != "error" {
		t.Fatalf("unit status = %q", rep.Units[0].Status)
	}
}

func TestFailingUnitNotFresh(t *testing.T) {
	u := Unit{Name: "u", Priority: P0, MinKeys: 0,
		Warm: func(context.Context, *Env) (int, error) {
			return 0, errors.New("source down")
		}}
	w := New(testEnv(), []Unit{u})
	rep := w.Run(context.Background(), "recovery")
	if rep.Units[0].Status != "error" || rep.Units[0].FreshUntil != "" {
		t.Fatalf("failed unit must carry no freshness claim: %+v", rep.Units[0])
	}
}

// Priority ordering: all P0 run before any P1.
func TestPriorityOrder(t *testing.T) {
	var seq []string
	mk := func(name string, p Priority) Unit {
		return Unit{Name: name, Priority: p,
			Warm: func(context.Context, *Env) (int, error) {
				seq = append(seq, name)
				return 1, nil
			}}
	}
	w := New(testEnv(), []Unit{mk("p1a", P1), mk("p0a", P0), mk("p1b", P1), mk("p0b", P0)})
	rep := w.Run(context.Background(), "deploy")
	if !rep.OK() || len(rep.Units) != 4 {
		t.Fatalf("report: %+v", rep)
	}
	if seq[0] != "p0a" || seq[1] != "p0b" {
		t.Fatalf("P0 units did not run first: %v", seq)
	}
}

// A context-budget overrun surfaces as a unit error (fail-closed),
// not a hang.
func TestBudgetOverrun(t *testing.T) {
	u := Unit{Name: "slow", Priority: P0,
		Warm: func(ctx context.Context, _ *Env) (int, error) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(10 * time.Second):
				return 1, nil
			}
		}}
	w := New(testEnv(), []Unit{u})
	// Run with an already-cancelled parent ctx — P0 budget ctx dies too.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := w.Run(ctx, "deploy")
	if rep.Units[0].Status != "error" {
		t.Fatalf("cancelled unit status = %q", rep.Units[0].Status)
	}
}

// Envelope freshness: wrong generation or expired age → cold.
func TestEnvelopeFreshness(t *testing.T) {
	now := time.Now()
	raw, err := Wrap(7, now, map[string]string{"a": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := UnwrapFresh(raw, 7, time.Minute, now); !ok {
		t.Fatal("fresh envelope rejected")
	}
	if _, ok := UnwrapFresh(raw, 8, time.Minute, now); ok {
		t.Fatal("wrong generation accepted as fresh")
	}
	if _, ok := UnwrapFresh(raw, 7, time.Minute, now.Add(2*time.Minute)); ok {
		t.Fatal("expired envelope accepted as fresh")
	}
	if _, ok := UnwrapFresh([]byte("{garbage"), 7, time.Minute, now); ok {
		t.Fatal("corrupt envelope accepted")
	}
}

// Trigger labels propagate into the report.
func TestTriggerDefault(t *testing.T) {
	w := New(testEnv(), nil)
	rep := w.Run(context.Background(), "")
	if rep.Trigger != "manual" {
		t.Fatalf("trigger = %q", rep.Trigger)
	}
}

// Concurrent Run calls must not corrupt the units slice or panic.
func TestConcurrentRuns(t *testing.T) {
	var calls atomic.Int64
	u := Unit{Name: "u", Priority: P0,
		Warm: func(context.Context, *Env) (int, error) { calls.Add(1); return 1, nil }}
	w := New(testEnv(), []Unit{u})
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			w.Run(context.Background(), "test")
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if calls.Load() != 4 {
		t.Fatalf("warm calls = %d, want 4", calls.Load())
	}
}
