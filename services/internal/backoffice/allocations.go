// Package backoffice implements the Phase-24 settlement-side backoffice
// domain. This file is Task 24.3.10 — bunched orders, average price and
// the post-trade allocation lifecycle (spec §5.31 schema, §17.8 workflow,
// §24 #172).
//
// Model: an account manager registers an average_price_group BEFORE order
// entry — the manager's omnibus account, the instrument + side, a declared
// capacity (CLIENT | PROPRIETARY — the two never mix in one group), the
// eligible beneficial accounts (with optional preset weights and
// PartyID/LEI identifiers), and the pre-declared deterministic allocation
// method (MANUAL | PRO_RATA | RULE_BASED | EQUAL_SPLIT). After execution
// the manager attaches the group's fills (trades.id refs); the engine
// computes the volume-weighted average price and books beneficiary
// allocations under the stored method. The hard invariant — allocated
// quantity may never exceed filled quantity — is enforced in the engine
// and by the trade_allocations_conservation_chk trigger (migration 055).
//
// Lifecycle: trade_allocations move PENDING → ALLOCATED → CLAIMED /
// REJECTED (fund-ops claim, spec §5.31), CANCELLED pre-settlement, and
// CORRECTED via offset/replacement record pairs. Claim, reject, cancel
// and correct all propagate the leg's PartyID/LEI/account identifiers to
// confirmations, PB give-up, settlement and regulatory reporting through
// the AllocationEvent sink seam. A group locks (settlement_locked /
// SETTLEMENT_LOCKED) once submitted to settlement; post-submission
// corrections require dual control (a distinct, role-eligible second
// principal) plus immutable before/after evidence in
// trade_allocation_events.
//
// Fail-closed (spec §2.7): a nil store, unresolvable fills, unknown
// methods, ineligible beneficiaries, or any conservation breach are coded
// errors — never silent skips.
package backoffice

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"exchange/internal/errs"
	"exchange/internal/ledger"
	"exchange/internal/pamm"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Error codes.
//
// ALLOCATION_INVALID and ALLOCATION_SUM_MISMATCH are spec §23 rows (owned
// by Phase-18 Task 18.3.13). The remaining codes are gateway-local and are
// registered into the errs registry by the api package's allocation
// handlers (Phase-24 Task 24.3.10/.15 owners).
// ---------------------------------------------------------------------------

const (
	// CodeAllocationInvalid — structural rejection: unknown method,
	// non-positive quantity/weight, unresolved fill, bad side, amend
	// against a locked/non-amendable row (spec §23, 400).
	CodeAllocationInvalid = "ALLOCATION_INVALID"
	// CodeAllocationSumMismatch — conservation breach: allocated legs
	// would exceed (or, where exactness is required, diverge from) the
	// filled quantity (spec §23, 400).
	CodeAllocationSumMismatch = "ALLOCATION_SUM_MISMATCH"
	// CodeAllocationNotFound — group/allocation id not present (404).
	CodeAllocationNotFound = "ALLOCATION_NOT_FOUND"
	// CodeAllocationStateConflict — lifecycle transition refused in the
	// current state (409).
	CodeAllocationStateConflict = "ALLOCATION_STATE_CONFLICT"
	// CodeAllocationIneligible — beneficiary not on the group's eligible
	// registry (or marked INELIGIBLE after execution) (422).
	CodeAllocationIneligible = "ALLOCATION_INELIGIBLE"
	// CodeAllocationCapacityMix — proprietary and client interest on one
	// group (422). The DB trigger raises the same token.
	CodeAllocationCapacityMix = "ALLOCATION_CAPACITY_MIX"
	// CodeAllocationLocked — mutation attempted on a settlement-locked
	// group without the dual-control path (423).
	CodeAllocationLocked = "ALLOCATION_LOCKED"
	// CodeUnallocatedBlockTrade — internal P2 compliance alert raised by
	// the T+0 EOD sweep (Task 24.3.15); not an HTTP surface.
	CodeUnallocatedBlockTrade = "UNALLOCATED_BLOCK_TRADE"
)

func allocErr(code, format string, args ...any) *excerrors.Error {
	return excerrors.New(code, fmt.Sprintf(format, args...))
}

// Register the task-local codes on errs.Default — same localRow branch the
// rest of the package uses (exceptions.go); each row cites its owning
// Phase-24 task. UNALLOCATED_BLOCK_TRADE is a durable ops-alert payload
// code (P2), never an HTTP surface — the 500 registration follows the
// NOSTRO_OVERDRAWN convention.
func init() {
	for _, d := range []errs.CodeDef{
		{Code: CodeAllocationNotFound, HTTPStatus: 404, Owner: "Phase-24 Task 24.3.10",
			Description: "Average-price group or trade allocation id does not resolve (spec §5.31)"},
		{Code: CodeAllocationStateConflict, HTTPStatus: 409, Owner: "Phase-24 Task 24.3.10",
			Description: "Allocation lifecycle transition refused in the current state — claim/reject/cancel/correct against a terminal, locked or non-amendable row (spec §5.31, §17.8)"},
		{Code: CodeAllocationIneligible, HTTPStatus: 422, Owner: "Phase-24 Task 24.3.10",
			Description: "Beneficiary account is not on the group's pre-registered eligibility set (or was marked INELIGIBLE after execution) — allocation refused (spec §5.31, §17.8)"},
		{Code: CodeAllocationCapacityMix, HTTPStatus: 422, Owner: "Phase-24 Task 24.3.10",
			Description: "Proprietary and client capacity on one average-price group — blocked at registration and by the apg_account_capacity_chk trigger (spec §17.8)"},
		{Code: CodeAllocationLocked, HTTPStatus: 423, Owner: "Phase-24 Task 24.3.10",
			Description: "Group is settlement-locked — mutations require the dual-control correction path (spec §5.31, §8.2)"},
		{Code: CodeUnallocatedBlockTrade, HTTPStatus: 500, Owner: "Phase-24 Task 24.3.15",
			Description: "Block trade still unallocated at the T+0 EOD deadline — durable P2 compliance alert (spec §17.8; ops-alert payload code, never an HTTP response)"},
	} {
		errs.Default.MustRegister(d)
	}
}

// ---------------------------------------------------------------------------
// Vocabulary (migration 055 enums).
// ---------------------------------------------------------------------------

// Method is the pre-declared deterministic allocation method.
type Method string

const (
	MethodManual     Method = "MANUAL"      // legs carry explicit quantities
	MethodProRata    Method = "PRO_RATA"    // weights submitted with the instruction
	MethodRuleBased  Method = "RULE_BASED"  // preset split weights on the eligibility registry
	MethodEqualSplit Method = "EQUAL_SPLIT" // equal split across eligible accounts
)

// ParseMethod validates a wire method name (fail-closed on unknown).
func ParseMethod(s string) (Method, error) {
	switch Method(s) {
	case MethodManual, MethodProRata, MethodRuleBased, MethodEqualSplit:
		return Method(s), nil
	}
	return "", allocErr(CodeAllocationInvalid, "unknown allocation method %q", s)
}

// Capacity is the proprietary/client classification of a group and each
// of its eligible accounts. The two may never share a group (Task
// 24.3.10 step 1 — enforced in code AND by apg_account_capacity_chk).
type Capacity string

const (
	CapacityClient      Capacity = "CLIENT"
	CapacityProprietary Capacity = "PROPRIETARY"
)

// ParseCapacity validates a wire capacity name.
func ParseCapacity(s string) (Capacity, error) {
	switch Capacity(s) {
	case CapacityClient, CapacityProprietary:
		return Capacity(s), nil
	}
	return "", allocErr(CodeAllocationInvalid, "unknown capacity %q", s)
}

// GroupStatus mirrors avg_group_status_enum.
type GroupStatus string

const (
	GroupOpen      GroupStatus = "OPEN"              // fills may attach; allocations not yet run
	GroupAllocated GroupStatus = "ALLOCATED"         // allocation run committed
	GroupLocked    GroupStatus = "SETTLEMENT_LOCKED" // submitted to settlement — corrections need dual control
	GroupClosed    GroupStatus = "CLOSED"            // all allocations terminal
	GroupCancelled GroupStatus = "CANCELLED"         // withdrawn pre-settlement
)

// Status mirrors trade_alloc_status_enum (spec §5.31 lifecycle plus the
// correction/settlement terminals).
type Status string

const (
	StatusPending   Status = "PENDING"   // booked by an intake, not yet allocated to beneficiaries
	StatusAllocated Status = "ALLOCATED" // booked to a beneficiary
	StatusClaimed   Status = "CLAIMED"   // fund-ops claimed/confirmed (→ settlement instruction)
	StatusRejected  Status = "REJECTED"  // fund-ops rejected
	StatusCancelled Status = "CANCELLED" // withdrawn pre-settlement
	StatusCorrected Status = "CORRECTED" // superseded by an offset/replacement pair
	StatusSettled   Status = "SETTLED"   // child settlement instruction SETTLED
)

// Active reports whether the status still counts toward the fill's
// allocated quantity (mirrors the conservation trigger's domain).
func (s Status) Active() bool {
	switch s {
	case StatusPending, StatusAllocated, StatusClaimed, StatusSettled:
		return true
	}
	return false
}

// Kind mirrors trade_alloc_kind_enum — corrections are never edits: an
// OFFSET row retires the original and a REPLACEMENT row carries the
// corrected booking.
type Kind string

const (
	KindPrimary     Kind = "PRIMARY"
	KindOffset      Kind = "OFFSET"
	KindReplacement Kind = "REPLACEMENT"
)

// Source records the intake channel (alloc_source_enum).
type Source string

const (
	SourceREST Source = "REST"
	SourceFIX  Source = "FIX"
)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// EligibleAccount is one pre-registered beneficial account on a group.
type EligibleAccount struct {
	AccountID int64           `json:"account_id"`
	Capacity  Capacity        `json:"capacity"`
	Weight    decimal.Decimal `json:"weight,omitempty"` // RULE_BASED/PRO_RATA preset
	PartyID   string          `json:"party_id,omitempty"`
	PartyLEI  string          `json:"party_lei,omitempty"`
	Status    string          `json:"status"` // ELIGIBLE | INELIGIBLE
}

// Group is one average_price_groups row.
type Group struct {
	ID               int64           `json:"id"`
	GroupRef         string          `json:"group_ref"`
	ManagerAccountID int64           `json:"manager_account_id"`
	InstrumentID     int64           `json:"instrument_id"`
	Side             byte            `json:"side"` // FIX 54: '1' buy, '2' sell
	Capacity         Capacity        `json:"capacity"`
	Method           Method          `json:"allocation_method"`
	Status           GroupStatus     `json:"status"`
	TotalQty         decimal.Decimal `json:"total_qty"`     // grouped filled qty
	AllocatedQty     decimal.Decimal `json:"allocated_qty"` // Σ active allocations
	AvgPrice         decimal.Decimal `json:"avg_price"`
	PriceResidual    decimal.Decimal `json:"price_residual"`
	SettlementLocked bool            `json:"settlement_locked"`
	Source           Source          `json:"source"`
	FixAllocationID  int64           `json:"fix_allocation_id,omitempty"`
	EscalatedAt      *time.Time      `json:"escalated_at,omitempty"`
	CreatedBy        int64           `json:"created_by"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	SubmittedAt      *time.Time      `json:"submitted_at,omitempty"`
}

// UnallocatedQty is the filled quantity not yet booked to beneficiaries
// (deterministic remainder — stays on the manager omnibus account).
func (g Group) UnallocatedQty() decimal.Decimal {
	return g.TotalQty.Sub(g.AllocatedQty)
}

// Fill is one average_price_group_fills row — a grouped source fill.
type Fill struct {
	ID             int64           `json:"id"`
	GroupID        int64           `json:"group_id"`
	TradeID        int64           `json:"trade_id"`
	AccountID      int64           `json:"account_id"` // manager omnibus side
	Quantity       decimal.Decimal `json:"quantity"`
	Price          decimal.Decimal `json:"price"`
	SettlementDate *time.Time      `json:"settlement_date,omitempty"`
}

// Trade is the engine-side view of a trades row (fills source).
type Trade struct {
	ID              int64
	InstrumentID    int64
	Symbol          string
	BaseCurrency    string
	QuoteCurrency   string
	SettlementCycle int
	BuyerAccountID  int64
	SellerAccountID int64
	Price           decimal.Decimal
	Quantity        decimal.Decimal
	SettlementDate  *time.Time
	Status          string // COMPLETED | BUSTED | PRICE_ADJUSTED
	CreatedAt       time.Time
}

// Allocation is one trade_allocations row.
type Allocation struct {
	ID                       int64           `json:"id"`
	TradeID                  int64           `json:"trade_id"`
	GroupID                  int64           `json:"group_id"`
	BeneficiaryAccountID     int64           `json:"beneficiary_account_id"`
	Quantity                 decimal.Decimal `json:"quantity"`
	AvgPrice                 decimal.Decimal `json:"avg_price"`
	Status                   Status          `json:"status"`
	Kind                     Kind            `json:"kind"`
	CorrectsAllocationID     *int64          `json:"corrects_allocation_id,omitempty"`
	PartyID                  string          `json:"party_id,omitempty"`
	PartyLEI                 string          `json:"party_lei,omitempty"`
	AllocAccountRef          string          `json:"alloc_account_ref,omitempty"`
	ConfirmationRef          string          `json:"confirmation_ref,omitempty"`
	SettlementInstructionIDs []int64         `json:"settlement_instruction_ids"`
	ClaimedAt                *time.Time      `json:"claimed_at,omitempty"`
	RejectedReason           string          `json:"rejected_reason,omitempty"`
	CreatedAt                time.Time       `json:"created_at"`
	UpdatedAt                time.Time       `json:"updated_at"`
}

// Event is one trade_allocation_events row — the immutable before/after
// audit record (spec §5.31 allocation audit history).
type Event struct {
	ID           int64           `json:"id"`
	GroupID      int64           `json:"group_id"`
	AllocationID *int64          `json:"allocation_id,omitempty"`
	Seq          int             `json:"seq"`
	Type         string          `json:"event_type"`
	Before       json.RawMessage `json:"before_state,omitempty"`
	After        json.RawMessage `json:"after_state"`
	Actor        string          `json:"actor"`
	ApprovedBy   *int64          `json:"approved_by,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Audit event types (emitted through the lifecycle; reporting consumers
// key off these).
const (
	EvGroupCreated          = "GROUP_CREATED"
	EvEligibleRegistered    = "ELIGIBLE_REGISTERED"
	EvFillAttached          = "FILL_ATTACHED"
	EvAveragePriced         = "AVERAGE_PRICED"
	EvAllocated             = "ALLOCATED"
	EvClaimed               = "CLAIMED"
	EvRejected              = "REJECTED"
	EvCancelled             = "CANCELLED"
	EvSettlementLocked      = "SETTLEMENT_LOCKED"
	EvCorrected             = "CORRECTED"
	EvSettlementInstruction = "SETTLEMENT_INSTRUCTION"
	EvReportEmitted         = "ALLOCATION_REPORT_EMITTED" // 35=AK per fund
	EvEscalated             = "UNALLOCATED_ESCALATED"     // T+0 EOD P2 alert
	EvFixIngested           = "FIX_INGESTED"              // 35=J sink
	EvAccountIneligible     = "ACCOUNT_INELIGIBLE"
)

// Actor is the principal performing an allocation action; ApproverID
// carries the second (four-eyes) principal on dual-controlled
// corrections.
type Actor struct {
	UserID     int64
	ApproverID int64
	ClientIP   string
}

// RoleResolver is shared with client_money.go (Task 24.3.11) — declared
// there once for the whole package; it resolves an admin user id to its
// spec §8.2 role name and a nil resolver fails closed.

// correctionRoles may initiate or approve a post-submission correction —
// Finance Ops own settlement ops; Risk Manager and Super Admin cover the
// break-glass path (§8.2 role semantics).
var correctionRoles = map[string]bool{
	"Risk Manager": true,
	"Finance Ops":  true,
	"Super Admin":  true,
}

// ---------------------------------------------------------------------------
// Persistence seam
// ---------------------------------------------------------------------------

// AllocationStore is the unit-of-work seam — scoped to Tasks 24.3.10/.15;
// the package-wide Store/Tx union lives in client_money.go (Task 24.3.11),
// so this task pair carries its own names. PgAllocationStore
// (allocation_engine.go) implements it over pgx; InTx runs fn inside one
// SERIALIZABLE transaction and rolls back on error.
type AllocationStore interface {
	// InTx runs fn inside one SERIALIZABLE transaction.
	InTx(ctx context.Context, fn func(ctx context.Context, tx AllocationTx) error) error

	// Read surface (non-transactional).
	Group(ctx context.Context, id int64) (*Group, error)
	GroupByRef(ctx context.Context, ref string) (*Group, error)
	Fills(ctx context.Context, groupID int64) ([]Fill, error)
	Allocations(ctx context.Context, groupID int64) ([]Allocation, error)
	EligibleAccounts(ctx context.Context, groupID int64) ([]EligibleAccount, error)
	Events(ctx context.Context, groupID int64) ([]Event, error)
}

// AllocationTx is the transactional view inside AllocationStore.InTx.
type AllocationTx interface {
	// Groups.
	InsertGroup(ctx context.Context, g *Group) error
	LockGroup(ctx context.Context, id int64) (*Group, error) // FOR UPDATE
	LockGroupByRef(ctx context.Context, ref string) (*Group, error)
	UpdateGroupComputed(ctx context.Context, id int64, totalQty, avgPrice, residual decimal.Decimal) error
	SetGroupStatus(ctx context.Context, id int64, status GroupStatus, locked bool) error
	MarkGroupEscalated(ctx context.Context, id int64, at time.Time) error
	// SetGroupFixRef records the originating fix_allocations.id on a
	// FIX-ingested group (idempotent — same value re-set is a no-op).
	SetGroupFixRef(ctx context.Context, id, fixAllocID int64) error

	// Eligibility registry.
	InsertEligible(ctx context.Context, groupID int64, e EligibleAccount) error
	EligibleAccountsTx(ctx context.Context, groupID int64) ([]EligibleAccount, error)
	SetEligibleStatus(ctx context.Context, groupID, accountID int64, status string) error

	// Fills.
	LoadTrade(ctx context.Context, tradeID int64) (*Trade, error)
	FillGroup(ctx context.Context, tradeID int64) (groupID int64, attached bool, err error)
	InsertFill(ctx context.Context, groupID int64, side byte, t *Trade) error
	GroupFillsTx(ctx context.Context, groupID int64) ([]Fill, error)

	// Allocations.
	InsertAllocation(ctx context.Context, a *Allocation) error
	LockAllocation(ctx context.Context, id int64) (*Allocation, error)
	AllocationsTx(ctx context.Context, groupID int64) ([]Allocation, error)
	ActiveFillQty(ctx context.Context, groupID, tradeID int64) (decimal.Decimal, error)
	UpdateAllocationStatus(ctx context.Context, id int64, status Status, claimedAt *time.Time, reason, confRef string) error
	AttachSettlementLegs(ctx context.Context, id int64, instructionIDs []int64) error
	VoidAllocationInstructions(ctx context.Context, allocID int64, ref string) (int, error)
	// AllocationLegCounts reports how many of the allocation's settlement
	// instructions are settled (SETTLED/RECONCILED) vs still pending —
	// amendment-after-partial-settlement gates on it.
	AllocationLegCounts(ctx context.Context, allocID int64) (settled, pending int, err error)

	// Hierarchy + settlement plumbing (engine paths).
	AccountInHierarchy(ctx context.Context, masterID, accountID int64) (bool, error)
	ActiveNostro(ctx context.Context, currency string) (int64, error)
	InsertSettlementLeg(ctx context.Context, leg SettlementLeg) (id int64, inserted bool, err error)

	// GL — posts a balanced journal inside this transaction through the
	// §5.3 write path (no GL bypass). The pgx implementation delegates to
	// the LedgerService.PostJournal contract.
	PostJournal(ctx context.Context, j ledger.Journal) (journalID int64, err error)

	// Audit + escalation.
	AppendEvent(ctx context.Context, groupID int64, allocID *int64, typ string,
		before, after any, actor string, approvedBy *int64) error
	UnallocatedGroups(ctx context.Context, cutoff time.Time) ([]Group, error)
	// InsertAlert lands a durable funding_ops_alerts row (shared Phase-24
	// alert trail — OpsAlertRow is declared in nostro.go).
	InsertAlert(ctx context.Context, a OpsAlertRow) (int64, error)
}

// SettlementLeg is one settlement_instructions row written for a
// confirmed allocation (reuses the Phase-03 Task 3.3.3 table; settlement
// date is inherited from the parent trade).
type SettlementLeg struct {
	TradeID         int64
	AccountID       int64
	Currency        string
	Amount          decimal.Decimal
	Direction       string // PAY | RECEIVE (settlement_direction_enum)
	SettlementDate  time.Time
	NostroAccountID *int64
}

// ---------------------------------------------------------------------------
// Pure computation — deterministic, decimal-exact (no float64 ever).
// ---------------------------------------------------------------------------

// FillQty is a fill with its still-unallocated remainder.
type FillQty struct {
	TradeID   int64
	Quantity  decimal.Decimal
	Price     decimal.Decimal
	Remaining decimal.Decimal
}

// VWAP computes the volume-weighted average price over the grouped fills:
// avg = Σ(p_i·q_i) / Σ(q_i), rounded to the DECIMAL(28,8) column quantum.
// Also returns the exact totals so the engine can carry the pricing
// residual (Σp·q − filledQty·avg) as audit evidence — the deterministic
// remainder rule documented for §17.8.
func VWAP(fills []Fill) (avg, totalQty, totalValue decimal.Decimal, err error) {
	if len(fills) == 0 {
		return avg, totalQty, totalValue, allocErr(CodeAllocationInvalid,
			"average price over an empty fill set")
	}
	for _, f := range fills {
		if !f.Price.IsPositive() || !f.Quantity.IsPositive() {
			return avg, totalQty, totalValue, allocErr(CodeAllocationInvalid,
				"fill %d has non-positive price/quantity", f.TradeID)
		}
		totalQty = totalQty.Add(f.Quantity)
		totalValue = totalValue.Add(f.Price.Mul(f.Quantity))
	}
	avg = totalValue.Div(totalQty).Round(8)
	return avg, totalQty, totalValue, nil
}

// BeneficiaryQty is one beneficiary's share of the group total.
type BeneficiaryQty struct {
	AccountID int64
	Quantity  decimal.Decimal
}

// distributeTotal splits the filled qty across beneficiaries under the
// pre-declared method. MANUAL validates the caller-supplied quantities
// (Σ must not exceed filled — the residual stays unallocated on the
// omnibus account, the deterministic partial-fill treatment). PRO_RATA,
// RULE_BASED and EQUAL_SPLIT distribute the full filled quantity via the
// deterministic largest-remainder splitter (pamm.AllocateProRata — one
// quantum at a time to the largest fractional remainder, ties by
// ascending id; Σ out == filled exactly).
func distributeTotal(method Method, filled decimal.Decimal, legs []LegRequest, eligible []EligibleAccount) ([]BeneficiaryQty, error) {
	if !filled.IsPositive() {
		return nil, allocErr(CodeAllocationInvalid, "cannot allocate a group with zero filled quantity")
	}
	elig := map[int64]EligibleAccount{}
	var eligIDs []int64
	for _, e := range eligible {
		if e.Status == "ELIGIBLE" {
			elig[e.AccountID] = e
			eligIDs = append(eligIDs, e.AccountID)
		}
	}
	sort.Slice(eligIDs, func(i, j int) bool { return eligIDs[i] < eligIDs[j] })

	switch method {
	case MethodManual:
		if len(legs) == 0 {
			return nil, allocErr(CodeAllocationInvalid, "MANUAL allocation carries no legs")
		}
		out := make([]BeneficiaryQty, 0, len(legs))
		var sum decimal.Decimal
		seen := map[int64]bool{}
		for _, l := range legs {
			if _, ok := elig[l.AccountID]; !ok {
				return nil, allocErr(CodeAllocationIneligible,
					"leg beneficiary %d is not an eligible account of this group", l.AccountID)
			}
			if seen[l.AccountID] {
				return nil, allocErr(CodeAllocationInvalid,
					"duplicate leg for beneficiary %d", l.AccountID)
			}
			seen[l.AccountID] = true
			if !l.Quantity.IsPositive() || !l.Quantity.Round(8).Equal(l.Quantity) {
				return nil, allocErr(CodeAllocationInvalid,
					"leg %d: quantity %s must be positive and within the 1e-8 quantum",
					l.AccountID, l.Quantity)
			}
			out = append(out, BeneficiaryQty{AccountID: l.AccountID, Quantity: l.Quantity})
			sum = sum.Add(l.Quantity)
		}
		if sum.GreaterThan(filled) {
			return nil, allocErr(CodeAllocationSumMismatch,
				"manual allocation %s exceeds filled quantity %s", sum, filled)
		}
		return out, nil
	}

	// Weighted methods — resolve the share set.
	var shares []pamm.Share
	switch method {
	case MethodProRata:
		if len(legs) == 0 {
			return nil, allocErr(CodeAllocationInvalid, "PRO_RATA allocation carries no weight legs")
		}
		for _, l := range legs {
			if _, ok := elig[l.AccountID]; !ok {
				return nil, allocErr(CodeAllocationIneligible,
					"weight leg %d is not an eligible account of this group", l.AccountID)
			}
			shares = append(shares, pamm.Share{ID: l.AccountID, Weight: l.Weight})
		}
	case MethodRuleBased:
		// Leg weights (REST percentage intake) take precedence; absent legs
		// fall back to the pre-registered weight on the eligibility row.
		legW := map[int64]decimal.Decimal{}
		for _, l := range legs {
			if _, ok := elig[l.AccountID]; !ok {
				return nil, allocErr(CodeAllocationIneligible,
					"weight leg %d is not an eligible account of this group", l.AccountID)
			}
			legW[l.AccountID] = l.Weight
		}
		for _, id := range eligIDs {
			w := legW[id]
			if !w.IsPositive() {
				w = elig[id].Weight
			}
			if !w.IsPositive() {
				return nil, allocErr(CodeAllocationInvalid,
					"RULE_BASED requires a weight (leg or preset) on every eligible account — %d has none", id)
			}
			shares = append(shares, pamm.Share{ID: id, Weight: w})
		}
	case MethodEqualSplit:
		for _, id := range eligIDs {
			shares = append(shares, pamm.Share{ID: id, Weight: decimal.One})
		}
	default:
		return nil, allocErr(CodeAllocationInvalid, "unknown allocation method %q", method)
	}
	if len(shares) == 0 {
		return nil, allocErr(CodeAllocationInvalid, "no eligible beneficiary accounts remain")
	}
	res, err := pamm.AllocateProRata(filled, shares)
	if err != nil {
		return nil, allocErr(CodeAllocationSumMismatch, "%s split: %v", method, err)
	}
	out := make([]BeneficiaryQty, 0, len(res))
	for _, r := range res {
		if r.Quantity.IsPositive() {
			out = append(out, BeneficiaryQty{AccountID: r.ID, Quantity: r.Quantity})
		}
	}
	return out, nil
}

// LegRequest is one allocation leg of an allocate call: quantity for
// MANUAL, weight for PRO_RATA/RULE_BASED (ignored by EQUAL_SPLIT).
// Ref carries the verbatim account reference (FIX AllocAccount(79) or a
// REST fund reference) for propagation to confirmations/reporting.
type LegRequest struct {
	AccountID int64           `json:"account_id"`
	Quantity  decimal.Decimal `json:"quantity,omitempty"`
	Weight    decimal.Decimal `json:"weight,omitempty"`
	Ref       string          `json:"ref,omitempty"`
	PartyID   string          `json:"party_id,omitempty"`
	PartyLEI  string          `json:"party_lei,omitempty"`
}

// spreadFills books each beneficiary's quantity across the group's fills
// in deterministic fill order (ascending trade id). Per-fill allocation
// can never exceed the fill's remaining quantity, and per-beneficiary
// output sums to exactly its input — the conservation invariant holds at
// fill granularity (spec §5.31 links each allocation row to its source
// fill).
func spreadFills(benef []BeneficiaryQty, fills []Fill) ([]Allocation, error) {
	ordered := append([]Fill{}, fills...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].TradeID < ordered[j].TradeID })
	remaining := make([]decimal.Decimal, len(ordered))
	for i, f := range ordered {
		remaining[i] = f.Quantity
	}
	bSorted := append([]BeneficiaryQty{}, benef...)
	sort.Slice(bSorted, func(i, j int) bool { return bSorted[i].AccountID < bSorted[j].AccountID })

	var out []Allocation
	for _, b := range bSorted {
		left := b.Quantity
		for i := range ordered {
			if !left.IsPositive() {
				break
			}
			assign := decimal.Min(remaining[i], left)
			if !assign.IsPositive() {
				continue
			}
			out = append(out, Allocation{
				TradeID:              ordered[i].TradeID,
				GroupID:              ordered[i].GroupID,
				BeneficiaryAccountID: b.AccountID,
				Quantity:             assign,
				Status:               StatusAllocated,
				Kind:                 KindPrimary,
			})
			remaining[i] = remaining[i].Sub(assign)
			left = left.Sub(assign)
		}
		if left.IsPositive() {
			return nil, allocErr(CodeAllocationSumMismatch,
				"beneficiary %d quantity %s exceeds grouped fill remainder", b.AccountID, b.Quantity)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Service — the group lifecycle.
// ---------------------------------------------------------------------------

// AllocationService owns the bunched-order group lifecycle: register →
// attach fills → allocate → claim/reject/cancel → correct → submit.
type AllocationService struct {
	store AllocationStore
	roles RoleResolver // required only for post-lock corrections; nil fails that path closed
	now   func() time.Time
}

// AllocationServiceDeps wires the service; Store is mandatory.
type AllocationServiceDeps struct {
	Store AllocationStore
	// Roles authorizes the dual-control second principal on post-lock
	// corrections (nil → locked corrections are refused).
	Roles RoleResolver
	// Now overrides the clock (tests).
	Now func() time.Time
}

// NewAllocationService wires the service (fail-closed on a nil store).
func NewAllocationService(d AllocationServiceDeps) (*AllocationService, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("backoffice allocation service: nil store")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &AllocationService{store: d.Store, roles: d.Roles, now: now}, nil
}

// ---------------------------------------------------------------------------
// Step 1 — register group + eligible accounts before order entry.
// ---------------------------------------------------------------------------

// CreateGroupInput is the registration contract.
type CreateGroupInput struct {
	GroupRef         string // client idempotency ref (UNIQUE)
	ManagerAccountID int64
	InstrumentID     int64
	Side             byte // '1' buy / '2' sell (FIX Tag 54)
	Capacity         Capacity
	Method           Method
	Eligible         []EligibleAccount
}

// CreateGroup registers the bunched-order group and its eligible
// beneficial accounts. Proprietary/client mixing is refused twice: here
// (fail fast with a coded error) and at the DB layer
// (apg_account_capacity_chk trigger). Idempotent on group_ref — a replay
// returns the existing row.
func (s *AllocationService) CreateGroup(ctx context.Context, actorID int64, in CreateGroupInput) (*Group, error) {
	if actorID <= 0 {
		return nil, allocErr("UNAUTHORIZED", "actor identity required")
	}
	if in.GroupRef == "" || len(in.GroupRef) > 64 {
		return nil, allocErr(CodeAllocationInvalid, "group_ref is required (≤64 chars)")
	}
	if in.ManagerAccountID <= 0 || in.InstrumentID <= 0 {
		return nil, allocErr(CodeAllocationInvalid, "manager_account_id and instrument_id are required")
	}
	if in.Side != '1' && in.Side != '2' {
		return nil, allocErr(CodeAllocationInvalid, "side must be '1' (buy) or '2' (sell)")
	}
	if _, err := ParseCapacity(string(in.Capacity)); err != nil {
		return nil, err
	}
	if _, err := ParseMethod(string(in.Method)); err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for _, e := range in.Eligible {
		if e.AccountID <= 0 {
			return nil, allocErr(CodeAllocationInvalid, "eligible account id must be positive")
		}
		if e.AccountID == in.ManagerAccountID {
			return nil, allocErr(CodeAllocationInvalid,
				"manager account %d cannot be its own beneficiary", e.AccountID)
		}
		if e.Capacity != in.Capacity {
			return nil, allocErr(CodeAllocationCapacityMix,
				"account %d capacity %s conflicts with group capacity %s — proprietary and client interest never share a group",
				e.AccountID, e.Capacity, in.Capacity)
		}
		if seen[e.AccountID] {
			return nil, allocErr(CodeAllocationInvalid, "duplicate eligible account %d", e.AccountID)
		}
		seen[e.AccountID] = true
		if e.Weight.IsNegative() {
			return nil, allocErr(CodeAllocationInvalid, "account %d weight must be positive", e.AccountID)
		}
		if e.Status == "" {
			e.Status = "ELIGIBLE"
		}
	}

	g := &Group{
		GroupRef:         in.GroupRef,
		ManagerAccountID: in.ManagerAccountID,
		InstrumentID:     in.InstrumentID,
		Side:             in.Side,
		Capacity:         in.Capacity,
		Method:           in.Method,
		Status:           GroupOpen,
		Source:           SourceREST,
		CreatedBy:        actorID,
	}
	err := s.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		if err := tx.InsertGroup(ctx, g); err != nil {
			return err
		}
		for _, e := range in.Eligible {
			if err := tx.InsertEligible(ctx, g.ID, e); err != nil {
				return err
			}
		}
		return tx.AppendEvent(ctx, g.ID, nil, EvGroupCreated, nil, map[string]any{
			"group_ref": g.GroupRef, "manager_account_id": g.ManagerAccountID,
			"instrument_id": g.InstrumentID, "side": string(g.Side),
			"capacity": string(g.Capacity), "method": string(g.Method),
			"eligible": len(in.Eligible),
		}, fmt.Sprint(actorID), nil)
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// ---------------------------------------------------------------------------
// Step 2 — attach fills; compute the weighted average price.
// ---------------------------------------------------------------------------

// AttachFills groups the named fills onto an OPEN group. Each fill must
// belong to the manager on the group's declared side (BUY →
// buyer_account_id, SELL → seller_account_id), match the instrument, be
// COMPLETED (a BUSTED/PRICE_ADJUSTED print can never be allocated), and
// not already belong to another group. total_qty and avg_price are
// recomputed after every attach — deterministic under replay.
func (s *AllocationService) AttachFills(ctx context.Context, groupID int64, tradeIDs []int64) (*Group, error) {
	if len(tradeIDs) == 0 {
		return nil, allocErr(CodeAllocationInvalid, "no trade ids supplied")
	}
	var g *Group
	err := s.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		var err error
		g, err = tx.LockGroup(ctx, groupID)
		if err != nil {
			return err
		}
		if g.Status != GroupOpen {
			return allocErr(CodeAllocationStateConflict,
				"group %d in status %s — fills attach only while OPEN", g.ID, g.Status)
		}
		for _, tid := range tradeIDs {
			if tid <= 0 {
				return allocErr(CodeAllocationInvalid, "trade id must be positive")
			}
			if _, attached, err := tx.FillGroup(ctx, tid); err != nil {
				return err
			} else if attached {
				return allocErr(CodeAllocationInvalid,
					"trade %d is already grouped — a fill joins at most one bunched group", tid)
			}
			tr, err := tx.LoadTrade(ctx, tid)
			if err != nil {
				return err
			}
			if tr.Status != "COMPLETED" {
				return allocErr(CodeAllocationInvalid,
					"trade %d status %s — only COMPLETED fills are allocatable", tid, tr.Status)
			}
			if tr.InstrumentID != g.InstrumentID {
				return allocErr(CodeAllocationInvalid,
					"trade %d instrument %d does not match group instrument %d",
					tid, tr.InstrumentID, g.InstrumentID)
			}
			var sideAcct int64
			if g.Side == '1' {
				sideAcct = tr.BuyerAccountID
			} else {
				sideAcct = tr.SellerAccountID
			}
			if sideAcct != g.ManagerAccountID {
				return allocErr(CodeAllocationInvalid,
					"trade %d side %c account %d is not the group manager %d",
					tid, g.Side, sideAcct, g.ManagerAccountID)
			}
			if err := tx.InsertFill(ctx, groupID, g.Side, tr); err != nil {
				return err
			}
			if err := tx.AppendEvent(ctx, groupID, nil, EvFillAttached, nil, map[string]any{
				"trade_id": tid, "quantity": tr.Quantity.String(), "price": tr.Price.String(),
			}, "backoffice.allocations", nil); err != nil {
				return err
			}
		}
		return s.recompute(ctx, tx, g)
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// recompute recomputes total_qty/avg_price/allocated_qty/price_residual
// from the persisted fills — deterministic, replayable.
func (s *AllocationService) recompute(ctx context.Context, tx AllocationTx, g *Group) error {
	fills, err := tx.GroupFillsTx(ctx, g.ID)
	if err != nil {
		return err
	}
	avg, total, totalValue, err := VWAP(fills)
	if err != nil {
		if len(fills) == 0 {
			return tx.UpdateGroupComputed(ctx, g.ID,
				decimal.Zero, decimal.Zero, decimal.Zero)
		}
		return err
	}
	residual := totalValue.Sub(total.Mul(avg))
	if err := tx.UpdateGroupComputed(ctx, g.ID, total, avg, residual); err != nil {
		return err
	}
	g.TotalQty, g.AvgPrice, g.PriceResidual = total, avg, residual
	return tx.AppendEvent(ctx, g.ID, nil, EvAveragePriced, nil, map[string]any{
		"total_qty": total.String(), "avg_price": avg.String(),
		"price_residual": residual.String(), "fills": len(fills),
	}, "backoffice.allocations", nil)
}

// ---------------------------------------------------------------------------
// Step 3 — allocate under the stored method.
// ---------------------------------------------------------------------------

// Allocate runs the pre-declared method over the grouped fills and writes
// the beneficiary rows (kind PRIMARY, status ALLOCATED). Eligibility is
// re-checked at allocation time — an account marked INELIGIBLE after
// execution is excluded from weighted methods (its share folds back into
// the split deterministically) and rejected on MANUAL legs. The run is
// idempotent at group level: a second Allocate on an already-allocated
// group is refused — corrections go through CorrectAllocation.
func (s *AllocationService) Allocate(ctx context.Context, groupID int64, legs []LegRequest) ([]Allocation, error) {
	var out []Allocation
	err := s.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		g, err := tx.LockGroup(ctx, groupID)
		if err != nil {
			return err
		}
		if g.Status != GroupOpen {
			return allocErr(CodeAllocationStateConflict,
				"group %d in status %s — allocation runs once; use corrections afterwards", g.ID, g.Status)
		}
		if !g.TotalQty.IsPositive() {
			return allocErr(CodeAllocationInvalid, "group %d has no attached fills", g.ID)
		}
		eligible, err := tx.EligibleAccountsTx(ctx, g.ID)
		if err != nil {
			return err
		}
		if len(eligible) == 0 {
			return allocErr(CodeAllocationIneligible, "group %d has no eligible beneficiary accounts", g.ID)
		}
		totals, err := distributeTotal(g.Method, g.TotalQty, legs, eligible)
		if err != nil {
			return err
		}
		fills, err := tx.GroupFillsTx(ctx, g.ID)
		if err != nil {
			return err
		}
		rows, err := spreadFills(totals, fills)
		if err != nil {
			return err
		}
		eligByID := map[int64]EligibleAccount{}
		for _, e := range eligible {
			eligByID[e.AccountID] = e
		}
		legByID := map[int64]LegRequest{}
		for _, l := range legs {
			legByID[l.AccountID] = l
		}
		var allocated decimal.Decimal
		for i := range rows {
			a := &rows[i]
			a.AvgPrice = g.AvgPrice
			e := eligByID[a.BeneficiaryAccountID]
			a.PartyID, a.PartyLEI = e.PartyID, e.PartyLEI
			// Leg-supplied identifiers (FIX AllocAccount/Parties or REST
			// fund refs) override registry blanks — they propagate to
			// confirmations, PB give-up and reporting.
			if l, ok := legByID[a.BeneficiaryAccountID]; ok {
				a.AllocAccountRef = l.Ref
				if l.PartyID != "" {
					a.PartyID = l.PartyID
				}
				if l.PartyLEI != "" {
					a.PartyLEI = l.PartyLEI
				}
			}
			if err := tx.InsertAllocation(ctx, a); err != nil {
				return err
			}
			allocated = allocated.Add(a.Quantity)
			if err := tx.AppendEvent(ctx, g.ID, &a.ID, EvAllocated, nil, map[string]any{
				"allocation_id": a.ID, "trade_id": a.TradeID,
				"beneficiary_account_id": a.BeneficiaryAccountID,
				"quantity":               a.Quantity.String(), "avg_price": a.AvgPrice.String(),
				"party_id": a.PartyID, "party_lei": a.PartyLEI,
			}, "backoffice.allocations", nil); err != nil {
				return err
			}
		}
		if allocated.GreaterThan(g.TotalQty) {
			return allocErr(CodeAllocationSumMismatch,
				"allocated %s exceeds filled %s", allocated, g.TotalQty)
		}
		// Persist allocated_qty = Σ active children so the group's
		// unallocated remainder (and the T+0 sweep) reads correctly.
		if err := s.syncAllocatedQty(ctx, tx, g); err != nil {
			return err
		}
		if err := tx.SetGroupStatus(ctx, g.ID, GroupAllocated, false); err != nil {
			return err
		}
		// Parent clears through 2090_BLOCK_ALLOCATION_CLEARING: park the
		// aggregate fill obligation so each confirmed child draws the
		// clearing balance back down (Task 24.3.15 step 6 — GL nets to
		// zero once the block is fully allocated).
		if err := s.parkGroup(ctx, tx, g, fills); err != nil {
			return err
		}
		out = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Lifecycle — claim / reject / cancel (pre-settlement).
// ---------------------------------------------------------------------------

// transition loads the allocation under lock, asserts the legal move, and
// writes the audit row with the immutable before/after pair.
func (s *AllocationService) transition(ctx context.Context, allocID int64,
	to Status, actor string, reason string, expect ...Status) (*Allocation, error) {

	var out *Allocation
	err := s.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		a, err := tx.LockAllocation(ctx, allocID)
		if err != nil {
			return err
		}
		legal := false
		for _, e := range expect {
			if a.Status == e {
				legal = true
				break
			}
		}
		if !legal {
			return allocErr(CodeAllocationStateConflict,
				"allocation %d status %s cannot move to %s", a.ID, a.Status, to)
		}
		g, err := tx.LockGroup(ctx, a.GroupID)
		if err != nil {
			return err
		}
		if g.SettlementLocked && to != StatusCorrected {
			return allocErr(CodeAllocationLocked,
				"group %d is settlement-locked — corrections require the dual-control path", g.ID)
		}
		before := map[string]any{"status": string(a.Status)}
		var claimedAt *time.Time
		if to == StatusClaimed {
			now := s.now().UTC()
			claimedAt = &now
		}
		if err := tx.UpdateAllocationStatus(ctx, a.ID, to, claimedAt, reason, ""); err != nil {
			return err
		}
		a.Status = to
		a.ClaimedAt = claimedAt
		a.RejectedReason = reason
		if err := tx.AppendEvent(ctx, a.GroupID, &a.ID, string(to), before, map[string]any{
			"status": string(to), "reason": reason,
			"beneficiary_account_id": a.BeneficiaryAccountID, "quantity": a.Quantity.String(),
		}, actor, nil); err != nil {
			return err
		}
		// Keep group.allocated_qty in step (reject/cancel release quantity).
		if err := s.syncAllocatedQty(ctx, tx, g); err != nil {
			return err
		}
		out = a
		return nil
	})
	return out, err
}

// syncAllocatedQty recomputes and persists the group's active allocated
// total (allocated_qty must equal Σ active allocations, never more than
// total_qty — the CHECK constraint backs this).
func (s *AllocationService) syncAllocatedQty(ctx context.Context, tx AllocationTx, g *Group) error {
	fills, err := tx.GroupFillsTx(ctx, g.ID)
	if err != nil {
		return err
	}
	var active decimal.Decimal
	for _, f := range fills {
		q, err := tx.ActiveFillQty(ctx, g.ID, f.TradeID)
		if err != nil {
			return err
		}
		active = active.Add(q)
	}
	g.AllocatedQty = active
	return tx.UpdateGroupComputed(ctx, g.ID, g.TotalQty, g.AvgPrice, g.PriceResidual)
}

// Claim marks an ALLOCATED row CLAIMED — the beneficiary/fund-ops
// acceptance leg (spec §5.31 claimed_at).
func (s *AllocationService) Claim(ctx context.Context, allocID int64, actor string) (*Allocation, error) {
	return s.transition(ctx, allocID, StatusClaimed, actor, "", StatusAllocated)
}

// Reject marks an ALLOCATED row REJECTED — the quantity returns to the
// group's unallocated remainder.
func (s *AllocationService) Reject(ctx context.Context, allocID int64, actor, reason string) (*Allocation, error) {
	if reason == "" {
		return nil, allocErr(CodeAllocationInvalid, "reject requires a reason")
	}
	return s.transition(ctx, allocID, StatusRejected, actor, reason, StatusAllocated)
}

// Cancel withdraws an ALLOCATED/PENDING row pre-settlement.
func (s *AllocationService) Cancel(ctx context.Context, allocID int64, actor, reason string) (*Allocation, error) {
	return s.transition(ctx, allocID, StatusCancelled, actor, reason, StatusPending, StatusAllocated)
}

// BlockAllocationClearing is the dedicated clearing liability seeded by
// migration 055 — the parent block trade's aggregate obligation parks
// here while children rebook against it (spec §24 #237 step 6).
func BlockAllocationClearing(ccy string) string {
	return "2090_BLOCK_ALLOCATION_CLEARING_" + ccy
}

// parkGroup posts the parent→clearing reclass journal: the manager-side
// fill obligation (both currency legs, at the fills' true Σp·q notional)
// moves into 2090. The journal is idempotent on key alloc-park:{group}
// and balanced per currency — a LIABILITY↔LIABILITY reclass carrying no
// wallet effects.
func (s *AllocationService) parkGroup(ctx context.Context, tx AllocationTx, g *Group, fills []Fill) error {
	if len(fills) == 0 || !g.TotalQty.IsPositive() {
		return nil
	}
	tr, err := tx.LoadTrade(ctx, fills[0].TradeID)
	if err != nil {
		return err
	}
	if tr.BaseCurrency == "" || tr.QuoteCurrency == "" {
		return allocErr(CodeAllocationInvalid,
			"group %d: instrument currencies unresolvable for the clearing park", g.ID)
	}
	// Parent's true quote notional is Σp·q = total·avg + residual.
	notional := g.TotalQty.Mul(g.AvgPrice).Add(g.PriceResidual)
	nar := fmt.Sprintf("block alloc park group %d", g.ID)
	var recvCcy, delvCcy string
	var recvAmt, delvAmt decimal.Decimal
	if g.Side == '1' { // manager bought base, paid quote
		recvCcy, recvAmt = tr.BaseCurrency, g.TotalQty
		delvCcy, delvAmt = tr.QuoteCurrency, notional
	} else { // manager sold base, received quote
		recvCcy, recvAmt = tr.QuoteCurrency, notional
		delvCcy, delvAmt = tr.BaseCurrency, g.TotalQty
	}
	// Manager received → DR customer liability / CR clearing.
	// Manager delivered → CR customer liability / DR clearing.
	// The key is content-addressed: a replayed allocate dedups, while a
	// superseding FIX replace with shifted totals posts a new journal
	// (the superseded one is reversed by the engine's unparkSuperseded).
	_, err = tx.PostJournal(ctx, ledger.Journal{
		EntryType:      ledger.EntrySettlement,
		ReferenceID:    g.ID,
		Description:    fmt.Sprintf("block allocation park group %d (%s)", g.ID, g.GroupRef),
		PostedBy:       "backoffice.allocations",
		IdempotencyKey: fmt.Sprintf("alloc-park:%d:%s:%s", g.ID, g.TotalQty.String(), g.AvgPrice.String()),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(recvCcy), recvCcy, recvAmt, nar),
			ledger.CreditLine(BlockAllocationClearing(recvCcy), recvCcy, recvAmt, nar),
			ledger.CreditLine(ledger.CustomerLiability(delvCcy), delvCcy, delvAmt, nar),
			ledger.DebitLine(BlockAllocationClearing(delvCcy), delvCcy, delvAmt, nar),
		},
	})
	return err
}

// SetEligibility flips an eligible account's registry status after
// execution (spec §17.8 edge case — a beneficiary that becomes ineligible
// mid-lifecycle). INELIGIBLE accounts are excluded from weighted methods
// and rejected on MANUAL legs; flipping an account that still holds
// ACTIVE allocations is refused — the legs must be corrected first so no
// ineligible beneficiary ever carries booked quantity.
func (s *AllocationService) SetEligibility(ctx context.Context, groupID, accountID int64, eligible bool, actor string) (*Group, error) {
	var g *Group
	status := "ELIGIBLE"
	if !eligible {
		status = "INELIGIBLE"
	}
	err := s.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		var err error
		g, err = tx.LockGroup(ctx, groupID)
		if err != nil {
			return err
		}
		if g.Status != GroupOpen && g.Status != GroupAllocated {
			return allocErr(CodeAllocationStateConflict,
				"group %d in status %s — eligibility is frozen once locked/closed", g.ID, g.Status)
		}
		if !eligible {
			allocs, err := tx.AllocationsTx(ctx, g.ID)
			if err != nil {
				return err
			}
			for _, a := range allocs {
				if a.BeneficiaryAccountID == accountID && a.Status.Active() &&
					(a.Kind == KindPrimary || a.Kind == KindReplacement) {
					return allocErr(CodeAllocationStateConflict,
						"account %d holds active allocation %d — correct it before marking ineligible",
						accountID, a.ID)
				}
			}
		}
		if err := tx.SetEligibleStatus(ctx, groupID, accountID, status); err != nil {
			return err
		}
		typ := EvEligibleRegistered
		if !eligible {
			typ = EvAccountIneligible
		}
		return tx.AppendEvent(ctx, g.ID, nil, typ, nil, map[string]any{
			"account_id": accountID, "status": status,
		}, actor, nil)
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// unparkLeg posts the inverse of the child rebooking journal — the leg's
// customer-liability/clearing movement is reversed exactly (GL reversal,
// never an in-place edit). Idempotent on alloc-unpark:{allocID}.
func (s *AllocationService) unparkLeg(ctx context.Context, tx AllocationTx, g *Group, a *Allocation) error {
	tr, err := tx.LoadTrade(ctx, a.TradeID)
	if err != nil {
		return err
	}
	notional := a.Quantity.Mul(a.AvgPrice).Round(8)
	nar := fmt.Sprintf("block alloc reversal alloc %d", a.ID)
	var recvCcy, delvCcy string
	var recvAmt, delvAmt decimal.Decimal
	if g.Side == '1' {
		recvCcy, recvAmt = tr.BaseCurrency, a.Quantity
		delvCcy, delvAmt = tr.QuoteCurrency, notional
	} else {
		recvCcy, recvAmt = tr.QuoteCurrency, notional
		delvCcy, delvAmt = tr.BaseCurrency, a.Quantity
	}
	_, err = tx.PostJournal(ctx, ledger.Journal{
		EntryType:      ledger.EntrySettlement,
		ReferenceID:    a.ID,
		Description:    fmt.Sprintf("block allocation reversal alloc %d", a.ID),
		PostedBy:       "backoffice.allocations",
		IdempotencyKey: fmt.Sprintf("alloc-unpark:%d", a.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(BlockAllocationClearing(recvCcy), recvCcy, recvAmt, nar),
			ledger.CreditLine(ledger.CustomerLiability(recvCcy), recvCcy, recvAmt, nar),
			ledger.CreditLine(BlockAllocationClearing(delvCcy), delvCcy, delvAmt, nar),
			ledger.DebitLine(ledger.CustomerLiability(delvCcy), delvCcy, delvAmt, nar),
		},
	})
	return err
}

// ---------------------------------------------------------------------------
// Step 4 — settlement lock + corrections.
// ---------------------------------------------------------------------------

// SubmitToSettlement locks the group: settlement_locked flips and the
// status moves to SETTLEMENT_LOCKED. Pre-lock corrections stay available
// below; post-lock corrections require dual control via Correct.
func (s *AllocationService) SubmitToSettlement(ctx context.Context, groupID int64, actor string) (*Group, error) {
	var g *Group
	err := s.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		var err error
		g, err = tx.LockGroup(ctx, groupID)
		if err != nil {
			return err
		}
		if g.Status != GroupAllocated {
			return allocErr(CodeAllocationStateConflict,
				"group %d in status %s — only ALLOCATED groups submit to settlement", g.ID, g.Status)
		}
		before := map[string]any{"status": string(g.Status), "settlement_locked": g.SettlementLocked}
		if err := tx.SetGroupStatus(ctx, g.ID, GroupLocked, true); err != nil {
			return err
		}
		g.Status, g.SettlementLocked = GroupLocked, true
		now := s.now().UTC()
		g.SubmittedAt = &now
		return tx.AppendEvent(ctx, g.ID, nil, EvSettlementLocked, before, map[string]any{
			"status": string(GroupLocked), "settlement_locked": true,
		}, actor, nil)
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// CorrectInput is a post-booking correction: move an allocation to a new
// beneficiary and/or re-quantity it. Zero values leave the field
// unchanged.
type CorrectInput struct {
	BeneficiaryAccountID int64           // 0 = keep
	Quantity             decimal.Decimal // zero = keep
	Reason               string          // required — audit evidence
}

// Correction is the offset/replacement pair a correction books.
type Correction struct {
	Offset      Allocation `json:"offset"`      // retires the original
	Replacement Allocation `json:"replacement"` // corrected booking
	Corrected   Allocation `json:"corrected"`   // the superseded original
}

// Correct applies an offset/replacement correction (Task 24.3.10 step 4).
// Pre-lock groups accept single-principal corrections; once the group is
// settlement-locked the caller must supply a distinct, role-eligible
// ApproverID — the §8.2 four-eyes contract executed synchronously (the
// admin endpoint also offers the queued path through the dual-control
// service; both converge on the same transaction body). The original row
// moves to CORRECTED; the OFFSET and REPLACEMENT rows carry
// corrects_allocation_id lineage, and the audit event records immutable
// before/after state plus both principals.
func (s *AllocationService) Correct(ctx context.Context, actor Actor, allocID int64, in CorrectInput) (*Correction, error) {
	if in.Reason == "" {
		return nil, allocErr(CodeAllocationInvalid, "correction requires a reason")
	}
	if in.BeneficiaryAccountID == 0 && in.Quantity.IsZero() {
		return nil, allocErr(CodeAllocationInvalid, "correction changes nothing")
	}
	if in.Quantity.IsNegative() {
		return nil, allocErr(CodeAllocationInvalid, "corrected quantity must be positive")
	}
	var out *Correction
	err := s.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		a, err := tx.LockAllocation(ctx, allocID)
		if err != nil {
			return err
		}
		if a.Kind != KindPrimary && a.Kind != KindReplacement {
			return allocErr(CodeAllocationInvalid, "allocation %d is itself an offset", a.ID)
		}
		switch a.Status {
		case StatusAllocated, StatusClaimed:
		default:
			return allocErr(CodeAllocationStateConflict,
				"allocation %d status %s is not correctable", a.ID, a.Status)
		}
		g, err := tx.LockGroup(ctx, a.GroupID)
		if err != nil {
			return err
		}
		var approvedBy *int64
		if g.SettlementLocked {
			// Post-submission corrections: dual control, fail closed.
			if actor.ApproverID <= 0 || actor.ApproverID == actor.UserID {
				return allocErr("DUAL_CONTROL_REQUIRED",
					"settlement-locked correction requires a distinct second authorizer (§8.2)")
			}
			if s.roles == nil {
				return allocErr("UNAUTHORIZED_ROLE", "role resolver unavailable — refusing locked correction")
			}
			role, rerr := s.roles(ctx, actor.ApproverID)
			if rerr != nil || !correctionRoles[role] {
				return allocErr("UNAUTHORIZED_ROLE",
					"correction approver must hold Risk Manager / Finance Ops / Super Admin")
			}
			ap := actor.ApproverID
			approvedBy = &ap
		}
		newBenef := a.BeneficiaryAccountID
		if in.BeneficiaryAccountID != 0 {
			// The corrected beneficiary must sit on the eligible registry.
			eligible, err := tx.EligibleAccountsTx(ctx, g.ID)
			if err != nil {
				return err
			}
			ok := false
			for _, e := range eligible {
				if e.AccountID == in.BeneficiaryAccountID && e.Status == "ELIGIBLE" {
					ok = true
					break
				}
			}
			if !ok {
				return allocErr(CodeAllocationIneligible,
					"corrected beneficiary %d is not eligible on group %d", in.BeneficiaryAccountID, g.ID)
			}
			newBenef = in.BeneficiaryAccountID
		}
		newQty := a.Quantity
		if !in.Quantity.IsZero() {
			newQty = in.Quantity
		}
		if !newQty.Round(8).Equal(newQty) {
			return allocErr(CodeAllocationInvalid, "corrected quantity exceeds the 1e-8 quantum")
		}
		// Conservation: the replacement may grow the leg only up to the
		// fill's remaining headroom — (active − this row) + new ≤ fill qty.
		active, err := tx.ActiveFillQty(ctx, g.ID, a.TradeID)
		if err != nil {
			return err
		}
		fillQty := decimal.Zero
		fills, ferr := tx.GroupFillsTx(ctx, g.ID)
		if ferr != nil {
			return ferr
		}
		for _, f := range fills {
			if f.TradeID == a.TradeID {
				fillQty = f.Quantity
			}
		}
		if active.Sub(a.Quantity).Add(newQty).GreaterThan(fillQty) {
			return allocErr(CodeAllocationSumMismatch,
				"corrected quantity %s would over-allocate fill %d (active %s, fill %s)",
				newQty, a.TradeID, active.Sub(a.Quantity), fillQty)
		}

		// If the superseded leg already rebooked GL at confirm time (it
		// carries settlement instruction ids), post the exact inverse
		// journal — GL entries are reversed and re-posted per allocation,
		// never edited in place (idempotent on alloc-unpark:{id}).
		if a.Status == StatusClaimed && len(a.SettlementInstructionIDs) > 0 {
			if err := s.unparkLeg(ctx, tx, g, a); err != nil {
				return err
			}
		}
		before, _ := json.Marshal(a)
		if err := tx.UpdateAllocationStatus(ctx, a.ID, StatusCorrected, nil, in.Reason, ""); err != nil {
			return err
		}
		// OFFSET row — the immutable reversal record.
		offset := &Allocation{
			TradeID: a.TradeID, GroupID: g.ID,
			BeneficiaryAccountID: a.BeneficiaryAccountID,
			Quantity:             a.Quantity, AvgPrice: a.AvgPrice,
			Status: StatusCorrected, Kind: KindOffset,
			CorrectsAllocationID: &a.ID,
			PartyID:              a.PartyID, PartyLEI: a.PartyLEI, AllocAccountRef: a.AllocAccountRef,
		}
		if err := tx.InsertAllocation(ctx, offset); err != nil {
			return err
		}
		// REPLACEMENT row — the corrected booking (returns to ALLOCATED;
		// a corrected CLAIMED leg must be re-confirmed by fund ops).
		repl := &Allocation{
			TradeID: a.TradeID, GroupID: g.ID,
			BeneficiaryAccountID: newBenef,
			Quantity:             newQty, AvgPrice: a.AvgPrice,
			Status: StatusAllocated, Kind: KindReplacement,
			CorrectsAllocationID: &a.ID,
			PartyID:              a.PartyID, PartyLEI: a.PartyLEI, AllocAccountRef: a.AllocAccountRef,
		}
		if err := tx.InsertAllocation(ctx, repl); err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{
			"offset": offset, "replacement": repl,
		})
		if err := tx.AppendEvent(ctx, g.ID, &a.ID, EvCorrected,
			json.RawMessage(before), json.RawMessage(after),
			fmt.Sprint(actor.UserID), approvedBy); err != nil {
			return err
		}
		a.Status = StatusCorrected
		out = &Correction{Offset: *offset, Replacement: *repl, Corrected: *a}
		return s.syncAllocatedQty(ctx, tx, g)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Read surface — audit chain.
// ---------------------------------------------------------------------------

// GroupDetail bundles a group with its fills, allocations, eligible
// registry and immutable event log — the §24 #172 evidence bundle.
type GroupDetail struct {
	Group       *Group            `json:"group"`
	Fills       []Fill            `json:"fills"`
	Allocations []Allocation      `json:"allocations"`
	Eligible    []EligibleAccount `json:"eligible"`
	Events      []Event           `json:"events"`
}

// Detail loads the full group evidence bundle.
func (s *AllocationService) Detail(ctx context.Context, groupID int64) (*GroupDetail, error) {
	g, err := s.store.Group(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, allocErr(CodeAllocationNotFound, "average-price group %d not found", groupID)
	}
	fills, err := s.store.Fills(ctx, groupID)
	if err != nil {
		return nil, err
	}
	allocs, err := s.store.Allocations(ctx, groupID)
	if err != nil {
		return nil, err
	}
	elig, err := s.store.EligibleAccounts(ctx, groupID)
	if err != nil {
		return nil, err
	}
	evs, err := s.store.Events(ctx, groupID)
	if err != nil {
		return nil, err
	}
	return &GroupDetail{Group: g, Fills: fills, Allocations: allocs, Eligible: elig, Events: evs}, nil
}
