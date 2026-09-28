# Runbook: `AvailabilityBurnFast` / `AvailabilityBurnSlow` — SLO error-budget burn

**Severity:** `AvailabilityBurnFast` p0 (page immediately); `AvailabilityBurnSlow` ticket class · **Rules:** multi-window burn-rate on the 99.99% availability SLO — fast: error ratio >14.4× budget on both 5m and 1h windows; slow: >6× on 30m and 6h (`deploy/prometheus/rules/exchange-alerts.yml`, `exchange.platform` group) · **Policy:** [../ops/slo-policy.md](../ops/slo-policy.md).

## Symptom

The venue is consuming its 30-day availability budget at ≥6× (slow) or ≥14.4× (fast) the allowed rate. Error ratio numerator is `exchange_errors_total{tier=~"L0|L1|L2"}` over `http_requests_total` — this is the *fleet-wide* availability SLI.

## Diagnosis

1. Find the dominant tier: `sum by (tier) (rate(exchange_errors_total[30m]))` — L0 dominant → [l0-error-observed.md](./l0-error-observed.md); L1 → [l1-errors-sustained.md](./l1-errors-sustained.md); L2 → [l2-rejection-spike.md](./l2-rejection-spike.md).
2. Find the dominant service: `sum by (service) (rate(exchange_errors_total{tier=~"L0|L1|L2"}[30m]))` — the burn alert is fleet-level; the per-service breakdown names the culprit.
3. Check maintenance exclusion: burn during a declared `Maintenance` window is policy-excluded from the SLI ([../ops/slo-policy.md](../ops/slo-policy.md) §SLI definitions) — confirm the window exists in `maintenance_windows` (migration 172; `GET /api/v1/admin/maintenance-windows`) before discounting the alert.
4. Partial-outage attribution: a single AZ/PoP failure shows as burn without a specific service fault — check edge/ingress health (WAF/CDN layer, Task 9.3.13; `deploy/edge/` configs + `deploy/edge/ddos-playbook.md`).

## Mitigation

1. Fast burn (p0): treat as an active incident — follow the dominant-tier runbook. The burn alert is the *aggregate*; the real fix is always in a component runbook.
2. Slow burn (ticket): same decomposition, worked during business hours. Do not snooze: 6× burn exhausts the full 30-day budget in ~5 days.
3. When the fleet is healthy but the SLI is still burning: suspect a measurement gap — `AlertDispatchErrors`, a service missing `/metrics` from `deploy/prometheus/prometheus.yml` job list, or a new route serving 5xx without error-tier labeling.
4. If the 30-day budget is exhausted or trending there: enforce the **error-budget freeze** — non-essential deploys frozen, feature flags dark-launch only, recovery requires 3 consecutive days inside budget ([../ops/slo-policy.md](../ops/slo-policy.md) §Error-Budget Policy).

## Escalation

- Fast: p0 tree ([incident-escalation.md](./incident-escalation.md)) — On-call SRE → Core Eng Lead → VP Eng → CTO.
- Slow: P2 ticket; weekly review at SLO board; escalate if burn factor rises.
- Budget exhaustion → notify Engineering Lead + Product that the freeze is in force; deploy resumes only after the 3-day clean window.
