#!/usr/bin/env bash
# =============================================================================
# harden-ipc-perms.sh — same-host trust boundary for engine IPC segments.
# Phase-13.5 Task 13.5.3.6 (spec §24 #213), companion:
# deploy/security/secrets-policy.md §3.
#
# Scope:
#   * Aeron media-driver dir     /dev/shm/aeron-exchange/ (aeronmd.service)
#   * Engine shm_open SPSC rings /dev/shm/<ipc-base>_<shard>_{in,out}
#     (SharedMemChannel; systemd unit runs -ipc-base exc → exc_*_*,
#     dev default exchange_ipc_*_*).
#
# Contract: every segment is exchange:exchange 0660 — the engine and the
# Go bridge run in the `exchange` group; nothing else on the host may
# attach the rings. Cross-host Aeron UDP does not rely on file perms at
# all — it rides a WireGuard/IPsec tunnel (secrets-policy §3.3).
#
# Usage:
#   harden-ipc-perms.sh --check                 # audit only (CI/pen-test)
#   harden-ipc-perms.sh --apply                 # enforce chmod/chown
#   harden-ipc-perms.sh --apply --base exc --base exchange_ipc
#
# Env: IPC_GROUP (default exchange), AERON_DIR (default
# /dev/shm/aeron-exchange).
#
# Exit: 0 ok · 1 violations found (--check) or a chmod/chown failed
# =============================================================================
set -euo pipefail

GROUP="${IPC_GROUP:-exchange}"
AERON_DIR="${AERON_DIR:-/dev/shm/aeron-exchange}"
APPLY=0
BASES=()

log()  { printf '[ipc-perms %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die()  { log "ERROR: $*" >&2; exit 1; }
usage(){ sed -n '2,28p' "$0"; exit 64; }

while [ $# -gt 0 ]; do
    case "$1" in
        --check) APPLY=0 ;;
        --apply) APPLY=1 ;;
        --base)  shift; [ $# -gt 0 ] || usage; BASES+=("$1") ;;
        *) usage ;;
    esac
    shift
done
[ ${#BASES[@]} -eq 0 ] && BASES=(exc exchange_ipc)

VIOLATIONS=0

# check_path <path> — verify owner group + exact 0660 mode.
check_path() {
    local p="$1" what="$2"
    local mode grp
    mode="$(stat -c '%a' "$p" 2>/dev/null || echo MISSING)"
    grp="$(stat -c '%G' "$p" 2>/dev/null || echo MISSING)"
    if [ "$mode" != "660" ] || [ "$grp" != "$GROUP" ]; then
        log "VIOLATION $what $p mode=$mode group=$grp (want 660/$GROUP)"
        VIOLATIONS=$((VIOLATIONS + 1))
        return 1
    fi
    return 0
}

fix_path() {
    local p="$1"
    if [ "$APPLY" -eq 1 ]; then
        chgrp "$GROUP" "$p" 2>/dev/null || die "chgrp failed: $p"
        chmod 0660 "$p" || die "chmod failed: $p"
        log "fixed $p"
    fi
}

# --- Aeron media-driver directory -------------------------------------------
if [ -d "$AERON_DIR" ]; then
    mode="$(stat -c '%a' "$AERON_DIR")"
    grp="$(stat -c '%G' "$AERON_DIR")"
    if [ "$mode" != "750" ] || [ "$grp" != "$GROUP" ]; then
        log "VIOLATION dir $AERON_DIR mode=$mode group=$grp (want 750/$GROUP)"
        VIOLATIONS=$((VIOLATIONS + 1))
        if [ "$APPLY" -eq 1 ]; then
            chgrp "$GROUP" "$AERON_DIR"; chmod 0750 "$AERON_DIR"
            log "fixed dir $AERON_DIR"
        fi
    fi
    while IFS= read -r f; do
        check_path "$f" "aeron-segment" || fix_path "$f"
    done < <(find "$AERON_DIR" -maxdepth 2 -type f \( -name 'cnc.dat' \
        -o -name '*.logbuffer' -o -name '*.dat' \) 2>/dev/null)
else
    log "aeron dir $AERON_DIR absent (driver not running on this host?)"
fi

# --- Engine shm_open SPSC rings ----------------------------------------------
for base in "${BASES[@]}"; do
    found=0
    for f in /dev/shm/"${base}"_*_in /dev/shm/"${base}"_*_out; do
        [ -e "$f" ] || continue
        found=1
        check_path "$f" "shm-ring" || fix_path "$f"
    done
    [ "$found" -eq 0 ] && log "no rings for base '$base' under /dev/shm"
done

# World-readable leftovers anywhere under the managed bases → violation.
for base in "${BASES[@]}"; do
    while IFS= read -r f; do
        m="$(stat -c '%a' "$f")"
        if [ "${m: -1}" != "0" ]; then
            log "VIOLATION world/other-readable: $f mode=$m"
            VIOLATIONS=$((VIOLATIONS + 1))
            fix_path "$f"
        fi
    done < <(find /dev/shm -maxdepth 1 -name "${base}_*" -type f 2>/dev/null)
done

if [ "$VIOLATIONS" -gt 0 ]; then
    log "$VIOLATIONS violation(s) $([ "$APPLY" -eq 1 ] && echo 'fixed' || echo 'found — run with --apply')"
    [ "$APPLY" -eq 1 ] || exit 1
else
    log "OK — all IPC segments conform (0660 $GROUP)"
fi
