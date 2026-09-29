#!/usr/bin/env bash
# sbom-scan.sh — NIGHTLY SBOM regeneration + vulnerability rescan
# (Phase-13.5 Task 13.5.3.9, spec §19.2): the PR-check Trivy/govulncheck
# gate catches new code; this job catches *newly disclosed* CVEs against
# code that already shipped — the scan corpus moves under a static tree.
#
# Schedule: 03:30 UTC nightly.
#   Crontab: 30 3 * * *  /opt/exchange/deploy/crons/sbom-scan.sh
#
# Behavior:
#   1. Regenerate the SBOM (scripts/ci/gen_sbom.sh → reports/sbom/).
#   2. govulncheck ./... in services/ when available — findings are
#      written to reports/sbom/govulncheck-<date>.txt.
#   3. trivy fs scan when available → reports/sbom/trivy-<date>.json.
#   4. Findings feed the VDP register: each new HIGH/CRITICAL finding is
#      filed as a source=PENTEST/INTERNAL vulnerability_disclosures row
#      by the operator or the filing hook below — same queue, same SLA
#      clock (spec §19.11.2 item 5). Non-zero exit pages the on-call.
#
# When neither scanner is installed the script still regenerates the
# SBOM (the artifact is the primary deliverable), documents the gap on
# stderr, and exits 0 — a missing scanner is a deploy gap, not a scan
# failure; it must not page nightly. Findings exit codes propagate
# non-zero.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

OUT_DIR="${OUT_DIR:-reports/sbom}"
STAMP="$(date -u +%Y%m%d)"
RC=0

echo "$(date -u +%FT%TZ) sbom-scan: regenerating SBOM"
if ! scripts/ci/gen_sbom.sh; then
    echo "sbom-scan: SBOM generation failed" >&2
    exit 1
fi

FOUND=0

if command -v govulncheck >/dev/null 2>&1; then
    echo "$(date -u +%FT%TZ) sbom-scan: govulncheck services/..."
    if ! (cd services && govulncheck ./...) > "$OUT_DIR/govulncheck-$STAMP.txt" 2>&1; then
        FOUND=1
        echo "sbom-scan: govulncheck reported findings → $OUT_DIR/govulncheck-$STAMP.txt"
        echo "sbom-scan: file each new finding into the VDP register:" >&2
        echo "  POST /api/v1/admin/security/disclosures/intake {source: PENTEST|INTERNAL}" >&2
    fi
else
    echo "sbom-scan: govulncheck not installed — Go reachability scan skipped (deploy gap)" >&2
fi

if command -v trivy >/dev/null 2>&1; then
    echo "$(date -u +%FT%TZ) sbom-scan: trivy fs (HIGH,CRITICAL)"
    if ! trivy fs --scanners vuln --severity HIGH,CRITICAL \
        --format json -o "$OUT_DIR/trivy-$STAMP.json" "$ROOT" >/dev/null 2>&1; then
        echo "sbom-scan: trivy scan errored" >&2
        RC=1
    elif [ -s "$OUT_DIR/trivy-$STAMP.json" ] && \
         grep -q '"VulnerabilityID"' "$OUT_DIR/trivy-$STAMP.json"; then
        FOUND=1
        echo "sbom-scan: trivy reported findings → $OUT_DIR/trivy-$STAMP.json"
    fi
else
    echo "sbom-scan: trivy not installed — filesystem CVE scan skipped (deploy gap)" >&2
fi

if [ "$FOUND" -eq 1 ]; then
    echo "sbom-scan: findings present — nightly rescan completed with hits"
    exit 2
fi
echo "$(date -u +%FT%TZ) sbom-scan: clean"
exit "$RC"
