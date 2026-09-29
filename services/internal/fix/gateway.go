// Gateway owns the QuickFIX acceptor/initiator lifecycle inside the
// fix binary: construct → Start (bind acceptor, connect initiators) →
// Stop (graceful Logout drain ≤10s → hard close). The OrchFix
// broadcaster seams (18.3.15 TradingSessionStatus, 18.3.12 gap
// resolution) hang off Gateway so sibling tasks never re-open
// quickfixgo plumbing.
package fix

import (
	"context"
	"fmt"
	"time"

	"github.com/quickfixgo/quickfix"

	vencfg "exchange/internal/config"
)

// DrainWait caps the graceful-shutdown peer-logout wait (spec §9.3 /
// Task 9.3.23): send 35=5 Text="Scheduled maintenance" to every active
// session, then give peers at most 10s to answer before the engine
// closes sockets.
const DrainWait = 10 * time.Second

// LogoutDrainText is stamped as Text(58) on the maintenance Logout.
const LogoutDrainText = "Scheduled maintenance"

// Gateway wires one QuickFIX Acceptor (inbound order-entry/drop-copy)
// plus zero or more Initiators (outward sessions).
type Gateway struct {
	app       *App
	msgStore  quickfix.MessageStoreFactory
	log       Log
	acceptor  *quickfix.Acceptor
	initiator *quickfix.Initiator
}

// NewGateway builds both halves from one fix config; either side may be
// absent (acceptor nil when cfg.Enabled=false, initiator nil without
// outward sessions) — the binary refuses to start with neither.
func NewGateway(app *App, cfg vencfg.FixConfig,
	storeFac quickfix.MessageStoreFactory, lg Log) (*Gateway, error) {
	g := &Gateway{app: app, msgStore: storeFac, log: lg}
	logFac := quickfix.NewNullLogFactory()

	if cfg.Enabled {
		settings, err := AcceptorSettings(cfg)
		if err != nil {
			return nil, err
		}
		g.acceptor, err = quickfix.NewAcceptor(app, storeFac, settings, logFac)
		if err != nil {
			return nil, fmt.Errorf("fix: acceptor: %w", err)
		}
	}
	if len(cfg.OutwardSessions) > 0 {
		settings, err := InitiatorSettings(cfg)
		if err != nil {
			return nil, err
		}
		g.initiator, err = quickfix.NewInitiator(app, storeFac, settings, logFac)
		if err != nil {
			return nil, fmt.Errorf("fix: initiator: %w", err)
		}
	}
	if g.acceptor == nil && g.initiator == nil {
		return nil, fmt.Errorf("fix: neither acceptor nor outward sessions configured")
	}
	return g, nil
}

// Start binds/connects non-blocking.
func (g *Gateway) Start() error {
	if g.acceptor != nil {
		if err := g.acceptor.Start(); err != nil {
			return fmt.Errorf("fix: acceptor start: %w", err)
		}
	}
	if g.initiator != nil {
		if err := g.initiator.Start(); err != nil {
			return fmt.Errorf("fix: initiator start: %w", err)
		}
	}
	if g.log != nil {
		g.log.Info("fix: gateway started")
	}
	return nil
}

// AcceptorStarted reports whether the inbound listener is bound — the
// /healthz surface ANDs this with process liveness.
func (g *Gateway) AcceptorStarted() bool { return g.acceptor != nil }

// Stop performs the Task 9.3.23 graceful drain: Logout(35=5) with
// Text(58)=Scheduled maintenance to every active session, wait ≤10s
// for peer Logout responses (quickfixgo's LogoutTimeout=5s bounds the
// per-session wait internally), then close the transport.
func (g *Gateway) Stop(ctx context.Context) {
	actives := g.app.ActiveSessions()
	for _, sid := range actives {
		m := quickfix.NewMessage()
		m.Header.SetField(TagMsgType, quickfix.FIXString(MsgLogout))
		m.Body.SetString(TagText, LogoutDrainText)
		if err := quickfix.SendToTarget(m, sid); err != nil {
			if g.log != nil {
				g.log.Warn("fix: drain logout send failed", "session", sid, "err", err)
			}
		}
	}
	// Wait for peers to close out — each session's LogoutTimeout (5s)
	// force-drops stragglers; we bound the whole wait at DrainWait.
	deadline := time.Now().Add(DrainWait)
	for len(g.app.ActiveSessions()) > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			goto done
		case <-time.After(25 * time.Millisecond):
		}
	}
done:
	if g.acceptor != nil {
		g.acceptor.Stop()
	}
	if g.initiator != nil {
		g.initiator.Stop()
	}
	if g.log != nil {
		g.log.Info("fix: gateway stopped")
	}
}

// ---------------------------------------------------------------------------
// OrchFix broadcaster seams — Tasks 18.3.15 / 18.3.12 fill these in.
// ---------------------------------------------------------------------------

// BroadcastTradingSessionStatus pushes a venue-level 35=h to every
// session subscribed through the SessionStatusService — the venue-wide
// session-status surface Task 18.3.15 drives from the 24/5 lifecycle.
// When TSS is unbound it falls back to the raw all-sessions broadcast
// (foundational shape); the service path maps OrchSessionStatus values
// and honours per-session subscription filters.
func (g *Gateway) BroadcastTradingSessionStatus(tradSesStatus string) {
	if tss := g.app.opt.TSS; tss != nil {
		tss.BroadcastVenue(tradSesStatus)
		return
	}
	for _, sid := range g.app.ActiveSessions() {
		m := quickfix.NewMessage()
		m.Header.SetField(TagMsgType, quickfix.FIXString(MsgTradingSessionStatus))
		m.Body.SetString(TagTradSesStatus, tradSesStatus)
		if err := quickfix.SendToTarget(m, sid); err != nil && g.log != nil {
			g.log.Warn("fix: TSS broadcast failed", "session", sid, "err", err)
		}
	}
}
