#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# verify_counts_test.sh — unit tests for backup.sh's verify_counts logic
# (Task 4.3.6). Feeds canned table<TAB>rows TSV files; no ClickHouse needed.
# -----------------------------------------------------------------------------
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Source only the function: run backup.sh with `verify` off canned files.
verify() { "$script_dir/backup.sh" verify "$1" "$2"; }

failures=0
expect_rc() { # expect_rc <want-rc> <name> <src> <dst>
    local want=$1 name=$2
    set +e
    verify "$3" "$4" >"$tmp/out.$name" 2>&1
    local got=$?
    set -e
    if [[ $got -ne $want ]]; then
        echo "FAIL $name: rc=$got want=$want" >&2
        cat "$tmp/out.$name" >&2
        failures=$((failures+1))
    else
        echo "ok   $name (rc=$got)"
    fi
}

cat > "$tmp/src_match.tsv" <<'EOF'
default.tick_history	1523400
default.ohlcv_1m	86400
EOF
cp "$tmp/src_match.tsv" "$tmp/dst_match.tsv"
expect_rc 0 match "$tmp/src_match.tsv" "$tmp/dst_match.tsv"

cat > "$tmp/dst_missing.tsv" <<'EOF'
default.tick_history	1523400
EOF
expect_rc 1 missing_table "$tmp/src_match.tsv" "$tmp/dst_missing.tsv"
grep -q "MISSING default.ohlcv_1m" "$tmp/out.missing_table" \
    && echo "ok   missing_table reported MISSING" \
    || { echo "FAIL missing_table: no MISSING line" >&2; failures=$((failures+1)); }

cat > "$tmp/dst_mismatch.tsv" <<'EOF'
default.tick_history	1523400
default.ohlcv_1m	86000
EOF
expect_rc 1 count_mismatch "$tmp/src_match.tsv" "$tmp/dst_mismatch.tsv"
grep -q "MISMATCH default.ohlcv_1m src=86400 restored=86000" \
    "$tmp/out.count_mismatch" \
    && echo "ok   count_mismatch reported counts" \
    || { echo "FAIL count_mismatch: no MISMATCH line" >&2; failures=$((failures+1)); }

# Extra tables in the restore are a NOTE, not a failure.
cat > "$tmp/dst_extra.tsv" <<'EOF'
default.tick_history	1523400
default.ohlcv_1m	86400
default.scratch	5
EOF
expect_rc 0 extra_table "$tmp/src_match.tsv" "$tmp/dst_extra.tsv"

# Empty source is trivially satisfied.
: > "$tmp/empty.tsv"
expect_rc 0 empty_source "$tmp/empty.tsv" "$tmp/dst_match.tsv"

if [[ $failures -eq 0 ]]; then
    echo "PASS: verify_counts unit tests"
else
    echo "FAIL: $failures test(s) failed" >&2
    exit 1
fi
