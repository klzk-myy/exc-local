// Task 5.3.13 — environment gate and cooldown-limiter unit tests;
// PG reset-transaction tests live in testenv_integration_test.go.
package testenv

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEnabledAllowlist(t *testing.T) {
	for _, env := range []string{"development", "dev", "staging", "stage",
		"test", "testing", "sandbox", "local", "ci",
		" Development ", "TEST", " staging "} {
		if !New(nil, env).Enabled() {
			t.Fatalf("env %q should enable resets", env)
		}
	}
}

// Fail-closed: production, unknown, and empty environments disable.
func TestEnabledFailClosed(t *testing.T) {
	for _, env := range []string{"production", "prod", "", "qa",
		"development2", "preprod"} {
		if New(nil, env).Enabled() {
			t.Fatalf("env %q must not enable resets", env)
		}
	}
}

// ResetAccount gates before touching the store: disabled env and a
// non-positive account id both return before the pool is used.
func TestResetGatesBeforeStore(t *testing.T) {
	if _, err := New(nil, "production").ResetAccount(context.Background(), 1); !errors.Is(err, ErrDisabled) {
		t.Fatalf("prod reset err=%v want ErrDisabled", err)
	}
	if _, err := New(nil, "").ResetAccount(context.Background(), 1); !errors.Is(err, ErrDisabled) {
		t.Fatalf("empty-env reset err=%v want ErrDisabled", err)
	}
	if _, err := New(nil, "test").ResetAccount(context.Background(), 0); err == nil {
		t.Fatal("account_id=0 accepted")
	}
}

// Cooldown: a second reset inside the window is rejected; a different
// account is unaffected. The limiter runs before the pool — a nil pool
// proves ordering.
func TestResetCooldown(t *testing.T) {
	svc := New(nil, "test")
	now := time.Now()
	svc.SetClockForTest(func() time.Time { return now })

	svc.limiter.Allow(7) // simulate a completed reset
	if _, err := svc.ResetAccount(context.Background(), 7); !errors.Is(err, ErrCooldown) {
		t.Fatalf("second reset err=%v want ErrCooldown", err)
	}
	// Foreign account is not gated — it proceeds to the nil pool and
	// panics; guard with a recover so this test asserts the ordering
	// rather than the panic itself.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("foreign account should reach the store (panic on nil pool)")
			}
		}()
		_, _ = svc.ResetAccount(context.Background(), 8)
	}()

	// Past the window the account reaches the store again.
	now = now.Add(ResetCooldown + time.Second)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("post-window reset should reach the store")
			}
		}()
		_, _ = svc.ResetAccount(context.Background(), 7)
	}()
}

// Limiter directly: boundary at exactly `width` is allowed again.
func TestLimiterBoundary(t *testing.T) {
	now := time.Now()
	l := NewLimiter(time.Minute)
	l.now = func() time.Time { return now }
	if !l.Allow(1) {
		t.Fatal("first allow rejected")
	}
	if l.Allow(1) {
		t.Fatal("in-window allow accepted")
	}
	now = now.Add(time.Minute)
	if !l.Allow(1) {
		t.Fatal("boundary allow rejected")
	}
}
