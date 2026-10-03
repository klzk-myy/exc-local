#!/bin/sh
# gen-nginx-headers.sh — generate nginx add_header includes from the
# canonical header contract frontend/public/_headers (Phase-10 Task
# 10.3.1 item 8). The _headers convention is Netlify/CF-Pages syntax —
# nginx cannot consume it directly, so the Docker build translates it
# instead of carrying a second, driftable copy of the same policy.
#
# Usage: gen-nginx-headers.sh <_headers-file> <output-dir>
# Emits, keyed off the path blocks:
#   /*          → <output-dir>/frontend.headers.inc   (all static locations)
#   /assets/*   → <output-dir>/frontend.assets.inc    (cache contract only —
#                                                    the /* set still applies
#                                                    via the location include)
#   /index.html → <output-dir>/frontend.shell.inc
set -eu

src=$1
out=$2
[ -f "$src" ] || { echo "gen-nginx-headers: $src not found" >&2; exit 1; }

awk -v hdr="$out/frontend.headers.inc" \
    -v assets="$out/frontend.assets.inc" \
    -v shell="$out/frontend.shell.inc" '
  /^[[:space:]]*#/ { next }
  /^[[:space:]]*$/ { next }
  /^[^[:space:]]/ {
    block = $0
    if (block == "/*")           out = hdr
    else if (block == "/assets/*")   out = assets
    else if (block == "/index.html") out = shell
    else out = ""
    next
  }
  out != "" {
    line = $0
    sub(/^[[:space:]]+/, "", line)
    pos = index(line, ":")
    key = substr(line, 1, pos - 1)
    val = substr(line, pos + 1)
    sub(/^[[:space:]]+/, "", val)
    gsub(/"/, "\\\"", val)
    printf "add_header %s \"%s\" always;\n", key, val > out
  }
' "$src"

# Every emitted file must exist (locations include them unconditionally).
for f in "$out/frontend.headers.inc" "$out/frontend.assets.inc" "$out/frontend.shell.inc"; do
  [ -s "$f" ] || { echo "gen-nginx-headers: no rules emitted for $f" >&2; exit 1; }
done
