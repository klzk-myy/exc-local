#!/usr/bin/env bash
# failover-runbook.sh — the RTO≤5min primary→secondary promotion
# procedure (Task 9.3.4, spec §18.6.4). Each step is a function so a drill
# can run steps selectively; the full cutover runs them in order.
#
# pending-infra: requires a live secondary region; validated end-to-end
# only during the monthly/quarterly DR drill (Task 9.3.21).
set -euo pipefail

PG_STANDBY="${PG_STANDBY:-pg-standby.dc2.exc.local}"
REDIS_DR="${REDIS_DR:-redis-replica.dc2.exc.local}"
WAL_BUCKET_DR="${WAL_BUCKET_DR:-s3://exchange-wal-archive-dr}"
ADMIN_SOCK="${ADMIN_SOCK:-/run/haproxy/admin.sock}"

step() { printf '\n=== %s ===\n' "$1"; }

# --- 1. Declare disaster ------------------------------------------------------
# Trigger: primary DC unreachable >30s / structural event / power loss.
# BCP quorum required before proceeding (spec §18.6.4 step 1).
declare_disaster() {
    step "declare disaster"
    echo "Confirm with on-call + incident commander; record decision time."
    echo "Wall clock: $(date -u +%FT%TZ) — failover budget starts now (RTO ≤5min)."
}

# --- 2. PostgreSQL semi-sync promotion (RPO ≤15s) -----------------------------
pg_promote() {
    step "pg promote"
    echo "Check replication gap ≤15s before promoting:"
    echo "  ssh $PG_STANDBY 'psql -c \"SELECT now()-pg_last_xact_replay_timestamp() AS gap\"'"
    ssh "$PG_STANDBY" "pg_ctlcluster 16 main promote"
    echo "Post-promotion: repoint synchronous_standby_names; update DSN."
}

# --- 3. Redis promotion (RPO ≤5s / RTO ≤30s) ----------------------------------
redis_promote() {
    step "redis promote"
    ssh "$REDIS_DR" "redis-cli REPLICAOF NO ONE"
    echo "Re-register Sentinel on the promoted node (deploy/redis/)."
}

# --- 4. WAL tail recovery (book RPO = 0) --------------------------------------
wal_replay() {
    step "wal replay"
    echo "Replay any un-replicated WAL from the DR-replicated archive:"
    echo "  deploy/postgres/restore_pitr.sh --bucket $WAL_BUCKET_DR (PG side)"
    echo "  services/cmd/replay --from-archive (engine side, spec exchange:replay-from-archive)"
    echo "Verify: latest archived segment replays clean in-region."
}

# --- 5. DNS/edge reroute ------------------------------------------------------
edge_reroute() {
    step "edge reroute"
    echo "Flip provider health checks / Route53 or Cloudflare load balancer"
    echo "to secondary DC endpoints (spec §18.6.4 step 4: 15–30s)."
    echo "HAProxy blue/green map stays authoritative in-region."
}

# --- 6. Integrity audit + resumption ladder ----------------------------------
post_failover() {
    step "post-failover verification"
    echo "Run the §18.6.5 6-stage integrity audit (RecoveryManager) before"
    echo "opening books; then the §18.6.6 resumption ladder (CANCEL_ONLY 60s →"
    echo "FIX resync → WS resume → reopening auction → continuous)."
    echo "Reconcile: PG replay position, book seq, ledger zero-sum, nostro."
}

case "${1:-all}" in
    declare) declare_disaster ;;
    pg) pg_promote ;;
    redis) redis_promote ;;
    wal) wal_replay ;;
    edge) edge_reroute ;;
    verify) post_failover ;;
    all)
        declare_disaster; pg_promote; redis_promote; wal_replay
        edge_reroute; post_failover
        ;;
    *) echo "usage: $0 [declare|pg|redis|wal|edge|verify|all]" >&2; exit 2 ;;
esac
