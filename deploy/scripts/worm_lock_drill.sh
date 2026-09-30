#!/usr/bin/env bash
# =============================================================================
# worm_lock_drill.sh — WORM Object Lock enforcement drill (Task 4.3.7,
# spec §24 row: "Parquet files stored in S3 with 5-year WORM Object Lock
# preventing deletion or modification").
#
# Provisions a REAL S3 Object Lock backend — versitygw posix (the same
# gateway binary already used for ClickHouse backups by ch_restore_drill.sh)
# in a busybox container — then proves, over signed S3 API calls, that the
# COMPLIANCE retention the archiver (services/internal/archiver) uploads
# with is server-side enforced, not merely stored metadata:
#
#   a. bucket created with x-amz-bucket-object-lock-enabled
#   b. PUT object with COMPLIANCE + retain-until -> HEAD echoes lock headers
#   c. DELETE locked object           -> 403 AccessDenied (expected)
#   d. PUT overwrite locked object    -> 403 AccessDenied (expected)
#   e. PUT GOVERNANCE + DELETE        -> 403 AccessDenied w/o bypass header
#   f. PUT unlocked object + DELETE   -> 204 (unlocked deletes fine)
#   g. PUT short-retention (4s)       -> DELETE denied before expiry,
#                                        DELETE allowed after expiry
#   h. go test TestWORMGatewayEnforcement — the committed objectstore.Client
#      (the archiver's own seam) against the live gateway
#
# Env: S3_CONTAINER (exc-worm-s3gw-$$), S3_HOST_PORT (17073),
#      VGW_VERSION (v1.8.0), VGW_BIN (pre-fetched binary override),
#      DRILL_WORKDIR (/tmp/worm-lock-drill), SHORT_RETAIN_SECONDS (4).
# Exit: 0 all checks pass - 1 a check failed. Container+volume always removed.
# =============================================================================
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)

S3_CONTAINER=${S3_CONTAINER:-exc-worm-s3gw-$$}
S3_VOLUME=${S3_VOLUME:-exc-worm-s3gw-data-$$}
S3_HOST_PORT=${S3_HOST_PORT:-17073}
S3_CTR_PORT=17070
S3_BUCKET=${S3_BUCKET:-worm-drill}
DRILL_WORKDIR=${DRILL_WORKDIR:-/tmp/worm-lock-drill}
VGW_VERSION=${VGW_VERSION:-v1.8.0}
SHORT_RETAIN_SECONDS=${SHORT_RETAIN_SECONDS:-4}
FAILS=0

log()  { printf '[worm-drill %s] %s\n' "$(date -u +%H:%M:%S.%3N)" "$*"; }
die()  { printf '!! %s\n' "$*" >&2; exit 1; }
check() {
    if [ "$2" = "PASS" ]; then log "  check $1: PASS - $3"
    else log "  check $1: FAIL - $3"; FAILS=$((FAILS+1)); fi
}
cleanup() {
    docker rm -f "$S3_CONTAINER" >/dev/null 2>&1 || true
    docker volume rm "$S3_VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "== WORM object-lock enforcement drill (versitygw posix) =="

mkdir -p "$DRILL_WORKDIR/bin"

# --- tools ------------------------------------------------------------------

ensure_vgw() {  # host-side versitygw binary (static -> runs in busybox ctr)
    local bin=$DRILL_WORKDIR/bin/versitygw
    [[ -n ${VGW_BIN:-} && -x $VGW_BIN ]] && { VGW_HOST_BIN=$VGW_BIN; return; }
    [[ -x $bin ]] && { VGW_HOST_BIN=$bin; return; }
    # reuse binaries staged by earlier drills, else download
    for cand in \
        "/tmp/ch-restore-drill/bin/versitygw" \
        "/tmp/chb-drill/versitygw_${VGW_VERSION#v}_Linux_x86_64/versitygw" \
        "/tmp/chb-drill/versitygw_${VGW_VERSION}_Linux_x86_64/versitygw"; do
        if cp "$cand" "$bin" 2>/dev/null; then
            chmod +x "$bin"; VGW_HOST_BIN=$bin; return
        fi
    done
    log "downloading versitygw $VGW_VERSION"
    local tgz=$DRILL_WORKDIR/vgw.tar.gz
    # release assets were renamed: try both 'versitygw_v1.8.0_...' (current)
    # and 'versitygw_1.8.0_...' (older) archive names
    curl -fsSL -o "$tgz" \
        "https://github.com/versity/versitygw/releases/download/$VGW_VERSION/versitygw_${VGW_VERSION}_Linux_x86_64.tar.gz" \
        || curl -fsSL -o "$tgz" \
        "https://github.com/versity/versitygw/releases/download/$VGW_VERSION/versitygw_${VGW_VERSION#v}_Linux_x86_64.tar.gz" \
        || die "versitygw download failed"
    mkdir -p "$DRILL_WORKDIR/vgw-extract" && tar -xzf "$tgz" -C "$DRILL_WORKDIR/vgw-extract"
    local found
    found=$(find "$DRILL_WORKDIR/vgw-extract" -name versitygw -type f | head -1)
    [[ -n $found ]] || die "versitygw binary not in tarball"
    cp "$found" "$bin"; chmod +x "$bin"; VGW_HOST_BIN=$bin
}

# --- S3 gateway: signed requests via stdlib python ---------------------------

write_signer() {
    cat > "$DRILL_WORKDIR/s3sign.py" <<'PYEOF'
import hashlib, hmac, os, sys, urllib.request, urllib.error, datetime
from urllib.parse import urlparse, parse_qsl, quote
method, path = sys.argv[1], sys.argv[2]
body = sys.argv[3].encode() if len(sys.argv) > 3 else b""
extra = {}
for a in sys.argv[4:]:
    k, v = a.split("=", 1); extra[k.lower()] = v
url = os.environ["S3EP"] + path
u = urlparse(url); host = u.netloc
t = datetime.datetime.now(datetime.timezone.utc)
amz, ds = t.strftime("%Y%m%dT%H%M%SZ"), t.strftime("%Y%m%d")
ph = hashlib.sha256(body).hexdigest()
hdrs = {"host": host, "x-amz-date": amz, "x-amz-content-sha256": ph}
hdrs.update(extra)
signed = ";".join(sorted(hdrs))
q = sorted((k, v) for k, v in parse_qsl(u.query, keep_blank_values=True))
canon_q = "&".join(f"{quote(k, safe='')}={quote(v, safe='')}" for k, v in q)
canon = "\n".join([method, u.path or "/", canon_q,
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
req = urllib.request.Request(url, method=method, headers=hdrs, data=body or None)
try:
    r = urllib.request.urlopen(req, timeout=15)
    print(f"HTTP {r.status}")
    for h, v in r.headers.items():
        if h.lower().startswith(("x-amz", "etag")):
            print(f"  {h}: {v}")
    sys.stdout.write(r.read().decode()[:2000] + "\n")
    sys.exit(0)
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}")
    sys.stdout.write(e.read().decode()[:2000] + "\n")
    sys.exit(1)
PYEOF
}

s3_req() {  # s3_req <METHOD> <path> [body] [hdr=v ...] -> stdout, rc
    S3EP="http://127.0.0.1:${S3_HOST_PORT}" S3A="$S3_ACCESS" S3S="$S3_SECRET" \
        python3 "$DRILL_WORKDIR/s3sign.py" "$@"
}

retain_until() {  # retain_until <seconds-from-now> -> RFC3339 UTC
    date -u -d "+$1 seconds" +%Y-%m-%dT%H:%M:%SZ
}

# --- provision (fresh container every run; trap cleans up) -------------------

S3_ACCESS="worm$(openssl rand -hex 4)"
S3_SECRET=$(openssl rand -hex 16)

ensure_vgw; write_signer
log "tools: vgw=$VGW_HOST_BIN ($("$VGW_HOST_BIN" --version | head -1))"

docker rm -f "$S3_CONTAINER" >/dev/null 2>&1 || true
docker volume rm "$S3_VOLUME" >/dev/null 2>&1 || true
log "starting S3 gateway $S3_CONTAINER (versitygw posix, :$S3_HOST_PORT)"
docker run -d --name "$S3_CONTAINER" \
    -p "${S3_HOST_PORT}:${S3_CTR_PORT}" \
    -v "$VGW_HOST_BIN:/usr/local/bin/versitygw:ro" \
    -v "$S3_VOLUME:/data" \
    busybox /usr/local/bin/versitygw \
        --port "0.0.0.0:${S3_CTR_PORT}" \
        --access "$S3_ACCESS" --secret "$S3_SECRET" \
        posix /data >/dev/null
for i in $(seq 1 30); do
    curl -s -o /dev/null "http://127.0.0.1:${S3_HOST_PORT}/" && break
    [[ $i == 30 ]] && die "s3 gateway not listening on :$S3_HOST_PORT"
    sleep 1
done

# --- a) bucket with Object Lock enabled -------------------------------------

if s3_req PUT "/${S3_BUCKET}" "" "x-amz-bucket-object-lock-enabled=true" >/dev/null; then
    check bucket_lock_enabled PASS "PUT /$S3_BUCKET +x-amz-bucket-object-lock-enabled accepted"
else
    check bucket_lock_enabled FAIL "bucket create w/ object-lock flag rejected"
fi

# --- b) PUT COMPLIANCE-locked object; HEAD must echo lock state -------------

LOCK_RET=$(retain_until 3600)
if s3_req PUT "/${S3_BUCKET}/locked.txt" "worm-payload" \
        "x-amz-object-lock-mode=COMPLIANCE" \
        "x-amz-object-lock-retain-until-date=$LOCK_RET" >/dev/null; then
    check put_compliance PASS "PUT locked.txt COMPLIANCE until=$LOCK_RET"
else
    check put_compliance FAIL "locked PUT rejected"
fi
HEAD_OUT=$(s3_req HEAD "/${S3_BUCKET}/locked.txt" 2>/dev/null || true)
if printf '%s' "$HEAD_OUT" | grep -qi 'x-amz-object-lock-mode: COMPLIANCE' \
   && printf '%s' "$HEAD_OUT" | grep -qi 'x-amz-object-lock-retain-until-date:'; then
    check head_echoes_lock PASS "HEAD surfaces mode=COMPLIANCE + retain-until"
else
    check head_echoes_lock FAIL "HEAD missing lock headers: $HEAD_OUT"
fi

# --- c) DELETE locked object must be denied ----------------------------------

DEL_OUT=$(s3_req DELETE "/${S3_BUCKET}/locked.txt" 2>&1 || true)
if printf '%s' "$DEL_OUT" | grep -q 'HTTP 403' \
   && printf '%s' "$DEL_OUT" | grep -qi 'object lock'; then
    check delete_locked_denied PASS "DELETE -> 403 AccessDenied (object lock)"
else
    check delete_locked_denied FAIL "DELETE outcome: $DEL_OUT"
fi

# --- d) PUT overwrite of locked object must be denied ------------------------

OW_OUT=$(s3_req PUT "/${S3_BUCKET}/locked.txt" "hijack" 2>&1 || true)
if printf '%s' "$OW_OUT" | grep -q 'HTTP 403' \
   && printf '%s' "$OW_OUT" | grep -qi 'object lock'; then
    check overwrite_locked_denied PASS "overwrite PUT -> 403 AccessDenied"
else
    check overwrite_locked_denied FAIL "overwrite outcome: $OW_OUT"
fi

# --- e) GOVERNANCE lock also denies delete (no bypass header) ----------------

GOV_RET=$(retain_until 3600)
s3_req PUT "/${S3_BUCKET}/gov.txt" "g" \
    "x-amz-object-lock-mode=GOVERNANCE" \
    "x-amz-object-lock-retain-until-date=$GOV_RET" >/dev/null
GDEL_OUT=$(s3_req DELETE "/${S3_BUCKET}/gov.txt" 2>&1 || true)
if printf '%s' "$GDEL_OUT" | grep -q 'HTTP 403'; then
    check governance_denied PASS "GOVERNANCE delete -> 403 without bypass"
else
    check governance_denied FAIL "GOVERNANCE delete outcome: $GDEL_OUT"
fi

# --- f) unlocked object deletes freely (control) ------------------------------

s3_req PUT "/${S3_BUCKET}/tmp.txt" "x" >/dev/null
if s3_req DELETE "/${S3_BUCKET}/tmp.txt" 2>&1 | grep -q 'HTTP 204'; then
    check unlocked_delete PASS "unlocked object DELETE -> 204 (control)"
else
    check unlocked_delete FAIL "unlocked delete did not return 204"
fi

# --- g) retention expiry releases the lock -----------------------------------

SHORT_RET=$(retain_until "$SHORT_RETAIN_SECONDS")
s3_req PUT "/${S3_BUCKET}/short.txt" "s" \
    "x-amz-object-lock-mode=COMPLIANCE" \
    "x-amz-object-lock-retain-until-date=$SHORT_RET" >/dev/null
PRE_OUT=$(s3_req DELETE "/${S3_BUCKET}/short.txt" 2>&1 || true)
if printf '%s' "$PRE_OUT" | grep -q 'HTTP 403'; then
    check expiry_pre_delete_denied PASS "pre-expiry DELETE -> 403"
else
    check expiry_pre_delete_denied FAIL "pre-expiry DELETE outcome: $PRE_OUT"
fi
log "  waiting ${SHORT_RETAIN_SECONDS}s+2 for retention expiry"
sleep $((SHORT_RETAIN_SECONDS + 2))
POST_OUT=$(s3_req DELETE "/${S3_BUCKET}/short.txt" 2>&1 || true)
if printf '%s' "$POST_OUT" | grep -q 'HTTP 204'; then
    check expiry_post_delete_allowed PASS "post-expiry DELETE -> 204"
else
    check expiry_post_delete_allowed FAIL "post-expiry DELETE outcome: $POST_OUT"
fi

# --- h) committed client seam against the live gateway ------------------------

if command -v go >/dev/null 2>&1 && [[ -d $REPO_ROOT/services ]]; then
    IT_BUCKET="${S3_BUCKET}-it"
    s3_req PUT "/${IT_BUCKET}" "" "x-amz-bucket-object-lock-enabled=true" >/dev/null
    log "h) go test objectstore.Client -> versitygw (TestWORMGatewayEnforcement)"
    if (cd "$REPO_ROOT/services" && \
        EXC_WORM_ENDPOINT="http://127.0.0.1:${S3_HOST_PORT}" \
        EXC_WORM_BUCKET="$IT_BUCKET" \
        EXC_WORM_ACCESS="$S3_ACCESS" EXC_WORM_SECRET="$S3_SECRET" \
        go test ./internal/objectstore -run TestWORMGatewayEnforcement -count=1 -v) \
            >"$DRILL_WORKDIR/go-it.log" 2>&1; then
        check client_seam PASS "objectstore.Client Put COMPLIANCE enforced by gateway (go-it.log)"
    else
        tail -20 "$DRILL_WORKDIR/go-it.log" >&2 || true
        check client_seam FAIL "go test failed — see $DRILL_WORKDIR/go-it.log"
    fi
else
    log "h) go toolchain absent — skipping client-seam check"
fi

log "== drill complete: $FAILS check(s) failed =="
[ "$FAILS" -eq 0 ] && { echo "WORM-LOCK-DRILL PASS"; exit 0; }
echo "WORM-LOCK-DRILL FAIL ($FAILS)" >&2
exit 1
