# IMP-PLAN — Close e2e registry coverage gaps

**Status: IMPLEMENTED & VERIFIED (2026-10-02).**
contracts.json regenerated: **163 EXECUTABLE / 256 PLANNED** (was 145/274) — +18 criteria bound.
Live run evidence on this host: `pg-pitr` PASS (real PG PITR), `bluegreen-gate` PASS,
`dr-failover` BLOCKED (no secondary region), `dr-drill-runner` FAIL (D6 chaos 15/18 runs,
D7 secretdrill exit=2 — truthful drill verdicts, recorded verbatim; not masked).
Integrity: TestContractRegistryIntegrity / TestContractsJSONFresh / TestStableIDsMatchTrace /
TestBindingReferences all PASS. All 23 new `-run` regexes verified via `go test -list`;
all ungated legs pass locally. #113 left PLANNED — `rollback.sh` is not exercised by
`bluegreen_gate_test.sh`, binding it would be name-matching only.

**Origin:** e2e coverage audit (2026-10-02). Findings to fix:

1. Criterion 189 (FIX failover) — `TestFailoverE2E` + `TestFailoverResumeLatencyDistribution`
   implemented in `services/internal/fix/failover_e2e_test.go` but PLANNED / zero bindings.
2. ~15 more Phase-18/09 criteria have implemented tests (`internal/fix`, `internal/sor`,
   drain/shutdown pkgs) but no `CriterionBindings` entry.
3. Compose legs `dr-failover` / `pg-pitr` / `pg-rpo-rto` bound but undispatched —
   runners exist (`dr_drill_runner.sh`, `pitr_smoke.sh`, `pg_failover_drill.sh`).
4. Criterion 211 (quarterly DR drill) — `scripts/ops/dr_drill_runner.sh` exists, unbound.
   Runner exits 0 on all-SKIP; must parse `results.jsonl` → all-SKIP = BLOCKED (never PASS).

**Binding discipline (unchanged):** bind only tests/runners that exist and assert the named
behavior; absent infra → BLOCKED with verbatim reason; SKIP never launders to PASS.

**Out of scope (deferred):** Playwright UI e2e expansion (feature-scale effort, separate
workstream); binding Phase-19..24 domain criteria without an obvious implemented leg.

---

## Task 1: Composite `needs` gate tags
* **Objective:** Let a binding declare multiple env gates (`pg+redis`) since `TestFailoverE2E` requires both EXC_PG_TEST and EXC_REDIS_TEST — today `Needs` is a single tag.
* **Step-by-Step Action Plan:**
  1. In `tests/integration/delegated_test.go` `gateFor`, split `b.Needs` on `+` and run each tag through the existing per-tag switch; return the first non-empty gate reason.
  2. Update the `Needs` field doc comment in `itest/contracts.go` to document `+`-joined tags.
  3. Verify single-tag bindings are unaffected (no `+` → identical behavior).
* **Deliverables:**
  1. Composite gate evaluation in `gateFor`.
  2. Updated `Needs` doc comment.
* **Completeness Checklist:**
  - [x] `pg+redis` blocks when either dep is absent, with a verbatim reason
  - [x] All 68 existing single-tag bindings behave identically
  - [x] Unknown tags still → `unknown gate` (fail-closed)

## Task 2: Bind Phase-18 FIX criteria to implemented tests
* **Objective:** Convert the obvious Phase-18 PLANNED criteria to EXECUTABLE by binding real tests in `internal/fix` (and `internal/sor`).
* **Step-by-Step Action Plan:**
  1. `go test -list` each candidate regex against `services/internal/fix` and `internal/sor` to confirm names resolve.
  2. Append entries to `itest/bindings.go` under a new `Phase 18` section:
     - 54 session lifecycle → `TestFromAdmin_ValidLogon|TestHeartbeatAbnormal|TestResumeOnLogon.*|TestPlanResend.*` + pg leg `TestPGStore_SessionLifecycleAndSeq`
     - 55 SP2/derivatives → `TestSP2.*|TestApplVerStamp|TestOptionContract|TestNDFRequiresFixingDate|TestSwapLegDates`
     - 56 FX tags → `TestSettlementDateStandardTag|TestSetFXParties.*|TestSetParties.*`
     - 128 mass quoting → `TestFromApp_MassQuote.*|TestMassQuote.*|TestQuoteCancel.*|TestQuoteService.*|TestQuoteSetLifecycle`
     - 135 entitlement → `TestEntitlementDenied|TestFromApp_.*entitle.*|TestMassQuoteRejectsUnentitled` + pg leg `TestPGStore_EntitlementUpdateTx`
     - 136 cancel-on-disconnect → `TestCoD.*|TestOnLogout_.*|TestDropSessionPurgesSubscriptions`
     - 137 per-session throttle → `TestThrottle_.*|TestFromApp_ThrottledNeverSilentlyDropped`
     - 139 MM program/MMP → `TestMassQuote.*MMP.*|TestMassQuoteLP.*|TestFromApp_MassQuoteLPSuspended.*`
     - 167 FIXS mTLS/certs → `TestCertificateMatrix|TestServerTLSConfigProfile|TestFromAdmin_BadCredential.*|TestFromAdmin_CredentialAccountMismatch|TestClaimSessionFencing`
     - 189 failover → `TestFailoverE2E|TestFailoverResumeLatencyDistribution` (needs `pg+redis`) + `TestFailoverStoreFallsBack|TestRedisSeqStoreIntegration` (needs `redis`)
     - 243 TradingSessionStatus → `TestBroadcastLatencyUnder50ms|TestSubscribeThenBroadcast|TestRapidStateFlap.*|TestInstrumentStatusMapping|TestUnmappedStatesDoNotBroadcast`
     - 319 seq-gap + CoD purge → `TestPlanResend.*|TestCoDPurges.*|TestCoDGracefulLogoutPreserves.*`
     - 193/194/242 SOR → `./internal/sor` legs (`TestLocalWhenLiquid|TestRouteFillLifecycle|TestTimeoutWalksToNextVenue|TestAllVenuesDeadSORTimeout|TestVenueRejectFailsClosed|TestConcurrentRouteGuard|TestFillDedup|TestPartialFillState|TestReleaseForLocal`) — split per criterion semantics; #194 only if an exec-report-mapping test verifies.
  3. Spot-check each bound test actually asserts (read bodies for non-obvious matches); drop any name-only match.
* **Deliverables:**
  1. `itest/bindings.go` Phase-18 block.
  2. Every regex verified via `go test -list`.
* **Completeness Checklist:**
  - [x] Each binding's tests exist (`-list` output non-empty)
  - [x] PG/Redis-gated files bound only under matching `Needs` tags
  - [x] No fabricated bindings (name-match only bindings removed)

## Task 3: Compose dispatch cases for bound-but-unrunner legs
* **Objective:** Wire the three `BindCompose` run names that fall into `default: BLOCKED (no scenario runner bound)` to their real repo runners.
* **Step-by-Step Action Plan:**
  1. `dr-failover` (#44) → `scripts/ops/dr_drill_runner.sh --drills D1 --out <tmpdir>`; parse `results.jsonl`: all-SKIP → BLOCKED, FAIL → FAIL, else PASS.
  2. `pg-pitr` (#85) → `deploy/postgres/pitr_smoke.sh`; exit 77 → BLOCKED ("pg binaries absent"), non-zero → FAIL, 0 → PASS. Change `Needs` from `docker` to `""` (script is host-native, self-gates via 77).
  3. `pg-rpo-rto` (#86) → `deploy/scripts/pg_failover_drill.sh` (keep `Needs:"docker"`).
  4. Factor results.jsonl parsing into one helper shared by dr-failover + dr-drill-runner legs.
* **Deliverables:**
  1. Three dispatch cases in `delegated_test.go`.
  2. Shared `parseDrillResults` helper.
* **Completeness Checklist:**
  - [x] All-SKIP catalogs report BLOCKED (no PASS-laundering)
  - [x] `pitr_smoke` exit 77 → BLOCKED
  - [x] Unknown run names still → BLOCKED

## Task 4: Bind criterion 211 via `dr-drill-runner` bin leg
* **Objective:** Quarterly DR drill orchestrator bound as an honest, self-gating leg.
* **Step-by-Step Action Plan:**
  1. Add `bindin` entry #211: `{Kind: BindBin, Binary:"dr-drill-runner", Needs:""}` with note that per-drill gating lives in results.jsonl.
  2. `runBinLeg` case `dr-drill-runner`: run `scripts/ops/dr_drill_runner.sh` with `--out`/`--report` into `os.MkdirTemp`; parse results via Task-3 helper; all-SKIP → BLOCKED.
  3. Add `dr-drill-runner` case to `TestBindingReferences` (stat the script).
* **Deliverables:**
  1. Binding + dispatch + reference-check case.
* **Completeness Checklist:**
  - [x] Host without drill infra → BLOCKED, not PASS
  - [x] Report/evidence go to a temp dir, not `docs/incidents/drills/` (no repo pollution)

## Task 5: Bind criterion 216 (graceful shutdown) + opportunistic #113
* **Objective:** Drain/shutdown tests exist across three packages; bind if they assert the criterion.
* **Step-by-Step Action Plan:**
  1. Verify `internal/middleware/shutdown_test.go`, `internal/fix/drain_test.go`, `internal/ws/drain_test.go` test names via `-list`.
  2. Bind #216 → legs per package (gateway drain / FIX logout / WS reconnect hint).
  3. #113 (blue-green rollback tested): bind only if `deploy/scripts/tests/bluegreen_gate_test.sh` runs standalone; otherwise leave PLANNED.
* **Deliverables:**
  1. #216 bindings; #113 binding if runner verified.
* **Completeness Checklist:**
  - [x] Each leg verified to exist and assert (not name-matched blindly)

## Task 6: Regenerate, verify, sync meta-docs
* **Objective:** Regenerate the registry artifact and prove integrity; sync stated counts.
* **Step-by-Step Action Plan:**
  1. `cd tests/integration && go run ./cmd/contractgen -write` → `contracts.json` statuses flip PLANNED→EXECUTABLE for newly bound criteria.
  2. `gofmt -l` + `go vet ./...` on tests/integration.
  3. `go test -run 'TestContractRegistryIntegrity|TestContractsJSONFresh|TestStableIDsMatchTrace|TestBindingReferences'`.
  4. `rg` AGENTS.md/CLAUDE.md/CONTEXT.md/MEMORY.md/WORKFLOWS.md for stated EXECUTABLE/PLANNED or bindings counts; update if present.
  5. Update bindings.go file header ("Phase 1–7 owners" no longer accurate).
* **Deliverables:**
  1. Regenerated `contracts.json`.
  2. Green integrity tests; meta-docs consistent.
* **Completeness Checklist:**
  - [x] `TestContractsJSONFresh` passes (no stale-registry drift)
  - [x] EXECUTABLE count rises by the number of newly bound criteria; no criterion unbound that was bound
  - [x] No dead code introduced; shared helper reused by both drill legs

---

# Phase 2 — Component Completeness Assessment (vertical slices)

**Status: COMPLETE (2026-10-02).** Code-truth assessment of the 13 functional
domains. Supersedes the 2026-09-27 planning-stage baseline in
`docs/PROJECT_COMPLETENESS_ASSESSMENT.md` / `docs/SLICE_3_ASSESSMENT.md`
(which reported Spec/Plan 100%, Code 0% — predating implementation).

**Method:** each component is scored on physical code evidence — implementing
source files, tests asserting the behavior, `contracts.json` bindings
(163 EXECUTABLE / 256 PLANNED of 419 criteria), migrations, and deploy config.
Coverage % = implemented-and-evidenced portion of the component's spec surface.
Statuses: **DONE** (code + tests + bound criterion), **PARTIAL** (code exists but
untested/stubbed/env-gated or incomplete surface), **GAP** (spec'd, no code found).

**Slices:** populated incrementally per domain below.

<!-- SLICES -->

## Slice 2.1 — Domain 1: Matching & Execution Core

**Verdict: ~88% code-complete (~78% evidence-weighted). DONE 9 · PARTIAL 3 · GAP 0.**
Zero stub markers in `core/`. Contracts: phase 01 5/5, phase 02 28/28, phase 02.5 2/3 EXECUTABLE.

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 1.1 | Limit Order Book | DONE | ~100% | `core/src/book/OrderBook.cpp`, `PriceLevel.cpp`, `utils/MemoryPool.hpp`; tests `test_order_book.cpp`, `test_book_pipette.cpp`; crit 1 bound |
| 1.2 | Deterministic matching | DONE | ~95% | `core/src/matching/MatchingEngine.cpp` (~4.4k ln), `WalWriter`, `recovery/RecoveryManager.cpp`; 29 gtest TESTs; crits 1–6, 10, 394 bound |
| 1.3 | Ingress queue / Aeron IPC + backpressure | DONE | ~90% | `core/src/ipc/{EnginePump,AeronChannel,SharedMemChannel}.cpp`, `EngineLoop.cpp` (80%-shed/95%-halt), Go mirror `services/internal/ipc/shm.go`; crit 299 bound |
| 1.4 | Price-time priority & tie-break | DONE | ~100% | `(price, ts_ns, ingress_seq)` FIFO in `OrderBook.hpp`; crits 1, 336 bound |
| 1.5 | Cross-shard 2PC / margin reservations | PARTIAL | ~70% | `CrossShardCoordinator.cpp` (~1.5k ln), `OptimisticShardCoordinator.cpp`, `risk/CrossShardMarginCoordinator.cpp`, `services/internal/risk/margin_coordinator.go`; crits 41, 118, 214, 404 bound |
| 1.6 | Atomic amend & keep-priority | DONE | ~95% | `OrderBook::modify_order`, `on_amend_received_ex`, `orders/service.go` CancelReplace; `test_amend.cpp`; crits 10, 336 |
| 1.7 | Scoped mass cancel & dead-man | DONE | ~95% | `orders/service.go:1478+`, `accounts/countdown.go`, `fix/deadman.go`, `ws/trading.go`; crits 153, 257, 260 bound |
| 1.8 | STP modes | DONE | ~100% | `matching/SelfTradeGuard.cpp`, `PreTradeChecker.cpp`; crits 4, 154, 274, 279, 368 bound |
| 1.9 | Execution rules (post/reduce-only, trade-through, collars, FAS) | DONE | ~90% | `TradeThroughGuard.hpp`, `ExecutionCollar.hpp`, `DiscretionaryExecutor.hpp` wired in engine; crits 277, 400, 405 bound |
| 1.10 | Slippage protection & banding | DONE | ~90% | market→synthetic-limit conversion, sparse-book guards in `MatchingEngine.cpp`; `test_book_protection.cpp`; crits 188, 220 bound |
| 1.11 | Liquidation auction matching | PARTIAL | ~70% | `risk/auction.go` (657 ln full phase machine), engine `auction_uncross`, `test_lifecycle_auction.cpp` (20 TESTs); crits 33–36 all PLANNED |
| 1.12 | OTR enforcement | PARTIAL | ~80% | `risk/otr_monitor.go` (548 ln, dual Go+C++ enforcement); crit 140 PLANNED |

**Done:** LOB (flat-array levels, intrusive FIFO, zero-heap pool); deterministic engine (journal-before-mutation, `on_time_tick` drives expiry/stops/auctions, byte-identical replay); real `aeron:ipc` + ABI-identical ShmRing fallback w/ poison-pill quarantine; full STP suite incl. TRANSFER; post_only/reduce_only/trade-through/collars/discretionary; market-order slippage conversion; multi-dim mass cancel + shared dead-man across REST/WS/FIX.
**Not done / gaps:**
1. **Cross-shard basket flow unwired** — ~2k lines of tested 2PC machinery but no Basket type in `core/proto/exchange.fbs`, no submission endpoint, zero refs from `MatchingEngine.cpp`/`main.cpp`; engine never consults `CrossShardMarginCoordinator` on pre-trade path.
2. **Perf evidence env-gated** — crits 11 (50k/s), 12 (p99≤50µs), 300 (72h soak) bound to short-run proxies or PLANNED; need dedicated benchmark host.
3. **Binding gap, not code gap** — crits 33–36 (liq auction), 140 (OTR), 142/401 (reopening auction), 152, 46 (kill-switches), all 20 Phase-16 crits PLANNED while implementing tests exist.
4. Minor: `EnginePump` amend decode doesn't forward `order_seq`/`gtd_expiry_ns`/`display_qty`/`trigger_source` (`MatchingEngine.hpp:99-102`, pending Task 5.3.22 routing).

## Slice 2.2 — Domain 2: Instrument & Market Admin

**Verdict: ~92% code-complete. DONE 6 · PARTIAL 0 · GAP 0.**
**All 10 Phase-15 §24 criteria PLANNED/unbound despite implementing tests** (spec-checks `phase15.go` bind 13 checkpoints).

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 2.1 | Instrument lifecycle (7 states, grace, sweeps) | DONE | ~95% | `admin/instrument_lifecycle.go`, engine `effective_status()/admission_gate()`, `orders/validate.go` mirror; `test_lifecycle_auction.cpp`; crits 119/290 PLANNED |
| 2.2 | Maker-checker maintenance / delisting | DONE | ~90% | `admin/instrument_maintenance.go` 3-stage chain, mig 219; FIX SecurityStatus republication deferred to Phase-18; crits 234/352 PLANNED |
| 2.3 | 24/5 trading hours + session lifecycle | DONE | ~90% | `admin/session_lifecycle.go`, `market_schedule.go`, core `ExpiryScheduler` DAY from session calendar; crit 217 PLANNED |
| 2.4 | Multi-currency holiday calendar | DONE | ~90% | `settlement/calendar_service.go`, mig 150 (8 centers 2025–2027); **crit 123 EXECUTABLE** |
| 2.5 | Ref-price collars / min notional / tick | DONE | ~90% | `PreTradeChecker.hpp`, `ExecutionCollar.cpp`, `orders/validate.go`; **crits 157, 277 EXECUTABLE** |
| 2.6 | Trade bust / adjust + reopening auction | DONE | ~90% | `admin/trade_busts.go`, `instruments/auction_calendar.go`, engine CALL/EXTEND/quarantine; crits 138/142/316 PLANNED |

**Done:** full lifecycle state machine w/ role matrix + dual control; SUSPENDED 5-min cancel-only sweep, RESTRICTED 24h, DELISTED 30-day reduce-only; weekly session machine w/ Fri close DAY/GTD cancel + Sun pre-open CALL uncross; mutual-business-day value dates; dual-layer pre-trade validation; bust/adjust w/ balanced GL + WAL-replay-convergent auction.
**Not done / gaps:** (1) FIX `SecurityStatus`/`TradingSessionStatus` republication on param change deferred to Phase-18; (2) dual session semantics (fixed-UTC control plane vs DST-aware 17:00 NY instrument calendar) — correct by design, no cross-check test; (3) all Phase-15 crits unbound.

## Slice 2.3 — Domain 3: Order Types

**Verdict: ~87% code-complete. DONE 8 · PARTIAL 2 · GAP 0.**
**All 22 Phase-16 §24 criteria PLANNED/unbound** despite 25 spec-checkpoints binding real gtest/gotest cases.

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 3.1 | Limit / market | DONE | 100% | `MatchingEngine.cpp`, `orders/validate.go`; crits 1, 2 EXECUTABLE |
| 3.2 | Stop / stop-limit / TP triggers | DONE | ~90% | `StopOrderTrigger.hpp`, trigger_source LAST/MARK/INDEX w/ stale-oracle fail-closed; TP = bracket child leg (no standalone enum — spec-consistent) |
| 3.3 | Trailing stops (PIPS/PCT/ABS) | PARTIAL | ~55% | engine complete (`OrderType::TRAILING_STOP`, ratchet, activation gate, WAL); **`SubmitRequest` lacks trailing fields, `orderNewMsg` never populates them, no REST/FIX route — unreachable from clients** |
| 3.4 | Bracket / OTO / OCO | DONE | ~90% | `orders/bracket.go`, `oco.go`, mig 225; crits 47/51 PLANNED |
| 3.5 | Iceberg / hidden / dark | DONE | ~90% | `DisplayQty` + replenish (crit 5 EXECUTABLE); hidden flag → `kOrderFlagHidden` midpoint-only; crits 197/198 PLANNED |
| 3.6 | TIF GTC/GTD/DAY/IOC/FOK | PARTIAL | ~85% | all TIFs + deterministic TIME_TICK expiry (crits 3, 394 EXECUTABLE); **R8 90-day GTC cap unimplemented — no clamp/sweeper/`GTD_EXPIRED`** |
| 3.7 | Pegged (Mid/Primary/Market) | DONE | ~90% | `peg_mode/offset/limit` wired end-to-end, `Phase16Peg.*` ×8; crit 395 PLANNED |
| 3.8 | TWAP/VWAP/VP + anti-gaming | DONE | ~85% | `algo/{twap,vwap,vp,antigaming}.go` (jitter/qty-perturbation, conservation); crits 48/49/275 PLANNED |
| 3.9 | OPO/OPOCO + grid + dual-price conditional | DONE | ~85% | `orders/orderlist.go`, `bots/engine.go`, `algo/raceguard.go`; crits 287/367/296/317 PLANNED |
| 3.10 | GSLO | DONE | ~85% | `algo/gslo.go` premium/refund/exposure cap, engine synthetic fill; crit 252 PLANNED; insurance-fund gap settlement lacks bound e2e |

**Top gaps:** (1) **trailing stops unreachable** — engine-done/gateway-gap (`SubmitRequest`/`orderNewMsg`/routes missing; `algo_params.offset` validated-then-dropped); (2) **90-day GTC cap (spec §27 R8) unimplemented and uncritiqued by matrix**; (3) Phase-16 traceability debt — 22/22 PLANNED.

## Slice 2.4 — Domain 4: Pricing & Liquidity Infra

**Verdict: ~83% code-complete. DONE 1 · PARTIAL 6 · GAP 0.**
Contracts: Phase-19.5 0/5, Phase-19 0/32 EXECUTABLE — entire oracle/margin domain unbound despite code+spec-checks; Phase-18 MM legs 19/19 EXECUTABLE.

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 4.1 | Mark price & index oracle | PARTIAL | ~95% | `oracle/{oracle,mark_price,publisher,aeron_sink}.go`, `feeds/http.go` (Refinitiv/BFIX/ECB adapters, MinFeeds=2); vendor feeds env-gated; crits 45/120/321 PLANNED |
| 4.2 | Staleness gates + fallback + flash breaker | PARTIAL | ~85% | `oracle/staleness*.go`, `risk/liquidation_fallback.go`, mig 236 (STALE_MARK/FLASH_COOL basis); crits 120/397 PLANNED |
| 4.3 | Yield-curve feeds | PARTIAL | ~90% | `oracle/rates/` 7 tenors, log-linear interp, ACT/360 vs ACT/365; crit 134 PLANNED |
| 4.4 | MTF / fair-value / fixing | PARTIAL | ~70% | `derivatives/curve.go` CIP, `algo/fixing.go` WMR/ECB/Tokyo, `fixing_scheduler.go`; **`FAIR_VALUE_DIVERGENCE`/`FIXING_WINDOW_CLOSED` codes spec-only, never emitted**; crit 401 PLANNED; spec §4515 "unwritten" row is stale |
| 4.5 | LP scorecard & management | PARTIAL | ~75% | `admin/lp.go` (~1000 ln), `marketdata/lp_*.go`; **`main.go:2819-24` wires nil MetricsSource/AlertSink — live scorecard pipeline + alerts unwired**; crit 227 EXECUTABLE |
| 4.6 | ADL ranking/execution | PARTIAL | ~80% | `risk/adl.go` (~1100 ln), wired into liquidation engine `main.go:4683-4714`; crits 97/269 PLANNED |
| 4.7 | MM program + MMP | DONE | ~95% | `marketmaking/{program,mmp,rebate,compliance}.go`, `fix/quoting.go` MMP lockout; **crits 128, 139 EXECUTABLE** |

**Top gaps:** (1) 37/37 Phase-19/19.5 criteria PLANNED — matrix lag, not code lag; (2) LP live-metrics pipeline unwired (nil sinks); (3) `FAIR_VALUE_DIVERGENCE`/`FIXING_WINDOW_CLOSED` absent from `errs/codes.go`.

## Slice 2.5 — Domain 5: APIs & Connectivity

**Verdict: ~67% code-complete (~60% weighted by FIX/SBE criticality). DONE 7 · PARTIAL 6 · GAP 0.**
Contracts: 85 unique crits tagged 05/06/18 → **84 EXECUTABLE / 1 PLANNED** (#99 SLA) — best-bound domain. **Caveat: several bound tests exercise code no production binary reaches.**

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 5.1 | REST API (orders/accounts/instruments/transfers/HMAC/idempotency) | DONE | ~90% | `gateway/routes_v1.go`, `api/handlers_*.go`, `middleware/idempotency.go`; `venue/info` still 501 stub; crits 363, 192, 258 bound |
| 5.2 | Public market-data WS | DONE | ~85% | `cmd/marketdata` mounts `/ws/v1/marketdata`, `ws_session_resume.go`; REST resync fallbacks (`market/depth`, `open-interest`, `market-data/snapshot`) are 501 stubs |
| 5.3 | Interactive trading WS | DONE | ~80% | `ws/trading.go`, auth upgrade/renewal, dedup, abuse, drain; crits 187, 253, 339 bound |
| 5.4 | SBE binary gateway / multicast | PARTIAL | ~40% | `internal/sbe/*` + `internal/fixsbe/*` fully tested; **no `cmd/` binary imports them — no SBE listener process exists; no deploy multicast config** |
| 5.5 | FIX 4.4 / 5.0 SP2 | PARTIAL | ~70% | `cmd/fix` live 4.4 acceptor (auth, resend/gap-fill, failover — crits 54–56, 189 bound); **SP2 settings never invoked — `NewGateway` calls only Acceptor/InitiatorSettings; no FIXT.1.1 listener** |
| 5.6 | FIXS mTLS + client certification | PARTIAL | ~45% | TLS1.3 acceptor works at CA level; **`ServerTLSConfig`/`VerifyConnection`/`WrapCertification` referenced only by tests — fingerprint→CompID binding + cert gate are dead code at runtime** |
| 5.7 | FIX mass quoting (35=i) | DONE | ~85% | `quoting.go`, `app_quoting.go`, MMP lockout, JetStream quote sink; crit 128 bound |
| 5.8 | Allocations 35=J/AK + PB drop copy + affirmation | PARTIAL | ~35% | `fix/allocation.go`, `dropcopy.go`, `pb_dropcopy.go`, `affirmation.go` tested; **`FromApp` has no J/AK case → MSGTYPE_UNSUPPORTED; `cmd/fix` instantiates zero allocation/drop-copy services** |
| 5.9 | Entitlement / CoD / session throttle | DONE | ~85% | `app.go` per-session throttle + instrument allowlist, CoD wired; crit 319 bound |
| 5.10 | Versioning / deprecation / rate tiers / headers | DONE | ~85% | `middleware/versioning.go`, `deprecation/` (183d min→410), rate tiers; crits 192/208/258/284/289 bound; no second live major demonstrated |
| 5.11 | Dead-man switch (cancel-all-after) | PARTIAL | ~60% | `accounts/countdown.go` canonical timer shared REST/WS/FIX *service-wise*; **REST endpoints `countdown-cancel-all`/`cancel-all-after` are 501 stubs — `accounts.Handler.Mount()` never invoked** |
| 5.12 | Smart Order Router | PARTIAL | ~30% | `internal/sor/*` complete+tested (crits 193/194 bound); **zero production imports — `orders.Service` never consults a router** |
| 5.13 | API gateway / LB (HAProxy L7) | DONE | ~85% | `deploy/haproxy/haproxy.cfg` TLS1.3, blue-green map flip, health-gated backends, drill harness w/ recorded results |

**Top gaps:**
1. **Unreachable Phase-18 FIX sub-surfaces** — SP2 listener absent, mTLS fingerprint binding + cert gate never invoked, 35=J/AK not dispatched, no drop-copy/affirmation services instantiated (`fix/gateway.go:41-70`, `fix/app.go:316-344`, `cmd/fix/main.go:340-370`).
2. **No SBE/FIXS binary or multicast deployment; `internal/sor` has zero callers** — tested libraries, absent production surface.
3. **19 `StatusStub` routes shadow implemented handlers** — dead-man REST endpoints, `positions/close-all`, market resync fallbacks, `venue/info` return 501 despite `accounts.Handler` existing (`pentest/evidence.jsonl` records real 501s).

## Slice 2.6 — Domain 6: Market Data Products

**Verdict: ~86% code-complete. DONE 4 · PARTIAL 4 · GAP 0.**
Contracts: Phase-06 25/26 EXECUTABLE; Phase-17 0/3; Phase-23 1/8.

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 6.1 | L2 depth | DONE | ~95% | `marketdata/l2.go` (100ms conflation, CRC, seq contiguity); engine `BookSerializer.cpp` |
| 6.2 | L3 order-level data | PARTIAL | ~85% | `ipc/L3Publisher.cpp` (alloc-free SPSC, pseudonym hash verified), `marketdata/l3.go` + `l3_snapshot.go`; crits 18/244/318 PLANNED |
| 6.3 | OHLCV klines | DONE | ~95% | `marketdata/ohlcv/` 13 TF, CH persistence; **crits 66, 226 EXECUTABLE** |
| 6.4 | Tick/trade history + block tape | PARTIAL | ~85% | `trades.go`, `aggtrades.go`, `tick_data_api.go`, `export*.go`, `block_trades.go` (15min deferral); **crits 262, 291 EXECUTABLE**; crit 236 PLANNED |
| 6.5 | Book ticker / BBO | DONE | ~95% | `bbo.go` zero-conflation BBO from book deltas; **crit 261 EXECUTABLE** |
| 6.6 | Liquidation feed | PARTIAL | ~55% | `liquidations.go` consumer+delay correct, **crit 263 EXECUTABLE — but no producer: risk engine never publishes `margin-events`; `liquidations@`/`auctions@` are empty shells fed only by manual-liquidation sink** |
| 6.7 | Subscriptions + WS session resume | DONE | ~95% | `channels.go` grammar/budgets, `ws_session_resume.go` ring-buffer replay + resync; multiple Phase-06 crits EXECUTABLE incl. 408 |
| 6.8 | Data SLA + SBE A/B multicast + TCP replay | PARTIAL | ~80% | `internal/sbe/*` full A/B+replay+snapshot; **crits 166, 284 EXECUTABLE**; crit 99 (p99≤100ms/99.95% SLA) PLANNED — env-gated |

**Top gaps:** (1) **missing `margin-events` liquidation producer** — feed transport correct but empty (Phase-19 landed without it); (2) SLA crit 99 has instrumentation, no verified conformance (env); (3) Phase-17/23 contract lag — 10 crits PLANNED w/ green spec-checks.

## Slice 2.7 — Domain 7: Risk & Credit

**Verdict: ~76% code-complete. DONE 6 · PARTIAL 5 · GAP 0.**
Contracts: **Phase-19 0/31, Phase-19.5 0/5, Phase-13 0/7 EXECUTABLE — entire risk domain unbound** (spec-checks `phase19.go` verify structure, not wiring). Phase-03 19/19.

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 7.1 | Margin modes (cross/isolated/portfolio, 90d corr, cross-shard) | PARTIAL | ~75% | `risk/margin*.go`, `correlation_offset.go`, `margin_coordinator.go`, `CrossShardMarginCoordinator.cpp`, `cmd/risk`; **corr refresh unwired (no ReturnSeriesSource), C++ margin coordinator has no engine driver — `main.cpp` never emits MARGIN_RESERVE_REQ; admission margin = static MaxLeverage division, not engine result** |
| 7.2 | Leverage tiers (ESMA/CFTC/notional bands) | DONE | ~90% | `risk/leverage.go`, `leverage_tiers.go`, mig 234; not consulted per-order at admission |
| 7.3 | Liquidation ladder | DONE | ~85% | `liquidation.go` (2s scanner), `auction.go` (CALL/EXTEND/FORCE_CASH), `margin_call.go`, ADV slicing, 0.05% LP rebate; crits 32–37, 241, 320 PLANNED |
| 7.4 | Retail NBP | DONE | ~90% | `risk/nbp.go`, insurance-fund GL pairs, P0 on insufficiency; crits 133/228 PLANNED |
| 7.5 | Insurance fund + governance | PARTIAL | ~75% | `insurance_fund.go`, `adl.go`, `model_validation.go` fund-target formula; **no automated replenishment sweep; "VaR sizing" = stress-shortfall not VaR** |
| 7.6 | Stress testing & backtesting | PARTIAL | ~65% | `stress_engine.go`, `model_validation.go`, schedulers wired; **Kupiec POF/Christoffersen absent; FlashCrashProvider bound nil in prod** |
| 7.7 | PB credit (NOP/DSL) | PARTIAL | ~40% | `pb_credit.go` full service + restitution; **`NewPBCreditService` never constructed in any cmd; no admission gate; "implements orders.PBCreditGate" comment references nonexistent interface** |
| 7.8 | Collateral haircuts & concentration | DONE | ~85% | `collateral*.go`, `volatility_scaler.go`, `CollateralMonitor`; vol source = in-memory ring only (no CH tick history) |
| 7.9 | Position/OTR/exposure limits | DONE | ~90% | `limits_service.go`, `exposure.go`, `otr_monitor.go`; **C++-side enforcement only engages with `-redis` flag — shipped systemd unit passes neither `-redis` nor `-dev-all-accounts`** |
| 7.10 | Bilateral credit groups | PARTIAL | ~55% | `bilateral_credit.go`, `BilateralCreditMatrix.cpp`, shm publication + divergence verify; **`ReserveMatch` has no production caller — mutual-credit gate dead on both Go-admission and C++-match paths** |
| 7.11 | Netting/hedging + margin-level thresholds | DONE | ~85% | `position_mode.go`, `margin_level.go` (retail 120/100/50), `MarginLevelWatcher` |

**Top gaps:**
1. **Engine-side risk dormant in shipped config** — `main.cpp` binds `DevAccountState` only under `-dev-all-accounts`; systemd unit passes neither flag nor `-redis`. `engine_risk_check` check-1 would reject every order `ACCOUNT_INACTIVE`; halt/OTR/sanctions checks, `IMarginCheck`, `bind_credit`, `CrossShardMarginCoordinator` all unbound. **All production risk decisions are Go-gateway-side only.**
2. PB NOP/DSL and bilateral `ReserveMatch` unwired (above).
3. Refresh gaps: correlation matrix never refreshes, flash-crash provider nil, no Kupiec/Christoffersen; 36 Phase-19/19.5/13 contract rows PLANNED.

## Slice 2.8 — Domain 8: Compliance & AML

**Verdict: ~87% code-complete. DONE 14 · PARTIAL 0 · GAP 1 (sub-gap).**
~95 files / ~25k+ lines in `services/internal/compliance` + `surveillance`; 226 Go test funcs; `core/tests/test_sanctions.cpp`. **Traceability hole: all 47 Phase-14/17/21 criteria PLANNED with zero bindings despite real tests.**

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 8.1 | KYC tiers/lifecycle/categorization/closure/holds | DONE | ~90% | `compliance/kyc.go`, `kyc_lifecycle.go`, `categorization.go`, `accounts/closure.go`, `hold_workflow.go` |
| 8.2 | PEP & adverse media + monitoring | DONE | ~85% | `screening.go` (838 ln), `monitoring.go`; vendor seam nil-injected (manual intake) |
| 8.3 | Sanctions + C++ pre-trade hook + scoped degradation | DONE | ~90% | `sanctions.go` (real OFAC/EU/UN/HMT parsers), `SanctionsCache.cpp`, `quarantine.go`; public-URL fetch not licensed-provider API |
| 8.4 | Travel Rule (IVMS 101) | DONE | ~90% | `travel_rule.go` (679 ln); no counterparty-VASP messaging (fiat wires carry data) |
| 8.5 | SAR/CTR | DONE | ~80% | `sar.go`, `sar_signals.go`, `aml.go`; no BSA e-file XML — `filing_ref` officer-entered |
| 8.6 | MiFID II (RTS 6/22/27/28, APA/ARM, taping) | DONE | ~90% | `rts6.go`, `reporting/service.go`, `rts27_report.go`, `rts28_report.go`, `comms_recording.go` (WORM); APA/ARM HTTP adapters never exercised vs live endpoint |
| 8.7 | EMIR REFIT + CFTC 43/45 | DONE | ~85% | `reporting/` lifecycle engine, `identifiers.go` (LEI mod-97); **UPI is deterministic local placeholder** pending DSB API |
| 8.8 | Dodd-Frank / FinCEN MSB | DONE | ~80% | `aml.go` program-artifact register; registration itself is human process |
| 8.9 | GDPR + data residency | DONE | ~85% | `gdpr.go` (1078 ln), `data_residency.go`, `GeoGate` wired `main.go:3897`; residency is policy layer, no physical region partitioning |
| 8.10 | Abuse surveillance + case mgmt | DONE | ~85% | `surveillance/signals.go` (7 detectors), `enforcement.go`, `cases.go`, `cmd/compliance` durable consumer |
| 8.11 | CRS/FATCA | DONE | ~90% | `tax_reporting.go`, `crs_xml.go`, `fatca_xml.go` golden-pinned; no transmission channel (by design) |
| 8.12 | Basel III | DONE | ~85% | `basel.go` (560 ln, GL-derived T1/T2); RWA is documented approximation |
| 8.13 | FX Global Code 55 | DONE | ~85% | `fx_global_code.go` (901 ln); only 4 principles machine-attested, rest officer verdicts |
| 8.14 | Geo-blocking | DONE | ~90% | `GeoGate` BLOCK/RETAIL_BLOCK/ALLOW, fail-closed, wired into API chain |

**Top gaps:**
1. `SURVEILLANCE_LAG_WARNING` (crit 392 / Phase-21 AC 72) — consumer-lag >10K alert + auto-scaling + detection-latency SLA **not implemented anywhere**; the only true functional gap.
2. Contract bindings absent for the entire domain — action: bind `internal/compliance` + `test_sanctions.cpp` to crits 27, 101–108, 132, 149–151, 169–170, 174, 178, 182, 184, 195–196, 202–204, 207, 219, 244, 249, 273, 315, 318, 323, 327–328, 333, 345, 369, 377, 392 + Phase-17 L3 items.
3. External integrations are honest fail-closed seams (KYC IDV vendors, adverse-media vendor, DSB UPI, APA/ARM/TR/SDR live endpoints) — spec §26 records them as interface-only.

## Slice 2.9 — Domain 9: Trade Lifecycle Ops

**Verdict: ~88% code-complete. DONE 11 · PARTIAL 1 · GAP 0.**
All settlement/funding/backoffice daemons live inside the `cmd/gateway` monolith (`main.go` ~2680–3750, 5407–6308) — `cmd/settlement` is a healthz-only scaffold. Contracts: Phase-03 19/19 EXECUTABLE; **Phase-24 0/19 EXECUTABLE despite existing `phase24_tasks_test.go`.**

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 9.1 | Settlement cycles / value-date engine | DONE | ~90% | `settlement/settlement_service.go` (1432 ln), `calendar_service.go`; ISDA Mod-Following, MT202/pacs.009 render; `NullDispatcher` default |
| 9.2 | Banking rails + cut-offs + fees + beneficiary registry + statements | PARTIAL | ~70% | `funding/rails.go` + per-rail builders, `rail_cutoff_service.go`, `fee_schedule.go`, `deposit_guard.go`, `statement_parser.go` (1151 ln); **`RailTransport` unwired — payments persist PREPARED, fail-closed** |
| 9.3 | CLS PvP + bunched allocations | DONE | ~85% | `cls_pvp.go` (1389 ln), `allocation_engine.go` (1542 ln); `ClsMemberAdapter` seam nil→`CLS_MEMBER_UNAVAILABLE` |
| 9.4 | Nostro/vostro + correspondent recon | DONE | ~90% | `funding/nostro.go`, `backoffice/nostro.go`, `reconciliation.go` (887 ln); PB affirmation/blotter feeds are seams |
| 9.5 | Double-entry GL (zero-bypass) | DONE | ~95% | `ledger/posting.go`, `chart.go`, `settlement/ledger_service.go` (SERIALIZABLE, mutex, idempotent) |
| 9.6 | Tom-Next rollover + swap engine + pip calc + commission | DONE | ~95% | `rollover_service.go`, `swap_engine.go`, `pip_calculator.go`, `commission_engine.go`, `vip_engine.go` |
| 9.7 | Confirmations/statements/MT515/portal | DONE | ~85% | `analytics/statements.go`, `reporting/confirmation_service.go`, `mt515.go`; `LogMT515Submitter` (log-only transport) |
| 9.8 | TCA | DONE | ~90% | `analytics/tca_engine.go`, `tca_report.go`; arrival = oracle-mid approximation (documented) |
| 9.9 | Block allocations (FIX J/AK give-up) | DONE | ~90% | `fix/allocation.go`, `backoffice/allocation_engine.go`, `pb_dropcopy.go`, `affirmation.go` |
| 9.10 | CSDR discipline + PB credit restitution | DONE | ~90% | `csdr_discipline.go`, `buyin.go`, `credit_restitution.go`, `ops_hardening.go` |
| 9.11 | Position transfers | DONE | ~95% | `settlement/position_transfer.go` (923 ln); `TestE2E_InternalTransfer` bound |
| 9.12 | Trade busts / price-adjust | DONE | ~90% | `admin/trade_busts.go` (1475 ln), reopening auction; mig 051 |

**Top gaps:** (1) no live bank-rail transport — `RailTransport`/settlement dispatcher unwired (all fail-closed `BANKING_RAIL_UNAVAILABLE`); (2) CLS member adapter, PB affirmation/blotter feeds, MT515 submitter, ERP delivery are seams; (3) Phase-24 criteria zero contract bindings — evidence gap only.

## Slice 2.12 — Domain 12: Client Experience

**Verdict: ~78% code-complete. DONE 7 · PARTIAL 6 · GAP 0.**
63 vitest specs; coverage-summary 88.9% lines / 74.1% branches; only 2 Playwright e2e specs. Contracts: 49 criteria tagged 07/10/12/14 → 9 EXECUTABLE; all pure-frontend crits PLANNED despite vitest suites.

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 12.1 | Trader UI Pro | DONE | ~95% | `features/workspace/WorkspacePage.tsx`, `order-entry/OrderEntry.tsx`, `AdvancedOrderPanel.tsx`, `order-book/`; live-stack e2e `trade.spec.ts` |
| 12.2 | Trader UI Lite | DONE | ~90% | `LiteDashboard.tsx`, `ModeToggle.tsx`, persisted `liteMode.ts` |
| 12.3 | Calculators | PARTIAL | ~70% | `features/calculator/` (notional/margin/pip/liq-estimate, STALE badging); missing swap/rollover, standalone P&L, position-size calc (crit 267 PLANNED) |
| 12.4 | Chart trading | DONE | ~90% | `charts/TradingChart.tsx` (UDF, 13 TF) + `advanced-orders/TradingChart.tsx` (draggable order lines→cancel-replace); no drawing tools/indicator overlays on main chart |
| 12.5 | Backtesting UI | DONE | ~90% | `backtesting/BacktestPage.tsx`, `lib/backtest/engine.ts`, `lib/indicators/`; causal fills, cost model, disclosures |
| 12.6 | Admin console UI | PARTIAL | ~65% | `features/admin/` (health, users, support queue, dual-control, audit, instruments lifecycle), `features/ops/`; missing KYC-review, webhook-DLQ, LP, RBAC, compliance-hold, margin consoles |
| 12.7 | Auth (TOTP/WebAuthn/anti-phish) | DONE | ~90% | `internal/auth/` (webauthn 515 ln, totp, sessions, lockout), full settings UI; delegated-users M-of-N UI missing (crit 285 PLANNED) |
| 12.8 | KYC portal | DONE | ~85% | `features/kyc/` UploadWizard+StatusTracker → live routes; admin review queue UI absent |
| 12.9 | Notifications | PARTIAL | ~65% | `internal/notifications/` full pipeline (prefs, quiet hours, retry→DLQ); **providers are dev seams only (no SES/Twilio/FCM); no frontend inbox — `private:notifications` never subscribed** |
| 12.10 | Webhooks | PARTIAL | ~60% | `internal/webhooks/` (693 ln, AES-256-GCM secrets, HMAC dispatch, DLQ) live; **zero frontend** |
| 12.11 | Copy trading | DONE | ~85% | `internal/copy/` engine (pro-rata fanout, dedup, HWM profit share), `features/copy-grid/`; `unfollowRegistered()` hardcoded false — stale flag hides live DELETE route |
| 12.12 | PAMM/MAM | PARTIAL | ~50% | `internal/pamm/` service+engine+allocation live w/ routes; **no frontend at all** |
| 12.13 | Support & self-service | DONE | ~85% | `internal/support/tickets.go` (965 ln), SupportPage/TicketDetail, settings center, ApiKeysPanel; attachments via JSON field; reopen hits unregistered route |

**Top gaps:** (1) PAMM/MAM zero UI + copy unfollow disabled by stale flag; (2) webhooks UI + notification inbox absent — built pipelines unreachable from SPA; (3) admin console breadth (KYC queue, DLQ viewer, LP, RBAC) + stale "stub/501" comments in `api.ts` files (routes now `v1live` — doc drift).

## Slice 2.13 — Domain 13: Governance & Business

**Verdict: ~82% code-complete. DONE 9 · PARTIAL 1 · GAP 1.**
Contracts: Phase-07 8/8 EXECUTABLE; Phase-11 0/14, Phase-14 0/15, Phase-20 ~1/15 EXECUTABLE (unbound, not unimplemented).

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 13.1 | Treasury / own funds | DONE | ~90% | `backoffice/treasury.go`, `contingent_capital.go`; liquidity breach freeze, `AdmissionSatisfied` seam |
| 13.2 | Client-money safeguarding | DONE | ~95% | `client_money.go` (2747 ln), `segregation_cert.go`, shortfall 4-tier waterfall |
| 13.3 | Insurance/reinsurance + fund governance | DONE | ~85% | `risk/insurance_fund.go`, `contingent_capital.go`; reinsurance = register-level rows, no reinsurer integration |
| 13.4 | Internal audit trail | DONE | ~85% | `audit/HashChain.go`, `merkle.go`, `cmd/exchange/verify_audit.go`; rows anchored via shared chain not content-bound prev_checksum (documented); no audit-plan/findings workflow |
| 13.5 | Employee dealing controls | DONE | ~90% | `compliance/employee_dealing.go` (729 ln) pre-clearance + blackouts at gateway admission |
| 13.6 | Regulatory change monitoring | DONE | ~90% | `regulatory_change.go` (1154 ln) TRACKED→CLOSED, SLA sweeps; manual entry (no regulator feed) |
| 13.7 | License/entity governance | DONE | ~85% | `compliance/venue/venue.go` (1286 ln) admission/product gates, `JURISDICTION_UNLICENSED` fail-closed |
| 13.8 | Incident comms / P0–P3 / status page | DONE | ~85% | `incident/` (manager, matrix, warroom), `operations/dora/`, `ops/status_exporter.go`; PagerDuty/chat are env-gated seams |
| 13.9 | Vendor / ICT third-party risk | GAP | ~25% | docs-only (`docs/ops/dora-third-party-register.md`); **no register tables, no concentration engine, no audit DB, no renewal scheduler, no exit-strategy tracking** |
| 13.10 | Client education / support / complaints | PARTIAL | ~75% | `support/tickets.go` (965 ln, ADR, SLA breach), `content/promotion_gate.go`; education = static curated help only, no CMS |
| 13.11 | Finance reporting (TB, P&L/BS, ERP, multi-ccy) | DONE | ~90% | `analytics/trial_balance.go`, `erp_export.go`, `income.go`, `position/pnl_converter.go`; ERP transport is interface-shaped |

**Top gaps:** (1) DORA Art. 28 vendor/ICT register — procedures documented, zero code; (2) no internal-audit plan/findings workflow; (3) education CMS absent; paging providers env-gated.

## Slice 2.10 — Domain 10: Recovery & Resilience

**Verdict: ~88% code-complete. DONE 9 · PARTIAL 3 · GAP 0.**
Committed run evidence: D3 archive-replay drill PASS (5 segs, 11.47M trades, sha256 byte-parity), chaos suite s1–s6 18/18 PASS, Redis failover drill PASS (RTO ~1.2s/RPO 0).

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 10.1 | WAL + recovery ladder + S3 archive/replay | DONE | ~95% | `core/src/wal/` (O_DIRECT 4KB flush, CRC32C, torn-tail), `recovery/RecoveryManager.cpp`, `cmd/wal-recovery`, `cmd/exchange/archive.go`, `archive_service.go` |
| 10.2 | PG reconciliation vs engine | DONE | ~95% | `reconciliation/` 9-category sweep engine, `recon_check_book.go`, `reconcile.go`; trades leg → INCONCLUSIVE until trades-projection writer lands |
| 10.3 | Multi-region DR | PARTIAL | ~70% | `dr_drill_runner.sh` (D1–D7), `deploy/dr/failover-runbook.sh`, DR orchestrator steps; **D1 end-to-end needs live secondary region — SKIP, not faked** |
| 10.4 | Chaos validation | DONE | ~95% | `tests/chaos/` 6 scenarios + tool; `chaos_splitbrain_test.go`, `chaos_contention_test.go` |
| 10.5 | 5-tier circuit breaker + degradation modes | DONE | ~90% | `risk/circuit_breaker.go` (1251 ln, exact §2.6 scopes), `core/src/degradation/ModeManager.cpp` (6 modes); account-loss/IV feeds are documented Phase-19/22 seams |
| 10.6 | Graceful shutdown/drain | DONE | ~95% | `middleware/shutdown.go`, `fix/drain.go` (News+Logout fan-out), `ws/drain.go` (retry_after + reconnect hints); crit 216 bound |
| 10.7 | DORA ICT resilience | DONE | ~85% | `operations/dora/` (gated close, 4h/72h/1mo reports); crit 171 PLANNED |
| 10.8 | Edge WAF/DDoS | DONE | ~80% | `deploy/haproxy/haproxy.cfg`, `deploy/waf/` CRS rules, `waf_drill.sh` real ModSecurity test; managed L3/4 provider pending-infra |
| 10.9 | Secrets (Vault/KMS) + drill | DONE | ~85% | `security/rotation.go` (1362 ln, real Vault KV-v2 client), `cmd/secretdrill`; KMS adapter absent (Vault-first seam); Vault revoke leg env-bound |
| 10.10 | Redis Sentinel HA + PTP + health | DONE | ~90% | sentinel topology + `redis/sentinel.go` split-brain guards, `timesync/{ptp,ptpmon,guard}.go`, `api/health.go` |
| 10.11 | Merkle solvency scheduler | DONE | ~85% | `reconciliation/merkle_tree.go`, `cmd/exchange/solvency.go`, daily cron; crit 186 PLANNED; GPG env-bound |
| 10.12 | Incident P0–P3 + escalation | DONE | ~85% | `incident/` matrix+manager+warroom, ~40 runbooks; crit 183 PLANNED; PagerDuty env-gated |

**Top gaps:** (1) D1 multi-region cutover unproven without a live secondary; (2) 72h@50k/s soak evidence pending dedicated host; (3) implemented features unbound in contracts.json (crits 160, 171, 179, 183, 186, 212, 213, 300).

## Slice 2.11 — Domain 11: Engineering & Delivery

**Verdict: ~90% code-complete. DONE 7 · PARTIAL 2 · GAP 0.**
Test base: 573 gtest TESTs (core, 36 files) · ~3,581 Go test funcs (services) · ~103 (tests/) · 63 vitest specs · 542/542 spec checkpoints bound · 419 §24 contracts.

| # | Component | Status | Cov | Key evidence |
|---|---|---|---|---|
| 11.1 | CI/CD pipelines | DONE | ~95% | `.github/workflows/ci.yml` (531 ln, spec-validation ×4 shards, `SPEC_FAIL_ON_SKIP`, traceability gate), `security.yml` (5 fail-closed jobs); hosted CI 10/10 + 5/5 green |
| 11.2 | Spec validation harness | DONE | ~95% | `tests/spec/` registry+validator, `checkpoints.json` 542, `traceability` 419/419 mapped 0 defects, `contractgen` + integrity tests |
| 11.3 | 72h@50k/s soak / p99≤50µs | PARTIAL | ~70% | harness complete (`tests/soak/monitor.sh` 648 ln, `loadgen.go`, `wal_audit.cpp`); committed 8h@15k/s PARTIAL report w/ real defects found+fixed; **literal 72h env-bound; post-restart throughput ~1.8k/s open defect** |
| 11.4 | 75k/s×4h staging gate | PARTIAL | ~70% | `tests/load/run.sh` 3 legs + committed run1 report; artifact contract `staging-report.json` + armed checker `ckP085StagingGate`; run itself env-bound; crit 308 PLANNED |
| 11.5 | Blue-green deploy + rollback | DONE | ~90% | `deploy/scripts/bluegreen.sh` (fail-closed synthetic-order probe), `rollback.sh`, `bluegreen_gate_test.sh` wired in CI; crit 309 bound; real cluster cutover env-bound |
| 11.6 | Capacity planning | DONE | ~85% | `operations/capacity/` model+generator+PG sink, alerts, SLO policy; quarterly cron is documented not committed |
| 11.7 | Hot/warm/cold tiering | DONE | ~90% | `archiver/` lifecycle (PG→CH→parquet+WORM S3, verify, holds), `cmd/archiver`, `retention/`, Trino federation; crit 179 PLANNED |
| 11.8 | Supply-chain scanning | DONE | ~95% | CodeQL, govulncheck, npm audit, `cpp_dep_audit.sh`, `dep_cooldown.sh` ≥7d, Trivy, gitleaks, SBOM, pentest surface (375 routes) |
| 11.9 | Test infra | DONE | ~90% | full pyramid incl. `tests/integration` live-stack self-gating, `ci/fault-injection/` harness; 256 PLANNED bindings = largest honest backlog |

**Top gaps:** (1) 72h soak + staging-gate evidence need dedicated hosts; (2) 256 PLANNED contract bindings — code mostly exists, bindings don't; (3) post-crash-restart throughput degradation (~1.8k/s vs 15k/s) — documented open perf defect.

<!-- /SLICES -->

## Phase 2 Summary — Domain Matrix & Cross-Cutting Metrics

| # | Domain | Components | DONE | PARTIAL | GAP | Coverage % | §24 crits EXEC/PLANNED (owner phases) |
|---|---|---|---|---|---|---|---|
| 1 | Matching & Execution Core | 12 | 9 | 3 | 0 | ~88% | 35/1 (ph 01/02/02.5) |
| 2 | Instrument & Market Admin | 6 | 6 | 0 | 0 | ~92% | 0/10 (ph 15) |
| 3 | Order Types | 10 | 8 | 2 | 0 | ~87% | 0/22 (ph 16) |
| 4 | Pricing & Liquidity Infra | 7 | 1 | 6 | 0 | ~83% | 0/37 (ph 19/19.5) |
| 5 | APIs & Connectivity | 13 | 7 | 6 | 0 | ~67% | 84/1 (ph 05/06/18) — best-bound, but see unwired-surface caveat |
| 6 | Market Data Products | 8 | 4 | 4 | 0 | ~86% | 26/11 (ph 06/17/23) |
| 7 | Risk & Credit | 11 | 6 | 5 | 0 | ~76% | 0/43 (ph 13/19/19.5); ph 03 19/19 |
| 8 | Compliance & AML | 14 | 13 | 0 | 1 | ~87% | 0/47 (ph 14/17/21) |
| 9 | Trade Lifecycle Ops | 12 | 11 | 1 | 0 | ~88% | 19/33 (ph 03/11/24 + others) |
| 10 | Recovery & Resilience | 12 | 9 | 3 | 0 | ~88% | mixed; 8+ PLANNED binding gaps |
| 11 | Engineering & Delivery | 9 | 7 | 2 | 0 | ~90% | harness EXECUTABLE; perf gates env-bound |
| 12 | Client Experience | 13 | 7 | 6 | 0 | ~78% | 9/40 (ph 07/10/12/14) |
| 13 | Governance & Business | 11 | 9 | 1 | 1 | ~82% | 9/~40 (ph 07/11/14/20/24) |
| — | **TOTAL** | **138** | **97** | **39** | **2** | **~83% weighted** | **163/256 of 419** |

**Aggregate repo metrics:** ~34.7k LoC C++ (core) · ~483k LoC Go (services, incl. tests) · ~56k LoC TS (frontend) · 211 migration pairs · 573 gtest TESTs · ~3,581 Go test funcs · 63 vitest specs (88.9% line cov) · 2 Playwright e2e specs · 542/542 spec checkpoints bound · contracts.json 163 EXECUTABLE / 256 PLANNED · ~zero TODO/`panic("not implemented")` markers in shipped code.

### Cross-cutting findings (apply repo-wide)

1. **Two evidence tracks disagree.** `tests/spec/checks/phase*.go` (542 checkpoints, CI-gated) bind real tests for nearly everything; `contracts.json` (the §24 matrix) shows 256 PLANNED — mostly **binding debt, not missing code**. Phases 10–24 are almost entirely unbound. Highest-leverage remediation: promote existing tests into `itest/bindings.go` (Phase-2.x follow-up).
2. **EXECUTABLE ≠ reachable.** Several bound tests exercise libraries no binary imports: `internal/sbe`, `internal/fixsbe`, `internal/sor`, FIX SP2 settings, mTLS fingerprint binding + cert gate, 35=J/AK dispatch, cross-shard basket flow, PB credit service, bilateral `ReserveMatch`. Contracts prove code quality, not production wiring — a second audit axis ("wired") is needed.
3. **Engine-side risk dormant in shipped config** — the systemd unit passes neither `-redis` nor `-dev-all-accounts`, leaving C++ halt/OTR/sanctions/margin/credit checks unbound; all production risk is Go-gateway-side. Biggest single-surface gap.
4. **Universal last-mile pattern:** domain logic is real and fail-closed; the consistent gap is external transport — bank rails (`RailTransport` nil), CLS member adapter, PB affirmation feeds, MT515 submitter, APA/ARM/TR/SDR endpoints, KYC IDV vendors, SES/Twilio/FCM, PagerDuty — all documented seams per spec §26, env-gated by design.
5. **True functional gaps (spec'd, no code):** `SURVEILLANCE_LAG_WARNING` (crit 392); R8 90-day GTC cap; DORA Art. 28 vendor/ICT register; `FAIR_VALUE_DIVERGENCE`/`FIXING_WINDOW_CLOSED` codes; `margin-events` liquidation producer (feeds empty); trailing-stop submit path (engine done, gateway missing); Kupiec POF/Christoffersen tests; automated insurance-fund replenishment.
6. **Unreachable client surfaces:** 19 `StatusStub` routes shadow working handlers (dead-man REST, `positions/close-all`, market resync, `venue/info`); PAMM/webhooks/notifications have no frontend; copy `unfollowRegistered()` stale flag; admin lacks KYC-review/DLQ/LP/RBAC consoles.
7. **Env-gated evidence** (matches AGENTS.md backlog): 72h@50k/s soak, 75k/s×4h staging gate, D1 multi-region failover, managed WAF, live third-party accounts. Open perf defect: post-restart throughput ~1.8k/s vs 15k/s.
8. **Doc drift to fix:** `docs/PROJECT_COMPLETENESS_ASSESSMENT.md` and `docs/SLICE_3_ASSESSMENT.md` still claim 0% code (stale); spec ~§4515 claims fixing scheduler "unwritten" (it exists); stale "stub/501" comments in several frontend `api.ts` files whose routes are now `v1live`; `cmd/settlement` is a healthz-only scaffold while all daemons run in `cmd/gateway`.

### Completeness Checklist
- [x] All 13 domains assessed against physical code (not docs) via 9 parallel slice agents
- [x] Per-component status (DONE/PARTIAL/GAP) + coverage % + evidence paths + done/not-done
- [x] Aggregate metrics: LoC, test counts, contract binding stats per phase, stub-marker sweep
- [x] Cross-cutting findings recorded once, not duplicated per-slice
- [x] Stale prior-assessment docs flagged (supersession noted, not silently diverged)

---

# Phase 3 — Implementation plan: close wiring gaps, true gaps, and binding debt

**Status: IMPLEMENTED (2026-10-02).** All task checklists below are ticked and carry
implementation notes. (supersedes the prior "PLANNED — Not yet implemented" header)

**Origin:** Phase 2 code-truth assessment (above). Findings to fix, grouped by class:

1. **Dormant engine-side risk** — shipped `deploy/systemd/matching-engine@.service` passes
   neither `-redis` nor `-dev-all-accounts`; `main.cpp` binds `DevAccountState` only under
   the dev flag, so C++ halt/OTR/sanctions/margin/credit checks are all unbound and
   `engine_risk_check` check-1 would reject every order if reached. All production risk
   is currently Go-gateway-side only.
2. **Unreachable tested surfaces** — `internal/sbe`, `internal/fixsbe`, `internal/sor`
   have zero `services/cmd` imports; FIX SP2 settings, mTLS fingerprint binding,
   `WrapCertification`, and 35=J/AK dispatch never invoked; `CrossShardCoordinator`
   machinery has no basket wire type or ingress; `NewPBCreditService` and bilateral
   `ReserveMatch` have no production callers.
3. **19 `StatusStub` routes shadow implemented handlers** — `accounts.Handler.Mount()`
   never invoked (dead-man REST endpoints 501); `positions/close-all`, market resync
   fallbacks, `venue/info` stubbed.
4. **True spec'd-but-absent code** — `SURVEILLANCE_LAG_WARNING` (crit 392); §27 R8
   90-day GTC cap; DORA Art. 28 vendor/ICT register; `FAIR_VALUE_DIVERGENCE` /
   `FIXING_WINDOW_CLOSED` codes; `margin-events` liquidation producer; Kupiec POF /
   Christoffersen tests; automated insurance-fund replenishment sweep.
5. **Frontend gaps** — no PAMM/webhooks/notification-inbox UI; admin lacks KYC-review,
   DLQ, LP, RBAC consoles; copy `unfollowRegistered()` stale flag; calculator set
   incomplete (swap/P&L/position-size).
6. **Binding debt** — 256 PLANNED contract rows; the owning tests largely exist
   (`test_phase16.cpp`, `phase24_tasks_test.go`, `internal/compliance` suite, vitest).
7. **Doc drift** — `PROJECT_COMPLETENESS_ASSESSMENT.md`/`SLICE_3_ASSESSMENT.md` claim
   0% code; spec ~§4515 claims fixing scheduler unwritten; stale stub comments in
   frontend `api.ts` files; `cmd/settlement` is a healthz-only scaffold.

**Ordering rationale:** Tasks 1–5 close *wiring* gaps (existing tested code, highest
safety risk — dormant risk checks first). Tasks 6–7 write *new* code for true gaps.
Tasks 8–10 promote *evidence* (bindings). Task 11 fixes doc drift. Env-gated items
(soak host, staging cluster, DR region, live vendor accounts) are listed at the end
as explicitly out of scope for this repo pass.

## Task 1: Enable engine-side risk enforcement in shipped config
* **Objective:** Make the C++ engine's pre-trade/standing risk checks live in the
  deployed unit, or make the gateway-only posture an explicit, tested decision.
* **Step-by-Step Action Plan:**
  1. Read `core/src/main.cpp:540-760` flag handling (`-redis`, `-dev-all-accounts`,
     `engine_risk_check` plumbing) and `deploy/systemd/matching-engine@.service`.
  2. Decide per spec §13 the intended split: either (a) wire a production
     `IAccountState` source (PG/Redis-backed, not `DevAccountState`) + `SuspensionRefresher`
     via `-redis`, or (b) disable `engine_risk_check` deterministically and document
     gateway-only enforcement in `deploy/systemd/` + spec §27.
  3. If (a): bind `IMarginCheck` to the margin coordinator client, open the shm
     bilateral-credit matrix, and call `bind_credit` in the match path.
  4. Update `deploy/systemd/matching-engine@.service` + ansible/k8s manifests to pass
     the chosen flags; add a boot-time self-check that fails closed if risk feeds absent.
  5. Add/extend gtest coverage for the engine-side check path actually enabled.
* **Deliverables:**
  1. systemd unit + deploy manifests passing the decided flags.
  2. gtest covering the enabled enforcement path (or a spec §27 ruling recording (b)).
* **Completeness Checklist:**
  - [x] Shipped unit no longer leaves halt/OTR/sanctions/margin checks silently unbound —
        `deploy/systemd/matching-engine@.service` + `deploy/supervisord.conf` now pass
        `-redis`/`-symbol`/`-instrument-id` (EnvironmentFile for prod), binding the
        suspension lattice, sanctions cache, instrument feed, oracle feed and the
        new `account:state` provider.
  - [x] Engine fails closed on missing risk feed at boot (no silent pass-through) —
        `AccountStateCache` reports UNKNOWN/-1/UINT32_MAX until the `account:state`
        poll verifies (heartbeat TTL 30s); `SanctionsCache` had a latent steady-vs-wall
        clock-domain bug (`applied_ns` stamped `steady_ns()` vs wall `ctx.now_ns`)
        that would have failed every admission — fixed.
  - [x] Decision recorded: engine-side enforcement — production `IAccountState` +
        `IPositionState` now exist (`core/include/risk/AccountStateCache.hpp` /
        `core/src/risk/AccountStateCache.cpp` — Redis `account:state` hash, 1s
        control-path poll, `-accounts-poll-ms`), published by
        `services/internal/risk/account_state.go` `AccountStateProjector` (atomic
        `account:state:next`→RENAME swap) running inside the singleton `cmd/risk`
        coordinator. `IMarginCheck` remains the Phase-19 seam (check 8 keeps the
        stub ratio check until the margin engine binds it) — tracked under Task 5.
* **Implementation notes (2026-10-02):**
  - New files: `core/include/risk/AccountStateCache.hpp`, `core/src/risk/AccountStateCache.cpp`,
    `core/tests/test_account_state.cpp` (12 tests, 10 pass / 2 live-gated on
    `EXC_REDIS_TEST_ADDR`), `services/internal/risk/account_state.go`,
    `services/internal/risk/account_state_test.go`.
  - `main.cpp`: `-accounts-poll-ms` flag (default 1000ms); binds `risk.bind_accounts`
    + `risk.bind_positions` to the cache when `-redis` is set without
    `-dev-all-accounts`.
  - `core/src/risk/SanctionsCache.cpp`: `applied_ns` now stamps `now_ns()` (wall
    domain matching the TIME_TICK-driven `ctx.now_ns`), not `steady_ns()`.
  - `deploy/systemd/matching-engine@.service`: `EnvironmentFile=-/etc/exchange/
    matching-engine-%i.env` carrying `EXC_REDIS_ADDR`/`EXC_SYMBOL`/`EXC_INSTRUMENT_ID`;
    ExecStart wraps in `sh -c` so unset vars emit no dangling flag tokens.

## Task 2: Wire trailing stops + extended amend fields end-to-end
* **Objective:** Make `TRAILING_STOP` reachable from clients and forward the full
  `AmendRequest` field set through `EnginePump`.
* **Step-by-Step Action Plan:**
  1. Add `trailing_offset`, `trailing_offset_unit`, `activation_price`,
     `discretionary_offset_pips` to `orders.SubmitRequest` (`internal/orders/types.go:331-386`)
     with validation mirroring `execparams.go:256-266`.
  2. Populate `m.TrailingOffset/TrailingOffsetUnit/ActivationPrice/DiscretionaryOffsetPips`
     in `internal/orders/ipc.go:158-241` `orderNewMsg()` (FlatBuffers schema already
     carries them — `core/proto/exchange.fbs:58`).
  3. Extend `EnginePump` OrderAmend decode to forward `order_seq`, `gtd_expiry_ns`,
     `display_qty`, `trigger_source` (`core/include/matching/MatchingEngine.hpp:99-102`).
  4. Add REST route + FIX trailing-stop field mapping; frontend `AdvancedOrderPanel`
     already emits TRAILING_STOP — verify payload reaches the new fields.
  5. Add integration test: REST submit → engine TRAILING_STOP → ratchet → trigger.
* **Deliverables:**
  1. Submit path fields + wiring + route.
  2. Amend decode forwarding + e2e test.
* **Completeness Checklist:**
  - [x] `POST /orders` accepts trailing params and they reach the wire struct
  - [x] `algo_params.offset` no longer validated-then-dropped
  - [x] Extended amend fields decoded by EnginePump; `STALE_MODIFY` on `order_seq` works over the wire
* **Implementation notes (2026-10-02):**
  - Submit path: `SubmitRequest` gains `trailing_offset` /
    `trailing_offset_unit` (PIPS|PERCENTAGE|ABSOLUTE) / `activation_price` /
    `discretionary_offset_pips`; first-class fields self-describe into a
    `TRAILING_STOP` `algo_params` row when no `algo_type` is given
    (`orders/types.go`, `orders/execparams.go`, `orders/service.go`,
    `orders/store.go`, migration-103 column already existed).
  - Reachability: `POST /orders` first-class fields; `POST /orders/algo`
    `algo_type=TRAILING_STOP` now routes through the order pipeline (was:
    strategy-parent path that dropped the fields) — `handlers_algo.go`;
    `ParseAlgoSubmit` accepts the frontend's `algo_params` key spelling
    (`algo/types.go`); FIX 4.4 + FIXT/5.0 SP2 map venue custom tags
    20003–20006 (`fix/tags.go`, `fix/mapfix.go`, `fix/fix50sp2.go`) —
    `OrdType=3` + trailing pair no longer requires `StopPx`.
  - Wire: `orderNewMsg` populates trailing offset/unit (pips raw, pct×100,
    absolute ticks), activation price, trigger source, discretionary pips;
    `EncodeOrderNewEvent` already carried the FlatBuffers fields
    (`orders/ipc.go`, `ipc/messages.go`). Engine decode was already live.
  - Amend: `OrderAmend` schema appended `display_qty` + `trigger_source`
    (append-only); new `IEngineIngress::AmendWire` +
    `on_amend_received_ex` forward the full set (order_seq fence, price,
    qty, stop, display, gtd_expiry_ns, trigger_source — `0xFF` = unchanged,
    wire-0 mapped to it); `MatchingEngine` converts `AmendWire` → existing
    `AmendRequest` (atomic, stale-fenced) (`exchange.fbs`,
    `EnginePump.{hpp,cpp}`, `MatchingEngine.{hpp,cpp}`, `orders/ipc.go`,
    `orders/service.go`).
  - Tests: pump-level amend-forwarding + trigger-sentinel tests
    (`test_engine_pump.cpp`); service e2e `TRAILING_STOP` → wire decode +
    synthesized `algo_params` persistence, legality matrix, algo_params
    fold, discretionary wire value (`orders/trailing_test.go`); FIX 4.4 +
    SP2 tag tests (`mapfix_test.go`, `fix50sp2_test.go`); `algo_params`
    alias test (`algo/algo_test.go`). All 38 C++ suites + `go test ./...`
    green.

## Task 3: Wire FIX unreachable surfaces (SP2, mTLS gate, J/AK, drop-copy)
* **Objective:** Promote tested Phase-18 library code into the running `cmd/fix` binary.
* **Step-by-Step Action Plan:**
  1. Invoke `AcceptorSettingsSP2`/`InitiatorSettingsSP2` in `fix/gateway.go:41-70`
     behind config; create the FIXT.1.1 + ApplVerID=9 listener.
  2. Wire `tls.ServerTLSConfig`/`VerifyConnection` + `WrapCertification` into
     `cmd/fix/main.go:340-370` so fingerprint→CompID binding and cert gate run at Logon.
  3. Add `FromApp` cases for 35=J (AllocationInstruction) producing 35=AK reports;
     instantiate drop-copy / PB-drop-copy / affirmation services in `cmd/fix/main.go`.
  4. Gate each surface behind config flags with fail-closed defaults; update
     `docs/` connectivity docs with the enabled surfaces.
  5. Bind the already-EXECUTABLE criteria (167, 193/194-adjacent, allocation legs) to
     tests that exercise the *wired* path, not only package-level units.
* **Deliverables:**
  1. `cmd/fix` instantiating SP2 acceptor, cert gate, J/AK dispatch, drop-copy services.
  2. Wiring tests proving the production path (not just library tests).
* **Completeness Checklist:**
  - [x] `grep WrapCertification services/cmd/fix` non-empty
  - [x] 35=J reaches `allocation.go` handler (no `MSGTYPE_UNSUPPORTED`)
  - [x] SP2 session logon works when enabled, refused cleanly when disabled
* **Implementation notes (2026-10-02):**
  - SP2 listener: `GatewayOpts.SP2Enabled` binds a second acceptor via the
    existing `AcceptorSettingsSP2` helper (FIXT.1.1 + `DefaultApplVerID=9`,
    dedicated host/port, TLS-mandatory) — `fix/gateway.go`; config keys
    `fix.sp2_enabled` / `fix.sp2_acceptor_{host,port}` (default `0.0.0.0:9880`,
    fail-closed validation: SP2 requires `tls_enabled`, distinct valid port)
    — `internal/config/config.go`; `cmd/fix` passes
    `SP2Enabled: cfg.Fix.SP2Enabled` (`cmd/fix/main.go`). Disabled ⇒ the
    port is never bound (connection refused).
  - mTLS/FIXS gate: `GatewayOpts.TLSConfig` is injected into
    `Acceptor.SetTLSConfig` so `tls.go` `VerifyPeer` (cert fingerprint →
    `fix_sessions` binding via `PgBindingLookup`, CN/SAN + environment
    match, `PgRevocationList` over migration-282 `fix_cert_revocations`,
    `LogCertAudit`) runs at the TLS handshake — before any FIX byte.
    `WrapCertification` wraps the app and gates Logon for uncertified
    order-entry sessions in production (`cmd/fix/main.go`;
    `fix.mtls_required` ⇒ `tls_enabled` + `tls_ca`, else config refuses).
  - 35=J/35=AK: `App.FromApp` dispatches `MsgAllocationInstruction` to
    `onAllocationInstruction` → `AllocationService.Handle`; returned AK
    reports are emitted per-allocation-account via `SendToTarget`;
    drop-copy sessions are refused with BusinessReject; unwired service ⇒
    BusinessReject (fail-closed). `cmd/fix` constructs PG-backed
    `AllocationStore` + `AllocationService`, `ExecRegistry` (Tag-17 ExecID
    resolution, tapped via `app.Report().WithTap`), `PgExecResolver`
    (orders+instruments lookup), `DropCopyRouter`, `PBDropCopy` +
    `AffirmationMonitor` (60s window/5s cadence defaults) — `fix/app.go`,
    `fix/exec_resolver.go` (new), `cmd/fix/main.go`.
  - Drop-copy session binding: migration 282 `fix_session_bindings`
    provisions per-session account sets; `App.OnLogon` → `bindDropCopy`
    registers the target in `DropCopyRouter` (unknown account ⇒ fail-
    closed Logon reject); `OnLogout` unbinds. `DropCopyRouter.Bind`
    dedupes by binding ID. `PgStore.Save` persists bindings
    (`fix/store.go`, `fix/dropcopy.go`, `fix/app.go`).
  - Tests: `gateway_test.go` (new) — SP2 acceptor constructed when
    enabled, plaintext SP2 config refused, absent when disabled, mTLS
    `tls.Config` injected into acceptor. `app_test.go` — 35=J dispatch
    reaches `AllocationService` (no `MSGTYPE_UNSUPPORTED`), drop-copy
    session refused on 35=J, unwired allocations ⇒ BusinessReject,
    Logon binds drop-copy accounts / Logout unbinds.
    `config_test.go` — fail-closed validation matrix for SP2/mTLS keys.
    `go build ./...` + `go test ./...` green.
  - Docs: runtime-wiring note appended to
    `docs/Phase-18-FIX-Protocol-Gateway.md` §18.2 recording the enabled
    surfaces and config keys; `config.example.yaml` documents the new
    `fix.*` keys.

## Task 4: Wire orphan subsystems: SBE binary, SOR, cross-shard baskets
* **Objective:** Give `internal/sbe`, `internal/fixsbe`, `internal/sor`, and the
  cross-shard coordinators a production caller each — or formally descope them.
* **Step-by-Step Action Plan:**
  1. SBE: create `cmd/fixsbe` (or `cmd/sbemd`) hosting the A/B multicast publisher +
     TCP replay/snapshot listeners from `internal/sbe`; add `deploy/` unit + multicast
     config; bind crit 305 leg-2.
  2. SOR: decide routing insertion point — either `orders.Service` consults a router
     interface on venue-unavailable, or FIX outward initiators are SOR-driven; wire +
     flag; update crits 193/194 bindings to cover the wired path.
  3. Baskets: add `Basket` message to `core/proto/exchange.fbs`, REST submission
     endpoint, and engine dispatch into `CrossShardCoordinator`/`OptimisticShardCoordinator`;
     wire `CrossShardMarginCoordinator` into the pre-trade path.
  4. If any surface is deliberately descoped, record the ruling in spec §27 instead of
     leaving dead code.
* **Deliverables:**
  1. SBE listener process + deploy config.
  2. SOR call site + basket ingress path (or §27 descope rulings).
* **Completeness Checklist:**
  - [x] `grep -r "internal/sbe\|internal/fixsbe\|internal/sor" services/cmd` non-empty
        — `cmd/fixsbe` (SBE order entry), `cmd/marketdata` (`buildSBEFeed`
        multicast/replay/snapshot), `cmd/gateway` (`buildSORRouter`,
        `sorBookCache` frame tap, FILL_BRIDGE consumer), `cmd/xshardrelay`.
  - [x] A basket order can be submitted and executes via 2PC (or ruling recorded)
        — `POST /api/v1/orders/basket` + `GET /api/v1/baskets/{op_id}` →
        `EncodeBasketSubmitEvent` → `EnginePump` `BasketSubmit` case →
        `OptimisticShardCoordinator` TRY_MATCH → synchronous leg exec →
        `BasketResult` out → bridge → `baskets`/`basket_legs` projection.
        Pump-level dispatch test + service→wire→decode round-trip test.
  - [x] No bound criterion tests only unreachable code — WAL enum collision
        (kWalEvtBasket*/OPT_* previously colliding with PREVENTED_MATCH..ORDER_NEW_EX
        at 8–11) moved to appended values; coordinators' `on_frame` now filters
        `dst_shard` before execution (fan-out double-exec closed).

**Implementation notes (Task 4):**
- **WAL contract fix:** `kWalEvtBasket*/Opt*` constants previously re-used enum
  values 8–11 (live `PREVENTED_MATCH`, `OCO_LINK`, `AUCTION_PHASE`, `ORDER_NEW_EX`)
  — journal writers would have produced ambiguous entries. Moved to appended
  `WalEvtType` values; `RecoveryManager` skips unknown types by design.
- **Basket wire:** `BasketLeg`/`BasketSubmit`/`BasketResult` appended to
  `exchange.fbs` (union-appended, generated bindings regenerated C++/Go).
  Deterministic 128-bit op ids = SHA-256 of client `op_id`; `op_hi`+`op_lo`
  carried exactly through ledger + unwind keys.
- **Ctl demux:** one Aeron publication per ctl channel, per-protocol
  `IpcChannel` views via `CtlDemux` (XHB2/XOHO/MRGC magics); both coordinators
  reject frames whose `dst_shard` ≠ self. `cmd/xshardrelay` is the
  inter-process relay (publications have no `Close`; `Offer` rc ≤ 0 counted
  as drop + logged).
- **Engine side:** `EnginePump::service_cross_shard` (poll→dispatch→emit),
  main.cpp binds optimistic coordinator, `OptMatchExec` runs legs through a
  fill-observer seam on `MatchingEngine` (synchronous VWAP/filled capture),
  unwind uses stored leg account identity; `BasketLegSink` = audit surface
  (engine has no reserved-order state API — documented).
- **Cross-shard margin:** `CrossShardMarginCoordinator` adapted to
  `IMarginCheck` in main.cpp; Aeron channels already served by `cmd/risk`.
- **SBE:** `cmd/fixsbe` — TLS 1.3 + SNI-bound Ed25519 negotiator (keying
  material makes plaintext structurally impossible), `PgSessionStore` +
  schema registry over migration 228, Aeron orders_in publication +
  orders_out subscription, mTLS optional. `cmd/marketdata` gained
  `buildSBEFeed` — a `TeeDeltaSource` tap diffing `BookDelta` snapshots into
  incremental A/B multicast packets with journal-backed replay/snapshot TCP
  listeners (EXC_SBE_MD_* env).
- **SOR insertion point:** orders-pipeline consult (not SOR-driven outward
  initiators) — `orders.Service.Submit` consults `Router.Route` before engine
  dispatch: `Local` → engine, `Shadow` → parent `RESERVED` + `ROUTED_EXTERNAL`
  ack, error → `REJECTED` (never left live). `sorEligible` excludes post-only/
  reduce-only/algo/stop/trailing/iceberg/pegged/composite. Book view =
  `sor.BookViewCache` (own `sor.Level` type — avoids orders→sor→marketdata→orders
  cycle) fed by the consumer `frameTap` + `DecodeBookDeltaFrame`; staleness TTL
  reads as no-BBO. Fills: `FILL_BRIDGE` JetStream consumer (`sorfill` subject)
  dedups via `sor_fill_dedup` and projects onto parent. Zero venues ⇒
  `buildSORRouter` returns nil ⇒ pure local path.
- **PgShadowStore adapter:** `PgxExecer` wraps `*pgxpool.Pool` behind a
  `database/sql`-shaped `Execer` seam (`cmdTagResult` adapts CommandTag→Result).
- **Deferred (env-gated, honest):** no live venue connector exists — only
  `loopback:` is wired; real LP connectors land when credentials exist.
  SBE multicast/live fixsbe sessions require TLS material + media driver —
  code-complete, live-reachability unproven (same status as Task 3 fixs).

## Task 5: Wire dormant service seams
* **Objective:** Construct the services that exist but are never instantiated, and
  produce the missing `margin-events` feed.
* **Step-by-Step Action Plan:**
  1. `NewPBCreditService` — construct in `cmd/gateway/main.go`; add PB gate call inside
     `orders/service.go` `checkOrderRisk` (define a real `PBCreditGate` interface first
     — the docstring references one that doesn't exist).
  2. Bilateral credit: give `ReserveMatch` a caller — either Go admission consults the
     matrix or `main.cpp` opens shm + `bind_credit` (coordinates with Task 1).
  3. Liquidation feed: publish `type=="liquidation"` records to JetStream `margin-events`
     from the liquidation/auction execution path (`risk/liquidation.go`,
     `risk/auction.go`); the `liquidations@`/`auctions@` consumers already exist.
  4. LP pipeline: pass real `MetricsSource`/`AlertSink` to `NewLPService` instead of
     nil (`cmd/gateway/main.go:2819-2824`).
  5. Bind `ReturnSeriesSource` (ClickHouse daily closes) for correlation refresh +
     add TTL gate; bind `FlashCrashProvider` for stress scenarios (`main.go:5143-5144`).
  6. Insurance fund: add scheduled replenishment sweep implementing the
     Fund→House→Capital→ADL governance waterfall.
* **Deliverables:**
  1. PB gate in admission path; bilateral `ReserveMatch` caller; `margin-events`
     producer; LP live metrics; correlation refresh; fund replenishment sweep.
* **Completeness Checklist:**
  - [x] `NewPBCreditService` called under `services/cmd`; NOP/DSL checked pre-trade
        — `cmd/gateway/main.go` constructs `risk.NewPBCreditService(
        risk.NewPgPBCreditStore(pool), …)` and binds `orderSvc.WithPB`;
        `orders.PBCreditGate` (ReserveHeadroom/ReleaseHeadroom/
        AdjustHeadroom) is consulted post-insert in `Submit`, released on
        cancel/reject paths, re-sized on amend. `internal/orders/
        pb_gate_test.go` covers all four lifecycle points.
  - [x] `liquidations@` channel emits real events — the fill-reconcile
        point publishes `{"type":"liquidation", …, "is_auction"}` to
        JetStream `margin-events.{shard}.{SYMBOL}` (one publish feeds both
        `liquidations@` and `auctions@` projections; `is_auction` selects
        downstream). Live observation of a triggered liquidation remains
        env-gated (needs NATS + a real breach).
  - [x] Correlation matrix refreshes and refuses stale (>TTL) matrices —
        `CorrelationMatrixDeps{MaxAge, Now}` gate: `Offset`/`OffsetFactor`
        return !ok past MaxAge; gateway binds a ClickHouse `OHLCVStore`
        return-series adapter + scheduled refresh; `chFlashCrash` binds
        `FlashCrashProvider` from CH 1D worst-day replay.
  - [x] Fund replenishment runs on schedule with dual-control + GL
        postings — `risk.NewFundReplenisher(pool, fundSvc, opsAlerter)` on
        `runDailyUTC` 00:10 UTC; waterfall = target recompute →
        fee-revenue sweep → contingent-facility draw → dual-control
        capital-call approval queue; every movement goes through
        `InsuranceFundService` (GL-posted), never direct balance writes.

**Implementation notes (Task 5):**
- **PB credit:** `PBCreditGate` is the orders-side seam; `PBCreditService`
  enforces NOP/DSL against `pb_credit_limits` with atomic PG reservation
  rows, converting quote notional→USD via `position.Converter`. Breaches
  surface `PB_NOP_LIMIT_EXCEEDED`/`PB_DSL_LIMIT_EXCEEDED`; non-PB accounts
  (no limit rows) are admitted. Reservations post-date order insert and
  unwind on every dead path.
- **Bilateral credit end-to-end (the big wire):** the match-time
  authority is the engine — `MatchingEngine::bind_credit` engages
  `BilateralCreditMatrix::consume_or_skip` inside `walk_match`'s member
  scan: per candidate pair, when BOTH parties map to credit parties, the
  fill's quote notional must debit mutual headroom before commit; `Skip`
  leaves the maker in place (skip-marker cursor + level frontier preserve
  price-time priority) and a remainder with no eligible contra anywhere
  dies `BILATERAL_CREDIT_EXHAUSTED` (new WAL reason 9). `main.cpp`
  attaches the shm matrix (`EXC_CREDIT_MATRIX_SHM`, default
  `exchange_credit_matrix`) and binds it to BOTH the admission pre-screen
  (`PreTradeChecker::bind_credit`) and the engine. `AccountStateCache`
  now implements `IPartyMap` via a new `c:{account}` snapshot field; the
  Go `AccountStateProjector` publishes `c:` rows from `credit_parties`
  (42P01-tolerant for minimal schemas). Go side: `orderSvc.WithBilateral(
  bilatSvc)` adds the `AdmitOrder` admission consult; `PublishSnapshot`/
  deltas drive the matrix; `ReleaseOrder`/`ConsumeFill`/sweep/verify
  hooks were already live. Tests: 6 `MatchingEngineCredit` gtests
  (mutual-credit fill + dual debit, exhaust-reject, skip-priority +
  descent, unscreened pass-through, drain-then-exhaust), `c:`-field parse
  + IPartyMap test, 4 `AdmitOrder` service tests.
- **margin-events producer:** single publish at the liquidation
  `RecordFill` reconcile point; instrument symbol resolved via
  `orderStore.InstrumentByID`; publish failures warn-logged (the
  liquidation itself is already GL-settled — the feed is a projection).
- **LP pipeline:** `admin.PgLPMetricsSource{Pool}` derives scorecards from
  `lp_accounts`-scoped order flow (fill ratio, ack latency, minute-bucket
  presence) — real data or nothing, no fabricated metrics;
  `admin.LPOpsAlertSink` bridges `lp_performance_alerts` into the ops
  alerter.
- **Correlation/stress:** CH daily closes → `ReturnSeriesSource`
  (90-day lookback); `MaxAge` staleness gate rejects offsets from an
  over-age matrix (fail-closed — a stale ρ cannot fabricate hedge
  credit); `Now` injected for determinism. `chFlashCrash` replays each
  instrument's worst 1-day drawdown/rally as `FLASH_CRASH_*` scenarios —
  nil when no CH store, leaving `ScenarioLibrary` coverage intact.
- **Env-gated honesty:** matrix shm attach failure logs WARN and leaves
  the screen inert by contract (unscreened flow); `credit_parties`
  population is an operator action; live `margin-events` observation needs
  NATS + a triggered liquidation.
- **Verification:** `go build ./...` + `go test ./...` green (87 pkgs),
  `ctest` 38/38, `go vet` clean.

## Task 6: Implement true code gaps
* **Objective:** Write the spec'd functionality that has no implementation.
* **Step-by-Step Action Plan:**
  1. `SURVEILLANCE_LAG_WARNING` (crit 392): consumer-lag probe in `cmd/compliance` —
     >10K lag → P1 alert + auto-scale hook + detection-latency SLA metric.
     *(implemented at P2 — autoscale-class severity matching
     `aeron_subscriber_lag`/`nats_consumer_pending` taxonomy; the alert
     drives worker autoscaling, not paging. supersedes "P1")*
  2. R8 90-day GTC cap: submission clamp (auto-convert to GTD at +90d) + expiry
     sweeper emitting `GTD_EXPIRED`; register criterion if absent from matrix.
  3. DORA Art. 28 vendor register: `ict_providers` tables + migrations, concentration
     assessment fields, audit/renewal scheduler, exit-strategy tracking; routes under
     admin; reconcile with `docs/ops/dora-third-party-register.md` procedure.
  4. `FAIR_VALUE_DIVERGENCE` + `FIXING_WINDOW_CLOSED`: register in `errs/codes.go`,
     emit from `algo/fixing.go`/`fixing_scheduler.go` divergence + cutoff paths.
  5. Kupiec POF + Christoffersen independence tests in `risk/model_validation.go`;
     persist results in `margin_model_runs`.
  6. `cmd/settlement`: either host real daemons or rename/annotate as healthz scaffold;
     sync `docs/ops/daemon-inventory.md`.
* **Deliverables:**
  1. The six items above with tests + error-code registry entries.
* **Completeness Checklist:**
  - [x] Lag probe emits `SURVEILLANCE_LAG_WARNING` on a forced >10K backlog
  - [x] GTC submission clamps at 90d; sweeper emits `GTD_EXPIRED`
        (implemented as admission clamp → engine expiry machinery; no
        separate sweeper needed — `OrderCancel reason=1` lands EXPIRED +
        `order.expired`/`GTD_EXPIRED` private notice)
  - [x] Vendor register CRUD + renewal alerts exist (not just docs)
  - [x] Backtester reports Kupiec/Christoffersen statistics

* **Implementation notes (items 1–2, 2026-10):**
  1. **Surveillance lag warning.** `internal/surveillance/lag.go` adds
     `WatchLag`: a pluggable `LagProbe` (poll interval, threshold
     10,000 events) firing `SURVEILLANCE_LAG_WARNING` (P2) through the
     observability evaluator + NATS publisher/log fanout, with
     hysteresis (fires once on breach, resolves on recovery) and an
     auto-scaling signal hook for analysis workers — backlog is never
     dropped. `cmd/compliance` polls the `l3` stream's `compliance-l3`
     durable every 15s (`NumPending + NumAckPending`) and exports
     `exchange_surveillance_l3_consumer_lag`,
     `exchange_surveillance_detection_latency_seconds`,
     `exchange_surveillance_events_total`; `signals.go` records
     event-time→detection latency per applied event atomically.
     Tests: `TestWatchLagFiresThenResolves`,
     `TestWatchLagBelowThresholdSilent`, `TestDetectionLatencyAccounting`.
  2. **R8 90-day GTC cap (spec §27).** `ValidateSubmit` auto-converts
     TIF=GTC (or empty-default) to GTD with `gtd_expiry = now+90d`;
     an explicit earlier expiry is honoured, a later one clamps, and a
     GTD request beyond the cap clamps too. `ValidateModify` applies the
     same ceiling to amends (TIF→GTC resolves to GTD at min(stored,
     cap)); signature gained `now`. The engine's existing expiry heap
     fires `kWalCancelReasonExpired` deterministically — no sweeper
     needed. Read model: migration 284 adds `orders.gtd_expire_at`
     (previously referenced by `UpdateField` but never created —
     latent runtime error on TIF/expiry amends), persisted on insert,
     scanned into `Order.GTDExpiry`; `PgStore.ApplyCancelReason` maps
     echo reason 1 → status `EXPIRED` (was always `CANCELLED`), and
     `OnCancel` emits `order.expired` / `GTD_EXPIRED` on the private
     channel (FIX feed already maps reason 1 → `reportExpired`).
     Adjacent fix: TIF=DAY orders previously arrived without
     `gtd_expiry_ns` and were engine-rejected — new
     `Options.DayExpiry` seam stamps the §6.7 session close
     (`admin.NextSessionClose`, Fri 22:00 UTC) in Submit/BatchSubmit/
     DryRun/amend; bound in `cmd/gateway`. Nil seam ⇒ engine fails
     closed. Tests: 7 ValidateSubmit/Modify cap cases,
     `TestSubmitGTCCapEndToEnd` (wire TIF=GTD + `gtd_expiry_ns` in
     [now+90d] window + persisted expiry), DAY stamp/unbound-seam
     cases, `TestOnCancelExpiredEmitsGTDExpired`,
     `TestNextSessionClose`.
     **Verification:** `go build ./...` + `go vet` clean;
     `go test ./...` green (87 pkgs). C++ untouched — expiry heap
     already covered by `test_expiry`.

  3. **DORA Art. 28 ICT vendor register.** Migration 285 adds
     `ict_providers` + `ict_provider_reviews` (seeded with the five
     documented vendors). `internal/operations/dora` carries the
     domain/service (`VendorService`: CRUD, annual reviews, daily
     06:00-UTC sweep emitting `ICT_REVIEW_OVERDUE` /
     `ICT_RENEWAL_DUE` / `ICT_EXIT_TEST_STALE` via OpsAlerter);
     `internal/db` `PgVendorStore`; `internal/api/handlers_ict.go` +
     routes under `/api/v1/admin/ict-providers` (Compliance Officer
     RBAC); `cmd/gateway` constructs + schedules. `docs/ops/
     dora-third-party-register.md` reconciled to the live routes.
     Tests: in-memory store covering CRUD + all three sweep scenarios.
  4. **§27.1 error codes.** `FIXING_WINDOW_CLOSED` (400, L2) +
     `FAIR_VALUE_DIVERGENCE` (503, L1) registered as `localRow`s —
     the matrix cites them but no §23 row exists (same transcription
     gap as sibling codes). `algo/fixing.go nextFixing` emits
     `FIXING_WINDOW_CLOSED` when no enabled window exists for the
     benchmark (was `INVALID_REQUEST`); `derivatives.checkFairValue`
     (shared by `BookForward` + `BookNdf`) prices CIP even when an
     explicit `AgreedRate` is supplied and rejects beyond the 25 bps
     band (`FairValueDivergenceBps` — the §27.1 oracle-divergence
     convention); pricer failure propagates (fail closed — an
     unchecked negotiated rate against a broken curve is worse).
     Tests: `TestFixingWindowClosed{WhenNoEnabledWindow,WhenRowDisabled}`,
     `TestForwardBookFairValueDivergence`; NDF/forward fixtures now
     derive their spot so CIP fair ≈ agreed.
  5. **Kupiec POF + Christoffersen independence.** `model_validation.go`
     gains `KupiecPOF` (χ²₁ exact via `Erfc(√(x/2))`),
     `ChristoffersenIndependence` (transition-count LR, χ²₁),
     unconditional coverage (LR_uc = POF+ind, χ²₂ via `exp(-x/2)`) and
     `EvaluateCoverage`; `CoverageTestResult` stamps onto each
     BACKTEST run's `result_metrics` (margin_model_runs) via
     `Backtester.windowCoverage` — POF over event totals
     (breaches/events_evaluated reconstructed from run rows, breach⇒≥1
     event fallback for metrics-predating rows), independence over
     ordered per-day breach flags. Reject at 95% raises
     `MARGIN_MODEL_ADEQUACY_BREACH` P1. Tests: hand-computed Kupiec
     table values (x=4→0.769, x=8→7.734, x=10→12.955), 0·ln0 edges,
     clustered-vs-shuffled independence, stamped/persisted verdict,
     pathological-window rejection; `fakeRunStore.ListRuns` now sorts
     oldest-first to match the PG contract. Note: zero breaches over
     a full 250-day window correctly flags (over-conservative model,
     p=0.025) — the test is deliberately two-sided.
  6. **`cmd/settlement` hosts the real dispatch daemon.** Was a
     pure healthz scaffold; now runs `GrossNetService.DispatchDue`
     (the mandated mode-aware entry point) every
     `EXC_SETTLE_DISPATCH_INTERVAL` (default 5m): release-queued legs,
     rail cut-off re-roll, GROSS per-leg + NET per-batch dispatch with
     claim-before-send. Refuses to boot without `EXC_SENDER_BIC`
     (fail closed — a dispatcher that cannot sign is a defect).
     GrossNetService gained the `Cutoff` option + `cutoffQueue`
     optional store seam (`PgxGrossNetStore.ReleaseQueued` /
     `QueueForNextCycle` ported verbatim) restoring the Task 24.3.20
     dispatch-day re-check the mode-blind path had. Deploy:
     `EXC_SENDER_BIC` added to testnet configmap + supervisord;
     `docs/ops/daemon-inventory.md` row updated. Tests:
     cutoff-rolls-same-day-leg, inside-window-dispatches,
     constructor fail-closed without queue seam.
     **Verification (items 3–6):** `go build ./...` + `go vet ./...`
     clean; `go test ./...` green.

## Task 7: Mount shadowed REST handlers
* **Objective:** Eliminate `StatusStub` routes that mask working handlers.
* **Step-by-Step Action Plan:**
  1. Inventory all 19 `StatusStub` entries in `gateway/routes_v1.go`; classify:
     mount (handler exists) vs keep-stub (deliberate 501) vs remove.
  2. Invoke `accounts.Handler.Mount()` in `cmd/gateway/main.go` for
     `orders/countdown-cancel-all`, `orders/cancel-all-after`; implement/mount
     `positions/close-all`, `market/depth`, `market/open-interest`, `market-data/snapshot`,
     `venue/info`.
  3. For intentionally-absent surfaces (legacy `/ws/market`, `/ws/stream` aliases),
     either delete the registry rows or mark deprecated→410 per the deprecation policy.
  4. Add a registry-vs-live-map completeness test so no `v1` route can be declared
     but unwired (extends `route_completeness_test.go`).
* **Deliverables:**
  1. Live mounts for all handler-backed stubs; explicit policy for deliberate stubs.
  2. CI test asserting registry↔handler-map parity.
* **Completeness Checklist:**
  - [x] `pentest` route sweep returns 0 unexpected 501s on declared-live routes — all 19 `StatusStub` entries flipped to `StatusLive` with mounted handlers or redirects; the sweep's live→501 default branch is now the enforcing verdict and `TestLiveRoutesHaveGatewayHandlers` asserts every live row has a `cmd/gateway` live-map key
  - [x] Dead-man REST arming works end-to-end (200, timer fires, mass cancel) — `TestCountdownHandlerArmsAndReports` (200 ack), `TestCountdownExpiryTriggersMassCancel` + `TestRedisCountdownExpirySweepPath` (timer → `MassCancel`), handler now mounted at both `countdown-cancel-all` and the `cancel-all-after` alias
  - [x] Registry rejects a declared-live route with no handler key — `Register` rejects `StatusLive`+nil (`TestDuplicateAndInvalidRegistration`); `MountSeedLive` mounts the fail-closed 503 shim for a live-but-unmapped key (`TestUnwiredLiveServes503Shim`); `TestLiveRoutesHaveGatewayHandlers` scans `main.go` and fails on any unwired live row

* **Implementation notes (Task 7, landed):**
  - **Inventory → disposition.** All 19 `StatusStub` rows resolved: 14 REST surfaces mounted on existing backends, 5 legacy WS aliases resolve to 410 `ENDPOINT_GONE` naming `/ws/v1`. Registry now carries **0 stub rows** (590 live).
  - **Handler-backed mounts (no new service code):** `orders/countdown-cancel-all` + `orders/cancel-all-after` (dead-man alias, spec §8.9 TTL-refresh semantics are the same `DeadManService.Set` contract) and `positions/close-all` → `accounts.Handler` — `DeadMan`/`CloseAll` fields now populated in `cmd/gateway` (`NewCloseAllService(freezeSvc, PgxPositionReader, PgxTOTPSecrets, auth.VerifyTOTP, orderDisp)`); `admin/accounts/{id}/freeze|unfreeze` → `FreezeAccount`/`UnfreezeAccount` (internal four-eyes: `approver_id` must differ from actor); `venue/info` → `ExchangeInfo` alias; `market/open-interest` → `AnalyticsOpenInterest` via `?symbol=`→path adapter; `instruments/{symbol}/swap-rates` → `HistorySwapRates` via path→query adapter; `instruments/{symbol}/pip-value` → the purpose-built `PipCalculator.PipValueHandler`; `orders/roll` → `derivatives.RollHandler`.
  - **New thin adapters (api):** `MarketDepth` (`?symbol=&limit=`→`Book.Snapshot`, WS-resync spelling of MarketBook), `MarketDataSnapshot` (`?symbol=&level=L2|L3`, L3 rides the WAL-reconstruction reader), `AdminArchiveStatus` (`?shard=`→`ArchiveService.Status` + missing-segment count), `AdminInstrumentUncrossOverride` (dual-controlled `OpInstrumentResume` with `auction:true` pinned — Task 15.3.10's quarantine release path per its AC).
  - **New wiring (cmd/gateway):** `NewPipCalculator(PGInstrumentProvider{pool}, oracleMidProvider{oracleProv}, RedisPipCache)` — `oracleMidProvider` adapter in `adapters.go` fails closed (`PRICE_ORACLE_UNAVAILABLE`/`MARK_PRICE_STALE`); `ArchiveService` over `EXC_S3_WAL_BUCKET` objectstore (dev/AWS, same pattern as kycObjects); `RollService` over `PgxRollStore`+`Dates(LoadCalendar)`+`Pricer(rates.Store, oracle.Provider)` — the first live mount of the whole derivatives cluster. All three mount conditionally: absent deps leave the key unset → 503 shim.
  - **WS aliases:** `/ws/v1/marketdata`, `/ws/v1/orders`, `/ws/market`, `/ws/trade`, `/ws/stream` → `wsAliasGone()` — deterministic 410 `ENDPOINT_GONE` problem body naming `/ws/v1` as successor + `Link: rel=successor-version`, per the spec's own wording ("legacy WS aliases remain registered stubs returning ENDPOINT_GONE-style responses"). A 308 was rejected: WS clients do not follow HTTP redirects during the upgrade handshake, and the deprecation-policy 410 is the spec'd contract.
  - **Parity test:** `TestLiveRoutesHaveGatewayHandlers` (route_completeness_test.go) source-scans `cmd/gateway/main.go` for `"METHOD /path"` map keys + `live[...]` assignments and fails on any `StatusLive` row without one (meta rows `/routes`/`/errors`/`openapi.json` excluded — router-installed).
  - **Tests:** deadman handler/service/Redis-sweep suites green; api/gateway suites green; `go test ./...` 87 pkgs, `go vet` clean.

## Task 8: Frontend gaps (PAMM, webhooks, notifications, admin, calculators)
* **Objective:** Surface implemented backends in the SPA.
* **Step-by-Step Action Plan:**
  1. PAMM: `features/pamm/` — pool browse/create/invest/redeem + statement views against
     live `routes_v1.go:867-873` routes.
  2. Webhooks: endpoint CRUD + delivery-log/DLQ views wired to `webhooks` routes +
     admin dead-letter list (`main.go:6234-36`).
  3. Notifications: subscribe `private:notifications` in the WS client; add inbox/bell
     component; keep `NotificationsPanel` as prefs editor.
  4. Copy trading: remove `unfollowRegistered()` hardcode in `copy-grid/api.ts:88-90`;
     wire unfollow to live `DELETE /copy/follows/{id}`.
  5. Admin consoles: KYC review queue (routes live `main.go:5750-51`), webhook DLQ,
     LP scorecard (`internal/admin/lp.go`), RBAC/binding management, compliance-hold
     review.
  6. Calculators: add swap/rollover, standalone P&L, position-size calculators (crit 267).
* **Deliverables:**
  1. New feature modules + vitest specs per component; Playwright coverage where a
     live-stack spec is cheap.
* **Completeness Checklist:**
  - [x] No live backend route lacks a reachable UI surface (or explicit descope)
  - [x] `unfollowRegistered` flag deleted; unfollow round-trips
  - [x] `private:notifications` subscribed; inbox renders deliveries

**Implementation notes (Task 8, landed):**

- **Copy trading (item 4):** `unfollowRegistered` hardcode deleted. New
  `listMyFollows` + `unfollow` adapters (`features/copy-grid/api.ts`) hit live
  `GET /copy/follows` and `DELETE /copy/follows/{id}`; `MyFollowsPanel` renders
  the investor's live follows with a HIGH-severity typed-`UNFOLLOW`
  confirmation and query invalidation. Unavailable states render
  `UnavailablePanel` — nothing fabricated.
- **Notifications (item 3):** `features/notifications/` — module-level session
  ring (`store.ts`, 200-item bound, `useSyncExternalStore`), `NotificationBell`
  in `AppShell` subscribing `private:notifications` **only once
  AUTHENTICATED** (anonymous sessions can't join private channels — an early
  subscribe would force RESYNCING and lock order entry), and `InboxPage` at
  `/notifications` rendering deliveries + `markAllRead`. Preferences stay in
  `NotificationsPanel`.
- **Webhooks (item 2):** `features/webhooks/` — register (event allowlist from
  `allowed_events`), one-time-secret reveal, rotate (1h overlap), disable,
  per-endpoint delivery log (`DELIVERED`/`DEAD_LETTERED` colored).
- **Admin consoles (item 5):** `ConsolesPanel` on `/admin` — five tabs: KYC
  review queue (**new live route** `GET /admin/kyc/pending` →
  `compliance.LifecycleService.PendingQueue` → `PgStore.PendingSubmissions`,
  role-gated like the decision endpoints), webhook DLQ + retransmit, LP
  inventory + 1h scorecard, RBAC binding grant/revoke (dual-control 202 →
  "submitted to four-eyes"), compliance-hold release (distinct approver) +
  escalate (`sar` | `closure` → dual-control note).
- **Calculators (item 6):** `calc.ts` gains `computePnL` (signed pips + quote
  P&L) and `computeSwap` — `qty × points × days` verbatim from
  `settlement.InterbankSwapCharge`, markup as a separate negative leg.
  `PnlSection` + `SwapSection` mount on `/calculator`; the swap card reads the
  live `GET /instruments/{symbol}/swap-rates` sheet (Task 7 mount) and refuses
  projection when no sheet is published. Position-size was already covered by
  the existing margin calculator.
- **PAMM (item 1):** `features/pamm/` — browse (`GET /pamm/pools`), detail +
  caller allocation (`GET /pamm/pools/{id}`), statement keyset
  (`GET /pamm/pools/{id}/statement`), invest/redeem movements, manager create.
  Backend gained three new live GET routes (`PammPoolList`, `PammPoolDetail`,
  `PammStatement` over `BrowsePools`/`PoolDetail`/`PoolStatement`) —
  previously only the POST mutations existed.
- **Generated artifacts:** `docs/openapi/openapi.json` regenerated (662
  operations) and `route-contracts.ts` re-emitted — the two stale
  stub-contract test fixtures (sub-accounts as the canonical stub) updated to
  the zero-stub reality.
- **Tests:** `frontend vitest` 607/607 green incl. new suites for
  notifications (3), webhooks (3), pamm (3), admin consoles (2), calculator
  extras (3). `go test` compliance/api/gateway/pamm/copy green; `tsc`,
  `eslint` clean.

## Task 9: Promote contract bindings — engine/order/admin phases (15/16/18 leftovers)
* **Objective:** Convert PLANNED criteria with existing tests to EXECUTABLE.
* **Step-by-Step Action Plan:**
  1. Phase-16 (22 crits): bind `core/tests/test_phase16.cpp` (`Phase16*` cases),
     `test_lifecycle_auction.cpp`, `orders/auction_test.go`, `algo/*_test.go`,
     `orders/orderlist`/`bots` ITs to crits 47–53, 124, 129, 130, 197, 198, 252, 255,
     275, 287, 296, 317, 365, 367, 395, 399 — verify each `-list`/`-run` first.
  2. Phase-15 (10 crits): bind `TestLifecycleTransitionMatrix`, `TestBustExecutePG`
     family, `TestSessionLifecycleWeeklyFlow`, maintenance/delist tests to 119, 138,
     142, 217, 234, 290, 316, 343, 352, 401.
  3. Stragglers: 33–36 (liq auction → `risk/auction_test.go` + new e2e), 140 (OTR →
     `orders/otr_test.go`, needs `redis`), 46/152 (kill-switch), 99 (SLA — leave
     PLANNED, env-gated), 236, 308, 300 (env-gated — keep PLANNED with notes).
* **Deliverables:**
  1. `itest/bindings.go` additions; regenerated `contracts.json`; green integrity tests.
* **Completeness Checklist:**
  - [x] Phase-15/16 EXECUTABLE count reflects bound tests (0 → ~30)
  - [x] No fabricated bindings (each verified via `-list`/body read)
  - [x] Env-gated crits stay PLANNED with explicit reasons, not fake EXECUTABLE

**Implementation notes (Task 9, landed):**

- **40 criteria promoted PLANNED → EXECUTABLE** (bound total 163 → 203 of
  419; PLANNED 256 → 216). New sections in `itest/bindings.go`: Phase 11
  (crit 46), Phase 13 (140, 152), Phase 15 (119, 138, 142, 217, 234, 290,
  316, 343, 352, 401), Phase 16 (47–53, 124, 129, 130, 197, 198, 252, 255,
  275, 287, 296, 317, 365, 367, 395, 399), Phase 19 stragglers (33–36),
  Phase 23 (236).
- **Verification evidence:** every bound `-run` regex exercised via
  `go test -list` (nonzero match per leg); gtest filters verified against
  `--gtest_list_tests`. All executable legs **ran green**: `test_phase16`
  28 tests, `test_oco` 12, `test_lifecycle_auction` 20, `test_pretrade` 2;
  Go legs in orders/algo/risk/admin/instruments/marketdata/analytics all
  pass. `TestContractsJSONFresh` + `TestBindingReferences` green;
  `contracts.json` regenerated (203 EXECUTABLE / 216 PLANNED).
- **Gating honesty:** PG/Redis legs carry `Needs: pg|redis` so hosts
  without deps report BLOCKED rather than skip-laundering; the live-CH
  leg on 236 (`TestCHTickStore`) self-skips → criterion rests at
  EXECUTABLE-partial. Criteria 99 (market-data SLA), 300 (72h soak), 308
  (staging load under error injection) remain **PLANNED** — env-gated
  measurements, no harness on this host.
- **Deviation from plan:** crit 236 was listed under "keep PLANNED" but
  real unit legs exist (`TestTickStore*`, `tick_data_api_test.go`
  formats/cursor/tier windows) — bound with the CH leg gated instead.
- **Residual gap (noted, not fabricated):** crit 33's live-stack
  CALL→FORCE_CASH phase e2e needs mark-price oracle manipulation on the
  full stack — bound the PG store lifecycle + unit legs; noted in the
  binding comment.

## Task 10: Promote contract bindings — risk/compliance/backoffice/frontend phases
* **Objective:** Same as Task 9 for phases 13/14/17/19/19.5/21/24/11/20 + frontend crits.
* **Step-by-Step Action Plan:**
  1. Phase-19/19.5/13 (43 crits): bind `risk/*_test.go` (margin, liquidation, auction,
     nbp, adl, otr, collateral, pb_credit), `test_margin_coordinator.cpp`, oracle tests.
  2. Phase-14/17/21 (47 crits): bind `internal/compliance` suite (~226 tests),
     `internal/surveillance`, `core/tests/test_sanctions.cpp` per the crit list in
     Slice 2.8.
  3. Phase-24/11/20: bind `settlement/phase24_tasks_test.go`, `backoffice/*_test.go`,
     `analytics/*_test.go`.
  4. Frontend crits (MAT-382–390, T10-007, TBN-*, ERR-310/312): decide a binding kind
     for vitest (new `kind: vitest` leg running `npm test -- -t <name>`) or leave
     PLANNED with a documented note.
  5. Regenerate `contracts.json`; run integrity suite; sync meta-doc counts.
* **Deliverables:**
  1. EXECUTABLE count rises materially (target: ≥300/419 where tests exist).
     → **354/419 EXECUTABLE** (was 203 after Task 9, 163 at Phase-3 start).
     65 remain PLANNED — all either env-gated (live rails, soak, staging,
     external pentest cadence) or genuinely untested (182 FX Global Code,
     376 target-market dual-gate, 332 disclosure program, 109 external
     pentest). No empty or fabricated bindings admitted.
* **Completeness Checklist:**
  - [x] `TestContractsJSONFresh` + `TestBindingReferences` green after regeneration
  - [x] No criterion bound to a nonexistent test; every new binding `-list`-verified
* **Implementation notes:**
  - New `vitest` binding kind (`BindVitest`): `Pkg` = spec file/dir under
    `frontend/`, `Needs: "frontend"` gates on `node_modules/.bin/vitest`.
    Runner `Env.RunVitest` fails zero-match instead of passing it;
    `TestBindingReferences` stat-checks the path; `FrontendDir` added to Env.
  - New `bin` leg `pentest`: runs `tests/pentest/run.sh` (seeds fixtures,
    boots real gateway w/ documented dev JWT key, black-box + white-box
    suites) gated `pg+redis+nats`. Bound to crits 314, 342.
  - Phase-10 frontend crits 251/267/268/292/293/294/295/310/351/382–390
    bound to real spec dirs (all 24 vitest legs pass today).
  - Phase-19/19.5 risk legs verified via `-list`: ADL/NBP/margin-level/
    leverage tiers/close-tranche slicing/isolated/collateral-monitor legs
    all real; engine-side `test_margin_coordinator`+`test_cross_shard`
    gtest filters bound for cross-shard 2PC (176, 320).
  - Oracle legs (45/120/321/397) bound to `internal/oracle` staleness/
    divergence/fallback suite — not risk pkg (first-draft regexes there
    zero-matched; replaced with `-list`-verified names).
  - Phase-21 regulatory legs: reporting sub-pkg (`compliance/reporting`
    UTI/USI/lifecycle), `compliance/venue` case-mgmt, APA/ARM dispatcher,
    tax golden XML (CRS/FATCA), p21surv pg suite (RTS6, employee dealing,
    tuning, cases, reg-change, exec-policy).
  - Phase-24: recon legs retargeted to `internal/reconciliation` checkers;
    client-money waterfall/segregation/evidence-pack → `backoffice`
    client_money/treasury/assurance suites; `TestITSuspenseAndRailPayments`
    moved to `internal/funding` (its real home).
  - Deliberately still PLANNED: 65/67→bound (CH self-skip reports BLOCKED
    honestly), 99, 109, 182, 236, 300, 308, 332, 376 — env-gated or
    untested paths left honest.

## Task 11: Doc drift repair
* **Objective:** Bring meta-docs and stale claims in line with Phase-2 reality.
* **Step-by-Step Action Plan:**
  1. `docs/PROJECT_COMPLETENESS_ASSESSMENT.md` + `docs/SLICE_3_ASSESSMENT.md`: prepend
     supersession banners pointing to IMP-PLAN Phase 2 (keep bodies as historical record).
  2. Spec ~§4515 matrix row "Fixing execution scheduler… unwritten" → update or move
     to §27 with "(supersedes…)" per change protocol.
  3. Frontend `api.ts` stale comments (`settings`, `kyc`, `copy-grid`, `OpsBoardPage`)
     claiming 501/stub → correct to `v1live`.
  4. `docs/ops/daemon-inventory.md`: record that Phase-03/24 daemons run inside
     `cmd/gateway`; `cmd/settlement` is a scaffold.
  5. Update AGENTS.md/CLAUDE.md counts if task completion changes canonical numbers.
* **Deliverables:**
  1. Supersession banners + corrected claims + synced counts.
* **Completeness Checklist:**
  - [x] `rg -F "0% Code\|unwritten" docs/` returns only historically-marked hits
  - [x] No stale 501 comments remain in `frontend/src/**/api.ts`

#### Task 11 implementation notes (2026-10-02)
- **Assessment docs:** `docs/PROJECT_COMPLETENESS_ASSESSMENT.md` +
  `docs/SLICE_3_ASSESSMENT.md` now carry SUPERSEDED banners pointing to this
  plan's Phase 2/3 (bodies preserved as the Day-0 record).
- **Spec §27 provenance:** the §4515 fixing-scheduler row was already corrected
  ("Landed: fixing scheduler …" at ~§4518). Added dated §27 provenance notes to
  **§27.1** (domain matrix), **§27.2** (133-component catalog), **§27.3**
  (scorecard), and **§27.4** (gap analysis) marking all un-updated `0%`/
  `unwritten`/`Ready for Phase NN` cells as Day-0 planning baselines superseded
  by IMP-PLAN Phase 2/3 — per-row rewrites deferred since the per-phase landing
  log is the owning convention. Phase-01's "unwritten" inventory is already
  labeled "Day-0 … (0.0% Production Code Baseline)".
- **Frontend comments/strings:** corrected stale stub/501 claims in
  `settings`, `kyc`, `funding` (fee-estimate), `history` (algo-orders,
  order-lists, dead-man), `performance`, `reports` (incl. DownloadCenter
  per-report `desc` strings — those were always-visible UI copy claiming
  "endpoint not yet live"), `copy-grid` (wizard/stop/pause notes,
  StrategyBrowser), `ops/OpsBoardPage`, `admin/AdminPage`,
  `lib/admin/api.ts`, `lib/trading/api.ts`. 501 fallbacks reworded as
  regression signals rather than expected state; `UnavailablePanel`/
  `PreviewPanel` copy made state-neutral ("unavailable", not "not yet live").
  Tests updated to the new contract (`history.test.tsx`, `CopyGrid.test.tsx`,
  `PerformancePage.test.tsx` fixture message).
- **Daemon inventory:** `docs/ops/daemon-inventory.md` Tier-4/5 table corrected —
  `risk`/`analytics`/`oracle`/`settlement` are real binaries; `liquidation_scanner`,
  `tomnext_rollover`, `regulatory_reporter`, `banking_rails`, `status_exporter`
  run **inside `cmd/gateway`**; `proof_of_reserves` is deliberately out-of-process
  (`exchange solvency-tree` + cron). `cmd/settlement` is no longer a scaffold
  (Task 6 landed the real dispatch daemon — supersedes the plan's step-4 wording).
- **Meta-docs:** `AGENTS.md` spec-corpus 542→**543** (canonical since remediation
  #37); migration pairs corrected to **215** in `AGENTS.md` (was 211) and
  `CLAUDE.md` (was 210). Canonical §24/tasks counts unchanged (419/479).
- **Sweep results:** all `0% Code`/`unwritten` hits now sit inside bannered
  historical docs, the explicitly-labeled Phase-01 Day-0 inventory, or
  bannered spec §27 sections — historically marked. `rg` for stale 501/stub
  claims across `frontend/src` returns only supersession notes, regression
  fallbacks, and the live 501-detection mechanism.

## Out of scope (environment-gated — unchanged from Phase-1 deferral)
- 72h@50k/s soak + p99≤50µs: needs dedicated benchmark host (crits 11/12/300).
- 75k/s×4h staging gate: needs staging cluster (`staging-report.json` contract armed).
- D1 multi-region failover: needs live secondary region.
- Live third-party accounts: PagerDuty, SES/Twilio/FCM, banking rails/CLS, KYC IDV,
  APA/ARM/TR/SDR endpoints — seams are correct; activation is account provisioning.
- Post-restart throughput defect (~1.8k/s vs 15k/s): needs a profiling session on the
  soak host; tracked as open perf bug, not wired into this plan's tasks.
