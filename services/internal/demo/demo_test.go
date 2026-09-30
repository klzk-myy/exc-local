// Unit tests for the Task 8.5.3.2 demo-environment service — the
// PG-backed paths (Provision, TouchActivity, ExpireSweep, IsDemo) are
// covered by the EXC_PG_TEST-gated suite in demo_pg_test.go.
package demo

import (
	"context"
	"errors"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func TestEnabledOnlyOnDemoEnv(t *testing.T) {
	for env, want := range map[string]bool{
		"demo": true, "DEMO": true, " demo ": true,
		"production": false, "staging": false, "testnet": false,
		"": false, "unknown": false,
	} {
		if got := New(nil, env).Enabled(); got != want {
			t.Errorf("env %q Enabled=%v, want %v", env, got, want)
		}
	}
}

func TestWithInitialBalanceGuards(t *testing.T) {
	s := New(nil, "demo")
	s.WithInitialBalance(decimal.NewFromInt(50_000))
	if !s.balance.Equal(decimal.NewFromInt(50_000)) {
		t.Fatalf("balance %s, want 50000", s.balance)
	}
	// Non-positive and over-cap values are rejected — the default stands.
	s.WithInitialBalance(decimal.NewFromInt(-1))
	if !s.balance.Equal(decimal.NewFromInt(50_000)) {
		t.Fatalf("negative seed accepted: %s", s.balance)
	}
	s.WithInitialBalance(decimal.NewFromInt(0))
	if !s.balance.Equal(decimal.NewFromInt(50_000)) {
		t.Fatalf("zero seed accepted: %s", s.balance)
	}
	s.WithInitialBalance(decimal.NewFromInt(1_000_000_000_001))
	if !s.balance.Equal(decimal.NewFromInt(50_000)) {
		t.Fatalf("over-cap seed accepted: %s", s.balance)
	}
}

func TestWithLifetimeGuards(t *testing.T) {
	s := New(nil, "demo")
	if s.lifetime != DefaultLifetime {
		t.Fatalf("default lifetime %v, want %v", s.lifetime, DefaultLifetime)
	}
	s.WithLifetime(0).WithLifetime(-time.Hour)
	if s.lifetime != DefaultLifetime {
		t.Fatalf("non-positive lifetime accepted: %v", s.lifetime)
	}
	s.WithLifetime(time.Hour)
	if s.lifetime != time.Hour {
		t.Fatalf("lifetime %v, want 1h", s.lifetime)
	}
}

// ---------------------------------------------------------------------------
// Funding gate — the demo isolation invariant
// ---------------------------------------------------------------------------

type stubChecker struct{ err error }

func (c stubChecker) AssertMutable(context.Context, int64) error { return c.err }

func errCodeIs(t *testing.T, err error, want string) {
	t.Helper()
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("error %v, want code %s", err, want)
	}
}

func TestFundingGateRejectsDemo(t *testing.T) {
	gate := fundingChecker{inner: stubChecker{},
		isDemo: func(context.Context, int64) (bool, error) { return true, nil }}
	err := gate.AssertMutable(context.Background(), 7)
	errCodeIs(t, err, "FORBIDDEN")
}

func TestFundingGatePassesNonDemo(t *testing.T) {
	gate := fundingChecker{inner: stubChecker{},
		isDemo: func(context.Context, int64) (bool, error) { return false, nil }}
	if err := gate.AssertMutable(context.Background(), 7); err != nil {
		t.Fatalf("non-demo rejected: %v", err)
	}
}

func TestFundingGatePropagatesInnerError(t *testing.T) {
	inner := excerrors.New("ACCOUNT_FROZEN", "frozen")
	gate := fundingChecker{inner: stubChecker{err: inner},
		isDemo: func(context.Context, int64) (bool, error) {
			t.Fatal("isDemo must not run when the inner gate rejects")
			return true, nil
		}}
	if err := gate.AssertMutable(context.Background(), 7); err != inner {
		t.Fatalf("inner error not propagated: %v", err)
	}
}

func TestFundingGateLookupFailureFailsClosed(t *testing.T) {
	gate := fundingChecker{inner: stubChecker{},
		isDemo: func(context.Context, int64) (bool, error) {
			return false, errors.New("pg down")
		}}
	if err := gate.AssertMutable(context.Background(), 7); err == nil {
		t.Fatal("demo-type lookup failure passed the gate")
	}
}

func TestFundingGateNilInner(t *testing.T) {
	// A nil inner is tolerated (the demo check still fires) — the
	// composition layer is trusted, but the gate never skips itself.
	gate := fundingChecker{inner: nil,
		isDemo: func(context.Context, int64) (bool, error) { return true, nil }}
	errCodeIs(t, gate.AssertMutable(context.Background(), 1), "FORBIDDEN")
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

func TestProvisionRejectsNonDemoEnv(t *testing.T) {
	// Enabled() gates ProvisionTx before any SQL — the pool is never
	// touched on a non-demo deployment (nil pool would panic otherwise).
	s := New(nil, "production")
	if _, err := s.ProvisionTx(context.Background(), nil, 1); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err %v, want ErrDisabled", err)
	}
}

func TestDefaultClosureIDShape(t *testing.T) {
	id, err := defaultClosureID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != len("demoexp_")+28 || id[:8] != "demoexp_" {
		t.Fatalf("malformed closure id %q", id)
	}
}
