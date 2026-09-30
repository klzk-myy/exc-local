// depreciation.go — Phase-20 Task 20.3.15 (spec §16.9, §24 #375):
// leveraged-position depreciation notifications, the MiFID II retail
// 10% rule.
//
// A RETAIL leveraged position (positions.margin_used > 0 joined to
// accounts.client_category = 'RETAIL'; PROFESSIONAL and
// ELIGIBLE_COUNTERPARTY accounts are excluded — the rule is
// retail-only) that has depreciated by a further −10% multiple must
// notify the holder no later than end of business day. Depreciation is
// measured per position, value-relative:
//
//	dep_pct = sign × (mark − entry) / entry × 100     sign = +1 LONG, −1 SHORT
//
// (equivalent to (current_value − open_value)/open_value — quantity
// cancels out of the ratio but is kept for the notice payload's
// depreciated_value).
//
// Episode model (depreciation_episodes, migration 237): one OPEN
// episode per position while it sits in drawdown past a notified
// threshold; deepest_notified_pct is the high-water mark of notified
// −10% multiples so each multiple notifies exactly once per episode.
// Recovery above −5% RESETs the episode — a subsequent re-crossing
// opens episode_seq+1 and notifies again. The notification_deliveries
// log (RecentDeliveries) could not carry this state: it is per-user,
// capped at 500 rows, and append-only (cannot express episode reset).
//
// Cadence: HourlyJob evaluates open positions; EndOfDaySweep re-runs
// the open set (intra-hour crossings that recovered are still recorded
// by the hourly pass — episode rows are durable) and additionally
// evaluates positions flattened since the UTC day start so a position
// closed intra-day while below a threshold still emits the "notice on
// close" the task requires. The event is critical (bypasses quiet
// hours): same-business-day delivery is the statutory duty.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/notifications"
	"exchange/internal/oracle"
	"exchange/internal/risk"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// DepMarkSource is the live-mark seam — *oracle.Provider satisfies it
// (the oracle.MarkReader contract). A mark read that fails, is missing
// or is stale fails the position's evaluation closed (a statutory
// notice on a stale mark is worse than a deferred one — the error is
// surfaced in the sweep result, never swallowed).
type DepMarkSource interface {
	Mark(ctx context.Context, symbol string) (oracle.MarkView, error)
}

// DepPosition is one position leg under evaluation. CloseMark, when
// non-nil, replaces the live oracle read (flat positions evaluate at
// their stored close mark).
type DepPosition struct {
	PositionID    int64
	AccountID     int64
	UserID        int64
	InstrumentID  int64
	Symbol        string // canonical "EUR/USD"
	QuoteCurrency string
	Side          string // LONG | SHORT
	Quantity      decimal.Decimal
	EntryPrice    decimal.Decimal
	MarginUsed    decimal.Decimal
	CloseMark     *decimal.Decimal // flat-row evaluation mark
	ClosedAt      *time.Time
}

// DepPositionSource enumerates the positions under evaluation.
type DepPositionSource interface {
	// RetailLeveraged returns OPEN retail leveraged positions
	// (quantity > 0, margin_used > 0, accounts.client_category RETAIL,
	// account not CLOSED — suspended/frozen accounts still get the
	// statutory notice).
	RetailLeveraged(ctx context.Context) ([]DepPosition, error)
	// FlatSince returns positions flattened since `since` (UTC) that
	// were leveraged (margin_used > 0) or carry a depreciation episode —
	// the EOD sweep's closed-today catch-up set.
	FlatSince(ctx context.Context, since time.Time) ([]DepPosition, error)
}

// Episode status vocabulary.
const (
	EpisodeOpen   = "OPEN"
	EpisodeReset  = "RESET"  // recovered above −5% — episode over
	EpisodeClosed = "CLOSED" // position flattened — episode over
)

// DepreciationEpisode is the durable per-position episode row
// (depreciation_episodes, migration 237).
type DepreciationEpisode struct {
	ID                 int64
	PositionID         int64
	AccountID          int64
	InstrumentID       int64
	Seq                int
	DeepestNotifiedPct decimal.Decimal // most negative notified −10% multiple (0 = none)
	LastDepPct         decimal.Decimal
	LastMark           decimal.Decimal
	LastQuantity       decimal.Decimal
	EntryPrice         decimal.Decimal
	Currency           string
	Status             string
	StartedAt          time.Time
	UpdatedAt          time.Time
	EndedAt            *time.Time
}

// DepreciationStore persists episode state. Latest returns the newest
// episode (any status) or (nil, nil); Save upserts by
// (position_id, episode_seq). Implementations must be durable — the
// HWM state may not live in process memory (spec §2.7 fail-closed: a
// restart must not re-notify a live episode nor lose a reset).
type DepreciationStore interface {
	Latest(ctx context.Context, positionID int64) (*DepreciationEpisode, error)
	Save(ctx context.Context, ep *DepreciationEpisode) error
}

// DepNotifier is the user-notification seam — *notifications.Service
// satisfies it (same shape as risk/auto_halt.go's Notifier).
type DepNotifier interface {
	Notify(ctx context.Context, userID int64, event string, payload map[string]any) (int, error)
}

// DepMarginReader supplies the current margin level for the notice
// payload (Task 19.3.16 link). Read failures omit the field — the
// statutory notice is never blocked by a missing margin hash, but the
// failure is recorded in the sweep result.
type DepMarginReader interface {
	MarginLevel(ctx context.Context, accountID int64) (*risk.MarginLevel, error)
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

// DepStepPct is the notification step: every −10% multiple crossed.
const DepStepPct = 10

// DepResetPct is the episode-reset bound: depreciation recovering above
// −5% ends the open episode (HWM reset); the next −10% crossing opens a
// new episode and re-notifies.
var DepResetPct = decimal.NewFromInt(-5)

// depStep is the decimal form of DepStepPct.
var depStep = decimal.NewFromInt(DepStepPct)

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// DepreciationDeps wires the service; all seams are required except
// Margin (nil → payload omits margin_level_pct) and Logf/Now.
type DepreciationDeps struct {
	Marks     DepMarkSource
	Positions DepPositionSource
	Episodes  DepreciationStore
	Notifier  DepNotifier
	Margin    DepMarginReader
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

// DepreciationService evaluates positions and emits
// position_depreciation notices.
type DepreciationService struct {
	marks     DepMarkSource
	positions DepPositionSource
	episodes  DepreciationStore
	notifier  DepNotifier
	margin    DepMarginReader
	now       func() time.Time
	logf      func(format string, args ...any)
}

// NewDepreciationService validates the wiring (fail closed: no marks,
// positions or episode store means the job cannot run at all).
func NewDepreciationService(d DepreciationDeps) (*DepreciationService, error) {
	if d.Marks == nil || d.Positions == nil || d.Episodes == nil || d.Notifier == nil {
		return nil, fmt.Errorf("depreciation: marks/positions/episodes/notifier are required")
	}
	s := &DepreciationService{
		marks: d.Marks, positions: d.Positions, episodes: d.Episodes,
		notifier: d.Notifier, margin: d.Margin, logf: d.Logf,
		now: time.Now,
	}
	if d.Now != nil {
		s.now = d.Now
	}
	return s, nil
}

func (s *DepreciationService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// DepResult is one position's evaluation outcome.
type DepResult struct {
	PositionID   int64            `json:"position_id"`
	AccountID    int64            `json:"account_id"`
	Symbol       string           `json:"symbol"`
	DepPct       decimal.Decimal  `json:"dep_pct"`
	NotifiedAt   *decimal.Decimal `json:"notified_threshold_pct,omitempty"`
	Closed       bool             `json:"closed"`
	EpisodeReset bool             `json:"episode_reset"`
	EpisodeSeq   int              `json:"episode_seq,omitempty"`
}

// SweepResult summarizes one job pass.
type SweepResult struct {
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Evaluated   int       `json:"evaluated"`
	Notified    int       `json:"notified"`
	Resets      int       `json:"resets"`
	Closed      int       `json:"closed"`
	Errors      []string  `json:"errors,omitempty"`
}

// depPct returns the signed depreciation percentage of a position leg:
// LONG depreciates when mark < entry; SHORT when mark > entry.
func depPct(side string, entry, mark decimal.Decimal) decimal.Decimal {
	if !entry.IsPositive() {
		return decimal.Zero
	}
	frac := mark.Sub(entry).Div(entry)
	if side == "SHORT" {
		frac = frac.Neg()
	}
	return frac.Mul(decimal.NewFromInt(100))
}

// deepestStep returns the deepest −10% multiple at-or-below dep
// (dep=-23.5 → -20); 0 when no threshold is crossed.
func deepestStep(dep decimal.Decimal) decimal.Decimal {
	if dep.GreaterThan(depStep.Neg()) { // dep > -10
		return decimal.Zero
	}
	steps := dep.Abs().Div(depStep).Floor()
	return steps.Mul(depStep).Neg()
}

// Evaluate assesses one position: updates episode state and emits the
// threshold notice when a new −10% multiple is crossed. Exported so the
// close-path and tests drive single evaluations.
func (s *DepreciationService) Evaluate(ctx context.Context, pos DepPosition) (*DepResult, error) {
	if pos.PositionID <= 0 || pos.AccountID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"depreciation: position requires position_id and account_id")
	}
	now := s.now().UTC()
	res := &DepResult{PositionID: pos.PositionID, AccountID: pos.AccountID,
		Symbol: pos.Symbol, Closed: pos.CloseMark != nil}

	// 1. Mark resolution — fail closed on missing/stale live marks.
	var mark decimal.Decimal
	if pos.CloseMark != nil {
		mark = *pos.CloseMark
	} else {
		mv, err := s.marks.Mark(ctx, pos.Symbol)
		if err != nil {
			return res, fmt.Errorf("depreciation: mark read %s: %w", pos.Symbol, err)
		}
		if !mv.Found {
			return res, excerrors.New("PRICE_ORACLE_UNAVAILABLE",
				fmt.Sprintf("depreciation: no oracle mark for %s", pos.Symbol))
		}
		if mv.Stale {
			return res, excerrors.New("MARK_PRICE_STALE",
				fmt.Sprintf("depreciation: stale mark for %s", pos.Symbol))
		}
		mark = mv.Price
	}
	dep := depPct(pos.Side, pos.EntryPrice, mark).Round(4)
	res.DepPct = dep

	ep, err := s.episodes.Latest(ctx, pos.PositionID)
	if err != nil {
		return res, fmt.Errorf("depreciation: episode read pos %d: %w", pos.PositionID, err)
	}

	// 2. Recovery above −5% ends the open episode (HWM reset). For a flat
	// position the episode terminates CLOSED rather than RESET — the
	// position is gone, there is nothing left to recover.
	if dep.GreaterThan(DepResetPct) {
		if ep != nil && ep.Status == EpisodeOpen {
			ep.Status = EpisodeReset
			if pos.CloseMark != nil {
				ep.Status = EpisodeClosed
				res.Closed = true
			}
			ep.LastDepPct = dep
			ep.LastMark = mark
			ep.LastQuantity = pos.Quantity
			ep.UpdatedAt = now
			ep.EndedAt = &now
			if err := s.episodes.Save(ctx, ep); err != nil {
				return res, fmt.Errorf("depreciation: episode reset pos %d: %w", pos.PositionID, err)
			}
			res.EpisodeReset = ep.Status == EpisodeReset
			res.EpisodeSeq = ep.Seq
		}
		return res, nil
	}

	// 3. Deepest crossed −10% multiple (0 while between −5% and −10%).
	deepest := deepestStep(dep)

	if deepest.IsZero() {
		// In drawdown but below the first threshold: refresh the open
		// episode's last_* fields (its last known open value feeds the
		// close-path payload), no notice.
		if ep != nil && ep.Status == EpisodeOpen {
			ep.LastDepPct, ep.LastMark, ep.LastQuantity = dep, mark, pos.Quantity
			ep.UpdatedAt = now
			if pos.CloseMark != nil {
				ep.Status = EpisodeClosed
				ep.EndedAt = &now
				res.Closed = true
			}
			if err := s.episodes.Save(ctx, ep); err != nil {
				return res, fmt.Errorf("depreciation: episode update pos %d: %w", pos.PositionID, err)
			}
			res.EpisodeSeq = ep.Seq
		}
		return res, nil
	}

	// 4. Threshold crossed: ensure an OPEN episode — a RESET/CLOSED
	// episode means a recovery happened in between, so the new crossing
	// opens seq+1 and re-notifies its thresholds.
	if ep == nil || ep.Status != EpisodeOpen {
		seq := 1
		if ep != nil {
			seq = ep.Seq + 1
		}
		ep = &DepreciationEpisode{
			PositionID:   pos.PositionID,
			AccountID:    pos.AccountID,
			InstrumentID: pos.InstrumentID,
			Seq:          seq,
			EntryPrice:   pos.EntryPrice,
			Currency:     pos.QuoteCurrency,
			Status:       EpisodeOpen,
			StartedAt:    now,
		}
	}
	ep.LastDepPct = dep
	ep.LastMark = mark
	ep.LastQuantity = pos.Quantity
	ep.UpdatedAt = now
	res.EpisodeSeq = ep.Seq

	// 5. Notify on each newly-deepened −10% multiple. One notice per
	// evaluation names the deepest crossed step (a jump from −5% to −25%
	// emits the −20% notice, covering both crossed multiples); dedupe is
	// the episode HWM, never the delivery log. A failed emit does NOT
	// advance the HWM — the next evaluation retries (fail closed).
	if deepest.LessThan(ep.DeepestNotifiedPct) {
		if err := s.emitNotice(ctx, pos, dep, deepest, mark); err != nil {
			_ = s.episodes.Save(ctx, ep) // persist last_* observation anyway
			return res, err
		}
		ep.DeepestNotifiedPct = deepest
		res.NotifiedAt = &deepest
	}

	// 6. Flat position — the episode is done (notice, if any, already sent
	// with closed=true).
	if pos.CloseMark != nil && ep.Status == EpisodeOpen {
		ep.Status = EpisodeClosed
		ep.EndedAt = &now
		res.Closed = true
	}
	if err := s.episodes.Save(ctx, ep); err != nil {
		return res, fmt.Errorf("depreciation: episode save pos %d: %w", pos.PositionID, err)
	}
	return res, nil
}

// emitNotice delivers the position_depreciation notification via the
// Phase-12 service; the margin-level field is best-effort (a missing
// margin hash must not block the statutory notice).
func (s *DepreciationService) emitNotice(ctx context.Context, pos DepPosition,
	dep, threshold, mark decimal.Decimal) error {

	payload := map[string]any{
		"position_id":       pos.PositionID,
		"account_id":        pos.AccountID,
		"symbol":            pos.Symbol,
		"side":              pos.Side,
		"dep_pct":           dep.String(),
		"threshold_pct":     threshold.String(),
		"open_value":        pos.Quantity.Mul(pos.EntryPrice).Round(8).String(),
		"depreciated_value": pos.Quantity.Mul(mark).Round(8).String(),
		"currency":          pos.QuoteCurrency,
		"mark":              mark.String(),
		"closed":            pos.CloseMark != nil,
		"detected_at":       s.now().UTC().Format(time.RFC3339),
	}
	if s.margin != nil {
		lvl, err := s.margin.MarginLevel(ctx, pos.AccountID)
		if err != nil {
			s.log("depreciation: margin level read acct %d failed: %v", pos.AccountID, err)
		} else if lvl != nil {
			payload["margin_level_pct"] = lvl.MarginLevelPct.String()
			payload["margin_level_status"] = lvl.Status
		}
	}
	_, err := s.notifier.Notify(ctx, pos.UserID,
		notifications.EventPositionDepreciation, payload)
	if err != nil {
		return fmt.Errorf("depreciation: notify user %d pos %d: %w",
			pos.UserID, pos.PositionID, err)
	}
	return nil
}

// HourlyJob evaluates every open retail leveraged position once — the
// hourly detection pass (crossings are durably recorded so an intra-hour
// spike that recovers is still caught here or at EOD).
func (s *DepreciationService) HourlyJob(ctx context.Context) (*SweepResult, error) {
	res := &SweepResult{StartedAt: s.now().UTC()}
	positions, err := s.positions.RetailLeveraged(ctx)
	if err != nil {
		return res, fmt.Errorf("depreciation: position scan: %w", err)
	}
	for _, pos := range positions {
		r, err := s.Evaluate(ctx, pos)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		res.Evaluated++
		if r.NotifiedAt != nil {
			res.Notified++
		}
		if r.EpisodeReset {
			res.Resets++
		}
	}
	res.CompletedAt = s.now().UTC()
	return res, nil
}

// EndOfDaySweep is the same-business-day catch-up: re-evaluate the open
// set, then evaluate positions flattened since `dayStart` (UTC) — a
// position closed intra-day while below a threshold still emits its
// notice ("notice on close if crossed").
func (s *DepreciationService) EndOfDaySweep(ctx context.Context, dayStart time.Time) (*SweepResult, error) {
	res, err := s.HourlyJob(ctx)
	if err != nil {
		return res, err
	}
	flat, err := s.positions.FlatSince(ctx, dayStart.UTC())
	if err != nil {
		return res, fmt.Errorf("depreciation: flat-position scan: %w", err)
	}
	for _, pos := range flat {
		r, err := s.Evaluate(ctx, pos)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		res.Evaluated++
		if r.NotifiedAt != nil {
			res.Notified++
		}
		if r.Closed {
			res.Closed++
		}
	}
	res.CompletedAt = s.now().UTC()
	return res, nil
}

// Start runs the hourly cadence until ctx ends — the orchestrator wires
// this alongside the EOD call (Task 20.3.13 snapshot builder invokes
// EndOfDaySweep at its day boundary).
func (s *DepreciationService) Start(ctx context.Context, interval time.Duration, onErr func(error)) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				res, err := s.HourlyJob(ctx)
				if err != nil {
					if onErr != nil {
						onErr(err)
					}
					continue
				}
				for _, e := range res.Errors {
					s.log("depreciation sweep: %s", e)
				}
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Pg implementations
// ---------------------------------------------------------------------------

// PgDepreciationStore implements DepreciationStore over
// depreciation_episodes (migration 237).
type PgDepreciationStore struct {
	Pool *pgxpool.Pool
}

// NewPgDepreciationStore binds the pool.
func NewPgDepreciationStore(pool *pgxpool.Pool) *PgDepreciationStore {
	return &PgDepreciationStore{Pool: pool}
}

const depEpisodeCols = `id, position_id, account_id, instrument_id, episode_seq,
	deepest_notified_pct::text, last_dep_pct::text,
	COALESCE(last_mark::text,''), COALESCE(last_quantity::text,''),
	entry_price::text, currency, status, started_at, updated_at, ended_at`

// Latest returns the newest episode for the position (any status) or
// (nil, nil).
func (s *PgDepreciationStore) Latest(ctx context.Context, positionID int64) (*DepreciationEpisode, error) {
	var ep DepreciationEpisode
	var dp, lp, lm, lq, en string
	var ended *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT `+depEpisodeCols+`
		  FROM depreciation_episodes
		 WHERE position_id = $1
		 ORDER BY episode_seq DESC LIMIT 1`, positionID).
		Scan(&ep.ID, &ep.PositionID, &ep.AccountID, &ep.InstrumentID,
			&ep.Seq, &dp, &lp, &lm, &lq, &en, &ep.Currency, &ep.Status,
			&ep.StartedAt, &ep.UpdatedAt, &ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("depreciation: episode read pos %d: %w", positionID, err)
	}
	for label, dst := range map[string]*decimal.Decimal{
		"deepest_notified_pct": &ep.DeepestNotifiedPct, "last_dep_pct": &ep.LastDepPct,
		"entry_price": &ep.EntryPrice,
	} {
		src := map[string]string{"deepest_notified_pct": dp, "last_dep_pct": lp,
			"entry_price": en}[label]
		d, perr := decimal.NewFromString(src)
		if perr != nil {
			return nil, fmt.Errorf("depreciation: episode pos %d %s %q: %w",
				positionID, label, src, perr)
		}
		*dst = d
	}
	if lm != "" {
		ep.LastMark = decimal.RequireFromString(lm)
	}
	if lq != "" {
		ep.LastQuantity = decimal.RequireFromString(lq)
	}
	ep.EndedAt = ended
	return &ep, nil
}

// Save upserts the episode on (position_id, episode_seq).
func (s *PgDepreciationStore) Save(ctx context.Context, ep *DepreciationEpisode) error {
	var mark, qty any
	if !ep.LastMark.IsZero() {
		mark = ep.LastMark.String()
	}
	if !ep.LastQuantity.IsZero() {
		qty = ep.LastQuantity.String()
	}
	return s.Pool.QueryRow(ctx, `
		INSERT INTO depreciation_episodes
		    (position_id, account_id, instrument_id, episode_seq,
		     deepest_notified_pct, last_dep_pct, last_mark, last_quantity,
		     entry_price, currency, status, started_at, updated_at, ended_at)
		VALUES ($1,$2,$3,$4,$5::numeric,$6::numeric,$7::numeric,$8::numeric,
		        $9::numeric,$10,$11,$12,$13,$14)
		ON CONFLICT (position_id, episode_seq) DO UPDATE
		  SET deepest_notified_pct = EXCLUDED.deepest_notified_pct,
		      last_dep_pct = EXCLUDED.last_dep_pct,
		      last_mark = EXCLUDED.last_mark,
		      last_quantity = EXCLUDED.last_quantity,
		      status = EXCLUDED.status,
		      updated_at = EXCLUDED.updated_at,
		      ended_at = EXCLUDED.ended_at
		RETURNING id`,
		ep.PositionID, ep.AccountID, ep.InstrumentID, ep.Seq,
		ep.DeepestNotifiedPct.String(), ep.LastDepPct.String(),
		mark, qty, ep.EntryPrice.String(), ep.Currency, ep.Status,
		ep.StartedAt.UTC(), ep.UpdatedAt.UTC(), ep.EndedAt).Scan(&ep.ID)
}

// PgDepreciationPositionSource implements DepositionSource over the
// positions/accounts/instruments join.
type PgDepreciationPositionSource struct {
	Pool *pgxpool.Pool
}

// NewPgDepreciationPositionSource binds the pool.
func NewPgDepreciationPositionSource(pool *pgxpool.Pool) *PgDepreciationPositionSource {
	return &PgDepreciationPositionSource{Pool: pool}
}

// depPosCols is the shared projection; p.mark_price may be NULL (never
// marked) and is surfaced as CloseMark only for flat rows.
const depPosCols = `p.id, p.account_id, a.user_id, p.instrument_id, i.symbol,
	i.quote_currency, p.side::text, p.quantity::text, p.entry_price::text,
	p.margin_used::text, COALESCE(p.mark_price::text,''), p.updated_at`

func scanDepPositions(rows pgx.Rows) ([]DepPosition, error) {
	defer rows.Close()
	out := []DepPosition{}
	for rows.Next() {
		var p DepPosition
		var qty, entry, margin, mark string
		var updatedAt time.Time
		if err := rows.Scan(&p.PositionID, &p.AccountID, &p.UserID,
			&p.InstrumentID, &p.Symbol, &p.QuoteCurrency, &p.Side,
			&qty, &entry, &margin, &mark, &updatedAt); err != nil {
			return nil, fmt.Errorf("depreciation: position scan: %w", err)
		}
		var err error
		if p.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, fmt.Errorf("depreciation: qty %q: %w", qty, err)
		}
		if p.EntryPrice, err = decimal.NewFromString(entry); err != nil {
			return nil, fmt.Errorf("depreciation: entry %q: %w", entry, err)
		}
		if p.MarginUsed, err = decimal.NewFromString(margin); err != nil {
			return nil, fmt.Errorf("depreciation: margin %q: %w", margin, err)
		}
		if mark != "" {
			m := decimal.RequireFromString(mark)
			p.CloseMark = &m
		}
		p.ClosedAt = &updatedAt
		out = append(out, p)
	}
	return out, rows.Err()
}

// RetailLeveraged implements DepPositionSource: open (quantity>0),
// leveraged (margin_used>0) positions on RETAIL accounts that are not
// CLOSED. mark_price is carried but NOT used as the evaluation mark —
// open positions evaluate on the live oracle mark.
func (s *PgDepreciationPositionSource) RetailLeveraged(ctx context.Context) ([]DepPosition, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+depPosCols+`
		  FROM positions p
		  JOIN accounts a    ON a.id = p.account_id
		  JOIN instruments i ON i.id = p.instrument_id
		 WHERE p.quantity > 0 AND p.margin_used > 0
		   AND a.client_category::text = 'RETAIL'
		   AND a.status::text <> 'CLOSED'
		 ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("depreciation: leveraged scan: %w", err)
	}
	out, err := scanDepPositions(rows)
	if err != nil {
		return nil, err
	}
	// Open positions evaluate on the live mark, not the stored one.
	for i := range out {
		out[i].CloseMark = nil
		out[i].ClosedAt = nil
	}
	return out, nil
}

// FlatSince implements the EOD catch-up set: positions flattened inside
// the window that were leveraged or carry a depreciation episode. The
// stored mark_price stands in as the close mark.
func (s *PgDepreciationPositionSource) FlatSince(ctx context.Context, since time.Time) ([]DepPosition, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+depPosCols+`
		  FROM positions p
		  JOIN accounts a    ON a.id = p.account_id
		  JOIN instruments i ON i.id = p.instrument_id
		 WHERE p.quantity = 0 AND p.updated_at >= $1
		   AND a.client_category::text = 'RETAIL'
		   AND (p.margin_used > 0
		        OR EXISTS (SELECT 1 FROM depreciation_episodes e
		                    WHERE e.position_id = p.id))
		 ORDER BY p.id`, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("depreciation: flat scan: %w", err)
	}
	return scanDepPositions(rows)
}
