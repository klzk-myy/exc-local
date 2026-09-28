# DORA ICT Incident Record — <INC-id>

**Template for the regulator-facing ICT incident record** (Phase-09 Task 9.3.15; the task cites `docs/templates/ict-incident.md` — canonical copy lives here under `docs/ops/`). Pair with the internal post-mortem ([postmortem-template.md](./postmortem-template.md)); this record tracks the *regulatory* workflow.

| Field | Value |
|---|---|
| Incident ID | INC-YYYYMMDD-NN |
| Opened (UTC) | |
| Classified major (UTC) | — clock start |
| Severity (internal) | P0 / P1 |
| Reportable regimes | DORA / MiFID II material incident / other: |
| Initial report due (+4h) | |
| Intermediate due (+72h) | |
| Final due (+1 month) | |
| Compliance owner | |

## 1. Classification assessment

| Criterion | Assessment |
|---|---|
| Services/functions affected | |
| Downtime (detect→restore) vs RTO | |
| Transactions/orders affected (count, codes) | |
| Clients & counterparties (count/segment) | |
| Client-money exposure | |
| Geographic reach | |
| Data loss vs RPO bound | |
| Critical-service impact (risk register link) | |
| Estimated costs | |

Major determination: **YES / NO** — rationale:

## 2. Report log

| Report | Due | Sent | Delivered to | Evidence of delivery |
|---|---|---|---|---|
| Initial | | | | |
| Intermediate | | | | |
| Final | | | | |

Missed/late deadlines — reason + proactive-notification evidence:

## 3. Root cause & remediation

- Root cause (link post-mortem §4):
- Lessons-learned actions (owner/due):
- Control remediation — accepted by (name/role, date):

## 4. Closure gate

- [ ] All three reports sent (or formally waived by Compliance — record waiver reason)
- [ ] Post-mortem closed
- [ ] Remediation items accepted + dated
- [ ] Immutable evidence bundle exported (WORM + SHA256 manifest path):
- [ ] Risk register updated if the incident revealed a new treatment ([dora-ict-risk-register.md](./dora-ict-risk-register.md))
