# Runbook: `WALLagGrowing` — unarchived WAL backlog approaching RPO bound

**Severity:** P1 (page) · **Rule:** `wal_lag_entries > 100000` for 5m · **Domain:** binary WAL durability (§3.4); archive per §18.2 — local WAL never trimmed past the last fully-archived segment (zero-loss guard).

## Symptom

>100,000 unarchived WAL entries on `{shard}` for >5m. The fsync/archive pipeline is stalled — if the shard host dies now, recovery depends on the S3 archive which is behind; RPO 0 (dual-write) is at risk of degrading to "whatever the archive last saw."

## Diagnosis

1. Where is the stall: local fsync vs S3 archive vs consumer.
   - fsync stall: NVMe health/latency on the shard host (Tier-3 watchdog trips `MarketDataOnly` at disk <10% free or write latency >10ms — check whether the mode already flipped).
   - archive stall: `exchange archive-status --shard=N` (`services/cmd/exchange/archive.go`) shows per-shard archive cursor vs WAL tail.
   - WAL dir growth: `wal/` directory segment count vs `archive-lifecycle` view.
2. S3 side: `devs3`/MinIO endpoint reachable in dev; in prod check bucket endpoint + credentials (Vault/KMS per alertmanager convention). `exchange archive-wal --wal-dir=DIR --shard=N` is the operator archive command — run it manually to see the real error.
3. Correlate with `BridgeBufferDepthHigh`: WAL archive and bridge spool share the host disk — a full disk stalls both.
4. Check the recovery service: `services/cmd/recovery` (snapshot persistence, one per shard) — a stalled snapshotter makes WAL growth unbounded because trim waits on snapshot+archive.

## Mitigation

1. Disk pressure: free space on the WAL/NVMe mount; the zero-loss guard means WAL *cannot* be trimmed ahead of archive — never delete `.wal` segments manually.
2. S3 unavailable: archive backlog is safe as long as disk holds (local segments persist until archived); restore S3 connectivity then run `exchange archive-wal --wal-dir=<dir> --shard=N` to catch up. `exchange archive-lifecycle --shard=N` confirms catch-up.
3. Snapshotter stalled → the archiver will never advance; restart `recovery` service for the shard, then re-check `archive-status`.
4. After catch-up verify tail integrity: `wal_audit` (`core/build/wal_audit`) fingerprint on a staged copy — see [engine-halt-failover.md](./engine-halt-failover.md) §Diagnosis 3 for the parity procedure.
5. If WAL files are already suspect (CRC warnings in engine log): stop and follow [wal-recovery-halt.md](./wal-recovery-halt.md) — do not archive-checksum a corrupt segment over a good one.

## Escalation

- P1: On-call SRE. Escalate to P0 if: lag crosses the point where disk cannot hold the backlog, `MarketDataOnly` trips, or a shard host failure occurs *while* lag >0 (loss window opened).
- Recurring archive lag → capacity item: archive throughput vs ingress rate belongs in [../ops/capacity-proof.md](../ops/capacity-proof.md).
