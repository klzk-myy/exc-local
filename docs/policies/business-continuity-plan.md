# Business Continuity Plan (BCP)

**Phase-09 Task 9.3.27** · **Authority:** spec §19.11.1, §24 #331 · **Status:** controlled document — same change control as phase plans; changes require the §1 owner + quorum sign-off · **Test of record:** annual Q4 exercise ([../runbooks/dr-drill.md](../runbooks/dr-drill.md)) · **Gate:** launch prerequisite (Phase-21 Task 21.3.13) and Phase-24 → production checkpoint.

Scope note (§19.11.1): §18/§19.1–19.10 cover *technical recovery* (WAL replay, RPO/RTO, multi-region DR, drills). This document covers the case where recovery is impossible — or the wrong call — and the governance around that decision.

## 1. Decision framework

- **Named decision-maker:** Chief Operating Officer (delegate: Head of Trading Operations when COO unreachable >30min — delegation recorded per incident).
- **Quorum:** minimum 2 — one technical (Core Eng Lead or senior on-call SRE), one non-technical (COO/delegate or Finance Ops lead). Sole-member *temporary* stand-down is permitted (fail-closed bias); go-forward requires full quorum and ratification of any sole-member call within 24h.
- **Invocation entry point:** [../runbooks/bcp-standdown.md](../runbooks/bcp-standdown.md) — four scored criteria: client-money integrity, oracle/price integrity, ability to reconcile, whether client harm grows with delay.

## 2. Go-forward modes

Defined once here; operated from [../runbooks/bcp-goforward.md](../runbooks/bcp-goforward.md):

| Mode | Summary | Entry authority | Expected duration |
|---|---|---|---|
| GF-1 manual trade capture | bilateral capture under dual control while engine is down | quorum | hours–days, 4h reviews |
| GF-2 withdrawal-only servicing | trading closed; client money exits under standard withdrawal controls | quorum | days–weeks, 48h reviews |
| GF-3 frozen-but-reconcilable | nothing moves; evidence sealed; weekly integrity re-verify | quorum | days–months, 7d reviews |

## 3. Alternate site

Topology per spec §18.4 — **Secondary region** is the alternate site: warm standby C++ core, K8s Go services, PG semi-sync replica, Redis replica; **Tertiary** = cold backup only (PG hourly snapshots, WAL S3 archive, Redis RDB 60s, ClickHouse daily S3). Activation sequence follows §18.6.4 (declare → promote storage → WAL replay-from-archive → edge reroute 15–30s → 6-stage audit → resumption ladder) and is drilled quarterly (D1, [../runbooks/dr-drill.md](../runbooks/dr-drill.md)).

**Residency gate:** cross-region PG/S3 replication carries an SCC/adequacy check — EU/UK PII partitions (Phase-21 Task 21.3.18 residency enforcer — pending) fail over only to adequate jurisdictions; the secondary site location + adequacy basis is recorded in the DR runbook evidence (Task 9.3.29 item 3).

**Alternate-site location of record:** secondary region `eu-west-1`-class facility (site identifiers live in the fleet inventory — `environments`/`fleet_hosts`/`releases`, migration 091 + `services/internal/fleet`); tertiary object storage region is the CRR target of the WAL/CH buckets.

## 4. Financial-impact assessment (standing estimate)

Updated annually + after each invocation. Bands are severity-relative; absolute figures come from the finance pack at review time.

| Severity driver | Client money at risk | Insurance-fund exposure (`insurance_fund`, mig. 016) | Contingent-capital draw (§17.13.1) | Revenue loss | Regulatory exposure |
|---|---|---|---|---|---|
| Single-shard halt (hours) | low — shard instruments only | nil (liquidations unaffected) | none | session fees on affected pairs | incident report only |
| Region loss (≤1d) | medium — 15s fill-loss residual (§18.3 note) | possible auction shortfall | tranche 1 | 1 trading day | DORA major + venue incident reports |
| Extended outage (>1d) / unrecoverable ledger window | high — withdrawal queue + trapped margin | full draw + NBP top-ups (retail) | full facility | churn + reputation | license-impacting; daily regulator contact |
| Fraud/security breach | highest — integrity unprovable until audited | as above + compensation | full + capital call | as above + fines | breach notification regimes + DORA |

## 5. Notification tree

1. **Internal:** [../runbooks/incident-escalation.md](../runbooks/incident-escalation.md) §2 matrix — P0 page tree + `#inc-*` war room + 30min stakeholder cadence.
2. **Clients:** status feed `status:current` (Task 9.3.25 aggregator `services/internal/ops/status_exporter.go`; public page frontend pending) + direct notices within the §19.8 comms SLA (status ≤30min, email ≤1h).
3. **Regulators:** DORA major-ICT phases — **4h initial / 72h intermediate / 1-month final** ([../ops/dora-incident-reporting.md](../ops/dora-incident-reporting.md)); MiFID material-incident deadlines in parallel; named contact = Compliance Officer (deputy: MLRO for financial-crime-adjacent events). Templates: [../ops/ict-incident-template.md](../ops/ict-incident-template.md).
4. **Counterparties/banks:** settlement-critical events notify nostro/CLS counterparties through Finance Ops within the settlement cut-off calendar (§17.2 rails, cut-off enforcement Phase-24 Task 24.3.20).
5. **Evidence of delivery:** every notification logged (channel, recipient, timestamp, artifact) — "delays are logged, not rationalised."

## 6. Exercise & review

- **Annual exercise** (Q4): combined DR failover + BCP decision simulation + crisis-comms test — the BCP's test of record ([../runbooks/dr-drill.md](../runbooks/dr-drill.md) §2). Scenario rotates through: region loss → GF-1, ledger corruption → stand-down, third-party chain outage → GF-3.
- **Post-incident review:** mandatory ≤10 business days after any invocation — [../ops/post-incident-review-template.md](../ops/post-incident-review-template.md) — updates this plan and the runbook set *before* incident close.
- **Adoption/prerequisite gates:** current, exercised BCP is a launch prerequisite (Phase-21 Task 21.3.13) and gates Phase-24 → production release.

## 7. Document control

- Owner: COO (content), SRE (runbook currency), Compliance (regulatory sections).
- Versioned in-repo; every change references the incident/exercise that motivated it.
- This file is the BCP "of record" — the phase-task path `docs/governance/bcp.md` resolves here (ops doc set consolidated under `docs/policies/`).
