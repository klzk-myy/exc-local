# ptp — PTP (IEEE 1588v2) provisioning for bare-metal matching nodes

Phase-09 Task 9.3.12 · MiFID II RTS 25 (clock accuracy ≤100µs of UTC).

## Layout

```
tasks/main.yml                    linuxptp install + unit/timer deploy
templates/ptp4l.conf.j2           hardware timestamping, E2E, domain
templates/ptp4l.service.j2        ptp4l on the PTP NIC
templates/phc2sys.service.j2      CLOCK_REALTIME ← PHC (systemd, rr sched)
templates/ptp-status.service.j2   one-shot status writer
templates/ptp-status.timer.j2     5s cadence
files/ptp-status.sh               renders /run/ptp/status (key=value)
defaults/main.yml                 ptp_interface / ptp_domain
```

## Monitor contract

`ptp-status.sh` writes `/run/ptp/status` every 5s:

```
offset_ns=<signed ns>   state=<SLAVE|…>   synced=<0|1>   read_at=<RFC3339>
```

`services/internal/timesync` (`StatsFileReader` → `PMCReader` fallback)
samples it into `clock_offset_nanoseconds`, `ptp_sync_status`,
`ptp_available`, `ptp_last_read_age_seconds`,
`clock_divergence_daily_max_nanoseconds` (UTC-day max, JSONL daily
report via `EXC_PTP_REPORT_PATH`). Alert rules in
`deploy/prometheus/rules/exchange-alerts.yml` fire P1 on offset>100µs,
stale readings, or `ptp_available==0` on PTP-expected hosts
(`EXC_PTP_EXPECTED=1` in the admin service env on those nodes).

## Hardware note

`ptp_interface` must be a NIC with PHC hardware timestamping (`ethtool -T
<if>` shows `hardware-transmit`/`hardware-receive`). Virtual/K8s hosts
simply don't run this role — the monitor reports `ptp_available 0` and
pages only where `EXC_PTP_EXPECTED=1`.
