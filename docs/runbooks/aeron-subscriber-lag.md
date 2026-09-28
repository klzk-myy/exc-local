# Runbook: `AeronSubscriberLag` — IPC subscriber behind publisher

**Severity:** P2 (ticket + Slack) · **Rule:** `aeron_subscriber_lag_bytes > 1000` for 30s (`AeronSubscriberLagAlertBytes = 1000`, Task 7.3.8); in-process `aeron_subscriber_lag` → `ops.alerts.monitoring` · **Domain:** Aeron shared-memory IPC backbone (§2.3.1, §2.7.3).

## Symptom

An Aeron subscriber position lags its publisher by >1000 bytes on the labeled `channel`/`stream_id` for >30s. Consumers are falling behind on engine egress or ingress rings (`/dev/shm` SPSC rings + Aeron publications).

## Diagnosis

1. Which subscriber is lagging: the labels identify the channel/stream. `bridge` instances (`services/cmd/bridge`, `/metrics` on `bridge.metrics_addr`, default 127.0.0.1:9100+shard) export `aeron_subscriber_lag_bytes` — check the bridge dashboard `ipc-backbone`.
2. Inspect the media driver: `aeron_driver_up` and CnC counters via `services/internal/observability/aeronmon.go` (admin service samples `cnc.dat`); if driver is unreadable this becomes [aeron-driver-down.md](./aeron-driver-down.md) (P1).
3. Check consumer-side cause: bridge CPU/GC stall, JetStream publish timeout backlog (`bridge_buffer_depth` — ack timeout 2000ms, 3 retries 100/300/900ms then disk spool per §2.7.3.2), or slow marketdata producer attach.
4. Baseline transport health: `deploy/scripts/bench_ipc.sh` — shm ring p99 ~3.9µs / Aeron p99 ~3.0µs measured (`docs/perf/phase08-tuning-report.md` §4.1). Lag at these numbers is a consumer problem, not transport.

## Mitigation

1. Bridge-side backlog: see [bridge-buffer-depth-high.md](./bridge-buffer-depth-high.md) — let the spool drain; do not restart the bridge while it is catching up (restart forfeits in-memory backlog ordering).
2. Consumer CPU starvation: on bare metal verify engine cores remain isolated (`isolcpus`, Aeron `config/aeron-low-latency.properties` affinity, `scripts/tune-kernel-network.sh`); on K8s check pod CPU throttling for `marketdata`/`bridge`.
3. If a subscriber is wedged (image detached >60s): `BridgeSubscriptionDisconnected` fires — [bridge-subscription-disconnected.md](./bridge-subscription-disconnected.md).
4. Persistent lag with healthy consumer → suspect NAK storms / MTU mismatch: Aeron MTU is 1408B in the low-latency profile; check NIC errors on the shard host.

## Escalation

- P2 ticket. Escalate to P1 if lag exceeds the ring's durable window (data loss imminent), if `bridge_buffer_depth` crosses 10,000, or if `aeron_driver_up == 0`.
- Recurring lag on one channel → capacity backlog via [../ops/capacity-proof.md](../ops/capacity-proof.md).
