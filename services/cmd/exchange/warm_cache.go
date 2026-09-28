package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"exchange/internal/cache"
	"exchange/internal/config"
	"exchange/internal/redis"
)

// Task 9.3.8 item 5 — `exchange warm-cache`: the manual operator path for
// cache warming (post-DR Redis flush, cold standby promotion, runbook
// recovery). It runs the same Warmer the gateway boots with — P0 units
// serialized under the 5s SLA, P1 concurrent under 30s, freshness
// markers written only after each unit's source-verified population.
// The pass fails closed (exit 1) when any unit reports an error.
var warmCacheCmd = &cobra.Command{
	Use:   "warm-cache",
	Short: "Warm Redis cache units from authoritative sources (Task 9.3.8)",
	Long: `warm-cache repopulates the Redis cache surface — shard map, session
index, fee tiers, instruments, market tickers, book snapshots, account
locks — from PostgreSQL/engine truth. Equivalent to the automatic pass
the gateway runs on deploy and on the warm:trigger pub/sub funnel
published after recovery/failover.

Exit 0 when every unit warms within its priority budget; exit 1 when any
unit fails or exceeds budget — the caller/runbook must treat that as an
incomplete warm (markers are not written for failed units, so readers
never see a false-fresh key).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		ctx := cmd.Context()

		pool, err := openPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		rdb := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
		defer func() { _ = rdb.Close() }()

		log := slog.New(slog.NewTextHandler(os.Stderr,
			&slog.HandlerOptions{Level: slog.LevelInfo}))
		w := cache.New(&cache.Env{Pool: pool, RDB: rdb.Client, Log: log},
			cache.DefaultUnits())

		rep := w.Run(ctx, "manual")
		for _, u := range rep.Units {
			fmt.Printf("  %-14s %-4s keys=%-6d %6dms %s\n",
				u.Name, u.Priority, u.Keys, u.DurationMs, u.Status)
		}
		fmt.Printf("warm-cache: gen=%d trigger=%s finished=%s\n",
			rep.Gen, rep.Trigger, rep.FinishedAt.UTC().Format("2006-01-02T15:04:05Z"))
		if !rep.OK() {
			return fmt.Errorf("cache warm incomplete")
		}
		return nil
	},
}
