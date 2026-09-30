# K8s Probe Coverage Matrix (Phase-07 AC audit, Task 7.3.6 / ruling R9)

Static audit of every pod spec under `deploy/k8s/`. Contract per
`README.md`: liveness = `GET /health/live` (process only, never gates
traffic); readiness = `GET /health/ready` (dependency-checked: PG + Redis +
engine IPC required→503, NATS optional→degraded, Maintenance→503).

## Deployments — 16 pod specs, all with liveness + readiness

| Manifest | Pod | Probe port | Live path | Ready path | Startup | Binary endpoint today | Reachability wiring |
|---|---|---|---|---|---|---|---|
| `order-gateway.yaml` | order-gateway-blue | `http` :8080 | `/health/live` | `/health/ready` | — | **Full R9** (`cmd/gateway` :5610–14 + `/health`,`/ready` aliases) | `gateway.host=0.0.0.0:8080` ✓ |
| `order-gateway.yaml` | order-gateway-green | `http` :8080 | `/health/live` | `/health/ready` | — | same | same ✓ |
| `fix-gateway.yaml` | fix-gateway | `health` :8082 | `/health/live` | `/health/ready` | — | **R9 live** via `observability.ServeMetrics` (live+ready added) | `fix.host=0.0.0.0` default ✓ |
| `marketdata-service.yaml` | marketdata-service | `ws` :8081 | `/health/live` | `/health/ready` | — | **R9 live** (live+ready added to ws mux, redis dep) | `marketdata.host=0.0.0.0` default ✓ |
| `admin.yaml` | admin | `http` :8085 | `/health/live` | `/health/ready` | — | **R9 live** (live+ready added, nats optional dep) | `EXC_ADMIN_HOST=0.0.0.0` ✓ |
| `settlement-service.yaml` | settlement-service | `health` :8083 | `/health/live` | `/health/ready` | — | **R9 live** via `observability.ServeMetrics` | `EXC_SETTLEMENT_HOST=0.0.0.0` ✓ |
| `compliance-worker.yaml` | compliance-worker | `health` :8084 | `/health/live` | `/health/ready` | — | **R9 live** via `observability.ServeMetrics` | `EXC_COMPLIANCE_HOST=0.0.0.0` ✓ |
| `aeron-nats-bridge.yaml` | aeron-nats-bridge | `health` :9101 | `/health/live` | `/health/ready` | — | **R9 live** (live+ready added, nats dep) | `EXC_BRIDGE_METRICS_ADDR=0.0.0.0:9101` ✓ |
| `analytics-etl.yaml` | analytics-etl | `metrics` :9200 | `/healthz` | `/healthz` | — | `/healthz` real (cmd/analytics `-metrics-addr`) | `args: -metrics-addr=0.0.0.0:9200` ✓ (loopback default would be unreachable) |
| `analytics-spooler.yaml` | analytics-spooler | `health` :9104 | `/health/live` | `/health/ready` | — | planned binary (R9 at build) | `EXC_ANALYTICS_HEALTH_ADDR=0.0.0.0:9104` |
| `risk-coordinator.yaml` | risk-coordinator | `health` :9102 | `/health/live` | `/health/ready` | — | **R9 live** — `EXC_RISK_HEALTH_ADDR` listener (pg+redis+nats deps) | `EXC_RISK_HEALTH_ADDR=0.0.0.0:9102` |
| `liquidation-scanner.yaml` | liquidation-scanner | `health` :9103 | `/health/live` | `/health/ready` | — | planned binary | `EXC_LIQ_HEALTH_ADDR=0.0.0.0:9103` |
| `oracle-service.yaml` | oracle-service | `health` :9105 | `/health/live` | `/health/ready` | — | **R9 live** — `EXC_ORACLE_HEALTH_ADDR` listener (redis dep; smoke-verified) | `EXC_ORACLE_HEALTH_ADDR=0.0.0.0:9105` |
| `regulatory-reporter.yaml` | regulatory-reporter | `health` :9106 | `/health/live` | `/health/ready` | — | planned binary | `EXC_REGREP_HEALTH_ADDR=0.0.0.0:9106` |
| `banking-rails-worker.yaml` | banking-rails-worker | `health` :9107 | `/health/live` | `/health/ready` | — | planned binary | `EXC_RAILS_HEALTH_ADDR=0.0.0.0:9107` |
| `status-exporter.yaml` | status-exporter | `metrics` :9100 | `/health/live` | `/health/ready` | — | planned binary | `EXC_STATUS_ADDR` |

All `httpGet` probes use **named ports** that resolve to a declared
`containerPort`, and each `Service` `targetPort` matches the same name.
`startupProbe` is intentionally absent fleet-wide — Go binaries start in
ms; readiness gates traffic from the first second.

## CronJobs — probes not applicable (batch completion supervision)

| Manifest | Schedule (UTC) | Supervision |
|---|---|---|
| `tomnext-rollover.yaml` | `0 21 * * *` | `concurrencyPolicy: Forbid`, `activeDeadlineSeconds: 3600`, `backoffLimit: 2`, Redis execution lock (double-roll guard) |
| `proof-of-reserves-builder.yaml` | `0 22 * * *` | `Forbid`, deadline 1800s, `backoffLimit: 2`, SHA256 check |
| `partition-archival-worker.yaml` | `0 2 * * *` | `Forbid`, deadline 7200s, `backoffLimit: 1`, checksum verifier |
| `balance-snapshot-builder.yaml` | `59 23 * * *` | `Forbid`, deadline 1800s, `backoffLimit: 2` (idempotent replay) |

K8s liveness/readiness probes are meaningless on run-to-completion Job
pods; supervision is deadline + bounded retry + idempotency. Not a gap.

## §19.13.1 daemon-inventory cross-check

All 16 K8s-tier rows in `docs/ops/daemon-inventory.md` have manifests and
every Deployment has both probes. Extras beyond the table: `analytics-etl`
(Deployment) and `balance-snapshot-builder` / `partition-archival-worker`
(CronJobs — the latter also has a bare-metal systemd twin). Bare-metal
tier (matching-engine, aeronmd, ptp4l/phc2sys, watchdogd, redis, PG,
pgbouncer, CH, nats) correctly lives outside this tree; `sentinel-exporter`
is supervisord-only (Redis sentinel metrics, :9109) and not a K8s row.

## Endpoint-semantics notes

- `order-gateway` is the only binary serving the full R9 schema today
  (`services/cmd/gateway/main.go:5604–5614`; `services/internal/api/health.go`).
- Legacy surfaces: marketdata `/healthz`+`/readyz`; fix/settlement/
  compliance/bridge `/healthz`; admin `/health`; analytics `/healthz`.
  Manifests pin the R9 target contract per README; affected files carry a
  marker comment naming today's swappable endpoint.
- Open items outside the probe criterion: `fix-gateway` exposes FIX TCP
  `9800` per spec §19.13 but `fix.acceptor_port` defaults to `9879` and no
  `EXC_FIX_ACCEPTOR_PORT` override is set; `exchange-watchdogd` has an
  optional "K8s DaemonSet on service nodes" row with no manifest here.
