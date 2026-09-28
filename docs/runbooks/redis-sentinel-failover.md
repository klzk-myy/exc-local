# Runbook: Redis Sentinel master failover

**Severity:** P1 while failover in progress (L1 trigger per §2.7.2); self-resolving inside RTO ≤30s / RPO ≤5s (spec §18.3, Task 9.3.20 targets <3s promotion) · **Domain:** Redis 7 Sentinel 3-node quorum (`deploy/redis/sentinel.conf`, `config/redis-sentinel.yaml`; compose `redis-primary` + `redis-replica-1/2` + `redis-sentinel-1/2/3`).

> Scope: this is the *incident* runbook — what to do when an unplanned failover pages you. Cluster deployment, health probing, and the scheduled drill harness are Task 9.3.20's document: [`../ops/redis-sentinel-failover.md`](../ops/redis-sentinel-failover.md).

## Symptom

Sentinel elected a new master: `+failover-*` events in sentinel logs, clients reconnecting, possible brief `L1ErrorsSustained`. Coordination keys at stake: `engine:leader:{shardId}` epoch leases, `system:degradation:*`, `circuit_breaker:*`, `halt:global`, session/rate-limit state.

## Diagnosis

1. Sentinel view: `redis-cli -p 26379 SENTINEL get-master-addr-by-name <master>` on each sentinel — all three must agree; `SENTINEL sentinels <master>` shows quorum (target `quorum=2`, `down-after-milliseconds=2000`, `failover-timeout=5000` per Task 9.3.20).
2. Failover legitimacy: a failover with no dead master = partition/ flap — check `SENTINEL CKQUORUM` and the failover counter metric (exported per Task 9.3.20 health probing — pending `deploy/monitoring/redis-sentinel-alerts.yml`).
3. Client-side impact: Go services use sentinel-aware clients (`services/internal/redis/sentinel.go` — same key space across failover); during the failover window Go gateways fall back to in-memory token buckets for rate limiting (§18.6.4). Sessions in `session:*` survive within RPO ≤5s; anything older is re-auth, not loss.
4. Leader-lease safety: engine leases are 2,000ms TTL — a Redis blip longer than that expires leases and *triggers engine standby promotion* (§18.6.2). Check `engine:leader:*` epochs after the failover settles — unexpected epoch increments mean engines fenced themselves (correct but incident-worthy).

## Mitigation

1. Let Sentinel finish — failover completes in 1–3s normally. Manual `SENTINEL failover <master>` only if the master is confirmed dead but not yet flagged past `down-after-milliseconds`.
2. Clients should self-heal via sentinel discovery; services stuck on the old master need a restart (their pools pinned the dead endpoint — log as a client-library defect if seen).
3. Re-establish replica topology: the old master, when it returns, must rejoin as replica (Sentinel reconfigures it; verify `INFO replication` shows `role:slave`).
4. Quorum-loss scenario (2/3 sentinels unreachable): Redis stays up but cannot fail over — restore sentinel processes before touching the data tier; a master failure during quorum loss requires manual promote + client repoint (record as dual-control op).
5. Post-failover audit: `system:degradation:*` unchanged, `engine:leader:*` epochs consistent, no `reconciliation_mismatches_total` delta, session continuity sample (`auth/session_store.go` keys).

## Escalation

- P1 → platform on-call. Escalate to P0 if the failover itself causes split-brain writes (two masters accepting writes — detectable via diverging `engine:leader`/`circuit_breaker` state) or Sentinel cannot reach quorum within 5min.
- Drill evidence: failover timings feed the quarterly DR report ([dr-drill.md](./dr-drill.md)) — capture promotion elapsed + client reconnect time.
