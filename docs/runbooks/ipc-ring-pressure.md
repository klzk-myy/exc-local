# Runbook: `IPCRingSaturated` / `IPCRingCritical` — ingress ring backpressure

**Severity:** `IPCRingSaturated` p2 (ticket); `IPCRingCritical` p0 (page) · **Rules:** `engine_ipc_ring_depth > 800000` for 30s (80% of the 1M-entry ring → load shedding); `engine_ipc_ring_depth > 950000` (95% → `CRITICAL_BACKPRESSURE`) · **Domain:** spec §2.7.3 IPC flow control — depths are *entries*, not percentages.

## Symptom

Ingress ring on `{shard}` crossed 80% (shedding threshold — gateways reject market orders/batch submissions with `CAPACITY_EXCEEDED`, HTTP 503 / FIX OrdRejReason=99) or 95% (critical — engine halts ingress to flush matches to WAL). Cancels are never shed: `DELETE /orders/*`, FIX 35=F/35=q flow through the dedicated cancel lane (Task 9.3.10).

## Diagnosis

1. Producer vs consumer: is ingress legitimately bursting (volatility event — check `VOLUME_SPIKE` breaker, market calendar) or is the engine consumer stalled?
   - Engine consume stall → `MATCHING_LOOP_STALLED` warnings (>500µs) in engine log; `WatchdogThread` SIGABRTs at >2ms.
   - CPU starvation → check co-tenant load on the host; engine must own its isolated cores (perf report §4.2: at ~5% of a core the 4,096-slot inbound ring fills in the first beats).
2. **Known defect — permanent halt latch (perf report §4.3):** `EngineLoop::spin_once` skips `run_once` entirely at ≥95% occupancy; once crossed, occupancy can never fall — the halt is a *permanent latch* requiring engine restart. If `IPCRingCritical` fired and depth stays pinned at ~max with `CRITICAL_BACKPRESSURE` log lines, you are in this state: recovery = engine restart through the failover path ([engine-halt-failover.md](./engine-halt-failover.md)), not waiting.
3. Gateway-side: confirm shedding engaged at 80% — `CAPACITY_EXCEEDED` rejections in gateway logs, `http_requests_total` 503 rate. If shedding did NOT engage at 80%, that's a second defect (shedding middleware `services/internal/middleware/shedding.go` — pending Task 9.3.10 wiring check).
4. Queue depth vs the API-gateway shedding threshold: Task 9.3.10 sheds at queue depth >500 (10%→25%→50%), recovers <250 — different ladder from the engine ring (800k/950k); both should agree on direction.

## Mitigation

1. At 80% (saturated, still draining): confirm cancels still flow (cancel lane exempt), monitor `engine_ipc_ring_depth` — the engine drains if consumers are healthy. Do NOT add ingress pressure (no mass order replays, no load tests).
2. At 95% (critical): verify the halt; engine restart via [engine-halt-failover.md](./engine-halt-failover.md) — the latch means no self-recovery exists in the current build (defect flagged to spec owners, perf report §11 item 1; a drain-with-shed or hysteresis fix is a spec-level decision, not an ops one).
3. If bursts are recurring without a defect: this is capacity — `Throttled` mode (capacity >80% → rate limits 50%) should engage upstream; verify `system:degradation:mode`.
4. After recovery: check for dropped ingress — `send_drops`/`ring_drops` counters on producers; dropped client orders are rejections (L2), not silent loss — clients hold their own retry semantics. Verify no *accepted* order was lost: `order_audit` (migration 153) continuity for the window.

## Escalation

- `IPCRingCritical` → P0 tree ([incident-escalation.md](./incident-escalation.md)) — the halt domain is L0-adjacent.
- `IPCRingSaturated` → P2; escalate to P1 if depth keeps climbing 15m with consumers healthy.
- Permanent-halt recurrence is a release blocker — cite perf report §4.3/§11 in the post-mortem.

## telemetry-gap

`EngineIPCTelemetryAbsent` (p3): `engine_ipc_ring_depth`/`engine_ipc_last_seq` emit no samples — `SetIPCRingDepth`/`SetIPCLastSeq` have no wired caller yet. The gateway shedder already reads the same occupancy internally (`orderSubmitter.Channel(id).Occupancy()`, cmd/gateway Task 9.3.10 block) — wire that source into the metrics registry to close the gap; until then `load_shed_stage` is the only ring-pressure signal.
