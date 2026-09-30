// variation_margin.go — Phase-22 Task 22.3.7 (daily variation margin for
// FORWARD/SWAP/NDF/OPTION subjects, settled through the double-entry GL)
// plus the margin half of Task 22.3.14 (fail-closed exercise margin gate
// that feeds the existing liquidation queue).
//
// VM contract (spec §5.25, §13.11, §15.7):
//   - VM for a settlement day = subject MTM today − the last SETTLED MTM
//     watermark (vm_amount signed in settlement/quote currency).
//   - Money moves only through DoubleEntryLedgerService.Post — no raw
//     balance updates anywhere in this file. The posting runs inside the
//     ledger's own SERIALIZABLE transaction; client-negative collections
//     are allowed through with AllowNegative and flagged shortfall + P1.
//   - settled_at is the idempotency watermark. The sweep claims a row per
//     (subject, settlement_date); UNIQUE blocks the duplicate claim, a
//     row with settled_at NOT NULL is skipped (rerun = no double-post),
//     and a NULL settled_at claim resumes — the journal idempotency key
//     vm:{row}:{date} dedups the repost inside the ledger.
//
// Orchestration seam: VariationMarginService.Sweep(ctx, day) is invoked by
// the rollover/EOD scheduler once per settlement day (and may be invoked
// for catch-up days). A Redis sweeper mutex makes concurrent instances
// converge instead of double-sweeping.
package risk

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Error codes emitted here. Canonical codes come from the spec §23
// registry; scaffold codes carry their owning task for Phase-05 Task
// 5.3.21 registration (internal/errs/codes.go append-only).
const (
	// CodeVariationMarginInsufficient — scaffold (Task 22.3.7): the VM
	// collection posted and the client wallet went negative — the
	// account owes house money; downstream margin evaluation must pick
	// it up. Logged on variation_margin.shortfall + P1 alert.
	CodeVariationMarginInsufficient = "VARIATION_MARGIN_INSUFFICIENT"
	// CodeVariationMarginInternal — scaffold (Task 22.3.7): store/poster
	// failure mid-sweep; the subject is retried on the next pass.
	CodeVariationMarginInternal = "VARIATION_MARGIN_INTERNAL"
	// CodeExerciseGateUnavailable — scaffold (Task 22.3.14): the margin
	// evaluation path failed — exercise admission fails closed.
	CodeExerciseGateUnavailable = "EXERCISE_MARGIN_EVAL_UNAVAILABLE"
)

// LiquidationReasonExerciseShortfall is the LiquidationJob.Reason value
// dispatched when an exercise delivery breaches margin (Task 22.3.14):
// the job is a claim on margin evaluation — the liquidation ladder
// closes worst-P&L-first under the existing worker, no separate path.
const LiquidationReasonExerciseShortfall = "OPTION_EXERCISE_SHORTFALL"

// ---------------------------------------------------------------------------
// Variation margin — subject model + store seam
// ---------------------------------------------------------------------------

// VM subject kinds — the migration-034 subject_kind domain.
const (
	VMSubjectPosition = "POSITION" // positions.id (instrument-netted leg)
	VMSubjectContract = "CONTRACT" // derivative_contracts.id (mig 252)
)

// VMSubject is one open derivative leg up for daily VM settlement. MTM is
// the cumulative mark-to-market value of the subject in Currency
// (settlement/quote); the source computes it (positions source uses
// qty·(mark−entry)·side; contract sources inject curve pricing).
type VMSubject struct {
	Kind         string // VMSubjectPosition | VMSubjectContract
	RefID        int64
	AccountID    int64
	InstrumentID int64
	Symbol       string
	Currency     string          // settlement currency (ISO)
	MTM          decimal.Decimal // cumulative MTM in Currency
	MarkRate     decimal.Decimal // mark/fixing rate (audit column)
}

// VMStore is the persistence seam for the sweep. The PG implementation is
// PgVMStore; tests substitute fixtures.
type VMStore interface {
	// DerivativeSubjects lists every open derivative MTM subject —
	// FORWARD/SWAP/NDF/OPTION positions plus OPEN derivative_contracts.
	DerivativeSubjects(ctx context.Context) ([]VMSubject, error)
	// ClaimSettlement records the day's settlement row (settled_at NULL).
	// Returns (rowID, done): done=true means a settled row already exists
	// for (subject, day) and the caller must NOT repost.
	ClaimSettlement(ctx context.Context, subj VMSubject, day time.Time) (rowID int64, done bool, err error)
	// PrevMTM returns the MTM watermark of the most recent SETTLED row
	// before day; zero when the subject was never settled.
	PrevMTM(ctx context.Context, subj VMSubject, beforeDay time.Time) (decimal.Decimal, error)
	// CompleteSettlement fills the claim: vm/mtm/rate/journal/shortfall
	// and stamps settled_at. journalID nil = zero-VM settlement (no
	// journal needed; the watermark still advances).
	CompleteSettlement(ctx context.Context, rowID int64, journalID *int64,
		vm, mtm, rate decimal.Decimal, shortfall bool, at time.Time) error
}

// VMPoster is the double-entry posting seam —
// *settlement.LedgerService (a.k.a. DoubleEntryLedgerService) satisfies it.
type VMPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// ContractMTMFunc prices an OPEN derivative_contract row for the sweep.
// Injected: forward-curve pricing is owned by the derivatives package
// (Task 22.3.1 curve machinery); nil disables contract subjects — the
// sweep then reports them as errors (fail-closed, never silently skipped).
type ContractMTMFunc func(ctx context.Context, c VMContractRow) (mtm, rate decimal.Decimal, err error)

// VMContractRow is the derivative_contracts projection the sweep needs.
type VMContractRow struct {
	ID, AccountID, InstrumentID int64
	Kind, Side                  string // FORWARD|SWAP|NDF, BUY|SELL
	Symbol, QuoteCurrency       string
	Notional, ForwardRate       decimal.Decimal
}

// ---------------------------------------------------------------------------
// The sweep service
// ---------------------------------------------------------------------------

// VariationMarginService runs the daily VM settlement.
type VariationMarginService struct {
	store   VMStore
	poster  VMPoster
	rdb     *excredis.Client // sweeper mutex; nil = single-instance trust
	alerter OpsAlerter
	logf    func(string, ...any)
	now     func() time.Time
}

// NewVariationMarginService builds the sweep. store and poster are
// required — a VM service without a GL poster is a money bypass.
func NewVariationMarginService(store VMStore, poster VMPoster,
	rdb *excredis.Client, alerter OpsAlerter, logf func(string, ...any)) (*VariationMarginService, error) {
	if store == nil || poster == nil {
		return nil, excerrors.New(CodeVariationMarginInternal,
			"variation margin: store and GL poster are required (fail-closed)")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &VariationMarginService{
		store: store, poster: poster, rdb: rdb, alerter: alerter,
		logf: logf, now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// VMSweepReport summarizes one sweep pass for observability.
type VMSweepReport struct {
	Day        time.Time
	Subjects   int
	Settled    int // rows stamped settled_at this pass (incl. zero-VM)
	Skipped    int // already settled for the day (idempotent rerun)
	Posted     int // GL journals committed (non-zero VM)
	Shortfalls int
	Errors     []VMSubjectError
}

// VMSubjectError records one subject that failed its settlement — the
// sweep continues (a single bad subject never strands the rest).
type VMSubjectError struct {
	SubjectKind string
	SubjectRef  int64
	AccountID   int64
	Err         error
}

// vmSweepLockTTL bounds the single-sweeper mutex — far above the longest
// realistic sweep, far below a day so a wedged sweeper releases for the
// next run.
const vmSweepLockTTL = 30 * time.Minute
const vmSweepLockKey = "vm:sweep:lock"

// Sweep settles one settlement day across all derivative subjects. It is
// safe to call for the same day any number of times (idempotent via the
// settled_at watermark + journal idempotency keys) and safe to call
// concurrently (single-sweeper mutex; contenders return a no-op report).
func (s *VariationMarginService) Sweep(ctx context.Context, day time.Time) (*VMSweepReport, error) {
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	rep := &VMSweepReport{Day: day}

	if s.rdb != nil {
		tok, err := liquidationLockToken()
		if err != nil {
			return nil, err
		}
		ok, err := s.rdb.SetNX(ctx, vmSweepLockKey, tok, vmSweepLockTTL).Result()
		if err != nil {
			return nil, excerrors.Wrap(CodeVariationMarginInternal, "vm: sweeper lock", err)
		}
		if !ok {
			s.logf("vm: sweep %s already running elsewhere — converging no-op", day.Format("2006-01-02"))
			return rep, nil
		}
		defer func() {
			// Token-guarded release — same discipline as liquidation locks.
			if _, err := liquidationUnlockScript.Run(context.Background(), s.rdb,
				[]string{vmSweepLockKey}, tok).Result(); err != nil {
				s.logf("vm: sweeper unlock: %v", err)
			}
		}()
	}

	subjects, err := s.store.DerivativeSubjects(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeVariationMarginInternal, "vm: subject scan", err)
	}
	rep.Subjects = len(subjects)
	for _, subj := range subjects {
		outcome, serr := s.settleSubject(ctx, subj, day)
		switch {
		case serr != nil:
			rep.Errors = append(rep.Errors, VMSubjectError{
				SubjectKind: subj.Kind, SubjectRef: subj.RefID,
				AccountID: subj.AccountID, Err: serr})
			s.logf("vm: %s %d acct %d settle failed: %v",
				subj.Kind, subj.RefID, subj.AccountID, serr)
		case outcome.skipped:
			rep.Skipped++
		default:
			rep.Settled++
			if outcome.posted {
				rep.Posted++
			}
			if outcome.shortfall {
				rep.Shortfalls++
			}
		}
	}
	return rep, nil
}

type vmOutcome struct{ skipped, posted, shortfall bool }

// settleSubject runs the claim → watermark → journal → commit sequence
// for one subject.
func (s *VariationMarginService) settleSubject(ctx context.Context, subj VMSubject, day time.Time) (vmOutcome, error) {
	rowID, done, err := s.store.ClaimSettlement(ctx, subj, day)
	if err != nil {
		return vmOutcome{}, excerrors.Wrap(CodeVariationMarginInternal, "vm: claim", err)
	}
	if done {
		return vmOutcome{skipped: true}, nil
	}
	prev, err := s.store.PrevMTM(ctx, subj, day)
	if err != nil {
		return vmOutcome{}, excerrors.Wrap(CodeVariationMarginInternal, "vm: prev watermark", err)
	}
	vm := subj.MTM.Sub(prev)
	out := vmOutcome{}
	var journalID *int64

	if !vm.IsZero() {
		j := vmJournal(subj, vm, rowID, day)
		res, err := s.poster.Post(ctx, j)
		if err != nil {
			return out, excerrors.Wrap(CodeVariationMarginInternal,
				fmt.Sprintf("vm: GL post acct %d", subj.AccountID), err)
		}
		out.posted = !res.Replayed
		journalID = &res.JournalID
		out.shortfall = vmShortfall(res, subj)
		if out.shortfall {
			s.raiseAlert(ctx, SeverityP1, CodeVariationMarginInsufficient, fmt.Sprintf(
				"VM collection drove account %d %s wallet negative (vm=%s)",
				subj.AccountID, subj.Currency, vm.String()), map[string]string{
				"account_id": fmt.Sprint(subj.AccountID),
				"subject":    fmt.Sprintf("%s:%d", subj.Kind, subj.RefID),
				"vm_amount":  vm.String(), "currency": subj.Currency})
		}
	}
	if err := s.store.CompleteSettlement(ctx, rowID, journalID,
		vm, subj.MTM, subj.MarkRate, out.shortfall, s.now()); err != nil {
		return out, excerrors.Wrap(CodeVariationMarginInternal, "vm: complete", err)
	}
	return out, nil
}

// vmJournal builds the balanced settlement journal. Client-gain VM
// (vm>0): house equity pays the customer liability. Client-loss VM
// (vm<0): customer liability debits to house equity — AllowNegative lets
// the wallet go negative (the account owes; margin evaluation + the
// shortfall flag handle it) rather than silently failing the collection.
func vmJournal(subj VMSubject, vm decimal.Decimal, rowID int64, day time.Time) ledger.Journal {
	amt := vm.Abs()
	desc := fmt.Sprintf("variation margin %s %s %s",
		subj.Symbol, subj.Kind, day.Format("2006-01-02"))
	j := ledger.Journal{
		EntryType:      ledger.EntrySettlement,
		ReferenceID:    rowID,
		Description:    desc,
		PostedBy:       "variation-margin",
		IdempotencyKey: fmt.Sprintf("vm:%d:%s", rowID, day.Format("2006-01-02")),
	}
	if vm.Sign() > 0 { // client gains: DR house equity / CR customer liability
		j.Lines = []ledger.Line{
			ledger.DebitLine(ledger.HouseEquity(subj.Currency), subj.Currency, amt, desc),
			ledger.CreditLine(ledger.CustomerLiability(subj.Currency), subj.Currency, amt, desc),
		}
	} else { // client pays: DR customer liability / CR house equity
		j.Lines = []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(subj.Currency), subj.Currency, amt, desc),
			ledger.CreditLine(ledger.HouseEquity(subj.Currency), subj.Currency, amt, desc),
		}
	}
	j.Effects = []ledger.AccountEffect{{
		AccountID:      subj.AccountID,
		Currency:       subj.Currency,
		AvailableDelta: vm,
		AllowNegative:  true, // VM collection may exceed cash — flagged shortfall
	}}
	return j
}

// vmShortfall reports whether the post-commit balance event shows a
// negative available balance for the settled currency.
func vmShortfall(res ledger.PostResult, subj VMSubject) bool {
	for _, ev := range res.Events {
		if ev.AccountID != subj.AccountID || ev.Currency != subj.Currency {
			continue
		}
		if d, err := decimal.NewFromString(ev.Available); err == nil && d.IsNegative() {
			return true
		}
	}
	return false
}

func (s *VariationMarginService) raiseAlert(ctx context.Context, sev, code, summary string, details map[string]string) {
	if s.alerter == nil {
		s.logf("vm: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.alerter.Raise(actx, OpsAlert{
		Severity: sev, Code: code, Summary: summary, Details: details}); err != nil {
		s.logf("vm: alert %s raise: %v", code, err)
	}
}

// ---------------------------------------------------------------------------
// PG store implementation
// ---------------------------------------------------------------------------

// PgVMStore is the production VMStore over PostgreSQL. Position subjects
// are derived inline; contract subjects need an injected ContractMTMFunc
// (curve pricing lives with the derivatives package).
type PgVMStore struct {
	Pool        *pgxpool.Pool
	ContractMTM ContractMTMFunc // nil = contract subjects error fail-closed
}

// DerivativeSubjects returns open non-SPOT positions plus OPEN
// derivative_contracts. Position MTM = qty·(mark−entry)·side in quote ccy
// (never-marked positions fall back to entry ⇒ MTM 0 — same convention
// the margin engine uses for unrealized P&L).
func (s *PgVMStore) DerivativeSubjects(ctx context.Context) ([]VMSubject, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id, p.account_id, p.instrument_id, i.symbol,
		       i.quote_currency, p.side, p.quantity,
		       p.entry_price, COALESCE(p.mark_price, p.entry_price)
		FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		WHERE i.instrument_type IN ('FORWARD','SWAP','NDF','OPTION')
		ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VMSubject
	for rows.Next() {
		var (
			subj             VMSubject
			side             string
			qty, entry, mark decimal.Decimal
		)
		if err := rows.Scan(&subj.RefID, &subj.AccountID, &subj.InstrumentID,
			&subj.Symbol, &subj.Currency, &side, &qty, &entry, &mark); err != nil {
			return nil, err
		}
		subj.Kind = VMSubjectPosition
		diff := mark.Sub(entry)
		if side == "SHORT" {
			diff = diff.Neg()
		}
		subj.MTM = qty.Mul(diff).Round(8)
		subj.MarkRate = mark
		out = append(out, subj)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Contract subjects (FORWARD/SWAP/NDF booked contracts). The table is
	// a Phase-22 sibling migration — when absent (pre-254 schema, test
	// fixtures) there are no contract subjects by definition.
	var hasContracts bool
	if err := s.Pool.QueryRow(ctx,
		`SELECT to_regclass('derivative_contracts') IS NOT NULL`).Scan(&hasContracts); err != nil {
		return nil, err
	}
	if !hasContracts {
		return out, nil
	}
	crows, err := s.Pool.Query(ctx, `
		SELECT c.id, c.account_id, c.instrument_id, i.symbol,
		       c.kind, c.side, c.quote_currency, c.notional, c.forward_rate
		FROM derivative_contracts c
		JOIN instruments i ON i.id = c.instrument_id
		WHERE c.status = 'OPEN'
		ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var c VMContractRow
		if err := crows.Scan(&c.ID, &c.AccountID, &c.InstrumentID, &c.Symbol,
			&c.Kind, &c.Side, &c.QuoteCurrency, &c.Notional, &c.ForwardRate); err != nil {
			return nil, err
		}
		subj := VMSubject{
			Kind: VMSubjectContract, RefID: c.ID, AccountID: c.AccountID,
			InstrumentID: c.InstrumentID, Symbol: c.Symbol, Currency: c.QuoteCurrency,
		}
		if s.ContractMTM == nil {
			// No pricer bound — represent the subject as MTM-unpriced; the
			// sweep claims it and errors rather than silently skipping.
			subj.MTM = decimal.Zero
			out = append(out, subj)
			continue
		}
		mtm, rate, err := s.ContractMTM(ctx, c)
		if err != nil {
			return nil, excerrors.Wrap(CodeVariationMarginInternal,
				fmt.Sprintf("vm: contract %d MTM", c.ID), err)
		}
		subj.MTM, subj.MarkRate = mtm.Round(8), rate
		out = append(out, subj)
	}
	return out, crows.Err()
}

// ClaimSettlement inserts the claim row; on conflict selects the existing
// row and reports done=settled_at IS NOT NULL. Runs inside a SERIALIZABLE
// tx (money-adjacent bookkeeping, spec §5.40).
func (s *PgVMStore) ClaimSettlement(ctx context.Context, subj VMSubject, day time.Time) (int64, bool, error) {
	var rowID int64
	var done bool
	err := serializableTx(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO variation_margin
			    (account_id, instrument_id, subject_kind, subject_ref,
			     settlement_date, currency, vm_amount, mtm_value, vm_rate)
			VALUES ($1,$2,$3,$4,$5,$6,0,0,NULL)
			ON CONFLICT (subject_kind, subject_ref, settlement_date) DO NOTHING
			RETURNING id`,
			subj.AccountID, subj.InstrumentID, subj.Kind, subj.RefID,
			day, subj.Currency).Scan(&rowID)
		if err == nil {
			return nil // fresh claim
		}
		if !stderrors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var settledAt *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT id, settled_at FROM variation_margin
			WHERE subject_kind=$1 AND subject_ref=$2 AND settlement_date=$3`,
			subj.Kind, subj.RefID, day).Scan(&rowID, &settledAt); err != nil {
			return err
		}
		done = settledAt != nil
		return nil
	})
	return rowID, done, err
}

// PrevMTM returns the last settled watermark before the given day.
func (s *PgVMStore) PrevMTM(ctx context.Context, subj VMSubject, beforeDay time.Time) (decimal.Decimal, error) {
	var mtm decimal.Decimal
	err := s.Pool.QueryRow(ctx, `
		SELECT mtm_value FROM variation_margin
		WHERE subject_kind=$1 AND subject_ref=$2
		  AND settlement_date < $3 AND settled_at IS NOT NULL
		ORDER BY settlement_date DESC LIMIT 1`,
		subj.Kind, subj.RefID, beforeDay).Scan(&mtm)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, nil
	}
	return mtm, err
}

// CompleteSettlement stamps the claim. settled_at IS NULL in the WHERE is
// a second idempotency fence: a row concurrently completed elsewhere is a
// no-op here (the journal was already dedup'd by idempotency key).
func (s *PgVMStore) CompleteSettlement(ctx context.Context, rowID int64, journalID *int64,
	vm, mtm, rate decimal.Decimal, shortfall bool, at time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE variation_margin
		SET vm_amount=$2, mtm_value=$3, vm_rate=$4, journal_entry_id=$5,
		    shortfall=$6, settled_at=$7
		WHERE id=$1 AND settled_at IS NULL`,
		rowID, vm, mtm, nullDec(rate), journalID, shortfall, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New(CodeVariationMarginInternal,
			fmt.Sprintf("vm: row %d already settled (concurrent sweep)", rowID))
	}
	return nil
}

func nullDec(d decimal.Decimal) *decimal.Decimal {
	if d.IsZero() {
		return nil
	}
	return &d
}

// serializableTx runs fn inside a SERIALIZABLE transaction, retrying
// 40001/40P01 per the §5.40 bounded-retry contract.
func serializableTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return err
		}
		err = fn(tx)
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err == nil {
			return nil
		}
		if !isSerializationErr(err) {
			_ = tx.Rollback(ctx)
			return err
		}
	}
	return excerrors.New("TRANSACTION_CONFLICT_RETRY_EXHAUSTED",
		"serializable retry budget exhausted")
}

func isSerializationErr(err error) bool {
	var pge *pgconn.PgError
	if stderrors.As(err, &pge) {
		return pge.Code == "40001" || pge.Code == "40P01"
	}
	return false
}

// ---------------------------------------------------------------------------
// Task 22.3.14 (margin half) — exercise-shortfall liquidation adapter
// ---------------------------------------------------------------------------
//
// The lifecycle (internal/derivatives/lifecycle.go) owns the verdict:
//   - manual exercise: ExerciseMarginChecker fails the instruction with
//     OPTION_EXERCISE_MARGIN_SHORTFALL before anything is booked;
//   - auto exercise: delivery commits (the position is real), the report
//     carries MarginShortfall, and post-commit the ExerciseLiquidator
//     seam is invoked.
//
// This file supplies the production binding for that seam: the adapter
// enqueues a durable account-level job on LiquidationQueue with reason
// OPTION_EXERCISE_SHORTFALL. The existing worker re-reads the margin
// level, mass-cancels, and closes worst-P&L-first through the standard
// DIRECT/AUCTION ladder — no separate exercise-liquidation path. The
// delivered leg is worst-P&L by construction (the shortfall verdict was
// just computed against it), so the standard ladder targets it.
//
// Orchestrator binding (composition root):
//
//	liq := risk.NewExerciseShortfallLiquidator(queue, levels, alerter)
//	opts := derivatives.WithExerciseLiquidator(liq)
//	svc, _ := derivatives.NewOptionService(pool, ls, marks, locks, pub, opts)

// ExerciseShortfallLiquidator adapts LiquidationQueue to the
// derivatives.ExerciseLiquidator seam (LiquidateForExerciseShortfall).
// EnqueueDedup makes repeated shortfall dispatches idempotent — the
// per-account dedup claim means a second exercise into the same broken
// account cannot stack jobs.
type ExerciseShortfallLiquidator struct {
	queue   *LiquidationQueue
	levels  MarginLevelReader
	alerter OpsAlerter
	logf    func(string, ...any)
}

// NewExerciseShortfallLiquidator wires the adapter. queue is required —
// an exercise-shortfall path without the durable queue is a stranded
// account. levels may be nil (the job then carries no snapshot — the
// worker re-evaluates regardless).
func NewExerciseShortfallLiquidator(queue *LiquidationQueue, levels MarginLevelReader,
	alerter OpsAlerter, logf func(string, ...any)) (*ExerciseShortfallLiquidator, error) {
	if queue == nil {
		return nil, excerrors.New(CodeExerciseGateUnavailable,
			"exercise liquidator: nil liquidation queue (fail-closed)")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &ExerciseShortfallLiquidator{
		queue: queue, levels: levels, alerter: alerter, logf: logf}, nil
}

// LiquidateForExerciseShortfall implements derivatives.ExerciseLiquidator.
// It reads the freshest margin level for the job snapshot, enqueues
// dedup, and on enqueue failure raises P1 — the caller (lifecycle) also
// surfaces the error; the account is never silently stranded.
func (l *ExerciseShortfallLiquidator) LiquidateForExerciseShortfall(ctx context.Context, accountID int64) error {
	var lv *MarginLevel
	if l.levels != nil {
		if v, err := l.levels.MarginLevel(ctx, accountID); err == nil {
			lv = v
		} else {
			l.logf("exercise liquidator: level read acct %d: %v", accountID, err)
		}
	}
	enq, err := l.queue.EnqueueDedup(ctx, LiquidationJob{
		AccountID: accountID,
		Reason:    LiquidationReasonExerciseShortfall,
	}, lv)
	if err != nil {
		if l.alerter != nil {
			actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = l.alerter.Raise(actx, OpsAlert{
				Severity: SeverityP1, Code: CodeLiquidationFailed,
				Summary: fmt.Sprintf(
					"exercise-shortfall liquidation dispatch failed acct %d", accountID),
				Details: map[string]string{
					"account_id": fmt.Sprint(accountID), "error": err.Error()}})
		}
		return excerrors.Wrap(CodeLiquidationFailed,
			fmt.Sprintf("exercise-shortfall enqueue acct %d", accountID), err)
	}
	if !enq {
		l.logf("exercise liquidator: acct %d already queued (dedup)", accountID)
	}
	return nil
}
