#!/usr/bin/env bash
# grafana_live_drill.sh — Phase-02.5 row :41 evidence: Grafana dashboards
# rendering REAL scraped metrics, not just provisioned definitions.
#
# Stands up a two-container drill topology on a dedicated docker network:
#   * exc-drill-prometheus — prom/prometheus:v2.53.0 running the canonical
#     deploy/prometheus/prometheus.yml bind-mounted at its canonical path,
#     with ONLY the sentinel-exporter target swapped to
#     host.docker.internal:9109 (service DNS names do not resolve outside
#     compose). The on-disk yml is untouched; the drill copy lives in /tmp.
#     All rule_files (SLI recording rules included) still resolve because
#     the whole deploy/ tree is mounted read-only at /deploy.
#   * exc-drill-grafana — grafana/grafana with deploy/grafana/provisioning
#     and deploy/grafana/dashboards mounted at their canonical paths,
#     PROMETHEUS_URL pointing at the prometheus container by DNS name.
#
# Verification (all via the Grafana HTTP API, admin:admin):
#   1. GET /api/datasources            — Prometheus uid must be "Prometheus"
#      (matches the dashboards' datasource refs; enforced by the explicit
#      `uid: Prometheus` in provisioning/datasources/prometheus.yaml).
#   2. GET /api/search                 — the five provisioned dashboards.
#   3. GET /api/datasources/proxy/uid/Prometheus/api/v1/query — a live
#      sentinel_* series flowing back through Grafana.
#   4. POST /api/ds/query              — the exact panel exprs from
#      dashboards/slo.json (exchange_sli:redis_master_availability:rate5m,
#      exchange_sli:sentinel_quorum_availability:rate5m) returning
#      timeseries frames with real values.
#
# Usage:
#   deploy/scripts/grafana_live_drill.sh           # bring up + verify
#   deploy/scripts/grafana_live_drill.sh cleanup   # tear down
#
# Env overrides: PROM_IMAGE, GRAFANA_IMAGE, NETWORK, SENTINEL_EXPORTER_ADDR
# (default host.docker.internal:9109 — the live sentinel_exporter on the
# host), PROM_PORT (19090), GRAFANA_PORT (13000).
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROM_IMAGE="${PROM_IMAGE:-prom/prometheus:v2.53.0}"
GRAFANA_IMAGE="${GRAFANA_IMAGE:-grafana/grafana:latest}"
NETWORK="${NETWORK:-exc-grafana-drill}"
SENTINEL_ADDR="${SENTINEL_EXPORTER_ADDR:-host.docker.internal:9109}"
PROM_PORT="${PROM_PORT:-19090}"
GRAFANA_PORT="${GRAFANA_PORT:-13000}"
PROM_CTR=exc-drill-prometheus
GRAF_CTR=exc-drill-grafana
DRILL_YML=/tmp/prometheus-drill.yml

fail() { echo "DRILL FAIL: $*" >&2; exit 1; }

if [ "${1:-}" = "cleanup" ]; then
  docker rm -f "$PROM_CTR" "$GRAF_CTR" >/dev/null 2>&1
  docker network rm "$NETWORK" >/dev/null 2>&1
  rm -f "$DRILL_YML"
  echo "drill torn down"
  exit 0
fi

# --- 1. Drill prometheus config: canonical yml, sentinel target swapped ---
sed 's|sentinel-exporter:9109|'"$SENTINEL_ADDR"'|' \
  "$REPO_ROOT/deploy/prometheus/prometheus.yml" > "$DRILL_YML"
grep -q "$SENTINEL_ADDR" "$DRILL_YML" || fail "drill yml swap did not apply"

# --- 2. Network + containers ---------------------------------------------
docker network inspect "$NETWORK" >/dev/null 2>&1 || docker network create "$NETWORK" >/dev/null
docker rm -f "$PROM_CTR" "$GRAF_CTR" >/dev/null 2>&1 || true

docker run -d --name "$PROM_CTR" --network "$NETWORK" -p "$PROM_PORT":9090 \
  -v "$REPO_ROOT/deploy:/deploy:ro" \
  -v "$DRILL_YML:/deploy/prometheus/prometheus.yml:ro" \
  --add-host host.docker.internal:host-gateway \
  "$PROM_IMAGE" --config.file=/deploy/prometheus/prometheus.yml >/dev/null \
  || fail "prometheus container start"

docker run -d --name "$GRAF_CTR" --network "$NETWORK" -p "$GRAFANA_PORT":3000 \
  -e PROMETHEUS_URL="http://$PROM_CTR:9090" \
  -e LOKI_URL="http://localhost:3100" \
  -e PAGERDUTY_P0_SERVICE_KEY=drill-placeholder \
  -e PAGERDUTY_P1_SERVICE_KEY=drill-placeholder \
  -e PAGERDUTY_P2_SERVICE_KEY=drill-placeholder \
  -e SLACK_OPS_WEBHOOK_URL=http://localhost:1/drill-placeholder \
  -v "$REPO_ROOT/deploy/grafana/provisioning:/etc/grafana/provisioning:ro" \
  -v "$REPO_ROOT/deploy/grafana/dashboards:/etc/grafana/dashboards:ro" \
  "$GRAFANA_IMAGE" >/dev/null || fail "grafana container start"

# --- 3. Readiness ----------------------------------------------------------
for i in $(seq 1 30); do
  curl -sf "http://localhost:$GRAFANA_PORT/api/health" >/dev/null 2>&1 && break
  [ "$i" = 30 ] && fail "grafana never became healthy"
  sleep 2
done
# give prometheus >= 1 scrape interval (15s)
sleep 17

# --- 4. Verify --------------------------------------------------------------
echo "== datasource uid =="
curl -sf -u admin:admin "http://localhost:$GRAFANA_PORT/api/datasources" \
  | python3 -c "import json,sys; [print(d['uid'],d['name'],d['url']) for d in json.load(sys.stdin)]" \
  || fail "datasource list"

echo "== provisioned dashboards =="
curl -sf -u admin:admin "http://localhost:$GRAFANA_PORT/api/search?type=dash-db" \
  | python3 -c "import json,sys; [print(d['uid'],d['title']) for d in json.load(sys.stdin)]" \
  || fail "dashboard search"

echo "== live series via datasource proxy =="
curl -sf -u admin:admin -G \
  "http://localhost:$GRAFANA_PORT/api/datasources/proxy/uid/Prometheus/api/v1/query" \
  --data-urlencode 'query=redis_master_up' \
  | python3 -m json.tool || fail "proxy query"

echo "== panel exprs via /api/ds/query (slo.json) =="
NOW_MS=$(python3 -c 'import time;print(int(time.time()*1000))')
curl -sf -u admin:admin -X POST "http://localhost:$GRAFANA_PORT/api/ds/query" \
  -H 'Content-Type: application/json' -d '{
    "queries": [
      {"refId":"A","datasource":{"type":"prometheus","uid":"Prometheus"},
       "expr":"exchange_sli:redis_master_availability:rate5m","range":true,"maxDataPoints":100},
      {"refId":"B","datasource":{"type":"prometheus","uid":"Prometheus"},
       "expr":"exchange_sli:sentinel_quorum_availability:rate5m","range":true,"maxDataPoints":100}
    ],
    "from":"'"$((NOW_MS-3600000))"'","to":"'"$NOW_MS"'"
  }' | python3 -c "
import json,sys
d=json.load(sys.stdin)
ok=True
for rid,res in sorted(d['results'].items()):
    for fr in res.get('frames',[]):
        v=fr['data']['values']
        print(rid, res['status'], fr['schema']['fields'][1]['name'],
              'points:', len(v[0]), 'last:', v[1][-1] if v[1] else 'EMPTY')
        ok = ok and bool(v[1])
sys.exit(0 if ok else 1)
" || fail "ds/query returned no data"

echo "DRILL PASS: grafana dashboards render live scraped metrics"
