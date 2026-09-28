# Runbook: Incident classification P0–P3, escalation matrix & post-mortem workflow

**Phase-09 Task 9.3.18** · **Authority:** spec §19.8 (canonical SLA table), §2.7.2 (L0–L3 error tiers the severities map onto), §19.5 (DORA reporting clocks) · **Config surfaces:** `deploy/prometheus/alertmanager.yml` (routing), `deploy/prometheus/rules/exchange-alerts.yml` (severity labels), `deploy/pagerduty/escalation-rules.json` (**pending** — the machine-readable mirror of this matrix).

## 1. Severity definitions

The canonical table is spec §19.8; the tighter values are internal stretch targets layered on the canonical SLAs (remediation #35 — not a contradiction, the stricter clock wins operationally):

| Severity | Definition | Canonical SLA (§19.8) | Internal stretch | Error-tier mapping (§2.7.2) | Examples (this repo) |
|---|---|---|---|---|---|
| **P0** Critical | Trading halted, data corruption/loss, security breach, halt >2min | 15min ack / 1h mitigate | <5m ack | L0 Critical/Fatal | `L0ErrorObserved`, `IPCRingCritical`, `AvailabilityBurnFast`, `WAL_RECOVERY_HALT` w/ shard down, `LEDGER_IMBALANCE_ABORT`, split-brain, engine halt |
| **P1** Major | Single shard down w/ failover pending, market-data degraded, gateway p99 >50ms, bank rail down, partial data-loss risk | 30min ack / 4h mitigate | <15m ack | L1 Systemic | `L1ErrorsSustained`, `AeronDriverDown`, `Bridge*`×3, `NATSConsumerPendingHigh`, `AlertDispatchErrors`, `ReconciliationMismatch`, `WALLagGrowing`, `MarketDataOnly`/`SpotOnly` modes, `TIME_SYNC_LOSS_HALT` |
| **P2** Moderate | Non-critical service degraded, elevated error rate, monitoring gap, `ReadOnly`/`Throttled` modes | 2h ack / 24h resolve | <1h ack | L2 Transaction (plus watch-level L1) | `L2RejectionSpike`, `ErrorRateAnomaly`, `AeronSubscriberLag`, `IPCRingSaturated`, `DegradationModeActive` (RO/Throttled), `CircuitBreakerOpen`, `AvailabilityBurnSlow`, `SECRET_ROTATION_OVERDUE` |
| **P3** Minor | Cosmetic, docs gaps, non-customer-facing | next business day | <24h | L3 Edge/Protocol noise, informational | `DLQEntriesAccumulating`, IP-ban escalations, SLA warnings |

Tier↔severity is a *mapping*, not identity: L2 transaction rejections are healthy per-request behavior (P2 only when they spike >5%/min); L3 edge rejections are routine and only matter as anomalies (rate spikes, repeated offenses → progressive IP ban).

## 2. Escalation matrix (automated paging)

| Severity | First page | If unacknowledged | Management escalation | Comms obligations (§19.8) |
|---|---|---|---|---|
| P0 | On-call SRE + Core Eng Lead + VP Eng + CTO (simultaneous, `pagerduty-p0`, group_wait 0s, repeat 15m) | 5min → secondary engineering leadership | CTO owns regulator/BCP calls | status page ≤30min, client email ≤1h, post-mortem ≤48h, DORA clocks below |
| P1 | On-call SRE + Component Lead (`pagerduty-p1`, repeat 1h) | 15min → primary on-call manager | Eng Lead informed at 30min | status page ≤30min if client-visible, post-mortem ≤48h |
| P2 | Ticket + `#exchange-ops` (`pagerduty-p2-ticket`, repeat 4h) | 4h repeat → team lead queue review | weekly ops review | none required |
| P3 | `#exchange-ops` Slack only (repeat 12h) | — | ticket queue | none |

**Suppression:** alertmanager inhibit rules — a firing P0 suppresses p1–p3 on the same `service`+`shard` ("fix the halt first, noise second"); a firing P1 suppresses P3. Do not manually silence inhibited alerts; silence the root.

**PagerDuty provider outage fallback:** oncall phones via SMS/phone tree — ordering: primary SRE mobile → secondary SRE → Core Eng Lead → VP Eng → CTO. Numbers live in the on-call record (PagerDuty profile + duplicated into the ops wiki); fallback activation is itself a P2 event (monitoring redundancy lost).

## 3. War-room protocol (P0/P1)

1. Incident channel `#inc-YYYYMMDD-id` created (bot automation — **pending** incident-manager service `services/internal/operations/incident_manager.go`, Task 9.3.18; interim: on-call creates it manually).
2. Conference bridge spin-up; incident commander named (on-call SRE by default; Core Eng Lead for engine-domain P0s).
3. Stakeholder status updates every **30min** for P0 — template: impact / scope / current mode (`X-Degradation-Mode`) / mitigation in progress / next update time.
4. All mitigation actions narrated in-channel before execution (dual-control actions need the second operator in-channel).

## 4. DORA & regulator clocks (P0 and material P1)

From §19.8/§19.5 — tracked in the incident record ([../ops/ict-incident-template.md](../ops/ict-incident-template.md)):

- **4h** initial notification to regulator (major ICT incident)
- **72h** intermediate report
- **1 month** final report
- MiFID material-incident deadlines run in parallel (see [../ops/dora-incident-reporting.md](../ops/dora-incident-reporting.md))

## 5. Post-mortem workflow

- Required within **48h** for every P0 and every P1 with client impact — template [../ops/postmortem-template.md](../ops/postmortem-template.md); archive in `docs/incidents/` (create on first use).
- Blameless, mandatory fields: timeline, impact, root cause, action items with owners+dates, lessons. Action items tracked to closure (a post-mortem with open actions keeps the incident "open" for DORA closure rules — Task 9.3.15 item 5).
- Monthly SLO review feeds post-mortem findings into the reliability backlog ([../ops/slo-policy.md](../ops/slo-policy.md)).
- BCP-invoked incidents additionally require the **10-business-day** post-incident review updating the BCP ([../policies/business-continuity-plan.md](../policies/business-continuity-plan.md), template [../ops/post-incident-review-template.md](../ops/post-incident-review-template.md)).

## 6. Severity change rules

- Upgrade any time on new information (shard loss discovered during a P2 → re-grade P1; downgrade only with incident-commander sign-off recorded in-channel).
- Simultaneous cascading alerts: the inhibit rules collapse same-service noise; cross-service cascade = one incident, severity of the worst symptom.
- Planned maintenance is excluded from severity labeling — declare the window (`maintenance_windows`, migration 172; `POST /api/v1/admin/maintenance-windows`) *before* the work, not after the alert.
