# DORA ICT Incident Classification & Regulator Reporting Workflow

**Phase-09 Task 9.3.15 (items 2 & 5)** · **Authority:** spec §19.5, §19.8 (comms + DORA clocks), §24 #171 · **Report template:** [ict-incident-template.md](./ict-incident-template.md) · **Backend workflow:** `services/internal/operations/dora/` + `regulatory_submissions` (migration — table in §5.36; reporter service pending Phase-21)

## 1. Classification

An operational event becomes a DORA ICT incident record when it meets the platform's own severity bar (P0–P2, [incident-escalation.md](../runbooks/incident-escalation.md)) — then it is assessed for **major** (regulator-reportable) status on the DORA criteria:

| Criterion | What to capture |
|---|---|
| Impact | services/functions affected, downtime, transactions/orders affected (count) |
| Clients & counterparties | number/segments affected, client-money exposure |
| Duration | detect → mitigate → restore (against §18.3 RTOs) |
| Geography | regions/AZs, cross-border reach |
| Data loss | records lost vs RPO bound; integrity vs availability |
| Critical services | whether matching/settlement/clearing-adjacent functions were hit (ties to [risk register](./dora-ict-risk-register.md) critical functions) |
| Costs | recovery cost, client remediation, regulatory exposure estimate (BCP financial table [../policies/business-continuity-plan.md](../policies/business-continuity-plan.md) §4) |

**Major** = trading halted, client money at risk, data loss/corruption, or cross-service outage exceeding RTO — in practice: all P0s and material P1s (§19.8).

## 2. Reporting timeline (DORA major-ICT-incident phases)

Clocks start at **classification as major**, not at incident start — record the classification timestamp explicitly:

| Report | Deadline | Content | Owner |
|---|---|---|---|
| Initial notification | **≤ 4h** | what happened, scope, immediate measures, whether clients/counterparties affected | Incident commander → Compliance Officer |
| Intermediate report | **≤ 72h** | updated impact, root-cause progress, actions taken, evolving risk | Compliance Officer |
| Final report | **≤ 1 month** | full root cause, impact final, lessons + remediation plan, cost, threat actor/vector if security | Compliance Officer + CTO sign-off |

MiFID II material-incident deadlines run in parallel per venue obligations — same record feeds both; reconcile clocks with Compliance at incident open. **Delays are logged, not rationalised** (§19.11.1.4): if a deadline will be missed, record the miss + reason and notify proactively.

## 3. Workflow states

`OPEN → CLASSIFIED (major?) → INITIAL_SENT → INTERMEDIATE_SENT → FINAL_SENT → CLOSED`

- Record-of-truth while `services/internal/operations/dora/` is pending: the incident doc in `docs/incidents/` + this workflow's checklist in the ticket + evidence export to `regulatory_submissions` (§5.36 — table exists in schema index; reporter backend Phase-21).
- Concurrent reportable incidents: each gets its own record and clocks — never merge reports across incidents.
- Deadline change by regulator: record the change + source; recompute due times; alert Compliance.

## 4. Material-incident closure gate (Task 9.3.15 item 5)

A material incident **cannot close** until ALL of:

- [ ] Initial + intermediate + final regulator reports completed (or formally waived by Compliance with reason)
- [ ] Root cause established (post-mortem §4, [postmortem-template.md](./postmortem-template.md))
- [ ] Lessons-learned actions each have owner + due date
- [ ] Control remediation accepted by Risk/board delegate (named acceptor + date)
- [ ] Evidence export sealed — immutable bundle: incident doc, alert timeline, `recovery_reports`/digests, ledger/reconciliation evidence, comms log. Export mechanism pending Task 9.3.15 backend; interim: WORM bucket upload + SHA256 manifest (same convention as Task 9.3.17 WORM archives).

## 5. Evidence & resilience-testing linkage

- Annual resilience-test program (vulnerability scans, scenario/chaos `tests/chaos/`, backup restore `deploy/postgres/pitr_smoke.sh` + `deploy/clickhouse/` drill, failover [dr-drill.md](../runbooks/dr-drill.md), capacity [capacity-proof.md](./capacity-proof.md), physical/network failure, crisis comms) produces evidence retained for DORA — drill reports file under `docs/incidents/drills/`.
- Risk-based TLPT (threat-led penetration testing) schedule: Phase-13.5 pentest scope §19.15 — results + remediation evidence join this register.
