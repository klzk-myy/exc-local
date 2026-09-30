// Task 24.3.14 — PB credit restitution on settlement failure
// (spec §13.7 extension, §24 #200).
//
// Trigger: the Task 24.3.6 exception workflow (or a CLS rescind) emits
// SETTLEMENT_FAILED — HandleSettlementFailed lands the DSL restitution
// journal (reason SETTLEMENT_FAIL_RESTITUTION), reverses the failed
// trade's NOP impact via the risk.PgPBCreditStore adjuster (the existing
// Phase-19 counters — no parallel credit model), recalculates PB credit
// utilization + margin, notifies the PB via the FIX drop-copy seam and
// raises the admin-dashboard alert.
//
// Guards: one restitution per settlement event (pb_credit_restitutions is
// UNIQUE on settlement_instruction_id — a replay returns the applied row);
// restitution is BLOCKED (row recorded, 409 surfaced) when the trade was
// already replaced or allocated.

package backoffice

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// RestitutionReason is the journal reason tag mandated by Task 24.3.14
// step 2 — mirrors risk.RestitutionReason (kept literal so the backoffice
// package never imports internal/risk: the adjuster seam is structural).
const RestitutionReason = "SETTLEMENT_FAIL_RESTITUTION"

// SettlementFailedEvent is the SETTLEMENT_FAILED trigger payload — one
// row per settlement instruction leg that failed.
type SettlementFailedEvent struct {
	InstructionID  int64           `json:"settlement_instruction_id"`
	TradeID        int64           `json:"trade_id"`
	ClientID       int64           `json:"client_account_id"`
	PrimeBrokerID  int64           `json:"prime_broker_id,omitempty"`
	CurrencyPair   string          `json:"currency_pair,omitempty"` // compact "EURUSD"; "" = global scope
	Currency       string          `json:"currency"`
	Amount         decimal.Decimal `json:"amount"`           // leg amount (DSL/NOP USD-equivalent when USD-leg; callers pass usd values below)
	DSLConsumedUSD decimal.Decimal `json:"dsl_consumed_usd"` // DSL headroom the failed trade consumed
	NOPConsumedUSD decimal.Decimal `json:"nop_consumed_usd"` // signed NOP impact of the failed trade
	DetectedBy     string          `json:"detected_by"`
}

// Restitution is one pb_credit_restitutions row — the correction journal.
type Restitution struct {
	ID            int64
	InstructionID int64
	TradeID       int64
	PrimeBrokerID *int64
	ClientID      int64
	CurrencyPair  string
	DSLCreditUSD  decimal.Decimal
	NOPDeltaUSD   decimal.Decimal
	Reason        string
	Status        string // APPLIED | BLOCKED
	BlockedReason string
	JournalID     *int64
	PBNotified    bool
	MarginQueued  bool
	CreatedAt     time.Time
}

// CreditAdjuster is the PB-credit counter seam — the production binding is
// risk.RestitutionAdjuster (risk/pb_credit_adjuster.go), which asserts the
// raw tx is a pgx.Tx inside this SERIALIZABLE transaction so counters +
// journal + row commit atomically. The tx argument is `any` (the store's
// Querier) so in-memory fakes can satisfy the seam without a pgx handle.
type CreditAdjuster interface {
	RestituteInTx(ctx context.Context, tx any, clientID int64, pair string,
		dslCreditUSD, nopConsumedUSD decimal.Decimal) (rows int, err error)
}

// CreditAdjusterFunc adapts a function to the seam.
type CreditAdjusterFunc func(ctx context.Context, tx any, clientID int64,
	pair string, dslCreditUSD, nopConsumedUSD decimal.Decimal) (int, error)

// RestituteInTx implements CreditAdjuster.
func (f CreditAdjusterFunc) RestituteInTx(ctx context.Context, tx any,
	clientID int64, pair string, dslCreditUSD, nopConsumedUSD decimal.Decimal) (int, error) {
	return f(ctx, tx, clientID, pair, dslCreditUSD, nopConsumedUSD)
}

// RestitutionAlert is the admin-dashboard alert payload (P1 ops tier).
// Self-contained — the sibling OpsAlert/OpsAlerter seam is owned by the
// confirmation/client-money files; this avoids coupling to their shape.
type RestitutionAlert struct {
	Severity string
	Code     string
	Summary  string
	Details  map[string]string
}

// RestitutionAlerter is the admin-dashboard alert seam (NATS ops.alerts.*
// in production; faked in tests).
type RestitutionAlerter interface {
	RaiseRestitutionAlert(ctx context.Context, a RestitutionAlert) error
}

// PBRestitutionNotifier is the FIX drop-copy seam for the real-time PB
// notification (Task 18.3.6 drop copy — production adapter is
// FixPBRestitutionNotifier below).
type PBRestitutionNotifier interface {
	NotifyRestitution(ctx context.Context, pbID, tradeID int64,
		detail map[string]string) error
}

// MarginRecalculator triggers post-restitution margin recalc for the
// affected account (risk margin service binding; nil → flagged only).
type MarginRecalculator interface {
	Recalculate(ctx context.Context, accountID int64) error
}

// RestitutionStore is the persistence seam — the tx runs SERIALIZABLE;
// the callback receives the typed RestitutionTx plus the raw Querier (the
// live pgx.Tx in production) so the credit adjuster joins atomically.
type RestitutionStore interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx RestitutionTx, raw Querier) error) error
	// RestitutionByInstruction returns the recorded restitution (dedupe
	// read); found=false when none.
	RestitutionByInstruction(ctx context.Context, instructionID int64) (Restitution, bool, error)
	// MarkDelivered stamps the post-commit delivery flags.
	MarkDelivered(ctx context.Context, id int64, pbNotified, marginQueued bool) error
}

// RestitutionTx is the transactional view inside InTx.
type RestitutionTx interface {
	RestitutionByInstruction(ctx context.Context, instructionID int64) (Restitution, bool, error)
	// GiveUpForTrade resolves the give-up row (PB + accounts) for the trade.
	GiveUpForTrade(ctx context.Context, tradeID int64) (GiveUpRow, bool, error)
	// TradeReplaced reports whether a superseding settlement leg or a
	// replacement give-up already covers the trade.
	TradeReplaced(ctx context.Context, tradeID, exceptInstructionID int64) (bool, error)
	// TradeAllocated reports whether the trade entered the §5.31
	// allocation workflow (any non-terminal trade_allocations row).
	TradeAllocated(ctx context.Context, tradeID int64) (bool, error)
	// InsertRestitution persists the row (UNIQUE instruction id).
	InsertRestitution(ctx context.Context, r Restitution) (Restitution, error)
	PostJournal(ctx context.Context, entryType, description, postedBy, idemKey string,
		referenceID int64, lines []JournalLine) (int64, error)
}

// RestitutionService consumes SETTLEMENT_FAILED events.
type RestitutionService struct {
	store    RestitutionStore
	adjuster CreditAdjuster
	margin   MarginRecalculator    // optional — nil leaves margin_recalc_queued=false
	notifier PBRestitutionNotifier // optional — nil leaves pb_notified=false
	alerter  RestitutionAlerter    // optional admin-dashboard alert seam
	now      func() time.Time
}

// NewRestitutionService wires the service; store + adjuster are mandatory
// (fail closed — a restitution without the counter correction must never
// record APPLIED).
func NewRestitutionService(store RestitutionStore, adjuster CreditAdjuster) (*RestitutionService, error) {
	if store == nil || adjuster == nil {
		return nil, excerrors.New(CodeServiceDegraded,
			"restitution service requires store and credit adjuster")
	}
	return &RestitutionService{store: store, adjuster: adjuster,
		now: func() time.Time { return time.Now().UTC() }}, nil
}

// WithMargin / WithNotifier / WithAlerter wire the optional seams.
func (s *RestitutionService) WithMargin(m MarginRecalculator) *RestitutionService {
	s.margin = m
	return s
}
func (s *RestitutionService) WithNotifier(n PBRestitutionNotifier) *RestitutionService {
	s.notifier = n
	return s
}
func (s *RestitutionService) WithAlerter(a RestitutionAlerter) *RestitutionService {
	s.alerter = a
	return s
}

// SetClockForTest overrides the clock; tests only.
func (s *RestitutionService) SetClockForTest(now func() time.Time) { s.now = now }

// HandleSettlementFailed processes one SETTLEMENT_FAILED event. Returns
// the restitution row (idempotent replay-safe) — an existing APPLIED row
// returns as-is; a BLOCKED row or a blocked-by-guard event returns
// CodeRestitutionBlocked (409) with the row attached via error detail.
func (s *RestitutionService) HandleSettlementFailed(ctx context.Context,
	ev SettlementFailedEvent) (*Restitution, error) {
	if ev.InstructionID == 0 || ev.TradeID == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"settlement-failed event requires instruction_id and trade_id")
	}
	var rest *Restitution
	var blockedReason string
	err := s.store.InTx(ctx, func(ctx context.Context, rtx RestitutionTx, raw Querier) error {
		if existing, found, err := rtx.RestitutionByInstruction(ctx, ev.InstructionID); err != nil {
			return fmt.Errorf("restitution dedupe read: %w", err)
		} else if found {
			rest = &existing
			return nil // idempotent replay — no second credit
		}
		// Resolve the give-up row for PB + client scope.
		var pbID *int64
		clientID := ev.ClientID
		if g, found, err := rtx.GiveUpForTrade(ctx, ev.TradeID); err != nil {
			return fmt.Errorf("give-up lookup trade %d: %w", ev.TradeID, err)
		} else if found {
			pbID = &g.PrimeBrokerID
			if clientID == 0 {
				clientID = g.ClientAcctID
			}
		}
		if ev.PrimeBrokerID > 0 {
			pbID = &ev.PrimeBrokerID
		}
		if clientID == 0 {
			return excerrors.New("INVALID_REQUEST",
				"restitution requires the PB client account (event or give-up)")
		}
		// Guards: already-replaced or already-allocated trades must not
		// be restituted — the exposure still stands under the new booking.
		replaced, err := rtx.TradeReplaced(ctx, ev.TradeID, ev.InstructionID)
		if err != nil {
			return fmt.Errorf("replaced check trade %d: %w", ev.TradeID, err)
		}
		allocated, err := rtx.TradeAllocated(ctx, ev.TradeID)
		if err != nil {
			return fmt.Errorf("allocated check trade %d: %w", ev.TradeID, err)
		}
		row := Restitution{
			InstructionID: ev.InstructionID, TradeID: ev.TradeID,
			PrimeBrokerID: pbID, ClientID: clientID, CurrencyPair: ev.CurrencyPair,
			DSLCreditUSD: ev.DSLConsumedUSD, NOPDeltaUSD: ev.NOPConsumedUSD,
			Reason: RestitutionReason,
		}
		switch {
		case replaced:
			blockedReason = "trade already replaced"
		case allocated:
			blockedReason = "trade already allocated"
		}
		if blockedReason != "" {
			row.Status, row.BlockedReason = "BLOCKED", blockedReason
			ins, err := rtx.InsertRestitution(ctx, row)
			if err != nil {
				return fmt.Errorf("record blocked restitution: %w", err)
			}
			rest = &ins
			return nil
		}
		// DSL credit-back + NOP reversal — same tx, clamps at zero.
		if _, err := s.adjuster.RestituteInTx(ctx, raw, clientID,
			ev.CurrencyPair, ev.DSLConsumedUSD, ev.NOPConsumedUSD); err != nil {
			return fmt.Errorf("credit restitution client %d: %w", clientID, err)
		}
		// DSL restitution correction journal — zero-net memo pair
		// (1090/2090): a utilization correction, not a cash movement.
		var jid *int64
		if ev.DSLConsumedUSD.IsPositive() {
			id, err := rtx.PostJournal(ctx, "ADJUSTMENT",
				fmt.Sprintf("%s — DSL credit-back instruction %d trade %d",
					RestitutionReason, ev.InstructionID, ev.TradeID),
				"credit-restitution-service",
				fmt.Sprintf("pb-restitute:%d", ev.InstructionID), ev.TradeID,
				[]JournalLine{
					{AccountCode: "1090_SETTLEMENT_FAIL_MEMO_USD",
						Debit: ev.DSLConsumedUSD, Currency: "USD",
						Narrative: RestitutionReason},
					{AccountCode: "2090_PB_CREDIT_MEMO_USD",
						Credit: ev.DSLConsumedUSD, Currency: "USD",
						Narrative: RestitutionReason},
				})
			if err != nil {
				return fmt.Errorf("restitution journal: %w", err)
			}
			jid = &id
		}
		row.Status, row.JournalID = "APPLIED", jid
		ins, err := rtx.InsertRestitution(ctx, row)
		if err != nil {
			return fmt.Errorf("record restitution: %w", err)
		}
		rest = &ins
		return nil
	})
	if err != nil {
		return nil, err
	}
	if rest == nil {
		return nil, excerrors.New(CodeServiceDegraded, "restitution produced no row")
	}
	if rest.Status == "BLOCKED" {
		return rest, excerrors.New(CodeRestitutionBlocked, fmt.Sprintf(
			"restitution blocked for instruction %d: %s",
			rest.InstructionID, rest.BlockedReason))
	}
	if rest.Status != "APPLIED" || rest.PBNotified {
		return rest, nil // replay of an already-delivered restitution
	}
	// Post-commit deliveries (best-effort — failures mark the row so a
	// sweep can retry; the restitution itself is already durable).
	pbNotified, marginQueued := rest.PBNotified, rest.MarginQueued
	if s.margin != nil {
		if err := s.margin.Recalculate(ctx, rest.ClientID); err == nil {
			marginQueued = true
		}
	}
	if s.notifier != nil && rest.PrimeBrokerID != nil {
		detail := map[string]string{
			"reason":         RestitutionReason,
			"instruction":    fmt.Sprintf("%d", rest.InstructionID),
			"dsl_credit_usd": rest.DSLCreditUSD.String(),
			"nop_delta_usd":  rest.NOPDeltaUSD.String(),
		}
		if err := s.notifier.NotifyRestitution(ctx, *rest.PrimeBrokerID,
			rest.TradeID, detail); err == nil {
			pbNotified = true
		}
	}
	if s.alerter != nil {
		_ = s.alerter.RaiseRestitutionAlert(ctx, RestitutionAlert{
			Severity: "P1", Code: CodeSettlementFailed,
			Summary: fmt.Sprintf("PB credit restitution applied — trade %d client %d DSL +%s USD",
				rest.TradeID, rest.ClientID, rest.DSLCreditUSD),
			Details: map[string]string{
				"instruction_id": fmt.Sprintf("%d", rest.InstructionID),
				"restitution_id": fmt.Sprintf("%d", rest.ID),
			},
		})
	}
	if pbNotified != rest.PBNotified || marginQueued != rest.MarginQueued {
		if err := s.store.MarkDelivered(ctx, rest.ID, pbNotified, marginQueued); err == nil {
			rest.PBNotified, rest.MarginQueued = pbNotified, marginQueued
		}
	}
	return rest, nil
}

// ---------------------------------------------------------------------------
// PgxRestitutionStore — production impl
// ---------------------------------------------------------------------------

// PgxRestitutionStore implements RestitutionStore.
type PgxRestitutionStore struct{ Pool *pgxpool.Pool }

// NewPgxRestitutionStore binds the store to a pool.
func NewPgxRestitutionStore(pool *pgxpool.Pool) *PgxRestitutionStore {
	return &PgxRestitutionStore{Pool: pool}
}

func (s *PgxRestitutionStore) InTx(ctx context.Context,
	fn func(ctx context.Context, tx RestitutionTx, raw Querier) error) error {
	if s.Pool == nil {
		return excerrors.New(CodeServiceDegraded, "restitution store not configured")
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("restitution: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxRestitutionTx{q: tx}, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const restitutionCols = `id, settlement_instruction_id, trade_id, prime_broker_id,
	client_account_id, COALESCE(currency_pair,''), dsl_credit_usd::text,
	nop_delta_usd::text, reason, status, COALESCE(blocked_reason,''),
	journal_entry_id, pb_notified, margin_recalc_queued, created_at`

func scanRestitution(row interface{ Scan(...any) error }) (Restitution, error) {
	var r Restitution
	var dsl, nop string
	err := row.Scan(&r.ID, &r.InstructionID, &r.TradeID, &r.PrimeBrokerID,
		&r.ClientID, &r.CurrencyPair, &dsl, &nop, &r.Reason, &r.Status,
		&r.BlockedReason, &r.JournalID, &r.PBNotified, &r.MarginQueued,
		&r.CreatedAt)
	if err != nil {
		return r, err
	}
	if r.DSLCreditUSD, err = decimal.NewFromString(dsl); err != nil {
		return r, err
	}
	r.NOPDeltaUSD, err = decimal.NewFromString(nop)
	return r, err
}

func (s *PgxRestitutionStore) RestitutionByInstruction(ctx context.Context,
	instructionID int64) (Restitution, bool, error) {
	r, err := scanRestitution(s.Pool.QueryRow(ctx,
		`SELECT `+restitutionCols+` FROM pb_credit_restitutions
		 WHERE settlement_instruction_id=$1`, instructionID))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return Restitution{}, false, nil
	}
	return r, err == nil, err
}

func (s *PgxRestitutionStore) MarkDelivered(ctx context.Context, id int64,
	pbNotified, marginQueued bool) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE pb_credit_restitutions
		   SET pb_notified=$2, margin_recalc_queued=$3
		 WHERE id=$1`, id, pbNotified, marginQueued)
	return err
}

// pgxRestitutionTx is the tx view.
type pgxRestitutionTx struct{ q Querier }

func (t pgxRestitutionTx) RestitutionByInstruction(ctx context.Context,
	instructionID int64) (Restitution, bool, error) {
	r, err := scanRestitution(t.q.QueryRow(ctx,
		`SELECT `+restitutionCols+` FROM pb_credit_restitutions
		 WHERE settlement_instruction_id=$1 FOR UPDATE`, instructionID))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return Restitution{}, false, nil
	}
	return r, err == nil, err
}

func (t pgxRestitutionTx) GiveUpForTrade(ctx context.Context, tradeID int64) (GiveUpRow, bool, error) {
	g, err := scanGiveUp(t.q.QueryRow(ctx,
		`SELECT `+giveupCols+` FROM pb_giveup_trades
		 WHERE trade_id=$1 ORDER BY id LIMIT 1`, tradeID))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return GiveUpRow{}, false, nil
	}
	return g, err == nil, err
}

func (t pgxRestitutionTx) TradeReplaced(ctx context.Context, tradeID,
	exceptInstructionID int64) (bool, error) {
	var replaced bool
	// A replacement is either a superseding settlement leg for the same
	// trade+currency (PENDING/SETTLED — the fail was re-booked), a
	// REPLACEMENT-kind allocation, or a second give-up to another PB.
	err := t.q.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM settlement_instructions s
		     WHERE s.trade_id=$1 AND s.id <> $2 AND s.status::text IN ('PENDING','SETTLED','RECONCILED'))
		   OR EXISTS (
		    SELECT 1 FROM trade_allocations a
		     WHERE a.trade_id=$1 AND a.kind='REPLACEMENT'
		       AND a.status::text NOT IN ('REJECTED','CANCELLED','CORRECTED'))
		   OR EXISTS (
		    SELECT 1 FROM pb_giveup_trades g
		     WHERE g.trade_id=$1 AND g.giveup_status::text IN ('PENDING','AFFIRMED','SETTLED')
		       AND g.id <> ALL (SELECT g2.id FROM pb_giveup_trades g2
		                         WHERE g2.trade_id=$1 AND g2.giveup_status::text IN
		                             ('REJECTED','DISPUTED')))`,
		tradeID, exceptInstructionID).Scan(&replaced)
	return replaced, err
}

func (t pgxRestitutionTx) TradeAllocated(ctx context.Context, tradeID int64) (bool, error) {
	var allocated bool
	err := t.q.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM trade_allocations
		     WHERE trade_id=$1
		       AND status::text IN ('ALLOCATED','CLAIMED','SETTLED'))`, tradeID).Scan(&allocated)
	return allocated, err
}

func (t pgxRestitutionTx) InsertRestitution(ctx context.Context, r Restitution) (Restitution, error) {
	var pair any
	if r.CurrencyPair != "" {
		pair = r.CurrencyPair
	}
	row := t.q.QueryRow(ctx, `
		INSERT INTO pb_credit_restitutions
		    (settlement_instruction_id, trade_id, prime_broker_id, client_account_id,
		     currency_pair, dsl_credit_usd, nop_delta_usd, reason, status,
		     blocked_reason, journal_entry_id)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8,$9,NULLIF($10,''),$11)
		ON CONFLICT (settlement_instruction_id) DO NOTHING
		RETURNING `+restitutionCols,
		r.InstructionID, r.TradeID, r.PrimeBrokerID, r.ClientID, pair,
		r.DSLCreditUSD.String(), r.NOPDeltaUSD.String(), r.Reason, r.Status,
		r.BlockedReason, r.JournalID)
	out, err := scanRestitution(row)
	if stderrors.Is(err, pgx.ErrNoRows) {
		// Concurrent insert won the race — return the winner's row.
		winner, found, err2 := t.RestitutionByInstruction(ctx, r.InstructionID)
		if err2 != nil {
			return Restitution{}, err2
		}
		if !found {
			return Restitution{}, excerrors.New(CodeServiceDegraded,
				"restitution insert conflicted but row not visible")
		}
		return winner, nil
	}
	return out, err
}

func (t pgxRestitutionTx) PostJournal(ctx context.Context, entryType, description,
	postedBy, idemKey string, referenceID int64, lines []JournalLine) (int64, error) {
	return pgxExceptionTx{q: t.q}.PostJournal(ctx, entryType, description,
		postedBy, idemKey, referenceID, lines)
}
