#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# trino_federation.sh — provision + prove the Trino federated cold-query path
# (Phase-09 Task 9.3.24 AC "Cold data queryable via Presto/Trino federated
# query", docs/ops/data-tiering-policy.md §5 option 2).
#
# Topology (all docker on the dev compose network so names resolve):
#
#   exc-dev-postgres-1   HOT + WARM tiers + archive ledger
#                        (partition_archive_log / partition_tier_state,
#                         warm.* detached partitions, archive_restore.*)
#   exc-archive-s3gw     versitygw posix S3 gateway (busybox + host-mounted
#                        static binary — identical pattern to
#                        ch_restore_drill.sh's exc-ch-s3gw). Bucket:
#                        exchange-partition-archive (DefaultPartitionBucket).
#   exc-dev-trino        trinodb/trino:476, catalogs mounted from
#                        deploy/trino/catalog/:
#                          exchange_pg  -> PostgreSQL connector (warm+ledger)
#                          archive      -> Hive connector + native-S3 fs over
#                                          the archive bucket, file metastore
#
# Subcommands:
#   up        idempotent: s3gw + bucket + trino container, waits for readiness
#   seed      export archive_restore.itest_orders_p2020_01 (a partition the
#             archiver itself re-materialized — genuine cold-tier content) to
#             parquet+zstd via a throwaway clickhouse-local, PUT it to
#             s3://exchange-partition-archive/itest_orders/itest_orders_p2020_01/
#             (archiver s3Keys layout), then register the external table in the
#             Trino `archive` catalog
#   query     run the proof queries through the Trino HTTP API
#   down      stop & remove the trino/s3 containers + volumes
#
# Usage:
#   trino_federation.sh            # up + seed + query (full demo)
#   trino_federation.sh up|seed|query|down
#
# Env overrides:
#   TRINO_IMAGE/TRINO_CONTAINER/TRINO_HOST_PORT (trinodb/trino:476 /
#                                               exc-dev-trino / 18443)
#   S3_CONTAINER/S3_HOST_PORT/S3_BUCKET         (exc-archive-s3gw / 17072 /
#                                               exchange-partition-archive)
#   S3_ACCESS/S3_SECRET  gateway creds          (trino_dev / trino_dev_secret —
#                                               dev-only, match
#                                               deploy/trino/catalog/archive.properties)
#   NETWORK              docker network         (exc-dev_default)
#   PG_CONTAINER/PG_*    dev postgres           (exc-dev-postgres-1 / exchange /
#                                               exchange_dev / exchange)
#   SEED_TABLE           restored partition     (archive_restore.itest_orders_p2020_01)
#   WORKDIR              binary workdir         (/tmp/trino-federation)
#   VGW_VERSION          versitygw release      (v1.8.0)
# -----------------------------------------------------------------------------
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)

TRINO_IMAGE=${TRINO_IMAGE:-trinodb/trino:476}
TRINO_CONTAINER=${TRINO_CONTAINER:-exc-dev-trino}
TRINO_HOST_PORT=${TRINO_HOST_PORT:-18443}
TRINO_USER=${TRINO_USER:-coldquery}
METASTORE_VOLUME=${METASTORE_VOLUME:-exc-dev-trino-metastore}

S3_CONTAINER=${S3_CONTAINER:-exc-archive-s3gw}
S3_HOST_PORT=${S3_HOST_PORT:-17072}
S3_CTR_PORT=17070
S3_BUCKET=${S3_BUCKET:-exchange-partition-archive}
S3_ACCESS=${S3_ACCESS:-trino_dev}
S3_SECRET=${S3_SECRET:-trino_dev_secret}
S3_VOLUME=${S3_VOLUME:-exc-archive-s3gw-data}
S3_ALIAS=${S3_ALIAS:-archive-s3}
VGW_VERSION=${VGW_VERSION:-v1.8.0}

PG_CONTAINER=${PG_CONTAINER:-exc-dev-postgres-1}
PG_USER=${PG_USER:-exchange}
PG_PASSWORD=${PG_PASSWORD:-exchange_dev}
PG_DB=${PG_DB:-exchange}
SEED_TABLE=${SEED_TABLE:-archive_restore.itest_orders_p2020_01}
# archiver s3Keys() layout: {parent}/{partition}/{partition}.parquet
SEED_PARENT=${SEED_PARENT:-itest_orders}
SEED_PARTITION=${SEED_PARTITION:-itest_orders_p2020_01}

NETWORK=${NETWORK:-exc-dev_default}
WORKDIR=${WORKDIR:-/tmp/trino-federation}
CH_IMAGE=${CH_IMAGE:-clickhouse/clickhouse-server:25.8-alpine}

log() { printf '[trino-federation] %s\n' "$*"; }
die() { printf '[trino-federation] ERROR: %s\n' "$*" >&2; exit 1; }

mkdir -p "$WORKDIR/bin" "$WORKDIR/seed"

# --- versitygw host binary (mirrors ch_restore_drill.sh ensure_vgw) ----------

ensure_vgw() {
    local bin=$WORKDIR/bin/versitygw
    [[ -n ${VGW_BIN:-} && -x $VGW_BIN ]] && { VGW_HOST_BIN=$VGW_BIN; return; }
    [[ -x $bin ]] && { VGW_HOST_BIN=$bin; return; }
    log "downloading versitygw $VGW_VERSION"
    local tgz=$WORKDIR/vgw.tar.gz
    curl -fsSL -o "$tgz" \
        "https://github.com/versity/versitygw/releases/download/$VGW_VERSION/versitygw_${VGW_VERSION}_Linux_x86_64.tar.gz"
    mkdir -p "$WORKDIR/vgw-extract" && tar -xzf "$tgz" -C "$WORKDIR/vgw-extract"
    local found
    found=$(find "$WORKDIR/vgw-extract" -name versitygw -type f | head -1)
    [[ -n $found ]] || die "versitygw binary not in tarball"
    cp "$found" "$bin"; chmod +x "$bin"; VGW_HOST_BIN=$bin
}

# --- signed S3 request (stdlib python, same SigV4 as ch_restore_drill.sh) ----
# s3_req <METHOD> <path> [body-file]  — extra arg uploads a file with its
# sha256 as x-amz-content-sha256 (empty-hash signing for bodiless requests).
s3_req() {
    S3EP="http://127.0.0.1:${S3_HOST_PORT}" S3A="$S3_ACCESS" S3S="$S3_SECRET" \
    python3 - "$1" "$2" "${3:-}" <<'PYEOF'
import hashlib, hmac, os, sys, urllib.request, urllib.error, datetime
from urllib.parse import urlparse
method, path, body = sys.argv[1], sys.argv[2], sys.argv[3] or None
url = os.environ["S3EP"] + path
u = urlparse(url); host = u.netloc
data = open(body, "rb").read() if body else b""
t = datetime.datetime.now(datetime.timezone.utc)
amz, ds = t.strftime("%Y%m%dT%H%M%SZ"), t.strftime("%Y%m%d")
ph = hashlib.sha256(data).hexdigest()
hdrs = {"host": host, "x-amz-date": amz, "x-amz-content-sha256": ph}
signed = ";".join(sorted(hdrs))
canon = "\n".join([method, u.path or "/", u.query,
                   "".join(f"{k}:{hdrs[k]}\n" for k in sorted(hdrs)),
                   signed, ph])
scope = f"{ds}/us-east-1/s3/aws4_request"
sts = "\n".join(["AWS4-HMAC-SHA256", amz, scope,
                 hashlib.sha256(canon.encode()).hexdigest()])
def sign(k, m): return hmac.new(k, m.encode(), hashlib.sha256).digest()
k = sign(("AWS4" + os.environ["S3S"]).encode(), ds)
k = sign(k, "us-east-1"); k = sign(k, "s3"); k = sign(k, "aws4_request")
sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()
hdrs["Authorization"] = (f"AWS4-HMAC-SHA256 Credential={os.environ['S3A']}/{scope},"
                         f" SignedHeaders={signed}, Signature={sig}")
req = urllib.request.Request(url, method=method, headers=hdrs, data=(data or None))
try:
    r = urllib.request.urlopen(req, timeout=30)
    sys.stdout.write(r.read().decode()[:4000]); sys.exit(0 if r.status < 300 else 1)
except urllib.error.HTTPError as e:
    sys.stderr.write(f"s3_req {method} {path}: HTTP {e.code} {e.read().decode()[:500]}\n")
    sys.exit(1)
PYEOF
}

# --- trino HTTP API ----------------------------------------------------------
# trino_sql <sql>  — POST /v1/statement on the host-mapped port, follow
# nextUri (netloc rewritten to the mapped port: trino announces its
# in-container address), print columnar data. No trino-cli needed.
trino_sql() {
    local attempt
    for attempt in 1 2 3; do
        curl -sf -H "X-Trino-User: $TRINO_USER" \
            --data-binary "$1" \
            "http://127.0.0.1:${TRINO_HOST_PORT}/v1/statement" > "$WORKDIR/stmt.json" \
            || die "trino statement POST failed"
        if TRINO_HOST_PORT="$TRINO_HOST_PORT" python3 - "$WORKDIR/stmt.json" <<'PYEOF'
import json, os, re, sys, urllib.request
f = json.load(open(sys.argv[1]))
headers = {"X-Trino-User": "coldquery"}
def fetch(u):
    # nextUri announces the container-side address; rewrite to the host port.
    u = re.sub(r"^http://[^/]+", "http://127.0.0.1:" + os.environ["TRINO_HOST_PORT"], u)
    req = urllib.request.Request(u, headers=headers)
    return json.load(urllib.request.urlopen(req, timeout=60))
cols, rows = None, []
while True:
    if "columns" in f and cols is None:
        cols = [c["name"] for c in f["columns"]]
    if "data" in f:
        rows.extend(f["data"])
    if "error" in f:
        if "No nodes available" in f["error"].get("message", ""):
            sys.stderr.write("TRINO RETRYABLE: no nodes yet\n"); sys.exit(2)
        sys.stderr.write("TRINO ERROR: %s\n" % json.dumps(f["error"], indent=1)[:2000])
        sys.exit(1)
    if "nextUri" not in f:
        break
    f = fetch(f["nextUri"])
if cols:
    w = [len(c) for c in cols]
    for r in rows:
        for i, v in enumerate(r):
            w[i] = max(w[i], len(str(v)))
    print(" | ".join(c.ljust(w[i]) for i, c in enumerate(cols)))
    print("-+-".join("-" * x for x in w))
    for r in rows:
        print(" | ".join(str(v).ljust(w[i]) for i, v in enumerate(r)))
    print("(%d row%s)" % (len(rows), "" if len(rows) == 1 else "s"))
else:
    st = f.get("stats", {})
    print("OK (state=%s, elapsed=%sms)" % (st.get("state"), st.get("elapsedTimeMillis", "?")))
PYEOF
        then
            return 0
        elif [[ $? == 2 ]]; then
            sleep 5   # worker still re-announcing after restart
            continue
        else
            return 1
        fi
    done
    die "trino query kept failing after retries"
}

# --- provisioning ------------------------------------------------------------

provision_s3() {
    if ! docker inspect "$S3_CONTAINER" >/dev/null 2>&1; then
        log "starting archive S3 gateway $S3_CONTAINER (versitygw posix)"
        docker run -d --name "$S3_CONTAINER" --network "$NETWORK" \
            --network-alias "$S3_ALIAS" \
            -p "${S3_HOST_PORT}:${S3_CTR_PORT}" \
            -v "$VGW_HOST_BIN:/usr/local/bin/versitygw:ro" \
            -v "$S3_VOLUME:/data" \
            busybox /usr/local/bin/versitygw \
                --port "0.0.0.0:${S3_CTR_PORT}" \
                --access "$S3_ACCESS" --secret "$S3_SECRET" \
                posix /data >/dev/null
    fi
    docker start "$S3_CONTAINER" >/dev/null 2>&1 || true
    for i in $(seq 1 30); do
        curl -s -o /dev/null "http://127.0.0.1:${S3_HOST_PORT}/" && break
        [[ $i == 30 ]] && die "s3 gateway not listening on :$S3_HOST_PORT"
        sleep 1
    done
    if ! s3_req GET / | grep -q "<Name>${S3_BUCKET}</Name>"; then
        log "creating bucket $S3_BUCKET"
        s3_req PUT "/${S3_BUCKET}" || die "bucket create failed"
    fi
}

provision_trino() {
    # file metastore writes catalog JSON under local:///metastore
    # (= $local.location/metastore); the named volume is root-owned and the
    # trino server runs as uid 1000 — pre-create with open perms.
    docker run --rm -v "$METASTORE_VOLUME:/var/trino" busybox \
        sh -c 'mkdir -p /var/trino/metastore && chmod -R 0777 /var/trino' >/dev/null
    if ! docker inspect "$TRINO_CONTAINER" >/dev/null 2>&1; then
        log "starting Trino $TRINO_CONTAINER ($TRINO_IMAGE)"
        docker run -d --name "$TRINO_CONTAINER" --network "$NETWORK" \
            -p "${TRINO_HOST_PORT}:8080" \
            -v "$REPO_ROOT/deploy/trino/catalog:/etc/trino/catalog:ro" \
            -v "$METASTORE_VOLUME:/var/trino" \
            "$TRINO_IMAGE" >/dev/null
    fi
    docker start "$TRINO_CONTAINER" >/dev/null 2>&1 || true
    log "waiting for trino /v1/info"
    for i in $(seq 1 90); do
        curl -sf "http://127.0.0.1:${TRINO_HOST_PORT}/v1/info" \
            | grep -q '"starting":false' && { log "trino up"; return; }
        [[ $i == 90 ]] && die "trino did not become ready"
        sleep 2
    done
}

cmd_up() {
    ensure_vgw
    provision_s3
    provision_trino
}

cmd_seed() {
    local pq=$WORKDIR/seed/${SEED_PARTITION}.parquet
    if [[ ! -s $pq ]]; then
        log "exporting $SEED_TABLE -> parquet+zstd via clickhouse-local"
        docker run --rm --network "$NETWORK" \
            -v "$WORKDIR/seed:/out" \
            "$CH_IMAGE" clickhouse-local --query "
                SELECT * FROM postgresql(
                    'postgres:5432', '$PG_DB', '${SEED_TABLE#*.}',
                    '$PG_USER', '$PG_PASSWORD', '${SEED_TABLE%%.*}')
                INTO OUTFILE '/out/${SEED_PARTITION}.parquet'
                FORMAT Parquet" \
            --output_format_parquet_compression_method=zstd \
            || die "parquet export failed (postgresql() in clickhouse-local)"
    fi
    local key="${SEED_PARENT}/${SEED_PARTITION}/${SEED_PARTITION}.parquet"
    log "PUT s3://$S3_BUCKET/$key"
    s3_req PUT "/$S3_BUCKET/$key" "$pq" || die "archive PUT failed"
    # Manifest is written at the archiver's real post-fix layout
    # (_manifests/ prefix — Hive treats every non-hidden file under
    # external_location as table data; engines skip _- and .-paths).
    # See deploy/trino/README.md.
    s3_req PUT "/$S3_BUCKET/${SEED_PARENT}/${SEED_PARTITION}/_manifests/${SEED_PARTITION}.manifest.json" \
        <(printf '{"partition":"%s","parent":"%s","format":"parquet+zstd","source":"trino_federation.sh seed"}' \
            "$SEED_PARTITION" "$SEED_PARENT") \
        || die "manifest PUT failed"

    log "registering external table archive.cold.$SEED_PARTITION in trino"
    trino_sql "CREATE SCHEMA IF NOT EXISTS archive.cold"
    trino_sql "DROP TABLE IF EXISTS archive.cold.$SEED_PARTITION"
    # Hive catalog has no timestamptz column type: pg timestamptz archives land
    # as TIMESTAMP(MICROS, utc); declare timestamp(6) (µs precision per
    # hive.timestamp-precision) — instant values are preserved, zone is the
    # reader's session zone (UTC in this deployment).
    trino_sql "CREATE TABLE archive.cold.$SEED_PARTITION (
        id bigint, account_id bigint, qty decimal(20,8), created_at timestamp(6))
        WITH (format='PARQUET',
              external_location='s3://$S3_BUCKET/$SEED_PARENT/$SEED_PARTITION/')"
}

cmd_query() {
    log "Q1 catalogs"
    trino_sql "SHOW CATALOGS"
    log "Q2 warm/ledger via exchange_pg: archive log roll-up"
    trino_sql "SELECT status, count(*) AS entries, sum(row_count) AS rows_archived
               FROM exchange_pg.public.partition_archive_log GROUP BY status ORDER BY status"
    log "Q3 cold via archive catalog: parquet rows on s3://$S3_BUCKET"
    trino_sql "SELECT count(*) AS cold_rows, min(created_at) AS first, max(created_at) AS last
               FROM archive.cold.$SEED_PARTITION"
    log "Q4 cross-catalog federation: ledger JOIN cold parquet"
    trino_sql "SELECT l.partition_name, l.status, l.row_count AS ledger_rows,
                      c.cold_rows AS parquet_rows
               FROM exchange_pg.public.partition_archive_log l
               JOIN (SELECT count(*) AS cold_rows FROM archive.cold.$SEED_PARTITION) c
                 ON true
               WHERE l.partition_name = '$SEED_PARTITION'
               GROUP BY l.partition_name, l.status, l.row_count, c.cold_rows"
}

cmd_down() {
    docker rm -f "$TRINO_CONTAINER" "$S3_CONTAINER" 2>/dev/null || true
    docker volume rm "$METASTORE_VOLUME" "$S3_VOLUME" 2>/dev/null || true
    log "removed $TRINO_CONTAINER / $S3_CONTAINER (+ volumes)"
}

case "${1:-demo}" in
    up)    cmd_up ;;
    seed)  cmd_seed ;;
    query) cmd_query ;;
    down)  cmd_down ;;
    demo)  cmd_up; cmd_seed; cmd_query ;;
    *)     die "usage: $0 [up|seed|query|down|demo]" ;;
esac
