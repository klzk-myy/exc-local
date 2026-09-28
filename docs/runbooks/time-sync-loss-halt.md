# Runbook: `TIME_SYNC_LOSS_HALT` — PTP clock drift >100µs

**Severity:** P1 (spec §19.4: divergence >100µs → immediate P1 alert; Tier-3 watchdog trips `TIME_SYNC_LOSS_HALT`, HTTP 503) · **Domain:** MiFID II RTS 25 timestamp fidelity — hardware timestamping NICs sync via PTP IEEE 1588v2 to atomic/GPS grandmaster; `ptp4l` + `phc2sys` discipline the PHC and system clock; metric `clock_offset_nanoseconds`.

## Symptom

`clock_offset_nanoseconds` exceeds 100µs on a trading bare-metal node, or `TIME_SYNC_LOSS_HALT` returned/halted. Timestamps on order events are no longer certifiably within RTS 25 tolerance — matching must stop rather than emit untraceable executions.

## Diagnosis

1. Confirm the halt is clock-driven, not coincidence: `clock_offset_nanoseconds` series on `shard-health` dashboard; `ptp4l`/`phc2sys` unit states (`deploy/systemd/ptp4l.service`, `phc2sys.service`; Ansible role `deploy/ansible/roles/ptp/` incl. `ptp-status.sh` probe timer); the `services/internal/timesync` guard (`ptpmon.go`) exports the offset metric — `exchange-watchdogd` Tier-3 sampler pending `services/cmd/watchdogd`.
2. Isolate the fault domain:
   - Grandmaster loss: `ptp4l` shows grandmaster identity change or announce timeouts — check upstream PTP infrastructure, not the host.
   - NIC/PHC fault: hardware timestamping errors in `ptp4l` log; `ethtool -T <if>` capability check.
   - Host clock discipline: `phc2sys` sync failures, NMEA/GPS source flap.
3. Check blast radius: is it one node or the fleet? Fleet-wide offset drift = grandmaster problem; single-node = local NIC/config.
4. Note the system clock was the *fallback*: dev/single-box mode runs software clock fallback (§19.13.4) — confirm the halted node was supposed to be on hardware PTP.

## Mitigation

1. Grandmaster path: restore PTP grandmaster reachability (network/PTP infra team). Offset returns <100µs → `ptp4l` re-locks → host exits halt through the normal ModeManager hysteresis (30s healthy telemetry).
2. Local NIC path: rebind `ptp4l`/`phc2sys` to the surviving PTP-capable NIC or restart the daemons (`systemctl restart ptp4l phc2sys`); verify `clock_offset_nanoseconds` <100µs sustained.
3. Engine resumption: halted node re-enters via the standard ladder — its leader lease expired during the halt; standby may already hold epoch N+1 ([engine-halt-failover.md](./engine-halt-failover.md)). Do not force-promote the drifted node while offset >100µs.
4. Evidence for regulators: preserve `ptp4l` stats + the offset series for the halt window — RTS 25 clock-traceability evidence is an audit artifact, not just ops telemetry.

## Escalation

- P1 → platform/network team owns PTP infra. Escalate to P0 if >1 trading node halts simultaneously (venue-wide timestamping lost) or if the halt occurs during a settlement/fixing window.
- Every `TIME_SYNC_LOSS_HALT` is reportable in the incident register; MiFID II venues must evidence clock governance — file the window + cause in `docs/incidents/` and the DORA register ([../ops/dora-ict-risk-register.md](../ops/dora-ict-risk-register.md)).
