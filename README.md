# exc.local — Institutional Fiat-FX Exchange Suite

A complete implementation of the exchange system defined in
[`docs/Specification - Complete Exchange System Suite.md`](./docs/Specification%20-%20Complete%20Exchange%20System%20Suite.md) (v7.0):
spot FX, forwards, swaps, NDFs, and vanilla/barrier options on a **firm-liquidity
central limit order book** — fiat currencies only, no cryptocurrency.

**Status:** implementation-complete — all 30 phases landed. Hosted CI fully green
(10 CI + 5 security jobs). Spec corpus **543/543 checkpoints bound, 0 pending
stubs**. Remaining work is environment-gated evidence only (72h soak, staging
gate, DR drills, live third-party accounts) — see [Current status](#current-status).

## Scope

- **In:** CLOB matching with firm liquidity (FX Global Code Principle 17), REST /
  WebSocket / FIX 4.2+FIXS gateways, risk & liquidation, margin (incl. prime-broker
  credit), fiat funding rails (SWIFT, SEPA, FedNow, ACH, CHAPS, TARGET2, CLS PvP),
  compliance (sanctions/AML, surveillance, MiFID II / EMIR / Dodd-Frank / FinCEN /
  GDPR / DORA), analytics, backoffice, multi-region deployment.
- **Out (spec §6.4, deliberate):** RFQ, RFS, indicative quoting, last-look.

## Architecture

```
clients ──► edge (HAProxy/WAF) ──► api-gateway (Go) ──┐
                 │                                    ▼
                 └─► marketdata (Go, WS)          Aeron/shm IPC
                                                    │
                              bare-metal C++ core ──▼──► binary WAL ──► S3 archive
                              (shard-pinned, no containers in hot path)
                                                    │
   Go services ──► NATS JetStream ◄── bridge ◄──────┘
        │
        ├─► PostgreSQL 16 (OLTP, SERIALIZABLE balance mutations, partitioned)
        ├─► Redis 7 + Sentinel (cache/sessions/coordination — never the book)
        ├─► ClickHouse (ticks, OHLCV, analytics) ──► Trino (cold S3 query)
        └─► React 18 + TS frontend
```

Hard invariants: C++ core on bare metal (no containers in hot path — canonical; docker opt-in per spec §27) · Aeron/shm for
core↔services (never HTTP/gRPC) · custom binary WAL · PostgreSQL `SERIALIZABLE` for
balance mutations · fail-closed zero-loss pessimism (spec §2.7) · degradation modes
`Normal | ReadOnly | MarketDataOnly | SpotOnly | Throttled | Maintenance`.

## Stack

| Layer | Tech |
|---|---|
| Matching core | C++17/20, CMake, NUMA-pinned shards, pipette fixed-point (10⁸) |
| Services | Go 1.23+, 33 `cmd/*` binaries (gateway, fix, risk, oracle, bridge, compliance, …) |
| Frontend | React 18 + TypeScript, Vite, Vitest, Playwright e2e |
| Data | PostgreSQL 16 + pg_partman · Redis 7 (Sentinel HA + cache instance) · ClickHouse · Trino · S3 archives |
| Messaging | Aeron / shared memory (hot path) · NATS JetStream (event backbone) |
| Infra | docker-compose dev stack · K8s (deploy/k8s, 28 manifests) · bare-metal (deploy/baremetal) · docker (4 images) · systemd · Ansible · PTP · multi-region DR · WAF · Grafana · Prometheus · Trino |

## Repository layout

```
core/            C++ matching engine, WAL, IPC, FlatBuffers proto (CMake: build/, build-debug/, build-prof/)
services/        Go services — cmd/<binary> entrypoints (33 binaries), internal/ packages,
                 pkg/, config.example.yaml, internal/db/migrations/ (215 migration pairs, 001–285)
frontend/        React 18 + TS web UI (Vite, Vitest, Playwright e2e)
tests/           spec/ (543 checkpoint harness) · soak/ (72h loadgen) · load/ (AC matrix) ·
                 chaos/ (6 scenarios) · integration/ (63 tests) · pentest/ (17 tests)
deploy/          k8s/ (28 manifests) · baremetal/ · ansible/ · docker/ (4 Dockerfiles) ·
                 scripts/ (22 ops scripts) · grafana/ · haproxy/ · prometheus/ · monitoring/
                 redis/ · nats/ · sentinel/ · postgres/ · clickhouse/ · trino/ · waf/
                 systemd/ · edge/ · cloudflare/ · dr/ · crons/ · pgbouncer/ · otel/
                 storage/ · security/
infrastructure/  IaC (multi-region data tiering)
ci/              pipeline support scripts
docs/            master spec v7.0 + 30 phase plans + runbooks + ops + compliance + openapi
wal/             WAL archives (per-shard)
```

## Quick Start

Two deployment modes. Both share the same Stage 0 infrastructure (Docker) and
source code — they differ only in how the application daemons run.

### Mode A — Bare-metal (canonical, p99 ≤ 50µs valid)

C++ engine and Go services run as host binaries. NUMA-pinned, no containers in
the hot path (spec §19.1). This is the production-reference profile.

```bash
git clone <repo-url> /www/wwwroot/exc.local && cd /www/wwwroot/exc.local

# 1. Stage 0 infrastructure (PG/Redis/NATS/ClickHouse/Trino)
docker compose -f docker-compose.dev.yml up -d

# 2. Build everything natively
cmake -B core/build -S core && cmake --build core/build -j$(nproc)
cd services && go build -o bin/ ./cmd/...
cd frontend && npm ci && npm run build

# 3. Boot: engine + Go services as host processes (§19.13.2 tier order)
cd /www/wwwroot/exc.local
deploy/scripts/dev_stack.sh start all
```

Gateway `:8080` · marketdata `:8081`.

### Mode B — Full Docker (opt-in, latency jitter)

Every daemon containerized via `docker-compose.app.yml`. Adds latency jitter —
soak/benchmark numbers under this mode are **not** p99 production evidence
(spec §27). Requires building 4 images first.

```bash
git clone <repo-url> /www/wwwroot/exc.local && cd /www/wwwroot/exc.local

# 1. Stage 0 infrastructure
docker compose -f docker-compose.dev.yml up -d

# 2. Build application images (~2 min)
docker build -f deploy/docker/Dockerfile.aeron   -t exc-aeronmd:local .
docker build -f deploy/docker/Dockerfile.engine  -t exc-matching-engine:local .
docker build -f deploy/docker/Dockerfile.go      -t exc-go-service:local .
docker build -f deploy/docker/Dockerfile.frontend -t exc-frontend:local .

# 3. Boot full stack (infra + app, single command)
docker compose -f docker-compose.dev.yml -f docker-compose.app.yml up -d
```

Gateway `:8080` · marketdata `:8081` · frontend `:3000`.

### After either mode

```bash
# Apply PostgreSQL migrations (must complete < 60s)
scripts/ci/apply_migrations.sh

# Verify health
curl http://127.0.0.1:8080/health        # gateway
curl http://127.0.0.1:8090/health/ready  # oracle
curl http://127.0.0.1:8091/health/ready  # risk
```

### Test

```bash
ctest --test-dir core/build -j$(nproc) --output-on-failure          # C++ (38 tests)
cd services && go test -count=1 -short ./...                         # Go (~3657 tests)
cd frontend && npx vitest run                                        # frontend (607 tests)
cd tests/spec && go run . run --report /tmp/report.json              # spec (543 checkpoints)
```

## Deployment

Two profiles, both following the mandatory **6-stage bootstrap** (spec §19.13.2):
Tier 0 Storage → Tier 1 IPC → Tier 2 State → Tier 3 Core Engines → Tier 4
Gateways → Tier 5 Aux. A later stage never starts before the prior stage is
healthy — that ordering is the fail-closed guarantee.

### Local dev (single host)

Stage 0 runs in compose; Stages 1–5 run as local binaries supervised by
`deploy/scripts/dev_stack.sh` (PID files under `$EXC_DEV_RUN_DIR`, default
`/tmp/exc-dev-stack`), which maps the §19.13.1 daemon inventory to tier order
in §19.13.2 sequence:

```bash
# 1. Stage 0: clustered infra (PG16, Redis 1p+2r+3sentinel, ClickHouse, NATS ×3)
docker compose -f docker-compose.dev.yml up -d --wait

# 2. Verify the Stage-0 sub-DAG (ordering + sentinel quorum)
deploy/scripts/compose_stage_validate.sh

# 3. Apply DB migrations (must complete < 60s)
scripts/ci/apply_migrations.sh

# 3b. Service env: JWT signing key, ClickHouse creds, Redis/NATS addresses
# (values match the compose stack — copy + source before stage 4)
cp deploy/dev.env.example deploy/dev.env
set -a; . deploy/dev.env; set +a

# 4. Stages 1–5: Aeron/shm, matching engine, bridge, oracle, services, gateways
deploy/scripts/dev_stack.sh build          # first time only: Go + C++ binaries
deploy/scripts/dev_stack.sh start all      # Stage-0 infra, then Stages 1–5
deploy/scripts/dev_stack.sh status         # per-daemon state + health probes
# fast restart (keeps Stage-0 infra warm): stop app / start app
# full teardown: deploy/scripts/dev_stack.sh stop all
# multi-user hosts: export EXC_DEV_RUN_DIR=/tmp/exc-dev-stack-$USER (PID/log
# dir); status/start must use the same value the daemons were started with.
```

Gateway lands on `:8080` (REST+WS), marketdata on `:8081`.

> `deploy/supervisord.conf` is legacy: `supervisord` is not installed on a
> fresh host, and several `command=` entries name `cmd/` binaries that no
> longer exist (`aeron_nats_bridge`, `liquidation_scanner`, `banking_rails`,
> `regulatory_reporter`, `status_exporter`, `tomnext_rollover`,
> `proof_of_reserves` — now `bridge`, `risk`, `settlement`, `compliance`,
> `analytics`, `sentinel_exporter`, etc.). Use `dev_stack.sh`; the conf file
> remains only as the tier-priority reference.

### Boot everything from cold

```bash
# 0. First time only.
#    docker mode: build the 4 app images (minutes: base pulls + compiles)
docker build -f deploy/docker/Dockerfile.engine -t exc-matching-engine:local .
docker build -f deploy/docker/Dockerfile.go -t exc-go-service:local .
docker build -f deploy/docker/Dockerfile.aeron -t exc-aeronmd:local .
docker build -f deploy/docker/Dockerfile.frontend -t exc-frontend:local .
#    host mode: build local binaries instead
deploy/scripts/dev_stack.sh build

# 1. Service env (first time only; gitignored — never commit real secrets)
cp deploy/dev.env.example deploy/dev.env   # skip if deploy/dev.env exists

# 2. Boot all: Stage-0 infra, then Stages 1–5 in §19.13.2 order
EXC_APP_MODE=docker deploy/scripts/dev_stack.sh start all   # all-containers
# deploy/scripts/dev_stack.sh start all                     # host binaries

# 3. Apply DB migrations (`start all` does not do this; must complete < 60s)
scripts/ci/apply_migrations.sh

# 4. Verify
EXC_APP_MODE=docker deploy/scripts/dev_stack.sh status
```

UI `:3000` (docker mode only — frontend has no host binary) · gateway `:8080`
· marketdata `:8081`. Stop: `stop all` (full teardown) or `stop app` (keeps
infra warm for fast restarts; `start app` resumes without touching Stage 0).

Docker alternative (2026-10-02, prod docker redesign, spec §27): docker mode
runs every daemon from `docker-compose.app.yml` (`ipc: host` + host networking,
so `127.0.0.1` behaves exactly like host mode; compose supervision replaces
supervisord per daemon). Only PTP stays on the host. Bare metal stays
canonical — docker numbers are not p99 evidence. `dev_stack.sh` itself is a
host orchestrator: inside a container only full-docker mode works (needs bash,
docker CLI + socket, repo mount); host-mode supervision there is refused.

### Production

1. **Provision bare-metal matching hosts** (one host per shard) — stage binaries
   + systemd units, merge the `isolcpus` GRUB fragment and reboot, then:

   ```bash
   sudo deploy/baremetal/provision-matching-host.sh --apply --shard 0 --nic enp65s0f0
   sudo deploy/baremetal/provision-matching-host.sh --check  --shard 0 --nic enp65s0f0
   # Tier-1 hardware watchdog:
   sudo install -D -m 0644 deploy/baremetal/system.conf.d/50-exchange-watchdog.conf \
       /etc/systemd/system.conf.d/ && sudo systemctl daemon-reload
   ```

   Full procedure: [`docs/ops/baremetal-provisioning.md`](./docs/ops/baremetal-provisioning.md).

2. **Tier-0 storage clusters** (outside the K8s tree): PostgreSQL 16 semi-sync
   pair, Redis Sentinel 3-node, ClickHouse, NATS JetStream 3-node. Verify quorum,
   then apply migrations.

3. **K8s foundation + Go services** (Stages 3–5 daemons):

   ```bash
   kubectl apply -f deploy/k8s/00-namespace.yaml -f deploy/k8s/01-configmap.yaml \
     -f deploy/k8s/02-externalsecret.yaml -f deploy/k8s/03-rbac.yaml
   kubectl apply -f deploy/k8s/services/ -f deploy/k8s/cronjobs/
   ```

   Secrets come from Vault/KMS via ExternalSecrets (`exchange-db`,
   `exchange-secrets`, `exchange-s3`); if ESO is absent, create the same-named
   Secrets out-of-band **before** applying `services/` — pods fail closed
   without them.

4. **Core bring-up per shard host** (Tier 1→2, in order):

   ```bash
   sudo systemctl enable --now ptp4l phc2sys        # clock lock <100µs (MiFID II RTS 25)
   sudo systemctl enable --now aeronmd              # shm rings in /dev/shm
   sudo systemctl enable --now exchange-watchdogd   # Tier-3 supervisor
   sudo systemctl start matching-engine@0           # snapshot load + WAL tail replay
   ```

   The engine then acquires its Redis leader token (`engine:leader:{shardId}`)
   and enters `Normal`.

5. **Blue-green deploy of the Go services**:

   ```bash
   deploy/scripts/bluegreen.sh deploy --image-tag v1.2.3
   ```

   Gated sequence: deploy idle color → `rollout status` → per-pod
   `/health/ready` (R9 schema) → 300s canary window (Prometheus 5xx ≤1% +
   **mandatory** synthetic order probe) → zero-drop HAProxy map flip → old color
   held warm 30 min → scale to 0. Rollback: `deploy/scripts/rollback.sh --color
   green`. Runbook: [`docs/ops/blue-green-deploy.md`](./docs/ops/blue-green-deploy.md).

6. **Verify**: `deploy/scripts/bluegreen.sh status` ·
   `deploy/scripts/canary-check.sh --color green --watch --window 900`.

### Deployment hard rules

1. **C++ core is never blue-green** — it rolls per-shard with the 8-step
   drain/snapshot/swap/replay procedure
   ([`docs/ops/shard-binary-swap.md`](./docs/ops/shard-binary-swap.md)), ≥60s
   between shards. Never combine a color flip and a shard swap in one window.
2. **No synthetic-order pass ⇒ no map flip**, ever.
3. **Readiness is dependency-checked** — a pod failing PG/Redis/engine-IPC
   probes returns 503 and is pulled from rotation.
4. **Zero inline secrets** — `EXC_SECRETS_SOURCE` must resolve to Vault/KMS in
   production; the gateway fails closed otherwise.

## Testing & validation

### Quick test

```bash
ctest --test-dir core/build -j$(nproc) --output-on-failure          # C++        38 tests
cd services && go test -count=1 -short ./...                         # Go      ~3,657 tests (88 packages)
cd frontend && npx vitest run                                        # frontend   607 tests
cd tests/spec && go run . run --report /tmp/report.json              # spec       543 checkpoints
```

### Spec harness

The spec harness (`tests/spec`) mechanically binds every `Spec checkpoint:` marker in
the phase docs to an executable check — 543 checkpoints + golden corpus, 4 shards:

```bash
cd tests/spec
go run . extract            # corpus drift check (CI gate)
go run . run                # all checkpoints
go run . run --shard 0..3   # CI sharding
go run . run --report /tmp/report.json
```

Status semantics: `pending` = implemented but waiting on external evidence
(e.g. soak/staging artifacts); `missing` = no registered implementation (CI fails).

### Other suites

| Suite | Scope | Count |
|---|---|---|
| `core/tests/` | C++ Google Test (matching, WAL, recovery, risk, IPC, implied, L3…) | 38 tests / 38 targets |
| `services/**/*_test.go` | Go unit + integration (env-gated: `EXC_PG_TEST`, `EXC_REDIS_TEST`) | ~3,657 tests / 88 pkgs |
| `frontend/src/**/*.test.tsx` | Vitest + Testing Library + axe-core WCAG 2.1 AA | 607 tests / 72 files |
| `tests/spec/` | Phase checkpoint → implementation binding | 543 checkpoints |
| `tests/integration/` | Full-stack (gateway↔engine↔PG↔Redis) | 63 tests |
| `tests/pentest/` | Black-box security (IDOR, RBAC, SQLi, lockout, NATS, WS…) | 17 tests |
| `tests/chaos/` | 6 crash/recovery scenarios × 3 runs | 18 runs |
| `tests/soak/` | 72h @ 50k ord/s sustained | loadgen + monitor |
| `tests/load/` | AC matrix: engine/WS/REST 3-leg | staging gate |
| `frontend/e2e/` | Playwright smoke | browser-level |

CI: `.github/workflows/ci.yml` (10 jobs) + `security.yml` (5 jobs) — currently green.

## Current status

- All 30 phases (24 core + 6 buffer) implemented — 479 tasks, 543 checkpoints.
- 215 migration pairs · 419 §24 acceptance criteria · 234 error codes emitted (207 in spec §23 registry + matrix-resident).
- Canonical counts verified by mechanical audit: §24=419, error codes=234, AC rows=1,079, migrations=001–285, spec checkpoints=543.
- Open items are **environment-bound evidence gates**, not code gaps:
  - **72h soak @ 50k ord/s** — needs a dedicated benchmark host for the p99≤50µs criterion; engine ceiling ≥90.7k/s measured (supersedes "~15k/s dev-host ceiling" — that figure was the loadgen's ~20µs/order send loop, not engine capacity); all other Phase-02.5 criteria verified incl. crash-restart 614ms–1359ms ≪10s.
  - **75k/s × 4h staging gate** — needs a provisioned staging cluster; artifact contract `staging-report.json` armed (`ckP085StagingGate`).
  - Multi-region DR legs · live third-party accounts (PagerDuty, SES/Twilio/FCM, banking rails) · sustained ops windows (uptime SLA, annual BCP).

## Documentation

| Doc | Content |
|---|---|
| [`AGENTS.md`](./AGENTS.md) | Working rules, canonical values, mechanism ownership, phase index, gates — **read first** |
| [`CONTEXT.md`](./CONTEXT.md) | Orientation: repo state, stack, canonical values table |
| [`MEMORY.md`](./MEMORY.md) | Operational memory: counts, facts, roadmap |
| [`ARCHITECTURE.md`](./ARCHITECTURE.md) | System design, feature map, drift ledger |
| [`DESIGN.md`](./DESIGN.md) | Design intent & scope boundaries |
| [`WORKFLOWS.md`](./WORKFLOWS.md) | Change protocol, verification, CI workflow |
| [`CHANGELOG.md`](./CHANGELOG.md) | Append-only change log |
| `docs/` | Master spec v7.0 + 30 phase implementation plans |
| [GitHub Wiki](../../wiki) | Comprehensive synthesized wiki (architecture, systems, operations) |

Contributing rules live in `AGENTS.md`: spec is the contract; append-only task
numbering; `(supersedes …)` notes for replaced values; meta-docs sync on any
canonical-count change.
