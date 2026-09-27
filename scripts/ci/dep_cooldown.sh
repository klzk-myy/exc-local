#!/usr/bin/env bash
# dep_cooldown.sh — supply-chain publish-age gate (Phase-01.5 Task 1.5.3.4, spec §19.2).
#
# POLICY: every dependency version newly introduced by a change must have been
# published to the public registry at least DEP_COOLDOWN_DAYS (default 7) days
# before merge. Fresh releases are the canonical supply-chain attack window
# (maintainer-account takeover, typosquat promotion, malicious version bumps).
# Covers:
#   - services/go.mod        → publish timestamp via https://proxy.golang.org
#   - **/package-lock.json   → publish timestamp via https://registry.npmjs.org
#     (skipped gracefully while the repo has no frontend / lockfile)
# Applies to NEW dependencies AND version upgrades of existing ones — both
# appear as added `require`/`version` lines in the diff.
#
# Exit codes: 0 = pass or nothing to check; 1 = cooldown violation or an
# unresolvable module (fail-closed: an unpublishable/unknown version cannot
# prove its age, so it is rejected).
#
# Env:
#   BASE_SHA           compare this commit's lockfiles against HEAD.
#                      Default: pull_request base sha / push before-sha / HEAD~1.
#   DEP_COOLDOWN_DAYS  minimum publish age in days (default 7).
#   GO_PROXY           Go module proxy base (default https://proxy.golang.org).
#   NPM_REGISTRY       npm registry base   (default https://registry.npmjs.org).
#   ALLOW_UNRESOLVABLE set to 1 to downgrade "cannot verify age" to a warning —
#                      documented escape hatch for private (GOPRIVATE) modules
#                      that proxy.golang.org cannot resolve. Default: strict.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

DEP_COOLDOWN_DAYS="${DEP_COOLDOWN_DAYS:-7}"
GO_PROXY="${GO_PROXY:-https://proxy.golang.org}"
NPM_REGISTRY="${NPM_REGISTRY:-https://registry.npmjs.org}"
ALLOW_UNRESOLVABLE="${ALLOW_UNRESOLVABLE:-0}"
COOLDOWN_SECS=$((DEP_COOLDOWN_DAYS * 86400))
NOW_EPOCH="$(date -u +%s)"
VIOLATIONS=0
CHECKED=0

log()  { printf '%s\n' "$*"; }
fail() { log "FAIL: $*"; VIOLATIONS=$((VIOLATIONS + 1)); }

# Resolve the base commit to diff against.
resolve_base() {
    if [ -n "${BASE_SHA:-}" ]; then
        printf '%s' "$BASE_SHA"; return
    fi
    if [ -n "${GITHUB_BASE_REF:-}" ]; then
        # PR builds check out a merge commit; the base ref head is fetched
        # below as FETCH_HEAD if needed. Prefer the PR base SHA in the event.
        if [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -f "$GITHUB_EVENT_PATH" ]; then
            local sha
            sha="$(jq -r '.pull_request.base.sha // empty' "$GITHUB_EVENT_PATH" 2>/dev/null || true)"
            [ -n "$sha" ] && { printf '%s' "$sha"; return; }
        fi
        printf 'origin/%s' "$GITHUB_BASE_REF"; return
    fi
    # push events: github.event.before carries the pre-push head.
    if [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -f "$GITHUB_EVENT_PATH" ]; then
        local before
        before="$(jq -r '.before // empty' "$GITHUB_EVENT_PATH" 2>/dev/null || true)"
        if [ -n "$before" ] && [ "$before" != "0000000000000000000000000000000000000000" ]; then
            printf '%s' "$before"; return
        fi
    fi
    printf 'HEAD~1'
}

# GOPROXY module-path escaping: uppercase letters become !lowercase.
escape_mod()  { printf '%s' "$1" | sed 's/\([A-Z]\)/!\L\1/g'; }

# epoch of a registry publish timestamp; empty on failure.
publish_epoch_go() {
    # $1 = module path, $2 = version
    local url info
    url="${GO_PROXY}/$(escape_mod "$1")/@v/$(escape_mod "$2").info"
    info="$(curl -fsSL --retry 2 --max-time 20 "$url" 2>/dev/null)" || return 1
    printf '%s' "$info" | jq -r '.Time // empty' | while read -r t; do
        [ -n "$t" ] && date -u -d "$t" +%s
    done
}

publish_epoch_npm() {
    # $1 = package, $2 = version
    local url ts
    url="${NPM_REGISTRY}/$(printf '%s' "$1" | sed 's|/|%2f|')"
    ts="$(curl -fsSL --retry 2 --max-time 20 "$url" 2>/dev/null \
          | jq -r --arg v "$2" '.time[$v] // empty' 2>/dev/null)" || return 1
    [ -n "$ts" ] && date -u -d "$ts" +%s
}

check_age() {
    # $1 = ecosystem label, $2 = module, $3 = version, $4 = publish epoch
    local age=$((NOW_EPOCH - $4))
    if [ "$age" -lt "$COOLDOWN_SECS" ]; then
        fail "$1 $2@$3 published $((age / 86400))d ago (< ${DEP_COOLDOWN_DAYS}d cooldown)"
    else
        log "  ok  $1 $2@$3 (published $((age / 86400))d ago)"
        CHECKED=$((CHECKED + 1))
    fi
}

unresolvable() {
    local msg="$1 ($2@$3): cannot verify publish date (registry lookup failed)"
    if [ "$ALLOW_UNRESOLVABLE" = "1" ]; then
        log "  warn  $msg — ALLOW_UNRESOLVABLE=1"
    else
        fail "$msg"
    fi
}

BASE="$(resolve_base)"
log "dep-cooldown: base=$BASE min-age=${DEP_COOLDOWN_DAYS}d"

# --- go.mod (every tracked Go module in the repo) ----------------------------
mapfile -t gomods < <(git ls-files '*go.mod' 2>/dev/null \
    | grep -vE '(^|/)build[^/]*/|third_party/' || true)
if [ "${#gomods[@]}" -eq 0 ]; then
    log "go.mod: none tracked — skipped"
fi
for GO_MOD in "${gomods[@]:-}"; do
    [ -n "$GO_MOD" ] || continue
    if ! git rev-parse --verify --quiet "$BASE" >/dev/null; then
        log "go.mod: base commit '$BASE' not resolvable — skipped (shallow clone?)"
        break
    fi
    # Added require entries: `+module vX.Y.Z` (also covers version bumps,
    # whose old line is `-` and new line is `+`).
    mapfile -t added < <(
        git diff --unified=0 "$BASE"..HEAD -- "$GO_MOD" 2>/dev/null \
        | grep -E '^\+[[:space:]]*[[:alnum:]_.~/-]+[[:space:]]+v[0-9]' \
        | sed -E 's|^\+[[:space:]]*([^[:space:]]+)[[:space:]]+(v[^[:space:]]+).*|\1 \2|' \
        | sort -u || true)
    if [ "${#added[@]}" -eq 0 ]; then
        log "$GO_MOD: no new/changed dependency versions"
    else
        log "$GO_MOD: ${#added[@]} new/changed version(s)"
        for line in "${added[@]}"; do
            mod="${line%% *}"; ver="${line##* }"
            # The `go 1.x` / `toolchain goX.Y` directives can never match —
            # the regex above only admits v-prefixed versions.
            if epoch="$(publish_epoch_go "$mod" "$ver")" && [ -n "$epoch" ]; then
                check_age "go" "$mod" "$ver" "$epoch"
            else
                unresolvable "proxy.golang.org lookup failed" "$mod" "$ver"
            fi
        done
    fi
done

# --- package-lock.json (frontend absent today; skip gracefully) -------------
mapfile -t locks < <(git ls-files '*package-lock.json' 2>/dev/null || true)
if [ "${#locks[@]}" -eq 0 ]; then
    log "npm: no package-lock.json tracked — skipped (frontend lands in Phase-10)"
else
    for lock in "${locks[@]}"; do
        mapfile -t pkgs < <(
            git diff --unified=0 "$BASE"..HEAD -- "$lock" 2>/dev/null \
            | grep -E '^\+[[:space:]]*"node_modules/' \
            | sed -E 's|^\+[[:space:]]*"node_modules/([^"]+)".*|\1|' | sort -u || true)
        [ "${#pkgs[@]}" -eq 0 ] && continue
        for pkg in "${pkgs[@]}"; do
            ver="$(jq -r --arg p "node_modules/$pkg" \
                  '.packages[$p].version // empty' "$lock" 2>/dev/null)"
            [ -z "$ver" ] && { unresolvable "version not in lockfile" "$pkg" "?"; continue; }
            if epoch="$(publish_epoch_npm "$pkg" "$ver")" && [ -n "$epoch" ]; then
                check_age "npm" "$pkg" "$ver" "$epoch"
            else
                unresolvable "registry.npmjs.org lookup failed" "$pkg" "$ver"
            fi
        done
    done
fi

log "dep-cooldown: checked=$CHECKED violations=$VIOLATIONS"
[ "$VIOLATIONS" -eq 0 ]
