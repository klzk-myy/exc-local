# phase08-run1 — host-bounded smoke run (NOT the 50k/s gate)

This directory is the committed evidence artifact the spec corpus
(`P08-T8.3.2-C1`) binds to: the Phase-08 load harness exists, is
parameterized for `50k`/`p99`, and has been exercised end-to-end on a
live stack.

**Honest scope:** this run executed on the contended dev host —
`report.json` records ~7.2 orders/s effective, ~30M send drops,
p99 ≈ 1.1s. It proves the harness + measurement path, **not** the
50k/s-sustained / p99 ≤ 50µs acceptance criterion, which stays open
and environment-bound (Phase-08 rows 61–64; requires a dedicated
benchmark host).

Tracked: `report.json`, `report.md`, this README. The bulk run output
(`bin/`, `wal/`, `metrics/`, `*.log`) stays ignored.
