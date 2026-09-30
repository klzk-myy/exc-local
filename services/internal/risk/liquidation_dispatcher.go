// liquidation_dispatcher.go — Phase-19 Tasks 19.3.26/19.3.27: the
// breach-dispatch seam the event-driven margin engine and the isolated
// margin service share.
//
// THE CONTRACT: a detected breach — account-level stop-out (CROSS /
// PORTFOLIO) or position-level isolated deficit — must reach the
// durable liquidation queue WITHOUT waiting for the §13.5 2-second
// scanner. The queue (liquidation.go) owns dedup (liquidation:dedup:
// {acct}[/pos:{id}], 3600s), anti-stranding retries, and the worker's
// lock-serialized consume path — dispatching through it preserves crash
// safety: a job that has been accepted survives this process dying
// mid-liquidation.
//
// Production binds QueueLiquidationDispatcher over *LiquidationQueue.
// Tests inject fakes — the seam is constructor-injected so no queue
// (and no Redis) is needed to exercise the engine's decision path.
package risk

import (
	"context"
	"fmt"
	"time"

	"exchange/pkg/decimal"
)

// LiquidationDispatcher is the breach-dispatch seam: submit one
// liquidation job into the durable queue contract. The boolean reports
// whether the job was actually queued (false ⇒ the dedup key was held —
// an identical job is already in flight, which is a SAFE no-op, never an
// error).
type LiquidationDispatcher interface {
	Dispatch(ctx context.Context, job LiquidationJob, snap *MarginSnapshot) (enqueued bool, err error)
}

// QueueLiquidationDispatcher binds *LiquidationQueue to the seam via
// EnqueueDedup — the engine sees the coalesced-vs-queued distinction
// for its dispatch counters.
type QueueLiquidationDispatcher struct {
	Queue *LiquidationQueue
}

// NewQueueLiquidationDispatcher binds the queue. A nil queue is rejected
// fail-closed — a breach path with nowhere to dispatch must not exist.
func NewQueueLiquidationDispatcher(q *LiquidationQueue) (*QueueLiquidationDispatcher, error) {
	if q == nil {
		return nil, fmt.Errorf("liquidation dispatcher: nil queue")
	}
	return &QueueLiquidationDispatcher{Queue: q}, nil
}

// Dispatch implements LiquidationDispatcher.
func (d *QueueLiquidationDispatcher) Dispatch(ctx context.Context,
	job LiquidationJob, snap *MarginSnapshot) (bool, error) {
	return d.Queue.EnqueueDedup(ctx, job, snapshotLevel(snap))
}

// snapshotLevel projects a MarginSnapshot into the advisory MarginLevel
// payload the queue carries (identical projection to
// LiquidationQueue.EnqueueSnapshot — the worker re-reads margin:level
// before acting; this is a hint, never authority).
func snapshotLevel(snap *MarginSnapshot) *MarginLevel {
	if snap == nil {
		return nil
	}
	lv := &MarginLevel{
		AccountID:  snap.AccountID,
		Equity:     snap.Equity,
		UsedMargin: snap.UsedMargin,
		Status:     snap.Status,
		UpdatedAt:  snap.Ts,
	}
	if snap.LevelPct != nil {
		lv.MarginLevelPct = *snap.LevelPct
	}
	return lv
}

// ---------------------------------------------------------------------------
// Job builders
// ---------------------------------------------------------------------------

// StopOutJob builds the account-level stop-out job the engine dispatches
// on a CROSS/PORTFOLIO breach of the tier stop-out (retail 50%, spec
// §13.6d). The snapshot economics ride along as advisory fields.
func StopOutJob(snap *MarginSnapshot, at time.Time) LiquidationJob {
	job := LiquidationJob{
		V:          1,
		AccountID:  snap.AccountID,
		Reason:     LiquidationReasonStopOut,
		EnqueuedAt: at,
		TsMs:       at.UnixMilli(),
	}
	if snap.LevelPct != nil {
		job.MarginLevelPct = snap.LevelPct.String()
	}
	job.Equity = snap.Equity.String()
	job.UsedMargin = snap.UsedMargin.String()
	return job
}

// IsolatedDeficitJob builds the position-scoped isolated-margin deficit
// job (Task 19.3.27): Reason ISOLATED_MARGIN_DEFICIT + PositionID set ⇒
// the queue's dedupKeyFor scopes the marker to the position, so the job
// can never suppress — or be suppressed by — the account-level path.
func IsolatedDeficitJob(accountID, positionID int64, symbol string,
	levelPct *decimal.Decimal, at time.Time) LiquidationJob {

	job := LiquidationJob{
		V:          1,
		AccountID:  accountID,
		PositionID: positionID,
		Symbol:     symbol,
		Reason:     LiquidationReasonIsolatedDeficit,
		EnqueuedAt: at,
		TsMs:       at.UnixMilli(),
	}
	if levelPct != nil {
		job.MarginLevelPct = levelPct.String()
	}
	return job
}

// compile-time seam assertion.
var _ LiquidationDispatcher = (*QueueLiquidationDispatcher)(nil)
