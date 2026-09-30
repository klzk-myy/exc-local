#!/usr/bin/env bash
# TEST B — blue/green zero-drop flip under sustained load.
# 500 sequential requests through https_in; map flipped blue->green at
# req 250 and green->blue at req 400 via the admin socket `set map`
# (the documented prod mechanism — no reload). Criterion: 0 failures.
# Paced ~55ms/req to stay under the flood ladder (500 req/10s).
set -u
cd "$(dirname "$0")"
SOCK="./sock.sh"
OUT=results/test_b_requests.tsv; : > "$OUT"

# make sure we start blue
$SOCK "set map /etc/haproxy/active_color.map gateway blue" >/dev/null
echo "map at start: $($SOCK 'show map /etc/haproxy/active_color.map' | tr '\n' ' ')"

TOTAL=500
FLIP1=250; FLIP2=400
i=0
while [ $i -lt $TOTAL ]; do
    i=$((i+1))
    [ $i -eq $FLIP1 ] && { echo "== req$i: set map gateway green =="; $SOCK "set map /etc/haproxy/active_color.map gateway green"; }
    [ $i -eq $FLIP2 ] && { echo "== req$i: set map gateway blue  =="; $SOCK "set map /etc/haproxy/active_color.map gateway blue"; }
    CODE=$(curl -sk -o /tmp/hxb_body.$$ -w "%{http_code}" https://localhost:8443/)
    BODY=$(cat /tmp/hxb_body.$$ | tr -d '\n')
    printf "%d\t%s\t%s\n" "$i" "$CODE" "$BODY" >> "$OUT"
    sleep 0.055
done
rm -f /tmp/hxb_body.$$

echo "== summary =="
awk -F'\t' '{c[$2]++; if($3!="") b[$3]++} END{for(k in c) print "HTTP "k": "c[k]; for(k in b) print "body "k": "b[k]}' "$OUT"
echo "-- responses around flips --"
awk -F'\t' -v a=$((FLIP1-3)) -v b=$((FLIP1+3)) -v c=$((FLIP2-2)) -v d=$((FLIP2+3)) \
    '$1>=a&&$1<=b||$1>=c&&$1<=d' "$OUT"
awk -F'\t' '$2!=200{f++} END{print "TOTAL FAILED:", f+0}' "$OUT"
