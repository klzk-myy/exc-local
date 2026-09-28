package accounts

import (
	"context"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Task 5.3.33 — dead-man switch / countdown cancel-all.
//
// One timer per account, shared across REST (`POST
// /api/v1/orders/countdown-cancel-all`), WS (`order.countdown_cancel_all`)
// and later FIX (Phase-18) — spec §24 #257. If the timer is not renewed
// before expiry, all resting orders for the account are cancelled
// atomically through OrderDispatcher.MassCancel.
//
// Redis layout (spec §4 key schema):
//
//	countdown:{account_id}   — deadline (unix ms), PX TTL = countdown
//	countdown:index          — ZSET account_id → deadline unix ms; the
//	                           sweeper's lookup, since expired keys
//	                           cannot be enumerated.
const (
	countdownKeyPrefix = "countdown:"
	countdownIndexKey  = "countdown:index"

	// Min/MaxCountdownMs — task text range (1s–300s, aligned with the
	// Phase-10 UI's 30s–300s configurable timeout; remediation #35).
	MinCountdownMs = 1000
	MaxCountdownMs = 300000

	// DefaultSweepInterval is the sweeper cadence between expiry checks.
	DefaultSweepInterval = 250 * time.Millisecond

	// sweepBatch bounds one PopExpired call's work.
	sweepBatch = 256
)

// CountdownStore is the persistence seam for timer state — satisfied by
// RedisCountdownStore in production and an in-memory fake in tests.
type CountdownStore interface {
	// Arm sets countdown:{id}=deadlineMs with PX ttl and indexes it.
	// onlyIfAbsent=true is a strict start (NX): returns armed=false when
	// a timer is already live.
	Arm(ctx context.Context, accountID int64, deadlineMs int64, ttl time.Duration, onlyIfAbsent bool) (armed bool, err error)
	// Disarm removes the timer key and index entry. Idempotent.
	Disarm(ctx context.Context, accountID int64) error
	// Deadline returns the armed deadline, or ok=false when absent.
	Deadline(ctx context.Context, accountID int64) (deadlineMs int64, ok bool, err error)
	// PopExpired atomically removes index entries due at `nowMs` whose
	// key has expired, heals stale index scores for renewed timers, and
	// returns the account ids whose countdown actually expired.
	PopExpired(ctx context.Context, nowMs int64, limit int) ([]int64, error)
}

// CountdownAck is the endpoint response: server_time and
// countdown_expiry (unix ms; 0 when disabled).
type CountdownAck struct {
	ServerTime      int64 `json:"server_time"`
	CountdownExpiry int64 `json:"countdown_expiry"`
}

// DeadManService owns the countdown lifecycle and the expiry sweep.
type DeadManService struct {
	store CountdownStore
	disp  OrderDispatcher
	now   func() time.Time
}

// NewDeadManService builds the service. disp is invoked on expiry; it
// must not be nil for Run/SweepOnce to do useful work (a nil dispatcher
// fails closed — SweepOnce returns an error rather than skipping the
// cancel).
func NewDeadManService(store CountdownStore, disp OrderDispatcher) *DeadManService {
	return &DeadManService{store: store, disp: disp, now: time.Now}
}

// Set arms or renews the account timer (heartbeat semantics — every call
// refreshes). countdownMs=0 disables (see Disable). renew=false is a
// strict start: when a timer is already live it rejects with
// COUNTDOWN_ALREADY_ACTIVE.
func (s *DeadManService) Set(ctx context.Context, accountID int64, countdownMs int64, renew bool) (*CountdownAck, error) {
	if countdownMs == 0 {
		return s.Disable(ctx, accountID)
	}
	if countdownMs < MinCountdownMs || countdownMs > MaxCountdownMs {
		return nil, errorf(CodeCountdownInvalid,
			"countdown_ms %d outside %d..%d", countdownMs, MinCountdownMs, MaxCountdownMs)
	}
	deadline := s.now().Add(time.Duration(countdownMs) * time.Millisecond)
	armed, err := s.store.Arm(ctx, accountID, deadline.UnixMilli(),
		time.Duration(countdownMs)*time.Millisecond, !renew)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "arm countdown: %v", err)
	}
	if !armed {
		return nil, newError(CodeCountdownAlreadyActive,
			"countdown already active for this account")
	}
	return &CountdownAck{
		ServerTime:      s.now().UnixMilli(),
		CountdownExpiry: deadline.UnixMilli(),
	}, nil
}

// Disable clears the account timer (countdown_ms=0 semantics).
func (s *DeadManService) Disable(ctx context.Context, accountID int64) (*CountdownAck, error) {
	if err := s.store.Disarm(ctx, accountID); err != nil {
		return nil, errorf("INTERNAL_ERROR", "disable countdown: %v", err)
	}
	return &CountdownAck{ServerTime: s.now().UnixMilli(), CountdownExpiry: 0}, nil
}

// Status reports the current timer (for UI/admin introspection).
func (s *DeadManService) Status(ctx context.Context, accountID int64) (*CountdownAck, error) {
	ms, ok, err := s.store.Deadline(ctx, accountID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "countdown status: %v", err)
	}
	ack := &CountdownAck{ServerTime: s.now().UnixMilli()}
	if ok {
		ack.CountdownExpiry = ms
	}
	return ack, nil
}

// SweepOnce fires the cancel-all for every timer expired as of now.
// Returns the number of accounts whose orders were mass-cancelled.
// Cancel failures are fail-closed: the error propagates and the index
// entry is re-armed for a short retry horizon (the key is already gone,
// so the account is rescheduled rather than silently dropped).
func (s *DeadManService) SweepOnce(ctx context.Context) (int, error) {
	if s.disp == nil {
		return 0, errorf("INTERNAL_ERROR", "dead-man dispatcher not wired")
	}
	expired, err := s.store.PopExpired(ctx, s.now().UnixMilli(), sweepBatch)
	if err != nil {
		return 0, errorf("INTERNAL_ERROR", "countdown sweep: %v", err)
	}
	cancelled := 0
	for _, accountID := range expired {
		_, cerr := s.disp.MassCancel(ctx, MassCancelScope{
			AccountID: accountID,
			Reason:    "deadman",
		})
		if cerr != nil {
			// Re-index for retry in ~1s: the orders are still resting and
			// the account asked for dead-man protection — do not drop.
			deadline := s.now().Add(time.Second).UnixMilli()
			if _, rerr := s.store.Arm(ctx, accountID, deadline, time.Second, false); rerr != nil {
				return cancelled, errorf("INTERNAL_ERROR",
					"dead-man cancel for %d failed (%v) and re-arm failed: %v", accountID, cerr, rerr)
			}
			return cancelled, errorf("INTERNAL_ERROR",
				"dead-man mass cancel for account %d: %v", accountID, cerr)
		}
		cancelled++
	}
	return cancelled, nil
}

// Run sweeps on interval until ctx is cancelled. The gateway runs this
// loop in one instance per deployment (the Redis index is shared and
// PopExpired is atomic, so multiple replicas are safe — each expiry is
// claimed by exactly one sweeper).
func (s *DeadManService) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := s.SweepOnce(ctx); err != nil {
				return err
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Redis implementation
// ---------------------------------------------------------------------------

// RedisCountdownStore implements CountdownStore over the coordination
// Redis (internal/redis.Client embeds *goredis.Client; *goredis.Client
// satisfies this too).
type RedisCountdownStore struct {
	rdb *goredis.Client
}

// NewRedisCountdownStore builds the store. Any client satisfying
// *goredis.Client works — pass excredis.New(...).Client or the wrapper.
func NewRedisCountdownStore(rdb *goredis.Client) *RedisCountdownStore {
	return &RedisCountdownStore{rdb: rdb}
}

func countdownKey(accountID int64) string {
	return countdownKeyPrefix + strconv.FormatInt(accountID, 10)
}

// armScript atomically SETs the timer key and indexes the deadline.
// ARGV: deadline_ms, ttl_ms, nx ("1" = strict start), account_id.
// Returns 1 when armed, 0 when NX blocked the write.
var armScript = goredis.NewScript(`
local key = KEYS[1]
if ARGV[3] == "1" then
  if redis.call("SET", key, ARGV[1], "PX", ARGV[2], "NX") == false then
    return 0
  end
else
  redis.call("SET", key, ARGV[1], "PX", ARGV[2])
end
redis.call("ZADD", KEYS[2], ARGV[1], ARGV[4])
return 1
`)

func (s *RedisCountdownStore) Arm(ctx context.Context, accountID int64, deadlineMs int64,
	ttl time.Duration, onlyIfAbsent bool) (bool, error) {
	nx := "0"
	if onlyIfAbsent {
		nx = "1"
	}
	res, err := armScript.Run(ctx, s.rdb,
		[]string{countdownKey(accountID), countdownIndexKey},
		deadlineMs, ttl.Milliseconds(), nx, accountID).Int()
	if err != nil {
		return false, fmt.Errorf("redis arm: %w", err)
	}
	return res == 1, nil
}

func (s *RedisCountdownStore) Disarm(ctx context.Context, accountID int64) error {
	return disarmScript.Run(ctx, s.rdb,
		[]string{countdownKey(accountID), countdownIndexKey}, accountID).Err()
}

var disarmScript = goredis.NewScript(`
redis.call("DEL", KEYS[1])
redis.call("ZREM", KEYS[2], ARGV[1])
return 1
`)

func (s *RedisCountdownStore) Deadline(ctx context.Context, accountID int64) (int64, bool, error) {
	v, err := s.rdb.Get(ctx, countdownKey(accountID)).Result()
	if err == goredis.Nil {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("redis get countdown: %w", err)
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("countdown value corrupt for %d: %w", accountID, err)
	}
	return ms, true, nil
}

// popScript claims due index entries: members whose key is gone expired
// for real (returned to the caller); members whose key still lives were
// renewed after being indexed — their score is healed to the true
// deadline instead of being removed. Lua runs atomically, so a renewal
// landing mid-sweep can never produce a lost index entry.
var popScript = goredis.NewScript(`
local due = redis.call("ZRANGEBYSCORE", KEYS[1], "-inf", ARGV[1], "LIMIT", 0, ARGV[2])
local expired = {}
for _, id in ipairs(due) do
  local pttl = redis.call("PTTL", "countdown:" .. id)
  if pttl <= 0 then
    redis.call("ZREM", KEYS[1], id)
    table.insert(expired, id)
  else
    redis.call("ZADD", KEYS[1], ARGV[1] + pttl, id)
  end
end
return expired
`)

func (s *RedisCountdownStore) PopExpired(ctx context.Context, nowMs int64, limit int) ([]int64, error) {
	res, err := popScript.Run(ctx, s.rdb, []string{countdownIndexKey}, nowMs, limit).Result()
	if err != nil {
		return nil, fmt.Errorf("redis pop expired: %w", err)
	}
	arr, ok := res.([]interface{})
	if !ok {
		return nil, fmt.Errorf("redis pop expired: unexpected reply %T", res)
	}
	out := make([]int64, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("redis pop expired: non-string member %T", v)
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis pop expired: bad member %q", s)
		}
		out = append(out, id)
	}
	return out, nil
}
