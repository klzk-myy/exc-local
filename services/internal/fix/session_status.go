// session_status.go — Phase-18 Task 18.3.15: FIX TradingSessionStatus
// (35=h) broadcast within 50ms of instrument/market state change (§24
// #243, Phase-18 AC row 44).
//
// Event sources (both behind injectable channel seams so tests and
// alternative transports stay honest):
//
//   - Instrument lifecycle: NATS subject "marketdata.security_status"
//     (admin.SecurityStatusSubject — Phase-15 instrument maintenance /
//     lifecycle emit admin.SecurityStatusEvent JSON; NATSSource adapts
//     a core-NATS subscribe). PublishInstrumentStatus is the direct
//     push seam for embedders/tests.
//   - Venue session lifecycle: Phase-15 weekly session machine events
//     (admin.SessionEvent — session.open / session.pre_open /
//     session.pre_close / session.closed). That pipeline currently
//     publishes on the WS channel "session.status" only — wiring a
//     NATS republisher onto subject "session.status" (or calling
//     PublishSessionEvent from a SessionPublisher tap) activates this
//     path; see the package report for the gap note.
//
// TradSesStatus(340) mapping per Phase-18 Task 18.3.15 (remediation
// #35, FIX 4.4 dictionary corrected):
//
//	ACTIVE → 2 (Open)      HALTED → 3 (Closed)     RESTRICTED → 4 (Pre-Open)
//	SUSPENDED → 5 (Pre-Close)   DELISTED → 6       CANCEL_ONLY → none
//	(venue session: OPEN→2  CLOSED→3  PRE_OPEN→4  PRE_CLOSE→5)
//
// CANCEL_ONLY and DRAFT carry no TradSesStatus value — CANCEL_ONLY is
// signalled over the auction channel instead (Task 18.3.15 note); a
// state change into an unmapped state produces no broadcast but still
// updates the cached last-known state.
//
// Client control: TradingSessionStatusRequest (35=g) — 263=0 snapshot
// reply, 263=1 subscribe, 263=2 unsubscribe; optional Symbol(55) scopes
// a subscription to one instrument (absent = all instruments + venue
// session events). Rejections reply 35=h with TradSesStatus=6 (Request
// Rejected) + TradSesStatusRejReason(567) + Text, never a silent drop.
//
// Sibling seams: the acceptor's FromApp dispatch calls
// HandleTradingSessionStatusRequest on MsgType "g"; OnLogout calls
// DropSession. Gateway.BroadcastTradingSessionStatus (the
// recovery.OrchFixBroadcaster seam) delegates to BroadcastVenue.
package fix

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gonats "github.com/nats-io/nats.go"
	"github.com/quickfixgo/quickfix"

	excnats "exchange/internal/nats"
)

// ---------------------------------------------------------------------------
// Tags / enums introduced by this file (shared tags — TagSymbol(55),
// TagText(58), TagTradingSessionID(336), TagTradSesStatus(340),
// TagTradSesStatusRejReas(567) — live in tags.go).
// ---------------------------------------------------------------------------

const (
	TagUnsolicitedIndicator quickfix.Tag = 325
	TagTradSesReqID         quickfix.Tag = 335
	TagTradSesMethod        quickfix.Tag = 337
	TagTradSesMode          quickfix.Tag = 338
)

// TradSesStatus(340) values the venue emits — mapping fixed by Task
// 18.3.15 (remediation #35). Note tradSesDelisted and
// TradSesRequestRejected share wire value 6: the task doc maps
// DELISTED → "6 (Closed)"; request rejections on 35=g replies use the
// same code's dictionary meaning. Solicited replies carry 335 (echo)
// and no UnsolicitedIndicator, so clients can distinguish the two.
const (
	TradSesOpen            = 2 // ACTIVE / OPEN
	TradSesClosed          = 3 // HALTED / CLOSED
	TradSesPreOpen         = 4 // RESTRICTED / PRE_OPEN / auction call
	TradSesPreClose        = 5 // SUSPENDED / PRE_CLOSE
	TradSesRequestRejected = 6 // DELISTED per task doc; request reject on replies
)

// TradSesStatusRejReason(567) values we emit.
const (
	TSRejReasonOther              = 0
	TSRejReasonUnknownSessionOrID = 1 // unknown/invalid TradingSessionID or state
)

// TradSesMethod(337)
const (
	TradSesMethodElectronic = 1
)

// NATS subjects — security status is the live Phase-15 publisher;
// the session-lifecycle subject carries the venue session machine's
// events once bridged (see file header).
const (
	SubjectSecurityStatus  = "marketdata.security_status"
	SubjectSessionStatus   = "session.status"
	venueDefaultSessionTag = "FX"
)

// ---------------------------------------------------------------------------
// Event shapes (decoded locally — no import coupling to internal/admin;
// the JSON field names mirror admin.SecurityStatusEvent / SessionEvent).
// ---------------------------------------------------------------------------

// SecurityStatusEvent is the marketdata.security_status payload.
type SecurityStatusEvent struct {
	Event    string `json:"event"`
	Symbol   string `json:"symbol"`
	Status   string `json:"status"` // lifecycle state on status-driven events
	Field    string `json:"field,omitempty"`
	OldValue string `json:"old_value,omitempty"`
	NewValue string `json:"new_value,omitempty"`
	ChangeID int64  `json:"change_id"`
	Source   string `json:"source"`
	TsMs     int64  `json:"ts_ms"`
}

// SessionLifecycleEvent is the Phase-15 session.status payload.
type SessionLifecycleEvent struct {
	Event            string `json:"event"` // session.open|session.pre_open|session.pre_close|session.closed
	State            string `json:"state"` // OPEN | PRE_OPEN | PRE_CLOSE | CLOSED
	At               string `json:"at"`
	NextState        string `json:"next_state"`
	NextTransitionAt string `json:"next_transition_at"`
}

// StatusSource yields instrument lifecycle events; SessionSource yields
// venue session events. Both close their channel on termination.
type StatusSource func(ctx context.Context) (<-chan SecurityStatusEvent, error)
type SessionSource func(ctx context.Context) (<-chan SessionLifecycleEvent, error)

// ---------------------------------------------------------------------------
// NATS adapter — core-NATS subscribe over the wrapped client
// ---------------------------------------------------------------------------

// NATSChanSource subscribes a core-NATS subject and delivers raw
// payloads. JetStream-published messages reach plain subject
// subscribers (the publish is a regular NATS send plus a stream ack),
// so this binds marketdata.security_status regardless of stream
// provisioning.
func NATSChanSource(nc *excnats.Client, subject string) func(context.Context) (<-chan []byte, error) {
	return func(ctx context.Context) (<-chan []byte, error) {
		raw := make(chan *gonats.Msg, 256)
		sub, err := nc.Conn().ChanSubscribe(subject, raw)
		if err != nil {
			return nil, fmt.Errorf("fix tss: subscribe %q: %w", subject, err)
		}
		out := make(chan []byte, 256)
		go func() {
			defer close(out)
			defer func() { _ = sub.Unsubscribe() }()
			for {
				select {
				case <-ctx.Done():
					return
				case m, ok := <-raw:
					if !ok {
						return
					}
					b := make([]byte, len(m.Data))
					copy(b, m.Data)
					select {
					case out <- b:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return out, nil
	}
}

// SecurityStatusNATS builds a StatusSource over the canonical
// marketdata.security_status subject.
func SecurityStatusNATS(nc *excnats.Client, logf func(string, ...any)) StatusSource {
	raw := NATSChanSource(nc, SubjectSecurityStatus)
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return func(ctx context.Context) (<-chan SecurityStatusEvent, error) {
		in, err := raw(ctx)
		if err != nil {
			return nil, err
		}
		out := make(chan SecurityStatusEvent, 256)
		go func() {
			defer close(out)
			for {
				select {
				case <-ctx.Done():
					return
				case b, ok := <-in:
					if !ok {
						return
					}
					var ev SecurityStatusEvent
					if err := json.Unmarshal(b, &ev); err != nil {
						logf("fix tss: malformed security_status payload: %v", err)
						continue
					}
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return out, nil
	}
}

// SessionLifecycleNATS builds a SessionSource over the session.status
// subject — activates once Phase-15 session events are bridged to NATS
// (see file header).
func SessionLifecycleNATS(nc *excnats.Client, logf func(string, ...any)) SessionSource {
	raw := NATSChanSource(nc, SubjectSessionStatus)
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return func(ctx context.Context) (<-chan SessionLifecycleEvent, error) {
		in, err := raw(ctx)
		if err != nil {
			return nil, err
		}
		out := make(chan SessionLifecycleEvent, 256)
		go func() {
			defer close(out)
			for {
				select {
				case <-ctx.Done():
					return
				case b, ok := <-in:
					if !ok {
						return
					}
					var ev SessionLifecycleEvent
					if err := json.Unmarshal(b, &ev); err != nil {
						logf("fix tss: malformed session.status payload: %v", err)
						continue
					}
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return out, nil
	}
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// SessionStatusDeps wires the TradingSessionStatus broadcaster.
// Sender is required; every other seam is optional.
type SessionStatusDeps struct {
	Sender MessageSender
	// SecurityStatus / SessionEvents are the event sources; nil sources
	// park their loops — Publish* methods still drive broadcasts.
	SecurityStatus StatusSource
	SessionEvents  SessionSource
	// InstrumentState resolves a symbol's current lifecycle state for
	// 35=g snapshot replies (production wires the Redis
	// instrument:status:{symbol} feed). Nil → last-observed state only.
	InstrumentState func(symbol string) (state string, ok bool)
	// VenueState resolves the current venue session state for
	// snapshot replies. Nil → last-observed only.
	VenueState func() (state string, ok bool)
	// Known validates a symbol on 35=g requests (same lookup as the
	// market-data handler). Nil → any symbol shape accepted.
	Known func(symbol string) bool
	// TradingSessionID is the venue session tag emitted in 336;
	// default "FX".
	TradingSessionID string
	// Now is injectable — the 50ms latency test steps a fake clock.
	Now func() time.Time
	// Latency is invoked after each broadcast batch with
	// handler-entry→last-send duration (kind "instrument"|"venue").
	// Wire to metrics; nil disables.
	Latency func(kind string, d time.Duration)
	// Logf defaults to slog.
	Logf func(format string, args ...any)
}

// tsSub is one TradingSessionStatusRequest subscription.
type tsSub struct {
	reqID  string
	symbol string // "" = all instruments + venue session events
}

// SessionStatusService owns 35=g subscription state and the
// state-change→35=h fan-out. Goroutine-safe.
type SessionStatusService struct {
	deps SessionStatusDeps

	mu        sync.Mutex
	subs      map[quickfix.SessionID]map[string]tsSub // sessionID → reqID → sub
	lastState map[string]string                       // symbol → last lifecycle state
	venueLast string                                  // last venue session state

	eventsIn   atomic.Uint64
	broadcasts atomic.Uint64
	skipped    atomic.Uint64
	sendErrors atomic.Uint64
	lastLatNs  atomic.Int64 // most recent broadcast latency (ns)
}

// NewSessionStatusService fails closed on a missing Sender.
func NewSessionStatusService(d SessionStatusDeps) (*SessionStatusService, error) {
	if d.Sender == nil {
		return nil, fmt.Errorf("fix tss: requires a MessageSender")
	}
	if d.TradingSessionID == "" {
		d.TradingSessionID = venueDefaultSessionTag
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		l := slog.Default()
		d.Logf = func(f string, a ...any) { l.Warn(fmt.Sprintf("fix tss: "+f, a...)) }
	}
	return &SessionStatusService{
		deps:      d,
		subs:      map[quickfix.SessionID]map[string]tsSub{},
		lastState: map[string]string{},
	}, nil
}

// ---------------------------------------------------------------------------
// State mapping (Task 18.3.15 remediation-#35 table)
// ---------------------------------------------------------------------------

// mapInstrumentStatus maps an §7.1 instrument lifecycle state to
// TradSesStatus(340). Unmapped states (DRAFT, CANCEL_ONLY) return
// ok=false — no broadcast (CANCEL_ONLY is carried by the auction
// channel instead).
func mapInstrumentStatus(state string) (int, bool) {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "ACTIVE":
		return TradSesOpen, true
	case "HALTED":
		return TradSesClosed, true
	case "RESTRICTED":
		return TradSesPreOpen, true
	case "SUSPENDED":
		return TradSesPreClose, true
	case "DELISTED":
		return TradSesRequestRejected, true // task-doc mapping: 6
	default: // DRAFT, CANCEL_ONLY, unknown
		return 0, false
	}
}

// mapSessionState maps the §6.7 weekly session state (and the recovery
// orchestrator's OrchSessionStatus vocabulary) to TradSesStatus(340).
// AUCTION broadcasts as Pre-Open — the reopening call is the order-
// accumulation window (Task 15.3.6 semantics).
func mapSessionState(state string) (int, bool) {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "OPEN":
		return TradSesOpen, true
	case "CLOSED", "HALT", "HALTED":
		return TradSesClosed, true
	case "PRE_OPEN", "AUCTION":
		return TradSesPreOpen, true
	case "PRE_CLOSE":
		return TradSesPreClose, true
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// Inbound 35=g
// ---------------------------------------------------------------------------

// HandleTradingSessionStatusRequest processes 35=g. Malformed required
// fields return MessageRejectError (session layer emits 35=3); business
// problems reply 35=h with 340=6 + 567 + Text.
func (s *SessionStatusService) HandleTradingSessionStatusRequest(
	id quickfix.SessionID, msg *quickfix.Message) quickfix.MessageRejectError {
	reqID, err := msg.Body.GetString(TagTradSesReqID)
	if err != nil {
		return err // required
	}
	subType, err := msg.Body.GetInt(TagSubscriptionRequestType)
	if err != nil {
		return err // required
	}
	var symbol string
	if msg.Body.Has(TagSymbol) {
		symbol, err = msg.Body.GetString(TagSymbol)
		if err != nil {
			return err
		}
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
	}
	// TradingSessionID(336), when present, must name this venue.
	if msg.Body.Has(TagTradingSessionID) {
		tsid, err := msg.Body.GetString(TagTradingSessionID)
		if err != nil {
			return err
		}
		if tsid != "" && tsid != s.deps.TradingSessionID {
			s.sendReply(id, s.buildStatus(reqID, "", TradSesRequestRejected,
				TSRejReasonUnknownSessionOrID, "UNKNOWN_TRADING_SESSION_ID", false))
			return nil
		}
	}
	if subType != SubTypeSnapshot && subType != SubTypeSnapshotUpdates &&
		subType != SubTypeDisable {
		s.sendReply(id, s.buildStatus(reqID, "", TradSesRequestRejected,
			TSRejReasonOther, "UNSUPPORTED_SUBSCRIPTION_REQUEST_TYPE", false))
		return nil
	}
	if symbol != "" && s.deps.Known != nil && !s.deps.Known(symbol) {
		s.sendReply(id, s.buildStatus(reqID, symbol, TradSesRequestRejected,
			TSRejReasonOther, "UNKNOWN_SYMBOL", false))
		return nil
	}

	s.mu.Lock()
	switch subType {
	case SubTypeDisable:
		if reqs := s.subs[id]; reqs != nil {
			delete(reqs, reqID)
			if len(reqs) == 0 {
				delete(s.subs, id)
			}
		}
		s.mu.Unlock()
		return nil
	case SubTypeSnapshotUpdates:
		reqs := s.subs[id]
		if reqs == nil {
			reqs = map[string]tsSub{}
			s.subs[id] = reqs
		}
		reqs[reqID] = tsSub{reqID: reqID, symbol: symbol}
		s.mu.Unlock()
	default: // SubTypeSnapshot — reply only
		s.mu.Unlock()
	}

	// Reply with the current state — resolves from last-observed events
	// or the injectable lookups; a never-seen state rejects honestly
	// rather than guessing Open (§2.7).
	s.replyCurrentState(id, reqID, symbol)
	return nil
}

// replyCurrentState answers a 35=g request with the resolved current
// state for the requested scope (one instrument, or the venue session).
func (s *SessionStatusService) replyCurrentState(id quickfix.SessionID,
	reqID, symbol string) {
	if symbol != "" {
		state, ok := s.lookupInstrumentState(symbol)
		if !ok {
			s.sendReply(id, s.buildStatus(reqID, symbol, TradSesRequestRejected,
				TSRejReasonUnknownSessionOrID, "STATE_UNAVAILABLE", false))
			return
		}
		code, mapped := mapInstrumentStatus(state)
		if !mapped {
			// CANCEL_ONLY/DRAFT have no TradSesStatus — reply rejected
			// with Text explaining; the state itself is carried by the
			// auction channel per the task note.
			s.sendReply(id, s.buildStatus(reqID, symbol, TradSesRequestRejected,
				TSRejReasonOther, "STATE_UNMAPPED:"+state, false))
			return
		}
		s.sendReply(id, s.buildStatus(reqID, symbol, code, 0, "", false))
		return
	}
	state, ok := s.lookupVenueState()
	if !ok {
		s.sendReply(id, s.buildStatus(reqID, "", TradSesRequestRejected,
			TSRejReasonUnknownSessionOrID, "STATE_UNAVAILABLE", false))
		return
	}
	code, mapped := mapSessionState(state)
	if !mapped {
		s.sendReply(id, s.buildStatus(reqID, "", TradSesRequestRejected,
			TSRejReasonOther, "STATE_UNMAPPED:"+state, false))
		return
	}
	s.sendReply(id, s.buildStatus(reqID, "", code, 0, "", false))
}

func (s *SessionStatusService) lookupInstrumentState(symbol string) (string, bool) {
	if s.deps.InstrumentState != nil {
		if st, ok := s.deps.InstrumentState(symbol); ok {
			return st, true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.lastState[symbol]
	return st, ok
}

func (s *SessionStatusService) lookupVenueState() (string, bool) {
	if s.deps.VenueState != nil {
		if st, ok := s.deps.VenueState(); ok {
			return st, true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.venueLast == "" {
		return "", false
	}
	return s.venueLast, true
}

// ---------------------------------------------------------------------------
// Broadcast paths — event ingest → 35=h fan-out
// ---------------------------------------------------------------------------

// Run consumes the configured sources until ctx ends. Nil sources park.
func (s *SessionStatusService) Run(ctx context.Context) error {
	var secCh <-chan SecurityStatusEvent
	var sesCh <-chan SessionLifecycleEvent
	if s.deps.SecurityStatus != nil {
		ch, err := s.deps.SecurityStatus(ctx)
		if err != nil {
			return fmt.Errorf("fix tss: security status source: %w", err)
		}
		secCh = ch
	}
	if s.deps.SessionEvents != nil {
		ch, err := s.deps.SessionEvents(ctx)
		if err != nil {
			return fmt.Errorf("fix tss: session event source: %w", err)
		}
		sesCh = ch
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-secCh:
			if !ok {
				secCh = nil
				continue
			}
			s.handleInstrumentEvent(ev)
		case ev, ok := <-sesCh:
			if !ok {
				sesCh = nil
				continue
			}
			s.handleSessionEvent(ev)
		}
	}
}

// PublishInstrumentStatus is the direct-injection seam — same handling
// as a NATS-consumed event (unit tests and embedders call it).
func (s *SessionStatusService) PublishInstrumentStatus(ev SecurityStatusEvent) {
	s.handleInstrumentEvent(ev)
}

// PublishSessionEvent is the direct-injection seam for venue session
// lifecycle events.
func (s *SessionStatusService) PublishSessionEvent(ev SessionLifecycleEvent) {
	s.handleSessionEvent(ev)
}

// BroadcastVenue pushes a venue-level 35=h to every subscribed session
// — the recovery.OrchFixBroadcaster seam (Gateway.BroadcastTradingSessionStatus
// delegates here with OrchSessionStatus values). Returns the number of
// sessions messaged.
func (s *SessionStatusService) BroadcastVenue(state string) int {
	code, ok := mapSessionState(state)
	if !ok {
		s.skipped.Add(1)
		return 0
	}
	start := s.deps.Now()
	s.mu.Lock()
	s.venueLast = strings.ToUpper(state)
	n := s.broadcastLocked(s.buildStatus("", "", code, 0, "", true))
	s.mu.Unlock()
	s.noteLatency("venue", start)
	return n
}

// handleInstrumentEvent is the state-change→broadcast path: ingest →
// map → fan out to matching subscribers; latency measured from handler
// entry to last send (the ≤50ms contract, §24 #243).
func (s *SessionStatusService) handleInstrumentEvent(ev SecurityStatusEvent) {
	start := s.deps.Now()
	s.eventsIn.Add(1)
	symbol := strings.ToUpper(strings.TrimSpace(ev.Symbol))
	if symbol == "" || ev.Status == "" {
		s.skipped.Add(1)
		return // parameter-change events (Field set, no Status) carry no state
	}
	code, ok := mapInstrumentStatus(ev.Status)
	s.mu.Lock()
	s.lastState[symbol] = strings.ToUpper(ev.Status)
	s.mu.Unlock()
	if !ok {
		s.skipped.Add(1)
		return
	}
	m := s.buildStatus("", symbol, code, 0, "", true)
	s.mu.Lock()
	s.broadcastSymbolLocked(m, symbol)
	s.mu.Unlock()
	s.noteLatency("instrument", start)
}

func (s *SessionStatusService) handleSessionEvent(ev SessionLifecycleEvent) {
	start := s.deps.Now()
	s.eventsIn.Add(1)
	state := strings.ToUpper(strings.TrimSpace(ev.State))
	code, ok := mapSessionState(state)
	s.mu.Lock()
	if state != "" {
		s.venueLast = state
	}
	s.mu.Unlock()
	if !ok {
		s.skipped.Add(1)
		return
	}
	m := s.buildStatus("", "", code, 0, "", true)
	s.mu.Lock()
	s.broadcastLocked(m)
	s.mu.Unlock()
	s.noteLatency("venue", start)
}

func (s *SessionStatusService) noteLatency(kind string, start time.Time) {
	d := s.deps.Now().Sub(start)
	s.lastLatNs.Store(d.Nanoseconds())
	if s.deps.Latency != nil {
		s.deps.Latency(kind, d)
	}
}

// broadcastLocked sends to every subscribed session (venue-level
// reach — a venue state change is material to all subscribers).
// Caller holds s.mu. Returns sessions messaged.
func (s *SessionStatusService) broadcastLocked(m *quickfix.Message) int {
	n := 0
	for id, reqs := range s.subs {
		if len(reqs) == 0 {
			continue
		}
		s.sendLocked(id, m)
		n++
	}
	s.broadcasts.Add(1)
	return n
}

// broadcastSymbolLocked sends an instrument-scoped 35=h to sessions
// whose subscription covers symbol (symbol-filtered subs match exactly;
// unfiltered subs match everything).
// Caller holds s.mu.
func (s *SessionStatusService) broadcastSymbolLocked(m *quickfix.Message, symbol string) {
	for id, reqs := range s.subs {
		for _, sub := range reqs {
			if sub.symbol == "" || sub.symbol == symbol {
				s.sendLocked(id, m)
				break // one delivery per session
			}
		}
	}
	s.broadcasts.Add(1)
}

// sendLocked ships one message. Caller holds s.mu.
func (s *SessionStatusService) sendLocked(id quickfix.SessionID, m *quickfix.Message) {
	if err := s.deps.Sender.SendTo(m, id); err != nil {
		s.sendErrors.Add(1)
		s.deps.Logf("send %s to %s failed: %v", msgTypeOf(m), id.String(), err)
	}
}

// sendReply ships a solicited reply (no lock held — replies are built
// fresh and ordering vs broadcasts is immaterial).
func (s *SessionStatusService) sendReply(id quickfix.SessionID, m *quickfix.Message) {
	if err := s.deps.Sender.SendTo(m, id); err != nil {
		s.sendErrors.Add(1)
		s.deps.Logf("send %s reply to %s failed: %v", msgTypeOf(m), id.String(), err)
	}
}

// buildStatus assembles a TradingSessionStatus (35=h). symbol "" means
// a venue-level message; unsolicited flags UnsolicitedIndicator(326)=Y.
// rejReason 0 with status == TradSesRequestRejected is still a rejection
// reply (carries 567 + Text); pass rejReason=TSRejReasonOther explicitly.
func (s *SessionStatusService) buildStatus(reqID, symbol string, status,
	rejReason int, text string, unsolicited bool) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgTradingSessionStatus)
	if reqID != "" {
		m.Body.SetString(TagTradSesReqID, reqID)
	}
	m.Body.SetString(TagTradingSessionID, s.deps.TradingSessionID)
	m.Body.SetInt(TagTradSesMethod, TradSesMethodElectronic)
	if symbol != "" {
		m.Body.SetString(TagSymbol, symbol)
	}
	if unsolicited {
		m.Body.SetString(TagUnsolicitedIndicator, "Y")
	}
	m.Body.SetInt(TagTradSesStatus, status)
	if status == TradSesRequestRejected {
		m.Body.SetInt(TagTradSesStatusRejReas, rejReason)
	}
	if text != "" {
		m.Body.SetString(TagText, text)
	}
	m.Body.SetField(TagTransactTime, quickfix.FIXUTCTimestamp{
		Time: s.deps.Now().UTC(), Precision: quickfix.Millis,
	})
	return m
}

// ---------------------------------------------------------------------------
// Lifecycle + introspection
// ---------------------------------------------------------------------------

// DropSession purges a departed session's status subscriptions —
// called from the acceptor's OnLogout/OnDisconnect path.
func (s *SessionStatusService) DropSession(id quickfix.SessionID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, id)
}

// SubscriptionCount reports live status subscriptions for a session.
func (s *SessionStatusService) SubscriptionCount(id quickfix.SessionID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[id])
}

// TSSStats is the broadcaster's counter snapshot.
type TSSStats struct {
	EventsIn      uint64
	Broadcasts    uint64
	Skipped       uint64
	SendErrors    uint64
	LastLatencyNs int64
}

// Stats returns the counter snapshot.
func (s *SessionStatusService) Stats() TSSStats {
	return TSSStats{
		EventsIn:      s.eventsIn.Load(),
		Broadcasts:    s.broadcasts.Load(),
		Skipped:       s.skipped.Load(),
		SendErrors:    s.sendErrors.Load(),
		LastLatencyNs: s.lastLatNs.Load(),
	}
}
