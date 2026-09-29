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
		"test", "testing", "sandbox", "local", "ci", "testnet", "Testnet",
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

// ---------------------------------------------------------------------------
// Phase-14 Task 14.3.3 — testnet gating & simulated-funding validation
// ---------------------------------------------------------------------------

func TestTestnetLabelEnabled(t *testing.T) {
	svc := New(nil, "testnet")
	if !svc.Enabled() || !svc.IsTestnet() {
		t.Fatal("testnet label must enable test endpoints and report IsTestnet")
	}
	if New(nil, "staging").IsTestnet() {
		t.Fatal("staging must not report IsTestnet")
	}
}

// Testnet/production mismatch fails closed: a production-labelled process
// can never serve test endpoints — seed, reset+seed and both simulated
// funding directions all return ErrDisabled before touching the store.
func TestTestnetFailClosedOnProduction(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, "production")
	if _, err := svc.Seed(ctx, 1, PresetStandard); !errors.Is(err, ErrDisabled) {
		t.Fatalf("seed err=%v want ErrDisabled", err)
	}
	if _, _, err := svc.ResetTo(ctx, 1, PresetStandard); !errors.Is(err, ErrDisabled) {
		t.Fatalf("reset-to err=%v want ErrDisabled", err)
	}
	if _, err := svc.SimulateDeposit(ctx, 1, "USD", "100"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("deposit err=%v want ErrDisabled", err)
	}
	if _, err := svc.SimulateWithdrawal(ctx, 1, "USD", "100"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("withdrawal err=%v want ErrDisabled", err)
	}
	// Unknown/empty labels fail the same closed way.
	for _, env := range []string{"", "qa", "PRODUCTION", "testnet-prod-mismatch"} {
		svc := New(nil, env)
		if _, err := svc.SimulateDeposit(ctx, 1, "USD", "1"); !errors.Is(err, ErrDisabled) {
			t.Fatalf("env %q deposit err=%v want ErrDisabled", env, err)
		}
	}
}

// Validation order: unknown presets and malformed funding params return
// typed errors before the pool is touched (nil pool = proof of order).
func TestTestnetValidationBeforeStore(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, "testnet")
	if _, err := svc.Seed(ctx, 1, "nope"); !errors.Is(err, ErrInvalidPreset) {
		t.Fatalf("seed err=%v want ErrInvalidPreset", err)
	}
	if _, _, err := svc.ResetTo(ctx, 1, "nope"); !errors.Is(err, ErrInvalidPreset) {
		t.Fatalf("reset-to err=%v want ErrInvalidPreset", err)
	}
	for _, ccy := range []string{"", "us", "USD1", "usd-dollar", "EURUSD"} {
		if _, err := svc.SimulateDeposit(ctx, 1, ccy, "10"); !errors.Is(err, ErrInvalidCurrency) {
			t.Fatalf("deposit ccy=%q err=%v want ErrInvalidCurrency", ccy, err)
		}
	}
	for _, amt := range []string{"", "abc", "-5", "0", "0.00", "9999999999999.01"} {
		if _, err := svc.SimulateDeposit(ctx, 1, "USD", amt); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("deposit amt=%q err=%v want ErrInvalidAmount", amt, err)
		}
		if _, err := svc.SimulateWithdrawal(ctx, 1, "USD", amt); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("withdrawal amt=%q err=%v want ErrInvalidAmount", amt, err)
		}
	}
	if _, err := svc.SimulateDeposit(ctx, 0, "USD", "1"); err == nil {
		t.Fatal("account_id=0 accepted")
	}
}

// ResetTo shares the reset cooldown: one slot covers the whole
// reset+seed, and a consumed slot blocks it like a plain reset.
func TestResetToSharesCooldown(t *testing.T) {
	svc := New(nil, "testnet")
	svc.limiter.Allow(7) // simulate a completed reset
	if _, _, err := svc.ResetTo(context.Background(), 7, PresetStandard); !errors.Is(err, ErrCooldown) {
		t.Fatalf("reset-to inside cooldown err=%v want ErrCooldown", err)
	}
	// Seed does NOT consume or check the cooldown — additive fixture.
	if _, err := svc.Seed(context.Background(), 7, "nope"); !errors.Is(err, ErrInvalidPreset) {
		t.Fatalf("seed validation must precede cooldown concerns: %v", err)
	}
}

func TestPresetCatalogue(t *testing.T) {
	found := false
	for _, p := range Presets() {
		if p == PresetStandard {
			found = true
		}
	}
	if !found {
		t.Fatalf("standard preset missing from Presets(): %v", Presets())
	}
}
