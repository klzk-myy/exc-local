// Task 1.3.12 — PTP/NTP clock-sync guard tests. All clock samples are
// injected so nothing depends on the host's NTP/PTP state.
package timesync

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

func staticSource(offset time.Duration, synced bool, err error) Source {
	return func() (time.Duration, bool, error) { return offset, synced, err }
}

func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *excerrors.Error
	if !stderrors.As(err, &e) {
		t.Fatalf("err %v is not a coded *errors.Error", err)
	}
	if e.Code != want {
		t.Fatalf("code = %q, want %q", e.Code, want)
	}
}

func TestCheckWithinBound(t *testing.T) {
	g := New(staticSource(42*time.Microsecond, true, nil))
	if err := g.Check(); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
	if !g.Healthy() {
		t.Fatal("Healthy() = false within bound")
	}
}

func TestCheckExactlyAtBoundIsOk(t *testing.T) {
	// Spec §2.7.2: skew *greater than* 100µs trips; the bound itself passes.
	g := New(staticSource(MaxOffset, true, nil))
	if err := g.Check(); err != nil {
		t.Fatalf("Check() at exactly 100µs = %v, want nil", err)
	}
}

func TestCheckDriftTripsHaltCode(t *testing.T) {
	for _, off := range []time.Duration{MaxOffset + 1, -150 * time.Microsecond} {
		g := New(staticSource(off, true, nil))
		err := g.Check()
		if err == nil {
			t.Fatalf("Check(offset=%v) = nil, want halt", off)
		}
		requireCode(t, err, excerrors.CodeTimeSyncLossHalt)
		if excerrors.SeverityOf(err) != excerrors.SeverityL0 {
			t.Fatal("TIME_SYNC_LOSS_HALT must classify as L0 fatal")
		}
		if excerrors.HTTPStatusOf(err) != 503 {
			t.Fatal("TIME_SYNC_LOSS_HALT must map to HTTP 503")
		}
		if g.Healthy() {
			t.Fatal("Healthy() = true on drift violation")
		}
	}
}

func TestCheckUnsynchronizedFailsClosed(t *testing.T) {
	g := New(staticSource(0, false, nil))
	err := g.Check()
	if err == nil {
		t.Fatal("Check() = nil for undisciplined clock")
	}
	requireCode(t, err, excerrors.CodeTimeSyncLossHalt)
}

func TestCheckProbeErrorFailsClosed(t *testing.T) {
	g := New(staticSource(0, false, fmt.Errorf("adjtimex failed")))
	err := g.Check()
	if err == nil {
		t.Fatal("Check() = nil on probe error")
	}
	requireCode(t, err, excerrors.CodeTimeSyncLossHalt)
}

func TestGuardZeroValueUsesKernelSource(t *testing.T) {
	// Zero-value Guard: nil Source must select KernelSource — the result is
	// environment-dependent but must be a *errors.Error on failure, never a
	// panic or an uncoded error.
	var g Guard
	err := g.Check()
	if err != nil {
		requireCode(t, err, excerrors.CodeTimeSyncLossHalt)
	}
}

func TestMonitorReportsViolationsUntilCancel(t *testing.T) {
	// Non-blocking send: a full channel must not stall Monitor's loop.
	violations := make(chan error, 8)
	g := New(staticSource(MaxOffset+1, true, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// Immediate check + 5ms ticks → at least one violation, then clean exit.
	g.Monitor(ctx, 5*time.Millisecond, func(err error) {
		select {
		case violations <- err:
		default:
		}
	})
	select {
	case err := <-violations:
		requireCode(t, err, excerrors.CodeTimeSyncLossHalt)
	default:
		t.Fatal("Monitor reported no violation for an out-of-bounds clock")
	}
}

func TestMonitorSilentWhenHealthy(t *testing.T) {
	violations := make(chan error, 8)
	g := New(staticSource(10*time.Microsecond, true, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	g.Monitor(ctx, 5*time.Millisecond, func(err error) {
		select {
		case violations <- err:
		default:
		}
	})
	select {
	case err := <-violations:
		t.Fatalf("Monitor reported %v for a healthy clock", err)
	default:
	}
}
