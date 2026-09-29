// mmp.go — Phase-18 Task 18.3.10 item 3: Market-Maker Protection (MMP).
//
// Spec §9.6: if a session's quotes are filled more than mmp_max_fills
// times within mmp_window_ms, all remaining quotes for that
// session/instrument are mass-cancelled automatically. The C++ engine
// owns the authoritative sub-millisecond trigger; this Go-side tracker
// mirrors it from the read-model fill feed: it keeps an in-memory
// sliding window per program, invokes the mass-cancel seam on breach,
// raises MM_OBLIGATION_BREACH, and holds an MMP_LOCKED_OUT lockout that
// the quoting path (internal/fix/quoting.go) consults before admitting
// new quotes — until an explicit reset (§24.x MM Program row: "requires
// explicit reset (FIX 35=c or REST)").
//
// The window is keyed per program row — per-instrument rows and the
// program-wide row evaluate independently (a fill counts in the most
// specific covering program's window).
package marketmaking

import (
	"context"
	"fmt"
	"sync"
	"time"

	"exchange/internal/observability"
	excerrors "exchange/pkg/errors"
)

// MMPCancelFunc is the mass-cancel seam invoked on MMP trigger —
// production wiring adapts orders.Service.MassCancel scoped to
// (program.account_id, fill instrumentID): "mass-cancel that MM's
// quotes for the instrument" per the task text. instrumentID is the
// fill's instrument, not the program's binding column — a program-wide
// row (instrument_id NULL) still cancels only the breached instrument's
// quotes. Tests substitute a fake.
type MMPCancelFunc func(ctx context.Context, p *Program, instrumentID int64) (cancelled int, err error)

// MMPTracker counts fills against program thresholds and owns the
// lockout lifecycle. Not goroutine-heavy: all state sits behind one
// mutex — fill-rate churn is bounded by window pruning.
type MMPTracker struct {
	svc    *Service
	cancel MMPCancelFunc
	sink   observability.Sink
	logf   func(format string, args ...any)
	now    func() time.Time

	mu      sync.Mutex
	windows map[int64][]time.Time // programID → fill timestamps inside window
	locked  map[int64]time.Time   // programID → trigger time (locked out)
}

// NewMMPTracker wires the tracker; svc resolves covering programs,
// cancel is the mass-cancel seam (may be nil → trigger still locks +
// alerts, documented for tests/dev).
func NewMMPTracker(svc *Service, cancel MMPCancelFunc) *MMPTracker {
	return &MMPTracker{
		svc: svc, cancel: cancel,
		logf:    func(string, ...any) {},
		now:     time.Now,
		windows: map[int64][]time.Time{},
		locked:  map[int64]time.Time{},
	}
}

// WithAlerts binds the alert sink (P2 on trigger).
func (t *MMPTracker) WithAlerts(s observability.Sink) *MMPTracker {
	t.sink = s
	return t
}

// WithLogger binds the operator log line.
func (t *MMPTracker) WithLogger(fn func(format string, args ...any)) *MMPTracker {
	if fn != nil {
		t.logf = fn
	}
	return t
}

// WithClock injects a deterministic clock (tests).
func (t *MMPTracker) WithClock(now func() time.Time) *MMPTracker {
	if now != nil {
		t.now = now
	}
	return t
}

// MMPLocked reports whether the program covering (accountID,
// instrumentID) is currently inside an MMP lockout — the quoting path
// rejects with MMP_LOCKED_OUT while true. Resolves the program inline
// so a stale cache can never mask a lockout (fail-closed direction:
// an unresolved program is NOT locked, matching entitlement semantics
// where unresolved already rejects).
func (t *MMPTracker) MMPLocked(ctx context.Context, accountID, instrumentID int64) bool {
	p, err := t.svc.Entitled(ctx, accountID, instrumentID)
	if err != nil || p == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.locked[p.ID]
	return ok
}

// MMPLockedProgram reports the same for a known program row.
func (t *MMPTracker) MMPLockedProgram(programID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.locked[programID]
	return ok
}

// OnFill records one quote fill for (accountID, instrumentID) against
// the covering program's sliding window. On breach: lockout + mass-
// cancel via the seam + MM_OBLIGATION_BREACH alert. Returns true when
// this fill triggered the protection.
func (t *MMPTracker) OnFill(ctx context.Context, accountID, instrumentID int64) (bool, error) {
	p, err := t.svc.Entitled(ctx, accountID, instrumentID)
	if err != nil {
		return false, excerrors.Wrap(CodeMMInternal, "mm program lookup", err)
	}
	if p == nil {
		return false, nil // not a registered MM — nothing to protect
	}
	now := t.now()
	t.mu.Lock()
	cutoff := now.Add(-p.MMPWindow())
	win := t.windows[p.ID]
	kept := win[:0]
	for _, ts := range win {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	t.windows[p.ID] = kept
	triggered := len(kept) > p.MMPMaxFills
	if triggered {
		if _, already := t.locked[p.ID]; !already {
			t.locked[p.ID] = now
		}
	}
	count := len(kept)
	t.mu.Unlock()

	if !triggered {
		return false, nil
	}
	t.logf("mmp: program %d triggered — %d fills in %s (max %d)",
		p.ID, count, p.MMPWindow(), p.MMPMaxFills)
	cancelled := 0
	if t.cancel != nil {
		c, cerr := t.cancel(ctx, p, instrumentID)
		if cerr != nil {
			// The lockout stands even when the cancel fails — quotes
			// remain unsafe; the error surfaces for ops retry.
			t.raise(ctx, p, count, cancelled, cerr)
			return true, excerrors.Wrap(CodeMMPTriggered,
				fmt.Sprintf("mmp mass-cancel failed for program %d", p.ID), cerr)
		}
		cancelled = c
	}
	t.raise(ctx, p, count, cancelled, nil)
	return true, nil
}

func (t *MMPTracker) raise(ctx context.Context, p *Program, fills, cancelled int, cause error) {
	if t.sink == nil {
		return
	}
	status, summary := "firing", fmt.Sprintf(
		"MMP triggered for program %d (account %d): %d fills > %d in %s — %d quotes mass-cancelled, lockout until reset",
		p.ID, p.AccountID, fills, p.MMPMaxFills, p.MMPWindow(), cancelled)
	if cause != nil {
		summary = fmt.Sprintf(
			"MMP triggered for program %d (account %d) but mass-cancel FAILED: %v — lockout held",
			p.ID, p.AccountID, cause)
	}
	_ = t.sink.Raise(ctx, observability.Alert{
		Rule:     "mmp_triggered",
		Severity: observability.SeverityP2,
		Code:     CodeMMPTriggered,
		Summary:  summary,
		Status:   status,
		Details: map[string]string{
			"program_id":    fmt.Sprint(p.ID),
			"account_id":    fmt.Sprint(p.AccountID),
			"fills":         fmt.Sprint(fills),
			"mmp_max_fills": fmt.Sprint(p.MMPMaxFills),
			"window_ms":     fmt.Sprint(p.MMPWindowMs),
			"cancelled":     fmt.Sprint(cancelled),
		},
		FiredAt: t.now().UTC().Format(time.RFC3339Nano),
	})
}

// ResetMMP clears the lockout for the program covering (accountID,
// instrumentID) — the §24.x explicit reset path (FIX QuoteStatusRequest
// 35=a / REST admin). Also clears the fill window so the pre-trigger
// count cannot re-arm instantly.
func (t *MMPTracker) ResetMMP(ctx context.Context, accountID, instrumentID int64) error {
	p, err := t.svc.Entitled(ctx, accountID, instrumentID)
	if err != nil {
		return excerrors.Wrap(CodeMMInternal, "mm program lookup", err)
	}
	if p == nil {
		return excerrors.New(CodeMMProgramNotFound,
			fmt.Sprintf("no active mm program covers account %d instrument %d", accountID, instrumentID))
	}
	t.mu.Lock()
	delete(t.locked, p.ID)
	delete(t.windows, p.ID)
	t.mu.Unlock()
	return nil
}

// ResetMMPProgram clears the lockout by program id (admin path).
func (t *MMPTracker) ResetMMPProgram(programID int64) {
	t.mu.Lock()
	delete(t.locked, programID)
	delete(t.windows, programID)
	t.mu.Unlock()
}
