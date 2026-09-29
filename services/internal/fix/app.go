// Application — the quickfixgo Application implementation: logon
// authentication (SenderCompID pair + API-key credential), inbound
// throttling, entitlement checks, order-message dispatch and
// cancel-on-disconnect. Session protocol mechanics (heartbeat,
// ResendRequest replay, gap detection, timeouts) are quickfixgo's —
// this layer never hand-rolls FIX framing.
package fix

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/config"
	"exchange/internal/orders"
	excerrors "exchange/pkg/errors"
)

// Log is the minimal logging seam — *slog.Logger satisfies it.
type Log interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Options wires the Application's dependencies. OrderFlow/Store are
// required for order-entry sessions; DeadManTimer is required for the
// 35=BE countdown command (nil → countdown requests rejected).
type Options struct {
	Store       Store
	Orders      OrderFlow
	OrderRead   OrderRead
	DeadMan     DeadManTimer
	SubAccounts SubAccountChecker
	Log         Log
	Now         func() time.Time
	// TSS handles TradingSessionStatusRequest(35=g) — nil →
	// MSGTYPE_UNSUPPORTED. Task 18.3.15's SessionStatusService binds it.
	TSS *SessionStatusService
	// MDS handles MarketDataRequest(35=V) — nil → MSGTYPE_UNSUPPORTED.
	// Task 18.3.3's MarketDataService binds it.
	MDS *MarketDataService
	// CoD owns the measured cancel-on-disconnect path (rate gate §24
	// #245 + audit events) — nil falls back to the inline purge below.
	CoD *CoDExecutor
	// LogonGate refuses inbound Logons while a maintenance drain is in
	// progress (Task 18.3.17/9.3.23). *Drainer satisfies it; nil admits.
	LogonGate interface{ AdmitLogon() error }
}

// App is the quickfix.Application plus the venue's session policy.
type App struct {
	opt   Options
	bus   *ReportBus
	throt *throttleSet
	now   func() time.Time
	log   Log

	mu       sync.Mutex
	orderly  map[string]bool // sessionID → peer sent 35=5
	sessions map[string]quickfix.SessionID
}

// NewApp builds the application. The ReportBus is created empty —
// register taps via App.Report().WithTap.
func NewApp(o Options) *App {
	lg := o.Log
	if lg == nil {
		lg = slog.Default()
	}
	return &App{
		opt:      o,
		bus:      &ReportBus{},
		throt:    newThrottleSet(o.Now),
		now:      coalesceNow(o.Now),
		log:      lg,
		orderly:  map[string]bool{},
		sessions: map[string]quickfix.SessionID{},
	}
}

func coalesceNow(f func() time.Time) func() time.Time {
	if f == nil {
		return time.Now
	}
	return f
}

// Report exposes the ExecutionReport fan-out bus (drop-copy tap).
func (a *App) Report() *ReportBus { return a.bus }

// ActiveSessions returns the currently logged-on session IDs — the
// graceful-drain iterator used by Gateway.Stop and the ops surface.
func (a *App) ActiveSessions() []quickfix.SessionID {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]quickfix.SessionID, 0, len(a.sessions))
	for _, id := range a.sessions {
		out = append(out, id)
	}
	return out
}

// SetLogonGate binds the maintenance-drain logon gate post-construction
// (the Drainer needs App.ActiveSessions, so it is necessarily built
// after the App). Safe to call before the acceptor starts.
func (a *App) SetLogonGate(g interface{ AdmitLogon() error }) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opt.LogonGate = g
}

// ---------------------------------------------------------------------------
// quickfix.Application
// ---------------------------------------------------------------------------

func (a *App) OnCreate(sessionID quickfix.SessionID) {}

// OnLogon marks the session ACTIVE.
func (a *App) OnLogon(sessionID quickfix.SessionID) {
	a.mu.Lock()
	a.sessions[sessionID.String()] = sessionID
	a.mu.Unlock()
	now := a.now()
	if err := a.opt.Store.SetStatus(context.Background(), sessionID.String(),
		"ACTIVE", &now); err != nil {
		a.log.Error("fix: session status update failed", "session", sessionID, "err", err)
	}
	a.log.Info("fix: session logged on", "session", sessionID)
}

// OnLogout implements cancel-on-disconnect (spec §9.3, §24 #136/#153):
// a disconnect not preceded by an orderly Logout(35=5) mass-cancels the
// session's resting orders when the row carries cancel_on_disconnect.
// Orderly logouts preserve orders.
func (a *App) OnLogout(sessionID quickfix.SessionID) {
	key := sessionID.String()
	a.mu.Lock()
	orderly := a.orderly[key]
	delete(a.orderly, key)
	delete(a.sessions, key)
	a.mu.Unlock()
	a.throt.Drop(key)
	if a.opt.TSS != nil {
		a.opt.TSS.DropSession(sessionID)
	}
	if a.opt.MDS != nil {
		a.opt.MDS.DropSession(sessionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if a.opt.CoD != nil {
		ev, err := a.opt.CoD.OnDisconnect(ctx, key, "socket_drop", orderly)
		if err != nil {
			a.log.Error("fix: CoD executor failed", "session", sessionID, "err", err)
		} else if ev != nil && !ev.Graceful && ev.Cancelled > 0 {
			a.log.Info("fix: cancel-on-disconnect mass-cancel executed",
				"session", sessionID, "cancelled", ev.Cancelled,
				"elapsed", ev.Elapsed)
		}
		return
	}
	if err := a.opt.Store.SetStatus(ctx, key, "DISCONNECTED", nil); err != nil {
		a.log.Error("fix: session status update failed", "session", sessionID, "err", err)
	}
	if orderly {
		a.log.Info("fix: orderly logout — orders preserved", "session", sessionID)
		return
	}
	row, err := a.opt.Store.SessionByID(ctx, key)
	if err != nil || row == nil {
		if err != nil {
			a.log.Error("fix: CoD session lookup failed", "session", sessionID, "err", err)
		}
		return
	}
	if !row.CancelOnDisconnect || row.AccountID == nil {
		return
	}
	res, err := a.opt.Orders.CancelOnDisconnect(ctx, *row.AccountID, key)
	if err != nil {
		a.log.Error("fix: cancel-on-disconnect failed", "session", sessionID, "err", err)
		return
	}
	a.log.Info("fix: cancel-on-disconnect mass-cancel executed",
		"session", sessionID, "account", *row.AccountID, "cancelled", res.Cancelled)
}

func (a *App) ToAdmin(*quickfix.Message, quickfix.SessionID)     {}
func (a *App) ToApp(*quickfix.Message, quickfix.SessionID) error { return nil }

// FromAdmin handles Logon authentication and orderly-logout marking.
// An unknown compID pair or a failed credential check returns
// RejectLogon — quickfixgo answers with Logout + TCP drop (fail closed).
func (a *App) FromAdmin(msg *quickfix.Message, sessionID quickfix.SessionID) quickfix.MessageRejectError {
	mt, err := msg.MsgType()
	if err != nil {
		return nil
	}
	switch mt {
	case MsgLogon:
		if a.opt.LogonGate != nil {
			if err := a.opt.LogonGate.AdmitLogon(); err != nil {
				return quickfix.RejectLogon{Text: "SESSION_DRAINING"}
			}
		}
		return a.authenticateLogon(msg, sessionID)
	case MsgLogout:
		a.mu.Lock()
		a.orderly[sessionID.String()] = true
		a.mu.Unlock()
	case MsgHeartbeat:
		now := a.now()
		_ = a.opt.Store.SetStatus(context.Background(), sessionID.String(),
			"ACTIVE", &now)
	}
	return nil
}

// authenticateLogon verifies the session pair is provisioned and the
// presented Username(553)/Password(554) match the row's bound api_keys
// credential (spec §9.3 "SenderCompID + API-key auth on Logon").
func (a *App) authenticateLogon(msg *quickfix.Message,
	sessionID quickfix.SessionID) quickfix.MessageRejectError {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	row, err := a.opt.Store.SessionByID(ctx, sessionID.String())
	if err != nil {
		a.log.Error("fix: session lookup failed", "session", sessionID, "err", err)
		return quickfix.RejectLogon{Text: "SESSION_UNAVAILABLE"}
	}
	if row == nil {
		return quickfix.RejectLogon{Text: "SESSION_NOT_PROVISIONED"}
	}
	user, uerr := msg.Body.GetString(TagUsername)
	pass, perr := msg.Body.GetString(TagPassword)
	if uerr != nil || perr != nil || strings.TrimSpace(user) == "" {
		return quickfix.RejectLogon{Text: "SESSION_CREDENTIALS_REQUIRED"}
	}
	if row.APIKeyID == nil {
		return quickfix.RejectLogon{Text: "SESSION_NOT_PROVISIONED"}
	}
	keyAccount, ok, err := a.opt.Store.VerifyAPIKey(ctx, *row.APIKeyID,
		strings.TrimSpace(user), pass)
	if err != nil {
		a.log.Error("fix: credential verify failed", "session", sessionID, "err", err)
		return quickfix.RejectLogon{Text: "SESSION_UNAVAILABLE"}
	}
	if !ok {
		return quickfix.RejectLogon{Text: "SESSION_INVALID_CREDENTIALS"}
	}
	// Bound-account sessions additionally require the credential's own
	// account to be the bound account or one of its sub-accounts —
	// a stranger's valid key never opens the session.
	if row.AccountID != nil && keyAccount != *row.AccountID {
		sub := false
		if a.opt.SubAccounts != nil {
			if ok, serr := a.opt.SubAccounts.IsSubAccountOf(ctx, *row.AccountID, keyAccount); serr == nil {
				sub = ok
			} else {
				a.log.Error("fix: sub-account check failed", "session", sessionID, "err", serr)
				return quickfix.RejectLogon{Text: "SESSION_UNAVAILABLE"}
			}
		}
		if !sub {
			return quickfix.RejectLogon{Text: "SESSION_NOT_ENTITLED"}
		}
	}
	return nil
}

// FromApp gates every inbound business message through the per-session
// throttle (§24 #137) then dispatches. Rejects are always emitted —
// nothing is silently dropped.
func (a *App) FromApp(msg *quickfix.Message, sessionID quickfix.SessionID) quickfix.MessageRejectError {
	mt, err := msg.MsgType()
	if err != nil {
		return nil
	}
	key := sessionID.String()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	row, serr := a.opt.Store.SessionByID(ctx, key)
	if serr != nil {
		a.log.Error("fix: session lookup failed", "session", sessionID, "err", serr)
		a.emit(sessionID, businessReject(mt, "", "SESSION_UNAVAILABLE",
			BusinessRejectReasonOther))
		return nil
	}
	if row == nil {
		a.emit(sessionID, businessReject(mt, "", "SESSION_NOT_PROVISIONED",
			BusinessRejectReasonNotEntitled))
		return nil
	}
	a.throt.SetRate(key, row.MaxMsgsPerSec)
	if !a.throt.Allow(key, row.MaxMsgsPerSec) {
		a.emit(sessionID, businessReject(mt, clOrdIDOf(msg, mt),
			"SESSION_THROTTLED", BusinessRejectReasonThrottled))
		return nil
	}

	switch mt {
	case MsgNewOrderSingle:
		a.onNewOrderSingle(ctx, msg, sessionID, row)
	case MsgOrderCancelRequest:
		a.onOrderCancelRequest(ctx, msg, sessionID, row)
	case MsgOrderCancelReplace:
		a.onOrderCancelReplace(ctx, msg, sessionID, row)
	case MsgUserRequest:
		a.onUserRequest(ctx, msg, sessionID, row)
	case MsgTradingSessionStatusRequest:
		if a.opt.TSS != nil {
			return a.opt.TSS.HandleTradingSessionStatusRequest(sessionID, msg)
		}
		a.emit(sessionID, businessReject(mt, "",
			"MSGTYPE_UNSUPPORTED", BusinessRejectReasonOther))
	case MsgMarketDataRequest:
		if a.opt.MDS != nil {
			return a.opt.MDS.HandleMarketDataRequest(sessionID, msg)
		}
		a.emit(sessionID, businessReject(mt, "",
			"MSGTYPE_UNSUPPORTED", BusinessRejectReasonOther))
	default:
		a.emit(sessionID, businessReject(mt, "",
			"MSGTYPE_UNSUPPORTED", BusinessRejectReasonOther))
	}
	return nil
}

// clOrdIDOf extracts the best reference id for a reject: ClOrdID for
// order messages, UserRequestID for 35=BE.
func clOrdIDOf(msg *quickfix.Message, mt string) string {
	tag := TagClOrdID
	if mt == MsgUserRequest {
		tag = TagUserRequestID
	}
	if v, err := msg.Body.GetString(tag); err == nil {
		return v
	}
	return ""
}

// emit routes one outbound message through the report bus (send +
// taps). Send failures are logged — a dead session is reaped by
// heartbeat/OnLogout, the emit path never panics.
func (a *App) emit(sessionID quickfix.SessionID, m *quickfix.Message) {
	if err := a.bus.Emit(ReportEvent{SessionID: sessionID, Msg: m}); err != nil {
		a.log.Warn("fix: emit failed", "session", sessionID, "err", err)
	}
}

// sessionAccount resolves the session's bound orders.Account snapshot.
func (a *App) sessionAccount(ctx context.Context, row *Session) (*orders.Account, error) {
	if row.AccountID == nil {
		return nil, excerrors.New("SESSION_NOT_ENTITLED",
			"drop-copy session has no order entry")
	}
	acct, err := a.opt.Orders.AccountByID(ctx, *row.AccountID)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, excerrors.New("SESSION_NOT_ENTITLED",
			fmt.Sprintf("bound account %d not found", *row.AccountID))
	}
	return acct, nil
}

// checkEntitlement enforces §24 #135: the order's Tag-1 Account (when
// present) must equal the bound account or a sub-account of it, and the
// instrument must be inside allowed_instruments.
func (a *App) checkEntitlement(ctx context.Context, row *Session,
	msg *quickfix.Message, symbol string) *MappingError {
	if row.AccountID == nil {
		return &MappingError{Code: "SESSION_NOT_ENTITLED",
			Detail:    "read-only session cannot submit orders",
			OrdReject: OrdRejReasonOther}
	}
	if acctStr := optionalStr(msg, TagAccount); acctStr != "" {
		want, perr := strconv.ParseInt(acctStr, 10, 64)
		sub := false
		if perr == nil && a.opt.SubAccounts != nil {
			if ok, serr := a.opt.SubAccounts.IsSubAccountOf(ctx, *row.AccountID, want); serr == nil {
				sub = ok
			}
		}
		if (perr != nil || want != *row.AccountID) && !sub {
			return &MappingError{Code: "SESSION_NOT_ENTITLED",
				Detail:    "Account(1) outside bound account hierarchy",
				OrdReject: OrdRejReasonOther}
		}
	}
	if !row.Entitled(config.CanonicalSymbol(symbol)) {
		return &MappingError{Code: "SESSION_NOT_ENTITLED",
			Detail:    "instrument outside allowed_instruments",
			OrdReject: OrdRejReasonOther}
	}
	return nil
}
