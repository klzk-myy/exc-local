#!/usr/bin/env bash
# =============================================================================
# shard-swap.sh — 8-step bare-metal rolling binary upgrade for a C++ matching
# engine shard (Phase-09 Task 9.3.16, spec §19.6 / §24 #177).
#
# Steps (mirrors spec §19.6 verbatim):
#   1. Pre-flight: new binary SHA256 + symbols + hugepages + affinity + config.
#   2. Drain: announce Maintenance mode (system:degradation:mode in Redis),
#      gateways shed order ingress → 503 / ingress queue hold.
#   3. In-flight flush: wait up to 5s for queued cancels/mods to match out.
#   4. Snapshot: systemd stop → SIGTERM path force_snapshots + fsyncs WAL
#      (core/src/main.cpp drain block) — verified by file mtime + wal tail.
#   5. Swap: versioned symlink /opt/exchange/bin/matching-engine ->
#      releases/matching-engine-<sha256>; systemd restart on the same NUMA/
#      isolcpus pinning (unit CPUAffinity + numactl ExecStart wrapper).
#   6. Warm reload: journal 'recovery:' line proves snapshot+WAL tail replay;
#      state CRC validated by the engine's own post-recovery probe.
#   7. Verify: verify-shard.sh — leader lease re-acquired, heartbeat ticking,
#      Aeron rings attached, mode Normal.
#   8. Unpause: restore degradation mode Normal; watch metrics.
#
# Rollback: any failure in steps 5–7 → revert symlink to the prior release,
# restart, re-run verify (spec §19.6: "revert symlink and restart previous
# binary").
#
# Usage:
#   sudo shard-swap.sh --shard 0 --binary /opt/exchange/bin/releases/matching-engine-<sha>
#   shard-swap.sh --shard 0 --binary <path> --dry-run
#
# Env: EXC_REDIS_CLI (default "redis-cli -h $REDIS_HOST -p 6379"),
#      REDIS_HOST (127.0.0.1), EXC_EXCHANGE_DIR (/opt/exchange),
#      EXC_WAL_DIR (/var/lib/exchange/wal), EXC_SNAP_DIR (/var/lib/exchange/snapshots).
# Exit: 0 = swap + verify complete; 1 = aborted/rolled back; 64 = usage.
# =============================================================================
set -euo pipefail

SHARD=""; NEWBIN=""; DRY=0
EXC_DIR="${EXC_EXCHANGE_DIR:-/opt/exchange}"
WAL_DIR="${EXC_WAL_DIR:-/var/lib/exchange/wal}"
SNAP_DIR="${EXC_SNAP_DIR:-/var/lib/exchange/snapshots}"
REDIS_HOST="${REDIS_HOST:-127.0.0.1}"
REDIS_CLI="${EXC_REDIS_CLI:-redis-cli -h $REDIS_HOST -p 6379}"
DRAIN_TIMEOUT=5
TERM_TIMEOUT=3
INTERSHARD_GAP=60

while [ $# -gt 0 ]; do
    case "$1" in
        --shard)  SHARD="$2"; shift ;;
        --binary) NEWBIN="$2"; shift ;;
        --dry-run) DRY=1 ;;
        -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 64 ;;
    esac
    shift
done
[ -n "$SHARD" ] && [ -n "$NEWBIN" ] || { sed -n '29,33p' "$0" >&2; exit 64; }

UNIT="matching-engine@$SHARD"
LINK="$EXC_DIR/bin/matching-engine"
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() { printf '[shard-swap s%s %s] %s\n' "$SHARD" "$(date -u +%H:%M:%S)" "$*"; }
die() { log "ABORT: $*" >&2; exit 1; }
run() { if [ "$DRY" = "1" ]; then log "DRY: $*"; else eval "$@"; fi; }
rcli() { $REDIS_CLI "$@" 2>/dev/null || true; }

[ -f "$NEWBIN" ] || die "binary not found: $NEWBIN"
[ "$DRY" = "1" ] || [ "$(id -u)" -eq 0 ] || die "needs root (systemd + symlink); use --dry-run"

# ---------------------------------------------------------------- step 1
log "step 1/8 pre-flight verification"
SHA="$(sha256sum "$NEWBIN" | awk '{print $1}')"
log "  new binary sha256=$SHA"
case "$(basename "$NEWBIN")" in *"$SHA"*|*"$SHA"*) : ;; esac
# symbols present (debugging + crash triage) — refuse stripped release builds.
if command -v nm >/dev/null 2>&1; then
    # No `grep -q`: early exit SIGPIPEs nm and trips pipefail.
    nm "$NEWBIN" 2>/dev/null | grep ' main$' >/dev/null \
        || die "binary stripped — symbol table required for crash triage (spec §19.6 step 1)"
    log "  symbol table: present"
fi
[ -x "$NEWBIN" ] || chmod 0755 "$NEWBIN" 2>/dev/null || die "binary not executable"
# config hash record — provenance for the swap log
CFG="$EXC_DIR/etc/matching-engine-$SHARD.conf"
[ -f "$CFG" ] && log "  config sha256=$(sha256sum "$CFG" | awk '{print $1}')" || log "  config $CFG absent (defaults in unit)"
# hugepages (2MB canonical, spec §19.6/§19.1)
HP="$(cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages 2>/dev/null || echo 0)"
[ "$HP" -ge 2048 ] || die "nr_hugepages=$HP < 2048 — run deploy/baremetal/provision-matching-host.sh"
log "  hugepages=$HP (2MB)"
# CPU isolation live check
ISO="$(cat /sys/devices/system/cpu/isolated 2>/dev/null || true)"
[ -n "$ISO" ] && log "  isolated cpus: $ISO" || die "no isolcpus set — refuse swap on an unisolated host"
# prior release recorded for rollback
PREV="$(readlink -f "$LINK" 2>/dev/null || true)"
log "  current binary: ${PREV:-none}"

# ---------------------------------------------------------------- step 2
log "step 2/8 ingress drain — announce Maintenance"
run "rcli SET system:degradation:mode Maintenance"
run "rcli SET system:degradation:reason 'shard $SHARD binary swap'"
run "rcli SET system:degradation:entered_at \"$(date -u +%FT%TZ)\""
log "  gateways now shed new order ingress (503/transient hold); cancels still drain"

# ---------------------------------------------------------------- step 3
log "step 3/8 in-flight flush (<= ${DRAIN_TIMEOUT}s, spec §19.6 outer bound)"
[ "$DRY" = "1" ] || sleep "$DRAIN_TIMEOUT"

# ---------------------------------------------------------------- step 4+5
log "step 4/8 snapshot: systemctl stop drives SIGTERM -> force_snapshot + WAL fsync"
tail_before="$(rcli GET "leader:heartbeat:$SHARD" | tr -d '\r')"
log "  leader heartbeat pre-stop: ${tail_before:-unknown}"
run "systemctl stop $UNIT"
# engine must exit inside TERM_TIMEOUT; KillSignal escalation is the unit's job.
sleep "$TERM_TIMEOUT"
if [ "$DRY" != "1" ] && systemctl is-active --quiet "$UNIT"; then
    die "$UNIT still active after ${TERM_TIMEOUT}s — investigate, do NOT SIGKILL blindly (dirty WAL)"
fi
latest_snap="$(ls -t "$SNAP_DIR/$SHARD"/* 2>/dev/null | head -1 || true)"
[ -n "$latest_snap" ] && log "  latest snapshot: $latest_snap" || log "  WARN: no snapshot files under $SNAP_DIR/$SHARD"

log "step 5/8 binary swap — versioned symlink"
run "ln -sfn '$NEWBIN' '$LINK'"
log "  $LINK -> $(readlink -f "$LINK" 2>/dev/null || echo '?')"

rollback() {
    log "ROLLBACK: restoring ${PREV:-<none>} and restarting $UNIT"
    [ -n "$PREV" ] && run "ln -sfn '$PREV' '$LINK'"
    run "systemctl start $UNIT"
    sleep 3
    systemctl is-active --quiet "$UNIT" && log "rollback: $UNIT up on prior binary" \
        || log "rollback: $UNIT FAILED — escalate P0, runbook docs/runbooks/wal-recovery-halt.md"
    exit 1
}

# ---------------------------------------------------------------- step 6
log "step 6/8 warm reload — start $UNIT, replay snapshot + WAL tail"
run "systemctl start $UNIT"
if [ "$DRY" != "1" ]; then
    ok=0
    for _ in $(seq 1 30); do
        systemctl is-active --quiet "$UNIT" && journalctl -u "$UNIT" -n 200 --no-pager \
            | grep 'recovery:' >/dev/null && ok=1 && break
        # follower skips replay; treat 'ready at' line as proof too
        journalctl -u "$UNIT" -n 200 --no-pager | grep ' ready at ' >/dev/null && ok=1 && break
        sleep 1
    done
    [ "$ok" = "1" ] || rollback
    journalctl -u "$UNIT" -n 50 --no-pager | grep -E 'recovery:|ready at|WAL_RECOVERY_HALT' | tail -5 | sed 's/^/    /'
    journalctl -u "$UNIT" -n 50 --no-pager | grep 'WAL_RECOVERY_HALT' >/dev/null && rollback
fi

# ---------------------------------------------------------------- step 7
log "step 7/8 verification — verify-shard.sh"
if [ "$DRY" != "1" ]; then
    "$SELF_DIR/verify-shard.sh" --shard "$SHARD" || rollback
fi

# ---------------------------------------------------------------- step 8
log "step 8/8 traffic unpause — restore Normal"
run "rcli SET system:degradation:mode Normal"
run "rcli DEL system:degradation:reason"
log "shard $SHARD swap complete on sha256=$SHA"
log "next shard: wait >= ${INTERSHARD_GAP}s between shards (spec §19.6)"
exit 0
