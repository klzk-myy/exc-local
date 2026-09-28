// alerts_test.go — rule semantics against a captured sink: delta (L0),
// sustained threshold (L1/30s), rolling ratio (L2 spike), threshold
// (7.3.8 limits), anomaly detection.
package observability

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type capSink struct {
	mu     sync.Mutex
	alerts []Alert
}

func (c *capSink) Raise(_ context.Context, a Alert) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alerts = append(c.alerts, a)
	return nil
}

func (c *capSink) list() []Alert {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Alert(nil), c.alerts...)
}

func (c *capSink) firing(ruleID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, a := range c.alerts {
		if a.Rule == ruleID && a.Status == "firing" {
			n++
		}
	}
	return n
}

func TestDeltaRuleL0FiresImmediately(t *testing.T) {
	sink := &capSink{}
	ev := NewEvaluator(sink, nil)
	var l0 float64
	ev.AddDelta("l0_errors", SeverityP0, "L0_ERROR_OBSERVED", "L0 seen",
		func() float64 { return l0 })
	now := time.Now()

	ev.EvalOnce(now) // baseline
	if sink.firing("l0_errors") != 0 {
		t.Fatal("fired on baseline")
	}
	l0 = 1
	ev.EvalOnce(now.Add(10 * time.Second))
	if sink.firing("l0_errors") != 1 {
		t.Fatal("L0 delta did not fire")
	}
	a := sink.list()[0]
	if a.Severity != SeverityP0 || a.Status != "firing" {
		t.Fatalf("alert = %+v", a)
	}
	// flat next tick → auto-resolved
	ev.EvalOnce(now.Add(20 * time.Second))
	alerts := sink.list()
	if len(alerts) != 2 || alerts[1].Status != "resolved" {
		t.Fatalf("expected resolve, got %+v", alerts)
	}
}

func TestThresholdForDuration(t *testing.T) {
	sink := &capSink{}
	ev := NewEvaluator(sink, nil)
	var l1 float64
	ev.AddThreshold("l1", SeverityP1, "L1_SUSTAINED", "L1 sustained",
		30*time.Second, func() float64 { return l1 }, 0)
	now := time.Now()

	l1 = 1
	ev.EvalOnce(now)                       // breach starts
	ev.EvalOnce(now.Add(10 * time.Second)) // still breaching, <30s
	if sink.firing("l1") != 0 {
		t.Fatal("fired before For elapsed")
	}
	ev.EvalOnce(now.Add(35 * time.Second)) // sustained ≥30s → fire
	if sink.firing("l1") != 1 {
		t.Fatal("did not fire after For")
	}
	// dip below → pending resets
	l1 = 0
	ev.EvalOnce(now.Add(45 * time.Second)) // resolve
	l1 = 1
	ev.EvalOnce(now.Add(50 * time.Second)) // breach restarts
	if n := sink.firing("l1"); n != 1 {
		t.Fatalf("fired %d times — pending must reset after clear", n)
	}
}

func TestRatioWindowRule(t *testing.T) {
	sink := &capSink{}
	ev := NewEvaluator(sink, nil)
	var errs, total float64
	ev.AddRatioWindow("l2_spike", SeverityP2, "L2_REJECTION_SPIKE",
		"L2 spike", time.Minute,
		func() float64 { return errs }, func() float64 { return total },
		0.05, 10)
	now := time.Now()

	// healthy traffic: 1% rejection — min-den not met until total crosses 10
	for i := 0; i < 4; i++ {
		total += 100
		errs += 1
		ev.EvalOnce(now.Add(time.Duration(i) * 10 * time.Second))
	}
	if sink.firing("l2_spike") != 0 {
		t.Fatal("fired at 1%")
	}
	// spike: 40 errors on 50 new requests inside the window
	errs += 40
	total += 50
	ev.EvalOnce(now.Add(50 * time.Second))
	if sink.firing("l2_spike") != 1 {
		t.Fatalf("did not fire; alerts=%+v", sink.list())
	}
	// window slides past the spike → resolves
	for i := 0; i < 8; i++ {
		total += 100
		ev.EvalOnce(now.Add(60*time.Second + time.Duration(i)*10*time.Second))
	}
	last := sink.list()[len(sink.list())-1]
	if last.Status != "resolved" {
		t.Fatalf("expected resolution, last=%+v", last)
	}
}

func TestRatioWindowLowTrafficInert(t *testing.T) {
	sink := &capSink{}
	ev := NewEvaluator(sink, nil)
	var errs, total float64
	ev.AddRatioWindow("l2_spike", SeverityP2, "L2_REJECTION_SPIKE",
		"L2 spike", time.Minute,
		func() float64 { return errs }, func() float64 { return total },
		0.05, 100)
	now := time.Now()
	errs, total = 5, 10 // 50% but below min-denominator
	ev.EvalOnce(now)
	ev.EvalOnce(now.Add(10 * time.Second))
	if sink.firing("l2_spike") != 0 {
		t.Fatal("fired below min denominator")
	}
}

func TestStandardRulesTask738(t *testing.T) {
	sink := &capSink{}
	ev := NewEvaluator(sink, nil)
	var lag, buf, pending, hbAge float64
	AddStandardRules(ev, StandardSources{
		AeronSubscriberLag:  func() float64 { return lag },
		BridgeBufferDepth:   func() float64 { return buf },
		NATSConsumerPending: func() float64 { return pending },
		BridgeHeartbeatAge:  func() float64 { return hbAge },
	})
	now := time.Now()
	ev.EvalOnce(now)
	if len(sink.list()) != 0 {
		t.Fatalf("unexpected alerts %+v", sink.list())
	}
	lag = 1500 // > 1000 → P2
	buf = 15000
	pending = 60000
	hbAge = 20
	ev.EvalOnce(now.Add(10 * time.Second))
	fired := map[string]string{}
	for _, a := range sink.list() {
		fired[a.Rule] = a.Severity
	}
	want := map[string]string{
		"aeron_subscriber_lag":   SeverityP2,
		"bridge_buffer_depth":    SeverityP1,
		"nats_consumer_pending":  SeverityP1,
		"bridge_heartbeat_stale": SeverityP1,
	}
	for r, sev := range want {
		if fired[r] != sev {
			t.Fatalf("rule %s severity = %q want %q (alerts=%v)", r, fired[r], sev, sink.list())
		}
	}
}

func TestErrorRateAnomaly(t *testing.T) {
	sink := &capSink{}
	ev := NewEvaluator(sink, nil)
	var errs, total float64
	ev.AddErrorRateAnomaly("anom", "ERROR_RATE_ANOMALY", "anomalous rate",
		AnomalyConfig{MinSamples: 10, SigmaMultiplier: 3},
		func() float64 { return errs }, func() float64 { return total })
	now := time.Now()

	// ~1% steady baseline across 20 windows
	for i := 0; i < 20; i++ {
		errs += 1
		total += 100
		ev.EvalOnce(now.Add(time.Duration(i) * 10 * time.Second))
	}
	if sink.firing("anom") != 0 {
		t.Fatalf("false positive on baseline: %+v", sink.list())
	}
	// sudden 40% window
	errs += 40
	total += 100
	ev.EvalOnce(now.Add(200 * time.Second))
	if sink.firing("anom") != 1 {
		t.Fatalf("anomaly not detected: %+v", sink.list())
	}
}

func TestPublisherSinkPayload(t *testing.T) {
	var gotSubject string
	var got []byte
	sink := PublisherSink{Pub: pubFunc(func(_ context.Context, s string, p []byte) error {
		gotSubject, got = s, p
		return nil
	})}
	if err := sink.Raise(context.Background(), Alert{
		Rule: "r", Severity: SeverityP1, Code: "C", Summary: "s",
		Status: "firing", FiredAt: "t"}); err != nil {
		t.Fatal(err)
	}
	if gotSubject != OpsAlertSubject {
		t.Fatalf("subject = %q", gotSubject)
	}
	if !strings.Contains(string(got), `"severity":"P1"`) {
		t.Fatalf("payload = %s", got)
	}
	// nil publisher → error (alerting is contractual)
	if err := (PublisherSink{}).Raise(context.Background(), Alert{}); err == nil {
		t.Fatal("nil publisher did not error")
	}
}

type pubFunc func(ctx context.Context, subject string, payload []byte) error

func (f pubFunc) Publish(ctx context.Context, subject string, payload []byte) error {
	return f(ctx, subject, payload)
}
