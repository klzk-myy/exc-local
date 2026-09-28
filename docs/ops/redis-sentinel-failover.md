# Redis Sentinel Failover — Runbook & Drill (Task 9.3.20)

**Spec:** §4.5 (topology/quorum/durability), §4.6 (client failover), §18.3 (RPO ≤5s, RTO ≤30s) · **Artifacts:** `deploy/sentinel/*`, `deploy/crons/redis-failover-drill.sh`, `deploy/monitoring/redis-sentinel-alerts.yml`.

---

## 1. Healthy state

| Check | Command | Expected |
|---|---|---|
| Quorum | `deploy/sentinel/health-probe.sh` | 3 sentinels up, quorum=2 met |
| Master | `redis-cli -h <sentinel> -p 26379 SENTINEL get-master-addr-by-name mymaster` | one addr, all sentinels agree |
| Replicas | `redis-cli -h <master> INFO replication` | `connected_slaves:2`, `state=online`, lag ≤5s |
| Failovers | `SENTINEL master mymaster` → `num-other-sentinels`/`failover-in-progress` | quorum ok, none in flight |

Nagios-style probe (Cron/systemd timer, 30s):
`deploy/sentinel/health-probe.sh --textfile /var/lib/node_exporter/textfile_collector`

## 2. Scheduled failover drill (staging canary + quarterly prod)

```sh
# graceful promotion + full timing measurement:
REDISCLI_AUTH=*** deploy/crons/redis-failover-drill.sh --json /var/log/exchange/drill-$(date +%F).json
# restore original master afterwards:
REDISCLI_AUTH=*** deploy/crons/redis-failover-drill.sh --restore
```

Verdict targets (script enforces): master detect ≤3s (§4.5), write RTO ≤30s,
RPO proxy ≤5s (write-gap × probe cadence). Report lands in the drill JSON —
attach it to the quarterly DR report (Task 9.3.21).

Client-side expectations during the drill (spec §4.6): Go `FailoverClient`
reconnects <100ms; rate limiting falls back to in-memory token buckets;
session validation trusts unexpired JWT TTL; the C++ engine only loses
leader-lease refresh for the failover window (TTL 2000ms — a >2s partition
is a *leader demotion*, not just a blip; expect `exchange-watchdogd` /
sentinel promotion interplay — see §4 below).

## 3. Failure scenarios

### master-down (unplanned)
1. Sentinel detects `+sdown` at down-after=2s, quorum votes, `+failover`
   promotes a replica (total 1–3s).
2. If promotion stalls >5s: `SENTINEL failover mymaster` (force).
3. Confirm clients re-resolved: gateway `/health/ready` back to 200.
4. Repaired node rejoins as replica — verify `SENTINEL replicas mymaster`
   shows it `online` before closing the incident.

### quorum-loss
Two sentinels unreachable → **failover is impossible by design**. The master
keeps serving; restores quorum first (restart sentinel units
`redis-sentinel@N`, check inter-AZ links). Do NOT drop quorum to 1 — a lone
sentinel can split-brain the fleet.

### split-brain
After partition heal, two masters may both claim writes briefly.
`min-replicas-to-write 1` + `min-replicas-max-lag 5` bounds the window: the
isolated primary refuses writes once replicas fall out of the 5s lag window.
Fence: `CONFIG SET slaveof <true-master>` on the stale node, then
`SENTINEL reset mymaster` if sentinels cached the wrong topology.

### failover-stuck
`SENTINEL master mymaster` shows `failover-in-progress` for >`failover-timeout`
(5s): check `SENTINEL replicas` for a promotable candidate; if none is
`online`, restore connectivity to the primary instead — promotion of a stale
replica violates the RPO ≤5s contract.

## 4. Interaction with engine leader leases

The matching engine holds `engine:leader:{shard}` with a 2000ms TTL
(§18.6.2). A Sentinel failover >TTL forces every shard through
leader re-acquisition on reconnect — expected and safe (fencing tokens in
`engine:leader:epoch:{shard}` prevent dual-writer). If a failover drill shows
leases flapping *before* the failover, `leader:heartbeat:{shard}` was already
stale — escalate to P1 per §19.13.3 Tier-3 rules.
