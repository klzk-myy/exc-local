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

// walDirsForSymbol resolves the WAL dir(s) owning the symbol's shard —
// Phase-17 Task 17.3.2 L3 snapshot reader's WalDirs seam. Under the
// EXC_WAL_ROOT convention it is exactly root/<shard>; under the explicit
// EXC_WAL_DIRS list the full set is returned and the reader's
// per-segment shard filter picks the matching dir (a foreign-shard dir
// is skipped, not an error).
func walDirsForSymbol(shardMap *config.ShardMap, symbol string) []string {
	if strings.TrimSpace(os.Getenv("EXC_WAL_DIRS")) != "" {
		return reconciliationWalDirs(shardMap)
	}
	root := strings.TrimSpace(os.Getenv("EXC_WAL_ROOT"))
	if root == "" {
		root = "/wal"
	}
	return []string{root + "/" + strconv.Itoa(shardMap.GetShard(symbol))}
}

// walDirsForShard resolves the WAL dir(s) for one shard id — the
// per-shard analog of walDirsForSymbol for the boot-time missing-fill
// recovery. Under the EXC_WAL_ROOT convention it is exactly
// root/<shard>; the explicit EXC_WAL_DIRS list returns the full set and
// the segment-header shard filter picks the matching dir.
func walDirsForShard(shardMap *config.ShardMap, shard uint16) []string {
	if strings.TrimSpace(os.Getenv("EXC_WAL_DIRS")) != "" {
		return reconciliationWalDirs(shardMap)
	}
	root := strings.TrimSpace(os.Getenv("EXC_WAL_ROOT"))
	if root == "" {
		root = "/wal"
	}
	return []string{root + "/" + strconv.Itoa(int(shard))}
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
