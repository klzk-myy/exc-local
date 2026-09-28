#!/usr/bin/env bash
# ptp-status.sh — render ptp4l/phc2sys state into /run/ptp/status as
# key=value pairs; services/internal/timesync.StatsFileReader consumes it.
# Contract (readers default conservatively):
#   offset_ns=<signed int>   state=<SLAVE|s2|…>   synced=<0|1>
#   read_at=<RFC3339 UTC>
set -u

OUT="${PTP_STATUS_PATH:-/run/ptp/status}"
SOCK="${PTP_UDS:-/run/ptp4l}"
TMP="$(mktemp "${OUT}.XXXXXX")" || exit 1
trap 'rm -f "$TMP"' EXIT

mkdir -p "$(dirname "$OUT")"

offset_ns=""
state=""
gm_present=""
port_state=""

if command -v pmc >/dev/null 2>&1 && [ -S "$SOCK" ]; then
    ts_out="$(pmc -u -b 0 -s "$SOCK" 'GET TIME_STATUS_NP' 2>/dev/null || true)"
    pd_out="$(pmc -u -b 0 -s "$SOCK" 'GET PORT_DATA_SET' 2>/dev/null || true)"
    offset_ns="$(printf '%s\n' "$ts_out" | awk '/master_offset/ {print $2; exit}')"
    gm_present="$(printf '%s\n' "$ts_out" | awk '/gmPresent/ {print $2; exit}')"
    port_state="$(printf '%s\n' "$pd_out" | awk '/port_state|portState/ {print $2; exit}')"
fi

# phc2sys's servo state surfaces via journald; use ptp4l port state as
# the discipline proxy (SLAVE + GM present = synchronized).
state="${port_state:-${state:-UNKNOWN}}"
synced=0
if [ "$gm_present" = "true" ] && { [ "$state" = "SLAVE" ] || [ "$state" = "MASTER" ]; }; then
    synced=1
fi
# Fall back to adjtimex STA_UNSYNC when pmc is silent — the kernel PLL is
# the same discipline source Guard.Check() reads.
if [ -z "$offset_ns" ]; then
    offset_ns="$(cat /sys/class/ptp/ptp0/offset 2>/dev/null || echo 0)"
fi

{
    echo "offset_ns=${offset_ns:-0}"
    echo "state=${state}"
    echo "synced=${synced}"
    echo "read_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} >"$TMP"
chmod 0644 "$TMP"
mv -f "$TMP" "$OUT"
