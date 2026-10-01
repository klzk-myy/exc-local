# Spec Validation Harness (Phase-01.5 Task 1.5.3.2)

Per-task spec checkpoint extraction, deterministic sharding, execution, and
JSON reporting for the whole exchange suite. CI runs one invocation per
shard; each invocation fails (exit ≠ 0) if any *implemented* checkpoint
fails or any *completed-phase* checkpoint lacks coverage.

## Layout

```
tests/spec/
├── go.mod                  module exchange-testspec (replace exchange => ../../services)
├── validator.go            CLI: run | extract | list | stubs | merge
├── spec/                   framework library
│   ├── checkpoint.go         extractor: `Spec checkpoint:` lines → Checkpoint{ID,shard,…}
│   ├── registry.go           CheckFunc registry, Status enum, Env (endpoints/flags)
│   ├── exec.go               subprocess helpers (ctest / go test / gtest) + live probes
│   ├── runner.go             per-checkpoint timeout, retries/flaky, panic capture
│   ├── report.go             JSON report schema + exit-code policy + merge
│   └── spec_test.go          framework unit tests (fixture docs; no repo coupling)
├── checks/                 document-checkpoint implementations (P<phase>-T<task>-C<n>)
│   ├── phase01.go            all 23 Phase-01 checkpoints (phase is [x]-complete)
│   └── phase01_5.go          Task 1.5.3.2 self-checks
├── golden/                 the golden corpus — 29 spec-derived cases (GOLDEN-* IDs)
└── checkpoints/            GENERATED artifacts — regenerate via `validator extract --write`
    ├── checkpoints.json      committed corpus snapshot (vanished-detection baseline)
    ├── stubs.gen.go          PendingStubs: checkpoint IDs lacking implementations
    └── PENDING.md            human-readable stub list with copy-paste skeleton
```

## Checkpoint IDs

`P<phase>-T<task>-C<index>` — e.g. `P01-T1.3.6-C2`.

* `phase` from the doc filename (`Phase-01` → `01`, `Phase-19.5` → `19.5`)
* `task` from the nearest preceding `### Task N.N.N:` header
* `index` = ordinal of the checkpoint line inside that task's SDD checklist

IDs are deterministic across runs and stable across unrelated doc edits.
Reordering checkpoints *inside* a task shifts ordinals — that is a docs
defect (AGENTS.md: never renumber), and it surfaces as corpus drift.

## Sharding

`shard = fnv1a32(checkpointID) % 4` — deterministic on every toolchain.
Current distribution: 117 / 131 / 140 / 154 across the 4 shards. Every
shard owns a non-empty proper subset — no single shard runs all.

## Status semantics (CI policy)

| status      | meaning                                              | fails CI |
|-------------|------------------------------------------------------|----------|
| `pass`      | implemented; ran; assertions held                    | no       |
| `fail`      | implemented; ran; assertion failed                   | **yes**  |
| `timeout`   | implemented; exceeded per-checkpoint `--timeout`     | **yes**  |
| `error`     | implemented; panicked / harness error                | **yes**  |
| `skip`      | implemented; dependency absent (env-gated)           | no¹      |
| `pending`   | doc checkbox `[ ]` and no implementation registered, or an env-bound implementation reports pending (artifact-gated) | no       |
| `missing`   | doc checkbox `[x]` but NO implementation registered  | **yes**  |
| `vanished`  | in committed corpus, absent from docs, was `[x]`     | **yes**  |
| `dropped`   | in committed corpus, absent from docs, was `[ ]`     | no (warn)|

¹ `--fail-on-skip` upgrades `skip` to a failure — use on CI runners where
the ephemeral stack is health-checked before tests, so a missing
dependency can't mask a regression.

This policy is what keeps CI green while phases execute: pending work is
*reported* but doesn't fail; the moment a task's checkbox flips to `[x]`,
an implementation must exist and must pass.

## Running

```bash
cd tests/spec
go build -o validator .

./validator run --shard=0 --shards=4 --report=reports/shard-0.json
./validator run --shard=1 --shards=4 --report=reports/shard-1.json
# … 2, 3
./validator merge --reports='reports/shard-*.json' --report=reports/all.json
```

Flags: `--timeout` (per-checkpoint, default 30s), `--retries` (default 1 —
a checkpoint that recovers is `pass` + `flaky:true`, so flakiness is
surfaced, never hidden), `--fail-on-skip`, `--only=ID,ID`, `--docs=`.

Other commands:

* `./validator extract` — diff live extraction vs committed corpus
  (non-zero on drift). `--write` regenerates `checkpoints/*` artifacts —
  run it whenever phase docs legitimately add/remove checkpoints.
* `./validator list [--shard=N]` — every checkpoint + shard + impl state.
* `./validator stubs` — checkpoint IDs lacking implementations.

## CI contract

`scripts/ci/shard_runner.sh <shard_idx> <num_shards> <report_dir>`
(Task 1.5.3.1) invokes `validator run` per shard; the validator exits
non-zero on any failing status. 4 shards, deterministic.

## Live-dependency gates

Checks probe real infra when reachable and `skip` otherwise. Defaults
match `docker-compose.dev.yml`; override via env:

| var                   | default                                                        |
|-----------------------|----------------------------------------------------------------|
| `EXC_TEST_DSN`        | `postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable` |
| `EXC_REDIS_TEST_ADDR` | `127.0.0.1:16379`                                              |
| `EXC_NATS_URLS`       | `nats://127.0.0.1:4222`                                        |
| `EXC_SENTINEL_ADDRS`  | `127.0.0.1:36379,…:36380,…:36381`                             |
| `EXC_CLICKHOUSE_HTTP` | `http://127.0.0.1:8123`                                        |
| `EXC_CORE_BUILD`      | `<root>/core/build` (ctest target dir)                         |
| `EXC_REPO_ROOT`       | auto-detected (walk up for README.md + docs/ + services/go.mod)|

Internal-package checks (`exchange/internal/**`) run as `go test -run`
subprocesses inside `services/` — Go's internal-visibility rule forbids
direct import from this module; subprocess delegation also inherits the
services' own env-gated skips.

## Coverage notes / honest limits

* Phase-01's 23 `[x]` checkpoints all have implementations (structural +
  behavioral + live probes, per checkpoint).
* Matching-engine behaviors named in the corpus brief (FIFO/STP/FOK/IOC/
  ICEBERG) are covered at the level Phase-01 code supports: enum/wire
  parity, order-book structure, crash/WAL machinery. Their semantic
  executions bind to Phase-02/16 checkpoints which stay `pending` until
  those phases land — the same CheckFuncs get upgraded, not replaced.
* Canonical count reconciliation: extraction is strict (checkbox lines
  only) → **542** checkpoints across 479 tasks. The canonical "543" quoted
  in AGENTS.md/Phase-01.5 is a raw grep count that also matches one prose
  occurrence inside this task's own implementation text (line ~62). The
  self-check P01.5-T1.5.3.2-C1 asserts ≥400 and reconciles the ±1.

## Regeneration workflow

1. Edit a phase doc (checkpoints added/removed/reordered).
2. `validator extract` — shows added/removed IDs; non-zero on drift.
3. `validator extract --write` — rewrite `checkpoints/checkpoints.json`,
   `stubs.gen.go`, `PENDING.md`; commit together.
4. `vanished` (was `[x]`) still fails on the next `run` until the corpus
   is regenerated — completed-phase coverage cannot silently disappear.
