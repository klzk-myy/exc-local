# Redis Sentinel 3-Node HA — Production Configs (Task 9.3.20, spec §4.5)

Production sentinel/data-node configs distinct from the docker-compose dev
profile (`deploy/redis/sentinel.conf` — pinned 10.99.0.x dev topology).

## Topology (spec §4.5, §19.13.1)

| Node | Role | Sentinel | Failure domain |
|---|---|---|---|
| redis-1 | primary (initial) | sentinel-1 :26379 | AZ-a / rack 1 |
| redis-2 | replica | sentinel-2 :26379 | AZ-b / rack 2 |
| redis-3 | replica | sentinel-3 :26379 | AZ-c / rack 3 |

- `quorum = 2` — a single partitioned sentinel cannot trigger failover.
- `down-after-milliseconds 2000` — sub-3s detection per spec §4.5.
- `failover-timeout 5000` — canonical Task 9.3.20 value (the dev compose file
  uses 10000; production is stricter — record in spec §27 if reconciled).
- `parallel-syncs 1` — only one replica resyncs at a time, keeping one full
  reader available during promotion.
- Data nodes: `deploy/redis/redis.conf` (AOF everysec, 512MB backlog,
  `min-replicas-to-write 1` / `min-replicas-max-lag 5` — RPO ≤5s guarantee).

## Auth

`include /etc/redis/sentinel-auth.conf` — provisioned by the secrets pipeline
(Phase-13.5 Task 13.5.3.5, Vault agent → tmpfs). It must contain:

```
sentinel auth-pass mymaster <redis-password>
sentinel auth-user mymaster default
requirepass <sentinel-client-password>
masterauth <redis-password>
```

Never commit the include file.

## Files

| File | Purpose |
|---|---|
| `sentinel-{1,2,3}.conf` | per-node sentinel configs (identical except announce-ip) |
| `health-probe.sh` | quorum/master/replica-lag probe; exits non-zero on degradation; `--textfile` emits Prometheus node-exporter metrics |
| `../crons/redis-failover-drill.sh` | automated failover drill (RPO≤5s / RTO≤30s) |
| `../monitoring/redis-sentinel-alerts.yml` | Prometheus alert rules |
| `../../docs/ops/redis-sentinel-failover.md` | operator runbook + drill procedure |

## Wiring

Go services connect via `EXC_SENTINEL_ADDRS` (go-redis `FailoverClient`,
reconnect <100ms — spec §4.5). The C++ engine uses Sentinel discovery only
for `engine:leader:{shard}` leases and `system:degradation:*` mode flags.
