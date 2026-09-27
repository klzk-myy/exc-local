#!/usr/bin/env bash
# =============================================================================
# tune-kernel-network.sh — host kernel tuning for the low-latency Aeron media
# driver (Task 1.3.10, spec §2.3 / §24 #185).
#
# Applies the socket-buffer ceilings the driver needs (aeronmd requests
# SO_RCVBUF/SO_SNDBUF=16MB — Linux clamps those at net.core.{rmem,wmem}_max),
# enables kernel busy-polling for latency-critical sockets, and switches the
# CPU governor to `performance` (frequency scaling off).
#
# Scope discipline:
#   * All sysctl writes are RUNTIME-ONLY (`sysctl -w`) and governor changes go
#     through sysfs — nothing here edits persistent config (/etc/sysctl.d,
#     grub, udev). Reboot reverts everything.
#   * isolcpus is NEVER applied by this script — it requires a kernel-cmdline
#     reboot. The manual guidance block at the end prints what to add.
#
# Usage:
#   sudo ./scripts/tune-kernel-network.sh            # apply + verify
#   ./scripts/tune-kernel-network.sh --check         # verify only (no writes)
#   ./scripts/tune-kernel-network.sh --print         # show plan, do nothing
#
# Env toggles:
#   EXCH_SKIP_GOVERNOR=1   never touch scaling_governor
#   EXCH_SYSCTL_ONLY=1     apply sysctls, skip governor section entirely
#
# Exit: 0 = everything applied (or correctly reported as N/A); 1 = a sysctl we
# were asked to write could not be applied/verified.
# =============================================================================
set -euo pipefail

MODE="apply"
for arg in "$@"; do
    case "$arg" in
        --check) MODE="check" ;;
        --print) MODE="print" ;;
        -h|--help)
            sed -n '2,38p' "$0"; exit 0 ;;
        *) echo "unknown flag: $arg" >&2; exit 64 ;;
    esac
done

# --- privilege shim ------------------------------------------------------------
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
    if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
        SUDO="sudo -n"
    else
        echo "need root or passwordless sudo (mode=$MODE still works read-only)" >&2
        [ "$MODE" = "apply" ] && exit 1
    fi
fi

ok=0; fail=0; skip=0
pass() { printf '  \033[32mOK\033[0m    %-34s = %s\n' "$1" "$2"; ok=$((ok+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %-34s (want %s, got %s)\n' "$1" "$2" "$3"; fail=$((fail+1)); }
skipf(){ printf '  \033[33mSKIP\033[0m  %-34s (%s)\n' "$1" "$2"; skip=$((skip+1)); }
hdr()  { printf '\n== %s ==\n' "$1"; }

proc_path() { echo "/proc/sys/$(echo "$1" | tr '.' '/')"; }

# apply_sysctl <key> <value>: write via sysctl -w (runtime only), then verify
# by reading /proc/sys back. In --check mode only the readback runs.
apply_sysctl() {
    local key="$1" want="$2" path got
    path="$(proc_path "$key")"
    if [ ! -e "$path" ]; then
        skipf "$key" "kernel has no $path"
        return 0
    fi
    if [ "$MODE" = "print" ]; then
        printf '  PLAN   %-34s -> %s (now: %s)\n' "$key" "$want" "$(cat "$path" 2>/dev/null || echo '?')"
        return 0
    fi
    if [ "$MODE" = "apply" ]; then
        if ! $SUDO sysctl -w "$key=$want" >/dev/null 2>&1; then
            bad "$key" "$want" "sysctl -w failed"
            return 0
        fi
    fi
    got="$(cat "$path" 2>/dev/null || true)"
    if [ "$got" = "$want" ]; then
        pass "$key" "$got"
    else
        bad "$key" "$want" "$got"
    fi
}

hdr "sysctl: socket buffer ceilings (aeronmd SO_RCVBUF/SO_SNDBUF=16MB need >=16MB max)"
apply_sysctl net.core.rmem_max 16777216
apply_sysctl net.core.wmem_max 16777216
# Keep per-socket defaults modest — the driver sets its buffers explicitly.
apply_sysctl net.core.rmem_default 262144
apply_sysctl net.core.wmem_default 262144

hdr "sysctl: busy-poll (SO_BUSY_POLL support; aeron UDP sockets opt in)"
apply_sysctl net.core.busy_read 50
apply_sysctl net.core.busy_poll 50

hdr "sysctl: packet-processing budget (avoid softirq drops under burst)"
apply_sysctl net.core.netdev_budget 600
apply_sysctl net.core.netdev_max_backlog 4096
apply_sysctl net.core.netdev_budget_usecs 5000

hdr "cpu: performance governor (disable frequency scaling)"
if [ "${EXCH_SYSCTL_ONLY:-0}" = "1" ] || [ "${EXCH_SKIP_GOVERNOR:-0}" = "1" ]; then
    skipf "scaling_governor" "disabled via env toggle"
elif [ ! -d /sys/devices/system/cpu/cpu0/cpufreq ]; then
    # VM / container / CPU without cpufreq — graceful degrade, not an error.
    skipf "scaling_governor" "no cpufreq sysfs — VM or fixed-frequency CPU"
else
    avail="$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_available_governors 2>/dev/null || true)"
    case " $avail " in
        *" performance "*) ;;
        *)
            skipf "scaling_governor" "governor 'performance' not in scaling_available_governors ($avail)" ;;
    esac
    if [ "$MODE" = "print" ]; then
        printf '  PLAN   scaling_governor -> performance on %s cpus\n' \
            "$(ls -d /sys/devices/system/cpu/cpu[0-9]*/cpufreq 2>/dev/null | wc -l)"
    elif [ "$MODE" = "apply" ] && printf '%s' "$avail" | grep -qw performance; then
        applied=0; total=0
        for gov in /sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_governor; do
            total=$((total+1))
            if echo performance | $SUDO tee "$gov" >/dev/null 2>&1; then
                applied=$((applied+1))
            fi
        done
        # Verify readback on every cpufreq node.
        mism=0
        for gov in /sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_governor; do
            [ "$(cat "$gov" 2>/dev/null)" = "performance" ] || mism=$((mism+1))
        done
        if [ "$mism" -eq 0 ] && [ "$applied" -eq "$total" ]; then
            pass "scaling_governor" "performance on $total cpus"
        else
            bad "scaling_governor" "performance on $total cpus" \
                "$applied applied, $mism mismatched"
        fi
    else
        # --check mode: just read back current state.
        mism=0; total=0
        for gov in /sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_governor; do
            total=$((total+1))
            [ "$(cat "$gov" 2>/dev/null)" = "performance" ] || mism=$((mism+1))
        done
        if [ "$total" -gt 0 ] && [ "$mism" -eq 0 ]; then
            pass "scaling_governor" "performance on $total cpus"
        elif [ "$total" -gt 0 ]; then
            bad "scaling_governor" "performance" \
                "$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null)"
        fi
    fi
fi

hdr "isolcpus — manual guidance (NEVER applied by this script; needs reboot)"
cat <<'EOF'
  The low-latency profile expects dedicated cores for aeronmd's DEDICATED
  agents (conductor/sender/receiver) and the matching engine. To carve them
  out at boot, add to GRUB_CMDLINE_LINUX_DEFAULT (example: cores 2-5 on a
  2-NUMA-node host, keep 0-1 for OS):

      isolcpus=2,3,4,5 nohz_full=2,3,4,5 rcu_nocbs=2,3,4,5
      irqaffinity=0-1

  then `sudo update-grub` and reboot. Afterwards set:
      aeron.conductor.cpu.affinity=2
      aeron.sender.cpu.affinity=3
      aeron.receiver.cpu.affinity=4
  in config/aeron-low-latency.properties (or AeronDriverConfig fields) and
  pin the matching-engine thread to core 5. Leaving affinities at -1 on an
  unisolated host is SAFER than pinning to shared cores (unpinned jitter
  beats contended pinning). Also consider:
      echo 0 | sudo tee /proc/sys/kernel/timer_migration   # with nohz_full
      echo -1 | sudo tee /proc/sys/kernel/sched_rt_runtime_us  # RT throttle off
EOF

hdr "summary"
printf '  applied/verified: %d   failed: %d   skipped: %d   mode: %s\n' \
    "$ok" "$fail" "$skip" "$MODE"

if [ "$fail" -gt 0 ]; then
    echo "tune-kernel-network: FAIL ($fail tunables not in the desired state)" >&2
    exit 1
fi
echo "tune-kernel-network: PASS"
