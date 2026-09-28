# Runbook: `L1ErrorsSustained` — systemic degradation >30s

**Severity:** P1 (pages `pagerduty-p1`, repeat 1h) · **Rule:** `rate(exchange_errors_total{tier="L1"}[1m]) > 0` for 30s; in-process `l1_errors_sustained` (sustain window `L1SustainFor = 30s`, `services/internal/observability/rules.go`) → `ops.alerts.monitoring` · **Domain:** spec §2.7.2 L1 — systemic/infrastructure degradation.

## Symptom

Sustained L1 error rate on a service for >30s. L1 triggers per §2.7.2: Redis Sentinel failover, IPC ring buffer >80% watermark, PostgreSQL replica lag >5s, Price Oracle staleness >5s. Expected accompaniment: a degradation-mode transition (`Normal` → `ReadOnly`/`Throttled`/`MarketDataOnly`) within 500ms (§2.7.3) and/or `DegradationModeActive` firing.

## Diagnosis

1. Read current mode + reason: `GET /health` → `X-Degradation-Mode` header on any gateway response; authoritative record is Redis `system:degradation:mode` / `system:degradation:reason` / `system:degradation:entered_at` (`services/internal/redis/client.go` `GetDegradationMode`).
2. Identify which L1 trigger is live, in order of blast radius:
   - IPC ring pressure: `engine_ipc_ring_depth` — see [ipc-ring-pressure.md](./ipc-ring-pressure.md).
   - Redis Sentinel event: Sentinel failover counter / `redis-sentinel-failover.md`.
   - PostgreSQL replica lag >5s: `postgres-failover.md` diagnosis §2.
   - Oracle staleness: `PRICE_ORACLE_UNAVAILABLE` path — service pending Phase-19.5; check `oracle` feed metrics when deployed.
3. Correlate on the `system-overview` Grafana dashboard: mode transition timestamp vs first L1 increment.
4. Confirm `AlertDispatchErrors` is NOT also firing — if it is, treat monitoring as degraded first ([alert-dispatch-errors.md](./alert-dispatch-errors.md)).

## Mitigation

1. The mode transition is the mitigation — verify it engaged rather than overriding it. Mode semantics (§2.4): `ReadOnly` rejects orders with `DEGRADED_MODE` (reads continue); `MarketDataOnly` blocks all trading; `Throttled` halves rate limits with institutional > standard > basic priority.
2. Fix the underlying trigger per its own runbook (links in Diagnosis §2). Do not force `Normal` while the trigger persists — hysteresis requires **30 consecutive seconds of healthy telemetry** across all components before `Normal` returns (§2.7.3, ModeManager `core/include/degradation/ModeManager.hpp`).
3. If a manual mode change is genuinely required (e.g., clear `Maintenance` after a deploy): write via `Client.SetDegradationMode` (coordination Redis, `system:degradation:*`) — the spec's admin re-enable path `/admin/maintenance/disable` is **pending route registration**; today the Redis record is the control surface. Log the manual transition with reason in `admin_audit_log` (migration 010).
4. Watch for flap: any transition inside the 30s healthy window is itself an incident signal (T4 tabletop scenario).

## Escalation

- On-call SRE + Component Lead (P1 tree, [incident-escalation.md](./incident-escalation.md)). Canonical SLA: 30min acknowledge / 4h mitigate (§19.8); internal stretch <15m.
- If the L1 source cannot be identified within 30min or mode escalates toward `MarketDataOnly`, re-grade toward P0 handling.
- Post-mortem within 48h if client-facing impact occurred.
