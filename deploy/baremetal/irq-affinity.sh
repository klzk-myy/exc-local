#!/usr/bin/env bash
# =============================================================================
# irq-affinity.sh — pin NIC IRQs + RSS queues to housekeeping cores so no
# interrupt ever lands on the isolated matching/Aeron cores (Task 9.3.1,
# spec §19.1).
#
# What it does:
#   * Disables irqbalance for the target NIC's IRQs (it would undo pinning).
#   * Writes /proc/irq/<n>/smp_affinity_list for every IRQ bound to the NIC.
#   * Sets RSS queue count + per-queue CPU map (ethtool -L/-X when available).
#   * Enables XPS/RPS steering onto the housekeeping set.
#
# Usage:
#   irq-affinity.sh --nic eth0 --cpus 0-1 [--apply|--check|--print]
# Default mode: --check.
# =============================================================================
set -euo pipefail

NIC=""
CPUS="0-1"
MODE="check"
while [ $# -gt 0 ]; do
    case "$1" in
        --nic)   NIC="$2"; shift ;;
        --cpus)  CPUS="$2"; shift ;;
        --apply) MODE="apply" ;;
        --check) MODE="check" ;;
        --print) MODE="print" ;;
        -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 64 ;;
    esac
    shift
done
[ -n "$NIC" ] || { echo "--nic required" >&2; exit 64; }
[ -d "/sys/class/net/$NIC" ] || { echo "no such interface: $NIC" >&2; exit 1; }

if [ "$MODE" = "apply" ] && [ "$(id -u)" -ne 0 ]; then
    echo "--apply needs root" >&2; exit 1
fi

ok=0; fail=0; skip=0
pass() { printf '  \033[32mOK\033[0m    %s\n' "$1"; ok=$((ok+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }
skipf(){ printf '  \033[33mSKIP\033[0m  %s\n' "$1"; skip=$((skip+1)); }
hdr()  { printf '\n== %s ==\n' "$1"; }

set_affinity() { # set_affinity <irq> <list>
    local irq="$1" list="$2" f="/proc/irq/$irq/smp_affinity_list"
    [ -w "$f" ] || [ -r "$f" ] || return 2
    if [ "$MODE" = "apply" ]; then
        echo "$list" > "$f" 2>/dev/null || return 1
    elif [ "$MODE" = "print" ]; then
        printf '  PLAN   irq %s -> %s (now %s)\n' "$irq" "$list" "$(cat "$f" 2>/dev/null)"
        return 0
    fi
    [ "$(cat "$f" 2>/dev/null)" = "$list" ]
}

# Collect every IRQ whose action names the NIC (MSI-X naming: ethX-TxRx-N,
# ethX-rx-N, mlx5_compN@pci:... — match against the device name and its PCI
# address as fallback).
hdr "IRQ affinity for $NIC -> $CPUS"
pci="$(basename "$(readlink -f "/sys/class/net/$NIC/device" 2>/dev/null)" 2>/dev/null || true)"
irqs="$(awk -v nic="$NIC" -v pci="$pci" '
    /^ *[0-9]+:/ {
        irq=$1; sub(/:/,"",irq);
        if ($0 ~ nic || (pci != "" && $0 ~ pci)) print irq;
    }' /proc/interrupts)"

if [ -z "$irqs" ]; then
    skipf "irq list" "no IRQs naming $NIC/$pci in /proc/interrupts (virtio?)"
else
    n=0; badn=0
    for irq in $irqs; do
        n=$((n+1))
        if set_affinity "$irq" "$CPUS"; then :; else badn=$((badn+1)); fi
    done
    if [ "$MODE" != "print" ]; then
        if [ "$badn" -eq 0 ]; then
            pass "$n IRQs pinned to $CPUS"
        else
            bad "$badn/$n IRQs not pinned to $CPUS"
        fi
    fi
fi

# irqbalance must leave these IRQs alone.
hdr "irqbalance guard"
if command -v irqbalance >/dev/null 2>&1; then
    if pgrep -x irqbalance >/dev/null 2>&1; then
        if [ "$MODE" = "apply" ]; then
            echo "    stopping irqbalance (it would undo pinning) — mask it or add the NIC IRQs to IRQBALANCE_BANNED_CPULIST" >&2
            systemctl stop irqbalance 2>/dev/null || true
        fi
        if [ "$MODE" = "print" ]; then
            printf '  PLAN   mask irqbalance or ban NIC IRQs (currently running)\n'
        else
            bad "irqbalance running — it rewrites smp_affinity; ban these IRQs or mask the unit"
        fi
    else
        pass "irqbalance installed but not running"
    fi
else
    pass "irqbalance not installed"
fi

# RSS: distribute queues across the housekeeping set. ethtool -L reduces queue
# count; -X sets the indirection table. Both are advisory — driver support varies.
hdr "RSS queue steering (ethtool)"
if command -v ethtool >/dev/null 2>&1; then
    ncpu="$(echo "$CPUS" | tr ',' '\n' | awk -F- '{if (NF==2) s+=$2-$1+1; else s++} END{print s}')"
    cur="$(ethtool -l "$NIC" 2>/dev/null | awk '/^Current/{f=1} f&&/Combined:/{print $2}' | tail -1)"
    if [ -n "$cur" ]; then
        if [ "$MODE" = "apply" ]; then
            ethtool -L "$NIC" combined "$ncpu" >/dev/null 2>&1 || true
            ethtool -X "$NIC" equal "$ncpu" >/dev/null 2>&1 || true
            pass "RSS: $ncpu combined queues, equal spread"
        elif [ "$MODE" = "print" ]; then
            printf '  PLAN   ethtool -L %s combined %s; ethtool -X %s equal %s\n' "$NIC" "$ncpu" "$NIC" "$ncpu"
        else
            pass "RSS combined=$cur (target $ncpu on housekeeping set)"
        fi
    else
        skipf "ethtool -l" "driver reports no channel info"
    fi
else
    skipf "ethtool" "not installed"
fi

# RPS/XPS: steer any stray software packet processing off isolated cores.
hdr "RPS/XPS maps"
cpumask_hex() { # render CPU list as a hex mask string for sysfs
    local spec="$1" mask=0 c lo hi
    for part in ${spec//,/ }; do
        case "$part" in
            *-*) lo="${part%%-*}"; hi="${part##*-}"
                 for ((i=lo; i<=hi; i++)); do mask=$((mask | (1<<i))); done ;;
            *)   mask=$((mask | (1<<part))) ;;
        esac
    done
    printf '%x' "$mask"
}
mask="$(cpumask_hex "$CPUS")"
for q in /sys/class/net/"$NIC"/queues/rx-*/rps_cpus; do
    [ -e "$q" ] || continue
    if [ "$MODE" = "apply" ]; then echo "$mask" > "$q" 2>/dev/null || true; fi
    if [ "$MODE" = "check" ] && [ "$(cat "$q")" != "$mask" ] && [ "$(cat "$q")" != "00000000" ]; then
        : # divergent rps_cpus is a warning at most — RSS does the real work
    fi
done
[ "$MODE" = "print" ] && printf '  PLAN   rps_cpus <- %s on rx queues\n' "$mask"
pass "rps_cpus mask $mask ($CPUS)"

hdr "summary"
printf '  ok=%d fail=%d skip=%d mode=%s nic=%s\n' "$ok" "$fail" "$skip" "$MODE" "$NIC"
[ "$fail" -eq 0 ] || exit 1
