# Post-Incident Review (BCP) — <INC-id>

**Phase-09 Task 9.3.27 item 6** — mandatory within **10 business days of recovery** for any incident that invoked the BCP decision framework, a go-forward mode, or a disaster declaration (the task cites `docs/templates/post-incident-review.md` — canonical copy lives here under `docs/ops/`). This is the *plan-level* review; the technical post-mortem ([postmortem-template.md](./postmortem-template.md)) is a prerequisite input.

| Field | Value |
|---|---|
| Incident ID | |
| Recovery date | |
| Review date (≤10 business days) | |
| Decision-maker + quorum members | |
| Modes invoked | stand-down / GF-1 manual capture / GF-2 withdrawal-only / GF-3 frozen |

## 1. Decision review

- Was the four-criteria assessment (client-money / oracle / reconcile-ability / harm-vs-delay) scored correctly with the information available at the time?
- Did the quorum convene within target (≤30min)? Any unreachable-quorum sole-member calls — were they ratified ≤24h?
- Was the chosen mode the right one in hindsight?

## 2. Execution review

| Step | Planned | Actual | Gap |
|---|---|---|---|
| Mode entry conditions | | | |
| Notification tree (internal + regulator + clients) | | | |
| Alternate-site activation (if used) | | | |
| Evidence preservation | | | |
| Mode exit / resumption ladder | | | |

## 3. Financial-impact actuals vs standing estimate

Compare BCP §4 estimate by severity against observed: client money at risk, insurance-fund draw (`insurance_fund`, migration 016), contingent-capital draw (§17.13.1), revenue loss, regulatory exposure.

## 4. Plan updates (mandatory before close)

| Update | Document | Owner | Merged |
|---|---|---|---|
| e.g. GF-2 needs faster nostro read access | [../runbooks/bcp-goforward.md](../runbooks/bcp-goforward.md) | | |
| BCP criteria/contact/mode changes | [../policies/business-continuity-plan.md](../policies/business-continuity-plan.md) | | |
| Runbook changes | `docs/runbooks/` | | |
| Risk-register rows | [dora-ict-risk-register.md](./dora-ict-risk-register.md) | | |

## 5. Closure

- [ ] BCP document updated and re-versioned (controlled document, same change control as phase plans)
- [ ] Runbook set updated
- [ ] Lessons fed to annual exercise scenario list ([../runbooks/dr-drill.md](../runbooks/dr-drill.md) Q4)
- [ ] Admission-gate evidence refreshed (BCP remains a Phase-21 Task 21.3.13 launch prerequisite + Phase-24 → production gate)
