// Task 18.3.13 — FIX AllocationInstruction (35=J) / AllocationReport (35=AK)
// post-trade block allocation (spec §9.2, §17.8, §24 #195/#200/#237).
//
// Inbound 35=J is parsed into an AllocationInstruction, validated (sum of
// NoAllocs(78) AllocQty(80) legs MUST equal the referenced executed quantity
// exactly — over/under-allocation is rejected with 35=P
// AllocRejCode(88)=4 / Text=ALLOCATION_SUM_MISMATCH per spec §27.1 matrix),
// persisted under migration 226 (fix_allocations / fix_allocation_legs /
// allocation_events), and confirmed with 35=AK AllocationReport carrying
// per-leg booking records.
//
// Settlement-side records (average_price_groups / trade_allocations,
// migration 055) and per-leg settlement obligations are Phase-24 Tasks
// 24.3.10/24.3.15 — this package hands committed changes to the
// AllocationChangeSink seam (settlement propagation + PB drop copy).
package fix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quickfixgo/quickfix"

	"exchange/internal/pamm"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Error codes (spec §23 rows — ALLOCATION_SUM_MISMATCH, ALLOCATION_INVALID).
// ---------------------------------------------------------------------------

const (
	CodeAllocationSumMismatch = "ALLOCATION_SUM_MISMATCH" // 400
	CodeAllocationInvalid     = "ALLOCATION_INVALID"      // 400
)

func allocErr(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------------------
// Domain types (migration 226).
// ---------------------------------------------------------------------------

// AllocMethod is the venue allocation method (Task 18.3.13 step 2). It is
// carried in AllocType(626) as a name string; absent 626 defaults to MANUAL.
type AllocMethod string

const (
	AllocMethodProRata AllocMethod = "PRO_RATA" // legs carry weights; engine splits exec qty
	AllocMethodManual  AllocMethod = "MANUAL"   // legs carry explicit quantities
	AllocMethodStepOut AllocMethod = "STEP_OUT" // legs are external executing brokers
)

// AllocTransType mirrors Tag 71.
type AllocTransType int

const (
	AllocTransNew     AllocTransType = 0
	AllocTransReplace AllocTransType = 1
	AllocTransCancel  AllocTransType = 2
)

// AllocStatus mirrors fix_alloc_status_enum.
type AllocStatus string

const (
	AllocStatusReceived  AllocStatus = "RECEIVED"
	AllocStatusAccepted  AllocStatus = "ACCEPTED"
	AllocStatusRejected  AllocStatus = "REJECTED"
	AllocStatusReplaced  AllocStatus = "REPLACED"
	AllocStatusCancelled AllocStatus = "CANCELLED"
)

// AllocLegStatus mirrors alloc_leg_status_enum.
type AllocLegStatus string

const (
	LegBooked         AllocLegStatus = "BOOKED"
	LegRejected       AllocLegStatus = "REJECTED"
	LegCancelled      AllocLegStatus = "CANCELLED"
	LegExternalBooked AllocLegStatus = "EXTERNAL_BOOKED" // STEP_OUT leg
)

// FIX AllocStatus(87) values emitted on 35=P / 35=AK.
const (
	AllocStatusAcceptedFIX        = 0
	AllocStatusBlockLevelReject   = 1
	AllocStatusAccountLevelReject = 2
	AllocStatusReceivedFIX        = 3
)

// AllocRejCode(88) values emitted on rejection (spec §27.1 pins 4 for the
// sum-mismatch surface).
const (
	AllocRejCodeUnknownAccount = 0
	AllocRejCodeIncorrectQty   = 1
	AllocRejCodeSumMismatch    = 4 // spec §27.1 Allocation row
	AllocRejCodeUnknownExecID  = 10
	AllocRejCodeMismatchedData = 11
	AllocRejCodeOther          = 7
)

// AllocationLeg is one NoAllocs(78) entry after validation.
type AllocationLeg struct {
	LegNo         int
	AllocAccount  string          // Tag 79 verbatim
	AccountID     int64           // resolved accounts.id (0 = STEP_OUT external)
	AllocQty      decimal.Decimal // resolved quantity (post PRO_RATA split)
	AllocPrice    decimal.Decimal
	StepOutBroker string // STEP_OUT external broker mnemonic
	Status        AllocLegStatus
	ParentExecID  string // source fill linkage (first ExecID ref)
	ChildExecID   string // generated booking ref "ALLOC-{id}-{leg}"
}

// AllocationInstruction is a parsed 35=J.
type AllocationInstruction struct {
	AllocID     string
	RefAllocID  string
	TransType   AllocTransType
	Method      AllocMethod
	Symbol      string
	Side        byte
	DeclaredQty decimal.Decimal // Tag 53 when present — must equal exec qty
	ExecRefs    []string        // NoExecs(124) → ExecID(17)
	OrderRefs   []string        // NoOrders(73) → OrderID(37)
	Legs        []AllocationLeg
}

// ExecRef is the resolved fill/order-side context an allocation references.
type ExecRef struct {
	Qty       decimal.Decimal
	AvgPx     decimal.Decimal
	Symbol    string
	Side      byte
	AccountID int64
}

// Allocation is the persisted instruction row plus its legs.
type Allocation struct {
	ID               int64
	AllocID          string
	RefAllocID       string
	TransType        AllocTransType
	Method           AllocMethod
	Status           AllocStatus
	SessionID        string
	MasterAccountID  int64
	Symbol           string
	Side             byte
	ExecQty          decimal.Decimal
	ExecRefs         []string
	SettlementLocked bool
	RejectReason     string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Legs             []AllocationLeg
}

// AllocationEventType values for allocation_events.
const (
	EvReceived          = "RECEIVED"
	EvAccepted          = "ACCEPTED"
	EvRejected          = "REJECTED"
	EvSuperseded        = "SUPERSEDED" // prior instruction replaced
	EvCancelled         = "CANCELLED"
	EvPropagationFailed = "PROPAGATION_FAILED"
)

// ---------------------------------------------------------------------------
// Parser — AllocationInstruction (35=J).
// ---------------------------------------------------------------------------

// ParseAllocationInstruction decodes a raw 35=J message. Malformed frames
// (missing AllocID/AllocTransType/NoAllocs) return error — the session-core
// sibling surfaces those as session-level Reject(35=3) per spec §9.9.
func ParseAllocationInstruction(msg *quickfix.Message) (*AllocationInstruction, error) {
	b := &msg.Body
	req := func(tag quickfix.Tag) (string, error) {
		s, rerr := b.GetString(tag)
		if rerr != nil {
			return "", fmt.Errorf("fix: 35=J missing tag %d", int(tag))
		}
		return s, nil
	}
	allocID, err := req(TagAllocID)
	if err != nil {
		return nil, err
	}
	ttRaw, rerr := b.GetInt(TagAllocTransType)
	if rerr != nil {
		return nil, fmt.Errorf("fix: 35=J missing tag 71 (AllocTransType)")
	}
	tt := AllocTransType(ttRaw)
	if tt != AllocTransNew && tt != AllocTransReplace && tt != AllocTransCancel {
		return nil, allocErr(CodeAllocationInvalid, "unknown AllocTransType %d", ttRaw)
	}
	out := &AllocationInstruction{AllocID: allocID, TransType: tt, Method: AllocMethodManual}
	out.RefAllocID, _ = b.GetString(TagRefAllocID)
	if m, _ := b.GetString(TagAllocType); m != "" {
		method, ok := parseAllocMethod(m)
		if !ok {
			return nil, allocErr(CodeAllocationInvalid, "unknown allocation method %q", m)
		}
		out.Method = method
	}
	out.Symbol, _ = b.GetString(TagSymbol)
	if s, _ := b.GetString(TagSide); s != "" {
		out.Side = s[0]
	}
	if q, _ := b.GetString(TagQuantity); q != "" {
		d, derr := decimal.NewFromString(q)
		if derr != nil {
			return nil, allocErr(CodeAllocationInvalid, "Quantity(53) %q unparseable", q)
		}
		out.DeclaredQty = d
	}
	// NoExecs(124) → ExecID(17)
	execs := quickfix.NewRepeatingGroup(TagNoExecs, quickfix.GroupTemplate{quickfix.GroupElement(TagExecID)})
	if b.GetGroup(execs) == nil {
		for i := 0; i < execs.Len(); i++ {
			if id, _ := execs.Get(i).GetString(TagExecID); id != "" {
				out.ExecRefs = append(out.ExecRefs, id)
			}
		}
	}
	// NoOrders(73) → OrderID(37)
	orders := quickfix.NewRepeatingGroup(TagNoOrders, quickfix.GroupTemplate{quickfix.GroupElement(TagOrderID)})
	if b.GetGroup(orders) == nil {
		for i := 0; i < orders.Len(); i++ {
			if id, _ := orders.Get(i).GetString(TagOrderID); id != "" {
				out.OrderRefs = append(out.OrderRefs, id)
			}
		}
	}
	// NoAllocs(78) → AllocAccount(79)/AllocQty(80)/AllocPrice(366)
	allocs := quickfix.NewRepeatingGroup(TagNoAllocs, quickfix.GroupTemplate{
		quickfix.GroupElement(TagAllocAccount),
		quickfix.GroupElement(TagAllocQty),
		quickfix.GroupElement(TagAllocPrice),
	})
	// NEW/REPLACE carry legs; CANCEL is leg-free (it names the target via
	// AllocID/RefAllocID only) per FIX 4.4.
	if tt != AllocTransCancel && (b.GetGroup(allocs) != nil || allocs.Len() == 0) {
		return nil, allocErr(CodeAllocationInvalid, "35=J carries no NoAllocs(78) legs")
	}
	if tt == AllocTransCancel && b.GetGroup(allocs) != nil {
		allocs = quickfix.NewRepeatingGroup(TagNoAllocs, quickfix.GroupTemplate{quickfix.GroupElement(TagAllocAccount)})
	}
	for i := 0; i < allocs.Len(); i++ {
		g := allocs.Get(i)
		acct, _ := g.GetString(TagAllocAccount)
		if acct == "" {
			return nil, allocErr(CodeAllocationInvalid, "leg %d: AllocAccount(79) empty", i+1)
		}
		leg := AllocationLeg{LegNo: i + 1, AllocAccount: acct, Status: LegBooked}
		if q, _ := g.GetString(TagAllocQty); q != "" {
			d, derr := decimal.NewFromString(q)
			if derr != nil {
				return nil, allocErr(CodeAllocationInvalid, "leg %d: AllocQty(80) %q unparseable", i+1, q)
			}
			leg.AllocQty = d
		}
		if p, _ := g.GetString(TagAllocPrice); p != "" {
			d, derr := decimal.NewFromString(p)
			if derr != nil {
				return nil, allocErr(CodeAllocationInvalid, "leg %d: AllocPrice(366) %q unparseable", i+1, p)
			}
			leg.AllocPrice = d
		}
		out.Legs = append(out.Legs, leg)
	}
	return out, nil
}

func parseAllocMethod(s string) (AllocMethod, bool) {
	switch AllocMethod(strings.ToUpper(strings.TrimSpace(s))) {
	case AllocMethodProRata:
		return AllocMethodProRata, true
	case AllocMethodManual:
		return AllocMethodManual, true
	case AllocMethodStepOut:
		return AllocMethodStepOut, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Resolution seams (implemented by the store / order-side sibling).
// ---------------------------------------------------------------------------

// ExecQuantityResolver resolves referenced fills/orders to cumulative
// quantities. Implemented against orders/trades tables by the session-side
// wiring (Task 18.3.2 owns the order store; this interface is the contract).
type ExecQuantityResolver interface {
	Exec(ctx context.Context, execID string) (ExecRef, error)
	OrderCum(ctx context.Context, orderID string) (ExecRef, error)
}

// AccountResolver validates that an AllocAccount(79) value resolves to an
// account linked to the same master institutional account (direct child or
// the master itself).
type AccountResolver interface {
	ResolveAllocAccount(ctx context.Context, masterAccountID int64, allocAccount string) (int64, error)
}

// ErrResolverUnconfigured fails closed when a required resolver is absent —
// never guess a quantity or skip the linkage check (spec §2.7).
var ErrResolverUnconfigured = errors.New("fix: allocation resolver not configured")

// ---------------------------------------------------------------------------
// Validation.
// ---------------------------------------------------------------------------

// validateLegs resolves quantities and accounts. On success every leg carries
// a resolved AllocQty and either an internal AccountID or a StepOutBroker.
// Errors are coded: ALLOCATION_SUM_MISMATCH for conservation failures,
// ALLOCATION_INVALID for bad accounts/methods.
func (s *AllocationService) validate(ctx context.Context, instr *AllocationInstruction, masterAccountID int64) (decimal.Decimal, decimal.Decimal, error) {
	var zero decimal.Decimal
	if len(instr.ExecRefs) == 0 && len(instr.OrderRefs) == 0 {
		return zero, zero, allocErr(CodeAllocationInvalid, "35=J carries no execution references (NoExecs/NoOrders)")
	}
	if s.execResolver == nil {
		return zero, zero, allocErr(CodeAllocationInvalid, "%v", ErrResolverUnconfigured)
	}
	// Resolve the referenced executed quantity; every ref must belong to the
	// master account's own fill set and agree on symbol/side.
	var execQty, avgPxSum decimal.Decimal
	var refs []ExecRef
	for _, id := range instr.ExecRefs {
		r, err := s.execResolver.Exec(ctx, id)
		if err != nil {
			return zero, zero, allocErr(CodeAllocationInvalid, "ExecID %q unresolvable", id)
		}
		refs = append(refs, r)
	}
	for _, id := range instr.OrderRefs {
		r, err := s.execResolver.OrderCum(ctx, id)
		if err != nil {
			return zero, zero, allocErr(CodeAllocationInvalid, "OrderID %q unresolvable", id)
		}
		refs = append(refs, r)
	}
	for i, r := range refs {
		if r.AccountID != masterAccountID {
			return zero, zero, allocErr(CodeAllocationInvalid,
				"execution ref %d belongs to account %d, not master %d", i, r.AccountID, masterAccountID)
		}
		if instr.Symbol != "" && r.Symbol != "" && r.Symbol != instr.Symbol {
			return zero, zero, allocErr(CodeAllocationInvalid,
				"execution ref %d symbol %s != instruction symbol %s", i, r.Symbol, instr.Symbol)
		}
		execQty = execQty.Add(r.Qty)
		avgPxSum = avgPxSum.Add(r.AvgPx.Mul(r.Qty))
	}
	var avgPx decimal.Decimal
	if execQty.IsPositive() {
		avgPx = avgPxSum.Div(execQty)
	}
	if instr.DeclaredQty.IsPositive() && !instr.DeclaredQty.Equal(execQty) {
		return zero, zero, allocErr(CodeAllocationSumMismatch,
			"declared Quantity(53) %s != referenced executed qty %s", instr.DeclaredQty, execQty)
	}

	switch instr.Method {
	case AllocMethodProRata:
		shares := make([]pamm.Share, len(instr.Legs))
		for i, l := range instr.Legs {
			if !l.AllocQty.IsPositive() {
				return zero, zero, allocErr(CodeAllocationInvalid, "leg %d: PRO_RATA weight must be positive", l.LegNo)
			}
			shares[i] = pamm.Share{ID: int64(l.LegNo), Weight: l.AllocQty}
		}
		out, err := pamm.AllocateProRata(execQty, shares)
		if err != nil {
			return zero, zero, allocErr(CodeAllocationSumMismatch, "pro-rata split: %v", err)
		}
		for i := range instr.Legs {
			instr.Legs[i].AllocQty = out[i].Quantity
		}
	default:
		var sum decimal.Decimal
		for _, l := range instr.Legs {
			if !l.AllocQty.IsPositive() {
				return zero, zero, allocErr(CodeAllocationInvalid, "leg %d: AllocQty must be positive", l.LegNo)
			}
			sum = sum.Add(l.AllocQty)
		}
		if !sum.Equal(execQty) {
			return zero, zero, allocErr(CodeAllocationSumMismatch,
				"allocated %s != executed %s (over/under-allocation)", sum, execQty)
		}
	}

	// Account linkage: internal legs must resolve inside the master account
	// hierarchy; STEP_OUT legs name an external executing broker instead.
	if s.accounts == nil && instr.Method != AllocMethodStepOut {
		return zero, zero, allocErr(CodeAllocationInvalid, "%v", ErrResolverUnconfigured)
	}
	for i := range instr.Legs {
		l := &instr.Legs[i]
		if instr.Method == AllocMethodStepOut {
			l.StepOutBroker = l.AllocAccount
			l.Status = LegExternalBooked
			continue
		}
		id, err := s.accounts.ResolveAllocAccount(ctx, masterAccountID, l.AllocAccount)
		if err != nil {
			return zero, zero, allocErr(CodeAllocationInvalid,
				"leg %d: AllocAccount %q not linked to master %d", l.LegNo, l.AllocAccount, masterAccountID)
		}
		l.AccountID = id
		if l.AllocPrice.IsZero() {
			l.AllocPrice = avgPx
		}
	}
	return execQty, avgPx, nil
}

// ---------------------------------------------------------------------------
// AllocationService — orchestration.
// ---------------------------------------------------------------------------

// AllocationChange is the propagation unit handed to sinks after a committed
// instruction transition (REPLACE/CANCEL corrections reach settlement and PB
// drop copy through it).
type AllocationChange struct {
	Type        string // "COMMITTED" | "REPLACED" | "CANCELLED"
	PrevAllocID string // set on REPLACED
	Allocation  Allocation
	Report      *quickfix.Message // the emitted 35=AK
}

// AllocationChangeSink receives committed allocation lifecycle events —
// Phase-24 settlement propagation and PB drop copy plug in here.
type AllocationChangeSink interface {
	OnAllocationChange(ctx context.Context, ev AllocationChange) error
}

// allocationPersister is the persistence seam — *AllocationStore satisfies
// it; unit tests substitute an in-memory fake so 35=J orchestration is
// exercisable without PostgreSQL.
type allocationPersister interface {
	SaveNew(ctx context.Context, a *Allocation, legs []AllocationLeg) error
	Supersede(ctx context.Context, refAllocID string, repl *Allocation, legs []AllocationLeg) error
	Cancel(ctx context.Context, allocID, reason string) (*Allocation, error)
	AppendEvent(ctx context.Context, allocID int64, eventType string, payload map[string]any, actor string) error
}

// AllocationService parses, validates, persists and reports 35=J/35=AK.
type AllocationService struct {
	store        allocationPersister
	execResolver ExecQuantityResolver
	accounts     AccountResolver
	sinks        []AllocationChangeSink
	now          func() time.Time
}

// NewAllocationService wires the service. Sinks may be registered later via
// AddSink (PB drop copy registers itself, settlement via adapter).
func NewAllocationService(store allocationPersister, exec ExecQuantityResolver, accounts AccountResolver) *AllocationService {
	return &AllocationService{store: store, execResolver: exec, accounts: accounts, now: time.Now}
}

// AddSink registers a committed-change sink.
func (s *AllocationService) AddSink(sink AllocationChangeSink) { s.sinks = append(s.sinks, sink) }

// AllocationOutcome is what the session-core handler sends back: an ack
// (35=P) on rejection or a report (35=AK) on success.
type AllocationOutcome struct {
	Allocation Allocation
	Ack        *quickfix.Message // 35=P — set on reject
	Report     *quickfix.Message // 35=AK — set on accept/cancel-confirm
}

// Handle processes one 35=J. sessionID is the originating FIX session;
// masterAccountID is the session's bound account (spec §5.20 entitlement —
// allocations may only be instructed against the bound account's fills).
func (s *AllocationService) Handle(ctx context.Context, sessionID string, masterAccountID int64, msg *quickfix.Message) (*AllocationOutcome, error) {
	instr, err := ParseAllocationInstruction(msg)
	if err != nil {
		return nil, err // malformed — session-core emits 35=3 Reject
	}
	switch instr.TransType {
	case AllocTransNew:
		return s.handleNew(ctx, sessionID, masterAccountID, instr)
	case AllocTransReplace:
		return s.handleReplace(ctx, sessionID, masterAccountID, instr)
	case AllocTransCancel:
		return s.handleCancel(ctx, sessionID, masterAccountID, instr)
	}
	return nil, allocErr(CodeAllocationInvalid, "unhandled AllocTransType %d", instr.TransType)
}

// rejectAck builds the 35=P AllocationInstructionAck carrying the coded
// rejection (spec §27.1: AllocRejCode=4 for ALLOCATION_SUM_MISMATCH).
func rejectAck(allocID, code string, rejCode int, status int) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgAllocationInstructionAck)
	m.Body.SetString(TagAllocID, allocID)
	m.Body.SetInt(TagAllocStatus, status)
	m.Body.SetInt(TagAllocRejCode, rejCode)
	m.Body.SetString(TagText, code)
	return m
}

func rejCodeFor(err error) int {
	var ce *excerrors.Error
	if errors.As(err, &ce) {
		if ce.Code == CodeAllocationSumMismatch {
			return AllocRejCodeSumMismatch
		}
	}
	return AllocRejCodeUnknownAccount
}

// handleNew validates and commits a NEW instruction.
func (s *AllocationService) handleNew(ctx context.Context, sessionID string, masterAccountID int64, instr *AllocationInstruction) (*AllocationOutcome, error) {
	a := Allocation{
		AllocID: instr.AllocID, RefAllocID: instr.RefAllocID,
		TransType: instr.TransType, Method: instr.Method,
		SessionID: sessionID, MasterAccountID: masterAccountID,
		Symbol: instr.Symbol, Side: instr.Side,
		ExecRefs: append(append([]string{}, instr.ExecRefs...), instr.OrderRefs...),
	}
	execQty, _, verr := s.validate(ctx, instr, masterAccountID)
	if verr != nil {
		a.Status = AllocStatusRejected
		a.RejectReason = verr.Error()
		a.ExecQty = execQty
		if perr := s.store.SaveNew(ctx, &a, nil); perr != nil {
			return nil, fmt.Errorf("fix: persist rejected allocation %s: %w (validation: %v)", instr.AllocID, perr, verr)
		}
		return &AllocationOutcome{
			Allocation: a,
			Ack:        rejectAck(instr.AllocID, codeOf(verr), rejCodeFor(verr), statusFor(verr)),
		}, nil
	}
	a.Status = AllocStatusAccepted
	a.ExecQty = execQty
	if instr.ExecRefs != nil {
		for i := range instr.Legs {
			instr.Legs[i].ParentExecID = instr.ExecRefs[0]
		}
	}
	if err := s.store.SaveNew(ctx, &a, instr.Legs); err != nil {
		return nil, fmt.Errorf("fix: persist allocation %s: %w", instr.AllocID, err)
	}
	report := BuildAllocationReport(a, a.Legs)
	out := &AllocationOutcome{Allocation: a, Report: report}
	s.propagate(ctx, AllocationChange{Type: "COMMITTED", Allocation: a, Report: report})
	return out, nil
}

// handleReplace amends an ACCEPTED instruction pre-settlement (Task 18.3.13
// step 6). The prior row moves to REPLACED; the new row supersedes it.
func (s *AllocationService) handleReplace(ctx context.Context, sessionID string, masterAccountID int64, instr *AllocationInstruction) (*AllocationOutcome, error) {
	if instr.RefAllocID == "" {
		return &AllocationOutcome{
			Ack: rejectAck(instr.AllocID, CodeAllocationInvalid, AllocRejCodeMismatchedData, AllocStatusBlockLevelReject),
		}, nil
	}
	a := Allocation{
		AllocID: instr.AllocID, RefAllocID: instr.RefAllocID,
		TransType: instr.TransType, Method: instr.Method,
		SessionID: sessionID, MasterAccountID: masterAccountID,
		Symbol: instr.Symbol, Side: instr.Side,
		ExecRefs: append(append([]string{}, instr.ExecRefs...), instr.OrderRefs...),
	}
	execQty, _, verr := s.validate(ctx, instr, masterAccountID)
	if verr != nil {
		a.Status = AllocStatusRejected
		a.RejectReason = verr.Error()
		a.ExecQty = execQty
		if perr := s.store.SaveNew(ctx, &a, nil); perr != nil {
			return nil, perr
		}
		return &AllocationOutcome{Allocation: a,
			Ack: rejectAck(instr.AllocID, codeOf(verr), rejCodeFor(verr), statusFor(verr))}, nil
	}
	a.Status = AllocStatusAccepted
	a.ExecQty = execQty
	if len(instr.ExecRefs) > 0 {
		for i := range instr.Legs {
			instr.Legs[i].ParentExecID = instr.ExecRefs[0]
		}
	}
	if err := s.store.Supersede(ctx, instr.RefAllocID, &a, instr.Legs); err != nil {
		var ce *excerrors.Error
		if errors.As(err, &ce) {
			return &AllocationOutcome{Allocation: a,
				Ack: rejectAck(instr.AllocID, ce.Code, AllocRejCodeOther, AllocStatusBlockLevelReject)}, nil
		}
		return nil, err
	}
	report := BuildAllocationReport(a, a.Legs)
	s.propagate(ctx, AllocationChange{Type: "REPLACED", PrevAllocID: instr.RefAllocID, Allocation: a, Report: report})
	return &AllocationOutcome{Allocation: a, Report: report}, nil
}

// handleCancel reverses an ACCEPTED instruction pre-settlement.
func (s *AllocationService) handleCancel(ctx context.Context, sessionID string, masterAccountID int64, instr *AllocationInstruction) (*AllocationOutcome, error) {
	target := instr.RefAllocID
	if target == "" {
		target = instr.AllocID
	}
	a, err := s.store.Cancel(ctx, target, "client 35=J CANCEL")
	if err != nil {
		var ce *excerrors.Error
		if errors.As(err, &ce) {
			return &AllocationOutcome{Ack: rejectAck(instr.AllocID, ce.Code, AllocRejCodeOther, AllocStatusBlockLevelReject)}, nil
		}
		return nil, err
	}
	report := BuildAllocationReport(*a, a.Legs)
	report.Body.SetString(TagText, "ALLOCATION_CANCELLED")
	s.propagate(ctx, AllocationChange{Type: "CANCELLED", Allocation: *a, Report: report})
	return &AllocationOutcome{Allocation: *a, Report: report}, nil
}

func codeOf(err error) string {
	var ce *excerrors.Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return "INTERNAL_ERROR"
}

func statusFor(err error) int {
	var ce *excerrors.Error
	if errors.As(err, &ce) && ce.Code == CodeAllocationInvalid {
		return AllocStatusAccountLevelReject
	}
	return AllocStatusBlockLevelReject
}

// propagate fans committed changes to sinks; sink failures are durable — a
// PROPAGATION_FAILED audit event is appended so Phase-24 can replay.
func (s *AllocationService) propagate(ctx context.Context, ev AllocationChange) {
	var errs []error
	for _, sink := range s.sinks {
		if err := sink.OnAllocationChange(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 && s.store != nil {
		_ = s.store.AppendEvent(ctx, ev.Allocation.ID, EvPropagationFailed,
			map[string]any{"type": ev.Type, "errors": fmt.Sprint(errs)}, "fix.allocation")
	}
}

// ---------------------------------------------------------------------------
// Report builder — AllocationReport (35=AK).
// ---------------------------------------------------------------------------

// BuildAllocationReport emits the per-allocation 35=AK with per-leg booking
// records (NoAllocs echo: AllocAccount/AllocQty/IndividualAllocID).
func BuildAllocationReport(a Allocation, legs []AllocationLeg) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgAllocationReport)
	m.Body.SetString(TagAllocReportID, fmt.Sprintf("ALLOC-%d", a.ID))
	m.Body.SetString(TagAllocID, a.AllocID)
	if a.RefAllocID != "" {
		m.Body.SetString(TagRefAllocID, a.RefAllocID)
	}
	m.Body.SetInt(TagAllocTransType, int(a.TransType))
	m.Body.SetInt(quickfix.Tag(756), 5) // AllocReportType = Complete
	status := AllocStatusAcceptedFIX
	if a.Status == AllocStatusRejected {
		status = AllocStatusBlockLevelReject
	}
	m.Body.SetInt(TagAllocStatus, status)
	if a.RejectReason != "" {
		m.Body.SetString(TagText, a.RejectReason)
	}
	m.Body.SetString(TagSymbol, a.Symbol)
	if a.Side != 0 {
		m.Body.SetString(TagSide, string(a.Side))
	}
	m.Body.SetString(TagQuantity, a.ExecQty.String())
	m.Body.SetString(quickfix.Tag(60), time.Now().UTC().Format("20060102-15:04:05.000")) // TransactTime
	if len(legs) > 0 {
		grp := quickfix.NewRepeatingGroup(TagNoAllocs, quickfix.GroupTemplate{
			quickfix.GroupElement(TagAllocAccount),
			quickfix.GroupElement(TagAllocQty),
			quickfix.GroupElement(TagAllocPrice),
			quickfix.GroupElement(TagIndividualAllocID),
		})
		for _, l := range legs {
			g := grp.Add()
			g.SetString(TagAllocAccount, l.AllocAccount)
			g.SetString(TagAllocQty, l.AllocQty.String())
			if !l.AllocPrice.IsZero() {
				g.SetString(TagAllocPrice, l.AllocPrice.String())
			}
			g.SetString(TagIndividualAllocID, l.ChildExecID)
		}
		m.Body.SetGroup(grp)
	}
	return m
}

// ---------------------------------------------------------------------------
// AllocationStore — migration 226 persistence.
// ---------------------------------------------------------------------------

// AllocationStore persists fix_allocations / fix_allocation_legs /
// allocation_events.
type AllocationStore struct{ pool *pgxpool.Pool }

// NewAllocationStore binds the store.
func NewAllocationStore(pool *pgxpool.Pool) *AllocationStore { return &AllocationStore{pool: pool} }

const allocColumns = `id, alloc_id, COALESCE(ref_alloc_id,''), alloc_trans_type, method::text,
	status::text, session_id, master_account_id, symbol, COALESCE(side::text,''), exec_qty,
	settlement_locked, COALESCE(reject_reason,''), created_at, updated_at`

func scanAlloc(row pgx.Row) (Allocation, error) {
	var a Allocation
	var tt int
	var qty, side string
	err := row.Scan(&a.ID, &a.AllocID, &a.RefAllocID, &tt, &a.Method, &a.Status,
		&a.SessionID, &a.MasterAccountID, &a.Symbol, &side, &qty,
		&a.SettlementLocked, &a.RejectReason, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return a, err
	}
	a.TransType = AllocTransType(tt)
	if side != "" {
		a.Side = side[0]
	}
	a.ExecQty, err = decimal.NewFromString(qty)
	return a, err
}

// SaveNew persists an instruction (ACCEPTED with legs, or REJECTED header
// only) plus its audit events in one SERIALIZABLE transaction.
func (s *AllocationStore) SaveNew(ctx context.Context, a *Allocation, legs []AllocationLeg) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	refs, _ := json.Marshal(a.ExecRefs)
	err = tx.QueryRow(ctx,
		`INSERT INTO fix_allocations
		  (alloc_id, ref_alloc_id, alloc_trans_type, method, status, session_id,
		   master_account_id, symbol, side, exec_qty, exec_refs, reject_reason)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING id, created_at, updated_at`,
		a.AllocID, nilIfEmpty(a.RefAllocID), int(a.TransType), string(a.Method),
		string(a.Status), a.SessionID, a.MasterAccountID, a.Symbol,
		nilIfByte(a.Side), a.ExecQty.String(), refs, nilIfEmpty(a.RejectReason)).
		Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("fix: insert fix_allocations %s: %w", a.AllocID, err)
	}
	a.Legs = legs
	for i := range a.Legs {
		a.Legs[i].ChildExecID = fmt.Sprintf("ALLOC-%d-%d", a.ID, a.Legs[i].LegNo)
	}
	if err := insertLegs(ctx, tx, a); err != nil {
		return err
	}
	if err := stampChildExecIDs(ctx, tx, a); err != nil {
		return err
	}
	if err := appendEvents(ctx, tx, a.ID, a.SessionID,
		event{EvReceived, map[string]any{"alloc_id": a.AllocID, "trans_type": int(a.TransType)}},
		event{string(a.Status), map[string]any{"reason": a.RejectReason, "legs": len(a.Legs)}},
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// insertLegs writes leg rows; child_exec_id is stamped after the parent id is
// known (two-pass: insert without it, then update — keeps the UNIQUE column).
func insertLegs(ctx context.Context, tx pgx.Tx, a *Allocation) error {
	for _, l := range a.Legs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO fix_allocation_legs
			  (allocation_id, leg_no, alloc_account, alloc_account_id, alloc_qty,
			   alloc_price, step_out_broker, leg_status, parent_exec_id)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			a.ID, l.LegNo, l.AllocAccount, nilIfZero(l.AccountID),
			l.AllocQty.String(), nilIfZeroDec(l.AllocPrice),
			nilIfEmpty(l.StepOutBroker), string(l.Status), nilIfEmpty(l.ParentExecID)); err != nil {
			return fmt.Errorf("fix: insert leg %d of %s: %w", l.LegNo, a.AllocID, err)
		}
	}
	return nil
}

// stampChildExecIDs writes the generated booking refs once the parent id is
// known. Called inside SaveNew/Supersede transactions.
func stampChildExecIDs(ctx context.Context, tx pgx.Tx, a *Allocation) error {
	for _, l := range a.Legs {
		if _, err := tx.Exec(ctx,
			`UPDATE fix_allocation_legs SET child_exec_id=$3
			  WHERE allocation_id=$1 AND leg_no=$2`,
			a.ID, l.LegNo, l.ChildExecID); err != nil {
			return err
		}
	}
	return nil
}

type event struct {
	typ     string
	payload map[string]any
}

// appendEvents appends audit rows with per-allocation monotonic seq.
func appendEvents(ctx context.Context, tx pgx.Tx, allocID int64, actor string, evs ...event) error {
	var seq int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq),0) FROM allocation_events WHERE allocation_id=$1`, allocID).Scan(&seq); err != nil {
		return err
	}
	for _, e := range evs {
		seq++
		p, _ := json.Marshal(e.payload)
		if _, err := tx.Exec(ctx,
			`INSERT INTO allocation_events (allocation_id, seq, event_type, payload, actor)
			 VALUES ($1,$2,$3,$4,$5)`, allocID, seq, e.typ, p, actor); err != nil {
			return fmt.Errorf("fix: append allocation event %s: %w", e.typ, err)
		}
	}
	return nil
}

// Supersede atomically retires the ACCEPTED instruction named by refAllocID
// and commits its replacement (Task 18.3.13 REPLACE). Settlement-locked or
// non-ACCEPTED instructions are rejected ALLOCATION_INVALID.
func (s *AllocationStore) Supersede(ctx context.Context, refAllocID string, repl *Allocation, legs []AllocationLeg) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	prevID, err := lockAmendable(ctx, tx, refAllocID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE fix_allocations SET status='REPLACED', updated_at=now() WHERE id=$1`, prevID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE fix_allocation_legs SET leg_status='CANCELLED' WHERE allocation_id=$1`, prevID); err != nil {
		return err
	}
	if err := appendEvents(ctx, tx, prevID, repl.SessionID,
		event{EvSuperseded, map[string]any{"superseded_by": repl.AllocID}}); err != nil {
		return err
	}
	refs, _ := json.Marshal(repl.ExecRefs)
	err = tx.QueryRow(ctx,
		`INSERT INTO fix_allocations
		  (alloc_id, ref_alloc_id, alloc_trans_type, method, status, session_id,
		   master_account_id, symbol, side, exec_qty, exec_refs, reject_reason)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING id, created_at, updated_at`,
		repl.AllocID, refAllocID, int(repl.TransType), string(repl.Method),
		string(repl.Status), repl.SessionID, repl.MasterAccountID, repl.Symbol,
		nilIfByte(repl.Side), repl.ExecQty.String(), refs, nilIfEmpty(repl.RejectReason)).
		Scan(&repl.ID, &repl.CreatedAt, &repl.UpdatedAt)
	if err != nil {
		return fmt.Errorf("fix: insert replacement %s: %w", repl.AllocID, err)
	}
	repl.Legs = legs
	if err := insertLegs(ctx, tx, repl); err != nil {
		return err
	}
	for i := range repl.Legs {
		repl.Legs[i].ChildExecID = fmt.Sprintf("ALLOC-%d-%d", repl.ID, repl.Legs[i].LegNo)
	}
	if err := stampChildExecIDs(ctx, tx, repl); err != nil {
		return err
	}
	if err := appendEvents(ctx, tx, repl.ID, repl.SessionID,
		event{EvReceived, map[string]any{"alloc_id": repl.AllocID, "trans_type": int(repl.TransType), "supersedes": refAllocID}},
		event{EvAccepted, map[string]any{"legs": len(repl.Legs)}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Cancel marks an ACCEPTED instruction CANCELLED with legs CANCELLED
// (Task 18.3.13 CANCEL). Returns the updated row set.
func (s *AllocationStore) Cancel(ctx context.Context, allocID, reason string) (*Allocation, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := lockAmendable(ctx, tx, allocID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE fix_allocations SET status='CANCELLED', updated_at=now() WHERE id=$1`, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE fix_allocation_legs SET leg_status='CANCELLED' WHERE allocation_id=$1`, id); err != nil {
		return nil, err
	}
	if err := appendEvents(ctx, tx, id, "fix.allocation",
		event{EvCancelled, map[string]any{"reason": reason}}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	a, err := s.LoadByAllocID(ctx, allocID)
	return a, err
}

// lockAmendable takes the FOR UPDATE row lock on the instruction and asserts
// the pre-settlement amendability contract.
func lockAmendable(ctx context.Context, tx pgx.Tx, allocID string) (int64, error) {
	var id int64
	var status string
	var locked bool
	err := tx.QueryRow(ctx,
		`SELECT id, status::text, settlement_locked FROM fix_allocations
		  WHERE alloc_id=$1 FOR UPDATE`, allocID).Scan(&id, &status, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, allocErr(CodeAllocationInvalid, "allocation %q not found", allocID)
	}
	if err != nil {
		return 0, err
	}
	if locked {
		return 0, allocErr(CodeAllocationInvalid, "allocation %q claimed by settlement — corrections closed", allocID)
	}
	if status != string(AllocStatusAccepted) {
		return 0, allocErr(CodeAllocationInvalid, "allocation %q in status %s is not amendable", allocID, status)
	}
	return id, nil
}

// LoadByAllocID returns the instruction plus legs.
func (s *AllocationStore) LoadByAllocID(ctx context.Context, allocID string) (*Allocation, error) {
	a, err := scanAlloc(s.pool.QueryRow(ctx,
		`SELECT `+allocColumns+` FROM fix_allocations WHERE alloc_id=$1`, allocID))
	if err != nil {
		return nil, err
	}
	refs, err := s.loadExecRefs(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	a.ExecRefs = refs
	legs, err := s.loadLegs(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	a.Legs = legs
	return &a, nil
}

// loadExecRefs fetches exec_refs for an allocation row.
func (s *AllocationStore) loadExecRefs(ctx context.Context, id int64) ([]string, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT exec_refs FROM fix_allocations WHERE id=$1`, id).Scan(&raw); err != nil {
		return nil, err
	}
	var out []string
	return out, json.Unmarshal(raw, &out)
}

func (s *AllocationStore) loadLegs(ctx context.Context, allocID int64) ([]AllocationLeg, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT leg_no, alloc_account, COALESCE(alloc_account_id,0), alloc_qty::text,
		        COALESCE(alloc_price::text,''), COALESCE(step_out_broker,''),
		        leg_status::text, COALESCE(parent_exec_id,''), COALESCE(child_exec_id,'')
		 FROM fix_allocation_legs WHERE allocation_id=$1 ORDER BY leg_no`, allocID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AllocationLeg
	for rows.Next() {
		var l AllocationLeg
		var qty, px string
		if err := rows.Scan(&l.LegNo, &l.AllocAccount, &l.AccountID, &qty, &px,
			&l.StepOutBroker, &l.Status, &l.ParentExecID, &l.ChildExecID); err != nil {
			return nil, err
		}
		l.AllocQty, _ = decimal.NewFromString(qty)
		if px != "" {
			l.AllocPrice, _ = decimal.NewFromString(px)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// AppendEvent appends an audit row (public seam for propagation records).
func (s *AllocationStore) AppendEvent(ctx context.Context, allocID int64, eventType string, payload map[string]any, actor string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := appendEvents(ctx, tx, allocID, actor, event{eventType, payload}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Events returns the immutable audit trail for one instruction.
func (s *AllocationStore) Events(ctx context.Context, allocID int64) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, event_type, payload, COALESCE(actor,''), created_at
		 FROM allocation_events WHERE allocation_id=$1 ORDER BY seq`, allocID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var seq int
		var typ, actor string
		var payload []byte
		var at time.Time
		if err := rows.Scan(&seq, &typ, &payload, &actor, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"seq": seq, "event_type": typ, "payload": json.RawMessage(payload),
			"actor": actor, "created_at": at,
		})
	}
	return out, rows.Err()
}

// ResolveAllocAccount implements AccountResolver: AllocAccount(79) resolves
// to accounts.id (numeric string) or, failing that, is rejected. The resolved
// account must equal the master or carry parent_account_id = master — legs
// may never cross master boundaries (Task 18.3.13 step 3).
func (s *AllocationStore) ResolveAllocAccount(ctx context.Context, masterAccountID int64, allocAccount string) (int64, error) {
	var id int64
	if _, err := fmt.Sscanf(allocAccount, "%d", &id); err != nil || id <= 0 {
		return 0, fmt.Errorf("fix: alloc account %q not numeric", allocAccount)
	}
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM accounts
		   WHERE id=$1 AND (id=$2 OR parent_account_id=$2))`, id, masterAccountID).Scan(&ok)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("fix: alloc account %d outside master %d hierarchy", id, masterAccountID)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilIfByte(b byte) any {
	if b == 0 {
		return nil
	}
	return string(b)
}

func nilIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nilIfZeroDec(d decimal.Decimal) any {
	if d.IsZero() {
		return nil
	}
	return d.String()
}
