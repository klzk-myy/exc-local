# Runbook: `L2RejectionSpike` / `ErrorRateAnomaly` — transaction-layer rejection surge

**Severity:** P2 (`pagerduty-p2-ticket` + `#exchange-ops`) · **Rules:** `L2RejectionSpike` — L2 rejection ratio >5% of requests over 1m with ≥100 req/min floor (`L2SpikeWindow`, `L2SpikeRatio`, `L2SpikeMinRequests` in `rules.go`); `ErrorRateAnomaly` — 5m error ratio >4× the 1h baseline for 10m · **Domain:** spec §2.7.2 L2 — transaction/state-boundary errors (atomic rejection, zero side-effects).

## Symptom

More than 5% of a service's requests are being rejected at L2 (margin shortfall, negative-balance breach, counterparty credit exhaustion, STP self-match, cross-shard 2PC timeout), or total error rate jumped >4× baseline. Clients see structured RFC 7807 envelopes with the rejecting code (§8.7); no state mutation occurs per rejection.

## Diagnosis

1. Break down `exchange_errors_total{tier="L2"}` by `service` and error `code` label — the top code is the investigation target:
   - `MARGIN_*` / credit codes → Risk Coordinator path (`services/internal/risk/`) and `shard_margin_reservations` (spec §5.35) for stuck 2PC reservations (5s timeout / max 10 concurrent — Phase-02 Task 2.3.14).
   - `STP_*` self-match codes → check `execution_rules_stp_groups` (migration 072) group config for the affected account.
   - `SERVICE_DEGRADED` (503) → gateway circuit breaker open: upstream error rate >15% over 10s (Task 5.3.29).
2. Check whether the spike is client-concentrated: `http_requests_total` by route/account labels. A single abusive client → L3 handling (rate-limit tier, `ip_bans` — Task 5.3.34 endpoints `GET/PUT/DELETE /api/v1/admin/ip-bans/{ip}`), not a platform incident.
3. `ErrorRateAnomaly` alone (no L2 spike): check `tier="L3"` edge rejections — malformed JSON/SBE, HMAC mismatch, replay outside 30s window — for a broken client release or API change (deprecation registry: `api_deprecations` table, migration 182).
4. Look for an upstream L1 cause in the same window — L2 spikes are frequently downstream of degradation (e.g., 2PC timeouts while a shard is in `ReadOnly`).

## Mitigation

1. L2 rejections are already safe (atomic, rolled back, compensated) — the goal is restoring acceptance rate, not state repair. Verify `reconciliation_mismatches_total` is flat before anything else; a nonzero reconciliation counter changes this to [reconciliation-mismatch.md](./reconciliation-mismatch.md) (P1).
2. Client-caused: engage Support Agent for the offending account; apply/verify rate-limit tier and IP-ban escalation per §8.3/§2.7.2 L3 progressive ban.
3. Reservation-caused: drain stuck `shard_margin_reservations` rows older than the 5s 2PC timeout; if the coordinator is backing up, check `risk-coordinator` — pending Phase-19 (service not yet in `services/cmd/`); today inspect the table directly.
4. Regression-caused (spike correlates with a deploy): roll back the offending service per the blue-green procedure [`../ops/blue-green-deploy.md`](../ops/blue-green-deploy.md) (Task 9.3.3); record `DEPLOYMENT_AUTOMATED_ROLLBACK`-equivalent action in `admin_audit_log`.
5. If the anomaly is rejection-*shape* change (new code appearing), confirm the route registry + error registry are consistent: regenerate `docs/openapi/openapi.json` via `services/cmd/openapi-gen` and diff.

## Escalation

- P2: ticket + Slack. Escalate to P1 if rejection ratio exceeds 25% for 15m, touches >1 service, or coexists with an L1 event.
- Recurring L2 spikes on the same code across two incidents → reliability backlog item via monthly SLO review ([../ops/slo-policy.md](../ops/slo-policy.md)).
