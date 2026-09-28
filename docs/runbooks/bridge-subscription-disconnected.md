# Runbook: `BridgeSubscriptionDisconnected` — Aeron image detached >60s

**Severity:** P1 (page) · **Rule:** `aeron_subscription_connected == 0` for 60s · **Domain:** bridge-side Aeron subscription — engine events not flowing to NATS even though the bridge process is alive.

## Symptom

The bridge's Aeron subscription image detached for >60s. Distinct from `AeronDriverDown` (driver dead) and `BridgeHeartbeatStale` (process dead): the bridge lives but its egress feed is cut — typically an Aeron channel/session loss or engine-side publication stop.

## Diagnosis

1. Which side detached: check the engine's publication counters (engine metrics / `aeron_publication_*` if exported) vs bridge `aeron_subscription_connected`. Engine publishing but bridge detached → network/IPC session issue; engine not publishing → engine-side fault ([engine-halt-failover.md](./engine-halt-failover.md)).
2. Driver health on both ends: `aeron_driver_up` and CnC sampling (`services/internal/observability/aeronmon.go`, admin service).
3. For `/dev/shm` IPC channels (colocated bridge): check the shm ring header — capacity/slot geometry is adopted from the ring header on attach (`ipc.DefaultRingCapacity`/`DefaultRingSlotPayload`, fixed per perf report §6). A recreated ring with different geometry detaches the old image.
4. For UDP channels: NIC errors, MTU 1408B profile (`config/aeron-low-latency.properties`), conntrack/firewall drops.

## Mitigation

1. Transient network session loss: Aeron re-establishes the image automatically — verify `aeron_subscription_connected` returns 1 within ~2× the session timeout. No action if it self-heals.
2. Stuck detached: restart the bridge (it re-subscribes on boot). If the engine's publication is the dead side, an engine *publication* restart without full engine restart is **pending Phase-02/09 Task 9.3.16** (the binary-swap procedure is the supported path).
3. After reconnection, expect a catch-up burst: monitor `bridge_buffer_depth` and `nats_consumer_pending` until drained.
4. If detach recurs at the same wall-clock times, check for cron/traffic patterns colliding with Aeron `conductor` timeouts in `config/aeron-low-latency.properties` (dedicated threading mode, busy-spin — do not lower timeouts without benchmarking via `deploy/scripts/bench_ipc.sh`).

## Escalation

- P1: On-call SRE + Core Eng Lead if the engine publication side is implicated.
- Recurring detach >3× in 24h → incident review; candidate for permanent channel reconfiguration via Task 9.3.16 deployment window.
