#!/usr/bin/env bash
# TEST A — health-check failover (haproxy.cfg: inter 2s, fall 3, rise 2).
# Measures: first-check-failure latency, full-DOWN latency, requests that
# 503 during the fall window (dispatched to the dying primary while
# backup stays ineligible until primary is fully DOWN), recovery time.
set -u
cd "$(dirname "$0")"
SOCK="./sock.sh"
bluestat() { $SOCK "show stat" | awk -F, '$1=="be_gateway_blue" && $2=="gw-blue-1"{print $18" ("$37")"}'; }

echo "== baseline =="
$SOCK "show stat" | awk -F, '$1=="be_gateway_blue" && NR>1{printf "%-10s status=%-10s check=%s\n",$2,$18,$37}'
curl -sk https://localhost:8443/ | sed 's|^|GET / -> |'

PID=$(pgrep -f "stub_server.py BLUE-1 18081")
T0=$(date +%s.%N)
echo "== kill BLUE-1 (pid $PID) @ $(date +%H:%M:%S.%N) =="
kill "$PID"

FIRSTFAIL=""; FULLDOWN=""; WIN_OK=0; WIN_FAIL=0
while :; do
    S=$($SOCK "show stat" | awk -F, '$1=="be_gateway_blue" && $2=="gw-blue-1"{print $18}')
    NOW=$(date +%s.%N)
    # probe a request each poll tick through the fall window
    R=$(curl -sk -o /dev/null -w "%{http_code}" https://localhost:8443/)
    if [ "$R" = 200 ]; then WIN_OK=$((WIN_OK+1)); else WIN_FAIL=$((WIN_FAIL+1)); echo "  probe @$([ "$FIRSTFAIL" ] && echo "post-fail" || echo "pre-fail") status=$S -> HTTP $R"; fi
    if [ -z "$FIRSTFAIL" ] && [ "$S" != "UP" ]; then
        FIRSTFAIL=$NOW; echo "first check failure ('$S') at +$(echo "$NOW-$T0"|bc)s"
    fi
    if [ "$S" = "DOWN" ]; then
        FULLDOWN=$NOW; echo "fully DOWN at +$(echo "$NOW-$T0"|bc)s"; break
    fi
    sleep 0.2
done
echo "fall-window probes: $WIN_OK ok, $WIN_FAIL failed"

echo "== traffic immediately after DOWN (expect BLUE-2 backup) =="
PASS=0; FAIL503=0
for i in $(seq 1 10); do
    R=$(curl -sk -o /dev/null -w "%{http_code}" https://localhost:8443/)
    [ "$R" = 200 ] && PASS=$((PASS+1)) || { FAIL503=$((FAIL503+1)); echo "  req$i -> $R"; }
done
echo "post-DOWN: $PASS ok, $FAIL503 failed"
curl -sk https://localhost:8443/ | sed 's|^|GET / -> |'

T2=$(date +%s.%N)
echo "== restart BLUE-1 @ $(date +%H:%M:%S.%N) =="
nohup python3 stub_server.py BLUE-1 18081 >>run/blue1.log 2>&1 & echo $! > run/blue1.pid
while :; do
    S=$($SOCK "show stat" | awk -F, '$1=="be_gateway_blue" && $2=="gw-blue-1"{print $18}')
    [ "$S" = "UP" ] && { T3=$(date +%s.%N); echo "UP again at +$(echo "$T3-$T2"|bc)s (rise 2 x inter 2s)"; break; }
    sleep 0.2
done
echo "== post-recovery traffic (leastconn -> primary) =="
for i in 1 2 3; do curl -sk https://localhost:8443/ | sed 's|^|GET / -> |'; done
