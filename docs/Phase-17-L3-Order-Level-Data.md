# Phase 17 — L3 Order-Level Data

**Duration:** 3–4 days
**Dependencies:** Phases 2, 6
**Spec Reference:** §11 (L3 Order-Level Data), §14.4 (Market-Abuse Signals), §24 (Acceptance Criteria)

---

## 17.1 Objectives

Implement L3 order-level market data: individual order changes (add, modify, cancel, fill), WS distribution with sequence numbers, and market-abuse surveillance signal generation.

---

## 17.2 Prerequisites

- Phases 2, 6 complete

---

## 17.3 Tasks

### Task 17.3.1: L3 Data Generation

**Objective:** Generate L3 order-level data from C++ core.

**File Locations:** `core/src/ipc/L3Publisher.cpp`

**Implementation:**
1. C++ core publishes every order event: add, modify, cancel, fill.
2. Each event: order_id, account_id (hashed), side, price, qty, timestamp, seq.
3. Published via Aeron to Go market data service.
4. Per-symbol sequence counter.
5. **(implementation note 2026-09-29 — wire contract landed both sides):** `core/proto/exchange.fbs` defines `L3OrderEvent` (union member 9 → `EventType_L3OrderEvent=9`, 15 fields) emitted by `core/src/ipc/L3Publisher.cpp` via the generated builder — journal-first (`wal_seq` = triggering WAL row seq, §24 #318), salted-FNV-1a `account_hash` (`"exc.l3.account.v1"`), `qty` = remaining AFTER the event + signed `qty_delta`, flags bit0 hidden / bit1 pegged / bit2 synthetic / bit3 iceberg / bit4 detail. `MatchingEngine` emits at every lifecycle mutation site (`l3_emit_ev`/`l3_cancel`) and `main.cpp` binds `L3Publisher` on the `_out` channel. Per-instrument `seq` is **1-based** (0 is the flatbuffers absent-field sentinel the Go gate rejects). The Go decode seam `services/internal/ipc/l3.go` reads the row through the generated `wire.L3OrderEvent` accessors (regenerated with the schema) and projects it to the transport-neutral `L3OrderFields` + contract validation; `internal/ipc/l3_test.go` pins round-trip + rejection. C++ build: `cmake --build` green, `ctest` 33/33 — dedicated suite `core/tests/test_l3.cpp` (14 cases: hash determinism, gap seqs, add/modify/cancel/fill matrix, hidden/peg/IOC paths, WAL 1:1 correlation, journal-free replay bit-identical).

**Definition of Done (Acceptance Criteria):**
* [x] C++ core publishes all order events (add, modify, cancel, fill) — `core/src/ipc/L3Publisher.cpp` + `MatchingEngine::l3_emit_ev`/`l3_cancel` call sites covering admission/amend/cancel/fill legs (taker/maker/auction roles); `_out` channel wiring in `core/src/main.cpp`
* [x] Each event has order_id, side, price, qty, timestamp, seq — `table L3OrderEvent` in `core/proto/exchange.fbs` (instrument_id, kind, order_id, account_hash, side, price, ref_price, qty, qty_delta, seq, wal_seq, trade_id, fill_role, flags, cancel_reason); `ts` rides the `Event` envelope
* [x] Per-symbol sequence counter maintained — `L3Publisher::next_seq` per-instrument open-addressed seq table, consumed even on send failure (gap → consumer resync, spec §11.1)

**SDD Checklist:**
- [x] Spec checkpoint: L3 order-level data — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `cmake --build` + `ctest` 33/33 green (2026-09-29; 33rd = new `test_l3` suite)

---

### Task 17.3.2: L3 WS Distribution

**Objective:** Distribute L3 data via WebSocket.

**File Locations:** `services/internal/marketdata/l3.go`

**Implementation:**
1. WS endpoint: `/ws/v1/l3/{symbol}` (authenticated, premium tier).
2. Max 5 L3 subscriptions per session.
3. No conflation — real-time order events.
4. Reconnect: last_seq replay or snapshot.
5. **(amended 2026-09-20 — feature-completeness audit remediation #11):** L3 snapshot generation uses asynchronous WAL-based reconstruction: snapshot requests trigger a WAL position marker, then reconstruct from the last periodic snapshot + WAL replay on a dedicated reader thread (never blocking the matching loop). REST endpoint: `GET /api/v1/market-data/l3-snapshot/{symbol}` with cursor pagination for books exceeding 100,000 orders. Maximum snapshot staleness: ≤ 500ms from current engine state. For instruments with > 100,000 resting orders, streaming-only recovery is enforced (snapshot requests return 413 with `L3_SNAPSHOT_TOO_LARGE`).

**Definition of Done (Acceptance Criteria):**
* [x] L3 WS endpoint works (authenticated, premium) — `services/internal/marketdata/l3.go` `L3Server.ServeHTTP` mounts `/ws/v1/l3/{symbol}` (route `StatusLive` in `internal/gateway/routes_v1.go`, handler in `cmd/gateway/main.go`); shares the unified auth seams (JWT keyring + API-key HMAC) plus `l3PremiumTier` gate (professional/institutional/admin → `ENTITLEMENT_REQUIRED` 403 otherwise); `TestL3PremiumTierGate`
* [x] Max 5 L3 subscriptions enforced — `L3Server.accountCapAdmit` counts per-account L3 conns across sessions (cap `L3MaxSubsPerAccount=5`), rejects with `WS_MAX_SUBSCRIPTIONS_EXCEEDED`; `TestL3Server_FiveSubscriptionCap`
* [x] Real-time order events (no conflation) — `L3Hub.Publish` appends every event to the per-symbol ring and pushes one frame per subscriber queue; no batching/dedup anywhere in the path (`l3Conn.push`); `TestL3Hub_NoConflationEveryEventForwarded`, `TestL3Hub_SubscriberGetsEveryLiveEvent`
* [x] Reconnect with last_seq works — `L3Hub.attach` replays `(last_seq, tail]` frames from the 100k-event ring before live tail; `TestL3Hub_ReplayWithinHorizon`, `TestL3Conn_ReplayGateOrdering`
* [x] L3 snapshot generated asynchronously via WAL replay — never blocks matching loop — `services/internal/marketdata/l3_snapshot.go` `L3SnapshotReader`: prescans the shard's WAL dir for a marker (`RecScanWalDir`, wantShard-filtered), locates the newest `BOOK_SNAPSHOT`, replays tail to the marker on a dedicated reader; zero interaction with `core/`; `TestL3SnapshotReader_RebuildsRestingBook`, `TestL3SnapshotReader_SnapshotPlusTailReplay`
* [x] REST `GET /api/v1/market-data/l3-snapshot/{symbol}` with pagination — `internal/api/handlers_l3.go` `MarketDataL3Snapshot` (route v1live); cursor = last emitted `order_id` (`?cursor=`, `?limit=`); `TestL3SnapshotReader_CursorPagination`
* [x] Snapshot staleness ≤ 500ms; books > 100K orders return 413 L3_SNAPSHOT_TOO_LARGE — `L3SnapshotDeps.MaxStale`/`Now` gate (default 500ms → 503 `SERVICE_DEGRADED` via `ErrL3SnapshotStale`) + `MaxOrders` ceiling (default 100_000 → 413); `TestL3SnapshotReader_StalenessGate`, `TestL3SnapshotReader_TooLargeCeiling`

**SDD Checklist:**
- [x] Spec checkpoint: L3 WS distribution — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go build ./...`, `go vet ./...`, `go test ./internal/marketdata` + `./internal/ipc` green

---

### Task 17.3.3: Market-Abuse Surveillance Signals

**Objective:** Generate market-abuse surveillance signals from L3 data.

**File Locations:** `services/internal/surveillance/signals.go`

**Implementation:**
1. Signals: spoofing, layering, wash trades, marking the close, momentum ignition — plus front-running and insider dealing per spec §14.4.
2. Computed from L3 order stream (add/cancel patterns). Front-running: order placed ahead of a large known incoming order. Insider dealing: anomalous profitable trades preceding scheduled market-moving events (rate fixes, instrument transitions).
3. Signals stored in `surveillance_signals` table.
4. Phase 21 enforces on these signals.
5. **Migration note:** `migrations/029_create_surveillance_signals.up.sql` — `surveillance_signals` table (id, signal_type ENUM, account_id, instrument_id, evidence JSONB, detected_at, status).

**Definition of Done (Acceptance Criteria):**
* [x] Spoofing signal: rapid add/cancel pattern detected — `services/internal/surveillance/signals.go` `onCancel`: ≥3 cancels of orders rested <800ms inside the rolling 10s window per (symbol, account); `TestEngine_Spoofing`, negative case `TestEngine_Spoofing_RestedOrdersNotFlagged`
* [x] Layering signal: multiple levels add/cancel detected — `onAdd`: ≥3 same-side ADDs at distinct prices inside the window; `TestEngine_Layering`
* [x] Wash trade signal: self-trade pattern detected — `onExecute`: adjacent EXECUTE legs (≤`WashSeqSpan`) pairing the same `account_hash` on opposite sides; `TestEngine_WashTrading`
* [x] Marking the close signal: concentrated activity near close detected — `onExecute` + `closeW`: EXECUTE inside the configurable UTC close window moving >`CloseMoveBps` off the window-open reference; `TestEngine_MarkingTheClose` (in/out-of-window cases)
* [x] Momentum ignition signal: rapid aggressive orders triggering stop cascades detected — `onExecute`: consecutive same-direction EXECUTEs by one account inside `IgnitionWindowMs` moving >`IgnitionMoveBps`; `TestEngine_MomentumIgnition` (stop-cascade linkage is the Phase-21 alert layer)
* [x] Front-running signal: order placed ahead of known large incoming order detected (spec §14.4) — `onExecute`: consecutive same-account EXECUTEs with an adverse >`FrontRunMoveBps` follow-on move inside `FrontRunWindowMs` (positioning-ahead heuristic — venue-side proxy pending Phase-21 order-flow attribution); `TestEngine_FrontRunning`
* [x] Insider-dealing signal: anomalous trades before scheduled market-moving events detected (spec §14.4) — `onExecute` + `AnnounceWindows` (`EXC_SURVEILLANCE_ANNOUNCE_WINDOWS`): EXECUTE inside a configured daily announcement window raises the candidate; `TestEngine_InsiderDealing`
* [x] Signals stored in surveillance_signals table — `PgSink.Emit` INSERT … `ON CONFLICT (dedup_key) DO NOTHING` into the migration-029 table; `TestEngine_DedupKeyDeterminism` + PG-gated `TestSurveillanceSignalsPgIntegration` (EXC_PG_TEST=1)

**SDD Checklist:**
- [x] Spec checkpoint: market-abuse signal generation — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go test ./internal/surveillance` green; pump wired in `cmd/compliance/main.go` (JetStream durable `compliance-l3`)

---

### Task 17.3.4: L3 Monotonic Gap Recovery & Buffer Overrun Throttling

**Objective:** Implement strict monotonic sequence gap recovery, consumer overrun disconnects, and buffer memory safeguards for L3 market data streams per spec §2.7, §11.1, and §24 #318.

**Implementation:**
1. **L3 Monotonic Gap Detection:** On client reconnection with `last_seq`, verify sequence continuity against server ring buffer (retaining last 100,000 events). If sequence gap exceeds buffer capacity, emit `L3_SEQUENCE_GAP_DETECTED` and stream a complete point-in-time L3 book snapshot before resuming incrementals.
2. **Slow Consumer Overrun Isolation:** Maintain dedicated per-subscriber non-blocking ring buffers for L3 data. If a client falls behind by $>5,000$ messages or socket buffers saturate for $>1.5\text{s}$, terminate connection with `L3_CONSUMER_OVERRUN` to prevent backpressure propagating to upstream matching threads.
3. **Audit Trail Verification:** Ensure order lifecycle event sequences in L3 data match WAL sequence numbers 1:1.

**Definition of Done (Acceptance Criteria):**
* [x] Sequence gaps gracefully recover via incremental replay or snapshot — `L3Hub.attach` returns `ReplayGapTooLarge` when `last_seq` predates the ring horizon; `L3Server` answers `L3_SEQUENCE_GAP_DETECTED` then streams a full point-in-time snapshot (`L3BookMirror.Snapshot`/`L3SnapshotReader`) before deferred incrementals (`beginReplay`/`endReplay` pending queue preserves frame order); feed-side discontinuities mark the mirror suspect + journal to Redis (`feedGap`); `TestL3Hub_ReplayGapTooLarge`, `TestL3Hub_FeedGapMarksMirrorSuspect`
* [x] Slow L3 consumers disconnect with structured overrun reason — per-conn outbox lag >`L3MaxOutboxLag` (5,000) or write saturation >`L3SaturationTimeout` (1.5s, `writeStall` counts timeout/deadline errors only — TCP resets are normal disconnects) evicts with `L3_CONSUMER_OVERRUN` + hub `Overruns()` counter; `TestL3Conn_OutboxLagOverrun`, `TestL3Conn_PendingOverflowOverruns`
* [x] L3 sequence monotonic progression perfectly matches WAL tail — the engine emits `wal_seq` = the exact triggering WAL row seq (`journal_seq()` captured pre-append; `L3OrderEvent.wal_seq` slot 10, `internal/ipc/l3.go` decoder rejects zero `l3_seq`/`order_id`); Go-side monotonicity enforced by the hub's per-symbol seq gap detector; `RecoveryManager` re-anchors `set_replay_wal_seq` so replayed engines emit bit-identical wal_seqs

**SDD Checklist:**
- [x] Spec checkpoint: L3 monotonic gap recovery and buffer overrun throttling (§24 #318) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go build ./...`, `go vet ./...`, `go test ./...` green (59 packages)

---

## 17.4 Deliverables

- L3 order-level data generation
- L3 WS distribution
- Market-abuse surveillance signals
- L3 sequence gap recovery protocol & buffer overrun throttling (Task 17.3.4)

---

## 17.5 Dependencies

- Phases 2, 6

---

## 17.6 Duration Estimate

3–4 days (Task 17.3.4 absorbed in range):
- Task 17.3.1 (L3 generation): 1 day
- Task 17.3.2 (L3 WS): 1 day
- Task 17.3.3 (Surveillance): 1 day
- Task 17.3.4 (L3 sequence gap recovery & throttling): 0.5 day
- Testing: 0.5 day

---

## 17.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | C++ core publishes all order events (add, modify, cancel, fill) |
| 2 | Each L3 event has order_id, side, price, qty, timestamp, seq |
| 3 | Per-symbol sequence counter maintained |
| 4 | L3 WS endpoint works (authenticated, premium tier) |
| 5 | Max 5 L3 subscriptions enforced |
| 6 | Real-time order events (no conflation) |
| 7 | Reconnect with last_seq works |
| 8 | Spoofing signal: rapid add/cancel pattern detected |
| 9 | Layering signal: multiple levels add/cancel detected |
| 10 | Wash trade signal: self-trade pattern detected |
| 11 | Marking the close signal: concentrated activity near close detected |
| 12 | Signals stored in surveillance_signals table |
| 13 | Momentum ignition signal detected (rapid aggressive orders triggering stop cascades) |
| 14 | Front-running signal detected per spec §14.4 |
| 15 | Insider-dealing signal detected per spec §14.4 |
| 16 | L3 snapshot generated via async WAL replay (never blocks matching loop); REST endpoint with pagination; staleness ≤ 500ms; > 100K orders returns 413 (§24 #244) |
| 17 | L3 sequence gaps trigger automated TCP snapshot re-sync; slow consumers exceeding buffer limits disconnect with L3_CONSUMER_OVERRUN (§24 #318) |
