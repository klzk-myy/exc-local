package main

// Phase-13 Task 13.3.2 — reconciliation engine wiring helpers.
//
// WAL discovery: the C++ matching engine journals per shard at
// <wal-root>/<shard_id>/{seq}.wal (core/src/main.cpp -wal-dir). The
// gateway resolves the dir set two ways:
//   - EXC_WAL_DIRS  — explicit comma-separated per-shard dirs (wins
//     outright; co-located deployments point this at the engine mount).
//   - EXC_WAL_ROOT  — root whose children are the shard dirs (default
//     "/wal"), expanded with every shard id in the loaded shard map.
// No dirs configured/discovered → the ORDERS/TRADES legs record
// INCONCLUSIVE (fail-closed), never a fabricated pass.
//
// Cadence: EXC_RECON_INTERVAL overrides the hourly default (duration
// syntax, e.g. "30m"; used by integration tests at ~100ms granularity).

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"exchange/internal/config"
)

// reconciliationWalDirs resolves the per-shard WAL directories.
func reconciliationWalDirs(shardMap *config.ShardMap) []string {
	if raw := strings.TrimSpace(os.Getenv("EXC_WAL_DIRS")); raw != "" {
		var out []string
		for _, d := range strings.Split(raw, ",") {
			if d = strings.TrimSpace(d); d != "" {
				out = append(out, d)
			}
		}
		return out
	}
	root := strings.TrimSpace(os.Getenv("EXC_WAL_ROOT"))
	if root == "" {
		root = "/wal"
	}
	if shardMap == nil {
		return nil
	}
	seen := map[int]bool{}
	var ids []int
	for _, id := range shardMap.Entries() {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, root+"/"+strconv.Itoa(id))
	}
	return out
}

// reconciliationInterval reads EXC_RECON_INTERVAL (default 1h).
func reconciliationInterval() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("EXC_RECON_INTERVAL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return time.Hour
}
