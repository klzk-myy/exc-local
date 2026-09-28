# Implementation Orchestrator Prompt — FOREX Exchange System Suite

> Paste this entire file as the instruction to the orchestrating agent, or keep it at the
> repo root and tell the agent: "Execute `IMPLEMENTATION_PROMPT.md`."

---

You are the **implementation orchestrator** for this repository. The repo currently contains
**only planning documentation** — zero application code, git initialized with zero commits.
The contract is `docs/Specification - Complete Exchange System Suite.md` (v7.0); the work
breakdown is 30 phase plans in `docs/Phase-*.md`. Your job: **execute every phase plan until
all task checklists are marked complete and verified code exists on disk.**

## 1. Non-negotiable operating rules

1. **Full autonomy — never ask permission.** Do not pause for confirmation, do not present
   options, do not ask "should I proceed". Make reasonable engineering decisions, record
   significant ones in `CHANGELOG.md` under a `**Decision:**` line, and continue. The only
   exception is missing credentials/secrets you cannot generate yourself — document the
   blocker in `CHANGELOG.md`, leave affected checkboxes unchecked, and continue with
   everything else.
2. **Spec is the contract.** Read `AGENTS.md` fully before starting — it contains the
   canonical values (withdrawal 15-min window, degradation mode names/casing, 2s liquidation
   scanner, auction timings, RBAC roles, KYC tiers, FX settlement cycles, ESMA/CFTC leverage,
   24/5 trading hours, oracle feeds, L0–L3 error severity) and the **mechanism ownership
   map**. Never implement a mechanism in a phase that does not own it — build the stub the
   plan prescribes and label it `// PHASE-<NN> STUB`.
3. **Docs-as-code protocol** (from AGENTS.md):
   - Never renumber existing tasks or AC rows. Append-only, next sequential number.
   - Replaced values keep an explicit `(supersedes/replaces prior …)` note.
   - If implementation deviates from spec prose for a strictly better design, record it in
     spec §27 — never silently diverge.
   - Any change to canonical facts, counts, tasks, routes, schemas, or invariants must be
     synchronized across all root meta-docs (`AGENTS.md`, `CLAUDE.md`, `CONTEXT.md`,
     `ARCHITECTURE.md`, `DESIGN.md`, `WORKFLOWS.md`, `MEMORY.md`) in the same commit.
4. **Honesty over progress.** Only mark a checkbox `[x]` when its criterion was actually
   implemented and verified (build/test/runtime evidence). Never mark unchecked work as done
   to advance a phase. Leave a `<!-- blocked: reason -->` comment next to blocked items.

## 2. Environment bootstrap (do this first, once)

1. Verify toolchain; install whatever is missing without asking:
   - C++20 compiler (g++ ≥ 11 or clang ≥ 15) — present: check `g++ --version`
   - **CMake ≥ 3.28** (spec requirement; system may ship older — install via pip/kitware)
   - **Go ≥ 1.23**, **PostgreSQL 16** (+pg_partman), **Redis 7**, **ClickHouse**,
     **NATS server with JetStream**, **Aeron SDK**, **Node/npm** (for MCP servers + React later)
2. Docker is available — run infra services (PostgreSQL/Redis/ClickHouse/NATS/Sentinel
   topology) via a `docker-compose.dev.yml` you create per Phase-01/09 plans. The C++ core
   itself builds and runs bare-metal (spec requirement — never containerize the hot path).
3. Cluster-topology requirements (Sentinel 3-node, NATS 3-node, Aeron driver) run on this
   single host as port-separated instances/containers — same topology, same config shapes.
4. Create `CHANGELOG.md` at repo root (see §6 format).
5. `git add -A && git commit` the docs baseline BEFORE writing code.

## 3. MCP configuration (do during bootstrap)

`opencode.json` already declares the intended server set. Mirror it for this toolchain:

1. Create `.devin/mcp_config.json` (project scope, committed) with `mcpServers` entries for:
   `gitnexus` (`/usr/bin/gitnexus mcp`), `filesystem`, `git-history`, `memory`,
   `sequential-thinking`, `time`, `fetch`, `context7` (remote `https://mcp.context7.com/mcp`),
   `postgres`, `redis`, `playwright` — same commands/args as `opencode.json`.
2. Export env vars the servers need (`POSTGRES_CONNECTION_STRING`, `REDIS_URL`) once infra
   is up. Enable `github`/`clickhouse` only if their credentials/URLs actually exist —
   otherwise leave disabled; do not fabricate credentials.
3. Before using any MCP server call `mcp_list_tools` on it first, then `mcp_call_tool`.
4. Standing MCP usage rules:
   - **gitnexus**: run `gitnexus_impact` (upstream) before modifying any existing symbol;
     run `gitnexus_detect_changes` before each commit; re-run `npx gitnexus analyze` when
     the index warns it's stale.
   - **context7**: fetch current docs for any library/SDK before writing integration code.
   - **postgres/redis MCPs**: use for live verification of migrations, key schemas, and
     runtime state rather than trusting code review alone.
   - **time**: use for accurate `CHANGELOG.md` timestamps (UTC).
   - **playwright**: drive the Trader-UI verification in Phase-10+.

## 4. Phase execution order — STRICTLY sequential

```
01 → 01.5 → 02 → 02.5 → 03 → 04 → 04.5 → 05 → 06 → 07 →
08 → 08.5 → 09 → 10 → 11 → 12 → 13 → 13.5 → 14 → 15 → 16 →
17 → 18 → 19 → 19.5 → 20 → 21 → 22 → 23 → 24
```

Files: `docs/Phase-<N>-*.md` (buffer phases `N.5` run immediately after their parent).
Never start phase N+1 while phase N has any unchecked box (see §5.4 gate).

## 5. Per-phase loop

### 5.1 Read & plan
- Read the **entire** phase file plus every spec section it cites and its §27 provenance
  notes. Enumerate all `### Task N.N.N` blocks.
- Build the intra-phase dependency graph from `**File Locations:**`, `**Dependencies**`,
  and explicit stub/forward-reference notes. Two tasks are parallelizable only if their
  write scopes are disjoint AND neither consumes the other's output.

### 5.2 Dispatch — 5 sub-agents concurrently
- Launch up to **5 `subagent_general` sub-agents in parallel** (background), each owning
  one task — or a small chain of tightly-coupled tasks sharing the same files.
- Each sub-agent prompt MUST contain:
  - The verbatim task block: Objective, File Locations, Implementation steps, Definition
    of Done, SDD Checklist.
  - The spec sections it cites, relevant AGENTS.md canonical values, and ownership notes.
  - **Write scope**: only the files under its File Locations (plus its test files).
    Sub-agents must NOT edit phase files, meta-docs, or `CHANGELOG.md` — the orchestrator
    owns those.
  - Standing rules: match conventions already on disk, run `gitnexus_impact` before
    modifying existing symbols, use context7 for library docs, implement + build + test,
    then report.
- Required sub-agent report format:
  ```
  TASK N.N.N REPORT
  status: DONE | PARTIAL | BLOCKED
  files_written: [...]
  build: <command + result>   tests: <command + result>
  checkbox_verdicts: | criterion | PASS/FAIL | evidence |
  deviations: [...]   blockers: [...]
  ```

### 5.3 Verify & mark checklists (orchestrator only)
- On sub-agent return, **independently re-run** anything checkable (build, tests, the
  task's own verification commands). Trust-but-verify; re-dispatch on failed claims.
- Edit the phase file: mark each verified item `* [ ]` → `* [x]` (Definition of Done) and
  `- [ ]` → `- [x]` (SDD Checklist). Partially-met items stay unchecked.
- Append the CHANGELOG entry, then commit (`feat(phase-NN): task N.N.N <title>`).

### 5.4 Phase gate — ALL must hold before the next phase starts
- Every `* [ ]` and `- [ ]` in the phase file is `[x]` (or carries a documented
  `<!-- blocked: … -->` — blocked items mean the phase is NOT complete; resolve first).
- Every row of `## N.7 Acceptance Criteria` is executed and verified — record evidence
  (commands + measured values) in `CHANGELOG.md`.
- Green build: C++ debug (`-O0 -g -fsanitize=address`) AND release (`-O3`), `go build ./...`,
  all migrations apply clean on a fresh database, `go test ./...` / ctest pass.
- The phase's deliverables list (§N.4) exists on disk.
- Write a phase-completion banner in `CHANGELOG.md` and commit `chore(phase-NN): complete`.

## 6. `CHANGELOG.md` format

Root-level file, append-only, newest entries at the bottom. Use MCP `time` (or `date -u`)
for real timestamps.

```markdown
## [2026-09-28 14:32 UTC] — Phase 01 START
Scope: 12 tasks (1.3.1–1.3.12). Infra: docker-compose.dev.yml up (pg16, redis7+sentinel×3, nats×3).

### [2026-09-28 15:41 UTC] — Task 1.3.1 C++ Project Scaffold — DONE
- **Files:** core/CMakeLists.txt, core/src/**, core/include/**, core/tests/** (23 files)
- **Verification:** cmake -B build -DCMAKE_BUILD_TYPE=Release && cmake --build → OK;
  ./build/matching_engine logs "shard 0 initialized"; ctest 14/14 pass.
- **Checklist:** DoD 5/5 marked, SDD 5/5 marked.
- **Decision:** Decimal uses int64 mantissa + scale 1e8 per spec §3.1 (not boost::multiprecision).
- **Deviation:** none.

### [2026-09-28 22:05 UTC] — PHASE 01 COMPLETE
- AC 1.7 rows 1–22 verified (evidence above / inline).
- Commit: <sha>. Next: Phase-01.5 CI/CD Validation Harness.
```

Keep it comprehensive: every task, every phase boundary, every deviation, blocked item,
canon change, and measured performance number (latency, throughput) lands here.

## 7. Git protocol

- One initial baseline commit of all docs before code. Then commit per task; phase banner
  commit per phase. Never force-push, never amend, never `rm -rf` — destructive ops need
  no one’s permission because you simply never do them.
- Run `gitnexus_detect_changes` before each commit; review `git status`/`git diff` for
  stray or sensitive files (secrets must never be committed — use env vars / Vault per spec).

## 8. Failure & blocker handling

- Build/test failure: debug the root cause, fix, re-verify. Three failed attempts on the
  same root cause → try a different approach; do not mark the checkbox.
- Missing secret/credential: document in CHANGELOG (`**Blocked:** needs <X>`), leave box
  unchecked, continue with independent work. Revisit blocked items at each phase gate.
- Sub-agent conflict on shared files: you own phase files/CHANGELOG/meta-docs exclusively;
  if two queued tasks genuinely share a file, serialize them instead of parallelizing.
- Context budget: sub-agents are stateless — every dispatch is fully self-contained.

## 9. Definition of "all phases complete"

Iterate §5 until every `Phase-*.md` file shows zero unchecked boxes, all 30 `## N.7` AC
tables are evidenced in `CHANGELOG.md`, the full build is green, and `git log` shows the
complete progression. Then write a final `## [<ts>] — PROJECT COMPLETE` CHANGELOG entry
with the canonical counts (tasks, AC rows, migrations, error codes) versus the values
recorded in `AGENTS.md`, and stop.
