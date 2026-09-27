#!/usr/bin/env bash
# cpp_dep_audit.sh — vendored C/C++ dependency audit (Phase-01.5 Task 1.5.3.4,
# spec §19.2 "cargo-audit-equivalent for C++ deps").
#
# Two checks, both fail-closed:
#
# 1. FetchContent pin audit — every FetchContent_Declare in core/**/*.cmake /
#    CMakeLists.txt that fetches over the network must be content-pinned:
#      * URL <archive>            → requires URL_HASH SHA256=<64-hex>
#      * GIT_REPOSITORY + GIT_TAG → tag must be a full 40-hex commit SHA
#        (mutable refs like `main` or `v1.2.3` are unpinned — a force-push to
#        the upstream tag silently changes our build inputs).
#    `find_package`/system deps are out of scope (they carry no fetch inputs).
#
# 2. CVE feed check — every entry in core/third_party/vendored-deps.txt is
#    queried against OSV (https://api.osv.org/v1/querybatch) by release-tag
#    commit SHA (GIT-ecosystem advisories — the OSV coverage that actually
#    applies to arbitrary C++ GitHub deps), plus name+version for deps that
#    set an osv_name in an OSV ecosystem. Any reported vulnerability fails
#    the audit. Known-acceptable advisories are suppressed in this script's
#    SUPPRESS_IDS list with a justification comment (keep empty; presence
#    requires a SECURITY.md-quality reason).
#
# Env:
#   MANIFEST         default core/third_party/vendored-deps.txt
#   CMAKE_FILES      space-separated CMake files to scan (default: core/CMakeLists.txt
#                    plus any core/**/*.cmake)
#   OSV_BATCH        default https://api.osv.dev/v1/querybatch
#   ALLOW_FEED_UNAVAILABLE=1  downgrade OSV-unreachable to a warning.
#                      Default strict: a dep whose CVE status cannot be checked
#                      is treated as unverified → fail (spec §2.7 fail-closed).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MANIFEST="${MANIFEST:-$REPO_ROOT/core/third_party/vendored-deps.txt}"
OSV_BATCH="${OSV_BATCH:-https://api.osv.dev/v1/querybatch}"
ALLOW_FEED_UNAVAILABLE="${ALLOW_FEED_UNAVAILABLE:-0}"
FAILURES=0

log()  { printf '%s\n' "$*"; }
fail() { log "FAIL: $*"; FAILURES=$((FAILURES + 1)); }

# --- 1. FetchContent pin audit ----------------------------------------------
log "== FetchContent pin audit =="
mapfile -t cmake_files < <(
    { printf '%s\n' "$REPO_ROOT/core/CMakeLists.txt"; \
      find "$REPO_ROOT/core" -path '*/build*' -prune -o -name '*.cmake' -print \
        2>/dev/null; } | sort -u)
# CMakeLists.txt anywhere outside build trees, not just the root one.
while IFS= read -r f; do cmake_files+=("$f"); done < <(
    find "$REPO_ROOT/core" -path '*/build*' -prune -o -name 'CMakeLists.txt' -print)

PIN_VIOLATIONS=0
DECLARED_DEPS=""
for f in $(printf '%s\n' "${cmake_files[@]}" | sort -u); do
    # Extract each FetchContent_Declare(<name> ... ) block (may span lines).
    awk '
        /FetchContent_Declare[ \t]*\(/ { inblock=1; buf=""; depth=0 }
        inblock {
            buf = buf " " $0
            n = gsub(/\(/, "(") ; m = gsub(/\)/, ")")
            depth += n - m
            if (depth <= 0) { print buf; inblock=0 }
        }' "$f" | while read -r block; do
        name="$(printf '%s' "$block" | sed -E 's/.*FetchContent_Declare[ \t]*\([ \t]*([A-Za-z0-9_.-]+).*/\1/')"
        if printf '%s' "$block" | grep -qE 'GIT_REPOSITORY'; then
            tag="$(printf '%s' "$block" | sed -nE 's/.*GIT_TAG[ \t]+([A-Za-z0-9_./-]+).*/\1/p')"
            if printf '%s' "$tag" | grep -qE '^[0-9a-f]{40}$'; then
                printf '  ok   %s: %s pinned to commit %s\n' "$f" "$name" "$tag" >&2
            else
                printf 'PIN_FAIL\t%s\n' "$f: $name GIT_TAG '$tag' is not a full 40-hex commit SHA (mutable ref)"
            fi
        elif printf '%s' "$block" | grep -qE '(^|[ \t])URL[ \t]'; then
            if printf '%s' "$block" | grep -qE 'URL_HASH[ \t]+SHA256=[0-9a-f]{64}'; then
                printf '  ok   %s: %s URL pinned with SHA256\n' "$f" "$name" >&2
            else
                printf 'PIN_FAIL\t%s\n' "$f: $name URL without URL_HASH SHA256= (unpinned fetch)"
            fi
        else
            # Declared but no fetch method (e.g. SOURCE_DIR only) — not a
            # supply-chain fetch, but record it for the manifest cross-check.
            printf '  note %s: %s declares no URL/GIT fetch (local source)\n' "$f" "$name" >&2
        fi
        printf 'DECL\t%s\n' "$name"
    done
done > /tmp/fc_audit.$$.txt

# Above loop ran in a pipeline subshell — re-read results.
while IFS=$'\t' read -r tag rest; do
    case "$tag" in
        PIN_FAIL) fail "FetchContent: $rest" ;;
        DECL)     DECLARED_DEPS="$DECLARED_DEPS $rest" ;;
    esac
done < /tmp/fc_audit.$$.txt
rm -f /tmp/fc_audit.$$.txt

# --- 2. Manifest ↔ tree cross-check ------------------------------------------
log "== vendored-deps manifest cross-check =="
declare -A MANIFESTED
while IFS=$'\t' read -r name version osv_name source _rest; do
    case "$name" in ''|\#*) continue ;; esac
    MANIFESTED[$name]="$version"
done < "$MANIFEST"

# Every vendored directory must be manifested.
for d in "$REPO_ROOT"/core/third_party/*/; do
    dir="$(basename "$d")"
    if [ -z "${MANIFESTED[$dir]:-}" ]; then
        fail "vendored dir core/third_party/$dir missing from $MANIFEST"
    else
        log "  ok   vendored $dir ${MANIFESTED[$dir]}"
    fi
done
# Every FetchContent dep must be manifested.
for dep in $DECLARED_DEPS; do
    if [ -z "${MANIFESTED[$dep]:-}" ]; then
        fail "FetchContent dep '$dep' missing from $MANIFEST"
    fi
done

# --- 3. OSV CVE feed check ----------------------------------------------------
log "== OSV vulnerability feed check =="
# Emit one OSV querybatch element per manifest row that requests one:
#   git_commit set -> {"commit": sha} — GIT-ecosystem advisories, the only OSV
#                     coverage that applies to arbitrary C++ GitHub deps
#                     (name+version queries require an OSV package ecosystem;
#                     aeron/googletest have none and the API rejects them).
#   osv_name set   -> {"version", package:{name}} — for deps that DO live in an
#                     OSV ecosystem (e.g. a vendored crate/npm package).
queries="$(while IFS=$'\t' read -r name version osv_name source git_commit _rest; do
    case "$name" in ''|\#*) continue ;; esac
    if [ -n "${git_commit:-}" ] && [ "$git_commit" != "-" ]; then
        jq -cn --arg c "$git_commit" '{commit:$c}'
    fi
    if [ "$osv_name" != "-" ]; then
        jq -cn --arg n "$osv_name" --arg v "$version" \
            '{version:$v, package:{name:$n}}'
    fi
done < "$MANIFEST")"

if [ -z "$queries" ]; then
    log "  no manifest entries to query"
else
    payload="$(printf '%s\n' "$queries" | jq -cs '{queries: .}')"
    body="/tmp/osv_body.$$.json"
    # Never use curl -f here: a 4xx means OUR query is malformed (a script/manifest
    # bug to fix loudly), while only a transport failure or 5xx is a genuinely
    # unavailable feed. `-f` conflates the two and misreports 4xx as "unreachable".
    http_code="$(curl -sSL --retry 3 --retry-all-errors --max-time 30 \
            -o "$body" -w '%{http_code}' -H 'Content-Type: application/json' \
            -d "$payload" "$OSV_BATCH")" || http_code="000"
    if [ "$http_code" = "200" ]; then
        resp="$(cat "$body")"
        # Dep names in query order (OSV returns one result element per query).
        mapfile -t dep_names < <(while IFS=$'\t' read -r name version osv_name source git_commit _rest; do
            case "$name" in ''|\#*) continue ;; esac
            [ -n "${git_commit:-}" ] && [ "$git_commit" != "-" ] && \
                printf '%s\n' "$name@${git_commit:0:12}"
            [ "$osv_name" != "-" ] && printf '%s\n' "$name ($osv_name $version)"
        done < "$MANIFEST")
        printf '%s\n' "$resp" | jq -r '
            .results // [] | to_entries[] | .key as $i |
            (.value.vulns // [])[] | "\($i)\t\(.id)"' \
            > /tmp/osv_hits.$$.txt || true
        if [ -s /tmp/osv_hits.$$.txt ]; then
            while IFS=$'\t' read -r i id; do
                fail "OSV advisory $id affects vendored C++ dep '${dep_names[$i]:-?}' (see $MANIFEST)"
            done < /tmp/osv_hits.$$.txt
        else
            count="$(printf '%s\n' "$queries" | wc -l)"
            log "  ok   $count vendored dep quer(ies), 0 advisories"
        fi
        rm -f /tmp/osv_hits.$$.txt
    elif [ "$http_code" = "000" ] || [ "${http_code:0:1}" = "5" ]; then
        if [ "$ALLOW_FEED_UNAVAILABLE" = "1" ]; then
            log "  warn OSV unreachable (HTTP $http_code) — ALLOW_FEED_UNAVAILABLE=1, skipping"
        else
            fail "OSV query failed ($OSV_BATCH unreachable, HTTP $http_code) — feed check is fail-closed; set ALLOW_FEED_UNAVAILABLE=1 to override"
        fi
    else
        fail "OSV rejected the querybatch request (HTTP $http_code) — manifest/script bug, not a feed outage; body: $(head -c 300 "$body" | tr '\n' ' ')"
    fi
    rm -f "$body"
fi

log "cpp-dep-audit: failures=$FAILURES"
[ "$FAILURES" -eq 0 ]
