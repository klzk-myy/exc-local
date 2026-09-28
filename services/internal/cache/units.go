package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/config"
	"exchange/internal/marketapi"
)

// DefaultUnits is the production warm set — the task's P0/P1 split:
//
//	P0 (≤5s):  shard_map, sessions, account_locks
//	P1 (≤30s): instruments, fee_tiers, tickers, book_snapshots
//
// Sessions and locks are Redis-native state — "warming" them means
// rebuilding the secondary indexes and guard-rail sets the hot paths
// consult (the session:user/acct/ip ZSET indexes, the frozen-account
// denylist) from the primary records that survived the failover, not
// fabricating sessions. Books and tickers are re-materialized from the
// persisted PG read model — every payload is wrapped in an Envelope so
// readers can verify generation + freshness instead of trusting the key.
func DefaultUnits() []Unit {
	return []Unit{
		{
			Name: "shard_map", Priority: P0, MinKeys: 1,
			FreshTTL: time.Hour, // config file is the source of truth
			Warm:     warmShardMap,
		},
		{
			Name: "sessions", Priority: P0, MinKeys: 0,
			FreshTTL: 5 * time.Minute, // index rebuild is only valid until churn resumes
			Warm:     warmSessions,
		},
		{
			Name: "account_locks", Priority: P0, MinKeys: 0,
			FreshTTL: 5 * time.Minute,
			Warm:     warmAccountLocks,
		},
		{
			Name: "instruments", Priority: P1, MinKeys: 1,
			FreshTTL: time.Minute, // refresh cadence on the read side is 1min
			Warm:     warmInstruments,
		},
		{
			Name: "fee_tiers", Priority: P1, MinKeys: 1,
			FreshTTL: 5 * time.Minute,
			Warm:     warmFeeTiers,
		},
		{
			Name: "tickers", Priority: P1, MinKeys: 0,
			FreshTTL: 30 * time.Second,
			Warm:     warmTickers,
		},
		{
			Name: "book_snapshots", Priority: P1, MinKeys: 0,
			FreshTTL: 30 * time.Second,
			Warm:     warmBookSnapshots,
		},
	}
}

// ---------------------------------------------------------------------------
// P0 units
// ---------------------------------------------------------------------------

// warmShardMap rewrites the shard:map HASH from the sharding.yaml source
// of truth — after failover the file always wins over an empty/partial
// hash (the gateway boot path fails hard without it, Task 1.3.7).
func warmShardMap(ctx context.Context, env *Env) (int, error) {
	if env.RDB == nil {
		return 0, errors.New("shard_map: redis required")
	}
	sm, err := config.LoadShardMap(env.ShardMapPath)
	if err != nil {
		return 0, fmt.Errorf("shard_map: load: %w", err)
	}
	// goredis.Cmdable already satisfies config.ShardMapStore.
	if err := sm.WriteToRedis(ctx, env.RDB); err != nil {
		return 0, err
	}
	return len(sm.Entries()), nil
}

// warmSessions rebuilds the secondary ZSET indexes (sess:acct/user/ip)
// from live session:{sid} hashes — after Sentinel failover the hashes
// replicate but the indexes may lag; without them the per-account and
// per-IP concurrency caps admit more sessions than the §8.8 bounds.
func warmSessions(ctx context.Context, env *Env) (int, error) {
	if env.RDB == nil {
		return 0, errors.New("sessions: redis required")
	}
	var (
		cursor  uint64
		indexed int
	)
	for {
		keys, next, err := env.RDB.Scan(ctx, cursor, "session:*", 500).Result()
		if err != nil {
			return indexed, fmt.Errorf("sessions: scan: %w", err)
		}
		for _, key := range keys {
			sid := key[len("session:"):]
			f, err := env.RDB.HGetAll(ctx, key).Result()
			if err != nil || len(f) == 0 {
				continue // dead/corrupt hash — skip, never fabricate
			}
			score, ok := sessionScore(f)
			if !ok {
				continue
			}
			pttl, err := env.RDB.PTTL(ctx, key).Result()
			if err != nil || pttl <= 0 {
				continue // expired or persistent — index only live TTL'd rows
			}
			pipe := env.RDB.TxPipeline()
			addIdx := func(idx string) {
				pipe.ZAdd(ctx, idx, goredis.Z{Score: score, Member: sid})
				pipe.Expire(ctx, idx, pttl+time.Minute)
			}
			if acct, err := strconv.ParseInt(f["account_id"], 10, 64); err == nil && acct > 0 {
				addIdx("sess:acct:" + f["account_id"])
			}
			if f["user_id"] != "" {
				addIdx("sess:user:" + f["user_id"])
			}
			if f["ip"] != "" {
				addIdx("sess:ip:" + f["ip"])
			}
			if _, err := pipe.Exec(ctx); err != nil {
				return indexed, fmt.Errorf("sessions: index write %q: %w", sid, err)
			}
			indexed++
		}
		if cursor = next; cursor == 0 {
			break
		}
	}
	return indexed, nil
}

// sessionScore extracts the session creation instant as the ZSET score
// (sess:* indexes are scored by created unixnano, mirroring the
// session-store indexing contract).
func sessionScore(f map[string]string) (float64, bool) {
	t, err := time.Parse(time.RFC3339Nano, f["created_at"])
	if err != nil {
		return 0, false
	}
	return float64(t.UnixNano()), true
}

// warmAccountLocks republishes the frozen-account guard set. The
// account:lock:{id} mutexes themselves are transient 10s coordination —
// they are observed (counted) but never re-created: a lock that died
// with the old primary must not masquerade as held. What IS durable is
// the PG-persisted FROZEN account status, re-materialized into
// accounts:frozen so order/withdrawal hot paths keep failing closed
// while PG may still be recovering.
func warmAccountLocks(ctx context.Context, env *Env) (int, error) {
	if env.Pool == nil || env.RDB == nil {
		return 0, errors.New("account_locks: pool and redis required")
	}
	rows, err := env.Pool.Query(ctx,
		`SELECT id FROM accounts WHERE status = 'FROZEN'`)
	if err != nil {
		return 0, fmt.Errorf("account_locks: frozen query: %w", err)
	}
	var frozen []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		frozen = append(frozen, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	pipe := env.RDB.TxPipeline()
	pipe.Del(ctx, "accounts:frozen")
	if len(frozen) > 0 {
		members := make([]any, len(frozen))
		for i, id := range frozen {
			members[i] = id
		}
		pipe.SAdd(ctx, "accounts:frozen", members...)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("account_locks: frozen set: %w", err)
	}
	return len(frozen), nil
}

// ---------------------------------------------------------------------------
// P1 units
// ---------------------------------------------------------------------------

// warmInstruments writes md:instruments (the venue snapshot) and
// md:instrument:{symbol} entries, envelope-wrapped. An empty instrument
// table is a source failure, not "fresh empty data" (MinKeys=1).
func warmInstruments(ctx context.Context, env *Env) (int, error) {
	if env.Pool == nil || env.RDB == nil {
		return 0, errors.New("instruments: pool and redis required")
	}
	insts, err := marketapi.NewPgStore(env.Pool).ListInstruments(ctx)
	if err != nil {
		return 0, fmt.Errorf("instruments: source: %w", err)
	}
	if len(insts) == 0 {
		return 0, nil // MinKeys gate turns this into a unit error
	}
	gen, _ := Gen(ctx, env.RDB)
	srcTS := env.Now()
	pipe := env.RDB.TxPipeline()
	all, err := Wrap(gen, srcTS, insts)
	if err != nil {
		return 0, err
	}
	pipe.Set(ctx, "md:instruments", all, 0)
	for _, in := range insts {
		raw, err := Wrap(gen, srcTS, in)
		if err != nil {
			return 0, err
		}
		pipe.Set(ctx, "md:instrument:"+in.Symbol, raw, 0)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("instruments: write: %w", err)
	}
	return len(insts), nil
}

// warmFeeTiers republishes rl:tier:{name} (spec §4.1 per-tier rate-limit
// config) from fee_tiers so the limiter never starts cold against a
// missing row and silently defaults.
func warmFeeTiers(ctx context.Context, env *Env) (int, error) {
	if env.Pool == nil || env.RDB == nil {
		return 0, errors.New("fee_tiers: pool and redis required")
	}
	rows, err := env.Pool.Query(ctx, `
		SELECT tier_name, maker_bps::text, taker_bps::text,
		       COALESCE(promo_until::text, ''),
		       COALESCE(promo_maker_bps::text, ''),
		       COALESCE(promo_taker_bps::text, '')
		  FROM fee_tiers ORDER BY tier_name`)
	if err != nil {
		return 0, fmt.Errorf("fee_tiers: source: %w", err)
	}
	defer rows.Close()
	type tier struct {
		name, maker, taker, pUntil, pMaker, pTaker string
	}
	var tiers []tier
	for rows.Next() {
		var t tier
		if err := rows.Scan(&t.name, &t.maker, &t.taker,
			&t.pUntil, &t.pMaker, &t.pTaker); err != nil {
			return 0, err
		}
		tiers = append(tiers, t)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(tiers) == 0 {
		return 0, nil
	}
	pipe := env.RDB.TxPipeline()
	for _, t := range tiers {
		pipe.HSet(ctx, "rl:tier:"+t.name, map[string]any{
			"maker_bps":       t.maker,
			"taker_bps":       t.taker,
			"promo_until":     t.pUntil,
			"promo_maker_bps": t.pMaker,
			"promo_taker_bps": t.pTaker,
		})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("fee_tiers: write: %w", err)
	}
	return len(tiers), nil
}

// warmTickers re-materializes ticker:{symbol} HASHes (spec §4.3) from
// the trades table projection for each ACTIVE instrument — the 24h
// rolling aggregate is derivable from PG, never fabricated.
func warmTickers(ctx context.Context, env *Env) (int, error) {
	if env.Pool == nil || env.RDB == nil {
		return 0, errors.New("tickers: pool and redis required")
	}
	symbols, err := activeSymbols(ctx, env)
	if err != nil {
		return 0, err
	}
	store := marketapi.NewPgStore(env.Pool)
	now := env.Now()
	n := 0
	for _, sym := range symbols {
		tk, err := store.Ticker24h(ctx, sym, now)
		if err != nil {
			return n, fmt.Errorf("ticker %s: %w", sym, err)
		}
		if tk == nil {
			continue
		}
		raw, err := json.Marshal(tk)
		if err != nil {
			return n, err
		}
		if err := env.RDB.HSet(ctx, "ticker:"+sym,
			map[string]any{"json": string(raw)}).Err(); err != nil {
			return n, fmt.Errorf("ticker %s write: %w", sym, err)
		}
		n++
	}
	return n, nil
}

// warmBookSnapshots writes md:book:{symbol} envelope payloads from the
// persisted L2 book. Empty books are legitimate (instrument with no
// resting orders) — MinKeys 0 — but the envelope still carries the
// generation so a reader can distinguish "warmed empty book" from a
// cold key.
func warmBookSnapshots(ctx context.Context, env *Env) (int, error) {
	if env.Pool == nil || env.RDB == nil {
		return 0, errors.New("book_snapshots: pool and redis required")
	}
	symbols, err := activeSymbols(ctx, env)
	if err != nil {
		return 0, err
	}
	store := marketapi.NewPgStore(env.Pool)
	gen, _ := Gen(ctx, env.RDB)
	srcTS := env.Now()
	n := 0
	for _, sym := range symbols {
		snap, err := store.Snapshot(ctx, sym, 20)
		if err != nil {
			return n, fmt.Errorf("book %s: %w", sym, err)
		}
		if snap == nil {
			continue
		}
		raw, err := Wrap(gen, srcTS, snap)
		if err != nil {
			return n, err
		}
		if err := env.RDB.Set(ctx, "md:book:"+sym, raw, 0).Err(); err != nil {
			return n, fmt.Errorf("book %s write: %w", sym, err)
		}
		n++
	}
	return n, nil
}

// activeSymbols is the shared P1 symbol driver — ACTIVE instruments,
// bounded by env.MaxSymbols.
func activeSymbols(ctx context.Context, env *Env) ([]string, error) {
	rows, err := env.Pool.Query(ctx,
		`SELECT symbol FROM instruments WHERE status = 'ACTIVE'
		 ORDER BY symbol LIMIT $1`, env.MaxSymbols)
	if err != nil {
		return nil, fmt.Errorf("active symbols: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
