# Runbook: Rate-limit utilization ≥80% (`RateLimitUtilizationHigh`)

**Severity:** P2 · **Metric:** `exchange_rate_limit_utilization_over80_total{tier}` (gateway `/metrics`, Task 14.3.6) · **Owner:** Ops

## Symptom

`increase(exchange_rate_limit_utilization_over80_total[10m]) > 0` — at least one identity (account for authenticated tiers, IP for `public`) reached ≥80% of its effective tier request rate inside a 1-second window in the last 10 minutes. The counter fires once per identity per window-second; the label tells you **which tier** is saturating, not which account (per-account labels are rejected by design — cardinality).

## Diagnosis

1. Read the tier label: `public` (anonymous IPs, keyed by IP) vs `basic`/`standard`/`professional`/`institutional` (per-account).
2. Find the identity behind the counter — it is not in the metric; pull it from:
   - gateway logs / request logs around the alert timestamp (the 429s that usually follow saturation carry the identity);
   - the Task 5.3.40 usage view: `GET /api/v1/rate-limits/usage` (admin) or the `rl:usage:<key>` Redis hash — `raw:s:*` / `w:m:*` fields show the charged counters per identity;
   - `RL4xx`/429 rate on the access log — identities above 80% typically tip over into `RATE_LIMIT_TIER_EXCEEDED` within minutes.
3. Distinguish shape:
   - **Steady high utilization on one institutional/standard account** → legitimate growth — the account is about to need a tier move or the tier table needs revisiting;
   - **public (IP) tier saturation** → abuse candidate — check whether the post-429 ban machinery already escalated (`ip_ban:<ip>`, `ip_ban_strikes:<ip>`, `ip_ban_audit` list);
   - **whole tier trending up** → capacity signal, not an incident (feed capacity review, not a page).
4. Check degradation mode first: under `Throttled`, effective limits are multiplied down (DefaultThrottle) — an account at 80% of a *throttled* limit is expected; confirm mode via `exchange_degradation_mode` or Redis `system:degradation:*` before treating it as client behaviour.

## Mitigation

1. **Abuse candidate (public tier, repeated windows):** the progressive-ban machinery (Task 5.3.34) escalates automatically after 429s — verify `ip_ban:<ip>` exists; if a single IP floods without tripping 429 (e.g. weight-heavy endpoints), a manual `BanAdmin.BanIP` or an edge WAF rule is the lever — playbook [../../deploy/edge/ddos-playbook.md](../../deploy/edge/ddos-playbook.md) if it fans out.
2. **Legitimate account nearing exhaustion:** contact the account owner / ops adjusts the tier (`accounts.fee_tier` + tier resolution — Task 5.3.40 introspection shows the effective numbers). Raising a tier is a business decision — do not `CONFIG` around it.
3. **Throttle-mode false positive:** under `Throttled` the threshold shrinks; resolve the degradation cause first ([degradation-mode-active.md](./degradation-mode-active.md)) — the utilization alert clears with it.
4. **Burstiness, not volume:** single-second spikes that never 429 are informational — if the alert repeats daily for the same tier, schedule a weight-table review (Task 5.3.42 weights may be mis-costing hot endpoints).

## Recovery / stand-down

The counter is monotonic — the alert clears as soon as no new identity
crosses 80% in a 10-minute window (`increase([10m])` returns to 0). No
manual reset exists or is needed; a flapping alert means recurring
per-second bursts — treat as the burstiness case in Mitigation §4.

## Escalation

- P2 ticket → ops triage within the business day; recurring public-tier saturation upgrades to a WAF/edge review (SRE).
- If utilization escalates to actual `RATE_LIMIT_TIER_EXCEEDED` errors on institutional accounts, treat as a revenue-impacting ticket — P1 to ops lead; tier changes need Finance Ops sign-off.
- Linked rules: sustained abuse feeding this alert usually co-fires `L2RejectionSpike` (throughput domain) — use that runbook when rejections dominate.
