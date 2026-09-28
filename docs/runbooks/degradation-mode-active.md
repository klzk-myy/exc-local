# Runbook: `DegradationModeActive` — venue operating in a degraded mode

**Severity:** P2 ticket (P1-equivalent for `MarketDataOnly`/`SpotOnly` per spec §2.4 alerting — treat those two as P1 operationally) · **Rule:** `exchange_degradation_mode{mode!~"Normal|Maintenance"} == 1` for 1m · **Domain:** spec §2.4 degradation state machine; `ModeManager` (`core/include/degradation/ModeManager.hpp`) is the owner; Redis `system:degradation:*` is the record.

## Symptom

A service reports an active mode other than `Normal`/`Maintenance`. Modes (PascalCase, exact): `Normal | ReadOnly | MarketDataOnly | SpotOnly | Throttled | Maintenance`. Clients see `X-Degradation-Mode` on every gateway response and `DEGRADED_MODE`/`MAINTENANCE_MODE`/`CAPACITY_EXCEEDED` rejections; WS gets `system.status` events on transition.

## Diagnosis

1. Read the record: Redis `GET system:degradation:mode`, `system:degradation:reason`, `system:degradation:entered_at` (`Client.GetDegradationMode`, `services/internal/redis/client.go`). The `reason` string names the trigger.
2. Map mode → trigger (§2.4):
   - `ReadOnly` — engine slow (matching-loop p50 >500µs self-probe) or ≥1 shard unhealthy.
   - `MarketDataOnly` — PostgreSQL down or WAL corrupted → [postgres-failover.md](./postgres-failover.md) / [wal-recovery-halt.md](./wal-recovery-halt.md).
   - `SpotOnly` — derivatives engine unhealthy (pending Phase-22 — treat as Phase-22 scope when derivatives ship).
   - `Throttled` — capacity >80% (queue depth >250 for 30s) → rate limits halved; check `ipc-backbone` dashboard.
   - `Maintenance` — planned; verify against `maintenance_windows` (migration 172, `GET /api/v1/admin/maintenance-windows`). The alert excludes `Maintenance` already; if you are here manually, check it is a *declared* window.
3. Gateway gate behavior check: `services/internal/middleware/degradation.go` — under `MarketDataOnly` only book/trades/ticker/klines/instruments/exchange-info/time/fees + WS + health stay open; under `Maintenance` only `/health`,`/ready`,`/metrics`. Verify clients are seeing the right rejections (a gate that admits writes under `MarketDataOnly` is a P1 defect).
4. Fail-closed read: if `system:degradation:mode` cannot be read, the gateway reports `Maintenance` (strictest-safe). A venue *reporting* Maintenance with no declared window = Redis coordination read failure → [redis-sentinel-failover.md](./redis-sentinel-failover.md).

## Mitigation

1. Fix the trigger, not the mode. The mode is correct fail-closed behavior; forcing `Normal` early is the anti-pattern the 30s hysteresis exists to prevent.
2. Recovery criteria (§2.4): `ReadOnly` auto-recovers when engine p50 <100µs for 30 consecutive seconds; `MarketDataOnly` after PG failover + WAL verify; `Throttled` when capacity <60% for 5min; `Maintenance` manually cleared after deploy.
3. Manual transition when justified: write `system:degradation:mode` (+reason+entered_at) via `Client.SetDegradationMode` or `redis-cli` on the coordination Redis; record actor+reason in `admin_audit_log`. Mode writes bypassing `ModeManager` are for `Maintenance` enter/exit and documented emergencies only.
4. Verify gate + header after any manual change: `curl -sI <gateway>/health | grep -i x-degradation-mode`.

## Escalation

- P2 for `ReadOnly`/`Throttled`; **P1 for `MarketDataOnly`/`SpotOnly`** (§2.4 alerting table). Escalate to P0 if `MarketDataOnly` exceeds 1h (client-money read/write impaired) or mode oscillates.
- Status-page entry required for any mode ≠ `Normal` visible to clients — the Task 9.3.25 aggregator (`services/internal/ops/status_exporter.go`) already opens an `ops_incidents` row on non-`Normal` mode activation; verify `status:current`/`GET /api/v1/system/status` reflects it, and post to `#exchange-ops` + support macro for direct notice.
