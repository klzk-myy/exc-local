# Runbook: WAL archive stalled / telemetry gap (`WALLagNotDraining`, `WALLagTelemetryAbsent`)

**Severity:** P2 (stall) / P3 (telemetry gap) · **Metric:** `wal_lag_entries{shard}` (registered in `services/internal/observability/http.go` — `SetWALLag` feeder pending wiring) · **Owner:** SRE

## Symptom

`wal_lag_entries` has been nonzero for >30m (archive pipeline not draining), or the series emits no samples at all for >15m (telemetry gap).

## Diagnosis

1. Distinguish backlog vs stall: `wal_lag_entries` >100000 for 5m is `WALLagGrowing` (P1 — see [wal-lag-growing.md](./wal-lag-growing.md)); a small but *persistent* backlog for 30m means the archive path itself is stuck.
2. Archive path: `exchange archive-status --shard=N` (services/cmd/exchange) shows the archive cursor vs WAL tail; `exchange archive-lifecycle --shard=N` confirms segment transitions. S3 archive wiring: `deploy/dr/s3-crr-wal-archive.json` + `EXC_S3_*` env (archiver cmd in `services/cmd/archiver`).
3. WAL trim is gated on snapshot+archive (zero-loss) — a stalled `recovery` snapshotter blocks archival transitively; check the recovery service before blaming S3.
4. Shared-disk correlation: WAL archive and bridge spool share the host disk — check `WALVolumeNearlyFull`/`CapacityDiskHeadroom`.

## Mitigation

1. Manual catch-up once the fault clears: `exchange archive-wal --wal-dir=<dir> --shard=N`.
2. Never delete `.wal` segments manually — the zero-loss guard only trims past the last archived segment.
3. If the WAL tail is suspect (CRC warnings), stop and follow [wal-recovery-halt.md](./wal-recovery-halt.md).

## telemetry-gap

`WALLagTelemetryAbsent` = the gauge family is registered but no feeder calls `SetWALLag`. Until the waldir/archive monitor (`services/internal/recovery/waldir.go`) wires it, WAL durability alerting is blind — wire the feed; do not silence the rule. Interim visibility: `exchange archive-status` CLI and `wal/` directory segment count.

## Escalation

- Stall (P2) → SRE; escalate to P1 if the backlog starts growing (crosses `WALLagGrowing`) or a shard host degrades while lag >0.
- Telemetry gap (P3) → observability backlog item; blocks closure of the WAL-lag domain in Task 13.3.3 acceptance.
