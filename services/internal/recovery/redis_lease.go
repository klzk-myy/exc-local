// Task 4.3.10 — Redis-backed OrchLeaseBackend adapter over the existing
// exchange/internal/redis client (engine:leader:{shard} epoch-lease keys,
// spec §4.2/§18.6.2). The client's token-checked Lua scripts provide the
// revocation/acquire primitives; this file only adapts signatures and
// parses the "{token}:{epoch}" value.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	exredis "exchange/internal/redis"
)

// OrchRedisLeases adapts *redis.Client to OrchLeaseBackend.
type OrchRedisLeases struct {
	C *exredis.Client
}

// NewOrchRedisLeases builds the adapter; nil client is a construction
// error (fail closed).
func NewOrchRedisLeases(c *exredis.Client) (*OrchRedisLeases, error) {
	if c == nil {
		return nil, errors.New("redis leases: nil client")
	}
	return &OrchRedisLeases{C: c}, nil
}

func orchLeaderKey(shardID int) string {
	return fmt.Sprintf("engine:leader:%d", shardID)
}

func (l *OrchRedisLeases) LeaderValue(ctx context.Context, shardID int) (string, bool, error) {
	v, err := l.C.Get(ctx, orchLeaderKey(shardID)).Result()
	if errors.Is(err, goredis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("redis leader get shard %d: %w", shardID, err)
	}
	return v, true, nil
}

func (l *OrchRedisLeases) RevokeLeader(ctx context.Context, shardID int, expectValue string) (bool, error) {
	token, epoch, err := OrchParseLeaderValue(expectValue)
	if err != nil {
		return false, fmt.Errorf("redis revoke shard %d: %w", shardID, err)
	}
	return l.C.ReleaseLeader(ctx, shardID, token, epoch)
}

func (l *OrchRedisLeases) AcquireLeader(ctx context.Context, shardID int, token string, epoch uint64, ttl time.Duration) (bool, error) {
	return l.C.TryAcquireLeader(ctx, shardID, token, epoch, ttl)
}
