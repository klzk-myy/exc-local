// margin_call.go — §13.3 margin-call lifecycle (Phase-19 Task 19.3.3
// item 1; §24 #93 class).
//
// Lifecycle:
//
//	margin_level_pct <= 111.1% (canonical §13.3 threshold)
//	  → MarginCallNotified: email + in-app notification
//	  → margin_call:{account_id} Redis key, 15min TTL (deposit window)
//	  → margin_call:block:{account_id} order-entry block flag
//	    (persists for the whole episode per §13.3 precedence — the
//	    window cures the shortfall, it does not restore trading)
//	  → margin_call_events row (status OPEN) + admin_audit_log entry
//
//	recovery above threshold within the window
//	  → window key + block cleared, event → RESTORED, status NORMAL
//
//	window expiry without recovery
//	  → event → LIQUIDATED, account enqueued on liquidation:queue
//	    (one enqueue per episode — liquidation:dedup:{acct} guards)
//
//	level <= stop-out threshold at any evaluation
//	  → §13.3 precedence: stop-out supersedes the deposit window —
//	    margin_call:{acct} is voided, liquidation proceeds immediately.
package risk

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Canonical §13.3/§13.6d thresholds (percent units). Per-account
// overrides live in account_margin_thresholds (mig 235 — sibling Task
// 19.3.16 cluster; this cluster consumes, never writes it).
var (
	// MarginCallThresholdPct is the §13.3 canonical deposit-window
	// trigger — 111.1% (margin_utilization >= 0.90). Also the retail
	// fallback when no per-account override row exists.
	MarginCallThresholdPct = decimal.RequireFromString("111.1")
	// MarginCallProfessionalPct is the §13.6d professional margin-call
	// tier — 80%.
	MarginCallProfessionalPct = decimal.RequireFromString("80")
	// StopOutRetailPct is the §13.6d ESMA retail stop-out — 50%.
	StopOutRetailPct = decimal.RequireFromString("50")
	// StopOutProfessionalPct is the §13.6d professional stop-out — 30%.
	StopOutProfessionalPct = decimal.RequireFromString("30")
	// StopOutInstitutionalPct is the institutional fallback — 100%
	// (custom agreements override via account_margin_thresholds).
	StopOutInstitutionalPct = decimal.NewFromInt(100)
)

// MarginCallThresholds is the resolved (margin-call, stop-out) pair for one
// account: account_margin_thresholds row wins; otherwise the
// client_category defaults above.
type MarginCallThresholds struct {
	MarginCallPct decimal.Decimal
	StopOutPct    decimal.Decimal
	Source        string // 'OVERRIDE' | 'RETAIL' | 'PROFESSIONAL' | 'ECP'
}

// Margin call event outcomes (margin_call_events.status CHECK, mig 230).
const (
	MarginCallOpen       = "OPEN"
	MarginCallRestored   = "RESTORED"
	MarginCallLiquidated = "LIQUIDATED"
	MarginCallStoppedOut = "STOP_OUT"
)

// MarginCallEvent is one margin_call_events row.
type MarginCallEvent struct {
	ID             int64
	AccountID      int64
	MarginLevelPct decimal.Decimal
	ThresholdPct   decimal.Decimal
	Status         string
	NotifiedAt     time.Time
	ExpiresAt      time.Time
	ResolvedAt     *time.Time
}

// MarginCallStore is the Postgres seam for the margin-call lifecycle.
type MarginCallStore interface {
	// AccountCategory returns the account's client_category + nbp flag —
	// the threshold-tier and retail-classification input.
	AccountCategory(ctx context.Context, accountID int64) (category string, nbp bool, err error)
	// MarginThresholdOverride reads account_margin_thresholds (mig 235,
	// sibling-owned) — (nil, nil) when no override row exists.
	MarginThresholdOverride(ctx context.Context, accountID int64) (*MarginCallThresholds, error)
	// OpenMarginCall returns the account's OPEN margin_call_events row,
	// or (nil, nil).
	OpenMarginCall(ctx context.Context, accountID int64) (*MarginCallEvent, error)
	// InsertMarginCall persists a new OPEN episode inside tx and writes
	// the §13.3 admin_audit_log row in the same transaction.
	InsertMarginCall(ctx context.Context, tx pgx.Tx, ev *MarginCallEvent) (int64, error)
	// ResolveMarginCall closes the OPEN episode with outcome
	// (RESTORED | LIQUIDATED | STOP_OUT).
	ResolveMarginCall(ctx context.Context, accountID int64, outcome string, at time.Time) error
	// ExpiredOpenMarginCalls lists OPEN episodes past expires_at.
	ExpiredOpenMarginCalls(ctx context.Context, now time.Time) ([]MarginCallEvent, error)
	// SetMarginAccountStatus updates margin_accounts.status
	// (NORMAL | MARGIN_CALL | LIQUIDATING).
	SetMarginAccountStatus(ctx context.Context, accountID int64, status string) error
	// SetMarginAccountStatusTx is the tx-scoped variant used when the
	// event row and the status transition commit together.
	SetMarginAccountStatusTx(ctx context.Context, tx pgx.Tx, accountID int64, status string) error
	// AuditMarginCallRelease writes the admin_audit_log row for a
	// dual-control manual block release.
	AuditMarginCallRelease(ctx context.Context, accountID int64, actor int64) error
	// AccountUserID resolves the notification target.
	AccountUserID(ctx context.Context, accountID int64) (int64, error)
}

// MarginCallService owns the §13.3 lifecycle. Construct via
// NewMarginCallService; drive Evaluate per account and SweepExpired
// from the 2s scanner cadence.
type MarginCallService struct {
	pool    *pgxpool.Pool
	rdb     *excredis.Client
	store   MarginCallStore
	levels  MarginLevelReader
	notify  Notifier // auto_halt.go's seam — *notifications.Service
	queue   *LiquidationQueue
	alerter OpsAlerter
	now     func() time.Time
	logf    func(format string, args ...any)
}

// MarginCallDeps wires the service. Levels and Store are required —
// without a live level source there is nothing to evaluate honestly.
type MarginCallDeps struct {
	Pool    *pgxpool.Pool
	Redis   *excredis.Client
	Store   MarginCallStore
	Levels  MarginLevelReader
	Queue   *LiquidationQueue // required — expiry enqueues liquidation
	Notify  Notifier
	Alerter OpsAlerter
	Now     func() time.Time
	Logf    func(format string, args ...any)
}

// NewMarginCallService builds the service.
func NewMarginCallService(d MarginCallDeps) (*MarginCallService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("margin call: nil pgx pool")
	}
	if d.Redis == nil {
		return nil, fmt.Errorf("margin call: nil redis — the deposit window is Redis state")
	}
	if d.Store == nil {
		return nil, fmt.Errorf("margin call: nil store")
	}
	if d.Levels == nil {
		return nil, fmt.Errorf("margin call: nil margin-level reader")
	}
	if d.Queue == nil {
		return nil, fmt.Errorf("margin call: nil liquidation queue — expiry would strand accounts")
	}
	s := &MarginCallService{
		pool: d.Pool, rdb: d.Redis, store: d.Store, levels: d.Levels,
		queue: d.Queue, notify: d.Notify, alerter: d.Alerter,
		now: d.Now, logf: d.Logf,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// thresholdsFor resolves the account's (margin-call, stop-out) pair:
// account_margin_thresholds (mig 235) wins; else the client_category
// defaults — RETAIL 111.1/50 (§13.3 canonical + §13.6d ESMA),
// PROFESSIONAL 80/30, ECP/institutional 80/100. A store read failure
// fails closed to the most protective retail pair.
func (s *MarginCallService) thresholdsFor(ctx context.Context, accountID int64) MarginCallThresholds {
	if ov, err := s.store.MarginThresholdOverride(ctx, accountID); err == nil && ov != nil {
		return *ov
	} else if err != nil {
		s.logf("margin call: threshold override read acct %d failed (fail-closed retail defaults): %v",
			accountID, err)
	}
	cat, _, err := s.store.AccountCategory(ctx, accountID)
	if err != nil {
		s.logf("margin call: category read acct %d failed (assuming retail): %v", accountID, err)
		return MarginCallThresholds{MarginCallPct: MarginCallThresholdPct,
			StopOutPct: StopOutRetailPct, Source: "RETAIL"}
	}
	switch cat {
	case "PROFESSIONAL":
		return MarginCallThresholds{MarginCallPct: MarginCallProfessionalPct,
			StopOutPct: StopOutProfessionalPct, Source: "PROFESSIONAL"}
	case "ELIGIBLE_COUNTERPARTY":
		return MarginCallThresholds{MarginCallPct: MarginCallProfessionalPct,
			StopOutPct: StopOutInstitutionalPct, Source: "ECP"}
	default:
		return MarginCallThresholds{MarginCallPct: MarginCallThresholdPct,
			StopOutPct: StopOutRetailPct, Source: "RETAIL"}
	}
}

// Evaluate inspects the live margin level and drives one account through
// the margin-call lifecycle. Safe to call on every level update and on
// the 2s scanner sweep — every transition is idempotent on the OPEN
// episode row + Redis keys.
func (s *MarginCallService) Evaluate(ctx context.Context, accountID int64) error {
	lv, err := s.levels.MarginLevel(ctx, accountID)
	if err != nil {
		return err
	}
	if lv == nil {
		return nil // no computed level — nothing to honestly evaluate
	}
	th := s.thresholdsFor(ctx, accountID)

	switch {
	case lv.IsStopOutLevel(th.StopOutPct):
		// §13.3 precedence — stop-out supersedes the deposit window.
		return s.stopOut(ctx, accountID, lv, th)
	case lv.IsMarginCallLevel(th.MarginCallPct):
		return s.ensureMarginCall(ctx, accountID, lv, th)
	default:
		return s.recoverIfOpen(ctx, accountID, lv)
	}
}

// ensureMarginCall opens the episode when none exists: 15min window key +
// persistent block flag + event row + audit + notification. An existing
// episode (key or open row) is left untouched — idempotent re-entry.
func (s *MarginCallService) ensureMarginCall(ctx context.Context, accountID int64,
	lv *MarginLevel, th MarginCallThresholds) error {
	open, err := s.store.OpenMarginCall(ctx, accountID)
	if err != nil {
		return err
	}
	if open != nil {
		return nil // episode in flight — nothing to re-notify
	}
	// Redis key may exist without a row (crash between SET and INSERT —
	// rare; treat key presence as in-flight too and heal the row).
	hasKey, err := s.rdb.Exists(ctx, MarginCallKey(accountID)).Result()
	if err != nil {
		return fmt.Errorf("margin call: dedup read acct %d: %w", accountID, err)
	}

	expires := s.now().Add(MarginCallWindowSeconds * time.Second)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("margin call: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ev := &MarginCallEvent{
		AccountID:      accountID,
		MarginLevelPct: lv.MarginLevelPct,
		ThresholdPct:   th.MarginCallPct,
		Status:         MarginCallOpen,
		NotifiedAt:     s.now(),
		ExpiresAt:      expires,
	}
	evID, err := s.store.InsertMarginCall(ctx, tx, ev)
	if err != nil {
		return err
	}
	if err := s.store.SetMarginAccountStatusTx(ctx, tx, accountID, "MARGIN_CALL"); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("margin call: commit event %d: %w", evID, err)
	}

	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, MarginCallKey(accountID), fmt.Sprint(evID),
		MarginCallWindowSeconds*time.Second)
	// The order-entry block outlives the window (§13.3 precedence) — no TTL.
	pipe.Set(ctx, MarginCallBlockKey(accountID), fmt.Sprint(evID), 0)
	if _, err := pipe.Exec(ctx); err != nil {
		// The durable row committed; a Redis failure leaves the episode
		// replayable by the sweeper (OPEN row with a past-due expiry is
		// re-enqueued). Alert loudly rather than silently degrade.
		s.raiseAlert(ctx, SeverityP1, "MARGIN_CALL_STATE_DEGRADED",
			fmt.Sprintf("margin call %d committed but Redis window write failed for account %d", evID, accountID),
			map[string]string{"account_id": fmt.Sprint(accountID)})
		return excerrors.Wrap(CodeLiquidationFailed,
			"margin call committed but window flags failed", err)
	}
	if hasKey == 0 {
		s.notifyMarginCall(ctx, accountID, lv, th)
	}
	return nil
}

// notifyMarginCall emits the margin-call notice — email + in-app via the
// notifications service seam (liquidation_warning is the canonical
// critical client event; it bypasses quiet hours by registry rule).
func (s *MarginCallService) notifyMarginCall(ctx context.Context, accountID int64,
	lv *MarginLevel, th MarginCallThresholds) {
	if s.notify == nil {
		return
	}
	userID, err := s.store.AccountUserID(ctx, accountID)
	if err != nil || userID == 0 {
		s.logf("margin call: user lookup acct %d: %v", accountID, err)
		return
	}
	if _, err := s.notify.Notify(ctx, userID, "liquidation_warning", map[string]any{
		"account_id":       accountID,
		"margin_level_pct": lv.MarginLevelPct.String(),
		"threshold_pct":    th.MarginCallPct.String(),
		"window_seconds":   MarginCallWindowSeconds,
	}); err != nil {
		s.logf("margin call: notify acct %d: %v", accountID, err)
	}
}

// recoverIfOpen cancels the episode when the level has recovered above
// the margin-call threshold: clears the window + block keys, resolves the
// event RESTORED, and restores margin_accounts.status=NORMAL.
func (s *MarginCallService) recoverIfOpen(ctx context.Context, accountID int64, lv *MarginLevel) error {
	open, err := s.store.OpenMarginCall(ctx, accountID)
	if err != nil {
		return err
	}
	blockSet, err := s.rdb.Exists(ctx, MarginCallBlockKey(accountID)).Result()
	if err != nil {
		return fmt.Errorf("margin call: block read acct %d: %w", accountID, err)
	}
	if open == nil && blockSet == 0 {
		return nil
	}
	if err := s.store.ResolveMarginCall(ctx, accountID, MarginCallRestored, s.now()); err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, MarginCallKey(accountID))
	pipe.Del(ctx, MarginCallBlockKey(accountID))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("margin call: clear keys acct %d: %w", accountID, err)
	}
	if err := s.store.SetMarginAccountStatus(ctx, accountID, "NORMAL"); err != nil {
		return err
	}
	s.logf("margin call: account %d restored above threshold (level %s%%)",
		accountID, lv.MarginLevelPct)
	return nil
}

// stopOut voids the deposit window and enqueues immediate liquidation —
// §13.3 precedence rule 2.
func (s *MarginCallService) stopOut(ctx context.Context, accountID int64,
	lv *MarginLevel, th MarginCallThresholds) error {

	if err := s.store.ResolveMarginCall(ctx, accountID, MarginCallStoppedOut, s.now()); err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, MarginCallKey(accountID)) // window voided at stop-out entry
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("margin call: stop-out void acct %d: %w", accountID, err)
	}
	if err := s.store.SetMarginAccountStatus(ctx, accountID, "LIQUIDATING"); err != nil {
		return err
	}
	return s.queue.Enqueue(ctx, LiquidationJob{
		AccountID: accountID,
		Reason:    LiquidationReasonStopOut,
	}, lv)
}

// SweepExpired is the deposit-window sweeper: every OPEN episode past
// expires_at is re-evaluated — recovered accounts resolve RESTORED,
// still-breached accounts enqueue for liquidation. Called on the §13.5
// 2-second scanner cadence.
func (s *MarginCallService) SweepExpired(ctx context.Context) (int, error) {
	due, err := s.store.ExpiredOpenMarginCalls(ctx, s.now())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ev := range due {
		lv, lerr := s.levels.MarginLevel(ctx, ev.AccountID)
		if lerr != nil {
			// Level unreadable — fail closed: enqueue the account; the
			// worker re-evaluates with fresh reads.
			s.logf("margin call: level read acct %d failed (%v) — enqueueing liquidation", ev.AccountID, lerr)
			if qerr := s.enqueueExpired(ctx, ev, nil); qerr != nil {
				s.logf("margin call: enqueue acct %d: %v", ev.AccountID, qerr)
			}
			continue
		}
		th := s.thresholdsFor(ctx, ev.AccountID)
		if lv != nil && lv.MarginLevelPct.GreaterThan(th.MarginCallPct) {
			if rerr := s.recoverIfOpen(ctx, ev.AccountID, lv); rerr != nil {
				s.logf("margin call: recover acct %d: %v", ev.AccountID, rerr)
			}
			continue
		}
		if err := s.enqueueExpired(ctx, ev, lv); err != nil {
			s.logf("margin call: expire acct %d: %v", ev.AccountID, err)
			continue
		}
		n++
	}
	return n, nil
}

// enqueueExpired resolves the episode LIQUIDATED and queues the account.
func (s *MarginCallService) enqueueExpired(ctx context.Context, ev MarginCallEvent, lv *MarginLevel) error {
	if err := s.store.ResolveMarginCall(ctx, ev.AccountID, MarginCallLiquidated, s.now()); err != nil {
		return err
	}
	if err := s.store.SetMarginAccountStatus(ctx, ev.AccountID, "LIQUIDATING"); err != nil {
		return err
	}
	// The window key is naturally expired; the block persists until
	// recovery — liquidation runs under it.
	return s.queue.Enqueue(ctx, LiquidationJob{
		AccountID: ev.AccountID,
		Reason:    LiquidationReasonMarginCallExpired,
	}, lv)
}

// HasMarginCallBlock reports whether the account's order-entry block
// stands — the pre-trade gate seam the order pipeline consults for the
// §13.6d MARGIN_CALL_EXCEEDED rejection (consumed by the sibling
// margin-level cluster's pre-trade gate; also bound into gateway order
// admission at wiring time).
func (s *MarginCallService) HasMarginCallBlock(ctx context.Context, accountID int64) (bool, error) {
	n, err := s.rdb.Exists(ctx, MarginCallBlockKey(accountID)).Result()
	if err != nil {
		return false, fmt.Errorf("margin call: block probe acct %d: %w", accountID, err)
	}
	return n > 0, nil
}

// AdminRelease lifts the order block on an audited Risk Manager
// re-enable (§13.3 precedence item 1) without resolving the margin-call
// episode — the account may still liquidate on the scanner.
func (s *MarginCallService) AdminRelease(ctx context.Context, accountID int64, actor int64) error {
	if actor <= 0 {
		return excerrors.New("UNAUTHORIZED_ROLE", "margin-call release requires an actor")
	}
	if err := s.rdb.Del(ctx, MarginCallBlockKey(accountID)).Err(); err != nil {
		return fmt.Errorf("margin call: release acct %d: %w", accountID, err)
	}
	if err := s.store.AuditMarginCallRelease(ctx, accountID, actor); err != nil {
		return err
	}
	return nil
}

// raiseAlert pages ops on degraded margin-call state.
func (s *MarginCallService) raiseAlert(ctx context.Context, severity, code, summary string, details map[string]string) {
	if s.alerter == nil {
		s.logf("margin call: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	}); err != nil {
		s.logf("margin call: alert %s dispatch failed: %v", code, err)
	}
}
