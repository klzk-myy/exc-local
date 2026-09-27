#!/usr/bin/env bash
# lint_cpp.sh — C++ static-analysis + format gate (Phase-01.5 Task 1.5.3.1;
# runs in .github/workflows/ci.yml job `build-and-lint`).
#
#   1. clang-tidy — every translation unit in $BUILD_DIR/compile_commands.json
#      (parallel). Diagnostics located in system/vendor code (/usr/include,
#      /usr/lib, core/third_party/, build _deps/) are printed as informational
#      only: they are not ours to fix (e.g. the flatbuffers 1.12 system header
#      trips a clang-19 clang-diagnostic-error the project cannot patch).
#      Any warning/error diagnostic in repo-owned sources FAILS the gate.
#   2. clang-format — line-level diff gate: lines under core/ changed vs the
#      PR merge-base must be format-clean (clang-format-diff.py). The
#      pre-existing tree predates clang-format; line-scoping adopts the style
#      incrementally with zero reformat churn. FORMAT_ALL=1 audits the whole
#      tree with --dry-run --Werror.
#
# Env:
#   BUILD_DIR       dir holding compile_commands.json (default core/build).
#                   Configure with: cmake -S core -B core/build \
#                     -DCMAKE_EXPORT_COMPILE_COMMANDS=ON
#   BASE_SHA        diff base for the format gate. Empty or all-zeros resolves
#                   `git merge-base HEAD origin/master` → HEAD~1 → skip.
#   TIDY_BIN        clang-tidy binary   (default: clang-tidy)
#   FORMAT_BIN      clang-format binary (default: clang-format)
#   JOBS            tidy parallelism    (default: nproc, min 2)
#   FORMAT_ALL=1    format-check every core file (audit mode)
#
# Exit: 0 clean · 1 findings · 2 misuse/missing tool (fail-closed: a gate
# that cannot run must not silently pass).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

BUILD_DIR="${BUILD_DIR:-core/build}"
TIDY_BIN="${TIDY_BIN:-clang-tidy}"
FORMAT_BIN="${FORMAT_BIN:-clang-format}"
JOBS="${JOBS:-$(nproc 2>/dev/null || echo 4)}"
[ "$JOBS" -ge 2 ] || JOBS=2
FORMAT_ALL="${FORMAT_ALL:-0}"

fail() { echo "lint_cpp: $*" >&2; exit 1; }
die_usage() { echo "lint_cpp: $*" >&2; exit 2; }

command -v "$TIDY_BIN"   >/dev/null 2>&1 || die_usage "clang-tidy not on PATH (pip install clang-tidy==19.1.0.1)"
command -v "$FORMAT_BIN" >/dev/null 2>&1 || die_usage "clang-format not on PATH (pip install clang-format==19.1.0)"

DB="$BUILD_DIR/compile_commands.json"
[ -f "$DB" ] || die_usage "$DB missing — run: cmake -S core -B $BUILD_DIR -DCMAKE_EXPORT_COMPILE_COMMANDS=ON"

# --- 1. clang-tidy ----------------------------------------------------------
mapfile -t TUS < <(python3 - "$DB" <<'PY'
import json, sys
seen = set()
for e in json.load(open(sys.argv[1])):
    f = e.get("file", "")
    if f.endswith((".cpp", ".cc", ".cxx")) and f not in seen:
        seen.add(f)
        print(f)
PY
)
[ "${#TUS[@]}" -gt 0 ] || die_usage "no translation units in $DB"

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
echo "lint_cpp: clang-tidy ($TIDY_BIN) over ${#TUS[@]} TUs, -j$JOBS"
i=0
for f in "${TUS[@]}"; do
    i=$((i + 1))
    ( "$TIDY_BIN" -p "$DB" --quiet "$f" >"$TMP/$i.log" 2>&1; echo "$f" >>"$TMP/$i.src" ) &
    while (( $(jobs -rp | wc -l) >= JOBS )); do sleep 0.1; done
done
wait

FINDINGS=0
declare -a FILES_HIT
for log in "$TMP"/*.log; do
    src="$(cat "${log%.log}.src" 2>/dev/null || echo '?')"
    # Diagnostics on repo-owned code: paths under the repo (absolute paths in
    # compile_commands are $ROOT/…) minus vendored/generated trees and the
    # host toolchain. System-header diagnostics stay informational below.
    hits="$(grep -hE ':[0-9]+:[0-9]+: (warning|error):' "$log" \
            | grep -vE 'third_party/|/_deps/|/usr/(include|lib)/' || true)"
    if [ -n "$hits" ]; then
        FINDINGS=$((FINDINGS + 1))
        FILES_HIT+=("$src")
        echo "── tidy findings in ${src#"$ROOT"/}"
        echo "$hits" | head -20
    fi
done
if (( FINDINGS > 0 )); then
    echo "lint_cpp: clang-tidy gate FAILED — ${#FILES_HIT[@]} file(s) with repo-owned diagnostics" >&2
    exit 1
fi
echo "lint_cpp: clang-tidy clean (${#TUS[@]} TUs)"

# --- 2. clang-format ---------------------------------------------------------
# FORMAT_ALL=1 audits every core file (whole-tree mode, for periodic hygiene).
# Default = diff gate: only lines added/modified vs the PR merge-base must be
# format-clean (LLVM clang-format-diff.py, vendored). This enforces style on
# new/edited code without reformatting legacy files en masse.
if [ "$FORMAT_ALL" = "1" ]; then
    mapfile -t FMT_FILES < <(find core/src core/include core/tests \
        -type f \( -name '*.cpp' -o -name '*.cc' -o -name '*.cxx' \
                   -o -name '*.hpp' -o -name '*.hh' -o -name '*.h' \) | sort)
    echo "lint_cpp: FORMAT_ALL=1 — checking ${#FMT_FILES[@]} core files"
    DIRTY=()
    for f in "${FMT_FILES[@]}"; do
        "$FORMAT_BIN" --dry-run --Werror "$f" >/dev/null 2>&1 || DIRTY+=("$f")
    done
    if [ "${#DIRTY[@]}" -gt 0 ]; then
        echo "lint_cpp: clang-format gate FAILED (FORMAT_ALL=1):" >&2
        printf '  %s\n' "${DIRTY[@]}" >&2
        exit 1
    fi
    echo "lint_cpp: clang-format gate clean (whole tree)"
else
    BASE="${BASE_SHA:-}"
    if [ -n "$BASE" ] && ! [[ "$BASE" =~ ^0+$ ]]; then
        MB="$(git merge-base "$BASE" HEAD 2>/dev/null || true)"
        [ -n "$MB" ] || MB="$BASE"
    else
        MB="$(git rev-parse --verify HEAD~1 2>/dev/null || true)"
    fi
    if [ -z "$MB" ]; then
        echo "lint_cpp: no diff base resolvable — format gate skipped (first push)"
    else
        echo "lint_cpp: format gate base ${MB:0:12} — line-level (clang-format-diff)"
        # diff $MB → worktree (in CI the checkout == PR head, so identical to
        # $MB..HEAD; locally this also covers staged/uncommitted edits).
        if ! git diff -U0 --no-color "$MB" -- core/src core/include core/tests \
                ':!core/third_party' \
             | python3 "$ROOT/scripts/ci/clang-format-diff.py" \
                 -p1 -binary="$FORMAT_BIN" >"$TMP/fmt.diff"; then
            echo "lint_cpp: clang-format diff gate FAILED — suggested formatting:" >&2
            cat "$TMP/fmt.diff" >&2
            echo "lint_cpp: apply with: git diff -U0 <base> -- core | \\" >&2
            echo "            python3 scripts/ci/clang-format-diff.py -p1 -i" >&2
            exit 1
        fi
        echo "lint_cpp: clang-format gate clean (changed lines only)"
    fi
fi
