#!/usr/bin/env bash
# sarif_gate.sh — fail a job when SARIF results carry HIGH+ security findings
# (Phase-01.5 Task 1.5.3.4: "CodeQL/SAST runs on every PR and fails on HIGH").
#
# Usage: sarif_gate.sh <sarif-dir-or-file> [threshold]   (threshold default 7.0)
#
# A finding counts as blocking when its rule metadata carries
# properties["security-severity"] >= threshold (CVSS 3.x HIGH band = 7.0–8.9,
# CRITICAL >= 9.0). Rules tagged with the text severities "high"/"critical"
# instead of a number are mapped to the band floor. Results whose rule has no
# security-severity metadata (pure code-quality queries) are NOT gated.
# Each SARIF file is evaluated independently — one malformed file cannot hide
# findings in its siblings (and is itself a gate failure: fail-closed).
set -euo pipefail

TARGET="${1:?usage: sarif_gate.sh <sarif-dir-or-file> [threshold]}"
THRESHOLD="${2:-7.0}"

mapfile -t files < <(
    if [ -d "$TARGET" ]; then
        find "$TARGET" -name '*.sarif' -type f
    else
        printf '%s\n' "$TARGET"
    fi)

if [ "${#files[@]}" -eq 0 ]; then
    echo "sarif-gate: no .sarif files under $TARGET"
    exit 0
fi

JQ_PROG='
    .runs[] as $run |
    (($run.tool.driver.rules // [])
      | map({
          key: .id,
          value: (
            (.properties["security-severity"] // "0") as $s |
            if   ($s | type) == "number" then $s
            elif ($s | test("^[0-9]"))    then ($s | tonumber)
            elif ($s | ascii_downcase) == "critical" then 9.0
            elif ($s | ascii_downcase) == "high"     then 7.0
            else 0 end)
        }) | from_entries) as $sev |
    ($run.results // [])[]
    | (.rule.id // .ruleId // "") as $r
    | select(($sev[$r] // 0) >= $thr)
    | [ input_filename,
        ($sev[$r] | tostring),
        $r,
        ((.locations[0].physicalLocation.artifactLocation.uri // "?")
          + ":" + ((.locations[0].physicalLocation.region.startLine // 0) | tostring)),
        (.message.text // "" | gsub("\n"; " ") | .[0:120])
      ] | @tsv'

HITS=""
for f in "${files[@]}"; do
    if out="$(jq -r --argjson thr "$THRESHOLD" "$JQ_PROG" "$f" 2>/dev/null)"; then
        [ -n "$out" ] && HITS="${HITS}${out}"$'\n'
    else
        echo "sarif-gate: FAIL — $f is not parseable SARIF (fail-closed)"
        exit 1
    fi
done

if [ -n "$HITS" ]; then
    echo "sarif-gate: HIGH/CRITICAL (>= ${THRESHOLD}) security findings:"
    printf '%s' "$HITS" | \
        awk -F'\t' '{printf "  [sev=%s] %s\n      at %s\n      %s\n      sarif: %s\n", $2, $3, $4, $5, $1}' \
        | head -60
    echo "sarif-gate: $(printf '%s' "$HITS" | grep -c '') blocking finding(s) — failing"
    exit 1
fi
echo "sarif-gate: no HIGH/CRITICAL security findings in ${#files[@]} SARIF file(s)"
