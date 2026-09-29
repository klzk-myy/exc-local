# Runbook: JetStream/NATS backbone down (`NATSBackboneDisconnected`, `AdminMonitorTelemetryAbsent`)

**Severity:** P1 (connectivity lost) / P2 (admin monitor series absent) · **Metrics:** `nats_connected` (admin NATSMonitor probe), `bridge_health_nats_connected{shard}` (per-shard bridge heartbeat payload) · **Owner:** SRE

## Symptom

The JetStream event backbone is unreachable from the admin monitor's health probe, or one or more bridges report `nats_connected=0` in their `bridge.health.<shard>` heartbeat payload.

## Diagnosis

1. Scope: `nats_connected==0` on the admin job alone = probe/cluster down globally; `bridge_health_nats_connected==0` on one shard only = that bridge's client, not the cluster.
2. Cluster side: NATS 3-node JetStream (`docker-compose.dev.yml` topology mirrors prod) — check `nats-server` health and stream/consumer info via `natsctl` (`services/cmd/natsctl`).
3. Bridge side: `bridge_nats_reconnect_total` climbing while connected==0 = flapping reconnects (auth expiry, intermittent network); flat = hard outage.
4. Consequence check: `bridge_buffer_depth` starts growing — the bridge buffers engine events awaiting publish (§2.3.1 100k in-memory cap, then disk spool >5GB forces ReadOnly).

## Mitigation

1. Cluster outage: restore NATS first; bridges recover on their own reconnect path (heartbeat payload flips `nats_connected` back to 1 and drains the buffer — monitor `bridge_buffer_depth` until zero).
2. Single-bridge outage: restart that bridge; buffered/spooled events replay on reconnect.
3. If the outage outlasted the buffer cap and `bridge_events_dropped_total` >0 → treat as potential event loss per [throughput-collapse.md](./throughput-collapse.md) — downstream consumers must re-baseline from the next snapshot, not the gap.

## telemetry-gap

`AdminMonitorTelemetryAbsent` = `nats_connected` or `bridge_heartbeat_age_seconds` produce no samples for 10m while `up{job="admin"}==1` — the admin service is alive but its NATSMonitor/BridgeHeartbeatWatcher failed to start (boot log: "admin: nats unavailable" / "heartbeat subscribe failed"). Backbone alerting is *inert* in this state — restarting admin restores it; check the NATS connect timeout path in `cmd/admin/main.go`.

## Escalation

- P1 page on connectivity loss. Escalate to P0 if the outage crosses the spool bound (ReadOnly trip) or coincides with engine trouble — the venue is then running without its event backbone *and* impaired matching.
