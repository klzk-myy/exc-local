// Phase-19 Task 19.3.16 — margin-level display + per-account thresholds
// (spec §13.3/§13.6d, §24 #163).
//
// Read side: the sibling margin engine owns the canonical
// margin:level:{account_id} HASH (keys.go contract); this file only
// READS it through the MarginLevelReader seam and decorates the view
// with the account's effective threshold set — the same fields the
// engine publishes: equity / used_margin / margin_level_pct / status /
// updated_at. A missing hash is an honest "not evaluated" view, never
// a fabricated snapshot (§2.7).
//
// Threshold side: ESMA retail defaults 120%/100%/50% (warning /
// margin-call / stop-out, §13.6d) resolve from accounts.client_category
// via ThresholdsFor; a row in account_margin_thresholds (migration 235)
// overrides per account. The SET path is guarded: thresholds must stay
// strictly ordered and can NEVER loosen below the category floor —
// retail cannot widen protection past ESMA's intervention values;
// professional/ECP floors match their category defaults; institutional
// (ECP) overrides are negotiable within sanity bounds.
//
// Push side: MarginLevelWatcher polls margin:level:* on a configurable
// interval and publishes private:margin frames on change — the engine
// writes the hash, this watcher only observes it (publisher seam keeps
// the WS fan-out identical to pnl/positions pushes).
package risk

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Threshold persistence
// ---------------------------------------------------------------------------

// MarginThresholdStore is the override persistence seam
// (PgMarginThresholdStore in production).
type MarginThresholdStore interface {
	// ThresholdsFor returns the account's override row or nil.
	ThresholdsFor(ctx context.Context, accountID int64) (*MarginThresholds, error)
	// ClientCategory returns accounts.client_category ('RETAIL' default).
	ClientCategory(ctx context.Context, accountID int64) (string, error)
	// SetThresholds upserts the override and writes the audit row
	// atomically (before/after images).
	SetThresholds(ctx context.Context, accountID int64, th MarginThresholds,
		before *MarginThresholds, userID int64) error
}

// ThresholdFloor is the immutable-per-category lower bound a custom
// threshold set may not weaken past (§13.6d): retail = the ESMA
// intervention values, professional = the §13.6d professional set,
// ECP = the institutional fallback (call at the §13.3 utilization
// bound). "Weakening" means a LOWER threshold — a floor of F means the
// stored value must be >= F.
func ThresholdFloor(clientCategory string) MarginThresholds {
	return ThresholdsFor(clientCategory)
}

// maxThresholdPct bounds every threshold value — a guard against
// absurd inputs, not a business rule.
const maxThresholdPct = 1000.0

// MarginThresholdService resolves and guards per-account thresholds.
type MarginThresholdService struct {
	store MarginThresholdStore
}

// NewMarginThresholdService wires the service.
func NewMarginThresholdService(store MarginThresholdStore) *MarginThresholdService {
	return &MarginThresholdService{store: store}
}

// Thresholds resolves the effective set: account override row wins,
// else the client_category default (ESMA retail fail-closed).
func (s *MarginThresholdService) Thresholds(ctx context.Context, accountID int64) (MarginThresholds, error) {
	cat, err := s.store.ClientCategory(ctx, accountID)
	if err != nil {
		return MarginThresholds{}, internalError("client category", err)
	}
	ov, err := s.store.ThresholdsFor(ctx, accountID)
	if err != nil {
		return MarginThresholds{}, internalError("margin thresholds", err)
	}
	if ov != nil {
		return *ov, nil
	}
	return ThresholdsFor(cat), nil
}

// Set validates and persists a per-account override. Fail-closed:
//   - strict ordering warning > call > stop_out > 0;
//   - values within (0, maxThresholdPct];
//   - never looser than the category floor (a lower threshold = less
//     protection = a regulatory weakening — rejected for retail/pro).
func (s *MarginThresholdService) Set(ctx context.Context, accountID int64, th MarginThresholds, userID int64) (MarginThresholds, error) {
	if !th.Warning.IsPositive() || !th.Call.IsPositive() || !th.StopOut.IsPositive() {
		return MarginThresholds{}, excerrors.New(CodeLeverageInvalid,
			"margin thresholds must all be positive")
	}
	max := decimal.NewFromInt(maxThresholdPct)
	if th.Warning.GreaterThan(max) || th.Call.GreaterThan(max) || th.StopOut.GreaterThan(max) {
		return MarginThresholds{}, excerrors.New(CodeLeverageInvalid,
			fmt.Sprintf("margin thresholds must be <= %s", max))
	}
	if !th.Warning.GreaterThan(th.Call) || !th.Call.GreaterThan(th.StopOut) {
		return MarginThresholds{}, excerrors.New(CodeLeverageInvalid,
			"margin thresholds must be strictly ordered: warning > margin_call > stop_out")
	}
	cat, err := s.store.ClientCategory(ctx, accountID)
	if err != nil {
		return MarginThresholds{}, internalError("client category", err)
	}
	if cat != CategoryEligible {
		floor := ThresholdFloor(cat)
		if th.StopOut.LessThan(floor.StopOut) || th.Call.LessThan(floor.Call) || th.Warning.LessThan(floor.Warning) {
			return MarginThresholds{}, excerrors.New("PRODUCT_NOT_PERMITTED",
				fmt.Sprintf("thresholds may not weaken below the %s floor (warning>=%s, call>=%s, stop-out>=%s)",
					cat, floor.Warning, floor.Call, floor.StopOut))
		}
	}
	before, err := s.store.ThresholdsFor(ctx, accountID)
	if err != nil {
		return MarginThresholds{}, internalError("margin thresholds read", err)
	}
	if err := s.store.SetThresholds(ctx, accountID, th, before, userID); err != nil {
		return MarginThresholds{}, internalError("set margin thresholds", err)
	}
	return th, nil
}

// ---------------------------------------------------------------------------
// Margin-level view (GET /api/v1/account/margin-level)
// ---------------------------------------------------------------------------

// MarginLevelView is the REST payload: the engine-published hash plus
// the account's resolved thresholds. Evaluated=false carries an honest
// "no live margin view" (engine has never evaluated the account).
type MarginLevelView struct {
	AccountID      int64      `json:"account_id"`
	Evaluated      bool       `json:"evaluated"`
	Equity         *string    `json:"equity,omitempty"`
	UsedMargin     *string    `json:"used_margin,omitempty"`
	MarginLevelPct *string    `json:"margin_level_pct,omitempty"`
	Status         string     `json:"status"`
	WarningPct     string     `json:"warning_pct"`
	MarginCallPct  string     `json:"margin_call_pct"`
	StopOutPct     string     `json:"stop_out_pct"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
	Source         string     `json:"source"` // "engine" (margin:level hash)
}

// MarginLevelViewService composes the read surface.
type MarginLevelViewService struct {
	reader     MarginLevelReader
	thresholds *MarginThresholdService
}

// NewMarginLevelViewService wires the view: reader is the canonical
// margin:level read seam (RedisMarginLevelReader in production).
func NewMarginLevelViewService(reader MarginLevelReader, thresholds *MarginThresholdService) *MarginLevelViewService {
	return &MarginLevelViewService{reader: reader, thresholds: thresholds}
}

// View builds the endpoint response. A missing hash returns
// Evaluated=false with no fabricated numbers; a reader/store error is
// fail-closed (SERVICE_DEGRADED-family codes).
func (s *MarginLevelViewService) View(ctx context.Context, accountID int64) (*MarginLevelView, error) {
	if s.reader == nil {
		return nil, excerrors.New(CodeLeverageUnavailable,
			"margin-level reader unavailable")
	}
	lvl, err := s.reader.MarginLevel(ctx, accountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeLeverageUnavailable, "margin level read", err)
	}
	v := &MarginLevelView{AccountID: accountID, Source: "engine", Status: "NORMAL"}
	if s.thresholds != nil {
		th, terr := s.thresholds.Thresholds(ctx, accountID)
		if terr != nil {
			return nil, terr
		}
		v.WarningPct = th.Warning.String()
		v.MarginCallPct = th.Call.String()
		v.StopOutPct = th.StopOut.String()
	}
	if lvl == nil {
		return v, nil // honest "not evaluated" — no fabricated snapshot
	}
	v.Evaluated = true
	eq := lvl.Equity.String()
	v.Equity = &eq
	um := lvl.UsedMargin.String()
	v.UsedMargin = &um
	if lvl.MarginLevelPct.IsPositive() {
		p := lvl.MarginLevelPct.String()
		v.MarginLevelPct = &p
	}
	if lvl.Status != "" {
		v.Status = lvl.Status
	}
	if !lvl.UpdatedAt.IsZero() {
		t := lvl.UpdatedAt
		v.UpdatedAt = &t
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// MarginLevelWatcher — private:margin WS pushes on hash change
// ---------------------------------------------------------------------------

// MarginLevelPublisher is the WS push seam — ws.Server.PublishPrivate's
// signature bound as a func in cmd/gateway.
type MarginLevelPublisher func(ctx context.Context, accountID int64, channel, eventType string, payload any) error

// MarginLevelHashLister enumerates live margin:level:* keys (the
// coordination-Redis SCAN seam).
type MarginLevelHashLister interface {
	ScanKeys(ctx context.Context, pattern string) ([]string, error)
}

// MarginLevelWatcher diffs margin:level:* hashes on an interval and
// publishes private:margin frames on any field change — the sibling
// engine stays the only writer; this loop is a read-only change feed.
type MarginLevelWatcher struct {
	reader MarginLevelReader
	lister MarginLevelHashLister
	pub    MarginLevelPublisher

	mu   sync.Mutex
	last map[int64]MarginLevel
}

// NewMarginLevelWatcher wires the watcher. pub may be nil (push
// disabled — e.g. WS server unwired).
func NewMarginLevelWatcher(reader MarginLevelReader, lister MarginLevelHashLister, pub MarginLevelPublisher) *MarginLevelWatcher {
	return &MarginLevelWatcher{reader: reader, lister: lister, pub: pub, last: map[int64]MarginLevel{}}
}

// Start runs the poll loop until ctx ends (default 500ms cadence —
// §13.3 real-time display bound).
func (w *MarginLevelWatcher) Start(ctx context.Context, interval time.Duration, onErr func(error)) {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := w.tick(ctx); err != nil && onErr != nil {
					onErr(err)
				}
			}
		}
	}()
}

// TickOnce runs one diff pass — exposed for tests.
func (w *MarginLevelWatcher) TickOnce(ctx context.Context) error { return w.tick(ctx) }

func (w *MarginLevelWatcher) tick(ctx context.Context) error {
	if w.lister == nil || w.reader == nil {
		return nil
	}
	keys, err := w.lister.ScanKeys(ctx, "margin:level:*")
	if err != nil {
		return fmt.Errorf("margin:level scan: %w", err)
	}
	for _, key := range keys {
		idStr := strings.TrimPrefix(key, "margin:level:")
		acct, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || acct <= 0 {
			continue
		}
		lvl, err := w.reader.MarginLevel(ctx, acct)
		if err != nil {
			return fmt.Errorf("margin:level read %d: %w", acct, err)
		}
		if lvl == nil {
			continue
		}
		w.mu.Lock()
		prev, seen := w.last[acct]
		changed := !seen || !prev.Equity.Equal(lvl.Equity) ||
			!prev.UsedMargin.Equal(lvl.UsedMargin) ||
			!prev.MarginLevelPct.Equal(lvl.MarginLevelPct) ||
			prev.Status != lvl.Status
		if changed {
			w.last[acct] = *lvl
		}
		w.mu.Unlock()
		if changed && w.pub != nil {
			payload := map[string]any{
				"account_id":       acct,
				"equity":           lvl.Equity.String(),
				"used_margin":      lvl.UsedMargin.String(),
				"margin_level_pct": lvl.MarginLevelPct.String(),
				"status":           lvl.Status,
				"updated_at":       lvl.UpdatedAt.UTC().Format(time.RFC3339Nano),
			}
			if err := w.pub(ctx, acct, "private:margin", "margin_level", payload); err != nil {
				return fmt.Errorf("margin level publish %d: %w", acct, err)
			}
		}
	}
	return nil
}

// ForgetAccount drops the watcher's last-seen snapshot (e.g. after a
// threshold change that should re-announce the view).
func (w *MarginLevelWatcher) ForgetAccount(accountID int64) {
	w.mu.Lock()
	delete(w.last, accountID)
	w.mu.Unlock()
}

// ---------------------------------------------------------------------------
// PgMarginThresholdStore
// ---------------------------------------------------------------------------

// PgMarginThresholdStore implements MarginThresholdStore over pgx.
type PgMarginThresholdStore struct {
	pool *pgxpool.Pool
}

// NewPgMarginThresholdStore wraps pool.
func NewPgMarginThresholdStore(pool *pgxpool.Pool) *PgMarginThresholdStore {
	return &PgMarginThresholdStore{pool: pool}
}

func (s *PgMarginThresholdStore) ThresholdsFor(ctx context.Context, accountID int64) (*MarginThresholds, error) {
	var w, c, so string
	err := s.pool.QueryRow(ctx, `
		SELECT warning_pct::text, margin_call_pct::text, stop_out_pct::text
		  FROM account_margin_thresholds WHERE account_id = $1`, accountID).
		Scan(&w, &c, &so)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	wd, err := decimal.NewFromString(w)
	if err != nil {
		return nil, fmt.Errorf("parse warning_pct %q: %w", w, err)
	}
	cd, err := decimal.NewFromString(c)
	if err != nil {
		return nil, fmt.Errorf("parse margin_call_pct %q: %w", c, err)
	}
	sod, err := decimal.NewFromString(so)
	if err != nil {
		return nil, fmt.Errorf("parse stop_out_pct %q: %w", so, err)
	}
	return &MarginThresholds{Warning: wd, Call: cd, StopOut: sod}, nil
}

func (s *PgMarginThresholdStore) ClientCategory(ctx context.Context, accountID int64) (string, error) {
	var c *string
	err := s.pool.QueryRow(ctx,
		`SELECT client_category::text FROM accounts WHERE id = $1`, accountID).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return CategoryRetail, nil
	}
	if err != nil {
		return "", err
	}
	if c == nil || *c == "" {
		return CategoryRetail, nil
	}
	return *c, nil
}

// SetThresholds upserts the override and appends the audit row in one
// transaction — the guarded threshold update is audit-logged (§13.6d).
func (s *PgMarginThresholdStore) SetThresholds(ctx context.Context, accountID int64,
	th MarginThresholds, before *MarginThresholds, userID int64) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_margin_thresholds
		    (account_id, warning_pct, margin_call_pct, stop_out_pct, updated_by, updated_at)
		VALUES ($1,$2::numeric,$3::numeric,$4::numeric,$5, now())
		ON CONFLICT (account_id) DO UPDATE
		  SET warning_pct = EXCLUDED.warning_pct,
		      margin_call_pct = EXCLUDED.margin_call_pct,
		      stop_out_pct = EXCLUDED.stop_out_pct,
		      updated_by = EXCLUDED.updated_by,
		      updated_at = now()`,
		accountID, th.Warning.String(), th.Call.String(), th.StopOut.String(), userID); err != nil {
		return err
	}
	var beforeJSON any
	if before != nil {
		beforeJSON = fmt.Sprintf(`{"warning_pct":%q,"margin_call_pct":%q,"stop_out_pct":%q}`,
			before.Warning.String(), before.Call.String(), before.StopOut.String())
	}
	afterJSON := fmt.Sprintf(`{"warning_pct":%q,"margin_call_pct":%q,"stop_out_pct":%q}`,
		th.Warning.String(), th.Call.String(), th.StopOut.String())
	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log (admin_user_id, action, target_type, target_id,
		                             before_state, after_state)
		VALUES ($1, 'account.margin_thresholds.update', 'account', $2, $3::jsonb, $4::jsonb)`,
		userID, accountID, beforeJSON, afterJSON); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
