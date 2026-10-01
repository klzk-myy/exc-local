#!/usr/bin/env bash
# =============================================================================
# pagerduty_egress_drill.sh — PagerDuty delivery-path drill
# (Phase-07 AC "PagerDuty alerting configured" / Phase-09 on-call rows:
# proves the full alert -> Alertmanager -> PagerDuty-Events-API egress path
# end-to-end against a real Alertmanager process, with the PD Events API
# endpoint pointed at a recording mock — no PD account required).
#
# What it proves:
#   a. deploy/prometheus/alertmanager.yml LOADS on a real Alertmanager
#      (amtool check-config on the env-rendered production file).
#   b. severity=p0 routes to the pagerduty-p0 receiver and produces a PD
#      "trigger" event carrying the p0 service key + severity=critical.
#   c. severity=p2 routes to pagerduty-p2-ticket -> PD trigger with
#      severity=warning after its 15s group window.
#   d. alert resolve -> PD "resolve" event on the same dedup/incident key.
#   e. PagerDuty Events-API response is consumed (AM logs no notifier error).
#
# The mock answers with the canonical PD {"status":"success"} body so the
# notifier's success path is exercised exactly as against events.pagerduty.com.
# Remaining env-bound leg: acceptance by PD's real API + on-call delivery.
#
# Env: AM_IMAGE (prom/alertmanager:v0.26.0), AM_CTR (exc-pd-am-$$),
#      MOCK_PORT (17393), DRILL_WORKDIR (/tmp/pd-egress-drill-$$).
# Exit: 0 all checks pass - 1 a check failed. Container+mock always removed.
# =============================================================================
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)

AM_IMAGE=${AM_IMAGE:-prom/alertmanager:v0.26.0}
AM_CTR=${AM_CTR:-exc-pd-am-$$}
MOCK_PORT=${MOCK_PORT:-17393}
DRILL_WORKDIR=${DRILL_WORKDIR:-/tmp/pd-egress-drill-$$}
MOCK_LOG="$DRILL_WORKDIR/received.jsonl"
MOCK_PID=""
FAILS=0

log()   { printf '[pd-drill %s] %s\n' "$(date -u +%H:%M:%S.%3N)" "$*"; }
check() {
    if [ "$2" = "PASS" ]; then log "  check $1: PASS - $3"
    else log "  check $1: FAIL - $3"; FAILS=$((FAILS+1)); fi
}
cleanup() {
    docker rm -f "$AM_CTR" >/dev/null 2>&1 || true
    [ -n "$MOCK_PID" ] && kill "$MOCK_PID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$DRILL_WORKDIR"
log "== PagerDuty egress-path drill (Alertmanager -> PD Events API mock) =="

# --- 1. Recording mock for the PagerDuty Events API --------------------------
cat > "$DRILL_WORKDIR/mock_pd.py" <<'PY'
import http.server, json, sys
log_path = sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(n).decode('utf-8', 'replace')
        with open(log_path, 'a') as f:
            f.write(json.dumps({"path": self.path, "body": body}) + "\n")
        self.send_response(200)
        self.end_headers()
        if '/slack' in self.path:
            # Slack incoming webhooks answer plain "ok" (what AM expects)
            self.wfile.write(b'ok')
        else:
            # PagerDuty Events API v2 success body
            self.wfile.write(b'{"status":"success","message":"Event processed","dedup_key":"drill"}')
    def log_message(self, *a): pass
http.server.HTTPServer(('0.0.0.0', int(sys.argv[1])), H).serve_forever()
PY

python3 "$DRILL_WORKDIR/mock_pd.py" "$MOCK_PORT" "$MOCK_LOG" &
MOCK_PID=$!
sleep 0.5
curl -sf -o /dev/null -X POST "http://127.0.0.1:$MOCK_PORT/v2/enqueue" -d '{}' \
    || { log "mock not listening"; exit 1; }
: > "$MOCK_LOG"

# --- 2. Render the production alertmanager.yml for the drill ----------------
# Production secrets become drill placeholders; pagerduty_configs get a url
# override to the mock; slack api_urls point at the mock too (both receivers
# exercise the same egress proof; slack delivery verified alongside).
export PAGERDUTY_P0_SERVICE_KEY=drill-key-p0 \
       PAGERDUTY_P1_SERVICE_KEY=drill-key-p1 \
       PAGERDUTY_P2_SERVICE_KEY=drill-key-p2 \
       PAGERDUTY_P3_SERVICE_KEY=drill-key-p3 \
       SLACK_OPS_WEBHOOK_URL="http://host.docker.internal:$MOCK_PORT/slack"
envsubst < "$REPO_ROOT/deploy/prometheus/alertmanager.yml" > "$DRILL_WORKDIR/am.yml"
# Add a `url:` sibling to each routing_key inside pagerduty_configs — the field
# AM honours as the Events API endpoint substitute.
python3 - "$DRILL_WORKDIR/am.yml" "$MOCK_PORT" <<'PY'
import sys, re
path, port = sys.argv[1], sys.argv[2]
src = open(path).read()
src = re.sub(r'(pagerduty_configs:\s*\n\s*- )routing_key: "([^"]*)"',
             lambda m: (m.group(1) + 'routing_key: "' + m.group(2) + '"\n'
                        + '        url: "http://host.docker.internal:' + port + '"'),
             src)
open(path, 'w').write(src)
PY
[ "$(grep -cE '^\s+url: "http://host.docker.internal' "$DRILL_WORKDIR/am.yml")" -eq 4 ] \
    || { log "url override insertion failed"; exit 1; }

# --- 3. Config validation + container ----------------------------------------
docker rm -f "$AM_CTR" >/dev/null 2>&1 || true
docker run --rm --entrypoint amtool \
    -v "$DRILL_WORKDIR/am.yml:/am.yml:ro" \
    "$AM_IMAGE" check-config /am.yml >/dev/null \
    && check "config-validates" "PASS" "amtool check-config SUCCESS" \
    || check "config-validates" "FAIL" "amtool rejected rendered alertmanager.yml"

docker run -d --name "$AM_CTR" \
    -p 19093:9093 \
    --add-host host.docker.internal:host-gateway \
    -v "$DRILL_WORKDIR/am.yml:/etc/alertmanager/alertmanager.yml:ro" \
    "$AM_IMAGE" >/dev/null
for i in $(seq 1 20); do
    curl -sf "http://127.0.0.1:19093/-/ready" >/dev/null 2>&1 && break
    [ "$i" = 20 ] && { log "alertmanager never ready"; exit 1; }
    sleep 0.5
done
log "alertmanager up on :19093"

# --- 4. Fire a P0 alert (group_wait 0s -> immediate page) --------------------
curl -sf -X POST http://127.0.0.1:19093/api/v2/alerts -H 'Content-Type: application/json' -d '[{
  "labels": {"alertname":"PDDrillP0","service":"core","shard":"0","severity":"p0"},
  "annotations": {"summary":"drill p0"},
  "startsAt": "'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'",
  "endsAt": "2999-01-01T00:00:00Z"
}]' >/dev/null

sleep 4   # group_wait 0s + notification latency

# --- 5. Fire a P2 alert (15s group_wait) -------------------------------------
curl -sf -X POST http://127.0.0.1:19093/api/v2/alerts -H 'Content-Type: application/json' -d '[{
  "labels": {"alertname":"PDDrillP2","service":"gateway","shard":"0","severity":"p2"},
  "annotations": {"summary":"drill p2", "description":"drill p2 detail"},
  "startsAt": "'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'",
  "endsAt": "2999-01-01T00:00:00Z"
}]' >/dev/null

log "waiting 20s for p2 group window + delivery"
sleep 20

# --- 6. Resolve the P0 -------------------------------------------------------
curl -sf -X POST http://127.0.0.1:19093/api/v2/alerts -H 'Content-Type: application/json' -d '[{
  "labels": {"alertname":"PDDrillP0","service":"core","shard":"0","severity":"p0"},
  "annotations": {"summary":"drill p0"},
  "startsAt": "'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'",
  "endsAt": "'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'"
}]' >/dev/null
# Resolved notifications flush on the next group window — allow > group_interval.
log "waiting 65s for resolve flush (group_interval=1m)"
sleep 65

# --- 7. Assertions ------------------------------------------------------------
log "== recorded deliveries =="
cat "$MOCK_LOG" | python3 -c 'import json,sys
for l in sys.stdin:
    r = json.loads(l)
    try:
        b = json.loads(r["body"])
        kind = b.get("event_action") or "?"
        key = b.get("routing_key") or "?"
        print(r["path"], "| action:", kind, "| key:", key, "| sev:", b.get("severity", b.get("payload",{}).get("severity","-")))
    except Exception:
        print(r["path"], "| non-json:", r["body"][:80])'

# Emit one verdict line per check into verdicts.txt, then evaluate.
python3 - "$MOCK_LOG" "$DRILL_WORKDIR/verdicts.txt" <<'PY'
import json, sys
evs, slack = [], 0
for l in open(sys.argv[1]):
    r = json.loads(l)
    if "/slack" in r["path"]: slack += 1; continue
    try: evs.append(json.loads(r["body"]))
    except Exception: pass
def has(action, key, sev=None):
    return any(e.get("event_action")==action and e.get("routing_key")==key
               and (sev is None or e.get("payload",{}).get("severity")==sev)
               for e in evs)
out = {
  "P0_TRIG": has("trigger", "drill-key-p0", "critical"),
  "P2_TRIG": has("trigger", "drill-key-p2", "warning"),
  "P0_RES":  has("resolve", "drill-key-p0"),
  "SLACK":   slack > 0,
}
open(sys.argv[2], 'w').write(' '.join(f'{k}={int(v)}' for k,v in out.items()))
print(' '.join(f'{k}={int(v)}' for k,v in out.items()))
PY
V=$(cat "$DRILL_WORKDIR/verdicts.txt")
echo "$V" | grep -q 'P0_TRIG=1' && check "pd-trigger-p0" "PASS" "p0 -> pagerduty-p0 trigger (critical, drill-key-p0)" \
                                || check "pd-trigger-p0" "FAIL" "no p0 trigger event recorded"
echo "$V" | grep -q 'P2_TRIG=1' && check "pd-trigger-p2" "PASS" "p2 -> pagerduty-p2-ticket trigger (warning)" \
                                || check "pd-trigger-p2" "FAIL" "no p2 trigger event recorded"
echo "$V" | grep -q 'P0_RES=1'  && check "pd-resolve-p0" "PASS" "resolve event on p0 key" \
                                || check "pd-resolve-p0" "FAIL" "no resolve event recorded"
echo "$V" | grep -q 'SLACK=1'   && check "slack-egress" "PASS" "slack webhook delivery recorded" \
                                || check "slack-egress" "FAIL" "no slack delivery recorded"

# --- 8. AM-side error scan ----------------------------------------------------
if docker logs "$AM_CTR" 2>&1 | grep -i 'notify.*failed\|error.*pagerduty\|level=error' | grep -v 'api_url.*slack.*localhost:1'; then
    check "am-clean" "FAIL" "notifier errors in alertmanager log"
else
    check "am-clean" "PASS" "no PD notifier errors in alertmanager log"
fi

echo
if [ "$FAILS" -eq 0 ]; then
    log "== DRILL PASS — PagerDuty egress path verified end-to-end to a PD-Events-API endpoint =="
    exit 0
else
    log "== DRILL FAIL — $FAILS check(s) failed =="
    exit 1
fi
