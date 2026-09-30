// Task 24.3.7 — Prime Brokerage Give-Up reconciliation & break management
// (spec §5.22, §17.1, §24 #125).
//
// Reconciles pb_giveup_trades (written by the Phase-18 Task 18.3.6 drop
// copy) against PB affirmation feeds (Traiana Harmony / MarkitSERV) and
// EOD electronic broker blotters via the AffirmationFeed/BlotterFeed
// adapter seams. Auto-match correlates on external_trade_id (the venue
// message id / traiana_message_id), currency pair, notional and executed
// rate inside the per-currency recon_tolerances bands (§17.14.1).
//
// Break detection flags rate mismatches, quantity mismatches, missing
// tickets and trades still PENDING past the affirmation timeout (default
// 60s — the same window Task 18.3.6's AffirmationMonitor uses). Breaks
// carry the middle-office workflow PENDING→AFFIRMED/REJECTED/DISPUTED
// (guarded pb_giveup_trades transitions) with a pb_recon_events audit
// trail; aging/escalation/write-off live in Task 24.3.19
// (ops_hardening.go).
//
// Collateral rebalance invariant (spec §13.9/§5.22, remediation #38):
// when a give-up transfers exposure from the executing-broker account to
// the PB client account, the allocated initial-margin collateral moves
// inside the SAME SERIALIZABLE transaction as the status flip —
// balances.locked released on the source, recomputed and locked on the
// destination; insufficient destination free balance aborts the whole
// thing with INSUFFICIENT_MARGIN (HTTP 409).
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

// AffirmationTimeout is the default un-affirmed window before a give-up
// becomes an UNAFFIRMED_TIMEOUT break (Task 24.3.7 step 2 / 18.3.6 step 5).
const AffirmationTimeout = 60 * time.Second

// AutoMatchKPI is the §17.14.1 auto-match-rate floor — runs below it
// raise an ops alert.
const AutoMatchKPI = 98.0

// ---------------------------------------------------------------------------
// Feed adapter seams — Traiana/MarkitSERV affirmation feeds + EOD blotters
// ---------------------------------------------------------------------------

// AffirmationRecord is the venue-normalized view of one PB-side trade
// report — the adapter (Traiana Harmony / MarkitSERV JSON, or an EOD
// electronic broker blotter row) maps its dialect onto this shape.
type AffirmationRecord struct {
	ExternalTradeID string          // venue trade ref — correlates traiana_message_id / trade id
	TradeID         int64           // our trade id when the venue echoes it
	Pair            string          // compact "EURUSD"
	Notional        decimal.Decimal // base-currency quantity
	Rate            decimal.Decimal // executed rate
	Status          string          // AFFIRMED | REJECTED (venue vocabulary, normalized)
	Reason          string          // reject narrative
	MessageID       string          // venue message id for correlation
	At              time.Time
}

// Affirmed / Rejected are the normalized feed dispositions.
const (
	FeedAffirmed = "AFFIRMED"
	FeedRejected = "REJECTED"
)

// AffirmationFeed is the PB affirmation-feed adapter seam (Traiana
// Harmony / MarkitSERV). Fetch returns all venue records for the PB on
// the run date.
type AffirmationFeed interface {
	Fetch(ctx context.Context, pbID int64, day time.Time) ([]AffirmationRecord, error)
}

// BlotterFeed is the EOD electronic-broker-blotter adapter seam — same
// record shape, distinct source label for run accounting.
type BlotterFeed interface {
	Fetch(ctx context.Context, pbID int64, day time.Time) ([]AffirmationRecord, error)
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// GiveUpRow is the pb_giveup_trades view the reconciler needs.
type GiveUpRow struct {
	ID               int64
	TradeID          int64
	PrimeBrokerID    int64
	ExecutingAcctID  int64
	ClientAcctID     int64
	Status           string // PENDING|AFFIRMED|REJECTED|DISPUTED|SETTLED
	TraianaMessageID string
	RejectionReason  string
	CreatedAt        time.Time
}

// TradeRow is the trades view for tolerance comparison.
type TradeRow struct {
	ID       int64
	Symbol   string // compact "EURUSD"
	Price    decimal.Decimal
	Quantity decimal.Decimal // base units
}

// PBReconRun is one pb_recon_runs row.
type PBReconRun struct {
	ID             int64
	PrimeBrokerID  int64
	RunDate        time.Time
	Source         string // AFFIRMATION | BLOTTER
	TradesScanned  int
	AutoMatched    int
	BreaksDetected int
	AutoMatchRate  decimal.Decimal // pct
	CreatedAt      time.Time
}

// BreakType mirrors the pb_recon_breaks CHECK list.
type BreakType string

const (
	BreakRateMismatch      BreakType = "RATE_MISMATCH"
	BreakQuantityMismatch  BreakType = "QUANTITY_MISMATCH"
	BreakPairMismatch      BreakType = "PAIR_MISMATCH"
	BreakMissingTicket     BreakType = "MISSING_TICKET"
	BreakUnaffirmedTimeout BreakType = "UNAFFIRMED_TIMEOUT"
	BreakMissingAtPB       BreakType = "MISSING_AT_PB"   // we booked, PB never reported (EOD)
	BreakMissingLocally    BreakType = "MISSING_LOCALLY" // PB reported a ticket we never booked
)

// GiveUpBreakStatus mirrors the pb_recon_breaks status CHECK list.
type GiveUpBreakStatus string

const (
	PBBreakOpen          GiveUpBreakStatus = "OPEN"
	PBBreakInvestigating GiveUpBreakStatus = "INVESTIGATING"
	PBBreakResolved      GiveUpBreakStatus = "RESOLVED"
	PBBreakWrittenOff    GiveUpBreakStatus = "WRITTEN_OFF"
)

// PBReconBreak is one pb_recon_breaks row.
type PBReconBreak struct {
	ID             int64
	RunID          int64
	GiveUpTradeID  *int64
	PrimeBrokerID  int64
	Type           BreakType
	Status         GiveUpBreakStatus
	Expected       map[string]any
	Actual         map[string]any
	AssignedTo     *int64
	ResolutionNote string
	EscalatedAt    *time.Time
	ResolvedBy     *int64
	CreatedAt      time.Time
	ResolvedAt     *time.Time
}

// PBBreakTolerance is the per-currency auto-match band (recon_tolerances).
type PBBreakTolerance struct {
	Currency        string
	AmountAbs       decimal.Decimal // absolute notional tolerance
	AmountPct       decimal.Decimal // relative notional tolerance (0.0001 = 1bp)
	RateToleranceBP decimal.Decimal
}

// ---------------------------------------------------------------------------
// Store seam
// ---------------------------------------------------------------------------

// PBReconStore is the persistence seam for the reconciler.
type PBReconStore interface {
	// GiveUpsFor returns the PB's give-up trades booked on/before the run
	// day that are not terminal-SETTLED (recon covers the open set).
	GiveUpsFor(ctx context.Context, pbID int64, day time.Time) ([]GiveUpRow, error)
	// TradeByID resolves the executed rate/quantity/symbol for a trade.
	TradeByID(ctx context.Context, id int64) (TradeRow, bool, error)
	// InsertRun records the run (UNIQUE pb+date+source replays update).
	InsertRun(ctx context.Context, r PBReconRun) (PBReconRun, error)
	// InsertBreak records a break idempotently — a matching OPEN/
	// INVESTIGATING row on the same give-up+type is returned, not duplicated.
	InsertBreak(ctx context.Context, b PBReconBreak) (PBReconBreak, bool, error)
	// RunsFor lists runs; BreaksFor lists breaks (admin report view).
	RunsFor(ctx context.Context, pbID int64, day time.Time) ([]PBReconRun, error)
	BreaksFor(ctx context.Context, pbID int64, day time.Time) ([]PBReconBreak, error)
	// ToleranceFor resolves the per-currency match band; nil row →
	// default USD-style band (documented default, never a hard fail).
	ToleranceFor(ctx context.Context, currency string) (PBBreakTolerance, error)
	// InTx runs fn inside a SERIALIZABLE transaction.
	InTx(ctx context.Context, fn func(ctx context.Context, tx PBReconTx) error) error
}

// PBReconTx is the transactional view inside PBReconStore.InTx — also the
// shape handed to the dual-control executor via ReconTxFromPgx.
type PBReconTx interface {
	GiveUpForUpdate(ctx context.Context, id int64) (GiveUpRow, bool, error)
	// SetGiveUpStatus applies a guarded PENDING→AFFIRMED/REJECTED/DISPUTED/
	// SETTLED transition (from→to whitelist enforced at SQL level).
	SetGiveUpStatus(ctx context.Context, id int64, to, reason string) error
	InsertBreak(ctx context.Context, b PBReconBreak) (PBReconBreak, bool, error)
	AppendReconEvent(ctx context.Context, breakID, giveUpID *int64, actorID *int64,
		action string, detail map[string]any) error
	ResolveBreak(ctx context.Context, id int64, note string, resolverID int64, at time.Time) error
	AssignBreak(ctx context.Context, id, assignee int64, at time.Time) error
	// MoveLocked atomically releases releaseAmt of locked collateral on
	// the source account and locks lockAmt on the destination — the
	// §13.9/§5.22 give-up collateral rebalance. Returns
	// INSUFFICIENT_MARGIN-coded error when the destination cannot cover.
	MoveLocked(ctx context.Context, giveUpID, fromAcct, toAcct int64,
		currency string, releaseAmt, lockAmt decimal.Decimal) error
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// PBReconOptions configure the reconciler.
type PBReconOptions struct {
	// Timeout overrides the 60s un-affirmed window (tests).
	Timeout time.Duration
	// Clock overrides time.Now (tests).
	Clock func() time.Time
	// OnAlert fires on KPI misses / new breaks — wire to the ops-alert
	// taxonomy (funding_ops_alerts / PagerDuty).
	OnAlert func(ctx context.Context, code, summary string, detail map[string]any)
}

// PBReconService reconciles give-ups against PB feeds.
type PBReconService struct {
	store PBReconStore
	opts  PBReconOptions
	now   func() time.Time
}

// NewPBReconService wires the service; nil store fails closed.
func NewPBReconService(store PBReconStore, opts PBReconOptions) *PBReconService {
	now := opts.Clock
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &PBReconService{store: store, opts: opts, now: now}
}

func (s *PBReconService) timeout() time.Duration {
	if s.opts.Timeout > 0 {
		return s.opts.Timeout
	}
	return AffirmationTimeout
}

func (s *PBReconService) alert(ctx context.Context, code, summary string, detail map[string]any) {
	if s.opts.OnAlert != nil {
		s.opts.OnAlert(ctx, code, summary, detail)
	}
}

// Reconcile runs one reconciliation pass for (pb, day, source): loads the
// open give-up set and the feed records, auto-matches inside tolerance,
// raises breaks for discrepancies/timeouts/missing tickets, and persists
// the run with its auto-match rate (KPI feed for §17.14.1).
func (s *PBReconService) Reconcile(ctx context.Context, pbID int64, day time.Time,
	source string, feed AffirmationFeed) (*PBReconRun, []PBReconBreak, error) {
	if s.store == nil {
		return nil, nil, excerrors.New(CodeServiceDegraded, "pb recon store not configured")
	}
	if feed == nil {
		return nil, nil, excerrors.New(CodeServiceDegraded, "affirmation feed not configured")
	}
	if source != "AFFIRMATION" && source != "BLOTTER" {
		return nil, nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unknown recon source %q", source))
	}
	day = day.UTC().Truncate(24 * time.Hour)

	records, err := feed.Fetch(ctx, pbID, day)
	if err != nil {
		return nil, nil, fmt.Errorf("pb recon: feed fetch pb %d: %w", pbID, err)
	}
	giveups, err := s.store.GiveUpsFor(ctx, pbID, day)
	if err != nil {
		return nil, nil, fmt.Errorf("pb recon: give-ups pb %d: %w", pbID, err)
	}

	run := PBReconRun{PrimeBrokerID: pbID, RunDate: day, Source: source}
	matched := map[int64]AffirmationRecord{} // giveup id → matched record
	var breaks []PBReconBreak

	// Index feed records by external trade ref + trade id.
	byExt := map[string]AffirmationRecord{}
	byTrade := map[int64]AffirmationRecord{}
	var orphanRecords []AffirmationRecord
	for _, r := range records {
		if r.ExternalTradeID != "" {
			byExt[r.ExternalTradeID] = r
		}
		if r.TradeID != 0 {
			byTrade[r.TradeID] = r
		}
	}
	consume := func(g GiveUpRow) (AffirmationRecord, bool) {
		if r, ok := byExt[g.TraianaMessageID]; ok && g.TraianaMessageID != "" {
			return r, true
		}
		if r, ok := byTrade[g.TradeID]; ok {
			return r, true
		}
		return AffirmationRecord{}, false
	}

	cutoff := s.now().Add(-s.timeout())
	for _, g := range giveups {
		run.TradesScanned++
		rec, ok := consume(g)
		if !ok {
			// Un-affirmed past the window → timeout break (real-time
			// view); at EOD the blotter absence is MISSING_AT_PB.
			if g.Status == "PENDING" && g.CreatedAt.Before(cutoff) {
				bt := BreakUnaffirmedTimeout
				if source == "BLOTTER" {
					bt = BreakMissingAtPB
				}
				br, created, err := s.openBreak(ctx, run.ID, g, bt,
					map[string]any{"status": g.Status, "created_at": g.CreatedAt}, nil)
				if err != nil {
					return nil, nil, err
				}
				if created {
					breaks = append(breaks, br)
					run.BreaksDetected++
				}
			}
			continue
		}
		matched[g.ID] = rec
		// Tolerance comparison — rate and notional against the booked trade.
		tr, found, err := s.store.TradeByID(ctx, g.TradeID)
		if err != nil {
			return nil, nil, fmt.Errorf("pb recon: trade %d: %w", g.TradeID, err)
		}
		if !found {
			br, created, err := s.openBreak(ctx, run.ID, g, BreakMissingTicket,
				map[string]any{"trade_id": g.TradeID}, recToMap(rec))
			if err != nil {
				return nil, nil, err
			}
			if created {
				breaks = append(breaks, br)
				run.BreaksDetected++
			}
			continue
		}
		tol, err := s.store.ToleranceFor(ctx, quoteCcy(tr.Symbol))
		if err != nil {
			return nil, nil, fmt.Errorf("pb recon: tolerance %s: %w", tr.Symbol, err)
		}
		bt := matchBreak(tr, rec, tol)
		if bt != "" {
			br, created, err := s.openBreak(ctx, run.ID, g, bt,
				map[string]any{"rate": tr.Price.String(), "qty": tr.Quantity.String()},
				recToMap(rec))
			if err != nil {
				return nil, nil, err
			}
			if created {
				breaks = append(breaks, br)
				run.BreaksDetected++
			}
			// Discrepancy parks the give-up in DISPUTED for investigation.
			if g.Status == "PENDING" || g.Status == "AFFIRMED" {
				if err := s.transition(ctx, g.ID, "DISPUTED",
					fmt.Sprintf("%s on recon run", bt)); err != nil {
					return nil, nil, err
				}
			}
			continue
		}
		// In-tolerance: apply the feed disposition.
		to := "AFFIRMED"
		if rec.Status == FeedRejected {
			to = "REJECTED"
		}
		if g.Status == "PENDING" || g.Status == "DISPUTED" {
			if err := s.transition(ctx, g.ID, to, rec.Reason); err != nil {
				return nil, nil, err
			}
		}
		if to == "AFFIRMED" {
			run.AutoMatched++
		}
	}

	// PB-reported tickets we never booked → MISSING_LOCALLY.
	seenRec := map[string]bool{}
	for _, r := range matched {
		if r.MessageID != "" {
			seenRec[r.MessageID] = true
		}
		if r.ExternalTradeID != "" {
			seenRec[r.ExternalTradeID] = true
		}
	}
	for _, r := range records {
		key := r.MessageID
		if key == "" {
			key = r.ExternalTradeID
		}
		if key == "" || seenRec[key] || seenRec[r.ExternalTradeID] {
			continue
		}
		br, created, err := s.openBreak(ctx, run.ID, GiveUpRow{PrimeBrokerID: pbID},
			BreakMissingLocally, nil, recToMap(r))
		if err != nil {
			return nil, nil, err
		}
		if created {
			breaks = append(breaks, br)
			run.BreaksDetected++
		}
		_ = orphanRecords
	}

	if run.TradesScanned > 0 {
		run.AutoMatchRate = decimal.NewFromInt(int64(run.AutoMatched)).
			Mul(decimal.NewFromInt(100)).
			Div(decimal.NewFromInt(int64(run.TradesScanned)))
	}
	run, err = s.store.InsertRun(ctx, run)
	if err != nil {
		return nil, nil, fmt.Errorf("pb recon: insert run: %w", err)
	}
	if run.AutoMatchRate.IsPositive() &&
		run.AutoMatchRate.LessThan(decimal.NewFromFloat(AutoMatchKPI)) {
		s.alert(ctx, "PB_RECON_MATCH_RATE", fmt.Sprintf(
			"pb %d %s recon auto-match %.2f%% below %.0f%% KPI",
			pbID, day.Format("2006-01-02"), run.AutoMatchRate.InexactFloat64(), AutoMatchKPI),
			map[string]any{"run_id": run.ID})
	}
	for _, br := range breaks {
		s.alert(ctx, "PB_RECON_BREAK", fmt.Sprintf(
			"pb %d %s break give-up %v", pbID, br.Type,
			derefInt64(br.GiveUpTradeID)), map[string]any{"break_id": br.ID})
	}
	return &run, breaks, nil
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func recToMap(r AffirmationRecord) map[string]any {
	return map[string]any{
		"external_trade_id": r.ExternalTradeID, "trade_id": r.TradeID,
		"pair": r.Pair, "notional": r.Notional.String(), "rate": r.Rate.String(),
		"status": r.Status, "reason": r.Reason, "message_id": r.MessageID,
	}
}

// quoteCcy extracts the quote currency from a compact 6-letter pair.
func quoteCcy(symbol string) string {
	if len(symbol) >= 6 {
		return symbol[len(symbol)-3:]
	}
	return symbol
}

// matchBreak returns the break type for a field-level violation, or "".
// Pair is compared first — a wrong-instrument affirmation is never a
// rate/quantity discrepancy.
func matchBreak(tr TradeRow, rec AffirmationRecord, tol PBBreakTolerance) BreakType {
	if rec.Pair != "" && normSymbol(rec.Pair) != normSymbol(tr.Symbol) {
		return BreakPairMismatch
	}
	if !rec.Rate.IsZero() && !tr.Price.IsZero() {
		diffBps := rec.Rate.Sub(tr.Price).Abs().
			Div(tr.Price).Mul(decimal.NewFromInt(10000))
		if tol.RateToleranceBP.IsPositive() && diffBps.GreaterThan(tol.RateToleranceBP) {
			return BreakRateMismatch
		}
	}
	if !rec.Notional.IsZero() && !tr.Quantity.IsZero() {
		diff := rec.Notional.Sub(tr.Quantity).Abs()
		bound := tol.AmountAbs
		if tol.AmountPct.IsPositive() {
			rel := tr.Quantity.Mul(tol.AmountPct)
			if rel.GreaterThan(bound) {
				bound = rel
			}
		}
		if bound.IsPositive() && diff.GreaterThan(bound) {
			return BreakQuantityMismatch
		}
	}
	return ""
}

func (s *PBReconService) openBreak(ctx context.Context, runID int64,
	g GiveUpRow, bt BreakType, expected, actual map[string]any) (PBReconBreak, bool, error) {
	var gid *int64
	if g.ID != 0 {
		gid = &g.ID
	}
	br, created, err := s.store.InsertBreak(ctx, PBReconBreak{
		RunID: runID, GiveUpTradeID: gid, PrimeBrokerID: g.PrimeBrokerID,
		Type: bt, Status: PBBreakOpen, Expected: expected, Actual: actual,
	})
	if err != nil {
		return PBReconBreak{}, false, fmt.Errorf("pb recon: open break %s: %w", bt, err)
	}
	return br, created, nil
}

// transition applies the guarded give-up status move.
func (s *PBReconService) transition(ctx context.Context, giveUpID int64, to, reason string) error {
	return s.store.InTx(ctx, func(ctx context.Context, tx PBReconTx) error {
		if err := tx.SetGiveUpStatus(ctx, giveUpID, to, reason); err != nil {
			return err
		}
		return tx.AppendReconEvent(ctx, nil, &giveUpID, nil,
			"giveup_status", map[string]any{"to": to, "reason": reason})
	})
}

// ---------------------------------------------------------------------------
// Break workflow — assign / resolve with audit
// ---------------------------------------------------------------------------

// AssignBreak parks a break with a middle-office investigator.
func (s *PBReconService) AssignBreak(ctx context.Context, breakID, assignee int64) error {
	if s.store == nil {
		return excerrors.New(CodeServiceDegraded, "pb recon store not configured")
	}
	if assignee <= 0 {
		return excerrors.New("INVALID_REQUEST", "assignee is required")
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx PBReconTx) error {
		if err := tx.AssignBreak(ctx, breakID, assignee, s.now()); err != nil {
			return err
		}
		return tx.AppendReconEvent(ctx, &breakID, nil, &assignee, "assign", nil)
	})
}

// ResolveBreak closes a break and applies the give-up disposition the
// investigator chose: AFFIRMED / REJECTED / DISPUTED (or RESOLVED with no
// status move when the break was informational). Guarded transitions make
// replays idempotent; a RESOLVED/WRITTEN_OFF break rejects re-resolution.
func (s *PBReconService) ResolveBreak(ctx context.Context, tx PBReconTx,
	breakID int64, disposition string, note string, resolverID int64) error {
	if tx == nil {
		return excerrors.New(CodeServiceDegraded, "pb recon tx not configured")
	}
	br, err := lockBreak(ctx, tx, breakID)
	if err != nil {
		return err
	}
	if br.Status == PBBreakResolved || br.Status == PBBreakWrittenOff {
		return excerrors.New(CodeBreakConflict, fmt.Sprintf(
			"break %d already %s", breakID, br.Status))
	}
	switch disposition {
	case "AFFIRMED", "REJECTED", "DISPUTED", "RESOLVED":
	default:
		return excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unknown break disposition %q", disposition))
	}
	if br.GiveUpTradeID != nil && disposition != "RESOLVED" {
		if err := tx.SetGiveUpStatus(ctx, *br.GiveUpTradeID, disposition, note); err != nil {
			return excerrors.Wrap(CodeBreakConflict, "give-up transition", err)
		}
	}
	if err := tx.ResolveBreak(ctx, breakID, note, resolverID, s.now()); err != nil {
		return err
	}
	return tx.AppendReconEvent(ctx, &breakID, br.GiveUpTradeID, &resolverID,
		"resolve", map[string]any{"disposition": disposition, "note": note})
}

// BreakResolutionPayload rides the dual-control request row for the
// break-resolution op — the executor replays it inside the approval tx.
type BreakResolutionPayload struct {
	BreakID     int64  `json:"break_id"`
	Disposition string `json:"disposition"` // AFFIRMED | REJECTED | DISPUTED | RESOLVED
	Note        string `json:"note"`
	ResolverID  int64  `json:"resolver_id"`
}

// DualQueueForBreaks — PBReconService reuses the backoffice DualQueue seam
// declared in exceptions.go.
//
// RequestBreakResolution queues a four-eyes break resolution (money /
// exposure-affecting dispositions AFFIRMED/REJECTED ride the queue; a
// bare RESOLVED disposition is informational and may ride it too — the
// approver still vets it). The mutation lands via ResolveBreak inside the
// approval tx.
func (s *PBReconService) RequestBreakResolution(ctx context.Context, dual DualQueue,
	breakID int64, disposition, note string, resolverID int64, clientIP string) (*DualResult, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "pb recon store not configured")
	}
	if dual == nil {
		return nil, excerrors.New(CodeServiceDegraded, "dual-control queue not configured")
	}
	switch disposition {
	case "AFFIRMED", "REJECTED", "DISPUTED", "RESOLVED":
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unknown break disposition %q", disposition))
	}
	return dual.Submit(ctx, DualSubmit{
		Operation:    OpPBBreakResolve,
		TargetType:   "pb_recon_break",
		TargetID:     fmt.Sprintf("%d", breakID),
		Payload:      BreakResolutionPayload{BreakID: breakID, Disposition: disposition, Note: note, ResolverID: resolverID},
		RequiredRole: "Finance Ops",
		RequestedBy:  resolverID,
		Reason:       fmt.Sprintf("resolve pb recon break %d as %s", breakID, disposition),
		ClientIP:     clientIP,
	})
}

// ResolveBreakDirect wraps ResolveBreak in a service-owned tx (tests /
// internal flows); the REST surface rides the dual-control queue.
func (s *PBReconService) ResolveBreakDirect(ctx context.Context, breakID int64,
	disposition, note string, resolverID int64) error {
	if s.store == nil {
		return excerrors.New(CodeServiceDegraded, "pb recon store not configured")
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx PBReconTx) error {
		return s.ResolveBreak(ctx, tx, breakID, disposition, note, resolverID)
	})
}

// lockBreak loads+locks the break row via the tx (implemented by stores
// that support it through GiveUpForUpdate-style locking; falls back to a
// plain read).
func lockBreak(ctx context.Context, tx PBReconTx, breakID int64) (PBReconBreak, error) {
	if lt, ok := tx.(interface {
		LockBreak(context.Context, int64) (PBReconBreak, bool, error)
	}); ok {
		br, found, err := lt.LockBreak(ctx, breakID)
		if err != nil {
			return PBReconBreak{}, fmt.Errorf("lock break %d: %w", breakID, err)
		}
		if !found {
			return PBReconBreak{}, excerrors.New("NOT_FOUND",
				fmt.Sprintf("recon break %d not found", breakID))
		}
		return br, nil
	}
	return PBReconBreak{}, excerrors.New(CodeServiceDegraded,
		"break store does not support row locking")
}

// ---------------------------------------------------------------------------
// Atomic collateral rebalance (spec §13.9/§5.22 invariant)
// ---------------------------------------------------------------------------

// AffirmAndRebalance performs the give-up affirmation AND the margin
// collateral move in one SERIALIZABLE transaction — the §13.9/24.3.7
// invariant. releaseAmt is the locked collateral freed on the executing
// broker account (clamped to what is actually locked); lockAmt is the
// recomputed initial margin locked on the PB client account — if the
// client's available balance cannot cover it the ENTIRE transaction
// aborts with INSUFFICIENT_MARGIN (HTTP 409) and the give-up stays
// PENDING.
func (s *PBReconService) AffirmAndRebalance(ctx context.Context, giveUpID int64,
	currency string, releaseAmt, lockAmt decimal.Decimal) error {
	if s.store == nil {
		return excerrors.New(CodeServiceDegraded, "pb recon store not configured")
	}
	if releaseAmt.IsNegative() || lockAmt.IsNegative() {
		return excerrors.New("INVALID_REQUEST", "collateral amounts must be >= 0")
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx PBReconTx) error {
		g, found, err := tx.GiveUpForUpdate(ctx, giveUpID)
		if err != nil {
			return fmt.Errorf("lock give-up %d: %w", giveUpID, err)
		}
		if !found {
			return excerrors.New("NOT_FOUND", fmt.Sprintf("give-up %d not found", giveUpID))
		}
		if g.Status != "PENDING" && g.Status != "DISPUTED" {
			return excerrors.New(CodeBreakConflict, fmt.Sprintf(
				"give-up %d is %s — only PENDING/DISPUTED affirm", giveUpID, g.Status))
		}
		// Collateral move FIRST — a shortfall aborts before the status flip.
		if err := tx.MoveLocked(ctx, giveUpID, g.ExecutingAcctID, g.ClientAcctID,
			currency, releaseAmt, lockAmt); err != nil {
			return err // INSUFFICIENT_MARGIN or store failure — atomic abort
		}
		if err := tx.SetGiveUpStatus(ctx, giveUpID, "AFFIRMED", "affirmed+collateral rebalanced"); err != nil {
			return err
		}
		return tx.AppendReconEvent(ctx, nil, &giveUpID, nil, "collateral_rebalance",
			map[string]any{
				"from_account": g.ExecutingAcctID, "to_account": g.ClientAcctID,
				"currency": currency, "released": releaseAmt.String(), "locked": lockAmt.String(),
			})
	})
}

// ---------------------------------------------------------------------------
// Report view (GET /api/v1/admin/pb-reconciliation?pb_id=&date=)
// ---------------------------------------------------------------------------

// PBReconReport is the admin reconciliation export: runs + open breaks.
type PBReconReport struct {
	PrimeBrokerID int64          `json:"prime_broker_id"`
	Date          string         `json:"date"`
	Runs          []PBReconRun   `json:"runs"`
	OpenBreaks    []PBReconBreak `json:"open_breaks"`
	AutoMatchRate string         `json:"auto_match_rate_pct"` // day aggregate
}

// Report builds the daily reconciliation view.
func (s *PBReconService) Report(ctx context.Context, pbID int64, day time.Time) (*PBReconReport, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "pb recon store not configured")
	}
	day = day.UTC().Truncate(24 * time.Hour)
	runs, err := s.store.RunsFor(ctx, pbID, day)
	if err != nil {
		return nil, fmt.Errorf("pb recon runs: %w", err)
	}
	breaks, err := s.store.BreaksFor(ctx, pbID, day)
	if err != nil {
		return nil, fmt.Errorf("pb recon breaks: %w", err)
	}
	var scanned, matched int64
	for _, r := range runs {
		scanned += int64(r.TradesScanned)
		matched += int64(r.AutoMatched)
	}
	rep := &PBReconReport{
		PrimeBrokerID: pbID, Date: day.Format("2006-01-02"),
		Runs: runs, OpenBreaks: breaks, AutoMatchRate: "0",
	}
	if scanned > 0 {
		rep.AutoMatchRate = decimal.NewFromInt(matched).
			Mul(decimal.NewFromInt(100)).
			Div(decimal.NewFromInt(scanned)).StringFixed(4)
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// PgxReconStore — production PBReconStore over pgx
// ---------------------------------------------------------------------------

// PgxReconStore implements PBReconStore over pgx.
type PgxReconStore struct{ Q Querier }

// NewPgxReconStore binds the store to a pool.
func NewPgxReconStore(pool *pgxpool.Pool) *PgxReconStore {
	return &PgxReconStore{Q: pool}
}

// ReconTxFromPgx exposes the tx primitives over a caller-owned pgx.Tx.
func ReconTxFromPgx(tx pgx.Tx) PBReconTx { return pgxReconTx{q: tx} }

const giveupCols = `id, trade_id, prime_broker_id, executing_broker_account_id,
	client_account_id, giveup_status::text, COALESCE(traiana_message_id,''),
	COALESCE(rejection_reason,''), created_at`

func scanGiveUp(row interface{ Scan(...any) error }) (GiveUpRow, error) {
	var g GiveUpRow
	err := row.Scan(&g.ID, &g.TradeID, &g.PrimeBrokerID, &g.ExecutingAcctID,
		&g.ClientAcctID, &g.Status, &g.TraianaMessageID, &g.RejectionReason, &g.CreatedAt)
	return g, err
}

func (s *PgxReconStore) GiveUpsFor(ctx context.Context, pbID int64, day time.Time) ([]GiveUpRow, error) {
	rows, err := s.Q.Query(ctx,
		`SELECT `+giveupCols+` FROM pb_giveup_trades
		 WHERE prime_broker_id=$1 AND created_at < $2 AND giveup_status <> 'SETTLED'
		 ORDER BY id`, pbID, day.Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GiveUpRow
	for rows.Next() {
		g, err := scanGiveUp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *PgxReconStore) TradeByID(ctx context.Context, id int64) (TradeRow, bool, error) {
	var t TradeRow
	var price, qty string
	err := s.Q.QueryRow(ctx, `
		SELECT t.id, COALESCE(i.symbol,''), t.price::text, t.quantity::text
		  FROM trades t LEFT JOIN instruments i ON i.id = t.instrument_id
		 WHERE t.id = $1`, id).Scan(&t.ID, &t.Symbol, &price, &qty)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return TradeRow{}, false, nil
	}
	if err != nil {
		return TradeRow{}, false, err
	}
	t.Symbol = normSymbol(t.Symbol)
	if t.Price, err = decimal.NewFromString(price); err != nil {
		return TradeRow{}, false, fmt.Errorf("trade %d price %q: %w", id, price, err)
	}
	if t.Quantity, err = decimal.NewFromString(qty); err != nil {
		return TradeRow{}, false, fmt.Errorf("trade %d qty %q: %w", id, qty, err)
	}
	return t, true, nil
}

// normSymbol compacts an instrument symbol for pair comparison
// ("EUR/USD" → "EURUSD").
func normSymbol(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c >= 'A' && c <= 'Z' {
			b = append(b, c)
		}
	}
	return string(b)
}

func (s *PgxReconStore) InsertRun(ctx context.Context, r PBReconRun) (PBReconRun, error) {
	var rate any
	if !r.AutoMatchRate.IsZero() {
		rate = r.AutoMatchRate.String()
	}
	err := s.Q.QueryRow(ctx, `
		INSERT INTO pb_recon_runs
		    (prime_broker_id, run_date, source, trades_scanned, auto_matched,
		     breaks_detected, auto_match_rate)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric)
		ON CONFLICT (prime_broker_id, run_date, source) DO UPDATE SET
		    trades_scanned=EXCLUDED.trades_scanned,
		    auto_matched=EXCLUDED.auto_matched,
		    breaks_detected=EXCLUDED.breaks_detected,
		    auto_match_rate=EXCLUDED.auto_match_rate
		RETURNING id, prime_broker_id, run_date, source, trades_scanned,
		          auto_matched, breaks_detected, COALESCE(auto_match_rate::text,'0'), created_at`,
		r.PrimeBrokerID, r.RunDate, r.Source, r.TradesScanned, r.AutoMatched,
		r.BreaksDetected, rate).
		Scan(&r.ID, &r.PrimeBrokerID, &r.RunDate, &r.Source, &r.TradesScanned,
			&r.AutoMatched, &r.BreaksDetected, &rate, &r.CreatedAt)
	if err != nil {
		return r, err
	}
	return r, nil
}

func jsonbArg(v map[string]any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (s *PgxReconStore) InsertBreak(ctx context.Context, b PBReconBreak) (PBReconBreak, bool, error) {
	exp, err := jsonbArg(b.Expected)
	if err != nil {
		return b, false, err
	}
	act, err := jsonbArg(b.Actual)
	if err != nil {
		return b, false, err
	}
	err = s.Q.QueryRow(ctx, `
		INSERT INTO pb_recon_breaks
		    (run_id, giveup_trade_id, prime_broker_id, break_type, status, expected, actual)
		VALUES ($1,$2,$3,$4,'OPEN',$5::jsonb,$6::jsonb)
		ON CONFLICT (giveup_trade_id, break_type)
		    WHERE status IN ('OPEN','INVESTIGATING') DO NOTHING
		RETURNING id, run_id, giveup_trade_id, prime_broker_id, break_type, status, created_at`,
		b.RunID, b.GiveUpTradeID, b.PrimeBrokerID, string(b.Type), exp, act).
		Scan(&b.ID, &b.RunID, &b.GiveUpTradeID, &b.PrimeBrokerID, &b.Type, &b.Status, &b.CreatedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		// Open break already exists for this give-up+type — idempotent.
		var id int64
		if err := s.Q.QueryRow(ctx, `
			SELECT id FROM pb_recon_breaks
			 WHERE giveup_trade_id IS NOT DISTINCT FROM $1 AND break_type=$2
			   AND status IN ('OPEN','INVESTIGATING') LIMIT 1`,
			b.GiveUpTradeID, string(b.Type)).Scan(&id); err != nil {
			return b, false, err
		}
		b.ID = id
		return b, false, nil
	}
	return b, err == nil, err
}

func (s *PgxReconStore) RunsFor(ctx context.Context, pbID int64, day time.Time) ([]PBReconRun, error) {
	q := `SELECT id, prime_broker_id, run_date, source, trades_scanned, auto_matched,
	             breaks_detected, COALESCE(auto_match_rate::text,'0'), created_at
	        FROM pb_recon_runs WHERE run_date=$1`
	args := []any{day}
	if pbID > 0 {
		args = append(args, pbID)
		q += " AND prime_broker_id=$2"
	}
	q += " ORDER BY id"
	rows, err := s.Q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PBReconRun
	for rows.Next() {
		var r PBReconRun
		var rate string
		if err := rows.Scan(&r.ID, &r.PrimeBrokerID, &r.RunDate, &r.Source,
			&r.TradesScanned, &r.AutoMatched, &r.BreaksDetected, &rate, &r.CreatedAt); err != nil {
			return nil, err
		}
		if r.AutoMatchRate, err = decimal.NewFromString(rate); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const pbBreakCols = `id, run_id, giveup_trade_id, prime_broker_id, break_type,
	status, expected, actual, assigned_to, COALESCE(resolution_note,''),
	escalated_at, resolved_by, created_at, resolved_at`

func scanPBBreak(row interface{ Scan(...any) error }) (PBReconBreak, error) {
	var b PBReconBreak
	var exp, act []byte
	err := row.Scan(&b.ID, &b.RunID, &b.GiveUpTradeID, &b.PrimeBrokerID, &b.Type,
		&b.Status, &exp, &act, &b.AssignedTo, &b.ResolutionNote,
		&b.EscalatedAt, &b.ResolvedBy, &b.CreatedAt, &b.ResolvedAt)
	if err != nil {
		return b, err
	}
	if len(exp) > 0 {
		_ = json.Unmarshal(exp, &b.Expected)
	}
	if len(act) > 0 {
		_ = json.Unmarshal(act, &b.Actual)
	}
	return b, nil
}

func (s *PgxReconStore) BreaksFor(ctx context.Context, pbID int64, day time.Time) ([]PBReconBreak, error) {
	q := `SELECT ` + pbBreakCols + ` FROM pb_recon_breaks WHERE created_at::date = $1`
	args := []any{day}
	if pbID > 0 {
		args = append(args, pbID)
		q += " AND prime_broker_id=$2"
	}
	q += " ORDER BY id"
	rows, err := s.Q.Query(ctx, q, args...)
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

// BreaksOpen lists still-open breaks (aging sweep + admin views).
func (s *PgxReconStore) BreaksOpen(ctx context.Context) ([]PBReconBreak, error) {
	rows, err := s.Q.Query(ctx,
		`SELECT `+pbBreakCols+` FROM pb_recon_breaks
		 WHERE status IN ('OPEN','INVESTIGATING') ORDER BY created_at`)
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

func (s *PgxReconStore) ToleranceFor(ctx context.Context, currency string) (PBBreakTolerance, error) {
	var t PBBreakTolerance
	var abs, pct, bp string
	err := s.Q.QueryRow(ctx, `
		SELECT currency, amount_abs::text, amount_pct::text, rate_tolerance_bp::text
		  FROM recon_tolerances WHERE currency=$1`, currency).
		Scan(&t.Currency, &abs, &pct, &bp)
	if stderrors.Is(err, pgx.ErrNoRows) {
		// Documented default (USD-style band) — never a hard failure.
		return PBBreakTolerance{Currency: currency,
			AmountAbs:       decimal.NewFromInt(1000),
			AmountPct:       decimal.NewFromFloat(0.0001),
			RateToleranceBP: decimal.NewFromFloat(0.5)}, nil
	}
	if err != nil {
		return t, err
	}
	if t.AmountAbs, err = decimal.NewFromString(abs); err != nil {
		return t, err
	}
	if t.AmountPct, err = decimal.NewFromString(pct); err != nil {
		return t, err
	}
	if t.RateToleranceBP, err = decimal.NewFromString(bp); err != nil {
		return t, err
	}
	return t, nil
}

func (s *PgxReconStore) InTx(ctx context.Context, fn func(ctx context.Context, tx PBReconTx) error) error {
	return RunInTx(ctx, s.Q, func(q Querier) error {
		return fn(ctx, pgxReconTx{q: q})
	})
}

// pgxReconTx implements PBReconTx over any Querier (service tx or the
// dual-control executor's approval tx).
type pgxReconTx struct{ q Querier }

func (t pgxReconTx) GiveUpForUpdate(ctx context.Context, id int64) (GiveUpRow, bool, error) {
	g, err := scanGiveUp(t.q.QueryRow(ctx,
		`SELECT `+giveupCols+` FROM pb_giveup_trades WHERE id=$1 FOR UPDATE`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return GiveUpRow{}, false, nil
	}
	return g, err == nil, err
}

// giveupTransitions mirrors the fix package's state machine (kept local —
// the enum owner is migration 037/spec §5.22).
var giveupTransitions = map[string][]string{
	"PENDING":  {"AFFIRMED", "REJECTED", "DISPUTED"},
	"AFFIRMED": {"DISPUTED", "SETTLED"},
	"REJECTED": {"DISPUTED"},
	"DISPUTED": {"AFFIRMED", "REJECTED", "SETTLED"},
	"SETTLED":  {},
}

func (t pgxReconTx) SetGiveUpStatus(ctx context.Context, id int64, to, reason string) error {
	var cur string
	err := t.q.QueryRow(ctx,
		`SELECT giveup_status::text FROM pb_giveup_trades WHERE id=$1 FOR UPDATE`, id).Scan(&cur)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("give-up %d not found", id))
	}
	if err != nil {
		return err
	}
	if cur == to {
		return nil // idempotent re-delivery
	}
	legal := false
	for _, s := range giveupTransitions[cur] {
		if s == to {
			legal = true
		}
	}
	if !legal {
		return excerrors.New(CodeBreakConflict, fmt.Sprintf(
			"give-up %d: illegal transition %s -> %s", id, cur, to))
	}
	tag, err := t.q.Exec(ctx, `
		UPDATE pb_giveup_trades
		   SET giveup_status=$2::giveup_status_enum,
		       rejection_reason=CASE WHEN $2='REJECTED' THEN NULLIF($3,'') ELSE rejection_reason END,
		       affirmed_at=CASE WHEN $2='AFFIRMED' THEN now() ELSE affirmed_at END,
		       updated_at=now()
		 WHERE id=$1 AND giveup_status=$4::giveup_status_enum`,
		id, to, reason, cur)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeBreakConflict,
			fmt.Sprintf("give-up %d lost %s mid-transition", id, cur))
	}
	return nil
}

func (t pgxReconTx) InsertBreak(ctx context.Context, b PBReconBreak) (PBReconBreak, bool, error) {
	exp, err := jsonbArg(b.Expected)
	if err != nil {
		return b, false, err
	}
	act, err := jsonbArg(b.Actual)
	if err != nil {
		return b, false, err
	}
	err = t.q.QueryRow(ctx, `
		INSERT INTO pb_recon_breaks
		    (run_id, giveup_trade_id, prime_broker_id, break_type, status, expected, actual)
		VALUES ($1,$2,$3,$4,'OPEN',$5::jsonb,$6::jsonb)
		ON CONFLICT (giveup_trade_id, break_type)
		    WHERE status IN ('OPEN','INVESTIGATING') DO NOTHING
		RETURNING id, run_id, giveup_trade_id, prime_broker_id, break_type, status, created_at`,
		b.RunID, b.GiveUpTradeID, b.PrimeBrokerID, string(b.Type), exp, act).
		Scan(&b.ID, &b.RunID, &b.GiveUpTradeID, &b.PrimeBrokerID, &b.Type, &b.Status, &b.CreatedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return b, false, nil
	}
	return b, err == nil, err
}

func (t pgxReconTx) AppendReconEvent(ctx context.Context, breakID, giveUpID, actorID *int64,
	action string, detail map[string]any) error {
	d, err := jsonbArg(detail)
	if err != nil {
		return err
	}
	var br, g, ac any
	if breakID != nil {
		br = *breakID
	}
	if giveUpID != nil {
		g = *giveUpID
	}
	if actorID != nil {
		ac = *actorID
	}
	_, err = t.q.Exec(ctx, `
		INSERT INTO pb_recon_events (break_id, giveup_id, actor_id, action, detail)
		VALUES ($1,$2,$3,$4,$5::jsonb)`, br, g, ac, action, d)
	return err
}

func (t pgxReconTx) LockBreak(ctx context.Context, breakID int64) (PBReconBreak, bool, error) {
	b, err := scanPBBreak(t.q.QueryRow(ctx,
		`SELECT `+pbBreakCols+` FROM pb_recon_breaks WHERE id=$1 FOR UPDATE`, breakID))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return PBReconBreak{}, false, nil
	}
	return b, err == nil, err
}

func (t pgxReconTx) ResolveBreak(ctx context.Context, id int64, note string,
	resolverID int64, at time.Time) error {
	tag, err := t.q.Exec(ctx, `
		UPDATE pb_recon_breaks
		   SET status='RESOLVED', resolution_note=NULLIF($2,''), resolved_by=$3,
		       resolved_at=$4, updated_at=$4
		 WHERE id=$1 AND status IN ('OPEN','INVESTIGATING')`,
		id, note, resolverID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeBreakConflict,
			fmt.Sprintf("break %d lost open status mid-resolve", id))
	}
	return nil
}

func (t pgxReconTx) AssignBreak(ctx context.Context, id, assignee int64, at time.Time) error {
	tag, err := t.q.Exec(ctx, `
		UPDATE pb_recon_breaks
		   SET status='INVESTIGATING', assigned_to=$2, updated_at=$3
		 WHERE id=$1 AND status IN ('OPEN','INVESTIGATING')`, id, assignee, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("recon break %d not found or already closed", id))
	}
	return nil
}

// MoveLocked — the §13.9/§5.22 atomic collateral rebalance. Both balance
// rows lock FOR UPDATE in deterministic (account_id) order; the source
// release is clamped at its actual locked amount, the destination lock
// requires sufficient available — shortfall aborts the whole transaction
// with INSUFFICIENT_MARGIN (409).
func (t pgxReconTx) MoveLocked(ctx context.Context, giveUpID, fromAcct, toAcct int64,
	currency string, releaseAmt, lockAmt decimal.Decimal) error {
	if fromAcct == toAcct {
		return excerrors.New("INVALID_REQUEST",
			"collateral rebalance requires distinct source/destination accounts")
	}
	// Deterministic lock order prevents deadlocks.
	first, second := fromAcct, toAcct
	if second < first {
		first, second = second, first
	}
	for _, acct := range []int64{first, second} {
		if _, err := t.q.Exec(ctx, `
			INSERT INTO balances (account_id, currency, available, locked)
			VALUES ($1,$2,0,0) ON CONFLICT (account_id, currency) DO NOTHING`,
			acct, currency); err != nil {
			return err
		}
		var avail, locked string
		if err := t.q.QueryRow(ctx, `
			SELECT available::text, locked::text FROM balances
			 WHERE account_id=$1 AND currency=$2 FOR UPDATE`, acct, currency).
			Scan(&avail, &locked); err != nil {
			return err
		}
	}
	// Destination headroom check FIRST — insufficient available aborts
	// before any mutation lands.
	var destAvail string
	if err := t.q.QueryRow(ctx, `
		SELECT available::text FROM balances
		 WHERE account_id=$1 AND currency=$2`, toAcct, currency).Scan(&destAvail); err != nil {
		return err
	}
	avail, err := decimal.NewFromString(destAvail)
	if err != nil {
		return fmt.Errorf("dest balance parse: %w", err)
	}
	if lockAmt.IsPositive() && avail.LessThan(lockAmt) {
		return excerrors.New("INSUFFICIENT_MARGIN", fmt.Sprintf(
			"account %d available %s %s < required margin %s",
			toAcct, avail, currency, lockAmt))
	}
	// Source release — clamped to what is actually locked (locked can
	// never go negative).
	var srcLockedStr string
	if err := t.q.QueryRow(ctx, `
		SELECT locked::text FROM balances
		 WHERE account_id=$1 AND currency=$2`, fromAcct, currency).Scan(&srcLockedStr); err != nil {
		return err
	}
	srcLocked, err := decimal.NewFromString(srcLockedStr)
	if err != nil {
		return fmt.Errorf("src balance parse: %w", err)
	}
	release := releaseAmt
	if release.GreaterThan(srcLocked) {
		release = srcLocked
	}
	if release.IsPositive() {
		if _, err := t.q.Exec(ctx, `
			UPDATE balances SET locked = locked - $3::numeric, available = available + $3::numeric,
			       version = version + 1
			 WHERE account_id=$1 AND currency=$2`, fromAcct, currency, release.String()); err != nil {
			return err
		}
	}
	if lockAmt.IsPositive() {
		if _, err := t.q.Exec(ctx, `
			UPDATE balances SET locked = locked + $3::numeric, available = available - $3::numeric,
			       version = version + 1
			 WHERE account_id=$1 AND currency=$2`, toAcct, currency, lockAmt.String()); err != nil {
			return err
		}
	}
	if _, err := t.q.Exec(ctx, `
		INSERT INTO pb_giveup_collateral_moves
		    (giveup_trade_id, from_account_id, to_account_id, currency, amount)
		VALUES ($1,$2,$3,$4,$5::numeric)`,
		giveUpID, fromAcct, toAcct, currency, lockAmt.String()); err != nil {
		return err
	}
	return nil
}
