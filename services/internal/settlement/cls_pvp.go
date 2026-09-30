// cls_pvp.go — Continuous Linked Settlement (CLS) third-party PvP
// settlement service (Phase-24 Task 24.3.8; spec §17.6, §24 #126).
//
// The exchange is not a CLS settlement member; it instructs through a
// contracted settlement member's CLS-supported SWIFT ISO 20022 XML
// interface. This service therefore:
//
//   - loads eligibility + cut-off reference data from the VERSIONED
//     cls_reference_* tables (migration 258) — the prior hard-coded
//     18-currency list and 06:30/09:00 CET constants are superseded and
//     deliberately absent from code;
//   - submits / amends / rescinds paired instructions via the
//     ClsMemberAdapter seam — a nil adapter fails closed
//     (CLS_MEMBER_UNAVAILABLE) and the instruction is never marked
//     dispatched;
//   - persists the full status lifecycle (RECEIVED → VALIDATED →
//     MATCHED|UNMATCHED → ELIGIBLE|INELIGIBLE → PAY_IN → SETTLED |
//     RESCINDED | EXPIRED | REJECTED) with an append-only event journal;
//   - posts GL/nostro finality ONLY from an authenticated member finality
//     notification (spec §17.6 step 4 — CLS provides PvP finality in
//     central-bank money; this service records and reconciles it);
//   - routes ineligible flow through the settlement-risk waterfall:
//     alternative PvP → legally enforceable bilateral netting →
//     controlled gross settlement with principal-risk amount/duration
//     limits and alerts.
//
// Fail-closed (spec §2.7): missing reference data, unknown currencies,
// unauthenticated finality and state-machine violations are errors,
// never silent transitions.
package settlement

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// Error codes — registered in internal/errs (localRows pending §23 rows).
const (
	// CodeClsMemberUnavailable — the member adapter (SWIFT ISO 20022
	// transport) is not wired; the instruction stays in its pre-dispatch
	// state, never claimed sent.
	CodeClsMemberUnavailable = "CLS_MEMBER_UNAVAILABLE"
	// CodeClsWindowClosed — the versioned cut-off window (pay-in /
	// rescind deadline) for the instruction's value date has closed.
	CodeClsWindowClosed = "CLS_WINDOW_CLOSED"
	// CodeClsMatchFailed — the member reported the paired instruction
	// unmatched, or a member match report conflicts with the persisted
	// instruction (spec §27.1 CLS matrix code).
	CodeClsMatchFailed = "CLS_MATCH_FAILED"
	// CodeClsStateConflict — requested transition violates the
	// instruction status lifecycle.
	CodeClsStateConflict = "CLS_INSTRUCTION_STATE_CONFLICT"
	// CodeClsRefDataMissing — no ACTIVE cls_reference_versions row; every
	// eligibility/cut-off decision fails closed without reference data.
	CodeClsRefDataMissing = "CLS_REFERENCE_DATA_MISSING"
	// CodeClsNotEligible — pair/product/member fails the versioned
	// eligibility checks; the caller must take the settlement-risk
	// waterfall (RouteIneligible).
	CodeClsNotEligible = "CLS_NOT_ELIGIBLE"
)

// ClsStatus mirrors cls_instruction_status_enum.
type ClsStatus string

const (
	ClsReceived   ClsStatus = "RECEIVED"
	ClsValidated  ClsStatus = "VALIDATED"
	ClsMatched    ClsStatus = "MATCHED"
	ClsUnmatched  ClsStatus = "UNMATCHED"
	ClsEligible   ClsStatus = "ELIGIBLE"
	ClsIneligible ClsStatus = "INELIGIBLE"
	ClsPayIn      ClsStatus = "PAY_IN"
	ClsSettled    ClsStatus = "SETTLED"
	ClsRescinded  ClsStatus = "RESCINDED"
	ClsExpired    ClsStatus = "EXPIRED"
	ClsRejected   ClsStatus = "REJECTED"
)

// clsTransitions is the legal lifecycle edge set. Anything outside this
// table is CLS_INSTRUCTION_STATE_CONFLICT — the lifecycle is the contract.
var clsTransitions = map[ClsStatus]map[ClsStatus]bool{
	ClsReceived:   {ClsValidated: true, ClsRejected: true},
	ClsValidated:  {ClsMatched: true, ClsUnmatched: true, ClsEligible: true, ClsIneligible: true, ClsRejected: true, ClsExpired: true},
	ClsMatched:    {ClsEligible: true, ClsIneligible: true, ClsRescinded: true, ClsExpired: true},
	ClsUnmatched:  {ClsValidated: true, ClsRescinded: true, ClsExpired: true, ClsRejected: true},
	ClsEligible:   {ClsPayIn: true, ClsRescinded: true, ClsExpired: true},
	ClsIneligible: {ClsRescinded: true, ClsExpired: true, ClsRejected: true},
	ClsPayIn:      {ClsSettled: true, ClsExpired: true, ClsRejected: true},
}

// clsTerminal lists statuses from which no further transition is legal.
func clsTerminal(s ClsStatus) bool {
	switch s {
	case ClsSettled, ClsRescinded, ClsExpired, ClsRejected:
		return true
	}
	return false
}

// ClsRoute mirrors cls_settlement_route_enum — the settlement-risk
// waterfall outcome (spec §17.6 step 5).
type ClsRoute string

const (
	ClsRoutePvp             ClsRoute = "CLS_PVP"
	ClsRouteAltPvp          ClsRoute = "ALT_PVP"
	ClsRouteNetting         ClsRoute = "NETTING"
	ClsRouteControlledGross ClsRoute = "CONTROLLED_GROSS"
)

// ---------------------------------------------------------------------------
// Versioned reference data (migration 258) — the ONLY source of eligible
// currencies / products / members / cut-offs / principal-risk limits.
// ---------------------------------------------------------------------------

// ClsCutoff is one named cut-off window expressed as a local wall-clock
// time in an IANA timezone (e.g. initial pay-in 06:30 Europe/Berlin).
type ClsCutoff struct {
	Name     string // INITIAL_PAY_IN | FINAL_PAY_IN | RESCIND_DEADLINE
	Local    string // "HH:MM"
	Timezone string // IANA name

	loc       *time.Location
	cutoffMin int // minutes since local midnight
}

// Minutes parses "HH:MM" into minutes since midnight.
func (c ClsCutoff) minutes() int { return c.cutoffMin }

// ClsPrincipalLimit is the controlled-gross principal-risk bound for a
// currency: maximum gross principal per settlement and the maximum
// duration the principal may remain exposed.
type ClsPrincipalLimit struct {
	Currency     string
	MaxPrincipal decimal.Decimal // nil-able: zero means "no gross settlement allowed"
	MaxDurationH int             // hours of permitted principal exposure
}

// ClsRefData is the assembled ACTIVE reference version.
type ClsRefData struct {
	VersionID  int64
	Version    string
	Currencies map[string]bool              // CLS-eligible ISO currencies
	Products   map[string]bool              // eligible product codes
	Members    map[string]bool              // settlement-member BICs we may instruct through
	Cutoffs    map[string]ClsCutoff         // named cut-off windows
	AltPvP     map[string]bool              // pair key "CCY1/CCY2" with an alternative PvP provider
	Limits     map[string]ClsPrincipalLimit // controlled-gross principal-risk limits per currency
}

// Eligible reports whether the pair/product/member passes every
// reference-data check; reasons lists each failed gate (audit).
func (r *ClsRefData) Eligible(buyCcy, sellCcy, product, memberBIC string) (bool, []string) {
	var reasons []string
	if !r.Currencies[buyCcy] {
		reasons = append(reasons, "buy currency "+buyCcy+" not CLS-eligible in version "+r.Version)
	}
	if !r.Currencies[sellCcy] {
		reasons = append(reasons, "sell currency "+sellCcy+" not CLS-eligible in version "+r.Version)
	}
	if !r.Products[strings.ToUpper(product)] {
		reasons = append(reasons, "product "+product+" not eligible in version "+r.Version)
	}
	if !r.Members[strings.ToUpper(memberBIC)] {
		reasons = append(reasons, "member "+memberBIC+" not a contracted CLS settlement member in version "+r.Version)
	}
	return len(reasons) == 0, reasons
}

// AltPvPFor reports whether an alternative PvP provider exists for the
// pair (either orientation).
func (r *ClsRefData) AltPvPFor(buyCcy, sellCcy string) bool {
	return r.AltPvP[buyCcy+"/"+sellCcy] || r.AltPvP[sellCcy+"/"+buyCcy]
}

// Cutoff resolves a named cut-off window; missing = error at the caller
// (fail-closed — never assume a time).
func (r *ClsRefData) Cutoff(name string) (ClsCutoff, bool) {
	c, ok := r.Cutoffs[name]
	return c, ok
}

// Passed reports whether at is at/after the cut-off on the cut-off's own
// local calendar day containing `at`.
func (c ClsCutoff) Passed(at time.Time) (bool, error) {
	if c.loc == nil {
		loc, err := time.LoadLocation(c.Timezone)
		if err != nil {
			return false, fmt.Errorf("cls cutoff %s: load timezone %q: %w", c.Name, c.Timezone, err)
		}
		c.loc = loc
	}
	local := at.In(c.loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.loc)
	return !local.Before(midnight.Add(time.Duration(c.cutoffMin) * time.Minute)), nil
}

// ---------------------------------------------------------------------------
// Member adapter seam — CLS-supported SWIFT ISO 20022 XML interface.
// nil adapter ⇒ fail closed, never claim dispatch.
// ---------------------------------------------------------------------------

// ClsMessageKind enumerates the ISO 20022 instruction verbs.
type ClsMessageKind string

const (
	ClsMsgSubmit  ClsMessageKind = "SUBMIT"
	ClsMsgAmend   ClsMessageKind = "AMEND"
	ClsMsgRescind ClsMessageKind = "RESCIND"
	ClsMsgStatus  ClsMessageKind = "STATUS_QUERY"
)

// ClsMessage is one outbound member-interface envelope.
type ClsMessage struct {
	Kind           ClsMessageKind
	InstructionRef string
	MemberBIC      string
	UETR           string
	XML            string // rendered ISO 20022 body
}

// ClsMemberAck is the member's synchronous acknowledgement.
type ClsMemberAck struct {
	MemberInstructionID string
	AckRef              string
	Matched             bool   // paired instruction matched in CLS
	RejectReason        string // non-empty on member rejection
}

// ClsMemberAdapter is the transport seam to the CLS settlement member's
// ISO 20022 interface. The production adapter is the correspondent
// connector; tests bind a recording fake.
type ClsMemberAdapter interface {
	Send(ctx context.Context, msg ClsMessage) (*ClsMemberAck, error)
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// ClsInstruction is one cls_settlement_instructions row.
type ClsInstruction struct {
	ID                    int64
	InstructionRef        string
	CounterpartyAccountID int64
	MemberBIC             string
	Product               string
	BuyCurrency           string
	BuyAmount             decimal.Decimal
	SellCurrency          string
	SellAmount            decimal.Decimal
	ValueDate             time.Time
	TradeID               *int64
	Status                ClsStatus
	SettlementRoute       ClsRoute
	MemberInstructionID   *string
	MemberAckRef          *string
	UETR                  *string
	MessagePayload        *string
	PayloadVersion        int
	RejectReason          *string
	RefVersionID          *int64
	GLJournalID           *int64
	CreatedAt             time.Time
	SettledAt             *time.Time
}

// ClsStatusEvent is the append-only transition record; Authenticated is
// set only on member-authenticated status notifications.
type ClsStatusEvent struct {
	InstructionID int64
	From          ClsStatus
	To            ClsStatus
	Authenticated bool
	MemberRef     string
	Detail        string
}

// ClsNewInstruction is the submit request payload.
type ClsNewInstruction struct {
	InstructionRef        string // client/correlation ref; empty → generated
	CounterpartyAccountID int64
	MemberBIC             string
	Product               string // SPOT|FORWARD|SWAP|NDF — checked against ref data
	BuyCurrency           string
	BuyAmount             decimal.Decimal
	SellCurrency          string
	SellAmount            decimal.Decimal
	ValueDate             time.Time
	TradeID               *int64
}

// ClsAmend carries mutable fields (amounts/value date pre-dispatch).
type ClsAmend struct {
	BuyAmount  *decimal.Decimal
	SellAmount *decimal.Decimal
	ValueDate  *time.Time
}

// ---------------------------------------------------------------------------
// Store seam
// ---------------------------------------------------------------------------

// ClsStore is the persistence seam (PgxClsStore implements it).
type ClsStore interface {
	// LoadReference loads the ACTIVE reference version; found=false when
	// none is active (fail-closed → CLS_REFERENCE_DATA_MISSING).
	LoadReference(ctx context.Context) (ref *ClsRefData, found bool, err error)
	// NettingAgreementExists reports whether the counterparty holds a
	// legally enforceable netting agreement (legal_agreements ISDA row).
	NettingAgreementExists(ctx context.Context, counterpartyAccountID int64) (bool, error)
	InTx(ctx context.Context, fn func(ctx context.Context, tx ClsTx) error) error
}

// ClsTx is the transactional view inside ClsStore.InTx.
type ClsTx interface {
	InsertInstruction(ctx context.Context, i ClsInstruction) (int64, error)
	// LockInstruction SELECTs the row FOR UPDATE inside the tx by its
	// correlation ref (instruction_ref) or, when ref is empty, by id.
	LockInstruction(ctx context.Context, ref string, id int64) (*ClsInstruction, bool, error)
	// Transition moves the instruction to a new status, appends the event
	// row, and stamps member refs / payload / route as provided.
	Transition(ctx context.Context, id int64, from, to ClsStatus, e ClsStatusEvent) error
	// UpdatePayload stores the (re)rendered ISO 20022 body + bumped
	// payload_version after a successful submit/amend/rescind.
	UpdatePayload(ctx context.Context, id int64, payload string, version int, uetr *string) error
	// SetMemberIDs persists member-assigned ids from the ack.
	SetMemberIDs(ctx context.Context, id int64, memberInstrID, ackRef string) error
	// SetSettled marks finality + the GL journal link.
	SetSettled(ctx context.Context, id int64, at time.Time, journalID int64) error
	// SetRoute records the settlement-risk waterfall outcome.
	SetRoute(ctx context.Context, id int64, route ClsRoute) error
	// InsertException writes a settlement_exceptions break row.
	InsertException(ctx context.Context, e SettlementException) error
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// ClsPvpOptions wires optional collaborators.
type ClsPvpOptions struct {
	// Member is the CLS member ISO 20022 adapter; nil fails closed on any
	// dispatch-class operation (CLS_MEMBER_UNAVAILABLE).
	Member ClsMemberAdapter
	// Poster posts the finality GL journal; required for RecordFinality —
	// a nil poster refuses finality rather than settling off-ledger.
	Poster JournalPoster
	// Alerter raises P1/P2 ops alerts (unmatched, exceptions, principal
	// risk breaches).
	Alerter OpsAlerter
	// Clock overrides time.Now (tests).
	Clock func() time.Time
}

// ClsPvpService owns CLS instruction lifecycle + eligibility + routing.
type ClsPvpService struct {
	store   ClsStore
	member  ClsMemberAdapter
	poster  JournalPoster
	alerter OpsAlerter
	clock   func() time.Time
}

// NewClsPvpService wires the service; store is required (fail-closed).
func NewClsPvpService(store ClsStore, opts ClsPvpOptions) (*ClsPvpService, error) {
	if store == nil {
		return nil, fmt.Errorf("cls pvp: nil store")
	}
	clk := opts.Clock
	if clk == nil {
		clk = time.Now
	}
	return &ClsPvpService{
		store:   store,
		member:  opts.Member,
		poster:  opts.Poster,
		alerter: opts.Alerter,
		clock:   clk,
	}, nil
}

func (s *ClsPvpService) raise(ctx context.Context, sev, code, summary string, err error) {
	if s.alerter == nil {
		return
	}
	a := OpsAlert{Severity: sev, Code: code, Summary: summary}
	if err != nil {
		a.Err = err.Error()
	}
	_ = s.alerter.Raise(ctx, a)
}

// ClsEligible exposes pair-level CLS eligibility to the bilateral netting
// engine (Task 24.3.9 excludes CLS-eligible flow — CLS nets internally).
// A missing/ineligible pair returns false; a store error propagates.
func (s *ClsPvpService) ClsEligible(ctx context.Context, buyCcy, sellCcy, product string) (bool, error) {
	ref, found, err := s.store.LoadReference(ctx)
	if err != nil {
		return false, fmt.Errorf("cls: load reference data: %w", err)
	}
	if !found {
		return false, excerrors.New(CodeClsRefDataMissing,
			"cls: no ACTIVE reference version — eligibility cannot be evaluated")
	}
	ok, _ := ref.Eligible(buyCcy, sellCcy, product, "")
	// member check is instruction-scoped; pair-level eligibility checks
	// currencies + product only.
	if !ref.Currencies[buyCcy] || !ref.Currencies[sellCcy] ||
		!ref.Products[strings.ToUpper(product)] {
		ok = false
	}
	return ok && ref.Currencies[buyCcy] && ref.Currencies[sellCcy], nil
}

// ---------------------------------------------------------------------------
// Submit / amend / rescind
// ---------------------------------------------------------------------------

// SubmitInstruction validates + persists a paired PvP instruction and —
// when the member adapter is wired — submits it through the member's ISO
// 20022 interface. Persisted state survives every failure; the caller can
// inspect the returned instruction for the landed status.
//
// Lifecycle: RECEIVED → VALIDATED → (member submit) MATCHED|UNMATCHED.
// Eligibility is evaluated BEFORE member submission; an ineligible
// instruction lands INELIGIBLE with settlement_route set by
// RouteIneligible and never reaches the member.
func (s *ClsPvpService) SubmitInstruction(ctx context.Context, in ClsNewInstruction) (*ClsInstruction, error) {
	if err := s.validateNew(in); err != nil {
		return nil, err
	}
	ref, found, err := s.store.LoadReference(ctx)
	if err != nil {
		return nil, fmt.Errorf("cls: load reference data: %w", err)
	}
	if !found {
		return nil, excerrors.New(CodeClsRefDataMissing,
			"cls: no ACTIVE reference version — refusing instruction")
	}

	instrRef := strings.TrimSpace(in.InstructionRef)
	if instrRef == "" {
		instrRef = swiftRef("CLS", s.clock().UnixNano()%1_000_000_000_000)
	}
	eligible, reasons := ref.Eligible(in.BuyCurrency, in.SellCurrency, in.Product, in.MemberBIC)

	inst := ClsInstruction{
		InstructionRef:        instrRef,
		CounterpartyAccountID: in.CounterpartyAccountID,
		MemberBIC:             strings.ToUpper(in.MemberBIC),
		Product:               strings.ToUpper(in.Product),
		BuyCurrency:           strings.ToUpper(in.BuyCurrency),
		BuyAmount:             in.BuyAmount.Round(8),
		SellCurrency:          strings.ToUpper(in.SellCurrency),
		SellAmount:            in.SellAmount.Round(8),
		ValueDate:             normalizeDay(in.ValueDate),
		TradeID:               in.TradeID,
		Status:                ClsReceived,
		SettlementRoute:       ClsRoutePvp,
		RefVersionID:          &ref.VersionID,
	}

	var id int64
	err = s.store.InTx(ctx, func(ctx context.Context, tx ClsTx) error {
		var err error
		id, err = tx.InsertInstruction(ctx, inst)
		if err != nil {
			return err
		}
		inst.ID = id
		// RECEIVED → VALIDATED (structural validation already passed).
		if err := s.transition(ctx, tx, id, ClsReceived, ClsValidated, "structural validation passed", ""); err != nil {
			return err
		}
		if !eligible {
			return s.transition(ctx, tx, id, ClsValidated, ClsIneligible,
				strings.Join(reasons, "; "), "")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if !eligible {
		inst.Status = ClsIneligible
		inst.ID = id
		// Waterfall routing (alternative PvP → enforceable netting →
		// controlled gross) lands via RouteIneligible; propagate its
		// recorded route on the returned instruction.
		if routed, werr := s.RouteIneligible(ctx, id); werr != nil {
			return &inst, werr
		} else if routed != nil {
			inst.SettlementRoute = routed.SettlementRoute
		}
		return &inst, excerrors.New(CodeClsNotEligible,
			"cls: instruction ineligible — "+strings.Join(reasons, "; "))
	}
	inst.Status = ClsValidated
	inst.ID = id
	return &inst, nil
}

// Dispatch submits a VALIDATED instruction to the CLS member. A nil
// member adapter fails closed — the row stays VALIDATED, never claimed
// dispatched (RailService.Submit discipline).
func (s *ClsPvpService) Dispatch(ctx context.Context, instructionRef string) (*ClsInstruction, error) {
	if s.member == nil {
		return nil, excerrors.New(CodeClsMemberUnavailable,
			"cls: member adapter not wired — instruction remains VALIDATED")
	}
	var out *ClsInstruction
	err := s.store.InTx(ctx, func(ctx context.Context, tx ClsTx) error {
		in, found, err := tx.LockInstruction(ctx, instructionRef, 0)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New(CodeSettlementNotFound,
				fmt.Sprintf("cls instruction %q not found", instructionRef))
		}
		if in.Status != ClsValidated && in.Status != ClsMatched {
			return excerrors.New(CodeClsStateConflict, fmt.Sprintf(
				"cls: instruction %s is %s — dispatch requires VALIDATED", in.InstructionRef, in.Status))
		}
		if in.Status == ClsMatched {
			out = in
			return nil // idempotent re-dispatch — already at member
		}

		payload, uetr := s.renderISO20022(in)
		ack, err := s.member.Send(ctx, ClsMessage{
			Kind:           ClsMsgSubmit,
			InstructionRef: in.InstructionRef,
			MemberBIC:      in.MemberBIC,
			UETR:           uetr,
			XML:            payload,
		})
		if err != nil {
			return fmt.Errorf("cls: member submit: %w", err)
		}
		if err := tx.UpdatePayload(ctx, in.ID, payload, in.PayloadVersion+1, &uetr); err != nil {
			return err
		}
		if err := tx.SetMemberIDs(ctx, in.ID, ack.MemberInstructionID, ack.AckRef); err != nil {
			return err
		}
		to := ClsMatched
		detail := "member acknowledged paired instruction"
		if ack.RejectReason != "" {
			to = ClsRejected
			detail = "member rejected: " + ack.RejectReason
		} else if !ack.Matched {
			to = ClsUnmatched
			detail = "member ack: pair unmatched"
		}
		if err := s.transition(ctx, tx, in.ID, in.Status, to, detail, ack.AckRef); err != nil {
			return err
		}
		if to == ClsUnmatched || to == ClsRejected {
			// Pre-cut-off exception management (spec §17.6 step 3).
			if err := tx.InsertException(ctx, SettlementException{
				Code:             stmtExceptionClsUnmatched,
				ClsInstructionID: &in.ID,
				Currency:         in.SellCurrency,
				ActualAmount:     &in.SellAmount,
				Detail:           detail,
			}); err != nil {
				return err
			}
		}
		// MATCHED → ELIGIBLE gate (reference data already admitted it at
		// submit; the member match confirms the pair).
		if to == ClsMatched {
			if err := s.transition(ctx, tx, in.ID, ClsMatched, ClsEligible,
				"eligibility confirmed on member match", ack.AckRef); err != nil {
				return err
			}
			in.Status = ClsEligible
		} else {
			in.Status = to
		}
		cp := *in
		out = &cp
		return nil
	})
	return out, err
}

// AmendInstruction amends a pre-settlement instruction through the
// member's ISO 20022 amend verb. Amendments are legal from
// VALIDATED/MATCHED/ELIGIBLE/PAY_IN only; SETTLED/terminal rows conflict.
func (s *ClsPvpService) AmendInstruction(ctx context.Context, instructionRef string, am ClsAmend) (*ClsInstruction, error) {
	if s.member == nil {
		return nil, excerrors.New(CodeClsMemberUnavailable,
			"cls: member adapter not wired — amend refused")
	}
	var out *ClsInstruction
	err := s.store.InTx(ctx, func(ctx context.Context, tx ClsTx) error {
		in, found, err := tx.LockInstruction(ctx, instructionRef, 0)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New(CodeSettlementNotFound,
				fmt.Sprintf("cls instruction %q not found", instructionRef))
		}
		switch in.Status {
		case ClsMatched, ClsEligible, ClsPayIn:
		default:
			return excerrors.New(CodeClsStateConflict, fmt.Sprintf(
				"cls: instruction %s is %s — amend requires MATCHED/ELIGIBLE/PAY_IN",
				in.InstructionRef, in.Status))
		}
		if am.BuyAmount != nil {
			if !am.BuyAmount.IsPositive() {
				return excerrors.New(CodeSettlementInvalidFill, "cls: buy amount must be positive")
			}
			in.BuyAmount = am.BuyAmount.Round(8)
		}
		if am.SellAmount != nil {
			if !am.SellAmount.IsPositive() {
				return excerrors.New(CodeSettlementInvalidFill, "cls: sell amount must be positive")
			}
			in.SellAmount = am.SellAmount.Round(8)
		}
		if am.ValueDate != nil {
			in.ValueDate = normalizeDay(*am.ValueDate)
		}
		payload, uetr := s.renderISO20022(in)
		ack, err := s.member.Send(ctx, ClsMessage{
			Kind:           ClsMsgAmend,
			InstructionRef: in.InstructionRef,
			MemberBIC:      in.MemberBIC,
			UETR:           uetr,
			XML:            payload,
		})
		if err != nil {
			return fmt.Errorf("cls: member amend: %w", err)
		}
		if ack.RejectReason != "" {
			return excerrors.New(CodeClsStateConflict,
				"cls: member rejected amend: "+ack.RejectReason)
		}
		if err := tx.UpdatePayload(ctx, in.ID, payload, in.PayloadVersion+1, &uetr); err != nil {
			return err
		}
		if err := s.transition(ctx, tx, in.ID, in.Status, in.Status,
			"amended via member ISO 20022", ack.AckRef); err != nil {
			return err
		}
		cp := *in
		out = &cp
		return nil
	})
	return out, err
}

// RescindInstruction rescinds a live instruction before finality.
func (s *ClsPvpService) RescindInstruction(ctx context.Context, instructionRef, reason string) (*ClsInstruction, error) {
	if s.member == nil {
		return nil, excerrors.New(CodeClsMemberUnavailable,
			"cls: member adapter not wired — rescind refused")
	}
	var out *ClsInstruction
	err := s.store.InTx(ctx, func(ctx context.Context, tx ClsTx) error {
		in, found, err := tx.LockInstruction(ctx, instructionRef, 0)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New(CodeSettlementNotFound,
				fmt.Sprintf("cls instruction %q not found", instructionRef))
		}
		switch in.Status {
		case ClsReceived, ClsValidated, ClsMatched, ClsUnmatched, ClsEligible, ClsIneligible, ClsPayIn:
		default:
			return excerrors.New(CodeClsStateConflict, fmt.Sprintf(
				"cls: instruction %s is %s — cannot rescind", in.InstructionRef, in.Status))
		}
		// Rescind cut-off gate (versioned RESCIND_DEADLINE).
		ref, found2, err := s.store.LoadReference(ctx)
		if err != nil {
			return err
		}
		if found2 {
			if co, ok := ref.Cutoff("RESCIND_DEADLINE"); ok {
				if passed, perr := co.Passed(s.clock()); perr == nil && passed &&
					normalizeDay(s.clock()).Equal(in.ValueDate) {
					return excerrors.New(CodeClsWindowClosed, fmt.Sprintf(
						"cls: rescind deadline %s %s passed for value date %s",
						co.Local, co.Timezone, in.ValueDate.Format("2006-01-02")))
				}
			}
		}
		payload, uetr := s.renderISO20022(in)
		ack, err := s.member.Send(ctx, ClsMessage{
			Kind:           ClsMsgRescind,
			InstructionRef: in.InstructionRef,
			MemberBIC:      in.MemberBIC,
			UETR:           uetr,
			XML:            payload,
		})
		if err != nil {
			return fmt.Errorf("cls: member rescind: %w", err)
		}
		if ack.RejectReason != "" {
			return excerrors.New(CodeClsStateConflict,
				"cls: member rejected rescind: "+ack.RejectReason)
		}
		if err := s.transition(ctx, tx, in.ID, in.Status, ClsRescinded,
			"rescinded: "+reason, ack.AckRef); err != nil {
			return err
		}
		in.Status = ClsRescinded
		cp := *in
		out = &cp
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Member status notifications + finality
// ---------------------------------------------------------------------------

// ApplyMemberStatus processes an inbound member status notification
// (camt/pacs status report). Unmatched/rejected notifications open a
// settlement exception for pre-cut-off exception management.
func (s *ClsPvpService) ApplyMemberStatus(ctx context.Context, instructionRef string,
	to ClsStatus, memberRef, detail string) (*ClsInstruction, error) {

	var out *ClsInstruction
	err := s.store.InTx(ctx, func(ctx context.Context, tx ClsTx) error {
		in, found, err := tx.LockInstruction(ctx, instructionRef, 0)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New(CodeSettlementNotFound,
				fmt.Sprintf("cls instruction %q not found", instructionRef))
		}
		if to == ClsSettled {
			return excerrors.New(CodeClsStateConflict,
				"cls: SETTLED is reachable only via RecordFinality (authenticated finality)")
		}
		if err := s.transition(ctx, tx, in.ID, in.Status, to, detail, memberRef); err != nil {
			return err
		}
		if to == ClsUnmatched || to == ClsRejected {
			code := stmtExceptionClsUnmatched
			if to == ClsRejected {
				code = stmtExceptionClsRejected
			}
			if err := tx.InsertException(ctx, SettlementException{
				Code:             code,
				ClsInstructionID: &in.ID,
				Currency:         in.SellCurrency,
				ActualAmount:     &in.SellAmount,
				Detail:           "member status: " + detail,
			}); err != nil {
				return err
			}
		}
		in.Status = to
		cp := *in
		out = &cp
		return nil
	})
	if err == nil && (to == ClsUnmatched || to == ClsRejected) {
		s.raise(ctx, "P2", string(to), fmt.Sprintf(
			"cls instruction %s → %s: %s", instructionRef, to, detail), nil)
	}
	return out, err
}

// RecordFinality posts GL/nostro final settlement — allowed ONLY from an
// authenticated member finality notification (spec §17.6 step 4). The
// journal discharges both legs atomically:
//
//	pay leg:    Dr 2011_PENDING_SETTLEMENT_DELIVERY_{sellCcy}
//	            Cr 1010_NOSTRO_{sellCcy}               (cash out)
//	receive leg:Dr 1010_NOSTRO_{buyCcy}                (cash in)
//	            Cr 2011_PENDING_SETTLEMENT_DELIVERY_{buyCcy}
//
// Idempotent: an already-SETTLED instruction returns itself.
func (s *ClsPvpService) RecordFinality(ctx context.Context, instructionRef, memberRef string, authenticated bool) (*ClsInstruction, error) {
	if !authenticated {
		return nil, excerrors.New(CodeClsStateConflict,
			"cls: finality requires an authenticated member notification — unauthenticated status cannot settle")
	}
	if s.poster == nil {
		return nil, excerrors.New(CodeClsMemberUnavailable,
			"cls: ledger poster not wired — finality cannot be recorded (fail-closed)")
	}
	var out *ClsInstruction
	var journal *ledger.Journal
	err := s.store.InTx(ctx, func(ctx context.Context, tx ClsTx) error {
		in, found, err := tx.LockInstruction(ctx, instructionRef, 0)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New(CodeSettlementNotFound,
				fmt.Sprintf("cls instruction %q not found", instructionRef))
		}
		if in.Status == ClsSettled {
			cp := *in
			out = &cp
			return nil // idempotent replay
		}
		if in.Status != ClsPayIn && in.Status != ClsEligible {
			return excerrors.New(CodeClsStateConflict, fmt.Sprintf(
				"cls: instruction %s is %s — finality requires PAY_IN", in.InstructionRef, in.Status))
		}
		journal = &ledger.Journal{
			EntryType:      ledger.EntrySettlement,
			ReferenceID:    in.ID,
			Description:    fmt.Sprintf("CLS PvP finality %s (%s/%s)", in.InstructionRef, in.SellCurrency, in.BuyCurrency),
			PostedBy:       "settlement:cls-pvp",
			IdempotencyKey: fmt.Sprintf("cls-finality:%s", in.InstructionRef),
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.PendingSettlementDelivery(in.SellCurrency), in.SellCurrency,
					in.SellAmount, "discharge CLS pay obligation"),
				ledger.CreditLine(ledger.Nostro(in.SellCurrency), in.SellCurrency,
					in.SellAmount, "CLS PvP pay-out to member"),
				ledger.DebitLine(ledger.Nostro(in.BuyCurrency), in.BuyCurrency,
					in.BuyAmount, "CLS PvP pay-in from member"),
				ledger.CreditLine(ledger.PendingSettlementDelivery(in.BuyCurrency), in.BuyCurrency,
					in.BuyAmount, "discharge CLS receive obligation"),
			},
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out != nil {
		return out, nil // replayed
	}

	pr, perr := s.poster.Post(ctx, *journal)
	if perr != nil && !pr.Committed {
		return nil, excerrors.Wrap("INTERNAL_ERROR",
			"cls: finality GL posting failed — instruction remains PAY_IN", perr)
	}
	jid := pr.JournalID
	settledAt := s.clock().UTC()
	err = s.store.InTx(ctx, func(ctx context.Context, tx ClsTx) error {
		in, found, err := tx.LockInstruction(ctx, instructionRef, 0)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New(CodeSettlementNotFound, "cls: instruction vanished mid-finality")
		}
		if in.Status == ClsSettled {
			cp := *in
			out = &cp
			return nil
		}
		if err := s.transition(ctx, tx, in.ID, in.Status, ClsSettled,
			"authenticated member finality "+memberRef, memberRef); err != nil {
			return err
		}
		if err := tx.SetSettled(ctx, in.ID, settledAt, jid); err != nil {
			return err
		}
		in.Status = ClsSettled
		in.SettledAt = &settledAt
		in.GLJournalID = &jid
		cp := *in
		out = &cp
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Settlement-risk waterfall (spec §17.6 step 5)
// ---------------------------------------------------------------------------

// RouteIneligible walks the waterfall for an INELIGIBLE instruction:
// alternative PvP where the reference data offers one → bilateral netting
// where a legally enforceable agreement exists → controlled gross
// settlement bounded by the per-currency principal-risk limit (breach
// opens a PRINCIPAL_RISK_BREACH exception + P1 alert instead of
// settling uncontrolled).
func (s *ClsPvpService) RouteIneligible(ctx context.Context, instructionID int64) (*ClsInstruction, error) {
	var out *ClsInstruction
	var alert *OpsAlert
	err := s.store.InTx(ctx, func(ctx context.Context, tx ClsTx) error {
		in, found, err := tx.LockInstruction(ctx, "", instructionID)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New(CodeSettlementNotFound,
				fmt.Sprintf("cls instruction %d not found", instructionID))
		}
		if in.Status != ClsIneligible {
			return excerrors.New(CodeClsStateConflict, fmt.Sprintf(
				"cls: instruction %d is %s — waterfall applies to INELIGIBLE only", in.ID, in.Status))
		}
		ref, found2, err := s.store.LoadReference(ctx)
		if err != nil {
			return err
		}
		if !found2 {
			return excerrors.New(CodeClsRefDataMissing, "cls: no ACTIVE reference version")
		}

		var route ClsRoute
		switch {
		case ref.AltPvPFor(in.BuyCurrency, in.SellCurrency):
			route = ClsRouteAltPvp
		default:
			netted, nerr := s.store.NettingAgreementExists(ctx, in.CounterpartyAccountID)
			if nerr != nil {
				return fmt.Errorf("cls: netting agreement check: %w", nerr)
			}
			if netted {
				route = ClsRouteNetting
			} else {
				route = ClsRouteControlledGross
			}
		}

		if route == ClsRouteControlledGross {
			// Principal-risk gate: gross settlement is bounded by the
			// versioned per-currency limit. A missing limit fails closed —
			// no unbounded gross settlement.
			lim, ok := ref.Limits[in.SellCurrency]
			if !ok {
				return excerrors.New(CodeClsStateConflict, fmt.Sprintf(
					"cls: no controlled-gross principal limit for %s — refusing gross route", in.SellCurrency))
			}
			if in.SellAmount.GreaterThan(lim.MaxPrincipal) {
				if err := tx.InsertException(ctx, SettlementException{
					Code:             stmtExceptionPrincipalBreach,
					ClsInstructionID: &in.ID,
					Currency:         in.SellCurrency,
					ActualAmount:     &in.SellAmount,
					ExpectedAmount:   &lim.MaxPrincipal,
					Detail: fmt.Sprintf("controlled-gross principal %s exceeds limit %s (max duration %dh)",
						in.SellAmount, lim.MaxPrincipal, lim.MaxDurationH),
				}); err != nil {
					return err
				}
				alert = &OpsAlert{Severity: "P1", Code: stmtExceptionPrincipalBreach,
					Summary: fmt.Sprintf("cls instruction %s principal risk breach: %s %s > limit %s",
						in.InstructionRef, in.SellAmount, in.SellCurrency, lim.MaxPrincipal)}
				in.SettlementRoute = route
				cp := *in
				out = &cp
				// Route stays recorded; the exception holds settlement.
				return tx.SetRoute(ctx, in.ID, route)
			}
		}
		if err := tx.SetRoute(ctx, in.ID, route); err != nil {
			return err
		}
		in.SettlementRoute = route
		cp := *in
		out = &cp
		return nil
	})
	if err == nil && alert != nil {
		s.raise(ctx, alert.Severity, alert.Code, alert.Summary, nil)
	}
	return out, err
}

// ---------------------------------------------------------------------------
// Pay-in + expiry sweep
// ---------------------------------------------------------------------------

// MarkPayIn records pay-in to CLS for an ELIGIBLE instruction, gated on
// the versioned INITIAL_PAY_IN cut-off (after it → CLS_WINDOW_CLOSED).
func (s *ClsPvpService) MarkPayIn(ctx context.Context, instructionRef string) (*ClsInstruction, error) {
	var out *ClsInstruction
	err := s.store.InTx(ctx, func(ctx context.Context, tx ClsTx) error {
		in, found, err := tx.LockInstruction(ctx, instructionRef, 0)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New(CodeSettlementNotFound,
				fmt.Sprintf("cls instruction %q not found", instructionRef))
		}
		if in.Status != ClsEligible {
			return excerrors.New(CodeClsStateConflict, fmt.Sprintf(
				"cls: instruction %s is %s — pay-in requires ELIGIBLE", in.InstructionRef, in.Status))
		}
		ref, found2, err := s.store.LoadReference(ctx)
		if err != nil {
			return err
		}
		if found2 {
			if co, ok := ref.Cutoff("INITIAL_PAY_IN"); ok {
				if passed, perr := co.Passed(s.clock()); perr == nil && passed &&
					normalizeDay(s.clock()).Equal(in.ValueDate) {
					return excerrors.New(CodeClsWindowClosed, fmt.Sprintf(
						"cls: initial pay-in window %s %s closed for value date %s",
						co.Local, co.Timezone, in.ValueDate.Format("2006-01-02")))
				}
			}
		}
		if err := s.transition(ctx, tx, in.ID, in.Status, ClsPayIn, "pay-in recorded", ""); err != nil {
			return err
		}
		in.Status = ClsPayIn
		cp := *in
		out = &cp
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (s *ClsPvpService) validateNew(in ClsNewInstruction) error {
	if in.CounterpartyAccountID <= 0 {
		return excerrors.New(CodeSettlementInvalidFill, "cls: counterparty account required")
	}
	if !bicRe.MatchString(strings.ToUpper(in.MemberBIC)) {
		return excerrors.New(CodeSettlementInvalidFill, "cls: member BIC malformed")
	}
	for _, ccy := range []string{strings.ToUpper(in.BuyCurrency), strings.ToUpper(in.SellCurrency)} {
		if !currencyRe.MatchString(ccy) {
			return excerrors.New(CodeSettlementInvalidFill, fmt.Sprintf("cls: bad currency %q", ccy))
		}
	}
	if strings.EqualFold(in.BuyCurrency, in.SellCurrency) {
		return excerrors.New(CodeSettlementInvalidFill, "cls: degenerate same-currency pair")
	}
	if !in.BuyAmount.IsPositive() || !in.SellAmount.IsPositive() ||
		!in.BuyAmount.Round(8).Equal(in.BuyAmount) || !in.SellAmount.Round(8).Equal(in.SellAmount) {
		return excerrors.New(CodeSettlementInvalidFill,
			"cls: amounts must be positive ≤8dp values")
	}
	if in.ValueDate.IsZero() {
		return excerrors.New(CodeSettlementInvalidFill, "cls: value date required")
	}
	return nil
}

// transition applies one lifecycle edge + appends the event row.
func (s *ClsPvpService) transition(ctx context.Context, tx ClsTx, id int64,
	from, to ClsStatus, detail, memberRef string) error {
	if from != to {
		if !clsTransitions[from][to] {
			return excerrors.New(CodeClsStateConflict, fmt.Sprintf(
				"cls: illegal transition %s → %s", from, to))
		}
	}
	return tx.Transition(ctx, id, from, to, ClsStatusEvent{
		InstructionID: id, From: from, To: to, Detail: detail, MemberRef: memberRef,
	})
}

// renderISO20022 renders the paired-instruction XML body (pacs.009-style
// both-legs descriptor). The member adapter transports this verbatim.
func (s *ClsPvpService) renderISO20022(in *ClsInstruction) (payload, uetr string) {
	uetr = clsUETR(in.InstructionRef)
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.009.001.08">`)
	sb.WriteString(`<FIToFICdtTrf><GrpHdr>`)
	fmt.Fprintf(&sb, `<MsgId>%s</MsgId>`, xmlEscape(in.InstructionRef))
	fmt.Fprintf(&sb, `<CreDtTm>%s</CreDtTm>`, s.clock().UTC().Format(time.RFC3339))
	sb.WriteString(`</GrpHdr><CdtTrfTxInf><PmtId>`)
	fmt.Fprintf(&sb, `<InstrId>%s</InstrId><EndToEndId>%s</EndToEndId><UETR>%s</UETR>`,
		xmlEscape(in.InstructionRef), xmlEscape(in.InstructionRef), uetr)
	sb.WriteString(`</PmtId><SttlmPrty>CLSS</SttlmPrty>`)
	fmt.Fprintf(&sb, `<SellLeg><Amt Ccy="%s">%s</Amt></SellLeg>`,
		in.SellCurrency, in.SellAmount.String())
	fmt.Fprintf(&sb, `<BuyLeg><Amt Ccy="%s">%s</Amt></BuyLeg>`,
		in.BuyCurrency, in.BuyAmount.String())
	fmt.Fprintf(&sb, `<IntrBkSttlmDt>%s</IntrBkSttlmDt>`, in.ValueDate.Format("2006-01-02"))
	fmt.Fprintf(&sb, `<MmbId><FinInstnId><BICFI>%s</BICFI></FinInstnId></MmbId>`, bic11(in.MemberBIC))
	sb.WriteString(`</CdtTrfTxInf></FIToFICdtTrf></Document>`)
	return sb.String(), uetr
}

// clsUETR derives a stable RFC-4122-shaped UETR from the instruction ref
// (deterministic — the same instruction always carries the same UETR for
// downstream statement matching).
func clsUETR(ref string) string {
	h := uint64(0)
	for _, b := range []byte(ref) {
		h = h*1099511628211 + uint64(b)
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(h>>32), uint16(h>>16)&0xffff, uint16(h)&0xffff,
		uint16(h>>48)&0xffff|0x4000, h&0xffffffffffff)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
// PgxClsStore
// ---------------------------------------------------------------------------

// PgxClsStore implements ClsStore/ClsTx over pgx.
type PgxClsStore struct{ Pool *pgxpool.Pool }

// NewPgxClsStore wires the store.
func NewPgxClsStore(pool *pgxpool.Pool) *PgxClsStore { return &PgxClsStore{Pool: pool} }

// LoadReference assembles the ACTIVE version into ClsRefData.
func (s *PgxClsStore) LoadReference(ctx context.Context) (*ClsRefData, bool, error) {
	var versionID int64
	var version string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, version FROM cls_reference_versions
		 WHERE status = 'ACTIVE' ORDER BY id DESC LIMIT 1`).Scan(&versionID, &version)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT entry_type, entry_key, payload FROM cls_reference_entries
		 WHERE version_id = $1`, versionID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	ref := &ClsRefData{
		VersionID: versionID, Version: version,
		Currencies: map[string]bool{}, Products: map[string]bool{},
		Members: map[string]bool{}, Cutoffs: map[string]ClsCutoff{},
		AltPvP: map[string]bool{}, Limits: map[string]ClsPrincipalLimit{},
	}
	for rows.Next() {
		var typ, key string
		var payload []byte
		if err := rows.Scan(&typ, &key, &payload); err != nil {
			return nil, false, err
		}
		if err := applyClsRefEntry(ref, typ, key, payload); err != nil {
			return nil, false, err // malformed reference data fails the load
		}
	}
	return ref, true, rows.Err()
}

// applyClsRefEntry folds one cls_reference_entries row into the ACTIVE
// reference set. Malformed payloads fail the whole load — a partially
// parsed reference set never gates money (spec §2.7).
func applyClsRefEntry(ref *ClsRefData, typ, key string, payload []byte) error {
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return fmt.Errorf("cls ref entry type %s has empty key", typ)
	}
	switch strings.ToUpper(typ) {
	case "CURRENCY":
		ref.Currencies[key] = true
	case "PRODUCT":
		ref.Products[key] = true
	case "MEMBER":
		if !bicRe.MatchString(key) {
			return fmt.Errorf("cls ref member %q is not a valid BIC", key)
		}
		ref.Members[key] = true
	case "ALT_PVP":
		// pair key "CCY1/CCY2"
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 || !currencyRe.MatchString(parts[0]) || !currencyRe.MatchString(parts[1]) {
			return fmt.Errorf("cls ref alt-pvp key %q malformed", key)
		}
		ref.AltPvP[key] = true
	case "CUTOFF":
		var p struct {
			Local    string `json:"local"`    // "HH:MM"
			Timezone string `json:"timezone"` // IANA
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("cls ref cutoff %s payload: %w", key, err)
		}
		loc, err := time.LoadLocation(p.Timezone)
		if err != nil {
			return fmt.Errorf("cls ref cutoff %s timezone %q: %w", key, p.Timezone, err)
		}
		var hh, mm int
		if n, _ := fmt.Sscanf(p.Local, "%d:%d", &hh, &mm); n != 2 || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
			return fmt.Errorf("cls ref cutoff %s bad local %q", key, p.Local)
		}
		ref.Cutoffs[key] = ClsCutoff{
			Name: key, Local: fmt.Sprintf("%02d:%02d", hh, mm),
			Timezone: p.Timezone, loc: loc, cutoffMin: hh*60 + mm,
		}
	case "PRINCIPAL_LIMIT":
		var p struct {
			MaxPrincipal string `json:"max_principal"`
			MaxDurationH int    `json:"max_duration_hours"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("cls ref limit %s payload: %w", key, err)
		}
		lim, err := decimal.NewFromString(p.MaxPrincipal)
		if err != nil || !lim.IsPositive() {
			return fmt.Errorf("cls ref limit %s bad max_principal %q", key, p.MaxPrincipal)
		}
		ref.Limits[key] = ClsPrincipalLimit{
			Currency: key, MaxPrincipal: lim, MaxDurationH: p.MaxDurationH,
		}
	default:
		return fmt.Errorf("cls ref entry type %q unknown", typ)
	}
	return nil
}

// NettingAgreementExists checks legal_agreements for an ACTIVE ISDA row.
func (s *PgxClsStore) NettingAgreementExists(ctx context.Context, accountID int64) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM legal_agreements
		     WHERE account_id = $1 AND agreement_type = 'ISDA'
		       AND status = 'EXECUTED'
		       AND (expires_at IS NULL OR expires_at > now()))`, accountID).Scan(&ok)
	return ok, err
}

// InTx runs fn inside a SERIALIZABLE transaction.
func (s *PgxClsStore) InTx(ctx context.Context, fn func(ctx context.Context, tx ClsTx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("cls tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxClsTx{tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("cls tx commit: %w", err)
	}
	return nil
}

const clsSelect = `
	SELECT id, instruction_ref, counterparty_account_id, member_bic, product,
	       buy_currency, buy_amount::text, sell_currency, sell_amount::text,
	       value_date, trade_id, status::text, settlement_route::text,
	       member_instruction_id, member_ack_ref, uetr, message_payload,
	       payload_version, reject_reason, ref_version_id, gl_journal_id,
	       created_at, settled_at
	  FROM cls_settlement_instructions`

func scanCls(row pgx.Row) (*ClsInstruction, bool, error) {
	var in ClsInstruction
	var status, route string
	var buy, sell string
	err := row.Scan(&in.ID, &in.InstructionRef, &in.CounterpartyAccountID,
		&in.MemberBIC, &in.Product, &in.BuyCurrency, &buy, &in.SellCurrency,
		&sell, &in.ValueDate, &in.TradeID, &status, &route,
		&in.MemberInstructionID, &in.MemberAckRef, &in.UETR, &in.MessagePayload,
		&in.PayloadVersion, &in.RejectReason, &in.RefVersionID, &in.GLJournalID,
		&in.CreatedAt, &in.SettledAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	in.Status, in.SettlementRoute = ClsStatus(status), ClsRoute(route)
	if in.BuyAmount, err = decimal.NewFromString(buy); err != nil {
		return nil, false, fmt.Errorf("cls instruction %d buy amount: %w", in.ID, err)
	}
	if in.SellAmount, err = decimal.NewFromString(sell); err != nil {
		return nil, false, fmt.Errorf("cls instruction %d sell amount: %w", in.ID, err)
	}
	return &in, true, nil
}

type pgxClsTx struct{ tx pgx.Tx }

func (t pgxClsTx) InsertInstruction(ctx context.Context, in ClsInstruction) (int64, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `
		INSERT INTO cls_settlement_instructions
		    (instruction_ref, counterparty_account_id, member_bic, product,
		     buy_currency, buy_amount, sell_currency, sell_amount,
		     value_date, trade_id, status, settlement_route, ref_version_id)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8::numeric,$9,$10,$11,$12,$13)
		RETURNING id`,
		in.InstructionRef, in.CounterpartyAccountID, in.MemberBIC, in.Product,
		in.BuyCurrency, in.BuyAmount.String(), in.SellCurrency, in.SellAmount.String(),
		in.ValueDate, in.TradeID, string(in.Status), string(in.SettlementRoute),
		in.RefVersionID).Scan(&id)
	return id, err
}

func (t pgxClsTx) LockInstruction(ctx context.Context, ref string, id int64) (*ClsInstruction, bool, error) {
	if ref != "" {
		row := t.tx.QueryRow(ctx, clsSelect+` WHERE instruction_ref = $1 FOR UPDATE`, ref)
		return scanCls(row)
	}
	row := t.tx.QueryRow(ctx, clsSelect+` WHERE id = $1 FOR UPDATE`, id)
	return scanCls(row)
}

func (t pgxClsTx) Transition(ctx context.Context, id int64, from, to ClsStatus, e ClsStatusEvent) error {
	if from != to {
		tag, err := t.tx.Exec(ctx, `
			UPDATE cls_settlement_instructions
			   SET status = $2, updated_at = now()
			 WHERE id = $1 AND status = $3`, id, string(to), string(from))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return excerrors.New(CodeClsStateConflict, fmt.Sprintf(
				"cls: instruction %d lost status %s mid-transition", id, from))
		}
	}
	_, err := t.tx.Exec(ctx, `
		INSERT INTO cls_instruction_events
		    (instruction_id, from_status, to_status, authenticated, member_ref, detail)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''))`,
		id, nullClsStatus(from), string(to), e.Authenticated, e.MemberRef, e.Detail)
	return err
}

func nullClsStatus(s ClsStatus) any {
	if s == "" {
		return nil
	}
	return string(s)
}

func (t pgxClsTx) UpdatePayload(ctx context.Context, id int64, payload string, version int, uetr *string) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE cls_settlement_instructions
		   SET message_payload = $2, payload_version = $3,
		       uetr = COALESCE($4, uetr), updated_at = now()
		 WHERE id = $1`, id, payload, version, uetr)
	return err
}

func (t pgxClsTx) SetMemberIDs(ctx context.Context, id int64, memberInstrID, ackRef string) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE cls_settlement_instructions
		   SET member_instruction_id = NULLIF($2,''), member_ack_ref = NULLIF($3,''),
		       updated_at = now()
		 WHERE id = $1`, id, memberInstrID, ackRef)
	return err
}

func (t pgxClsTx) SetSettled(ctx context.Context, id int64, at time.Time, journalID int64) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE cls_settlement_instructions
		   SET settled_at = $2, gl_journal_id = $3, updated_at = now()
		 WHERE id = $1`, id, at, journalID)
	return err
}

func (t pgxClsTx) SetRoute(ctx context.Context, id int64, route ClsRoute) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE cls_settlement_instructions
		   SET settlement_route = $2, updated_at = now()
		 WHERE id = $1`, id, string(route))
	return err
}

func (t pgxClsTx) InsertException(ctx context.Context, e SettlementException) error {
	_, err := insertSettlementException(ctx, t.tx, e)
	return err
}
