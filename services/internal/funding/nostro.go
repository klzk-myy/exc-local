// Nostro-aware withdrawal dispatch — Phase-11 Task 11.3.6, spec §5.18
// (nostro_accounts, migration 018 — the balance column already carries
// the per-currency nostro balance; no separate nostro_balances table).
//
// Release path (invoked when a withdrawal reaches CONFIRMED — post
// confirmation for AUTO/STANDARD tiers, post admin-approve for the
// PENDING_REVIEW tier — and retried by the sweep):
//
//  1. Destination hold not lapsed (hold_until > now)   → queue row
//     (DESTINATION_HOLD), withdrawal stays CONFIRMED.
//  2. Aggregate ACTIVE nostro balance for the currency < amount → the
//     withdrawal is QUEUED, never rejected: a withdrawal_dispatch_queue
//     row + a durable funding_ops_alerts row (NOSTRO_INSUFFICIENT_FUNDS,
//     P1) + a best-effort PENDING_APPROVAL replenishment request
//     (auto-dual-control path) + an ops.alerts page.
//  3. Sufficient → the rail instruction is persisted (RailService when
//     wired), the funding row flips COMPLETED and the hold is consumed:
//     DR 2160_CLEARING_TRANSIT / CR 1010_NOSTRO with wallet locked −amt
//     (journal idempotent on withdrawal-dispatch:{id}).
//
// Replenishment (task steps 2–3): reserve→operating nostro movements
// are dual-controlled — requested_by ≠ approved_by; approval executes
// the two-sided balance movement inside one SERIALIZABLE tx and a
// source overdraft aborts fail-closed.
package funding

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// CodeNostroInsufficientFunds is stamped on the durable ops-alert row
// when dispatch queues for nostro headroom (spec §23 / §27.1 code —
// an internal alert code, never an HTTP rejection: queue-not-reject).
const CodeNostroInsufficientFunds = "NOSTRO_INSUFFICIENT_FUNDS"

// NostroStore is the persistence seam (PgStore satisfies it).
type NostroStore interface {
	BeginTx(ctx context.Context) (pgx.Tx, error)
	WithdrawalForDispatch(ctx context.Context, tx pgx.Tx, id int64) (*DispatchableWithdrawal, error)
	ConfirmedForDispatch(ctx context.Context, limit int) ([]DispatchableWithdrawal, error)
	SetWithdrawalCompleted(ctx context.Context, tx pgx.Tx, id int64, at time.Time) error
	InsertDispatchQueue(ctx context.Context, tx pgx.Tx, withdrawalID int64, reason string) (*DispatchQueueRow, error)
	DispatchQueueForUpdate(ctx context.Context, tx pgx.Tx, withdrawalID int64) (*DispatchQueueRow, error)
	SetDispatchQueueStatus(ctx context.Context, tx pgx.Tx, id int64, status string, lastErr *string, dispatchedAt *time.Time) error
	InsertFundingOpsAlert(ctx context.Context, tx pgx.Tx, a FundingOpsAlertRow) (int64, error)
	ListFundingOpsAlerts(ctx context.Context, limit int) ([]FundingOpsAlertRow, error)
	NostroBalances(ctx context.Context, currency string) ([]NostroBalanceRow, error)
	NostroForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*NostroBalanceRow, error)
	AdjustNostroBalance(ctx context.Context, tx pgx.Tx, id int64, delta decimal.Decimal) (*NostroBalanceRow, error)
	BeneficiaryByDestination(ctx context.Context, accountID int64, destination string) (*BeneficiaryRow, error)
	InsertReplenishmentRequest(ctx context.Context, tx pgx.Tx, r ReplenishmentRow) (*ReplenishmentRow, error)
	ReplenishmentForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*ReplenishmentRow, error)
	SetReplenishmentStatus(ctx context.Context, tx pgx.Tx, id int64, status string, approvedBy int64, note *string, at time.Time) error
	ListReplenishments(ctx context.Context, limit int) ([]ReplenishmentRow, error)
}

// RailDispatcher is the Task 11.3.1 rail-instruction seam
// (*RailService.Dispatch). nil → the journal/status transition still
// lands; only the rail_payments instruction is deferred.
type RailDispatcher interface {
	Dispatch(ctx context.Context, tx pgx.Tx, p OutboundPayment) (*RailPaymentRow, error)
}

// DispatchService releases CONFIRMED withdrawals against nostro
// headroom and owns the replenishment request lifecycle.
type DispatchService struct {
	store    NostroStore
	poster   JournalPoster
	rails    RailDispatcher // optional
	travel   TravelRuleGate // optional — Phase-21 Task 21.3.2 FATF R.16 gate
	alerter  OpsAlerter     // optional page channel
	notifier Notifier       // optional Phase-12 client-notification seam
	clock    func() time.Time
	logf     func(format string, args ...any)
}

// NewDispatchService wires the service; store + poster are mandatory.
func NewDispatchService(store NostroStore, poster JournalPoster) (*DispatchService, error) {
	if store == nil || poster == nil {
		return nil, fmt.Errorf("funding: dispatch service requires store and poster")
	}
	return &DispatchService{store: store, poster: poster, clock: time.Now}, nil
}

// WithRails wires the rail-instruction dispatcher.
func (s *DispatchService) WithRails(r RailDispatcher) *DispatchService {
	s.rails = r
	return s
}

// WithTravelRule binds the Phase-21 Task 21.3.2 FATF R.16 outbound gate
// (compliance.TravelRuleService). nil → no travel-rule evaluation —
// production wiring must bind it for regulated rails.
func (s *DispatchService) WithTravelRule(g TravelRuleGate) *DispatchService {
	s.travel = g
	return s
}

// WithAlerter wires the ops paging seam.
func (s *DispatchService) WithAlerter(a OpsAlerter) *DispatchService {
	s.alerter = a
	return s
}

// WithNotifier wires the Phase-12 client-notification seam — emits
// withdrawal_completed after the dispatch journal commits (best-effort).
func (s *DispatchService) WithNotifier(n Notifier) *DispatchService {
	s.notifier = n
	return s
}

// WithClock overrides the clock (tests).
func (s *DispatchService) WithClock(c func() time.Time) *DispatchService {
	s.clock = c
	return s
}

// WithLogger wires a log sink.
func (s *DispatchService) WithLogger(f func(format string, args ...any)) *DispatchService {
	s.logf = f
	return s
}

// ---------------------------------------------------------------------------
// Release — the dispatch decision for one CONFIRMED withdrawal.
// ---------------------------------------------------------------------------

// ReleaseResult reports the dispatch outcome to the caller.
type ReleaseResult struct {
	WithdrawalID int64           `json:"withdrawal_id"`
	Disposition  string          `json:"disposition"` // DISPATCHED | QUEUED | HELD | SKIPPED
	Reason       string          `json:"reason,omitempty"`
	Queued       bool            `json:"queued,omitempty"`
	NostroTotal  string          `json:"nostro_total,omitempty"`
	Shortfall    string          `json:"shortfall,omitempty"`
	RailPayment  *RailPaymentRow `json:"rail_payment,omitempty"`
}

// Release evaluates one withdrawal for dispatch. Only CONFIRMED rows
// are actionable — anything else returns SKIPPED (idempotent).
func (s *DispatchService) Release(ctx context.Context, withdrawalID int64) (*ReleaseResult, error) {
	res := &ReleaseResult{WithdrawalID: withdrawalID}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "dispatch tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := s.store.WithdrawalForDispatch(ctx, tx, withdrawalID)
	if err != nil {
		return nil, err
	}
	if w.Status != FundingConfirmed {
		res.Disposition = "SKIPPED"
		res.Reason = "status " + w.Status
		_ = tx.Rollback(ctx)
		return res, nil
	}
	now := s.clock().UTC()
	if w.HoldUntil != nil && w.HoldUntil.After(now) {
		if _, err := s.store.InsertDispatchQueue(ctx, tx, w.ID,
			QueueReasonDestinationHold); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "hold queue commit", err)
		}
		res.Disposition = "HELD"
		res.Reason = QueueReasonDestinationHold
		res.Queued = true
		return res, nil
	}

	balances, err := s.store.NostroBalances(ctx, w.Currency)
	if err != nil {
		return nil, err
	}
	total := decimal.Zero
	var debtor *NostroBalanceRow
	for i := range balances {
		b := balances[i]
		if b.Status != "ACTIVE" {
			continue
		}
		if debtor == nil {
			debtor = &balances[i]
		}
		total = total.Add(b.Balance)
	}
	res.NostroTotal = total.String()

	if total.LessThan(w.Amount) {
		// Queue-not-reject: durable queue + alert rows commit atomically,
		// then a best-effort auto-replenishment request + page.
		shortfall := w.Amount.Sub(total)
		res.Disposition = "QUEUED"
		res.Reason = QueueReasonNostroInsufficient
		res.Queued = true
		res.Shortfall = shortfall.String()
		if _, err := s.store.InsertDispatchQueue(ctx, tx, w.ID,
			QueueReasonNostroInsufficient); err != nil {
			return nil, err
		}
		summary := fmt.Sprintf(
			"withdrawal %d queued — nostro %s cover %s < required %s (shortfall %s)",
			w.ID, w.Currency, total.String(), w.Amount.String(), shortfall.String())
		detail := fmt.Sprintf(`{"withdrawal_id":%d,"currency":%q,"amount":%q,"nostro_total":%q}`,
			w.ID, w.Currency, w.Amount.String(), total.String())
		if _, err := s.store.InsertFundingOpsAlert(ctx, tx, FundingOpsAlertRow{
			Code:                 CodeNostroInsufficientFunds,
			Severity:             "P1",
			FundingTransactionID: &w.ID,
			AccountID:            &w.AccountID,
			Currency:             &w.Currency,
			Amount:               &w.Amount,
			Summary:              summary,
			Detail:               []byte(detail),
		}); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "queue commit", err)
		}
		s.autoReplenish(ctx, w.Currency, shortfall)
		s.raise(ctx, "P1", CodeNostroInsufficientFunds, summary, nil)
		return res, nil
	}
	return s.dispatch(ctx, tx, res, w, debtor, now)
}

// dispatch performs the sufficient-headroom leg: rail instruction +
// queue close + COMPLETED inside tx, wallet-consuming journal after
// commit (idempotent — a crashed post replays, never doubles).
func (s *DispatchService) dispatch(ctx context.Context, tx pgx.Tx,
	res *ReleaseResult, w *DispatchableWithdrawal, debtor *NostroBalanceRow,
	now time.Time) (*ReleaseResult, error) {
	var payment *RailPaymentRow
	if s.rails != nil && debtor != nil {
		p := s.outboundPayment(ctx, w, debtor)
		// Phase-21 Task 21.3.2 — FATF R.16 gate: the outbound wire for a
		// >= $1,000 transfer carries no envelope until originator and
		// beneficiary info is complete. Missing fields → the record row
		// lands inside this tx and the withdrawal queues HELD (never
		// dispatched); the gate enriching Originator* feeds MT103 50K.
		if s.travel != nil {
			ep, missing, terr := s.travel.EnforceOutbound(ctx, tx, w, p)
			if terr != nil {
				return nil, terr // rollback — withdrawal stays CONFIRMED
			}
			if len(missing) > 0 {
				if _, err := s.store.InsertDispatchQueue(ctx, tx, w.ID,
					QueueReasonTravelRuleMissing); err != nil {
					return nil, err
				}
				summary := fmt.Sprintf(
					"withdrawal %d held — FATF travel-rule fields missing (%s)",
					w.ID, strings.Join(missing, ","))
				detail := fmt.Sprintf(
					`{"withdrawal_id":%d,"account_id":%d,"missing":%q}`,
					w.ID, w.AccountID, strings.Join(missing, ","))
				if _, err := s.store.InsertFundingOpsAlert(ctx, tx, FundingOpsAlertRow{
					Code:                 QueueReasonTravelRuleMissing,
					Severity:             "P1",
					FundingTransactionID: &w.ID,
					AccountID:            &w.AccountID,
					Currency:             &w.Currency,
					Amount:               &w.Amount,
					Summary:              summary,
					Detail:               []byte(detail),
				}); err != nil {
					return nil, err
				}
				if err := tx.Commit(ctx); err != nil {
					return nil, wrapCode("INTERNAL_ERROR", "travel-rule hold commit", err)
				}
				s.raise(ctx, "P1", QueueReasonTravelRuleMissing, summary, nil)
				res.Disposition = "HELD"
				res.Reason = QueueReasonTravelRuleMissing
				res.Queued = true
				return res, nil
			}
			p = ep
		}
		rp, rerr := s.rails.Dispatch(ctx, tx, p)
		if rerr != nil {
			return nil, rerr
		}
		payment = rp
	}
	if q, qerr := s.store.DispatchQueueForUpdate(ctx, tx, w.ID); qerr != nil {
		return nil, qerr
	} else if q != nil {
		if err := s.store.SetDispatchQueueStatus(ctx, tx, q.ID,
			QueueDispatched, nil, &now); err != nil {
			return nil, err
		}
	}
	if err := s.store.SetWithdrawalCompleted(ctx, tx, w.ID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "dispatch commit", err)
	}

	jres, jerr := s.poster.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryWithdrawal,
		ReferenceID:    w.ID,
		Description:    fmt.Sprintf("withdrawal %d dispatched %s %s", w.ID, w.Amount, w.Currency),
		PostedBy:       "funding:dispatch",
		IdempotencyKey: fmt.Sprintf("withdrawal-dispatch:%d", w.ID),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.ClearingTransit(w.Currency), w.Currency, w.Amount,
				"payout dispatched — transit liability settled"),
			ledger.CreditLine(ledger.Nostro(w.Currency), w.Currency, w.Amount,
				"nostro account debited for client wire"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:   w.AccountID,
			Currency:    w.Currency,
			LockedDelta: w.Amount.Neg(), // hold consumed — funds left
		}},
	})
	if jerr != nil && !jres.Committed {
		s.log("funding: dispatch journal for withdrawal %d failed: %v", w.ID, jerr)
		s.raiseOpsRow(ctx, w, "P1", "DISPATCH_JOURNAL_FAILED",
			fmt.Sprintf("withdrawal %d dispatch journal failed — ledger needs ops replay", w.ID))
		return nil, wrapCode("INTERNAL_ERROR", "dispatch journal", jerr)
	}
	res.Disposition = "DISPATCHED"
	res.RailPayment = payment
	// Phase-12 Task 12.3.5: withdrawal_completed user notification —
	// post-commit, best-effort (notify swallows its own errors).
	s.notify(ctx, w.AccountID, "withdrawal_completed", map[string]any{
		"withdrawal_id": w.ID,
		"currency":      w.Currency,
		"amount":        w.Amount.String(),
	})
	return res, nil
}

// notify fires the optional client notification — post-commit,
// best-effort; the panic guard keeps a misbehaving emitter from
// crashing the funding path.
func (s *DispatchService) notify(ctx context.Context, accountID int64, event string, payload map[string]any) {
	if s.notifier == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.log("funding: notifier panic on %s: %v", event, r)
		}
	}()
	s.notifier.Notify(ctx, accountID, event, payload)
}

// outboundPayment builds the Task 11.3.1 rail instruction envelope for
// the withdrawal. Beneficiary details come from the verified registry
// row when resolvable (the whitelist path verified it at create); the
// free-form reference remains the fallback creditor account.
func (s *DispatchService) outboundPayment(ctx context.Context,
	w *DispatchableWithdrawal, debtor *NostroBalanceRow) OutboundPayment {
	p := OutboundPayment{
		FundingTxID:   w.ID,
		AccountID:     w.AccountID,
		Currency:      w.Currency,
		Amount:        w.Amount,
		DebtorName:    debtor.BankName,
		DebtorAccount: strVal(debtor.IBAN),
		EndToEndID:    fmt.Sprintf("wd-%d", w.ID),
		Charges:       "OUR",
	}
	if debtor.BankCode != nil {
		p.DebtorBIC = *debtor.BankCode
	}
	if w.BankMethod != nil {
		p.Rail = RailID(*w.BankMethod)
	}
	if w.ReferenceAccount != nil {
		p.CreditorIBAN = *w.ReferenceAccount
		p.RemittanceInfo = fmt.Sprintf("withdrawal %d", w.ID)
	}
	if ben, err := s.store.BeneficiaryByDestination(ctx, w.AccountID,
		strVal(w.ReferenceAccount)); err == nil && ben != nil {
		p.CreditorName = ben.BeneficiaryName
		if ben.IBAN != nil {
			p.CreditorIBAN = *ben.IBAN
		}
		if ben.SwiftBIC != nil {
			p.CreditorBIC = *ben.SwiftBIC
		}
		if p.Rail == "" {
			p.Rail = RailID(ben.Rail)
		}
	}
	return p
}

// autoReplenish opens a PENDING_APPROVAL replenishment request for the
// shortfall when a reserve→operating nostro pair exists for the
// currency (≥2 ACTIVE rows: richest = reserve source, poorest =
// operating target). Without a pair the durable alert stands alone —
// the request row is best-effort, never a mask for the queued state.
func (s *DispatchService) autoReplenish(ctx context.Context, currency string,
	shortfall decimal.Decimal) {
	balances, err := s.store.NostroBalances(ctx, currency)
	if err != nil {
		s.log("funding: auto-replenish lookup %s: %v", currency, err)
		return
	}
	var act []NostroBalanceRow
	for _, b := range balances {
		if b.Status == "ACTIVE" {
			act = append(act, b)
		}
	}
	if len(act) < 2 {
		return
	}
	sort.Slice(act, func(i, j int) bool { return act[i].Balance.LessThan(act[j].Balance) })
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return
	}
	if _, err := s.store.InsertReplenishmentRequest(ctx, tx, ReplenishmentRow{
		Currency:       currency,
		Amount:         shortfall,
		SourceNostroID: act[len(act)-1].ID, // reserve (richest)
		TargetNostroID: act[0].ID,          // operating (most drained)
	}); err != nil {
		s.log("funding: auto-replenish insert %s: %v", currency, err)
	}
	_ = tx.Commit(ctx)
}

// raise pages the ops channel (best-effort — the durable alert row is
// the record of truth).
func (s *DispatchService) raise(ctx context.Context, sev, code, summary string, cause error) {
	if s.alerter == nil {
		return
	}
	a := OpsAlert{Severity: sev, Code: code, Summary: summary}
	if cause != nil {
		a.Err = cause.Error()
	}
	_ = s.alerter.Raise(ctx, a)
}

// raiseOpsRow persists a durable alert in its own tx (post-commit paths
// where the business tx already closed).
func (s *DispatchService) raiseOpsRow(ctx context.Context, w *DispatchableWithdrawal,
	sev, code, summary string) {
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return
	}
	_, _ = s.store.InsertFundingOpsAlert(ctx, tx, FundingOpsAlertRow{
		Code: code, Severity: sev, FundingTransactionID: &w.ID,
		AccountID: &w.AccountID, Currency: &w.Currency, Amount: &w.Amount,
		Summary: summary,
	})
	_ = tx.Commit(ctx)
	s.raise(ctx, sev, code, summary, nil)
}

// ---------------------------------------------------------------------------
// Sweep — retries due CONFIRMED withdrawals (queued, hold-lapsed or
// dispatch-raced). Wired on a ticker at the composition root.
// ---------------------------------------------------------------------------

// SweepDue dispatches up to limit CONFIRMED withdrawals whose holds
// have lapsed. Per-item failures are logged and skipped — the next tick
// retries; a withdrawal is never dropped.
func (s *DispatchService) SweepDue(ctx context.Context, limit int) (int, error) {
	rows, err := s.store.ConfirmedForDispatch(ctx, limit)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, w := range rows {
		if _, err := s.Release(ctx, w.ID); err != nil {
			s.log("funding: dispatch sweep withdrawal %d: %v", w.ID, err)
			continue
		}
		done++
	}
	return done, nil
}

// ---------------------------------------------------------------------------
// Coverage — nostro balance tracking + low-balance alert (task step 1/2).
// ---------------------------------------------------------------------------

// NostroCoverageRow reports one currency's headroom vs obligations.
type NostroCoverageRow struct {
	Currency     string             `json:"currency"`
	NostroTotal  string             `json:"nostro_total"`
	ConfirmedDue string             `json:"confirmed_due"` // sum of CONFIRMED withdrawals awaiting dispatch
	Queued       int64              `json:"queued"`
	Deficit      bool               `json:"deficit"`
	Accounts     []NostroBalanceRow `json:"accounts"`
}

// Coverage lists nostro balances per currency with the confirmed-but-
// undispatched withdrawal load — the "balances tracked per currency"
// view (GET /api/v1/admin/funding/nostro).
func (s *DispatchService) Coverage(ctx context.Context) ([]NostroCoverageRow, error) {
	balances, err := s.store.NostroBalances(ctx, "")
	if err != nil {
		return nil, err
	}
	due, err := s.store.ConfirmedForDispatch(ctx, 10000)
	if err != nil {
		return nil, err
	}
	byCCY := map[string]*NostroCoverageRow{}
	order := []string{}
	for _, b := range balances {
		row, ok := byCCY[b.Currency]
		if !ok {
			row = &NostroCoverageRow{Currency: b.Currency, NostroTotal: "0", ConfirmedDue: "0"}
			byCCY[b.Currency] = row
			order = append(order, b.Currency)
		}
		row.Accounts = append(row.Accounts, b)
		if b.Status == "ACTIVE" {
			t, _ := decimal.NewFromString(row.NostroTotal)
			row.NostroTotal = t.Add(b.Balance).String()
		}
	}
	for _, w := range due {
		row, ok := byCCY[w.Currency]
		if !ok {
			row = &NostroCoverageRow{Currency: w.Currency, NostroTotal: "0", ConfirmedDue: "0"}
			byCCY[w.Currency] = row
			order = append(order, w.Currency)
		}
		t, _ := decimal.NewFromString(row.ConfirmedDue)
		row.ConfirmedDue = t.Add(w.Amount).String()
		row.Queued++
	}
	sort.Strings(order)
	out := make([]NostroCoverageRow, 0, len(order))
	for _, c := range order {
		row := byCCY[c]
		total, _ := decimal.NewFromString(row.NostroTotal)
		d, _ := decimal.NewFromString(row.ConfirmedDue)
		row.Deficit = d.GreaterThan(total)
		out = append(out, *row)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Replenishment — reserve→operating nostro, dual control (task steps 2–3).
// ---------------------------------------------------------------------------

// RequestReplenishment opens a PENDING_APPROVAL request (Finance Ops).
func (s *DispatchService) RequestReplenishment(ctx context.Context, adminID int64,
	currency string, amount decimal.Decimal, sourceID, targetID int64) (*ReplenishmentRow, error) {
	if sourceID == targetID {
		return nil, errCode("INVALID_REQUEST", "source and target nostro accounts must differ")
	}
	if !amount.IsPositive() {
		return nil, errCode("INVALID_REQUEST", "amount must be positive")
	}
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "replenishment tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, id := range sortedIDs(sourceID, targetID) {
		n, nerr := s.store.NostroForUpdate(ctx, tx, id)
		if nerr != nil {
			return nil, nerr
		}
		if n.Currency != currency {
			return nil, errf("INVALID_REQUEST",
				"nostro account %d is %s-denominated, not %s", id, n.Currency, currency)
		}
		if n.Status != "ACTIVE" {
			return nil, errf("INVALID_LIFECYCLE_TRANSITION",
				"nostro account %d is %s — replenishment requires ACTIVE", id, n.Status)
		}
	}
	r, err := s.store.InsertReplenishmentRequest(ctx, tx, ReplenishmentRow{
		Currency:       currency,
		Amount:         amount,
		SourceNostroID: sourceID,
		TargetNostroID: targetID,
		RequestedBy:    &adminID,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "replenishment commit", err)
	}
	return r, nil
}

// DecideReplenishment applies the four-eyes decision. Approve executes
// the reserve→operating movement inside the same SERIALIZABLE tx —
// approver must differ from requester (DUAL_CONTROL_VIOLATION); a
// source overdraft aborts INSUFFICIENT_BALANCE (fail closed).
func (s *DispatchService) DecideReplenishment(ctx context.Context, adminID,
	requestID int64, approve bool, note string) (*ReplenishmentRow, error) {
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "replenishment tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	r, err := s.store.ReplenishmentForUpdate(ctx, tx, requestID)
	if err != nil {
		return nil, err
	}
	if r.Status != ReplenPending {
		return nil, errf("INVALID_LIFECYCLE_TRANSITION",
			"replenishment %d is %s — only PENDING_APPROVAL is decidable", r.ID, r.Status)
	}
	// Dual control: a human approver must differ from the requester; a
	// dispatcher-auto request (requested_by NULL) takes any single admin.
	if r.RequestedBy != nil && *r.RequestedBy == adminID {
		return nil, errCode("DUAL_CONTROL_VIOLATION",
			"approver must differ from the requester")
	}
	now := s.clock().UTC()
	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	if !approve {
		if err := s.store.SetReplenishmentStatus(ctx, tx, r.ID, ReplenRejected,
			adminID, notePtr, now); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, wrapCode("INTERNAL_ERROR", "reject commit", err)
		}
		r.Status = ReplenRejected
		return r, nil
	}
	// Execute the two-sided movement — both rows locked in id order
	// (deadlock-safe); source overdraft refuses INSUFFICIENT_BALANCE.
	var src, tgt *NostroBalanceRow
	for _, id := range sortedIDs(r.SourceNostroID, r.TargetNostroID) {
		n, nerr := s.store.NostroForUpdate(ctx, tx, id)
		if nerr != nil {
			return nil, nerr
		}
		if n.Status != "ACTIVE" {
			return nil, errf("INVALID_LIFECYCLE_TRANSITION",
				"nostro account %d is %s — replenishment requires ACTIVE", id, n.Status)
		}
		switch id {
		case r.SourceNostroID:
			src = n
		default:
			tgt = n
		}
	}
	if src.Currency != r.Currency || tgt.Currency != r.Currency {
		return nil, errf("INVALID_REQUEST", "nostro currency mismatch vs request %s", r.Currency)
	}
	if _, err := s.store.AdjustNostroBalance(ctx, tx, src.ID, r.Amount.Neg()); err != nil {
		return nil, err
	}
	if _, err := s.store.AdjustNostroBalance(ctx, tx, tgt.ID, r.Amount); err != nil {
		return nil, err
	}
	if err := s.store.SetReplenishmentStatus(ctx, tx, r.ID, ReplenExecuted,
		adminID, notePtr, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "execute commit", err)
	}
	r.Status = ReplenExecuted
	r.ApprovedBy = &adminID
	return r, nil
}

// Replenishments lists recent requests (admin read).
func (s *DispatchService) Replenishments(ctx context.Context, limit int) ([]ReplenishmentRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.store.ListReplenishments(ctx, limit)
}

// OpsAlerts lists the durable funding alert trail (admin read).
func (s *DispatchService) OpsAlerts(ctx context.Context, limit int) ([]FundingOpsAlertRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.store.ListFundingOpsAlerts(ctx, limit)
}

func sortedIDs(a, b int64) []int64 {
	if a < b {
		return []int64{a, b}
	}
	return []int64{b, a}
}

func (s *DispatchService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}
