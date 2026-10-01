# 8h Engine Soak — 2026-09-28 02:34 → 10:35 UTC

**Verdict: PARTIAL (honest).** Not a pass against the literal ACs (72h / 50k/s /
recovery <10s). The run's value is defect evidence: two real production defects
found and fixed, plus four open items documented below.

## Configuration
- Rate 15,000 ord/s · shard 0 · instrument 7 · duration 8h
- Crashes injected at +3600s/+10800s/+18000s/+25200s (SIGKILL)
- Burst at +21600s: 15k→18k/s for 120s (the +7200s burst was dropped —
  `--burst-at` is not repeatable in monitor.sh, second flag overwrote it)

## Journal integrity (final wal_audit scan)
- 303,738,471 entries / 22 rotated segments / ~23GB
- ORDER_NEW 138,788,877 · TRADE 33,936,152 · zero seq gaps, zero regressions,
  zero corrupt payloads, zero duplicate trade_ids

## Crash recovery timeline
| crash | offset | recovery | note |
|---|---|---|---|
| #1 | 1h   | 1.76s   | small WAL |
| #2 | 3h   | 65.4s   | full replay 21GB — no snapshot machinery yet |
| #3 | 5h   | 81.4s   | full replay 28GB; post-boot ingress starved ~2h (see findings) |
| #4 | 7h   | 189.5s  | full replay ~40GB; post-boot snapshot emission live (commit 481d3cf binary) |

Post-run restart using the shutdown snapshot (seq 303,738,471, 108MB file):
**19.9s** kill→ready vs 189s full replay — snapshot path works at scale but
restore (≈1M orders + 103M-entry dedup rebuild) still exceeds the 10s AC.

## Memory
RSS peaked ≈6.5–6.9GB — under the 12GB criterion.

## Throughput honesty (ORDER_NEW deltas between 30-min audits)
- 02:34→07:34 (~5h): ~15k/s sustained at target
- 07:35→09:34 (~2h): **~0/s** — the crash-#3 binary attempted snapshot
  serialization on every 1ms tick and failed at the 64MiB WAL-entry cap
  (book ≈1M orders ≈108MB blob); the failure was silent (`SerializeFailed`
  unreported) and each attempt walked the full book — matching loop starved.
  Fixed in 481d3cf: dedicated file cap (u32 format bound) + exact file-size
  cross-check before alloc + SerializeFailed transition logging.
- 09:37→10:35 (~1h): ~1.8k/s — degraded post-restart ingest (deep book +
  103M-entry dedup ledger; profiling is an open item)

## Open defects (follow-up work)
1. Post-restart throughput ~1.8k/s vs 15k/s pre-crash — needs profiling
   (candidates: dedup-ledger size, book depth, per-order cost growth).
2. `CRITICAL_BACKPRESSURE` logged per cycle, unthrottled — engine.log reached
   25.6GB in 8h (head+tail excerpts preserved under logs/).
3. monitor.sh `wal_tail`/`wal_size` sample columns freeze after WAL segment
   rotation (lexical vs numeric segment pick — cosmetic, WAL itself healthy).
4. monitor.sh `--burst-at` is not repeatable (overwrites; pass a quoted list).
5. Editing monitor.sh mid-run corrupts bash's lazy-read — the exit path hit a
   syntax error at line 617; this archive was assembled manually.

## Files
- `events.jsonl` — crash/burst events with recovery_ms
- `audits.jsonl` — 15 periodic WAL scans (all clean)
- `final-audit.json` — post-run full scan
- `samples.csv` — 60s engine RSS/CPU/WAL samples
- `loadgen-report-{1..7}.json` — per-loadgen-interval stats (5–7 reflect the
  starvation windows: send_drops/ring_drops dominate)
- `monitor.log`, `logs/` — monitor log + engine log head/tail excerpts
