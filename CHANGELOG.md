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
