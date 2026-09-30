#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# ch_restore_drill.sh — ClickHouse restore drill, full lifecycle (Task 4.3.6,
# spec §18.3, §24 #159). Provisions the drill topology on docker, then runs:
#
#   a. `backup.sh counts` on the SOURCE cluster (baseline, sampled partition)
#   b. `backup.sh full` -> S3-compatible gateway; `backup.sh list` confirms it
#   c. `backup.sh restore <name>` into a separate scratch ClickHouse container
#      (wall-clock timed — RTO <= 30 min evidence)
#   d. `backup.sh counts` on scratch + `backup.sh verify` — sampled partition
#      row counts must match (exit non-zero otherwise)
#
# Topology (all docker, all on the dev compose network so containers resolve
# each other by name):
#
#   exc-dev-clickhouse-1   source (touched read-only: counts + create_remote)
#   exc-ch-s3gw            versitygw posix S3 gateway (busybox container —
#                          MinIO images/binaries are unpublishable: project
#                          archived 2025, dl.min.io returns 410 Gone)
#   exc-ch-scratch         empty ClickHouse, same image tag as the source
#
# clickhouse-backup must run on the ClickHouse host (it reads/writes the data
# dir), so the binary is docker-cp'd into each CH container and driven via
# generated `docker exec` wrappers behind CLICKHOUSE_BACKUP_BIN /
# CLICKHOUSE_CLIENT_BIN — the committed deploy/clickhouse/backup.sh driver is
# used for every step unchanged. The committed config templates
# (clickhouse-backup/config.yml, config-scratch.yml) are rendered with
# envsubst — clickhouse-backup does not expand ${VAR} itself.
#
# Usage:
#   ch_restore_drill.sh            # provision (idempotent) + run a–d
#   ch_restore_drill.sh cleanup    # stop & remove scratch/s3 containers, volume
#
# Env overrides (defaults = dev topology from docker-compose.dev.yml):
#   CH_CONTAINER        source container        (exc-dev-clickhouse-1)
#   CH_USER/CH_PASSWORD source + scratch creds  (exchange / exchange_dev)
#   CH_DATABASE         sampled database        (exchange_analytics)
#   PARTITION           partition_id to sample  (auto: largest active)
#   BACKUP_NAME         backup name             (daily-YYYYMMDD, same as
#                        `backup.sh full` derives)
#   NETWORK             docker network          (auto: first net of source)
#   S3_CONTAINER/S3_HOST_PORT/S3_BUCKET         (exc-ch-s3gw / 17071 / exchange-ch-backup)
#   S3_ACCESS/S3_SECRET gateway creds           (generated once, kept in
#                        $DRILL_WORKDIR/s3.env — never committed)
#   SCRATCH_CONTAINER/SCRATCH_HTTP_PORT/SCRATCH_NATIVE_PORT/SCRATCH_IMAGE
#                                             (exc-ch-scratch / 18123 / 19000 /
#                                              source container's image tag)
#   CHB_BIN / VGW_BIN   pre-fetched binaries    (else downloaded to workdir)
#   DRILL_WORKDIR                               (/tmp/ch-restore-drill)
#   RTO_SECONDS                                 (1800 — spec §18.3 bound)
# -----------------------------------------------------------------------------
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
BACKUP_SH=$REPO_ROOT/deploy/clickhouse/backup.sh
CFG_SRC_TPL=$REPO_ROOT/deploy/clickhouse/clickhouse-backup/config.yml
CFG_DST_TPL=$REPO_ROOT/deploy/clickhouse/clickhouse-backup/config-scratch.yml

CH_CONTAINER=${CH_CONTAINER:-exc-dev-clickhouse-1}
CH_USER=${CH_USER:-exchange}
CH_PASSWORD=${CH_PASSWORD:-exchange_dev}
CH_DATABASE=${CH_DATABASE:-exchange_analytics}
BACKUP_NAME=${BACKUP_NAME:-daily-$(date -u +%Y%m%d)}
SCRATCH_CONTAINER=${SCRATCH_CONTAINER:-exc-ch-scratch}
SCRATCH_HTTP_PORT=${SCRATCH_HTTP_PORT:-18123}
SCRATCH_NATIVE_PORT=${SCRATCH_NATIVE_PORT:-19000}
SCRATCH_VOLUME=${SCRATCH_VOLUME:-exc-ch-scratch-data}
S3_CONTAINER=${S3_CONTAINER:-exc-ch-s3gw}
S3_HOST_PORT=${S3_HOST_PORT:-17071}
S3_CTR_PORT=17070
S3_BUCKET=${S3_BUCKET:-exchange-ch-backup}
S3_VOLUME=${S3_VOLUME:-exc-ch-s3gw-data}
DRILL_WORKDIR=${DRILL_WORKDIR:-/tmp/ch-restore-drill}
RTO_SECONDS=${RTO_SECONDS:-1800}
CHB_VERSION=${CHB_VERSION:-v2.8.1}
VGW_VERSION=${VGW_VERSION:-v1.8.0}
CTR_CFG_PATH=/tmp/chb-config.yml        # in-container rendered config
CTR_CHB_PATH=/tmp/clickhouse-backup     # in-container binary

NETWORK=${NETWORK:-$(docker inspect "$CH_CONTAINER" \
    --format '{{range $k,$_ := .NetworkSettings.Networks}}{{println $k}}{{end}}' | head -1)}
SCRATCH_IMAGE=${SCRATCH_IMAGE:-$(docker inspect "$CH_CONTAINER" --format '{{.Config.Image}}')}

mkdir -p "$DRILL_WORKDIR/bin"

log() { printf '>> %s\n' "$*"; }
die() { printf '!! %s\n' "$*" >&2; exit 1; }

# --- tools -----------------------------------------------------------------

ensure_chb() {  # host-side clickhouse-backup binary
    local bin=$DRILL_WORKDIR/bin/clickhouse-backup
    [[ -n ${CHB_BIN:-} && -x $CHB_BIN ]] && { CHB_HOST_BIN=$CHB_BIN; return; }
    [[ -x $bin ]] && { CHB_HOST_BIN=$bin; return; }
    # reuse the binary already staged by an earlier drill run, else download
    # (cp-guarded, not -f: sandboxed file tests have proven flaky on /tmp)
    if cp /tmp/chb-drill/clickhouse-backup "$bin" 2>/dev/null; then
        chmod +x "$bin"; CHB_HOST_BIN=$bin; return
    fi
    log "downloading clickhouse-backup $CHB_VERSION"
    local tgz=$DRILL_WORKDIR/chb.tar.gz
    curl -fsSL -o "$tgz" \
        "https://github.com/Altinity/clickhouse-backup/releases/download/$CHB_VERSION/clickhouse-backup-linux-amd64.tar.gz"
    tar -xzf "$tgz" -C "$DRILL_WORKDIR/bin" --strip-components=2 \
        "build/linux/amd64/clickhouse-backup" 2>/dev/null \
        || tar -xzf "$tgz" -C "$DRILL_WORKDIR/bin"
    chmod +x "$bin"; CHB_HOST_BIN=$bin
}

ensure_vgw() {  # host-side versitygw binary (static -> runs in busybox ctr)
    local bin=$DRILL_WORKDIR/bin/versitygw
    [[ -n ${VGW_BIN:-} && -x $VGW_BIN ]] && { VGW_HOST_BIN=$VGW_BIN; return; }
    [[ -x $bin ]] && { VGW_HOST_BIN=$bin; return; }
    if cp "/tmp/chb-drill/versitygw_${VGW_VERSION#v}_Linux_x86_64/versitygw" "$bin" \
        2>/dev/null; then
        chmod +x "$bin"; VGW_HOST_BIN=$bin; return
    fi
    log "downloading versitygw $VGW_VERSION"
    local tgz=$DRILL_WORKDIR/vgw.tar.gz
    curl -fsSL -o "$tgz" \
        "https://github.com/versity/versitygw/releases/download/$VGW_VERSION/versitygw_${VGW_VERSION#v}_Linux_x86_64.tar.gz"
    mkdir -p "$DRILL_WORKDIR/vgw-extract" && tar -xzf "$tgz" -C "$DRILL_WORKDIR/vgw-extract"
    local found
    found=$(find "$DRILL_WORKDIR/vgw-extract" -name versitygw -type f | head -1)
    [[ -n $found ]] || die "versitygw binary not in tarball"
    cp "$found" "$bin"; chmod +x "$bin"; VGW_HOST_BIN=$bin
}

s3_creds() {
    local f=$DRILL_WORKDIR/s3.env
    if [[ -z ${S3_ACCESS:-} || -z ${S3_SECRET:-} ]]; then
        if [[ ! -f $f ]]; then
            S3_ACCESS=drill$(openssl rand -hex 4)
            S3_SECRET=$(openssl rand -hex 16)
            umask 077
            printf 'S3_ACCESS=%s\nS3_SECRET=%s\n' "$S3_ACCESS" "$S3_SECRET" > "$f"
            umask 022
            log "generated drill S3 creds -> $f (gitignore-safe: $DRILL_WORKDIR)"
        fi
        # shellcheck disable=SC1090
        . "$f"
    fi
}

# --- S3 gateway: signed requests via stdlib python (no aws-cli/mc needed) ----

s3_req() {  # s3_req <METHOD> <path> ; endpoint = container-mapped host port
    S3EP="http://127.0.0.1:${S3_HOST_PORT}" S3A="$S3_ACCESS" S3S="$S3_SECRET" \
    python3 - "$1" "$2" <<'PYEOF'
import hashlib, hmac, os, sys, urllib.request, urllib.error, datetime
from urllib.parse import urlparse
method, path = sys.argv[1], sys.argv[2]
url = os.environ["S3EP"] + path
u = urlparse(url); host = u.netloc
t = datetime.datetime.now(datetime.timezone.utc)
amz, ds = t.strftime("%Y%m%dT%H%M%SZ"), t.strftime("%Y%m%d")
ph = hashlib.sha256(b"").hexdigest()
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
req = urllib.request.Request(url, method=method, headers=hdrs)
try:
    r = urllib.request.urlopen(req, timeout=15)
    sys.stdout.write(r.read().decode()[:4000]); sys.exit(0 if r.status < 300 else 1)
except urllib.error.HTTPError as e:
    sys.stderr.write(f"s3_req {method} {path}: HTTP {e.code} {e.read().decode()[:500]}\n")
    sys.exit(1)
PYEOF
}

# --- container provisioning (idempotent) ------------------------------------

provision_s3() {
    if ! docker inspect "$S3_CONTAINER" >/dev/null 2>&1; then
        log "starting S3 gateway container $S3_CONTAINER (versitygw posix)"
        docker run -d --name "$S3_CONTAINER" --network "$NETWORK" \
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

provision_scratch() {
    if ! docker inspect "$SCRATCH_CONTAINER" >/dev/null 2>&1; then
        log "starting scratch ClickHouse $SCRATCH_CONTAINER ($SCRATCH_IMAGE)"
        docker run -d --name "$SCRATCH_CONTAINER" --network "$NETWORK" \
            -p "${SCRATCH_HTTP_PORT}:8123" -p "${SCRATCH_NATIVE_PORT}:9000" \
            -e CLICKHOUSE_USER="$CH_USER" -e CLICKHOUSE_PASSWORD="$CH_PASSWORD" \
            -v "$SCRATCH_VOLUME:/var/lib/clickhouse" \
            "$SCRATCH_IMAGE" >/dev/null
    fi
    docker start "$SCRATCH_CONTAINER" >/dev/null 2>&1 || true
    for i in $(seq 1 60); do
        docker exec "$SCRATCH_CONTAINER" wget -qO- http://127.0.0.1:8123/ping \
            2>/dev/null | grep -q Ok && break
        [[ $i == 60 ]] && die "scratch CH not healthy on :8123"
        sleep 1
    done
}

inject_chb() {  # inject_chb <container>
    docker cp "$CHB_HOST_BIN" "$1:$CTR_CHB_PATH"
    docker exec "$1" chmod +x "$CTR_CHB_PATH"
}

# ${VAR:-default}-aware template renderer. GNU envsubst does NOT understand
# the :-(default) form used throughout the committed config templates —
# it leaves them literal, which then fails clickhouse-backup's YAML parse
# (e.g. `port: ${CH_PORT:-9000}` is a str, not uint). Stdlib-only.
render_tpl() {  # render_tpl <template> <out>
    python3 - "$1" "$2" <<'PYEOF'
import os, re, sys
tpl = open(sys.argv[1]).read()
def sub(m):
    var, default = m.group(1), m.group(2)
    val = os.environ.get(var)
    return val if val not in (None, "") else (default or "")
out = re.sub(r'\$\{(\w+)(?::-(.*?))?\}', sub, tpl)
open(sys.argv[2], "w").write(out)
PYEOF
}

render_configs() {
    # Rendered for IN-CONTAINER execution: chb runs via docker exec on the CH
    # host itself, so clickhouse.host is always 127.0.0.1. Distinct rendered
    # files per side; both point at the gateway by container DNS name.
    # AWS_REGION=us-east-1: versitygw is region-locked to the region the
    # bucket was created under (us-east-1); a wrong region in SigV4 yields
    # AuthorizationHeaderMalformed. Overridable via env for real AWS drills.
    AWS_ENDPOINT_URL="http://${S3_CONTAINER}:${S3_CTR_PORT}" \
    AWS_ACCESS_KEY_ID="$S3_ACCESS" AWS_SECRET_ACCESS_KEY="$S3_SECRET" \
    AWS_REGION="${AWS_REGION:-us-east-1}" S3_BUCKET="$S3_BUCKET" \
    CH_HOST=127.0.0.1 CH_PORT=9000 CH_USER="$CH_USER" CH_PASSWORD="$CH_PASSWORD" \
        render_tpl "$CFG_SRC_TPL" "$DRILL_WORKDIR/config-src.yml"

    AWS_ENDPOINT_URL="http://${S3_CONTAINER}:${S3_CTR_PORT}" \
    AWS_ACCESS_KEY_ID="$S3_ACCESS" AWS_SECRET_ACCESS_KEY="$S3_SECRET" \
    AWS_REGION="${AWS_REGION:-us-east-1}" S3_BUCKET="$S3_BUCKET" \
    CH_SCRATCH_HOST=127.0.0.1 CH_SCRATCH_PORT=9000 \
    CH_SCRATCH_USER="$CH_USER" CH_SCRATCH_PASSWORD="$CH_PASSWORD" \
        render_tpl "$CFG_DST_TPL" "$DRILL_WORKDIR/config-dst.yml"

    docker cp "$DRILL_WORKDIR/config-src.yml" "$CH_CONTAINER:$CTR_CFG_PATH"
    docker cp "$DRILL_WORKDIR/config-dst.yml" "$SCRATCH_CONTAINER:$CTR_CFG_PATH"
}

gen_wrappers() {
    cat > "$DRILL_WORKDIR/bin/chb-src" <<EOF
#!/usr/bin/env bash
exec docker exec "$CH_CONTAINER" "$CTR_CHB_PATH" "\$@"
EOF
    cat > "$DRILL_WORKDIR/bin/chb-dst" <<EOF
#!/usr/bin/env bash
exec docker exec "$SCRATCH_CONTAINER" "$CTR_CHB_PATH" "\$@"
EOF
    cat > "$DRILL_WORKDIR/bin/chc-src" <<EOF
#!/usr/bin/env bash
exec docker exec "$CH_CONTAINER" clickhouse-client "\$@"
EOF
    cat > "$DRILL_WORKDIR/bin/chc-dst" <<EOF
#!/usr/bin/env bash
exec docker exec "$SCRATCH_CONTAINER" clickhouse-client "\$@"
EOF
    chmod +x "$DRILL_WORKDIR/bin/"{chb-src,chb-dst,chc-src,chc-dst}
}

chc_src() { docker exec "$CH_CONTAINER" clickhouse-client \
    --user "$CH_USER" --password "$CH_PASSWORD" "$@"; }
chc_dst() { docker exec "$SCRATCH_CONTAINER" clickhouse-client \
    --user "$CH_USER" --password "$CH_PASSWORD" "$@"; }

pick_partition() {  # largest active partition in the sampled database
    chc_src --query "SELECT partition_id FROM system.parts
        WHERE active AND database='$CH_DATABASE'
        GROUP BY partition_id ORDER BY sum(rows) DESC LIMIT 1
        FORMAT TabSeparated"
}

# --- drill -------------------------------------------------------------------

cmd=${1:-drill}
case "$cmd" in
cleanup)
    docker rm -f "$SCRATCH_CONTAINER" "$S3_CONTAINER" 2>/dev/null || true
    docker volume rm "$SCRATCH_VOLUME" "$S3_VOLUME" 2>/dev/null || true
    log "removed drill containers/volumes (workdir kept: $DRILL_WORKDIR)"
    exit 0 ;;
drill) ;; *) sed -n '2,55p' "$0" >&2; exit 64 ;;
esac

ensure_chb; ensure_vgw; s3_creds
log "tools: chb=$CHB_HOST_BIN ($("$CHB_HOST_BIN" --version | head -1 | tr -d '\t'))"
log "network=$NETWORK scratch_image=$SCRATCH_IMAGE backup=$BACKUP_NAME bucket=$S3_BUCKET"

provision_s3
provision_scratch
inject_chb "$CH_CONTAINER"
inject_chb "$SCRATCH_CONTAINER"
render_configs
gen_wrappers

PARTITION=${PARTITION:-$(pick_partition)}
[[ -n $PARTITION ]] || die "no active partition in $CH_DATABASE to sample"
log "sampled partition: $PARTITION"

# Clean-slate guarantees for repeatability:
#  * drop a same-named previous backup (local + remote) — backup.sh full
#    always derives daily-YYYYMMDD
#  * drop the target database on scratch so restore_remote rebuilds it
"$DRILL_WORKDIR/bin/chb-src" -c "$CTR_CFG_PATH" delete remote "$BACKUP_NAME" \
    >/dev/null 2>&1 || true
"$DRILL_WORKDIR/bin/chb-src" -c "$CTR_CFG_PATH" delete local "$BACKUP_NAME" \
    >/dev/null 2>&1 || true
chc_dst --query "DROP DATABASE IF EXISTS $CH_DATABASE SYNC" || true

log "a) source counts -> $DRILL_WORKDIR/src_counts.tsv"
CLICKHOUSE_CLIENT_BIN="$DRILL_WORKDIR/bin/chc-src" \
CH_HOST=127.0.0.1 CH_PORT=9000 CH_USER="$CH_USER" CH_PASSWORD="$CH_PASSWORD" \
CH_DATABASE="$CH_DATABASE" \
    "$BACKUP_SH" counts "$DRILL_WORKDIR/src_counts.tsv" "$PARTITION"
cat "$DRILL_WORKDIR/src_counts.tsv"

log "b) full backup $BACKUP_NAME -> s3://$S3_BUCKET/"
CLICKHOUSE_BACKUP_BIN="$DRILL_WORKDIR/bin/chb-src" \
CLICKHOUSE_BACKUP_CONFIG="$CTR_CFG_PATH" \
    "$BACKUP_SH" full

log "b) remote listing (backup.sh list)"
CLICKHOUSE_BACKUP_BIN="$DRILL_WORKDIR/bin/chb-src" \
CLICKHOUSE_BACKUP_CONFIG="$CTR_CFG_PATH" \
    "$BACKUP_SH" list

log "c) restoring $BACKUP_NAME into scratch container $SCRATCH_CONTAINER"
t0=$(date +%s%3N)
CLICKHOUSE_BACKUP_BIN="$DRILL_WORKDIR/bin/chb-dst" \
CLICKHOUSE_BACKUP_CONFIG="$CTR_CFG_PATH" \
    "$BACKUP_SH" restore "$BACKUP_NAME"
t1=$(date +%s%3N)
rto_ms=$((t1 - t0))
log "c) restore wall-clock: ${rto_ms} ms (RTO bound ${RTO_SECONDS}s = $((RTO_SECONDS*1000)) ms)"

log "d) scratch counts + verify"
CLICKHOUSE_CLIENT_BIN="$DRILL_WORKDIR/bin/chc-dst" \
CH_HOST=127.0.0.1 CH_PORT=9000 CH_USER="$CH_USER" CH_PASSWORD="$CH_PASSWORD" \
CH_DATABASE="$CH_DATABASE" \
    "$BACKUP_SH" counts "$DRILL_WORKDIR/dst_counts.tsv" "$PARTITION"

if "$BACKUP_SH" verify "$DRILL_WORKDIR/src_counts.tsv" \
        "$DRILL_WORKDIR/dst_counts.tsv"; then
    [[ $rto_ms -le $((RTO_SECONDS * 1000)) ]] \
        || die "verify OK but RTO exceeded: ${rto_ms} ms"
    log "DRILL PASS: $BACKUP_NAME restored to $SCRATCH_CONTAINER, "\
"partition $PARTITION counts match, RTO ${rto_ms} ms"
else
    die "DRILL FAIL: row-count mismatch (see verify output above)"
fi
