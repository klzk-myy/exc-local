# Kubernetes Manifests — Go Services (Task 9.3.2, spec §19.1/§19.13.1)

Raw manifests for the K8s-deployed services of the §19.13.1 daemon
inventory. Apply order matters (§19.13.2 stage graph): infra clusters
(PG/Redis/CH/NATS) live outside this tree; these services are Stages 3–5.

```sh
kubectl apply -f 00-namespace.yaml -f 01-configmap.yaml \
  -f 02-externalsecret.yaml -f 03-rbac.yaml
kubectl apply -f services/ -f cronjobs/
```

## Inventory coverage

| Manifest | §19.13.1 daemon | Kind | Health port | HPA |
|---|---|---|---|---|
| `services/order-gateway.yaml` | `order-gateway` (×2: blue+green) | Deployment | 8080 | CPU 70% + p99 + queue depth, 2→10 |
| `services/fix-gateway.yaml` | `fix-gateway` | Deployment (+LoadBalancer) | 8082 (FIX TCP 9800) | static 2 (sessionful) |
| `services/marketdata-service.yaml` | `marketdata-service` | Deployment | 8081 | CPU 70% + WS conns + queue, 2→10 |
| `services/aeron-nats-bridge.yaml` | `aeron-nats-bridge` | Deployment + PVC | 9101 | CPU 70% + publish lag, 2→10 |
| `services/risk-coordinator.yaml` | `risk-coordinator` | Deployment | 9102 | CPU 70% + RPC p99 µs, 2→10 |
| `services/liquidation-scanner.yaml` | `liquidation-scanner` | Deployment | 9103 | static 2 (2s cadence, P1 on miss) |
| `services/settlement-service.yaml` | `settlement-service` | Deployment | 8083 | CPU 70% + queue, 2→10 |
| `services/compliance-worker.yaml` | `compliance-worker` | Deployment | 8084 | CPU 70% + consumer lag, 2→10 |
| `services/regulatory-reporter.yaml` | `regulatory-reporter` | Deployment | 9106 | static 2 |
| `services/banking-rails-worker.yaml` | `banking-rails-worker` | Deployment | 9107 | static 2 |
| `services/analytics-spooler.yaml` | `analytics-spooler` | Deployment + PVC | 9104 | CPU 70% + insert lag, 2→10 |
| `services/oracle-service.yaml` | `oracle-service` | Deployment | 9105 | CPU 70%, zone-spread, 2→10 |
| `services/status-exporter.yaml` | `status-exporter` | Deployment | 9100 | static 2 |
| `services/admin.yaml` | admin console (`services/cmd/admin`) | Deployment | 8085 | static 2, ops subnet only |
| `cronjobs/tomnext-rollover.yaml` | `tomnext-rollover` | CronJob `0 21 * * *` UTC | — | — |
| `cronjobs/proof-of-reserves-builder.yaml` | `proof-of-reserves-builder` | CronJob `0 22 * * *` UTC | — | — |
| `cronjobs/partition-archival-worker.yaml` | `partition-archival-worker` | CronJob `0 2 * * *` UTC | — | — |

All 16 §19.13.1 K8s rows are covered, plus `admin` (buildable today,
ops-subnet only). `partition-archival-worker` runs the built `archiver`
binary (`services/cmd/archiver`).

## Probe contract (R9 health schema, Task 7.3.6 / business ruling R9)

- **Liveness:** `GET /health/live` — process alive only; never gate traffic.
- **Readiness:** `GET /health/ready` — dependency-checked (PG + Redis +
  engine IPC rings required → 503; NATS optional → degraded/200; Maintenance
  mode → 503 pulls the pod).

Current binary support: `order-gateway` implements the full R9 schema plus
`/health` + `/ready` aliases. `marketdata` serves `/healthz` + `/readyz`;
`fix`/`settlement`/`compliance` serve `/healthz`; `admin` serves `/health`.
Unbuilt inventory services must implement R9 at image build time. Until the
R9 aliases land on the worker binaries, swap the probe paths to the listed
legacy endpoints (marked in each file) or deploy-time patching via
`kubectl patch` — the manifests pin the target contract.

## Secrets

Zero secrets inline. `02-externalsecret.yaml` defines ExternalSecrets
backed by Vault/KMS (`exchange-db`, `exchange-secrets`, `exchange-s3`).
Pods consume via `secretKeyRef`/`envFrom`. If ESO is not installed, create
the same-named Secrets out-of-band before applying `services/`.

## Deploy flow

Blue-green + canary for `order-gateway` is driven by
`deploy/scripts/bluegreen.sh` + `deploy/scripts/canary-check.sh` against
`deploy/haproxy/active_color.map` — see `docs/ops/blue-green-deploy.md`.
