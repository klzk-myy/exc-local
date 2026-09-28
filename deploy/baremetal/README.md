# Bare-Metal Matching-Engine Host Provisioning (Task 9.3.1, spec §19.1/§19.6)

One bare-metal host per matching shard (spec §19.9: minimum 4 isolated physical
cores, 16 GB ECC DDR5, dual 25GbE Mellanox, NVMe >500k random write IOPS;
production target profile is dual-socket EPYC, see
`docs/ops/capacity-model.md`).

## Files

| File | Purpose |
|---|---|
| `provision-matching-host.sh` | Idempotent host provisioning: hugepages, NUMA layout check, sysctl, IRQ/RSS pinning, isolcpus verification. |
| `irq-affinity.sh` | Pins NIC IRQs and RSS queues away from isolated cores onto housekeeping cores. |
| `sysctl.d/90-exchange-latency.conf` | Persistent `/etc/sysctl.d/` drop-in (runtime equivalents are applied by `scripts/tune-kernel-network.sh`). |
| `system.conf.d/50-exchange-watchdog.conf` | `RuntimeWatchdogSec`/`ShutdownWatchdogSec` for the Tier-1 hardware watchdog (Task 9.3.28, spec §19.13.3). |
| `grub/isolcpus.example.conf` | `GRUB_CMDLINE_LINUX_DEFAULT` fragment — CPU isolation requires a reboot and is never applied silently. |

## Procedure

The full procedure, verification checklist, and rollback notes live in
[`docs/ops/baremetal-provisioning.md`](../../docs/ops/baremetal-provisioning.md).

```sh
# Dry-run / verify only — safe on any host:
sudo ./provision-matching-host.sh --check

# Apply (requires root):
sudo ./provision-matching-host.sh --apply --shard 0
```

`isolcpus`/`nohz_full`/`rcu_nocbs` are kernel-cmdline parameters — the script
verifies them in `--check` mode and prints the required GRUB fragment but never
edits the bootloader itself (reboot-risking changes stay operator-gated).
