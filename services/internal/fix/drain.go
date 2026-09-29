// Graceful maintenance drain — the drain half of Task 18.3.17 (spec
// §9.3/§9.8, §24 #289; sequencing hook per Task 9.3.23 item 3).
//
// Contract:
//   - Drain() marks the gateway draining; AdmitLogon() then refuses new
//     Logons (session-not-available → Logout(35=5) refusal via
//     LogonRefusal). Existing sessions get a News(35=B) advisory
//     ("Scheduled maintenance", replacement endpoint when configured)
//     then a Logout(35=5) Text="Scheduled maintenance".
//   - Peer Logout replies are awaited up to LogoutWait (≤10s); TCP is
//     force-closed on stragglers.
//   - Graceful drain PRESERVES resting orders (AC #30: only abnormal
//     disconnect mass-cancels) — no CoD, no cancels; cancels and
//     in-flight responses stay available while sessions log out
//     (§24 #289).
//
// The DrainRegistry/DrainSession seams are satisfied by LiveDrainRegistry
// over App.ActiveSessions + quickfix.SendToTarget (adapter below); with a
// nil registry Drain is a correct no-op for the current scaffold in
// cmd/fix.
//
// Interplay note: Gateway.Stop (gateway.go, sibling-owned) performs the
// minimal Logout fan-out inside quickfixgo's own teardown; Drainer is the
// fuller Task-18.3.17 contract — logon gate (AdmitLogon/LogonRefusal),
// News(35=B) advisory with the replacement endpoint, one bounded
// peer-Logout wait, then forced socket close. The two paths send the
// same 35=5 text so a caller that runs Drainer before Gateway.Stop sees
// idempotent behaviour (a second Logout to an already-drained session is
// a no-op send failure, logged).
package fix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"
)

const (
	// DrainLogoutWait bounds the peer-Logout wait (Task 9.3.23 item 3:
	// "wait ≤10s"; the §9.3 5s logout timeout bounds individual peers —
	// the aggregate deadline is 10s).
	DrainLogoutWait = 10 * time.Second

	// DrainMaintenanceText is the canonical Logout Text(58) for a
	// planned drain.
	DrainMaintenanceText = "Scheduled maintenance"
)

// ErrDraining is returned by AdmitLogon while a drain is in progress —
// the session layer translates it to LogonRefusal() on the wire.
var ErrDraining = errors.New("fix: gateway draining — session not available")

// DrainSession is the drain-facing view of one live FIX session.
// The session layer (sibling) wraps its session handle + send path.
type DrainSession interface {
	// ID is the canonical session key.
	ID() string
	// Send enqueues an outbound message on the session.
	Send(ctx context.Context, m *quickfix.Message) error
	// LogoutDone closes when the peer's Logout(35=5) arrives or the
	// socket drops. May be nil — treated as never-done.
	LogoutDone() <-chan struct{}
	// Close forces the TCP disconnect after the wait window.
	Close() error
}

// DrainRegistry enumerates live sessions for the drain sweep. nil =
// no acceptor wired yet (scaffold) → Drain is an immediate no-op.
type DrainRegistry interface {
	ActiveSessions() []DrainSession
}

// DrainConfig tunes Drainer.
type DrainConfig struct {
	// LogoutWait is the aggregate peer-Logout deadline; DrainLogoutWait
	// when 0, hard-capped at 10s.
	LogoutWait time.Duration
	// ReplacementEndpoint directs clients to a ready secondary gateway
	// in the News advisory (Task 18.3.17 item 3); "" = omit.
	ReplacementEndpoint string
}

// Drainer owns the maintenance-drain state machine.
type Drainer struct {
	draining atomic.Bool
	reg      DrainRegistry
	cfg      DrainConfig
	log      *slog.Logger
	now      func() time.Time
}

// NewDrainer binds the drainer. reg may be nil until the acceptor
// layer lands; now is injectable for tests.
func NewDrainer(reg DrainRegistry, cfg DrainConfig, log *slog.Logger, now func() time.Time) *Drainer {
	if cfg.LogoutWait <= 0 || cfg.LogoutWait > DrainLogoutWait {
		cfg.LogoutWait = DrainLogoutWait
	}
	if now == nil {
		now = time.Now
	}
	return &Drainer{reg: reg, cfg: cfg, log: log, now: now}
}

// Draining reports whether a drain is in progress.
func (d *Drainer) Draining() bool { return d.draining.Load() }

// AdmitLogon is the logon gate: ErrDraining while draining, nil
// otherwise. Wire into the acceptor's session-create path.
func (d *Drainer) AdmitLogon() error {
	if d.draining.Load() {
		return ErrDraining
	}
	return nil
}

// LogonRefusal is the on-the-wire refusal for a Logon arriving
// mid-drain: Logout(35=5) with Text=SESSION_NOT_AVAILABLE — FIX has no
// dedicated logon-reject MsgType; replying Logout is the standards
// mechanism for refusing a Logon.
func (d *Drainer) LogonRefusal() *quickfix.Message {
	return NewLogout("SESSION_NOT_AVAILABLE: " + DrainMaintenanceText)
}

// newsAdvisory builds the maintenance News(35=B) broadcast.
func (d *Drainer) newsAdvisory() *quickfix.Message {
	headline := DrainMaintenanceText
	if d.cfg.ReplacementEndpoint != "" {
		headline += " — reconnect to " + d.cfg.ReplacementEndpoint
	}
	return NewNews(headline)
}

// Drain runs the orderly shutdown sequence. It is idempotent (second
// caller observes the first drain). Send failures are logged and the
// sweep continues — a wedged session must not stall the maintenance
// window; the aggregate peer-Logout wait is bounded by LogoutWait.
func (d *Drainer) Drain(ctx context.Context) error {
	if !d.draining.CompareAndSwap(false, true) {
		return nil // already draining/drained
	}
	if d.reg == nil {
		d.logf("fix drain: no session registry wired — nothing to drain")
		return nil
	}
	sessions := d.reg.ActiveSessions()
	if len(sessions) == 0 {
		return nil
	}
	advisory := d.newsAdvisory()
	var firstErr error
	for _, s := range sessions {
		if err := s.Send(ctx, advisory); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("fix drain: news advisory to %s: %w", s.ID(), err)
		}
	}
	for _, s := range sessions {
		if err := s.Send(ctx, NewLogout(DrainMaintenanceText)); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("fix drain: logout to %s: %w", s.ID(), err)
		}
	}
	// Await peer Logouts — all LogoutDone channels share one aggregate
	// deadline (≤10s; spec §9.3 bounds each peer's response at 5s).
	var wg sync.WaitGroup
	for _, s := range sessions {
		done := s.LogoutDone()
		if done == nil {
			continue // no waiter → will be force-closed
		}
		wg.Add(1)
		go func(ch <-chan struct{}) {
			defer wg.Done()
			select {
			case <-ch:
			case <-ctx.Done():
			}
		}(done)
	}
	allDone := make(chan struct{})
	go func() { wg.Wait(); close(allDone) }()
	timer := time.NewTimer(d.cfg.LogoutWait)
	defer timer.Stop()
	select {
	case <-allDone:
		d.logf("fix drain: %d sessions logged out cleanly", len(sessions))
	case <-timer.C:
		d.logf("fix drain: logout wait expired; force-closing %d sessions", len(sessions))
	case <-ctx.Done():
		d.logf("fix drain: context done; force-closing %d sessions", len(sessions))
	}
	// Uniform teardown: close every socket (peers that already logged
	// out tolerate a second close; stragglers get the forced drop).
	for _, s := range sessions {
		_ = s.Close()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return firstErr
}

func (d *Drainer) logf(format string, args ...any) {
	if d.log != nil {
		d.log.Info(fmt.Sprintf(format, args...))
	}
}

// ---------------------------------------------------------------------------
// quickfixgo adapter — DrainSession over the sibling App/Gateway seams
// ---------------------------------------------------------------------------

// LiveDrainRegistry adapts the session layer to DrainRegistry: Sessions
// supplies the live SessionID set (App.ActiveSessions in production).
// Sessions may be nil → zero live sessions.
type LiveDrainRegistry struct {
	Sessions func() []quickfix.SessionID
	// PollInterval for the LogoutDone membership poll; 25ms when 0.
	PollInterval time.Duration
}

// ActiveSessions snapshots the live set as DrainSession handles.
func (r LiveDrainRegistry) ActiveSessions() []DrainSession {
	if r.Sessions == nil {
		return nil
	}
	ids := r.Sessions()
	out := make([]DrainSession, 0, len(ids))
	for _, sid := range ids {
		out = append(out, &quickFIXDrainSession{sid: sid, reg: r})
	}
	return out
}

// quickFIXDrainSession is a DrainSession bound to one quickfix SessionID.
type quickFIXDrainSession struct {
	sid quickfix.SessionID
	reg LiveDrainRegistry
}

// ID returns the canonical session key.
func (s *quickFIXDrainSession) ID() string { return s.sid.String() }

// Send transmits via the session's channel — quickfix.SendToTarget is
// safe for both tag-value and future SBE transports because the engine
// owns serialization.
func (s *quickFIXDrainSession) Send(ctx context.Context, m *quickfix.Message) error {
	if err := quickfix.SendToTarget(m, s.sid); err != nil {
		return fmt.Errorf("send to %s: %w", s.sid, err)
	}
	return nil
}

// LogoutDone polls the live-session membership until this session drops
// out (peer Logout processed / socket closed). The goroutine
// self-terminates at DrainLogoutWait+2s so a wedged session cannot leak
// it past the drain.
func (s *quickFIXDrainSession) LogoutDone() <-chan struct{} {
	done := make(chan struct{})
	interval := s.reg.PollInterval
	if interval <= 0 {
		interval = 25 * time.Millisecond
	}
	go func() {
		defer close(done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		expire := time.NewTimer(DrainLogoutWait + 2*time.Second)
		defer expire.Stop()
		for {
			select {
			case <-expire.C:
				return
			case <-tick.C:
			}
			if s.reg.Sessions == nil {
				return
			}
			found := false
			for _, sid := range s.reg.Sessions() {
				if sid == s.sid {
					found = true
					break
				}
			}
			if !found {
				return
			}
		}
	}()
	return done
}

// Close force-drops the session — quickfix.UnregisterSession removes the
// dynamic session from the registry and closes its transport.
func (s *quickFIXDrainSession) Close() error {
	return quickfix.UnregisterSession(s.sid)
}
