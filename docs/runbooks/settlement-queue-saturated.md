# Runbook: `SettlementQueueSaturated` — fill queue backpressuring the out-ring

**Severity:** P2 (page) · **Rule:** `settlement_queue_depth{kind="depth"} / settlement_queue_depth{kind="capacity"} > 0.8` for 2m · **Domain:** in-process buffer between the engine out-ring drain and the settlement `FillConsumer` (`cmd/gateway/main.go`, capacity `1<<14` per shard).

## Symptom

The per-shard fill channel is >80% full. The frame-tap sender blocks on a full queue — backpressure propagates: fill queue → out-ring drain → engine out-ring → `engine_ipc_ring_depth` rises → `IPCRingSaturated`/`IPCRingCritical` fire if this is not resolved. Fills are being committed slower than the engine produces them, or not at all.

## Diagnosis

1. Check whether the fill consumer goroutine is alive — `grep 'settlement fill consumer halted'` in the gateway log. An abort (republish transient failure, `ProcessFills` error) stops the queue from draining until restart.
2. Check `settlement_fill_consumer{counter="malformed"|"republished"|"repub_dropped"}` per shard — malformed-only growth means a decode problem upstream; republished growth with steady depth means PG write latency is the bottleneck, not the consumer itself.
3. Check PG health: `pg_locks` contention on `balances`/`positions`, a runaway transaction pinning the settlement writes, or connection-pool saturation (`db_pool` metrics). The consumer serializes commits per shard — a slow PG stalls one shard's queue while others drain.
4. Check the order-side consumer — order fills and cancel echoes share the out-ring; a burst of mass cancel confirmations can be the volume driver (correlate with `cancel_echo` counters).

## Mitigation

1. **If the consumer goroutine aborted:** restart the gateway — the consumer re-subscribes and replays; `processed_trades` dedups already-committed fills, so restart is safe and cheap.
2. **If PG is the bottleneck:** kill the runaway/pinned transaction blocking settlement writes; the queue drains once commit latency returns to normal. Do NOT bounce PG while the queue is deep — in-flight fills hold locks that must commit or rollback cleanly.
3. **If depth keeps growing after consumer + PG are healthy:** the producer rate exceeds the commit rate durably — the queue is a shock absorber, not a cure. Shed load upstream (`Throttled`/`ReadOnly` mode via `system:degradation:mode`, see [degradation-mode-active.md](./degradation-mode-active.md)) or scale the shard's settlement path.
4. The queue is in-process memory — a restart while it is non-empty loses uncommitted in-flight fills. `engine_seq`-keyed `processed_trades` + WAL replay recover them; a clean consumer-side drain before restart avoids the replay path entirely.

## Escalation

- P2 while depth is <100%; if `IPCRingSaturated`/`IPCRingCritical` fires concurrently, treat as P1 — the engine's own ring is now backpressuring, which is the spec §2.7 fail-closed boundary.
- If the saturation is caused by a fill that deterministically fails `ProcessFills` (poison fill wedge), escalate to P1 — the consumer abort loop will restart-wedge until the bad frame is addressed, and the ring cannot advance past it.
