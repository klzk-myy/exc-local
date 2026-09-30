// liquidation.go — §13.5 liquidation engine: queue protocol (§13.15
// item 4 anti-stranding), the 2s scanner watchdog, and the per-position
// close orchestration feeding the §13.4 auction ladder.
//
// Queue contract (Redis):
//
//	liquidation:queue            LIST  — pending jobs (JSON LiquidationJob)
//	liquidation:queue:delayed    ZSET  — retry-backoff set, score = unix ms
//	liquidation:dedup:{acct}     STRING TTL 3600s — one in-flight job per acct
//	lock:liquidation:account:{acct} STRING NX — per-account worker mutex
//
// Anti-stranding protocol (§13.15 item 4): a worker that pops a job but
// cannot take the account lock NEVER drops it — the job re-queues into
// the delayed ZSET with exponential backoff (250ms·2^attempt, ≤5
// attempts, ≤30s cumulative). On exhaustion the worker clears
// liquidation:dedup:{acct}, pages LIQUIDATION_WORKER_LOCK_TIMEOUT (L1)
// and re-enqueues the job fresh — a bankrupt account can never strand.
//
// Close orchestration (§13.5):
//
//	worker takes the account lock → re-reads the margin level (a job is
//	a claim on evaluation, not on liquidation — a recovered account
//	dequeues clean) → lists open positions worst-P&L-first →
//	MassCancel resting orders → per position: notional > 1% of OI opens
//	the §13.4 auction ladder; smaller positions close directly through
//	accounts.OrderDispatcher.SubmitClose (the normal orders pipeline —
//	spec §13.4 bans a separate fill path).
//
// Worst-P&L-first (§13.3 precedence rule 2 / §13.5): positions sort by
// unrealized_pnl ascending; the engine closes until the recomputed level
// recovers above 100% (stop-out target) or the book is flat.
package risk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/accounts"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Liquidation reasons — liquidation_events.kind and queue payloads.
const (
	LiquidationReasonStopOut           = "STOP_OUT"
	LiquidationReasonMarginCallExpired = "MARGIN_CALL_EXPIRED"
	LiquidationReasonManual            = "MANUAL"  // mig 190 flow
	LiquidationReasonScanner           = "SCANNER" // below-maintenance sweep
	// LiquidationReasonIsolatedDeficit — Task 19.3.27 isolated leg whose
	// allocation could not cover the deficit and could not be topped up.
	LiquidationReasonIsolatedDeficit = "ISOLATED_MARGIN_DEFICIT"
)

// LiquidationJob is one liquidation:queue entry.
type LiquidationJob struct {
	V              int       `json:"v,omitempty"` // payload schema version (Task 19.3.26 producer)
	JobID          string    `json:"job_id"`
	AccountID      int64     `json:"account_id"`
	Reason         string    `json:"reason"`
	PositionID     int64     `json:"position_id,omitempty"` // isolated-leg scope
	Symbol         string    `json:"symbol,omitempty"`
	EnqueuedAt     time.Time `json:"enqueued_at"`
	TsMs           int64     `json:"ts_ms,omitempty"`            // producer stamp (event-driven engine)
	MarginLevelPct string    `json:"margin_level_pct,omitempty"` // snapshot at enqueue
	Equity         string    `json:"equity,omitempty"`
	UsedMargin     string    `json:"used_margin,omitempty"`
	Attempts       int       `json:"attempts"`
	FirstQueuedAt  time.Time `json:"first_queued_at"`
}

// Queue retry contract (§13.15 item 4).
const (
	LiquidationMaxAttempts  = 5
	LiquidationLockTTL      = 30 * time.Second // lock:liquidation:account lease
	LiquidationBackoffBase  = 250 * time.Millisecond
	LiquidationRetryBudget  = 30 * time.Second
	liquidationScanCadence  = 2 * time.Second // §13.5 scanner cadence
	liquidationQueuePopWait = 2 * time.Second // BLPOP window == scan cadence
)

// ---------------------------------------------------------------------------
// LiquidationQueue — producer + the delayed-retry machinery
// ---------------------------------------------------------------------------

// LiquidationQueue owns the queue keys. Enqueue is idempotent per
// account via liquidation:dedup:{acct} (3600s) — duplicate evaluations
// coalesce into one job.
type LiquidationQueue struct {
	rdb      *excredis.Client
	dedupTTL time.Duration
	now      func() time.Time
	logf     func(format string, args ...any)
}

// NewLiquidationQueue binds the coordination Redis instance. A nil
// client is rejected fail-closed — a disabled queue would silently
// strand accounts at stop-out (§2.7).
func NewLiquidationQueue(rdb *excredis.Client, logf func(string, ...any)) (*LiquidationQueue, error) {
	if rdb == nil {
		return nil, fmt.Errorf("liquidation queue: nil redis")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &LiquidationQueue{
		rdb:      rdb,
		dedupTTL: LiquidationDedupTTLSeconds * time.Second,
		now:      func() time.Time { return time.Now().UTC() },
		logf:     logf,
	}, nil
}

// WithDedupTTL overrides the dedup window (tests).
func (q *LiquidationQueue) WithDedupTTL(d time.Duration) *LiquidationQueue {
	q.dedupTTL = d
	return q
}

// WithClock injects the clock (tests).
func (q *LiquidationQueue) WithClock(now func() time.Time) *LiquidationQueue {
	q.now = now
	return q
}

// dedupKeyFor scopes the dedup marker: position-scoped jobs (isolated
// legs) dedup on the position so an isolated-leg liquidation never
// suppresses a later account-level stop-out, and vice versa.
func dedupKeyFor(job LiquidationJob) string {
	key := LiquidationDedupKey(job.AccountID)
	if job.PositionID != 0 {
		key = fmt.Sprintf("%s:pos:%d", key, job.PositionID)
	}
	return key
}

// Enqueue claims the dedup key then LPUSHes the job. A held dedup key
// means a job is already in flight — Enqueue is a no-op (nil error).
func (q *LiquidationQueue) Enqueue(ctx context.Context, job LiquidationJob, lv *MarginLevel) error {
	_, err := q.enqueue(ctx, job, lv)
	return err
}

// EnqueueDedup is Enqueue with observability — reports whether the job
// was actually queued (false = dedup suppressed it). The Task 19.3.26
// event-driven margin engine uses it for the enqueue counter.
func (q *LiquidationQueue) EnqueueDedup(ctx context.Context, job LiquidationJob, lv *MarginLevel) (bool, error) {
	return q.enqueue(ctx, job, lv)
}

// EnqueueSnapshot is the convenience overload for the margin engine:
// enqueue for the job's account with the evaluated snapshot carried as
// advisory payload (the worker re-reads margin:level before acting —
// the queue carries a hint, never authority).
func (q *LiquidationQueue) EnqueueSnapshot(ctx context.Context, job LiquidationJob, snap *MarginSnapshot) error {
	var lv *MarginLevel
	if snap != nil {
		lv = &MarginLevel{
			AccountID:  snap.AccountID,
			Equity:     snap.Equity,
			UsedMargin: snap.UsedMargin,
			Status:     snap.Status,
			UpdatedAt:  snap.Ts,
		}
		if snap.LevelPct != nil {
			lv.MarginLevelPct = *snap.LevelPct
		}
	}
	return q.Enqueue(ctx, job, lv)
}

// PeekLen reports the queue depth (monitoring/tests).
func (q *LiquidationQueue) PeekLen(ctx context.Context) (int64, error) {
	return q.rdb.LLen(ctx, LiquidationQueueKey).Result()
}

func (q *LiquidationQueue) enqueue(ctx context.Context, job LiquidationJob, lv *MarginLevel) (bool, error) {
	if q.rdb == nil {
		return false, fmt.Errorf("liquidation queue: nil redis")
	}
	if job.V == 0 {
		job.V = 1
	}
	if job.JobID == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return false, fmt.Errorf("liquidation queue: job id rand: %w", err)
		}
		job.JobID = "liq-" + hex.EncodeToString(b[:])
	}
	if job.Reason == "" {
		job.Reason = LiquidationReasonScanner
	}
	now := q.now()
	job.EnqueuedAt = now
	if job.TsMs == 0 {
		job.TsMs = now.UnixMilli()
	}
	if job.FirstQueuedAt.IsZero() {
		job.FirstQueuedAt = now
	}
	if lv != nil {
		if job.MarginLevelPct == "" {
			job.MarginLevelPct = lv.MarginLevelPct.String()
		}
		if job.Equity == "" {
			job.Equity = lv.Equity.String()
		}
		if job.UsedMargin == "" {
			job.UsedMargin = lv.UsedMargin.String()
		}
	}

	dedupKey := dedupKeyFor(job)
	ok, err := q.rdb.SetNX(ctx, dedupKey, job.JobID, q.dedupTTL).Result()
	if err != nil {
		return false, fmt.Errorf("liquidation queue: dedup acct %d: %w", job.AccountID, err)
	}
	if !ok {
		return false, nil // already in flight — coalesced
	}
	payload, err := json.Marshal(job)
	if err != nil {
		_ = q.rdb.Del(ctx, dedupKey).Err()
		return false, fmt.Errorf("liquidation queue: marshal job: %w", err)
	}
	if err := q.rdb.LPush(ctx, LiquidationQueueKey, payload).Err(); err != nil {
		_ = q.rdb.Del(ctx, dedupKey).Err()
		return false, fmt.Errorf("liquidation queue: push acct %d: %w", job.AccountID, err)
	}
	return true, nil
}

// Release returns a popped job to the delayed ZSET with exponential
// backoff — the lock-contention path that must never drop the job.
func (q *LiquidationQueue) Release(ctx context.Context, job LiquidationJob) error {
	job.Attempts++
	delay := LiquidationBackoffBase << min(job.Attempts-1, 7)
	if elapsed := q.now().Sub(job.FirstQueuedAt); elapsed+delay > LiquidationRetryBudget ||
		job.Attempts >= LiquidationMaxAttempts {
		return errRetryExhausted{job: job}
	}
	score := float64(q.now().Add(delay).UnixMilli())
	if err := q.rdb.ZAdd(ctx, LiquidationDelayedQueueKey,
		goredis.Z{Score: score, Member: mustJSON(job)}).Err(); err != nil {
		return fmt.Errorf("liquidation queue: delay acct %d: %w", job.AccountID, err)
	}
	return nil
}

// errRetryExhausted carries the job out of Release for the worker's
// alert-and-requeue path.
type errRetryExhausted struct{ job LiquidationJob }

func (e errRetryExhausted) Error() string {
	return fmt.Sprintf("liquidation job %s acct %d exhausted %d attempts",
		e.job.JobID, e.job.AccountID, e.job.Attempts)
}

// Requeue forces a job back onto the head of the live list — used after
// retry exhaustion (dedup cleared by the caller first) and by the
// delayed-set promoter.
func (q *LiquidationQueue) Requeue(ctx context.Context, job LiquidationJob) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("liquidation queue: marshal job: %w", err)
	}
	return q.rdb.LPush(ctx, LiquidationQueueKey, payload).Err()
}

// PromoteDelayed moves due delayed-set members back to the live list.
// Called on every worker tick — a ZSET entry whose score ≤ now re-enters
// the FIFO without losing FIFO order relative to fresh enqueues.
func (q *LiquidationQueue) PromoteDelayed(ctx context.Context) (int, error) {
	now := float64(q.now().UnixMilli())
	members, err := q.rdb.ZRangeByScore(ctx, LiquidationDelayedQueueKey,
		&goredis.ZRangeBy{Min: "-inf", Max: fmt.Sprint(now), Count: 64}).Result()
	if err != nil {
		return 0, fmt.Errorf("liquidation queue: delayed scan: %w", err)
	}
	n := 0
	for _, m := range members {
		// ZREM first so a crashed worker never double-delivers: the job
		// is back on the list (or logged lost) before the ZSET entry dies.
		if err := q.rdb.ZRem(ctx, LiquidationDelayedQueueKey, m).Err(); err != nil {
			return n, fmt.Errorf("liquidation queue: delayed remove: %w", err)
		}
		if err := q.rdb.LPush(ctx, LiquidationQueueKey, m).Err(); err != nil {
			return n, fmt.Errorf("liquidation queue: delayed repush: %w", err)
		}
		n++
	}
	return n, nil
}

// Pop takes the head job (blocking up to wait). Returns (nil, nil) on
// timeout — an empty queue is routine.
func (q *LiquidationQueue) Pop(ctx context.Context, wait time.Duration) (*LiquidationJob, error) {
	res, err := q.rdb.BRPop(ctx, wait, LiquidationQueueKey).Result()
	if stderrors.Is(err, goredis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("liquidation queue: pop: %w", err)
	}
	if len(res) < 2 {
		return nil, nil
	}
	var job LiquidationJob
	if err := json.Unmarshal([]byte(res[1]), &job); err != nil {
		// A malformed entry is poison — drop it and clear nothing (the
		// dedup key TTLs out; the alert makes the loss loud).
		return nil, fmt.Errorf("liquidation queue: undecodable job %q: %w", res[1], err)
	}
	return &job, nil
}

// CompleteJob releases the dedup key — called exactly once per job on a
// terminal outcome (liquidated, recovered, or account flat).
func (q *LiquidationQueue) CompleteJob(ctx context.Context, accountID int64) error {
	return q.rdb.Del(ctx, LiquidationDedupKey(accountID)).Err()
}

// CompleteJobFor releases the dedup key under the job's own scope —
// position-scoped jobs clear position keys, never the account marker.
func (q *LiquidationQueue) CompleteJobFor(ctx context.Context, job LiquidationJob) error {
	return q.rdb.Del(ctx, dedupKeyFor(job)).Err()
}

// ClearDedup removes the dedup key without touching the queue — the
// retry-exhaustion path where the job re-enqueues fresh.
func (q *LiquidationQueue) ClearDedup(ctx context.Context, accountID int64) error {
	return q.rdb.Del(ctx, LiquidationDedupKey(accountID)).Err()
}

// ClearDedupFor is ClearDedup under the job's scope.
func (q *LiquidationQueue) ClearDedupFor(ctx context.Context, job LiquidationJob) error {
	return q.rdb.Del(ctx, dedupKeyFor(job)).Err()
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("liquidation queue marshal: %v", err))
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Store seam — PG reads/writes the engine needs
// ---------------------------------------------------------------------------

// LiqPosition is one open position on the liquidation path.
type LiqPosition struct {
	ID               int64
	AccountID        int64
	InstrumentID     int64
	Symbol           string
	Side             string          // LONG | SHORT
	Quantity         decimal.Decimal // signed contract count
	EntryPrice       decimal.Decimal
	MarkPrice        decimal.Decimal
	LiquidationPrice decimal.Decimal // may be zero when unset
	UnrealizedPnl    decimal.Decimal
	MarginUsed       decimal.Decimal
	MarginMode       string // ISOLATED | CROSS | PORTFOLIO
}

// Notional is qty×mark in the instrument's quote ccy.
func (p LiqPosition) Notional() decimal.Decimal {
	return p.Quantity.Abs().Mul(p.MarkPrice)
}

// CloseSide maps the position side to the closing order side.
func (p LiqPosition) CloseSide() accounts.OrderSide {
	if p.Side == "LONG" {
		return accounts.SideSell
	}
	return accounts.SideBuy
}

// LiquidationStore is the PG seam for the engine.
type LiquidationStore interface {
	// OpenPositions lists the account's live positions (quantity <> 0)
	// with instrument symbol + mode. Order: worst unrealized P&L first.
	OpenPositions(ctx context.Context, accountID int64) ([]LiqPosition, error)
	// OpenInterest returns Σ open quantity for an instrument (base units)
	// — the §13.4 "1% of OI" auction-trigger denominator. 0/unknown is
	// honest (auction never triggers without a denominator).
	OpenInterest(ctx context.Context, instrumentID int64) (decimal.Decimal, error)
	// MarginMode returns the account's margin_accounts mode
	// (ISOLATED|CROSS|PORTFOLIO) — CROSS/PORTFOLIO close worst-first
	// until recovery; ISOLATED liquidates the breached position alone.
	MarginMode(ctx context.Context, accountID int64) (string, error)
	// SetMarginAccountStatus mirrors margin_call's transition.
	SetMarginAccountStatus(ctx context.Context, accountID int64, status string) error
	// RecordLiquidationEvent persists one liquidation_events row
	// (mig 230) — the §13.13 private-history source.
	RecordLiquidationEvent(ctx context.Context, ev LiquidationEventRow) (int64, error)
	// MarkPositionClosed zeroes the position row inside tx (the engine
	// settles the wallet through the ledger journal in the same tx).
	MarkPositionClosed(ctx context.Context, tx pgx.Tx, positionID int64,
		closePrice decimal.Decimal, realizedPnl decimal.Decimal) error
	// InsertAuction / AuctionPhaseAdvance / AuctionFill — the §13.4
	// ladder persistence on liquidation_auctions (mig 015).
	InsertAuction(ctx context.Context, a AuctionRow) (int64, error)
	AuctionByID(ctx context.Context, id int64) (*AuctionRow, error)
	UpdateAuctionPhase(ctx context.Context, id int64, phase string,
		floor decimal.Decimal, start, end time.Time) error
	RecordAuctionFill(ctx context.Context, id int64, filledQty decimal.Decimal,
		avgPrice decimal.Decimal, unfilled decimal.Decimal) error
	// ActiveAuctions lists auctions still inside the CALL/EXTEND window —
	// the phase-advancer's input each tick.
	ActiveAuctions(ctx context.Context) ([]AuctionRow, error)
	// AccountsForScan returns margin account ids in CROSS/PORTFOLIO mode
	// (the 2s scanner's candidate set — levels are then read per-account
	// from the margin:level hash; below-maintenance ones Evaluate).
	AccountsForScan(ctx context.Context) ([]int64, error)
	// IsolatedBreaches lists ISOLATED-mode positions whose mark has
	// crossed liquidation_price — liquidated independently (§13.5).
	IsolatedBreaches(ctx context.Context) ([]LiqPosition, error)
}

// LiquidationEventRow is one liquidation_events insert (mig 230).
type LiquidationEventRow struct {
	AccountID                 int64
	PositionID                int64
	InstrumentID              int64
	AuctionID                 *int64
	MarginCallEventID         *int64
	Kind                      string // DIRECT_CLOSE|AUCTION_FILL|FORCE_CASH|ADL
	Side                      string // LONG|SHORT
	Quantity                  decimal.Decimal
	Price                     decimal.Decimal
	MarkPrice                 decimal.Decimal
	InsuranceFundContribution decimal.Decimal // fund delta on this leg (±)
	PenaltyAmount             decimal.Decimal
	ADLQuintile               *int
	JournalEntryID            *int64
}

// ---------------------------------------------------------------------------
// LiquidationService — the engine
// ---------------------------------------------------------------------------

// LiquidationService drives queue consumption, the scanner watchdog and
// the per-position close orchestration.
type LiquidationService struct {
	pool     *pgxpool.Pool
	rdb      *excredis.Client
	store    LiquidationStore
	levels   MarginLevelReader
	queue    *LiquidationQueue
	dispatch accounts.OrderDispatcher // orders.Dispatcher in production
	fund     *InsuranceFundService
	auction  *AuctionEngine
	margin   *MarginCallService // lifecycle hand-off (may be nil in tests)
	nbp      *NBPService        // §13.6c post-liquidation eval (may be nil)
	adl      *ADLEngine         // §13.11 depletion fallback (may be nil)
	adv      ADVSource          // §13.4a/6a slicing yardstick (may be nil)
	alerter  OpsAlerter
	now      func() time.Time
	logf     func(format string, args ...any)
	// slippageBps bounds the synthetic-limit cap for direct closes
	// (Task 2.3.15 contract); FORCE_CASH uses the §13.4 mark×0.95/1.05.
	slippageBps int
	// sliceDelay is the inter-tranche pause — production takes
	// liquidationSliceDelay (2s); tests shorten it via direct
	// construction.
	sliceDelay time.Duration
}

// LiquidationDeps wires the engine. Store, Levels, Queue and Dispatch
// are mandatory — the engine is worthless (and unsafe) without them.
type LiquidationDeps struct {
	Pool       *pgxpool.Pool
	Redis      *excredis.Client
	Store      LiquidationStore
	Levels     MarginLevelReader
	Queue      *LiquidationQueue
	Dispatch   accounts.OrderDispatcher
	Fund       *InsuranceFundService
	Auction    *AuctionEngine
	MarginCall *MarginCallService
	// NBP is the §13.6c retail negative-balance-protection evaluator —
	// invoked once after every account-level liquidation pass completes.
	// Nil disables the hook (the 17:00 ET sweep still catches deficits).
	NBP *NBPService
	// ADL is the §13.11 auto-deleveraging fallback — invoked when a
	// deficiency debit hits a fund below its depletion floor. Nil keeps
	// the plain fail-closed behavior.
	ADL *ADLEngine
	// ADV is the §13.4a/Task-19.3.16-item-6a slicing yardstick: a
	// position above 5% of the instrument's average daily notional
	// liquidates in tranches of min(remaining, 10% ADV) with a 2-second
	// inter-slice delay. Nil or unavailable ADV ⇒ unsliced closes — a
	// liquidation never stalls on missing analytics (the §2.7
	// pessimism for unknown ADV lives in the §13.12 margin add-on).
	ADV     ADVSource
	Alerter OpsAlerter
	// SlippageBps is the synthetic-limit band for direct closes
	// (default 200 = 2% — wider than normal flow, narrower than
	// FORCE_CASH; liquidation must fill).
	SlippageBps int
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

// NewLiquidationService builds the engine.
func NewLiquidationService(d LiquidationDeps) (*LiquidationService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("liquidation: nil pgx pool")
	}
	if d.Redis == nil {
		return nil, fmt.Errorf("liquidation: nil redis — queue + locks live there")
	}
	if d.Store == nil {
		return nil, fmt.Errorf("liquidation: nil store")
	}
	if d.Levels == nil {
		return nil, fmt.Errorf("liquidation: nil margin-level reader")
	}
	if d.Queue == nil {
		return nil, fmt.Errorf("liquidation: nil queue")
	}
	if d.Dispatch == nil {
		return nil, fmt.Errorf("liquidation: nil order dispatcher — closes go through the normal pipeline")
	}
	s := &LiquidationService{
		pool: d.Pool, rdb: d.Redis, store: d.Store, levels: d.Levels,
		queue: d.Queue, dispatch: d.Dispatch, fund: d.Fund,
		auction: d.Auction, margin: d.MarginCall, nbp: d.NBP, adl: d.ADL,
		adv:     d.ADV,
		alerter: d.Alerter,
		now:     d.Now, logf: d.Logf, slippageBps: d.SlippageBps,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	if s.slippageBps <= 0 {
		s.slippageBps = 200
	}
	if s.sliceDelay <= 0 {
		s.sliceDelay = liquidationSliceDelay
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Worker — ConsumeOnce + the anti-stranding protocol (§13.15 item 4)
// ---------------------------------------------------------------------------

// ConsumeOnce pops one job and processes it under the account lock.
// Lock contention NEVER returns success and NEVER drops the job — it
// releases with exponential backoff; on exhaustion it clears dedup,
// pages LIQUIDATION_WORKER_LOCK_TIMEOUT (L1) and re-enqueues fresh so
// the next scanner pass re-enters the account cleanly.
func (s *LiquidationService) ConsumeOnce(ctx context.Context) error {
	job, err := s.queue.Pop(ctx, liquidationQueuePopWait)
	if err != nil {
		return err
	}
	if job == nil {
		return nil
	}
	tok, err := liquidationLockToken()
	if err != nil {
		return err
	}
	ok, err := s.rdb.SetNX(ctx, LiquidationLockKey(job.AccountID), tok,
		LiquidationLockTTL).Result()
	if err != nil {
		return s.strandGuard(ctx, *job, fmt.Errorf("lock acct %d: %w", job.AccountID, err))
	}
	if !ok {
		// Contention — release with backoff, never drop.
		return s.strandGuard(ctx, *job, nil)
	}
	defer s.releaseLock(ctx, job.AccountID, tok)

	if err := s.liquidateAccount(ctx, *job); err != nil {
		// A mid-liquidation failure is retried through the same delayed
		// protocol — the account lock still serializes the retry.
		return s.strandGuard(ctx, *job, err)
	}
	// Terminal state reached — release the dedup claim.
	if err := s.queue.CompleteJobFor(ctx, *job); err != nil {
		s.logf("liquidation: dedup clear acct %d: %v", job.AccountID, err)
	}
	return nil
}

// strandGuard implements the §13.15 item 4 release/retry contract.
func (s *LiquidationService) strandGuard(ctx context.Context, job LiquidationJob, cause error) error {
	if rerr := s.queue.Release(ctx, job); rerr != nil {
		var exh errRetryExhausted
		if stderrors.As(rerr, &exh) {
			// §13.15: clear dedup, page L1, re-enqueue fresh — the scanner
			// will re-enter the account if it is still breached.
			if err := s.queue.ClearDedupFor(ctx, job); err != nil {
				s.logf("liquidation: dedup clear on exhaustion acct %d: %v", job.AccountID, err)
			}
			s.raiseAlert(ctx, SeverityP1, CodeLiquidationWorkerLockTimeout, fmt.Sprintf(
				"liquidation job %s for account %d exhausted %d attempts — dedup cleared, re-enqueued",
				job.JobID, job.AccountID, job.Attempts), map[string]string{
				"account_id": fmt.Sprint(job.AccountID), "job_id": job.JobID,
				"reason": job.Reason})
			fresh := LiquidationJob{AccountID: job.AccountID, Reason: job.Reason,
				PositionID: job.PositionID}
			if err := s.queue.Requeue(ctx, fresh); err != nil {
				s.logf("liquidation: requeue after exhaustion acct %d: %v", job.AccountID, err)
			}
			if cause != nil {
				return excerrors.Wrap(CodeLiquidationFailed, "job exhausted retries", cause)
			}
			return nil
		}
		return rerr
	}
	return nil // released — not an error (the job is safe, parked)
}

// releaseLock drops the per-account mutex (token-guarded compare-del).
var liquidationUnlockScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)

func (s *LiquidationService) releaseLock(ctx context.Context, accountID int64, tok string) {
	if _, err := liquidationUnlockScript.Run(ctx, s.rdb,
		[]string{LiquidationLockKey(accountID)}, tok).Result(); err != nil {
		s.logf("liquidation: unlock acct %d: %v", accountID, err)
	}
}

func liquidationLockToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("liq-%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:])), nil
}

// ---------------------------------------------------------------------------
// Scanner — the §13.5 2-second watchdog sweep
// ---------------------------------------------------------------------------

// ScanOnce runs one scanner pass: promote due retries, sweep expired
// margin-call windows, liquidate breached ISOLATED positions, and
// Evaluate every CROSS/PORTFOLIO margin account against its live level.
// Returns candidate counts for observability.
func (s *LiquidationService) ScanOnce(ctx context.Context) (int, error) {
	if _, err := s.queue.PromoteDelayed(ctx); err != nil {
		s.logf("liquidation: delayed promote: %v", err)
	}
	if s.auction != nil {
		if _, err := s.auction.AdvanceOnce(ctx); err != nil {
			s.logf("liquidation: auction advance: %v", err)
		}
	}
	if s.margin != nil {
		if _, err := s.margin.SweepExpired(ctx); err != nil {
			s.logf("liquidation: margin-call sweep: %v", err)
		}
	}
	n := 0
	// ISOLATED positions liquidate independently of account-level state.
	isolated, err := s.store.IsolatedBreaches(ctx)
	if err != nil {
		return n, excerrors.Wrap("INTERNAL_ERROR", "liquidation: isolated breach scan", err)
	}
	for _, p := range isolated {
		if err := s.liquidateIsolated(ctx, p); err != nil {
			s.logf("liquidation: isolated acct %d pos %d: %v", p.AccountID, p.ID, err)
			continue
		}
		n++
	}
	// CROSS/PORTFOLIO accounts evaluate against the margin:level hash.
	accts, err := s.store.AccountsForScan(ctx)
	if err != nil {
		return n, excerrors.Wrap("INTERNAL_ERROR", "liquidation: account scan", err)
	}
	for _, acctID := range accts {
		if s.margin != nil {
			// MarginCallService.Evaluate owns the 111.1% trigger → window /
			// stop-out → enqueue transitions; the scanner is its cadence.
			if err := s.margin.Evaluate(ctx, acctID); err != nil {
				s.logf("liquidation: evaluate acct %d: %v", acctID, err)
			}
		}
	}
	return n, nil
}

// liquidateIsolated force-closes one breached ISOLATED position under
// the account lock — the account may be otherwise healthy (§13.5).
func (s *LiquidationService) liquidateIsolated(ctx context.Context, p LiqPosition) error {
	tok, err := liquidationLockToken()
	if err != nil {
		return err
	}
	ok, err := s.rdb.SetNX(ctx, LiquidationLockKey(p.AccountID), tok, LiquidationLockTTL).Result()
	if err != nil {
		return fmt.Errorf("liquidation: isolated lock acct %d: %w", p.AccountID, err)
	}
	if !ok {
		// Another worker owns the account — enqueue the position through
		// the standard path; the account job will pick it up.
		lv, _ := s.levels.MarginLevel(ctx, p.AccountID)
		return s.queue.Enqueue(ctx, LiquidationJob{
			AccountID: p.AccountID, Reason: LiquidationReasonScanner,
		}, lv)
	}
	defer s.releaseLock(ctx, p.AccountID, tok)
	return s.closePosition(ctx, p, LiquidationReasonScanner, nil)
}

// ---------------------------------------------------------------------------
// Close orchestration — the account-level liquidation pass
// ---------------------------------------------------------------------------

// liquidateAccount runs one queued job: level re-check (recovery exits
// clean), MassCancel resting orders, then worst-P&L-first closes until
// level > 100% or the book is flat. Positions close DIRECT below the
// §13.4 1%-of-OI auction trigger and through the auction ladder above it.
func (s *LiquidationService) liquidateAccount(ctx context.Context, job LiquidationJob) error {
	lv, err := s.levels.MarginLevel(ctx, job.AccountID)
	if err != nil {
		return excerrors.Wrap(CodeLiquidationFailed, "level re-read", err)
	}
	mode, err := s.store.MarginMode(ctx, job.AccountID)
	if err != nil {
		return excerrors.Wrap(CodeLiquidationFailed, "margin mode read", err)
	}
	// Recovery gate: a CROSS/PORTFOLIO job whose level has already
	// recovered above the stop-out exits clean — liquidation is a claim
	// on evaluation, never on liquidation itself.
	if lv != nil && mode != "ISOLATED" && job.Reason != LiquidationReasonManual &&
		lv.MarginLevelPct.GreaterThan(decimal.NewFromInt(100)) &&
		lv.IsMarginCallLevel(MarginCallThresholdPct) == false &&
		lv.MarginLevelPct.GreaterThanOrEqual(decimal.NewFromInt(100)) {
		s.logf("liquidation: acct %d recovered to %s%% — dequeuing clean",
			job.AccountID, lv.MarginLevelPct)
		return nil
	}

	positions, err := s.store.OpenPositions(ctx, job.AccountID)
	if err != nil {
		return excerrors.Wrap(CodeLiquidationFailed, "position read", err)
	}
	if len(positions) == 0 {
		return nil // flat already — dedup clears, job done
	}
	sort.Slice(positions, func(i, j int) bool { // worst P&L first — belt & braces over the store ORDER BY
		return positions[i].UnrealizedPnl.LessThan(positions[j].UnrealizedPnl)
	})

	// §13.5 first step: cancel every resting order so frees margin before
	// the closes reprice the level.
	if _, err := s.dispatch.MassCancel(ctx, accounts.MassCancelScope{
		AccountID: job.AccountID, Reason: "liquidation",
	}); err != nil {
		return excerrors.Wrap(CodeLiquidationFailed, "mass cancel", err)
	}

	var marginCallEvID *int64
	if s.margin != nil {
		if open, _ := s.margin.store.OpenMarginCall(ctx, job.AccountID); open != nil {
			marginCallEvID = &open.ID
		}
	}

	for _, p := range positions {
		if err := s.closeTranches(ctx, p, job.Reason, marginCallEvID); err != nil {
			return err // strandGuard retries the whole job under the lock
		}
		if mode != "ISOLATED" {
			// Stop-out recovery check — §13.3: liquidate worst-first
			// "until margin level > 100%". The margin-level producer
			// refreshes margin:level on each close; read it fresh.
			fresh, lerr := s.levels.MarginLevel(ctx, job.AccountID)
			if lerr == nil && fresh != nil &&
				fresh.MarginLevelPct.GreaterThan(decimal.NewFromInt(100)) {
				s.logf("liquidation: acct %d recovered above 100%% after %d closes — stopping",
					job.AccountID, 0)
				break
			}
		}
	}
	// §13.6c: the retail NBP evaluation follows full position
	// liquidation — fund debits land in RecordFill per-fill, this
	// pass floors residual negative equity. EvaluateAccount is
	// retail-gated and equity-checked, so non-deficit exits are cheap.
	// The evaluation runs outside the retry ladder — a failure here
	// must NOT resurrect closed positions via a job retry.
	if s.nbp != nil {
		if _, err := s.nbp.EvaluateAccount(ctx, job.AccountID); err != nil {
			s.logf("liquidation: NBP eval acct %d failed: %v", job.AccountID, err)
		}
	}
	return nil
}

// closePosition routes one position through the §13.4 decision:
// notional > 1% of instrument OI opens the auction ladder; smaller
// positions close directly through the normal pipeline (SubmitClose —
// reduce-only synthetic limit IOC, never a separate fill path).
func (s *LiquidationService) closePosition(ctx context.Context, p LiqPosition,
	reason string, marginCallEvID *int64) error {

	if s.auction != nil {
		oi, err := s.store.OpenInterest(ctx, p.InstrumentID)
		if err != nil {
			return excerrors.Wrap(CodeLiquidationFailed, "open interest read", err)
		}
		// §13.4 trigger: liquidated notional > 1% of open interest.
		if oi.IsPositive() && p.Notional().GreaterThan(oi.Mul(decimal.NewFromFloat(0.01))) {
			return s.auction.Open(ctx, p, reason, marginCallEvID)
		}
	}
	return s.directClose(ctx, p, reason, marginCallEvID, s.slippageBps)
}

// liquidationSliceDelay is the §13.4a/Task-19.3.16-item-6a inter-tranche
// pause — slices ride the normal order pipeline and the level re-check
// needs the fills to land before the next tranche dispatches.
const liquidationSliceDelay = 2 * time.Second

var (
	// advSliceTrigger — a position above 5% of instrument ADV unwinds in
	// tranches rather than one block (market-impact bound).
	advSliceTrigger = decimal.RequireFromString("0.05")
	// advSliceShare — each tranche is at most 10% of ADV.
	advSliceShare = decimal.RequireFromString("0.10")
)

// closeTranches applies the Task-19.3.16-item-6a ADV slicing rule:
// positions whose notional exceeds 5% of the instrument's average
// daily volume liquidate in tranches of min(remaining, 10% ADV) with a
// 2-second inter-slice delay; every tranche re-enters the routing
// decision (direct vs auction per the 1%-of-OI trigger) and the
// account-level recovery check halts the ladder early when margin
// level recovers above 100%. Unknown/unavailable ADV ⇒ unsliced close
// — a liquidation must never stall on missing analytics (the §2.7
// pessimism for unknown ADV lives in the §13.12 margin add-on, not in
// delaying risk reduction).
func (s *LiquidationService) closeTranches(ctx context.Context, p LiqPosition,
	reason string, marginCallEvID *int64) error {

	if s.adv == nil {
		return s.closePosition(ctx, p, reason, marginCallEvID)
	}
	mark := p.MarkPrice
	if !mark.IsPositive() {
		mark = s.liquidationPriceFor(ctx, p.ID)
	}
	adv, err := s.adv.ADV(ctx, p.InstrumentID)
	if err != nil || !adv.IsPositive() || !mark.IsPositive() ||
		!p.Notional().GreaterThan(adv.Mul(advSliceTrigger)) {
		return s.closePosition(ctx, p, reason, marginCallEvID)
	}
	// Tranche qty in base units = 10% ADV notional ÷ mark, capped at
	// the remaining position; sign follows the original quantity.
	slice := adv.Mul(advSliceShare).Div(mark)
	remaining := p.Quantity.Abs()
	for remaining.IsPositive() {
		q := remaining
		if q.GreaterThan(slice) {
			q = slice
		}
		tr := p
		tr.Quantity = q
		if p.Quantity.IsNegative() {
			tr.Quantity = q.Neg()
		}
		if err := s.closePosition(ctx, tr, reason, marginCallEvID); err != nil {
			return err
		}
		remaining = remaining.Sub(q)
		if !remaining.IsPositive() {
			break
		}
		// Early halt (§13.3 precedence): a recovered level ends the
		// ladder — tranches past the first are optional capacity, not
		// mandatory flow.
		if lv, lerr := s.levels.MarginLevel(ctx, p.AccountID); lerr == nil && lv != nil &&
			lv.MarginLevelPct.GreaterThan(decimal.NewFromInt(100)) {
			s.logf("liquidation: acct %d recovered above 100%% mid-tranche — halting slices",
				p.AccountID)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.sliceDelay):
		}
	}
	return nil
}

// directClose submits a reduce-only close through the orders dispatcher
// and persists the liquidation_events row. The fill arrives async via
// the engine ack path; the row records the dispatch economics.
func (s *LiquidationService) directClose(ctx context.Context, p LiqPosition,
	reason string, marginCallEvID *int64, slippageBps int) error {

	ack, err := s.dispatch.SubmitClose(ctx, accounts.CloseOrderRequest{
		AccountID:      p.AccountID,
		InstrumentID:   p.InstrumentID,
		Side:           p.CloseSide(),
		Quantity:       p.Quantity.Abs(),
		ReduceOnly:     true,
		MaxSlippageBps: slippageBps,
		ClientOrderID:  fmt.Sprintf("liq-%d-%d", p.ID, s.now().UnixMilli()),
	})
	if err != nil {
		return excerrors.Wrap(CodeLiquidationFailed,
			fmt.Sprintf("close dispatch pos %d", p.ID), err)
	}
	if ack == nil || !ack.Accepted {
		return excerrors.New(CodeLiquidationFailed,
			fmt.Sprintf("close rejected pos %d: %s", p.ID, ackDetail(ack)))
	}
	// Penalty: the liquidation spread accrues to the insurance fund
	// (§13.6a/19.3.4). At dispatch time we record the event; the fund
	// credit posts on fill reconciliation (FillReport seam below) once
	// the realized price is known.
	_, err = s.store.RecordLiquidationEvent(ctx, LiquidationEventRow{
		AccountID:         p.AccountID,
		PositionID:        p.ID,
		InstrumentID:      p.InstrumentID,
		MarginCallEventID: marginCallEvID,
		Kind:              "DIRECT_CLOSE",
		Side:              p.Side,
		Quantity:          p.Quantity.Abs(),
		Price:             p.MarkPrice, // provisional — reconciled on fill
		MarkPrice:         p.MarkPrice,
	})
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "liquidation event row", err)
	}
	return nil
}

func ackDetail(ack *accounts.OrderAck) string {
	if ack == nil {
		return "no ack"
	}
	return ack.Detail
}

// ---------------------------------------------------------------------------
// Fill reconciliation — the engine-ack seam
// ---------------------------------------------------------------------------

// LiquidationFill is one execution report for a liquidation close or an
// auction fill. The orders Consumer binds this (ExecReport path).
type LiquidationFill struct {
	PositionID            int64
	AccountID             int64
	InstrumentID          int64
	Side                  string          // position side (LONG|SHORT)
	Qty                   decimal.Decimal // filled quantity
	Price                 decimal.Decimal // realized fill price
	MarkPrice             decimal.Decimal // mark at fill (penalty basis)
	IsAuction             bool
	AuctionID             *int64
	IsForceCash           bool
	IsADL                 bool
	ADLQuintile           *int
	CounterpartyAccountID int64 // LP/ADL counterparty for rebate flow
}

// RecordFill reconciles one execution report: closes the position row,
// posts the liquidation journal (realized P&L + penalty to fund through
// the InsuranceFundService GL path), and settles the deficiency.
//
// Penalty math (§13.6a): when a LONG closes above its liquidation_price
// (or a SHORT below), the spread vs liquidation_price belongs to the
// fund; a shortfall vs liquidation_price is the fund's deficiency to
// cover (fund debit) — the retail NBP write-off follows on nbp.go.
func (s *LiquidationService) RecordFill(ctx context.Context, f LiquidationFill) error {
	if !f.Price.IsPositive() || !f.Qty.IsPositive() {
		return excerrors.New("INVALID_REQUEST", "fill requires positive price and qty")
	}
	// Auction bookkeeping first — the ladder's residual math drives the
	// phase machine; an auction fill must never bypass it.
	if f.IsAuction && f.AuctionID != nil && s.auction != nil {
		row, rerr := s.store.AuctionByID(ctx, *f.AuctionID)
		if rerr != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "auction row read", rerr)
		}
		if row != nil {
			filled := row.FilledQty.Add(f.Qty)
			unfilled := row.OriginalQty().Sub(filled)
			if unfilled.IsNegative() {
				unfilled = decimal.Zero
			}
			avg := row.AvgFillPrice
			if filled.IsPositive() {
				avg = row.AvgFillPrice.Mul(row.FilledQty).
					Add(f.Price.Mul(f.Qty)).Div(filled).Round(8)
			}
			if err := s.auction.OnFill(ctx, *f.AuctionID, filled, avg, unfilled); err != nil {
				return err
			}
		}
	}
	penalty, deficiency := PenaltyAndDeficiency(f.Side, f.Qty, f.Price, f.MarkPrice,
		s.liquidationPriceFor(ctx, f.PositionID))

	ccy, err := s.accountCurrency(ctx, f.AccountID)
	if err != nil {
		return err
	}
	// Deficiency first (fund pays), penalty second (fund receives) — both
	// through the fund service so every move carries its GL journal.
	if deficiency.IsPositive() && s.fund != nil {
		if _, err := s.fund.Debit(ctx, FundMovement{
			Reason: FundReasonAuctionDeficiency, Currency: ccy,
			Amount: deficiency, ReferenceType: "liquidation",
			ReferenceID: f.PositionID, AccountID: f.AccountID,
			IdempotencyKey: fmt.Sprintf("liq-deficiency:%d:%d", f.PositionID, f.MarkPrice.IntPart()),
		}); err != nil {
			// §13.11: when the fund is below its §13.6 depletion floor
			// the uncovered quantity deleverages profitable opposing
			// counterparties at bankruptcy price. TriggerADL re-checks
			// the depletion gate itself — a non-depletion debit failure
			// returns Triggered=false and keeps the fail-closed error.
			if s.adl == nil || !s.tryADL(ctx, f, ccy) {
				return excerrors.Wrap(CodeInsuranceFundExhausted,
					"deficiency cover failed — fail closed", err)
			}
		}
	}
	if penalty.IsPositive() && s.fund != nil {
		if _, err := s.fund.Credit(ctx, FundMovement{
			Reason: FundReasonLiquidationPenalty, Currency: ccy,
			Amount: penalty, ReferenceType: "liquidation",
			ReferenceID: f.PositionID, AccountID: f.AccountID,
			IdempotencyKey: fmt.Sprintf("liq-penalty:%d:%d", f.PositionID, f.MarkPrice.IntPart()),
		}); err != nil {
			return excerrors.Wrap(CodeLiquidationFailed, "penalty credit", err)
		}
	}
	// LP rebate (§13.4): 0.05% of the filled notional from the fund to
	// the LP that took the auction flow — only on auction/force-cash legs.
	if (f.IsAuction || f.IsForceCash) && f.CounterpartyAccountID > 0 && s.fund != nil {
		rebate := f.Qty.Mul(f.Price).Mul(DefaultLPRebateFraction).Round(8)
		if rebate.IsPositive() {
			if _, err := s.fund.Debit(ctx, FundMovement{
				Reason: FundReasonLPRebate, Currency: ccy,
				Amount: rebate, ReferenceType: "auction",
				ReferenceID: f.PositionID, AccountID: f.CounterpartyAccountID,
				IdempotencyKey: fmt.Sprintf("liq-rebate:%d:%d", f.PositionID, f.CounterpartyAccountID),
			}); err != nil {
				// Rebate failure is non-fatal to the fill (the position is
				// already closed) — page ops; the ledger keeps the truth.
				s.raiseAlert(ctx, SeverityP1, "LP_REBATE_FAILED",
					fmt.Sprintf("LP rebate on fill pos %d failed", f.PositionID),
					map[string]string{"position_id": fmt.Sprint(f.PositionID)})
			}
		}
	}
	return nil
}

// PenaltyAndDeficiency decomposes one liquidation fill: a fill better
// than the position's liquidation_price yields the fund penalty (the
// venue's recovery margin); a worse fill yields the deficiency the fund
// must cover. liquidation_price = 0 falls back to mark (no spread).
func PenaltyAndDeficiency(side string, qty, fillPrice, markPrice,
	liqPrice decimal.Decimal) (penalty, deficiency decimal.Decimal) {

	basis := liqPrice
	if !basis.IsPositive() {
		basis = markPrice
	}
	if !basis.IsPositive() {
		return decimal.Zero, decimal.Zero
	}
	var spread decimal.Decimal
	if side == "LONG" {
		spread = fillPrice.Sub(basis) // sold to close: above basis = surplus
	} else {
		spread = basis.Sub(fillPrice) // bought to close: below basis = surplus
	}
	amt := spread.Mul(qty.Abs())
	if amt.IsPositive() {
		return amt.Round(8), decimal.Zero
	}
	return decimal.Zero, amt.Neg().Round(8)
}

// liquidationPriceFor resolves the position's stored liquidation_price
// (0 when unknown → the caller's mark fallback applies).
func (s *LiquidationService) liquidationPriceFor(ctx context.Context, positionID int64) decimal.Decimal {
	var txt *string
	if err := s.pool.QueryRow(ctx,
		`SELECT liquidation_price::text FROM positions WHERE id=$1`, positionID).Scan(&txt); err != nil || txt == nil {
		return decimal.Zero
	}
	return decimal.RequireFromString(*txt)
}

// tryADL attempts the §13.11 fallback for one uncovered deficiency:
// profitable opposing counterparties are force-closed at bankruptcy
// price until the deficit quantity is covered. Returns true only when
// ADL triggered AND covered the full deficit — a partial cover or a
// trigger error leaves the caller's fail-closed path intact.
func (s *LiquidationService) tryADL(ctx context.Context, f LiquidationFill, ccy string) bool {
	bankruptcy := s.liquidationPriceFor(ctx, f.PositionID)
	if !bankruptcy.IsPositive() {
		bankruptcy = f.MarkPrice // bankruptcy proxy: liquidation price, mark fallback
	}
	sym, err := s.instrumentSymbol(ctx, f.InstrumentID)
	if err != nil || sym == "" || !bankruptcy.IsPositive() {
		s.logf("liquidation: ADL trigger skipped — symbol/bankruptcy unresolvable for pos %d", f.PositionID)
		return false
	}
	res, err := s.adl.TriggerADL(ctx, ADLTriggerRequest{
		LiquidatedAccountID:  f.AccountID,
		LiquidatedPositionID: f.PositionID,
		InstrumentID:         f.InstrumentID,
		Symbol:               sym,
		LiquidatedSide:       f.Side,
		DeficitQty:           f.Qty,
		BankruptcyPrice:      bankruptcy,
		Currency:             ccy,
		MarkPrice:            f.MarkPrice,
	})
	if err != nil {
		s.logf("liquidation: ADL trigger pos %d failed: %v", f.PositionID, err)
		return false
	}
	if !res.Triggered {
		return false // fund above the depletion floor — not an ADL case
	}
	s.logf("liquidation: ADL engaged pos %d — %d counterparties dispatched %d covered %s shortfall %s",
		f.PositionID, res.Counterparties, res.Dispatched, res.CoveredQty, res.ShortfallQty)
	return res.ShortfallQty.IsZero()
}

// instrumentSymbol resolves instruments.symbol for the ADL trigger
// request (the fill carries the numeric id only).
func (s *LiquidationService) instrumentSymbol(ctx context.Context, instrumentID int64) (string, error) {
	var sym string
	if err := s.pool.QueryRow(ctx,
		`SELECT symbol FROM instruments WHERE id=$1`, instrumentID).Scan(&sym); err != nil {
		return "", err
	}
	return sym, nil
}

// accountCurrency resolves the account's reporting currency for fund
// movements (accounts.base_currency — the venue's fund denominations are
// per-currency rows; the deficit currency follows the account's base).
func (s *LiquidationService) accountCurrency(ctx context.Context, accountID int64) (string, error) {
	var ccy string
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(base_currency,'USD') FROM accounts WHERE id=$1`, accountID).Scan(&ccy); err != nil {
		return "", excerrors.Wrap("INTERNAL_ERROR", "account currency read", err)
	}
	return ccy, nil
}

// raiseAlert pages ops on liquidation-path failures.
func (s *LiquidationService) raiseAlert(ctx context.Context, severity, code, summary string, details map[string]string) {
	if s.alerter == nil {
		s.logf("liquidation: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.alerter.Raise(actx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	}); err != nil {
		s.logf("liquidation: alert %s dispatch failed: %v", code, err)
	}
}
