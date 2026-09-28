# DORA ICT Third-Party Register

**Phase-09 Task 9.3.15 (item 4)** · **Authority:** spec §19.5, §24 #171 · **Owner:** Vendor/Procurement + Compliance · **Review:** onboarding, renewal, and annually; concentration re-assessed on any outage event

Register contract per task: provider → service → supported functions → locations/subcontractors → concentration risk → audit/access/termination terms → exit strategy → tested substitution/insourcing plan.

## 1. Register

| Provider | ICT service | Functions supported | Locations / subcontractors | Concentration | Audit/access/termination terms | Exit strategy | Substitution test |
|---|---|---|---|---|---|---|---|
| Cloud edge/WAF (Cloudflare/Fastly-class) | Edge ingress, DDoS, Anycast DNS reroute | all public endpoints; DR edge reroute (§18.6.4) | global PoPs | **High** — sole edge path | contract SLA + status page; 30d termination | secondary DNS provider pre-staged; direct-origin IP allowlist emergency mode | exercised in annual D1-adjacent test — pending Task 9.3.13 deploy |
| PagerDuty | On-call paging (P0–P2 receivers) | incident paging | US/EU processing | Medium — fail-safe exists | standard SaaS terms | SMS/phone-tree fallback documented in [incident-escalation.md](../runbooks/incident-escalation.md) | quarterly paging test |
| Slack | `#exchange-ops` + incident channels | ops comms, P2/P3 alerts | US/EU | Low–Medium | standard terms | email bridge + PagerDuty notes as fallback | N/A (comms only) |
| AWS S3 (or S3-compatible: MinIO/Ceph/R2) | WAL archive (WORM), CH backup `exchange-ch-backup`, PG PITR archive, Parquet cold tier | DR replay, backups, retention | region-pinned + CRR to secondary | **High** — all archives | Object Lock compliance mode; contract exit 90d | second provider bucket mirror (CRR target); `devs3` proves portability | D1/D3/D4 drills verify restore in-region |
| Refinitiv feed | Price oracle source A | mark/index pricing, liquidations | vendor DCs | Medium — 1 of 2+ feeds | feed license | second feed mandatory (<2 feeds → `PRICE_ORACLE_UNAVAILABLE`) | oracle feed-loss test — pending Phase-19.5 |
| Bloomberg BFIX | Price oracle source B | same | vendor DCs | Medium | feed license | ditto | ditto |
| ECB reference rates | Price oracle source C | same | ECB | Low | public reference | ditto | ditto |
| CLS Bank | PvP settlement (§17.6) | FX settlement finality | CLS network | High for settlement path | CLS member terms | bilateral gross settlement fallback (§17.7 netting/SSIs) | settlement-failure drill — pending Phase-24 |
| Banking rails (SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2) | Funding in/out, nostro | client money movement | per rail | Medium per rail | per-rail agreements | alternate rail routing per currency (Phase-11) | rail-down scenario in DR catalog — pending Phase-11 |
| Kubernetes/cloud or bare-metal DC operator | Hosts for Go services + metal | compute substrate | primary+secondary regions | High | DC contract + audit rights | multi-region (§18.4), bare-metal deploy docs | D1 drill |
| Grafana/Prometheus (self-hosted) | Metrics/dashboards/alertmanager | observability | self-hosted | Low | OSS | `ops.alerts.*` NATS fail-safe path independent | covered by AlertDispatchErrors checks |
| Vault/KMS (HashiCorp Vault or cloud KMS) | Secrets store | all credentials | primary+secondary region replicas | Medium | license/cloud terms | secondary-region copies, break-glass procedure ([secrets-inventory.md](./secrets-inventory.md)) | D7 decrypt-in-secondary drill |
| Sanctions/PEP screening vendor | Real-time screening | compliance gate | vendor SaaS | Medium | vendor SLA | scoped degradation mode (`SANCTIONS_SERVICE_UNAVAILABLE`) keeps venue up (Phase-21 AC #43) | provider-outage test — pending Phase-21 |

## 2. Concentration assessment

- **Single points:** S3 archive, edge provider, CLS, DC operator are high-concentration — each has a documented exit + tested substitution row above. Concentration is *accepted* where exit cost exceeds risk, recorded with the acceptor's name.
- **Chain outages:** a third-party chain failure (e.g., edge + DNS together, or S3 region + CRR target) is exercised as a drill edge case (Task 9.3.15 edge case list) — the [BCP](../policies/business-continuity-plan.md) covers the stand-down decision if a chain failure makes operation untenable.

## 3. Exit-plan testing

Every High/Medium concentration row must have its substitution or insourcing plan **tested on the annual cycle** (piggyback on [dr-drill.md](../runbooks/dr-drill.md) quarterly cadence): evidence = drill report row + artifact (e.g., restored backup hash, failover timestamps). An exit plan that fails its test is a remediation item on the [risk register](./dora-ict-risk-register.md), not a silent edit.
