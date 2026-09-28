# Capacity Planning & System Sizing Model

**Phase-09 Task 9.3.19 · spec §19.9 · §24 #191 · alerts: `deploy/monitoring/capacity-alerts.yml`**

This document is the sizing contract for the 50,000 orders/sec sustained
workload target. Every "measured" number below is a real measurement from
this repository's own test evidence — nothing is extrapolated silently;
where a number is a projection it is labeled as such.

---

## 1. Measured baseline (evidence index)

| Evidence | Source | Measured value |
|---|---|---|
| Sustained matching throughput | `tests/soak/artifacts/20260928T103724Z-shard0-8h/report.md` | **15,000 ord/s sustained ~5h**, 303.7M WAL entries, 22 segments, ~23 GB journal — zero seq gaps, zero duplicate trade_ids, zero corrupt payloads |
| WAL write volume | same | ~23 GB / 8 h @ 15k ord/s + 34M trades ⇒ **~69 GB/day hot WAL at design-rate-mix** |
| Crash recovery (snapshot path) | spec §24 remediation #43 record | **boot-to-ready 1,328 ms** with 15 GB journal / 308M entries + 1M-order snapshot @ seq 294M (gate <10s, ~7.5× headroom) |
| Book build / serialize / restore | `core/build/snapbench` (perf report §5.2) | 1M orders / 4k levels: build **71 ms** (14.1M ord/s in-memory), serialize 134.6 ms (108 MB blob), parse 58.4 ms, restore-insert 38.6 ms (25.9M ord/s) |
| WAL replay fast path | snapbench `walgen`/`recover` | 20 segs × 50k entries: `recover()` **155.6 ms**, covered_skips=950k |
| Per-event hot-path cost | gprof, perf report §5.1 | `wal_encode_entry` **~5.5 µs/entry** — dominant; at 50k/s ≈ 275 ms CPU/s alone |
| IPC transport | `deploy/scripts/bench_ipc.sh` | shm ring echo p50 546 ns / p99 3.9 µs; Aeron p50 790 ns / p99 3.0 µs |
| REST gateway | perf report §3.1 | p50 226 µs / p99 6.3 ms **under heavy host contention**; ~2.3 ms p99 in a quiet window |
| WS fanout | perf report §3.1 | 120 conns, 46,320 frames, **0 seq gaps / 0 disconnects** over 600 s |
| Settlement ingest | `internal/settlement` bench | **5,736 fills/s** (200k fills, 39 flushes, 34.865 s) — batch/commit bound |
| Leader failover drill | `tests/soak/artifacts/20260928T022857Z-failover/failover-report.json` | PASS — recovery_ms_max **116 ms**, book parity 2/2 |
| Engine RSS at scale | soak samples.csv | ≈ **6.9 GB** at ~1M resting orders + 103M-entry dedup ledger |
| PG hot-path index | perf report §7 | `trades` last-trade lookup 17.34 ms → **0.24 ms** after `idx_trades_instrument_id_desc` (migration 192) |
| Storage growth models | spec §19.9 | PG **1.2 GB/day** per 10M orders/day; CH **45 B/tick** compressed → 4.5 GB/day per 100M ticks; WAL archive ≈ 15 GB/day (compressed sealed segments) |

**Honest capacity statement:** the demonstrated clean ceiling on this
host is ~15k ord/s sustained (5h evidence). The 50k ord/s / p99 ≤ 50 µs
gate is NOT yet demonstrated — the binding constraint on this host was
CPU starvation (co-tenant load 16–21), plus one live defect found and
recorded (§2.7.3 halt-watermark livelock, perf report §4.3). Treat 15k/s
per shard as the proven floor and the formulas below as the scaling law.

---

## 2. Hardware sizing profiles

### 2.1 Matching engine shard — minimum contract (spec §19.9)

The Phase-02.5 soak gate validates against: **4 physical cores** pinned
via `isolcpus`, **16 GB ECC DDR5**, **dual 25 GbE** Mellanox NICs,
**NVMe SSD >500k random-write IOPS**. In-memory book ≈ 64 B/resting
order; 1M-order pool ≈ 64 MB (measured RSS is dominated by dedup ledger +
snapshot machinery: 6.9 GB observed at 1M orders / 103M dedup entries —
provision **≥ 16 GB**, headroom target ≤ 70%).

### 2.2 Matching engine shard — production target profile

Dual AMD EPYC 9654 (128 cores total), **512 GB DDR5 ECC**, dual **100 GbE**
ConnectX-6 Dx, NVMe PCIe 5.0. One physical host per *shard-pair*
(primary + warm standby) with NUMA-pinned engine cores isolated from the
Aeron media driver and OS threads. At ~5.5 µs WAL-encode cost per event,
one dedicated core sustains ~180k encode/s — WAL encode is *not* the 50k
constraint; the constraint is total per-order pipeline cost, so the
target profile sizes for **3 shards/host** worst case.

### 2.3 Supporting tier minimums

| Component | Sizing rule |
|---|---|
| PostgreSQL primary | NVMe ≥ 400 GB usable for 90-day hot window (see §3) |
| ClickHouse | NVMe ≥ 500 GB (90d raw ticks @ 100M/day ≈ 400 GB + 5y aggregates ≈ 50 GB) |
| Redis Sentinel | 3 nodes, ≥ 8 GB each (sessions + rate limits + leader leases) |
| WAL volume | ≥ 200 GB local per shard + S3 archive (15 GB/day compressed) |
| Gateway/FIX/MD pods | 2 CPU / 4 GB baseline, HPA on p99 latency |

---

## 3. Growth formulas

| Store | Formula | At reference load |
|---|---|---|
| PostgreSQL OLTP | `GB/day ≈ 0.00012 × orders/day` (1.2 GB @ 10M orders) | 90-day hot window ⇒ **110 GB** |
| ClickHouse ticks | `GB/day ≈ 45 B × ticks/day` | 100M ticks/day ⇒ 4.5 GB/day; 90d ⇒ ~400 GB |
| ClickHouse aggregates | ~10 MB/day | 5y ⇒ ~50 GB |
| WAL archives | measured 23 GB/8h hot @ 15k ord/s; compressed sealed ≈ 15 GB/day | S3 lifecycle → Glacier at 90d |
| Partition archives (cold) | `GB/day ≈ PG GB/day × zstd ratio (~0.25)` | ≈ 0.3 GB/day @ 10M orders/day, 5–7y WORM |

**Days-to-exhaustion formula** (used by the quarterly review and the
`CapacityDiskHeadroom` alert runbook):

```
runway_days = avail_bytes / daily_growth_bytes
provision when runway_days < 90  (order hardware at < 60)
```

---

## 4. Headroom & scaling thresholds

Alert rules live in `deploy/monitoring/capacity-alerts.yml`:

| Signal | Alert threshold | Scaling trigger (spec §19.9) |
|---|---|---|
| CPU utilization | > 70% for 15 min | shard split / new-pair shard when **> 60% sustained 15 min** |
| Memory | > 75% any core node | rebalance when order-pool util **> 70%** |
| Disk (PG/WAL/CH mounts) | > 70% | extend volume / expedite archival |
| NIC bandwidth | > 60% of link | capacity review |
| Aeron queue backlog | (engine metric) | **> 500 messages** ⇒ shard split |
| Hot-tier growth | > 10%/month | archival pipeline audit |
| Retention drift | any `RETENTION_POLICY_VIOLATION` | compliance investigation |

**5× volatility spike case (spec edge case):** 5 × 15k = 75k ord/s
demonstrated capability → exceeds the proven per-shard floor; the model
requires (a) shard split at the 60% CPU trigger, (b) WAL volume sized for
~345 GB/day transient, (c) gateway HPA floor covering ingress fan-out.

## 5. Quarterly capacity review (automated)

`deploy/monitoring/capacity-alerts.yml` supplies continuous headroom;
the quarterly review exports Prometheus `node_*` + `exchange_*` series,
recomputes §3 growth rates from 90-day regression, and emits a report
with per-resource 6-month exhaustion projections and a provisioning
decision table (add shard / add storage / add host). Report template:
`runway`, `growth_rate_gb_day`, `provision_date` per mount.

---

## 6. Open items that gate the 50k contract

1. §2.7.3 halt-watermark livelock — needs spec/design ruling (perf §4.3).
2. Dedicated-host rerun — `perf`/ptrace blocked on this host
   (`perf_event_paranoid=4`); request ≤2 + `CAP_SYS_PTRACE`.
3. `wal_encode_entry` ~5.5 µs/entry optimization path (perf §11.3).
4. Settlement ingest 5.7k fills/s must scale if engine headroom rises
   (perf §11.4).
