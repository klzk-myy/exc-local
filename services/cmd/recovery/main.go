// Command recovery is the snapshot persistence service (Phase-04 Task
// 4.3.1, spec §3.5): one instance per matching-engine shard, colocated
// with the engine, consuming SnapReadyMsg descriptors off the shm ring and
// writing verified snapshot bytes to PostgreSQL book_snapshots, then
// acking back so the engine can trim sealed WAL segments.
//
// Boot: load shared config (logging, postgres.dsn) + env overrides
// EXC_RECOVERY_SHARD / EXC_RECOVERY_IPC_BASE / EXC_RECOVERY_SNAP_DIR /
// EXC_RECOVERY_SCAN_INTERVAL_S, attach the ring pair (advisory create —
// boot order vs the engine is irrelevant), open the pgx pool, then Run.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/ipc"
	"exchange/internal/recovery"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envU16(key string, def uint16) (uint16, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return uint16(n), nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level, _ := logging.ParseLevel(cfg.Logging.Level)
	log, err := logging.New(level, cfg.Logging.Format)
	if err != nil {
		return err
	}

	shard, err := envU16("EXC_RECOVERY_SHARD", 0)
	if err != nil {
		return err
	}
	rcfg := recovery.Config{
		ShardID: shard,
		IPCBase: envStr("EXC_RECOVERY_IPC_BASE", ipc.DefaultShmBase),
		SnapDir: envStr("EXC_RECOVERY_SNAP_DIR",
			fmt.Sprintf("snapshots/%d", shard)),
	}
	if v := os.Getenv("EXC_RECOVERY_SCAN_INTERVAL_S"); v != "" {
		secs, perr := strconv.ParseFloat(v, 64)
		if perr != nil || secs <= 0 {
			return fmt.Errorf("EXC_RECOVERY_SCAN_INTERVAL_S: %q", v)
		}
		rcfg.ScanInterval = time.Duration(secs * float64(time.Second))
	}
	if err := rcfg.Validate(); err != nil {
		return err
	}

	ctx, stop := utils.SignalContext()
	defer stop()

	pool, err := db.NewPool(ctx, cfg.Postgres.DSN, 4)
	if err != nil {
		return err
	}
	defer pool.Close()

	store, err := recovery.NewPgxSnapshotStore(pool)
	if err != nil {
		return err
	}
	svc, err := recovery.Open(rcfg, store, log)
	if err != nil {
		return err
	}
	defer svc.Close()

	log.Info("recovery service ready",
		slog.Uint64("shard", uint64(rcfg.ShardID)),
		slog.String("snap_dir", rcfg.SnapDir),
		slog.String("ready_ring", recovery.ReadyRingName(rcfg.IPCBase, rcfg.ShardID)),
		slog.String("ack_ring", recovery.AckRingName(rcfg.IPCBase, rcfg.ShardID)))

	err = svc.Run(ctx)
	if errors.Is(err, ctx.Err()) {
		log.Info("recovery service stopped", "metrics", svc.Metrics())
		return nil
	}
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "recovery: %v\n", err)
		os.Exit(1)
	}
}
