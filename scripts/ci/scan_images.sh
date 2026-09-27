#!/usr/bin/env bash
# scan_images.sh — Trivy container gate (Phase-01.5 Task 1.5.3.4, spec §19.2).
#
# Discovers every image the repo deploys locally, then scans each with:
#   trivy image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1
#
# Sources:
#   1. Every tracked Dockerfile (currently deploy/postgres/Dockerfile) is
#      docker-built locally and scanned — so findings include packages the
#      Dockerfile itself adds (e.g. postgresql-16-partman), not just the base.
#   2. `docker compose -f <file> config` resolves every service image from
#      docker-compose*.yml (anchors/extensions resolved); pullable images are
#      scanned, build-defined services are covered by (1).
#
# `--ignore-unfixed` is deliberate: findings with no published fix (Debian
# 'affected'/'fix_deferred' packages) are logged in full output but cannot be
# remediated by us — the gate trips on actionable HIGH/CRITICAL only.
# Upstream-owned findings we have reviewed and accepted are enumerated in
# .trivyignore (OS packages) and .trivyignore.yaml (path-scoped to the exact
# third-party binary, so the same CVE in OUR OWN future binaries still trips
# the gate).
#
# Env:
#   COMPOSE_FILE     default docker-compose.dev.yml ('' disables compose scan)
#   TRIVY_BIN        default trivy
#   SCAN_UNFIXED=1   include unfixed findings in the gate (audit mode)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"
COMPOSE_FILE="${COMPOSE_FILE-docker-compose.dev.yml}"
TRIVY_BIN="${TRIVY_BIN:-trivy}"
IGNORE_YAML="$REPO_ROOT/.trivyignore.yaml"
FAILURES=0
SCANNED=0

severity_args=(--severity HIGH,CRITICAL --exit-code 1 --scanners vuln)
[ -f "$IGNORE_YAML" ] && severity_args+=(--ignorefile "$IGNORE_YAML")
[ "${SCAN_UNFIXED:-0}" = "1" ] || severity_args+=(--ignore-unfixed)

scan() { # $1 = image ref, $2 = origin
    SCANNED=$((SCANNED + 1))
    echo "── trivy image $1   ($2)"
    if ! "$TRIVY_BIN" image "${severity_args[@]}" --skip-version-check "$1"; then
        echo "FAIL: $1 has blocking HIGH/CRITICAL findings"
        FAILURES=$((FAILURES + 1))
    fi
}

# --- 1. Build + scan every tracked Dockerfile --------------------------------
mapfile -t dockerfiles < <(git ls-files '*Dockerfile' 'Dockerfile' 2>/dev/null \
    | grep -v 'node_modules' || true)
for df in "${dockerfiles[@]:-}"; do
    [ -n "$df" ] || continue
    tag="scan-gate/$(dirname "$df" | tr '/.' '__'):local"
    echo "── docker build -f $df -t $tag"
    docker build --quiet -f "$df" -t "$tag" "$(dirname "$df")"
    scan "$tag" "Dockerfile $df"
done
[ "${#dockerfiles[@]}" -eq 0 ] && echo "── no Dockerfiles tracked — build step skipped"

# --- 2. Compose-resolved images ----------------------------------------------
if [ -n "$COMPOSE_FILE" ] && [ -f "$COMPOSE_FILE" ]; then
    mapfile -t pull_images < <(
        docker compose -f "$COMPOSE_FILE" config --format json 2>/dev/null \
        | jq -r '.services | to_entries[]
                 | select(.value.build == null) | .value.image' \
        | sort -u || true)
    for img in "${pull_images[@]:-}"; do
        [ -n "$img" ] || continue
        scan "$img" "$COMPOSE_FILE"
    done
    [ "${#pull_images[@]}" -eq 0 ] && \
        echo "── compose file defines no pullable images — skipped"
fi

echo "image-scan: scanned=$SCANNED failing=$FAILURES"
[ "$FAILURES" -eq 0 ]
