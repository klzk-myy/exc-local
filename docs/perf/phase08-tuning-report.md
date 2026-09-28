# Phase-08 Task 8.3.2/8.3.3 — Load Test & Performance Tuning Report

**Date:** 2026-09-28 · **Author:** devin (Phase-08 perf scope) · **Status:** honest-measurement report; no acceptance checkbox ticked.

This report covers Task 8.3.2 (load testing harness + bounded measurements) and
Task 8.3.3 (profiling and evidence-based tuning). Every number below is a real
measurement on this host; nothing is extrapolated to a PASS.

---

## 1. Environment & measurement caveats

- Host: Intel Core i7-14700K (20 threads), Linux, `/dev/shm` IPC, kernel
  `perf_event_paranoid=4`.
- **Heavy third-party contention during all Phase-08 runs:** unrelated
  long-running `matching_engine` processes (bigipc soak, snapbench, integration
  shards owned by other agents) pinned 75–90% CPU each; load average
  **16–21** during the measurement window. The engine under test received only
  ~5% of one core. Numbers here measure behavior *under starvation*, not clean
  capacity.
- Same host, earlier same day (02:34–10:35 UTC, quieter): the Phase-02.5 8h
  soak sustained **15,000 orders/s at target for ~5h** with zero WAL seq gaps,
  zero duplicate trade_ids, zero corrupt payloads over 303.7M WAL entries
  (`tests/soak/artifacts/20260928T103724Z-shard0-8h/`). That is the honest
  demonstrated ceiling of this codebase on this hardware to date.
- **The 50,000 orders/s, 1h, p99 ≤ 50µs criteria are NOT demonstrated** and are
  not expected to pass on this host. They remain OPEN.

## 2. Harness (new, `tests/load/`)

| file | role |
|---|---|
| `tests/load/run.sh` | orchestrator: engine(s) + soak loadgen (leg A), second engine + marketdata + `bookpump` + `wsprobe` (leg B), gateway + `restprobe` (leg C); computes AC verdicts; `-dev-all-accounts` default; snapshot-ack shm cleanup |
| `tests/load/bookpump/main.go` | order injector driving real book activity on the md engine's `_out` stream for fanout measurement |
| `tests/load/wsprobe/main.go` | N WebSocket conns, subscribe, counts open failures / seq gaps / disconnects / reconnects / zero-frame conns |
| `tests/load/restprobe/main.go` | paced or closed-loop REST probing, 1µs histogram, status/error accounting, JSON report |
| `tests/load/go.mod`, `go.sum` | module reusing `services` IPC packages + gorilla/websocket |

Leg A reuses `tests/soak/loadgen` unchanged — it already writes
`Event{OrderNew}` FlatBuffers to the real shm producer ring
(`ipc.OpenChannel`, `ShmRing::try_write`), drains `_out`, and reports
latency histograms, send/ring drops, duplicate trade_ids, decode errors.

Artifacts: `tests/load/results/phase08-run1/` (600s, three legs, 50k target),
`tests/load/results/phase08-run2/` (300s, core leg only, 20k target).

## 3. Load measurements (bounded, contended)

### 3.1 Run 1 — 600s, 50k target, all three legs (`phase08-run1/`)

| metric | measured |
|---|---|
| duration | 600.0s |
| orders_sent | 4,352 (target 30,000,000) |
| fills | 4 · cancels 100 |
| achieved send rate | 7.25/s |
| p50/p99/p999 order latency | 110,437µs (only 4 fill samples) |
| send_drops | 29,988,233 |
| ring_drops (TryWrite misses) | 1,919,247,120 |
| dup trade_ids / decode errors | 0 / 0 |
| **WS leg** | 120/120 conns opened · 46,320 frames delivered · **0 seq gaps · 0 disconnects · 0 reconnects · 0 zero-frame conns** |
| **REST leg** | 300,000 reqs @500/s target (499.99/s achieved) · **0 errors · all HTTP 200** · p50 226µs · p99 6,312µs · p999 21,275µs · max 82.2ms |

### 3.2 Run 2 — 300s, 20k target, core leg only (`phase08-run2/`)

| metric | measured |
|---|---|
| orders_sent | 4,352 · fills 4 · cancels 100 |
| achieved send rate | 14.5/s |
| p99 order latency | 207,182µs |
| send_drops / ring_drops | 5,995,636 / 383,721,216 |
| dup / decode errors | 0 / 0 |
| engine log | 301 `CRITICAL_BACKPRESSURE` lines (throttled 1/s) |

### 3.3 AC verdicts

| AC | verdict |
|---|---|
| 50k orders/s sustained 1h | **FAIL / NOT RUN to duration** (7–15/s achieved under contention; bounded runs ≤600s) |
| p99 ≤ 50µs tick-to-trade | **FAIL** under contention (110–207ms); transport-only p99 is 3–4µs (§5) |
| p99 ≤ 5ms REST | **FAIL** in the 600s contended run (6,312µs); a short low-contention window earlier measured p99 ≈ 2.3ms — the criterion is host-bound marginal |
| zero order loss | **FAIL** — massive send/ring drops while the engine sat in ≥95% halt (§4.3) |
| 100+ WS zero drops | **PASS** — 120 conns, 46,320 frames, zero gaps/disconnects over 600s including while the md engine was itself in backpressure |

## 4. Root-cause analysis of the throughput collapse

### 4.1 Transport is not the bottleneck

`deploy/scripts/bench_ipc.sh` (50k-frame echo, same host, same contention):

| transport | avg | p50 | p99 | max |
|---|---|---|---|---|
| shm ring | 675ns | 546ns | 3,921ns | 393µs |
| Aeron | 1,131ns | 790ns | 3,001ns | 1.9ms |

Both well inside the 50µs p99 budget even under load — the collapse is in the
engine consume path, not the IPC mechanics.

### 4.2 CPU starvation → ring fills

With the engine at ~5% of a core, the drain rate (~500k loop beats/s effective,
each polling the inbound SPSC ring) cannot keep up with a 20–50k/s producer.
The 4,096-slot inbound ring fills within the first beats.

### 4.3 Defect finding — the ≥95% halt watermark is self-sustaining

`core/src/matching/EngineLoop.cpp` `spin_once()` implements §2.7.3:

- occupancy ≥80% → `shed_new_orders`, still drains (cancels never shed);
- occupancy ≥95% → **halt ingress for the cycle — `run_once` is skipped entirely.**

Once occupancy crosses 95%, the consumer never consumes again: `tail` is
frozen, `head` only grows, occupancy can never fall below 95%. The halt is a
**permanent latch** — every later producer write drops at the ring; measured
`ring_drops` in the billions are retry attempts against a frozen-full ring.
The only recovery is engine restart. Engine logs confirm: hundreds of
`CRITICAL_BACKPRESSURE` lines (correctly throttled to 1/s by the Phase-02.5
fix) for the entire run.

This is fail-closed by intent ("producer sees a full transport instead of us
consuming commands we cannot honor"), but as implemented it converts any
single beat that jumps ≥80%→≥95% into permanent engine death. A burst of
~820 slots between two polls — trivial under CPU starvation — is sufficient.

**Recommendation (not applied — §2.7.3 is spec policy, needs spec/design
review):** at ≥95%, keep draining with `shed_new_orders` (sequenced rejects
preserve fail-closed accounting and let the ring empty), or add a hysteresis
(e.g., halt only while a decaying overload counter is hot). As written, the
halt cannot ever un-halt. Flagging to spec owners.

### 4.4 Why fills were 4/4352

`cross-pct 2` means ~98% of orders are non-crossing LIMITs that rest on the
book; only ~2% cross. With the engine halted almost immediately, only the
early trickle produced matches. This is expected workload behavior, not a
matching defect.

## 5. C++ profiling

- `perf`: **blocked** — `perf_event_paranoid=4` denies CPU/kernel/ftrace events.
- `gdb` attach sampling: **blocked** — `ptrace` restrictions (`Inappropriate ioctl for device`).
- Fallbacks actually used:

### 5.1 gprof (-pg build, 90s run @5k target, `/tmp/prof`)

| % self | symbol | calls | note |
|---|---|---|---|
| 39.1% | `wal_encode_entry` | 45,331 | ~5.5µs/WAL entry — dominant per-event cost |
| 20.3% | `EngineLoop::spin_once` | 8.55M | spin-loop overhead (idle beats under starvation) |
| 10.9% | `LatencyHistogram::record` | 8.55M | per-beat histogram |
| 9.4% | watchdog thread `_M_run` | — | thread overhead |
| 6.3% | `Watchdog::check_once` | 779k | |
| 3.1% | `MatchingEngine::process_triggered` | 45k | |

`wal_encode_entry` at ~5.5µs/entry is the top real hot path: at 50k/s it
alone would consume ~275ms CPU/s. Combined with crc32c_hw per entry and
per-beat bookkeeping, a dedicated core is mandatory for the 50k target.

### 5.2 snapbench (`core/build/snapbench`)

- `book 1000000 4000`: build 1M orders/4k levels in 71ms (**14.1M ord/s** raw
  in-memory ops); serialize 134.6ms (108MB); parse 58.4ms; restore-insert
  38.6ms (25.9M ord/s); validate 15.7ms.
- `walgen`/`recover`: 20 segs × 50k entries; snapshot store 199.7ms (54MB);
  `recover()` **155.6ms** with covered_skips=950k, dedup=50k — the fast
  WAL-skip path is healthy; the 8h soak's >10s restore cost is dominated by
  book deserialize + dedup-ledger rebuild, not WAL scanning.

### 5.3 C++ changes made (minimal, verified)

Two changes justified by direct measurement, not speculation:

1. **`IpcPublisher::publish_book_snapshot` — L2 frame never fit the ring.**
   Previously serialized *all* book levels (up to `kMaxLevels`/side). A
   spec-§10.2 top-20 frame is ~1.3–1.5KB > 1024B slot → every deep-book
   publish dropped at `emit()` (`drops_++`), and the O(depth) serialization
   ran on the matching thread per book change. Fixed: cap at
   `kWireDepthLevels = 20` (spec §10.2 "Top 20 price levels"; the Go
   conflator keeps top-20 internally anyway — `internal/marketdata/l2.go`)
   and raise the engine's shm slot payload to **2048B** (`main.cpp`; attach
   side adopts geometry from the ring header). Files:
   `core/include/matching/IpcPublisher.hpp`,
   `core/src/matching/IpcPublisher.cpp`, `core/src/main.cpp`.
2. **marketdata IPC attach fix** (Go side — see §6): the service could not
   attach at all before this fix; deep-book frames now flow.

`ctest --output-on-failure`: **29/29 pass** after these changes.

## 6. Go services

- **Fix applied:** `services/cmd/marketdata/main.go` and `producers.go`
  passed `capacity=0, slot_payload=0` to `ipc.OpenChannel`/`OpenRing`, which
  validates geometry *before* reading the existing ring header →
  `ipc: capacity 0 not a power of two`, marketdata never attached. Now passes
  `ipc.DefaultRingCapacity`/`DefaultRingSlotPayload` (validated; overridden by
  the live header on attach). Confirmed live: `marketdata: ipc delta source
  attached`, 46,320 frames fanned out.
- Benchmarks:
  - `go test -bench=. ./internal/bridge` → `BenchmarkBufferPush` **39.2ns/op,
    32B, 1 alloc** — bridge buffer is not a hotspot.
  - `go test -run TestIngestBenchSustainedRate ./internal/settlement`
    (bench env enabled): **200,000 fills, 39 flushes, 34.865s ≈ 5,736
    fills/s** — settlement ingest is well under 50k and bounded by
    batch/commit path. Candidate tuning (larger flush batches, prepared
    statements, async apply) documented here; no code change made — the
    5.7k/s number exceeds the engine's demonstrated throughput anyway.
- pprof: no service exposes a pprof endpoint; adding one was judged outside
  the "no hot-path changes without evidence" scope. Evidence gathered via
  in-repo benchmarks + live `healthz`/`metrics` fields instead.
- `gofmt -l`: clean on all touched Go files.

## 7. PostgreSQL — EXPLAIN ANALYZE + one justified index

Scratch DB: `postgres://exchange:exchange_dev@/migverify?host=/tmp&port=55433`.
300k rows seeded into today's `trades` partition.

**Hot query** (`internal/orders/store.go` `ReferencePrice`, runs on the
order-ingress validation path):

```sql
SELECT price::text FROM trades WHERE instrument_id=$1 ORDER BY id DESC LIMIT 1
```

`trades` is RANGE-partitioned on `created_at`; pkey is `(id, created_at)` —
no index could satisfy `instrument_id` + `id DESC`. Planner did an
Index-Scan-Backward-per-partition filtering post-hoc.

| | execution time |
|---|---|
| before | **17.341ms** (sparse instrument, last trade 300k rows deep) |
| after | **0.237ms** (~73×); re-verified post-apply: Merge Append + per-partition index probes, exec **0.207ms** |

**Migration added:** `services/internal/db/migrations/192_trades_instrument_id_desc.{up,down}.sql`
— `CREATE INDEX idx_trades_instrument_id_desc ON trades (instrument_id, id DESC)`
on the partitioned parent; PostgreSQL propagated it to all 31 leaf
partitions (`trades_p*_instrument_id_id_idx`, all `indisvalid`). Verified
applied on the scratch DB. Registered in the Phase-01 forward-migrations
index (§1.3 task note added).

Audited and already-covered paths (no change): `balances` pkey
`(account_id, currency)`, `idx_orders_account_id_status`,
positions account+instrument unique/index, settlement account/reference
indexes.

## 8. Redis audit — no change justified

Existing pipelines already cover batch paths: `internal/settlement/vip_engine.go`,
`internal/config/sharding.go`, `internal/auth/session_store.go`,
`internal/redis/client.go`, `internal/risk/limits_service.go`.

Examined candidates:

- marketdata seq-mirror: single Lua high-water-mark script per event —
  already one round trip.
- WS dedup (`SETNX` then `GET` on collision): the `GET` is semantically
  dependent on the `SETNX` result — cannot be safely batched into one
  pipeline without changing semantics. No measurable win.
- No other per-op round trips on hot paths found.

## 9. Aeron audit — no change on this host

`config/aeron-low-latency.properties` + `AeronDriverConfig` already encode
the low-latency profile: dedicated threading mode, busy-spin sender/
receiver, conductor backoff, 128MB term buffers, sparse term files,
1408B MTU, 16MB socket buffers, optional CPU affinity, and a kernel-tuning
script (`scripts/tune-kernel-network.sh`).

Measured Aeron echo p99 = **3.0µs** (bench_ipc) — within budget. The binding
constraint on this host is CPU availability, not Aeron configuration.
Recommendations for the dedicated benchmark host, unchanged from the existing
profile: isolated cores for driver + engine affinity, `tune-kernel-network.sh`,
IRQ affinity off the engine core, and Aeron `channel`/`term` settings left as
profiled.

## 10. Files changed

- `core/include/matching/IpcPublisher.hpp` — `kWireDepthLevels = 20` + comment.
- `core/src/matching/IpcPublisher.cpp` — top-20 cap on snapshot serialization.
- `core/src/main.cpp` — engine shm slot payload 1024 → 2048B (+comment).
- `services/cmd/marketdata/main.go`, `producers.go` — IPC attach geometry fix.
- `services/internal/db/migrations/192_trades_instrument_id_desc.{up,down}.sql` — new.
- `docs/Phase-01-Project-Foundation.md` — forward-migrations index note for 192.
- `tests/load/` — new harness (run.sh, bookpump, wsprobe, restprobe, go.mod/sum).
- `docs/perf/phase08-tuning-report.md` — this file.

`tests/integration/` untouched (owned by another agent). No spec/phase
checkboxes ticked. No commits.

## 11. Open items / recommended follow-ups

1. **§2.7.3 halt-watermark livelock** (§4.3) — needs spec/design decision:
   drain-with-shed at ≥95% or hysteresis. Currently any ≥95% cross is a
   permanent engine halt.
2. Re-run the load suite on a **dedicated host** (isolated cores, kernel
   tuning applied, no co-tenant load). The honest expectation on this class
   of hardware is ~15k/s (measured 5h sustained), not 50k/s; the 50k/50µs
   target likely needs the tuning roadmap below plus dedicated silicon.
3. `wal_encode_entry` ~5.5µs/entry is the top per-event cost — candidates:
   encode directly into the WAL segment buffer (skip staging), reduce per-
   entry CRC/seq work, or batch-encode. Requires profiling on a quiet host.
4. Settlement ingest ~5.7k fills/s — batch/commit bound; tune if settlement
   becomes the constraint after the engine improves.
5. REST p99 is host-marginal (2.3ms quiet / 6.3ms contended): likely passes
   on a dedicated host at p50=226µs baseline.
6. Phase-01 forward-migrations index does not list implementation-era
   numbers 109–191 — pre-existing doc drift; only 192 was registered here.
7. `perf`/ptrace blocked by host policy — request
   `perf_event_paranoid≤2` + `CAP_SYS_PTRACE` on the benchmark host for real
   CPU sampling.
