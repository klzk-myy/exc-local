# Bare-Metal Matching-Engine Host Provisioning

**Task:** Phase-09 9.3.1 · **Spec:** §19.1 topology, §19.6 deploy prerequisites, §19.9 sizing · **Applies to:** every host running `matching-engine@N`, `aeronmd`, `exchange-watchdogd`, `ptp4l`/`phc2sys`.

---

## 1. Hardware contract (spec §19.9)

| Resource | Minimum (soak-gate) | Production target |
|---|---|---|
| CPU | 4 physical cores, `isolcpus`-pinned | Dual AMD EPYC 9654, 128 cores — one NUMA node per shard carve-out |
| RAM | 16 GB ECC DDR5 | 512 GB ECC DDR5 |
| NIC | dual 25GbE Mellanox ConnectX | dual 100GbE ConnectX-6 Dx, PTP hardware timestamping |
| WAL storage | NVMe, >500k random-write IOPS | NVMe PCIe 5.0, dedicated mount at `/var/lib/exchange/wal` |

One host = one shard (vertical scaling only, spec §19.1). The inventory of
shards ↔ instruments is `config/sharding.yaml` (static shards 0–3, elastic
4–7).

## 2. Provisioning sequence

```sh
# 0. Stage binaries + units first (shard deploy procedure is separate —
#    docs/ops/shard-binary-swap.md):
sudo install -m 0755 core/build/bin/matching_engine /opt/exchange/bin/matching-engine
sudo install -m 0755 core/build/bin/aeronmd          /opt/exchange/bin/aeronmd   # if locally built
sudo install -D -m 0644 deploy/systemd/*.service     /etc/systemd/system/
sudo install -D -m 0644 deploy/systemd/journald.conf.d/*.conf /etc/systemd/journald.conf.d/

# 1. CPU isolation (REBOOT-REQUIRED — never scripted):
#    merge deploy/baremetal/grub/isolcpus.example.conf into /etc/default/grub
sudo update-grub && sudo reboot

# 2. After reboot — apply + verify everything else:
sudo deploy/baremetal/provision-matching-host.sh --apply --shard 0 --nic enp65s0f0
sudo deploy/baremetal/provision-matching-host.sh --check --shard 0 --nic enp65s0f0

# 3. Hardware watchdog (Tier-1, spec §19.13.3):
sudo install -D -m 0644 deploy/baremetal/system.conf.d/50-exchange-watchdog.conf \
    /etc/systemd/system.conf.d/50-exchange-watchdog.conf
sudo systemctl daemon-reload
modprobe ipmi_watchdog   # or the BMC-specific driver; verify /dev/watchdog exists

# 4. Enable services — order follows the §19.13.2 stage graph:
sudo systemctl enable --now ptp4l phc2sys aeronmd exchange-watchdogd
sudo systemctl enable matching-engine@0   # enabled, NOT started until deploy gate
```

## 3. What each step does

| Step | Mechanism | Failure mode if skipped |
|---|---|---|
| CPU isolation | `isolcpus`/`nohz_full`/`rcu_nocbs`/`irqaffinity`/`kthread_cpus` kernel cmdline | Scheduler tick + IRQ jitter on the matching thread → p99 latency spikes above the ≤50µs gate |
| Hugepages | `vm.nr_hugepages=2048` (4 GiB of 2 MB pages) via `sysctl.d` + runtime write | WAL mmap page-fault jitter; snapshot flush stalls |
| Socket tuning | `net.core.{rmem,wmem}_max=16MB`, busy-poll, netdev budget — persistent copy in `sysctl.d/90-exchange-latency.conf`, runtime via `scripts/tune-kernel-network.sh` | aeronmd's 16 MB SO_*BUF requests clamped → ring-buffer drops under burst |
| NUMA | verify isolated cores + NIC + NVMe all report `numa_node=0`; run engine under `numactl --cpunodebind=0 --membind=0` | cross-socket memory/PCIe traffic doubles hot-path latency |
| IRQ/RSS | `irq-affinity.sh` pins every NIC IRQ to housekeeping cores, `ethtool -L/-X` spreads RSS queues, irqbalance must be masked or ban-listed | interrupt storm on an isolated core = undiagnosable latency outliers |
| NVMe mount | dedicated mount, `noatime`, XFS/ext4 | WAL fsync latency contaminated by other I/O |
| Clock | `ptp4l`+`phc2sys` units (MiFID II RTS 25, drift <100µs) | `TIME_SYNC_LOSS_HALT` trips the matching core |

## 4. Verification checklist (gate for `matching-engine@N` start)

```sh
cat /sys/devices/system/cpu/isolated           # = isolcpus set
numactl --hardware                              # isolated cores on node 0
cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages   # >= 2048
cat /proc/cmdline | tr ' ' '\n' | grep -E 'isolcpus|nohz_full|rcu_nocbs|irqaffinity'
scripts/tune-kernel-network.sh --check          # socket buffers + governor
deploy/baremetal/irq-affinity.sh --nic <nic> --cpus 0-1 --check
ls -l /dev/watchdog                             # BMC watchdog bound
findmnt /var/lib/exchange/wal                   # dedicated NVMe, noatime
systemctl status ptp4l phc2sys                  # clock locked <100µs
```

## 5. Rollback / decommission

Provisioning is additive and idempotent — runtime sysctls revert on reboot;
to decommission a host: `systemctl disable --now matching-engine@N
aeronmd exchange-watchdogd`, remove the sysctl.d/journald.d drop-ins, and
revert the GRUB fragment (reboot). No state leaves the host except the WAL
archive (`exchange:replay-from-archive` S3 path, spec §3.5).

## Trigger

Run this procedure when a new matching-engine host enters inventory,
when a host is re-imaged, or when a kernel/firmware baseline change
requires re-verifying the §19.9 hardware contract. It is not a runtime
recovery procedure — a degraded-but-provisioned host follows
`docs/ops/daemon-supervision.md` instead.

## Escalation

A host that cannot pass the verification checklist stays out of the
fleet (the supervisor keeps it unschedulable — cordon persists).
Hardware-contract failures escalate to the infrastructure owner (P2 —
capacity reserve) per `docs/runbooks/incident-escalation.md`; a host
discovered non-conforming *after* serving traffic escalates to P1 and
is cordoned immediately (`docs/ops/daemon-supervision.md` §4).
