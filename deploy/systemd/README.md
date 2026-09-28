# systemd Units — Bare-Metal Daemon Supervision (Task 9.3.28, spec §19.13)

Systemd units for the **bare-metal** tier of the §19.13.1 daemon inventory.
K8s-tier daemons are supervised by kubelet probes (`deploy/k8s/`), not
systemd.

## Layout

| Unit | Daemon | Watchdog tier | Notes |
|---|---|---|---|
| `matching-engine@.service` | `matching-engine -shard %i` (leader) | T1 `WatchdogSec=1s` + T2 in-process `WatchdogThread` | numactl NUMA-0 pin, `Type=notify`, **requires the sd_notify build** — see below |
| `matching-engine-follower@.service` | `-follower` warm standby | same | conflicts with leader unit on same host |
| `aeronmd.service` | Aeron media driver | T1 `WatchdogSec=1s` | `/dev/shm/aeron-exchange` rings |
| `exchange-watchdogd.service` | Tier-3 supervisor | `WatchdogSec=500ms` | canary probes, leader-lock revocation |
| `ptp4l.service` / `phc2sys.service` | PTP clock sync | unit restart + watchdogd offset sampler | MiFID II RTS 25, <100µs |
| `redis-server.service` | coordination Redis | `WatchdogSec=2s` | config `deploy/redis/redis.conf` |
| `redis-sentinel@.service` | sentinel node %i∈{1,2,3} | `WatchdogSec=2s` | config `deploy/sentinel/sentinel-%i.conf` |
| `nats-server.service` | JetStream node | restart + raft health | `NATS_NODE` env picks `deploy/nats/nats-%i.conf` |
| `postgresql@16-main.service.d-override.conf` | PG16 | restart + Patroni leader | drop-in over distro unit |
| `pgbouncer.service.d-override.conf` | pooler | restart | drop-in |
| `clickhouse-server.service.d-override.conf` | ClickHouse | restart + Keeper health | drop-in |
| `exchange-partition-archival.{service,timer}` | nightly archival | timer + oneshot | K8s CronJob is canonical where K8s exists |
| `matching-engine.target` | Stage-2 boot anchor | — | dependency ordering per §19.13.2 |
| `journald.conf.d/99-exchange.conf` | journald policy | — | persistent, sealed, no rate-limit drops |

## Install

```sh
sudo install -D -m 0644 *.service *.target -t /etc/systemd/system/
sudo install -D -m 0644 *.d-override.conf --target-directory=… # see each file header for the target .d dir
sudo install -D -m 0644 journald.conf.d/99-exchange.conf /etc/systemd/journald.conf.d/
sudo install -D -m 0644 ../baremetal/system.conf.d/50-exchange-watchdog.conf /etc/systemd/system.conf.d/
sudo systemctl daemon-reload && sudo systemctl restart systemd-journald
```

## sd_notify caveat

`WatchdogSec=1s`/`500ms` units require binaries emitting
`sd_notify("WATCHDOG=1")` (spec §19.13.3 Tier-1/3). Current dev builds of
`matching_engine` log watchdog trips to stderr but do not pet systemd —
deploying `matching-engine@.service` against that binary will SIGABRT it
every second. On dev hosts, drop in `WatchdogSec=0` until the sd_notify
build lands (tracked as a Phase-09 open item; do not weaken production
units).
