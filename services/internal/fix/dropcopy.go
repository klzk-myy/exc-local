// Task 18.3.4 — Drop Copy: read-only FIX sessions that receive a real-time
// copy of every ExecutionReport (35=8) emitted for their bound accounts,
// but may never submit order flow.
//
//   - DropCopySessionKind classifies a *Session row (spec §5.20:
//     account_id NULL marks a drop-copy session).
//   - DropCopyRouter fans one report to every bound target — it attaches
//     to the session-core emitter via App.Report().WithTap(router.Tap())
//     (ReportBus is owned by the sibling session-core; this file is the
//     tap, not the emitter).
//   - OrderEntryRejector produces the 35=j BusinessReject emitted when a
//     drop-copy session attempts order entry — Text(58) carries
//     "drop copy is read-only".
//   - SetParties / SetFXParties stamp the spec §9.2 Parties block and the
//     FX-specific tag set (spec §9.7 party/settlement fields) on cloned
//     reports.
package fix

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"
)

// Spec §9.2 Parties component tags and Task 18.3.13 allocation tags —
// additive to tags.go (owned by the session-core sibling).
const (
	TagPartyID           quickfix.Tag = 448
	TagPartyIDSource     quickfix.Tag = 447
	TagPartyRole         quickfix.Tag = 452
	TagQuantity          quickfix.Tag = 53
	TagAllocID           quickfix.Tag = 70
	TagAllocTransType    quickfix.Tag = 71
	TagRefAllocID        quickfix.Tag = 72
	TagNoOrders          quickfix.Tag = 73
	TagNoAllocs          quickfix.Tag = 78
	TagAllocAccount      quickfix.Tag = 79
	TagAllocQty          quickfix.Tag = 80
	TagAllocStatus       quickfix.Tag = 87
	TagAllocRejCode      quickfix.Tag = 88
	TagNoExecs           quickfix.Tag = 124
	TagAllocPrice        quickfix.Tag = 366
	TagIndividualAllocID quickfix.Tag = 467
	TagAllocType         quickfix.Tag = 626
	TagAllocReportID     quickfix.Tag = 755
	TagFXNoPartyIDs      quickfix.Tag = 9018
	TagFXPartyIDSource   quickfix.Tag = 9019
	TagFXPartyID         quickfix.Tag = 9020
	TagFXSettlementType  quickfix.Tag = 9501
	TagFXSettlementDate  quickfix.Tag = 9502
	// TagCODExempt marks a 35=D order exempt from the session-scope
	// cancel-on-disconnect sweep (spec §9.9, orders.cod_exempt,
	// migration 229; Task 18.3.18). Truthy values: Y/1/TRUE.
	TagCODExempt quickfix.Tag = 9510
)

// MsgType values added by Tasks 18.3.4/18.3.6/18.3.13.
const (
	MsgAllocationInstruction    = "J"
	MsgAllocationInstructionAck = "P"
	MsgAllocationReport         = "AK"
)

// PartyRole(452) values we emit (FIX 4.4).
const (
	PartyRoleExecutingFirm = "1"
	PartyRoleClientID      = "3"
	PartyRolePrimeBroker   = "36"
	PartyRoleClearingFirm  = "4"
)

// PartyIDSource(447) values.
const (
	PartyIDSourceProprietary = "D"
	PartyIDSourceBIC         = "B"
)

// SessionKind discriminates trading sessions from drop-copy variants.
// Spec §5.20 encodes the role on fix_sessions: account_id NULL means
// drop copy (Session.Entitled already denies order entry for it). A
// prime-broker drop copy additionally carries a PB marker in
// allowed_instruments' "PB:<compID>" sentinel so the PB router can find
// it without a schema change (Task 18.3.6 — pb FIX sessions are
// identified by their TargetCompID against prime_brokers.fix_comp_id).
type SessionKind int

const (
	KindTrading SessionKind = iota
	KindDropCopy
)

// DropCopySessionKind classifies one fix_sessions row.
func DropCopySessionKind(s *Session) SessionKind {
	if s != nil && s.AccountID == nil {
		return KindDropCopy
	}
	return KindTrading
}

// ---------------------------------------------------------------------------
// Drop-copy fan-out
// ---------------------------------------------------------------------------

// ErrDropCopyNotBound is returned when a drop-copy target is registered
// with no bound accounts — an unbound tap would leak the whole tape.
var ErrDropCopyNotBound = errors.New("fix: drop copy target has no bound accounts")

// DropCopyTarget is one bound drop-copy destination: a live acceptor-side
// session (quickfix.SessionID — same transport binding the sibling's
// QuickFIXSender/MessageSender uses) plus the account set it may copy.
type DropCopyTarget struct {
	ID        string             // operator label (e.g. session key)
	SessionID quickfix.SessionID // wire destination for MessageSender.SendTo
	Sender    MessageSender      // nil → QuickFIXSender() (sibling binding)
	Accounts  map[int64]bool     // bound account set — nil/empty rejected
	// FXParties requests the FX-specific tag block (9018–9020, 9501/9502,
	// spec §9.7) in addition to the standard Parties group.
	FXParties bool
	// Parties is the static Parties block stamped on every copied report
	// (e.g. the copying firm + executing broker roles).
	Parties []Party
}

// inScope is the binding check — a report may only be copied when the
// account that owns it is in the bound set (spec: "all ExecutionReports
// for bound accounts").
func (t *DropCopyTarget) inScope(accountID int64) bool {
	return t.Accounts[accountID]
}

func (t *DropCopyTarget) sender() MessageSender {
	if t.Sender != nil {
		return t.Sender
	}
	return QuickFIXSender()
}

// Party is one §9.2 Parties group entry.
type Party struct {
	ID     string // 448
	Source string // 447
	Role   string // 452
}

// DropCopyRouter fans emitted ExecutionReports out to every bound
// drop-copy target. It is attached to the session-core emitter as a
// ReportBus tap:
//
//	app.Report().WithTap(router.Tap())
//
// ReportBus.Emit sends to the owning session first, then calls taps —
// panics inside a tap are already isolated by the bus.
type DropCopyRouter struct {
	mu      sync.RWMutex
	targets []DropCopyTarget
	// OrderAccount resolves the owning account of a report's order
	// (ReportEvent carries OrderID, not AccountID). Production binding
	// wraps OrderRead.GetOrder. When nil the router falls back to the
	// report's Tag 1 (Account) — if neither yields an account the report
	// is NOT copied (fail closed: never leak the tape).
	OrderAccount func(ctx context.Context, orderID int64) (int64, error)
	// OnError receives per-target send failures — PB/drop-copy fan-out
	// must never break the trading session that triggered it.
	OnError func(err error)
}

// NewDropCopyRouter builds an empty router.
func NewDropCopyRouter() *DropCopyRouter { return &DropCopyRouter{} }

// Bind registers one drop-copy target for its bound account set.
func (r *DropCopyRouter) Bind(t DropCopyTarget) error {
	if len(t.Accounts) == 0 {
		return ErrDropCopyNotBound
	}
	r.mu.Lock()
	r.targets = append(r.targets, t)
	r.mu.Unlock()
	return nil
}

// Tap adapts the router to the session-core ReportBus tap signature
// (ReportTap func(ReportEvent)). Send errors are routed to OnError —
// the bus swallows panics, and a tap must not return errors.
func (r *DropCopyRouter) Tap() ReportTap {
	return func(ev ReportEvent) {
		ctx, cancel := context.WithTimeout(context.Background(), ReportSendTimeout)
		defer cancel()
		if err := r.OnReport(ctx, ev); err != nil && r.OnError != nil {
			r.OnError(err)
		}
	}
}

// ReportSendTimeout bounds one drop-copy fan-out — a wedged sender must
// not stall the trading session's report path.
const ReportSendTimeout = 5 * time.Second

// OnReport copies one emitted ExecutionReport to every in-scope target.
// The owning session receives its copy from ReportBus.Emit — this method
// only handles the tap fan-out.
func (r *DropCopyRouter) OnReport(ctx context.Context, ev ReportEvent) error {
	if ev.Msg == nil {
		return nil
	}
	accountID, err := r.reportAccount(ctx, ev)
	if err != nil {
		return err // fail closed — do not copy an unscoped report
	}
	r.mu.RLock()
	targets := make([]DropCopyTarget, 0, len(r.targets))
	for _, t := range r.targets {
		if t.inScope(accountID) {
			targets = append(targets, t)
		}
	}
	r.mu.RUnlock()
	var errs []error
	for _, t := range targets {
		select {
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		default:
		}
		cp := CloneMessage(ev.Msg)
		if len(t.Parties) > 0 {
			SetParties(cp, t.Parties)
		}
		if t.FXParties {
			SetFXParties(cp, t.Parties)
		}
		if err := t.sender().SendTo(cp, t.SessionID); err != nil {
			errs = append(errs, fmt.Errorf("fix: drop copy %s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

// reportAccount determines which account a report belongs to. OrderID is
// authoritative (via OrderAccount); Tag 1 (Account) is the fallback for
// reports whose order is no longer resolvable.
func (r *DropCopyRouter) reportAccount(ctx context.Context, ev ReportEvent) (int64, error) {
	if r.OrderAccount != nil && ev.OrderID != 0 {
		if id, err := r.OrderAccount(ctx, ev.OrderID); err == nil && id > 0 {
			return id, nil
		}
	}
	if s, err := ev.Msg.Body.GetString(TagAccount); err == nil && s != "" {
		if id, perr := strconv.ParseInt(s, 10, 64); perr == nil && id > 0 {
			return id, nil
		}
	}
	return 0, fmt.Errorf("fix: drop copy: account for order %d unresolvable", ev.OrderID)
}

// OnAllocationChange implements AllocationChangeSink — drop copy taps
// receive every committed allocation report (35=AK), including
// REPLACE/CANCEL corrections, scoped by the master account
// (Task 18.3.13 DoD row 6).
func (r *DropCopyRouter) OnAllocationChange(ctx context.Context, ev AllocationChange) error {
	if ev.Report == nil {
		return nil
	}
	r.mu.RLock()
	targets := make([]DropCopyTarget, 0, len(r.targets))
	for _, t := range r.targets {
		if t.inScope(ev.Allocation.MasterAccountID) {
			targets = append(targets, t)
		}
	}
	r.mu.RUnlock()
	var errs []error
	for _, t := range targets {
		select {
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		default:
		}
		if err := t.sender().SendTo(CloneMessage(ev.Report), t.SessionID); err != nil {
			errs = append(errs, fmt.Errorf("fix: drop copy %s alloc report: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Read-only enforcement
// ---------------------------------------------------------------------------

// ErrDropCopyReadOnly is the rejection reason for order-entry attempts on
// a drop-copy session. The literal phrase is part of the Task 18.3.4
// contract and lands in Reject/BusinessReject Text(58).
var ErrDropCopyReadOnly = errors.New("drop copy is read-only")

// OrderEntryRejector is the session-core seam for Task 18.3.4's
// "submission attempts rejected" requirement. The sibling's FromApp
// already denies order flow for sessions with account_id NULL via
// sessionAccount()/Entitled() — this type exists for the admin/test
// surface and for any sibling dispatch path that wants the canonical
// "drop copy is read-only" text.
type OrderEntryRejector struct {
	Kind func(s *Session) SessionKind // nil → DropCopySessionKind
}

// NewOrderEntryRejector returns the default guard.
func NewOrderEntryRejector() *OrderEntryRejector {
	return &OrderEntryRejector{Kind: DropCopySessionKind}
}

// Reject returns a 35=j BusinessReject when the session must not submit
// order flow, else nil. The caller (session-core dispatch) emits the
// message through its normal send path.
func (e *OrderEntryRejector) Reject(s *Session, msgType, refID string) *quickfix.Message {
	kind := DropCopySessionKind(s)
	if e != nil && e.Kind != nil {
		kind = e.Kind(s)
	}
	if kind != KindDropCopy {
		return nil
	}
	return businessReject(msgType, refID, ErrDropCopyReadOnly.Error(),
		BusinessRejectReasonOther)
}

// IsOrderEntry reports whether a MsgType is order-entry traffic a
// drop-copy session must never send.
func IsOrderEntry(msgType string) bool {
	switch msgType {
	case MsgNewOrderSingle, MsgOrderCancelRequest, MsgOrderCancelReplace,
		MsgAllocationInstruction:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Party / FX field stamping (spec §9.2, §9.7)
// ---------------------------------------------------------------------------

// CloneMessage deep-copies an outgoing message so each drop-copy target
// gets its own instance to stamp.
func CloneMessage(src *quickfix.Message) *quickfix.Message {
	if src == nil {
		return nil
	}
	dst := quickfix.NewMessage()
	src.Header.CopyInto(&dst.Header.FieldMap)
	src.Body.CopyInto(&dst.Body.FieldMap)
	src.Trailer.CopyInto(&dst.Trailer.FieldMap)
	return dst
}

// SetParties stamps the standard FIX 4.4 Parties repeating group
// (NoPartyIDs 453 → 448/447/452) per spec §9.2.
func SetParties(msg *quickfix.Message, parties []Party) {
	if len(parties) == 0 {
		return
	}
	grp := quickfix.NewRepeatingGroup(TagNoPartyIDs, quickfix.GroupTemplate{
		quickfix.GroupElement(TagPartyID),
		quickfix.GroupElement(TagPartyIDSource),
		quickfix.GroupElement(TagPartyRole),
	})
	for _, p := range parties {
		g := grp.Add()
		g.SetString(TagPartyID, p.ID)
		g.SetString(TagPartyIDSource, p.Source)
		g.SetString(TagPartyRole, p.Role)
	}
	msg.Body.SetGroup(grp)
}

// SetFXParties stamps the venue's FX party tags (9018 NoPartyIDs →
// 9020 PartyID, 9019 PartyIDSource) plus settlement fields per the
// spec §9.7 FX-specific tag table.
func SetFXParties(msg *quickfix.Message, parties []Party) {
	if len(parties) == 0 {
		return
	}
	grp := quickfix.NewRepeatingGroup(TagFXNoPartyIDs, quickfix.GroupTemplate{
		quickfix.GroupElement(TagFXPartyID),
		quickfix.GroupElement(TagFXPartyIDSource),
	})
	for _, p := range parties {
		g := grp.Add()
		g.SetString(TagFXPartyID, p.ID)
		g.SetString(TagFXPartyIDSource, p.Source)
	}
	msg.Body.SetGroup(grp)
}

// SetFXSettlement stamps SettlementType(9501)/SettlementDate(9502).
func SetFXSettlement(msg *quickfix.Message, settlType, settlDate string) {
	if settlType != "" {
		msg.Body.SetString(TagFXSettlementType, settlType)
	}
	if settlDate != "" {
		msg.Body.SetString(TagFXSettlementDate, settlDate)
	}
}

// ---------------------------------------------------------------------------
// Persistence — fix_session_bindings records which accounts a drop-copy
// session may copy. Kept on migration 030's fix_sessions neighborhood;
// the binding table itself ships with the migration set below.
// ---------------------------------------------------------------------------

// DropCopyBinding is one (session → account) entitlement row for a
// drop-copy session.
type DropCopyBinding struct {
	SessionID string
	AccountID int64
}

// DropCopyBindingStore persists bindings. A nil store is legal — the
// router simply has no persisted bindings and relies on Bind calls made
// at wiring time (e.g. tests, or the admin surface that will land the
// provisioning path in Task 18.3.9).
type DropCopyBindingStore interface {
	// Bindings returns every bound account for a drop-copy session.
	Bindings(ctx context.Context, sessionID string) ([]int64, error)
	// Save upserts the binding set for a session.
	Save(ctx context.Context, sessionID string, accounts []int64) error
}

// DiscardSender drops outbound messages — used in tests to exercise
// fan-out counting without a transport.
type DiscardSender struct{ N int }

// SendTo implements MessageSender, counting and discarding.
func (d *DiscardSender) SendTo(_ *quickfix.Message, _ quickfix.SessionID) error {
	d.N++
	return nil
}
