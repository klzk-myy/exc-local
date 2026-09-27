// Package timesync implements the PTP/NTP clock-sync guard of Task 1.3.12:
// a startup gate plus a daemon health check that trip TIME_SYNC_LOSS_HALT
// (spec §23; HTTP 503, severity L0) when clock drift exceeds 100µs or the
// clock loses synchronization — the explicit L0 trigger of spec §2.7.2 and
// the MiFID II RTS 25 requirement.
//
// The offset source is injected, so tests never depend on host NTP/PTP
// state. The production source reads the kernel clock discipline via
// adjtimex(2) — the same PLL that chrony/ntpd steer now and phc2sys steers
// once hardware PTP lands (Phase-09 Task 9.3.12). STA_UNSYNC, a failed
// syscall, or a TIME_ERROR adjtimex state all count as unsynchronized
// (fail-closed, spec §2.7.1).
package timesync

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/unix"

	excerrors "exchange/pkg/errors"
)

// MaxOffset is the fail-closed drift bound (spec §2.7.2: PTP clock skew
// >100µs trips the L0 halt).
const MaxOffset = 100 * time.Microsecond

// Source samples the system's clock offset from its sync grandmaster.
// synced reports whether the clock is disciplined at all; offset may be
// negative (system clock behind the reference).
type Source func() (offset time.Duration, synced bool, err error)

// Guard evaluates clock-sync samples against the drift bound.
type Guard struct {
	// Source probes the clock; nil selects KernelSource.
	Source Source
	// MaxOffset overrides the bound; <= 0 selects timesync.MaxOffset.
	MaxOffset time.Duration
}

// New returns a Guard over src with the spec bound.
func New(src Source) Guard {
	return Guard{Source: src, MaxOffset: MaxOffset}
}

// NewKernelGuard returns the production guard probing adjtimex(2).
func NewKernelGuard() Guard {
	return New(KernelSource)
}

// KernelSource reads the kernel clock discipline via adjtimex(2) in
// query-only mode. Timex.Offset is nanoseconds when STA_NANO is set,
// microseconds otherwise; STA_UNSYNC or TIME_ERROR mark the clock as
// undisciplined.
func KernelSource() (time.Duration, bool, error) {
	var tx unix.Timex
	state, err := unix.Adjtimex(&tx)
	if err != nil {
		return 0, false, fmt.Errorf("timesync: adjtimex: %w", err)
	}
	offset := time.Duration(tx.Offset) * time.Microsecond
	if tx.Status&unix.STA_NANO != 0 {
		offset = time.Duration(tx.Offset) * time.Nanosecond
	}
	synced := tx.Status&unix.STA_UNSYNC == 0 && state != unix.TIME_ERROR
	return offset, synced, nil
}

// Check samples the source once — the startup gate and the per-tick daemon
// health check share this evaluation. Returns nil when synchronized within
// the bound, else a coded *errors.Error carrying TIME_SYNC_LOSS_HALT (L0);
// callers must treat a non-nil return as halt startup / trip the watchdog.
func (g Guard) Check() error {
	src := g.Source
	if src == nil {
		src = KernelSource
	}
	bound := g.MaxOffset
	if bound <= 0 {
		bound = MaxOffset
	}
	offset, synced, err := src()
	if err != nil {
		return excerrors.Wrap(excerrors.CodeTimeSyncLossHalt,
			"timesync: offset probe failed", err)
	}
	if !synced {
		return excerrors.New(excerrors.CodeTimeSyncLossHalt,
			"timesync: clock is not synchronized to any source")
	}
	if offset < 0 {
		offset = -offset
	}
	if offset > bound {
		return excerrors.New(excerrors.CodeTimeSyncLossHalt,
			fmt.Sprintf("timesync: clock offset %s exceeds %s bound", offset, bound))
	}
	return nil
}

// Healthy is the daemon health-check surface: true only when synchronized
// within the bound.
func (g Guard) Healthy() bool { return g.Check() == nil }

// Monitor polls Check every interval until ctx is cancelled, invoking
// onViolation for each failed check. The callback is responsible for the
// L0 consequence (halting matching / paging P0); Monitor never swallows a
// violation. onViolation must be fast and non-blocking — it runs on
// Monitor's goroutine, so a blocking callback stalls subsequent polls
// (signal a channel, don't do the halt inline). An immediate Check runs
// before the first tick so a bad clock cannot hide for a whole interval.
func (g Guard) Monitor(ctx context.Context, interval time.Duration, onViolation func(error)) {
	if err := g.Check(); err != nil && onViolation != nil {
		onViolation(err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := g.Check(); err != nil && onViolation != nil {
				onViolation(err)
			}
		}
	}
}
