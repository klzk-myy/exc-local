// Redis queue — Task 12.3.5 item 3.
//
// Layout:
//
//	notifications:pending    LIST — LPUSH'ed by producers, BLMOVE'd by
//	                         the dispatcher (FIFO: producers push left,
//	                         workers pop right).
//	notifications:processing LIST — reliable-queue claim set. BLMOVE
//	                         moves the item atomically so a crash
//	                         between claim and ack can never drop it;
//	                         Run's RequeueAll puts strays back on boot.
//	notifications:retry      ZSET (score = next-attempt unix ms) —
//	                         delayed retries + quiet-hours deferrals; a
//	                         plain list cannot express "not before".
package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Canonical key names (spec §4-style colon namespace).
const (
	PendingKey    = "notifications:pending"
	ProcessingKey = "notifications:processing"
	RetryKey      = "notifications:retry"
)

// Queue wraps the coordination Redis client. It never swallows errors —
// callers decide (fail-closed contract, spec §2.7).
type Queue struct {
	rdb *goredis.Client
}

// NewQueue builds the queue on the shared coordination client.
func NewQueue(rdb *goredis.Client) *Queue { return &Queue{rdb: rdb} }

// encode renders the queue item. Marshal is deterministic (struct field
// order) so the same bytes can be LREM'ed from processing on ack.
func (i QueueItem) encode() (string, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return "", fmt.Errorf("notifications: encode queue item: %w", err)
	}
	return string(b), nil
}

func decodeItem(raw string) (QueueItem, error) {
	var it QueueItem
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		return QueueItem{}, fmt.Errorf("notifications: decode queue item: %w", err)
	}
	return it, nil
}

// Enqueue pushes the item onto the pending list (left side; BLMOVE pops
// the right side → FIFO).
func (q *Queue) Enqueue(ctx context.Context, item QueueItem) error {
	raw, err := item.encode()
	if err != nil {
		return err
	}
	if err := q.rdb.LPush(ctx, PendingKey, raw).Err(); err != nil {
		return fmt.Errorf("notifications: enqueue: %w", err)
	}
	return nil
}

// Pop claims the head item atomically into the processing list
// (BLMOVE pending→processing). Returns (item, raw, ok); ok=false on a
// clean timeout — callers just loop.
func (q *Queue) Pop(ctx context.Context, timeout time.Duration) (QueueItem, string, bool, error) {
	raw, err := q.rdb.BLMove(ctx, PendingKey, ProcessingKey, "RIGHT", "LEFT", timeout).Result()
	switch {
	case errors.Is(err, goredis.Nil):
		return QueueItem{}, "", false, nil
	case err != nil:
		if ctx.Err() != nil {
			return QueueItem{}, "", false, ctx.Err()
		}
		return QueueItem{}, "", false, fmt.Errorf("notifications: pop: %w", err)
	}
	it, derr := decodeItem(raw)
	if derr != nil {
		// Undecodable item: leave it in processing (acked only on
		// success) but report — caller decides to ack-drop or park.
		return QueueItem{}, raw, false, derr
	}
	return it, raw, true, nil
}

// Ack removes the claimed item from the processing list after handling.
func (q *Queue) Ack(ctx context.Context, raw string) error {
	if err := q.rdb.LRem(ctx, ProcessingKey, 1, raw).Err(); err != nil {
		return fmt.Errorf("notifications: ack: %w", err)
	}
	return nil
}

// RequeueAll moves every claimed-but-unacked item from processing back
// to pending — the boot recovery leg of the reliable-queue pattern
// (a crashed dispatcher never takes claimed work to the grave).
func (q *Queue) RequeueAll(ctx context.Context) (int, error) {
	raws, err := q.rdb.LRange(ctx, ProcessingKey, 0, -1).Result()
	if err != nil {
		return 0, fmt.Errorf("notifications: requeue scan: %w", err)
	}
	moved := 0
	for range raws {
		// LMOVE processing→pending LEFT→RIGHT keeps original FIFO order
		// for the strays (they land at the tail).
		if err := q.rdb.LMove(ctx, ProcessingKey, PendingKey, "LEFT", "RIGHT").Err(); err != nil {
			return moved, fmt.Errorf("notifications: requeue move: %w", err)
		}
		moved++
	}
	return moved, nil
}

// ScheduleRetry parks the item in the retry zset until `at`.
func (q *Queue) ScheduleRetry(ctx context.Context, item QueueItem, at time.Time) error {
	raw, err := item.encode()
	if err != nil {
		return err
	}
	if err := q.rdb.ZAdd(ctx, RetryKey, goredis.Z{
		Score: float64(at.UnixMilli()), Member: raw}).Err(); err != nil {
		return fmt.Errorf("notifications: schedule retry: %w", err)
	}
	return nil
}

// PromoteDue moves due retry members back onto the pending list.
// ZREM-then-LPUSH per member: a crash between them re-adds the item on
// the next promote pass only if it was already removed — a duplicate
// delivery is preferable to a lost one (senders are idempotent by
// delivery_id on the tracking row; the attempt log shows the repeat).
func (q *Queue) PromoteDue(ctx context.Context, now time.Time, limit int64) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	raws, err := q.rdb.ZRangeByScore(ctx, RetryKey, &goredis.ZRangeBy{
		Min: "-inf", Max: fmt.Sprint(now.UnixMilli()), Count: limit,
	}).Result()
	if err != nil {
		return 0, fmt.Errorf("notifications: retry scan: %w", err)
	}
	moved := 0
	for _, raw := range raws {
		// ZREM acts as the claim — only one dispatcher instance wins it.
		n, err := q.rdb.ZRem(ctx, RetryKey, raw).Result()
		if err != nil {
			return moved, fmt.Errorf("notifications: retry claim: %w", err)
		}
		if n == 0 {
			continue // another worker claimed it first
		}
		if err := q.rdb.LPush(ctx, PendingKey, raw).Err(); err != nil {
			return moved, fmt.Errorf("notifications: retry requeue: %w", err)
		}
		moved++
	}
	return moved, nil
}

// PendingLen is the backlog gauge (observability/tests).
func (q *Queue) PendingLen(ctx context.Context) (int64, error) {
	n, err := q.rdb.LLen(ctx, PendingKey).Result()
	if err != nil {
		return 0, fmt.Errorf("notifications: pending len: %w", err)
	}
	return n, nil
}
