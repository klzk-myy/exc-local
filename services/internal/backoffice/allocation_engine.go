// allocation_engine.go — Phase-24 Task 24.3.15: post-trade allocation of
// block trades to fund sub-accounts (spec §17.8, §24 #237; the §5.31
// group/fill/allocation lifecycle lives in allocations.go).
//
// Intake:
//   - REST POST /api/v1/allocations → Engine.SubmitBlock (the caller's
//     trade_id + method + legs; the manager account on the trade's
//     matching side becomes the group's omnibus account).
//   - FIX 35=J → internal/fix.AllocationService validates, persists to
//     migration-226 tables and emits 35=AK; committed changes reach this
//     engine through Engine.OnAllocationChange (the fix.AllocationChangeSink
//     contract that file declared for Phase-24). The engine ingests each
//     COMMITTED/REPLACED instruction into a Source=FIX average_price_group
//     keyed "FIX-{AllocID}" and CANCELLED withdraws the active rows.
//
// Per-fund workflow: each booked allocation is claimed (fund-ops confirm)
// or rejected through the engine. A CLAIMED leg:
//  1. mints its own settlement_instructions rows — one PAY and one
//     RECEIVE currency leg on the fund account, settlement_date inherited
//     verbatim from the parent trade fill (never recomputed);
//  2. rebooks the leg on the GL against 2090_BLOCK_ALLOCATION_CLEARING
//     (parked by AllocationService.parkGroup at allocation commit — every
//     child journal is a per-currency-balanced LIABILity↔LIABILITY reclass
//     so the group's aggregate GL impact nets to zero while the parent
//     trade's own audit trail is untouched);
//  3. generates the per-fund 35=AK AllocationReport (fix.BuildAllocationReport
//     projected onto the single leg) and hands it to the ReportEmitter seam.
//
// Corrections after partial settlement (spec edge case): an allocation
// carrying SETTLED/RECONCILED legs can never be amended — the legs are
// final. PENDING legs are VOIDed (settlement_status_enum 'VOID', added by
// migration 051) and the offset/replacement pair rebooks through
// AllocationService.Correct (dual-control when the group is locked).
//
// T+0 deadline: EscalateUnallocated runs the EOD sweep — every group that
// still carries unallocated filled quantity past cutoff lands a durable
// funding_ops_alerts P2 row (code UNALLOCATED_BLOCK_TRADE), pages the
// OpsAlerter seam, and stamps escalated_at so repeats are idempotent.
//
// Fail-closed: nil store, nil exec resolver on the FIX path, unknown
// instruments/currencies, or conservation breaches are coded errors.
package backoffice

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quickfixgo/quickfix"

	"exchange/internal/fix"
	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Seams.
// ---------------------------------------------------------------------------

// TradeResolver resolves a FIX ExecID/OrderID reference to its trades.id —
// production binds the same resolver the FIX session layer uses
// (fix.ExecQuantityResolver extended to carry the trade key). Nil fails
// the FIX ingest path closed.
type TradeResolver interface {
	TradeForExec(ctx context.Context, execID string) (tradeID int64, err error)
}

// FundReportMeta is the per-fund context an emitter needs to route a
// generated 35=AK (session binding, Parties propagation).
type FundReportMeta struct {
	GroupID      int64  `json:"group_id"`
	GroupRef     string `json:"group_ref"`
	AllocationID int64  `json:"allocation_id"`
	AccountID    int64  `json:"account_id"`
	PartyID      string `json:"party_id,omitempty"`
	PartyLEI     string `json:"party_lei,omitempty"`
}

// ReportEmitter delivers a generated 35=AK to the fund's channel (FIX
// session bound to the beneficiary's drop-copy endpoint, or the report
// outbox). Production binds a queued dispatcher; a nil emitter still
// generates and records the report — delivery retries ride the
// confirmation_ref audit trail.
type ReportEmitter interface {
	EmitAllocationReport(ctx context.Context, msg *quickfix.Message, meta FundReportMeta) (confirmationRef string, err error)
}

// ---------------------------------------------------------------------------
// Engine.
// ---------------------------------------------------------------------------

// Engine is the block-trade allocation orchestrator (Task 24.3.15). It
// embeds the Task 24.3.10 group lifecycle (AllocationService) and adds the
// intake/confirm/settle/escalate surfaces.
type Engine struct {
	svc     *AllocationService
	store   AllocationStore
	execs   TradeResolver // FIX path only — nil fails ingest closed
	reports ReportEmitter // nil → report generated + ref recorded, no transport
	alerter OpsAlerter    // nil → durable alert row only (same convention as confirmation.go)
	now     func() time.Time
}

// Engine satisfies the fix.AllocationChangeSink contract declared by
// internal/fix/allocation.go for Phase-24 settlement propagation.
var _ fix.AllocationChangeSink = (*Engine)(nil)

// EngineDeps wires the engine; Store is mandatory.
type EngineDeps struct {
	Store   AllocationStore
	Roles   RoleResolver
	Execs   TradeResolver
	Reports ReportEmitter
	Alerter OpsAlerter
	Now     func() time.Time
}

// NewEngine wires the engine (fail-closed on a nil store).
func NewEngine(d EngineDeps) (*Engine, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("backoffice allocation engine: nil store")
	}
	svc, err := NewAllocationService(AllocationServiceDeps{
		Store: d.Store, Roles: d.Roles, Now: d.Now,
	})
	if err != nil {
		return nil, err
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Engine{svc: svc, store: d.Store, execs: d.Execs,
		reports: d.Reports, alerter: d.Alerter, now: now}, nil
}

// Service exposes the embedded group lifecycle for the handler layer.
func (e *Engine) Service() *AllocationService { return e.svc }

// Handler-facing passthroughs (single surface = the allocationBackend
// seam in internal/api/handlers_allocations.go).
func (e *Engine) CreateGroup(ctx context.Context, actorID int64, in CreateGroupInput) (*Group, error) {
	return e.svc.CreateGroup(ctx, actorID, in)
}
func (e *Engine) AttachFills(ctx context.Context, groupID int64, tradeIDs []int64) (*Group, error) {
	return e.svc.AttachFills(ctx, groupID, tradeIDs)
}
func (e *Engine) Allocate(ctx context.Context, groupID int64, legs []LegRequest) ([]Allocation, error) {
	return e.svc.Allocate(ctx, groupID, legs)
}
func (e *Engine) SubmitToSettlement(ctx context.Context, groupID int64, actor string) (*Group, error) {
	return e.svc.SubmitToSettlement(ctx, groupID, actor)
}
func (e *Engine) Cancel(ctx context.Context, allocID int64, actor, reason string) (*Allocation, error) {
	return e.svc.Cancel(ctx, allocID, actor, reason)
}
func (e *Engine) Correct(ctx context.Context, actor Actor, allocID int64, in CorrectInput) (*Correction, error) {
	return e.svc.Correct(ctx, actor, allocID, in)
}
func (e *Engine) SetEligibility(ctx context.Context, groupID, accountID int64, eligible bool, actor string) (*Group, error) {
	return e.svc.SetEligibility(ctx, groupID, accountID, eligible, actor)
}
func (e *Engine) Detail(ctx context.Context, groupID int64) (*GroupDetail, error) {
	return e.svc.Detail(ctx, groupID)
}

// ---------------------------------------------------------------------------
// REST intake — POST /api/v1/allocations.
// ---------------------------------------------------------------------------

// BlockSubmitInput is the REST intake contract: one source block trade
// split into per-fund legs under the named method.
type BlockSubmitInput struct {
	TradeID  int64        `json:"trade_id"`
	Side     byte         `json:"side"` // '1' — caller holds the BUY side; '2' — SELL
	Method   Method       `json:"allocation_method"`
	Capacity Capacity     `json:"capacity"`  // "" defaults CLIENT (block allocation is client interest)
	GroupRef string       `json:"group_ref"` // optional; default REST-BLK-{trade}-{side}
	Legs     []LegRequest `json:"legs"`
}

// BlockResult is the submit response payload.
type BlockResult struct {
	Group       *Group       `json:"group"`
	Allocations []Allocation `json:"allocations"`
}

// SubmitBlock processes POST /api/v1/allocations: register-or-load the
// group keyed to (trade, side), auto-register each leg's fund account
// into the eligibility registry (capacity enforced — client and
// proprietary never mix), attach the source fill, then run the stored
// method. The parent block's full filled quantity is allocated across the
// legs for weighted methods; MANUAL may leave a remainder parked on the
// omnibus (the T+0 sweep flags it).
func (e *Engine) SubmitBlock(ctx context.Context, actorID int64, in BlockSubmitInput) (*BlockResult, error) {
	if actorID <= 0 {
		return nil, allocErr("UNAUTHORIZED", "actor identity required")
	}
	if in.TradeID <= 0 {
		return nil, allocErr(CodeAllocationInvalid, "trade_id is required")
	}
	if in.Side != '1' && in.Side != '2' {
		return nil, allocErr(CodeAllocationInvalid, "side must be '1' (buy) or '2' (sell)")
	}
	if _, err := ParseMethod(string(in.Method)); err != nil {
		return nil, err
	}
	cap := in.Capacity
	if cap == "" {
		cap = CapacityClient
	}
	if _, err := ParseCapacity(string(cap)); err != nil {
		return nil, err
	}
	ref := in.GroupRef
	if ref == "" {
		ref = fmt.Sprintf("REST-BLK-%d-%c", in.TradeID, in.Side)
	}
	var g *Group
	var allocs []Allocation
	err := e.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		tr, err := tx.LoadTrade(ctx, in.TradeID)
		if err != nil {
			return err
		}
		if tr.Status != "COMPLETED" {
			return allocErr(CodeAllocationInvalid,
				"trade %d status %s — only COMPLETED blocks are allocatable", in.TradeID, tr.Status)
		}
		manager := tr.BuyerAccountID
		if in.Side == '2' {
			manager = tr.SellerAccountID
		}
		// REST intake is account-scoped: the caller must BE the account
		// holding the named side of the block (ownership check — admins
		// create groups via the admin surface instead).
		if manager != actorID {
			return allocErr("FORBIDDEN",
				"caller account %d does not hold the %s side of trade %d",
				actorID, map[byte]string{'1': "buy", '2': "sell"}[in.Side], in.TradeID)
		}
		if len(in.Legs) == 0 && in.Method == MethodManual {
			return allocErr(CodeAllocationInvalid, "MANUAL allocation carries no legs")
		}
		// Idempotent group resolution on (group_ref) — replays re-drive the
		// deterministic allocate instead of double-booking.
		g, err = tx.LockGroupByRef(ctx, ref)
		if err != nil {
			return err
		}
		if g == nil {
			g = &Group{
				GroupRef: ref, ManagerAccountID: manager,
				InstrumentID: tr.InstrumentID, Side: in.Side,
				Capacity: cap, Method: in.Method, Status: GroupOpen,
				Source: SourceREST, CreatedBy: actorID,
			}
			if err := tx.InsertGroup(ctx, g); err != nil {
				return err
			}
			if err := tx.AppendEvent(ctx, g.ID, nil, EvGroupCreated, nil, map[string]any{
				"group_ref": g.GroupRef, "manager_account_id": g.ManagerAccountID,
				"trade_id": in.TradeID, "side": string(in.Side), "source": "REST",
			}, fmt.Sprint(actorID), nil); err != nil {
				return err
			}
		} else {
			if g.Status != GroupOpen {
				return allocErr(CodeAllocationStateConflict,
					"group %d already allocated — use the correction path", g.ID)
			}
			if g.ManagerAccountID != manager || g.InstrumentID != tr.InstrumentID {
				return allocErr(CodeAllocationInvalid,
					"group_ref %s resolves to a different trade/side", ref)
			}
		}
		// Auto-register leg accounts as eligible (registry audit trail);
		// idempotent on (group, account) — the pgx layer upserts.
		for _, l := range in.Legs {
			if l.AccountID <= 0 {
				return allocErr(CodeAllocationInvalid, "leg fund_account_id must be positive")
			}
			if err := tx.InsertEligible(ctx, g.ID, EligibleAccount{
				AccountID: l.AccountID, Capacity: cap, Weight: l.Weight,
				PartyID: l.PartyID, PartyLEI: l.PartyLEI, Status: "ELIGIBLE",
			}); err != nil {
				return err
			}
		}
		if _, attached, err := tx.FillGroup(ctx, in.TradeID); err != nil {
			return err
		} else if !attached {
			if err := tx.InsertFill(ctx, g.ID, in.Side, tr); err != nil {
				return err
			}
			if err := tx.AppendEvent(ctx, g.ID, nil, EvFillAttached, nil, map[string]any{
				"trade_id": in.TradeID, "quantity": tr.Quantity.String(), "price": tr.Price.String(),
			}, "backoffice.allocations", nil); err != nil {
				return err
			}
		}
		// Recompute VWAP/totals inside the intake tx so Allocate below
		// sees the fresh fill set.
		return e.svc.recompute(ctx, tx, g)
	})
	if err != nil {
		return nil, err
	}
	allocs, err = e.svc.Allocate(ctx, g.ID, in.Legs)
	if err != nil {
		return nil, err
	}
	// Re-read so the response carries the persisted totals (Allocate
	// locked its own copy — allocated_qty moved inside that tx).
	if fresh, ferr := e.store.Group(ctx, g.ID); ferr == nil && fresh != nil {
		g = fresh
	}
	return &BlockResult{Group: g, Allocations: allocs}, nil
}

// ---------------------------------------------------------------------------
// FIX intake — fix.AllocationChangeSink.
// ---------------------------------------------------------------------------

// OnAllocationChange ingests a committed 35=J lifecycle event
// (COMMITTED/REPLACED/CANCELLED) from the Phase-18 FIX allocation
// service. The settlement-side projection lands in the migration-055
// tables — a Source=FIX group keyed "FIX-{AllocID}" with the resolved
// legs auto-registered; REPLACED supersedes the group's active rows with
// the new legs (immutable history via the event log, same contract as
// fix's own Supersede); CANCELLED withdraws them.
func (e *Engine) OnAllocationChange(ctx context.Context, ev fix.AllocationChange) error {
	a := ev.Allocation
	if a.ID == 0 || a.AllocID == "" {
		return allocErr(CodeAllocationInvalid, "allocation change carries no committed row")
	}
	ref := fmt.Sprintf("FIX-%s", a.AllocID)
	switch ev.Type {
	case "CANCELLED":
		return e.cancelFixGroup(ctx, ref, a, ev.PrevAllocID)
	case "COMMITTED", "REPLACED":
	default:
		return allocErr(CodeAllocationInvalid, "unknown allocation change type %q", ev.Type)
	}
	if e.execs == nil {
		return allocErr(CodeServiceDegraded,
			"FIX allocation intake requires a trade resolver (exec refs → trades.id)")
	}
	// Resolve the referenced fills; every ref must land on a trades row.
	var tradeIDs []int64
	for _, r := range a.ExecRefs {
		tid, err := e.execs.TradeForExec(ctx, r)
		if err != nil || tid <= 0 {
			return allocErr(CodeAllocationInvalid, "exec ref %q unresolvable to a fill", r)
		}
		tradeIDs = append(tradeIDs, tid)
	}
	if len(tradeIDs) == 0 {
		return allocErr(CodeAllocationInvalid, "FIX allocation %s carries no fill refs", a.AllocID)
	}
	var g *Group
	var wasParked bool
	var oldTotal, oldAvg, oldResid decimal.Decimal
	err := e.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		var err error
		g, err = tx.LockGroupByRef(ctx, ref)
		if err != nil {
			return err
		}
		if g == nil {
			// First fill resolves instrument/side/manager for the group.
			tr, err := tx.LoadTrade(ctx, tradeIDs[0])
			if err != nil {
				return err
			}
			manager := tr.BuyerAccountID
			if a.Side == '2' {
				manager = tr.SellerAccountID
			}
			g = &Group{
				GroupRef: ref, ManagerAccountID: manager,
				InstrumentID: tr.InstrumentID, Side: a.Side,
				Capacity: CapacityClient, // FIX allocation legs are client fund accounts
				// Legs arrive already quantity-resolved (the FIX side
				// applied the method + exact-sum conservation), so the
				// settlement-side booking method is MANUAL.
				Method: MethodManual, Status: GroupOpen,
				Source: SourceFIX, FixAllocationID: a.ID,
			}
			if err := tx.InsertGroup(ctx, g); err != nil {
				return err
			}
			if err := tx.AppendEvent(ctx, g.ID, nil, EvFixIngested, nil, map[string]any{
				"fix_allocation_id": a.ID, "alloc_id": a.AllocID, "type": ev.Type,
				"method": string(a.Method), "exec_qty": a.ExecQty.String(),
			}, "fix.allocation", nil); err != nil {
				return err
			}
		} else {
			if g.SettlementLocked {
				return allocErr(CodeAllocationLocked,
					"group %d is settlement-locked — FIX %s must travel the dual-control correction path",
					g.ID, ev.Type)
			}
			if err := tx.SetGroupFixRef(ctx, g.ID, a.ID); err != nil {
				return err
			}
			if ev.Type == "REPLACED" {
				wasParked = g.Status == GroupAllocated && g.TotalQty.IsPositive()
				oldTotal, oldAvg, oldResid = g.TotalQty, g.AvgPrice, g.PriceResidual
				// Supersede: every active leg of the prior instruction is
				// cancelled before the replacement legs book — the
				// immutable pair lives in the audit trail.
				rows, rerr := tx.AllocationsTx(ctx, g.ID)
				if rerr != nil {
					return rerr
				}
				for _, al := range rows {
					if (al.Kind == KindPrimary || al.Kind == KindReplacement) && al.Status.Active() {
						if err := tx.UpdateAllocationStatus(ctx, al.ID, StatusCancelled, nil,
							"superseded by FIX "+a.AllocID, ""); err != nil {
							return err
						}
						if err := tx.AppendEvent(ctx, g.ID, &al.ID, EvCancelled,
							map[string]any{"status": string(al.Status)},
							map[string]any{"status": "CANCELLED", "reason": "FIX REPLACE " + a.AllocID},
							"fix.allocation", nil); err != nil {
							return err
						}
					}
				}
				// Re-open for the replacement run.
				if err := tx.SetGroupStatus(ctx, g.ID, GroupOpen, false); err != nil {
					return err
				}
				g.Status = GroupOpen
			}
		}
		// Attach every referenced fill (idempotent — FillGroup dedups).
		for _, tid := range tradeIDs {
			if gid, attached, err := tx.FillGroup(ctx, tid); err != nil {
				return err
			} else if attached {
				if gid != g.ID {
					return allocErr(CodeAllocationInvalid,
						"fill %d already grouped under %d — cross-instruction reuse refused", tid, gid)
				}
				continue
			}
			tr, err := tx.LoadTrade(ctx, tid)
			if err != nil {
				return err
			}
			if tr.InstrumentID != g.InstrumentID {
				return allocErr(CodeAllocationInvalid,
					"fill %d instrument %d != group instrument %d", tid, tr.InstrumentID, g.InstrumentID)
			}
			if err := tx.InsertFill(ctx, g.ID, g.Side, tr); err != nil {
				return err
			}
			if err := tx.AppendEvent(ctx, g.ID, nil, EvFillAttached, nil, map[string]any{
				"trade_id": tid, "quantity": tr.Quantity.String(),
			}, "fix.allocation", nil); err != nil {
				return err
			}
		}
		// Legs → legs with resolved accounts (STEP_OUT external legs carry
		// no internal account — they book outside this ledger).
		for _, l := range a.Legs {
			if l.AccountID <= 0 {
				continue
			}
			if err := tx.InsertEligible(ctx, g.ID, EligibleAccount{
				AccountID: l.AccountID, Capacity: g.Capacity, Status: "ELIGIBLE",
			}); err != nil {
				return err
			}
		}
		return e.svc.recompute(ctx, tx, g)
	})
	if err != nil {
		return err
	}
	// REPLACED with shifted totals: the prior park journal stays posted —
	// reverse it before Allocate re-parks under the new totals (reversal,
	// never an in-place edit; the park key carries the content hash).
	if wasParked && (!oldTotal.Equal(g.TotalQty) || !oldAvg.Equal(g.AvgPrice)) {
		if err := e.unparkSuperseded(ctx, g, oldTotal, oldAvg, oldResid); err != nil {
			return err
		}
	}
	// Book the resolved leg quantities MANUAL-style — the FIX side already
	// applied the PRO_RATA split and exact-sum conservation.
	legs := make([]LegRequest, 0, len(a.Legs))
	for _, l := range a.Legs {
		if l.AccountID <= 0 || !l.AllocQty.IsPositive() {
			continue // external STEP_OUT or zero leg — no internal booking
		}
		legs = append(legs, LegRequest{
			AccountID: l.AccountID, Quantity: l.AllocQty, Ref: l.AllocAccount,
		})
	}
	_, err = e.svc.Allocate(ctx, g.ID, legs)
	return err
}

// unparkSuperseded reverses the superseded park journal (the inverse of
// the original park, keyed on the superseded totals) before the
// replacement allocation re-parks under the new totals.
func (e *Engine) unparkSuperseded(ctx context.Context, g *Group, oldTotal, oldAvg, oldResid decimal.Decimal) error {
	return e.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		fills, err := tx.GroupFillsTx(ctx, g.ID)
		if err != nil || len(fills) == 0 {
			return err
		}
		tr, err := tx.LoadTrade(ctx, fills[0].TradeID)
		if err != nil {
			return err
		}
		// Mirror parkGroup exactly: notional = Σp·q = total·avg + residual.
		notional := oldTotal.Mul(oldAvg).Add(oldResid)
		nar := fmt.Sprintf("block alloc park reversal group %d", g.ID)
		var recvCcy, delvCcy string
		var recvAmt, delvAmt decimal.Decimal
		if g.Side == '1' {
			recvCcy, recvAmt = tr.BaseCurrency, oldTotal
			delvCcy, delvAmt = tr.QuoteCurrency, notional
		} else {
			recvCcy, recvAmt = tr.QuoteCurrency, notional
			delvCcy, delvAmt = tr.BaseCurrency, oldTotal
		}
		_, err = tx.PostJournal(ctx, ledger.Journal{
			EntryType:   ledger.EntrySettlement,
			ReferenceID: g.ID,
			Description: fmt.Sprintf("block allocation park reversal group %d", g.ID),
			PostedBy:    "backoffice.allocations",
			IdempotencyKey: fmt.Sprintf("alloc-unpark-group:%d:%s:%s:%s",
				g.ID, oldTotal.String(), oldAvg.String(), oldResid.String()),
			Lines: []ledger.Line{
				ledger.CreditLine(ledger.CustomerLiability(recvCcy), recvCcy, recvAmt, nar),
				ledger.DebitLine(BlockAllocationClearing(recvCcy), recvCcy, recvAmt, nar),
				ledger.DebitLine(ledger.CustomerLiability(delvCcy), delvCcy, delvAmt, nar),
				ledger.CreditLine(BlockAllocationClearing(delvCcy), delvCcy, delvAmt, nar),
			},
		})
		return err
	})
}

// cancelFixGroup withdraws every active allocation of a FIX-ingested
// group (35=J CANCEL committed upstream).
func (e *Engine) cancelFixGroup(ctx context.Context, ref string, a fix.Allocation, prev string) error {
	return e.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		g, err := tx.LockGroupByRef(ctx, ref)
		if err != nil {
			return err
		}
		if g == nil && prev != "" {
			g, err = tx.LockGroupByRef(ctx, fmt.Sprintf("FIX-%s", prev))
		}
		if err != nil {
			return err
		}
		if g == nil {
			return allocErr(CodeAllocationNotFound, "FIX allocation %s has no settlement-side group", ref)
		}
		rows, err := tx.AllocationsTx(ctx, g.ID)
		if err != nil {
			return err
		}
		for _, al := range rows {
			if (al.Kind == KindPrimary || al.Kind == KindReplacement) && al.Status.Active() {
				if err := tx.UpdateAllocationStatus(ctx, al.ID, StatusCancelled, nil,
					"FIX cancel "+a.AllocID, ""); err != nil {
					return err
				}
				if err := tx.AppendEvent(ctx, g.ID, &al.ID, EvCancelled,
					map[string]any{"status": string(al.Status)},
					map[string]any{"status": "CANCELLED", "reason": "FIX CANCEL " + a.AllocID},
					"fix.allocation", nil); err != nil {
					return err
				}
			}
		}
		return tx.SetGroupStatus(ctx, g.ID, GroupCancelled, false)
	})
}

// ---------------------------------------------------------------------------
// Fund confirmation → per-fund settlement + GL rebook + 35=AK.
// ---------------------------------------------------------------------------

// ConfirmResult is one fund confirmation's evidence.
type ConfirmResult struct {
	AllocationID             int64             `json:"allocation_id"`
	SettlementInstructionIDs []int64           `json:"settlement_instruction_ids"`
	ConfirmationRef          string            `json:"confirmation_ref"`
	JournalID                int64             `json:"journal_id"`
	Report                   *quickfix.Message `json:"-"`
}

// ConfirmFund moves an ALLOCATED leg to CLAIMED and executes the
// Task-24.3.15 bundle in one transaction:
//  1. child settlement_instructions — PAY and RECEIVE legs for the fund
//     account, settlement_date inherited from the parent fill
//     (never recomputed); idempotent per (trade, account, currency,
//     direction) — replays attach the existing ids;
//  2. the 2090 rebook journal — the leg's received/delivered currencies
//     move fund liability ↔ clearing (per-currency balanced; GL net zero);
//  3. the per-fund 35=AK report via the ReportEmitter seam;
//  4. the CLAIMED transition + SETTLEMENT_INSTRUCTION/REPORT audit events
//     preserving the block→instruction→child→confirmation→settlement chain.
func (e *Engine) ConfirmFund(ctx context.Context, allocID int64, confirmer string) (*ConfirmResult, error) {
	res := &ConfirmResult{}
	var reportMsg *quickfix.Message
	var meta FundReportMeta
	err := e.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		a, err := tx.LockAllocation(ctx, allocID)
		if err != nil {
			return err
		}
		if a.Status != StatusAllocated {
			return allocErr(CodeAllocationStateConflict,
				"allocation %d status %s — fund confirmation expects ALLOCATED", a.ID, a.Status)
		}
		g, err := tx.LockGroup(ctx, a.GroupID)
		if err != nil {
			return err
		}
		tr, err := tx.LoadTrade(ctx, a.TradeID)
		if err != nil {
			return err
		}
		// Settlement date inherited from the parent fill verbatim.
		settleDate := tr.SettlementDate
		if settleDate == nil {
			d := tr.CreatedAt.UTC().Truncate(24 * time.Hour)
			settleDate = &d
		}
		notional := a.Quantity.Mul(a.AvgPrice).Round(8)
		var recvCcy, delvCcy string
		var recvAmt, delvAmt decimal.Decimal
		if g.Side == '1' { // fund bought base, pays quote
			recvCcy, recvAmt = tr.BaseCurrency, a.Quantity
			delvCcy, delvAmt = tr.QuoteCurrency, notional
		} else { // fund sold base, receives quote
			recvCcy, recvAmt = tr.QuoteCurrency, notional
			delvCcy, delvAmt = tr.BaseCurrency, a.Quantity
		}
		// 1. Child settlement legs (idempotent insert).
		var instrIDs []int64
		for _, leg := range []SettlementLeg{
			{TradeID: a.TradeID, AccountID: a.BeneficiaryAccountID, Currency: recvCcy,
				Amount: recvAmt, Direction: "RECEIVE", SettlementDate: *settleDate},
			{TradeID: a.TradeID, AccountID: a.BeneficiaryAccountID, Currency: delvCcy,
				Amount: delvAmt, Direction: "PAY", SettlementDate: *settleDate},
		} {
			if leg.Direction == "RECEIVE" {
				if n, nerr := tx.ActiveNostro(ctx, leg.Currency); nerr == nil && n > 0 {
					leg.NostroAccountID = &n
				}
			}
			id, inserted, ierr := tx.InsertSettlementLeg(ctx, leg)
			if ierr != nil {
				return ierr
			}
			if inserted {
				instrIDs = append(instrIDs, id)
			}
		}
		if err := tx.AttachSettlementLegs(ctx, a.ID, instrIDs); err != nil {
			return err
		}
		// 2. GL rebook — fund liability ↔ clearing (inverse of the park).
		nar := fmt.Sprintf("block alloc %d fund %d", a.ID, a.BeneficiaryAccountID)
		jid, err := tx.PostJournal(ctx, ledger.Journal{
			EntryType:      ledger.EntrySettlement,
			ReferenceID:    a.TradeID,
			Description:    fmt.Sprintf("block allocation %d group %d fund %d", a.ID, g.ID, a.BeneficiaryAccountID),
			PostedBy:       "backoffice.allocations",
			IdempotencyKey: fmt.Sprintf("alloc-leg:%d", a.ID),
			Lines: []ledger.Line{
				ledger.CreditLine(ledger.CustomerLiability(recvCcy), recvCcy, recvAmt, nar),
				ledger.DebitLine(BlockAllocationClearing(recvCcy), recvCcy, recvAmt, nar),
				ledger.DebitLine(ledger.CustomerLiability(delvCcy), delvCcy, delvAmt, nar),
				ledger.CreditLine(BlockAllocationClearing(delvCcy), delvCcy, delvAmt, nar),
			},
		})
		if err != nil {
			return err
		}
		// 3. Per-fund 35=AK.
		confRef := fmt.Sprintf("AK-%d-%d", g.ID, a.ID)
		reportMsg = BuildFundAllocationReport(g, a)
		meta = FundReportMeta{GroupID: g.ID, GroupRef: g.GroupRef,
			AllocationID: a.ID, AccountID: a.BeneficiaryAccountID,
			PartyID: a.PartyID, PartyLEI: a.PartyLEI}
		// 4. Transition + audit chain.
		now := e.now().UTC()
		if err := tx.UpdateAllocationStatus(ctx, a.ID, StatusClaimed, &now, "", confRef); err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, g.ID, &a.ID, EvClaimed,
			map[string]any{"status": string(StatusAllocated)},
			map[string]any{"status": string(StatusClaimed), "confirmation_ref": confRef},
			confirmer, nil); err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, g.ID, &a.ID, EvSettlementInstruction, nil, map[string]any{
			"instruction_ids": instrIDs, "settlement_date": settleDate.Format("2006-01-02"),
			"journal_id": jid,
		}, "backoffice.allocations", nil); err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, g.ID, &a.ID, EvReportEmitted, nil, map[string]any{
			"confirmation_ref": confRef, "msg_type": "AK",
		}, "backoffice.allocations", nil); err != nil {
			return err
		}
		a.SettlementInstructionIDs = instrIDs
		res.AllocationID = a.ID
		res.SettlementInstructionIDs = instrIDs
		res.ConfirmationRef = confRef
		res.JournalID = jid
		res.Report = reportMsg
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Transport — post-commit, best-effort; the durable confirmation_ref
	// + report event are the record of truth.
	if e.reports != nil && reportMsg != nil {
		if ref, rerr := e.reports.EmitAllocationReport(ctx, reportMsg, meta); rerr == nil && ref != "" {
			res.ConfirmationRef = ref
		}
	}
	return res, nil
}

// BuildFundAllocationReport generates the per-fund 35=AK — the FIX
// AllocationReport projected onto this leg (Tag 78 NoAllocs=1). The
// parent fix.Allocation row fields (AllocID/session) live on the 226
// record; the settlement-side report keys on the group ref.
func BuildFundAllocationReport(g *Group, a *Allocation) *quickfix.Message {
	fa := fix.Allocation{
		ID:        g.FixAllocationID,
		AllocID:   g.GroupRef,
		TransType: fix.AllocTransNew,
		Status:    fix.AllocStatusAccepted,
		Side:      g.Side,
		ExecQty:   a.Quantity,
	}
	legs := []fix.AllocationLeg{{
		LegNo: 1, AllocAccount: a.AllocAccountRef,
		AllocQty: a.Quantity, AllocPrice: a.AvgPrice,
		ChildExecID: fmt.Sprintf("ALLOC-%d-%d", g.ID, a.ID),
	}}
	if legs[0].AllocAccount == "" {
		legs[0].AllocAccount = fmt.Sprint(a.BeneficiaryAccountID)
	}
	return fix.BuildAllocationReport(fa, legs)
}

// RejectFund records a fund-operations rejection — the quantity returns
// to the group's unallocated remainder (the T+0 sweep still sees it).
func (e *Engine) RejectFund(ctx context.Context, allocID int64, confirmer, reason string) (*Allocation, error) {
	return e.svc.Reject(ctx, allocID, confirmer, reason)
}

// ---------------------------------------------------------------------------
// Amendment after partial settlement.
// ---------------------------------------------------------------------------

// AmendAllocation is the partial-settlement-safe correction path
// (spec §17.8 edge case): a leg whose settlement instructions already
// SETTLED/RECONCILED is final — refuse; a leg with only PENDING legs is
// VOIDed (its GL rebook reversed via the unpark journal inside Correct)
// then corrected under the normal rules (dual control once locked).
func (e *Engine) AmendAllocation(ctx context.Context, actor Actor, allocID int64, in CorrectInput) (*Correction, error) {
	// Gate on instruction state first — a SETTLED leg can never unwind.
	err := e.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		settled, pending, err := tx.AllocationLegCounts(ctx, allocID)
		if err != nil {
			return err
		}
		if settled > 0 {
			return allocErr(CodeAllocationStateConflict,
				"allocation %d has %d settled instruction(s) — post-settlement amendment refused; open a settlement exception instead",
				allocID, settled)
		}
		if pending > 0 {
			if _, err := tx.VoidAllocationInstructions(ctx, allocID, "amended"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e.svc.Correct(ctx, actor, allocID, in)
}

// ---------------------------------------------------------------------------
// T+0 EOD escalation sweep.
// ---------------------------------------------------------------------------

// EscalateResult reports one sweep run.
type EscalateResult struct {
	Scanned   int     `json:"scanned"`
	Escalated int     `json:"escalated"`
	GroupIDs  []int64 `json:"group_ids"`
}

// EscalateUnallocated sweeps groups still holding unallocated filled
// quantity at the T+0 end-of-day cutoff: each lands a durable
// funding_ops_alerts P2 row (code UNALLOCATED_BLOCK_TRADE), a paging
// OpsAlert, the escalated_at latch and the audit event — idempotent on
// repeat sweeps.
func (e *Engine) EscalateUnallocated(ctx context.Context, cutoff time.Time) (*EscalateResult, error) {
	if cutoff.IsZero() {
		cutoff = e.now().UTC()
	}
	res := &EscalateResult{}
	err := e.store.InTx(ctx, func(ctx context.Context, tx AllocationTx) error {
		groups, err := tx.UnallocatedGroups(ctx, cutoff)
		if err != nil {
			return err
		}
		res.Scanned = len(groups)
		for _, g := range groups {
			unalloc := g.TotalQty.Sub(g.AllocatedQty)
			if !unalloc.IsPositive() {
				continue
			}
			detail, _ := json.Marshal(map[string]any{
				"group_id": g.ID, "group_ref": g.GroupRef,
				"manager_account_id": g.ManagerAccountID,
				"unallocated_qty":    unalloc.String(), "total_qty": g.TotalQty.String(),
				"dedup_key": fmt.Sprintf("unallocated-block-%d", g.ID),
			})
			if _, err := tx.InsertAlert(ctx, OpsAlertRow{
				Code:     CodeUnallocatedBlockTrade,
				Severity: "P2",
				Amount:   &unalloc,
				Summary: fmt.Sprintf(
					"block group %d (%s) unallocated %s of %s past T+0 EOD",
					g.ID, g.GroupRef, unalloc, g.TotalQty),
				Detail: detail,
			}); err != nil {
				return err
			}
			if err := tx.MarkGroupEscalated(ctx, g.ID, cutoff); err != nil {
				return err
			}
			if err := tx.AppendEvent(ctx, g.ID, nil, EvEscalated, nil, map[string]any{
				"unallocated_qty": unalloc.String(), "severity": "P2",
				"code": CodeUnallocatedBlockTrade,
			}, "backoffice.allocations", nil); err != nil {
				return err
			}
			res.GroupIDs = append(res.GroupIDs, g.ID)
		}
		res.Escalated = len(res.GroupIDs)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Page post-commit — durable rows are the record of truth.
	if e.alerter != nil {
		for _, gid := range res.GroupIDs {
			_ = e.alerter.Raise(ctx, OpsAlert{
				Code:    CodeUnallocatedBlockTrade,
				Summary: fmt.Sprintf("T+0 EOD: block allocation group %d still unallocated", gid),
				Details: map[string]string{"group_id": fmt.Sprint(gid), "severity": "P2"},
			})
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// PgAllocStore — migration-055 persistence over pgx.
// ---------------------------------------------------------------------------

// LedgerTxPoster is the tx-scoped §5.3 GL seam — settlement.LedgerService
// (DoubleEntryLedgerService) satisfies it. Nil fails the GL paths closed.
type LedgerTxPoster interface {
	PostJournal(ctx context.Context, tx pgx.Tx, j ledger.Journal) (ledger.PostResult, error)
}

// PgAllocStore implements AllocationStore over pgx.
type PgAllocStore struct {
	pool   *pgxpool.Pool
	q      querier // pool or pgx.Tx
	ledger LedgerTxPoster
}

// NewPgAllocStore binds the store; pool is required, ledger may be nil
// (GL paths then fail closed with SERVICE_DEGRADED).
func NewPgAllocStore(pool *pgxpool.Pool, ledgerSvc LedgerTxPoster) (*PgAllocStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("backoffice: nil pgx pool")
	}
	return &PgAllocStore{pool: pool, q: pool, ledger: ledgerSvc}, nil
}

// InTx runs fn inside one SERIALIZABLE transaction.
func (s *PgAllocStore) InTx(ctx context.Context, fn func(ctx context.Context, tx AllocationTx) error) error {
	if s.pool == nil {
		return excerrors.New(CodeServiceDegraded, "backoffice: InTx on tx-scoped store")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, &PgAllocStore{q: tx, ledger: s.ledger}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Row scanners.
// ---------------------------------------------------------------------------

const allocGroupCols = `id, group_ref, manager_account_id, instrument_id, side,
	capacity::text, allocation_method::text, status::text, total_qty, allocated_qty,
	COALESCE(avg_price,0), price_residual, settlement_locked, source::text,
	COALESCE(fix_allocation_id,0), escalated_at, COALESCE(created_by,0),
	created_at, updated_at, submitted_at`

func scanAllocGroup(row pgx.Row) (*Group, error) {
	var g Group
	var side string
	if err := row.Scan(&g.ID, &g.GroupRef, &g.ManagerAccountID, &g.InstrumentID, &side,
		&g.Capacity, &g.Method, &g.Status, &g.TotalQty, &g.AllocatedQty,
		&g.AvgPrice, &g.PriceResidual, &g.SettlementLocked, &g.Source,
		&g.FixAllocationID, &g.EscalatedAt, &g.CreatedBy,
		&g.CreatedAt, &g.UpdatedAt, &g.SubmittedAt); err != nil {
		return nil, err
	}
	if len(side) > 0 {
		g.Side = side[0]
	}
	return &g, nil
}

const allocRowCols = `id, trade_id, group_id, beneficiary_account_id, quantity, avg_price,
	status::text, kind::text, corrects_allocation_id, COALESCE(party_id,''), COALESCE(party_lei,''),
	COALESCE(alloc_account_ref,''), COALESCE(confirmation_ref,''), settlement_instruction_ids,
	claimed_at, COALESCE(rejected_reason,''), created_at, updated_at`

func scanAllocRow(row pgx.Row) (*Allocation, error) {
	var a Allocation
	var ids []byte
	if err := row.Scan(&a.ID, &a.TradeID, &a.GroupID, &a.BeneficiaryAccountID,
		&a.Quantity, &a.AvgPrice, &a.Status, &a.Kind, &a.CorrectsAllocationID,
		&a.PartyID, &a.PartyLEI, &a.AllocAccountRef, &a.ConfirmationRef,
		&ids, &a.ClaimedAt, &a.RejectedReason, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.SettlementInstructionIDs = []int64{}
	if len(ids) > 0 {
		_ = json.Unmarshal(ids, &a.SettlementInstructionIDs)
	}
	return &a, nil
}

// ---------------------------------------------------------------------------
// Read surface.
// ---------------------------------------------------------------------------

// Group loads one group (nil when absent).
func (s *PgAllocStore) Group(ctx context.Context, id int64) (*Group, error) {
	g, err := scanAllocGroup(s.q.QueryRow(ctx,
		`SELECT `+allocGroupCols+` FROM average_price_groups WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, nil
	}
	return g, nil
}

// GroupByRef resolves the idempotency key (nil when absent).
func (s *PgAllocStore) GroupByRef(ctx context.Context, ref string) (*Group, error) {
	g, err := scanAllocGroup(s.q.QueryRow(ctx,
		`SELECT `+allocGroupCols+` FROM average_price_groups WHERE group_ref=$1`, ref))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return g, err
}

// Fills lists a group's attached fills (ascending trade id — the
// deterministic spread order).
func (s *PgAllocStore) Fills(ctx context.Context, groupID int64) ([]Fill, error) {
	return s.fillsAt(ctx, s.q, groupID)
}

func (s *PgAllocStore) fillsAt(ctx context.Context, q querier, groupID int64) ([]Fill, error) {
	rows, err := q.Query(ctx, `
		SELECT id, group_id, trade_id, account_id, quantity, price, settlement_date
		  FROM average_price_group_fills WHERE group_id=$1 ORDER BY trade_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Fill{}
	for rows.Next() {
		var f Fill
		if err := rows.Scan(&f.ID, &f.GroupID, &f.TradeID, &f.AccountID,
			&f.Quantity, &f.Price, &f.SettlementDate); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Allocations lists a group's rows (id order — history preserved).
func (s *PgAllocStore) Allocations(ctx context.Context, groupID int64) ([]Allocation, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+allocRowCols+` FROM trade_allocations WHERE group_id=$1 ORDER BY id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Allocation{}
	for rows.Next() {
		a, err := scanAllocRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// EligibleAccounts lists a group's beneficiary registry.
func (s *PgAllocStore) EligibleAccounts(ctx context.Context, groupID int64) ([]EligibleAccount, error) {
	rows, err := s.q.Query(ctx, `
		SELECT beneficiary_account_id, capacity::text, COALESCE(weight,0),
		       COALESCE(party_id,''), COALESCE(party_lei,''), status
		  FROM average_price_group_accounts WHERE group_id=$1
		  ORDER BY beneficiary_account_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EligibleAccount{}
	for rows.Next() {
		var e EligibleAccount
		if err := rows.Scan(&e.AccountID, &e.Capacity, &e.Weight,
			&e.PartyID, &e.PartyLEI, &e.Status); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Events returns the immutable audit chain (seq order).
func (s *PgAllocStore) Events(ctx context.Context, groupID int64) ([]Event, error) {
	rows, err := s.q.Query(ctx, `
		SELECT id, group_id, allocation_id, seq, event_type, before_state, after_state,
		       COALESCE(actor,''), approved_by, created_at
		  FROM trade_allocation_events WHERE group_id=$1 ORDER BY seq`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.GroupID, &e.AllocationID, &e.Seq, &e.Type,
			&e.Before, &e.After, &e.Actor, &e.ApprovedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Tx surface — groups.
// ---------------------------------------------------------------------------

// InsertGroup writes the row (unique group_ref replays surface 23505 →
// the caller resolves via LockGroupByRef).
func (s *PgAllocStore) InsertGroup(ctx context.Context, g *Group) error {
	return s.q.QueryRow(ctx, `
		INSERT INTO average_price_groups
		 (group_ref, manager_account_id, instrument_id, side, capacity,
		  allocation_method, status, source, fix_allocation_id, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,0),NULLIF($10,0))
		 RETURNING id, created_at, updated_at`,
		g.GroupRef, g.ManagerAccountID, g.InstrumentID, string(g.Side),
		string(g.Capacity), string(g.Method), string(g.Status), string(g.Source),
		g.FixAllocationID, g.CreatedBy).Scan(&g.ID, &g.CreatedAt, &g.UpdatedAt)
}

// LockGroup SELECT ... FOR UPDATE.
func (s *PgAllocStore) LockGroup(ctx context.Context, id int64) (*Group, error) {
	g, err := scanAllocGroup(s.q.QueryRow(ctx,
		`SELECT `+allocGroupCols+` FROM average_price_groups WHERE id=$1 FOR UPDATE`, id))
	if err == pgx.ErrNoRows {
		return nil, allocErr(CodeAllocationNotFound, "average-price group %d not found", id)
	}
	return g, err
}

// LockGroupByRef locks by idempotency ref; nil when the group doesn't exist.
func (s *PgAllocStore) LockGroupByRef(ctx context.Context, ref string) (*Group, error) {
	g, err := scanAllocGroup(s.q.QueryRow(ctx,
		`SELECT `+allocGroupCols+` FROM average_price_groups WHERE group_ref=$1 FOR UPDATE`, ref))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return g, err
}

// UpdateGroupComputed persists the deterministic recompute — allocated_qty
// is re-derived from active children so the row can never drift.
func (s *PgAllocStore) UpdateGroupComputed(ctx context.Context, id int64, totalQty, avgPrice, residual decimal.Decimal) error {
	_, err := s.q.Exec(ctx, `
		UPDATE average_price_groups SET
		    total_qty=$2, avg_price=$3, price_residual=$4,
		    allocated_qty=(SELECT COALESCE(SUM(quantity),0) FROM trade_allocations
		                   WHERE group_id=$1 AND kind IN ('PRIMARY','REPLACEMENT')
		                     AND status IN ('PENDING','ALLOCATED','CLAIMED','SETTLED')),
		    updated_at=now()
		 WHERE id=$1`, id, totalQty.String(), avgPrice.String(), residual.String())
	return err
}

// SetGroupStatus transitions status + lock latch.
func (s *PgAllocStore) SetGroupStatus(ctx context.Context, id int64, status GroupStatus, locked bool) error {
	sub := "NULL"
	if status == GroupLocked {
		sub = "now()"
	}
	_, err := s.q.Exec(ctx, `
		UPDATE average_price_groups SET status=$2::avg_group_status_enum,
		    settlement_locked=$3, submitted_at=COALESCE(`+sub+`::timestamptz, submitted_at),
		    updated_at=now()
		 WHERE id=$1`, id, string(status), locked)
	return err
}

// MarkGroupEscalated stamps the T+0 escalation latch.
func (s *PgAllocStore) MarkGroupEscalated(ctx context.Context, id int64, at time.Time) error {
	_, err := s.q.Exec(ctx,
		`UPDATE average_price_groups SET escalated_at=$2, updated_at=now() WHERE id=$1`, id, at)
	return err
}

// SetGroupFixRef records the fix_allocations.id (idempotent).
func (s *PgAllocStore) SetGroupFixRef(ctx context.Context, id, fixAllocID int64) error {
	_, err := s.q.Exec(ctx,
		`UPDATE average_price_groups SET fix_allocation_id=$2, updated_at=now() WHERE id=$1`,
		id, fixAllocID)
	return err
}

// ---------------------------------------------------------------------------
// Tx surface — eligibility + fills.
// ---------------------------------------------------------------------------

// InsertEligible upserts the registry row — REST/FIX intakes re-drive the
// same (group, account) pair idempotently.
func (s *PgAllocStore) InsertEligible(ctx context.Context, groupID int64, e EligibleAccount) error {
	status := e.Status
	if status == "" {
		status = "ELIGIBLE"
	}
	_, err := s.q.Exec(ctx, `
		INSERT INTO average_price_group_accounts
		 (group_id, beneficiary_account_id, capacity, weight, party_id, party_lei, status)
		 VALUES ($1,$2,$3,NULLIF($4::numeric,0),NULLIF($5,''),NULLIF($6,''),$7)
		 ON CONFLICT (group_id, beneficiary_account_id) DO UPDATE SET
		    weight   = COALESCE(EXCLUDED.weight, average_price_group_accounts.weight),
		    party_id = COALESCE(EXCLUDED.party_id, average_price_group_accounts.party_id),
		    party_lei= COALESCE(EXCLUDED.party_lei, average_price_group_accounts.party_lei)`,
		groupID, e.AccountID, string(e.Capacity), e.Weight.String(), e.PartyID, e.PartyLEI, status)
	return err
}

// EligibleAccountsTx is EligibleAccounts inside the tx.
func (s *PgAllocStore) EligibleAccountsTx(ctx context.Context, groupID int64) ([]EligibleAccount, error) {
	return s.EligibleAccounts(ctx, groupID)
}

// SetEligibleStatus flips the registry status.
func (s *PgAllocStore) SetEligibleStatus(ctx context.Context, groupID, accountID int64, status string) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE average_price_group_accounts SET status=$3
		 WHERE group_id=$1 AND beneficiary_account_id=$2`, groupID, accountID, status)
	if err == nil && tag.RowsAffected() == 0 {
		return allocErr(CodeAllocationNotFound,
			"eligible account %d not on group %d", accountID, groupID)
	}
	return err
}

// LoadTrade reads the trades row joined to its instrument currencies —
// partitioned parent, id-keyed read.
func (s *PgAllocStore) LoadTrade(ctx context.Context, tradeID int64) (*Trade, error) {
	var t Trade
	var settleDate *time.Time
	err := s.q.QueryRow(ctx, `
		SELECT t.id, t.instrument_id, i.symbol, i.base_currency, i.quote_currency,
		       COALESCE(i.settlement_cycle,2), t.buyer_account_id, t.seller_account_id,
		       t.price, t.quantity, t.settlement_date, t.status::text, t.created_at
		  FROM trades t JOIN instruments i ON i.id = t.instrument_id
		 WHERE t.id=$1`, tradeID).Scan(
		&t.ID, &t.InstrumentID, &t.Symbol, &t.BaseCurrency, &t.QuoteCurrency,
		&t.SettlementCycle, &t.BuyerAccountID, &t.SellerAccountID,
		&t.Price, &t.Quantity, &settleDate, &t.Status, &t.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, allocErr(CodeAllocationNotFound, "trade %d not found", tradeID)
	}
	if err != nil {
		return nil, err
	}
	t.SettlementDate = settleDate
	return &t, nil
}

// FillGroup resolves a fill's existing group membership (idempotency).
func (s *PgAllocStore) FillGroup(ctx context.Context, tradeID int64) (int64, bool, error) {
	var gid int64
	err := s.q.QueryRow(ctx,
		`SELECT group_id FROM average_price_group_fills WHERE trade_id=$1`, tradeID).Scan(&gid)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return gid, true, nil
}

// InsertFill attaches a fill to the group (UNIQUE trade_id refuses
// double-grouping); side picks the manager-account column.
func (s *PgAllocStore) InsertFill(ctx context.Context, groupID int64, side byte, t *Trade) error {
	acct := t.BuyerAccountID
	if side == '2' {
		acct = t.SellerAccountID
	}
	_, err := s.q.Exec(ctx, `
		INSERT INTO average_price_group_fills
		 (group_id, trade_id, account_id, quantity, price, settlement_date)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		groupID, t.ID, acct, t.Quantity.String(), t.Price.String(), t.SettlementDate)
	return err
}

// GroupFillsTx lists fills inside the tx.
func (s *PgAllocStore) GroupFillsTx(ctx context.Context, groupID int64) ([]Fill, error) {
	return s.fillsAt(ctx, s.q, groupID)
}

// ---------------------------------------------------------------------------
// Tx surface — allocations.
// ---------------------------------------------------------------------------

// InsertAllocation writes one leg row (conservation trigger backs the
// caller's math).
func (s *PgAllocStore) InsertAllocation(ctx context.Context, a *Allocation) error {
	ids := []byte("[]")
	if len(a.SettlementInstructionIDs) > 0 {
		ids, _ = json.Marshal(a.SettlementInstructionIDs)
	}
	return s.q.QueryRow(ctx, `
		INSERT INTO trade_allocations
		 (trade_id, group_id, beneficiary_account_id, quantity, avg_price,
		  status, kind, corrects_allocation_id, party_id, party_lei,
		  alloc_account_ref, settlement_instruction_ids)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),$12)
		 RETURNING id, created_at, updated_at`,
		a.TradeID, a.GroupID, a.BeneficiaryAccountID, a.Quantity.String(), a.AvgPrice.String(),
		string(a.Status), string(a.Kind), a.CorrectsAllocationID, a.PartyID, a.PartyLEI,
		a.AllocAccountRef, ids).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
}

// LockAllocation SELECT ... FOR UPDATE.
func (s *PgAllocStore) LockAllocation(ctx context.Context, id int64) (*Allocation, error) {
	a, err := scanAllocRow(s.q.QueryRow(ctx,
		`SELECT `+allocRowCols+` FROM trade_allocations WHERE id=$1 FOR UPDATE`, id))
	if err == pgx.ErrNoRows {
		return nil, allocErr(CodeAllocationNotFound, "allocation %d not found", id)
	}
	return a, err
}

// AllocationsTx lists rows inside the tx.
func (s *PgAllocStore) AllocationsTx(ctx context.Context, groupID int64) ([]Allocation, error) {
	return s.Allocations(ctx, groupID)
}

// ActiveFillQty is Σ active allocations on one fill.
func (s *PgAllocStore) ActiveFillQty(ctx context.Context, groupID, tradeID int64) (decimal.Decimal, error) {
	var str string
	err := s.q.QueryRow(ctx, `
		SELECT COALESCE(SUM(quantity),0)::text FROM trade_allocations
		 WHERE group_id=$1 AND trade_id=$2 AND kind IN ('PRIMARY','REPLACEMENT')
		   AND status IN ('PENDING','ALLOCATED','CLAIMED','SETTLED')`,
		groupID, tradeID).Scan(&str)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.RequireFromString(str), nil
}

// UpdateAllocationStatus transitions a leg row.
func (s *PgAllocStore) UpdateAllocationStatus(ctx context.Context, id int64, status Status, claimedAt *time.Time, reason, confRef string) error {
	_, err := s.q.Exec(ctx, `
		UPDATE trade_allocations SET status=$2::trade_alloc_status_enum,
		    claimed_at=COALESCE($3, claimed_at), rejected_reason=NULLIF($4,''),
		    confirmation_ref=COALESCE(NULLIF($5,''), confirmation_ref), updated_at=now()
		 WHERE id=$1`, id, string(status), claimedAt, reason, confRef)
	return err
}

// AttachSettlementLegs merges instruction ids onto the row (deduped).
func (s *PgAllocStore) AttachSettlementLegs(ctx context.Context, id int64, instructionIDs []int64) error {
	if len(instructionIDs) == 0 {
		return nil
	}
	sort.Slice(instructionIDs, func(i, j int) bool { return instructionIDs[i] < instructionIDs[j] })
	b, _ := json.Marshal(instructionIDs)
	_, err := s.q.Exec(ctx, `
		UPDATE trade_allocations SET settlement_instruction_ids =
		    (SELECT COALESCE(jsonb_agg(DISTINCT v), '[]'::jsonb)
		       FROM (SELECT jsonb_array_elements_text(settlement_instruction_ids)::bigint AS v
		             UNION SELECT jsonb_array_elements_text($2::jsonb)::bigint) u),
		    updated_at=now()
		 WHERE id=$1`, id, b)
	return err
}

// AllocationLegCounts reports settled vs pending instruction legs.
func (s *PgAllocStore) AllocationLegCounts(ctx context.Context, allocID int64) (int, int, error) {
	var settled, pending int
	err := s.q.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN si.status IN ('SETTLED','RECONCILED') THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN si.status = 'PENDING' THEN 1 ELSE 0 END),0)
		  FROM trade_allocations ta
		  LEFT JOIN LATERAL jsonb_array_elements_text(ta.settlement_instruction_ids) AS v(id)
		    ON TRUE
		  LEFT JOIN settlement_instructions si ON si.id = v.id::bigint
		 WHERE ta.id=$1`, allocID).Scan(&settled, &pending)
	return settled, pending, err
}

// VoidAllocationInstructions flips the leg's PENDING settlement
// instructions to VOID (reversal terminal — SETTLED legs untouched).
func (s *PgAllocStore) VoidAllocationInstructions(ctx context.Context, allocID int64, ref string) (int, error) {
	tag, err := s.q.Exec(ctx, `
		UPDATE settlement_instructions si SET status='VOID'
		 WHERE si.status='PENDING' AND si.id IN (
		    SELECT jsonb_array_elements_text(settlement_instruction_ids)::bigint
		      FROM trade_allocations WHERE id=$1)`, allocID)
	return int(tag.RowsAffected()), err
}

// ---------------------------------------------------------------------------
// Tx surface — hierarchy / nostro / settlement legs / GL / audit.
// ---------------------------------------------------------------------------

// AccountInHierarchy reports whether accountID sits under masterID
// (accounts.parent_account_id chain — depth-bounded recursive walk).
func (s *PgAllocStore) AccountInHierarchy(ctx context.Context, masterID, accountID int64) (bool, error) {
	if accountID == masterID {
		return true, nil
	}
	var ok bool
	err := s.q.QueryRow(ctx, `
		WITH RECURSIVE walk AS (
		    SELECT id, parent_account_id, 0 AS depth FROM accounts WHERE id=$2
		    UNION ALL
		    SELECT a.id, a.parent_account_id, w.depth+1
		      FROM accounts a JOIN walk w ON a.id = w.parent_account_id
		     WHERE w.depth < 8)
		SELECT EXISTS(SELECT 1 FROM walk WHERE id=$1)`, masterID, accountID).Scan(&ok)
	return ok, err
}

// ActiveNostro resolves the operating nostro for a currency (0 when none).
func (s *PgAllocStore) ActiveNostro(ctx context.Context, currency string) (int64, error) {
	var id int64
	err := s.q.QueryRow(ctx, `
		SELECT id FROM nostro_accounts
		 WHERE currency=$1 AND status='ACTIVE' ORDER BY id LIMIT 1`, currency).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// InsertSettlementLeg writes one child settlement_instructions row —
// idempotent on the natural key (trade, account, currency, direction) so
// a replayed confirm attaches the existing leg instead of duplicating.
func (s *PgAllocStore) InsertSettlementLeg(ctx context.Context, leg SettlementLeg) (int64, bool, error) {
	var existing int64
	err := s.q.QueryRow(ctx, `
		SELECT id FROM settlement_instructions
		 WHERE trade_id=$1 AND account_id=$2 AND currency=$3 AND direction=$4
		   AND status <> 'VOID'`, leg.TradeID, leg.AccountID, leg.Currency, leg.Direction).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if err != pgx.ErrNoRows {
		return 0, false, err
	}
	var id int64
	err = s.q.QueryRow(ctx, `
		INSERT INTO settlement_instructions
		 (trade_id, account_id, currency, amount, direction, settlement_date, nostro_account_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		leg.TradeID, leg.AccountID, leg.Currency, leg.Amount.String(),
		leg.Direction, leg.SettlementDate, leg.NostroAccountID).Scan(&id)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// PostJournal routes a balanced journal through the tx-scoped §5.3 write
// path — the injected LedgerTxPoster (settlement.LedgerService) owns
// validation, account resolution and the zero-sum re-verify. A nil poster
// fails closed.
func (s *PgAllocStore) PostJournal(ctx context.Context, j ledger.Journal) (int64, error) {
	if s.ledger == nil {
		return 0, excerrors.New(CodeServiceDegraded,
			"allocation GL posting requires a ledger service — none configured")
	}
	tx, ok := s.q.(pgx.Tx)
	if !ok {
		return 0, excerrors.New("INTERNAL_ERROR", "allocation PostJournal outside a transaction")
	}
	res, err := s.ledger.PostJournal(ctx, tx, j)
	if err != nil {
		return 0, err
	}
	return res.JournalID, nil
}

// AppendEvent writes the immutable audit row (seq per group).
func (s *PgAllocStore) AppendEvent(ctx context.Context, groupID int64, allocID *int64, typ string,
	before, after any, actor string, approvedBy *int64) error {

	var beforeB, afterB []byte
	var err error
	if before != nil {
		if rb, ok := before.(json.RawMessage); ok {
			beforeB = rb
		} else if beforeB, err = json.Marshal(before); err != nil {
			return err
		}
	}
	if rb, ok := after.(json.RawMessage); ok {
		afterB = rb
	} else if afterB, err = json.Marshal(after); err != nil {
		return err
	}
	_, err = s.q.Exec(ctx, `
		INSERT INTO trade_allocation_events
		 (group_id, allocation_id, seq, event_type, before_state, after_state, actor, approved_by)
		 VALUES ($1,$2,
		    COALESCE((SELECT MAX(seq) FROM trade_allocation_events WHERE group_id=$1),0)+1,
		    $3,$4,$5,NULLIF($6,''),$7)`,
		groupID, allocID, typ, beforeB, afterB, actor, approvedBy)
	return err
}

// UnallocatedGroups lists non-terminal groups still holding unallocated
// filled quantity before the cutoff, not yet escalated. A
// SETTLEMENT_LOCKED group is scanned too — submitting a group to
// settlement does not discharge the T+0 full-allocation obligation, it
// only forces dual-control on corrections (Task 24.3.15).
func (s *PgAllocStore) UnallocatedGroups(ctx context.Context, cutoff time.Time) ([]Group, error) {
	rows, err := s.q.Query(ctx, `
		SELECT `+allocGroupCols+` FROM average_price_groups
		 WHERE status IN ('OPEN','ALLOCATED','SETTLEMENT_LOCKED')
		   AND escalated_at IS NULL
		   AND created_at <= $1
		 ORDER BY id`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		var side string
		if err := rows.Scan(&g.ID, &g.GroupRef, &g.ManagerAccountID, &g.InstrumentID, &side,
			&g.Capacity, &g.Method, &g.Status, &g.TotalQty, &g.AllocatedQty,
			&g.AvgPrice, &g.PriceResidual, &g.SettlementLocked, &g.Source,
			&g.FixAllocationID, &g.EscalatedAt, &g.CreatedBy,
			&g.CreatedAt, &g.UpdatedAt, &g.SubmittedAt); err != nil {
			return nil, err
		}
		if len(side) > 0 {
			g.Side = side[0]
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// InsertAlert lands a durable funding_ops_alerts row.
func (s *PgAllocStore) InsertAlert(ctx context.Context, a OpsAlertRow) (int64, error) {
	var id int64
	var amt *string
	if a.Amount != nil {
		s := a.Amount.String()
		amt = &s
	}
	err := s.q.QueryRow(ctx, `
		INSERT INTO funding_ops_alerts (code, severity, account_id, currency, amount, summary, detail)
		 VALUES ($1,$2,NULL,$3,$4,$5,$6) RETURNING id`,
		a.Code, a.Severity, a.Currency, amt, a.Summary, a.Detail).Scan(&id)
	return id, err
}
