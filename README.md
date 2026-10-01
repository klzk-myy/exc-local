# exc.local — Institutional Fiat-FX Exchange Suite

A complete implementation of the exchange system defined in
[`docs/Specification - Complete Exchange System Suite.md`](./docs/Specification%20-%20Complete%20Exchange%20System%20Suite.md) (v7.0):
spot FX, forwards, swaps, NDFs, and vanilla/barrier options on a **firm-liquidity
central limit order book** — fiat currencies only, no cryptocurrency.

**Status:** implementation-complete — all 30 phases landed. Hosted CI fully green;
spec corpus **542/542 checkpoints bound, 0 pending stubs**. Remaining work is
environment-gated evidence only (72h soak, staging gate, DR drills, live
third-party accounts) — see [Current status](#current-status).

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

Hard invariants: C++ core on bare metal (no containers in hot path) · Aeron/shm for
core↔services (never HTTP/gRPC) · custom binary WAL · PostgreSQL `SERIALIZABLE` for
balance mutations · fail-closed zero-loss pessimism (spec §2.7) · degradation modes
`Normal | ReadOnly | MarketDataOnly | SpotOnly | Throttled | Maintenance`.

## Stack

| Layer | Tech |
|---|---|
| Matching core | C++17/20, CMake, NUMA-pinned shards, pipette fixed-point (10⁸) |
| Services | Go 1.23+, ~30 `cmd/*` binaries (gateway, fix, risk, oracle, bridge, …) |
| Frontend | React 18 + TypeScript, Vite, Vitest, Playwright e2e |
| Data | PostgreSQL 16 + pg_partman · Redis 7 (Sentinel HA + cache instance) · ClickHouse · Trino · S3 archives |
| Messaging | Aeron / shared memory (hot path) · NATS JetStream (event backbone) |
| Infra | docker-compose dev stack · K8s (deploy/k8s) · bare-metal (deploy/baremetal) · Ansible · PTP · multi-region DR |

## Repository layout

```
core/            C++ matching engine, WAL, IPC (CMake: build/, build-debug/, build-prof/)
services/        Go services — cmd/<binary> entrypoints, internal/ packages, config/,
                 internal/db/migrations/ (206 PostgreSQL migration pairs)
frontend/        React 18 + TS web UI
tests/           spec/ (checkpoint harness) · soak/ · load/ · chaos/ · integration/ · pentest/
deploy/          k8s/, baremetal/, ansible/, grafana/, haproxy/, edge/, dr/, clickhouse/, crons/
infrastructure/  IaC (multi-region)
ci/              pipeline support scripts
docs/            master spec + 30 phase plans (Phase-01 … Phase-24 + 6 buffer phases)
wal/             WAL tooling
```

## Quickstart

**Dev infrastructure** (PostgreSQL 5433, Redis 16379+replicas+sentinels, NATS ×3,
ClickHouse 8123/9000, Trino 18443):

```bash
docker compose -f docker-compose.dev.yml up -d
```

**C++ core:**

```bash
cmake -S core -B core/build -DCMAKE_BUILD_TYPE=Release
cmake --build core/build -j
ctest --test-dir core/build --output-on-failure
```

**Go services** (module `exchange`, services/go.mod):

```bash
cd services && go build ./... && go test ./...
# populate Redis shard map after dev Redis restart/flush:
go run ./cmd/exchange cache-shard-map
```

**Frontend:**

```bash
cd frontend && npm ci && npm run dev      # build: npm run build · test: npm run test
```

## Deployment

Two profiles, both following the mandatory **6-stage bootstrap** (spec §19.13.2):
Tier 0 Storage → Tier 1 IPC → Tier 2 State → Tier 3 Core Engines → Tier 4
Gateways → Tier 5 Aux. A later stage never starts before the prior stage is
healthy — that ordering is the fail-closed guarantee.

### Local dev (single host)

Stage 0 runs in compose; Stages 1–5 run as local binaries under supervisord
(`deploy/supervisord.conf` maps the §19.13.1 daemon inventory to tier
priorities 10→60):

```bash
# 1. Stage 0: clustered infra (PG16, Redis 1p+2r+3sentinel, ClickHouse, NATS ×3)
docker compose -f docker-compose.dev.yml up -d --wait

# 2. Verify the Stage-0 sub-DAG (ordering + sentinel quorum)
deploy/scripts/compose_stage_validate.sh

# 3. Apply DB migrations (must complete < 60s)
scripts/ci/apply_migrations.sh

# 4. Stages 1–5: Aeron/shm, matching engine, bridge, oracle, services, gateways
export REPO=/www/wwwroot/exc.local
sudo mkdir -p /var/log/exchange/dev
supervisord -c deploy/supervisord.conf
supervisorctl -c deploy/supervisord.conf status
```

Gateway lands on `:8080` (REST+WS), marketdata on `:8081`.

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

The spec harness (`tests/spec`) mechanically binds every `Spec checkpoint:` marker in
the phase docs to an executable check — 542 checkpoints + golden corpus, 4 shards:

```bash
cd tests/spec
go run . extract            # corpus drift check (CI gate)
go run . run                # all checkpoints
go run . run --shard 0..3   # CI sharding
go run . run --report /tmp/report.json
```

Status semantics: `pending` = implemented but waiting on external evidence
(e.g. soak/staging artifacts); `missing` = no registered implementation (CI fails).

Other suites: `tests/soak/` (72h/50k sustained + artifact gate) · `tests/load/`
(staging 75k×4h gate, `staging-report.json` contract) · `tests/chaos/` (6×3 drill
matrix) · `tests/integration/` · `tests/pentest/` · Go unit/integration tests ·
Vitest + Playwright.

CI: `.github/workflows/ci.yml` (10 jobs) + `security.yml` (5 jobs) — currently green.

## Current status

- All 30 phases (24 core + 6 buffer) implemented — 479 tasks, 542 checkpoints.
- 206 migration pairs · 419 §24 acceptance criteria · 149+ error codes.
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
