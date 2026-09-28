# Incident Post-Mortem — INC-YYYYMMDD-NN

**Phase-09 Task 9.3.9** · Required within **48h** of any P0 or any P1 with client impact (spec §19.8) · File completed post-mortems at `docs/incidents/INC-YYYYMMDD-NN-slug.md` (create `docs/incidents/` on first use) · Blameless: describe systems and decisions, not people.

> Note: the phase task cites `docs/templates/post-mortem.md`; this document is the canonical template, kept under `docs/ops/` with the rest of the operational doc set.

| Field | Value |
|---|---|
| Incident ID | INC-YYYYMMDD-NN |
| Severity | P0 / P1 / P2 |
| Error tier(s) | L0 / L1 / L2 / L3 (§2.7.2) |
| Status | Draft / In review / Closed |
| Commander | name |
| Author(s) | name(s) |
| Degradation modes traversed | e.g. Normal → MarketDataOnly → Normal |
| DORA-reportable | yes/no — if yes link the ICT incident record ([dora-incident-reporting.md](./dora-incident-reporting.md)) |

## 1. Summary

Two–four sentences: what happened, what broke, how it ended. Include the single most important contributing cause.

## 2. Impact

| Dimension | Value |
|---|---|
| Client-visible impact | orders rejected (count, codes e.g. `DEGRADED_MODE`/`CAPACITY_EXCEEDED`), fills affected, sessions dropped |
| Duration | detection → mitigation → full recovery timestamps |
| Blast radius | services/shards/accounts/instruments affected |
| Money movement | fills lost/duplicated (must be 0 or explain), settlement delay, ledger divergence |
| SLO burn | % of 30-day availability budget consumed (per [slo-policy.md](./slo-policy.md)) |
| Data loss | actual vs RPO bound (§18.3) |

## 3. Timeline (UTC, one row per fact)

| Time | Event | Source |
|---|---|---|
| 00:00:00 | e.g. `IPCRingCritical` fires on shard 1 | alertmanager / `ops.alerts.monitoring` |
| 00:00:05 | p0 page acknowledged | PagerDuty |
| 00:02:10 | engine:leader:{shard} epoch N→N+1 | coordination Redis |
| 00:03:40 | `recovery_reports` id=X written (`WAL_RECOVERY_HALT`) | migration 065 table |

Include detection lag explicitly (first symptom → first alert → first human).

## 4. Root cause

The mechanism-level explanation. Reference the concrete artifact: file/function (`core/src/matching/EngineLoop.cpp::spin_once`), config (`config/aeron-low-latency.properties`), migration (e.g. 065), or runbook gap. Distinguish *trigger* from *latent defect* from *process failure*.

## 5. Contributing factors

Secondary conditions that made the incident possible or worse (missing alert rule, runbook gap, deploy window, capacity at edge).

## 6. What went well

Detection/mitigation steps that worked — name them (e.g. "fencing epoch self-terminated the stale primary as designed").

## 7. Action items

| # | Action | Owner | Due | Verification |
|---|---|---|---|---|
| 1 | e.g. hysteresis fix for ≥95% halt latch (perf report §4.3) | Core | date | tests/chaos s1 green + no latch in soak |

Every action item needs a verifiable done-state (test, rule, migration, dashboard). "Be more careful" is not an action item.

## 8. Lessons learned

- For the on-call rotation (runbook edits → [../runbooks/README.md](../runbooks/README.md) §7 change control).
- For the architecture (spec §27 note candidates — design divergences get recorded, not silently kept).

## 9. Closure checklist

- [ ] Action items ticketed with owners/dates
- [ ] Runbook updates merged (if any step was wrong/missing)
- [ ] Alert rule added/tuned if detection lagged (Phase-13 Task 13.3.3 backlog)
- [ ] DORA incident record closed (initial/intermediate/final reports) if reportable
- [ ] SLO budget impact logged in monthly review
- [ ] If BCP was invoked: post-incident review scheduled ≤10 business days ([post-incident-review-template.md](./post-incident-review-template.md))
