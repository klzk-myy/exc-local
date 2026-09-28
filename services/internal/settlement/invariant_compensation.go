// invariant_compensation.go — Balance invariants & settlement error
// compensation (Phase-03 Task 3.3.18; spec §5.40, §13.11, §17.12,
// §24 #301).
//
// Three layers:
//
//  1. Zero-sum invariant — enforced per transaction before commit by the
//     landed ledger contract (journal.Validate + in-tx stored-line
//     re-aggregation + deferred gl_journal_zero_sum_chk trigger). The
//     balance service rides it unchanged: an imbalance aborts the tx and
//     surfaces LEDGER_IMBALANCE_ABORT; commitWithRetry additionally pages
//     ops at P0 (see balance_service.go).
//
//  2. Serialization retry — SQLSTATE 40001/40P01 get the §5.40 schedule
//     (5/15/45ms + decorrelated jitter, max 3 attempts) then
//     TRANSACTION_CONFLICT_RETRY_EXHAUSTED (HTTP 503). Implemented in
//     commitWithRetry, sharing isRetryableConflict/jitter with the
//     ledger service.
//
//  3. Settlement compensation (this file) — when a banking/settlement
//     rail rejects a confirmed instruction (ISO 20022 / SWIFT returns:
//     AC01, AM04, RR04 … — spec §17.12.1), CompensateRailRejection posts
//     the reversing journal that re-credits the client in full and books
//     the rail-imposed return fee as house expense (contra-entry), then
//     raises an ops alert. Everything rides LedgerService.Post → the
//     posting is idempotent on settlement-comp:{instruction_id}, locked,
//     SERIALIZABLE and zero-sum checked like every other mutation.
package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Scaffold codes — register Phase-05 Task 5.3.21 (spec §23).
const (
	// CodeCompensationInvalid — HTTP 400, L2: malformed rejection payload
	// (non-positive principal, fee > principal, missing account/currency).
	CodeCompensationInvalid = "SETTLEMENT_COMPENSATION_INVALID"
	// CodeOpsAlertDispatchFailed — HTTP 500: compensation committed but
	// the ops alert could not be raised (post-commit; funds are final —
	// the error demands resync, not a rollback).
	CodeOpsAlertDispatchFailed = "OPS_ALERT_DISPATCH_FAILED"
)

// Ops alert severities (spec §2.7 paging tiers — P0 pages, P1 tickets).
const (
	SeverityP0 = "P0"
	SeverityP1 = "P1"
)

// OpsAlertSubject is the JetStream subject for settlement ops alerts.
const OpsAlertSubject = "ops.alerts.settlement"

// OpsAlert is a structured operations alert dispatched after (or about)
// settlement compensation events.
type OpsAlert struct {
	Severity string            `json:"severity"` // P0 | P1
	Code     string            `json:"code"`
	Summary  string            `json:"summary"`
	Err      string            `json:"error,omitempty"`
	Details  map[string]string `json:"details,omitempty"`
}

// OpsAlerter is the ops-paging seam — production binds a NATS JetStream
// publisher; tests capture alerts in-memory.
type OpsAlerter interface {
	Raise(ctx context.Context, a OpsAlert) error
}

// PublisherAlerter raises alerts by JSON-publishing to OpsAlertSubject on
// the shared Publisher (NATS JetStream) seam.
type PublisherAlerter struct {
	Pub     Publisher
	Subject string // empty → OpsAlertSubject
}

// Raise publishes the alert; a nil/failed publisher surfaces the error —
// alerting is a contractual MUST, not best-effort.
func (a PublisherAlerter) Raise(ctx context.Context, al OpsAlert) error {
	if a.Pub == nil {
		return excerrors.New(CodeOpsAlertDispatchFailed, "ops alerter: nil publisher")
	}
	subj := a.Subject
	if subj == "" {
		subj = OpsAlertSubject
	}
	payload, err := json.Marshal(al)
	if err != nil {
		return excerrors.Wrap(CodeOpsAlertDispatchFailed, "ops alerter: marshal", err)
	}
	if err := a.Pub.Publish(ctx, subj, payload); err != nil {
		return excerrors.Wrap(CodeOpsAlertDispatchFailed,
			fmt.Sprintf("ops alerter: publish %s", subj), err)
	}
	return nil
}

// alertOps fires an alert if an alerter is wired; alert delivery failure
// is reported by callers that need it — this helper is best-effort so a
// pager outage never masks the underlying coded error.
func (s *BalanceService) alertOps(ctx context.Context, severity, code, summary string, cause error, rt ResolvedTrade) {
	if s.alerter == nil {
		return
	}
	a := OpsAlert{Severity: severity, Code: code, Summary: summary}
	if cause != nil {
		a.Err = cause.Error()
	}
	if rt.Fill.TradeID != 0 {
		a.Details = map[string]string{
			"trade_id": fmt.Sprint(rt.Fill.TradeID),
			"pair":     rt.BaseCurrency + "/" + rt.QuoteCurrency,
		}
	}
	// Bounded fresh context — the caller's ctx may be exhausted.
	actx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.alerter.Raise(actx, a)
}

// ---------------------------------------------------------------------------
// Settlement compensation (Task 3.3.18 item 3, spec §17.12)
// ---------------------------------------------------------------------------

// RailRejection is a rejected confirmed settlement instruction: the rail
// returned the principal minus its return fee, which the house absorbs
// (the client is re-credited in full — the wire fee is the venue's cost
// of a failed rail, booked as a contra expense).
type RailRejection struct {
	InstructionID int64           // settlement_instructions.id — reference + idempotency anchor
	AccountID     int64           // client to re-credit
	Currency      string          // ISO 4217
	Principal     decimal.Decimal // full amount to re-credit the client
	WireFee       decimal.Decimal // rail-imposed return fee (house expense)
	Rail          string          // "SWIFT" | "SEPA" | "FEDNOW" | …
	ReturnCode    string          // ISO 20022 / SWIFT return code (AC01, AM04, RR04 …)
}

// CompensationResult reports the committed compensation journal.
type CompensationResult struct {
	PostResult ledger.PostResult
	Alerted    bool // ops alert dispatched
}

// CompensateRailRejection posts the compensating journal for a rejected
// settlement instruction and alerts operations:
//
//	DR 1020_NOSTRO_CLEARING_{ccy}   principal − wireFee  (funds back at nostro)
//	DR 5300_BANK_RAIL_FEE_EXPENSE_{ccy}  wireFee          (house absorbs rail fee)
//	CR 2010_CUSTOMER_LIABILITY_{ccy}     principal        (client re-credited)
//
// Wallet effect: client available += principal. Idempotent on
// settlement-comp:{instruction_id} — a replay resolves to the original
// journal (zero double-credit). On posting failure the alert is still
// raised (ops must see an uncompensated rejection louder than a
// compensated one) before the error is returned.
func (s *BalanceService) CompensateRailRejection(ctx context.Context, r RailRejection) (*CompensationResult, error) {
	j, err := buildCompensationJournal(r, s.postedBy)
	if err != nil {
		return nil, err
	}
	if s.poster == nil {
		return nil, excerrors.New(ledger.CodeLedgerLockUnavailable,
			"compensation requires the ledger Post path — service not wired with a managed poster")
	}
	res, err := s.poster.Post(ctx, j)
	if err != nil {
		s.raiseCompensationAlert(SeverityP0, r, fmt.Errorf("compensation journal failed: %w", err))
		return nil, err
	}
	if err := s.raiseCompensationAlert(SeverityP1, r, nil); err != nil {
		return &CompensationResult{PostResult: res}, err
	}
	return &CompensationResult{PostResult: res, Alerted: s.alerter != nil}, nil
}

// raiseCompensationAlert delivers the ops alert for a rejection — through
// the wired alerter. cause non-nil → the compensation itself failed.
func (s *BalanceService) raiseCompensationAlert(severity string, r RailRejection, cause error) error {
	if s.alerter == nil {
		if cause != nil {
			return excerrors.New(CodeOpsAlertDispatchFailed,
				"compensation failed and no ops alerter is wired")
		}
		return nil
	}
	summary := fmt.Sprintf("settlement instruction %d rejected by %s (%s) — client re-credited %s %s",
		r.InstructionID, r.Rail, r.ReturnCode, r.Principal, r.Currency)
	if cause != nil {
		summary = fmt.Sprintf("settlement instruction %d rejected by %s (%s) — COMPENSATION FAILED",
			r.InstructionID, r.Rail, r.ReturnCode)
	}
	a := OpsAlert{
		Severity: severity, Code: "SETTLEMENT_RAIL_REJECTION", Summary: summary,
		Details: map[string]string{
			"instruction_id": fmt.Sprint(r.InstructionID),
			"account_id":     fmt.Sprint(r.AccountID),
			"rail":           r.Rail,
			"return_code":    r.ReturnCode,
			"currency":       r.Currency,
			"principal":      r.Principal.String(),
			"wire_fee":       r.WireFee.String(),
		},
	}
	if cause != nil {
		a.Err = cause.Error()
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.alerter.Raise(actx, a)
}

// buildCompensationJournal is the pure constructor (unit-tested).
func buildCompensationJournal(r RailRejection, postedBy string) (ledger.Journal, error) {
	if r.InstructionID <= 0 || r.AccountID <= 0 || len(r.Currency) != 3 ||
		!r.Principal.IsPositive() || r.WireFee.IsNegative() {
		return ledger.Journal{}, excerrors.New(CodeCompensationInvalid,
			"rejection needs instruction/account/currency and principal > 0, wireFee >= 0")
	}
	if r.WireFee.GreaterThan(r.Principal) {
		return ledger.Journal{}, excerrors.New(CodeCompensationInvalid, fmt.Sprintf(
			"wire fee %s exceeds principal %s — cannot contra", r.WireFee, r.Principal))
	}
	returned := r.Principal.Sub(r.WireFee)
	lines := []ledger.Line{}
	if returned.IsPositive() {
		lines = append(lines,
			ledger.DebitLine(ledger.NostroClearing(r.Currency), r.Currency, returned,
				fmt.Sprintf("%s return of rejected instruction %d", r.Rail, r.InstructionID)))
	}
	if r.WireFee.IsPositive() {
		lines = append(lines,
			ledger.DebitLine(ledger.BankRailFeeExpense(r.Currency), r.Currency, r.WireFee,
				fmt.Sprintf("%s return fee %s contra-entry", r.Rail, r.ReturnCode)))
	}
	lines = append(lines,
		ledger.CreditLine(ledger.CustomerLiability(r.Currency), r.Currency, r.Principal,
			fmt.Sprintf("re-credit client %d after rail rejection", r.AccountID)))

	j := ledger.Journal{
		EntryType:      ledger.EntrySettlement,
		ReferenceID:    r.InstructionID,
		Description:    fmt.Sprintf("rail rejection compensation instr %d (%s %s)", r.InstructionID, r.Rail, r.ReturnCode),
		PostedBy:       postedBy,
		IdempotencyKey: fmt.Sprintf("settlement-comp:%d", r.InstructionID),
		Lines:          lines,
		Effects: []ledger.AccountEffect{{
			AccountID: r.AccountID, Currency: r.Currency, AvailableDelta: r.Principal,
		}},
	}
	return j, j.Validate()
}
