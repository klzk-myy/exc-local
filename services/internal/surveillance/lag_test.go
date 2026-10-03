package surveillance

// Phase-3 Task 6 (IMP-PLAN) — §24 #392 lag probe coverage.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"exchange/internal/marketdata"
	"exchange/internal/observability"
)

type capSink struct{ alerts chan observability.Alert }

func (s *capSink) Raise(_ context.Context, a observability.Alert) error {
	s.alerts <- a
	return nil
}

type capPub struct{ msgs chan string }

func (p *capPub) Publish(_ context.Context, subj string, _ []byte) error {
	p.msgs <- subj
	return nil
}

func TestWatchLagFiresThenResolves(t *testing.T) {
	var lag atomic.Uint64
	lag.Store(20_000)
	probe := func(context.Context) (uint64, error) { return lag.Load(), nil }
	sink := &capSink{alerts: make(chan observability.Alert, 4)}
	pub := &capPub{msgs: make(chan string, 4)}
	eval := observability.NewEvaluator(sink, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchLag(ctx, probe, eval, pub, time.Millisecond)

	select {
	case a := <-sink.alerts:
		if a.Code != LagWarningCode || a.Status != "firing" ||
			a.Severity != observability.SeverityP2 {
			t.Fatalf("unexpected alert %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no firing alert within 2s")
	}
	select {
	case subj := <-pub.msgs:
		if subj != AutoscaleSubject {
			t.Fatalf("autoscale subject %q", subj)
		}
	case <-time.After(time.Second):
		t.Fatal("no autoscale hook")
	}

	lag.Store(0) // backlog drained → resolved edge
	select {
	case a := <-sink.alerts:
		if a.Status != "resolved" || a.Code != LagWarningCode {
			t.Fatalf("unexpected resolution %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no resolved alert")
	}
}

func TestWatchLagBelowThresholdSilent(t *testing.T) {
	probe := func(context.Context) (uint64, error) { return 9_999, nil }
	sink := &capSink{alerts: make(chan observability.Alert, 2)}
	eval := observability.NewEvaluator(sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchLag(ctx, probe, eval, nil, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	select {
	case a := <-sink.alerts:
		t.Fatalf("sub-threshold alert fired: %+v", a)
	default:
	}
	if eval.Firing("surveillance-l3-lag") {
		t.Fatal("rule must not be firing under threshold")
	}
}

func TestDetectionLatencyAccounting(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	eng, err := NewEngine(Config{Now: func() time.Time { return now }},
		&collectSink{})
	if err != nil {
		t.Fatal(err)
	}
	ev := ev("EURUSD", marketdata.L3Add, 1, 1,
		marketdata.SideBuy, "1.10", 1, now.Add(-250*time.Millisecond))
	eng.Apply(context.Background(), ev)
	if got := eng.DetectionLatency(); got != 250*time.Millisecond {
		t.Fatalf("detection latency %v, want 250ms", got)
	}
	if eng.AppliedCount() != 1 {
		t.Fatalf("applied %d, want 1", eng.AppliedCount())
	}
}
