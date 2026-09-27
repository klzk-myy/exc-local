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

### [2026-09-27 17:45 UTC] — Task 1.3.4 Redis 7 Configuration — DONE
- **Files:** `services/internal/redis/client.go` (full coordination client: sessions, rate limits, leader epoch-lease, account locks, degradation mode, circuit breakers, halt flag; PoolSize 20 + ctx timeouts), `client_test.go` (8 integration tests, `EXC_REDIS_TEST` gate), `deploy/redis/redis.conf` (+`repl-backlog-size 512mb` per §4.5, applied live via CONFIG SET).
- **Verification (re-run):** `EXC_REDIS_TEST=1 go test ./internal/redis/` 8/8 PASS; live `CONFIG GET` confirms appendonly=yes, maxmemory-policy=noeviction (4GB); SETNX leader contention verified (first wins, second fails); DBSIZE=0 after tests (no leaked keys).
- **Checklist:** DoD 5/5, SDD 4/4 marked.
- **Decisions:** lease value `{token}:{epoch}` format + `epoch` param on TryAcquireLeader (spec §18.6.2 epoch model canonical — 2000ms TTL, Lua token-checked release/refresh); `HaltGlobal` stores reason; `GetDegradationMode` returns full DegradationState (mode+entered_at+reason).
- **Deviation:** none.

### [2026-09-27 18:05 UTC] — Task 1.3.1 C++ Project Scaffold — DONE
- **Files:** `core/CMakeLists.txt` (C++20, exch_core lib + matching_engine, `EXCH_WITH_AERON` option auto-detects vendored SDK), `src/main.cpp` (`-shard` arg, logs `shard {N} initialized`), real `utils/{Decimal,MemoryPool,TimeUtils,CheckedMath}` + spec §3.1 `book/{Order,PriceLevel,OrderBook}` POD/layout + phase-tagged stubs for matching/risk/wal/ipc/recovery/degradation/health/election, 6 gtest suites (FetchContent googletest 1.14.0 SHA-pinned), vendored Aeron 1.48.6 SDK at `core/third_party/aeron/` (aeronmd + libaeron_static/libaeron_client/libaeron_driver_static + C/C++ headers — built from source, 139/139 upstream tests pass).
- **Verification (re-run):** Release (`-O3 -march=native -DNDEBUG`) + Debug (`-O0 -g -fsanitize=address`) both clean, 0 warnings; ctest 6/6 both configs; `matching_engine` logs `shard 0 initialized`; Decimal 11 cases incl. `0.1+0.2==0.3` exact (zero float/double in core per grep); MemoryPool 1M allocs with **0 heap ops** under armed replaceable-new hook; TimeUtils monotonic over 1e6 calls.
- **Checklist:** DoD 5/5, SDD 5/5 marked.
- **Decisions:** Decimal = int64 mantissa ×1e8 (pipette), `__int128` intermediates + `__builtin_*_overflow` → `std::overflow_error`; pool exhaustion → nullptr (maps to `ORDER_BOOK_CAPACITY_EXCEEDED` per §3.6); namespace `exch`; g++ 11.4 vs spec's GCC 13+ — IMPLEMENTATION_PROMPT permits ≥11 (no `<format>` usage).
- **Deviation:** g++ 11.4 (spec §1.2 says GCC 13+/Clang 17+; orchestrator prompt allows ≥11 — recorded).

### [2026-09-27 18:05 UTC] — Task 1.3.11 NATS JetStream Cluster — DONE
- **Files:** `services/internal/nats/{client,streams,publish,consumer,health}.go` + unit + `EXC_NATS_TEST`-gated integration tests; `services/cmd/natsctl` (init/health/smoke); `deploy/nats/provision.sh`; `nats.go v1.45.0` pinned (newest compatible with go1.23 toolchain).
- **Verification (re-run):** `/jsz` → meta cluster `exc-jetstream` size 3, leader nats-2; all 7 streams (`trades,settlements,compliance,analytics,funding,margin-events,surveillance`) confirmed R3 (leader+2 replicas), WorkQueue retention, MaxAge 168h, FileStorage, subject `{name}.>`; integration tests 5/5 (EnsureStreams idempotent, publish→durable fetch→ack→no redelivery, AckWait redelivery, mid-session node restart reconnect, health report); `natsctl smoke` green.
- **Checklist:** no checkbox block in plan (remediation task) — AC row 21 satisfied by this evidence.
- **Decisions:** `DiscardOld`+2min `Duplicates` window on streams (safe publisher-retry dedup beyond task text); `nats` CLI absent in alpine image → `/jsz` used for verification.
- **Deviation:** none.

### [2026-09-27 18:05 UTC] — Repo hygiene
- Removed 476 committed `core/build-debug/` artifacts from index; `.gitignore` now covers `core/build*/` and `**/build/`.

### [2026-09-27 18:20 UTC] — Task 1.3.8 Audit Hash Chain — DONE
- **Files:** `services/internal/audit/{HashChain,merkle}.go` + 3 test files; `services/cmd/exchange/` cobra CLI (`verify-audit --date`, `merkle --date|--run-daily`, `audit-append` operator fixture).
- **Verification (orchestrator re-run, live DB):** 3 self-verifiable rows appended → `verify-audit` exit 0 (chain + merkle root clean); `UPDATE ... SET record_id=9999` on middle row → exit **2**, named `sequence_num=2 field=payload_hash`; prev_hash linkage chains correctly (genesis = sha256("")). 21 tests green incl. live integration.
- **Checklist:** DoD 4/4, SDD 4/4 marked.
- **Decisions:** `canonical_payload` preimage = `seq|id|created_at_utc|hex(payload)` over stored fields — self-verifiable; opaque emitter payloads need a `PayloadProvider` (documented in package doc). Genesis prev_hash=sha256(""). Tail-read serialized via `pg_advisory_xact_lock`; `AppendAuto` retries on 23505/40001/40P01 ≤3×. verify-audit also re-checks stored daily merkle root.
- **Deviation:** none.

### [2026-09-27 18:35 UTC] — Task 1.3.9 Redis Sentinel HA — DONE
- **Files:** `services/internal/redis/sentinel.go` (+tests), `services/internal/config/redis_sentinel.go`, `config/redis-sentinel.yaml`, `deploy/redis/sentinel.conf`, `docker-compose.dev.yml`.
- **Verification (live drills ×2):** kill primary → +sdown→+odown→+promoted in **1.07s/1.09s** detection→promotion; ~3.0s end-to-end (bounded by spec down-after=2000ms). Session key survived both failovers; leader-lease key pattern verified. Reconnect to new master: **43ms / 81µs** (<100ms). Old primary rejoins as replica via sentinel rewrite. Live-config verified by orchestrator: quorum=2, down-after=2000, failover-timeout=10000, parallel-syncs=1, num-slaves=2, min-replicas-to-write=1, max-lag=5, appendonly=yes.
- **Checklist:** DoD 3/3, SDD 3/3 marked.
- **Decisions:** (a) **Root-cause fix** — Sentinel monitoring DNS hostname `redis-primary` deadlocks failover (container stop kills DNS → getaddrinfo stalls event loop ~8s → +tilt loop aborts every failover). Added `redis-ha` 10.99.0.0/24 pinned-IP subnet; sentinel now monitors 10.99.0.11 by IP. Production-correct pattern. (b) Three resolve modes: `as_announced` (in-network), `host_probe` (host), `announce_map`. (c) `min-replicas-to-write=1` is the split-brain guard.
- **Deviation:** compose/sentinel.conf deviate from plan prose (IP-pinned monitoring) — recorded in spec §27 pending; strictly better than monitoring ephemeral DNS names.

### [2026-09-27 18:55 UTC] — Task 1.3.6 Binary WAL Infrastructure — DONE
- **Files:** `core/include/wal/{Wal,WalEntry}.hpp`, `core/src/wal/{Wal,WalEntry}.cpp`, `core/tests/test_wal.cpp` (19 tests).
- **Verification (orchestrator re-run):** Release+Debug(ASan) ctest **7/7 both configs**; `test_wal` 19/19. Orchestrator reran crash/CRC/direct tests: `ThousandEntriesAllCrcVerified`, `CorruptCrcTruncatesLastEntry`, `DirectModeTornTail`, `TornTail*` — all PASS. Header magic `0x57414C00`/version=1/shard_id verified in file bytes + reader. CRC32C hw path (SSE4.2 `__builtin_ia32_crc32*` + cpuid dispatch) known-vector `0xE3069283` OK, sw table identical. O_DIRECT: file exact 4096 multiple, `posix_memalign` buffers, pad sentinel at block boundary; ENOSPC→clean `NoSpace` (no half-committed record); rotation emits `{seq_base}.wal` + `pending_archive()`.
- **Checklist:** DoD 5/5, SDD 5/5 marked.
- **Decisions:** (a) rotation names `{seq_base}.wal` under caller's shard dir; (b) `posix_fallocate` in 16MB quanta converts SIGBUS-on-full-disk → clean NoSpace; (c) O_DIRECT rejected by fs → staged-block buffered fallback with `direct_fallback()` flag (`strict_direct` fails open); (d) `seq=UINT64_MAX` reserved as pad sentinel; (e) EFBIG ≡ NoSpace.
- **Deviation:** none vs amended spec (all 5 deviations are design strengthenings, recorded here).

### [2026-09-27 19:10 UTC] — Task 1.3.5 Aeron IPC + Shared-Memory Transport — DONE
- **Files:** `core/proto/exchange.fbs` + gen (C++/Go), `core/include|src/ipc/` (ShmRing, SharedMemChannel, AeronChannel, echo bench), `services/internal/ipc/**` (shm.go ABI mirror, channel.go, messages.go, aeron cgo wrapper, bench), `deploy/scripts/bench_ipc.sh`, go flatbuffers v1.12.1.
- **Verification (orchestrator re-ran `deploy/scripts/bench_ipc.sh 5000`):** shm RTT avg=1767ns p50=732ns **p99=6.3µs** max=90µs, 5000/5000 echoed drops=0; aeron RTT avg=1160ns p50=913ns **p99=3.7µs** — both well under 50µs budget. ctest 7/7 both configs at verification. 1000-msg zero-loss shm test + Go↔C++ round-trips pass. Aeron loopback gated when aeronmd present.
- **Checklist:** DoD 4/4, SDD 5/5 marked.
- **Decisions:** shm ring = SPSC 64B-aligned slots, seq/len/flags slot header, zero-copy peek/consume; liveness = pid + realtime-ns heartbeat + kill(pid,0); `create` flag advisory (first-attacher-inits; live images never re-init — fixes create/attach race); AeronChannel channels `aeron:ipc?alias=orders_{in,out}` streams 1001/1002; EXCH_WITH_AERON=0 stub when SDK absent.
- **Deviation:** none. NOTE: concurrent ShardMap task (1.3.7) temporarily broke full build (std::atomic<shared_ptr> unsupported on g++11.4 libstdc++) — owner agent must fix before Phase-01 gate.

### [2026-09-27 19:40 UTC] — Task 1.3.7 Shard Mapping System — DONE
- **Files:** `config/sharding.yaml`, `services/internal/config/sharding.go`(+test), `core/include/utils/ShardMap.hpp`, `core/src/utils/ShardMap.cpp`, `core/tests/test_shard_map.cpp`, `services/cmd/exchange/cache_shard_map.go`, `cmd/gateway` startup wiring.
- **Verification (orchestrator re-run):** `exchange cache-shard-map` → `HGETALL shard:map` = 12 symbols + meta fields; deleted key + rewrote = 15 fields. Go `TestShardMap*` 7/7 PASS; C++ `test_shard_map` 7/7 PASS (canonical repo file). EUR/USD→0, USD/JPY→1 confirmed both languages + USD/MXN→2, EUR/JPY→3. Gateway logged `shard map loaded source=redis`.
- **Checklist:** DoD 4/4, SDD 3/3 marked.
- **Decisions:** elastic policy = FNV-1a-32 hash of canonical symbol into shards [4,8) — identical Go/C++ (pinned FNV vectors); canonicalization strips non-alnum + uppercase, 6-char→BASE/QUOTE; `shard:map` carries `meta:{version,elastic_base,elastic_count}` so Redis-only readers resolve identically; `LoadShardMapForService` Redis-first→yaml-fallback; C++ uses shared_mutex+shared_ptr swap (g++11.4 lacks atomic<shared_ptr>).
- **Deviation:** none.

### [2026-09-27 20:10 UTC] — Task 1.3.12 Error Handling & Safe Allocations — DONE
- **Files:** `core/include/utils/{error_severity,safe_math,ClockSyncGuard}.hpp`, `core/src/utils/ClockSyncGuard.cpp`, `core/tests/test_error_handling.cpp` (18 tests), `CheckedMath`/`MemoryPool` consolidated, `services/pkg/errors/{severity,codes,problem}.go` (+tests), `services/internal/timesync/guard.go` (+tests).
- **Verification (orchestrator re-run):** ctest **9/9 both configs** (test_error_handling 18/18 incl. INT64_MIN/MAX edges, i128, out-unmodified-on-failure); `go test` pkg/errors 12 + timesync 7 PASS; vet/gofmt clean.
- **Checklist:** SDD 2/2 marked.
- **Decisions:** `safe_math` uses scratch-then-copy out-param (intrinsics store wrapped value on overflow — preserves fail-closed); `OrderBookCapacityExceeded`/`TimeSyncLossHalt` are non-allocating literal-pointer exceptions (`alloc_or_throw` no heap in throw path); clock guard reads `ntp_adjtime` (STA_UNSYNC→fail-closed), injectable probe, bound 100µs; unknown codes → L2/500 per fail-closed pessimism.
- **Deviation:** `require_clock_sync` delivered as startup gate but not wired into main.cpp (Phase-02 owns engine wiring; documented at header).

### [2026-09-27 20:45 UTC] — Task 1.3.10 Aeron Driver Config & Kernel Tuning — DONE
- **Files:** `config/aeron-low-latency.properties`, `core/include/ipc/AeronDriverConfig.hpp`, `core/src/ipc/AeronDriverConfig.cpp`, `scripts/tune-kernel-network.sh`.
- **Verification (orchestrator re-run):** `tune-kernel-network.sh --check` → 10/10 tunables verified (rmem_max=wmem_max=16MB, busy_read/poll=50, netdev_budget=600, max_backlog=4096, governor=performance, exit 0). aeronmd `print.configuration` dump confirmed `term_buffer_length=134217728`, `ipc_term_buffer_length=134217728`, `socket_{snd,rcv}buf=16777216`, `mtu=1408`, `threading_mode=DEDICATED`, busy-spin sender/receiver + backoff conductor. Bench n=100k on tuned driver: **aeron p99=2.9µs**, shm p99=3.5µs ≪ 50µs. `launch_aeronmd` smoke: start → relaunch→`AlreadyRunning` (fail-closed) → SIGINT graceful.
- **Checklist:** DoD 3/3, SDD 3/3 marked.
- **Decisions:** added `aeron.ipc.term.buffer.length`/`ipc.mtu.length` (hot path is `aeron:ipc` — network props alone wouldn't apply); netdev_budget/max_backlog/budget_usecs sysctls added; governor via runtime sysfs (non-persistent); isolcpus = guidance only (single dev host); `aeron.dir` kept out of properties (file-wins over -D); vendored `aeronmd` swapped to static `aeronmd_s` — original had rpath into volatile `/tmp/aeron-src`.
- **Deviation:** none.

### [2026-09-27 21:05 UTC] — PHASE 01 COMPLETE — C++ Core Foundation
**AC 1.7 evidence (22/22):**
1. Debug (-O0 -g -fsanitize=address) + Release (-O3 -march=native) builds green — ctest **9/9 each**.
2. All 6+2 service binaries compile (`go build ./cmd/...` — admin, compliance, exchange, fix, gateway, marketdata, natsctl, settlement).
3. 21 migrations applied to fresh `fresh_gate` PG16 (post pg_partman+pgcrypto bootstrap): **86 tables, zero errors**.
4. 8 seed instruments present with correct settlement_cycle/max_leverage.
5. `trades` pg_partman daily partitioned — **63 partitions** (premake 30).
6. Redis AOF+maxmemory live; key types exercised (Task 1.3.4).
7. IPC RTT: **shm p99=6.3µs, aeron p99=3.7µs** <50µs (n=5000–100000).
8. WAL 1000 entries fsync'd + CRC32 verified (`test_wal` 19/19).
9. WAL crash sim: torn tail detected + truncated, appends resume clean.
10. Shard map EUR/USD→0, USD/JPY→1, cached in Redis `shard:map` (12 syms + meta).
11. SHA-256 payload_hash/prev_hash chain correct (genesis = sha256("")).
12. `verify-audit` detects modified row → exit 2, field+seq identified.
13. Daily Merkle root job (`merkle --run-daily`, 00:10 UTC body) computes+stores; scheduler wiring deferred to Phase-07 per ownership map.
14. Decimal: fixed-point int64 mantissa ×1e8, no float (test_decimal).
15. MemoryPool 1M orders preallocated, alloc/free heap-free, high-watermark+OrderBookCapacityExceeded.
16. slog JSON in all Go services.
17. YAML config + EXC_* env overrides in all services.
18. `exchange cache-shard-map` writes `shard:map` HASH.
19. Sentinel failover: promotion **1.07s**, reconnect **43ms/81µs**, zero session loss, 2 live drills.
20. Aeron C driver: 128MB term bufs, DEDICATED threading, 16MB sockbufs, kernel tuned, p99 2.9µs.
21. NATS 3-node healthy, 7 streams R3 workqueue 7d, at-least-once verified.
22. safe_math overflow detect, pool capacity bound, clock-drift>100µs halts (TIME_SYNC_LOSS_HALT) — 18/18 tests.
- **Commits:** c6422d0(1.3.7) 516b051(1.3.10) 866680c(1.3.12) 14a5083(1.3.5) 73f3fbb(1.3.6) 15c340d(1.3.9) c708087(1.3.8) + earlier da80696(1.3.1/1.3.11), 1.3.2–1.3.4 commits.
- **Next:** Phase-01.5 CI/CD Validation Harness.

## [2026-09-27 21:30 UTC] — Phase 01.5 START — CI/CD & Validation Harness
Scope: 5 tasks (1.5.3.1–1.5.3.5), 19 AC rows. GitHub remote `github.com/klzk-myy/exc-local` (private) created + pushed — real CI verification possible.

### [2026-09-27 21:55 UTC] — Task 1.5.3.2 Spec Validation Harness — DONE
- **Files:** `tests/spec/` module `exchange-testspec` (validator CLI run|extract|list|stubs|merge, spec/ framework, checks/ registry, golden/ corpus, checkpoints/ corpus+PENDING.md, README policy).
- **Verification:** `go test ./...` — spec 6 + golden 29 PASS; extractor yields **542 strict checkpoints / 479 tasks**; FNV-1a%4 shards deterministic [117,131,140,154], two runs identical; JSON report schema verified (571 records); failure/vanished → rc=1, clean → rc=0; `--fail-on-skip` works.
- **Checklist:** DoD 5/5, SDD 5/5 marked.
- **Decisions:** checkpoint ID `P<phase>-T<task>-C<idx>`; status policy `implemented`/`pending`/`skip` — pending (later-phase) checkpoints report but don't fail; harness is separate go.mod to isolate deps; checkers call ctest/gtest/`go test -run` subprocesses (internal/ not cross-module importable); skip-aware.
- **Deviation:** strict count 542 vs canonical 543 — raw grep includes 1 prose "Spec checkpoint:" mention (this task's text); reconciled + asserted in self-check C1.

### [2026-09-27 22:00 UTC] — Task 1.5.3.4 Supply-Chain Security — DONE
- **Files:** `.github/workflows/security.yml` (5 jobs), `.trivyignore`+`.trivyignore.yaml`, `.gitleaks.toml`, `scripts/ci/{dep_cooldown,cpp_dep_audit,scan_images,sarif_gate}.sh`, `core/third_party/vendored-deps.txt`.
- **Verification (local):** gitleaks clean (1 FP allowlisted by exact-token); trivy fs+image scanned 4 imgs, gate green after documented exceptions; dep_cooldown verified vs proxy.golang.org; cpp_dep_audit FetchContent SHA256 pin ✓ + OSV fail-closed ✓; sarif_gate high→FAIL/low→PASS/malformed→FAIL; actionlint clean.
- **Checklist:** DoD 4/4, SDD 2/2 marked.
- **Decisions:** CodeQL @v4; `--ignore-unfixed` on image gate (Debian unactionable noise) + SCAN_UNFIXED=1 audit mode; path-scoped yaml ignorefile as gate-canonical; dep-cooldown gates new deps AND version bumps (go + npm).
- **Security remediation applied by orchestrator:** bumped pgx v5.7.5→v5.9.2, x/crypto→v0.57, x/net→v0.59, x/text→v0.42, go directive→1.26.0/toolchain go1.26.8 (pgx v5.9.2 requires ≥1.25; new toolchain also clears 24 stdlib vulns) — `govulncheck` now reports **0 reachable vulns**; `go test`/`vet` all green.
- **Deviation:** triggers cover [main, master] (repo default branch is master).

### [2026-09-27 22:30 UTC] — Task 1.5.3.5 Fault Injection Harness — DONE
- **Files:** `ci/fault-injection/` (Go module `exchange/fault` + `cpp/walverify.cpp` + `run.sh`); 10 scenarios, 143 checks.
- **Verification (orchestrator re-ran `./run.sh`):** **10/10 scenarios, 143/143 checks, 0 fail** — wal_corrupt(38), wal_crash_recovery(10, kill -9 → replay to last valid seq), clock_jump(33, 200µs→TIME_SYNC_LOSS_HALT L0/503), audit_tamper(8, exit 2), shm_faults(9), aeron_unreachable(5), pg_serializable(14, deterministic 40001→TRANSACTION_CONFLICT_RETRY_EXHAUSTED, 12-writer exactly-once), arithmetic_overflow(4), malformed_frames(10), auth_edge_rejects(12, 401/403 RFC7807). Tiers L0–L3 all covered.
- **Checklist:** DoD 3/3, SDD 2/2 marked.
- **Decisions:** auth/scope edges asserted via harness-local HMAC oracle + real pkg/errors.Problem envelopes (Phase-05 middleware owns production path); audit scenario re-anchors today's Merkle root post-probe (prod anchors lag by design); storm asserts exactly-once+coded-failures (retry-exhaustion under SSI contention IS the fail-closed contract).
- **Deviation:** none.
