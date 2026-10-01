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
