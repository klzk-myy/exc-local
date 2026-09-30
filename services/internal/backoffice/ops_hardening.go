// Task 24.3.19 — Settlement Operations Hardening (spec §17.14, §24 #347).
//
// Contents:
//   1. Break lifecycle — T+1 investigate / T+2 escalate / T+5 write-off
//      review; suspense parking SLA (2d, suspense_account_mappings);
//      write-off authority matrix + dual control; auto-match KPI ≥98%
//      (PBReconService.Reconcile emits the rate; SweepAging alerts on
//      breaches); per-currency tolerances (recon_tolerances, used by
//      pb_reconciliation.go).
//   2. Nostro funding thresholds — 3-day projected outflow cover + CLS
//      pay-in cover per currency/correspondent; hourly evaluation;
//      concentration limit; designated backup correspondent.
//   3. Cut-off matrix — rail_cutoff_matrix is the ops overlay (late fee,
//      value-date roll flag); the cut-off authority itself is Task
//      24.3.20's settlement.RailCutoffService over banking_rail_schedules
//      (consumed here via the CutoffEvaluator seam).
//   4. CLS pay-in ops — prefunding (T-1 22:00 UTC), consequence ladder,
//      member-outage fallback to the bilateral waterfall.
//   5. FX fail economics — replacement-cost close-out at market + fail
//      interest at policy+100bps from ISD+1 (supersedes CSDR for FX).
//   6. Rail failover — queued-payment retry 15m/1h/4h, fallback rails,
//      duplicate-payment guard.
//   7. Herstatt principal-exposure metric with duration cap.
//   8. LP-default playbook — quote-withdrawal → widened floors → Q5 ADL.

package backoffice

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Canonical §17.14 values
// ---------------------------------------------------------------------------

const (
	// Break aging buckets (business days — the sweep uses wall-clock days
	// per the task text; the scheduler may pass business-day-adjusted now).
	AgeInvestigate = 24 * time.Hour     // T+1
	AgeEscalate    = 48 * time.Hour     // T+2
	AgeWriteOff    = 5 * 24 * time.Hour // T+5
	// SuspenseClearingSLA — suspense-account clearing bound (2 days).
	SuspenseClearingSLA = 48 * time.Hour
	// NostroMonitorCadence — intraday evaluation cadence (hourly in
	// session) — informational for the scheduler that calls
	// EvaluateNostroThresholds.
	NostroMonitorCadence = time.Hour
	// NostroOutflowCoverDays — projected outflow cover horizon (3 days).
	NostroOutflowCoverDays = 3
	// CLSPrefundClockHour/Minute — pay-in prefunding deadline on T-1.
	CLSPrefundHour   = 22
	CLSPrefundMinute = 0
	// FailInterestSpreadBP — fail interest spread over policy (100bps).
	FailInterestSpreadBP = "100.0000"
	// FailInterestDCC — day-count convention for fail interest.
	FailInterestDCC = 360.0
	// HerstattDefaultCapMinutes — default principal-at-risk duration cap.
	HerstattDefaultCapMinutes = 120
)

// RailRetrySchedule is the §17.14.5 queued-payment retry timetable:
// 15 minutes, 1 hour, 4 hours.
var RailRetrySchedule = []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour}

// Write-off authority matrix tiers (§17.14.1). USD-denominated amounts.
type writeOffTier struct {
	Tier   string
	MaxUSD decimal.Decimal // zero = unbounded
	Role   string
}

var writeOffMatrix = []writeOffTier{
	{Tier: "T1", MaxUSD: decimal.NewFromInt(1_000), Role: "Finance Ops"},
	{Tier: "T2", MaxUSD: decimal.NewFromInt(100_000), Role: "Risk Manager"},
	{Tier: "T3", MaxUSD: decimal.Zero, Role: "Super Admin"},
}

// WriteOffTierFor resolves the authority tier for a USD write-off amount.
func WriteOffTierFor(amountUSD decimal.Decimal) (tier, role string, err error) {
	for _, t := range writeOffMatrix {
		if t.MaxUSD.IsZero() || amountUSD.LessThanOrEqual(t.MaxUSD) {
			return t.Tier, t.Role, nil
		}
	}
	return "", "", excerrors.New(CodeWriteOffAuthorityExceeded,
		fmt.Sprintf("no write-off tier covers %s USD", amountUSD))
}

// ---------------------------------------------------------------------------
// Domain rows
// ---------------------------------------------------------------------------

// SuspenseItem is one suspense_account_mappings row (migration 108 — the
// sibling-owned suspense routing surface; we only read for SLA breach).
type SuspenseItem struct {
	ID              int64
	BankTxID        string
	Currency        string
	Amount          decimal.Decimal
	Reason          string
	QuarantineState string
	SLAExpiresAt    time.Time
}

// WriteOff is one settlement_write_offs row.
type WriteOff struct {
	ID            int64
	ExceptionID   *int64
	BreakID       *int64
	Currency      string
	Amount        decimal.Decimal
	Tier          string
	RequiredRole  string
	Reason        string
	DualControlID *int64
	RequestedBy   int64
	ApprovedBy    *int64
	GLJournalID   *int64
	Status        string // PENDING | EXECUTED | REJECTED
	CreatedAt     time.Time
}

// FundingThreshold is one nostro_funding_thresholds row after evaluation.
type FundingThreshold struct {
	ID                int64
	NostroAccountID   int64
	Currency          string
	MinOutflowDays    int
	CLSPayInCover     decimal.Decimal
	ComputedThreshold decimal.Decimal
	ConcentrationPct  decimal.Decimal
	BackupNostroID    *int64
	Breach            bool
	Balance           decimal.Decimal // live nostro_accounts.balance
}

// CutoffRule is one rail_cutoff_matrix row — the ops overlay carrying the
// late-fee pass-through and value-date-roll flag.
type CutoffRule struct {
	Rail      string
	Currency  string
	CutoffUTC string // "HH:MM"
	Roll      bool
	LateFee   decimal.Decimal
}

// CutoffEvaluation is the late-payment verdict for one dispatch.
type CutoffEvaluation struct {
	Rule          CutoffRule
	Late          bool
	RollValueDate bool
	LateFee       decimal.Decimal
	EffectiveAt   time.Time // value date after roll (or same day)
}

// CLSPayIn is one cls_payin_events row.
type CLSPayIn struct {
	ID              int64
	Currency        string
	ValueDate       time.Time
	NostroAccountID int64
	Required        decimal.Decimal
	Funded          decimal.Decimal
	PrefundDeadline time.Time
	LadderStep      int
	MemberOutage    bool
	Fallback        string
	Status          string
}

// FXFailCloseout is one fx_fail_closeouts row (§17.14 item 5).
type FXFailCloseout struct {
	ID              int64
	FailID          int64
	OriginalRate    decimal.Decimal
	CloseoutRate    decimal.Decimal
	ReplacementCost decimal.Decimal
	Currency        string
	PolicyRateBP    decimal.Decimal
	FailInterest    decimal.Decimal
	AccrualDays     int
	Treatment       string // ISDA_CLOSEOUT
}

// FailoverPayment is one rail_failover_queue row.
type FailoverPayment struct {
	ID            int64
	PaymentRef    string
	Rail          string
	FallbackRail  string
	Currency      string
	Amount        decimal.Decimal
	Payload       map[string]any
	Attempts      int
	NextAttemptAt time.Time
	Status        string
}

// HerstattExposure is one herstatt_exposures row.
type HerstattExposure struct {
	ID             int64
	CounterpartyID int64
	Currency       string
	Paid           decimal.Decimal
	Receivable     decimal.Decimal
	WindowOpenedAt time.Time
	CapMinutes     int
	Breached       bool
	SettledAt      *time.Time
}

// LPDefaultEvent is one lp_default_events row.
type LPDefaultEvent struct {
	ID     int64
	LPID   int64
	Stage  string // QUOTE_WITHDRAWN | FLOORS_WIDENED | ADL_Q5
	Detail map[string]any
	At     time.Time
}

// LP-default playbook stage order — only forward progression is legal.
var lpStageOrder = map[string]int{
	"QUOTE_WITHDRAWN": 1,
	"FLOORS_WIDENED":  2,
	"ADL_Q5":          3,
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// CutoffEvaluator is the Task 24.3.20 rail-cutoff seam —
// *settlement.RailCutoffService satisfies it (value-date roll already
// runs through the Task 3.3.8 holiday calendar there).
type CutoffEvaluator interface {
	Evaluate(rail, currency string, at time.Time) (CutoffDecisionView, error)
}

// CutoffDecisionView mirrors settlement.CutoffDecision — declared locally
// so backoffice does not import internal/settlement; the api/wiring layer
// adapts one struct field-for-field.
type CutoffDecisionView struct {
	CutoffPassed       bool
	ValueDate          time.Time
	QueuedForNextCycle bool
}

// RailDispatcher attempts one payment dispatch on a rail — production
// binds the Phase-11 rail adapters; error → the retry ladder advances.
type RailDispatcher interface {
	Dispatch(ctx context.Context, rail string, payload map[string]any) error
}

// OpsAlerter-like local seam (self-contained — sibling-owned OpsAlert
// lives in confirmation.go; this file uses the identical field shape so
// adapters are trivial).
type OpsAlertSink interface {
	Raise(ctx context.Context, sev, code, summary string, details map[string]string) error
}

// OpsStore is the persistence seam for all of Task 24.3.19.
type OpsStore interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx OpsTx) error) error
	// Read side.
	WriteOffByID(ctx context.Context, id int64) (WriteOff, bool, error)
	Thresholds(ctx context.Context) ([]FundingThreshold, error)
	CutoffRule(ctx context.Context, rail, currency string) (CutoffRule, bool, error)
	DueFailoverPayments(ctx context.Context, at time.Time) ([]FailoverPayment, error)
	OpenHerstatt(ctx context.Context) ([]HerstattExposure, error)
	LPDefaultLatest(ctx context.Context, lpID int64) (LPDefaultEvent, bool, error)
	RestitutionsUndelivered(ctx context.Context) ([]Restitution, error)
}

// OpsTx is the transactional view.
type OpsTx interface {
	// Aging sweep — open items detected before olderThan (caller passes
	// now-AgeInvestigate; bucketing by age happens in the service).
	StaleExceptions(ctx context.Context, olderThan time.Time) ([]SettlementException, error)
	StalePBBreaks(ctx context.Context, olderThan time.Time) ([]PBReconBreak, error)
	MarkExceptionStatus(ctx context.Context, id int64, st ExceptionStatus, at time.Time) error
	EscalatePBBreak(ctx context.Context, id int64, at time.Time) error
	AppendExceptionEvent(ctx context.Context, exceptionID int64, actorID *int64, action string, detail map[string]any) error
	AppendPBBreakEvent(ctx context.Context, breakID, actorID *int64, action string, detail map[string]any) error
	// Suspense SLA.
	SuspenseBreaches(ctx context.Context, at time.Time) ([]SuspenseItem, error)
	// Write-offs.
	InsertWriteOff(ctx context.Context, w WriteOff) (WriteOff, error)
	LockWriteOff(ctx context.Context, id int64) (WriteOff, bool, error)
	ExecuteWriteOff(ctx context.Context, id int64, journalID, approverID, dcID int64, at time.Time) error
	MarkExceptionWrittenOff(ctx context.Context, id int64, at time.Time) error
	MarkPBBreakWrittenOff(ctx context.Context, id int64, at time.Time) error
	PostJournal(ctx context.Context, entryType, description, postedBy, idemKey string,
		referenceID int64, lines []JournalLine) (int64, error)
	// Nostro funding thresholds.
	ThresholdRowsForUpdate(ctx context.Context) ([]FundingThreshold, error)
	ProjectedOutflow(ctx context.Context, nostroAccountID int64, ccy string, days int) (decimal.Decimal, error)
	NostroBalance(ctx context.Context, nostroAccountID int64) (decimal.Decimal, error)
	UpdateThreshold(ctx context.Context, id int64, computed decimal.Decimal, breach bool, at time.Time) error
	// CLS pay-ins.
	UpsertCLSPayIn(ctx context.Context, p CLSPayIn) (CLSPayIn, error)
	LockCLSPayIn(ctx context.Context, id int64) (CLSPayIn, bool, error)
	UpdateCLSPayIn(ctx context.Context, p CLSPayIn) error
	DueCLSPayIns(ctx context.Context, at time.Time) ([]CLSPayIn, error)
	// FX close-out.
	FailByIDForUpdate(ctx context.Context, failID int64) (SettlementFail, bool, error)
	TradePrice(ctx context.Context, tradeID int64) (decimal.Decimal, bool, error)
	InsertFXCloseout(ctx context.Context, c FXFailCloseout) (FXFailCloseout, bool, error)
	SetFailStatus(ctx context.Context, id int64, st FailStatus, at time.Time) error
	// Rail failover.
	EnqueuePayment(ctx context.Context, p FailoverPayment) (FailoverPayment, bool, error)
	LockFailoverPayment(ctx context.Context, id int64) (FailoverPayment, bool, error)
	UpdateFailoverPayment(ctx context.Context, p FailoverPayment) error
	// Herstatt.
	OpenHerstattForUpdate(ctx context.Context) ([]HerstattExposure, error)
	InsertHerstatt(ctx context.Context, h HerstattExposure) (HerstattExposure, error)
	SettleHerstatt(ctx context.Context, id int64, at time.Time) error
	MarkHerstattBreached(ctx context.Context, id int64) error
	// LP default.
	LPDefaultLatestTx(ctx context.Context, lpID int64) (LPDefaultEvent, bool, error)
	InsertLPDefault(ctx context.Context, e LPDefaultEvent) (LPDefaultEvent, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// OpsService drives every §17.14 hardening behavior.
type OpsService struct {
	store    OpsStore
	dual     DualQueue       // write-off dual control (nil → submit refuses)
	cutoffs  CutoffEvaluator // settlement.RailCutoffService adapter (optional)
	pricer   MarketPricer    // FX close-out marks (optional → close-out refuses)
	alerter  OpsAlertSink    // optional
	dispatch RailDispatcher  // required for ProcessFailover
	now      func() time.Time
}

// NewOpsService wires the service; store is mandatory (fail closed).
func NewOpsService(store OpsStore) *OpsService {
	return &OpsService{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// WithDual / WithCutoffs / WithPricer / WithAlerter / WithDispatcher wire
// the optional seams.
func (s *OpsService) WithDual(d DualQueue) *OpsService            { s.dual = d; return s }
func (s *OpsService) WithCutoffs(c CutoffEvaluator) *OpsService   { s.cutoffs = c; return s }
func (s *OpsService) WithPricer(p MarketPricer) *OpsService       { s.pricer = p; return s }
func (s *OpsService) WithAlerter(a OpsAlertSink) *OpsService      { s.alerter = a; return s }
func (s *OpsService) WithDispatcher(d RailDispatcher) *OpsService { s.dispatch = d; return s }

// SetClockForTest overrides the clock; tests only.
func (s *OpsService) SetClockForTest(now func() time.Time) { s.now = now }

func (s *OpsService) alert(ctx context.Context, sev, code, summary string, details map[string]string) {
	if s.alerter != nil {
		_ = s.alerter.Raise(ctx, sev, code, summary, details)
	}
}

// ===========================================================================
// 1. Break lifecycle — aging sweep + suspense SLA + write-off matrix
// ===========================================================================

// AgingResult reports one SweepAging pass.
type AgingResult struct {
	Investigated     int `json:"investigated"`     // T+1 auto-transition
	Escalated        int `json:"escalated"`        // T+2
	WriteOffReview   int `json:"write_off_review"` // T+5
	SuspenseBreaches int `json:"suspense_breaches"`
}

// SweepAging applies the T+1/T+2/T+5 timetable to open settlement
// exceptions AND open PB recon breaks, then checks the suspense parking
// SLA. Idempotent — state transitions are guarded; replays no-op.
func (s *OpsService) SweepAging(ctx context.Context) (*AgingResult, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	now := s.now()
	res := &AgingResult{}
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		// T+1: OPEN exceptions/breaks older than 1 day → INVESTIGATING.
		exs, err := tx.StaleExceptions(ctx, now.Add(-AgeInvestigate))
		if err != nil {
			return err
		}
		for _, ex := range exs {
			if ex.Status == ExcStatusOpen {
				if err := tx.MarkExceptionStatus(ctx, ex.ID, ExcStatusInvestigating, now); err != nil {
					return err
				}
				if err := tx.AppendExceptionEvent(ctx, ex.ID, nil, "auto_investigate",
					map[string]any{"age": "T+1"}); err != nil {
					return err
				}
				res.Investigated++
			}
			age := now.Sub(ex.DetectedAt)
			if age >= AgeWriteOff && ex.Actionable() {
				if err := tx.AppendExceptionEvent(ctx, ex.ID, nil, "write_off_review",
					map[string]any{"age_days": int(age.Hours() / 24)}); err != nil {
					return err
				}
				s.alert(ctx, "P1", "WRITE_OFF_REVIEW_DUE",
					fmt.Sprintf("exception %d open %dd — write-off review", ex.ID, int(age.Hours()/24)),
					map[string]string{"exception_id": fmt.Sprintf("%d", ex.ID)})
				res.WriteOffReview++
			} else if age >= AgeEscalate {
				if err := tx.AppendExceptionEvent(ctx, ex.ID, nil, "escalate",
					map[string]any{"age_days": int(age.Hours() / 24)}); err != nil {
					return err
				}
				res.Escalated++
			}
		}
		brks, err := tx.StalePBBreaks(ctx, now.Add(-AgeInvestigate))
		if err != nil {
			return err
		}
		for _, b := range brks {
			age := now.Sub(b.CreatedAt)
			if b.Status == PBBreakOpen {
				if err := tx.EscalatePBBreak(ctx, b.ID, now); err != nil {
					return err
				}
				res.Investigated++
			}
			if age >= AgeWriteOff && b.EscalatedAt != nil {
				if err := tx.AppendPBBreakEvent(ctx, &b.ID, nil, "write_off_review",
					map[string]any{"age_days": int(age.Hours() / 24)}); err != nil {
					return err
				}
				res.WriteOffReview++
			} else if age >= AgeEscalate && b.EscalatedAt == nil {
				if err := tx.AppendPBBreakEvent(ctx, &b.ID, nil, "escalate",
					map[string]any{"age_days": int(age.Hours() / 24)}); err != nil {
					return err
				}
				res.Escalated++
			}
		}
		// Suspense parking SLA (2-day clearing bound).
		items, err := tx.SuspenseBreaches(ctx, now)
		if err != nil {
			return err
		}
		for range items {
			res.SuspenseBreaches++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res.SuspenseBreaches > 0 {
		s.alert(ctx, "P1", CodeSuspenseSLABreach,
			fmt.Sprintf("%d suspense items past the 2-day clearing SLA", res.SuspenseBreaches), nil)
	}
	return res, nil
}

// RequestWriteOff validates the authority tier and queues the four-eyes
// request — the mutation lands via ApplyWriteOff inside the approval tx.
func (s *OpsService) RequestWriteOff(ctx context.Context, exceptionID, breakID *int64,
	currency string, amountUSD decimal.Decimal, reason string, makerID int64) (*WriteOff, *DualResult, error) {
	if s.store == nil {
		return nil, nil, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	if s.dual == nil {
		return nil, nil, excerrors.New(CodeServiceDegraded, "dual-control queue not configured")
	}
	if exceptionID == nil && breakID == nil {
		return nil, nil, excerrors.New("INVALID_REQUEST",
			"write-off requires an exception_id or break_id target")
	}
	if amountUSD.IsNegative() || reason == "" {
		return nil, nil, excerrors.New("INVALID_REQUEST",
			"write-off requires a non-negative amount and a reason")
	}
	tier, role, err := WriteOffTierFor(amountUSD)
	if err != nil {
		return nil, nil, err
	}
	var w WriteOff
	err = s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		ins, err := tx.InsertWriteOff(ctx, WriteOff{
			ExceptionID: exceptionID, BreakID: breakID, Currency: currency,
			Amount: amountUSD, Tier: tier, RequiredRole: role,
			Reason: reason, RequestedBy: makerID, Status: "PENDING",
			CreatedAt: s.now(),
		})
		if err != nil {
			return fmt.Errorf("insert write-off: %w", err)
		}
		w = ins
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	dc, err := s.dual.Submit(ctx, DualSubmit{
		Operation:    OpSettlementWriteOff,
		TargetType:   "settlement_write_off",
		TargetID:     fmt.Sprintf("%d", w.ID),
		Payload:      w,
		RequiredRole: role,
		RequestedBy:  makerID,
		Reason:       fmt.Sprintf("write-off %s %s (%s)", amountUSD, currency, tier),
	})
	if err != nil {
		return &w, nil, fmt.Errorf("dual-control submit: %w", err)
	}
	w.DualControlID = &dc.ID
	return &w, dc, nil
}

// ApplyWriteOff executes an approved write-off inside the caller's
// SERIALIZABLE tx: GL journal (DR 5990_SETTLEMENT_WRITE_OFF / CR
// 1020_SETTLEMENT_FAIL_CLAIM) + target exception/break → WRITTEN_OFF.
func (s *OpsService) ApplyWriteOff(ctx context.Context, tx OpsTx, writeOffID,
	approverID, dcID int64) error {
	if tx == nil {
		return excerrors.New(CodeServiceDegraded, "ops tx not configured")
	}
	w, found, err := tx.LockWriteOff(ctx, writeOffID)
	if err != nil {
		return fmt.Errorf("lock write-off %d: %w", writeOffID, err)
	}
	if !found {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("write-off %d not found", writeOffID))
	}
	if w.Status != "PENDING" {
		return excerrors.New(CodeExceptionConflict,
			fmt.Sprintf("write-off %d already %s", writeOffID, w.Status))
	}
	var jid int64
	if w.Amount.IsPositive() {
		jid, err = tx.PostJournal(ctx, "ADJUSTMENT",
			fmt.Sprintf("settlement write-off %d (%s tier)", w.ID, w.Tier),
			"ops-hardening", fmt.Sprintf("settle-wo:%d", w.ID), writeOffID,
			[]JournalLine{
				{AccountCode: "5990_SETTLEMENT_WRITE_OFF_" + w.Currency,
					Debit: w.Amount, Currency: w.Currency,
					Narrative: fmt.Sprintf("%s write-off: %s", w.Tier, w.Reason)},
				{AccountCode: "1020_SETTLEMENT_FAIL_CLAIM_" + w.Currency,
					Credit: w.Amount, Currency: w.Currency,
					Narrative: "claim written off"},
			})
		if err != nil {
			return excerrors.Wrap(CodeReversalFailed, "write-off journal", err)
		}
	}
	if err := tx.ExecuteWriteOff(ctx, w.ID, jid, approverID, dcID, s.now()); err != nil {
		return fmt.Errorf("execute write-off %d: %w", w.ID, err)
	}
	if w.ExceptionID != nil {
		if err := tx.MarkExceptionWrittenOff(ctx, *w.ExceptionID, s.now()); err != nil {
			return err
		}
	}
	if w.BreakID != nil {
		if err := tx.MarkPBBreakWrittenOff(ctx, *w.BreakID, s.now()); err != nil {
			return err
		}
	}
	return nil
}

// ===========================================================================
// 2. Nostro funding thresholds (3-day outflow cover + CLS pay-in cover)
// ===========================================================================

// EvaluateNostroThresholds recomputes every configured threshold: required
// cover = projected outflows over MinOutflowDays + CLS pay-in cover;
// breach when the live nostro balance < computed threshold. Runs hourly
// (NostroMonitorCadence) from the scheduler.
func (s *OpsService) EvaluateNostroThresholds(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	var breaches int
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		rows, err := tx.ThresholdRowsForUpdate(ctx)
		if err != nil {
			return err
		}
		for _, t := range rows {
			days := t.MinOutflowDays
			if days <= 0 {
				days = NostroOutflowCoverDays
			}
			outflow, err := tx.ProjectedOutflow(ctx, t.NostroAccountID, t.Currency, days)
			if err != nil {
				return fmt.Errorf("outflow projection nostro %d: %w", t.NostroAccountID, err)
			}
			computed := outflow.Add(t.CLSPayInCover)
			balance, err := tx.NostroBalance(ctx, t.NostroAccountID)
			if err != nil {
				return fmt.Errorf("nostro balance %d: %w", t.NostroAccountID, err)
			}
			breach := balance.LessThan(computed)
			if err := tx.UpdateThreshold(ctx, t.ID, computed, breach, s.now()); err != nil {
				return err
			}
			if breach {
				breaches++
				s.alert(ctx, "P1", CodeNostroFundingBreach, fmt.Sprintf(
					"nostro %d %s balance %s < required cover %s (3d outflow + CLS pay-in)",
					t.NostroAccountID, t.Currency, balance, computed), map[string]string{
					"nostro_account_id": fmt.Sprintf("%d", t.NostroAccountID),
					"currency":          t.Currency,
					"threshold":         computed.String(),
					"balance":           balance.String(),
				})
			}
		}
		return nil
	})
	return breaches, err
}

// ===========================================================================
// 3. Cut-off matrix — late-payment handling + fee pass-through
// ===========================================================================

// CheckCutoff evaluates one dispatch against the ops overlay: the sibling
// RailCutoffService decides past-cutoff + value-date roll (holiday-aware);
// this layer adds the §17.14.3 late-fee pass-through verdict.
func (s *OpsService) CheckCutoff(ctx context.Context, rail, currency string,
	at time.Time) (*CutoffEvaluation, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	if at.IsZero() {
		at = s.now()
	}
	rule, found, err := s.store.CutoffRule(ctx, rail, currency)
	if err != nil {
		return nil, fmt.Errorf("cutoff rule %s/%s: %w", rail, currency, err)
	}
	if !found {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("no cut-off rule for %s %s", rail, currency))
	}
	ev := &CutoffEvaluation{Rule: rule, LateFee: decimal.Zero, EffectiveAt: at}
	if s.cutoffs == nil {
		return ev, excerrors.New(CodeServiceDegraded,
			"rail cut-off evaluator not configured")
	}
	dec, err := s.cutoffs.Evaluate(rail, currency, at)
	if err != nil {
		return nil, fmt.Errorf("cutoff evaluate %s/%s: %w", rail, currency, err)
	}
	ev.Late = dec.CutoffPassed
	ev.RollValueDate = rule.Roll && dec.QueuedForNextCycle
	ev.EffectiveAt = dec.ValueDate
	if dec.CutoffPassed {
		// Late payment: fee pass-through rides the dispatch envelope.
		ev.LateFee = rule.LateFee
	}
	return ev, nil
}

// ===========================================================================
// 4. CLS pay-in operations
// ===========================================================================

// CLSPrefundDeadline computes the T-1 22:00 UTC prefunding deadline for a
// value date.
func CLSPrefundDeadline(valueDate time.Time) time.Time {
	vd := valueDate.Truncate(24 * time.Hour)
	return time.Date(vd.Year(), vd.Month(), vd.Day()-1,
		CLSPrefundHour, CLSPrefundMinute, 0, 0, time.UTC)
}

// ScheduleCLSPayIn records a CLS pay-in prefunding requirement — which
// nostro funds it and by when (UNIQUE ccy+value_date+nostro idempotent).
func (s *OpsService) ScheduleCLSPayIn(ctx context.Context, currency string,
	valueDate time.Time, nostroAccountID int64, required decimal.Decimal) (*CLSPayIn, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	var out *CLSPayIn
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		p, err := tx.UpsertCLSPayIn(ctx, CLSPayIn{
			Currency: currency, ValueDate: valueDate.Truncate(24 * time.Hour),
			NostroAccountID: nostroAccountID, Required: required,
			PrefundDeadline: CLSPrefundDeadline(valueDate), Status: "SCHEDULED",
		})
		if err != nil {
			return fmt.Errorf("schedule cls pay-in: %w", err)
		}
		out = &p
		return nil
	})
	return out, err
}

// RecordCLSFunding adds funded amount to a pay-in (FUNDED at cover).
func (s *OpsService) RecordCLSFunding(ctx context.Context, payInID int64,
	amount decimal.Decimal) (*CLSPayIn, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	var out *CLSPayIn
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		p, found, err := tx.LockCLSPayIn(ctx, payInID)
		if err != nil || !found {
			if !found {
				return excerrors.New("NOT_FOUND", fmt.Sprintf("cls pay-in %d not found", payInID))
			}
			return err
		}
		p.Funded = p.Funded.Add(amount)
		switch {
		case p.Funded.GreaterThanOrEqual(p.Required):
			p.Status = "FUNDED"
		default:
			p.Status = "SHORT"
		}
		if err := tx.UpdateCLSPayIn(ctx, p); err != nil {
			return err
		}
		out = &p
		return nil
	})
	return out, err
}

// Failed-pay-in consequence ladder (§17.14.4): each breach of the
// prefunding deadline advances one rung.
var clsLadder = map[int]string{
	1: "alert_finance_ops",
	2: "draw_intraday_borrowing",
	3: "defer_to_next_value_date",
	4: "bilateral_waterfall_fallback",
}

// CLSPayInMonitor evaluates open pay-ins: past the T-1 22:00 UTC deadline
// and underfunded → advance the consequence ladder; member-outage flag →
// fallback routing is armed.
func (s *OpsService) CLSPayInMonitor(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	var advanced int
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		due, err := tx.DueCLSPayIns(ctx, s.now())
		if err != nil {
			return err
		}
		for _, p := range due {
			if p.Status == "PAID_IN" || p.Status == "FAILED" {
				continue
			}
			changed := false
			if p.MemberOutage && p.Fallback == "" {
				// Settlement-member outage → bilateral waterfall fallback.
				p.Fallback = "NETTING" // ALT_PVP → NETTING → CONTROLLED_GROSS
				changed = true
			}
			if s.now().After(p.PrefundDeadline) && p.Funded.LessThan(p.Required) {
				if p.LadderStep < len(clsLadder) {
					p.LadderStep++
					changed = true
					s.alert(ctx, "P1", "CLS_PAYIN_SHORTFALL", fmt.Sprintf(
						"CLS pay-in %d %s %s underfunded — ladder step %d (%s)",
						p.ID, p.Required.Sub(p.Funded), p.Currency, p.LadderStep,
						clsLadder[p.LadderStep]), map[string]string{
						"payin_id": fmt.Sprintf("%d", p.ID),
						"step":     fmt.Sprintf("%d", p.LadderStep),
					})
				}
				if p.LadderStep >= len(clsLadder) {
					p.Status = "FAILED"
					p.Fallback = "CONTROLLED_GROSS"
					changed = true
				}
			}
			if changed {
				if err := tx.UpdateCLSPayIn(ctx, p); err != nil {
					return err
				}
				advanced++
			}
		}
		return nil
	})
	return advanced, err
}

// ===========================================================================
// 5. FX fail economics — close-out + fail interest (supersedes CSDR for FX)
// ===========================================================================

// FXCloseoutInput prices one FX_CLOSEOUT fail.
type FXCloseoutInput struct {
	PolicyRateBP decimal.Decimal // policy rate in basis points
	MarkRate     decimal.Decimal // optional explicit mark; zero → pricer seam
}

// CloseOutFXFail computes the §17.14 close-out for one FX-regime fail:
// replacement cost = |mark − original| × unsettled amount at the current
// market mark; fail interest = amount × (policy + 100bps)/10000 ×
// accrual_days/360 accruing from ISD+1. Idempotent (UNIQUE fail_id); the
// fail is stamped CLOSED_OUT and the ISDA/FX-Global-Code treatment is
// recorded on the row.
func (s *OpsService) CloseOutFXFail(ctx context.Context, failID int64,
	in FXCloseoutInput) (*FXFailCloseout, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	if s.pricer == nil && in.MarkRate.IsZero() {
		return nil, excerrors.New(CodeServiceDegraded,
			"market pricer not configured for close-out")
	}
	var out *FXFailCloseout
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		f, found, err := tx.FailByIDForUpdate(ctx, failID)
		if err != nil {
			return fmt.Errorf("lock fail %d: %w", failID, err)
		}
		if !found {
			return excerrors.New("NOT_FOUND", fmt.Sprintf("settlement fail %d not found", failID))
		}
		if f.Regime != RegimeFXCloseout {
			return excerrors.New("INVALID_REQUEST", fmt.Sprintf(
				"fail %d is %s — only FX_CLOSEOUT legs take §17.14 economics", failID, f.Regime))
		}
		if f.Status == FailClosedOut || f.Status == FailResolved {
			return excerrors.New(CodeExceptionConflict,
				fmt.Sprintf("fail %d already %s", failID, f.Status))
		}
		mark := in.MarkRate
		if mark.IsZero() {
			mark, err = s.pricer.MarkPrice(ctx, f.TradeID)
			if err != nil {
				return fmt.Errorf("close-out mark fail %d: %w", failID, err)
			}
		}
		orig, found, err := tx.TradePrice(ctx, f.TradeID)
		if err != nil {
			return fmt.Errorf("original rate trade %d: %w", f.TradeID, err)
		}
		if !found {
			return excerrors.New("NOT_FOUND",
				fmt.Sprintf("trade %d for fail %d not found", f.TradeID, failID))
		}
		now := s.now()
		// Fail interest accrues from ISD+1 through the close-out date.
		accrualDays := int(now.Truncate(24*time.Hour).Sub(
			f.ISD.Truncate(24*time.Hour).Add(24*time.Hour)).Hours()/24) + 1
		if accrualDays < 1 {
			accrualDays = 1
		}
		replacement := mark.Sub(orig).Abs().Mul(f.Amount)
		rate := in.PolicyRateBP.Add(decimal.RequireFromString(FailInterestSpreadBP))
		interest := f.Amount.Mul(rate).Div(decimal.NewFromInt(10000)).
			Mul(decimal.NewFromInt(int64(accrualDays))).
			Div(decimal.NewFromFloat(FailInterestDCC))
		co, created, err := tx.InsertFXCloseout(ctx, FXFailCloseout{
			FailID: failID, OriginalRate: orig, CloseoutRate: mark,
			ReplacementCost: replacement, Currency: f.Currency,
			PolicyRateBP: in.PolicyRateBP, FailInterest: interest,
			AccrualDays: accrualDays, Treatment: "ISDA_CLOSEOUT",
		})
		if err != nil {
			return fmt.Errorf("record close-out fail %d: %w", failID, err)
		}
		if !created {
			out = &co
			return nil // idempotent replay
		}
		// GL evidence: replacement-cost claim + fail-interest accrual
		// (claim asset vs fail-interest revenue contra).
		var lines []JournalLine
		if replacement.IsPositive() {
			lines = append(lines,
				JournalLine{AccountCode: "1020_SETTLEMENT_FAIL_CLAIM_" + f.Currency,
					Debit: replacement, Currency: f.Currency,
					Narrative: "FX replacement-cost close-out claim"},
				JournalLine{AccountCode: "1090_SETTLEMENT_FAIL_MEMO_" + f.Currency,
					Credit: replacement, Currency: f.Currency,
					Narrative: "FX close-out contra"})
		}
		if interest.IsPositive() {
			lines = append(lines,
				JournalLine{AccountCode: "1020_SETTLEMENT_FAIL_CLAIM_" + f.Currency,
					Debit: interest, Currency: f.Currency,
					Narrative: "fail interest (policy+100bps, ISD+1)"},
				JournalLine{AccountCode: "4600_FAIL_INTEREST_REVENUE_" + f.Currency,
					Credit: interest, Currency: f.Currency,
					Narrative: "fail-interest accrual"})
		}
		if len(lines) > 0 {
			if _, err := tx.PostJournal(ctx, "SETTLEMENT",
				fmt.Sprintf("FX fail close-out — fail %d trade %d", failID, f.TradeID),
				"ops-hardening", fmt.Sprintf("fx-closeout:%d", failID), f.TradeID, lines); err != nil {
				return fmt.Errorf("close-out journal: %w", err)
			}
		}
		if err := tx.SetFailStatus(ctx, failID, FailClosedOut, now); err != nil {
			return err
		}
		out = &co
		return nil
	})
	return out, err
}

// ===========================================================================
// 6. Rail failover — retry timetable + fallback rails + duplicate guard
// ===========================================================================

// RailFallbacks is the §17.14.5 outage-routing map (SWIFT → designated
// backup correspondent handled by the dispatcher layer; FEDNOW → ACH).
var RailFallbacks = map[string]string{
	"FEDNOW": "ACH",
	"SWIFT":  "CORRESPONDENT_BACKUP",
}

// QueuePayment enqueues a payment for rail dispatch — the duplicate-
// payment guard is payment_ref UNIQUE + the DISPATCHED latch: a replayed
// enqueue returns the existing row (created=false).
func (s *OpsService) QueuePayment(ctx context.Context, p FailoverPayment) (*FailoverPayment, bool, error) {
	if s.store == nil {
		return nil, false, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	if p.PaymentRef == "" || p.Amount.IsNegative() || p.Rail == "" {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"payment requires ref, rail and a non-negative amount")
	}
	if p.FallbackRail == "" {
		p.FallbackRail = RailFallbacks[p.Rail]
	}
	p.NextAttemptAt = s.now()
	p.Status = "QUEUED"
	var out *FailoverPayment
	var created bool
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		ins, c, err := tx.EnqueuePayment(ctx, p)
		if err != nil {
			return fmt.Errorf("enqueue payment %s: %w", p.PaymentRef, err)
		}
		out, created = &ins, c
		return nil
	})
	return out, created, err
}

// ProcessFailover dispatches due queued payments through the rail
// dispatcher. On failure the retry ladder advances (15m/1h/4h); when the
// ladder is exhausted the payment fails over to the fallback rail once
// (ladder resets), else lands EXHAUSTED with RAIL_FAILOVER_EXHAUSTED.
func (s *OpsService) ProcessFailover(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	if s.dispatch == nil {
		return 0, excerrors.New(CodeServiceDegraded, "rail dispatcher not configured")
	}
	now := s.now()
	due, err := s.store.DueFailoverPayments(ctx, now)
	if err != nil {
		return 0, err
	}
	var dispatched int
	for _, d := range due {
		err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
			p, found, err := tx.LockFailoverPayment(ctx, d.ID)
			if err != nil || !found {
				return err
			}
			if p.Status == "DISPATCHED" || p.Status == "EXHAUSTED" || p.Status == "CANCELLED" {
				return nil // terminal — never re-dispatch (dup guard)
			}
			if p.NextAttemptAt.After(now) {
				return nil // raced — not due under the lock
			}
			rail := p.Rail
			if p.Attempts >= len(RailRetrySchedule) && p.FallbackRail != "" {
				rail = p.FallbackRail // failover leg
			}
			if err := s.dispatch.Dispatch(ctx, rail, p.Payload); err == nil {
				p.Status = "DISPATCHED"
				p.Attempts++
				return tx.UpdateFailoverPayment(ctx, p)
			}
			// Dispatch failed — advance the ladder.
			p.Attempts++
			switch {
			case p.Attempts < len(RailRetrySchedule):
				p.Status = "RETRYING"
				p.NextAttemptAt = now.Add(RailRetrySchedule[p.Attempts])
			case p.FallbackRail != "" && p.Rail != p.FallbackRail:
				// Failover to the fallback rail — ladder restarts on it.
				p.Rail = p.FallbackRail
				p.FallbackRail = ""
				p.Attempts = 0
				p.Status = "RETRYING"
				p.NextAttemptAt = now.Add(RailRetrySchedule[0])
			default:
				p.Status = "EXHAUSTED"
				s.alert(ctx, "P0", CodeRailFailoverExhausted, fmt.Sprintf(
					"payment %s exhausted retries on %s", p.PaymentRef, p.Rail),
					map[string]string{"payment_ref": p.PaymentRef, "rail": p.Rail})
			}
			return tx.UpdateFailoverPayment(ctx, p)
		})
		if err != nil {
			return dispatched, err
		}
		dispatched++
	}
	return dispatched, nil
}

// ===========================================================================
// 7. Herstatt principal-exposure metric
// ===========================================================================

// OpenHerstatt records a paid-but-not-received window.
func (s *OpsService) OpenHerstatt(ctx context.Context, counterpartyID int64,
	currency string, paid, receivable decimal.Decimal, capMinutes int) (*HerstattExposure, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	if capMinutes <= 0 {
		capMinutes = HerstattDefaultCapMinutes
	}
	var out *HerstattExposure
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		h, err := tx.InsertHerstatt(ctx, HerstattExposure{
			CounterpartyID: counterpartyID, Currency: currency,
			Paid: paid, Receivable: receivable,
			WindowOpenedAt: s.now(), CapMinutes: capMinutes,
		})
		if err != nil {
			return err
		}
		out = &h
		return nil
	})
	return out, err
}

// SettleHerstatt closes the window — the counter-leg arrived.
func (s *OpsService) SettleHerstatt(ctx context.Context, id int64) error {
	if s.store == nil {
		return excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		return tx.SettleHerstatt(ctx, id, s.now())
	})
}

// MonitorHerstatt breaches exposures open past their duration cap —
// intraday cadence from the scheduler; each breach raises a P0 page once
// (breached flag is the dedupe latch).
func (s *OpsService) MonitorHerstatt(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	var n int
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		open, err := tx.OpenHerstattForUpdate(ctx)
		if err != nil {
			return err
		}
		for _, h := range open {
			if h.Breached {
				continue
			}
			if s.now().After(h.WindowOpenedAt.Add(time.Duration(h.CapMinutes) * time.Minute)) {
				if err := tx.MarkHerstattBreached(ctx, h.ID); err != nil {
					return err
				}
				n++
				s.alert(ctx, "P0", CodeHerstattLimitBreach, fmt.Sprintf(
					"Herstatt exposure %d — %s %s receivable from counterparty %d open %dm (cap %dm)",
					h.ID, h.Receivable, h.Currency, h.CounterpartyID,
					int(s.now().Sub(h.WindowOpenedAt).Minutes()), h.CapMinutes),
					map[string]string{
						"exposure_id":     fmt.Sprintf("%d", h.ID),
						"counterparty_id": fmt.Sprintf("%d", h.CounterpartyID),
						"currency":        h.Currency,
					})
			}
		}
		return nil
	})
	return n, err
}

// ===========================================================================
// 8. LP-default playbook
// ===========================================================================

// RecordLPDefault advances the LP-default ladder — QUOTE_WITHDRAWN
// (quote withdrawal beyond tolerance) → FLOORS_WIDENED (auction floors
// widened) → ADL_Q5 (escalate to quintile-5 ADL). Stages only move
// forward; a repeat stage is idempotent (returns the existing row).
func (s *OpsService) RecordLPDefault(ctx context.Context, lpID int64,
	stage string, detail map[string]any) (*LPDefaultEvent, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "ops store not configured")
	}
	if lpStageOrder[stage] == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unknown LP-default stage %q", stage))
	}
	var out *LPDefaultEvent
	err := s.store.InTx(ctx, func(ctx context.Context, tx OpsTx) error {
		latest, found, err := tx.LPDefaultLatestTx(ctx, lpID)
		if err != nil {
			return err
		}
		if found && lpStageOrder[latest.Stage] >= lpStageOrder[stage] {
			out = &latest // idempotent or regression — no backward stage
			return nil
		}
		e, err := tx.InsertLPDefault(ctx, LPDefaultEvent{
			LPID: lpID, Stage: stage, Detail: detail, At: s.now(),
		})
		if err != nil {
			return err
		}
		s.alert(ctx, "P1", "LP_DEFAULT_"+stage, fmt.Sprintf(
			"LP %d default playbook → %s", lpID, stage), map[string]string{
			"lp_id": fmt.Sprintf("%d", lpID), "stage": stage})
		out = &e
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// PgxOpsStore — production OpsStore
// ---------------------------------------------------------------------------

// PgxOpsStore implements OpsStore over pgx.
type PgxOpsStore struct{ Q Querier }

// NewPgxOpsStore binds the store to a pool.
func NewPgxOpsStore(pool *pgxpool.Pool) *PgxOpsStore {
	return &PgxOpsStore{Q: pool}
}

// OpsTxFromPgx exposes the tx primitives over a caller-owned pgx.Tx — the
// dual-control executor path for ApplyWriteOff.
func OpsTxFromPgx(tx pgx.Tx) OpsTx { return pgxOpsTx{q: tx} }

func (s *PgxOpsStore) InTx(ctx context.Context, fn func(ctx context.Context, tx OpsTx) error) error {
	return RunInTx(ctx, s.Q, func(q Querier) error {
		return fn(ctx, pgxOpsTx{q: q})
	})
}

func (s *PgxOpsStore) WriteOffByID(ctx context.Context, id int64) (WriteOff, bool, error) {
	w, err := scanWriteOff(s.Q.QueryRow(ctx,
		`SELECT `+writeOffCols+` FROM settlement_write_offs WHERE id=$1`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return WriteOff{}, false, nil
	}
	return w, err == nil, err
}

func (s *PgxOpsStore) Thresholds(ctx context.Context) ([]FundingThreshold, error) {
	return scanThresholds(ctx, s.Q, false)
}

func (s *PgxOpsStore) CutoffRule(ctx context.Context, rail, currency string) (CutoffRule, bool, error) {
	var r CutoffRule
	var cutoff string
	var fee string
	err := s.Q.QueryRow(ctx, `
		SELECT rail, currency, to_char(cutoff_utc,'HH24:MI'), value_date_roll,
		       late_fee::text
		  FROM rail_cutoff_matrix WHERE rail=$1 AND currency=$2`,
		rail, currency).Scan(&r.Rail, &r.Currency, &cutoff, &r.Roll, &fee)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return CutoffRule{}, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.CutoffUTC = cutoff
	r.LateFee, err = decimal.NewFromString(fee)
	return r, err == nil, err
}

func (s *PgxOpsStore) DueFailoverPayments(ctx context.Context, at time.Time) ([]FailoverPayment, error) {
	return scanPayments(ctx, s.Q, `status IN ('QUEUED','RETRYING') AND next_attempt_at <= $1`, at)
}

func (s *PgxOpsStore) OpenHerstatt(ctx context.Context) ([]HerstattExposure, error) {
	rows, err := s.Q.Query(ctx,
		`SELECT `+herstattCols+` FROM herstatt_exposures WHERE settled_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HerstattExposure
	for rows.Next() {
		h, err := scanHerstatt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *PgxOpsStore) LPDefaultLatest(ctx context.Context, lpID int64) (LPDefaultEvent, bool, error) {
	e, err := scanLPEvent(s.Q.QueryRow(ctx, `
		SELECT id, lp_id, stage, COALESCE(detail::text,'{}'), created_at
		  FROM lp_default_events WHERE lp_id=$1 ORDER BY id DESC LIMIT 1`, lpID))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return LPDefaultEvent{}, false, nil
	}
	return e, err == nil, err
}

func (s *PgxOpsStore) RestitutionsUndelivered(ctx context.Context) ([]Restitution, error) {
	rows, err := s.Q.Query(ctx, `
		SELECT `+restitutionCols+` FROM pb_credit_restitutions
		 WHERE status='APPLIED' AND (pb_notified=false OR margin_recalc_queued=false)
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Restitution
	for rows.Next() {
		r, err := scanRestitution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------

type pgxOpsTx struct{ q Querier }

func (t pgxOpsTx) StaleExceptions(ctx context.Context, olderThan time.Time) ([]SettlementException, error) {
	rows, err := t.q.Query(ctx, `
		SELECT `+exceptionCols+` FROM settlement_exceptions
		 WHERE status IN ('OPEN','INVESTIGATING') AND detected_at <= $1
		 ORDER BY detected_at FOR UPDATE`, olderThan)
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

func (t pgxOpsTx) StalePBBreaks(ctx context.Context, olderThan time.Time) ([]PBReconBreak, error) {
	rows, err := t.q.Query(ctx, `
		SELECT `+pbBreakCols+` FROM pb_recon_breaks
		 WHERE status IN ('OPEN','INVESTIGATING') AND created_at <= $1
		 ORDER BY created_at FOR UPDATE`, olderThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PBReconBreak
	for rows.Next() {
		b, err := scanPBBreak(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (t pgxOpsTx) MarkExceptionStatus(ctx context.Context, id int64, st ExceptionStatus, at time.Time) error {
	_, err := t.q.Exec(ctx, `
		UPDATE settlement_exceptions SET status=$2, updated_at=$3
		 WHERE id=$1 AND status IN ('OPEN','INVESTIGATING')`, id, string(st), at)
	return err
}

func (t pgxOpsTx) EscalatePBBreak(ctx context.Context, id int64, at time.Time) error {
	_, err := t.q.Exec(ctx, `
		UPDATE pb_recon_breaks SET escalated_at=$2, status='INVESTIGATING', updated_at=$2
		 WHERE id=$1 AND status='OPEN'`, id, at)
	return err
}

func (t pgxOpsTx) AppendExceptionEvent(ctx context.Context, exceptionID int64,
	actorID *int64, action string, detail map[string]any) error {
	return pgxExceptionTx{q: t.q}.AppendEvent(ctx, exceptionID, actorID, action, detail)
}

func (t pgxOpsTx) AppendPBBreakEvent(ctx context.Context, breakID, actorID *int64,
	action string, detail map[string]any) error {
	return pgxReconTx{q: t.q}.AppendReconEvent(ctx, breakID, nil, actorID, action, detail)
}

func (t pgxOpsTx) SuspenseBreaches(ctx context.Context, at time.Time) ([]SuspenseItem, error) {
	rows, err := t.q.Query(ctx, `
		SELECT id, bank_tx_id, currency, amount::text,
		       unmatched_reason::text, quarantine_status::text, sla_expires_at
		  FROM suspense_account_mappings
		 WHERE quarantine_status IN ('QUARANTINED','INVESTIGATING')
		   AND sla_expires_at < $1
		 ORDER BY sla_expires_at`, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SuspenseItem
	for rows.Next() {
		var it SuspenseItem
		var amt string
		if err := rows.Scan(&it.ID, &it.BankTxID, &it.Currency, &amt,
			&it.Reason, &it.QuarantineState, &it.SLAExpiresAt); err != nil {
			return nil, err
		}
		if it.Amount, err = decimal.NewFromString(amt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

const writeOffCols = `id, exception_id, break_id, currency, amount::text, tier,
	required_role, reason, dual_control_request_id, requested_by, approved_by,
	gl_journal_id, status, created_at`

func scanWriteOff(row interface{ Scan(...any) error }) (WriteOff, error) {
	var w WriteOff
	var amt string
	err := row.Scan(&w.ID, &w.ExceptionID, &w.BreakID, &w.Currency, &amt,
		&w.Tier, &w.RequiredRole, &w.Reason, &w.DualControlID, &w.RequestedBy,
		&w.ApprovedBy, &w.GLJournalID, &w.Status, &w.CreatedAt)
	if err != nil {
		return w, err
	}
	w.Amount, err = decimal.NewFromString(amt)
	return w, err
}

func (t pgxOpsTx) InsertWriteOff(ctx context.Context, w WriteOff) (WriteOff, error) {
	return scanWriteOff(t.q.QueryRow(ctx, `
		INSERT INTO settlement_write_offs
		    (exception_id, break_id, currency, amount, tier, required_role,
		     reason, requested_by, status)
		VALUES ($1,$2,$3,$4::numeric,$5,$6,$7,$8,'PENDING')
		RETURNING `+writeOffCols,
		w.ExceptionID, w.BreakID, w.Currency, w.Amount.String(),
		w.Tier, w.RequiredRole, w.Reason, w.RequestedBy))
}

func (t pgxOpsTx) LockWriteOff(ctx context.Context, id int64) (WriteOff, bool, error) {
	w, err := scanWriteOff(t.q.QueryRow(ctx,
		`SELECT `+writeOffCols+` FROM settlement_write_offs WHERE id=$1 FOR UPDATE`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return WriteOff{}, false, nil
	}
	return w, err == nil, err
}

func (t pgxOpsTx) ExecuteWriteOff(ctx context.Context, id int64, journalID,
	approverID, dcID int64, at time.Time) error {
	var jid, dc any
	if journalID > 0 {
		jid = journalID
	}
	if dcID > 0 {
		dc = dcID
	}
	tag, err := t.q.Exec(ctx, `
		UPDATE settlement_write_offs
		   SET status='EXECUTED', gl_journal_id=$2, approved_by=$3,
		       dual_control_request_id=COALESCE($4, dual_control_request_id),
		       decided_at=$5
		 WHERE id=$1 AND status='PENDING'`, id, jid, approverID, dc, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeExceptionConflict,
			fmt.Sprintf("write-off %d not pending", id))
	}
	return nil
}

func (t pgxOpsTx) MarkExceptionWrittenOff(ctx context.Context, id int64, at time.Time) error {
	_, err := t.q.Exec(ctx, `
		UPDATE settlement_exceptions SET status='WRITTEN_OFF',
		       resolution_action='WRITE_OFF', resolved_at=$2, updated_at=$2
		 WHERE id=$1 AND status IN ('OPEN','INVESTIGATING')`, id, at)
	return err
}

func (t pgxOpsTx) MarkPBBreakWrittenOff(ctx context.Context, id int64, at time.Time) error {
	_, err := t.q.Exec(ctx, `
		UPDATE pb_recon_breaks SET status='WRITTEN_OFF', updated_at=$2
		 WHERE id=$1 AND status IN ('OPEN','INVESTIGATING')`, id, at)
	return err
}

func (t pgxOpsTx) PostJournal(ctx context.Context, entryType, description,
	postedBy, idemKey string, referenceID int64, lines []JournalLine) (int64, error) {
	return pgxExceptionTx{q: t.q}.PostJournal(ctx, entryType, description,
		postedBy, idemKey, referenceID, lines)
}

const thresholdCols = `id, nostro_account_id, currency, min_outflow_cover_days,
	cls_payin_cover::text, computed_threshold::text, concentration_limit_pct::text,
	backup_nostro_id, breach`

func scanThresholds(ctx context.Context, q Querier, forUpdate bool) ([]FundingThreshold, error) {
	suffix := ""
	if forUpdate {
		suffix = " FOR UPDATE"
	}
	rows, err := q.Query(ctx, `SELECT `+thresholdCols+` FROM nostro_funding_thresholds ORDER BY id`+suffix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FundingThreshold
	for rows.Next() {
		var t FundingThreshold
		var cover, comp, pct string
		if err := rows.Scan(&t.ID, &t.NostroAccountID, &t.Currency, &t.MinOutflowDays,
			&cover, &comp, &pct, &t.BackupNostroID, &t.Breach); err != nil {
			return nil, err
		}
		var err2 error
		if t.CLSPayInCover, err2 = decimal.NewFromString(cover); err2 != nil {
			return nil, err2
		}
		if t.ComputedThreshold, err2 = decimal.NewFromString(comp); err2 != nil {
			return nil, err2
		}
		if t.ConcentrationPct, err2 = decimal.NewFromString(pct); err2 != nil {
			return nil, err2
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (t pgxOpsTx) ThresholdRowsForUpdate(ctx context.Context) ([]FundingThreshold, error) {
	return scanThresholds(ctx, t.q, true)
}

// ProjectedOutflow sums PENDING settlement-instruction amounts over the
// next `days` days for the nostro account — the 3-day cover input.
func (t pgxOpsTx) ProjectedOutflow(ctx context.Context, nostroAccountID int64,
	ccy string, days int) (decimal.Decimal, error) {
	var sum string
	err := t.q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount),0)::text FROM settlement_instructions
		 WHERE nostro_account_id=$1 AND currency=$2 AND direction='PAY'
		   AND status='PENDING'
		   AND settlement_date <= CURRENT_DATE + $3`, nostroAccountID, ccy, days).Scan(&sum)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromString(sum)
}

func (t pgxOpsTx) NostroBalance(ctx context.Context, nostroAccountID int64) (decimal.Decimal, error) {
	var bal string
	err := t.q.QueryRow(ctx,
		`SELECT balance::text FROM nostro_accounts WHERE id=$1`, nostroAccountID).Scan(&bal)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromString(bal)
}

func (t pgxOpsTx) UpdateThreshold(ctx context.Context, id int64, computed decimal.Decimal,
	breach bool, at time.Time) error {
	_, err := t.q.Exec(ctx, `
		UPDATE nostro_funding_thresholds
		   SET computed_threshold=$2::numeric, breach=$3, last_evaluated_at=$4, updated_at=$4
		 WHERE id=$1`, id, computed.String(), breach, at)
	return err
}

const clsPayInCols = `id, currency, value_date, nostro_account_id,
	required_amount::text, funded_amount::text, prefund_deadline, ladder_step,
	member_outage, COALESCE(fallback,''), status`

func scanCLSPayIn(row interface{ Scan(...any) error }) (CLSPayIn, error) {
	var p CLSPayIn
	var req, fund string
	err := row.Scan(&p.ID, &p.Currency, &p.ValueDate, &p.NostroAccountID,
		&req, &fund, &p.PrefundDeadline, &p.LadderStep, &p.MemberOutage,
		&p.Fallback, &p.Status)
	if err != nil {
		return p, err
	}
	if p.Required, err = decimal.NewFromString(req); err != nil {
		return p, err
	}
	p.Funded, err = decimal.NewFromString(fund)
	return p, err
}

func (t pgxOpsTx) UpsertCLSPayIn(ctx context.Context, p CLSPayIn) (CLSPayIn, error) {
	return scanCLSPayIn(t.q.QueryRow(ctx, `
		INSERT INTO cls_payin_events
		    (currency, value_date, nostro_account_id, required_amount,
		     prefund_deadline, status)
		VALUES ($1,$2,$3,$4::numeric,$5,'SCHEDULED')
		ON CONFLICT (currency, value_date, nostro_account_id)
		    DO UPDATE SET required_amount=EXCLUDED.required_amount, updated_at=now()
		RETURNING `+clsPayInCols,
		p.Currency, p.ValueDate, p.NostroAccountID, p.Required.String(), p.PrefundDeadline))
}

func (t pgxOpsTx) LockCLSPayIn(ctx context.Context, id int64) (CLSPayIn, bool, error) {
	p, err := scanCLSPayIn(t.q.QueryRow(ctx,
		`SELECT `+clsPayInCols+` FROM cls_payin_events WHERE id=$1 FOR UPDATE`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return CLSPayIn{}, false, nil
	}
	return p, err == nil, err
}

func (t pgxOpsTx) UpdateCLSPayIn(ctx context.Context, p CLSPayIn) error {
	var fb any
	if p.Fallback != "" {
		fb = p.Fallback
	}
	_, err := t.q.Exec(ctx, `
		UPDATE cls_payin_events
		   SET funded_amount=$2::numeric, ladder_step=$3, member_outage=$4,
		       fallback=$5, status=$6, updated_at=now()
		 WHERE id=$1`, p.ID, p.Funded.String(), p.LadderStep, p.MemberOutage, fb, p.Status)
	return err
}

func (t pgxOpsTx) DueCLSPayIns(ctx context.Context, at time.Time) ([]CLSPayIn, error) {
	rows, err := t.q.Query(ctx, `
		SELECT `+clsPayInCols+` FROM cls_payin_events
		 WHERE status IN ('SCHEDULED','SHORT','FUNDED')
		   AND (prefund_deadline <= $1 OR member_outage = true)
		 ORDER BY id FOR UPDATE`, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CLSPayIn
	for rows.Next() {
		p, err := scanCLSPayIn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (t pgxOpsTx) FailByIDForUpdate(ctx context.Context, failID int64) (SettlementFail, bool, error) {
	f, err := scanFail(t.q.QueryRow(ctx,
		`SELECT `+failCols+` FROM settlement_fails WHERE id=$1 FOR UPDATE`, failID))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return SettlementFail{}, false, nil
	}
	return f, err == nil, err
}

func (t pgxOpsTx) TradePrice(ctx context.Context, tradeID int64) (decimal.Decimal, bool, error) {
	return pgxBuyInTx{q: t.q}.TradePrice(ctx, tradeID)
}

const fxCloseoutCols = `id, fail_id, original_rate::text, closeout_rate::text,
	replacement_cost::text, currency, policy_rate_bp::text, fail_interest_amount::text,
	accrual_days, isda_treatment`

func (t pgxOpsTx) InsertFXCloseout(ctx context.Context, c FXFailCloseout) (FXFailCloseout, bool, error) {
	var orig, mark, repl, pol, intr string
	err := t.q.QueryRow(ctx, `
		INSERT INTO fx_fail_closeouts
		    (fail_id, original_rate, closeout_rate, replacement_cost, currency,
		     policy_rate_bp, fail_interest_amount, accrual_days, isda_treatment)
		VALUES ($1,$2::numeric,$3::numeric,$4::numeric,$5,$6::numeric,$7::numeric,$8,$9)
		ON CONFLICT (fail_id) DO NOTHING
		RETURNING id, original_rate::text, closeout_rate::text, replacement_cost::text,
		          policy_rate_bp::text, fail_interest_amount::text`,
		c.FailID, c.OriginalRate.String(), c.CloseoutRate.String(),
		c.ReplacementCost.String(), c.Currency, c.PolicyRateBP.String(),
		c.FailInterest.String(), c.AccrualDays, c.Treatment).
		Scan(&c.ID, &orig, &mark, &repl, &pol, &intr)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	for _, rec := range []struct {
		dst *decimal.Decimal
		raw string
	}{{&c.OriginalRate, orig}, {&c.CloseoutRate, mark},
		{&c.ReplacementCost, repl}, {&c.PolicyRateBP, pol}, {&c.FailInterest, intr}} {
		d, err := decimal.NewFromString(rec.raw)
		if err != nil {
			return c, false, fmt.Errorf("closeout numeric %q: %w", rec.raw, err)
		}
		*rec.dst = d
	}
	return c, true, nil
}

func (t pgxOpsTx) SetFailStatus(ctx context.Context, id int64, st FailStatus, at time.Time) error {
	return pgxFailTx{q: t.q}.SetFailStatus(ctx, id, st, at)
}

const failoverCols = `id, payment_ref, rail, COALESCE(fallback_rail,''), currency,
	amount::text, payload, attempts, next_attempt_at, status`

func scanPayments(ctx context.Context, q Querier, where string, args ...any) ([]FailoverPayment, error) {
	rows, err := q.Query(ctx, `SELECT `+failoverCols+` FROM rail_failover_queue WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailoverPayment
	for rows.Next() {
		p, err := scanPaymentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPaymentRow(row interface{ Scan(...any) error }) (FailoverPayment, error) {
	var p FailoverPayment
	var amt string
	var payload []byte
	err := row.Scan(&p.ID, &p.PaymentRef, &p.Rail, &p.FallbackRail, &p.Currency,
		&amt, &payload, &p.Attempts, &p.NextAttemptAt, &p.Status)
	if err != nil {
		return p, err
	}
	if p.Amount, err = decimal.NewFromString(amt); err != nil {
		return p, err
	}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &p.Payload)
	}
	return p, nil
}

func (t pgxOpsTx) EnqueuePayment(ctx context.Context, p FailoverPayment) (FailoverPayment, bool, error) {
	pb, err := jsonbArg(p.Payload)
	if err != nil {
		return p, false, err
	}
	var fb any
	if p.FallbackRail != "" {
		fb = p.FallbackRail
	}
	ins, err := scanPaymentRow(t.q.QueryRow(ctx, `
		INSERT INTO rail_failover_queue
		    (payment_ref, rail, fallback_rail, currency, amount, payload,
		     next_attempt_at, status)
		VALUES ($1,$2,$3,$4,$5::numeric,$6::jsonb,$7,'QUEUED')
		ON CONFLICT (payment_ref) DO NOTHING
		RETURNING `+failoverCols,
		p.PaymentRef, p.Rail, fb, p.Currency, p.Amount.String(), pb,
		p.NextAttemptAt))
	if stderrors.Is(err, pgx.ErrNoRows) {
		// Duplicate-payment guard: return the existing row, created=false.
		existing, err2 := scanPaymentRow(t.q.QueryRow(ctx,
			`SELECT `+failoverCols+` FROM rail_failover_queue WHERE payment_ref=$1`,
			p.PaymentRef))
		if err2 != nil {
			return p, false, err2
		}
		return existing, false, nil
	}
	return ins, err == nil, err
}

func (t pgxOpsTx) LockFailoverPayment(ctx context.Context, id int64) (FailoverPayment, bool, error) {
	p, err := scanPaymentRow(t.q.QueryRow(ctx,
		`SELECT `+failoverCols+` FROM rail_failover_queue WHERE id=$1 FOR UPDATE`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return FailoverPayment{}, false, nil
	}
	return p, err == nil, err
}

func (t pgxOpsTx) UpdateFailoverPayment(ctx context.Context, p FailoverPayment) error {
	var fb any
	if p.FallbackRail != "" {
		fb = p.FallbackRail
	}
	var dispatchedAt any
	if p.Status == "DISPATCHED" {
		dispatchedAt = time.Now().UTC()
	}
	_, err := t.q.Exec(ctx, `
		UPDATE rail_failover_queue
		   SET rail=$2, fallback_rail=$3, attempts=$4, next_attempt_at=$5,
		       status=$6, dispatched_at=COALESCE($7, dispatched_at), updated_at=now()
		 WHERE id=$1`, p.ID, p.Rail, fb, p.Attempts, p.NextAttemptAt, p.Status, dispatchedAt)
	return err
}

const herstattCols = `id, counterparty_id, currency, paid_amount::text,
	receivable_amount::text, window_open_at, duration_cap_minutes, breached, settled_at`

func scanHerstatt(row interface{ Scan(...any) error }) (HerstattExposure, error) {
	var h HerstattExposure
	var paid, rec string
	err := row.Scan(&h.ID, &h.CounterpartyID, &h.Currency, &paid, &rec,
		&h.WindowOpenedAt, &h.CapMinutes, &h.Breached, &h.SettledAt)
	if err != nil {
		return h, err
	}
	if h.Paid, err = decimal.NewFromString(paid); err != nil {
		return h, err
	}
	h.Receivable, err = decimal.NewFromString(rec)
	return h, err
}

func (t pgxOpsTx) OpenHerstattForUpdate(ctx context.Context) ([]HerstattExposure, error) {
	rows, err := t.q.Query(ctx,
		`SELECT `+herstattCols+` FROM herstatt_exposures WHERE settled_at IS NULL ORDER BY id FOR UPDATE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HerstattExposure
	for rows.Next() {
		h, err := scanHerstatt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (t pgxOpsTx) InsertHerstatt(ctx context.Context, h HerstattExposure) (HerstattExposure, error) {
	return scanHerstatt(t.q.QueryRow(ctx, `
		INSERT INTO herstatt_exposures
		    (counterparty_id, currency, paid_amount, receivable_amount,
		     window_open_at, duration_cap_minutes)
		VALUES ($1,$2,$3::numeric,$4::numeric,$5,$6)
		RETURNING `+herstattCols,
		h.CounterpartyID, h.Currency, h.Paid.String(), h.Receivable.String(),
		h.WindowOpenedAt, h.CapMinutes))
}

func (t pgxOpsTx) SettleHerstatt(ctx context.Context, id int64, at time.Time) error {
	tag, err := t.q.Exec(ctx, `
		UPDATE herstatt_exposures SET settled_at=$2
		 WHERE id=$1 AND settled_at IS NULL`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("herstatt exposure %d not open", id))
	}
	return nil
}

func (t pgxOpsTx) MarkHerstattBreached(ctx context.Context, id int64) error {
	_, err := t.q.Exec(ctx,
		`UPDATE herstatt_exposures SET breached=true WHERE id=$1`, id)
	return err
}

func scanLPEvent(row interface{ Scan(...any) error }) (LPDefaultEvent, error) {
	var e LPDefaultEvent
	var detail []byte
	err := row.Scan(&e.ID, &e.LPID, &e.Stage, &detail, &e.At)
	if err != nil {
		return e, err
	}
	if len(detail) > 0 {
		_ = json.Unmarshal(detail, &e.Detail)
	}
	return e, nil
}

func (t pgxOpsTx) LPDefaultLatestTx(ctx context.Context, lpID int64) (LPDefaultEvent, bool, error) {
	e, err := scanLPEvent(t.q.QueryRow(ctx, `
		SELECT id, lp_id, stage, COALESCE(detail::text,'{}'), created_at
		  FROM lp_default_events WHERE lp_id=$1 ORDER BY id DESC LIMIT 1`, lpID))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return LPDefaultEvent{}, false, nil
	}
	return e, err == nil, err
}

func (t pgxOpsTx) InsertLPDefault(ctx context.Context, e LPDefaultEvent) (LPDefaultEvent, error) {
	d, err := jsonbArg(e.Detail)
	if err != nil {
		return e, err
	}
	err = t.q.QueryRow(ctx, `
		INSERT INTO lp_default_events (lp_id, stage, detail, created_at)
		VALUES ($1,$2,$3::jsonb,$4) RETURNING id`, e.LPID, e.Stage, d, e.At).Scan(&e.ID)
	return e, err
}
