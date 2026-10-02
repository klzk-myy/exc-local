// Gateway owns the QuickFIX acceptor/initiator lifecycle inside the
// fix binary: construct → Start (bind acceptor, connect initiators) →
// Stop (graceful Logout drain ≤10s → hard close). The OrchFix
// broadcaster seams (18.3.15 TradingSessionStatus, 18.3.12 gap
// resolution) hang off Gateway so sibling tasks never re-open
// quickfixgo plumbing.
package fix

import (
	"context"
	"crypto/tls"
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

// Gateway wires the QuickFIX acceptors (FIX.4.4 inbound
// order-entry/drop-copy + an optional FIXT.1.1/FIX.5.0SP2 listener on
// its own port) plus zero or more Initiators (outward sessions).
type Gateway struct {
	app         *App
	msgStore    quickfix.MessageStoreFactory
	log         Log
	acceptor    *quickfix.Acceptor
	sp2Acceptor *quickfix.Acceptor
	initiator   *quickfix.Initiator
}

// GatewayOpts carries the wiring surfaces Phase-3 Task 3 adds — the
// wire application (certification-wrapped *App under mTLS), the
// injected tls.Config the mTLS fingerprint gate produces, and the
// second-protocol listener flags from cfg.Fix.
type GatewayOpts struct {
	// WireApp is what the acceptors attach to — *App or its
	// WrapCertification wrapper. Nil → app itself.
	WireApp quickfix.Application
	// TLSConfig overrides the settings-built tls.Config on both
	// acceptors (Acceceptor.SetTLSConfig). The mTLS fingerprint gate
	// (tls.go ServerTLSConfig + VerifyConnection) arrives through here;
	// nil leaves quickfixgo's settings-driven transport.
	TLSConfig *tls.Config
	// SP2Enabled binds a FIXT.1.1 + DefaultApplVerID=9 acceptor on
	// cfg.Fix.SP2AcceptorHost/Port. Requires cfg.Fix.TLSEnabled (the
	// SP2 settings builder refuses a plaintext SP2 listener).
	SP2Enabled bool
}

// NewGateway builds both halves from one fix config; either side may be
// absent (acceptor nil when cfg.Enabled=false, initiator nil without
// outward sessions) — the binary refuses to start with neither.
func NewGateway(app *App, cfg vencfg.FixConfig,
	storeFac quickfix.MessageStoreFactory, lg Log, opts GatewayOpts) (*Gateway, error) {
	g := &Gateway{app: app, msgStore: storeFac, log: lg}
	logFac := quickfix.NewNullLogFactory()
	wire := opts.WireApp
	if wire == nil {
		wire = app
	}

	if cfg.Enabled {
		settings, err := AcceptorSettings(cfg)
		if err != nil {
			return nil, err
		}
		g.acceptor, err = quickfix.NewAcceptor(wire, storeFac, settings, logFac)
		if err != nil {
			return nil, fmt.Errorf("fix: acceptor: %w", err)
		}
		if opts.TLSConfig != nil {
			g.acceptor.SetTLSConfig(opts.TLSConfig)
		}
	}
	if opts.SP2Enabled {
		// Dedicated port: clone the listener config with the SP2 bind.
		sp2 := cfg
		if cfg.SP2AcceptorPort != 0 {
			sp2.AcceptorPort = cfg.SP2AcceptorPort
		}
		if cfg.SP2AcceptorHost != "" {
			sp2.AcceptorHost = cfg.SP2AcceptorHost
		}
		settings, err := AcceptorSettingsSP2(sp2)
		if err != nil {
			return nil, err
		}
		g.sp2Acceptor, err = quickfix.NewAcceptor(wire, storeFac, settings, logFac)
		if err != nil {
			return nil, fmt.Errorf("fix: sp2 acceptor: %w", err)
		}
		if opts.TLSConfig != nil {
			g.sp2Acceptor.SetTLSConfig(opts.TLSConfig)
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
	if g.acceptor == nil && g.initiator == nil && g.sp2Acceptor == nil {
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
	if g.sp2Acceptor != nil {
		if err := g.sp2Acceptor.Start(); err != nil {
			return fmt.Errorf("fix: sp2 acceptor start: %w", err)
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

// AcceptorStarted reports whether an inbound listener is bound — the
// /healthz surface ANDs this with process liveness.
func (g *Gateway) AcceptorStarted() bool {
	return g.acceptor != nil || g.sp2Acceptor != nil
}

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
	if g.sp2Acceptor != nil {
		g.sp2Acceptor.Stop()
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
