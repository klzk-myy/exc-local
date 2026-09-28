package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-04 persistence & recovery checkpoints.
//
// The landed implementation spans the C++ core (snapshot emission +
// graduated recovery ladder), Go services (objectstore, archiver,
// recovery orchestrator, wal-recovery CLI, market-data exporter),
// deploy scripts (PostgreSQL PITR, ClickHouse backup), and migrations
// 023/065/092/120. Checkers require owning files, key symbols, and the
// proving unit/integration tests.
func registerPhase04(r *spec.Registry) {

	// ---- Task 4.3.1: Deterministic Snapshot Emission ----------------------
	r.Register("P04-T4.3.1-C1", ckP04SnapshotCadence,
		"snapshot cadence 100k trades / 5 min + book_snapshots persistence")
	r.Register("P04-T4.3.1-C2", ckP04WalTrimOnAck,
		"WAL trim only after snapshot ack (zero-loss guard)")

	// ---- Task 4.3.2: WAL Archive ------------------------------------------
	r.Register("P04-T4.3.2-C1", ckP04WalArchive,
		"WAL S3 archive before trim, ETag-verified ack")
	r.Register("P04-T4.3.2-C2", ckP04ArchiveRetention,
		"90-day retention + Glacier lifecycle, object-lock delete denial")

	// ---- Task 4.3.3: Replay-From-Archive ----------------------------------
	r.Register("P04-T4.3.3-C1", ckP04ReplayFromArchive,
		"replay-from-archive historical reconstruction, fail-closed on gaps")

	// ---- Task 4.3.4: PostgreSQL PITR --------------------------------------
	r.Register("P04-T4.3.4-C1", ckP04PostgresPITR,
		"PostgreSQL PITR RPO ≤15s / RTO ≤5min — WAL archive + restore scripts")

	// ---- Task 4.3.5: Snapshot + WAL Replay Recovery -----------------------
	r.Register("P04-T4.3.5-C1", ckP04ExactRecovery,
		"snapshot + WAL replay byte-exact state recovery")
	r.Register("P04-T4.3.5-C2", ckP04BootInvariant,
		"boot-time book_seq == WAL tail invariant, fail-closed last resort")

	// ---- Task 4.3.6: ClickHouse Backup + Restore Drill --------------------
	r.Register("P04-T4.3.6-C1", ckP04ClickHouseBackup,
		"ClickHouse S3 backup + restore drill (spec §18.3, §24 #159)")

	// ---- Task 4.3.7: PG Partition Archival --------------------------------
	r.Register("P04-T4.3.7-C1", ckP04PartitionArchival,
		"PostgreSQL partition archival pipeline (§19.7, §24 #179)")

	// ---- Task 4.3.8: ClickHouse→S3 Daily Export ---------------------------
	r.Register("P04-T4.3.8-C1", ckP04MarketDataExport,
		"ClickHouse-to-S3 daily batch compactor + bulk archive export")

	// ---- Task 4.3.9: Graduated WAL Recovery Ladder -------------------------
	r.Register("P04-T4.3.9-C1", ckP04RecoveryLadder,
		"L1 CRC repair / L2 snapshot rebase / L3 halt + report")

	// ---- Task 4.3.10: Recovery Orchestration + DR ----------------------------
	r.Register("P04-T4.3.10-C1", ckP04RecoveryOrchestration,
		"end-to-end crash recovery + cross-region DR failover")

	// ---- Task 4.3.11: Time-Boxed Audit + Running Digests ---------------------
	r.Register("P04-T4.3.11-C1", ckP04AuditDigests,
		"time-boxed audit, running digests, snapshot checksums")

	// ---- Task 4.3.12: Per-Shard Scoped Reopen --------------------------------
	r.Register("P04-T4.3.12-C1", ckP04ScopedReopen,
		"per-shard scoped reopen, external-feed fallback, zero-sequence")
}

const (
	recoveryPkg = "./internal/recovery"
	archiverPkg = "./internal/archiver"
	objectPkg   = "./internal/objectstore"
	persistPkg  = "./internal/persistence"
)

// bashSyntax syntax-checks deploy scripts (bash -n) as evidence they parse.
func bashSyntax(rel ...string) step {
	return func(ctx context.Context, env *spec.Env) spec.Result {
		for _, p := range rel {
			out, err := spec.RunOutput(ctx, env.RepoRoot, "bash", "-n", env.Path(p))
			if err != nil {
				return spec.Failf("bash -n %s: %v — %s", p, err, spec.Tail(out, 8))
			}
		}
		return spec.Passf("bash -n ok: %v", rel)
	}
}

var ckP04SnapshotCadence = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/include/recovery/SnapshotManager.hpp",
			"core/src/recovery/SnapshotManager.cpp",
			"core/tests/test_snapshot_manager.cpp",
			"services/internal/db/migrations/023_create_book_snapshots.up.sql",
			"services/internal/recovery/snapshot_service.go"),
		structural(env, "core/include/recovery/SnapshotStore.hpp",
			"trade_interval = 100'000", "interval_ns = 300ull"),
		structural(env, "core/src/recovery/SnapshotManager.cpp",
			"trim_sealed", "poll_acks"),
		ctest("test_snapshot_manager"),
		gotest(recoveryPkg,
			"TestPersistViaReadyNotifyAndAck|TestScanOnceCatchupPersistsAndAcks|TestParseReadyMsg"))
}

var ckP04WalTrimOnAck = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "core/src/wal/Wal.cpp", "core/include/wal/Wal.hpp"),
		structural(env, "core/src/wal/Wal.cpp", "trim"),
		gotest(recoveryPkg,
			"TestDivergentExistingRowNeverAcked|TestPersistRejectsCorruptDescriptor|TestStoreFailureBlocksAckAndRetries"))
}

var ckP04WalArchive = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/recovery/archive_service.go",
			"services/internal/recovery/wal.go",
			"services/internal/objectstore/s3.go"),
		gotest(recoveryPkg,
			"TestArchiveAndStatus|TestArchiveNoTrimOnBadETag|TestScanSegmentRoundTrip|TestScanSegmentCorrupt"))
}

var ckP04ArchiveRetention = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/objectstore/client.go",
			"services/internal/devs3/server.go"),
		structural(env, "services/internal/recovery/archive_service.go", "GLACIER", "90 days"),
		gotest(recoveryPkg, "TestLifecycleTransition"),
		gotest(objectPkg,
			"TestObjectLockDeleteDenied|TestPutGetHeadList|TestListPagination|TestNotFound"))
}

var ckP04ReplayFromArchive = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/recovery/replay.go",
			"services/cmd/replay/main.go"),
		gotest(recoveryPkg,
			"TestReplayFromArchive|TestReplayMissingRange|TestReplayCorruptSegmentFailsClosed|TestReplaySeqGapFailsClosed|TestReplayMissingObjectFailsClosed"))
}

var ckP04PostgresPITR = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/postgres/postgresql.conf",
			"deploy/postgres/wal_archive.sh",
			"deploy/postgres/wal_restore.sh",
			"deploy/postgres/backup.sh",
			"deploy/postgres/restore_pitr.sh",
			"deploy/postgres/pitr_smoke.sh"),
		structural(env, "deploy/postgres/postgresql.conf", "archive_command", "wal_level"),
		bashSyntax(
			"deploy/postgres/wal_archive.sh",
			"deploy/postgres/wal_restore.sh",
			"deploy/postgres/backup.sh",
			"deploy/postgres/restore_pitr.sh",
			"deploy/postgres/pitr_smoke.sh"))
}

var ckP04ExactRecovery = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/recovery/RecoveryManager.cpp",
			"core/src/recovery/SnapshotStore.cpp"),
		ctest("test_recovery"))
}

var ckP04BootInvariant = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "core/include/recovery/RecoveryManager.hpp", "recover_ladder"),
		gtest("test_recovery", "RecoveryLadder.*"),
		gotest(recoveryPkg,
			"TestRecScanWalDirCleanContiguous|TestRecScanWalDirTailCorruptAndRepair"))
}

var ckP04ClickHouseBackup = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/clickhouse/backup.sh",
			"deploy/clickhouse/clickhouse-backup/config.yml",
			"deploy/clickhouse/RUNBOOK.md",
			"deploy/clickhouse/verify_counts_test.sh"),
		bashSyntax(
			"deploy/clickhouse/backup.sh",
			"deploy/clickhouse/verify_counts_test.sh"))
}

var ckP04PartitionArchival = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/archiver/partition_archiver.go",
			"services/internal/db/migrations/120_partition_archive_log.up.sql"),
		gotest(archiverPkg,
			"TestArchiveAndRestorePartition|TestArchiveReattachesOnUploadFailure|TestDroppedPartitionQueryFails"))
}

var ckP04MarketDataExport = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/cmd/s3-market-data-exporter/main.go",
			"services/cmd/s3-market-data-exporter/ch_http.go",
			"services/cmd/s3-market-data-exporter/s3.go",
			"services/internal/persistence/s3_exporter.go"),
		gotest(persistPkg,
			"TestArchiveName|TestExportDayEndToEnd|TestExportDayFailClosed|TestQueryBuilders|TestDirUploaderETagAndEscape"))
}

var ckP04RecoveryLadder = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/recovery/waldir.go",
			"services/internal/recovery/report.go",
			"services/internal/recovery/reconcile.go",
			"services/cmd/wal-recovery/main.go",
			"services/internal/db/migrations/065_recovery_reports.up.sql"),
		gotest(recoveryPkg,
			"TestRecScanWalDirSealedDamageLostRange|TestRecWriteRebaseMarkerAndRescan|TestRecLatestVerifiedSnapshotFallback|TestRecReportJSONRoundTrip"),
		gtest("test_recovery", "RecoveryLadder.*"))
}

var ckP04RecoveryOrchestration = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/recovery/orchestrator.go",
			"services/internal/recovery/redis_lease.go",
			"services/cmd/recovery-orchestrator/main.go"),
		gotest(recoveryPkg,
			"TestDRFailoverSequence|TestDRFailoverAbortsOnRPOBreach|TestFenceAndPromoteDeadLeader|TestFenceSkipsLiveLeader|TestLeaderEpochVerification|TestShutdownFlushesWALAndReleasesLeases"))
}

var ckP04AuditDigests = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/recovery/digest.go",
			"services/internal/recovery/pg_source.go",
			"services/internal/db/migrations/092_recovery_digests.up.sql"),
		gotest(recoveryPkg,
			"TestAuditAllStagesPass|TestAuditLedgerImbalanceFreeze|TestAuditNegativeBalanceFails|TestAuditBookWALMismatchFails|TestAuditOrphanTradeFails|TestAuditNostroMismatchFails|TestAuditCLSUnfinalFails|TestAuditFeedDeferral|TestAuditZeroSumExemptFromFallback|TestAuditStageOverrunFailsClosed|TestAuditDigestFastPathAndFallback|TestAuditDigestMismatchFullScanFallback|TestDigestChainVerification|TestCheckpointerBoundariesAndImbalance"))
}

var ckP04ScopedReopen = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/recovery/orchestrator.go", "services/internal/recovery/resolve.go"),
		gotest(recoveryPkg,
			"TestPerShardScopedReopen|TestReopenLadderAndAdmissionGate|TestCancelOnlyRejectsNewOrders"))
}
