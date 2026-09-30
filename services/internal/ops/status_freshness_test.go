// Measurement harness for Task 9.3.25 AC:
// "Status page reflects engine mode changes within 3 seconds."
//
// Drives a real degradation-mode transition through the production
// signal path on the dev Redis coordination instance:
//
//	ModeManager write → Redis system:degradation:mode
//	  → Aggregator.Collect (ModeReader.GetDegradationMode)
//	  → Aggregator.Publish → Redis status:current
//	  → ops.Current (the GET /api/v1/system/status document)
//
// The aggregator runs at its production cadence (Run's 1s default);
// freshness is wall time from SetDegradationMode returning until the
// published status:current document reflects the new mode. Gated on
// EXC_REDIS_TEST=1 per repo convention (internal/redis tests).
//
// Run: EXC_REDIS_TEST=1 go test ./internal/ops/ -run StatusModeFreshness -v
package ops

import (
	"context"
	"os"
	"testing"
	"time"

	exchredis "exchange/internal/redis"
)

// statusFreshnessBound is the Task 9.3.25 AC bound under measurement.
const statusFreshnessBound = 3 * time.Second

// statusTestClient dials the dev coordination primary
// (127.0.0.1:16379; override with EXC_REDIS_TEST_ADDR).
func statusTestClient(t *testing.T) *exchredis.Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	c := exchredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		t.Skipf("redis coordination instance unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// waitMode polls status:current until it reports want or the deadline
// passes; returns the observed freshness.
func waitMode(ctx context.Context, c *exchredis.Client, want exchredis.DegradationMode, deadline time.Duration) (*Status, time.Duration, error) {
	start := time.Now()
	for {
		st, err := Current(ctx, c.Client)
		if err == nil && st.Mode == string(want) {
			return st, time.Since(start), nil
		}
		if time.Since(start) > deadline {
			if err != nil {
				return nil, time.Since(start), err
			}
			return st, time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return nil, time.Since(start), ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestStatusModeFreshness measures write→publish freshness for both
// Normal→ReadOnly and ReadOnly→Normal transitions. Asserts <3s per the
// AC; logs every measured value so a miss reports the honest number.
func TestStatusModeFreshness(t *testing.T) {
	c := statusTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Baseline the mode to Normal so the first publish is a known
	// state; leave it Normal on cleanup too.
	if err := c.SetDegradationMode(ctx, exchredis.ModeNormal, "freshness test baseline"); err != nil {
		t.Fatalf("baseline mode: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = c.SetDegradationMode(cctx, exchredis.ModeNormal, "freshness test cleanup")
	})

	// Production path: real client as ModeReader + publisher, one
	// healthy critical component, Run at the production default
	// interval (0 → 1s per status_exporter.go).
	agg := NewAggregator(nil, c.Client, c, []Component{
		{Name: "engine-shard-0", Critical: true, Probe: okProbe},
	}, nil)
	aggCtx, aggStop := context.WithCancel(ctx)
	defer aggStop()
	go agg.Run(aggCtx, 0)

	// Wait for the first publish so t0 starts from a live feed.
	if _, _, err := waitMode(ctx, c, exchredis.ModeNormal, 10*time.Second); err != nil {
		t.Fatalf("initial publish never observed: %v", err)
	}

	var degr, rec []time.Duration
	for i := 0; i < 3; i++ {
		// Normal → ReadOnly.
		if err := c.SetDegradationMode(ctx, exchredis.ModeReadOnly, "freshness drill"); err != nil {
			t.Fatalf("set ReadOnly: %v", err)
		}
		st, el, err := waitMode(ctx, c, exchredis.ModeReadOnly, 10*time.Second)
		if err != nil {
			t.Fatalf("wait ReadOnly: %v", err)
		}
		if st == nil || st.Mode != string(exchredis.ModeReadOnly) {
			t.Fatalf("ReadOnly never published within 10s")
		}
		if st.Status != StateDegraded {
			t.Fatalf("mode ReadOnly published with status %q, want %q", st.Status, StateDegraded)
		}
		degr = append(degr, el)
		t.Logf("transition %d Normal→ReadOnly freshness: %s", i, el)
		if el > statusFreshnessBound {
			t.Errorf("mode change freshness %s exceeds 3s bound", el)
		}

		// ReadOnly → Normal (recovery path).
		if err := c.SetDegradationMode(ctx, exchredis.ModeNormal, "drill recovery"); err != nil {
			t.Fatalf("set Normal: %v", err)
		}
		st, el, err = waitMode(ctx, c, exchredis.ModeNormal, 10*time.Second)
		if err != nil {
			t.Fatalf("wait Normal: %v", err)
		}
		if st == nil || st.Mode != string(exchredis.ModeNormal) {
			t.Fatalf("Normal never republished within 10s")
		}
		rec = append(rec, el)
		t.Logf("transition %d ReadOnly→Normal freshness: %s", i, el)
		if el > statusFreshnessBound {
			t.Errorf("mode recovery freshness %s exceeds 3s bound", el)
		}
	}
	t.Logf("summary: degrade freshness min=%s max=%s; recovery min=%s max=%s; bound=%s",
		minDur(degr), maxDur(degr), minDur(rec), maxDur(rec), statusFreshnessBound)
}

func minDur(ds []time.Duration) time.Duration {
	m := ds[0]
	for _, d := range ds[1:] {
		if d < m {
			m = d
		}
	}
	return m
}

func maxDur(ds []time.Duration) time.Duration {
	m := ds[0]
	for _, d := range ds[1:] {
		if d > m {
			m = d
		}
	}
	return m
}
