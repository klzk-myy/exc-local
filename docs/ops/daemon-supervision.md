# Daemon Supervision Runbook (Task 9.3.28 item 5, spec §19.13)

Operational procedures for watchdog trips, crash triage, leader demotion,
and bare-metal rolling restarts.

---

## 1. Telemetry

`exchange-watchdogd` exports on `:9110/metrics` (and each engine unit's own
Prometheus surface):

| Metric | Meaning | Trip line |
|---|---|---|
| `daemon_up{name}` | unit reachable + reporting | 0 for >2 scrapes → page |
| `watchdog_heartbeat_timestamp_seconds{daemon}` | last pet | age >2× WatchdogSec → hang |
| `loop_latency_microseconds{shard}` | matching-loop cycle | >500µs warn, >2ms halt (T2) |
| `clock_offset_nanoseconds` | PTP drift | >100µs → P1 (RTS 25) |

## 2. Watchdog trip triage

```
journalctl -u matching-engine@<shard> -n 500 --no-pager
```

| Signature | Cause | Action |
|---|---|---|
| `[P1] ENGINE_WARN stale_ns=…` | T2 warn (>500µs) — usually GC-free so: IRQ leak onto isolated core, NUMA miss, or snapshot flush | check `irq-affinity.sh --check`, snapshot IO, then re-baseline |
| `[P0] ENGINE_STALL …` + SIGABRT | T2 halt (>2ms) | engine auto-flushes dirty WAL + releases `engine:leader:{shard}`; warm follower promotes. Collect coredump (`coredumpctl`), **do not** restart until WAL state verified (`verify-shard.sh`) |
| systemd `Watchdog timeout` | T1: process didn't pet in 1s | same as STALL; check the sd_notify build is deployed (`README.md` caveat) |
| `sentinel … +failover` | Redis failover — see `docs/ops/redis-sentinel-failover.md` | — |
| `TIME_SYNC_LOSS_HALT` | ptp4l drift >100µs | check `pmc`/`ptp4l` logs, grandmaster reachability; engine stays halted until drift <100µs |

## 3. Crash dump collection

Units set `LimitCORE=infinity`; dumps land via `coredumpctl`:

```sh
coredumpctl list /opt/exchange/bin/matching-engine
coredumpctl dump <pid> > /var/lib/exchange/dumps/engine-<ts>.core
journalctl -u matching-engine@<shard> --since '-10 min' > /var/lib/exchange/dumps/engine-<ts>.log
# Attach both to the incident ticket — MiFID II evidence retention applies.
```

## 4. Manual leader demotion / override

When a primary is hung but systemd hasn't tripped (partial stall):

```sh
# 1. revoke the lease — watchdogd does this automatically on IPC stall >3s;
#    manual path:
redis-cli -h <sentinel-resolved-master> DEL engine:leader:<shard> engine:leader:epoch:<shard>
# 2. demote the process (graceful): SIGTERM lets it drain snapshot+WAL
systemctl stop matching-engine@<shard>
# 3. follower promotes automatically on lease acquisition; verify:
scripts/deploy/verify-shard.sh --shard <shard>
```

## 5. Bare-metal rolling restart

- **Same binary restart:** `systemctl restart matching-engine@<shard>` — engine replays snapshot+WAL tail (<10s, §18.3 C++ RTO).
- **Binary swap / drain:** follow `docs/ops/shard-binary-swap.md` (8-step §19.6 procedure, ≥60s inter-shard gap).
- **Host reboot:** drain shard first (mode `Maintenance` via Redis), `systemctl stop matching-engine@`, verify `/dev/watchdog` will not force-cycle mid-shutdown (`ShutdownWatchdogSec=10min` covers it), reboot, then `verify-shard.sh` + mode `Normal`.

## 6. False-positive guards

- **GC/snapshot flush:** T2's 500µs warn is intentional headroom; if WARN
  frequency correlates with snapshot ticks (`-snapshot-interval-s`), raise
  the cadence, never the halt threshold.
- **/dev/watchdog driver failure:** `RuntimeWatchdogSec` only fires on
  *unopened* device. If the BMC driver unbinds, `watchdogd` raises P1 — check
  `journalctl -k | grep watchdog`.
- **Network partition watchdogd↔Redis:** lease revocation requires a
  reachable master; a partitioned watchdogd cannot revoke (fail-closed —
  the lease TTL self-expires in 2s anyway, §18.6.2).
