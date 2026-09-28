# Runbook: `CircuitBreakerOpen` — five-tier breaker tripped

**Severity:** P2 (ticket + Slack) · **Rule:** `exchange_circuit_breaker_open == 1` for 2m · **Domain:** spec §2.6 five-tier circuit breaker (Phase-13 owns implementation; Redis `circuit_breaker:{scope}:{id}`; state machine `CLOSED → OPEN(hold) → HALF_OPEN(probe) → CLOSED`).

## Symptom

A circuit breaker labeled `{breaker}` is open on `{service}`. Tiers (§2.6): `INSTRUMENT` (price move >`instrument_price_limit`, default 5%, within 60s → 5min hold), `ACCOUNT` (3+ rapid losses >5% equity in 5min → 30min), `MARKET_WIDE` (aggregate volatility >20% on >2 instruments → manual resume), `OPTIONS_VOLATILITY` (IV spike >200% vs 30d avg → 15min), `VOLUME_SPIKE` (1-min volume z-score ≥4.0σ → 10min).

## Diagnosis

1. Which tier/scope: `breaker` label + Redis `circuit_breaker:{scope}:{id}` record (state, trip time, hold-until, trip reason).
2. `INSTRUMENT`/`MARKET_WIDE`/`VOLUME_SPIKE`: confirm a real market event vs bad reference data — check oracle/mark feeds and the instrument's `instrument_price_limit` (migration 170 instrument filter columns). A breaker tripped by a *stale* price feed is an oracle incident — `PRICE_ORACLE_UNAVAILABLE` path (oracle service pending Phase-19.5).
3. `ACCOUNT`: pull the account's loss events — 3+ >5%-of-equity losses in 5min is either distress or an exploit loop; cross-check with surveillance signals (Phase-17/21 — `surveillance` JetStream stream exists).
4. `OPTIONS_VOLATILITY`: derivatives pending Phase-22 — expect this tier dormant until then.
5. Flapping check: re-trip within 15min of recovery doubles the hold period (max 120min) and alerts Risk Management (Phase-13 Task 13.3.9 flapping penalty, composing with the 60s auto-recovery cooldown in Task 13.3.1).

## Diagnosis — pending Phase-13

The breaker state machine + metrics are Phase-13 deliverables; until `exchange_circuit_breaker_open` exports, detection is via `DEGRADED_MODE`-adjacent signals and instrument state (`SUSPENDED`/`HALTED`) on `GET /api/v1/instruments`. Steps below apply once deployed.

## Mitigation

1. Legitimate trip (real volatility): let the hold run; `HALF_OPEN` probes auto-close after 10/10 probes in 30s for the probe-recoverable tiers.
2. Manual trip needed (admin sees risk the breaker missed): `POST /api/v1/admin/circuit-breaker/{symbol}` — registered route; backend pending Phase-13.
3. Manual close (dual-control sensitive op): `POST /api/v1/admin/circuit-breaker/{symbol}/reset` — registered route; requires dual-control approval per §2.6; record in `admin_audit_log`.
4. `MARKET_WIDE` has no auto-recovery — manual admin resume only. Do not resume while aggregate volatility remains >20%; resume early only with Risk Manager sign-off recorded.
5. Breaker tripped on bad data: fix the feed/oracle first, then reset — resetting into a still-corrupt feed re-trips immediately and burns the penalty window.

## Escalation

- P2 → Risk Manager informed (all `ACCOUNT` and `MARKET_WIDE` trips, and every manual reset).
- Escalate to P1 if `MARKET_WIDE` opens during a live incident or a breaker fails to hold (orders matching through an open breaker = invariant breach → P0 territory, [l0-error-observed.md](./l0-error-observed.md)).
