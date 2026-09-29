# Runbook: funding money-path errors (`FundingMoneyPath5xx`, `FundingRejectionStorm`, `FundingRouteP99Slow`, `FundingAdminOpsErrors`)

**Severity:** P1 (5xx on money paths) / P2 (rejection storm, latency) / P3 (admin ops errors) · **Metric:** `http_requests_total{service="gateway",route,code}` (route label = mux pattern) · **Owner:** SRE + Finance Ops

## Symptom

Deposit/withdrawal/transfer endpoints (`/api/v1/deposits`, `/api/v1/withdrawals`, `/api/v1/funding/*`, `/api/v1/transfers`) are erroring, rejecting above 10%, or slow above 1s p99.

## Diagnosis

### five-xx

1. Which sub-path: `sum by (route) (rate(http_requests_total{service="gateway",route=~"/api/v1/(deposits|withdrawals|funding|transfers).*",code=~"5.."}[5m]))` — deposits failing vs withdrawals failing have different blast radius.
2. Dependencies under the handlers: Postgres (funding_transactions, migration 007/199), the beneficiary/whitelist registry (`/api/v1/funding/withdrawal-whitelist`), bank-rail adapters (Phase-11: SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2), and the ledger (`journal_sums` — settlement service).
3. Confirm scope: withdrawal confirm window is 15min (`withdrawal_confirmations.expires_at`, migration 008) — 5xx on `/withdrawals/{id}/confirm` can strand user funds mid-flow even at low rates.

### rejection-storm

>10% 4xx on money paths with ≥20 req/5m. Common causes: idempotency-key replays (409), expired withdrawal confirmations, KYC-tier/limit rejections, or a probing client. Sample the offending IPs in the gateway access log; check `exchange_errors_total{tier="L3"}` for the same window — edge rejections (418/429) mean a client is being rate-banned mid-flow.

### latency

p99 >1s on funding routes: these handlers legitimately call bank rails and beneficiary registries, so the bound is looser than the 5ms core SLO — but >1s means a downstream hang. Confirm which route; check whether `PENDING_REVIEW` rows are accumulating in `funding_transactions` (>$50K tier, 4h review SLA).

### admin-ops

`/api/v1/admin/funding/*` 5xx — Finance Ops cannot clear review queues (deposit reviews, nostro replenishment decisions, quarantine resolution). Low client impact but SLA-relevant: pending-review withdrawals age against the 4h tier SLA while ops are blocked.

## Mitigation

1. Money-path 5xx: stop the bleeding at the dependency — if a bank rail is down, mark the rail degraded rather than letting every withdrawal attempt 500.
2. Rejection storm from one client: verify it is not a legitimate retry loop before banning — the withdrawal path rejects by design (confirm expiry, whitelist enforcement); banning a confused client is a support incident, banning a prober is security work.
3. After any funding incident: reconcile `funding_transactions` status distribution (`PENDING`/`PENDING_REVIEW` ageing) before closing the incident — half-written funding rows are the quiet failure mode.

## Escalation

- `FundingMoneyPath5xx` is P1 (client money flows failing). Rejection storm and latency are P2 — escalate to P1 if either persists >30m or coincides with a rail/provider outage.
