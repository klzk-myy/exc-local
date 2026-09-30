// Package backoffice implements the Phase-24 backoffice & settlement-ops
// surface (spec §17): failed-settlement exceptions (Task 24.3.6),
// prime-broker give-up reconciliation & break management (Task 24.3.7),
// CSDR settlement discipline retained for CSD scope while FX legs use the
// §17.14 close-out/fail-interest economics (Tasks 24.3.13 + 24.3.19),
// PB credit restitution on settlement failure (Task 24.3.14) and the
// settlement-ops hardening machinery — break aging, write-off authority
// matrix, nostro funding thresholds, rail cut-off matrix, CLS pay-in ops,
// rail failover, Herstatt exposure and the LP-default playbook
// (Task 24.3.19).
//
// Conventions (repo rules): money is shopspring decimal end-to-end
// (string on the wire); all state transitions are SERIALIZABLE and
// idempotent; a nil store/dependency fails closed with SERVICE_DEGRADED;
// emitted error codes are registered on errs.Default in init() (the
// localRow branch of the Task 5.3.21 registry — each row cites its
// owning Phase-24 task).
package backoffice

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/errs"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Error codes — registered on errs.Default via init (registry localRow
// branch; each carries its owning "Phase-24 Task" citation).
// ---------------------------------------------------------------------------

const (
	// CodeExceptionConflict — 409: illegal exception state transition or a
	// resolve attempted on a non-actionable exception (Task 24.3.6).
	CodeExceptionConflict = "SETTLEMENT_EXCEPTION_CONFLICT"
	// CodeReversalFailed — 500: settlement reversal failed mid-posting
	// (Task 24.3.6; the tx rolls back — nothing partial lands).
	CodeReversalFailed = "SETTLEMENT_REVERSAL_FAILED"
	// CodeBreakConflict — 409: PB give-up break already resolved or illegal
	// transition (Task 24.3.7).
	CodeBreakConflict = "PB_RECON_BREAK_CONFLICT"
	// CodeRestitutionBlocked — 409: credit restitution refused because the
	// failed trade was already replaced/allocated (Task 24.3.14).
	CodeRestitutionBlocked = "SETTLEMENT_RESTITUTION_BLOCKED"
	// CodeBuyInTriggered — 500: mandatory buy-in dispatch degraded
	// (Task 24.3.13; named by the spec §27.1 CSDR matrix row, 500/L1-L2).
	CodeBuyInTriggered = "BUY_IN_TRIGGERED"
	// CodeWriteOffAuthorityExceeded — 403: write-off amount above the
	// maker's §17.14.1 authority-matrix tier (Task 24.3.19).
	CodeWriteOffAuthorityExceeded = "WRITE_OFF_AUTHORITY_EXCEEDED"
	// CodeNostroFundingBreach — 500: nostro balance below the 3-day+CLS
	// funding threshold; rides the ops-alert payload (Task 24.3.19).
	CodeNostroFundingBreach = "NOSTRO_FUNDING_BREACH"
	// CodeHerstattLimitBreach — 500: principal exposure open past its
	// duration cap (Task 24.3.19; alert payload code).
	CodeHerstattLimitBreach = "HERSTATT_LIMIT_BREACH"
	// CodeRailFailoverExhausted — 503: queued-payment retry timetable
	// (15m/1h/4h) exhausted without dispatch (Task 24.3.19).
	CodeRailFailoverExhausted = "RAIL_FAILOVER_EXHAUSTED"
	// CodeSuspenseSLABreach — 500: suspense item past its 2-day clearing
	// SLA (Task 24.3.19; alert payload code).
	CodeSuspenseSLABreach = "SUSPENSE_SLA_BREACH"

	// CodeSettlementFailed is the Task 24.3.14 event name carried on the
	// restitution trigger — an audit/bus event token like CTR_TRIGGERED,
	// never an HTTP emission, so it is intentionally NOT registered.
	CodeSettlementFailed = "SETTLEMENT_FAILED"

	// CodeServiceDegraded is the registered fail-closed code emitted for
	// unconfigured seams (nil store/seam → 503 SERVICE_DEGRADED).
	CodeServiceDegraded = "SERVICE_DEGRADED"
)

// Dual-control operation names (Phase-07 DualControlService queue). The Op*
// constants live in internal/admin; these strings ride the same queue via
// RegisterExecutor — declared here because internal/admin is upstream-only.
const (
	OpSettlementExceptionResolve = "settlement-exception-resolve" // Task 24.3.6
	OpSettlementWriteOff         = "settlement-write-off"         // Task 24.3.19
	OpPBBreakResolve             = "pb-break-resolve"             // Task 24.3.7
)

func init() {
	for _, d := range []errs.CodeDef{
		{Code: CodeExceptionConflict, HTTPStatus: 409, Owner: "Phase-24 Task 24.3.6",
			Description: "Settlement exception resolve refused — illegal state transition or already resolved (spec §17.1, §17.14)"},
		{Code: CodeReversalFailed, HTTPStatus: 500, Owner: "Phase-24 Task 24.3.6",
			Description: "Settlement reversal posting failed mid-transaction; rolled back atomically (spec §17.1)"},
		{Code: CodeBreakConflict, HTTPStatus: 409, Owner: "Phase-24 Task 24.3.7",
			Description: "PB give-up break already resolved or transition illegal under the PENDING→AFFIRMED/REJECTED/DISPUTED workflow (spec §5.22)"},
		{Code: CodeRestitutionBlocked, HTTPStatus: 409, Owner: "Phase-24 Task 24.3.14",
			Description: "PB credit restitution refused — the failed trade was already replaced or allocated (spec §13.7 extension, §24 #200)"},
		{Code: CodeBuyInTriggered, HTTPStatus: 500, Owner: "Phase-24 Task 24.3.13",
			Description: "Mandatory buy-in notification/execution dispatch degraded (spec §27.1 CSDR matrix; CSD scope only — FX legs use §17.14 close-out)"},
		{Code: CodeWriteOffAuthorityExceeded, HTTPStatus: 403, Owner: "Phase-24 Task 24.3.19",
			Description: "Settlement write-off amount exceeds the maker's authority-matrix tier (§17.14.1; T1 Finance Ops / T2 Risk Manager / T3 Super Admin)"},
		{Code: CodeNostroFundingBreach, HTTPStatus: 500, Owner: "Phase-24 Task 24.3.19",
			Description: "Nostro balance below the 3-day-outflow + CLS pay-in funding threshold; backup-correspondent failover armed (§17.14.2 — ops alert payload code)"},
		{Code: CodeHerstattLimitBreach, HTTPStatus: 500, Owner: "Phase-24 Task 24.3.19",
			Description: "Herstatt principal exposure open beyond its duration cap (§17.14.5 — intraday ops alert payload code)"},
		{Code: CodeRailFailoverExhausted, HTTPStatus: 503, Owner: "Phase-24 Task 24.3.19",
			Description: "Queued rail payment exhausted the 15m/1h/4h failover retry timetable without dispatch (§17.14.5)"},
		{Code: CodeSuspenseSLABreach, HTTPStatus: 500, Owner: "Phase-24 Task 24.3.19",
			Description: "Suspense-account item unresolved past the 2-day clearing SLA (§17.14.1 — ops alert payload code)"},
	} {
		errs.Default.MustRegister(d)
	}
}

// ---------------------------------------------------------------------------
// Shared pgx plumbing
// ---------------------------------------------------------------------------

// Querier is the shared Exec/Query/QueryRow surface of pgx.Tx and
// *pgxpool.Pool — tx-scoped primitives take it so the same code runs inside
// a service-owned SERIALIZABLE transaction or a caller-owned one (the
// Phase-07 dual-control executor hands us its approval pgx.Tx).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RunInTx invokes fn inside a SERIALIZABLE transaction when q is a pool,
// or inline when q is already a pgx.Tx (dual-control executor path).
func RunInTx(ctx context.Context, q Querier, fn func(Querier) error) error {
	if q == nil {
		return excerrors.New(CodeServiceDegraded, "backoffice: store not configured")
	}
	if tx, ok := q.(pgx.Tx); ok {
		return fn(tx)
	}
	pool, ok := q.(*pgxpool.Pool)
	if !ok {
		return excerrors.New(CodeServiceDegraded,
			fmt.Sprintf("backoffice: unsupported querier %T", q))
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("backoffice: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("backoffice: commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Task 24.3.6 — Failed Settlement Handling (spec §17.1)
// ---------------------------------------------------------------------------

// ExceptionType mirrors the settlement_exceptions CHECK list.
type ExceptionType string

const (
	ExcSwiftRejection     ExceptionType = "SWIFT_REJECTION"
	ExcInsufficientNostro ExceptionType = "INSUFFICIENT_NOSTRO"
	ExcCounterparty       ExceptionType = "COUNTERPARTY"
	ExcUnmatchedStatement ExceptionType = "UNMATCHED_STATEMENT" // Task 24.3.12 ingestion
	ExcAmountMismatch     ExceptionType = "AMOUNT_MISMATCH"     // Task 24.3.12 ingestion
	ExcMissingPayment     ExceptionType = "MISSING_PAYMENT"     // Task 24.3.12 ingestion
	ExcUnexpectedCredit   ExceptionType = "UNEXPECTED_CREDIT"   // Task 24.3.12 ingestion
	ExcCLSMismatch        ExceptionType = "CLS_MISMATCH"
	ExcOther              ExceptionType = "OTHER"
)

// ExceptionStatus mirrors the settlement_exceptions status CHECK list.
type ExceptionStatus string

const (
	ExcStatusOpen             ExceptionStatus = "OPEN"
	ExcStatusInvestigating    ExceptionStatus = "INVESTIGATING"
	ExcStatusResolvedRetry    ExceptionStatus = "RESOLVED_RETRY"
	ExcStatusResolvedReversed ExceptionStatus = "RESOLVED_REVERSED"
	ExcStatusResolvedManual   ExceptionStatus = "RESOLVED_MANUAL"
	ExcStatusWrittenOff       ExceptionStatus = "WRITTEN_OFF"
)

// ResolutionAction is the dispositive outcome of a resolve call.
type ResolutionAction string

const (
	ActionRetry    ResolutionAction = "RETRY"     // re-dispatch the instruction
	ActionReverse  ResolutionAction = "REVERSE"   // reverse settlement, return funds
	ActionManual   ResolutionAction = "MANUAL"    // resolved off-book with notes
	ActionWriteOff ResolutionAction = "WRITE_OFF" // via Task 24.3.19 authority matrix
)

func (a ResolutionAction) valid() bool {
	switch a {
	case ActionRetry, ActionReverse, ActionManual, ActionWriteOff:
		return true
	}
	return false
}

// terminal maps an action onto the exception's terminal status.
func (a ResolutionAction) terminal() ExceptionStatus {
	switch a {
	case ActionRetry:
		return ExcStatusResolvedRetry
	case ActionReverse:
		return ExcStatusResolvedReversed
	case ActionWriteOff:
		return ExcStatusWrittenOff
	default:
		return ExcStatusResolvedManual
	}
}

// SettlementException is one settlement_exceptions row.
type SettlementException struct {
	ID                int64
	InstructionID     *int64
	TradeID           *int64
	AccountID         *int64
	Currency          string
	Amount            decimal.Decimal
	Type              ExceptionType
	Status            ExceptionStatus
	DetectedBy        string
	Detail            string
	AssignedTo        *int64
	Action            *ResolutionAction
	Notes             string
	ResolvedBy        *int64
	ApprovedBy        *int64
	DualControlID     *int64
	ReversalJournalID *int64
	DetectedAt        time.Time
	ResolvedAt        *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Actionable reports whether the exception can still be resolved.
func (e SettlementException) Actionable() bool {
	return e.Status == ExcStatusOpen || e.Status == ExcStatusInvestigating
}

// ExceptionLeg is the settlement_instructions view the workflow needs.
type ExceptionLeg struct {
	ID              int64
	TradeID         int64
	AccountID       int64
	Currency        string
	Amount          decimal.Decimal
	Direction       string // PAY | RECEIVE
	SettlementDate  time.Time
	NostroAccountID *int64
	Status          string // PENDING|SETTLED|FAILED|RECONCILED|VOID
	SwiftMessageID  *string
}

// ExceptionFilter narrows ListExceptions.
type ExceptionFilter struct {
	Status   ExceptionStatus // "" = all
	Type     ExceptionType
	OpenOnly bool
	Limit    int
}

// ExceptionStore is the persistence seam; pgxExceptionStore is the
// production impl, tests inject fakes.
type ExceptionStore interface {
	// InsertException persists a new OPEN exception.
	InsertException(ctx context.Context, e SettlementException) (SettlementException, error)
	// ExceptionByID loads one exception; ErrNoRows → NOT_FOUND upstream.
	ExceptionByID(ctx context.Context, id int64) (SettlementException, error)
	// ListExceptions returns exceptions newest-first.
	ListExceptions(ctx context.Context, f ExceptionFilter) ([]SettlementException, error)
	// InstructionByID loads one settlement leg.
	InstructionByID(ctx context.Context, id int64) (ExceptionLeg, error)
	// InTx runs fn inside a SERIALIZABLE transaction.
	InTx(ctx context.Context, fn func(ctx context.Context, tx ExceptionTx) error) error
}

// ExceptionTx is the transactional view inside ExceptionStore.InTx and the
// shape the dual-control executor receives (see TxFromPgx).
type ExceptionTx interface {
	LockException(ctx context.Context, id int64) (SettlementException, bool, error)
	// ResolveException stamps the terminal status + resolution fields.
	ResolveException(ctx context.Context, id int64, st ExceptionStatus, a ResolutionAction,
		resolvedBy, approvedBy, dcID int64, notes string, journalID *int64, at time.Time) error
	AssignException(ctx context.Context, id, assignee int64, st ExceptionStatus, at time.Time) error
	AppendEvent(ctx context.Context, exceptionID int64, actorID *int64, action string, detail map[string]any) error
	InstructionForUpdate(ctx context.Context, id int64) (ExceptionLeg, bool, error)
	InstructionsForTradeForUpdate(ctx context.Context, tradeID int64) ([]ExceptionLeg, error)
	// FailInstruction flips a PENDING leg to FAILED.
	FailInstruction(ctx context.Context, id int64) error
	// ResetForRetry returns a FAILED/PENDING leg to PENDING and clears the
	// dispatch claim so the next DispatchDue pass re-sends it.
	ResetInstructionForRetry(ctx context.Context, id int64) error
	// VoidInstruction marks a leg VOID (reversal terminal state).
	VoidInstruction(ctx context.Context, id int64) error
	// VoidPendingMovements voids PENDING nostro_movements for the leg.
	VoidPendingMovements(ctx context.Context, instructionID int64) (int64, error)
	// CompensatePostedMovement appends the opposite-direction PENDING
	// movement for a POSTED nostro movement (clawback intent); returns
	// false when no POSTED movement exists.
	CompensatePostedMovement(ctx context.Context, instructionID int64) (bool, error)
	// AdjustBalance applies a signed available-balance delta (funds
	// return on PAY legs, clawback on RECEIVE legs).
	AdjustBalance(ctx context.Context, accountID int64, currency string, delta decimal.Decimal) error
	// PostJournal writes a balanced journal_entries + ledger_lines batch
	// (the deferred zero-sum trigger enforces the §5.3 invariant).
	PostJournal(ctx context.Context, entryType, description, postedBy, idemKey string,
		referenceID int64, lines []JournalLine) (int64, error)
}

// JournalLine is one ledger_lines leg of a reversal/write-off journal.
type JournalLine struct {
	AccountCode string
	Debit       decimal.Decimal
	Credit      decimal.Decimal
	Currency    string
	Narrative   string
}

// FailureDetection describes one detected settlement failure.
type FailureDetection struct {
	InstructionID *int64 // nil → external/unmatched break (Task 24.3.12)
	TradeID       *int64
	AccountID     *int64
	Currency      string
	Amount        decimal.Decimal
	Type          ExceptionType
	DetectedBy    string // e.g. "swift-return-parser", "nostro-monitor", "statement-ingest"
	Detail        string
}

// DualQueue is the Phase-07 DualControlService seam (SubmitInput-shaped) —
// declared locally so backoffice never imports internal/admin; the api
// layer adapts.
type DualQueue interface {
	Submit(ctx context.Context, in DualSubmit) (*DualResult, error)
}

// DualSubmit mirrors admin.SubmitInput.
type DualSubmit struct {
	Operation    string
	TargetType   string
	TargetID     string
	Payload      any
	RequiredRole string
	RequestedBy  int64
	Reason       string
	ClientIP     string
}

// DualResult mirrors the subset of admin.DualControlRequest callers need.
type DualResult struct {
	ID           int64
	Operation    string
	RequiredRole string
	Status       string
	ExpiresAt    time.Time
}

// ExceptionService drives the investigate→resolve→retry/reverse workflow.
type ExceptionService struct {
	store ExceptionStore
	dual  DualQueue // nil → resolves are rejected at submit (fail-closed)
	now   func() time.Time
}

// NewExceptionService wires the service; nil store fails closed.
func NewExceptionService(store ExceptionStore, dual DualQueue) *ExceptionService {
	return &ExceptionService{store: store, dual: dual, now: func() time.Time { return time.Now().UTC() }}
}

// SetClockForTest overrides the clock; tests only.
func (s *ExceptionService) SetClockForTest(now func() time.Time) { s.now = now }

// DetectFailure records a failed settlement: the instruction leg (when
// present) flips to FAILED and an OPEN exception row lands with a
// 'detect' audit event. Idempotent-ish: a repeat detection on the same
// instruction attaches to the existing OPEN/INVESTIGATING exception
// rather than duplicating (event recorded either way).
func (s *ExceptionService) DetectFailure(ctx context.Context, d FailureDetection) (*SettlementException, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "exception store not configured")
	}
	if d.Type == "" {
		d.Type = ExcOther
	}
	if d.DetectedBy == "" {
		return nil, excerrors.New("INVALID_REQUEST", "detected_by is required")
	}
	var out *SettlementException
	err := s.store.InTx(ctx, func(ctx context.Context, tx ExceptionTx) error {
		if d.InstructionID != nil {
			leg, found, err := tx.InstructionForUpdate(ctx, *d.InstructionID)
			if err != nil {
				return fmt.Errorf("lock instruction %d: %w", *d.InstructionID, err)
			}
			if !found {
				return excerrors.New("NOT_FOUND",
					fmt.Sprintf("settlement instruction %d not found", *d.InstructionID))
			}
			switch leg.Status {
			case "PENDING", "FAILED":
				if leg.Status == "PENDING" {
					if err := tx.FailInstruction(ctx, leg.ID); err != nil {
						return fmt.Errorf("fail instruction %d: %w", leg.ID, err)
					}
				}
			default:
				return excerrors.New(CodeExceptionConflict, fmt.Sprintf(
					"instruction %d is %s — failures cannot attach to settled/void legs",
					leg.ID, leg.Status))
			}
			if d.TradeID == nil {
				d.TradeID = &leg.TradeID
			}
			if d.AccountID == nil {
				d.AccountID = &leg.AccountID
			}
			if d.Currency == "" {
				d.Currency = leg.Currency
			}
			if d.Amount.IsZero() {
				d.Amount = leg.Amount
			}
		}
		ex, err := s.store.InsertException(ctx, SettlementException{
			InstructionID: d.InstructionID, TradeID: d.TradeID, AccountID: d.AccountID,
			Currency: d.Currency, Amount: d.Amount, Type: d.Type,
			Status: ExcStatusOpen, DetectedBy: d.DetectedBy, Detail: d.Detail,
			DetectedAt: s.now(),
		})
		if err != nil {
			return fmt.Errorf("insert exception: %w", err)
		}
		if err := tx.AppendEvent(ctx, ex.ID, nil, "detect", map[string]any{
			"type": string(d.Type), "detected_by": d.DetectedBy, "detail": d.Detail,
		}); err != nil {
			return fmt.Errorf("audit event: %w", err)
		}
		out = &ex
		return nil
	})
	return out, err
}

// Investigate moves OPEN→INVESTIGATING and assigns the break to a
// finance-ops owner (middle-office investigation step of the workflow).
func (s *ExceptionService) Investigate(ctx context.Context, id, assignee int64) (*SettlementException, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "exception store not configured")
	}
	if assignee <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "assignee is required for investigation")
	}
	var out *SettlementException
	err := s.store.InTx(ctx, func(ctx context.Context, tx ExceptionTx) error {
		ex, found, err := tx.LockException(ctx, id)
		if err != nil {
			return fmt.Errorf("lock exception %d: %w", id, err)
		}
		if !found {
			return excerrors.New("NOT_FOUND", fmt.Sprintf("exception %d not found", id))
		}
		if ex.Status != ExcStatusOpen && ex.Status != ExcStatusInvestigating {
			return excerrors.New(CodeExceptionConflict, fmt.Sprintf(
				"exception %d is %s — only OPEN/INVESTIGATING can be investigated", id, ex.Status))
		}
		if err := tx.AssignException(ctx, id, assignee, ExcStatusInvestigating, s.now()); err != nil {
			return fmt.Errorf("assign exception %d: %w", id, err)
		}
		if err := tx.AppendEvent(ctx, id, &assignee, "investigate", nil); err != nil {
			return fmt.Errorf("audit event: %w", err)
		}
		ex.Status = ExcStatusInvestigating
		ex.AssignedTo = &assignee
		out = &ex
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Dual-control resolution (POST /api/v1/admin/settlement-exceptions/{id}/resolve)
// ---------------------------------------------------------------------------

// ResolutionPayload rides the dual-control request row — the executor
// replays it inside the approval transaction.
type ResolutionPayload struct {
	ExceptionID int64            `json:"exception_id"`
	Action      ResolutionAction `json:"action"`
	Notes       string           `json:"notes"`
	RequestedBy int64            `json:"requested_by"`
}

// RequestResolution validates the exception is actionable and queues the
// four-eyes request. The mutation lands via ApplyResolution inside the
// approval tx — never directly from the maker's call.
func (s *ExceptionService) RequestResolution(ctx context.Context, exceptionID int64,
	action ResolutionAction, notes string, makerID int64, clientIP string) (*DualResult, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "exception store not configured")
	}
	if s.dual == nil {
		return nil, excerrors.New(CodeServiceDegraded, "dual-control queue not configured")
	}
	if !action.valid() || action == ActionWriteOff {
		return nil, excerrors.New("INVALID_REQUEST",
			"action must be RETRY|REVERSE|MANUAL (WRITE_OFF rides the §17.14.1 authority matrix)")
	}
	ex, err := s.store.ExceptionByID(ctx, exceptionID)
	if err != nil {
		return nil, fmt.Errorf("load exception %d: %w", exceptionID, err)
	}
	if ex.ID == 0 {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("exception %d not found", exceptionID))
	}
	if !ex.Actionable() {
		return nil, excerrors.New(CodeExceptionConflict, fmt.Sprintf(
			"exception %d already %s", exceptionID, ex.Status))
	}
	if action != ActionManual && ex.InstructionID == nil {
		return nil, excerrors.New("INVALID_REQUEST",
			"RETRY/REVERSE require a settlement instruction; use MANUAL for external breaks")
	}
	return s.dual.Submit(ctx, DualSubmit{
		Operation:    OpSettlementExceptionResolve,
		TargetType:   "settlement_exception",
		TargetID:     fmt.Sprintf("%d", exceptionID),
		Payload:      ResolutionPayload{ExceptionID: exceptionID, Action: action, Notes: notes, RequestedBy: makerID},
		RequiredRole: "Finance Ops",
		RequestedBy:  makerID,
		Reason:       fmt.Sprintf("resolve settlement exception %d as %s", exceptionID, action),
		ClientIP:     clientIP,
	})
}

// ApplyResolution executes an approved resolution inside the caller's
// transaction — the dual-control executor path (tx = approval pgx.Tx via
// TxFromPgx) or a service-owned tx when invoked directly.
func (s *ExceptionService) ApplyResolution(ctx context.Context, tx ExceptionTx,
	exceptionID int64, action ResolutionAction, resolverID, approverID, dcID int64, notes string) error {
	if tx == nil {
		return excerrors.New(CodeServiceDegraded, "exception tx not configured")
	}
	ex, found, err := tx.LockException(ctx, exceptionID)
	if err != nil {
		return fmt.Errorf("lock exception %d: %w", exceptionID, err)
	}
	if !found {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("exception %d not found", exceptionID))
	}
	if !ex.Actionable() {
		return excerrors.New(CodeExceptionConflict, fmt.Sprintf(
			"exception %d already %s", exceptionID, ex.Status))
	}
	var journalID *int64
	switch action {
	case ActionRetry:
		if err := s.applyRetry(ctx, tx, ex); err != nil {
			return err
		}
	case ActionReverse:
		jid, err := s.applyReversal(ctx, tx, ex)
		if err != nil {
			return err
		}
		journalID = jid
	case ActionManual:
		// status flip only — notes carry the off-book disposition
	default:
		return excerrors.New("INVALID_REQUEST", fmt.Sprintf("unknown action %q", action))
	}
	if err := tx.ResolveException(ctx, exceptionID, action.terminal(), action,
		resolverID, approverID, dcID, notes, journalID, s.now()); err != nil {
		return fmt.Errorf("resolve exception %d: %w", exceptionID, err)
	}
	return tx.AppendEvent(ctx, exceptionID, &resolverID, "resolve", map[string]any{
		"action": string(action), "approved_by": approverID, "notes": notes,
	})
}

// applyRetry re-arms the failed instruction for the next dispatch pass:
// FAILED/PENDING leg → PENDING with the dispatch claim cleared.
func (s *ExceptionService) applyRetry(ctx context.Context, tx ExceptionTx, ex SettlementException) error {
	leg, found, err := tx.InstructionForUpdate(ctx, *ex.InstructionID)
	if err != nil {
		return fmt.Errorf("lock instruction: %w", err)
	}
	if !found {
		return excerrors.New("NOT_FOUND", "settlement instruction not found")
	}
	switch leg.Status {
	case "FAILED", "PENDING":
	default:
		return excerrors.New(CodeExceptionConflict, fmt.Sprintf(
			"instruction %d is %s — only FAILED/PENDING legs retry", leg.ID, leg.Status))
	}
	return tx.ResetInstructionForRetry(ctx, leg.ID)
}

// applyReversal reverses the trade's settlement: every non-terminal leg →
// VOID, pending nostro movements voided, posted movements compensated by
// opposite-direction intents, account balances refunded/clawed-back, and a
// balanced GL reversal journal posted — all inside the approval tx.
func (s *ExceptionService) applyReversal(ctx context.Context, tx ExceptionTx,
	ex SettlementException) (*int64, error) {
	leg, found, err := tx.InstructionForUpdate(ctx, *ex.InstructionID)
	if err != nil {
		return nil, fmt.Errorf("lock instruction: %w", err)
	}
	if !found {
		return nil, excerrors.New("NOT_FOUND", "settlement instruction not found")
	}
	if leg.Status == "VOID" {
		return nil, excerrors.New(CodeExceptionConflict,
			fmt.Sprintf("instruction %d already VOID", leg.ID))
	}
	legs, err := tx.InstructionsForTradeForUpdate(ctx, leg.TradeID)
	if err != nil {
		return nil, fmt.Errorf("lock trade %d legs: %w", leg.TradeID, err)
	}
	if len(legs) == 0 {
		legs = []ExceptionLeg{leg}
	}
	// Per-currency net balance effect per account + nostro-side GL lines.
	type acctCcy struct {
		acct int64
		ccy  string
	}
	netBalance := map[acctCcy]decimal.Decimal{}
	for _, l := range legs {
		if l.Status == "VOID" {
			continue // already reversed
		}
		if _, err := tx.VoidPendingMovements(ctx, l.ID); err != nil {
			return nil, fmt.Errorf("void movements leg %d: %w", l.ID, err)
		}
		if _, err := tx.CompensatePostedMovement(ctx, l.ID); err != nil {
			return nil, fmt.Errorf("compensate movement leg %d: %w", l.ID, err)
		}
		if err := tx.VoidInstruction(ctx, l.ID); err != nil {
			return nil, fmt.Errorf("void instruction %d: %w", l.ID, err)
		}
		// PAY leg: the client owed the correspondent — reversal returns
		// the funds (available += amount). RECEIVE leg: funds that arrived
		// are clawed back (available -= amount).
		key := acctCcy{acct: l.AccountID, ccy: l.Currency}
		delta := l.Amount
		if l.Direction == "RECEIVE" {
			delta = delta.Neg()
		}
		netBalance[key] = netBalance[key].Add(delta)
	}
	for k, delta := range netBalance {
		if err := tx.AdjustBalance(ctx, k.acct, k.ccy, delta); err != nil {
			return nil, excerrors.Wrap(CodeReversalFailed,
				fmt.Sprintf("balance adjust acct %d %s %s", k.acct, k.ccy, delta), err)
		}
	}
	// Balanced reversal journal: nostro asset vs customer liability, per
	// currency — the §5.21 zero-sum trigger vets the batch at commit.
	var lines []JournalLine
	for ccy, delta := range netBalance {
		if delta.IsZero() {
			continue
		}
		liab := "2010_CUSTOMER_LIABILITY_" + ccy.ccy
		nostro := "1010_NOSTRO_" + ccy.ccy
		amt := delta.Abs()
		narr := fmt.Sprintf("settlement reversal exc %d trade %d", ex.ID, leg.TradeID)
		if delta.IsPositive() { // funds returned to client
			lines = append(lines,
				JournalLine{AccountCode: nostro, Debit: amt, Currency: ccy.ccy, Narrative: narr},
				JournalLine{AccountCode: liab, Credit: amt, Currency: ccy.ccy, Narrative: narr})
		} else { // client balance clawed back
			lines = append(lines,
				JournalLine{AccountCode: liab, Debit: amt, Currency: ccy.ccy, Narrative: narr},
				JournalLine{AccountCode: nostro, Credit: amt, Currency: ccy.ccy, Narrative: narr})
		}
	}
	var jid *int64
	if len(lines) > 0 {
		id, err := tx.PostJournal(ctx, "SETTLEMENT",
			fmt.Sprintf("settlement reversal — exception %d trade %d", ex.ID, leg.TradeID),
			"settlement-exception-service",
			fmt.Sprintf("settle-rev:%d", ex.ID), leg.TradeID, lines)
		if err != nil {
			return nil, excerrors.Wrap(CodeReversalFailed, "reversal journal", err)
		}
		jid = &id
	}
	return jid, nil
}

// ResolveDirect applies a resolution inside a service-owned SERIALIZABLE
// transaction — used by tests and by internal flows that hold their own
// approval evidence. REST resolution rides RequestResolution → the
// dual-control executor.
func (s *ExceptionService) ResolveDirect(ctx context.Context, exceptionID int64,
	action ResolutionAction, resolverID, approverID int64, notes string) error {
	if s.store == nil {
		return excerrors.New(CodeServiceDegraded, "exception store not configured")
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx ExceptionTx) error {
		return s.ApplyResolution(ctx, tx, exceptionID, action, resolverID, approverID, 0, notes)
	})
}

// Get returns one exception; List returns the filtered view.
func (s *ExceptionService) Get(ctx context.Context, id int64) (*SettlementException, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "exception store not configured")
	}
	ex, err := s.store.ExceptionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if ex.ID == 0 {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("exception %d not found", id))
	}
	return &ex, nil
}

// List returns exceptions matching f (newest first).
func (s *ExceptionService) List(ctx context.Context, f ExceptionFilter) ([]SettlementException, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "exception store not configured")
	}
	return s.store.ListExceptions(ctx, f)
}

// ---------------------------------------------------------------------------
// PgxExceptionStore — production ExceptionStore over pgx
// ---------------------------------------------------------------------------

// PgxExceptionStore implements ExceptionStore. Its transaction view wraps
// Querier so the same primitives serve service-owned tx and the
// dual-control executor's approval tx.
type PgxExceptionStore struct{ Q Querier }

// NewPgxExceptionStore binds the store to a pool.
func NewPgxExceptionStore(pool *pgxpool.Pool) *PgxExceptionStore {
	return &PgxExceptionStore{Q: pool}
}

// TxFromPgx exposes the tx primitives over a caller-owned pgx.Tx — the
// dual-control executor path (approval tx must carry the mutation).
func TxFromPgx(tx pgx.Tx) ExceptionTx { return pgxExceptionTx{q: tx} }

const exceptionCols = `id, settlement_instruction_id, trade_id, account_id,
	COALESCE(currency,''), COALESCE(amount::text,'0'), exception_type, status,
	detected_by, COALESCE(detail,''), assigned_to, resolution_action,
	COALESCE(resolution_notes,''), resolved_by, approved_by,
	dual_control_request_id, reversal_journal_id, detected_at, resolved_at,
	created_at, updated_at`

func scanException(row interface{ Scan(...any) error }) (SettlementException, error) {
	var e SettlementException
	var amt string
	var action *string
	err := row.Scan(&e.ID, &e.InstructionID, &e.TradeID, &e.AccountID,
		&e.Currency, &amt, (*string)(&e.Type), (*string)(&e.Status),
		&e.DetectedBy, &e.Detail, &e.AssignedTo, &action,
		&e.Notes, &e.ResolvedBy, &e.ApprovedBy, &e.DualControlID,
		&e.ReversalJournalID, &e.DetectedAt, &e.ResolvedAt,
		&e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return e, err
	}
	e.Amount, err = decimal.NewFromString(amt)
	if err != nil {
		return e, fmt.Errorf("exception %d amount %q: %w", e.ID, amt, err)
	}
	if action != nil {
		a := ResolutionAction(*action)
		e.Action = &a
	}
	return e, nil
}

func (s *PgxExceptionStore) InsertException(ctx context.Context, e SettlementException) (SettlementException, error) {
	var amount any
	if !e.Amount.IsZero() {
		amount = e.Amount.String()
	}
	row := s.Q.QueryRow(ctx, `
		INSERT INTO settlement_exceptions
		    (settlement_instruction_id, trade_id, account_id, currency, amount,
		     exception_type, status, detected_by, detail, detected_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5::numeric,$6,$7,$8,NULLIF($9,''),$10)
		RETURNING `+exceptionCols,
		e.InstructionID, e.TradeID, e.AccountID, e.Currency, amount,
		string(e.Type), string(ExcStatusOpen), e.DetectedBy, e.Detail, e.DetectedAt)
	return scanException(row)
}

func (s *PgxExceptionStore) ExceptionByID(ctx context.Context, id int64) (SettlementException, error) {
	e, err := scanException(s.Q.QueryRow(ctx,
		`SELECT `+exceptionCols+` FROM settlement_exceptions WHERE id=$1`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return SettlementException{}, nil
	}
	return e, err
}

func (s *PgxExceptionStore) ListExceptions(ctx context.Context, f ExceptionFilter) ([]SettlementException, error) {
	q := `SELECT ` + exceptionCols + ` FROM settlement_exceptions`
	var args []any
	var where []string
	if f.OpenOnly {
		where = append(where, `status IN ('OPEN','INVESTIGATING')`)
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		where = append(where, fmt.Sprintf(`status = $%d`, len(args)))
	}
	if f.Type != "" {
		args = append(args, string(f.Type))
		where = append(where, fmt.Sprintf(`exception_type = $%d`, len(args)))
	}
	if len(where) > 0 {
		q += " WHERE " + joinAnd(where)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args))
	rows, err := s.Q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettlementException
	for rows.Next() {
		e, err := scanException(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func joinAnd(clauses []string) string {
	out := clauses[0]
	for _, c := range clauses[1:] {
		out += " AND " + c
	}
	return out
}

const legCols = `id, trade_id, account_id, currency, amount::text,
	direction::text, settlement_date, nostro_account_id, status::text,
	swift_message_id`

func scanLeg(row interface{ Scan(...any) error }) (ExceptionLeg, error) {
	var l ExceptionLeg
	var amt string
	err := row.Scan(&l.ID, &l.TradeID, &l.AccountID, &l.Currency, &amt,
		&l.Direction, &l.SettlementDate, &l.NostroAccountID, &l.Status, &l.SwiftMessageID)
	if err != nil {
		return l, err
	}
	l.Amount, err = decimal.NewFromString(amt)
	return l, err
}

func (s *PgxExceptionStore) InstructionByID(ctx context.Context, id int64) (ExceptionLeg, error) {
	return scanLeg(s.Q.QueryRow(ctx,
		`SELECT `+legCols+` FROM settlement_instructions WHERE id=$1`, id))
}

func (s *PgxExceptionStore) InTx(ctx context.Context, fn func(ctx context.Context, tx ExceptionTx) error) error {
	return RunInTx(ctx, s.Q, func(q Querier) error {
		return fn(ctx, pgxExceptionTx{q: q})
	})
}

// pgxExceptionTx implements ExceptionTx over any Querier.
type pgxExceptionTx struct{ q Querier }

func (t pgxExceptionTx) LockException(ctx context.Context, id int64) (SettlementException, bool, error) {
	e, err := scanException(t.q.QueryRow(ctx,
		`SELECT `+exceptionCols+` FROM settlement_exceptions WHERE id=$1 FOR UPDATE`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return SettlementException{}, false, nil
	}
	return e, err == nil, err
}

func (t pgxExceptionTx) ResolveException(ctx context.Context, id int64, st ExceptionStatus,
	a ResolutionAction, resolvedBy, approvedBy, dcID int64, notes string, journalID *int64, at time.Time) error {
	tag, err := t.q.Exec(ctx, `
		UPDATE settlement_exceptions
		   SET status=$2, resolution_action=$3, resolution_notes=NULLIF($4,''),
		       resolved_by=$5, approved_by=NULLIF($6,0), dual_control_request_id=NULLIF($7,0),
		       reversal_journal_id=$8, resolved_at=$9, updated_at=$9
		 WHERE id=$1 AND status IN ('OPEN','INVESTIGATING')`,
		id, string(st), string(a), notes, resolvedBy, approvedBy, dcID, journalID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeExceptionConflict,
			fmt.Sprintf("exception %d lost actionable status mid-resolve", id))
	}
	return nil
}

func (t pgxExceptionTx) AssignException(ctx context.Context, id, assignee int64,
	st ExceptionStatus, at time.Time) error {
	_, err := t.q.Exec(ctx, `
		UPDATE settlement_exceptions
		   SET status=$3, assigned_to=$2, updated_at=$4
		 WHERE id=$1`, id, assignee, string(st), at)
	return err
}

func (t pgxExceptionTx) AppendEvent(ctx context.Context, exceptionID int64,
	actorID *int64, action string, detail map[string]any) error {
	var detailArg any
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		detailArg = string(b)
	}
	var actor any
	if actorID != nil {
		actor = *actorID
	}
	_, err := t.q.Exec(ctx, `
		INSERT INTO settlement_exception_events (exception_id, actor_id, action, detail)
		VALUES ($1,$2,$3,$4::jsonb)`, exceptionID, actor, action, detailArg)
	return err
}

func (t pgxExceptionTx) InstructionForUpdate(ctx context.Context, id int64) (ExceptionLeg, bool, error) {
	l, err := scanLeg(t.q.QueryRow(ctx,
		`SELECT `+legCols+` FROM settlement_instructions WHERE id=$1 FOR UPDATE`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return ExceptionLeg{}, false, nil
	}
	return l, err == nil, err
}

func (t pgxExceptionTx) InstructionsForTradeForUpdate(ctx context.Context, tradeID int64) ([]ExceptionLeg, error) {
	rows, err := t.q.Query(ctx,
		`SELECT `+legCols+` FROM settlement_instructions
		 WHERE trade_id=$1 ORDER BY id FOR UPDATE`, tradeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExceptionLeg
	for rows.Next() {
		l, err := scanLeg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (t pgxExceptionTx) FailInstruction(ctx context.Context, id int64) error {
	_, err := t.q.Exec(ctx, `
		UPDATE settlement_instructions
		   SET status='FAILED', updated_at=now()
		 WHERE id=$1 AND status='PENDING'`, id)
	return err
}

func (t pgxExceptionTx) ResetInstructionForRetry(ctx context.Context, id int64) error {
	tag, err := t.q.Exec(ctx, `
		UPDATE settlement_instructions
		   SET status='PENDING', swift_message_id=NULL, message_format=NULL,
		       message_payload=NULL, dispatched_at=NULL, updated_at=now()
		 WHERE id=$1 AND status IN ('FAILED','PENDING')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeExceptionConflict,
			fmt.Sprintf("instruction %d not retryable", id))
	}
	return nil
}

func (t pgxExceptionTx) VoidInstruction(ctx context.Context, id int64) error {
	_, err := t.q.Exec(ctx, `
		UPDATE settlement_instructions
		   SET status='VOID', updated_at=now()
		 WHERE id=$1 AND status <> 'VOID'`, id)
	return err
}

func (t pgxExceptionTx) VoidPendingMovements(ctx context.Context, instructionID int64) (int64, error) {
	tag, err := t.q.Exec(ctx, `
		UPDATE nostro_movements SET status='VOID'
		 WHERE settlement_instruction_id=$1 AND status='PENDING'`, instructionID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (t pgxExceptionTx) CompensatePostedMovement(ctx context.Context, instructionID int64) (bool, error) {
	tag, err := t.q.Exec(ctx, `
		INSERT INTO nostro_movements
		    (settlement_instruction_id, nostro_account_id, currency, amount,
		     direction, status, confirmation_ref)
		SELECT m.settlement_instruction_id, m.nostro_account_id, m.currency, m.amount,
		       CASE WHEN m.direction='DEBIT' THEN 'CREDIT' ELSE 'DEBIT' END,
		       'PENDING', 'REVERSAL'
		  FROM nostro_movements m
		 WHERE m.settlement_instruction_id=$1 AND m.status='POSTED'
		ON CONFLICT (settlement_instruction_id) DO NOTHING`, instructionID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (t pgxExceptionTx) AdjustBalance(ctx context.Context, accountID int64,
	currency string, delta decimal.Decimal) error {
	// Lock-or-create the balance row, then apply the signed delta —
	// funds return (PAY reversal) or clawback (RECEIVE reversal).
	if _, err := t.q.Exec(ctx, `
		INSERT INTO balances (account_id, currency, available, locked)
		VALUES ($1,$2,0,0) ON CONFLICT (account_id, currency) DO NOTHING`,
		accountID, currency); err != nil {
		return err
	}
	var avail string
	if err := t.q.QueryRow(ctx, `
		SELECT available::text FROM balances
		 WHERE account_id=$1 AND currency=$2 FOR UPDATE`, accountID, currency).Scan(&avail); err != nil {
		return err
	}
	_, err := t.q.Exec(ctx, `
		UPDATE balances SET available = available + $3::numeric, version = version + 1
		 WHERE account_id=$1 AND currency=$2`, accountID, currency, delta.String())
	return err
}

func (t pgxExceptionTx) PostJournal(ctx context.Context, entryType, description,
	postedBy, idemKey string, referenceID int64, lines []JournalLine) (int64, error) {
	var journalID int64
	err := t.q.QueryRow(ctx, `
		INSERT INTO journal_entries (entry_type, reference_id, description, posted_by, idempotency_key)
		VALUES ($1::gl_entry_type_enum,$2,$3,$4,$5)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id`, entryType, referenceID, description, postedBy, idemKey).Scan(&journalID)
	if stderrors.Is(err, pgx.ErrNoRows) {
		// Replay: resolve the existing journal id (idempotent).
		if err := t.q.QueryRow(ctx,
			`SELECT id FROM journal_entries WHERE idempotency_key=$1`, idemKey).Scan(&journalID); err != nil {
			return 0, err
		}
		return journalID, nil
	}
	if err != nil {
		return 0, err
	}
	for _, l := range lines {
		if _, err := t.q.Exec(ctx, `
			INSERT INTO ledger_lines
			    (journal_entry_id, account_code, debit_amount, credit_amount, currency, narrative)
			VALUES ($1,$2,$3::numeric,$4::numeric,$5,$6)`,
			journalID, l.AccountCode, l.Debit.String(), l.Credit.String(), l.Currency, l.Narrative); err != nil {
			return 0, err
		}
	}
	return journalID, nil
}
