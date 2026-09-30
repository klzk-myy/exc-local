// Task 24.3.2 — Nostro/Vostro Reconciliation (spec §17; §24 #21;
// Phase-24 AC #4–#8, #25).
//
// Daily reconciliation runs our nostro_movements ledger (the internal
// record) against the correspondent bank's statement entries
// (nostro_statement_entries, migration 261 — fed by the Task 24.3.12
// MT940/MT942/camt.053 parsers and by the NostroStatementSource polling seam
// this file defines for real-time balance tracking, Task 24.3.1).
//
// Auto-resolution: a statement entry matching an internal movement on
// SWIFT reference + amount + date auto-resolves — a still-PENDING
// movement is posted through the Task 24.3.1 poster (balance updated)
// and any open break on that movement flips AUTO_RESOLVED.
//
// Breaks land per category — TIMING (ref+amount match on a different
// day), MISSING_CONFIRMATION (our movement never appeared on the bank
// statement), AMOUNT_MISMATCH (ref match, amount differs), FEES (bank
// charges — narrative/reference fee pattern), UNMATCHED (everything
// else) — and feed the investigation workflow (OPEN → INVESTIGATING →
// RESOLVED).
//
// Any mismatch raises a durable P1 NOSTRO_RECON_MISMATCH alert
// (funding_ops_alerts trail, dedup-keyed per account+date). The spec
// §24 #21 discrepancy flag trips when |statement_net − our_net| exceeds
// the greater of $1,000-equivalent or 0.01% of the expected amount.
// NOTE (documented deviation): the absolute threshold is evaluated in
// the account's own currency — a USD cross-rate conversion would need
// an oracle dependency this task deliberately does not take.
package backoffice

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// CodeNostroReconMismatch is the durable P1 alert code for a nostro
// reconciliation mismatch (§24 #5).
const CodeNostroReconMismatch = "NOSTRO_RECON_MISMATCH"

// Discrepancy thresholds (spec §24 #21): flag any settlement
// discrepancy > $1,000-equivalent or > 0.01% of the expected amount.
var (
	NostroReconThresholdAbs = decimal.NewFromInt(1000)
	NostroReconThresholdPct = decimal.New(1, -4) // 0.0001
)

// nostroThresholdExceeded applies the §24 #21 OR-rule.
func nostroThresholdExceeded(diff, expected decimal.Decimal) bool {
	abs := diff.Abs()
	if abs.GreaterThan(NostroReconThresholdAbs) {
		return true
	}
	return abs.GreaterThan(expected.Abs().Mul(NostroReconThresholdPct))
}

// Break categories mirror nostro_break_category_enum.
type NostroBreakCategory string

const (
	NostroBreakTiming              NostroBreakCategory = "TIMING"
	NostroBreakMissingConfirmation NostroBreakCategory = "MISSING_CONFIRMATION"
	NostroBreakFees                NostroBreakCategory = "FEES"
	NostroBreakAmountMismatch      NostroBreakCategory = "AMOUNT_MISMATCH"
	NostroBreakUnmatched           NostroBreakCategory = "UNMATCHED"
)

// NostroBreakStatus mirrors nostro_break_status_enum.
type NostroBreakStatus string

const (
	NostroBreakOpen          NostroBreakStatus = "OPEN"
	NostroBreakInvestigating NostroBreakStatus = "INVESTIGATING"
	NostroBreakAutoResolved  NostroBreakStatus = "AUTO_RESOLVED"
	NostroBreakResolved      NostroBreakStatus = "RESOLVED"
)

// NostroStatementEntry is one nostro_statement_entries row — a normalized
// bank-statement line (Task 24.3.12 parsers + the polling seam write
// here; ingest is idempotent on the natural key).
type NostroStatementEntry struct {
	ID              int64            `json:"id"`
	NostroAccountID int64            `json:"nostro_account_id"`
	StatementDate   time.Time        `json:"statement_date"` // UTC-midnight day
	SwiftReference  string           `json:"swift_reference"`
	Direction       string           `json:"direction"` // DEBIT|CREDIT
	Amount          decimal.Decimal  `json:"amount"`
	Currency        string           `json:"currency"`
	ClosingBalance  *decimal.Decimal `json:"closing_balance,omitempty"`
	Narrative       string           `json:"narrative,omitempty"`
	Source          string           `json:"source"` // MT940|MT942|CAMT053|POLL|MANUAL
	CreatedAt       time.Time        `json:"created_at"`
}

func (e NostroStatementEntry) signed() decimal.Decimal {
	if e.Direction == "DEBIT" {
		return e.Amount.Neg()
	}
	return e.Amount
}

// NostroStatementSource is the bank-statement polling seam — the second
// balance-tracking input named by Task 24.3.1. Task 24.3.12's parsers
// (MT940/MT942/camt.053) and Phase-11 rail adapters implement it; nil →
// reconciliation runs over already-stored entries only.
type NostroStatementSource interface {
	Poll(ctx context.Context, nostroAccountID int64, day time.Time) ([]NostroStatementEntry, error)
}

// NostroReconRun is one nostro_recon_runs row — one pass over one account/day.
type NostroReconRun struct {
	ID                 int64           `json:"id"`
	NostroAccountID    int64           `json:"nostro_account_id"`
	ReconDate          time.Time       `json:"recon_date"`
	OurNet             decimal.Decimal `json:"our_net"`
	StatementNet       decimal.Decimal `json:"statement_net"`
	Difference         decimal.Decimal `json:"difference"`
	ThresholdBreach    bool            `json:"threshold_breach"` // §24 #21 flag
	BreaksOpened       int             `json:"breaks_opened"`
	BreaksAutoResolved int             `json:"breaks_auto_resolved"`
	Status             string          `json:"status"` // CLEAN|BREAKS_OPEN|DISCREPANCY
	CreatedAt          time.Time       `json:"created_at"`
}

// NostroReconBreak is one nostro_recon_breaks row — the investigation item.
type NostroReconBreak struct {
	ID               int64               `json:"id"`
	RunID            int64               `json:"run_id"`
	NostroAccountID  int64               `json:"nostro_account_id"`
	ReconDate        time.Time           `json:"recon_date"`
	Category         NostroBreakCategory `json:"category"`
	MovementID       *int64              `json:"nostro_movement_id,omitempty"`
	StatementEntryID *int64              `json:"statement_entry_id,omitempty"`
	SwiftReference   string              `json:"swift_reference,omitempty"`
	Currency         string              `json:"currency,omitempty"`
	ExpectedAmount   *decimal.Decimal    `json:"expected_amount,omitempty"`
	ActualAmount     *decimal.Decimal    `json:"actual_amount,omitempty"`
	Difference       *decimal.Decimal    `json:"difference,omitempty"`
	Status           NostroBreakStatus   `json:"status"`
	AssignedTo       *int64              `json:"assigned_to,omitempty"`
	ResolutionNotes  string              `json:"resolution_notes,omitempty"`
	DetectedAt       time.Time           `json:"detected_at"`
	ResolvedAt       *time.Time          `json:"resolved_at,omitempty"`
	ResolvedBy       *int64              `json:"resolved_by,omitempty"`
}

// NostroReconReport is the GET /api/v1/admin/nostro-reconciliation payload —
// latest run per account plus every break opened for the day.
type NostroReconReport struct {
	Date               time.Time          `json:"date"`
	Runs               []NostroReconRun   `json:"runs"`
	Breaks             []NostroReconBreak `json:"breaks"`
	AccountsReconciled int                `json:"accounts_reconciled"`
	OpenBreaks         int                `json:"open_breaks"`
	ThresholdBreaches  int                `json:"threshold_breaches"`
}

// NostroReconStore is the persistence seam — PgNostroStore implements it.
type NostroReconStore interface {
	NostroStore
	// HasOpenAlert is the dedup check on the durable alert trail.
	HasOpenAlert(ctx context.Context, code, dedupKey string) (bool, error)
	// UpsertStatementEntries ingests idempotently (natural-key UNIQUE);
	// returns rows actually inserted.
	UpsertStatementEntries(ctx context.Context, entries []NostroStatementEntry) (int, error)
	// StatementEntriesFor returns the day's entries for one account.
	StatementEntriesFor(ctx context.Context, accountID int64, day time.Time) ([]NostroStatementEntry, error)
	// MovementsInWindow returns the account's movements in [from,to) —
	// the recon matcher pulls a ±window so TIMING breaks can see
	// adjacent-day confirmations.
	MovementsInWindow(ctx context.Context, accountID int64, from, to time.Time) ([]NostroMovement, error)
	// InsertReconRun appends the run row and fills id/created_at.
	InsertReconRun(ctx context.Context, r *NostroReconRun) error
	// LatestRunsForDate returns the newest run per account for the day.
	LatestRunsForDate(ctx context.Context, day time.Time) ([]NostroReconRun, error)
	// InsertBreak appends the break row and fills id/detected_at.
	InsertBreak(ctx context.Context, b *NostroReconBreak) error
	// BreaksForDate lists the day's breaks (all accounts).
	BreaksForDate(ctx context.Context, day time.Time) ([]NostroReconBreak, error)
	// OpenBreaksFor lists OPEN/INVESTIGATING breaks for one account+date —
	// reruns auto-resolve them when their item now matches.
	OpenBreaksFor(ctx context.Context, accountID int64, day time.Time) ([]NostroReconBreak, error)
	// ResolveBreak flips OPEN/INVESTIGATING → RESOLVED (notes + actor);
	// applied=false when the break is already terminal.
	ResolveBreak(ctx context.Context, breakID int64, notes string, by int64, at time.Time) (bool, error)
	// AssignBreak flips OPEN → INVESTIGATING recording the owner.
	AssignBreak(ctx context.Context, breakID, actor int64, at time.Time) (bool, error)
	// AutoResolveForMovement flips open breaks on a movement to
	// AUTO_RESOLVED — the entry matched on a later pass.
	AutoResolveForMovement(ctx context.Context, movementID int64, at time.Time) (int, error)
}

// NostroReconService runs the daily reconciliation and owns the break
// investigation workflow.
type NostroReconService struct {
	store   NostroReconStore
	poster  MovementPoster        // *NostroService — auto-resolution posts balances
	source  NostroStatementSource // optional polling seam
	alerter OpsAlerter            // optional page channel
	clock   func() time.Time
	logf    func(format string, args ...any)
}

// NewNostroReconService wires the service; store + poster are mandatory
// (fail-closed — an auto-match that cannot post the balance must not
// mark anything resolved).
func NewNostroReconService(store NostroReconStore, poster MovementPoster) (*NostroReconService, error) {
	if store == nil || poster == nil {
		return nil, fmt.Errorf("backoffice: recon service requires store and poster")
	}
	return &NostroReconService{store: store, poster: poster, clock: time.Now}, nil
}

// WithStatements wires the bank-statement polling seam (optional).
func (s *NostroReconService) WithStatements(src NostroStatementSource) *NostroReconService {
	s.source = src
	return s
}

// WithAlerter wires the ops paging seam.
func (s *NostroReconService) WithAlerter(a OpsAlerter) *NostroReconService {
	s.alerter = a
	return s
}

// WithClock overrides the clock (tests).
func (s *NostroReconService) WithClock(c func() time.Time) *NostroReconService {
	s.clock = c
	return s
}

// WithLogger wires a log sink.
func (s *NostroReconService) WithLogger(f func(format string, args ...any)) *NostroReconService {
	s.logf = f
	return s
}

func (s *NostroReconService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// nostroFeeRe matches bank-charge narratives/refs for the FEES break category.
var nostroFeeRe = regexp.MustCompile(`(?i)(fee|chrg|charge|comm|commission|interest)`)

// RunDaily reconciles every ACTIVE nostro/vostro account (or a single
// account when accountID is set) for the given UTC day: optional
// statement poll → idempotent ingest → match → breaks → run row → P1
// alert on mismatch.
func (s *NostroReconService) RunDaily(ctx context.Context, date time.Time, accountID *int64) (*NostroReconReport, error) {
	day := date.UTC().Truncate(24 * time.Hour)
	f := AccountFilter{Status: "ACTIVE", Limit: 500}
	accounts, err := s.store.ListAccounts(ctx, f)
	if err != nil {
		return nil, err
	}
	if accountID != nil {
		var one []NostroAccount
		for _, a := range accounts {
			if a.ID == *accountID {
				one = append(one, a)
			}
		}
		accounts = one
	}
	rep := &NostroReconReport{Date: day, Runs: []NostroReconRun{}, Breaks: []NostroReconBreak{}}
	for _, acct := range accounts {
		run, breaks, err := s.reconcileAccount(ctx, acct, day)
		if err != nil {
			// Per-account failure is logged, never aborts the batch —
			// the day's report still surfaces the reconciled accounts.
			s.log("backoffice: recon account %d: %v", acct.ID, err)
			continue
		}
		rep.Runs = append(rep.Runs, *run)
		rep.Breaks = append(rep.Breaks, breaks...)
		if run.ThresholdBreach {
			rep.ThresholdBreaches++
		}
	}
	rep.AccountsReconciled = len(rep.Runs)
	for _, b := range rep.Breaks {
		if b.Status == NostroBreakOpen || b.Status == NostroBreakInvestigating {
			rep.OpenBreaks++
		}
	}
	return rep, nil
}

// reconcileAccount performs the match → break → run → alert pipeline
// for one account/day.
func (s *NostroReconService) reconcileAccount(ctx context.Context, acct NostroAccount, day time.Time) (*NostroReconRun, []NostroReconBreak, error) {
	// 1. Optional live statement poll → idempotent ingest.
	if s.source != nil {
		entries, err := s.source.Poll(ctx, acct.ID, day)
		if err != nil {
			return nil, nil, fmt.Errorf("statement poll acct %d: %w", acct.ID, err)
		}
		for i := range entries {
			entries[i].NostroAccountID = acct.ID
			entries[i].StatementDate = day
		}
		if _, err := s.store.UpsertStatementEntries(ctx, entries); err != nil {
			return nil, nil, fmt.Errorf("statement ingest acct %d: %w", acct.ID, err)
		}
	}
	entries, err := s.store.StatementEntriesFor(ctx, acct.ID, day)
	if err != nil {
		return nil, nil, err
	}
	// ±3d/+1d window so TIMING breaks can see adjacent-day confirmations.
	from := day.Add(-3 * 24 * time.Hour)
	to := day.Add(2 * 24 * time.Hour)
	movements, err := s.store.MovementsInWindow(ctx, acct.ID, from, to)
	if err != nil {
		return nil, nil, err
	}

	// 2. Match pass — auto-resolution keys: SWIFT ref + amount + date.
	byRef := map[string][]*NostroMovement{}
	for i := range movements {
		m := &movements[i]
		if m.ConfirmationRef != "" {
			byRef[m.ConfirmationRef] = append(byRef[m.ConfirmationRef], m)
		}
		if m.SwiftMessageID != "" && m.SwiftMessageID != m.ConfirmationRef {
			byRef[m.SwiftMessageID] = append(byRef[m.SwiftMessageID], m)
		}
	}
	matchedMov := map[int64]bool{}
	var breaks []NostroReconBreak
	autoResolved := 0
	now := s.clock().UTC()

	for _, e := range entries {
		cands := byRef[e.SwiftReference]
		var exact *NostroMovement
		var refOnly *NostroMovement
		for _, c := range cands {
			if c.Direction == e.Direction && c.Amount.Equal(e.Amount) &&
				c.CreatedAt.UTC().Truncate(24*time.Hour).Equal(day) {
				exact = c
				break
			}
			if refOnly == nil {
				refOnly = c
			}
		}
		switch {
		case exact != nil:
			matchedMov[exact.ID] = true
			if exact.Status == "PENDING" {
				// The bank statement is itself the confirmation —
				// post through the Task 24.3.1 seam.
				if _, err := s.poster.PostMovement(ctx, exact.ID); err != nil {
					return nil, nil, fmt.Errorf(
						"auto-resolve post movement %d: %w", exact.ID, err)
				}
			}
			n, err := s.store.AutoResolveForMovement(ctx, exact.ID, now)
			if err != nil {
				return nil, nil, err
			}
			autoResolved += n
		case refOnly != nil:
			cat := NostroBreakTiming
			if !refOnly.Amount.Equal(e.Amount) {
				cat = NostroBreakAmountMismatch
			}
			exp := refOnly.Amount
			act := e.Amount
			diff := e.Amount.Sub(refOnly.Amount)
			breaks = append(breaks, NostroReconBreak{
				NostroAccountID: acct.ID, ReconDate: day, Category: cat,
				MovementID: &refOnly.ID, StatementEntryID: &e.ID,
				SwiftReference: e.SwiftReference, Currency: e.Currency,
				ExpectedAmount: &exp, ActualAmount: &act, Difference: &diff,
				Status: NostroBreakOpen, DetectedAt: now,
			})
		default:
			cat := NostroBreakUnmatched
			if e.Direction == "DEBIT" &&
				nostroFeeRe.MatchString(e.SwiftReference+" "+e.Narrative) {
				cat = NostroBreakFees
			}
			act := e.Amount
			breaks = append(breaks, NostroReconBreak{
				NostroAccountID: acct.ID, ReconDate: day, Category: cat,
				StatementEntryID: &e.ID, SwiftReference: e.SwiftReference,
				Currency: e.Currency, ActualAmount: &act,
				Status: NostroBreakOpen, DetectedAt: now,
			})
		}
	}
	// Our movements dated on the recon day that no statement entry
	// matched are missing confirmations.
	for _, m := range movements {
		if matchedMov[m.ID] {
			continue
		}
		if !m.CreatedAt.UTC().Truncate(24 * time.Hour).Equal(day) {
			continue // adjacent-window row — not this day's problem
		}
		exp := m.Amount
		mv := m.ID
		breaks = append(breaks, NostroReconBreak{
			NostroAccountID: acct.ID, ReconDate: day,
			Category: NostroBreakMissingConfirmation, MovementID: &mv,
			SwiftReference: nostroFirstNonEmpty(m.ConfirmationRef, m.SwiftMessageID),
			Currency:       m.Currency, ExpectedAmount: &exp,
			Status: NostroBreakOpen, DetectedAt: now,
		})
	}

	// 3. Nets + §24 #21 threshold.
	var ourNet, stmtNet decimal.Decimal
	for _, m := range movements {
		if m.CreatedAt.UTC().Truncate(24 * time.Hour).Equal(day) {
			ourNet = ourNet.Add(m.signedAmount())
		}
	}
	for _, e := range entries {
		stmtNet = stmtNet.Add(e.signed())
	}
	diff := stmtNet.Sub(ourNet)
	breach := nostroThresholdExceeded(diff, ourNet)
	status := "CLEAN"
	if breach {
		status = "DISCREPANCY"
	} else if !diff.IsZero() || len(breaks) > 0 {
		status = "BREAKS_OPEN"
	}

	// 4. Persist run + breaks.
	run := &NostroReconRun{
		NostroAccountID: acct.ID, ReconDate: day,
		OurNet: ourNet, StatementNet: stmtNet, Difference: diff,
		ThresholdBreach: breach, BreaksOpened: len(breaks),
		BreaksAutoResolved: autoResolved, Status: status, CreatedAt: now,
	}
	if err := s.store.InsertReconRun(ctx, run); err != nil {
		return nil, nil, err
	}
	for i := range breaks {
		breaks[i].RunID = run.ID
		if err := s.store.InsertBreak(ctx, &breaks[i]); err != nil {
			return nil, nil, err
		}
	}

	// 5. Mismatch → durable P1 (dedup-keyed) + page.
	if status != "CLEAN" {
		s.raiseReconAlert(ctx, acct, run, len(breaks))
	}
	return run, breaks, nil
}

// raiseReconAlert lands the deduplicated P1 on the durable trail and
// pages ops best-effort.
func (s *NostroReconService) raiseReconAlert(ctx context.Context, acct NostroAccount, run *NostroReconRun, nBreaks int) {
	key := fmt.Sprintf("nostro-recon:%d:%s", acct.ID,
		run.ReconDate.Format("2006-01-02"))
	open, err := s.store.HasOpenAlert(ctx, CodeNostroReconMismatch, key)
	if err != nil || open {
		if err != nil {
			s.log("backoffice: recon alert dedup acct %d: %v", acct.ID, err)
		}
		return
	}
	ccy := acct.Currency
	diff := run.Difference
	summary := fmt.Sprintf(
		"nostro recon %d (%s %s) %s on %s — stmt_net %s vs our_net %s (diff %s, %d breaks)",
		acct.ID, acct.BankName, acct.Currency, run.Status,
		run.ReconDate.Format("2006-01-02"), run.StatementNet.String(),
		run.OurNet.String(), diff.String(), nBreaks)
	err = s.store.InTx(ctx, func(ctx context.Context, tx NostroTx) error {
		_, ierr := tx.InsertAlert(ctx, OpsAlertRow{
			Code:     CodeNostroReconMismatch,
			Severity: "P1",
			Currency: &ccy,
			Amount:   &diff,
			Summary:  summary,
			Detail: []byte(fmt.Sprintf(
				`{"nostro_account_id":%d,"recon_date":%q,"run_id":%d,"breaks":%d,"threshold_breach":%t,"dedup_key":%q}`,
				acct.ID, run.ReconDate.Format("2006-01-02"), run.ID,
				nBreaks, run.ThresholdBreach, key)),
		})
		return ierr
	})
	if err != nil {
		s.log("backoffice: recon alert insert acct %d: %v", acct.ID, err)
		return
	}
	if s.alerter != nil {
		_ = s.alerter.Raise(ctx, OpsAlert{
			Severity: "P1", Code: CodeNostroReconMismatch, Summary: summary})
	}
}

// Report builds the GET surface payload — latest run per account plus
// the day's breaks.
func (s *NostroReconService) Report(ctx context.Context, date time.Time) (*NostroReconReport, error) {
	day := date.UTC().Truncate(24 * time.Hour)
	runs, err := s.store.LatestRunsForDate(ctx, day)
	if err != nil {
		return nil, err
	}
	breaks, err := s.store.BreaksForDate(ctx, day)
	if err != nil {
		return nil, err
	}
	rep := &NostroReconReport{
		Date: day, Runs: runs, Breaks: breaks,
		AccountsReconciled: len(runs),
	}
	if rep.Runs == nil {
		rep.Runs = []NostroReconRun{}
	}
	if rep.Breaks == nil {
		rep.Breaks = []NostroReconBreak{}
	}
	for _, r := range rep.Runs {
		if r.ThresholdBreach {
			rep.ThresholdBreaches++
		}
	}
	for _, b := range rep.Breaks {
		if b.Status == NostroBreakOpen || b.Status == NostroBreakInvestigating {
			rep.OpenBreaks++
		}
	}
	return rep, nil
}

// AssignBreak moves OPEN → INVESTIGATING with an owner — the
// investigation-workflow entry point.
func (s *NostroReconService) AssignBreak(ctx context.Context, breakID, actor int64) (*NostroReconBreak, error) {
	if breakID <= 0 || actor <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "break id and actor are required")
	}
	applied, err := s.store.AssignBreak(ctx, breakID, actor, s.clock().UTC())
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("open recon break %d not found", breakID))
	}
	return &NostroReconBreak{ID: breakID, Status: NostroBreakInvestigating, AssignedTo: &actor}, nil
}

// ResolveBreak closes an OPEN/INVESTIGATING break with notes —
// idempotent on already-terminal rows (NOT_FOUND when nothing applied).
func (s *NostroReconService) ResolveBreak(ctx context.Context, breakID int64, notes string, actor int64) (*NostroReconBreak, error) {
	if breakID <= 0 || actor <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "break id and actor are required")
	}
	applied, err := s.store.ResolveBreak(ctx, breakID, notes, actor, s.clock().UTC())
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("open recon break %d not found", breakID))
	}
	return &NostroReconBreak{ID: breakID, Status: NostroBreakResolved,
		ResolutionNotes: notes, ResolvedBy: &actor}, nil
}

func nostroFirstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// PgNostroStore implementation
// ---------------------------------------------------------------------------

func (s *PgNostroStore) UpsertStatementEntries(ctx context.Context, entries []NostroStatementEntry) (int, error) {
	inserted := 0
	for _, e := range entries {
		var id int64
		src := e.Source
		if src == "" {
			src = "MT940"
		}
		err := s.Pool.QueryRow(ctx, `
			INSERT INTO nostro_statement_entries
			    (nostro_account_id, statement_date, swift_reference, direction,
			     amount, currency, closing_balance, narrative, source)
			VALUES ($1,$2,$3,$4::nostro_movement_direction_enum,$5::numeric,
			        $6,$7::numeric,NULLIF($8,''),$9)
			ON CONFLICT (nostro_account_id, statement_date, swift_reference,
			             direction, amount) DO NOTHING
			RETURNING id`,
			e.NostroAccountID, e.StatementDate, e.SwiftReference, e.Direction,
			e.Amount.String(), e.Currency, nostroDecStrPtr(e.ClosingBalance),
			e.Narrative, src).Scan(&id)
		if err == pgx.ErrNoRows {
			continue // idempotent replay
		}
		if err != nil {
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}

func nostroDecStrPtr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}

func (s *PgNostroStore) StatementEntriesFor(ctx context.Context, accountID int64, day time.Time) ([]NostroStatementEntry, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, nostro_account_id, statement_date, swift_reference,
		       direction::text, amount::text, currency, closing_balance::text,
		       COALESCE(narrative,''), source, created_at
		  FROM nostro_statement_entries
		 WHERE nostro_account_id = $1 AND statement_date = $2
		 ORDER BY id`, accountID, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NostroStatementEntry
	for rows.Next() {
		e, err := scanNostroEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func scanNostroEntry(row pgx.Row) (*NostroStatementEntry, error) {
	var e NostroStatementEntry
	var amt string
	var closing *string
	err := row.Scan(&e.ID, &e.NostroAccountID, &e.StatementDate,
		&e.SwiftReference, &e.Direction, &amt, &e.Currency, &closing,
		&e.Narrative, &e.Source, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	e.Amount, err = decimal.NewFromString(amt)
	if err != nil {
		return nil, fmt.Errorf("entry %d amount %q: %w", e.ID, amt, err)
	}
	if closing != nil {
		v, err := decimal.NewFromString(*closing)
		if err != nil {
			return nil, fmt.Errorf("entry %d closing %q: %w", e.ID, *closing, err)
		}
		e.ClosingBalance = &v
	}
	return &e, nil
}

func (s *PgNostroStore) MovementsInWindow(ctx context.Context, accountID int64, from, to time.Time) ([]NostroMovement, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+movementCols+`
		  FROM nostro_movements m
		  LEFT JOIN settlement_instructions si
		    ON si.id = m.settlement_instruction_id
		 WHERE m.nostro_account_id = $1
		   AND m.created_at >= $2 AND m.created_at < $3
		 ORDER BY m.id`, accountID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NostroMovement
	for rows.Next() {
		m, err := scanMovement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *PgNostroStore) InsertReconRun(ctx context.Context, r *NostroReconRun) error {
	return s.Pool.QueryRow(ctx, `
		INSERT INTO nostro_recon_runs
		    (nostro_account_id, recon_date, our_net, statement_net, difference,
		     threshold_breach, breaks_opened, breaks_auto_resolved, status)
		VALUES ($1,$2,$3::numeric,$4::numeric,$5::numeric,$6,$7,$8,
		        $9::nostro_recon_status_enum)
		RETURNING id, created_at`,
		r.NostroAccountID, r.ReconDate, r.OurNet.String(),
		r.StatementNet.String(), r.Difference.String(), r.ThresholdBreach,
		r.BreaksOpened, r.BreaksAutoResolved, r.Status).
		Scan(&r.ID, &r.CreatedAt)
}

func (s *PgNostroStore) LatestRunsForDate(ctx context.Context, day time.Time) ([]NostroReconRun, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT ON (nostro_account_id)
		       id, nostro_account_id, recon_date, our_net::text,
		       statement_net::text, difference::text, threshold_breach,
		       breaks_opened, breaks_auto_resolved, status::text, created_at
		  FROM nostro_recon_runs
		 WHERE recon_date = $1
		 ORDER BY nostro_account_id, id DESC`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NostroReconRun
	for rows.Next() {
		r, err := scanNostroRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func scanNostroRun(row pgx.Row) (*NostroReconRun, error) {
	var r NostroReconRun
	var a, b, d string
	err := row.Scan(&r.ID, &r.NostroAccountID, &r.ReconDate, &a, &b, &d,
		&r.ThresholdBreach, &r.BreaksOpened, &r.BreaksAutoResolved,
		&r.Status, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	if r.OurNet, err = decimal.NewFromString(a); err != nil {
		return nil, err
	}
	if r.StatementNet, err = decimal.NewFromString(b); err != nil {
		return nil, err
	}
	if r.Difference, err = decimal.NewFromString(d); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *PgNostroStore) InsertBreak(ctx context.Context, b *NostroReconBreak) error {
	return s.Pool.QueryRow(ctx, `
		INSERT INTO nostro_recon_breaks
		    (run_id, nostro_account_id, recon_date, category,
		     nostro_movement_id, statement_entry_id, swift_reference,
		     currency, expected_amount, actual_amount, difference, status)
		VALUES ($1,$2,$3,$4::nostro_break_category_enum,$5,$6,NULLIF($7,''),
		        NULLIF($8,''),$9::numeric,$10::numeric,$11::numeric,
		        'OPEN'::nostro_break_status_enum)
		RETURNING id, detected_at`,
		b.RunID, b.NostroAccountID, b.ReconDate, string(b.Category),
		b.MovementID, b.StatementEntryID, b.SwiftReference, b.Currency,
		nostroDecStrPtr(b.ExpectedAmount), nostroDecStrPtr(b.ActualAmount),
		nostroDecStrPtr(b.Difference)).Scan(&b.ID, &b.DetectedAt)
}

func (s *PgNostroStore) BreaksForDate(ctx context.Context, day time.Time) ([]NostroReconBreak, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+nostroBreakCols+`
		  FROM nostro_recon_breaks WHERE recon_date = $1 ORDER BY id`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNostroBreaks(rows)
}

func (s *PgNostroStore) OpenBreaksFor(ctx context.Context, accountID int64, day time.Time) ([]NostroReconBreak, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+nostroBreakCols+`
		  FROM nostro_recon_breaks
		 WHERE nostro_account_id = $1 AND recon_date = $2
		   AND status IN ('OPEN','INVESTIGATING')
		 ORDER BY id`, accountID, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNostroBreaks(rows)
}

const nostroBreakCols = `id, run_id, nostro_account_id, recon_date, category::text,
	nostro_movement_id, statement_entry_id, COALESCE(swift_reference,''),
	COALESCE(currency,''), expected_amount::text, actual_amount::text,
	difference::text, status::text, assigned_to, COALESCE(resolution_notes,''),
	detected_at, resolved_at, resolved_by`

func scanNostroBreaks(rows pgx.Rows) ([]NostroReconBreak, error) {
	var out []NostroReconBreak
	for rows.Next() {
		b, err := scanNostroBreak(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func scanNostroBreak(row pgx.Row) (*NostroReconBreak, error) {
	var b NostroReconBreak
	var exp, act, diff *string
	err := row.Scan(&b.ID, &b.RunID, &b.NostroAccountID, &b.ReconDate,
		(*string)(&b.Category), &b.MovementID, &b.StatementEntryID,
		&b.SwiftReference, &b.Currency, &exp, &act, &diff,
		(*string)(&b.Status), &b.AssignedTo, &b.ResolutionNotes,
		&b.DetectedAt, &b.ResolvedAt, &b.ResolvedBy)
	if err != nil {
		return nil, err
	}
	if b.ExpectedAmount, err = nostroDecFromPtr(exp); err != nil {
		return nil, err
	}
	if b.ActualAmount, err = nostroDecFromPtr(act); err != nil {
		return nil, err
	}
	if b.Difference, err = nostroDecFromPtr(diff); err != nil {
		return nil, err
	}
	return &b, nil
}

func nostroDecFromPtr(s *string) (*decimal.Decimal, error) {
	if s == nil {
		return nil, nil
	}
	d, err := decimal.NewFromString(*s)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *PgNostroStore) ResolveBreak(ctx context.Context, breakID int64, notes string, by int64, at time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE nostro_recon_breaks
		   SET status = 'RESOLVED', resolution_notes = NULLIF($2,''),
		       resolved_by = $3, resolved_at = $4
		 WHERE id = $1 AND status IN ('OPEN','INVESTIGATING')`,
		breakID, notes, by, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PgNostroStore) AssignBreak(ctx context.Context, breakID, actor int64, at time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE nostro_recon_breaks
		   SET status = 'INVESTIGATING', assigned_to = $2
		 WHERE id = $1 AND status = 'OPEN'`, breakID, actor)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PgNostroStore) AutoResolveForMovement(ctx context.Context, movementID int64, at time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE nostro_recon_breaks
		   SET status = 'AUTO_RESOLVED', resolved_at = $2
		 WHERE nostro_movement_id = $1
		   AND status IN ('OPEN','INVESTIGATING')`, movementID, at)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
