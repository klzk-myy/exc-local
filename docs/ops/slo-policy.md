# SLO Definitions & Error-Budget Policy

**Phase-09 Task 9.3.14** · **Authority:** spec §19.3, §24 #162, §25 · **Wiring:** `deploy/prometheus/rules/exchange-alerts.yml` (`exchange.platform` group — burn alerts deployed; per-SLI recording rules `deploy/prometheus/slo_rules.yml` **pending**) · **Review:** monthly SLO review feeds post-mortems + reliability backlog.

## 1. SLO table

Windows: **30-day rolling** for all SLOs. Availability numerator is `exchange_errors_total{tier=~"L0|L1|L2"}` over `http_requests_total` (deployed rule); latency SLIs measure from the service's own histograms.

| SLO | Target | SLI (how measured) | Service surface | Measured today (docs/perf/phase08-tuning-report.md) |
|---|---|---|---|---|
| Availability — order API + matching | **99.99%** | 1 − (L0+L1+L2 errors / total requests), 30d | `gateway` :8080 + engine ingress | REST leg run1: **0 errors / 300,000 req** (600s); full-window SLO not yet measurable — pending staging soak |
| REST p99 latency | **≤ 5ms** | per-request histogram p99 over window | gateway REST routes | **6.3ms p99 under heavy contention** (300k req @500/s); **~2.3ms p99** in a quiet window; p50 226µs — host-marginal, expected pass on dedicated host |
| Tick-to-trade p99 | **≤ 50µs** (supersedes ≤1ms — remediation #35) | engine `LatencyHistogram` order-latency p99 | matching engine `matching_engine` | **NOT met under contention**: 110–207ms p99 (engine starved to ~5% CPU); transport-only p99 is healthy: shm ring **3.9µs**, Aeron **3.0µs** (`bench_ipc.sh`); demonstrated sustained ceiling **15,000 ord/s over ~5h** (Phase-02.5 soak), 50k/s target OPEN |
| WS delivery latency | **≤ 100ms** | publish→client frame latency | `marketdata` :8081 | WS leg: 120 conns, 46,320 frames, **0 seq gaps / 0 disconnects / 0 zero-frame conns** over 600s — delivery integrity proven; latency SLI instrumentation pending |
| Market-data freshness | seq continuity + conflation lag | `marketdata` seq mirror / frame age | marketdata + `bridge` | included in WS leg result above |
| Settlement ingest | ≥ 5,000 fills/s sustained | `internal/settlement` ingest bench | `settlement` :8083 | **~5,736 fills/s** measured (200k fills, 34.9s bench) |
| Bridge buffer push | n/a (capacity SLI) | `BenchmarkBufferPush` | `bridge` 9100+N | **39.2ns/op, 1 alloc** |

**Honesty note:** the 50k ord/s sustained / p99 ≤50µs criteria are NOT demonstrated on the shared measurement host (perf report §1/§3.3). SLOs above are the *contract*; the measured column records reality. Re-baseline on the dedicated benchmark host per perf report §11.

## 2. SLI conventions

- **Maintenance exclusion:** requests during a declared `Maintenance` window (`maintenance_windows`, migration 172) are excluded from the availability numerator/denominator. Undeclared downtime counts in full.
- **Partial-outage attribution:** per-`service` label breakdown in the burn alerts; a single-service outage burns budget at its request share, not 1.0.
- **Client-caused 4xx:** L3 edge rejections (malformed JSON/SBE, HMAC fail, rate limit, replay window) are excluded — they are correct rejections, not unavailability. Only L0/L1/L2 tiers count.
- **Degradation time:** orders rejected by `DEGRADED_MODE`/`MAINTENANCE_MODE` count against availability (the venue declined service); `CAPACITY_EXCEEDED` counts too (shedding is a capacity failure, not a client error).

## 3. Burn-rate alerting (deployed)

Google SRE multiwindow model on the 99.99% budget (task text: 14.4× fast / 6× / 1×; deployed severities noted):

| Alert | Burn | Windows | Deployed severity | Response |
|---|---|---|---|---|
| `AvailabilityBurnFast` | >14.4× | 5m AND 1h | **p0** (task says P1 — deployed is stricter) | page → [availability-slo-burn.md](../runbooks/availability-slo-burn.md) |
| `AvailabilityBurnSlow` | >6× | 30m AND 6h | **p2** (ticket) | ticket → same runbook |
| 1× threshold | >1× | 3d | **pending** — not yet in rules | adds the long-burn ticket per task text; lands with Phase-13 Task 13.3.3 rule pack |

Recording rules `deploy/prometheus/slo_rules.yml` (per-endpoint SLIs, SLO dashboards) — **pending Phase-09 Task 9.3.14 wiring**; Grafana dashboards today: `system-overview`, `shard-health`, `ipc-backbone`, `trading`.

## 4. Error-budget policy

Budget = the tolerated unavailability inside the 99.99% SLO (~4.38min per 30d at fleet level).

1. **Budget exhausted (or projected to exhaust within 7 days at current burn):**
   - **Freeze non-essential deploys** — production promotions hold at the Task 9.3.30 gate; only incident-fixes and security patches ship.
   - Feature flags may **dark-launch only** (code ships disabled; no exposure increase).
   - Engine bare-metal deploys (Task 9.3.16 8-step swap) deferred unless they *are* the fix.
2. **Recovery:** budget considered restored after **3 consecutive days inside budget** (no burn alert, error ratio under target). The freeze lifts on the third clean day, automatically reviewed at the next ops review.
3. **Freeze enforcement:** the deploy pipeline gate (`releases`/`release_promotions` interlocks — migration 091, `services/internal/fleet`) encodes "no freeze active" as a production interlock; until the freeze flag is wired into the gate, enforcement is release-manager sign-off recorded in `admin_audit_log`.
4. **Freeze exceptions:** security/incident fixes, regulator-mandated changes — recorded in `admin_audit_log` with the freeze-waiver reason.

## 5. Governance loop

- Monthly SLO review: actuals vs table above, burn events, freeze days, top error codes.
- Post-mortem feeds backlog (per [postmortem-template.md](./postmortem-template.md) §9); SLO breaches are reliability-backlog candidates by default.
- SLO changes are spec-level changes — the table is derived from §19.3/§25; proposing a looser SLO is a spec §27 record, not a doc edit.
