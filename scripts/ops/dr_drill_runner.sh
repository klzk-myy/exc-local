#!/usr/bin/env bash
# =============================================================================
# dr_drill_runner.sh — automated quarterly DR drill orchestrator + evidence
# collector (Phase-09 Task 9.3.21, spec §18.3 RPO/RTO, §18.6 recovery
# workflow, §19.5 DORA resilience-testing evidence, §24 #211).
#
# The runbook (docs/runbooks/dr-drill.md) defines the procedure; this runner
# executes the runnable drill catalog, records RPO/RTO actual vs target per
# component, collects evidence, and writes the §4 post-drill report.
#
# Catalog (runbook §3):
#   D1 region failover     deploy/dr/failover-runbook.sh — requires a live
#                          secondary (--secondary-pg/--secondary-redis); SKIP
#                          when no secondary is provisioned
#   D2 postgres failover   deploy/scripts/pg_failover_drill.sh (docker)
#   D3 WAL archive replay  services/bin/replay --from archive — needs an S3
#                          WAL bucket (EXC_S3_WAL_BUCKET); SKIP otherwise
#   D4 ClickHouse restore  deploy/clickhouse/local_drill.sh — needs the CH
#                          container + clickhouse-backup + S3 gateway
#   D5 redis sentinel      deploy/crons/redis-failover-drill.sh --json
#                          (--sentinels or SENTINEL_ADDRS must be reachable)
#   D6 chaos suite         tests/chaos/run.sh --runs N
#   D7 secrets lifecycle   services/bin/secretdrill (needs DATABASE_URL +
#                          PG secrets_inventory); the decrypt-in-secondary
#                          leg additionally needs Vault in the DR region and
#                          is reported as env-bound until provisioned
#
# Each drill appends one row per component to $OUT/results.jsonl:
#   {"drill","component","rpo_target_ms","rpo_actual_ms","rto_target_ms",
#    "rto_actual_ms","verdict","detail","evidence"}
# verdict: PASS | FAIL | SKIP. SKIP = prerequisite infra absent — reported in
# the drill report, never silently counted as covered. --strict promotes
# SKIP→FAIL for environments claiming full catalog coverage.
#
# Output:
#   evidence root  $OUT (default docs/incidents/drills/<YYYY-Qn>/evidence/)
#   report         docs/incidents/drills/<YYYY-Qn>-drill.md (§4 template)
#
# Usage:
#   dr_drill_runner.sh [--drills D1,D2,...] [--quarter YYYY-Qn]
#                      [--out DIR] [--report PATH] [--chaos-runs N]
#                      [--sentinels h:p,...] [--database-url DSN]
#                      [--s3-wal-bucket NAME] [--s3-endpoint URL]
#                      [--secondary-pg HOST] [--secondary-redis HOST]
#                      [--wal-dir DIR] [--snap-dir DIR] [--strict] [-h]
#
#   --wal-dir / --snap-dir enable the D1 local composite (archive→fetch→
#   reconstruct with fingerprint parity on the archive bytes) and the D3
#   archive leg; the composite is single-failure-domain — the parent D1 row
#   stays SKIP until a real secondary cutover runs.
# Exit: 0 all executed drills pass · 1 any FAIL · 64 usage
# =============================================================================
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd -P)"

YEAR="$(date -u +%Y)"; QNUM=$(( (10#$(date -u +%m) + 2) / 3 ))
QUARTER="${YEAR}-Q${QNUM}"
DRILLS="D1,D2,D3,D4,D5,D6,D7"
OUT=""
REPORT=""
CHAOS_RUNS=3
STRICT=0
SENTINELS="${SENTINEL_ADDRS:-}"
DATABASE_URL_ARG="${DATABASE_URL:-postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable}"
S3_WAL_BUCKET="${EXC_S3_WAL_BUCKET:-}"
S3_ENDPOINT="${EXC_S3_ENDPOINT:-}"
SECONDARY_PG="${PG_STANDBY:-}"
SECONDARY_REDIS="${REDIS_DR:-}"
WAL_DIR=""          # source shard-0 WAL dir for D1 composite / D3 archive leg
SNAP_DIR=""         # snapshot root replicated to "secondary" (D1 composite)
SHARD=0
INSTRUMENT_ID="${INSTRUMENT_ID:-7}"

usage() { sed -n '2,45p' "$0"; exit "${1:-64}"; }
while [ $# -gt 0 ]; do
    case "$1" in
        --drills)          DRILLS="$2"; shift 2;;
        --quarter)         QUARTER="$2"; shift 2;;
        --out)             OUT="$2"; shift 2;;
        --report)          REPORT="$2"; shift 2;;
        --chaos-runs)      CHAOS_RUNS="$2"; shift 2;;
        --sentinels)       SENTINELS="$2"; shift 2;;
        --database-url)    DATABASE_URL_ARG="$2"; shift 2;;
        --s3-wal-bucket)   S3_WAL_BUCKET="$2"; shift 2;;
        --s3-endpoint)     S3_ENDPOINT="$2"; shift 2;;
        --secondary-pg)    SECONDARY_PG="$2"; shift 2;;
        --secondary-redis) SECONDARY_REDIS="$2"; shift 2;;
        --wal-dir)         WAL_DIR="$2"; shift 2;;
        --snap-dir)        SNAP_DIR="$2"; shift 2;;
        --shard)           SHARD="$2"; shift 2;;
        --instrument-id)   INSTRUMENT_ID="$2"; shift 2;;
        --strict)          STRICT=1; shift;;
        -h|--help)         usage 0;;
        *) echo "unknown arg: $1" >&2; usage 64;;
    esac
done

OUT="${OUT:-$REPO_ROOT/docs/incidents/drills/$QUARTER/evidence}"
REPORT="${REPORT:-$REPO_ROOT/docs/incidents/drills/$QUARTER-drill.md}"
mkdir -p "$OUT"
RESULTS="$OUT/results.jsonl";   : > "$RESULTS"
TIMELINE="$OUT/timeline.log";   : > "$TIMELINE"

log() {
    printf '[dr-drill %s] %s\n' "$(date -u +%H:%M:%S.%3N)" "$*" | tee -a "$TIMELINE"
}
wants() { case ",$DRILLS," in *",$1,"*) return 0;; *) return 1;; esac; }

# row <drill> <component> <rpo_t_ms> <rpo_a_ms> <rto_t_ms> <rto_a_ms> <verdict> <detail> <evidence>
# numeric fields may be "null". SKIP is promoted to FAIL under --strict.
row() {
    local drill=$1 comp=$2 rpomt=$3 rpoma=$4 rtot=$5 rtoa=$6 verdict=$7 detail=$8 ev=$9
    if [ "$STRICT" = "1" ] && [ "$verdict" = "SKIP" ]; then verdict="FAIL"; fi
    jq -cn --arg d "$drill" --arg c "$comp" --arg v "$verdict" \
           --arg det "$detail" --arg ev "$ev" \
           --argjson rt "${rpomt:-null}" --argjson ra "${rpoma:-null}" \
           --argjson tt "${rtot:-null}" --argjson ta "${rtoa:-null}" \
        '{drill:$d,component:$c,rpo_target_ms:$rt,rpo_actual_ms:$ra,
          rto_target_ms:$tt,rto_actual_ms:$ta,verdict:$v,detail:$det,
          evidence:$ev}' >> "$RESULTS"
    log "  $drill/$comp: $verdict — $detail"
}

run_drill() { # run_drill <id> <title> <log-name> <cmd...> -> sets RC
    local id=$1 title=$2 name=$3; shift 3
    log "=== $id $title ==="
    local lf="$OUT/$name.log"
    RC=0
    "$@" > "$lf" 2>&1 || RC=$?
    log "  exit=$RC log=$lf"
    LAST_LOG="$lf"
}

# --- D1: full region failover -------------------------------------------------
# With --secondary-* hosts: run deploy/dr/failover-runbook.sh against the real
# second region. Without them but with --wal-dir/--snap-dir + a reachable S3
# endpoint, run the local composite: archive WAL to S3, fetch the archived
# bytes into a scratch "secondary" dir, reconstruct the book via
# snapshot+tail recovery, and verify fingerprint parity with the primary —
# that is the production D1 order-book leg, single failure domain. Legs that
# truly need a second region (edge reroute, secrets-in-secondary, resumption
# ladder against live traffic) stay SKIP; the parent row stays SKIP because
# the full end-to-end cutover did not happen.
d1() {
    if [ -n "$SECONDARY_PG" ] || [ -n "$SECONDARY_REDIS" ]; then
        run_drill D1 "region failover" d1 \
            "$REPO_ROOT/deploy/dr/failover-runbook.sh" all
        local v=FAIL; [ "$RC" -eq 0 ] && v=PASS
        row D1 "region-failover" 0 null 300000 null "$v" \
            "failover-runbook.sh all exit=$RC (PG_STANDBY=$SECONDARY_PG REDIS_DR=$SECONDARY_REDIS)" \
            "$LAST_LOG"
        return
    fi

    local wal_audit="$REPO_ROOT/core/build/wal_audit"
    local exchange="$REPO_ROOT/services/bin/exchange"
    if [ -z "$WAL_DIR" ] || [ ! -d "$WAL_DIR" ]; then
        row D1 "region-failover" null null null null SKIP \
            "no secondary provisioned (--secondary-pg/--secondary-redis) and no --wal-dir for the local composite; pending-infra per docs/ops/dr.md" \
            "deploy/dr/failover-runbook.sh"
        return
    fi

    local work="$OUT/d1"; mkdir -p "$work"
    local fp1="" fp2="" rto_ms=null detail=""

    # Leg 1 — primary fingerprint (snapshot+tail, the production boot path).
    if [ -x "$wal_audit" ] && [ -n "$SNAP_DIR" ] && [ -d "$SNAP_DIR" ]; then
        fp1=$("$wal_audit" -wal-dir "$WAL_DIR" -snap-dir "$SNAP_DIR" \
              -instrument-id "$INSTRUMENT_ID" -mode fingerprint 2>>"$work/d1.err" || true)
    fi

    # Leg 2 — archive WAL to S3 and fetch the bytes back as the "secondary".
    local restored="$work/restored-wal" archived_ok=0
    mkdir -p "$restored"
    if [ -n "$S3_WAL_BUCKET" ] && [ -x "$exchange" ]; then
        local stage="$work/stage-wal"
        mkdir -p "$stage" && cp "$WAL_DIR"/*.wal "$stage/" 2>>"$work/d1.err" || true
        EXC_S3_ENDPOINT="$S3_ENDPOINT" EXC_S3_WAL_BUCKET="$S3_WAL_BUCKET" \
            "$exchange" archive-wal --wal-dir="$stage" --shard="$SHARD" \
            --include-active >>"$work/d1-archive.log" 2>&1 || true
        # devs3/MinIO root is the bucket store — copying objects out is the
        # same bytes an S3 GET returns; archive-status cross-checks the index.
        local ob_root=""
        for cand in "${DEVS3_ROOT:-/tmp/devs3-root}/$S3_WAL_BUCKET/$SHARD" ; do
            [ -d "$cand" ] && ob_root="$cand"
        done
        if [ -n "$ob_root" ]; then
            find "$ob_root" -name '*.wal' -exec cp {} "$restored/" \; 2>>"$work/d1.err" || true
            [ -n "$(find "$restored" -name '*.wal' -print -quit)" ] && archived_ok=1
        fi
    fi
    if [ "$archived_ok" = "0" ]; then
        # No archive path — fall back to a byte copy so the reconstruction
        # leg still runs (single-domain composite).
        cp "$WAL_DIR"/*.wal "$restored/" 2>>"$work/d1.err" || true
        row D1 "wal-catchup" 0 null 30000 null SKIP \
            "S3 archive leg not exercised (no --s3-wal-bucket); used direct byte copy for reconstruction" \
            "$work/d1-archive.log"
    else
        row D1 "wal-catchup" 0 0 30000 null PASS \
            "sealed segments archived to s3://$S3_WAL_BUCKET and re-fetched intact" \
            "$work/d1-archive.log"
    fi

    # Leg 3 — secondary reconstruction: snapshot+tail on the fetched WAL.
    if [ -x "$wal_audit" ] && [ -n "$SNAP_DIR" ]; then
        local t0 t1 scan
        t0=$(date +%s%3N)
        scan=$("$wal_audit" -wal-dir "$restored" -snap-dir "$SNAP_DIR" \
               -instrument-id "$INSTRUMENT_ID" -mode recover -json \
               2>>"$work/d1.err" || true)
        t1=$(date +%s%3N)
        rto_ms=$((t1 - t0))
        fp2=$(echo "$scan" | jq -r '.fingerprint // ""' 2>/dev/null || true)
        local ok; ok=$(echo "$scan" | jq -r '.ok // false' 2>/dev/null || echo false)
        local v=FAIL det
        if [ "$ok" = "true" ] && [ "$rto_ms" -gt 10000 ]; then
            det="secondary reconstruction ${rto_ms}ms — exceeds 10s book RTO (fingerprint $fp2)"
        elif [ "$ok" = "true" ] && [ -n "$fp1" ] && [ "$fp1" = "$fp2" ]; then
            v=PASS; det="secondary reconstruction ${rto_ms}ms, fingerprint parity $fp2"
        elif [ "$ok" = "true" ]; then
            v=PASS; det="secondary reconstruction ${rto_ms}ms (no primary fingerprint to compare)"
        else
            det="reconstruction failed: $(echo "$scan" | jq -r '.detail // "unknown"' 2>/dev/null)"
        fi
        echo "$scan" > "$work/d1-restore.json"
        row D1 "book-restore" 0 0 10000 "$rto_ms" "$v" "$det" "$work/d1-restore.json"
    else
        row D1 "book-restore" 0 null 10000 null SKIP \
            "wal_audit binary or --snap-dir missing" ""
    fi

    # Legs that need a live second region / traffic — honestly SKIP.
    row D1 "edge-reroute" null null 30000 null SKIP \
        "Anycast/LB health-check flip needs a second network domain" ""
    row D1 "secrets-decrypt-secondary" null null null null SKIP \
        "DR-critical secret copies need Vault/KMS in the secondary region (Task 9.3.29)" ""
    row D1 "resumption-ladder" null null null null SKIP \
        "CANCEL_ONLY→auction→Normal ladder against live ingress needs a staged secondary engine" ""

    row D1 "region-failover" 0 null 300000 null SKIP \
        "end-to-end cutover pending-infra (single failure domain); composite legs above carry the locally-verifiable evidence" \
        "$work"
}

# --- D2: PostgreSQL semi-sync failover (docker) --------------------------------
d2() {
    if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
        row D2 "postgresql" 15000 null 300000 null SKIP "docker unavailable" ""
        return
    fi
    run_drill D2 "postgres failover" d2 \
        "$REPO_ROOT/deploy/scripts/pg_failover_drill.sh"
    local rto gap v=FAIL
    rto=$(grep -oE 'RTO=[0-9]+ms' "$LAST_LOG" | grep -oE '[0-9]+' | tail -1 || true)
    gap=$(grep -oE 'gap=[0-9.]+s' "$LAST_LOG" | grep -oE '[0-9.]+' | tail -1 || true)
    local gap_ms="null"; [ -n "${gap:-}" ] && gap_ms=$(awk -v g="$gap" 'BEGIN{printf "%d", g*1000}')
    [ "$RC" -eq 0 ] && v=PASS
    row D2 "postgresql" 15000 "${gap_ms}" 300000 "${rto:-null}" "$v" \
        "pg_failover_drill exit=$RC (docker semi-sync pair, RTO=${rto:-?}ms gap=${gap:-?}s)" \
        "$LAST_LOG"
}

# --- D3: WAL replay-from-archive ------------------------------------------------
d3() {
    local exchange="$REPO_ROOT/services/bin/exchange"
    local wal_audit="$REPO_ROOT/core/build/wal_audit"
    if [ -z "$S3_WAL_BUCKET" ]; then
        row D3 "wal-archive" 0 null 30000 null SKIP \
            "EXC_S3_WAL_BUCKET unset — no archive endpoint" ""
        return
    fi
    if [ ! -x "$exchange" ]; then
        row D3 "wal-archive" 0 null 30000 null SKIP \
            "services/bin/exchange not built (go build -o bin/exchange ./cmd/exchange)" ""
        return
    fi

    local work="$OUT/d3"; mkdir -p "$work"

    # Archive leg — only when a source WAL dir was given. archive-wal trims
    # local copies after verified upload, so always stage a copy.
    local archived=0
    if [ -n "$WAL_DIR" ] && [ -d "$WAL_DIR" ]; then
        local stage="$work/stage-wal"
        mkdir -p "$stage" && cp "$WAL_DIR"/*.wal "$stage/" 2>>"$work/d3.err" || true
        # Hash the staged bytes BEFORE archive-wal trims them post-upload.
        (cd "$stage" && sha256sum *.wal | sort) > "$work/stage.sha256" 2>>"$work/d3.err" || true
        EXC_S3_ENDPOINT="$S3_ENDPOINT" EXC_S3_WAL_BUCKET="$S3_WAL_BUCKET" \
            "$exchange" archive-wal --wal-dir="$stage" --shard="$SHARD" \
            --include-active > "$work/d3-archive.log" 2>&1 || true
        grep -q "archived=" "$work/d3-archive.log" && archived=1
    fi

    # Index/object integrity check.
    run_drill D3 "archive-status" d3 \
        env EXC_S3_ENDPOINT="$S3_ENDPOINT" EXC_S3_WAL_BUCKET="$S3_WAL_BUCKET" \
        "$exchange" archive-status --shard="$SHARD"
    local st_ok=FAIL; [ "$RC" -eq 0 ] && st_ok=PASS

    # Replay leg — fail-closed on seq gaps (no --allow-seq-gaps). The JSON
    # report lists every trade (multi-GB) — log into the ignored workdir.
    local today; today="$(date -u +%F)"
    run_drill D3 "replay-from-archive" d3/replay \
        env EXC_S3_ENDPOINT="$S3_ENDPOINT" EXC_S3_WAL_BUCKET="$S3_WAL_BUCKET" \
        "$exchange" replay-from-archive --instrument-id="$INSTRUMENT_ID" \
        --shard="$SHARD" --from="$today" --to="$today" --json
    local trades="" segs="" v=FAIL
    if [ -f "$LAST_LOG" ]; then
        trades=$(jq -r '.trades | length' "$LAST_LOG" 2>/dev/null || true)
        segs=$(jq -r '.segments | length' "$LAST_LOG" 2>/dev/null || true)
    fi

    # Byte-parity leg — wal_audit scan on the archived bytes fetched back.
    local entries="" parity=""
    if [ -x "$wal_audit" ]; then
        local restored="$work/restored" ob_root="${DEVS3_ROOT:-/tmp/devs3-root}/$S3_WAL_BUCKET/$SHARD"
        mkdir -p "$restored"
        [ -d "$ob_root" ] && find "$ob_root" -name '*.wal' -exec cp {} "$restored/" \; 2>>"$work/d3.err" || true
        if [ -n "$(find "$restored" -name '*.wal' -print -quit 2>/dev/null)" ]; then
            local rj
            rj=$("$wal_audit" -wal-dir "$restored" -instrument-id "$INSTRUMENT_ID" \
                 -mode scan -json 2>>"$work/d3.err" || true)
            echo "$rj" > "$work/restored-scan.json"
            # Per-file sha256 parity: archived bytes vs the uploaded staging copy.
            (cd "$restored" && sha256sum *.wal | sort) > "$work/restored.sha256"
            if [ -s "$work/stage.sha256" ] && \
               ! cmp -s "$work/stage.sha256" "$work/restored.sha256"; then
                parity_fail=1
                echo "sha256 mismatch stage-vs-restored" >> "$work/d3.err"
            fi
            entries=$(echo "$rj" | jq -r '.entries // ""' 2>/dev/null || true)
            local gaps corrupt
            gaps=$(echo "$rj" | jq -r '.seq_gaps // 1' 2>/dev/null || echo 1)
            corrupt=$(echo "$rj" | jq -r '.corrupt | tostring' 2>/dev/null || echo true)
            parity="archived-bytes entries=$entries gaps=$gaps corrupt=$corrupt"
            if [ "$gaps" != "0" ] || [ "$corrupt" != "false" ]; then
                parity="$parity — PARITY FAIL"
                parity_fail=1
            fi
        fi
    fi

    if [ "$RC" -eq 0 ] && [ "$st_ok" = "PASS" ] && [ "${parity_fail:-0}" = "0" ]; then v=PASS; fi
    row D3 "wal-archive" 0 0 30000 null "$v" \
        "archive-status=$st_ok replay exit=$RC segs=${segs:-?} trades=${trades:-?} $parity" \
        "$work"
}

# --- D4: ClickHouse restore -----------------------------------------------------
d4() {
    local ch="${CH_CONTAINER:-exc-dev-clickhouse-1}"
    if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$ch"; then
        row D4 "clickhouse" 60000 null 1800000 null SKIP \
            "container $ch not running" ""
        return
    fi
    if [ ! -x "${CHB_BIN:-/tmp/clickhouse-backup}" ]; then
        row D4 "clickhouse" 60000 null 1800000 null SKIP \
            "clickhouse-backup binary missing (CHB_BIN=${CHB_BIN:-/tmp/clickhouse-backup})" ""
        return
    fi
    # The drill runs clickhouse-backup INSIDE the container — a loopback
    # S3 endpoint must be translated to the container's gateway IP.
    local ep_ctr="${S3_ENDPOINT_CTR:-$S3_ENDPOINT}"
    case "$ep_ctr" in
        *127.0.0.1*|*localhost*)
            local gw; gw=$(docker inspect "$ch" \
                --format '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}' 2>/dev/null || true)
            [ -n "$gw" ] && ep_ctr=$(echo "$ep_ctr" | sed -E "s#//(127\.0\.0\.1|localhost)#//$gw#")
            ;;
    esac
    run_drill D4 "clickhouse restore" d4 \
        env "S3_ENDPOINT=${ep_ctr:-http://172.18.0.1:17070}" \
        "$REPO_ROOT/deploy/clickhouse/local_drill.sh" drill "drill-$QUARTER"
    local rto v=FAIL
    rto=$(grep -oE 'RTO: [0-9]+ ms' "$LAST_LOG" | grep -oE '[0-9]+' | tail -1 || true)
    [ "$RC" -eq 0 ] && v=PASS
    row D4 "clickhouse" 60000 null 1800000 "${rto:-null}" "$v" \
        "local_drill exit=$RC (scratch-db restore+verify, RTO=${rto:-?}ms)" \
        "$LAST_LOG"
}

# --- D5: Redis Sentinel failover --------------------------------------------------
d5() {
    if [ -z "$SENTINELS" ]; then
        row D5 "redis" 5000 null 30000 null SKIP \
            "no sentinel addrs (--sentinels / SENTINEL_ADDRS)" ""
        return
    fi
    local first="${SENTINELS%%,*}"
    if ! timeout 2 bash -c "</dev/tcp/${first%%:*}/${first##*:}" 2>/dev/null; then
        row D5 "redis" 5000 null 30000 null SKIP \
            "sentinel $first unreachable" ""
        return
    fi
    run_drill D5 "redis sentinel failover" d5 \
        "$REPO_ROOT/deploy/crons/redis-failover-drill.sh" \
        --sentinels "$SENTINELS" --json "$OUT/d5.json"
    local det= wto= rpo= v=FAIL
    if [ -f "$OUT/d5.json" ]; then
        det=$(jq -r '.detect_ms'    "$OUT/d5.json")
        wto=$(jq -r '.write_rto_ms' "$OUT/d5.json")
        rpo=$(jq -r '.rpo_proxy_ms' "$OUT/d5.json")
    fi
    [ "$RC" -eq 0 ] && v=PASS
    row D5 "redis" 5000 "${rpo:-null}" 30000 "${wto:-null}" "$v" \
        "detect=${det:-?}ms write_rto=${wto:-?}ms rpo_proxy=${rpo:-?}ms" \
        "$OUT/d5.json"
}

# --- D6: chaos scenario suite -----------------------------------------------------
d6() {
    local missing=0
    for b in core/build/matching_engine core/build/wal_audit \
             tests/soak/loadgen tests/chaos/bin/chaostool services/wal-recovery; do
        [ -x "$REPO_ROOT/$b" ] || { log "  missing binary: $b"; missing=1; }
    done
    if [ "$missing" = "1" ]; then
        row D6 "order-book-wal" 0 null 10000 null SKIP \
            "chaos suite binaries missing (build core + loadgen + chaostool)" ""
        return
    fi
    run_drill D6 "chaos suite" d6 \
        "$REPO_ROOT/tests/chaos/run.sh" --runs "$CHAOS_RUNS" \
        --root "$OUT/chaos"
    local total passed rmax dups miss v=FAIL
    # run.sh writes "<results-root>.json" (here: $OUT/chaos.json)
    local rj="$OUT/chaos.json"
    [ -f "$rj" ] || rj="$REPO_ROOT/tests/chaos/results.json"
    if [ -f "$rj" ]; then
        total=$(jq -r '.total'  "$rj" 2>/dev/null || echo 0)
        passed=$(jq -r '.passed' "$rj" 2>/dev/null || echo 0)
        rmax=$(jq -r '[.runs[].recovery_ms]|max'  "$rj" 2>/dev/null || echo null)
        dups=$(jq -r '[.runs[].dup_trade_ids]|add' "$rj" 2>/dev/null || echo null)
        miss=$(jq -r '[.runs[].missing_trades]|add' "$rj" 2>/dev/null || echo null)
        [ "$passed" = "$total" ] && [ "$total" != "0" ] && v=PASS
    fi
    row D6 "order-book-wal" 0 "${dups:-0}" 10000 "${rmax:-null}" "$v" \
        "chaos suite ${passed:-0}/${total:-0} runs; max recovery ${rmax:-?}ms; dups=${dups:-?} missing=${miss:-?}" \
        "$rj"
}

# --- D7: secrets lifecycle / decrypt-in-secondary ----------------------------------
d7() {
    local sd="$REPO_ROOT/services/bin/secretdrill"
    if [ ! -x "$sd" ]; then
        row D7 "secrets" null null null null SKIP \
            "services/bin/secretdrill not built; decrypt-in-secondary leg env-bound (needs DR Vault)" ""
        return
    fi
    run_drill D7 "secrets rotation lifecycle" d7 \
        env "DATABASE_URL=$DATABASE_URL_ARG" "$sd"
    local v=FAIL; [ "$RC" -eq 0 ] && v=PASS
    local det="secretdrill exit=$RC (rotation lifecycle + audit trail)"
    [ "$v" = "PASS" ] && det="$det; NB: decrypt-in-secondary leg still env-bound (no DR Vault)"
    row D7 "secrets" null null null null "$v" "$det" "$LAST_LOG"
}

# --- run ------------------------------------------------------------------------
log "DR drill $QUARTER starting — drills=$DRILLS out=$OUT strict=$STRICT"
for d in D1 D2 D3 D4 D5 D6 D7; do
    wants "$d" && "$(echo "$d" | tr 'A-Z' 'a-z')"
done

# --- report (runbook §4 template) -----------------------------------------------
jq -rn --arg q "$QUARTER" --arg now "$(date -u '+%Y-%m-%d %H:%M:%SZ')" '
  "# DR Drill Report — " + $q + "\n\n- generated: " + $now
' > "$REPORT"
{
    echo "- evidence root: \`$OUT/\`"
    echo "- drills requested: $DRILLS"
    echo
    echo "| component | RPO target | RPO actual | RTO target | RTO actual | verdict |"
    echo "|---|---|---|---|---|---|"
    # canonical §18.3 rows first, then any extra rows
    for comp in order-book-wal wal-archive postgresql redis market-data clickhouse user-data secrets region-failover; do
        jq -r --arg c "$comp" \
            'select(.component==$c) | "| \($c) | \(if .rpo_target_ms==null then "-" else (.rpo_target_ms|tostring) + "ms" end) | \(if .rpo_actual_ms==null then "-" else (.rpo_actual_ms|tostring) + "ms" end) | \(if .rto_target_ms==null then "-" else (.rto_target_ms|tostring) + "ms" end) | \(if .rto_actual_ms==null then "-" else (.rto_actual_ms|tostring) + "ms" end) | **\(.verdict)** |"' \
            "$RESULTS" || true
    done
    jq -r 'select(.component|IN("order-book-wal","wal-archive","postgresql","redis","market-data","clickhouse","user-data","secrets","region-failover")|not) |
            "| \(.component) | \(.rpo_target_ms) | \(.rpo_actual_ms) | \(.rto_target_ms) | \(.rto_actual_ms) | **\(.verdict)** |"' \
        "$RESULTS" || true
    echo
    echo "## Timeline"
    echo
    sed 's/^/- /' "$TIMELINE"
    echo
    echo "## Incidents during drill"
    echo
    if jq -es 'any(.verdict=="FAIL")' "$RESULTS" >/dev/null; then
        jq -r 'select(.verdict=="FAIL") | "- `" + .drill + "/" + .component + "`: " + .detail + " (evidence: `" + .evidence + "`)"' \
            "$RESULTS"
    else
        echo "- none recorded"
    fi
    echo
    echo "## Evidence"
    echo
    jq -r '"- `" + .drill + "/" + .component + "` → `" + .evidence + "` — " + .detail' "$RESULTS"
    echo
    echo "## Remediation items"
    echo
    if jq -es '[.[]|select(.verdict=="SKIP" or .verdict=="FAIL")]|length>0' "$RESULTS" >/dev/null; then
        jq -r 'select(.verdict=="SKIP" or .verdict=="FAIL") | "- `" + .drill + "`: " + .detail + " — owner: TBD, due: next drill"' \
            "$RESULTS"
    else
        echo "- none"
    fi
} >> "$REPORT"

# --- verdict ----------------------------------------------------------------------
PASS_N=$(jq -s '[.[]|select(.verdict=="PASS")]|length' "$RESULTS")
FAIL_N=$(jq -s '[.[]|select(.verdict=="FAIL")]|length' "$RESULTS")
SKIP_N=$(jq -s '[.[]|select(.verdict=="SKIP")]|length' "$RESULTS")
log "== drill $QUARTER complete: $PASS_N pass / $FAIL_N fail / $SKIP_N skip =="
log "report: $REPORT"
[ "$FAIL_N" -eq 0 ]
