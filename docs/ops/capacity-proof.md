# Capacity Proof & Sizing Summary

**Phase-09 Task 9.3.29 (item 2)** · **Authority:** spec §19.14, §19.9 · **Status:** contract + current evidence. Sizing enforcement code `services/internal/ops/capacity.go` **pending**; numbers marked *measured* come from `docs/perf/phase08-tuning-report.md` (contended host — read §1 caveats).

## 1. Go service HPA targets (contract)

Task 9.3.2 sets the base rule (CPU >70% → scale, min 2 / max 10); Task 9.3.29 adds the full target set — CPU + p99 latency + queue depth per service:

| Service | CPU trigger | p99 latency trigger | Queue trigger | Min/Max |
|---|---|---|---|---|
| `gateway` | >70% | REST p99 >5ms sustained 5m | gateway queue depth >250 (load-shed arming) | 2/10 |
| `marketdata` | >70% | WS delivery p99 >100ms | producer depth / client saturation (evict 4008 at 2.0s) | 2/10 |
| `fix` | >70% | session RTT breach | inbound session queue | 2/6 |
| `settlement` | >70% | n/a (throughput-bound) | JetStream `nats_consumer_pending` >50k sustained | 2/6 |
| `compliance` | >70% | screening p99 | `surveillance`/`compliance` consumer pending | 2/6 |
| `admin` | >70% | n/a | evaluator lag | 2/4 |
| `bridge` | per-shard pinned (not HPA — colocated daemon) | publish ack >2s | `bridge_buffer_depth` >10k (alert), >100k (cap) | 1 per shard |

HPA metrics wiring (custom/external metrics for the latency+queue triggers) — pending Task 9.3.29 implementation; CPU-only HPA is the interim state (Task 9.3.2).

## 2. Store & backbone sizing

| Component | Contract | Evidence/notes |
|---|---|---|
| NATS JetStream | 7 streams, R3, `FileStorage`, `MaxAge` 7d, WorkQueuePolicy, 2min dedup (`services/internal/nats/streams.go`); size consumers for ≤50k pending (alert bound) | `natsctl health` lag report; stream/consumer sizing review each capacity cycle |
| pgbouncer | pool sized to PG `max_connections` headroom; per-service pool shares | `services/config.example.yaml` `postgres.max_conns` is the current lever; formal pool map pending `capacity.go` |
| Redis | connection pool per service; Sentinel 3-node (`quorum=2`, `down-after=2000ms`, `failover-timeout=5000`) | pipelines already on hot paths (perf report §8 — no change justified) |
| PostgreSQL | ~1.2GB/day at 10M orders (§19.9); 90d hot ≈110GB SSD before detach | nightly archival cron (Task 9.3.17), `partition_archive_log` (120) |
| ClickHouse | ~4.5GB/day compressed at 100M ticks; 90d ticks ≈400GB; 5y aggregates ≈50GB (§19.9) | `deploy/clickhouse/` backup cadence daily/hourly |
| WAL/segments | ~15GB/day archive projection (Task 9.3.19) | `exchange archive-lifecycle --shard=N` |

## 3. Headroom & burst contract

- **5× volatility burst test:** sustained 250k/sec envelope for 15min on the dedicated burst environment (separate from the 75k staging mini-mirror — remediation #35); executed as a **release-gate input**. Harness: `tests/load/run.sh` legs A–C (loadgen + bookpump/wsprobe/restprobe). Status: **not yet executed** — burst env + 250k generator capacity pending; current honest ceiling **15k ord/s for ~5h** (Phase-02.5 soak) and 500 REST req/s proven.
- **Headroom alerts (Task 9.3.19):** CPU >70%/>15min, memory >75% bare-metal, disk >70% on DB/WAL mounts, NIC >60% link — deployed as `deploy/monitoring/capacity-alerts.yml` (Task 9.3.19; sizing model doc [`capacity-planning.md`](./capacity-planning.md)).
- **Scale triggers (§19.9):** shard split/new pair when engine CPU >60% for 15min, order-pool utilization >70%, or Aeron queue backlog >500.

## 4. Current capacity position (measured, honest)

| Capability | Contract | Demonstrated | Gap |
|---|---|---|---|
| Core throughput | 50,000 ord/s, 1h | 15,000 ord/s ~5h (cleaner window); 7–15/s under heavy contention | dedicated host + `wal_encode_entry` (~5.5µs/entry) optimization — perf report §11 |
| Tick-to-trade p99 | ≤50µs | transport p99 shm 3.9µs/Aeron 3.0µs; end-to-end blocked by CPU starvation | dedicated cores + halt-latch fix (perf report §4.3 — spec-level) |
| REST p99 | ≤5ms | 2.3ms quiet / 6.3ms contended | marginal — likely passes on dedicated host |
| WS integrity | zero gaps/drops at 100+ conns | 120 conns, 46,320 frames, 0 gaps over 600s | met at this scale; 10k-conn target pending Phase-08.5 rerun |
| Settlement ingest | keep pace w/ engine | ~5,736 fills/s | exceeds current engine ceiling; revisit at 50k/s |
| Snap/recover | warm recovery ≤10s engine | `recover()` 155.6ms; full boot incl. book rebuild dominated by deserialize+dedup | meets |

## 5. Quarterly review

Capacity review automation exports Prometheus history into growth forecasts (6-month runway) — automation pending Task 9.3.19 item 4; interim: this table re-verified each quarter alongside the DR drill report and folded into the SLO monthly review.
