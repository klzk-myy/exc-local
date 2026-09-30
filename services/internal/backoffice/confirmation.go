// Task 24.3.3 — Settlement Confirmation (spec §17.1 step 2 tail;
// §24 #9–#12).
//
// Correspondent banks acknowledge settlement legs with SWIFT MT900
// (confirmation of debit) and MT910 (confirmation of credit). The
// confirmation carries our :20: dispatch reference in :21: — that is
// the join key back to settlement_instructions.swift_message_id.
//
// ProcessConfirmation chains four steps atomically-in-spirit (the
// journal row lands first so a mid-chain crash leaves evidence, never
// a silent confirmation):
//
//  1. Record the inbound message in swift_messages (Task 24.3.4's
//     immutable journal — an unrecorded confirmation never mutates
//     settlement state);
//  2. Resolve the leg by the :21: related reference;
//  3. ConfirmSettlement flips PENDING→SETTLED and writes the
//     nostro_movements intent row (Phase-03 seam, idempotent);
//  4. The Phase-24 poster applies the nostro_accounts.balance mutation
//     (Task 24.3.1 — DEBIT subtracts, CREDIT adds).
//
// Timeout (§24 #12): SweepOverdue flags dispatched-but-unconfirmed legs
// once two business days have elapsed — business days are computed
// holiday-aware against the leg currency's banking calendar when one is
// wired (BusinessCalendar; settlement.HolidayCalendar satisfies it),
// weekends-only otherwise — and lands a deduplicated P2 alert on the
// durable funding_ops_alerts trail.
package backoffice

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// CodeConfirmationOverdue is the durable P2 alert code for a dispatched
// settlement leg still unconfirmed after two business days (§24 #12).
const CodeConfirmationOverdue = "SETTLEMENT_CONFIRMATION_OVERDUE"

// Confirmation message types.
const (
	MsgMT900 = "MT900" // confirmation of debit
	MsgMT910 = "MT910" // confirmation of credit
)

// SwiftConfirmation is one inbound MT900/MT910 normalized for
// processing.
type SwiftConfirmation struct {
	MessageType string          // MT900 | MT910
	Reference   string          // :20: sender's reference
	RelatedRef  string          // :21: our dispatch reference (swift_message_id)
	Currency    string          // :32A: currency
	Amount      decimal.Decimal // :32A: amount
	ValueDate   time.Time       // :32A: value date
	NostroIBAN  string          // :25: account identification (optional)
	ReceivedAt  time.Time
	RawPayload  string
}

// InstructionRow is the settlement_instructions view the confirmation
// path needs.
type InstructionRow struct {
	ID              int64           `json:"id"`
	TradeID         int64           `json:"trade_id"`
	AccountID       int64           `json:"account_id"`
	Currency        string          `json:"currency"`
	Amount          decimal.Decimal `json:"amount"`
	Direction       string          `json:"direction"`
	SettlementDate  time.Time       `json:"settlement_date"`
	NostroAccountID *int64          `json:"nostro_account_id,omitempty"`
	Status          string          `json:"status"`
	SwiftMessageID  *string         `json:"swift_message_id,omitempty"`
	DispatchedAt    *time.Time      `json:"dispatched_at,omitempty"`
	SettledAt       *time.Time      `json:"settled_at,omitempty"`
}

// ConfirmationStore is the persistence seam for the confirmation path.
type ConfirmationStore interface {
	// InstructionBySwiftRef resolves the leg whose swift_message_id
	// equals ref (:21: related reference on MT900/910); nil when absent.
	InstructionBySwiftRef(ctx context.Context, ref string) (*InstructionRow, error)
	// StaleDispatched returns PENDING legs dispatched at/before
	// dispatchedBefore — the timeout-scan candidate set.
	StaleDispatched(ctx context.Context, dispatchedBefore time.Time, limit int) ([]InstructionRow, error)
	// HasOpenAlert reports whether a dedup_keyed alert row is already
	// OPEN — the sweep's idempotency guard.
	HasOpenAlert(ctx context.Context, code, dedupKey string) (bool, error)
	// InTx runs fn inside a SERIALIZABLE transaction (alert inserts ride
	// the same NostroTx surface as the poster).
	InTx(ctx context.Context, fn func(ctx context.Context, tx NostroTx) error) error
}

// ConfirmedInstruction is the minimal confirmation-path projection of
// the settled leg — the settlement package's full instruction type is a
// sibling seam; this surface takes only what it uses. The production
// adapter wraps *settlement.SettlementService.ConfirmSettlement.
type ConfirmedInstruction struct {
	Status string // SETTLED | ALREADY_SETTLED (post-ConfirmSettlement state)
}

// SettlementConfirmer is the Phase-03 settlement seam — an adapter over
// *settlement.SettlementService.ConfirmSettlement satisfies it.
type SettlementConfirmer interface {
	ConfirmSettlement(ctx context.Context, instructionID int64, confirmationRef string) (*ConfirmedInstruction, error)
}

// MovementPoster applies nostro intent rows to nostro_accounts.balance —
// *NostroService satisfies it (the confirmation path posts by
// instruction id, the reconciler auto-resolution posts by movement id).
type MovementPoster interface {
	PostMovement(ctx context.Context, movementID int64) (*PostResult, error)
	PostMovementForInstruction(ctx context.Context, instructionID int64) (*PostResult, error)
}

// BusinessCalendar is the optional holiday-aware business-day seam;
// *settlement.HolidayCalendar satisfies it. Nil → weekends-only.
type BusinessCalendar interface {
	IsBusinessDay(ccy string, d time.Time) bool
}

// OpsAlert / OpsAlerter — the shared ops-paging seam, declared as
// package-level aliases in client_money.go (Task 24.3.11):
//
//	OpsAlert   = settlement.OpsAlert
//	OpsAlerter = settlement.OpsAlerter
//
// The durable funding_ops_alerts row is the record of truth; paging is
// best-effort.

// ConfirmationResult reports what ProcessConfirmation did.
type ConfirmationResult struct {
	InstructionID  int64   `json:"instruction_id"`
	MessageID      int64   `json:"message_id"` // swift_messages row
	Status         string  `json:"status"`     // SETTLED | ALREADY_SETTLED
	MovementPosted bool    `json:"movement_posted"`
	BalanceAfter   *string `json:"balance_after,omitempty"` // decimal text
	AlreadySettled bool    `json:"already_settled"`
}

// ConfirmationService processes correspondent confirmations and runs
// the no-confirmation timeout sweep.
type ConfirmationService struct {
	store     ConfirmationStore
	confirmer SettlementConfirmer
	poster    MovementPoster
	recorder  SwiftRecorder
	cal       BusinessCalendar // optional — weekends-only when nil
	alerter   OpsAlerter       // optional page channel
	clock     func() time.Time
	logf      func(format string, args ...any)
}

// NewConfirmationService wires the service; store + confirmer + poster
// are mandatory (fail-closed — a confirmation that cannot settle or
// cannot post the balance must never be processed degraded).
func NewConfirmationService(store ConfirmationStore, conf SettlementConfirmer, poster MovementPoster) (*ConfirmationService, error) {
	if store == nil || conf == nil || poster == nil {
		return nil, fmt.Errorf("backoffice: confirmation service requires store, confirmer and poster")
	}
	return &ConfirmationService{
		store: store, confirmer: conf, poster: poster, clock: time.Now}, nil
}

// WithRecorder wires the swift_messages journal seam (Task 24.3.4).
// nil → inbound messages are not journaled; production must wire it.
func (s *ConfirmationService) WithRecorder(r SwiftRecorder) *ConfirmationService {
	s.recorder = r
	return s
}

// WithCalendar wires the holiday-aware business-day calendar.
func (s *ConfirmationService) WithCalendar(c BusinessCalendar) *ConfirmationService {
	s.cal = c
	return s
}

// WithAlerter wires the ops paging seam.
func (s *ConfirmationService) WithAlerter(a OpsAlerter) *ConfirmationService {
	s.alerter = a
	return s
}

// WithClock overrides the clock (tests).
func (s *ConfirmationService) WithClock(c func() time.Time) *ConfirmationService {
	s.clock = c
	return s
}

// WithLogger wires a log sink.
func (s *ConfirmationService) WithLogger(f func(format string, args ...any)) *ConfirmationService {
	s.logf = f
	return s
}

func (s *ConfirmationService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// ProcessConfirmation applies one inbound MT900/MT910: journal →
// resolve → confirm (SETTLED) → post the nostro balance. Idempotent on
// the instruction: a replayed confirmation returns ALREADY_SETTLED and
// does not re-apply the balance.
func (s *ConfirmationService) ProcessConfirmation(ctx context.Context, c SwiftConfirmation) (*ConfirmationResult, error) {
	if c.MessageType != MsgMT900 && c.MessageType != MsgMT910 {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("confirmation type %q — MT900|MT910 only", c.MessageType))
	}
	ref := c.RelatedRef
	if ref == "" {
		ref = c.Reference // some correspondents echo :20: in place of :21:
	}
	if ref == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"confirmation carries neither :21: related ref nor :20: reference")
	}
	if !currencyRe.MatchString(c.Currency) || !c.Amount.IsPositive() {
		return nil, excerrors.New("INVALID_REQUEST",
			"confirmation requires a 3-letter currency and positive :32A: amount")
	}

	res := &ConfirmationResult{}
	// Step 1 — journal the inbound message first (immutable audit trail;
	// an unrecorded confirmation must never mutate settlement state).
	if s.recorder != nil {
		m, err := s.recorder.Record(ctx, SwiftMessage{
			MessageType:      c.MessageType,
			Reference:        c.Reference,
			RelatedReference: c.RelatedRef,
			Direction:        SwiftIn,
			Status:           "RECEIVED",
			RawPayload:       c.RawPayload,
			MsgTimestamp:     c.ReceivedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("confirmation journal: %w", err)
		}
		res.MessageID = m.ID
	}

	// Step 2 — resolve the leg by the correspondent's related ref.
	leg, err := s.store.InstructionBySwiftRef(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve confirmation ref %q: %w", ref, err)
	}
	if leg == nil {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("no dispatched settlement leg for reference %q", ref))
	}
	res.InstructionID = leg.ID
	if leg.Currency != c.Currency {
		return nil, excerrors.New("INVALID_REQUEST", fmt.Sprintf(
			"confirmation currency %s != leg currency %s for %q",
			c.Currency, leg.Currency, ref))
	}

	// Step 3 — flip PENDING→SETTLED (idempotent on replay).
	confirmed, err := s.confirmer.ConfirmSettlement(ctx, leg.ID, ref)
	if err != nil {
		return nil, err
	}
	res.Status = confirmed.Status
	res.AlreadySettled = leg.Status == "SETTLED"

	// Step 4 — post the nostro balance movement (Task 24.3.1 seam).
	post, err := s.poster.PostMovementForInstruction(ctx, leg.ID)
	if err != nil {
		return nil, fmt.Errorf("confirmation %d balance post: %w", leg.ID, err)
	}
	res.MovementPosted = post.Posted
	if post.Posted {
		s := post.BalanceAfter.String()
		res.BalanceAfter = &s
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Timeout sweep — >2 business days without confirmation → P2 (§24 #12).
// ---------------------------------------------------------------------------

// SweepReport summarizes one SweepOverdue pass.
type SweepReport struct {
	AsOf      time.Time `json:"as_of"`
	Scanned   int       `json:"scanned"`
	Overdue   int       `json:"overdue"`
	Alerted   int       `json:"alerted"`    // newly raised P2 rows (dedup-filtered)
	AlreadyUp int       `json:"already_up"` // overdue with an OPEN alert already standing
}

// SweepOverdue scans dispatched-but-unconfirmed legs and raises a
// deduplicated P2 alert for each whose two-business-day confirmation
// window has elapsed.
func (s *ConfirmationService) SweepOverdue(ctx context.Context, limit int) (*SweepReport, error) {
	now := s.clock().UTC()
	rep := &SweepReport{AsOf: now}
	// Calendar upper bound: business days ⊆ calendar days, so anything
	// dispatched within the last 48h cannot be overdue — a cheap bound.
	legs, err := s.store.StaleDispatched(ctx, now.Add(-48*time.Hour), nostroLimitOr(limit, 500))
	if err != nil {
		return nil, err
	}
	rep.Scanned = len(legs)
	for _, leg := range legs {
		if leg.DispatchedAt == nil || leg.SwiftMessageID == nil {
			continue
		}
		deadline := s.confirmationDeadline(*leg.DispatchedAt, leg.Currency)
		if !now.After(deadline) {
			continue
		}
		rep.Overdue++
		key := fmt.Sprintf("settlement-confirm-overdue:%d", leg.ID)
		open, err := s.store.HasOpenAlert(ctx, CodeConfirmationOverdue, key)
		if err != nil {
			s.log("backoffice: overdue dedup check leg %d: %v", leg.ID, err)
			continue
		}
		if open {
			rep.AlreadyUp++
			continue
		}
		err = s.store.InTx(ctx, func(ctx context.Context, tx NostroTx) error {
			_, ierr := tx.InsertAlert(ctx, s.alertRow(leg, deadline, key))
			return ierr
		})
		if err != nil {
			s.log("backoffice: overdue alert leg %d: %v", leg.ID, err)
			continue
		}
		rep.Alerted++
		s.raise(ctx, "P2", CodeConfirmationOverdue,
			fmt.Sprintf("settlement leg %d unconfirmed past %s", leg.ID,
				deadline.Format("2006-01-02")), nil)
	}
	return rep, nil
}

// alertRow renders the durable funding_ops_alerts row for one overdue leg.
func (s *ConfirmationService) alertRow(leg InstructionRow, deadline time.Time, dedupKey string) OpsAlertRow {
	ref := ""
	if leg.SwiftMessageID != nil {
		ref = *leg.SwiftMessageID
	}
	ccy := leg.Currency
	return OpsAlertRow{
		Code:     CodeConfirmationOverdue,
		Severity: "P2",
		Currency: &ccy,
		Summary: fmt.Sprintf(
			"settlement leg %d (%s %s %s, ref %s) unconfirmed past 2-business-day deadline %s",
			leg.ID, leg.Direction, leg.Amount.String(), leg.Currency, ref,
			deadline.Format("2006-01-02")),
		Detail: []byte(fmt.Sprintf(
			`{"instruction_id":%d,"trade_id":%d,"swift_message_id":%q,"deadline":%q,"dedup_key":%q}`,
			leg.ID, leg.TradeID, ref, deadline.Format(time.RFC3339), dedupKey)),
	}
}

// confirmationDeadline returns the end of the second business day after
// the dispatch date for the leg's currency. Holiday-aware when a
// BusinessCalendar is wired; weekends-only otherwise (documented
// fallback — the alert is ops-grade, never a money-path gate).
func (s *ConfirmationService) confirmationDeadline(dispatched time.Time, ccy string) time.Time {
	d := dispatched.UTC().Truncate(24 * time.Hour)
	count := 0
	for i := 0; i < 31 && count < 2; i++ { // 31-day guard rail
		d = d.Add(24 * time.Hour)
		if s.isBusinessDay(ccy, d) {
			count++
		}
	}
	// End of the second business day (UTC).
	return d.Add(24 * time.Hour).Add(-time.Nanosecond)
}

func (s *ConfirmationService) isBusinessDay(ccy string, d time.Time) bool {
	if s.cal != nil {
		return s.cal.IsBusinessDay(ccy, d)
	}
	wd := d.Weekday()
	return wd != time.Saturday && wd != time.Sunday
}

func (s *ConfirmationService) raise(ctx context.Context, sev, code, summary string, cause error) {
	if s.alerter == nil {
		return
	}
	a := OpsAlert{Severity: sev, Code: code, Summary: summary}
	if cause != nil {
		a.Err = cause.Error()
	}
	if err := s.alerter.Raise(ctx, a); err != nil {
		s.log("backoffice: alert %s raise: %v", code, err)
	}
}

func nostroLimitOr(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// ---------------------------------------------------------------------------
// PgNostroStore implementation
// ---------------------------------------------------------------------------

func (s *PgNostroStore) InstructionBySwiftRef(ctx context.Context, ref string) (*InstructionRow, error) {
	leg, err := scanInstruction(s.Pool.QueryRow(ctx, `
		SELECT id, trade_id, account_id, currency, amount::text, direction::text,
		       COALESCE(settlement_date, '1970-01-01'::date), nostro_account_id,
		       status::text, swift_message_id, dispatched_at, settled_at
		  FROM settlement_instructions
		 WHERE swift_message_id = $1`, ref))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return leg, err
}

func (s *PgNostroStore) StaleDispatched(ctx context.Context, before time.Time, limit int) ([]InstructionRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, trade_id, account_id, currency, amount::text, direction::text,
		       COALESCE(settlement_date, '1970-01-01'::date), nostro_account_id,
		       status::text, swift_message_id, dispatched_at, settled_at
		  FROM settlement_instructions
		 WHERE status = 'PENDING'
		   AND swift_message_id IS NOT NULL
		   AND dispatched_at IS NOT NULL
		   AND dispatched_at <= $1
		 ORDER BY dispatched_at LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstructionRow
	for rows.Next() {
		leg, err := scanInstruction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *leg)
	}
	return out, rows.Err()
}

func scanInstruction(row pgx.Row) (*InstructionRow, error) {
	var l InstructionRow
	var amt string
	err := row.Scan(&l.ID, &l.TradeID, &l.AccountID, &l.Currency, &amt,
		&l.Direction, &l.SettlementDate, &l.NostroAccountID, &l.Status,
		&l.SwiftMessageID, &l.DispatchedAt, &l.SettledAt)
	if err != nil {
		return nil, err
	}
	l.Amount, err = decimal.NewFromString(amt)
	if err != nil {
		return nil, fmt.Errorf("instruction %d amount %q: %w", l.ID, amt, err)
	}
	return &l, nil
}

// HasOpenAlert checks the dedup key inside the alert detail payload —
// sweeps re-run freely without stacking duplicate P2 rows.
func (s *PgNostroStore) HasOpenAlert(ctx context.Context, code, dedupKey string) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS(
		    SELECT 1 FROM funding_ops_alerts
		     WHERE code = $1 AND status = 'OPEN'
		       AND detail->>'dedup_key' = $2)`, code, dedupKey).Scan(&exists)
	return exists, err
}
