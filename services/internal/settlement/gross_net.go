// gross_net.go — per-instrument GROSS / NET settlement mode
// (Phase-19 Task 19.3.6; spec §5.1 `settlement_mode`, §17.1, §24 #98).
//
// The instruments.settlement_mode column (migration 031,
// settlement_mode_enum) selects how settlement instructions are emitted:
//
//	GROSS — each trade settles independently: the four per-leg
//	        instructions written by SettlementService.GenerateInstructions
//	        dispatch one SWIFT MT202 / pacs.009 payment per leg.
//	NET   — trades between the same counterparty pair net to a single net
//	        obligation per currency per settlement_date: one payment per
//	        (account, counterparty, currency, value date) instead of one
//	        per trade leg.
//
// Per-trade instruction rows are written for BOTH modes — they are the
// immutable obligation audit trail. The mode is a GROUPING DECISION
// applied at instruction emission (settlement-date dispatch), exactly as
// Task 19.3.6 step 5 requires. Because the NET claim stamps the batch's
// message id onto every constituent leg (swift_message_id, same
// claim-before-send discipline as MarkDispatched), the per-leg
// DispatchDue scan is blind to netted legs — the mode-aware
// GrossNetService.DispatchDue MUST be the dispatch entry point whenever
// NET-mode instruments exist, and it must claim NET batches before any
// per-leg pass runs.
//
// Nostro accounting stays exact under netting: constituent legs keep
// PENDING status after a batch dispatch; when the correspondent confirms
// the net payment, the existing ConfirmSettlement path records each leg's
// gross nostro_movements intent row, whose sum equals the net payment.
// Fully-offset batches (net == 0) settle their legs immediately — no
// payment is owed.
//
// Distinct from spec §5.26 payment_netting_batches (Phase-24 Task 24.3.9):
// that is bilateral ISDA netting between the venue/omnibus and an external
// counterparty; this mode is the per-instrument netting of the exchange's
// own client instruction stream. The two compose; neither replaces the
// other.
//
// Fail-closed (spec §2.7): a missing instrument row or unreadable mode
// errors rather than assuming a mode; a batch whose constituent legs lose
// the PENDING/unclaimed predicate mid-claim aborts the batch atomically.
package settlement

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// SettlementMode mirrors settlement_mode_enum (migration 031, spec §5.1).
type SettlementMode string

const (
	// SettlementGross — every leg dispatches independently (default).
	SettlementGross SettlementMode = "GROSS"
	// SettlementNet — legs net per counterparty pair per settlement date.
	SettlementNet SettlementMode = "NET"
)

// CodeSettlementModeUnknown — scaffold code (Phase-05 Task 5.3.21
// registry): instruments.settlement_mode carried a value outside the enum
// domain — fail-closed, never silently assume GROSS.
const CodeSettlementModeUnknown = "SETTLEMENT_MODE_UNKNOWN"

// NettableLeg is a dispatch-eligible instruction enriched with the
// netting dimensions: its instrument, mode, and the counterparty account
// on the other side of the trade.
type NettableLeg struct {
	Instruction    SettlementInstruction
	InstrumentID   int64
	Symbol         string
	Mode           SettlementMode
	CounterpartyID int64
	Nostro         NostroAccount
}

// NetBatch is one net obligation owed between an account and a
// counterparty for one currency on one settlement date — the single
// payment that replaces its constituent per-trade legs.
type NetBatch struct {
	SettlementDate time.Time
	AccountID      int64 // the account this leg-group settles for
	CounterpartyID int64
	Currency       string
	GrossPay       decimal.Decimal // Σ constituent PAY legs
	GrossReceive   decimal.Decimal // Σ constituent RECEIVE legs
	// NetAmount is signed from AccountID's perspective: positive = the
	// account RECEIVEs |NetAmount|, negative = the account PAYs |NetAmount|,
	// zero = fully offset (no payment owed — legs settle NETTED).
	NetAmount      decimal.Decimal
	Direction      SettlementDirection // "" only when NetAmount is zero
	InstrumentIDs  []int64             // distinct — audit
	InstructionIDs []int64             // constituent settlement_instructions ids
	Nostro         NostroAccount       // currency nostro of the first leg
}

// Ref returns the batch's dispatch reference — "NB" + the smallest
// constituent instruction id (≤16 chars for the SWIFT :20: field).
func (b NetBatch) Ref() string {
	return swiftRef("NB", b.anchorID())
}

func (b NetBatch) anchorID() int64 {
	if len(b.InstructionIDs) == 0 {
		return 0
	}
	m := b.InstructionIDs[0]
	for _, id := range b.InstructionIDs[1:] {
		if id < m {
			m = id
		}
	}
	return m
}

// GroupNetLegs is the pure grouping decision: legs already filtered to
// NET-mode instruments are aggregated per (account, counterparty,
// currency, settlement_date). Output is sorted deterministically by
// (settlement_date, account, counterparty, currency) so replays and tests
// are stable.
func GroupNetLegs(legs []NettableLeg) []NetBatch {
	type key struct {
		day    time.Time
		acct   int64
		cp     int64
		ccy    string
		nostro NostroAccount
	}
	groups := map[key]*NetBatch{}
	var order []key
	for _, l := range legs {
		if l.Mode != SettlementNet {
			continue
		}
		day := normalizeDay(l.Instruction.SettlementDate)
		k := key{day, l.Instruction.AccountID, l.CounterpartyID, l.Instruction.Currency, l.Nostro}
		b := groups[k]
		if b == nil {
			b = &NetBatch{
				SettlementDate: day,
				AccountID:      k.acct,
				CounterpartyID: k.cp,
				Currency:       k.ccy,
				Nostro:         l.Nostro,
			}
			groups[k] = b
			order = append(order, k)
		}
		switch l.Instruction.Direction {
		case DirectionPay:
			b.GrossPay = b.GrossPay.Add(l.Instruction.Amount)
		case DirectionReceive:
			b.GrossReceive = b.GrossReceive.Add(l.Instruction.Amount)
		}
		b.InstructionIDs = append(b.InstructionIDs, l.Instruction.ID)
		found := false
		for _, id := range b.InstrumentIDs {
			if id == l.InstrumentID {
				found = true
				break
			}
		}
		if !found {
			b.InstrumentIDs = append(b.InstrumentIDs, l.InstrumentID)
		}
	}
	out := make([]NetBatch, 0, len(order))
	for _, k := range order {
		b := groups[k]
		b.NetAmount = b.GrossReceive.Sub(b.GrossPay).Round(8)
		switch {
		case b.NetAmount.IsPositive():
			b.Direction = DirectionReceive
		case b.NetAmount.IsNegative():
			b.Direction = DirectionPay
		}
		sort.Slice(b.InstructionIDs, func(i, j int) bool { return b.InstructionIDs[i] < b.InstructionIDs[j] })
		sort.Slice(b.InstrumentIDs, func(i, j int) bool { return b.InstrumentIDs[i] < b.InstrumentIDs[j] })
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, bb := out[i], out[j]
		if !a.SettlementDate.Equal(bb.SettlementDate) {
			return a.SettlementDate.Before(bb.SettlementDate)
		}
		if a.AccountID != bb.AccountID {
			return a.AccountID < bb.AccountID
		}
		if a.CounterpartyID != bb.CounterpartyID {
			return a.CounterpartyID < bb.CounterpartyID
		}
		return a.Currency < bb.Currency
	})
	return out
}

// ---------------------------------------------------------------------------
// Store seam (PositionStore/SettlementStore discipline — pgx production
// implementation below, fakes in tests).
// ---------------------------------------------------------------------------

// GrossNetStore is the persistence seam for mode reads, the enriched
// due-scan, and the atomic batch claim.
type GrossNetStore interface {
	// SettlementModeFor returns instruments.settlement_mode; a missing
	// instrument row errors (fail-closed).
	SettlementModeFor(ctx context.Context, instrumentID int64) (SettlementMode, error)
	// DueLegsEnriched returns undispatched PENDING legs due on/before day
	// joined to their trade (counterparty), instrument (mode) and nostro —
	// the mode-aware equivalent of SettlementStore.DueInstructions.
	DueLegsEnriched(ctx context.Context, day time.Time) ([]NettableLeg, error)
	// InTx runs fn inside a SERIALIZABLE transaction, rolling back on error.
	InTx(ctx context.Context, fn func(ctx context.Context, tx GrossNetTx) error) error
}

// GrossNetTx is the transactional view inside GrossNetStore.InTx.
type GrossNetTx interface {
	// ClaimLegs marks every listed leg claimed by the batch: stamps
	// swift_message_id + format + payload + dispatched_at. When settle is
	// true the legs also flip PENDING→SETTLED (zero-net batches — no
	// payment is owed) and carry confirmation_ref = ref. Returns the number
	// of rows claimed; the caller aborts when it differs from len(ids)
	// (a leg lost the PENDING/unclaimed predicate mid-claim → conflict).
	ClaimLegs(ctx context.Context, ids []int64, ref string, format MessageFormat,
		payload string, at time.Time, settle bool) (int, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// GrossNetOptions configures GrossNetService.
type GrossNetOptions struct {
	// SenderBIC is the exchange's own BIC — required (same contract as
	// SettlementOptions.SenderBIC).
	SenderBIC string
	// Format selects the payload dialect; empty defaults to FormatMT202.
	Format MessageFormat
	// Dispatcher is the rail send seam; nil installs NullDispatcher.
	Dispatcher MessageDispatcher
	// Clock overrides time.Now (tests); nil defaults to time.Now.
	Clock func() time.Time
}

// GrossNetService is the mode-aware settlement-instruction engine: it
// resolves instruments.settlement_mode, groups NET-mode legs into net
// batches, and dispatches one payment per batch (or per leg for GROSS).
type GrossNetService struct {
	store      GrossNetStore
	senderBIC  string
	format     MessageFormat
	dispatcher MessageDispatcher
	clock      func() time.Time
}

// NewGrossNetService wires the service; store and a valid SenderBIC are
// required (fail-closed, same contract as NewSettlementService).
func NewGrossNetService(store GrossNetStore, opts GrossNetOptions) (*GrossNetService, error) {
	if store == nil {
		return nil, fmt.Errorf("gross-net service: nil store")
	}
	if !bicRe.MatchString(opts.SenderBIC) {
		return nil, excerrors.New(CodeSettlementInvalidMessage,
			fmt.Sprintf("gross-net service: invalid sender BIC %q", opts.SenderBIC))
	}
	format := opts.Format
	if format == "" {
		format = FormatMT202
	}
	if format != FormatMT202 && format != FormatPacs009 {
		return nil, excerrors.New(CodeSettlementInvalidMessage,
			fmt.Sprintf("gross-net service: unknown message format %q", format))
	}
	d := opts.Dispatcher
	if d == nil {
		d = &NullDispatcher{}
	}
	clk := opts.Clock
	if clk == nil {
		clk = time.Now
	}
	return &GrossNetService{
		store:      store,
		senderBIC:  opts.SenderBIC,
		format:     format,
		dispatcher: d,
		clock:      clk,
	}, nil
}

// SettlementModeFor resolves the instrument's settlement mode.
func (s *GrossNetService) SettlementModeFor(ctx context.Context, instrumentID int64) (SettlementMode, error) {
	return s.store.SettlementModeFor(ctx, instrumentID)
}

// NetSettlementBatches groups the NET-mode legs due on/before day into
// net obligations per (account, counterparty, currency, settlement_date)
// — the grouping seam Phase-24/reporting callers consume without
// dispatching.
func (s *GrossNetService) NetSettlementBatches(ctx context.Context, day time.Time) ([]NetBatch, error) {
	legs, err := s.store.DueLegsEnriched(ctx, normalizeDay(day))
	if err != nil {
		return nil, fmt.Errorf("gross-net: due scan %s: %w", day.Format("2006-01-02"), err)
	}
	var netLegs []NettableLeg
	for _, l := range legs {
		if l.Mode == SettlementNet {
			netLegs = append(netLegs, l)
		}
	}
	return GroupNetLegs(netLegs), nil
}

// ---------------------------------------------------------------------------
// Mode-aware dispatch
// ---------------------------------------------------------------------------

// BatchError records one batch's failure — per-batch errors are collected,
// never abort the pass (same discipline as InstructionError).
type BatchError struct {
	Batch NetBatch
	Err   error
}

// GrossNetDispatchReport summarises one mode-aware DispatchDue pass.
type GrossNetDispatchReport struct {
	AsOf          time.Time
	Due           int // dispatch-eligible legs
	GrossClaimed  int // GROSS legs claimed + dispatched individually
	Batches       int // NET batches formed
	NetClaimed    int // legs claimed by dispatched NET batches
	Netted        int // legs settled by zero-net batches (no payment owed)
	NetDispatched int // net payments handed to the dispatcher
	Errors        []InstructionError
	BatchErrors   []BatchError
}

// DispatchDue is the mode-aware sibling of SettlementService.DispatchDue:
// every undispatched PENDING instruction due on/before asOf is claimed
// exactly once — GROSS legs dispatch individually (identical contract to
// the per-leg path), NET legs dispatch as one payment per net batch.
//
// Claim-before-send per spec §17.1: the batch's message reference and
// rendered payload persist onto every constituent leg inside one
// SERIALIZABLE transaction BEFORE the dispatcher runs, so a crash
// mid-dispatch leaves recoverable rows, never duplicate payments.
func (s *GrossNetService) DispatchDue(ctx context.Context, asOf time.Time) (*GrossNetDispatchReport, error) {
	day := normalizeDay(asOf)
	rep := &GrossNetDispatchReport{AsOf: day}

	legs, err := s.store.DueLegsEnriched(ctx, day)
	if err != nil {
		return nil, fmt.Errorf("gross-net: due scan %s: %w", day.Format("2006-01-02"), err)
	}
	rep.Due = len(legs)

	var grossLegs, netLegs []NettableLeg
	for _, l := range legs {
		switch l.Mode {
		case SettlementGross:
			grossLegs = append(grossLegs, l)
		case SettlementNet:
			netLegs = append(netLegs, l)
		default:
			// Unknown enum value — fail closed per leg, never assume GROSS.
			rep.Errors = append(rep.Errors, InstructionError{InstructionID: l.Instruction.ID,
				Err: excerrors.New(CodeSettlementModeUnknown, fmt.Sprintf(
					"instruction %d instrument %d has unknown settlement_mode %q",
					l.Instruction.ID, l.InstrumentID, l.Mode))})
		}
	}

	// GROSS legs: per-leg claim + dispatch, mirroring DispatchDue.
	for _, l := range grossLegs {
		leg := l.Instruction
		msg, err := s.buildLegMessage(leg, l.Nostro)
		if err != nil {
			rep.Errors = append(rep.Errors, InstructionError{InstructionID: leg.ID, Err: err})
			continue
		}
		claimed, err := s.claimGrossLeg(ctx, leg.ID, msg)
		if err != nil {
			rep.Errors = append(rep.Errors, InstructionError{InstructionID: leg.ID, Err: err})
			continue
		}
		if !claimed {
			continue // claimed by another dispatcher — never double-send
		}
		if err := s.dispatcher.Dispatch(ctx, msg); err != nil {
			rep.Errors = append(rep.Errors, InstructionError{InstructionID: leg.ID,
				Err: fmt.Errorf("dispatch claimed row: %w", err)})
			continue
		}
		rep.GrossClaimed++
	}

	// NET legs: group → per-batch claim in one tx → single dispatch.
	batches := GroupNetLegs(netLegs)
	rep.Batches = len(batches)
	for _, b := range batches {
		if b.NetAmount.IsZero() {
			// Fully offset obligations settle with no payment.
			n, err := s.claimBatch(ctx, b, "NETTED:FULLY_OFFSET", true)
			if err != nil {
				rep.BatchErrors = append(rep.BatchErrors, BatchError{Batch: b, Err: err})
				continue
			}
			rep.Netted += n
			continue
		}
		msg, err := s.buildBatchMessage(b)
		if err != nil {
			rep.BatchErrors = append(rep.BatchErrors, BatchError{Batch: b, Err: err})
			continue
		}
		n, err := s.claimBatch(ctx, b, msg.Payload, false)
		if err != nil {
			rep.BatchErrors = append(rep.BatchErrors, BatchError{Batch: b, Err: err})
			continue
		}
		if err := s.dispatcher.Dispatch(ctx, msg); err != nil {
			// Legs claimed with payload persisted — recoverable, not lost.
			rep.BatchErrors = append(rep.BatchErrors, BatchError{Batch: b,
				Err: fmt.Errorf("dispatch claimed batch: %w", err)})
			continue
		}
		rep.NetClaimed += n
		rep.NetDispatched++
	}
	return rep, nil
}

// claimBatch marks every constituent leg in one SERIALIZABLE tx — a leg
// that lost the PENDING/unclaimed predicate mid-claim aborts the batch
// (mixed-mode dispatch is a defect; fail closed, ops re-runs).
func (s *GrossNetService) claimBatch(ctx context.Context, b NetBatch, payload string, settle bool) (int, error) {
	var claimed int
	err := s.store.InTx(ctx, func(ctx context.Context, tx GrossNetTx) error {
		var err error
		claimed, err = tx.ClaimLegs(ctx, b.InstructionIDs, b.Ref(), s.format, payload, s.clock().UTC(), settle)
		if err != nil {
			return err
		}
		if claimed != len(b.InstructionIDs) {
			return excerrors.New(CodeSettlementStateConflict, fmt.Sprintf(
				"batch %s claimed %d of %d legs — constituents moved mid-claim",
				b.Ref(), claimed, len(b.InstructionIDs)))
		}
		return nil
	})
	return claimed, err
}

// claimGrossLeg claims one GROSS leg through the same claim-before-send
// predicate as SettlementService (swift_message_id IS NULL).
func (s *GrossNetService) claimGrossLeg(ctx context.Context, id int64, msg OutboundMessage) (bool, error) {
	var claimed bool
	err := s.store.InTx(ctx, func(ctx context.Context, tx GrossNetTx) error {
		n, err := tx.ClaimLegs(ctx, []int64{id}, msg.MessageID, msg.Format, msg.Payload, s.clock().UTC(), false)
		if err != nil {
			return err
		}
		claimed = n == 1
		return nil
	})
	return claimed, err
}

// buildLegMessage renders the per-leg payment — identical semantics to
// SettlementService.buildMessage, kept independent so this file does not
// need the SettlementService type.
func (s *GrossNetService) buildLegMessage(leg SettlementInstruction, nostro NostroAccount) (OutboundMessage, error) {
	return s.renderPayment(leg.ID, leg.TradeID, leg.SettlementDate, leg.Currency, leg.Amount, nostro,
		swiftRef("SI", leg.ID), swiftRef("TRD", leg.TradeID))
}

// buildBatchMessage renders the single net payment for a batch. The :20:
// transaction reference is the batch's "NB…" ref; :21: carries the anchor
// trade reference for reconciliation.
func (s *GrossNetService) buildBatchMessage(b NetBatch) (OutboundMessage, error) {
	if b.Direction == "" || b.NetAmount.IsZero() {
		return OutboundMessage{}, excerrors.New(CodeSettlementInvalidMessage, fmt.Sprintf(
			"batch %s has no payable direction (net=%s)", b.Ref(), b.NetAmount))
	}
	msg, err := s.renderPayment(b.anchorID(), 0, b.SettlementDate, b.Currency, b.NetAmount.Abs(),
		b.Nostro, b.Ref(), swiftRef("NBT", int64(len(b.InstructionIDs))))
	if err != nil {
		return OutboundMessage{}, err
	}
	msg.InstructionID = b.anchorID() // batch anchor — reconciliation key, not a single leg
	return msg, nil
}

// renderPayment is the shared MT202/pacs.009 renderer for legs and
// batches (same field model as SettlementService.buildMessage).
func (s *GrossNetService) renderPayment(anchorID, tradeID int64, valueDate time.Time,
	ccy string, amount decimal.Decimal, nostro NostroAccount, msgRef, relRef string) (OutboundMessage, error) {

	if nostro.ID == 0 || !bicRe.MatchString(nostro.BankCode) {
		return OutboundMessage{}, excerrors.New(CodeSettlementInvalidMessage, fmt.Sprintf(
			"instruction/batch %d: nostro %v lacks a valid BIC", anchorID, nostro.ID))
	}
	msg := OutboundMessage{
		InstructionID: anchorID,
		TradeID:       tradeID,
		Format:        s.format,
		MessageID:     msgRef,
		ValueDate:     valueDate,
		Currency:      ccy,
		Amount:        amount,
		ReceiverBIC:   bic11(nostro.BankCode),
	}
	switch s.format {
	case FormatMT202:
		payload, err := (SwiftMT202{
			SenderBIC:              s.senderBIC,
			ReceiverBIC:            nostro.BankCode,
			TransactionRef:         msgRef,
			RelatedRef:             relRef,
			ValueDate:              valueDate,
			Currency:               ccy,
			Amount:                 amount,
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
			MsgID:       msgRef,
			CreatedAt:   s.clock().UTC(),
			TxID:        swiftRef("TX", anchorID),
			EndToEndID:  relRef,
			ValueDate:   valueDate,
			Currency:    ccy,
			Amount:      amount,
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

// ---------------------------------------------------------------------------
// PgxGrossNetStore — production store over pgxpool.
// ---------------------------------------------------------------------------

// PgxGrossNetStore implements GrossNetStore over pgx.
type PgxGrossNetStore struct {
	Pool *pgxpool.Pool
}

// NewPgxGrossNetStore wires the store.
func NewPgxGrossNetStore(pool *pgxpool.Pool) *PgxGrossNetStore {
	return &PgxGrossNetStore{Pool: pool}
}

// SettlementModeFor reads instruments.settlement_mode; a missing
// instrument errors fail-closed; an empty/NULL mode (schema predating
// migration 031) resolves to GROSS — the always-correct default.
func (s *PgxGrossNetStore) SettlementModeFor(ctx context.Context, instrumentID int64) (SettlementMode, error) {
	var m *string
	err := s.Pool.QueryRow(ctx,
		`SELECT settlement_mode::text FROM instruments WHERE id = $1`,
		instrumentID).Scan(&m)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return "", excerrors.New(CodeSettlementNotFound,
			fmt.Sprintf("instrument %d not found", instrumentID))
	}
	if err != nil {
		return "", fmt.Errorf("gross-net: mode for instrument %d: %w", instrumentID, err)
	}
	if m == nil || *m == "" {
		return SettlementGross, nil
	}
	mode := SettlementMode(*m)
	if mode != SettlementGross && mode != SettlementNet {
		return "", excerrors.New(CodeSettlementModeUnknown, fmt.Sprintf(
			"instrument %d settlement_mode %q", instrumentID, *m))
	}
	return mode, nil
}

// DueLegsEnriched is the mode-aware due scan: PENDING + unclaimed legs due
// on/before day, joined to their trade (counterparty account on the other
// side of the fill) and instrument (settlement_mode). A leg whose trade
// or instrument row vanished fails the whole scan — a silent skip could
// strand an unsettled obligation (fail-closed, spec §2.7).
func (s *PgxGrossNetStore) DueLegsEnriched(ctx context.Context, day time.Time) ([]NettableLeg, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT si.id, si.trade_id, si.account_id, si.currency, si.amount::text,
		       si.direction::text, si.settlement_date, si.nostro_account_id,
		       si.status::text, si.swift_message_id, si.created_at, si.settled_at,
		       i.id, i.symbol, i.settlement_mode::text,
		       CASE WHEN t.buyer_account_id = si.account_id
		            THEN t.seller_account_id ELSE t.buyer_account_id END,
		       COALESCE(n.id, 0), COALESCE(n.bank_name, ''), COALESCE(n.bank_code, ''),
		       COALESCE(n.account_number, ''), COALESCE(n.iban, '')
		  FROM settlement_instructions si
		  JOIN trades      t ON t.id = si.trade_id
		  JOIN instruments i ON i.id = t.instrument_id
		  LEFT JOIN nostro_accounts n ON n.id = si.nostro_account_id
		 WHERE si.status = 'PENDING'
		   AND si.settlement_date <= $1
		   AND si.swift_message_id IS NULL
		 ORDER BY si.settlement_date, si.id`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NettableLeg
	for rows.Next() {
		var l NettableLeg
		var amt, mode string
		ins := &l.Instruction
		err := rows.Scan(&ins.ID, &ins.TradeID, &ins.AccountID, &ins.Currency, &amt,
			(*string)(&ins.Direction), &ins.SettlementDate, &ins.NostroAccountID,
			(*string)(&ins.Status), &ins.SwiftMessageID, &ins.CreatedAt, &ins.SettledAt,
			&l.InstrumentID, &l.Symbol, &mode, &l.CounterpartyID,
			&l.Nostro.ID, &l.Nostro.BankName, &l.Nostro.BankCode,
			&l.Nostro.AccountNumber, &l.Nostro.IBAN)
		if err != nil {
			return nil, err
		}
		ins.Amount, err = decimal.NewFromString(amt)
		if err != nil {
			return nil, fmt.Errorf("instruction %d amount %q: %w", ins.ID, amt, err)
		}
		l.Mode = SettlementMode(mode)
		out = append(out, l)
	}
	return out, rows.Err()
}

// InTx runs fn inside a SERIALIZABLE transaction, rolling back on error.
func (s *PgxGrossNetStore) InTx(ctx context.Context, fn func(ctx context.Context, tx GrossNetTx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("gross-net tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxGrossNetTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("gross-net tx commit: %w", err)
	}
	return nil
}

type pgxGrossNetTx struct{ tx pgx.Tx }

func (t pgxGrossNetTx) ClaimLegs(ctx context.Context, ids []int64, ref string,
	format MessageFormat, payload string, at time.Time, settle bool) (int, error) {
	var tag pgconn.CommandTag
	var err error
	if settle {
		// Zero-net batch: the obligations offset exactly — legs settle in
		// place, carrying the batch ref as the confirmation marker.
		tag, err = t.tx.Exec(ctx, `
			UPDATE settlement_instructions
			   SET swift_message_id = $2, message_format = $3, message_payload = $4,
			       dispatched_at = $5, status = 'SETTLED', settled_at = $5,
			       confirmation_ref = $2, updated_at = now()
			 WHERE id = ANY($1) AND swift_message_id IS NULL AND status = 'PENDING'`,
			ids, ref, string(format), payload, at)
	} else {
		tag, err = t.tx.Exec(ctx, `
			UPDATE settlement_instructions
			   SET swift_message_id = $2, message_format = $3, message_payload = $4,
			       dispatched_at = $5, updated_at = now()
			 WHERE id = ANY($1) AND swift_message_id IS NULL AND status = 'PENDING'`,
			ids, ref, string(format), payload, at)
	}
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
