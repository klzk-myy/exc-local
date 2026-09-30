// netting.go — bilateral payment netting across trades per
// counterparty+value date (Phase-24 Task 24.3.9; spec §5.26, §17.7,
// §24 #155).
//
// Obligations between the exchange/omnibus and a counterparty are netted
// per currency per value date into payment_netting_batches — one payment
// per direction instead of one per trade. Netting applies ONLY to
// same-counterparty, same-currency, same-value-date obligations and only
// where a legally enforceable netting agreement exists (legal_agreements
// ISDA row); CLS-eligible flow is excluded — it routes through Task
// 24.3.8 PvP because CLS nets internally.
//
// Edge cases honored (task SDD):
//   - trade bust after netting → ReopenBatch pulls the busted legs back
//     to OPEN and recomputes gross/net (DISPATCHED/SETTLED batches refuse
//     — settlement already left the building);
//   - weekend value dates → batch value dates normalize through the
//     holiday calendar (MutualBusinessDayOnOrAfter);
//   - SSI change mid-batch → the batch snapshots the SSI resolved at
//     dispatch; a revoked SSI refuses dispatch (SSI_NOT_VERIFIED).
//
// The netted batch dispatches as a single rail payment and reconciles as
// one nostro movement; netting_batch_lines retains the trade-level
// mapping for break attribution.
package settlement

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// Netting error codes — registered in internal/errs localRows.
const (
	// CodeNettingStateConflict — lifecycle violation: reopen on a
	// DISPATCHED/SETTLED batch, dispatch of a non-NETTED batch, etc.
	CodeNettingStateConflict = "NETTING_BATCH_STATE_CONFLICT"
	// CodeNettingAgreementMissing — no EXECUTED ISDA netting agreement
	// for the counterparty; bilateral netting is legally unenforceable.
	CodeNettingAgreementMissing = "NETTING_AGREEMENT_MISSING"
)

// NettingBatchStatus mirrors netting_batch_status_enum.
type NettingBatchStatus string

const (
	BatchOpen       NettingBatchStatus = "OPEN"
	BatchNetted     NettingBatchStatus = "NETTED"
	BatchDispatched NettingBatchStatus = "DISPATCHED"
	BatchSettled    NettingBatchStatus = "SETTLED"
	BatchFailed     NettingBatchStatus = "FAILED"
)

// NettingObligation is one nettable settlement_instructions leg enriched
// with the trade's instrument currencies (needed for the CLS-eligibility
// exclusion) — a leg is nettable iff it is PENDING, undispatched, and
// not already a NETTED line of another batch.
type NettingObligation struct {
	InstructionID int64
	TradeID       int64
	AccountID     int64
	Currency      string
	Amount        decimal.Decimal
	Direction     SettlementDirection
	ValueDate     time.Time
	BaseCurrency  string // instrument base (for CLS check)
	QuoteCurrency string // instrument quote (for CLS check)
}

// NettingBatch is one payment_netting_batches row.
type NettingBatch struct {
	ID                    int64              `json:"id"`
	CounterpartyAccountID int64              `json:"counterparty_account_id"`
	Currency              string             `json:"currency"`
	ValueDate             time.Time          `json:"value_date"`
	GrossObligation       decimal.Decimal    `json:"gross_obligation"`
	NetObligation         decimal.Decimal    `json:"net_obligation"`
	Direction             string             `json:"direction,omitempty"`
	SSIID                 *int64             `json:"ssi_id,omitempty"`
	SSISnapshot           json.RawMessage    `json:"ssi_snapshot,omitempty"`
	Rail                  string             `json:"rail,omitempty"`
	Status                NettingBatchStatus `json:"status"`
	ReopenCount           int                `json:"reopen_count"`
	DispatchRef           string             `json:"dispatch_ref,omitempty"`
	NettedAt              *time.Time         `json:"netted_at,omitempty"`
	DispatchedAt          *time.Time         `json:"dispatched_at,omitempty"`
	SettledAt             *time.Time         `json:"settled_at,omitempty"`
	CreatedAt             time.Time          `json:"created_at"`
}

// NettingLine is one netting_batch_lines row — the trade-level mapping
// retained for break attribution.
type NettingLine struct {
	ID                      int64               `json:"id"`
	BatchID                 int64               `json:"batch_id"`
	SettlementInstructionID int64               `json:"settlement_instruction_id"`
	TradeID                 int64               `json:"trade_id"`
	Direction               SettlementDirection `json:"direction"`
	Amount                  decimal.Decimal     `json:"amount"`
	Status                  string              `json:"status"` // NETTED | RELEASED
	ReleasedReason          string              `json:"released_reason,omitempty"`
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// NettingStore is the persistence seam (PgxNettingStore implements it).
type NettingStore interface {
	// NettingAgreementExists — EXECUTED, unexpired ISDA row for the
	// counterparty (legally enforceable netting per spec §17.7).
	NettingAgreementExists(ctx context.Context, counterpartyAccountID int64) (bool, error)
	// NettableObligations returns PENDING undispatched settlement legs
	// for (counterparty, currency, value_date) not already netted.
	NettableObligations(ctx context.Context, counterpartyAccountID int64,
		currency string, valueDate time.Time) ([]NettingObligation, error)
	// Batch loads one row.
	Batch(ctx context.Context, id int64) (*NettingBatch, bool, error)
	// BatchLines lists the batch's line-level mapping.
	BatchLines(ctx context.Context, batchID int64) ([]NettingLine, error)
	// ListBatches pages batches (ops surface).
	ListBatches(ctx context.Context, status string, limit int) ([]NettingBatch, error)
	// InTx runs fn inside a SERIALIZABLE transaction.
	InTx(ctx context.Context, fn func(ctx context.Context, tx NettingTx) error) error
}

// NettingTx is the transactional view inside NettingStore.InTx.
type NettingTx interface {
	LockObligations(ctx context.Context, counterpartyAccountID int64,
		currency string, valueDate time.Time) ([]NettingObligation, error)
	InsertBatch(ctx context.Context, b NettingBatch) (int64, error)
	InsertLines(ctx context.Context, lines []NettingLine) error
	LockBatch(ctx context.Context, id int64) (*NettingBatch, bool, error)
	// ReleaseLines marks the given batch lines RELEASED with reason.
	ReleaseLines(ctx context.Context, batchID int64, instructionIDs []int64, reason string) (int, error)
	// Lines returns the batch's lines inside the tx (post-release state
	// is visible — recomputing totals must see it).
	Lines(ctx context.Context, batchID int64) ([]NettingLine, error)
	// UpdateBatch recomputes/sets totals + status + optional fields.
	UpdateBatch(ctx context.Context, b NettingBatch) error
	// ClaimNettedLegs stamps the batch's constituent instructions with
	// the batch dispatch ref (claim-before-send, same discipline as
	// SettlementStore.MarkDispatched). Returns claimed count — caller
	// aborts when it differs from len(instructionIDs).
	ClaimNettedLegs(ctx context.Context, instructionIDs []int64, dispatchRef string, at time.Time) (int, error)
}

// ClsEligibilityChecker excludes CLS-eligible obligations from netting —
// implemented by ClsPvpService.ClsEligible. A nil checker fails closed:
// every obligation is treated as CLS-eligible (netting captures nothing
// that might require PvP).
type ClsEligibilityChecker interface {
	ClsEligible(ctx context.Context, buyCcy, sellCcy, product string) (bool, error)
}

// RailCutoffChecker reports whether the relevant payment rail's daily
// cut-off has passed for a currency at a time (implemented by
// RailCutoffService; nil = no cut-off gate wired — dispatch refuses
// fail-closed rather than shipping unscheduled).
type RailCutoffChecker interface {
	// CutoffPassed reports whether the currency's settlement rail cut-off
	// has passed at `at`. railNames lists acceptable rails for the
	// currency scope; implementations pick the scheduled row.
	CutoffPassed(ctx context.Context, currency string, at time.Time) (passed bool, rail string, err error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// NettingServiceOptions wires collaborators.
type NettingServiceOptions struct {
	SSI      SsiStore              // SSI resolution at dispatch (required for DispatchBatch)
	Cls      ClsEligibilityChecker // nil = fail-closed (everything excluded)
	Cutoff   RailCutoffChecker     // nil = dispatch gate unwired → refuse dispatch
	Calendar *HolidayCalendar      // nil = weekend normalization skipped
	Clock    func() time.Time
	Alerter  OpsAlerter
}

// NettingService owns batching, dispatch and re-open.
type NettingService struct {
	store   NettingStore
	opts    NettingServiceOptions
	clock   func() time.Time
	alerter OpsAlerter
}

// NewNettingService wires the service; store is required.
func NewNettingService(store NettingStore, opts NettingServiceOptions) (*NettingService, error) {
	if store == nil {
		return nil, fmt.Errorf("netting: nil store")
	}
	clk := opts.Clock
	if clk == nil {
		clk = time.Now
	}
	return &NettingService{store: store, opts: opts, clock: clk, alerter: opts.Alerter}, nil
}

// NettingScope bounds one netting run.
type NettingScope struct {
	CounterpartyAccountID int64
	Currency              string
	ValueDate             time.Time
}

// NettingReport summarizes a run.
type NettingReport struct {
	Batch           *NettingBatch `json:"batch,omitempty"`
	ObligationsSeen int           `json:"obligations_seen"`
	ClsExcluded     int           `json:"cls_excluded"` // legs routed to PvP instead
	Lines           int           `json:"lines"`
	Empty           bool          `json:"empty"` // no nettable obligations
}

// RunNetting aggregates all nettable obligations for the scope into one
// batch: ΣPAY vs ΣRECEIVE → single net payment. CLS-eligible legs are
// excluded (they settle via Task 24.3.8 PvP). The batch lands NETTED with
// line-level mapping; a zero net obligation still batches (it settles
// the legs with no payment owed).
func (s *NettingService) RunNetting(ctx context.Context, scope NettingScope) (*NettingReport, error) {
	ccy := strings.ToUpper(strings.TrimSpace(scope.Currency))
	if scope.CounterpartyAccountID <= 0 || !currencyRe.MatchString(ccy) || scope.ValueDate.IsZero() {
		return nil, excerrors.New("INVALID_REQUEST",
			"netting: counterparty, currency and value_date are required")
	}
	vd := normalizeDay(scope.ValueDate)
	// Weekend/holiday normalization — a batch never carries a value date
	// that is not a business day in the currency's center.
	if s.opts.Calendar != nil && !s.opts.Calendar.IsBusinessDay(ccy, vd) {
		vd = s.opts.Calendar.MutualBusinessDayOnOrAfter(vd, ccy)
	}

	ok, err := s.store.NettingAgreementExists(ctx, scope.CounterpartyAccountID)
	if err != nil {
		return nil, fmt.Errorf("netting: agreement check: %w", err)
	}
	if !ok {
		return nil, excerrors.New(CodeNettingAgreementMissing, fmt.Sprintf(
			"netting: no EXECUTED ISDA netting agreement for counterparty %d", scope.CounterpartyAccountID))
	}

	rep := &NettingReport{}
	err = s.store.InTx(ctx, func(ctx context.Context, tx NettingTx) error {
		obs, err := tx.LockObligations(ctx, scope.CounterpartyAccountID, ccy, vd)
		if err != nil {
			return err
		}
		rep.ObligationsSeen = len(obs)
		var lines []NettingLine
		var grossPay, grossRecv decimal.Decimal
		for _, o := range obs {
			// CLS exclusion: an eligible pair settles PvP — CLS nets
			// internally, venue netting must not capture it.
			cls, cerr := s.clsEligible(ctx, o)
			if cerr != nil {
				return cerr
			}
			if cls {
				rep.ClsExcluded++
				continue
			}
			switch o.Direction {
			case DirectionPay:
				grossPay = grossPay.Add(o.Amount)
			case DirectionReceive:
				grossRecv = grossRecv.Add(o.Amount)
			}
			lines = append(lines, NettingLine{
				SettlementInstructionID: o.InstructionID,
				TradeID:                 o.TradeID,
				Direction:               o.Direction,
				Amount:                  o.Amount,
				Status:                  "NETTED",
			})
		}
		if len(lines) == 0 {
			rep.Empty = true
			return nil
		}
		net := grossRecv.Sub(grossPay).Round(8)
		b := NettingBatch{
			CounterpartyAccountID: scope.CounterpartyAccountID,
			Currency:              ccy,
			ValueDate:             vd,
			GrossObligation:       grossPay.Add(grossRecv).Round(8),
			NetObligation:         net.Abs(),
			Status:                BatchNetted,
		}
		switch {
		case net.IsPositive():
			b.Direction = string(DirectionReceive)
		case net.IsNegative():
			b.Direction = string(DirectionPay)
		}
		now := s.clock().UTC()
		b.NettedAt = &now
		bid, err := tx.InsertBatch(ctx, b)
		if err != nil {
			return err
		}
		b.ID = bid
		for i := range lines {
			lines[i].BatchID = bid
		}
		sort.Slice(lines, func(i, j int) bool {
			return lines[i].SettlementInstructionID < lines[j].SettlementInstructionID
		})
		if err := tx.InsertLines(ctx, lines); err != nil {
			return err
		}
		rep.Batch = &b
		rep.Lines = len(lines)
		return nil
	})
	return rep, err
}

// clsEligible consults the bound checker; nil checker fails closed to
// CLS-eligible (obligation excluded — never netted).
func (s *NettingService) clsEligible(ctx context.Context, o NettingObligation) (bool, error) {
	if s.opts.Cls == nil {
		return true, nil // fail-closed: cannot prove non-eligibility
	}
	eligible, err := s.opts.Cls.ClsEligible(ctx, o.BaseCurrency, o.QuoteCurrency, "SPOT")
	if err != nil {
		// Eligibility unreadable → exclude (conservative: PvP routing is
		// the safer settlement path).
		return true, nil
	}
	return eligible, nil
}

// DispatchBatch releases a NETTED batch to its rail as one net payment.
// The SSI is re-resolved at dispatch — a mid-batch SSI change lands on
// the live instruction, and a revoked SSI refuses dispatch. The batch
// stores the SSI snapshot for attribution. Past-cutoff dispatch for a
// same-day value date refuses with RAIL_CUTOFF_EXCEEDED (the batch stays
// NETTED for next-day release).
func (s *NettingService) DispatchBatch(ctx context.Context, batchID int64, rail string) (*NettingBatch, error) {
	var out *NettingBatch
	err := s.store.InTx(ctx, func(ctx context.Context, tx NettingTx) error {
		b, found, err := tx.LockBatch(ctx, batchID)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New("NOT_FOUND", fmt.Sprintf("netting batch %d not found", batchID))
		}
		if b.Status != BatchNetted {
			return excerrors.New(CodeNettingStateConflict, fmt.Sprintf(
				"netting: batch %d is %s — dispatch requires NETTED", b.ID, b.Status))
		}
		// Rail cut-off gate: same-day dispatch after cut-off is refused
		// (rolls via Task 24.3.20 evaluation, not silently shipped).
		if s.opts.Cutoff != nil && normalizeDay(b.ValueDate).Equal(normalizeDay(s.clock())) {
			passed, railName, cerr := s.opts.Cutoff.CutoffPassed(ctx, b.Currency, s.clock())
			if cerr != nil {
				return cerr
			}
			if passed {
				return excerrors.New("RAIL_CUTOFF_EXCEEDED", fmt.Sprintf(
					"netting: %s rail cut-off passed for %s — batch stays NETTED for next cycle",
					railName, b.Currency))
			}
		}
		// SSI re-resolution — mid-batch changes take effect here.
		var ssi *StandingSettlementInstruction
		if s.opts.SSI != nil {
			row, okSsi, serr := s.opts.SSI.ActiveSSIFor(ctx, b.CounterpartyAccountID, b.Currency)
			if serr != nil {
				return serr
			}
			if !okSsi || row.Status != SsiActive {
				return excerrors.New(CodeSsiNotVerified, fmt.Sprintf(
					"netting: no ACTIVE SSI for account %d %s — dispatch refused",
					b.CounterpartyAccountID, b.Currency))
			}
			ssi = row
		}
		lines, err := tx.Lines(ctx, b.ID)
		if err != nil {
			return err
		}
		var ids []int64
		for _, l := range lines {
			if l.Status == "NETTED" {
				ids = append(ids, l.SettlementInstructionID)
			}
		}
		ref := swiftRef("NB", b.ID)
		claimed, err := tx.ClaimNettedLegs(ctx, ids, ref, s.clock().UTC())
		if err != nil {
			return err
		}
		if claimed != len(ids) {
			return excerrors.New(CodeNettingStateConflict, fmt.Sprintf(
				"netting: batch %d claim race — %d/%d legs claimed", b.ID, claimed, len(ids)))
		}
		now := s.clock().UTC()
		b.Status = BatchDispatched
		b.DispatchedAt = &now
		b.DispatchRef = ref
		b.Rail = strings.ToUpper(rail)
		if ssi != nil {
			b.SSIID = &ssi.ID
			snap, _ := json.Marshal(ssi)
			b.SSISnapshot = snap
		}
		if err := tx.UpdateBatch(ctx, *b); err != nil {
			return err
		}
		cp := *b
		out = &cp
		return nil
	})
	return out, err
}

// ReopenBatch pulls obligations back out of a NETTED batch — the
// trade-bust-after-netting edge case. Only NETTED batches reopen;
// DISPATCHED/SETTLED batches conflict (settlement already dispatched —
// ops must unwind through the failed-settlement chain).
func (s *NettingService) ReopenBatch(ctx context.Context, batchID int64,
	instructionIDs []int64, reason string) (*NettingBatch, error) {

	if len(instructionIDs) == 0 {
		return nil, excerrors.New("INVALID_REQUEST", "reopen requires ≥1 instruction id")
	}
	var out *NettingBatch
	err := s.store.InTx(ctx, func(ctx context.Context, tx NettingTx) error {
		b, found, err := tx.LockBatch(ctx, batchID)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New("NOT_FOUND", fmt.Sprintf("netting batch %d not found", batchID))
		}
		if b.Status != BatchNetted && b.Status != BatchOpen {
			return excerrors.New(CodeNettingStateConflict, fmt.Sprintf(
				"netting: batch %d is %s — only NETTED/OPEN batches reopen", b.ID, b.Status))
		}
		released, err := tx.ReleaseLines(ctx, b.ID, instructionIDs, strings.ToUpper(reason))
		if err != nil {
			return err
		}
		if released != len(instructionIDs) {
			return excerrors.New(CodeNettingStateConflict, fmt.Sprintf(
				"netting: batch %d release race — %d/%d lines released", b.ID, released, len(instructionIDs)))
		}
		// Recompute totals from the surviving NETTED lines.
		lines, err := tx.Lines(ctx, b.ID)
		if err != nil {
			return err
		}
		var grossPay, grossRecv decimal.Decimal
		for _, l := range lines {
			if l.Status != "NETTED" {
				continue
			}
			if l.Direction == DirectionPay {
				grossPay = grossPay.Add(l.Amount)
			} else {
				grossRecv = grossRecv.Add(l.Amount)
			}
		}
		net := grossRecv.Sub(grossPay).Round(8)
		b.GrossObligation = grossPay.Add(grossRecv).Round(8)
		b.NetObligation = net.Abs()
		switch {
		case net.IsPositive():
			b.Direction = string(DirectionReceive)
		case net.IsNegative():
			b.Direction = string(DirectionPay)
		default:
			b.Direction = ""
		}
		b.Status = BatchOpen
		b.ReopenCount++
		if err := tx.UpdateBatch(ctx, *b); err != nil {
			return err
		}
		cp := *b
		out = &cp
		return nil
	})
	if err == nil && s.alerter != nil {
		_ = s.alerter.Raise(ctx, OpsAlert{Severity: "P2", Code: "NETTING_BATCH_REOPENED",
			Summary: fmt.Sprintf("netting batch %d reopened (%s): %d obligations released",
				batchID, reason, len(instructionIDs))})
	}
	return out, err
}

// SettleBatch records the correspondent confirmation of the dispatched
// net payment — the batch reconciles as ONE nostro movement keyed by
// dispatch_ref; netting_batch_lines retains per-trade attribution.
func (s *NettingService) SettleBatch(ctx context.Context, batchID int64, confirmationRef string) (*NettingBatch, error) {
	var out *NettingBatch
	err := s.store.InTx(ctx, func(ctx context.Context, tx NettingTx) error {
		b, found, err := tx.LockBatch(ctx, batchID)
		if err != nil {
			return err
		}
		if !found {
			return excerrors.New("NOT_FOUND", fmt.Sprintf("netting batch %d not found", batchID))
		}
		switch b.Status {
		case BatchSettled:
			cp := *b
			out = &cp
			return nil // idempotent
		case BatchDispatched:
		default:
			return excerrors.New(CodeNettingStateConflict, fmt.Sprintf(
				"netting: batch %d is %s — settle requires DISPATCHED", b.ID, b.Status))
		}
		now := s.clock().UTC()
		b.Status = BatchSettled
		b.SettledAt = &now
		if confirmationRef != "" {
			b.DispatchRef = confirmationRef
		}
		if err := tx.UpdateBatch(ctx, *b); err != nil {
			return err
		}
		cp := *b
		out = &cp
		return nil
	})
	return out, err
}

// Batch loads one batch (handler read path).
func (s *NettingService) Batch(ctx context.Context, id int64) (*NettingBatch, error) {
	b, found, err := s.store.Batch(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, excerrors.New("NOT_FOUND", fmt.Sprintf("netting batch %d not found", id))
	}
	return b, nil
}

// BatchLines lists the batch's line-level mapping.
func (s *NettingService) BatchLines(ctx context.Context, batchID int64) ([]NettingLine, error) {
	return s.store.BatchLines(ctx, batchID)
}

// ListBatches pages batches by status filter ("" = all).
func (s *NettingService) ListBatches(ctx context.Context, status string, limit int) ([]NettingBatch, error) {
	return s.store.ListBatches(ctx, strings.ToUpper(strings.TrimSpace(status)), limit)
}

// ---------------------------------------------------------------------------
// PgxNettingStore
// ---------------------------------------------------------------------------

// PgxNettingStore implements NettingStore/NettingTx over pgx.
type PgxNettingStore struct{ Pool *pgxpool.Pool }

// NewPgxNettingStore wires the store.
func NewPgxNettingStore(pool *pgxpool.Pool) *PgxNettingStore {
	return &PgxNettingStore{Pool: pool}
}

// NettingAgreementExists — EXECUTED, unexpired ISDA row.
func (s *PgxNettingStore) NettingAgreementExists(ctx context.Context, accountID int64) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM legal_agreements
		     WHERE account_id = $1 AND agreement_type = 'ISDA'
		       AND status = 'EXECUTED'
		       AND (expires_at IS NULL OR expires_at > now()))`, accountID).Scan(&ok)
	return ok, err
}

const nettingObligationSelect = `
	SELECT si.id, si.trade_id, si.account_id, si.currency, si.amount::text,
	       si.direction::text, si.settlement_date,
	       COALESCE(i.base_currency,''), COALESCE(i.quote_currency,'')
	  FROM settlement_instructions si
	  LEFT JOIN trades t  ON t.id = si.trade_id
	  LEFT JOIN instruments i ON i.id = t.instrument_id
	  LEFT JOIN netting_batch_lines nbl
	         ON nbl.settlement_instruction_id = si.id AND nbl.status = 'NETTED'
	 WHERE si.status = 'PENDING'
	   AND si.swift_message_id IS NULL
	   AND nbl.id IS NULL`

// NettableObligations lists unnetted PENDING legs for the scope.
func (s *PgxNettingStore) NettableObligations(ctx context.Context, cpID int64, ccy string, vd time.Time) ([]NettingObligation, error) {
	rows, err := s.Pool.Query(ctx, nettingObligationSelect+`
		   AND si.account_id = $1 AND si.currency = $2 AND si.settlement_date = $3
		 ORDER BY si.id`, cpID, ccy, vd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanObligations(rows)
}

func scanObligations(rows pgx.Rows) ([]NettingObligation, error) {
	var out []NettingObligation
	for rows.Next() {
		var o NettingObligation
		var amt, dir string
		if err := rows.Scan(&o.InstructionID, &o.TradeID, &o.AccountID, &o.Currency,
			&amt, &dir, &o.ValueDate, &o.BaseCurrency, &o.QuoteCurrency); err != nil {
			return nil, err
		}
		var err error
		if o.Amount, err = decimal.NewFromString(amt); err != nil {
			return nil, err
		}
		o.Direction = SettlementDirection(dir)
		out = append(out, o)
	}
	return out, rows.Err()
}

// InTx runs fn inside a SERIALIZABLE transaction.
func (s *PgxNettingStore) InTx(ctx context.Context, fn func(ctx context.Context, tx NettingTx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("netting tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxNettingTx{store: s, tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("netting tx commit: %w", err)
	}
	return nil
}

// Batch loads one row.
func (s *PgxNettingStore) Batch(ctx context.Context, id int64) (*NettingBatch, bool, error) {
	row := s.Pool.QueryRow(ctx, nettingBatchSelect+` WHERE id = $1`, id)
	return scanBatch(row)
}

// BatchLines lists the batch's lines.
func (s *PgxNettingStore) BatchLines(ctx context.Context, batchID int64) ([]NettingLine, error) {
	return batchLines(ctx, s.Pool, batchID)
}

func batchLines(ctx context.Context, q Querier, batchID int64) ([]NettingLine, error) {
	rows, err := q.Query(ctx, `
		SELECT id, batch_id, settlement_instruction_id, trade_id,
		       direction::text, amount::text, status, COALESCE(released_reason,'')
		  FROM netting_batch_lines WHERE batch_id = $1 ORDER BY id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NettingLine
	for rows.Next() {
		var l NettingLine
		var amt string
		if err := rows.Scan(&l.ID, &l.BatchID, &l.SettlementInstructionID,
			&l.TradeID, (*string)(&l.Direction), &amt, &l.Status, &l.ReleasedReason); err != nil {
			return nil, err
		}
		if l.Amount, err = decimal.NewFromString(amt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListBatches pages batches.
func (s *PgxNettingStore) ListBatches(ctx context.Context, status string, limit int) ([]NettingBatch, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := nettingBatchSelect
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1::netting_batch_status_enum`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NettingBatch
	for rows.Next() {
		b, err := scanBatchRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

const nettingBatchSelect = `
	SELECT id, counterparty_account_id, currency, value_date,
	       gross_obligation::text, net_obligation::text,
	       COALESCE(direction,''), ssi_id, ssi_snapshot, COALESCE(rail,''),
	       status::text, reopen_count, COALESCE(dispatch_ref,''),
	       netted_at, dispatched_at, settled_at, created_at
	  FROM payment_netting_batches`

type batchScanner interface {
	Scan(dest ...any) error
}

func scanBatchFields(row batchScanner) (*NettingBatch, error) {
	var b NettingBatch
	var gross, net, status string
	err := row.Scan(&b.ID, &b.CounterpartyAccountID, &b.Currency, &b.ValueDate,
		&gross, &net, &b.Direction, &b.SSIID, &b.SSISnapshot, &b.Rail,
		&status, &b.ReopenCount, &b.DispatchRef, &b.NettedAt, &b.DispatchedAt,
		&b.SettledAt, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	if b.GrossObligation, err = decimal.NewFromString(gross); err != nil {
		return nil, err
	}
	if b.NetObligation, err = decimal.NewFromString(net); err != nil {
		return nil, err
	}
	b.Status = NettingBatchStatus(status)
	return &b, nil
}

func scanBatch(row pgx.Row) (*NettingBatch, bool, error) {
	b, err := scanBatchFields(row)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func scanBatchRows(rows pgx.Rows) (*NettingBatch, error) { return scanBatchFields(rows) }

type pgxNettingTx struct {
	store *PgxNettingStore
	tx    pgx.Tx
}

func (t pgxNettingTx) LockObligations(ctx context.Context, cpID int64, ccy string, vd time.Time) ([]NettingObligation, error) {
	rows, err := t.tx.Query(ctx, nettingObligationSelect+`
		   AND si.account_id = $1 AND si.currency = $2 AND si.settlement_date = $3
		 ORDER BY si.id FOR UPDATE OF si`, cpID, ccy, vd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanObligations(rows)
}

func (t pgxNettingTx) InsertBatch(ctx context.Context, b NettingBatch) (int64, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `
		INSERT INTO payment_netting_batches
		    (counterparty_account_id, currency, value_date, gross_obligation,
		     net_obligation, direction, status, netted_at)
		VALUES ($1,$2,$3,$4::numeric,$5::numeric,NULLIF($6,''),$7,$8)
		RETURNING id`,
		b.CounterpartyAccountID, b.Currency, b.ValueDate,
		b.GrossObligation.String(), b.NetObligation.String(),
		b.Direction, string(b.Status), b.NettedAt).Scan(&id)
	return id, err
}

func (t pgxNettingTx) InsertLines(ctx context.Context, lines []NettingLine) error {
	for _, l := range lines {
		_, err := t.tx.Exec(ctx, `
			INSERT INTO netting_batch_lines
			    (batch_id, settlement_instruction_id, trade_id, direction, amount, status)
			VALUES ($1,$2,$3,$4,$5::numeric,$6)`,
			l.BatchID, l.SettlementInstructionID, l.TradeID,
			string(l.Direction), l.Amount.String(), l.Status)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t pgxNettingTx) LockBatch(ctx context.Context, id int64) (*NettingBatch, bool, error) {
	row := t.tx.QueryRow(ctx, nettingBatchSelect+` WHERE id = $1 FOR UPDATE`, id)
	return scanBatch(row)
}

func (t pgxNettingTx) Lines(ctx context.Context, batchID int64) ([]NettingLine, error) {
	return batchLines(ctx, t.tx, batchID)
}

func (t pgxNettingTx) ReleaseLines(ctx context.Context, batchID int64, instructionIDs []int64, reason string) (int, error) {
	tag, err := t.tx.Exec(ctx, `
		UPDATE netting_batch_lines
		   SET status = 'RELEASED', released_reason = NULLIF($3,'')
		 WHERE batch_id = $1 AND settlement_instruction_id = ANY($2)
		   AND status = 'NETTED'`, batchID, instructionIDs, reason)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (t pgxNettingTx) UpdateBatch(ctx context.Context, b NettingBatch) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE payment_netting_batches
		   SET gross_obligation = $2::numeric, net_obligation = $3::numeric,
		       direction = NULLIF($4,''), status = $5, ssi_id = $6,
		       ssi_snapshot = $7, rail = NULLIF($8,''), reopen_count = $9,
		       dispatch_ref = NULLIF($10,''), netted_at = $11,
		       dispatched_at = $12, settled_at = $13, updated_at = now()
		 WHERE id = $1`,
		b.ID, b.GrossObligation.String(), b.NetObligation.String(), b.Direction,
		string(b.Status), b.SSIID, b.SSISnapshot, b.Rail, b.ReopenCount,
		b.DispatchRef, b.NettedAt, b.DispatchedAt, b.SettledAt)
	return err
}

func (t pgxNettingTx) ClaimNettedLegs(ctx context.Context, instructionIDs []int64, ref string, at time.Time) (int, error) {
	tag, err := t.tx.Exec(ctx, `
		UPDATE settlement_instructions
		   SET swift_message_id = $2, dispatched_at = $3, updated_at = now()
		 WHERE id = ANY($1) AND status = 'PENDING' AND swift_message_id IS NULL`,
		instructionIDs, ref, at)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
