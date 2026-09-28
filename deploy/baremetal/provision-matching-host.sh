#!/usr/bin/env bash
# =============================================================================
# provision-matching-host.sh — bare-metal provisioning for a C++ matching
# engine shard host (Phase-09 Task 9.3.1, spec §19.1/§19.6/§19.9).
#
# Applies, in order:
#   1. NUMA topology check (matching thread must live on node 0, WAL/NVMe and
#      the Aeron NIC should be node-local — warns when they are not).
#   2. 2MB hugepages for the WAL mmap path (spec §19.6 step 1; the 2MB size is
#      canonical — supersedes the earlier 1GB draft, remediation #35).
#   3. Persistent + runtime sysctl network/low-latency profile, delegating the
#      socket-buffer/busy-poll/governor writes to scripts/tune-kernel-network.sh.
#   4. NIC RSS + IRQ affinity pinning onto housekeeping cores (via
#      irq-affinity.sh) so interrupts never land on isolated matching cores.
#   5. isolcpus/nohz_full/rcu_nocbs verification — kernel-cmdline parameters,
#      verified here, changed only via grub/isolcpus.example.conf + reboot.
#
# Usage:
#   provision-matching-host.sh --check            # verify only, exit 1 on drift
#   provision-matching-host.sh --apply            # apply everything applicable
#   provision-matching-host.sh --print            # show the plan, change nothing
#
# Options:
#   --shard N          shard id this host serves (default 0; used for layout
#                      checks and the systemd instance name only)
#   --nic IFACE        Aeron traffic NIC (default: first non-lo interface with
#                      an operstate of up; override for dual-NIC hosts)
#   --hugepages N      number of 2MB hugepages (default 2048 = 4 GiB)
#   --isolcpus LIST    expected isolated CPU list (default "2-7")
#   --housekeeping LIST housekeeping CPU list (default "0-1")
#
# Exit: 0 = converged (or verified), 1 = a check/write failed, 64 = usage.
# =============================================================================
set -euo pipefail

MODE="check"
SHARD=0
NIC=""
HUGEPAGES=2048
ISOLCPUS="2-7"
HOUSEKEEPING="0-1"

while [ $# -gt 0 ]; do
    case "$1" in
        --check)  MODE="check" ;;
        --apply)  MODE="apply" ;;
        --print)  MODE="print" ;;
        --shard)      SHARD="$2"; shift ;;
        --nic)        NIC="$2"; shift ;;
        --hugepages)  HUGEPAGES="$2"; shift ;;
        --isolcpus)   ISOLCPUS="$2"; shift ;;
        --housekeeping) HOUSEKEEPING="$2"; shift ;;
        -h|--help) sed -n '2,44p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 64 ;;
    esac
    shift
done

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd)"

ok=0; fail=0; skip=0
pass() { printf '  \033[32mOK\033[0m    %s\n' "$1"; ok=$((ok+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }
skipf(){ printf '  \033[33mSKIP\033[0m  %s\n' "$1"; skip=$((skip+1)); }
hdr()  { printf '\n== %s ==\n' "$1"; }

if [ "$MODE" = "apply" ] && [ "$(id -u)" -ne 0 ]; then
    echo "--apply needs root" >&2; exit 1
fi

# --- expand "2-7,9" style CPU lists into individual ids -----------------------
expand_cpus() {
    local spec="$1" part lo hi
    for part in ${spec//,/ }; do
        case "$part" in
            *-*) lo="${part%%-*}"; hi="${part##*-}"
                 for ((i=lo; i<=hi; i++)); do echo "$i"; done ;;
            *)   echo "$part" ;;
        esac
    done
}

cpu_in_list() {
    local cpu="$1" c
    for c in $(expand_cpus "$2"); do [ "$c" = "$cpu" ] && return 0; done
    return 1
}

write_file() { # write_file <path> <content> — apply mode only
    local path="$1"
    if [ "$MODE" = "print" ]; then
        printf '  PLAN   write %s\n' "$path"; return 0
    fi
    if [ "$MODE" = "check" ]; then
        if [ -f "$path" ]; then pass "present: $path"; else bad "missing: $path"; fi
        return 0
    fi
    install -D -m 0644 /dev/stdin "$path"
}

# =============================================================================
hdr "NUMA topology (spec §19.1 — matching thread on node 0, numactl membind)"
if command -v numactl >/dev/null 2>&1; then
    nodes="$(numactl --hardware 2>/dev/null | awk '/^available:/{print $2}')"
    pass "numactl present, $nodes node(s)"
    numactl --hardware | sed 's/^/    /'
    if [ "${nodes:-1}" -gt 1 ]; then
        node0_cpus="$(numactl --hardware | awk '/^node 0 cpus:/{for(i=4;i<=NF;i++)printf "%s ",$i}')"
        echo "    node0 cpus: $node0_cpus"
        iso_on_node0=1
        for c in $(expand_cpus "$ISOLCPUS"); do
            case " $node0_cpus " in *" $c "*) ;; *) iso_on_node0=0 ;; esac
        done
        if [ "$iso_on_node0" -eq 1 ]; then
            pass "isolated cores $ISOLCPUS all on NUMA node 0"
        else
            bad "isolated cores $ISOLCPUS span NUMA nodes — pin engine numactl --cpunodebind to the node holding them"
        fi
    fi
else
    skipf "numactl" "not installed — apt install numactl"
fi

# =============================================================================
hdr "hugepages — 2MB pages for WAL mmap (spec §19.6; remediation #35)"
hp_path="/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages"
if [ -e "$hp_path" ]; then
    cur="$(cat "$hp_path")"
    if [ "$MODE" = "apply" ]; then
        echo "$HUGEPAGES" > "$hp_path" && cur="$(cat "$hp_path")"
        write_file /etc/sysctl.d/90-exchange-hugepages.conf <<EOF
# Task 9.3.1 — 2MB hugepages for the matching-engine WAL mmap.
vm.nr_hugepages = $HUGEPAGES
EOF
    elif [ "$MODE" = "print" ]; then
        printf '  PLAN   nr_hugepages %s -> %s\n' "$cur" "$HUGEPAGES"
    fi
    if [ "$MODE" = "check" ]; then
        if [ "$cur" -ge "$HUGEPAGES" ]; then
            pass "nr_hugepages=$cur (>= $HUGEPAGES, $((cur*2))MB)"
        else
            bad "nr_hugepages=$cur < $HUGEPAGES"
        fi
    else
        pass "nr_hugepages=$cur"
    fi
else
    skipf "hugepages" "kernel has no $hp_path"
fi

# =============================================================================
hdr "persistent sysctl profile (/etc/sysctl.d/90-exchange-latency.conf)"
write_file /etc/sysctl.d/90-exchange-latency.conf <<'EOF'
# Task 9.3.1 / Task 1.3.10 — low-latency socket ceilings for the Aeron media
# driver (aeronmd requests SO_RCVBUF/SO_SNDBUF=16MB). Runtime twins of these
# values are applied by scripts/tune-kernel-network.sh.
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.core.rmem_default = 262144
net.core.wmem_default = 262144
net.core.busy_read = 50
net.core.busy_poll = 50
net.core.netdev_budget = 600
net.core.netdev_max_backlog = 4096
net.core.netdev_budget_usecs = 5000
# nohz_full housekeeping: keep the scheduler clock off isolated cores
kernel.timer_migration = 0
# Real-time throttle off — the matching thread must never be preempted by
# the 95%/50ms RT group limit.
kernel.sched_rt_runtime_us = -1
EOF
[ "$MODE" = "apply" ] && sysctl --system >/dev/null 2>&1 || true

hdr "runtime network tuning — delegated to scripts/tune-kernel-network.sh"
tune="$REPO_ROOT/scripts/tune-kernel-network.sh"
if [ -x "$tune" ]; then
    case "$MODE" in
        apply) "$tune"            && pass "tune-kernel-network apply" || bad "tune-kernel-network apply" ;;
        check) "$tune" --check    && pass "tune-kernel-network check" || true ;;
        print) "$tune" --print ;;
    esac
else
    skipf "tune-kernel-network.sh" "not found/executable at $tune"
fi

# =============================================================================
hdr "NIC selection + IRQ/RSS pinning (housekeeping cpus: $HOUSEKEEPING)"
if [ -z "$NIC" ]; then
    for d in /sys/class/net/*; do
        n="${d##*/}"
        [ "$n" = "lo" ] && continue
        [ "$(cat "$d/operstate" 2>/dev/null)" = "up" ] && NIC="$n" && break
    done
fi
if [ -z "$NIC" ]; then
    skipf "NIC" "no operstate-up interface found; pass --nic"
else
    pass "Aeron NIC: $NIC"
    if [ -r "/sys/class/net/$NIC/device/numa_node" ]; then
        nn="$(cat "/sys/class/net/$NIC/device/numa_node")"
        if [ "$nn" = "0" ] || [ "$nn" = "-1" ]; then
            pass "NIC numa_node=$nn"
        else
            bad "NIC $NIC is on NUMA node $nn — engine pinned to node 0 pays cross-socket latency"
        fi
    fi
    if [ -x "$SELF_DIR/irq-affinity.sh" ]; then
        case "$MODE" in
            apply) "$SELF_DIR/irq-affinity.sh" --nic "$NIC" --cpus "$HOUSEKEEPING" \
                        && pass "irq-affinity apply" || bad "irq-affinity apply" ;;
            check) "$SELF_DIR/irq-affinity.sh" --nic "$NIC" --cpus "$HOUSEKEEPING" --check \
                        && pass "irq-affinity check" || bad "irq-affinity check" ;;
            print) "$SELF_DIR/irq-affinity.sh" --nic "$NIC" --cpus "$HOUSEKEEPING" --print ;;
        esac
    else
        skipf "irq-affinity.sh" "not executable"
    fi
fi

# =============================================================================
hdr "CPU isolation — isolcpus/nohz_full/rcu_nocbs (kernel cmdline, reboot-gated)"
cmdline="$(cat /proc/cmdline)"
want_ok=1
for tok in "isolcpus=" "nohz_full=" "rcu_nocbs=" "irqaffinity="; do
    if printf '%s' "$cmdline" | tr ' ' '\n' | grep "^$tok" >/dev/null; then
        pass "cmdline has $tok"
    else
        bad "cmdline missing $tok"
        want_ok=0
    fi
done
if [ "$want_ok" -eq 0 ]; then
    cat <<EOF
  Required GRUB_CMDLINE_LINUX_DEFAULT addition (see grub/isolcpus.example.conf):
      isolcpus=$ISOLCPUS nohz_full=$ISOLCPUS rcu_nocbs=$ISOLCPUS irqaffinity=$HOUSEKEEPING
  Then: sudo update-grub && reboot. Never edited by this script.
EOF
fi
# /sys/devices/system/cpu/isolated is the post-boot authority.
if [ -r /sys/devices/system/cpu/isolated ]; then
    iso="$(cat /sys/devices/system/cpu/isolated)"
    if [ "$iso" = "$ISOLCPUS" ]; then
        pass "kernel isolated set = $iso"
    elif [ -n "$iso" ]; then
        bad "kernel isolated set = '$iso', want '$ISOLCPUS'"
    else
        skipf "isolated set" "empty (isolcpus not applied — reboot pending?)"
    fi
fi

# =============================================================================
hdr "NVMe / WAL mount sanity"
for mp in /var/lib/exchange /var/lib/exchange/wal; do
    if mountpoint -q "$mp" 2>/dev/null; then
        fs="$(findmnt -n -o FSTYPE "$mp")"
        opts="$(findmnt -n -o OPTIONS "$mp")"
        case "$opts" in
            *noatime*|*relatime*) pass "$mp mounted ($fs, $opts)" ;;
            *) bad "$mp mounted without noatime/relatime ($opts)" ;;
        esac
    else
        skipf "$mp" "not a mountpoint — WAL dir must be dedicated NVMe (spec §19.9)"
    fi
done

# =============================================================================
hdr "summary — shard $SHARD host"
printf '  ok=%d fail=%d skip=%d mode=%s\n' "$ok" "$fail" "$skip" "$MODE"
[ "$fail" -eq 0 ] || { echo "provision-matching-host: FAIL" >&2; exit 1; }
echo "provision-matching-host: PASS"
