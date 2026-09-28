// settlement_service.go — T+0/T+1/T+2 Settlement Instruction Service
// (Phase-03 Task 3.3.3; spec §5.19, §6.3, §17.1, §24 #19/#20).
//
// On every engine trade fill the service writes the per-side currency
// legs of the spot settlement into settlement_instructions — a spot FX
// trade produces four rows: the buyer RECEIVEs the base amount and PAYs
// the quote notional; the seller PAYs base and RECEIVEs quote. Rows are
// deduped by the (trade_id, account_id, currency, direction) unique index
// (migration 112), so fill replays are idempotent on trade_id.
//
// The value date is instruments.settlement_cycle (T+1 most spot FX, T+2
// exotics, T+0 same-day for USD/CAD and USD/MXN — spec §6.3) walked
// forward by HolidayCalendar.SettlementDate under ISDA Modified
// Following across every settlement center (base, quote, plus USD for
// cross pairs — weekends, full and split holidays all shift the date).
//
// At the settlement date DispatchDue renders the SWIFT MT202 / ISO 20022
// pacs.009 payment payload, claims the row (swift_message_id = the
// :20:/MsgId transaction reference) and hands the message to the
// MessageDispatcher seam — the actual send is Phase-11 banking rails
// (Task 11.3.1), shipped here as NullDispatcher // PHASE-11 STUB.
// Claim-before-send is deliberate: a crash after dispatch leaves a
// recoverable PENDING row with message id + payload, never a duplicate
// payment.
//
// On correspondent confirmation ConfirmSettlement flips PENDING→SETTLED
// and records the nostro movement intent in nostro_movements (migration
// 112): a PAY leg DEBITs the nostro, a RECEIVE leg CREDITs it (spec
// §17.1). The nostro_accounts.balance mutation itself is Phase-24 nostro
// accounting (Task 24.3.1) — this service records the intent row that
// poster consumes. // PHASE-24 SEAM
//
// Fail-closed (spec §2.7): unknown cycles, missing calendars, missing or
// non-ACTIVE nostro accounts and malformed BICs are errors, never silent
// skips.
package settlement

import (
	"context"
	"encoding/xml"
	stderrors "errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// Scaffold error codes — register in Phase-05 Task 5.3.21.
const (
	// CodeSettlementInvalidFill: fill is missing ids, has non-positive
	// price/quantity, or carries an unset trade date.
	CodeSettlementInvalidFill = "SETTLEMENT_INVALID_FILL"
	// CodeSettlementNotFound: instruction id not present.
	CodeSettlementNotFound = "SETTLEMENT_NOT_FOUND"
	// CodeSettlementNostroMissing: no ACTIVE nostro account for the
	// settlement currency — instructions for that currency cannot settle.
	CodeSettlementNostroMissing = "SETTLEMENT_NOSTRO_MISSING"
	// CodeSettlementStateConflict: confirmation on a leg that is not
	// PENDING (FAILED/RECONCILED) — replayed SETTLED legs are idempotent.
	CodeSettlementStateConflict = "SETTLEMENT_STATE_CONFLICT"
	// CodeSettlementInvalidMessage: cannot render a payment payload —
	// e.g. the nostro row lacks a BIC.
	CodeSettlementInvalidMessage = "SETTLEMENT_INVALID_MESSAGE"
)

// SettlementDirection mirrors settlement_direction_enum.
type SettlementDirection string

const (
	DirectionPay     SettlementDirection = "PAY"
	DirectionReceive SettlementDirection = "RECEIVE"
)

// SettlementStatus mirrors settlement_status_enum.
type SettlementStatus string

const (
	SettlePending    SettlementStatus = "PENDING"
	SettleSettled    SettlementStatus = "SETTLED"
	SettleFailed     SettlementStatus = "FAILED"
	SettleReconciled SettlementStatus = "RECONCILED"
)

// NostroMovementDirection mirrors nostro_movement_direction_enum.
type NostroMovementDirection string

const (
	NostroDebit  NostroMovementDirection = "DEBIT"
	NostroCredit NostroMovementDirection = "CREDIT"
)

// NostroMovementStatus mirrors nostro_movement_status_enum.
type NostroMovementStatus string

const (
	MovementPending NostroMovementStatus = "PENDING" // intent — Phase-24 posts
	MovementPosted  NostroMovementStatus = "POSTED"
	MovementVoid    NostroMovementStatus = "VOID"
)

// MovementDirection maps the instruction direction onto the nostro
// movement it implies (spec §17.1: "Nostro account debited (PAY) or
// credited (RECEIVE)").
func (d SettlementDirection) MovementDirection() NostroMovementDirection {
	if d == DirectionPay {
		return NostroDebit
	}
	return NostroCredit
}

// InstrumentRef is the settlement-relevant view of instruments.
type InstrumentRef struct {
	ID              int64
	Symbol          string
	BaseCurrency    string
	QuoteCurrency   string
	SettlementCycle int // 0 same-day, 1 T+1, 2 T+2 (spec §5.1)
}

// NostroAccount is the settlement-relevant view of nostro_accounts.
type NostroAccount struct {
	ID            int64
	Currency      string
	BankName      string
	BankCode      string // SWIFT BIC / routing code of the correspondent bank
	AccountNumber string
	IBAN          string // empty when non-IBAN
}

// SettlementInstruction is one settlement_instructions leg.
type SettlementInstruction struct {
	ID              int64
	TradeID         int64
	AccountID       int64
	Currency        string
	Amount          decimal.Decimal
	Direction       SettlementDirection
	SettlementDate  time.Time // UTC midnight day
	NostroAccountID *int64
	Status          SettlementStatus
	SwiftMessageID  *string
	MessageFormat   *MessageFormat
	MessagePayload  *string
	ConfirmationRef *string
	CreatedAt       time.Time
	SettledAt       *time.Time
}

// SettlementFill is the engine fill resolved to account ids — the wire
// TradeFill carries order ids only; the caller resolves buyer/seller
// accounts and the trade timestamp upstream (same discipline as
// PositionFill in position_service.go).
type SettlementFill struct {
	TradeID         int64
	InstrumentID    int64
	BuyerAccountID  int64
	SellerAccountID int64
	Price           decimal.Decimal // quote per base unit
	Quantity        decimal.Decimal // base units filled
	TradeDate       time.Time       // trade created_at (UTC)
}

// GenerateResult reports what GenerateInstructions wrote.
type GenerateResult struct {
	SettlementDate time.Time
	Legs           []SettlementInstruction
	// Inserted is the number of rows actually inserted — 0 on a pure
	// idempotent replay of a previously generated fill.
	Inserted int
}

// NostroMovement is the nostro_movements intent row written on
// confirmation — Phase-24 nostro accounting consumes PENDING rows and
// applies the balance mutation.
type NostroMovement struct {
	ID                      int64
	SettlementInstructionID int64
	NostroAccountID         int64
	Currency                string
	Amount                  decimal.Decimal
	Direction               NostroMovementDirection
	Status                  NostroMovementStatus
	ConfirmationRef         string
}

// DueLeg pairs a dispatchable instruction with its nostro account.
type DueLeg struct {
	Instruction SettlementInstruction
	Nostro      NostroAccount
}

// MessageFormat selects the payment payload dialect.
type MessageFormat string

const (
	FormatMT202   MessageFormat = "MT202"   // SWIFT FIN category-2
	FormatPacs009 MessageFormat = "PACS009" // ISO 20022 financial-institution credit transfer
)

// ---------------------------------------------------------------------------
// Store seam
// ---------------------------------------------------------------------------

// SettlementStore is the persistence seam; PgxSettlementStore implements
// it over pgx. InTx implementations must run fn inside a SERIALIZABLE
// transaction and roll back on error (same discipline as PositionStore).
type SettlementStore interface {
	// Instrument returns the settlement fields for the instrument;
	// missing instrument is an error (fail-closed).
	Instrument(ctx context.Context, instrumentID int64) (InstrumentRef, error)
	// ActiveNostroFor resolves the primary ACTIVE nostro account for the
	// currency; missing is an error — never silently skip a leg.
	ActiveNostroFor(ctx context.Context, currency string) (NostroAccount, error)
	// InsertInstructions writes the legs idempotently (ON CONFLICT DO
	// NOTHING on the leg unique index); returns the rows inserted.
	InsertInstructions(ctx context.Context, legs []SettlementInstruction) (int, error)
	// DueInstructions returns undispatched PENDING legs whose
	// settlement_date is on/before day, joined with their nostro account.
	DueInstructions(ctx context.Context, day time.Time) ([]DueLeg, error)
	// MarkDispatched claims the row for dispatch: sets swift_message_id,
	// format, payload, dispatched_at — only while swift_message_id is
	// still NULL. claimed=false means a concurrent dispatcher already
	// claimed it (skip — never double-send).
	MarkDispatched(ctx context.Context, instructionID int64, messageID string, format MessageFormat, payload string, dispatchedAt time.Time) (claimed bool, err error)
	// InTx runs fn inside a SERIALIZABLE transaction.
	InTx(ctx context.Context, fn func(ctx context.Context, tx SettlementTx) error) error
}

// SettlementTx is the transactional view inside SettlementStore.InTx.
type SettlementTx interface {
	// LockInstruction SELECTs the row FOR UPDATE; found=false when absent.
	LockInstruction(ctx context.Context, instructionID int64) (leg SettlementInstruction, found bool, err error)
	// SetSettled flips PENDING→SETTLED recording settled_at and the
	// correspondent confirmation reference.
	SetSettled(ctx context.Context, instructionID int64, settledAt time.Time, confirmationRef string) error
	// RecordNostroMovement appends the nostro intent row.
	RecordNostroMovement(ctx context.Context, m NostroMovement) error
}

// ---------------------------------------------------------------------------
// Dispatch seam — PHASE-11 STUB
// ---------------------------------------------------------------------------

// OutboundMessage is the rendered payment handed to the banking rail.
type OutboundMessage struct {
	InstructionID int64
	TradeID       int64
	Format        MessageFormat
	// MessageID is the :20: / MsgId transaction reference persisted to
	// settlement_instructions.swift_message_id.
	MessageID   string
	Payload     string
	ValueDate   time.Time
	Currency    string
	Amount      decimal.Decimal
	ReceiverBIC string
}

// MessageDispatcher is the send seam for correspondent-bank rails.
//
// PHASE-11 STUB — SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2 dispatch is Phase-11
// Task 11.3.1 scope; this phase renders and persists the payload only.
type MessageDispatcher interface {
	Dispatch(ctx context.Context, msg OutboundMessage) error
}

// NullDispatcher is the Phase-3 stand-in: it records messages for
// inspection (tests, dev harness) and never errors.
type NullDispatcher struct {
	mu   sync.Mutex
	Sent []OutboundMessage
}

// Dispatch records the message; the send itself is PHASE-11 STUB.
func (d *NullDispatcher) Dispatch(_ context.Context, msg OutboundMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Sent = append(d.Sent, msg)
	return nil
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// SettlementOptions configures SettlementService.
type SettlementOptions struct {
	// SenderBIC is the exchange's own BIC — required (fail-closed); the
	// payment payload must never carry a placeholder sender.
	SenderBIC string
	// Format selects the payload dialect for dispatched instructions;
	// empty defaults to FormatMT202.
	Format MessageFormat
	// Dispatcher is the rail send seam; nil installs NullDispatcher.
	Dispatcher MessageDispatcher
	// Clock overrides time.Now (tests); nil defaults to time.Now.
	Clock func() time.Time
}

// SettlementService generates and dispatches settlement instructions.
type SettlementService struct {
	store      SettlementStore
	cal        *HolidayCalendar
	senderBIC  string
	format     MessageFormat
	dispatcher MessageDispatcher
	clock      func() time.Time
}

// NewSettlementService wires the service; store, cal and a valid
// SenderBIC are required (fail-closed per spec §2.7).
func NewSettlementService(store SettlementStore, cal *HolidayCalendar, opts SettlementOptions) (*SettlementService, error) {
	if store == nil {
		return nil, fmt.Errorf("settlement service: nil store")
	}
	if cal == nil {
		return nil, fmt.Errorf("settlement service: nil holiday calendar")
	}
	if !bicRe.MatchString(opts.SenderBIC) {
		return nil, excerrors.New(CodeSettlementInvalidMessage,
			fmt.Sprintf("settlement service: invalid sender BIC %q", opts.SenderBIC))
	}
	format := opts.Format
	if format == "" {
		format = FormatMT202
	}
	if format != FormatMT202 && format != FormatPacs009 {
		return nil, excerrors.New(CodeSettlementInvalidMessage,
			fmt.Sprintf("settlement service: unknown message format %q", format))
	}
	d := opts.Dispatcher
	if d == nil {
		d = &NullDispatcher{}
	}
	clk := opts.Clock
	if clk == nil {
		clk = time.Now
	}
	return &SettlementService{
		store:      store,
		cal:        cal,
		senderBIC:  opts.SenderBIC,
		format:     format,
		dispatcher: d,
		clock:      clk,
	}, nil
}

var bicRe = regexp.MustCompile(`^[A-Z]{6}[A-Z0-9]{2}([A-Z0-9]{3})?$`)

// ---------------------------------------------------------------------------
// Fill → settlement legs (spec §17.1 step 1)
// ---------------------------------------------------------------------------

// GenerateInstructions writes the four settlement legs for one engine
// trade fill — buyer RECEIVE base / PAY quote, seller PAY base / RECEIVE
// quote — with settlement_date computed by the holiday calendar from the
// instrument's settlement_cycle. Idempotent on trade_id: a replayed fill
// returns the same legs with Inserted=0 (INSERT ... ON CONFLICT DO
// NOTHING against the leg unique index).
func (s *SettlementService) GenerateInstructions(ctx context.Context, f SettlementFill) (*GenerateResult, error) {
	if f.TradeID <= 0 || f.InstrumentID <= 0 || f.BuyerAccountID <= 0 || f.SellerAccountID <= 0 {
		return nil, excerrors.New(CodeSettlementInvalidFill,
			"fill missing trade/instrument/account id")
	}
	if !f.Price.IsPositive() || !f.Quantity.IsPositive() {
		return nil, excerrors.New(CodeSettlementInvalidFill,
			"fill price and quantity must be > 0")
	}
	if f.TradeDate.IsZero() {
		return nil, excerrors.New(CodeSettlementInvalidFill,
			"fill trade date is required")
	}

	inst, err := s.store.Instrument(ctx, f.InstrumentID)
	if err != nil {
		return nil, fmt.Errorf("settlement: load instrument %d: %w", f.InstrumentID, err)
	}
	if inst.SettlementCycle < 0 || inst.SettlementCycle > 2 {
		return nil, excerrors.New(CodeSettlementInvalidFill, fmt.Sprintf(
			"instrument %d (%s) has unsupported settlement_cycle T+%d — spot cycles are 0/1/2",
			inst.ID, inst.Symbol, inst.SettlementCycle))
	}

	// Value date: T+n mutual business days across all settlement centers,
	// Modified Following (calendar_service.go — Task 3.3.8).
	sd, err := s.cal.SettlementDate(inst.BaseCurrency, inst.QuoteCurrency, f.TradeDate, inst.SettlementCycle)
	if err != nil {
		return nil, fmt.Errorf("settlement: value date for %s: %w", inst.Symbol, err)
	}

	baseNostro, err := s.store.ActiveNostroFor(ctx, inst.BaseCurrency)
	if err != nil {
		return nil, excerrors.Wrap(CodeSettlementNostroMissing,
			fmt.Sprintf("settlement: no ACTIVE nostro for %s", inst.BaseCurrency), err)
	}
	quoteNostro, err := s.store.ActiveNostroFor(ctx, inst.QuoteCurrency)
	if err != nil {
		return nil, excerrors.Wrap(CodeSettlementNostroMissing,
			fmt.Sprintf("settlement: no ACTIVE nostro for %s", inst.QuoteCurrency), err)
	}

	baseAmount := f.Quantity.Round(8)               // base currency units
	quoteAmount := f.Quantity.Mul(f.Price).Round(8) // quote notional paid per base
	baseID, quoteID := baseNostro.ID, quoteNostro.ID

	legs := []SettlementInstruction{
		{TradeID: f.TradeID, AccountID: f.BuyerAccountID, Currency: inst.BaseCurrency,
			Amount: baseAmount, Direction: DirectionReceive, SettlementDate: sd,
			NostroAccountID: &baseID, Status: SettlePending},
		{TradeID: f.TradeID, AccountID: f.BuyerAccountID, Currency: inst.QuoteCurrency,
			Amount: quoteAmount, Direction: DirectionPay, SettlementDate: sd,
			NostroAccountID: &quoteID, Status: SettlePending},
		{TradeID: f.TradeID, AccountID: f.SellerAccountID, Currency: inst.BaseCurrency,
			Amount: baseAmount, Direction: DirectionPay, SettlementDate: sd,
			NostroAccountID: &baseID, Status: SettlePending},
		{TradeID: f.TradeID, AccountID: f.SellerAccountID, Currency: inst.QuoteCurrency,
			Amount: quoteAmount, Direction: DirectionReceive, SettlementDate: sd,
			NostroAccountID: &quoteID, Status: SettlePending},
	}

	inserted, err := s.store.InsertInstructions(ctx, legs)
	if err != nil {
		return nil, fmt.Errorf("settlement: insert instructions trade %d: %w", f.TradeID, err)
	}
	return &GenerateResult{SettlementDate: sd, Legs: legs, Inserted: inserted}, nil
}

// ---------------------------------------------------------------------------
// Settlement-date dispatch (spec §17.1 step 2)
// ---------------------------------------------------------------------------

// InstructionError records one leg's dispatch failure — per-leg errors are
// collected, never abort the batch (the row stays claimed/pending for
// ops review, never silently dropped).
type InstructionError struct {
	InstructionID int64
	Err           error
}

// DispatchReport summarises one DispatchDue pass.
type DispatchReport struct {
	AsOf       time.Time
	Due        int // legs eligible for dispatch
	Claimed    int // rows newly claimed (payload stored)
	Dispatched int // messages handed to the dispatcher
	Errors     []InstructionError
}

// DispatchDue renders and claims the payment payload for every
// undispatched PENDING instruction whose settlement_date is on/before
// asOf (UTC-normalized). Claim-before-send: the payload + :20: reference
// persist before the dispatcher runs, so a crash mid-dispatch leaves a
// recoverable row rather than a duplicate payment.
func (s *SettlementService) DispatchDue(ctx context.Context, asOf time.Time) (*DispatchReport, error) {
	day := normalizeDay(asOf)
	rep := &DispatchReport{AsOf: day}

	due, err := s.store.DueInstructions(ctx, day)
	if err != nil {
		return nil, fmt.Errorf("settlement: due scan %s: %w", day.Format("2006-01-02"), err)
	}
	rep.Due = len(due)

	for _, dl := range due {
		leg := dl.Instruction
		msg, err := s.buildMessage(leg, dl.Nostro)
		if err != nil {
			rep.Errors = append(rep.Errors, InstructionError{InstructionID: leg.ID, Err: err})
			continue
		}
		claimed, err := s.store.MarkDispatched(ctx, leg.ID, msg.MessageID, msg.Format, msg.Payload, s.clock().UTC())
		if err != nil {
			rep.Errors = append(rep.Errors, InstructionError{InstructionID: leg.ID, Err: err})
			continue
		}
		if !claimed {
			continue // another dispatcher claimed it — never double-send
		}
		rep.Claimed++
		if err := s.dispatcher.Dispatch(ctx, msg); err != nil {
			// Row is claimed with payload persisted — recoverable via
			// ops re-dispatch (Phase-11 retry policy), not a silent loss.
			rep.Errors = append(rep.Errors, InstructionError{InstructionID: leg.ID,
				Err: fmt.Errorf("dispatch claimed row: %w", err)})
			continue
		}
		rep.Dispatched++
	}
	return rep, nil
}

// RunDispatchLoop fires DispatchDue every interval until ctx is
// cancelled — the settlement-date scheduler (same ticker pattern as
// VipEngine.RunDaily). interval <= 0 defaults to 5 minutes so same-day
// (T+0) legs still dispatch intraday.
func (s *SettlementService) RunDispatchLoop(ctx context.Context, interval time.Duration, onErr func(error)) error {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-ticker.C:
			if _, err := s.DispatchDue(ctx, t); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// buildMessage renders the outbound payment for one leg.
func (s *SettlementService) buildMessage(leg SettlementInstruction, nostro NostroAccount) (OutboundMessage, error) {
	if nostro.ID == 0 || !bicRe.MatchString(nostro.BankCode) {
		return OutboundMessage{}, excerrors.New(CodeSettlementInvalidMessage, fmt.Sprintf(
			"instruction %d: nostro %v lacks a valid BIC", leg.ID, leg.NostroAccountID))
	}
	msg := OutboundMessage{
		InstructionID: leg.ID,
		TradeID:       leg.TradeID,
		Format:        s.format,
		MessageID:     swiftRef("SI", leg.ID),
		ValueDate:     leg.SettlementDate,
		Currency:      leg.Currency,
		Amount:        leg.Amount,
		ReceiverBIC:   bic11(nostro.BankCode),
	}
	switch s.format {
	case FormatMT202:
		payload, err := (SwiftMT202{
			SenderBIC:              s.senderBIC,
			ReceiverBIC:            nostro.BankCode,
			TransactionRef:         msg.MessageID,
			RelatedRef:             swiftRef("TRD", leg.TradeID),
			ValueDate:              leg.SettlementDate,
			Currency:               leg.Currency,
			Amount:                 leg.Amount,
			OrderingInstitutionBIC: s.senderBIC,
			BeneficiaryBIC:         nostro.BankCode,
			BeneficiaryAccount:     nostroAccountRef(nostro),
		}).Marshal()
		if err != nil {
			return OutboundMessage{}, err
		}
		msg.Payload = payload
	case FormatPacs009:
		payload, err := marshalPacs009(pacs009Message{
			MsgID:       msg.MessageID,
			CreatedAt:   s.clock().UTC(),
			TxID:        swiftRef("TX", leg.ID),
			EndToEndID:  swiftRef("TRD", leg.TradeID),
			ValueDate:   leg.SettlementDate,
			Currency:    leg.Currency,
			Amount:      leg.Amount,
			SenderBIC:   s.senderBIC,
			ReceiverBIC: nostro.BankCode,
		})
		if err != nil {
			return OutboundMessage{}, err
		}
		msg.Payload = payload
	default:
		return OutboundMessage{}, excerrors.New(CodeSettlementInvalidMessage,
			fmt.Sprintf("unknown message format %q", s.format))
	}
	return msg, nil
}

// nostroAccountRef picks the nostro account reference carried on the
// :58A: account line — IBAN preferred, else the bank account number.
func nostroAccountRef(n NostroAccount) string {
	if n.IBAN != "" {
		return n.IBAN
	}
	return n.AccountNumber
}

// ---------------------------------------------------------------------------
// Confirmation (spec §17.1 step 2 tail + §17.3)
// ---------------------------------------------------------------------------

// ConfirmSettlement processes the correspondent-bank confirmation for one
// instruction: flips PENDING→SETTLED and records the nostro movement
// intent (PAY → DEBIT, RECEIVE → CREDIT) in nostro_movements inside the
// same transaction.
//
// Idempotent: confirming an already-SETTLED leg returns it unchanged.
// FAILED/RECONCILED legs conflict (SETTLEMENT_STATE_CONFLICT). The
// nostro_accounts.balance mutation itself is Phase-24 nostro accounting
// (Task 24.3.1); the PENDING nostro_movements row is the intent it
// posts. // PHASE-24 SEAM
func (s *SettlementService) ConfirmSettlement(ctx context.Context, instructionID int64, confirmationRef string) (*SettlementInstruction, error) {
	if instructionID <= 0 {
		return nil, excerrors.New(CodeSettlementInvalidFill,
			"settlement: instruction id must be positive")
	}
	var out *SettlementInstruction
	err := s.store.InTx(ctx, func(ctx context.Context, tx SettlementTx) error {
		leg, found, err := tx.LockInstruction(ctx, instructionID)
		if err != nil {
			return fmt.Errorf("settlement: lock instruction %d: %w", instructionID, err)
		}
		if !found {
			return excerrors.New(CodeSettlementNotFound,
				fmt.Sprintf("settlement instruction %d not found", instructionID))
		}
		switch leg.Status {
		case SettleSettled:
			out = &leg // idempotent replay — no new movement
			return nil
		case SettlePending:
		default:
			return excerrors.New(CodeSettlementStateConflict, fmt.Sprintf(
				"instruction %d is %s — only PENDING legs can confirm", instructionID, leg.Status))
		}
		if leg.NostroAccountID == nil {
			return excerrors.New(CodeSettlementNostroMissing, fmt.Sprintf(
				"instruction %d has no nostro_account_id", instructionID))
		}
		settledAt := s.clock().UTC()
		if err := tx.SetSettled(ctx, instructionID, settledAt, confirmationRef); err != nil {
			return fmt.Errorf("settlement: mark settled %d: %w", instructionID, err)
		}
		if err := tx.RecordNostroMovement(ctx, NostroMovement{
			SettlementInstructionID: leg.ID,
			NostroAccountID:         *leg.NostroAccountID,
			Currency:                leg.Currency,
			Amount:                  leg.Amount,
			Direction:               leg.Direction.MovementDirection(),
			Status:                  MovementPending,
			ConfirmationRef:         confirmationRef,
		}); err != nil {
			return fmt.Errorf("settlement: nostro movement %d: %w", instructionID, err)
		}
		leg.Status = SettleSettled
		leg.SettledAt = &settledAt
		if confirmationRef != "" {
			leg.ConfirmationRef = &confirmationRef
		}
		out = &leg
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// SWIFT MT202 rendering (spec §17.1 — payload only; send is Phase-11)
// ---------------------------------------------------------------------------

// SwiftMT202 is the field model for a FIN category-2 bank transfer.
type SwiftMT202 struct {
	SenderBIC              string // block-1 sender / :52A:
	ReceiverBIC            string // block-2 receiver (correspondent bank)
	TransactionRef         string // :20: — ≤16x, settlement leg reference
	RelatedRef             string // :21: — trade reference (optional)
	ValueDate              time.Time
	Currency               string
	Amount                 decimal.Decimal // positive, rendered with decimal comma
	OrderingInstitutionBIC string          // :52A: — optional
	BeneficiaryBIC         string          // :58A:
	BeneficiaryAccount     string          // :58A: account line — optional
}

// Marshal renders the FIN block. Field semantics per SWIFT MT202:
// :20: transaction reference, :21: related reference,
// :32A: value date (YYMMDD) + currency + amount (decimal comma),
// :52A: ordering institution, :58A: beneficiary institution [/account].
func (m SwiftMT202) Marshal() (string, error) {
	if !bicRe.MatchString(m.SenderBIC) || !bicRe.MatchString(m.ReceiverBIC) {
		return "", excerrors.New(CodeSettlementInvalidMessage,
			"MT202 requires valid 8/11-char sender and receiver BICs")
	}
	if m.TransactionRef == "" || len(m.TransactionRef) > 16 {
		return "", excerrors.New(CodeSettlementInvalidMessage,
			fmt.Sprintf("MT202 :20: must be 1..16 chars, got %q", m.TransactionRef))
	}
	if len(m.RelatedRef) > 16 {
		return "", excerrors.New(CodeSettlementInvalidMessage,
			fmt.Sprintf("MT202 :21: must be ≤16 chars, got %q", m.RelatedRef))
	}
	if !currencyRe.MatchString(m.Currency) {
		return "", excerrors.New(CodeSettlementInvalidMessage,
			fmt.Sprintf("MT202 :32A: invalid currency %q", m.Currency))
	}
	if !m.Amount.IsPositive() {
		return "", excerrors.New(CodeSettlementInvalidMessage,
			"MT202 :32A: amount must be positive")
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "{1:F01%s0000000000}", bic11(m.SenderBIC))
	fmt.Fprintf(&sb, "{2:I202%sN}", bic11(m.ReceiverBIC))
	sb.WriteString("{4:\n")
	fmt.Fprintf(&sb, ":20:%s\n", m.TransactionRef)
	if m.RelatedRef != "" {
		fmt.Fprintf(&sb, ":21:%s\n", m.RelatedRef)
	}
	fmt.Fprintf(&sb, ":32A:%s%s%s\n",
		m.ValueDate.Format("060102"), m.Currency, swiftAmount(m.Amount))
	if m.OrderingInstitutionBIC != "" {
		fmt.Fprintf(&sb, ":52A:%s\n", bic11(m.OrderingInstitutionBIC))
	}
	if m.BeneficiaryAccount != "" {
		fmt.Fprintf(&sb, ":58A:/%s\n%s\n", m.BeneficiaryAccount, bic11(m.BeneficiaryBIC))
	} else {
		fmt.Fprintf(&sb, ":58A:%s\n", bic11(m.BeneficiaryBIC))
	}
	sb.WriteString("-}")
	return sb.String(), nil
}

// bic11 normalizes a BIC8 to BIC11 by appending the XXX branch.
func bic11(bic string) string {
	if len(bic) == 8 {
		return bic + "XXX"
	}
	return bic
}

// swiftRef builds a ≤16-char transaction reference from a prefix and id
// (keeps the rightmost digits when the id overflows the field).
func swiftRef(prefix string, id int64) string {
	ref := fmt.Sprintf("%s%013d", prefix, id)
	if len(ref) > 16 {
		ref = ref[len(ref)-16:]
	}
	return ref
}

// swiftAmount renders a SWIFT amount: '.' → ',', never signed.
func swiftAmount(d decimal.Decimal) string {
	return strings.Replace(d.String(), ".", ",", 1)
}

// ---------------------------------------------------------------------------
// ISO 20022 pacs.009 rendering (FinancialInstitutionCreditTransfer)
// ---------------------------------------------------------------------------

// pacs009Message is the field model for one pacs.009 credit transfer.
type pacs009Message struct {
	MsgID       string
	CreatedAt   time.Time
	TxID        string
	EndToEndID  string
	ValueDate   time.Time
	Currency    string
	Amount      decimal.Decimal
	SenderBIC   string
	ReceiverBIC string
}

// marshalPacs009 renders the pacs.009.001.08 XML body.
func marshalPacs009(m pacs009Message) (string, error) {
	if !bicRe.MatchString(m.SenderBIC) || !bicRe.MatchString(m.ReceiverBIC) {
		return "", excerrors.New(CodeSettlementInvalidMessage,
			"pacs.009 requires valid sender/receiver BICs")
	}
	if !currencyRe.MatchString(m.Currency) || !m.Amount.IsPositive() {
		return "", excerrors.New(CodeSettlementInvalidMessage,
			"pacs.009 requires a 3-letter currency and positive amount")
	}
	doc := pacs009Doc{
		Xmlns: "urn:iso:std:iso:20022:tech:xsd:pacs.009.001.08",
	}
	doc.FIToFICdtTrf.GrpHdr.MsgID = m.MsgID
	doc.FIToFICdtTrf.GrpHdr.CreDtTm = m.CreatedAt.Format(time.RFC3339)
	doc.FIToFICdtTrf.GrpHdr.NbOfTxs = "1"
	doc.FIToFICdtTrf.GrpHdr.SttlmInf.SttlmMtd = "CLRG"
	tx := &doc.FIToFICdtTrf.CdtTrfTxInf
	tx.PmtID.InstrID = m.MsgID
	tx.PmtID.EndToEndID = m.EndToEndID
	tx.PmtID.TxID = m.TxID
	tx.IntrBkSttlmAmt.Ccy = m.Currency
	tx.IntrBkSttlmAmt.Value = m.Amount.String()
	tx.IntrBkSttlmDt = m.ValueDate.Format("2006-01-02")
	tx.InstgAgt.FinInstnID.BICFI = bic11(m.SenderBIC)
	tx.InstdAgt.FinInstnID.BICFI = bic11(m.ReceiverBIC)
	tx.Dbtr.FinInstnID.BICFI = bic11(m.SenderBIC)
	tx.Cdtr.FinInstnID.BICFI = bic11(m.ReceiverBIC)

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", excerrors.Wrap(CodeSettlementInvalidMessage,
			"pacs.009 marshal", err)
	}
	return xml.Header + string(body), nil
}

type pacs009FinInstn struct {
	BICFI string `xml:"BICFI"`
}

type pacs009Doc struct {
	XMLName      xml.Name `xml:"Document"`
	Xmlns        string   `xml:"xmlns,attr"`
	FIToFICdtTrf struct {
		GrpHdr struct {
			MsgID    string `xml:"MsgId"`
			CreDtTm  string `xml:"CreDtTm"`
			NbOfTxs  string `xml:"NbOfTxs"`
			SttlmInf struct {
				SttlmMtd string `xml:"SttlmMtd"`
			} `xml:"SttlmInf"`
		} `xml:"GrpHdr"`
		CdtTrfTxInf struct {
			PmtID struct {
				InstrID    string `xml:"InstrId"`
				EndToEndID string `xml:"EndToEndId"`
				TxID       string `xml:"TxId"`
			} `xml:"PmtId"`
			IntrBkSttlmAmt struct {
				Value string `xml:",chardata"`
				Ccy   string `xml:"Ccy,attr"`
			} `xml:"IntrBkSttlmAmt"`
			IntrBkSttlmDt string `xml:"IntrBkSttlmDt"`
			InstgAgt      struct {
				FinInstnID pacs009FinInstn `xml:"FinInstnId"`
			} `xml:"InstgAgt"`
			InstdAgt struct {
				FinInstnID pacs009FinInstn `xml:"FinInstnId"`
			} `xml:"InstdAgt"`
			Dbtr struct {
				FinInstnID pacs009FinInstn `xml:"FinInstnId"`
			} `xml:"Dbtr"`
			Cdtr struct {
				FinInstnID pacs009FinInstn `xml:"FinInstnId"`
			} `xml:"Cdtr"`
		} `xml:"CdtTrfTxInf"`
	} `xml:"FIToFICdtTrf"`
}

// ---------------------------------------------------------------------------
// PgxSettlementStore — PostgreSQL implementation
// ---------------------------------------------------------------------------

// PgxSettlementStore implements SettlementStore over pgx. Numerics cross
// the wire as text (::text read, string param on write) to avoid the
// pgtype-decimal shim — the convention used across this package.
type PgxSettlementStore struct {
	Pool *pgxpool.Pool
}

// NewPgxSettlementStore wires the store.
func NewPgxSettlementStore(pool *pgxpool.Pool) *PgxSettlementStore {
	return &PgxSettlementStore{Pool: pool}
}

// Instrument loads the settlement fields for an instrument.
func (s *PgxSettlementStore) Instrument(ctx context.Context, id int64) (InstrumentRef, error) {
	var r InstrumentRef
	err := s.Pool.QueryRow(ctx, `
		SELECT id, symbol, base_currency, quote_currency, settlement_cycle
		  FROM instruments WHERE id = $1`, id).
		Scan(&r.ID, &r.Symbol, &r.BaseCurrency, &r.QuoteCurrency, &r.SettlementCycle)
	return r, err
}

// ActiveNostroFor resolves the primary (lowest-id) ACTIVE nostro account
// for a currency.
func (s *PgxSettlementStore) ActiveNostroFor(ctx context.Context, currency string) (NostroAccount, error) {
	var n NostroAccount
	err := s.Pool.QueryRow(ctx, `
		SELECT id, currency, bank_name,
		       COALESCE(bank_code, ''), COALESCE(account_number, ''), COALESCE(iban, '')
		  FROM nostro_accounts
		 WHERE currency = $1 AND status = 'ACTIVE'
		 ORDER BY id LIMIT 1`, currency).
		Scan(&n.ID, &n.Currency, &n.BankName, &n.BankCode, &n.AccountNumber, &n.IBAN)
	return n, err
}

// InsertInstructions writes the legs idempotently — ON CONFLICT DO
// NOTHING against settlement_instructions_leg_ux makes fill replays
// no-ops. All four legs insert in one transaction (all-or-nothing).
func (s *PgxSettlementStore) InsertInstructions(ctx context.Context, legs []SettlementInstruction) (int, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inserted := 0
	for _, l := range legs {
		var id int64
		err := tx.QueryRow(ctx, `
			INSERT INTO settlement_instructions
			    (trade_id, account_id, currency, amount, direction,
			     settlement_date, nostro_account_id, status)
			VALUES ($1,$2,$3,$4::numeric,$5,$6,$7,$8)
			ON CONFLICT (trade_id, account_id, currency, direction) DO NOTHING
			RETURNING id`,
			l.TradeID, l.AccountID, l.Currency, l.Amount.String(),
			string(l.Direction), l.SettlementDate, l.NostroAccountID, string(l.Status),
		).Scan(&id)
		if stderrors.Is(err, pgx.ErrNoRows) {
			continue // leg already existed — idempotent replay
		}
		if err != nil {
			return 0, err
		}
		inserted++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return inserted, nil
}

// DueInstructions returns undispatched PENDING legs due on/before day,
// joined with their nostro account (LEFT JOIN — a leg whose nostro row
// vanished surfaces with Nostro.ID==0 and fails the message build
// fail-closed rather than dispatching unaddressed).
func (s *PgxSettlementStore) DueInstructions(ctx context.Context, day time.Time) ([]DueLeg, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT si.id, si.trade_id, si.account_id, si.currency, si.amount::text,
		       si.direction::text, si.settlement_date, si.nostro_account_id,
		       si.status::text, si.swift_message_id, si.created_at, si.settled_at,
		       COALESCE(n.id, 0), COALESCE(n.bank_name, ''), COALESCE(n.bank_code, ''),
		       COALESCE(n.account_number, ''), COALESCE(n.iban, '')
		  FROM settlement_instructions si
		  LEFT JOIN nostro_accounts n ON n.id = si.nostro_account_id
		 WHERE si.status = 'PENDING'
		   AND si.settlement_date <= $1
		   AND si.swift_message_id IS NULL
		 ORDER BY si.settlement_date, si.id`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueLeg
	for rows.Next() {
		var dl DueLeg
		var amt string
		l := &dl.Instruction
		err := rows.Scan(&l.ID, &l.TradeID, &l.AccountID, &l.Currency, &amt,
			(*string)(&l.Direction), &l.SettlementDate, &l.NostroAccountID,
			(*string)(&l.Status), &l.SwiftMessageID, &l.CreatedAt, &l.SettledAt,
			&dl.Nostro.ID, &dl.Nostro.BankName, &dl.Nostro.BankCode,
			&dl.Nostro.AccountNumber, &dl.Nostro.IBAN)
		if err != nil {
			return nil, err
		}
		l.Amount, err = decimal.NewFromString(amt)
		if err != nil {
			return nil, fmt.Errorf("instruction %d amount %q: %w", l.ID, amt, err)
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDispatched claims the leg: payload + message id persist while
// swift_message_id is still NULL (claim-before-send). RowsAffected==0
// means another dispatcher already claimed it.
func (s *PgxSettlementStore) MarkDispatched(ctx context.Context, instructionID int64, messageID string, format MessageFormat, payload string, dispatchedAt time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE settlement_instructions
		   SET swift_message_id = $2, message_format = $3, message_payload = $4,
		       dispatched_at = $5, updated_at = now()
		 WHERE id = $1 AND swift_message_id IS NULL`,
		instructionID, messageID, string(format), payload, dispatchedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// InTx runs fn inside a SERIALIZABLE transaction, rolling back on error.
func (s *PgxSettlementStore) InTx(ctx context.Context, fn func(ctx context.Context, tx SettlementTx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("settlement tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxSettlementTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("settlement tx commit: %w", err)
	}
	return nil
}

type pgxSettlementTx struct{ tx pgx.Tx }

func (t pgxSettlementTx) LockInstruction(ctx context.Context, id int64) (SettlementInstruction, bool, error) {
	var l SettlementInstruction
	var amt string
	var mf *string
	err := t.tx.QueryRow(ctx, `
		SELECT id, trade_id, account_id, currency, amount::text, direction::text,
		       settlement_date, nostro_account_id, status::text,
		       swift_message_id, message_format::text, message_payload,
		       confirmation_ref, created_at, settled_at
		  FROM settlement_instructions WHERE id = $1 FOR UPDATE`, id).
		Scan(&l.ID, &l.TradeID, &l.AccountID, &l.Currency, &amt,
			(*string)(&l.Direction), &l.SettlementDate, &l.NostroAccountID,
			(*string)(&l.Status), &l.SwiftMessageID, &mf,
			&l.MessagePayload, &l.ConfirmationRef, &l.CreatedAt, &l.SettledAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return SettlementInstruction{}, false, nil
	}
	if err != nil {
		return SettlementInstruction{}, false, err
	}
	if mf != nil {
		f := MessageFormat(*mf)
		l.MessageFormat = &f
	}
	l.Amount, err = decimal.NewFromString(amt)
	if err != nil {
		return SettlementInstruction{}, false,
			fmt.Errorf("instruction %d amount %q: %w", l.ID, amt, err)
	}
	return l, true, nil
}

func (t pgxSettlementTx) SetSettled(ctx context.Context, id int64, settledAt time.Time, confirmationRef string) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE settlement_instructions
		   SET status = 'SETTLED', settled_at = $2,
		       confirmation_ref = NULLIF($3, ''), updated_at = now()
		 WHERE id = $1 AND status = 'PENDING'`,
		id, settledAt, confirmationRef)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeSettlementStateConflict, fmt.Sprintf(
			"instruction %d lost PENDING status mid-confirm", id))
	}
	return nil
}

func (t pgxSettlementTx) RecordNostroMovement(ctx context.Context, m NostroMovement) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO nostro_movements
		    (settlement_instruction_id, nostro_account_id, currency, amount,
		     direction, status, confirmation_ref)
		VALUES ($1,$2,$3,$4::numeric,$5,$6,NULLIF($7,''))
		ON CONFLICT (settlement_instruction_id) DO NOTHING`,
		m.SettlementInstructionID, m.NostroAccountID, m.Currency,
		m.Amount.String(), string(m.Direction), string(m.Status), m.ConfirmationRef)
	return err
}
