// Per-session throttle tests (spec §9.3 max_msgs_per_sec, §24 #137).
package fix

import (
	"testing"
	"time"
)

func TestThrottle_BurstThenRejects(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := newThrottleSet(func() time.Time { return now })

	// burst = one second's worth: 3 messages at cap=3 pass, 4th trips.
	for i := 0; i < 3; i++ {
		if !ts.Allow("s", 3) {
			t.Fatalf("message %d under cap must pass", i+1)
		}
	}
	if ts.Allow("s", 3) {
		t.Fatal("4th message over cap=3 must be rejected")
	}
}

func TestThrottle_Refill(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	ts := newThrottleSet(func() time.Time { return now })

	ts.Allow("s", 2)
	ts.Allow("s", 2)
	if ts.Allow("s", 2) {
		t.Fatal("over cap must reject")
	}
	now = now.Add(500 * time.Millisecond) // +1 token at 2/s
	if !ts.Allow("s", 2) {
		t.Fatal("token refill must admit")
	}
	if ts.Allow("s", 2) {
		t.Fatal("drained again must reject")
	}
	now = now.Add(2 * time.Second) // burst re-caps at 2
	if !ts.Allow("s", 2) || !ts.Allow("s", 2) || ts.Allow("s", 2) {
		t.Fatal("burst must cap at configured rate, not accumulate")
	}
}

func TestThrottle_LiveRateChange(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := newThrottleSet(func() time.Time { return now })
	ts.Allow("s", 100) // establish at 100
	ts.SetRate("s", 1) // admin tightens to 1/s on the live session
	ts.Allow("s", 1)
	if ts.Allow("s", 1) {
		t.Fatal("tightened rate must take effect immediately")
	}
	now = now.Add(time.Second)
	if !ts.Allow("s", 1) {
		t.Fatal("refill at new rate must admit")
	}
}

func TestThrottle_DropReleasesBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := newThrottleSet(func() time.Time { return now })
	ts.Allow("s", 1)
	if ts.Allow("s", 1) {
		t.Fatal("second message over cap=1")
	}
	ts.Drop("s")
	if !ts.Allow("s", 1) {
		t.Fatal("reconnect must start with a fresh bucket")
	}
}

func TestThrottle_ZeroCapDefaults(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := newThrottleSet(func() time.Time { return now })
	// cap<=0 → the migration-046 default of 100/s applies.
	for i := 0; i < 100; i++ {
		if !ts.Allow("s", 0) {
			t.Fatalf("cap=0 must default to 100; failed at %d", i+1)
		}
	}
	if ts.Allow("s", 0) {
		t.Fatal("101st message must reject at the default cap")
	}
}
