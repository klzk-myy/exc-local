# Runbook: `BridgeBufferDepthHigh` — Aeron→NATS bridge backlog

**Severity:** P1 (page) · **Rule:** `bridge_buffer_depth > 10000` or `bridge_health_buffer_depth > 10000`; in-process `bridge_buffer_depth` (`BridgeBufferDepthAlert = 10000`) → `ops.alerts.monitoring` · **Domain:** spec §2.3.1 cold-path fan-out; buffer cap 100k events; disk spool `/var/spool/exchange/nats_spool/` >5GB trips `ReadOnly`.

## Symptom

A per-shard bridge (`services/cmd/bridge`, one instance per engine shard, colocated on the shard host) holds >10,000 events awaiting JetStream publish acknowledgment. Engine events are not reaching the NATS backbone at line rate — downstream consumers (`settlement`, `compliance`, `analytics`) will lag.

## Diagnosis

1. Is JetStream the cause? `natsctl health` — check `MissingStream` (run `natsctl init` if any of the 7 canonical streams are absent) and consumer pending. Ack timeout is 2,000ms with 3 exponential retries (100/300/900ms) before spool divert (§2.7.3.2).
2. Check spool state on the bridge host: `/var/spool/exchange/nats_spool/` size — approaching 5GB is the `ReadOnly` trip line (§2.7.3.2); verify `X-Degradation-Mode` did not already flip.
3. Is the bridge itself stalled? `bridge_heartbeat_age_seconds` (>15s ⇒ [bridge-heartbeat-stale.md](./bridge-heartbeat-stale.md)), CPU/GC on the bridge process, `aeron_subscription_connected`.
4. Measure backlog trend on `ipc-backbone` Grafana dashboard: depth rising vs plateauing. Rising ⇒ capacity problem; plateau at >10k with JetStream healthy ⇒ publish-path defect.

## Mitigation

1. JetStream degraded → restore NATS first ([alert-dispatch-errors.md](./alert-dispatch-errors.md) §Mitigation covers cluster recovery). Once healthy, the bridge drains its buffer/spool automatically — do **not** restart the bridge mid-drain (restart loses in-memory backlog ordering; spool on disk is the durable copy).
2. Spool near 5GB and still rising: the trip to `ReadOnly` is *correct fail-closed behavior* — let it trip rather than delete spool files. After NATS recovery the bridge re-publishes spooled events; verify `system:degradation:mode` returns `Normal` after the 30s healthy window.
3. Persistent publish failures with healthy JetStream: check for stream `MaxAge` (7d) / `DiscardOld` pressure or subject mis-publish — bridge publishes on `{stream}.{shard}.{symbol}` (e.g. `trades.0.EUR-USD`); inspect via `nats stream info <stream>` (server CLI) or `natsctl health`.
4. If the buffer repeatedly saturates at publish bursts, raise capacity planning via [../ops/capacity-proof.md](../ops/capacity-proof.md) — buffer cap is the spec §2.3.1 100k bound; benchmarked `BenchmarkBufferPush` 39.2ns/op (perf report §6) so the bound is memory-only.

## Escalation

- P1: On-call SRE. Escalate to P0 if spool exceeds 5GB (zero-loss boundary — events beyond the cap are at risk) or the outage crosses a settlement window (T+1 cut — see [../policies/business-continuity-plan.md](../policies/business-continuity-plan.md) comms tree).
- Aftermath: confirm zero event loss — `natsctl health` per-stream state + downstream consumer catch-up; spot-check `trades` stream replay window covers the outage period (7d `StreamMaxAge`).
