package main

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"exchange/internal/config"
	"exchange/internal/redis"
)

var cacheShardMapCmd = &cobra.Command{
	Use:   "cache-shard-map",
	Short: "Write the shard map to the Redis shard:map HASH",
	Long: `cache-shard-map loads config/sharding.yaml (the same file the C++
engine parses) and atomically replaces the Redis shard:map HASH so every
service can resolve symbols → shard ids at startup. Fields are
canonical-symbol → shard id plus meta:elastic_base / meta:elastic_count so
Redis-only readers apply the same FNV-1a elastic policy for unmapped
symbols. Run after editing sharding.yaml or after Redis DR recovery.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		sm, err := config.LoadShardMap("") // file is the source of truth
		if err != nil {
			return err
		}

		ctx := cmd.Context()
		rdb := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
		defer func() { _ = rdb.Close() }()

		if err := sm.WriteToRedis(ctx, rdb); err != nil {
			return err
		}

		byShard := map[int][]string{}
		for sym, shard := range sm.Entries() {
			byShard[shard] = append(byShard[shard], sym)
		}
		shards := make([]int, 0, len(byShard))
		for s := range byShard {
			shards = append(shards, s)
		}
		sort.Ints(shards)
		for _, s := range shards {
			sort.Strings(byShard[s])
			fmt.Printf("  shard %d: %v\n", s, byShard[s])
		}
		fmt.Printf("cache-shard-map: wrote %d symbols to %s @ %s (elastic=%d+%d)\n",
			len(sm.Entries()), config.ShardMapRedisKey, cfg.Redis.Addr,
			sm.ElasticBase(), sm.ElasticCount())
		return nil
	},
}
