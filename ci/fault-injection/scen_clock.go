// Clock-jump fault scenario (L0 tier): injects clock-source offsets into
// timesync.Guard — >100µs drift, lost sync, and probe failure must each
// produce the coded TIME_SYNC_LOSS_HALT L0 halt error (spec §2.7.2,
// MiFID II RTS 25), never a panic or silent accept.

package main

import (
	"context"
	stderrors "errors"
	"time"

	"exchange/internal/timesync"
	excerrors "exchange/pkg/errors"
)

func staticClock(offset time.Duration, synced bool, err error) timesync.Source {
	return func() (time.Duration, bool, error) { return offset, synced, err }
}

func scenarioClockJump(_ context.Context, _ *env) *Checks {
	c := &Checks{}

	type kase struct {
		name   string
		offset time.Duration
		synced bool
		srcErr error
	}
	for _, k := range []kase{
		{"jump_plus_200us", 200 * time.Microsecond, true, nil},
		{"jump_minus_1ms", -time.Millisecond, true, nil},
		{"jump_just_over_bound", timesync.MaxOffset + time.Nanosecond, true, nil},
		{"clock_unsynced", 0, false, nil},
		{"probe_failure", 0, false, stderrors.New("adjtimex: TIME_ERROR")},
	} {
		g := timesync.New(staticClock(k.offset, k.synced, k.srcErr))
		err, panicked := panicGuard(func() error { return g.Check() })
		c.okf(k.name+":no_panic", panicked == nil, "panic=%v", panicked)
		var e *excerrors.Error
		coded := stderrors.As(err, &e)
		c.okf(k.name+":coded_error", coded, "err=%v", err)
		if coded {
			c.okf(k.name+":code", e.Code == excerrors.CodeTimeSyncLossHalt,
				"code=%q want %q", e.Code, excerrors.CodeTimeSyncLossHalt)
			c.okf(k.name+":severity_L0",
				excerrors.SeverityOf(err) == excerrors.SeverityL0,
				"severity=%s", excerrors.SeverityOf(err))
			c.okf(k.name+":http_503", excerrors.HTTPStatusOf(err) == 503,
				"http=%d", excerrors.HTTPStatusOf(err))
		}
		c.okf(k.name+":not_healthy", !g.Healthy(), "Healthy()=true on bad clock")
	}

	// Boundary sanity: exactly at the bound and inside it must NOT trip —
	// fail-closed is only violated by *exceeding* the bound.
	for _, k := range []kase{
		{"at_bound_100us", timesync.MaxOffset, true, nil},
		{"within_bound", 42 * time.Microsecond, true, nil},
	} {
		g := timesync.New(staticClock(k.offset, k.synced, k.srcErr))
		c.okf(k.name+":no_trip", g.Check() == nil && g.Healthy(),
			"Check()=%v", g.Check())
	}

	// Monitor delivers the violation via callback — never swallows it.
	// The callback must be non-blocking (it runs on Monitor's goroutine);
	// a blocking send would stall the poll loop — the doc contract.
	mon := timesync.New(staticClock(200*time.Microsecond, true, nil))
	got := make(chan error, 1)
	ctx2, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		mon.Monitor(ctx2, 20*time.Millisecond, func(err error) {
			select {
			case got <- err:
			default: // already recorded one — don't stall the monitor
			}
		})
		close(done)
	}()
	select {
	case err := <-got:
		c.okf("monitor:violation_delivered", errCode(err) == excerrors.CodeTimeSyncLossHalt,
			"err=%v", err)
	case <-done:
		c.ok("monitor:violation_delivered", false, "Monitor returned without violation")
	case <-time.After(2 * time.Second):
		c.ok("monitor:violation_delivered", false, "timeout waiting for violation")
	}
	<-done
	return c
}
