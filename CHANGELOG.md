# Implementation Changelog — Complete FOREX Exchange System Suite

Append-only execution log for the 30-phase implementation. Newest entries at the bottom.
Format per IMPLEMENTATION_PROMPT.md §6. Canonical counts (AGENTS.md): §24 criteria 414,
phase tasks 479, spec checkpoints 543, AC rows 1,079, error codes 149, migrations 108.

---

## [2026-09-27 17:09 UTC] — BOOTSTRAP

- **Toolchain:** g++ 11.4.0, CMake 3.31.8 (`/opt/cmake-3.31.8`, spec requires ≥3.28),
  Go 1.23.12 (`/usr/local/go`), Node v24.21.0/npm 11.19.0, Docker 29.8.1 + Compose v5.5.1
  (via sudo — user not in docker group), git 2.34.1, flatc 1.12.0.
- **Baseline commit:** `6d15b75` — all planning docs (spec v7.0 + 30 phase plans + meta-docs).
- **MCP:** `.devin/mcp_config.json` already present, mirrors `opencode.json`
  (gitnexus/filesystem/git-history/memory/sequential-thinking/time/fetch/context7/postgres/
  redis/playwright; github + clickhouse disabled until credentials/URLs exist — per plan,
  not fabricated).
- **Infra (`docker-compose.dev.yml`, all healthy):**
  - PostgreSQL 16.15 + pg_partman + pgcrypto → host `5433` (host-local PG already owns 5432).
    `POSTGRES_CONNECTION_STRING=postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable`
  - Redis 7.2 primary `16379`, replicas `16380`/`16381`, Sentinel ×3 `36379`–`36381`
    (quorum=2, `mymaster` monitored, 2 slaves + 2 sentinels confirmed), cache-only
    volatile-lru instance `6382`. `REDIS_URL=redis://127.0.0.1:16379`.
  - NATS JetStream 3-node cluster `exc-jetstream` → client ports `4222`–`4224`, monitoring
    `8222`–`8224`, file-backed stores.
  - ClickHouse 25.8 → HTTP `8123`, native `9000` (`exchange`/`exchange_dev`, db `exchange_analytics`).
- **Aeron:** source 1.48.6 cloned to `/tmp/aeron-src`; C media driver (`aeronmd`) +
  C/C++ client build in progress via `cppbuild --build-aeron-driver --no-system-tests`.
- **Decisions:**
  - **Port remap:** host-local postgres (127.0.0.1:5432) and redis (127.0.0.1:6379) already
    occupied ports; dev topology shifted to 5433/16379-16381/36379-36381. Container-internal
    ports/config unchanged, so topology shape is identical to production.
  - **Sentinel client DNS:** sentinels announce container hostname `redis-primary:6379` —
    resolvable only inside the compose network. Sentinel-aware Go client failover tests run
    inside the network (e.g. `docker run --network exc-dev_default golang`), matching the
    production model where services are containerized. Host-side tests use direct
    `127.0.0.1:16379`.
  - **Migration column ownership:** Phase-01 migrations 001–021 create only baseline
    columns; columns annotated in spec §5 as added by later migrations (031/038/039/050/051/
    066/067/087/095/103/104/106, …) are deliberately excluded — the owning phase's ALTER
    migration adds them. ENUM types are created with their full spec-canonical value sets
    (spec is the contract); later migrations extending enums must use
    `ALTER TYPE … ADD VALUE IF NOT EXISTS`.
  - **Enum-complete types:** `orders.order_type` includes MOO/MOC/FIXING etc. at creation;
    `accounts.kyc_tier`/`users.kyc_status` carry full final value sets.

## [2026-09-27 17:09 UTC] — Phase 01 START

Scope: 12 tasks (1.3.1–1.3.12), 22 AC rows. Infra up per bootstrap. Dispatching in waves:
foundations (1.3.1 C++ scaffold / 1.3.2 Go scaffold / 1.3.3 PG migrations) → dependents
(1.3.4 Redis / 1.3.6 WAL / 1.3.8 audit) → integration (1.3.5 IPC / 1.3.7 shardmap /
1.3.11 NATS) → resilience (1.3.9 sentinel / 1.3.10 aeron tuning / 1.3.12 error handling).

### [2026-09-27 17:25 UTC] — Task 1.3.3 PostgreSQL 16 Schema Migrations — DONE
- **Files:** `services/internal/db/migrations/00[1-9]_*`, `01[0-9]_*`, `02[0-1]_*` up+down (42 files) + `test_serializable.sql` (43 total).
- **Verification:** all 21 up-migrations clean on fresh PG16.15 (and re-up after full down-chain); `trades` partitioned daily via pg_partman 5.5 — insert routed to `trades_p20260927` (61 premade partitions); seeds verified (EUR/USD…NZD/USD T+1 lev30, USD/CAD same-day lev30, USD/MXN same-day lev10); required indexes present; SERIALIZABLE concurrency proof: session B received SQLSTATE 40001, retry committed, final balance 100−10−20=70 version=2 (no lost update).
- **Checklist:** DoD 5/5 marked, SDD 6/6 marked.
- **Decisions:**
  - pg_partman v5.5.0 signature used: `public.create_parent('public.trades','created_at','1 day', p_premake:=30)` — the task-text `'native','daily'` call is pg_partman 4.x syntax and fails on 5.5. Documented in 006 header.
  - `settlement_instructions.trade_id` kept plain BIGINT (no declarative FK — partitioned PK (id,created_at) precludes it; application-level enforcement; documented in 019).
  - `trades.id` = BIGINT GENERATED ALWAYS AS IDENTITY inside composite PK (id,created_at) — required for partitioned parent.
  - Column-ownership exclusions verified mechanically: zero later-migration columns present.
- **Deviation:** none vs plan intent.

### [2026-09-27 17:30 UTC] — Task 1.3.2 Go Services Scaffold — DONE
- **Files:** `services/go.mod` (module `exchange`, go 1.23.0 + toolchain go1.23.12), `cmd/{gateway,marketdata,fix,settlement,compliance,admin}/main.go`, `internal/{api,auth,config,db,redis,ipc,middleware,models,settlement,compliance,utils}`, `pkg/{decimal,errors,logging}`, `config.example.yaml`, `.gitignore`, `tools/tools.go` (build-tag dep pin).
- **Verification (orchestrator re-run):** `GOTOOLCHAIN=local go build ./...` clean; `go vet` clean; gateway bound :18099 via `EXC_GATEWAY_PORT`, `/health` → `{"status":"ok"}`, slog JSON, SIGTERM clean.
- **Checklist:** DoD 4/4 marked, SDD 4/4 marked.
- **Decisions:**
  - `shopspring/decimal v1.4.0` added (spec §5.3 precision standard names it).
  - `pgx v5.7.5` / `go-redis v9.7.3` pinned — newest versions compatible with go1.23.12 (v5.11/v9.22 require ≥1.24/1.25).
  - Invalid env value → fail-closed exit 1 (spec §2.7 pessimism); missing config file → defaults.
  - `EXC_CONFIG` env for explicit config path; `tools/tools.go` keeps gorilla/websocket+quickfix+cobra pinned until Phase-05/06/18 consumers land.
- **Deviation:** none.
