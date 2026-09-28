# Runbook: Engine halt & warm-standby promotion (matching-loop stall / watchdog trip)

**Severity:** P0 (matching stopped = trading halted) · **Triggers:** `L0ErrorObserved`, `MATCHING_LOOP_STALLED` → `SIGABRT` (Tier-2 watchdog >2ms), systemd `WatchdogSec=1s` kill, `/dev/watchdog` host reset, `IPCRingCritical` permanent-halt latch · **Domain:** spec §18.6 crash-recovery state machine; `services/cmd/recovery-orchestrator` owns promotion + pre-open audit.

## Symptom

A matching-engine shard stopped consuming: process dead/restarting, `engine:leader:{shardId}` lease expiring, `aeron` egress silent, `bridge_heartbeat_age_seconds` climbing. The §18.6.1 state machine should be traversing `CRASH_DETECTED → FENCED_INGRESS_CLAMP → STANDBY_PROMOTION → WAL_REPLAY_LADDER → DATA_INTEGRITY_AUDIT → CANCEL_ONLY_GRACE → REOPENING_AUCTION → NORMAL`.

## Diagnosis

1. Confirm the fence: coordination Redis `GET engine:leader:{shardId}` — value `{epoch: N, leader: "core-node-1"}`. The demoted primary must be dead; if it responds, it self-`SIGTERM`s on seeing epoch N+1 (§18.6.2). Two live leaders = split-brain: kill the stale one immediately, then treat as a security-grade incident (dual writes possible).
2. Confirm ingress clamp engaged: REST 503 (`SERVICE_DEGRADED`/`INSTRUMENT_HALTED`), WS error frame `INSTRUMENT_HALTED`, FIX `35=j` BusinessMessageReject; cancels (DELETE, 35=F/35=q) still accepted on the cancel lane.
3. Watch promotion: `recovery-orchestrator` logs the epoch increment and emits `ENGINE_RECOVERY_PROMOTED {shardId, epoch: N+1, recovered_seq}` to the bridge. If no promotion in ~3s (§18.6.3 RTO ≤3s local), check the orchestrator and the standby's `RecoveryManager` log.
4. Fingerprint parity after replay (the `failover_bench.sh` contract, `tests/soak/failover_bench.sh`): `wal_audit -mode fingerprint -live` on a staged copy pre/post — `fp_primary == fp_standby` proves deterministic replay. Use this same check for incident verification.
5. If the replay ladder halts (`WAL_RECOVERY_HALT` + `recovery_reports` row): switch to [wal-recovery-halt.md](./wal-recovery-halt.md) — the shard does not open.

## Mitigation

1. Promotion succeeded + audit green: let the resumption ladder run — `CANCEL_ONLY` 60s grace (clients cancel resting exposure), FIX `TradingSessionStatus` Halt→Open broadcast, WS `resume` replay, 5s call auction, then `Normal`. Per-shard verdicts: a failed shard enters `HALT_LEGAL_FREEZE` alone while healthy shards proceed (§18.6.7.2).
2. Promotion stalled >3s on a hot shard: check standby health (WAL dir readable, snapshot present in `book_snapshots`, Aeron dir clean). Manual nudge is restarting `recovery-orchestrator` — it re-reads the lease/epoch state idempotently. Do NOT manually start a second engine on the same shard; the fencing epoch, not operator discipline, is the split-brain guard.
3. Standby unpromotable (no warm copy): the §18.6.4 multi-region path (RTO ≤5min, RPO ≤15s) becomes the fallback — that is a declared disaster, engage [dr-drill.md](./dr-drill.md) failover procedure live + BCP decision framework ([bcp-standdown.md](./bcp-standdown.md)) for timelines.
4. `/dev/watchdog` host reset: the BMC rebooted the box — promote on the *secondary host*, then diagnose the dead host offline (kernel logs, NVMe health — watchdog trip triage detail in [`../ops/daemon-supervision.md`](../ops/daemon-supervision.md), Task 9.3.28). Do not re-admit the host until root-caused; fleet state `MAINTENANCE` (`POST /api/v1/admin/fleet/hosts/{id}/cordon` — fleet backend `services/internal/fleet`, migration 091). Binary-swap restarts follow [`../ops/shard-binary-swap.md`](../ops/shard-binary-swap.md) (Task 9.3.16) rather than manual restarts.
5. Post-recovery: confirm `exchange_degradation_mode` returned `Normal` only after the 30s healthy window, `book_seq == wal_tail`, and zero `reconciliation_mismatches_total` delta from the failover window.

## Escalation

- P0 page tree auto-fires (On-call SRE → Core Eng Lead → VP Eng → CTO; unacked 5m → secondary leadership — [incident-escalation.md](./incident-escalation.md)).
- Trading halted >2min is P0 definitionally; comms: status page ≤30min, client email ≤1h, post-mortem ≤48h.
- Every failover writes incident evidence: `recovery_reports` row(s), epoch history, audit verdicts, fingerprint parity — required for the quarterly DR evidence file ([dr-drill.md](./dr-drill.md) §Evidence).
