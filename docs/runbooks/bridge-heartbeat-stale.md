# Runbook: `BridgeHeartbeatStale` — bridge (or its shard) silent >15s

**Severity:** P1 (page) · **Rule:** `bridge_heartbeat_age_seconds > 15`; in-process `bridge_heartbeat_stale` (`BridgeHeartbeatStaleAfter = 15s` = 3× the 5s heartbeat cadence, matching the §2.7.3 fencing convention) → `ops.alerts.monitoring` · **Domain:** bridge liveness on subject `bridge.health.{shard}`.

## Symptom

No `bridge.health.{shard}` heartbeat for >15s (3+ missed beats). Either the bridge process is down, or — worse — the shard it monitors is down and the heartbeat died with it.

## Diagnosis

1. Disambiguate bridge-down vs shard-down (the alert description warns both look alike):
   - Engine alive? Check `matching_engine` process on the shard host, `engine:leader:{shardId}` lease refreshing (500ms cadence) in coordination Redis, and shard metrics on the `shard-health` Grafana dashboard.
   - Bridge process alive? `services/cmd/bridge` on the shard host; `/metrics` + `/healthz` on `bridge.metrics_addr` (default `127.0.0.1:9100`+shard).
2. If the shard is down → this is an engine incident: go to [engine-halt-failover.md](./engine-halt-failover.md) immediately.
3. If only the bridge is down: check `aeron_driver_up` ([aeron-driver-down.md](./aeron-driver-down.md)), OOM/restart loop on the bridge, and its last log lines (JetStream publish timeout storm, Aeron image detach).

## Mitigation

1. Bridge-only loss: restart the bridge. On restart it re-attaches to the shm ring, re-subscribes, and resumes heartbeat cadence — buffered engine events during the gap live in the bridge's own buffer semantics only if the process stayed up; a dead bridge means the engine egress ring accumulated (check `aeron_subscriber_lag_bytes` after restart for the catch-up burst).
2. Verify heartbeat resumed: `bridge_heartbeat_age_seconds` < 15s, `bridge.health.{shard}` messages flowing (inspect with a NATS subscribe on the subject).
3. Repeated bridge death on one shard → preserve logs + core, then run the failover-side drill logic from [dr-drill.md](./dr-drill.md) bridge section; likely Aeron dir corruption — see [aeron-driver-down.md](./aeron-driver-down.md) §Mitigation 3.
4. If the heartbeat subject itself is the problem (bridge up, heartbeats absent): check `ops.alerts.monitoring` consumers and `admin` service NATS monitor (`services/cmd/admin` hosts the heartbeat watcher).

## Escalation

- P1: On-call SRE. If the shard proves down and standby promotion stalls, escalate to P0 per [engine-halt-failover.md](./engine-halt-failover.md).
- Silent >60s during active trading → treat market data + settlement feeds for that shard as stale; warn Finance Ops that T+1 settlement inputs may be delayed.
