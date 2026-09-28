# Runbook: `AeronDriverDown` — media driver unreadable/down

**Severity:** P1 (page) · **Rule:** `aeron_driver_up == 0` for 30s · **Domain:** Aeron C media driver (`aeronmd`, spec §19.13.1, systemd `WatchdogSec=1s`).

## Symptom

`cnc.dat` unreadable or missing on the monitored host — engine IPC telemetry lost. The matching engine may still be matching (Aeron driver and engine are separate processes), but all Aeron-mediated fan-out (bridge → JetStream, market data IPC) is blind.

## Diagnosis

1. On the shard host: `systemctl status aeronmd` — unit `deploy/systemd/aeronmd.service`; expect `active`, `Type=notify`, `WatchdogSec=1s`. Systemd restarts it automatically (`Restart=always`, `RestartSec=1s` per Task 9.3.28 unit template).
2. Check `/dev/shm/aeron-{user}`: driver dir exists, `cnc.dat` readable, timestamps advancing. A deleted `/dev/shm` tree means the driver died *and* buffers are gone — engine must re-create or re-attach.
3. Was this a host-level event? Check `/dev/watchdog` trip evidence and `RuntimeWatchdogSec` (`deploy/baremetal/system.conf.d/50-exchange-watchdog.conf` = 10s; spec §19.13.3 says 15s — drift flagged to spec owners) reboot logs; a BMC reset kills driver + engine together → treat as [engine-halt-failover.md](./engine-halt-failover.md).
4. Check what else is down: `matching_engine` process, `recovery` (snapshot service, one per shard), `bridge` instance on the same host.

## Mitigation

1. Driver-only loss (engine alive): restart `aeronmd`; clients re-map term buffers and reconnect per Aeron semantics (§19.13.1: "buffers remapped; client reconnection retry"). Verify `aeron_subscription_connected` recovers on bridges.
2. Driver + engine loss: follow [engine-halt-failover.md](./engine-halt-failover.md) — promotion runs through `recovery-orchestrator`, not manual restart.
3. If `cnc.dat` is corrupt but the driver is up: stop the driver, clear the stale Aeron dir, restart — clients re-attach on the fresh CnC file.
4. After recovery verify the cold path: `bridge_heartbeat_age_seconds` back under 15s on `bridge.health.{shard}` heartbeats, `aeron_subscriber_lag_bytes` trending to 0.

## Escalation

- P1: On-call SRE + Core Eng Lead. Escalate to P0 if the engine is also down AND standby promotion fails (see [engine-halt-failover.md](./engine-halt-failover.md) §Escalation).
- If the driver repeatedly crashes on one host, pull the host from rotation (fleet `DRAINING`/`MAINTENANCE` state — `POST /api/v1/admin/fleet/hosts/{id}/drain`, fleet backend `services/internal/fleet` + migration 091) and preserve `cnc.dat` + core dumps for RCA.
