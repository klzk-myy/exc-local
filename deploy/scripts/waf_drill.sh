#!/usr/bin/env bash
# =============================================================================
# waf_drill.sh — Phase-09 Task 9.3.13 WAF verification (spec §19.1, §24 #160).
#
# Provisions a REAL OWASP CRS deployment — the official
# owasp/modsecurity-crs:nginx image (ModSecurity v3 + CRS), SecRuleEngine On,
# paranoia level 1 — fronting the Task 5.3.29 Go validation stub (rebuilt
# from deploy/haproxy/test/stub_server.go, mounted into a busybox container).
# It then proves the ruleset is ACTIVE, not merely loaded:
#
#   * classic CRS-covered attacks → HTTP 403 at the WAF (SQLi 942xxx,
#     XSS 941xxx, path traversal 930xxx) + rule ids captured from the
#     ModSecurity audit/error log
#   * legitimate REST GET and JSON POST → HTTP 200, body = stub color
#     (proves clean traffic is proxied THROUGH to the backend, not just
#     served by the WAF)
#   * WS upgrade (Connection/Upgrade headers, /ws/ path shape) → 101
#
# Honest scope: ModSecurity inspects the HTTP upgrade request only —
# WebSocket frames are post-upgrade tunnel bytes and are NOT inspected at
# any paranoia level. Frame-level protection stays in the gateway
# (Task 6.3.7). This drill is the origin-side WAF tier; managed L3/L4 DDoS
# remains the provider layer (deploy/cloudflare/).
#
# Env: WAF_IMAGE (default owasp/modsecurity-crs:nginx), DRILL_NET,
#      DRILL_PORT (default 28080).
# Exit: 0 all checks pass · 1 a check failed. Containers are always removed.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGE="${WAF_IMAGE:-owasp/modsecurity-crs:nginx}"
NET="${DRILL_NET:-waf-drill-net-$$}"
PORT="${DRILL_PORT:-28080}"
WAF="waf-drill-$$"
STUB="waf-drill-stub-$$"
TMPD="$(mktemp -d)"
FAILS=0

log()  { printf '[waf-drill %s] %s\n' "$(date -u +%H:%M:%S.%3N)" "$*"; }
check() {
    if [ "$2" = "PASS" ]; then log "  check $1: PASS — $3"
    else log "  check $1: FAIL — $3"; FAILS=$((FAILS+1)); fi
}
cleanup() {
    docker rm -f "$WAF" "$STUB" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
    rm -rf "$TMPD" || true
}
trap cleanup EXIT

log "== OWASP CRS WAF drill (CRS block / pass / WS-upgrade) =="

# --- backend stub: rebuild canonical source, run static binary in busybox ----
log "building stub from deploy/haproxy/test/stub_server.go"
CGO_ENABLED=0 go build -o "$TMPD/stub" "$REPO_ROOT/deploy/haproxy/test/stub_server.go"

docker network create "$NET" >/dev/null
docker run -d --name "$STUB" --network "$NET" \
    -v "$TMPD/stub:/stub:ro" busybox:latest /stub BLUE 8080 >/dev/null

# --- WAF container -------------------------------------------------------------
# Same env surface as deploy/waf/docker-compose.waf.yml. Audit log to stdout
# (JSON, RelevantOnly) so `docker logs` carries the rule ids as evidence.
log "starting WAF ($IMAGE, CRS, SecRuleEngine On, PL1)"
docker run -d --name "$WAF" --network "$NET" -p "$PORT":8080 \
    -e BACKEND="http://$STUB:8080" \
    -e PORT=8080 -e SERVER_NAME=api.exc.local \
    -e PARANOIA=1 -e ANOMALY_INBOUND=5 -e BLOCKING_PARANOIA=1 \
    -e MODSEC_RULE_ENGINE=On \
    -e MODSEC_AUDIT_ENGINE=RelevantOnly \
    -e MODSEC_AUDIT_LOG_FORMAT=JSON \
    -e MODSEC_AUDIT_LOG=/dev/stdout \
    -v "$REPO_ROOT/deploy/waf/rules/after-crs:/opt/modsecurity/rules/after-crs:ro" \
    "$IMAGE" >/dev/null

# Modal version across the rules dir — REQUEST-901 also carries legacy
# version strings, so a first-match grep can under-report (3.1.0 vs 4.29.0).
CRS_VER="$(docker exec "$WAF" sh -c \
    "grep -rhoE 'OWASP_CRS/[0-9]+\.[0-9]+\.[0-9]+' /etc/modsecurity.d/owasp-crs/rules/ \
     | sort | uniq -c | sort -rn | head -1 | awk '{print \$2}'" 2>/dev/null \
    || echo 'unknown')"
log "ruleset: $CRS_VER"

# /healthz is nginx-local (never proxied): proves the front layer is up.
READY=0
for i in $(seq 1 90); do
    curl -sf -o /dev/null "http://127.0.0.1:$PORT/healthz" && { READY=1; break; }
    sleep 1
done
[ "$READY" = "1" ] \
    && check waf_ready PASS "/healthz 200 (nginx+modsec up)" \
    || { check waf_ready FAIL "WAF never became ready"; log "== aborting =="; exit 1; }

BASE="http://127.0.0.1:$PORT"
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# --- attack payloads: must 403 -------------------------------------------------
log "firing attack probes (expect 403 at the WAF, nothing reaches backend)"
C="$(code "$BASE/api/v1/instruments?id=1%27%20OR%20%271%27%3D%271")"
[ "$C" = "403" ] \
    && check block_sqli PASS "GET ?id=1' OR '1'='1 → $C" \
    || check block_sqli FAIL "SQLi probe got $C (want 403)"

C="$(code "$BASE/api/v1/search?q=%3Cscript%3Ealert%281%29%3C%2Fscript%3E")"
[ "$C" = "403" ] \
    && check block_xss PASS "GET ?q=<script>alert(1)</script> → $C" \
    || check block_xss FAIL "XSS probe got $C (want 403)"

C="$(code "$BASE/api/v1/files?f=../../etc/passwd")"
[ "$C" = "403" ] \
    && check block_traversal PASS "GET ?f=../../etc/passwd → $C" \
    || check block_traversal FAIL "traversal probe got $C (want 403)"

# --- rule-id evidence from ModSecurity audit/error log --------------------------
sleep 1   # let the audit log flush
# Two emit formats: the nginx error log carries `id "942100"` on the
# disruptive match; the JSON audit log carries `"ruleId":"942100"` for every
# contributing rule. Collect both — detection rules (941/942/930) only
# appear in the audit log; the error log shows just the 949110 block.
IDS="$(docker logs "$WAF" 2>&1 | \
    grep -oE '(ruleId":"|\[id ")[0-9]{6}' | grep -oE '[0-9]{6}' | sort -u | tr '\n' ' ')"
log "triggered rule ids: ${IDS:-none}"
printf '%s' "$IDS" | grep -q '942' \
    && check sqli_rule_id PASS "942xxx SQLi rule family in log ($IDS)" \
    || check sqli_rule_id FAIL "no 942xxx id in log ($IDS)"
printf '%s' "$IDS" | grep -q '941' \
    && check xss_rule_id PASS "941xxx XSS rule family in log ($IDS)" \
    || check xss_rule_id FAIL "no 941xxx id in log ($IDS)"
printf '%s' "$IDS" | grep -q '930' \
    && check lfi_rule_id PASS "930xxx traversal rule family in log ($IDS)" \
    || check lfi_rule_id FAIL "no 930xxx id in log ($IDS)"
docker logs "$WAF" 2>&1 | grep -m3 'ModSecurity: Access denied' | \
    sed 's/^/    error-log: /' || true
docker logs "$WAF" 2>&1 | grep '"transaction"' | \
    grep -oE '"message":"[^"]+","details":{"match":"[^"]{0,80}' | head -6 | \
    sed 's/^/    audit-json: /' || true

# --- legitimate traffic: must 200 through to the backend -------------------------
log "firing legit probes (expect 200, proxied to stub → body=BLUE)"
C="$(code "$BASE/api/v1/instruments")"
BODY="$(curl -s "$BASE/api/v1/instruments" || true)"
if [ "$C" = "200" ] && [ "$BODY" = "BLUE" ]; then
    check pass_rest_get PASS "GET /api/v1/instruments → 200 body=$BODY"
else
    check pass_rest_get FAIL "GET → $C body='$BODY' (want 200 BLUE)"
fi

C="$(code -X POST "$BASE/api/v1/orders" \
    -H 'Content-Type: application/json' \
    -d '{"symbol":"EURUSD","side":"buy","qty":1000000,"type":"limit","price":"1.0842"}')"
[ "$C" = "200" ] \
    && check pass_rest_post PASS "POST /api/v1/orders JSON order body → $C" \
    || check pass_rest_post FAIL "JSON POST got $C (want 200)"

# --- WS upgrade path shape -------------------------------------------------------
# CRS inspects the upgrade REQUEST; frames after 101 are tunnel bytes.
C="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/ws/stream" \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
    -H 'Sec-WebSocket-Version: 13' --max-time 5 || true)"
[ "$C" = "101" ] \
    && check ws_upgrade PASS "GET /ws/stream upgrade → 101 through CRS" \
    || check ws_upgrade FAIL "WS upgrade got $C (want 101)"

log "== drill complete: $FAILS check(s) failed (ruleset $CRS_VER) =="
[ "$FAILS" -eq 0 ] && { echo "WAF-DRILL PASS"; exit 0; }
echo "WAF-DRILL FAIL ($FAILS)" >&2
exit 1
