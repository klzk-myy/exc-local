# Runbook: host resource pressure (`HostMemoryPressure`, `HostMemoryHigh`, `HostSwapInUse`, `WALVolumeNearlyFull`, `HostCPUSaturated`, `HostCPUCritical`, `HostRunqueueHigh`)

**Severity:** P1 (>90% mem, >95% CPU, WAL volume <10% free) / P2 (sustained high) / P3 (swap, runqueue) · **Source:** node-exporter job (`:9100`) in `deploy/prometheus/prometheus.yml` · **Owner:** SRE

These are *pressure* rules layered on the Phase-09 capacity-headroom pack (`deploy/monitoring/capacity-alerts.yml` — 70–75% watch thresholds). Capacity planning model: [../ops/capacity-planning.md](../ops/capacity-planning.md).

## Symptom

A bare-metal or container host is exhausting memory, CPU, or a durability-path filesystem.

## Diagnosis

### memory

`node_memory_MemAvailable_bytes`/`MemTotal_bytes`. >90% for 5m is OOM territory — identify the consumer (`ps aux --sort=-rss`); on shard hosts the engine's pre-allocated pools dominate, so growth usually means a leak in a sidecar (bridge buffer, admin monitor), not the engine itself.

### swap

Any swap use on a matching host violates the latency contract (p99 ≤50µs). Check whether hot processes are actually `mlockall`'d; swap growth without RSS growth = the kernel is reclaiming file-cache-adjacent pages, usually harmless but worth the ticket.

### cpu

`node_cpu_seconds_total{mode="idle"}` aggregate per instance. Check whether saturation is concentrated on one core (pinned engine loop) — `mode!="idle"` per-CPU breakdown shows it; a single pinned core at 100% with others idle is a hot-shard symptom, not a host problem.

### disk

`node_filesystem_*` on `/var/lib/exchange/wal` and `/var/spool/exchange` (bridge spool). WAL volume <10% free blocks the durability path; spool full → bridge drops events (`bridge_events_dropped_total`).

## Mitigation

1. Memory/CPU page on a shard host: evaluate failover before the host dies — [engine-halt-failover.md](./engine-halt-failover.md). Do not "wait and see" through an OOM on the matching path.
2. Disk: free the WAL mount via archive catch-up (`exchange archive-wal`), never by deleting `.wal` segments or the bridge spool — both are zero-loss guarded.
3. Chronic pressure → capacity item per §19.9 (30% safety-margin contract); record in the quarterly capacity review.

## Escalation

- P1 rules page immediately. P2 escalate to P1 if pressure keeps climbing toward the page threshold for 15m after triage starts.
