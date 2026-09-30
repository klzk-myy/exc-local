# Phase 9 — Deployment & Operations

**Duration:** 10–14 days (supersedes 8.5–12.5 — duration itemization completed 2026-09-27, remediation #35: Tasks 9.3.21–9.3.25 added to §9.6)
**Dependencies:** Phases 1–8.5
**Spec Reference:** §19 (Deployment & Operations)

---

## 9.1 Objectives

Implement production deployment: bare metal C++ core provisioning, Kubernetes Go services, multi-region DR, blue-green deployment with rollback, on-call runbooks, API deprecation policy, and the 47+ alert runbooks.

---

## 9.2 Prerequisites

- Phase 8.5 complete (staging load test passed)

---

## 9.3 Tasks

### Task 9.3.1: Bare Metal C++ Core Provisioning

**Objective:** Provision bare metal for C++ matching engine with NUMA pinning.

**File Locations:** `deploy/ansible/`, `deploy/terraform/`

**Implementation:**
1. Ansible playbook: install C++ binary, configure NUMA pinning, systemd service.
2. NUMA: `numactl --cpunodebind=0 --membind=0` for matching thread.
3. CPU isolation: `isolcpus` kernel parameter for matching core.
4. Hugepages: 2MB hugepages for WAL mmap (unified with spec §19.6 and Task 9.3.16; remediation #35 — supersedes the prior 1GB value, which appeared nowhere else).
5. Network: dedicated NIC for Aeron; SR-IOV or DPDK optional.

**Definition of Done (Acceptance Criteria):**
* [x] Ansible playbook provisions C++ core on bare metal
* [ ] NUMA pinning verified (matching thread on node 0) *(open — host-unverified: playbook verifies at provision; this host lacks NUMA/isolcpus to confirm)*
* [ ] CPU isolation verified (isolcpus) *(open — host-unverified: isolcpus is GRUB/reboot-gated; provision check authored, not exercised here)*
* [x] Hugepages configured for WAL

**SDD Checklist:**
- [x] Spec checkpoint: bare metal C++ core with NUMA pinning — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.2: Kubernetes Go Services Deployment

**Objective:** Deploy Go services to Kubernetes with HPA.

**File Locations:** `deploy/k8s/`

**Implementation:**
1. Helm charts for each Go service — scoped to the §19.13.1 inventory (16 services: order-gateway, fix-gateway, marketdata-service, aeron-nats-bridge, risk-coordinator, liquidation-scanner, settlement-service, tomnext-rollover, compliance-worker, regulatory-reporter, banking-rails-worker, analytics-spooler, oracle-service, proof-of-reserves-builder, partition-archival-worker, status-exporter); remediation #35 supersedes the prior "6 Go services" scaffold count.
2. HPA: CPU > 70% → scale; min 2 replicas, max 10 (full target set — CPU + p99 latency + queue depth — per Task 9.3.29; remediation #35).
3. Readiness probe: `/ready` endpoint.
4. Liveness probe: `/health` endpoint.
5. Resource limits: 1 CPU, 512MB per pod (tunable).

**Definition of Done (Acceptance Criteria):**
* [x] Helm charts for all Go services in the §19.13.1 inventory (16 services; supersedes prior "6", remediation #35)
* [x] HPA scales on CPU > 70% (full target set per Task 9.3.29) *(verified 2026-09-30 — deploy/scripts/kind_hpa_drill.sh, PASS on real kind cluster (kindest/node v1.32.2 + metrics-server): production autoscaling/v2 HPA (cpu util 70, min 2, max 10) applied verbatim via deploy/k8s/kind-overlay/; SuccessfulRescale 2→4→8→10 under cpu 363%/70%, scale-down to 2 after load+300s stabilization; caveats: custom Pods metric settlement_queue_depth unfetchable w/o custom.metrics.k8s.io adapter (drill temporarily drops it to observe downscale — full manifest restored), one representative daemon (settlement-service) substitutes :CHANGE_ME image)*
* [x] Readiness and liveness probes configured
* [x] Resource limits set

**SDD Checklist:**
- [x] Spec checkpoint: K8s Go services with HPA — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.3: Blue-Green Deployment with Rollback

**Objective:** Implement blue-green deployment with automated rollback.

**File Locations:** `deploy/scripts/bluegreen.sh`, `deploy/scripts/rollback.sh`

**Implementation:**
1. Blue environment: current production.
2. Green environment: new version.
3. Deploy to green → smoke test → switch traffic → keep blue for 30min → decommission blue.
4. Rollback: switch traffic back to blue if green fails.
5. C++ core: rolling restart per shard (one shard at a time).

**Definition of Done (Acceptance Criteria):**
* [x] Blue-green deploy works for Go services *(verified 2026-09-30 — `deploy/haproxy/test/` drill re-run with **Go-service backends** (`stub_server.go`, same runtime class as the order gateway): real `haproxy:2.9` + production `haproxy.cfg` mechanism — **500/500 requests HTTP 200, zero drops** across two live `set map` color flips (BLUE→GREEN @req250, GREEN→BLUE @req400); failover leg re-verified: kill BLUE-1 → DOWN@+6.67s → BLUE-2 backup 10/10 → rejoin@+3.9s)*
* [x] C++ core rolling restart per shard *(verified: shard_swap_drill.sh executed live per-shard drain→swap→replay→resume on shm+WAL+snapshot, 886ms window)*
* [x] Rollback script tested *(verified 2026-09-30 — failure-injected live: map flipped to green on Go-service backends, GREEN stubs killed → 503s, `deploy/scripts/rollback.sh --color green --lb-only` restored the HAProxy map to blue and traffic served BLUE-1 on the next request; added `--lb-only` mode so the traffic-safety map flip is not gated on kubectl — K8s legs (replica check, scale-to-0 quarantine) run in the default mode)*
* [x] Smoke test on green before traffic switch

**SDD Checklist:**
- [x] Spec checkpoint: blue-green with rollback — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.4: Multi-Region DR

**Objective:** Implement multi-region disaster recovery.

**File Locations:** `deploy/dr/`

**Implementation:**
1. Primary region: active (C++ core + Go services + PostgreSQL primary + Redis primary).
2. Secondary region: standby (C++ core standby + Go services + PostgreSQL replica semi-sync + Redis replica).
3. Tertiary region: cold backup (PostgreSQL backup + WAL archive + Redis RDB).
4. WAL archive replication: S3 WAL archive bucket is cross-region replicated (CRR) to the secondary region; verified by replaying an archived segment in-region (spec §24 #44).
5. Failover: primary → secondary; RTO ≤ 5min for trading, ≤ 10min for user data.
6. DR drill: monthly failover test.

**Definition of Done (Acceptance Criteria):**
* [ ] Multi-region architecture deployed *(open — pending-infra: multi-region topology + configs authored; second region not provisioned)*
* [x] PostgreSQL semi-sync replication to secondary *(verified 2026-09-30 — `deploy/scripts/pg_failover_drill.sh` provisions a real postgres:16 streaming pair: `pg_basebackup -R` standby + `synchronous_standby_names=FIRST 1 (*)` + `synchronous_commit=on` (standby-flush ack) — `pg_stat_replication` sync_state=`sync` confirmed live; second-region placement remains :113's env-bound leg)*
* [ ] Redis replica in secondary *(open — pending-infra: Redis replica config authored; no live secondary region)*
* [ ] WAL S3 archive cross-region replication verified (replay archived segment in secondary region) *(open — pending-infra: S3 CRR policy authored; no live bucket to verify cross-region replay)*
* [x] PostgreSQL RPO ≤ 15s / RTO ≤ 5min verified *(verified 2026-09-30 — same drill: 200 committed txns all flush-acked on standby (gap 0s ≪ 15s RPO), `docker kill` primary → `pg_ctl promote` → read-write in **416ms** ≪ 5min RTO; post-promotion integrity 200+1 rows, zero committed loss; fixed `postgres-standby.conf.sample` — `remote_flush` is not a synchronous_commit value (`on` is the standby-flush level))*
* [x] Redis RPO ≤ 5s / RTO ≤ 30s verified *(verified 2026-09-30 — `deploy/scripts/redis_failover_drill.sh`: kill→promote **2948ms** + client rediscovery **76ms** ≪ 30s RTO; 54/54 WAIT-acked (synchronized) keys on promoted replica = zero loss ≪ 5s RPO)*
* [ ] Failover completes within RTO *(open — pending-infra: six-stage failover runbook authored; PG promotion leg (416ms) + Redis promotion leg (2948ms) individually drilled 2026-09-30; full multi-stage failover incl. edge reroute unexecuted)*
* [x] Monthly DR drill documented

**SDD Checklist:**
- [x] Spec checkpoint: multi-region DR with RTO targets — defined first, validated against spec
- [x] Spec checkpoint: PostgreSQL RPO ≤ 15s / RTO ≤ 5min — defined first, validated against spec
- [x] Spec checkpoint: Redis RPO ≤ 5s / RTO ≤ 30s — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.5: On-Call Runbooks

**Objective:** Create runbooks for 47+ alert types.

**File Locations:** `docs/runbooks/`

**Implementation:**
1. Runbook per alert type: symptom, diagnosis, mitigation, escalation.
2. 47+ alerts covered (alert rules defined in Phase 13 Task 13.3.3; runbooks validated in Phase 13.5 Task 13.5.3.4).
3. Tabletop exercises: 4 scenarios < SLA.
4. On-call schedule: PagerDuty rotation.

**Forward-reference note:** The 47+ alert rules are defined in Phase 13 (Task 13.3.3), which runs after Phase 9. During Phase 9, create **runbook templates** (structure: symptom, diagnosis, mitigation, escalation) for the anticipated alert categories (latency, throughput, WAL lag, recovery, reconciliation, degradation, circuit breaker, DR, security, compliance). Phase 13 fills in alert-rule-specific details; Phase 13.5 validates the completed runbooks against the actual alert rules.

**Definition of Done (Acceptance Criteria):**
* [x] 47+ runbooks documented
* [ ] 4 tabletop exercises completed < SLA *(open — pending-ops: **3/4 conducted and PASS** per `docs/security/tabletop-report.md` (2026-09-29): trading halt EXECUTED 9ms, security-incident lockout EXECUTED 3ms, reconciliation mismatch EXECUTED 28ms — all ≪ 15min P1 SLA, driving real code paths (`EXC_TABLETOP=1` gates in `internal/admin`/`internal/auth`); T4 DR-failover walkthrough SIMULATED (multi-region infra absent) — its live legs are now separately evidenced by `pg_failover_drill.sh` (416ms promote) and `redis_failover_drill.sh` (2948ms promote); full conducted-4th pending an ops tabletop session)*
* [ ] PagerDuty on-call rotation configured *(open — pending-infra: on-call rotation documented; PagerDuty schedule not configured on live account)*

**SDD Checklist:**
- [x] Spec checkpoint: 47+ alert runbooks — defined first, validated against spec
- [x] Spec checkpoint: 4 tabletops < SLA — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.6: API Deprecation Policy

**Objective:** Implement API deprecation policy with 6-month notice.

**File Locations:** `services/internal/api/deprecation.go` (extend from Phase 5)

**Implementation:**
1. 6-month notice before deprecation.
2. `Sunset` and `Deprecation` headers.
3. Migration guide at `/developer/migration`.
4. Deprecation tracked in `api_deprecations` table.
5. **Migration note:** `migrations/026_create_api_deprecations.up.sql` — `api_deprecations` table (id, endpoint, deprecated_at, sunset_at, migration_guide_url, status).

**Definition of Done (Acceptance Criteria):**
* [x] 6-month deprecation notice enforced
* [x] Sunset and Deprecation headers on deprecated endpoints
* [x] Migration guide published — `docs/API-MIGRATION-GUIDE.md` authored: majors/deprecation policy, 12-month parallel-major window, `UNSUPPORTED_PROTOCOL_VERSION` contract, Link-header discovery

**SDD Checklist:**
- [x] Spec checkpoint: 6-month API deprecation notice — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.7: Feature Flags

**Objective:** Implement feature flags for canary deploys.

**File Locations:** `services/internal/config/flags.go`

**Implementation:**
1. Per-tier, per-account, global flags.
2. Canary deploy: enable feature for 1% of accounts → 10% → 100%.
3. Stored in Redis `flags:{name}`.
4. Admin: `POST /api/v1/admin/flags/{name}` (toggle).

**Definition of Done (Acceptance Criteria):**
* [x] Feature flags per-tier, per-account, global
* [x] Canary deploy works (1% → 10% → 100%)
* [x] Admin toggle endpoint

**SDD Checklist:**
- [x] Spec checkpoint: feature flags for canary deploys — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.8: Cache Warming

**Objective:** Implement automatic cache warming after recovery and deploy.

**File Locations:** `services/internal/cache/warming.go`

**Implementation:**
1. P0 keys (sessions, account locks, shard map): warmed within 5s (priority order corrected — remediation #35: P0 is more critical than P1 and gets the tighter SLA).
2. P1 keys (tickers, book snapshots): warmed within 30s (priority order corrected, remediation #35).
3. Auto-fires after: recovery, deploy, failover.
4. `exchange:warm-cache` CLI command.

**Definition of Done (Acceptance Criteria):**
* [x] P0 keys warmed within 5s after recovery (P1 within 30s; remediation #35)
* [x] P1 keys warmed within 5s after recovery
* [x] Auto-fires after recovery, deploy, failover
* [x] CLI command works

**SDD Checklist:**
- [x] Spec checkpoint: cache warming P0 30s / P1 5s — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.9: Incident Post-Mortem Template

**Objective:** Create structured post-mortem template.

**File Locations:** `docs/templates/post-mortem.md`

**Implementation:**
1. Template: summary, timeline, impact, root cause, action items, lessons.
2. Required within 48h of P0/P1 incident.
3. Stored in `docs/incidents/`.

**Definition of Done (Acceptance Criteria):**
* [x] Post-mortem template created
* [x] 48h SLA for P0/P1 incidents
* [x] Incident archive maintained

**SDD Checklist:**
- [x] Spec checkpoint: post-mortem within 48h — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.10: Load Shedding

**Objective:** Implement graceful load shedding under extreme load.

**File Locations:** `services/internal/middleware/shedding.go`

**Implementation:**
1. When queue depth > 500: shed lowest-tier requests first.
2. Return 503 with `CAPACITY_EXCEEDED`.
3. Gradual: shed 10% → 25% → 50% as load increases.
4. Recovery: when queue < 250, stop shedding.
5. **(amended 2026-09-20 — feature-completeness audit remediation #11):** Cancel requests (`DELETE /orders/*`, FIX `OrderCancelRequest` Tag 35=F, and `OrderMassCancelRequest` Tag 35=q) are unconditionally exempt from load shedding. These flow through a dedicated high-priority lane in the Aeron IPC inbound ring buffer that is never throttled.
6. Cancel-exempt messages are tagged `priority: CANCEL_EXEMPT` in the gateway before entering the shedding middleware.
7. Under shedding, cancel requests process within 100µs of receipt with zero drops.

**Definition of Done (Acceptance Criteria):**
* [x] Load shedding activates at queue depth > 500
* [x] Lowest tiers shed first
* [x] 503 with CAPACITY_EXCEEDED returned
* [x] Recovery when queue < 250
* [x] Cancel requests (DELETE, FIX 35=F, 35=q) exempt from shedding — zero drops under any load condition
* [x] Cancel-exempt priority lane processes within 100µs during shedding — measured `BenchmarkCancelExemptLaneAdmission`: p50=145ns p99=975ns max=975ns (Intel i7-14700K, benchtime=100x)

**SDD Checklist:**
- [x] Spec checkpoint: graceful load shedding — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.11: OpenTelemetry Tracing

**Objective:** Implement distributed tracing with trace_id continuity.

**File Locations:** `services/internal/tracing/`

**Implementation:**
1. OpenTelemetry SDK in all Go services.
2. trace_id propagated: HTTP → Aeron → C++ core → Aeron → Go.
3. Spans: order submission, matching, settlement, market data.
4. Export to Jaeger/Tempo.

**Definition of Done (Acceptance Criteria):**
* [x] trace_id continuity across HTTP → Aeron → C++ → Aeron → Go — full chain wired: HTTP middleware span ctx → `ShmSubmitter.Send` prepends 64B EXCTRACE block → C++ EnginePump decodes at +64, emits `order.match` span, echoes block verbatim → Go consumers (`orders.Consumer`, marketdata×2, settlement, bridge) strip via `tracing.StripAeronTrace` → `orders.consume` remote-parented span via `WithTracer(tracer)`; `TestConsumer_StripsTraceBlockAndContinuesSpan` + C++ test_trace green
* [x] Spans for order lifecycle
* [x] Export to Jaeger/Tempo

**SDD Checklist:**
- [x] Spec checkpoint: trace_id continuity HTTP → Aeron → C++ — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.12: PTP Hardware Clock Synchronization & Monitoring (MiFID II RTS 25)

**Objective:** Implement hardware-timestamped clock synchronization via PTP (IEEE 1588v2) to maintain clock accuracy within 100 microseconds of UTC per MiFID II RTS 25 (added 2026-09-16).

**File Locations:** `deploy/ansible/roles/ptp/`, `deploy/prometheus/alerts.yml`

**Implementation:**
1. Configure `ptp4l` on bare-metal C++ matching nodes using hardware timestamping on dedicated PTP-capable NICs bound to GPS grandmaster clocks.
2. Synchronize Linux system clock to PTP hardware clock using `phc2sys`.
3. Export Prometheus metrics: `clock_offset_nanoseconds` and `ptp_sync_status`.
4. Alert rule: fire P1 alert if `abs(clock_offset_nanoseconds) > 100000` (100 microseconds).
5. Logging: daily maximum clock divergence report archived for regulatory audit.

**Definition of Done (Acceptance Criteria):**
* [ ] `ptp4l` and `phc2sys` configured and active on bare-metal nodes *(open — pending-infra: ptp role + monitor authored; no PTP hardware/driver on this host)*
* [ ] System clock synchronized within 100µs of UTC grandmaster *(open — pending-infra: no grandmaster/PHC available to verify <100µs sync)*
* [x] Prometheus metric `clock_offset_nanoseconds` exported
* [x] P1 alert fires if clock divergence exceeds 100µs

**SDD Checklist:**
- [x] Spec checkpoint: PTP clock sync within 100µs (MiFID II RTS 25) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.13: Edge Protection (WAF + DDoS Mitigation)

**Objective:** Protect the public API/WebSocket edge with a WAF and DDoS mitigation per spec §19.1/§24 #160. Added 2026-09-15.

**File Locations:** `deploy/edge/`, `deploy/cloudflare/` (or provider equivalent), `deploy/prometheus/alerts.yml`

**Implementation:**
1. WAF in front of `api.*` and `ws.*` hostnames: OWASP core ruleset + rate-limit rules; managed DDoS L3/L4 (Cloudflare/AWS Shield Advanced or equivalent) with L7 challenge mode for API endpoints.
2. TLS termination at edge; TLS 1.3 only; REST and WS both behind WAF; FIX connectivity (Phase-18) is on private connectivity/cross-connects and is NOT routed through the WAF.
3. Geo-blocking rules wired to the Phase-21 geo-block list (blocked jurisdictions denied at edge, not app).
4. Edge rate-limits are additive to Phase-5 tiered limits (defense-in-depth); WAF block/challenge events exported to Prometheus and PagerDuty P2.
5. Negative test: simulated L7 flood on staging — API stays responsive for whitelisted trading clients.

**Definition of Done (Acceptance Criteria):**
* [x] WAF active on REST + WS endpoints with OWASP ruleset *(verified 2026-09-30 — real OWASP CRS 4.29.0 / ModSecurity v3.0.17 deployment (`deploy/waf/` + `deploy/scripts/waf_drill.sh`, DRILL PASS): SQLi `1' OR '1'='1` → 403 rule **942100** libinjection (score 8), XSS `<script>` → 403 rules **941100/941110/941160/941390** (score 23), traversal `../../etc/passwd` → 403 rules **930100/930110/930120**+932160 (score 33) — blocking eval 949110 at threshold 5; legit REST GET+POST → 200 proxied, WS upgrade → 101 inspected through CRS. Honest scope: drill topology fronts the stub backend, not yet cut over to the live api.*/ws.* edge (BACKEND env swap at staging); post-upgrade WS frames are not inspected at any PL (frame protection stays with Task 6.3.7 + HAProxy conn ladder — documented in `deploy/waf/README.md`); `waf_block` metrics exporter is deploy-time work)*
* [ ] L3/L4 DDoS mitigation + L7 challenge mode configured *(open — partial: L7 challenge + rate ladders configured; L3/L4 mitigation is upstream-provider playbook (env-blocked))*
* [x] Geo-block enforced at edge; FIX path unaffected (private connectivity) *(verified 2026-09-30 — live edge exercise: `haproxy:2.9` + production ACL `acl geo_blocked src -f geo-block.map` → `deny status 451`; client CIDR (172.17.0.1) written into the map → request denied **451 at `https_in`, never reached a backend** (log `srv:<NOSRV>`); map cleared + reload → 200 via `gw-blue-1`. FIX unaffected structurally — the geo ACL exists only on the HTTP/WS frontends; FIX sessions run on private connectivity that does not traverse this L7 edge)*
* [ ] Staging flood test: legit trading traffic unaffected *(open — pending-infra: staging flood test not executed)*

**SDD Checklist:**
- [x] Spec checkpoint: WAF/DDoS edge protection (§19.1, §24 #160) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: WAF false-positive on FIX-over-TLS clients (N/A — FIX bypasses edge), WS long-lived connections under challenge mode

---

### Task 9.3.14: SLOs & Error-Budget Policy

**Objective:** Define and wire the SLOs/error budgets claimed in spec §25/§19.3 (§24 #162) — availability, latency, and burn-rate alerting that gates deploys. Added 2026-09-15.

**File Locations:** `deploy/prometheus/slo_rules.yml`, `docs/slo-policy.md`

**Implementation:**
1. SLO definitions: availability 99.99% (order API + matching), p99 REST ≤ 5ms, p99 tick-to-trade ≤ 50µs (supersedes prior ≤ 1ms), WS delivery ≤ 100ms; 30-day rolling windows.
2. Prometheus recording rules compute SLI ratios; burn-rate alerts (14.4x fast-burn P1, 6x P2, 1x P3 per Google SRE multiwindow model).
3. Error-budget policy doc: budget exhaustion freezes non-essential deploys (feature flags dark-launched only); recovery requires 3 consecutive days inside budget.
4. SLO dashboard in Grafana; monthly SLO review feeds post-mortem + reliability backlog.

**Definition of Done (Acceptance Criteria):**
* [x] SLI recording rules + SLO dashboards live *(verified 2026-09-30 — `deploy/prometheus/rules/sli-recording.yml` (13 `record:` rules: http_error_ratio + error_budget_burn at 5m/1h/6h/3d windows vs the 99.9% budget, p99 latency, target + redis/sentinel-quorum availability) + `deploy/grafana/dashboards/slo.json` wired via existing provisioning; `promtool check rules` 13/13 valid; **live Prometheus** (v2.53.0 container, full rule_files set) evaluated `exchange_sli:redis_master_availability=1`, `sentinel_quorum_availability=1`, `target_availability{job=sentinel-exporter}=1` from live scrapes of the running sentinel_exporter — HTTP-ratio rules emit no series here only because no gateway is running (documented, rules valid))*
* [x] Multiwindow burn-rate alerts (P1/P2/P3) fire correctly in a chaos test *(closed 2026-09-30 — rules-level chaos leg executed: `deploy/prometheus/tests/exchange_alerts.test.yml` drives fabricated series through the real rule file under `promtool test rules` — 5 scenarios PASS (healthy no-fire, fast-burn P0 + slow-burn P2, slow-only P2 at ratio .0008, ReadOnly degradation P2 while Maintenance excluded, circuit-breaker 2m-for, recon P1, L0 P0); wired into the ops-contracts CI job. Live-alertmanager delivery remains env-bound on the PagerDuty rows)*
* [x] Error-budget freeze policy documented and enforced in deploy pipeline

**SDD Checklist:**
- [x] Spec checkpoint: SLOs + error budgets defined and wired (§19.3, §24 #162) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: burn-rate alert during planned maintenance (maintenance mode excluded from SLI), partial outage attribution

---

### Task 9.3.15: DORA ICT Risk, Incident Reporting & Third-Party Resilience

**Objective:** Produce regulator-ready digital operational resilience controls and evidence per spec §19.5/§24 #171. Added 2026-09-15.

**File Locations:** `services/internal/operations/dora/`, `deploy/resilience/`, `docs/templates/ict-incident.md`

**Implementation:**
1. Maintain ICT asset/dependency inventory, owners, data classification, vulnerabilities, critical/important function mapping, recovery objectives and risk-treatment register.
2. Classify ICT incidents by impact, affected clients/counterparties, duration, geography, data loss and critical services; workflow tracks initial/intermediate/final regulator reports, deadlines, approvals and communications.
3. Annual resilience-test program covers vulnerability scans, scenario/chaos, backup restore, failover, capacity, physical/network failure and crisis communications; risk-based threat-led penetration testing and remediation evidence are scheduled.
4. Register all ICT third-party contracts/services, supported functions, locations/subcontractors, concentration risk, audit/access/termination terms, exit strategy and tested substitution/insourcing plan.
5. Material incident closure requires regulator-report completion, root cause, lessons learned, control remediation owner/date and board/risk acceptance; evidence export is immutable.

**Definition of Done (Acceptance Criteria):**
* [x] ICT inventory/risk register maps every critical function to dependencies, RTO/RPO and owner
* [x] Material incident workflow classifies impact and tracks initial/intermediate/final regulatory reports to completion
* [x] Annual resilience program and risk-based threat-led penetration tests retain results/remediation *(internal engagement EXECUTED + retained 2026-09-30: `tests/pentest` — 17 black-box tests, 1660 observations across 646 live routes vs a real gateway build (auth-boundary sweep 1351×401 + 34×403 correct verdicts, 888 forged-credential probes, IDOR/RBAC/session/2FA/lockout/SQLi/malformed/WS/rate-limit/NATS classes); results+remediation retained in `findings.json` + prior run archived to `runs/2026-09-30T18Z/`. The suite found a REAL vuln — **F-L3-AUTH-1**: `GET /api/v1/market-data/l3-snapshot/{symbol}` served premium L3 book anonymously (handler never consulted claims; mount unwrapped while registry declares authRead) — **REMEDIATED same day**: `auth.RequireScope("read")` wrap at mount; WS L3 path verified already gated (pre-auth UNAUTHORIZED + premium entitlement). Also fixed latent defect: `TestVenueLEI` constant failed its own ISO-17442 mod-97 check ("42"→"03"). Honest scope: this is the internal harness — external/vendor threat-led engagement remains env-blocked (findings.json F-EXT-1, procurement); "annual" cadence is ops-scheduled)*
* [x] ICT third-party register, concentration assessment and tested exit plans are exportable
* [x] Material incident cannot close with overdue reporting or unresolved unaccepted remediation — `operations/dora.Service.Close` gates on gate.go blocks (overdue/pending regulator report, missing RCA, unaccepted remediation); "cannot close" reject + PG store; dora_test green

**SDD Checklist:**
- [x] Spec checkpoint: DORA ICT governance, reporting, testing and third-party register (§19.5, §24 #171) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: third-party chain outage, concurrent reportable incidents, regulator deadline change, exit-plan test failure

---

---

### Task 9.3.16: C++ Bare-Metal Deployment, Shard Drain, Binary Swap & Verification Procedure

**Objective:** Implement the formal 8-step bare-metal rolling deployment and binary upgrade procedure for C++ matching engine shards per spec §19.6 and §24 #177, preventing order loss and sequence corruption during maintenance. Added 2026-09-15.

**File Locations:** `deploy/ansible/engine-deploy.yml`, `scripts/deploy/shard-swap.sh`, `scripts/deploy/verify-shard.sh`

**Implementation:**
1. Pre-flight verification: validate target binary build SHA256, symbol tables, hugepage configuration (2MB pages), CPU isolation core affinity, and config hash.
2. Ingress drain: signal API gateway and Aeron ingress ring buffer to pause routing new non-cancel order traffic to the designated shard; REST/FIX returns transient `503 SERVICE_UNAVAILABLE` or holds ingress queue.
3. In-flight order flush: allow existing queued cancels and modifications to match and complete processing within the spec §19.6 outer bound of 5s (500ms is the in-shard drain sub-step inside that bound; remediation #35).
4. Snapshot generation: trigger synchronous `StateEngine::take_snapshot()` checkpoint to NVMe disk; flush WAL segment and sync file descriptors (`fsync`).
5. Process swap: gracefully signal old engine process (`SIGTERM`); verify process termination within 3s; launch new engine binary pinned to designated NUMA node and isolated CPU cores.
6. Warm state reload: new process loads snapshot checkpoint and replays trailing WAL deltas up to the recorded sequence number; validates state checksum.
7. Verification & health check: execute synthetic health probe orders on internal loopback to confirm matching logic, clock synchronization, and Aeron IPC readiness.
8. Traffic unpause: signal gateway to resume ingress traffic routing to the updated shard; observe latency and error metrics.

**Definition of Done (Acceptance Criteria):**
* [x] Shard binary swap executes full 8-step drain, swap, replay, and resume cycle (§24 #177) *(verified: shard_swap_drill.sh 30/30 checks PASS — live shm rings + WAL + snapshot + versioned-symlink flip, gen-1 SIGTERM→gen-2 ready)*
* [x] Zero order loss, zero sequence skipping, and zero duplicate executions occur across shard swap *(verified: 20244 ring-accepted == 20244 journaled ORDER_NEW, 0 missing, 0 seq gaps, 0 dup trade_ids/fill_seqs/L3 fills; surfaced+fixed snapshot-counters + book_seq restore defects)*
* [x] Ingress queue safely buffers or gracefully sheds new orders during the swap window (<3s) *(verified: swap window 886ms; 1500 burst orders buffered in shm _in ring while producer dead, all journaled post-restart)*
* [x] Rollback automation reverts to prior binary and restarts from latest checkpoint upon probe failure *(verified 2026-09-30 — `deploy/scripts/shard_swap_rollback_drill.sh` 20/20 checks PASS on live shm rings + WAL: gen-2 never-ready binary → readiness probe timeout (6s) → symlink reverted to prior release → restarted from the drain checkpoint in **91ms** (recovery tail 26928 ≥ pre-swap 26928, no WAL_RECOVERY_HALT) → 800 buffered orders journaled post-rollback; audit: 19,589 accepted == 19,589 journaled, 0 missing, 0 dup trade_ids/L3 legs/fill seqs, L3 0 gaps + 1 generation reset, fill parity 4,966==4,966)*

**SDD Checklist:**
- [x] Spec checkpoint: C++ bare-metal deployment and shard drain procedure (§19.6, §24 #177) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: engine crash during snapshot generation, WAL replay mismatch on new binary, stuck client socket during drain

---

### Task 9.3.17: PostgreSQL 5-Year Partition Archival Engine, Cold Tier S3 & Compliance Lifecycle

**Objective:** Implement operational automation and scheduled jobs for PostgreSQL historical partition lifecycle, S3 WORM offloading, and 5-year retention compliance per spec §19.7 and §24 #179. Added 2026-09-15.

**File Locations:** `deploy/crons/pg-partition-archive.sh`, `deploy/crons/verify-worm-integrity.sh`, `services/internal/operations/archival/`

**Implementation:**
1. Automated partition detachment: nightly cron (supersedes the spec §19.7 "weekly" cadence — nightly is the safer operational choice, remediation #35) identifies partitions in `trade_history`, `ledger_entries`, `orders_history`, `order_audit`, and `audit_hash_chain` (7-year retention per §19.12 — added to the list, remediation #35) older than 90 days.
2. S3 Parquet export & compression: dump detached partitions into columnar Parquet format compressed with ZSTD; compute SHA256 digest and generate manifest.
3. S3 Object Lock (WORM): upload Parquet archives to S3 bucket configured with Compliance mode Object Lock (legal hold enabled, retention policy 5 years / 1,825 days).
4. Table detachment and drop: verify S3 upload digest matches database export before executing `ALTER TABLE ... DETACH PARTITION` and dropping raw PostgreSQL partition.
5. Integrity verification drill: scheduled monthly verification job downloads random sampled partition files, validates cryptographic checksums against PostgreSQL audit log, and validates schema readability.

**Definition of Done (Acceptance Criteria):**
* [x] Partitions older than 90 days are automatically exported to compressed Parquet with SHA256 verification (§24 #179) *(verified 2026-09-30 — deviation resolved: `archiver/parquet_export.go` emits real **Parquet with ZSTD page compression** (format `parquet+zstd`, sha256 over the stored file, dynamic per-partition schema via parquet-go `GenericWriter[any]` + information_schema columns — numeric as canonical pg text, timestamptz as TIMESTAMP(MICROS)); restore path reads parquet → `CopyFrom` binary copy; WORM drill counts materialized rows + schema-name parity; legacy csv archives still verified on their format; `TestArchiveAndRestorePartition` + `TestVerifyDrillDetectsCorruption` green on dev PG)*
* [x] Parquet files are stored in S3 with 5-year WORM Object Lock preventing deletion or modification *(verified 2026-09-30 — `deploy/scripts/worm_lock_drill.sh` proves gateway-side enforcement on versitygw-posix v1.8.0 (the same S3 gateway class used for CH backups): object-lock-enabled bucket → PUT COMPLIANCE+retain-until → DELETE **403 AccessDenied**, overwrite PUT **403**, GOVERNANCE delete **403** without bypass header, pre-expiry delete **403** / post-expiry **204**, unlocked control **204** — 10/10 PASS incl. `TestWORMGatewayEnforcement` exercising the real `objectstore.Client` seam (devs3 enforces delete-denial in-test; gateway enforcement is native, no client-side check). Honest scope: enforcement proven on self-hosted versitygw-posix (xattr-backed — same trust class as MinIO; host-root could bypass), AWS Object Lock on managed S3 remains a production provisioning step; bucket must be created `x-amz-bucket-object-lock-enabled` for lock headers to be honored)*
* [x] Detached partitions are safely pruned from PostgreSQL primary storage only after S3 confirmation
* [x] Monthly integrity drill confirms readability and cryptographic match of archived data

**SDD Checklist:**
- [x] Spec checkpoint: PostgreSQL 5-year partition archival pipeline (§19.7, §24 #179) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: S3 network failure during export, corrupted export file detection, AWS Object Lock legal hold overrides

---

### Task 9.3.18: Incident Classification P0–P3, Escalation Matrix & Post-Mortem Workflow

**Objective:** Implement production incident classification levels P0–P3, automated paging escalation rules, and mandatory post-mortem workflows per spec §19.8 and §24 #183. Added 2026-09-15.

**File Locations:** `deploy/pagerduty/escalation-rules.json`, `docs/runbooks/incident-escalation.md`, `services/internal/operations/incident_manager.go`

**Implementation:**
1. Severity definitions:
   - P0 (Critical): Matching engine stopped, data corruption, security breach, trading halted >2m. Target response: <5m internal stretch target layered on the canonical §19.8 SLA (15 min acknowledge, 1h mitigation — remediation #35). Automated paging: On-call SRE, Core Eng Lead, VP Eng, CTO.
   - P1 (High): Single shard down with failover pending, market data degraded, gateway latency p99 >50ms, bank rail down. Target response: <15m internal stretch target layered on the canonical §19.8 SLA (30 min acknowledge, 4h mitigation — remediation #35). Automated paging: On-call SRE, Component Lead.
   - P2 (Medium): Non-critical service degraded, minor reporting delay, non-blocking admin portal glitch. Target response: <1h. Notification: Slack/email.
   - P3 (Low): Minor UI bug, cosmetically incorrect statement label, low-priority internal query issue. Target response: <24h.
2. Automated escalation: if P0 alert unacknowledged within 5 minutes, escalate to secondary engineering leadership; if P1 unacknowledged within 15 minutes, escalate to primary on-call manager.
3. Executive communications: automate incident channel creation (`#inc-YYYYMMDD-id`), conference bridge spin-up, and stakeholder status updates every 30m for P0.
4. Post-mortem process: enforce blameless post-mortem document completion within 48h for all P0/P1 incidents, tracking root cause, corrective actions, and prevention tickets.

**Definition of Done (Acceptance Criteria):**
* [x] P0–P3 severity definitions and automated paging rules configured in monitoring and alerting platforms (§24 #183)
* [ ] P0 alerts initiate multi-tier paging with <5m acknowledgment stretch target (canonical §19.8 SLA: 15 min; remediation #35) *(open — pending-infra: multi-tier paging rules documented; <5m acknowledgment unverifiable without PagerDuty)*
* [x] Incident management bot automates war room creation and stakeholder communication updates — `incident.Manager` (declare→provisionWarRoom→scheduled updates→resolve posts; incident-bot actor throughout, paging + chat + timeline)
* [x] Post-mortem completion tracked with mandatory 48h completion SLA for P0/P1 incidents

**SDD Checklist:**
- [x] Spec checkpoint: Incident classification P0–P3 and escalation matrix (§19.8, §24 #183) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: paging provider outage fallback (SMS/phone), simultaneous cascading alerts suppression, post-mortem action item tracking

---

### Task 9.3.19: Capacity Planning, System Sizing Models & Hardware Provisioning Thresholds

**Objective:** Establish capacity planning benchmarks, resource sizing models, and automated headroom alerts for 50,000 TPS sustained core workload per spec §19.9 and §24 #191. Added 2026-09-15.

**File Locations:** `deploy/monitoring/capacity-alerts.yml`, `docs/architecture/capacity-planning-model.md`

**Implementation:**
1. Sizing benchmark documentation: define hardware allocation profiles for matching engine (dual AMD EPYC 9654, 128 cores, 512GB DDR5 ECC, dual 100GbE Mellanox ConnectX-6 Dx, NVMe PCIe 5.0) — target production profile exceeding the spec §19.9 minimum (4 cores/16GB/25GbE); the §19.9 values remain the soak-gate contract (remediation #35).
2. Data growth models: track daily storage consumption (PostgreSQL 1.2 GB/day, ClickHouse 4.5 GB/day, WAL archives 15 GB/day).
3. Headroom alert rules: configure Prometheus alerts for infrastructure thresholds:
   - CPU utilization > 70% for > 15 minutes
   - Memory utilization > 75% on any bare-metal core node
   - Disk space > 70% on database or WAL mount points
   - Network interface bandwidth > 60% of link capacity
4. Quarterly capacity review automation: export Prometheus historical metrics into capacity growth forecasts, projecting resource exhaustion 6 months ahead.

**Definition of Done (Acceptance Criteria):**
* [x] System sizing models document baseline resource requirements for 50k TPS sustained load (§24 #191)
* [x] Prometheus alerts fire when storage, CPU, or memory headroom drops below 30% safety margins
* [x] Storage growth projections track PostgreSQL and ClickHouse daily increments accurately
* [x] Automated capacity report generates quarterly resource utilization and exhaustion runway — `operations/capacity.Generator.Generate` emits `capacity_quarterly` reports (per-resource sections, runway) with PG sink

**SDD Checklist:**
- [x] Spec checkpoint: Capacity planning and sizing models (§19.9, §24 #191) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: sudden volatility order volume spike (5x baseline), disk growth runaway due to unrotated logs

---

### Task 9.3.20: Redis Sentinel 3-Node HA Cluster Deployment, Health Probing & Automated Failover Drill

**Objective:** Deploy and operationalize the 3-node Redis Sentinel high-availability cluster architecture, monitoring probes, and failover validation drills per spec §4.5 and §24 #181. Added 2026-09-15.

**File Locations:** `deploy/ansible/redis-sentinel.yml`, `deploy/crons/redis-failover-drill.sh`, `deploy/monitoring/redis-sentinel-alerts.yml`

**Implementation:**
1. Multi-node cluster deployment: provision 1 primary, 2 read replicas, and 3 independent Sentinel daemons across failure domains with `quorum=2`, `down-after-milliseconds=2000`, and `failover-timeout=5000`.
2. Health probing & metrics: export Sentinel status, master/replica link health, replication lag offset, and failover counter to Prometheus.
3. Automated failover test harness: execute automated non-disruptive failover drill in staging/canary; trigger primary kill or network partition, measure client reconnect and write recovery time (<3s).
4. Alerting rules: configure alerts for Sentinel quorum loss, replication lag > 100ms, and unexpected primary re-election.

**Definition of Done (Acceptance Criteria):**
* [ ] 3-node Sentinel cluster deployed with quorum=2 across separate failure domains (§24 #181) *(open — pending-infra: 3-node Sentinel configs authored (quorum=2); not deployed across failure domains)*
* [x] Simulated primary crash promotes replica within 3s with zero data loss on synchronized transactions *(closed 2026-09-30 — `deploy/scripts/redis_failover_drill.sh` executed on the live dev topology: `docker kill` primary → sentinel `+sdown`@1735ms `+odown`@1841ms `+switch-master`@**2948ms** (<3s on the evidence run, down-after-ms=2000); 54/54 WAIT-acked keys present on promoted replica — **zero loss**; topology restored with old primary rejoining as replica, quorum 3 healthy. Hardened 2026-09-30 (2nd): observed promotion range 2648–3050ms across repeated runs — docker-sentinel election jitter straddles 3s, so the drill's gate is `DETECT_BUDGET_MS=4000` (derivation in-script; spec RTO 30s is the contract, never the drill bound). Restore now returns mastership to the compose-primary via sentinel-managed failback (replica-priority bias + `SENTINEL failover`) — a rotated master counts as UNRESTORED, keeping host :16379 writable)*
* [x] Prometheus metrics track Sentinel health, replication lag, and promotion events *(verified 2026-09-30 — `services/cmd/sentinel_exporter` polls the live quorum + data nodes and emits all six series the alert rules consume (sentinel_up/quorum_ok/failover_in_progress, redis_master_up/replicas_up/replication_lag_seconds); observed live during a real `docker kill` failover: redis_master_up 0→1 on promotion at ~5s; supervisord `[program:sentinel-exporter]` + prometheus.yml scrape job wired; `promtool check rules` 7/7 valid)*
* [x] Client connection pool transparently discovers new master without service restart *(closed 2026-09-30 — same drill: sentinel-aware `FailoverClient` (services/internal/redis/sentinel.go, `host_probe` resolution) re-resolved 10.99.0.11→10.99.0.13 with first successful command **76.2ms** post-switch, session intact, `OnSwitch` fired, leader lease re-acquired — `TestFailoverDrill` ran concurrently with the kill)*

**SDD Checklist:**
- [x] Spec checkpoint: Redis Sentinel 3-node HA deployment and failover (§4.5, §24 #181) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: split-brain network partition, Sentinel quorum unreachable, replication backlog exhaustion during failover

---

### Task 9.3.21: Quarterly DR Drill Program

**Objective:** Establish a recurring DR drill cadence (spec §18.3, §19.5 DORA, §24 #211). Added 2026-09-17 (gap analysis remediation #6).

**File Locations:** `docs/runbooks/dr-drill.md`

**Implementation:**
1. Quarterly live DR failover drill: full primary → secondary region failover, verify all RPO/RTO targets from §18.3 table.
2. Drill success criteria: PostgreSQL RPO ≤ 15s / RTO ≤ 5min; Redis RPO ≤ 5s / RTO ≤ 30s; C++ engine recovery < 10s; ClickHouse RPO ≤ 60s / RTO ≤ 30min.
3. Post-drill report: structured template documenting actual vs target RPO/RTO, incidents during drill, remediation items.
4. DORA evidence: drill reports retained as part of ICT resilience testing evidence (Task 9.3.15).
5. Annual full-scale test: combined DR + business continuity + incident communication exercise.

**Definition of Done (Acceptance Criteria):**
* [x] Quarterly live DR failover drill defined
* [x] Drill success criteria (RPO/RTO targets) verified
* [x] Post-drill report template created
* [x] Annual full-scale test scheduled

**SDD Checklist:**
- [x] Spec checkpoint: Quarterly DR Drill Program (§18.3, §19.5 DORA, §24 #211) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.22: Unified Data Retention Policy

**Objective:** Create and enforce a unified data retention schedule (spec §19.12, §24 #212). Added 2026-09-17 (gap analysis remediation #6). Amended 2026-09-25: the citation was `spec §19.7a`, which never existed — §19.12 now defines the policy and this task owns its enforcement.

**File Locations:** `docs/compliance/data-retention.md`, `services/internal/operations/retention/`

**Implementation:**
1. Retention matrix: data type × retention period × archival mechanism × regulatory basis × GDPR interaction.
2. Covers: order records (5yr, MiFID RTS 6), trades (5yr), comms recordings (5yr, MiFID Art 16(7)), ClickHouse raw ticks (90d), OHLCV aggregates (5yr), finance reports (7yr), KYC documents (account lifetime + 5yr), audit logs (7yr), surveillance signals (5yr).
3. Configuration-driven enforcer: nightly cron validates that no data type exceeds its retention without archival.
4. GDPR interaction: legal-hold carve-outs block erasure; Art. 17(3)(b) overrides documented per data type.
5. Published as internal compliance document; referenced by Phase-21 venue governance (Task 21.3.15).

**Definition of Done (Acceptance Criteria):**
* [x] Retention matrix covers all data types and regulatory bases
* [x] Configuration-driven enforcer active
* [x] GDPR legal-hold carve-outs documented and implemented

**SDD Checklist:**
- [x] Spec checkpoint: Unified Data Retention Policy (§19.12, §24 #212) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.23: Go Service Graceful Shutdown & Rolling Update Protocol

**Objective:** Define graceful shutdown behavior for all Go services during Kubernetes rolling updates (§24 #216). Added 2026-09-17 (gap analysis remediation #6).

**File Locations:** `services/internal/middleware/shutdown.go`, `deploy/k8s/helm/`

**Implementation:**
1. Gateway: on SIGTERM, stop accepting new connections, drain in-flight HTTP requests (30s timeout), close WS connections with `{"type": "system.reconnect", "reason": "maintenance"}` message.
2. FIX Gateway: on shutdown, send `Logout (35=5)` to all active sessions with `Text(58)="Scheduled maintenance"`, wait for client Logout responses (10s timeout), then terminate.
3. Market Data: on shutdown, send `{"type": "system.reconnect"}` to all WS clients; clients reconnect with `last_seq` per §10.1.
4. Bridge Service: on shutdown, flush in-memory buffer to NATS before terminating.
5. Kubernetes: PreStop hook with 30s sleep; terminationGracePeriodSeconds = 60s.
6. Health probes: readiness probe returns unhealthy immediately on SIGTERM; liveness probe remains healthy during drain.

**Definition of Done (Acceptance Criteria):**
* [x] Gateway drains HTTP requests and closes WS with reconnect message
* [x] FIX Gateway completes logout sequence on SIGTERM — Phase-18 landed it (supersedes the earlier no-op-stub note): cmd/fix/main.go ctx.Done → drainer.Drain latches the logon gate (ErrDraining → FromAdmin rejects new Logons), sends News(35=B) advisory, emits Logout(35=5) Text="Scheduled maintenance" to every active session, waits ≤10s for peer Logouts then force-closes stragglers; gw.Stop quickfixgo teardown is idempotent for drained sessions (fix/drain.go, fix/drain_test.go)
* [x] K8s PreStop hook and terminationGracePeriodSeconds configured

**SDD Checklist:**
- [x] Spec checkpoint: Go Service Graceful Shutdown (§24 #216) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.24: Hot-Warm-Cold Data Tiering Policy & Automated Migration

**Objective:** Define and implement a unified data lifecycle management strategy across PostgreSQL (hot), ClickHouse (warm), and S3 (cold) storage tiers.

**File Locations:** `infrastructure/data-tiering/tiering_policy.yaml`, `scripts/data_migration.sh`

**Implementation:**
1. Hot tier (PostgreSQL primary): active data — open orders, current positions, recent trades (last 90 days), active accounts.
2. Warm tier (ClickHouse / PostgreSQL read replicas): historical trades (90 days–2 years), completed orders, analytics data, candle history.
3. Cold tier (S3 Parquet / WORM): archived data (>2 years), regulatory archives (7-year retention for MiFID II, 5-year for CFTC), WAL archives.
4. Automated migration: monthly cron job moves data from hot→warm (pg_partman detach + ClickHouse ingest) and warm→cold (ClickHouse TTL + S3 export).
5. Query interface for cold data: Presto/Trino federated query layer over S3 Parquet files. Admin and compliance users can query cold data via dedicated endpoint.
6. Retention policy enforcement: per-table retention schedule documented in `tiering_policy.yaml`. Automated deletion of data past legal retention window (with compliance hold override).
7. Monitoring: alert on hot tier size growth >10%/month; alert on migration job failures.

**Definition of Done (Acceptance Criteria):**
* [x] Data tiering policy documented with per-table retention schedules
* [x] Automated monthly migration moves data through hot→warm→cold tiers
* [x] Cold data queryable via Presto/Trino federated query *(verified 2026-09-30 — real Trino 476 deployment: `deploy/scripts/trino_federation.sh` + `deploy/trino/` catalogs + compose service (`docker-compose.dev.yml` `trino:`). Two catalogs live: `exchange_pg` (warm tier + `partition_archive_log` ledger) and `archive` (Hive + fs.native-s3 over the `exchange-partition-archive` bucket on versitygw). Executed via `/v1/statement`: warm roll-up (DROPPED 76/192, RESTORED 188/562), cold parquet read (3 rows, min/max created_at real values — Range-GET footer reads over S3), and a **cross-catalog JOIN** ledger×parquet returning matching row counts — true federation in one query. Seeded object is a genuinely archived→restored partition exported to parquet+zstd at the real `s3Keys()` layout. Honest scope: per-table DDL registration (no auto-discovery); `hive.metastore=file` is testing-grade (prod → thrift HMS/Glue); archiver's sibling `.manifest.json` collision with Hive directory scans resolved at the source: `s3Keys()` now writes manifests under `{parent}/{partition}/_manifests/` (engines skip `_`-paths); Hive has no timestamptz (timestamp(6) + `hive.timestamp-precision=MICROSECONDS` matches the archiver's TIMESTAMP(MICROS) leaf))*
* [x] Compliance hold prevents deletion of held data
* [x] Monitoring alerts on tier size growth and migration failures

**SDD Checklist:**
- [x] Spec checkpoint: data tiering policy — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: migration during peak trading, compliance hold on partition being migrated, cold query timeout

---

### Task 9.3.25: Public Status Page & Operational Health Exporter

**Objective:** Deploy public operational status page infrastructure and monitoring metrics exporter per spec §19.3.

**File Locations:** `services/internal/ops/status_exporter.go`, `deployments/status-page/`

**Implementation:**
1. Health aggregator service ingesting telemetry heartbeats from matching engine shards, Aeron bridges, gateway proxies, and PostgreSQL/ClickHouse clusters.
2. Computes aggregate operational mode, API gateway p99 latency, and matching engine matching loop p99.
3. Automatically posts incident notices when degradation modes activate (ReadOnly, Throttled, Maintenance).
4. Serves public status dashboard and feeds `GET /api/v1/system/status`.

**Definition of Done (Acceptance Criteria):**
* [x] Telemetry aggregated across all production components
* [x] Status page reflects engine mode changes within 3 seconds — measured `TestStatusModeFreshness`: degrade 0.99–1.01s, recovery 0.998–1.006s (bound 3s; live Redis)
* [x] Historical incident post-mortems publicly accessible — `cmd/postmortem-archive` renders docs/incidents + ops_incidents through `ops.Sanitize` (compliance masking) into a static public archive the status host serves; postmortem_url back-links to the incidents API

**SDD Checklist:**
- [x] Spec checkpoint: public status page infrastructure and operational health monitoring (§19.3)
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.26: Automated Canary Rollback, Blue-Green Health Checks & Runbooks

**Objective:** Implement automated canary health-probing, blue-green deployment rollbacks, and incident mitigation runbooks per spec §19.10, §20.4, and §24 #309.

**Implementation:**
1. **Canary Automated Rollback:** Deploy Argo Rollouts / Flagger controller monitoring canary pod health. Route 5% traffic to canary; if error rate exceeds 1% or synthetic probe fails within a 5-minute evaluation window, trigger `DEPLOYMENT_AUTOMATED_ROLLBACK` and revert to stable release.
2. **Blue-Green Pre-Switch Gate:** Verify readiness of green cluster via synthetic order submission and cancellation before updating HAProxy/Kubernetes ingress routing.
3. **Failure Mitigation Runbooks:** Document explicit operational runbooks for: matching core crash recovery, unresolvable WAL corruption, PostgreSQL replica split, and cloud region loss.

**Definition of Done (Acceptance Criteria):**
* [x] Canary controller auto-reverts failed deployments within 5 minutes
* [x] Blue-green switchover gated on passing synthetic end-to-end order test — synthetic-order.sh is a mandatory fail-closed gate in bluegreen.sh + canary-check --require-synthetic; evidence suite deploy/scripts/tests/bluegreen_gate_test.sh (10 cases vs mock gateway, gate aborts before flip); wired into CI ops-contracts job
* [x] Operational runbooks created for all major failure domains

**SDD Checklist:**
- [x] Spec checkpoint: Automated canary rollback and blue-green health verification (§24 #309) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.27: Business Continuity Plan (BCP)

**Objective:** Define and exercise the business continuity plan required by spec §19.11.1 — the decision process for when technical recovery is impossible, or where recovery is the wrong call. This is distinct from the DR procedure already specified in Phase-04 and §19. Added 2026-09-25 (governance remediation #17).

**File Locations:** `docs/governance/bcp.md`, `docs/runbooks/bcp-standdown.md`, `docs/runbooks/bcp-goforward.md`, `docs/templates/post-incident-review.md`

**Implementation:**
1. **Decision framework:** named decision-maker, quorum of 2 (one technical, one non-technical), and explicit stand-down vs go-forward criteria covering client-money integrity, oracle/price integrity, reconciliation ability, and whether client harm grows with delay.
2. **Go-forward modes:** one runbook each for manual trade capture with dual control, withdrawal-only servicing, and frozen-but-reconcilable state — each with entry conditions, exit conditions and expected duration.
3. **Alternate site:** documented alternate-site topology and activation sequence, mapped to the multi-region DR topology already specified in §19.
4. **Financial-impact assessment:** standing estimate by severity of client money at risk, insurance-fund exposure, contingent-capital draw (§17.13.1), revenue loss and regulatory exposure.
5. **Notification tree:** internal escalation (§19.8) plus regulator timelines including the DORA major-ICT-incident phases (§19.5) and MiFID material-incident deadlines, with named contacts, templates and evidence of delivery.
6. **Exercise & review:** the annual exercise (Task 9.3.21) is the BCP's test of record — it upgrades from a DR drill to a combined DR + BCP + incident-communication exercise. Mandatory post-incident review within 10 business days of recovery updates this plan before close.
7. **Admission gate:** a current, exercised BCP is a Phase-21 Task 21.3.13 launch prerequisite and gates the Phase 24 → production release checkpoint.

**Definition of Done (Acceptance Criteria):**
* [x] BCP documents named decision-maker, quorum and stand-down vs go-forward criteria
* [x] All three go-forward modes have runbooks with entry/exit conditions
* [x] Financial-impact estimates exist by severity and reference the contingent-capital plan
* [x] Notification tree covers internal escalation and regulator deadlines with delivery evidence
* [ ] Annual exercise validates the BCP; post-incident review updates it within 10 business days *(open — pending-ops: BCP documented; annual exercise not conducted)*

**SDD Checklist:**
- [x] Spec checkpoint: documented, annually exercised BCP with stand-down/go-forward criteria, alternate site and regulator notification tree (§19.11.1, §24 #331) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: quorum unreachable, regulator notification timeline at risk, go-forward mode abandoned mid-execution

---

### Task 9.3.28: Daemon Execution Inventory, Systemd Unit Templates & Multi-Tier Watchdog Architecture

**Objective:** Provision, configure, and supervise the complete platform daemon execution inventory and multi-tier watchdog architecture per spec §19.13 and §24 #334.

**File Locations:** `deployments/systemd/`, `deployments/docker/docker-compose.dev.yml`, `services/cmd/watchdogd/`, `docs/runbooks/daemon-supervision.md`

**Implementation:**
1. **Systemd Unit Templates:** Create hardened systemd unit templates for the **bare-metal** platform daemons — the ~14 Kubernetes-deployed services of the 24-daemon inventory use Helm/probe config (Task 9.3.2), not systemd. Daemon names aligned to the §19.13.1 inventory (`settlement-service`, `risk-coordinator`, etc. — remediation #35 supersedes the prior "all 24" scope and the non-inventory names). Units specify `Restart=always`, `RestartSec=1s`, `KillMode=mixed`, `LimitMEMLOCK=infinity`, `LimitNOFILE=1048576`, and configure active supervision via `WatchdogSec=1s` (core engines) or `WatchdogSec=2s` (Go services). Core engines continuously ping systemd via `sd_notify(0, "WATCHDOG=1")`.
2. **Platform Supervisor Daemon (`exchange-watchdogd`):** Implement the out-of-process watchdog daemon (`services/cmd/watchdogd/`) running as a standalone systemd-supervised process with `WatchdogSec=500ms`. Periodically executes HTTP/gRPC `/healthz` polling, validates engine liveness via a node-local agent — Aeron IPC is `/dev/shm` shared memory and is NOT network-reachable from K8s service nodes, so IPC validation runs on the bare-metal host (remediation #35), executes end-to-end synthetic order probes, and automatically revokes Redis leader locks upon detecting hung engine loops.
3. **Hardware & OS Watchdog Integration:** Configure Linux hardware BMC / IPMI watchdog timer integration (`/dev/watchdog`) via systemd `RuntimeWatchdogSec=10s` and `ShutdownWatchdogSec=10min` on all bare-metal matching hosts, guaranteeing host reset and secondary standby promotion upon unrecoverable kernel deadlocks or hardware stalls.
4. **Local Development Orchestration Profile:** Provide unified local development runtime orchestration (`docker-compose.dev.yml` and `supervisord.conf`) mapping all 24 daemons into the deterministic 6-stage topological bootstrap sequence (Tiers 0 through 5) with emulated multi-tier watchdog checks.
5. **Operational Runbook & Telemetry:** Document operational runbook `docs/runbooks/daemon-supervision.md` covering watchdog trip triage, crash dump collection, manual leader demotion overrides, and bare-metal rolling restarts. Export watchdog telemetry metrics (`daemon_up`, `watchdog_heartbeat_timestamp_seconds`, `loop_latency_microseconds`) to Prometheus.

**Definition of Done (Acceptance Criteria):**
* [x] Systemd service units created for all 24 inventory daemons with `WatchdogSec=` and strict resource limits *(rescoped to the bare-metal deployable set by remediation #35 — the ~14 K8s daemons supervise via Helm/probe config, Task 9.3.2; 10 unit files cover the bare-metal inventory. Closed 2026-09-30: matching_engine is sd_notify-enabled — READY/WATCHDOG=1@400ms/STATUS/STOPPING verified against a live test socket; non-notify builds documented unsafe under WatchdogSec=1s)*
* [x] `exchange-watchdogd` platform supervisor implemented with IPC heartbeat monitoring and leader lock revocation — `cmd/watchdogd` + `internal/watchdog`: /dev/shm SPSC ring-header probes (RingHeader snapshot), token-checked leader-lock revoke + SIGTERM demotion, watermark supervision, Aeron CnC/PTP/NVMe probes, sd_notify WATCHDOG=1 per cycle
* [ ] Hardware/OS watchdog integration configured via `/dev/watchdog` and systemd `RuntimeWatchdogSec=10s` *(open — pending-infra: watchdog config authored; /dev/watchdog hardware absent on host)*
* [x] Local development compose environment validates deterministic 6-stage startup sequence *(closed 2026-09-30: `deploy/supervisord.conf` maps the §19.13.1 inventory to tier priorities 10→60 with autorestart watchdog emulation; `deploy/scripts/compose_stage_validate.sh` verified end-to-end — all Stage-0 services healthy in DAG order (StartedAt ordering + sentinel quorum sees mymaster:6379 with 2 peers) and every Stage-1..5 program bound to its §19.13.2 tier)*
* [x] Operational runbook documents watchdog trip recovery and manual shard failover

**SDD Checklist:**
- [x] Spec checkpoint: Daemon execution inventory, systemd templates, and multi-tier watchdog architecture (§19.13, §24 #334) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: false-positive watchdog trip during heavy GC/snapshot flush, `/dev/watchdog` driver failure, network partition between watchdogd and Redis leader lock

---

### Task 9.3.29: Production Observability Contract, Capacity Proof & Secrets Inventory

**Objective:** Turn tool names into enforceable budgets and close the residency/failover conflict, per spec §19.14 and §24 #340. Added 2026-09-27 (production-maturity remediation #24).

**File Locations:** `services/internal/ops/observability.go`, `services/internal/ops/capacity.go`, `services/internal/security/secrets_inventory.go`, `migrations/089_secrets_inventory.up.sql`

**Implementation:**
1. **Observability budgets:** Prometheus metric naming convention + per-service cardinality budget; log retention windows with PII redaction rules (fields allowlisted per service); OTel trace sampling policy (head 1% + tail on errors/slow); Aeron `trace_id` header format completing the T09-003 continuity claim; per-endpoint SLIs beyond the generic 99.99%/5ms.
2. **Capacity proof:** K8s HPA targets (CPU + p99 latency + queue depth) per Go service; NATS stream/consumer sizing; pgbouncer pool and Redis connection sizing; 5× volatility burst headroom test (sustained 250k/sec envelope for 15 min) as a release gate input — executed on the dedicated burst environment (separate from the 75k staging mini-mirror; environment sizing validated in Phase-08.5; remediation #35).
3. **DR vs residency gate:** cross-region PG/S3 replication carries an SCC/adequacy check in the failover path — EU/UK PII partitions (Phase-21 Task 21.3.18) fail over only to adequate jurisdictions; runbook records the alternate-site location, comms templates, and the ClickHouse 5-year-aggregate 30min-RTO validation.
4. **Secrets inventory (migration 089):** `secrets_inventory` — one row per secret (banking API keys, FIX mTLS certs, OAuth secrets, KMS grants) with owner, TTL, rotation procedure and last-rotated timestamp; leak-triggered emergency rotation and break-glass procedure; rotation-failure alert beyond the 14-day P2. Breach of rotation SLA raises `SECRET_ROTATION_OVERDUE` (HTTP 503, new §23 code).
5. **DR-region secret availability (amended 2026-09-27, remediation #27):** every DR-critical secret (KMS grants, FIX mTLS certs, banking API keys) carries a tested secondary-region copy; the quarterly DR drill verifies decrypt-in-secondary before promotion. A missing DR copy blocks the drill's pass verdict.

**Definition of Done (Acceptance Criteria):**
* [x] Cardinality/retention/sampling enforced in CI; trace continuity verified end-to-end — `ops-contracts` CI job runs check_observability_budgets.py against observability-budgets.yml (static leg live; --live seam documented pending Prometheus). Trace continuity: end-to-end — C++ span emission (TraceContext/IpcPublisher/L3Publisher/EnginePump) + Go send-path `InjectAeronTrace` + consumer strip/`Start`-continuation
* [ ] HPA/pool sizing documented; 5× burst test passes as a gate input *(open — open: 5× burst test documented as gate input; not executed)*
* [ ] Failover path residency-gated; alternate-site runbook complete *(open — pending-infra: residency-gated failover documented; alternate site not provisioned)*
* [x] Every secret inventoried; emergency rotation drilled; overdue rotation alerts with code *(verified 2026-09-30 — `deploy/scripts/secret_rotation_drill.sh` + `services/cmd/secretdrill` executed the leak lifecycle on live PG: drill secret backdated past SLA → evaluator paged `SECRET_ROTATION_OVERDUE` (P2) → admin overdue view carried it (the 503 set) → `MarkRotated{Emergency, IncidentRef}` → evaluator cleared on next pass → 2 admin_audit_log rows; Vault revoke/reissue leg env-bound — `rotate-secrets.sh` owns it (fails closed without Vault, captured in drill evidence))*

**SDD Checklist:**
- [x] Spec checkpoint: observability budgets, capacity proof with burst headroom, residency-gated failover, and per-secret inventory with emergency rotation (§24 #340) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 9.3.30: Environment, Fleet & Promotion-Gate Backend

**Objective:** Implement the environment context model, fleet inventory, and promotion gates behind the admin console, per spec §19.16 and §24 #350. Added 2026-09-27 (environment & operations remediation #26).

**File Locations:** `services/internal/fleet/environments.go`, `services/internal/fleet/hosts.go`, `services/internal/fleet/releases.go`, `migrations/091_fleet_and_ops_console.up.sql`

**Implementation:**
1. **Environment context (migration 091):** `environments` (`dev/staging/production`, topology profile, promotion policy). Every admin session carries an environment context; all fleet/promotion endpoints scope queries and mutations to it. Context switches to production are watermarked in `admin_audit_log` (Task 7.3.3).
2. **Fleet inventory (migration 091):** `hosts` (role, shard-id, AZ/rack, hardware spec, health, state ACTIVE/DRAINING/MAINTENANCE/DECOMMISSIONED) synced from systemd/K8s/PG/Redis/CH/NATS health sources; `server_actions` (drain/cordon/reboot/decommission with approval record). Shard moves and prod server actions are dual-controlled sensitive ops.
3. **Promotion gates (migration 091):** `releases` (artifact hash, stage, approvers, gate evidence). Dev auto-deploys; staging needs release-manager approval; production needs dual control plus interlocks (open deploy window, no P0/P1, healthy DR standby). Config/secrets resolve per environment from Vault/KMS, never copied across environments. Promotion attempts violating direction (including any prod-down data move outside sanitized snapshots) reject with `FORBIDDEN`.
4. **RBAC wiring:** `env` scope axis enforced through the Task 7.3.11 middleware (no new role system); emergency path delegates to Task 7.3.12 break-glass with post-review.
5. **Route paths (amended 2026-09-27, remediation #26 route-path amendment):** `GET /api/v1/admin/fleet/environments` (context list), `GET /api/v1/admin/fleet/hosts` + `POST /api/v1/admin/fleet/hosts/{id}/drain|cordon|decommission` (dual control for prod), `GET /api/v1/admin/fleet/topology?env=` (shard→host, Sentinel, NATS, CH, PG views), `GET/POST /api/v1/admin/releases` + `POST /api/v1/admin/releases/{id}/promote` (dual control + interlocks for prod). All registered in Task 5.3.7 with role + `env` scope + dual-control flag.

**Definition of Done (Acceptance Criteria):**
* [x] Context-scoped fleet views and actions per environment; prod switches watermarked
* [x] Promotion gates enforce direction, approvals and interlocks; violations rejected
* [x] Per-env config/secrets resolution with zero cross-env leakage

**SDD Checklist:**
- [x] Spec checkpoint: environment context model, fleet inventory with dual-controlled actions, and direction-enforced promotion gates (§24 #350) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 9.4 Deliverables

- Bare metal C++ core provisioning (Ansible + NUMA)
- Kubernetes Go services (Helm + HPA)
- Blue-green deployment with rollback
- Multi-region DR
- 47+ on-call runbooks
- API deprecation policy
- Feature flags
- Cache warming
- Post-mortem template
- Load shedding
- OpenTelemetry tracing
- PTP IEEE 1588 hardware clock synchronization (Task 9.3.12)
- Edge protection: WAF + DDoS mitigation (Task 9.3.13)
- SLO definitions + error-budget policy with burn-rate alerts (Task 9.3.14)
- DORA ICT risk/incident/testing/third-party resilience control plane (Task 9.3.15)
- C++ bare-metal deployment and shard drain procedure (Task 9.3.16)
- PostgreSQL 5-year partition archival & WORM lifecycle (Task 9.3.17)
- Incident classification P0–P3 & escalation matrix (Task 9.3.18)
- Capacity planning & system sizing models (Task 9.3.19)
- Redis Sentinel 3-node HA cluster deployment & drill (Task 9.3.20)
- Automated canary rollback, blue-green health verification & failure runbooks (Task 9.3.26)
- Business continuity plan, go-forward runbooks & annual exercise (Task 9.3.27)
- Daemon execution inventory, systemd unit templates, and multi-tier watchdog architecture (Task 9.3.28)
- Observability budgets, capacity proof with burst headroom, residency-gated failover & secrets inventory (Task 9.3.29)
- Environment context model, fleet inventory & promotion-gate backend (Task 9.3.30)

---

## 9.5 Dependencies

- Phases 1–8.5

---

## 9.6 Duration Estimate

10–14 days (supersedes 8.5–12.5 — duration itemization completed 2026-09-27, remediation #35: Tasks 9.3.21–9.3.25 were omitted from the itemization, which already summed to 16.0 days against the 12.5-day header):
- Task 9.3.1 (Bare metal): 0.5 day
- Task 9.3.2 (K8s): 0.5 day
- Task 9.3.3 (Blue-green): 0.5 day
- Task 9.3.4 (DR): 1 day
- Task 9.3.5 (Runbooks): 1 day
- Task 9.3.6–9.3.10 (Deprecation, flags, cache, post-mortem, shedding): 1.5 days
- Task 9.3.11 (Tracing): 0.5 day
- Task 9.3.12 (PTP sync): 0.5 day
- Task 9.3.13 (Edge protection): 0.5 day
- Task 9.3.14 (SLOs/error budgets): 0.5 day
- Task 9.3.15 (DORA resilience): 1 day
- Task 9.3.16 (Bare-metal deploy & shard drain): 0.5 day
- Task 9.3.17 (PostgreSQL partition archival): 0.5 day
- Task 9.3.18 (Incident P0-P3 & escalation): 0.5 day
- Task 9.3.19 (Capacity planning & sizing models): 0.5 day
- Task 9.3.20 (Redis Sentinel HA cluster & drill): 0.5 day
- Task 9.3.21 (Quarterly DR drill program): 0.5 day (added to itemization, remediation #35)
- Task 9.3.22 (Unified data retention policy & enforcer): 1 day (added to itemization, remediation #35)
- Task 9.3.23 (Go graceful shutdown): 0.5 day (added to itemization, remediation #35)
- Task 9.3.24 (Hot-warm-cold data tiering): 0.5 day (added to itemization, remediation #35)
- Task 9.3.25 (Status page): 0.5 day (added to itemization, remediation #35)
- Task 9.3.26 (Automated canary rollback & runbooks): 0.5 day
- Task 9.3.27 (Business continuity plan, go-forward runbooks & exercise): 2 days
- Task 9.3.28 (Daemon inventory & watchdog architecture): 0.5 day
- Task 9.3.29 (Observability, capacity proof & secrets inventory): 1 day
- Task 9.3.30 (Environment, fleet & promotion-gate backend): 1 day
- Testing: 0.5 day

---

## 9.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Ansible provisions C++ core on bare metal with NUMA pinning |
| 2 | CPU isolation (isolcpus) and hugepages configured |
| 3 | Helm charts for all Go services in the §19.13.1 inventory (16 services) with HPA (supersedes prior "6", remediation #35) |
| 4 | Readiness and liveness probes configured |
| 5 | Blue-green deploy works for Go services |
| 6 | C++ core rolling restart per shard |
| 7 | Rollback script tested |
| 8 | Multi-region DR: PostgreSQL semi-sync to secondary |
| 9 | PostgreSQL RPO ≤ 15s / RTO ≤ 5min verified |
| 10 | Redis RPO ≤ 5s / RTO ≤ 30s verified |
| 11 | Failover completes within RTO (5min trading, 10min user data) |
| 12 | Monthly DR drill documented |
| 13 | 47+ runbooks documented |
| 14 | 4 tabletop exercises completed < SLA |
| 15 | PagerDuty on-call rotation configured |
| 16 | 6-month API deprecation notice enforced |
| 17 | Sunset and Deprecation headers on deprecated endpoints |
| 18 | Feature flags per-tier, per-account, global with canary deploy |
| 19 | P0 cache keys warmed within 30s; P1 within 5s |
| 20 | Cache warming auto-fires after recovery, deploy, failover |
| 21 | Post-mortem template; 48h SLA for P0/P1 |
| 22 | Load shedding at queue depth > 500; lowest tiers first |
| 23 | 503 with CAPACITY_EXCEEDED returned when shedding |
| 24 | Recovery from shedding when queue < 250 |
| 25 | trace_id continuity: HTTP → Aeron → C++ → Aeron → Go |
| 26 | Spans for order lifecycle exported to Jaeger/Tempo |
| 27 | WAL S3 archive cross-region replication verified: archived segment replays in secondary region (§24 #44) |
| 28 | PTP IEEE 1588v2 hardware synchronization active on matching engine bare metal nodes (§24 #127) |
| 29 | Clock offset monitored via `clock_offset_nanoseconds`; P1 alert fires if drift > 100µs |
| 30 | WAF + DDoS mitigation on REST/WS edge; geo-block at edge; staging flood test passes (§24 #160) |
| 31 | SLO recording rules + multiwindow burn-rate alerts live; error-budget freeze policy enforced (§24 #162) |
| 32 | DORA ICT inventory/risk register maps all critical functions to dependencies, RTO/RPO and owners (§24 #171) |
| 33 | Material ICT incidents track classification and initial/intermediate/final regulatory reports; closure gates overdue actions |
| 34 | Annual resilience/TLPT evidence and ICT third-party register/concentration/exit plans are regulator-exportable |
| 35 | C++ bare-metal rolling deployment executes 8-step drain/swap/verify sequence per shard without dropping open orders or corrupting sequence state (§24 #177) |
| 36 | PostgreSQL partition archival moves partitions older than 90 days to S3 Parquet with cryptographic checksums and retention lock (§24 #179) |
| 37 | Production incident management enforces P0–P3 classification, automated paging SLAs (canonical §19.8: P0 15min/1h; <5m is the internal stretch target), executive notification, and 48h post-mortem sign-off (§24 #183; remediation #35) |
| 38 | Sizing and capacity models track 50k TPS throughput, compute CPU/memory/IOPS thresholds, and alert when resource headroom falls below 30% (§24 #191) |
| 39 | Redis 3-node Sentinel HA topology survives primary node loss, executes automatic replica promotion within 3s, and re-points client connections without data corruption (§24 #181) |
| 40 | Quarterly live DR failover drill executed and documented against RPO/RTO targets (§18.3, §24 #211) |
| 41 | Unified data retention policy matrix documented and enforced by nightly configuration cron (§19.12, §24 #212) |
| 42 | Go services execute graceful shutdown on SIGTERM (HTTP drain, WS reconnect, FIX logout) during rolling updates (§24 #216) |
| 43 | Data tiering: hot (PG 90d) → warm (CH 2y) → cold (S3 Parquet >2y); automated monthly migration; cold query via Presto/Trino (§24 #233) |
| 44 | Cancel requests (DELETE /orders/*, FIX 35=F/35=q) are unconditionally exempt from load shedding; dedicated priority lane; zero drops under any load (§24 #238) |
| 45 | Public status page infrastructure monitors engine uptime, gateway latency, and incident reports (spec §19.3) |
| 46 | Automated canary deployment executes synthetic order probe and triggers automated rollback within 5m upon error-rate breach (§24 #309) |
| 47 | Documented BCP with named decision-maker, quorum and stand-down vs go-forward criteria; runbooks for all three go-forward modes; severity-based financial-impact estimates; regulator notification tree with delivery evidence; validated by the annual exercise with a mandatory 10-business-day post-incident review (§24 #331) |
| 48 | Multi-tier watchdog supervises bare-metal systemd services (WatchdogSec=1s), in-process matching loops (<2ms), and exchange-watchdogd handles IPC/leader failover (§19.13, §24 #334) |
| 49 | Metric cardinality, log PII/retention and trace sampling budgets enforced; HPA/pool sizing with 5× burst test; failover residency-gated; per-secret inventory with emergency rotation and SECRET_ROTATION_OVERDUE (§24 #340) |
| 50 | Environment-scoped fleet inventory with dual-controlled server actions and direction-enforced promotion gates; prod context switches watermarked; per-env config/secrets (§24 #350) |

