// Task 4.3.10 — end-to-end crash recovery & cross-region DR orchestration
// (spec §18.6, §24 #335); Task 4.3.12 — per-shard scoped reopen &
// external-feed fallback (§18.6.7, §24 #354); Task 4.3.11 stage budgets
// (§24 #353).
//
// The RecoveryOrchestrator owns the §18.6 reopen state machine per shard:
//
//	DOWN → AUDITING → (HALT_LEGAL_FREEZE | CANCEL_ONLY → CALL_AUCTION → NORMAL)
//
// plus the fencing/promotion path (epoch-lease verify, stale-leader
// revocation, warm-standby promotion inside RTO ≤ 3s), the multi-region
// DR step sequence (PG semi-sync verify → Sentinel promotion → WAL
// archive replay catch-up), and WAL dirty-block flush on termination.
//
// All storage/feed/session dependencies are interfaces so the whole engine
// is exercised with in-process fakes; production wiring lives in
// cmd/recovery-orchestrator + pg_source.go + redis_lease.go.
//
// Fail-closed contract (spec §2.7): every failure path ends in a typed
// error and/or a frozen shard. Nothing degrades silently.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Error codes (spec §23 names; canonical HTTP registration is Phase-05
// Task 5.3.21 — these constants are the emitted strings).
// ---------------------------------------------------------------------------

const (
	// OrchCodeLedgerImbalanceAbort — spec stage-1 hard failure (L0).
	OrchCodeLedgerImbalanceAbort = "LEDGER_IMBALANCE_ABORT"
	// OrchCodeCancelOnly — new non-cancel order during the 60s grace.
	OrchCodeCancelOnly = "ORDER_REJECTED_CANCEL_ONLY_MODE"
	// OrchCodeServiceDegraded — cross-shard basket touching a frozen shard,
	// or any order admitted against a non-NORMAL shard.
	OrchCodeServiceDegraded = "SERVICE_DEGRADED"
	// OrchCodeStageOverrun — a stage exceeded its budget (P1, fail closed).
	OrchCodeStageOverrun = "RECOVERY_STAGE_OVERRUN"
	// OrchCodeFeedUnreachable — bank/CLS feed unreachable past the 120s
	// deferral deadline; carried inside recovery_reports detail, and the
	// sentinel error below marks feed-side reachability failures.
	OrchCodeFeedUnreachable = "EXTERNAL_FEED_UNREACHABLE"
)

// OrchErrFeedUnreachable is the sentinel a pluggable feed returns (wrapped)
// when the counterparty cannot be reached. Only errors.Is-matching failures
// are eligible for the 120s deferral — data that ARRIVED but mismatched is
// an ordinary stage FAIL.
var OrchErrFeedUnreachable = errors.New("external feed unreachable")

// ---------------------------------------------------------------------------
// Canonical tunables (spec §18.6 / §18.6.7)
// ---------------------------------------------------------------------------

const (
	// OrchPromotionRTO — warm-standby promotion must complete inside 3s.
	OrchPromotionRTO = 3 * time.Second
	// OrchCancelOnlyGrace — CANCEL_ONLY grace before auction (60s).
	OrchCancelOnlyGrace = 60 * time.Second
	// OrchAuctionWindow — call-auction uncrossing window (5s).
	OrchAuctionWindow = 5 * time.Second
	// OrchFeedDeadline — bank/CLS unreachability horizon before deferral
	// (Task 4.3.12: "unreachable past a 120s deadline").
	OrchFeedDeadline = 120 * time.Second
	// OrchFeedPoll — retry cadence while waiting on a feed.
	OrchFeedPoll = 5 * time.Second
	// OrchAuditRTO — whole-audit wall-clock bound (RTO ≤ 5min).
	OrchAuditRTO = 5 * time.Minute
	// OrchDRRTO — multi-region failover bound (spec DR table: ≤ 5min).
	OrchDRRTO = 5 * time.Minute
	// OrchDRRPO — PostgreSQL semi-sync lag bound for a safe failover
	// (spec DR table: ≤ 15s).
	OrchDRRPO = 15 * time.Second
)

// OrchStageBudgets are the per-stage wall-clock budgets from Task 4.3.11.
// Stages run concurrently per shard (and shards run in parallel), so the
// union is bounded by the largest single budget plus the feed deadline —
// comfortably inside the 5-minute audit RTO.
type OrchStageBudgets struct {
	Ledger    time.Duration // stage 1+2 ledger scans (zero-sum, coverage, non-negativity)
	BookWAL   time.Duration // stage 3 book_seq == wal_tail
	Monotonic time.Duration // stage 4 orphan/monotonic check
	Nostro    time.Duration // stage 5 reconcile work (feed wait excluded, bounded by FeedDeadline)
	CLS       time.Duration // stage 6 reconcile work (same)
	Margin    time.Duration // margin consistency
}

// OrchDefaultStageBudgets is the spec Table: 60/30/60/90/60/30.
func OrchDefaultStageBudgets() OrchStageBudgets {
	return OrchStageBudgets{
		Ledger:    60 * time.Second,
		BookWAL:   30 * time.Second,
		Monotonic: 60 * time.Second,
		Nostro:    90 * time.Second,
		CLS:       60 * time.Second,
		Margin:    30 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// Shard state machine
// ---------------------------------------------------------------------------

// OrchShardState is the §18.6 reopen state per shard.
type OrchShardState string

const (
	OrchStateDown       OrchShardState = "DOWN"
	OrchStateAuditing   OrchShardState = "AUDITING"
	OrchStateFrozen     OrchShardState = "HALT_LEGAL_FREEZE"
	OrchStateCancelOnly OrchShardState = "CANCEL_ONLY"
	OrchStateAuction    OrchShardState = "CALL_AUCTION"
	OrchStateNormal     OrchShardState = "NORMAL"
)

// OrchSessionStatus maps ladder states to FIX TradingSessionStatus (35=h,
// tag 340) values for the broadcast seam.
type OrchSessionStatus string

const (
	OrchSessionHalt    OrchSessionStatus = "HALT"     // 340=3 (Not-Active/Halted)
	OrchSessionPreOpen OrchSessionStatus = "PRE_OPEN" // 340=4 (CANCEL_ONLY grace)
	OrchSessionAuction OrchSessionStatus = "AUCTION"  // call auction in progress
	OrchSessionOpen    OrchSessionStatus = "OPEN"     // 340=2 continuous trading
)

// ---------------------------------------------------------------------------
// Pluggable seams
// ---------------------------------------------------------------------------

// OrchLeaseBackend is the Redis epoch-lease facade (engine:leader:{shard},
// value "{token}:{epoch}" — spec §4.2/§18.6.2).
type OrchLeaseBackend interface {
	// LeaderValue reads the current lease value; ok=false when absent.
	LeaderValue(ctx context.Context, shardID int) (value string, ok bool, err error)
	// RevokeLeader deletes the lease only if it still equals expectValue
	// (token-checked delete — never kill a re-acquired lease).
	RevokeLeader(ctx context.Context, shardID int, expectValue string) (bool, error)
	// AcquireLeader attempts SET NX PX; epoch is embedded in the value.
	AcquireLeader(ctx context.Context, shardID int, token string, epoch uint64, ttl time.Duration) (bool, error)
}

// OrchParseLeaderValue splits "{token}:{epoch}".
func OrchParseLeaderValue(v string) (token string, epoch uint64, err error) {
	i := strings.LastIndexByte(v, ':')
	if i <= 0 || i == len(v)-1 {
		return "", 0, fmt.Errorf("leader value %q not in {token}:{epoch} form", v)
	}
	ep, err := strconv.ParseUint(v[i+1:], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("leader value %q: bad epoch: %w", v, err)
	}
	return v[:i], ep, nil
}

// OrchStandbyRegistry resolves the warm-standby token for a shard.
type OrchStandbyRegistry interface {
	NextStandby(ctx context.Context, shardID int) (token string, err error)
}

// OrchLivenessProbe answers "is this leader token still alive" — heartbeats,
// process table, IPC ping; the implementation is deployment-specific.
type OrchLivenessProbe interface {
	LeaderAlive(ctx context.Context, shardID int, token string) (bool, error)
}

// OrchWalFlusher flushes dirty memory-aligned 4KB WAL blocks for a shard
// (termination path — every dirty block must hit durable media before the
// process exits; the C++ Wal owns the buffers, this is the trigger seam).
type OrchWalFlusher interface {
	FlushDirtyBlocks(ctx context.Context, shardID int) error
}

// OrchDRDriver is the multi-region failover driver seam (live infra is
// unavailable in this environment — production implementations talk to
// Patroni/repmgr, Sentinel and the S3 archive; tests use fakes).
type OrchDRDriver interface {
	// VerifyPGSemiSync confirms the secondary is within RPO for a safe
	// promotion; returns the observed replication lag.
	VerifyPGSemiSync(ctx context.Context) (lag time.Duration, err error)
	// PromoteRedisSecondary promotes the DR-region Redis master via
	// Sentinel; returns the new master address.
	PromoteRedisSecondary(ctx context.Context) (newMasterAddr string, err error)
	// ReplayWALArchive replays S3 WAL archive catch-up for a shard
	// (exchange:replay-from-archive path, Task 4.3.3's mechanism driven
	// as an orchestration step); returns the WAL seq reached.
	ReplayWALArchive(ctx context.Context, shardID int, fromSeq uint64) (toSeq uint64, err error)
	// SecondaryWALTail returns the DR secondary's current WAL tail for a
	// shard (the fromSeq for replay catch-up).
	SecondaryWALTail(ctx context.Context, shardID int) (uint64, error)
}

// OrchEngineTelemetry is the live engine's view of the shard sequence
// domain (book_seq vs WAL tail — C++ side exposes this via the IPC seam).
type OrchEngineTelemetry interface {
	BookSeqVsWALTail(ctx context.Context, shardID int) (bookSeq, walTail uint64, err error)
}

// OrchAuditData is the internal (Postgres-backed) audit data provider.
// All members take a shard scope; sinceJournalSeq=0 means full scan.
type OrchAuditData interface {
	OrchDigestDataSource // GLSums / BalanceDeltas / BookSeqWatermark / JournalWatermark

	// NostroCoverage returns, per currency, the internal requirement
	// (settled customer balances + house equity + insurance fund) vs the
	// nostro cash recorded on the books.
	NostroCoverage(ctx context.Context, shardID int) ([]OrchNostroCoverage, error)
	// NegativeBalances lists customer balances with total < 0.
	NegativeBalances(ctx context.Context, shardID int) ([]OrchNegativeBalance, error)
	// ExecutionChain lists orphan trade ids (no valid order) and whether
	// execution ids are strictly increasing.
	ExecutionChain(ctx context.Context, shardID int) (orphans []uint64, monotonicOK bool, err error)
	// MarginViolations lists margin accounts breaching structural
	// invariants (negative equity/used/available).
	MarginViolations(ctx context.Context, shardID int) ([]OrchMarginViolation, error)
	// OmnibusMovements is the internal omnibus movement ledger used to
	// reconcile the bank feed (nostro_movements, migration 112).
	OmnibusMovements(ctx context.Context, shardID int) ([]OrchBankMovement, error)
}

// OrchNostroCoverage is one currency's internal-vs-nostro comparison.
type OrchNostroCoverage struct {
	Currency   string
	Required   decimal.Decimal // settled customer + house equity + insurance
	NostroCash decimal.Decimal
}

// OrchNegativeBalance is one sub-zero customer balance.
type OrchNegativeBalance struct {
	AccountID uint64
	Currency  string
	Total     decimal.Decimal
}

// OrchMarginViolation is one margin account breaking a structural rule.
type OrchMarginViolation struct {
	AccountID uint64
	Field     string // equity | used_margin | available_margin
	Value     decimal.Decimal
}

// OrchBankMovement is a normalized MT942/camt.053 (or internal omnibus)
// movement for multiset reconciliation.
type OrchBankMovement struct {
	Ref       string
	Currency  string
	Amount    decimal.Decimal
	Direction string // DEBIT | CREDIT
}

// OrchCLSLeg is one CLS PvP settlement leg awaiting finality check.
type OrchCLSLeg struct {
	SettlementID string
	Currency     string
	Status       string // CONFIRMED | SETTLED | PENDING | FAILED
}

// OrchBankFeed is the external MT942/camt.053 intraday statement feed.
// Reachability failures MUST wrap OrchErrFeedUnreachable.
type OrchBankFeed interface {
	IntradayMovements(ctx context.Context, shardID int) ([]OrchBankMovement, error)
}

// OrchCLSFeed is the external CLS settlement-finality feed.
// Reachability failures MUST wrap OrchErrFeedUnreachable.
type OrchCLSFeed interface {
	SettlementFinality(ctx context.Context, shardID int) ([]OrchCLSLeg, error)
}

// OrchSuspenseMarker flags internal nostro/omnibus lines as suspense when
// a feed deferral reopens against the internal ledger (Task 4.3.12).
type OrchSuspenseMarker interface {
	FlagSuspense(ctx context.Context, shardID int, reason string) error
}

// OrchReconcileHook is the post-open auto-reconcile seam (Task 24.3.2
// aging workflow, T+1 investigate).
type OrchReconcileHook interface {
	SchedulePostOpenReconcile(ctx context.Context, shardID int, stage OrchStageID) error
}

// OrchReportSink persists recovery_reports rows (migration 065 contract —
// owned by Task 4.3.9; the struct mirrors its column contract).
type OrchReportSink interface {
	Record(ctx context.Context, r OrchRecoveryReport) error
}

// OrchRecoveryReport mirrors recovery_reports (migration 065):
// (id, shard_id, book_seq, wal_tail, last_valid_seq, snapshot_seq,
// first_divergent_seq, stage, outcome, detail JSONB, created_at).
type OrchRecoveryReport struct {
	ShardID           int
	BookSeq           int64
	WalTail           int64
	LastValidSeq      int64
	SnapshotSeq       int64
	FirstDivergentSeq int64
	Stage             string // e.g. "nostro_recon", "audit", "dr_failover"
	Outcome           string // PASS | FAIL | DEFERRED | HALT_LEGAL_FREEZE | PROMOTED | ...
	Detail            map[string]any
}

// OrchAlerter is the P1 paging seam (stage overrun, freeze, unsafe DR).
type OrchAlerter interface {
	RaiseP1(ctx context.Context, summary string, detail map[string]any)
}

// OrchLogAlerter is the default alerter: structured slog P1 records.
type OrchLogAlerter struct{ Log *slog.Logger }

func (a OrchLogAlerter) RaiseP1(_ context.Context, summary string, detail map[string]any) {
	l := a.Log
	if l == nil {
		l = slog.Default()
	}
	args := make([]any, 0, len(detail)*2+2)
	args = append(args, "severity", "P1")
	for k, v := range detail {
		args = append(args, k, v)
	}
	l.Error("P1: "+summary, args...)
}

// OrchFixBroadcaster — FIX session resync seams (35=h / 35=2 / 35=4).
type OrchFixBroadcaster interface {
	// BroadcastTradingSessionStatus sends 35=h with the mapped 340 status.
	BroadcastTradingSessionStatus(ctx context.Context, shardID int, status OrchSessionStatus) error
	// ResolveGaps drives ResendRequest (35=2) + SequenceReset-GapFill
	// (35=4) for sessions whose ingress/egress seqs gapped across the halt.
	ResolveGaps(ctx context.Context, shardID int) error
}

// OrchWSResumeSeam — WebSocket {"action":"resume"} ring-buffer replay.
type OrchWSResumeSeam interface {
	ResumeClients(ctx context.Context, shardID int) error
}

// ---------------------------------------------------------------------------
// Audit stages
// ---------------------------------------------------------------------------

// OrchStageID names an audit stage (also the recovery_reports.stage value).
type OrchStageID string

const (
	OrchStageLedgerZeroSum OrchStageID = "ledger_zero_sum"        // spec stage 1 — hard-fail, never deferrable
	OrchStageNonNegative   OrchStageID = "balance_non_negativity" // spec stage 2
	OrchStageBookWALSeq    OrchStageID = "book_wal_seq"           // spec stage 3
	OrchStageMonotonic     OrchStageID = "monotonic_sequencing"   // spec stage 4
	OrchStageNostroRecon   OrchStageID = "nostro_recon"           // spec stage 5 — deferrable
	OrchStageCLSFinality   OrchStageID = "cls_finality"           // spec stage 6 — deferrable
	OrchStageMargin        OrchStageID = "margin_consistency"     // Task 4.3.11 budget row
)

// orchDeferrable reports whether a stage may defer on feed outage
// (Task 4.3.12: only stages 5–6; stage 1 is explicitly exempt and every
// other internal stage fails rather than defers).
func orchDeferrable(id OrchStageID) bool {
	return id == OrchStageNostroRecon || id == OrchStageCLSFinality
}

// OrchStageVerdict is one stage's outcome.
type OrchStageVerdict string

const (
	OrchVerdictPass     OrchStageVerdict = "PASS"
	OrchVerdictFail     OrchStageVerdict = "FAIL"
	OrchVerdictDeferred OrchStageVerdict = "DEFERRED"
)

// OrchStageResult is one stage's audited record.
type OrchStageResult struct {
	Stage   OrchStageID
	Verdict OrchStageVerdict
	Elapsed time.Duration
	Budget  time.Duration
	Overrun bool // exceeded Budget → fail closed + P1
	Detail  map[string]any
	Err     error
}

// OrchShardAudit is the per-shard audit outcome (4.3.12 verdict input).
type OrchShardAudit struct {
	ShardID        int
	Stages         []OrchStageResult
	Fail           bool // any FAIL/overrun
	HardFail       bool // zero-sum failure — exempt from every fallback
	FallbackUsed   bool // at least one stage deferred
	DigestFallback bool // digest mismatch → shard-local full scan
	Elapsed        time.Duration
}

// OrchAuditConfig bundles the engine's dependencies + clocks.
type OrchAuditConfig struct {
	Budgets      OrchStageBudgets
	FeedDeadline time.Duration // ≤0 → OrchFeedDeadline
	FeedPoll     time.Duration // ≤0 → OrchFeedPoll
	AuditRTO     time.Duration // ≤0 → OrchAuditRTO

	Data     OrchAuditData       // internal checks (nil → stage fails closed)
	Engine   OrchEngineTelemetry // book_seq == wal_tail (nil → stage fails)
	Bank     OrchBankFeed        // MT942/camt.053 (nil → deferrable stage fails)
	CLS      OrchCLSFeed         // CLS finality (nil → same)
	Digests  OrchDigestStore     // optional digest fast path
	Reports  OrchReportSink      // optional; nil drops rows (with slog warn)
	Alerts   OrchAlerter         // optional; nil → OrchLogAlerter default
	Suspense OrchSuspenseMarker  // nostro suspense flagging (nil → skip)
	Recon    OrchReconcileHook   // post-open auto-reconcile (nil → skip)

	// Injectable clock/timers for tests.
	Now   func() time.Time
	After func(d time.Duration) <-chan time.Time
}

func (c *OrchAuditConfig) defaults() {
	if c.Budgets == (OrchStageBudgets{}) {
		c.Budgets = OrchDefaultStageBudgets()
	}
	if c.FeedDeadline <= 0 {
		c.FeedDeadline = OrchFeedDeadline
	}
	if c.FeedPoll <= 0 {
		c.FeedPoll = OrchFeedPoll
	}
	if c.AuditRTO <= 0 {
		c.AuditRTO = OrchAuditRTO
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.After == nil {
		c.After = time.After
	}
	if c.Alerts == nil {
		c.Alerts = OrchLogAlerter{}
	}
}

// OrchAuditEngine runs the 6-stage (+margin) pre-open integrity audit.
type OrchAuditEngine struct {
	cfg OrchAuditConfig
}

// NewOrchAuditEngine builds the engine; missing tunables get spec defaults.
func NewOrchAuditEngine(cfg OrchAuditConfig) *OrchAuditEngine {
	cfg.defaults()
	return &OrchAuditEngine{cfg: cfg}
}

// orchCheck is a stage's raw outcome: nil error = pass.
type orchCheck struct {
	detail map[string]any
	err    error
}

func orchPass(detail map[string]any) orchCheck { return orchCheck{detail: detail} }
func orchFail(err error, detail map[string]any) orchCheck {
	return orchCheck{detail: detail, err: err}
}

// AuditShard runs all stages for one shard, concurrently, each under its
// budget, inside the audit RTO. Never returns nil — a fail-closed engine
// always produces a verdict record.
func (e *OrchAuditEngine) AuditShard(ctx context.Context, shardID int) *OrchShardAudit {
	start := e.cfg.Now()
	rtoCtx, cancel := context.WithTimeout(ctx, e.cfg.AuditRTO)
	defer cancel()

	// Stage order is fixed; each result carries its own OrchStageID, so
	// position is only for stable presentation.
	runs := []func(context.Context) OrchStageResult{
		func(c context.Context) OrchStageResult {
			return e.runStage(c, shardID, OrchStageLedgerZeroSum, e.cfg.Budgets.Ledger, e.checkLedgerZeroSum)
		},
		func(c context.Context) OrchStageResult {
			return e.runStage(c, shardID, OrchStageNonNegative, e.cfg.Budgets.Ledger, e.checkNonNegative)
		},
		func(c context.Context) OrchStageResult {
			return e.runStage(c, shardID, OrchStageBookWALSeq, e.cfg.Budgets.BookWAL, e.checkBookWAL)
		},
		func(c context.Context) OrchStageResult {
			return e.runStage(c, shardID, OrchStageMonotonic, e.cfg.Budgets.Monotonic, e.checkMonotonic)
		},
		func(c context.Context) OrchStageResult {
			return e.runStage(c, shardID, OrchStageMargin, e.cfg.Budgets.Margin, e.checkMargin)
		},
		func(c context.Context) OrchStageResult {
			return e.runNostroStage(c, shardID)
		},
		func(c context.Context) OrchStageResult {
			return e.runCLSStage(c, shardID)
		},
	}

	out := &OrchShardAudit{ShardID: shardID, Stages: make([]OrchStageResult, len(runs))}
	var wg sync.WaitGroup
	for i, r := range runs {
		wg.Add(1)
		go func(i int, fn func(context.Context) OrchStageResult) {
			defer wg.Done()
			out.Stages[i] = fn(rtoCtx)
		}(i, r)
	}
	wg.Wait()
	out.Elapsed = e.cfg.Now().Sub(start)

	for _, s := range out.Stages {
		switch s.Verdict {
		case OrchVerdictFail:
			out.Fail = true
			if s.Stage == OrchStageLedgerZeroSum {
				out.HardFail = true
			}
		case OrchVerdictDeferred:
			out.FallbackUsed = true
		}
		if s.Stage == OrchStageLedgerZeroSum {
			if v, ok := s.Detail["digest_fallback"].(bool); ok && v {
				out.DigestFallback = true
			}
		}
	}
	return out
}

// runStage executes one internal stage under its budget; an overrun is a
// fail-closed FAIL + P1 with the stage timer attached.
func (e *OrchAuditEngine) runStage(ctx context.Context, shardID int, id OrchStageID,
	budget time.Duration, fn func(ctx context.Context, shardID int) orchCheck) OrchStageResult {
	start := e.cfg.Now()
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	resCh := make(chan orchCheck, 1)
	go func() { resCh <- fn(sctx, shardID) }()

	var res orchCheck
	overrun := false
	select {
	case res = <-resCh:
	case <-sctx.Done():
		overrun = true
		res = orchFail(fmt.Errorf("stage %s exceeded budget %s", id, budget), nil)
	}
	r := OrchStageResult{
		Stage:   id,
		Elapsed: e.cfg.Now().Sub(start),
		Budget:  budget,
		Overrun: overrun,
		Detail:  res.detail,
		Err:     res.err,
	}
	if res.err != nil {
		r.Verdict = OrchVerdictFail
		e.recordStage(shardID, r)
		if overrun {
			e.raiseP1(shardID, string(OrchCodeStageOverrun)+": "+string(id), map[string]any{
				"stage": string(id), "budget": budget.String(), "elapsed": r.Elapsed.String(),
			})
		}
	} else {
		r.Verdict = OrchVerdictPass
	}
	return r
}

// orchAwaitFeed polls fetch until it yields data, returns a non-
// reachability error, or the 120s feed deadline elapses (→ deferred).
func (e *OrchAuditEngine) orchAwaitFeed(ctx context.Context, fetch func(context.Context) error) (deferred bool, err error) {
	deadline := e.cfg.Now().Add(e.cfg.FeedDeadline)
	for {
		err := fetch(ctx)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, OrchErrFeedUnreachable) {
			return false, err
		}
		remaining := deadline.Sub(e.cfg.Now())
		if remaining <= 0 {
			return true, nil // unreachable past deadline → deferral
		}
		wait := e.cfg.FeedPoll
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-e.cfg.After(wait):
		}
	}
}

// runFeedStage is the shared skeleton for the external stages: wait for
// the feed (≤ FeedDeadline → DEFERRED), then reconcile under the stage
// budget. Deferral bookkeeping (recovery_reports + suspense + post-open
// reconcile hook) happens here per Task 4.3.12.
func (e *OrchAuditEngine) runFeedStage(ctx context.Context, shardID int, id OrchStageID,
	budget time.Duration, fetch func(context.Context) error,
	reconcile func(ctx context.Context) orchCheck) OrchStageResult {
	start := e.cfg.Now()
	if !orchDeferrable(id) {
		// Structural guard: only feed stages may ever defer. A caller
		// routing an internal stage through this path fails closed.
		return OrchStageResult{Stage: id, Verdict: OrchVerdictFail,
			Elapsed: e.cfg.Now().Sub(start), Budget: budget,
			Err: fmt.Errorf("stage %s is not deferrable", id)}
	}
	deferred, err := e.orchAwaitFeed(ctx, fetch)
	if err != nil {
		r := OrchStageResult{Stage: id, Verdict: OrchVerdictFail,
			Elapsed: e.cfg.Now().Sub(start), Budget: budget, Err: err}
		e.recordStage(shardID, r)
		return r
	}
	if deferred {
		r := OrchStageResult{
			Stage: id, Verdict: OrchVerdictDeferred,
			Elapsed: e.cfg.Now().Sub(start), Budget: budget,
			Err: fmt.Errorf("%s unreachable past %s deadline", id, e.cfg.FeedDeadline),
			Detail: map[string]any{
				"reason":         OrchCodeFeedUnreachable,
				"feed_deadline":  e.cfg.FeedDeadline.String(),
				"suspense_flags": id == OrchStageNostroRecon,
			},
		}
		e.recordStage(shardID, r)
		// Task 4.3.12: reopen against internal ledger with nostro lines
		// suspense-flagged + post-open auto-reconcile (T+1 aging seam).
		if id == OrchStageNostroRecon && e.cfg.Suspense != nil {
			if err := e.cfg.Suspense.FlagSuspense(ctx, shardID, "nostro feed deferred at reopen"); err != nil {
				slog.Error("recovery: suspense flag failed", "shard", shardID, "err", err)
			}
		}
		if e.cfg.Recon != nil {
			if err := e.cfg.Recon.SchedulePostOpenReconcile(ctx, shardID, id); err != nil {
				slog.Error("recovery: post-open reconcile schedule failed", "shard", shardID, "stage", id, "err", err)
			}
		}
		return r
	}
	r := e.runStage(ctx, shardID, id, budget,
		func(c context.Context, _ int) orchCheck { return reconcile(c) })
	r.Elapsed = e.cfg.Now().Sub(start) // include the feed wait
	return r
}

// --- stage bodies -----------------------------------------------------------

// checkLedgerZeroSum — spec stage 1:
//
//	SUM(debits) == SUM(credits) per currency across GL + customer lines;
//	settled + house equity + insurance == nostro cash.
//
// Digest fast path: a verified digest chain bounds the scan to the
// post-checkpoint window; a mismatch forces the shard-local full scan.
func (e *OrchAuditEngine) checkLedgerZeroSum(ctx context.Context, shardID int) orchCheck {
	if e.cfg.Data == nil {
		return orchFail(errors.New("no audit data provider wired"), nil)
	}
	detail := map[string]any{}
	since := uint64(0)
	if e.cfg.Digests != nil {
		v, err := OrchVerifyShardDigest(ctx, shardID, e.cfg.Digests, e.cfg.Data)
		switch {
		case err != nil:
			// Digest infra error → shard-local fallback (fail closed: the
			// full scan, not the digest, is authoritative on error).
			detail["digest_fallback"] = true
			detail["digest_error"] = err.Error()
		case !v.OK:
			detail["digest_fallback"] = true
			detail["digest_divergent_seq"] = v.FirstDivergent
			detail["digest_reason"] = v.Reason
		default:
			since = v.JournalSeq
			detail["digest_checkpoint_seq"] = v.CheckpointSeq
			detail["digest_fallback"] = false
		}
	}
	sums, err := e.cfg.Data.GLSums(ctx, shardID, since)
	if err != nil {
		return orchFail(fmt.Errorf("gl sums: %w", err), detail)
	}
	var imbalanced []string
	for _, s := range sums {
		if !s.Debits.Equal(s.Credits) {
			imbalanced = append(imbalanced,
				fmt.Sprintf("%s d=%s c=%s", s.Currency, s.Debits, s.Credits))
		}
	}
	if len(imbalanced) > 0 {
		sort.Strings(imbalanced)
		detail["imbalanced_currencies"] = imbalanced
		return orchFail(excerrors.New(OrchCodeLedgerImbalanceAbort,
			"SUM(debits) != SUM(credits): "+strings.Join(imbalanced, "; ")), detail)
	}
	coverage, err := e.cfg.Data.NostroCoverage(ctx, shardID)
	if err != nil {
		return orchFail(fmt.Errorf("nostro coverage: %w", err), detail)
	}
	var uncovered []string
	for _, c := range coverage {
		if !c.Required.Equal(c.NostroCash) {
			uncovered = append(uncovered,
				fmt.Sprintf("%s required=%s nostro=%s", c.Currency, c.Required, c.NostroCash))
		}
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		detail["uncovered_currencies"] = uncovered
		return orchFail(excerrors.New(OrchCodeLedgerImbalanceAbort,
			"settled+house+insurance != nostro cash: "+strings.Join(uncovered, "; ")), detail)
	}
	return orchPass(detail)
}

func (e *OrchAuditEngine) checkNonNegative(ctx context.Context, shardID int) orchCheck {
	if e.cfg.Data == nil {
		return orchFail(errors.New("no audit data provider wired"), nil)
	}
	negs, err := e.cfg.Data.NegativeBalances(ctx, shardID)
	if err != nil {
		return orchFail(fmt.Errorf("negative balances: %w", err), nil)
	}
	if len(negs) > 0 {
		sample := make([]string, 0, len(negs))
		for _, n := range negs {
			sample = append(sample, fmt.Sprintf("acct=%d %s=%s", n.AccountID, n.Currency, n.Total))
		}
		sort.Strings(sample)
		return orchFail(fmt.Errorf("%d negative customer balances", len(negs)),
			map[string]any{"violations": sample})
	}
	return orchPass(nil)
}

func (e *OrchAuditEngine) checkBookWAL(ctx context.Context, shardID int) orchCheck {
	if e.cfg.Engine == nil {
		return orchFail(errors.New("no engine telemetry wired"), nil)
	}
	book, wal, err := e.cfg.Engine.BookSeqVsWALTail(ctx, shardID)
	if err != nil {
		return orchFail(fmt.Errorf("book/wal seq: %w", err), nil)
	}
	if book != wal {
		return orchFail(fmt.Errorf("book_seq %d != wal_tail %d", book, wal),
			map[string]any{"book_seq": book, "wal_tail": wal})
	}
	return orchPass(map[string]any{"book_seq": book, "wal_tail": wal})
}

func (e *OrchAuditEngine) checkMonotonic(ctx context.Context, shardID int) orchCheck {
	if e.cfg.Data == nil {
		return orchFail(errors.New("no audit data provider wired"), nil)
	}
	orphans, ok, err := e.cfg.Data.ExecutionChain(ctx, shardID)
	if err != nil {
		return orchFail(fmt.Errorf("execution chain: %w", err), nil)
	}
	if len(orphans) > 0 || !ok {
		return orchFail(fmt.Errorf("execution chain broken: %d orphans, monotonic=%v",
			len(orphans), ok),
			map[string]any{"orphan_trades": orphans, "monotonic": ok})
	}
	return orchPass(nil)
}

func (e *OrchAuditEngine) checkMargin(ctx context.Context, shardID int) orchCheck {
	if e.cfg.Data == nil {
		return orchFail(errors.New("no audit data provider wired"), nil)
	}
	viol, err := e.cfg.Data.MarginViolations(ctx, shardID)
	if err != nil {
		return orchFail(fmt.Errorf("margin consistency: %w", err), nil)
	}
	if len(viol) > 0 {
		sample := make([]string, 0, len(viol))
		for _, v := range viol {
			sample = append(sample, fmt.Sprintf("acct=%d %s=%s", v.AccountID, v.Field, v.Value))
		}
		sort.Strings(sample)
		return orchFail(fmt.Errorf("%d margin violations", len(viol)),
			map[string]any{"violations": sample})
	}
	return orchPass(nil)
}

// runNostroStage — spec stage 5: MT942/camt.053 intraday movements vs the
// internal omnibus ledger (multiset reconcile). Feed unreachable past the
// deadline → DEFERRED (suspense-flagged reopen).
func (e *OrchAuditEngine) runNostroStage(ctx context.Context, shardID int) OrchStageResult {
	if e.cfg.Bank == nil {
		r := OrchStageResult{Stage: OrchStageNostroRecon, Verdict: OrchVerdictFail,
			Budget: e.cfg.Budgets.Nostro,
			Err:    errors.New("no bank feed wired")}
		e.recordStage(shardID, r)
		return r
	}
	var external []OrchBankMovement
	return e.runFeedStage(ctx, shardID, OrchStageNostroRecon, e.cfg.Budgets.Nostro,
		func(ctx context.Context) error {
			mv, err := e.cfg.Bank.IntradayMovements(ctx, shardID)
			if err == nil {
				external = mv
			}
			return err
		},
		func(ctx context.Context) orchCheck {
			internal, err := e.cfg.Data.OmnibusMovements(ctx, shardID)
			if err != nil {
				return orchFail(fmt.Errorf("omnibus movements: %w", err), nil)
			}
			missIn, missEx := orchMovementDiff(internal, external)
			if len(missIn) > 0 || len(missEx) > 0 {
				return orchFail(fmt.Errorf("nostro reconcile mismatch: %d internal-only, %d external-only",
					len(missIn), len(missEx)),
					map[string]any{
						"internal_only": orchMovementStrings(missIn),
						"external_only": orchMovementStrings(missEx),
					})
			}
			return orchPass(map[string]any{"movements_reconciled": len(internal)})
		})
}

// runCLSStage — spec stage 6: CLS PvP settlement finality before open.
func (e *OrchAuditEngine) runCLSStage(ctx context.Context, shardID int) OrchStageResult {
	if e.cfg.CLS == nil {
		r := OrchStageResult{Stage: OrchStageCLSFinality, Verdict: OrchVerdictFail,
			Budget: e.cfg.Budgets.CLS,
			Err:    errors.New("no cls feed wired")}
		e.recordStage(shardID, r)
		return r
	}
	var legs []OrchCLSLeg
	return e.runFeedStage(ctx, shardID, OrchStageCLSFinality, e.cfg.Budgets.CLS,
		func(ctx context.Context) error {
			l, err := e.cfg.CLS.SettlementFinality(ctx, shardID)
			if err == nil {
				legs = l
			}
			return err
		},
		func(ctx context.Context) orchCheck {
			var unfinal []string
			for _, l := range legs {
				if l.Status != "CONFIRMED" && l.Status != "SETTLED" {
					unfinal = append(unfinal, l.SettlementID+":"+l.Status)
				}
			}
			if len(unfinal) > 0 {
				sort.Strings(unfinal)
				return orchFail(fmt.Errorf("%d CLS legs not final", len(unfinal)),
					map[string]any{"unfinal": unfinal})
			}
			return orchPass(map[string]any{"cls_legs_final": len(legs)})
		})
}

// orchMovementDiff multiset-compares internal vs external movements on
// (ref|currency|amount|direction); returns movements present only on each
// side.
func orchMovementDiff(a, b []OrchBankMovement) (onlyA, onlyB []OrchBankMovement) {
	key := func(m OrchBankMovement) string {
		return m.Ref + "|" + m.Currency + "|" + orchDec(m.Amount) + "|" + m.Direction
	}
	counts := map[string]int{}
	for _, m := range a {
		counts[key(m)]++
	}
	for _, m := range b {
		counts[key(m)]--
	}
	for _, m := range a {
		if counts[key(m)] > 0 {
			onlyA = append(onlyA, m)
			counts[key(m)]--
		}
	}
	for _, m := range b {
		if counts[key(m)] < 0 {
			onlyB = append(onlyB, m)
			counts[key(m)]++
		}
	}
	return onlyA, onlyB
}

func orchMovementStrings(ms []OrchBankMovement) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Ref+" "+m.Direction+" "+m.Currency+" "+m.Amount.String())
	}
	sort.Strings(out)
	return out
}

func (e *OrchAuditEngine) recordStage(shardID int, r OrchStageResult) {
	if e.cfg.Reports == nil {
		return
	}
	outcome := string(r.Verdict)
	if r.Overrun {
		outcome = OrchCodeStageOverrun
	}
	detail := map[string]any{
		"budget_ms":  r.Budget.Milliseconds(),
		"elapsed_ms": r.Elapsed.Milliseconds(),
	}
	for k, v := range r.Detail {
		detail[k] = v
	}
	if r.Err != nil {
		detail["error"] = r.Err.Error()
	}
	err := e.cfg.Reports.Record(context.Background(), OrchRecoveryReport{
		ShardID: shardID,
		Stage:   string(r.Stage),
		Outcome: outcome,
		Detail:  detail,
	})
	if err != nil {
		slog.Error("recovery: report sink failed", "shard", shardID, "stage", r.Stage, "err", err)
	}
}

func (e *OrchAuditEngine) raiseP1(shardID int, summary string, detail map[string]any) {
	d := map[string]any{"shard_id": shardID}
	for k, v := range detail {
		d[k] = v
	}
	e.cfg.Alerts.RaiseP1(context.Background(), summary, d)
}

// ---------------------------------------------------------------------------
// RecoveryOrchestrator
// ---------------------------------------------------------------------------

// OrchConfig is the orchestrator-level configuration.
type OrchConfig struct {
	Shards          []int // managed shard ids
	PromotionRTO    time.Duration
	CancelOnlyGrace time.Duration
	AuctionWindow   time.Duration
	LeaderTTL       time.Duration // ≤0 → 2s epoch-lease TTL
	RPOMax          time.Duration // ≤0 → 15s
	DRRTO           time.Duration // ≤0 → 5min
}

func (c *OrchConfig) defaults() {
	if c.PromotionRTO <= 0 {
		c.PromotionRTO = OrchPromotionRTO
	}
	if c.CancelOnlyGrace <= 0 {
		c.CancelOnlyGrace = OrchCancelOnlyGrace
	}
	if c.AuctionWindow <= 0 {
		c.AuctionWindow = OrchAuctionWindow
	}
	if c.LeaderTTL <= 0 {
		c.LeaderTTL = 2 * time.Second // redis.LeaderLeaseTTL (§18.6.2)
	}
	if c.RPOMax <= 0 {
		c.RPOMax = OrchDRRPO
	}
	if c.DRRTO <= 0 {
		c.DRRTO = OrchDRRTO
	}
}

// OrchDeps are the orchestrator's pluggable collaborators.
type OrchDeps struct {
	Leases   OrchLeaseBackend
	Standbys OrchStandbyRegistry
	Liveness OrchLivenessProbe
	Flusher  OrchWalFlusher
	DR       OrchDRDriver
	Audit    *OrchAuditEngine
	Fix      OrchFixBroadcaster
	WS       OrchWSResumeSeam
	Reports  OrchReportSink
	Alerts   OrchAlerter

	// Injectable timers for tests.
	Now   func() time.Time
	After func(d time.Duration) <-chan time.Time
	// OnStateChange fires on every shard state transition (metrics/UI).
	OnStateChange func(shardID int, from, to OrchShardState)
}

// OrchEpochCheck is the result of lease-epoch verification
// (epoch_local == epoch_current).
type OrchEpochCheck struct {
	LeasePresent bool
	Token        string
	Epoch        uint64
	LocalEpoch   uint64
	Matches      bool
}

// OrchPromotion records one fencing+promotion cycle.
type OrchPromotion struct {
	ShardID      int
	Promoted     bool
	Token        string
	Epoch        uint64
	RevokedStale bool
	Elapsed      time.Duration
	WithinRTO    bool
}

// OrchDRStepResult is one DR step's record.
type OrchDRStepResult struct {
	Name    string
	Elapsed time.Duration
	Detail  string
	Err     error
}

// OrchDRReport is the failover run record.
type OrchDRReport struct {
	Steps     []OrchDRStepResult
	Elapsed   time.Duration
	WithinRTO bool
}

// OrchOrderAdmission is the reopen-gate view of an inbound order.
type OrchOrderAdmission struct {
	IsCancel     bool
	BasketShards []int // >1 element ⇒ cross-shard basket order
}

type orchShardCtl struct {
	state     OrchShardState
	epoch     uint64 // last-known epoch_current for fencing
	heldToken string
	heldEpoch uint64
	audit     *OrchShardAudit
}

// RecoveryOrchestrator is the §18.6 per-shard reopen + fencing engine.
type RecoveryOrchestrator struct {
	cfg  OrchConfig
	deps OrchDeps

	mu     sync.Mutex
	shards map[int]*orchShardCtl
}

// NewRecoveryOrchestrator wires the orchestrator. Audit is REQUIRED —
// reopening without the integrity engine would violate fail-closed.
func NewRecoveryOrchestrator(cfg OrchConfig, deps OrchDeps) (*RecoveryOrchestrator, error) {
	if deps.Audit == nil {
		return nil, errors.New("recovery orchestrator: audit engine is required")
	}
	cfg.defaults()
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.After == nil {
		deps.After = time.After
	}
	if deps.Alerts == nil {
		deps.Alerts = OrchLogAlerter{}
	}
	o := &RecoveryOrchestrator{cfg: cfg, deps: deps, shards: map[int]*orchShardCtl{}}
	for _, s := range cfg.Shards {
		o.shards[s] = &orchShardCtl{state: OrchStateDown}
	}
	return o, nil
}

func (o *RecoveryOrchestrator) ctl(shardID int) (*orchShardCtl, bool) {
	c, ok := o.shards[shardID]
	return c, ok
}

// State returns the shard's current reopen-ladder state.
func (o *RecoveryOrchestrator) State(shardID int) OrchShardState {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c, ok := o.shards[shardID]; ok {
		return c.state
	}
	return OrchStateDown
}

// Audit returns the shard's last audit record (nil if none run).
func (o *RecoveryOrchestrator) Audit(shardID int) *OrchShardAudit {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c, ok := o.shards[shardID]; ok {
		return c.audit
	}
	return nil
}

func (o *RecoveryOrchestrator) setState(shardID int, to OrchShardState) {
	o.mu.Lock()
	c, ok := o.shards[shardID]
	if !ok {
		c = &orchShardCtl{}
		o.shards[shardID] = c
	}
	from := c.state
	c.state = to
	hook := o.deps.OnStateChange
	o.mu.Unlock()
	if hook != nil && from != to {
		hook(shardID, from, to)
	}
}

// ---------------------------------------------------------------------------
// Fencing & promotion (Task 4.3.10 step 1)
// ---------------------------------------------------------------------------

// VerifyLeaderEpoch implements epoch_local == epoch_current: read the
// lease, parse "{token}:{epoch}", compare to localEpoch. The caller
// (standby node) passes its local epoch; the orchestrator's fencing view
// is returned for the promotion decision.
func (o *RecoveryOrchestrator) VerifyLeaderEpoch(ctx context.Context, shardID int, localEpoch uint64) (OrchEpochCheck, error) {
	if o.deps.Leases == nil {
		return OrchEpochCheck{}, errors.New("recovery: no lease backend wired")
	}
	val, ok, err := o.deps.Leases.LeaderValue(ctx, shardID)
	if err != nil {
		return OrchEpochCheck{}, fmt.Errorf("recovery: leader value shard %d: %w", shardID, err)
	}
	chk := OrchEpochCheck{LeasePresent: ok, LocalEpoch: localEpoch}
	if !ok {
		return chk, nil // no lease → promotion path, epoch not violated
	}
	tok, ep, err := OrchParseLeaderValue(val)
	if err != nil {
		return chk, err
	}
	chk.Token, chk.Epoch = tok, ep
	chk.Matches = ep == localEpoch
	return chk, nil
}

// FenceAndPromote revokes a dead leader's lease and promotes the warm
// standby, bounded by PromotionRTO (3s). A live leader is left alone
// (Promoted=false). Fail-closed: any backend error aborts promotion.
func (o *RecoveryOrchestrator) FenceAndPromote(ctx context.Context, shardID int) (*OrchPromotion, error) {
	if o.deps.Leases == nil || o.deps.Standbys == nil {
		return nil, errors.New("recovery: fencing requires leases + standby registry")
	}
	start := o.deps.Now()
	ctx, cancel := context.WithTimeout(ctx, o.cfg.PromotionRTO)
	defer cancel()

	p := &OrchPromotion{ShardID: shardID}

	val, ok, err := o.deps.Leases.LeaderValue(ctx, shardID)
	if err != nil {
		return nil, fmt.Errorf("recovery: fence read shard %d: %w", shardID, err)
	}
	var curEpoch uint64
	if ok {
		tok, ep, err := OrchParseLeaderValue(val)
		if err != nil {
			return nil, fmt.Errorf("recovery: fence parse shard %d: %w", shardID, err)
		}
		curEpoch = ep
		// epoch_local adopts epoch_current when the lease shows a newer
		// epoch — the fencing-token invariant only ever moves forward.
		o.mu.Lock()
		if c, okc := o.shards[shardID]; okc && ep > c.epoch {
			c.epoch = ep
		}
		o.mu.Unlock()

		alive := false
		if o.deps.Liveness != nil {
			alive, err = o.deps.Liveness.LeaderAlive(ctx, shardID, tok)
			if err != nil {
				return nil, fmt.Errorf("recovery: liveness probe shard %d: %w", shardID, err)
			}
		}
		if alive {
			p.Elapsed = o.deps.Now().Sub(start)
			p.WithinRTO = p.Elapsed <= o.cfg.PromotionRTO
			return p, nil // leader is alive — nothing to fence
		}
		revoked, err := o.deps.Leases.RevokeLeader(ctx, shardID, val)
		if err != nil {
			return nil, fmt.Errorf("recovery: revoke stale leader shard %d: %w", shardID, err)
		}
		p.RevokedStale = revoked
	}

	token, err := o.deps.Standbys.NextStandby(ctx, shardID)
	if err != nil {
		return nil, fmt.Errorf("recovery: standby lookup shard %d: %w", shardID, err)
	}
	newEpoch := curEpoch + 1
	acq, err := o.deps.Leases.AcquireLeader(ctx, shardID, token, newEpoch, o.cfg.LeaderTTL)
	if err != nil {
		return nil, fmt.Errorf("recovery: acquire leader shard %d: %w", shardID, err)
	}
	if !acq {
		return nil, fmt.Errorf("recovery: shard %d lease contested during promotion", shardID)
	}
	o.mu.Lock()
	c, _ := o.ctl(shardID)
	if c == nil {
		c = &orchShardCtl{}
		o.shards[shardID] = c
	}
	c.epoch = newEpoch
	c.heldToken = token
	c.heldEpoch = newEpoch
	o.mu.Unlock()

	p.Promoted = true
	p.Token = token
	p.Epoch = newEpoch
	p.Elapsed = o.deps.Now().Sub(start)
	p.WithinRTO = p.Elapsed <= o.cfg.PromotionRTO
	if !p.WithinRTO {
		o.deps.Alerts.RaiseP1(context.Background(), "standby promotion exceeded RTO", map[string]any{
			"shard_id": shardID, "elapsed_ms": p.Elapsed.Milliseconds(), "rto_ms": o.cfg.PromotionRTO.Milliseconds(),
		})
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Pre-open audit + reopen ladder
// ---------------------------------------------------------------------------

// OrchShardVerdict is the 4.3.12 reopen decision.
type OrchShardVerdict string

const (
	OrchVerdictReopen OrchShardVerdict = "REOPEN"
	OrchVerdictFreeze OrchShardVerdict = "HALT_LEGAL_FREEZE"
)

// AuditShard runs the integrity engine and returns the verdict WITHOUT
// changing state yet (caller may inspect); ReopenShard drives the ladder.
func (o *RecoveryOrchestrator) AuditShard(ctx context.Context, shardID int) (*OrchShardAudit, OrchShardVerdict) {
	o.setState(shardID, OrchStateAuditing)
	a := o.deps.Audit.AuditShard(ctx, shardID)
	o.mu.Lock()
	if c, ok := o.shards[shardID]; ok {
		c.audit = a
	}
	o.mu.Unlock()
	if a.Fail {
		return a, OrchVerdictFreeze
	}
	return a, OrchVerdictReopen
}

// ReopenShard executes the full verdict+ladder for one shard:
//
//	audit → FREEZE (HALT_LEGAL_FREEZE) | CANCEL_ONLY(60s) → resync seams →
//	CALL_AUCTION(5s) → NORMAL
//
// Deferral-only audits reopen with FallbackUsed recorded.
func (o *RecoveryOrchestrator) ReopenShard(ctx context.Context, shardID int) (OrchShardVerdict, error) {
	audit, verdict := o.AuditShard(ctx, shardID)
	if verdict == OrchVerdictFreeze {
		o.setState(shardID, OrchStateFrozen)
		if o.deps.Reports != nil {
			outcome := "HALT_LEGAL_FREEZE"
			if audit.HardFail {
				outcome = OrchCodeLedgerImbalanceAbort
			}
			_ = o.deps.Reports.Record(context.Background(), OrchRecoveryReport{
				ShardID: shardID, Stage: "audit", Outcome: outcome,
				Detail: map[string]any{"hard_fail": audit.HardFail},
			})
		}
		o.deps.Alerts.RaiseP1(context.Background(), "shard frozen at reopen", map[string]any{
			"shard_id": shardID, "hard_fail": audit.HardFail,
		})
		if o.deps.Fix != nil {
			_ = o.deps.Fix.BroadcastTradingSessionStatus(ctx, shardID, OrchSessionHalt)
		}
		return verdict, nil
	}

	// CANCEL_ONLY grace.
	o.setState(shardID, OrchStateCancelOnly)
	if o.deps.Fix != nil {
		if err := o.deps.Fix.BroadcastTradingSessionStatus(ctx, shardID, OrchSessionPreOpen); err != nil {
			slog.Error("recovery: fix status broadcast failed", "shard", shardID, "err", err)
		}
	}
	if err := o.wait(ctx, o.cfg.CancelOnlyGrace); err != nil {
		return verdict, fmt.Errorf("recovery: cancel-only grace shard %d: %w", shardID, err)
	}

	// Client resynchronization seams: FIX ResendRequest/gap-fill + WS
	// ring-buffer resume.
	if o.deps.Fix != nil {
		if err := o.deps.Fix.ResolveGaps(ctx, shardID); err != nil {
			return verdict, fmt.Errorf("recovery: fix gap resolution shard %d: %w", shardID, err)
		}
	}
	if o.deps.WS != nil {
		if err := o.deps.WS.ResumeClients(ctx, shardID); err != nil {
			return verdict, fmt.Errorf("recovery: ws resume shard %d: %w", shardID, err)
		}
	}

	// 5s call auction → Normal.
	o.setState(shardID, OrchStateAuction)
	if o.deps.Fix != nil {
		_ = o.deps.Fix.BroadcastTradingSessionStatus(ctx, shardID, OrchSessionAuction)
	}
	if err := o.wait(ctx, o.cfg.AuctionWindow); err != nil {
		return verdict, fmt.Errorf("recovery: auction window shard %d: %w", shardID, err)
	}
	o.setState(shardID, OrchStateNormal)
	if o.deps.Fix != nil {
		_ = o.deps.Fix.BroadcastTradingSessionStatus(ctx, shardID, OrchSessionOpen)
	}
	return verdict, nil
}

func (o *RecoveryOrchestrator) wait(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.deps.After(d):
		return nil
	}
}

// OrchReopenResult is the venue-level rollup of ReopenAll.
type OrchReopenResult struct {
	Reopened  []int
	Frozen    []int
	Deferred  []int // reopened with feed fallback
	ShardErrs map[int]error
}

// ReopenAll audits and reopens every managed shard in parallel (4.3.12
// per-shard scoping — one shard's failure never blocks the others).
func (o *RecoveryOrchestrator) ReopenAll(ctx context.Context) *OrchReopenResult {
	res := &OrchReopenResult{ShardErrs: map[int]error{}}
	o.mu.Lock()
	shards := make([]int, 0, len(o.shards))
	for s := range o.shards {
		shards = append(shards, s)
	}
	o.mu.Unlock()
	sort.Ints(shards)

	type outcome struct {
		shard   int
		verdict OrchShardVerdict
		err     error
	}
	ch := make(chan outcome, len(shards))
	for _, s := range shards {
		go func(s int) {
			v, err := o.ReopenShard(ctx, s)
			ch <- outcome{s, v, err}
		}(s)
	}
	for range shards {
		oc := <-ch
		if oc.err != nil {
			res.ShardErrs[oc.shard] = oc.err
		}
		switch oc.verdict {
		case OrchVerdictFreeze:
			res.Frozen = append(res.Frozen, oc.shard)
		case OrchVerdictReopen:
			res.Reopened = append(res.Reopened, oc.shard)
			if a := o.Audit(oc.shard); a != nil && a.FallbackUsed {
				res.Deferred = append(res.Deferred, oc.shard)
			}
		}
	}
	sort.Ints(res.Reopened)
	sort.Ints(res.Frozen)
	sort.Ints(res.Deferred)
	return res
}

// ---------------------------------------------------------------------------
// Reopen admission gate (4.3.10 ladder + 4.3.12 scoping)
// ---------------------------------------------------------------------------

// AdmitOrder is the order-ingress gate during/after reopen. Errors are
// coded: ORDER_REJECTED_CANCEL_ONLY_MODE / SERVICE_DEGRADED.
func (o *RecoveryOrchestrator) AdmitOrder(shardID int, req OrchOrderAdmission) error {
	// Cross-shard basket scoping (4.3.12): any frozen member rejects the
	// whole basket with SERVICE_DEGRADED.
	if len(req.BasketShards) > 1 {
		for _, s := range req.BasketShards {
			if o.State(s) == OrchStateFrozen {
				return excerrors.New(OrchCodeServiceDegraded,
					fmt.Sprintf("basket touches frozen shard %d", s))
			}
		}
	}
	switch st := o.State(shardID); st {
	case OrchStateNormal, OrchStateAuction:
		return nil // auction orders queue into the uncrossing
	case OrchStateCancelOnly:
		if req.IsCancel {
			return nil
		}
		return excerrors.New(OrchCodeCancelOnly,
			"shard in CANCEL_ONLY grace: only cancels accepted")
	case OrchStateFrozen:
		return excerrors.New(OrchCodeServiceDegraded,
			"shard frozen (HALT_LEGAL_FREEZE)")
	default:
		return excerrors.New(OrchCodeServiceDegraded,
			fmt.Sprintf("shard not open (state %s)", st))
	}
}

// ---------------------------------------------------------------------------
// Multi-region DR failover (Task 4.3.10 step 2)
// ---------------------------------------------------------------------------

// RunDRFailover executes the ordered DR steps inside DRRTO:
//  1. PostgreSQL semi-sync verification (RPO gate — lag > RPOMax aborts,
//     fail closed: promoting a stale secondary would silently lose data)
//  2. Redis Sentinel secondary master promotion
//  3. per-shard S3 WAL archive replay catch-up
func (o *RecoveryOrchestrator) RunDRFailover(ctx context.Context) (*OrchDRReport, error) {
	if o.deps.DR == nil {
		return nil, errors.New("recovery: no DR driver wired")
	}
	start := o.deps.Now()
	ctx, cancel := context.WithTimeout(ctx, o.cfg.DRRTO)
	defer cancel()
	rep := &OrchDRReport{}

	step := func(name string, fn func() (string, error)) bool {
		s := o.deps.Now()
		detail, err := fn()
		rep.Steps = append(rep.Steps, OrchDRStepResult{
			Name: name, Elapsed: o.deps.Now().Sub(s), Detail: detail, Err: err,
		})
		return err == nil
	}

	ok := step("pg_semisync_verify", func() (string, error) {
		lag, err := o.deps.DR.VerifyPGSemiSync(ctx)
		if err != nil {
			return "", err
		}
		if lag > o.cfg.RPOMax {
			return "", fmt.Errorf("replication lag %s exceeds RPO %s — refusing unsafe promotion", lag, o.cfg.RPOMax)
		}
		return fmt.Sprintf("lag=%s", lag), nil
	})
	if !ok {
		rep.Elapsed = o.deps.Now().Sub(start)
		rep.WithinRTO = rep.Elapsed <= o.cfg.DRRTO
		return rep, fmt.Errorf("dr failover aborted at %s: %w", rep.Steps[len(rep.Steps)-1].Name, rep.Steps[len(rep.Steps)-1].Err)
	}

	ok = step("redis_sentinel_promotion", func() (string, error) {
		addr, err := o.deps.DR.PromoteRedisSecondary(ctx)
		if err != nil {
			return "", err
		}
		return "master=" + addr, nil
	})
	if !ok {
		rep.Elapsed = o.deps.Now().Sub(start)
		rep.WithinRTO = rep.Elapsed <= o.cfg.DRRTO
		return rep, fmt.Errorf("dr failover aborted at %s: %w", rep.Steps[len(rep.Steps)-1].Name, rep.Steps[len(rep.Steps)-1].Err)
	}

	o.mu.Lock()
	shards := make([]int, 0, len(o.shards))
	for s := range o.shards {
		shards = append(shards, s)
	}
	o.mu.Unlock()
	sort.Ints(shards)
	for _, shard := range shards {
		shard := shard
		ok = step(fmt.Sprintf("wal_archive_replay_shard_%d", shard), func() (string, error) {
			from, err := o.deps.DR.SecondaryWALTail(ctx, shard)
			if err != nil {
				return "", err
			}
			to, err := o.deps.DR.ReplayWALArchive(ctx, shard, from)
			if err != nil {
				return "", err
			}
			if to < from {
				return "", fmt.Errorf("replay regressed: %d -> %d", from, to)
			}
			return fmt.Sprintf("seq %d -> %d", from, to), nil
		})
		if !ok {
			rep.Elapsed = o.deps.Now().Sub(start)
			rep.WithinRTO = rep.Elapsed <= o.cfg.DRRTO
			return rep, fmt.Errorf("dr failover aborted at %s: %w", rep.Steps[len(rep.Steps)-1].Name, rep.Steps[len(rep.Steps)-1].Err)
		}
	}

	rep.Elapsed = o.deps.Now().Sub(start)
	rep.WithinRTO = rep.Elapsed <= o.cfg.DRRTO
	if !rep.WithinRTO {
		o.deps.Alerts.RaiseP1(context.Background(), "DR failover exceeded RTO", map[string]any{
			"elapsed_ms": rep.Elapsed.Milliseconds(), "rto_ms": o.cfg.DRRTO.Milliseconds(),
		})
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// Termination: WAL dirty-block flush (Task 4.3.10 step 1)
// ---------------------------------------------------------------------------

// Shutdown flushes every managed shard's dirty 4KB WAL blocks and releases
// any leader leases this orchestrator acquired. Errors are aggregated —
// every shard gets its flush attempt (fail-closed: a flush failure is
// surfaced, never skipped silently).
func (o *RecoveryOrchestrator) Shutdown(ctx context.Context) error {
	o.mu.Lock()
	shards := make([]int, 0, len(o.shards))
	held := map[int][2]any{}
	for s, c := range o.shards {
		shards = append(shards, s)
		if c.heldToken != "" {
			held[s] = [2]any{c.heldToken, c.heldEpoch}
		}
	}
	o.mu.Unlock()
	sort.Ints(shards)

	var errs []error
	if o.deps.Flusher != nil {
		for _, s := range shards {
			if err := o.deps.Flusher.FlushDirtyBlocks(ctx, s); err != nil {
				errs = append(errs, fmt.Errorf("flush wal shard %d: %w", s, err))
			}
		}
	}
	if o.deps.Leases != nil {
		for s, h := range held {
			expect := fmt.Sprintf("%s:%d", h[0].(string), h[1].(uint64))
			if _, err := o.deps.Leases.RevokeLeader(ctx, s, expect); err != nil {
				errs = append(errs, fmt.Errorf("release leader shard %d: %w", s, err))
			}
		}
	}
	return errors.Join(errs...)
}
