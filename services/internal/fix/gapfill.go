// Sequence gap resolution, structured rejects, outbound resend planning
// and Cancel-on-Disconnect — Task 18.3.18 (spec §2.7, §9.9, §24 #319)
// plus the resend half of Task 18.3.12 (spec §9.8, §24 #189).
//
// Layering note (honest scope): quickfixgo's in-session machinery already
// performs single-connection ResendRequest/gap-fill against the shared
// pgMessageStore. This file adds what the engine does NOT do —
//   - the >2,500-message unbridgeable-gap kill switch (spec §9.9 item 1:
//     Logout 35=5 Text=EXCESSIVE_SEQUENCE_GAP + forced TCP drop, defending
//     the gateway against replay-exhaustion floods),
//   - the cross-gateway resync verdicts the standby needs after failover
//     (paired with failover.go's shared SeqState),
//   - resend planning over the outbound archive (admin → SequenceReset
//     GapFillFlag=Y, application → PossDupFlag=Y + OrigSendingTime),
//   - structured Reject(35=3)/BusinessMessageReject(35=j) formatting and
//     the §23-code → BusinessRejectReason(380) mapping,
//   - the 50ms-budgeted cancel-on-disconnect executor.
//
// CoD interplay (Task 18.3.16 verification): the socket/heartbeat-driven
// purge here is session-scoped and immediate; the dead-man switch
// (deadman.go → accounts.DeadManService) is the account-level countdown
// shared across REST/WS/FIX. Both funnel into orders.Service.MassCancel —
// different scopes (session_id vs account), no conflict. CoD is gated by
// the §24 #245 one-purge-per-5s window so a flapping client cannot
// multiply mass-cancels.
package fix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/quickfixgo/quickfix"
	goredis "github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Constants (spec §9.3/§9.9)
// ---------------------------------------------------------------------------

const (
	// MaxResendGap — an inbound gap wider than this is unbridgeable:
	// the gateway emits Logout(35=5) Text=EXCESSIVE_SEQUENCE_GAP and
	// drops the TCP connection (spec §9.9 item 1).
	MaxResendGap int64 = 2500

	// CoDBudget — cancel-on-disconnect must purge the session's resting
	// orders within 50ms of the abnormal-disconnect decision (spec §9.9
	// item 3). The budget is measured, not enforced as a hard context
	// deadline: a breached cancel keeps running (fail-closed pessimism —
	// abandoning it would leave live orders on a dead session) and the
	// breach is surfaced on the CoDEvent for alerting.
	CoDBudget = 50 * time.Millisecond

	// CoDRateWindow — max one CoD purge per account per window (spec
	// §24 #245, shared with the WS path). A gated event is audited as
	// RateLimited rather than silently dropped.
	CoDRateWindow = 5 * time.Second

	// DefaultHeartBtInt — FIX heartbeat interval (spec §9.3: 30s).
	DefaultHeartBtInt = 30 * time.Second

	// ExcessiveGapLogoutText is the canonical Logout Text(58) for the
	// >2,500-message unbridgeable gap (spec §9.9).
	ExcessiveGapLogoutText = "EXCESSIVE_SEQUENCE_GAP"
)

// ---------------------------------------------------------------------------
// Inbound sequence assessment
// ---------------------------------------------------------------------------

// InboundAction is the §9.9 verdict for one inbound MsgSeqNum.
type InboundAction int

const (
	// InboundAccept — seq == expected; apply once, advance counters.
	InboundAccept InboundAction = iota
	// InboundResendRequest — seq > expected, gap ≤ MaxResendGap: emit
	// ResendRequest(35=2) and queue the message for ordered delivery.
	InboundResendRequest
	// InboundDropPossDup — seq < expected with PossDupFlag=Y: a replay
	// of an already-consumed message; drop, do not advance counters.
	InboundDropPossDup
	// InboundTooLow — seq < expected without PossDupFlag: fatal
	// protocol fault. Emit Reject(35=3) then Logout — fail-closed.
	InboundTooLow
	// InboundLogoutExcessive — gap > MaxResendGap: Logout(35=5)
	// Text=EXCESSIVE_SEQUENCE_GAP + forced TCP disconnect.
	InboundLogoutExcessive
)

// InboundAssessment is the verdict plus the prebuilt outbound messages
// the session layer must send (Reject/Logout only; the ResendRequest
// parameters are exposed numerically so the caller may also batch).
type InboundAssessment struct {
	Action   InboundAction
	Received int64
	Expected int64
	Gap      int64
	// ResendRequest parameters (Action == InboundResendRequest).
	BeginSeqNo int64
	EndSeqNo   int64 // 0 = through latest (FIX convention)
	// Prebuilt messages for the reject/logout paths (nil otherwise).
	Reject *quickfix.Message
	Logout *quickfix.Message
}

// AssessInbound evaluates one inbound MsgSeqNum(34) against the shared
// expectation per spec §9.9. possDup is the wire PossDupFlag(43).
//
// Usage: the failover layer feeds this from RecordInbound's
// ErrInboundSeqMismatch (received vs stored expectation); a raw session
// layer can call it directly per inbound message.
func AssessInbound(received, expected int64, possDup bool) InboundAssessment {
	a := InboundAssessment{Received: received, Expected: expected}
	switch {
	case received == expected:
		a.Action = InboundAccept
	case received > expected:
		a.Gap = received - expected
		if a.Gap > MaxResendGap {
			a.Action = InboundLogoutExcessive
			a.Logout = NewLogout(ExcessiveGapLogoutText)
			return a
		}
		a.Action = InboundResendRequest
		a.BeginSeqNo = expected
		a.EndSeqNo = 0
	default: // received < expected
		a.Gap = expected - received
		if possDup {
			a.Action = InboundDropPossDup
			return a
		}
		a.Action = InboundTooLow
		a.Reject = NewReject(received, TagMsgSeqNum, "",
			SessionRejectValueIncorrect,
			fmt.Sprintf("MsgSeqNum too low: expected %d, got %d", expected, received))
		a.Logout = NewLogout(fmt.Sprintf(
			"MsgSeqNum %d below expected %d without PossDupFlag", received, expected))
	}
	return a
}

// HeartbeatAbnormal reports whether the session counts as abnormally
// disconnected: no inbound traffic for more than 2× HeartBtInt
// (spec §9.9 item 3 — "2 consecutive missed heartbeats").
func HeartbeatAbnormal(lastHeard, now time.Time, heartBtInt time.Duration) bool {
	if heartBtInt <= 0 {
		heartBtInt = DefaultHeartBtInt
	}
	return now.Sub(lastHeard) > 2*heartBtInt
}

// ---------------------------------------------------------------------------
// Outbound resend planning (bidirectional gap fill, Task 18.3.12 item 4)
// ---------------------------------------------------------------------------

// adminMsgTypes are never retransmitted on ResendRequest — they are
// bridged by SequenceReset GapFillFlag=Y (spec §9.8 item 3). 'j'
// (BusinessMessageReject) is application-level and IS replayed.
var adminMsgTypes = map[string]bool{
	MsgHeartbeat:     true, // 0
	MsgTestRequest:   true, // 1
	MsgResendRequest: true, // 2
	MsgReject:        true, // 3
	MsgSequenceReset: true, // 4
	MsgLogout:        true, // 5
	MsgLogon:         true, // A
}

// IsAdminMsgType reports whether a MsgType(35) is session-administrative.
func IsAdminMsgType(msgType string) bool { return adminMsgTypes[msgType] }

// ArchivedMessage is one persisted outbound message from the execution
// log archive — the production binding is the shared pgMessageStore
// (msgstore.go: Store.Messages returns raw frames; the caller peels
// MsgSeqNum/MsgType/SendingTime off the wire bytes when constructing
// this view, or a future typed archive does it at write time).
type ArchivedMessage struct {
	SeqNum      int64
	MsgType     string // 35=
	SendingTime time.Time
	Raw         []byte // original wire bytes, retransmitted verbatim
}

// OutboundArchiver loads the outbound execution log for resend replay.
type OutboundArchiver interface {
	// Fetch returns every archived message with beginSeq <= seq <= endSeq
	// (endSeq==0: through the latest), ordered by SeqNum.
	Fetch(ctx context.Context, sessionID string, beginSeq, endSeq int64) ([]ArchivedMessage, error)
}

// ResendEntry is one planned outbound action for a ResendRequest reply:
// either a replayed application message or a gap-filling SequenceReset.
type ResendEntry struct {
	// Replay is non-nil → retransmit Raw with PossDupFlag(43)=Y and
	// OrigSendingTime(122)=SendingTime.
	Replay *ArchivedMessage
	// NewSeqNo > 0 (with Replay==nil) → SequenceReset(35=4)
	// GapFillFlag(123)=Y NewSeqNo(36)=NewSeqNo.
	NewSeqNo int64
}

// ErrResendHole marks a missing archive message inside the requested
// range — the gateway cannot prove the skipped seq was administrative,
// so it fails closed instead of silently gap-filling over a possibly
// lost ExecutionReport. Callers should treat this as unbridgeable:
// SequenceReset(reset) or Logout, never guess.
var ErrResendHole = errors.New("fix: resend archive hole — outbound message missing")

// PlanResend converts an incoming ResendRequest(BeginSeqNo(7),
// EndSeqNo(16)) plus the archived outbound messages into the ordered
// replay/gap-fill stream. currentOut is the next outbound seq (upper
// bound for endSeq==0 and for the trailing gap-fill NewSeqNo).
func PlanResend(beginSeq, endSeq, currentOut int64, msgs []ArchivedMessage) ([]ResendEntry, error) {
	if beginSeq < 1 {
		return nil, fmt.Errorf("fix: resend BeginSeqNo %d < 1", beginSeq)
	}
	if endSeq == 0 || endSeq >= currentOut {
		endSeq = currentOut - 1
	}
	if endSeq < beginSeq {
		// Nothing in range — still close the request deterministically
		// with a gap-fill to the current watermark.
		return []ResendEntry{{NewSeqNo: currentOut}}, nil
	}
	bySeq := make(map[int64]ArchivedMessage, len(msgs))
	for _, m := range msgs {
		if m.SeqNum >= beginSeq && m.SeqNum <= endSeq {
			bySeq[m.SeqNum] = m
		}
	}
	var out []ResendEntry
	// flushGap closes a pending run of administrative seqs with a single
	// SequenceReset(GapFillFlag=Y, NewSeqNo=next app seq or range end+1).
	gapFrom := int64(-1)
	flushGap := func(upto int64) {
		if gapFrom >= 0 {
			out = append(out, ResendEntry{NewSeqNo: upto})
			gapFrom = -1
		}
	}
	for seq := beginSeq; seq <= endSeq; seq++ {
		m, ok := bySeq[seq]
		switch {
		case !ok:
			return nil, fmt.Errorf("%w: seq %d in [%d,%d]", ErrResendHole, seq, beginSeq, endSeq)
		case adminMsgTypes[m.MsgType]:
			if gapFrom < 0 {
				gapFrom = seq
			}
		default:
			msg := m // copy: Replay points at a stable value
			flushGap(seq)
			out = append(out, ResendEntry{Replay: &msg})
		}
	}
	flushGap(endSeq + 1)
	return out, nil
}

// ---------------------------------------------------------------------------
// Message builders — raw quickfix.Message/FieldMap per the package
// convention (tags.go: no generated fix44 dictionaries in the module
// graph). MsgSeqNum/SendingTime/CompIDs are stamped by the engine on
// send; headers here carry only MsgType and the replay marks.
// ---------------------------------------------------------------------------

// Tags used only by this file — appended here rather than editing the
// shared tags.go registry mid-flight (dedup candidates when that file
// next changes).
const (
	TagNewSeqNo quickfix.Tag = 36  // SequenceReset
	TagHeadline quickfix.Tag = 148 // News
)

// NewResendRequest builds 35=2: BeginSeqNo(7), EndSeqNo(16) (0 = to latest).
func NewResendRequest(beginSeq, endSeq int64) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgResendRequest))
	m.Body.SetInt(TagBeginSeqNo, int(beginSeq))
	m.Body.SetInt(TagEndSeqNo, int(endSeq))
	return m
}

// NewSequenceReset builds 35=4: NewSeqNo(36), GapFillFlag(123)=Y when
// gapFill — Y bridges skipped administrative seqs; N is the hard reset
// used after an unbridgeable loss.
func NewSequenceReset(newSeqNo int64, gapFill bool) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgSequenceReset))
	m.Body.SetInt(TagNewSeqNo, int(newSeqNo))
	m.Body.SetBool(TagGapFillFlag, gapFill)
	return m
}

// NewLogout builds 35=5 with Text(58).
func NewLogout(text string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgLogout))
	m.Body.SetString(TagText, text)
	return m
}

// NewReject builds a session-level Reject(35=3): RefSeqNum(45),
// RefTagID(371) when refTagID>0, RefMsgType(372) when non-empty,
// SessionRejectReason(373), Text(58) — spec §9.9 item 2.
func NewReject(refSeqNum int64, refTagID quickfix.Tag, refMsgType string, sessionRejectReason int, text string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgReject))
	m.Body.SetInt(TagRefSeqNum, int(refSeqNum))
	if refTagID > 0 {
		m.Body.SetInt(TagRefTagID, int(refTagID))
	}
	if refMsgType != "" {
		m.Body.SetString(TagRefMsgType, refMsgType)
	}
	m.Body.SetInt(TagSessionRejectReason, sessionRejectReason)
	m.Body.SetString(TagText, text)
	return m
}

// NewBusinessReject builds BusinessMessageReject(35=j): RefSeqNum(45),
// RefMsgType(372), optional BusinessRejectRefID(379),
// BusinessRejectReason(380), Text(58) — spec §9.9 item 2. The emit-path
// variant used by the order flow is businessReject() in report.go;
// this builder adds RefSeqNum(45) so session-layer replies correlate
// to the offending inbound message.
func NewBusinessReject(refSeqNum int64, refMsgType, businessRejectRefID string, reason int, text string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgBusinessReject))
	m.Body.SetInt(TagRefSeqNum, int(refSeqNum))
	if refMsgType != "" {
		m.Body.SetString(TagRefMsgType, refMsgType)
	}
	if businessRejectRefID != "" {
		m.Body.SetString(TagBusinessRejectRefID, businessRejectRefID)
	}
	m.Body.SetInt(TagBusinessRejectReason, reason)
	m.Body.SetString(TagText, text)
	return m
}

// NewNews builds a News(35=B) advisory with Headline(148) — the
// maintenance-drain advisory of §24 #289 / Task 18.3.17 item 3. The
// LinesOfText(33/58) repeating group awaits generated dictionaries;
// Headline alone is dictionary-valid and carries the maintenance /
// replacement-endpoint text.
func NewNews(headline string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetField(TagMsgType, quickfix.FIXString(MsgNews))
	m.Body.SetString(TagHeadline, headline)
	return m
}

// MarkReplay stamps an outbound retransmission: PossDupFlag(43)=Y and
// OrigSendingTime(122) — spec §9.8 item 2 / §9.9 item 1.
func MarkReplay(m *quickfix.Message, origSendingTime time.Time) {
	m.Header.SetBool(TagPossDupFlag, true)
	m.Header.SetField(TagOrigSendingTime,
		&quickfix.FIXUTCTimestamp{Time: origSendingTime.UTC(), Precision: quickfix.Millis})
}

// ---------------------------------------------------------------------------
// Reject-reason registries (spec §9.3/§9.9 mapping tables)
// ---------------------------------------------------------------------------

// SessionRejectReason(373) subset the gateway emits (FIX 4.4 standard
// values — no venue customs here).
const (
	SessionRejectInvalidTag         = 0  // Invalid tag number
	SessionRejectRequiredTagMissing = 1  // Required tag missing
	SessionRejectTagNotDefined      = 2  // Tag not defined for message type
	SessionRejectUndefinedTag       = 3  // Undefined tag
	SessionRejectTagNoValue         = 4  // Tag specified without a value
	SessionRejectValueIncorrect     = 5  // Value is incorrect (out of range)
	SessionRejectDataFormat         = 6  // Incorrect data format for value
	SessionRejectCompIDProblem      = 9  // CompID problem
	SessionRejectSendingTime        = 10 // SendingTime accuracy problem
	SessionRejectInvalidMsgType     = 11 // Invalid MsgType
	SessionRejectTagRepeated        = 13 // Tag appears more than once
	SessionRejectOther              = 99 // Other
)

// BusinessRejectReason(380) values beyond the venue set already in
// tags.go (Other=0, UnknownID=1, NotEntitled=6, Throttled=7) — these
// are the FIX 4.4 standard values the §9.9 mapping below can emit.
const (
	BusinessRejectReasonUnknownSecurity     = 2 // Unknown Security
	BusinessRejectReasonUnsupportedMsgType  = 3 // Unsupported Message Type
	BusinessRejectReasonAppUnavailable      = 4 // Application not available at this time
	BusinessRejectReasonCondReqFieldMissing = 5 // Conditionally required field missing
)

// MapBusinessRejectReason maps registry error codes (spec §23) onto
// BusinessRejectReason(380). The Text(58) field always carries the
// canonical code verbatim — the numeric reason is coarse on purpose.
func MapBusinessRejectReason(errCode string) int {
	switch errCode {
	case "SESSION_NOT_ENTITLED", "UNAUTHORIZED", "FORBIDDEN",
		"ENTITLEMENT_REQUIRED", "FIX_SESSION_NOT_CERTIFIED":
		return BusinessRejectReasonNotEntitled
	case "SESSION_THROTTLED":
		return BusinessRejectReasonThrottled
	case "INSTRUMENT_NOT_FOUND", "UNKNOWN_SYMBOL",
		"INSTRUMENT_NOT_TRADEABLE", "INSTRUMENT_SUSPENDED":
		return BusinessRejectReasonUnknownSecurity
	case "ACCOUNT_NOT_FOUND", "ORDER_NOT_FOUND", "SESSION_NOT_FOUND":
		return BusinessRejectReasonUnknownID
	case "TRADING_HALTED", "MAINTENANCE_MODE", "MARKET_DATA_ONLY",
		"READ_ONLY_MODE", "SESSION_NOT_AVAILABLE":
		return BusinessRejectReasonAppUnavailable
	default:
		return BusinessRejectReasonOther
	}
}

// NewBusinessRejectForCode is the spec §9.9 convenience: one call maps
// the registry code and stamps Text=code.
func NewBusinessRejectForCode(refSeqNum int64, refMsgType, errCode string) *quickfix.Message {
	return NewBusinessReject(refSeqNum, refMsgType, "",
		MapBusinessRejectReason(errCode), errCode)
}

// ---------------------------------------------------------------------------
// Cancel-on-Disconnect (spec §9.9 item 3, §24 #136/#319)
// ---------------------------------------------------------------------------

// SessionCanceller is the seam into the order pipeline's
// session-attributed mass-cancel — satisfied by the sibling OrderFlow
// seam (types.go) via NewSessionCanceller, which in production is
// orders.Service.CancelOnDisconnect → MassCancel{AccountID, SessionID,
// Reason:"cancel_on_disconnect"} over orders.session_id (migration 155,
// idx_orders_session_open).
//
// Per-order `cod_exempt` (spec §5.4/§9.9, remediation #35 — "excluding
// orders explicitly marked COD_EXEMPT") landed with migration 229: the
// column + FIX venue tag 9510 marking + the PgStore.OpenOrders
// exemption scoped to Reason "cancel_on_disconnect" (dead-man, admin
// and close-all sweeps deliberately ignore it).
type SessionCanceller interface {
	// CancelSessionOrders mass-cancels every resting order attributed
	// to sessionID on accountID. Returns the number cancelled.
	CancelSessionOrders(ctx context.Context, accountID int64, sessionID string) (cancelled int, err error)
}

// SessionCancellerFunc adapts a plain function to SessionCanceller.
type SessionCancellerFunc func(ctx context.Context, accountID int64, sessionID string) (int, error)

// CancelSessionOrders implements SessionCanceller.
func (f SessionCancellerFunc) CancelSessionOrders(ctx context.Context, accountID int64, sessionID string) (int, error) {
	return f(ctx, accountID, sessionID)
}

// NewSessionCanceller adapts the OrderFlow seam (orders.Service in
// production) to SessionCanceller.
func NewSessionCanceller(flow OrderFlow) SessionCanceller {
	return SessionCancellerFunc(func(ctx context.Context, accountID int64, sessionID string) (int, error) {
		res, err := flow.CancelOnDisconnect(ctx, accountID, sessionID)
		if err != nil {
			return 0, err
		}
		return res.Cancelled, nil
	})
}

// SessionLifecycleStore is the fix_sessions read/status slice CoD and
// drain need — *PgStore (store.go) satisfies it.
type SessionLifecycleStore interface {
	SessionByID(ctx context.Context, sessionID string) (*Session, error)
	SetStatus(ctx context.Context, sessionID, status string, heartbeatAt *time.Time) error
}

// CoDRateGate bounds CoD purges per account (spec §24 #245: max 1 per
// 5s — the same cap the WS path enforces for flapping clients).
type CoDRateGate interface {
	// Allow returns true when a purge may run now for accountID.
	Allow(ctx context.Context, accountID int64, window time.Duration) (bool, error)
}

// RedisCoDRateGate implements CoDRateGate with a SET NX PX marker on
// the coordination cluster — shared across gateway replicas so a
// flap-across-failover cannot multiply purges.
type RedisCoDRateGate struct {
	rdb *goredis.Client
}

// NewRedisCoDRateGate binds the gate.
func NewRedisCoDRateGate(rdb *goredis.Client) *RedisCoDRateGate {
	return &RedisCoDRateGate{rdb: rdb}
}

// Allow claims cod:rl:{account} NX PX window.
func (g *RedisCoDRateGate) Allow(ctx context.Context, accountID int64, window time.Duration) (bool, error) {
	ok, err := g.rdb.SetArgs(ctx,
		fmt.Sprintf("cod:rl:%d", accountID), "1",
		goredis.SetArgs{Mode: "NX", TTL: window}).Result()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return false, fmt.Errorf("redis cod rate gate %d: %w", accountID, err)
	}
	return ok == "OK", nil
}

// CoDEvent is the audit record for one disconnect decision — emitted
// for every path (purged, disabled, gated, graceful) so the CoD history
// is complete downstream.
type CoDEvent struct {
	SessionID    string
	AccountID    int64
	Cause        string // "socket_drop" | "heartbeat_timeout" | "logon_timeout"
	Graceful     bool   // orderly Logout — orders preserved (AC #30)
	CoDDisabled  bool   // cancel_on_disconnect=false
	DropCopyOnly bool   // account_id NULL → read-only session
	RateLimited  bool   // §24 #245 gate fired
	Cancelled    int
	Elapsed      time.Duration
	WithinBudget bool // Elapsed <= CoDBudget
	At           time.Time
	Err          string
}

// CoDAuditSink receives CoD events; the production binding writes the
// admin/audit trail — nil sink = slog only.
type CoDAuditSink interface {
	RecordCoD(ctx context.Context, ev CoDEvent) error
}

// CoDExecutor runs cancel-on-disconnect for abnormal session loss.
// It complements App.OnLogout's inline CoD (app.go): that path covers
// the single-instance socket drop; this executor adds the measured
// 50ms budget, the §24 #245 rate gate, the audit event stream, and the
// heartbeat-timeout trigger (2× HeartBtInt) that OnLogout alone cannot
// see.
type CoDExecutor struct {
	repo   SessionLifecycleStore
	cancel SessionCanceller
	gate   CoDRateGate  // nil = no per-account cap
	audit  CoDAuditSink // nil = log only
	log    *slog.Logger
	now    func() time.Time
	budget time.Duration // CoDBudget when 0
}

// NewCoDExecutor binds the executor. repo/cancel are mandatory — a nil
// seam fails closed at OnDisconnect rather than pretending orders were
// purged.
func NewCoDExecutor(repo SessionLifecycleStore, cancel SessionCanceller,
	gate CoDRateGate, audit CoDAuditSink, log *slog.Logger,
	now func() time.Time) *CoDExecutor {
	if now == nil {
		now = time.Now
	}
	return &CoDExecutor{repo: repo, cancel: cancel, gate: gate, audit: audit,
		log: log, now: now, budget: CoDBudget}
}

// OnDisconnect handles session loss. graceful=true (peer Logout /
// protocol-clean close) preserves resting orders per AC #30 and only
// marks the session LOGGED_OUT; abnormal drops (socket_drop,
// heartbeat_timeout, logon_timeout) purge via the SessionCanceller seam
// and are measured against the 50ms budget.
func (e *CoDExecutor) OnDisconnect(ctx context.Context, sessionID, cause string, graceful bool) (*CoDEvent, error) {
	ev := &CoDEvent{SessionID: sessionID, Cause: cause, Graceful: graceful, At: e.now().UTC()}
	if e.repo == nil {
		return ev, errors.New("fix: session store not wired")
	}
	rec, err := e.repo.SessionByID(ctx, sessionID)
	if err != nil {
		return ev, fmt.Errorf("cod load session %s: %w", sessionID, err)
	}
	if rec == nil {
		return ev, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	if rec.AccountID != nil {
		ev.AccountID = *rec.AccountID
	}
	if graceful {
		ev.WithinBudget = true
		if merr := e.repo.SetStatus(ctx, sessionID, "LOGGED_OUT", nil); merr != nil {
			return ev, merr
		}
		e.record(ctx, *ev)
		return ev, nil
	}
	if err := e.repo.SetStatus(ctx, sessionID, "DISCONNECTED", &ev.At); err != nil {
		return ev, fmt.Errorf("cod mark disconnected %s: %w", sessionID, err)
	}
	switch {
	case !rec.CancelOnDisconnect:
		ev.CoDDisabled = true
		e.record(ctx, *ev)
		return ev, nil
	case rec.AccountID == nil:
		// Drop-copy session (account_id NULL, §9.3): read-only, owns no
		// orders — nothing to purge.
		ev.DropCopyOnly = true
		e.record(ctx, *ev)
		return ev, nil
	}
	if e.gate != nil {
		allowed, gerr := e.gate.Allow(ctx, *rec.AccountID, CoDRateWindow)
		if gerr != nil {
			// Fail-closed pessimism: a broken gate must not block the
			// purge (orders on a dead session are the bigger hazard) —
			// proceed and flag.
			e.warnf("cod rate gate error; proceeding with purge", rec, gerr)
		} else if !allowed {
			ev.RateLimited = true
			ev.WithinBudget = true
			e.record(ctx, *ev)
			return ev, nil
		}
	}
	if e.cancel == nil {
		return ev, errors.New("fix: session canceller not wired")
	}
	start := e.now()
	cancelled, cerr := e.cancel.CancelSessionOrders(ctx, *rec.AccountID, sessionID)
	ev.Elapsed = e.now().Sub(start)
	ev.WithinBudget = ev.Elapsed <= e.budget
	ev.Cancelled = cancelled
	if cerr != nil {
		ev.Err = cerr.Error()
	}
	e.record(ctx, *ev)
	if !ev.WithinBudget && e.log != nil {
		e.log.Warn("cod budget breached", "session", sessionID,
			"account", *rec.AccountID, "cancelled", cancelled,
			"elapsed", ev.Elapsed, "budget", e.budget)
	}
	if cerr != nil {
		return ev, fmt.Errorf("cod mass cancel %s: %w", sessionID, cerr)
	}
	return ev, nil
}

func (e *CoDExecutor) record(ctx context.Context, ev CoDEvent) {
	if e.audit != nil {
		if err := e.audit.RecordCoD(ctx, ev); err != nil && e.log != nil {
			e.log.Error("cod audit write failed", "session", ev.SessionID, "err", err)
		}
	}
	if e.log != nil {
		e.log.Info("cod decision", "session", ev.SessionID, "account", ev.AccountID,
			"cause", ev.Cause, "graceful", ev.Graceful, "disabled", ev.CoDDisabled,
			"rate_limited", ev.RateLimited, "cancelled", ev.Cancelled,
			"elapsed", ev.Elapsed, "err", ev.Err)
	}
}

func (e *CoDExecutor) warnf(msg string, rec *Session, err error) {
	if e.log != nil {
		e.log.Warn(msg, "session", rec.SessionID, "account", evAccountID(rec), "err", err)
	}
}

// evAccountID dereferences the optional bound account for logging.
func evAccountID(s *Session) int64 {
	if s != nil && s.AccountID != nil {
		return *s.AccountID
	}
	return 0
}
