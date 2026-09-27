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

**Definition of Done (Acceptance Criteria):**
* [ ] C++ core publishes all order events (add, modify, cancel, fill)
* [ ] Each event has order_id, side, price, qty, timestamp, seq
* [ ] Per-symbol sequence counter maintained

**SDD Checklist:**
- [ ] Spec checkpoint: L3 order-level data — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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
* [ ] L3 WS endpoint works (authenticated, premium)
* [ ] Max 5 L3 subscriptions enforced
* [ ] Real-time order events (no conflation)
* [ ] Reconnect with last_seq works
* [ ] L3 snapshot generated asynchronously via WAL replay — never blocks matching loop
* [ ] REST `GET /api/v1/market-data/l3-snapshot/{symbol}` with pagination
* [ ] Snapshot staleness ≤ 500ms; books > 100K orders return 413 L3_SNAPSHOT_TOO_LARGE

**SDD Checklist:**
- [ ] Spec checkpoint: L3 WS distribution — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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
* [ ] Spoofing signal: rapid add/cancel pattern detected
* [ ] Layering signal: multiple levels add/cancel detected
* [ ] Wash trade signal: self-trade pattern detected
* [ ] Marking the close signal: concentrated activity near close detected
* [ ] Momentum ignition signal: rapid aggressive orders triggering stop cascades detected
* [ ] Front-running signal: order placed ahead of known large incoming order detected (spec §14.4)
* [ ] Insider-dealing signal: anomalous trades before scheduled market-moving events detected (spec §14.4)
* [ ] Signals stored in surveillance_signals table

**SDD Checklist:**
- [ ] Spec checkpoint: market-abuse signal generation — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 17.3.4: L3 Monotonic Gap Recovery & Buffer Overrun Throttling

**Objective:** Implement strict monotonic sequence gap recovery, consumer overrun disconnects, and buffer memory safeguards for L3 market data streams per spec §2.7, §11.1, and §24 #318.

**Implementation:**
1. **L3 Monotonic Gap Detection:** On client reconnection with `last_seq`, verify sequence continuity against server ring buffer (retaining last 100,000 events). If sequence gap exceeds buffer capacity, emit `L3_SEQUENCE_GAP_DETECTED` and stream a complete point-in-time L3 book snapshot before resuming incrementals.
2. **Slow Consumer Overrun Isolation:** Maintain dedicated per-subscriber non-blocking ring buffers for L3 data. If a client falls behind by $>5,000$ messages or socket buffers saturate for $>1.5\text{s}$, terminate connection with `L3_CONSUMER_OVERRUN` to prevent backpressure propagating to upstream matching threads.
3. **Audit Trail Verification:** Ensure order lifecycle event sequences in L3 data match WAL sequence numbers 1:1.

**Definition of Done (Acceptance Criteria):**
* [ ] Sequence gaps gracefully recover via incremental replay or snapshot
* [ ] Slow L3 consumers disconnect with structured overrun reason
* [ ] L3 sequence monotonic progression perfectly matches WAL tail

**SDD Checklist:**
- [ ] Spec checkpoint: L3 monotonic gap recovery and buffer overrun throttling (§24 #318) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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
