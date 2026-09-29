# Runbook: queue-depth backlogs (`NATSConsumerPendingWarn`, `NATSConsumerAckPendingHigh`, `NATSConsumerRedeliveryStorm`, `LoadShedStageActive`, `LoadShedRejectionsOngoing`, `EngineIPCRingDepthWarn`, `EngineIPCSeqStalled`, `EngineIPCTelemetryAbsent`)

**Severity:** P1 (seq stalled) / P2 (ack-pending, load shedding) / P3 (early warnings, telemetry gaps) · **Owner:** SRE

## Symptom

A queue somewhere is not draining: JetStream consumer pending/ack-pending climbing, redeliveries repeating, or the gateway load-shedder active.

## Diagnosis

### ack-pending

`nats_consumer_ack_pending{stream,consumer}` = delivered-but-unacked. >10k for 10m means handlers are wedged mid-processing (not failing — failing messages would redeliver). Check the owning consumer's logs for a stuck batch; compare `nats_consumer_redelivered_total` — if it is *not* moving, messages are held inside their ack window.

### redelivery-storm

`nats_consumer_redelivered_total` increasing means messages repeatedly hit their ack deadline — usually a poison message crashing/timeout the handler on every delivery. Check `nats_stream_messages{stream="ops-dlq"}` (the DLQ; see [dlq-entries-accumulating.md](./dlq-entries-accumulating.md)) — the poison message eventually dead-letters there. Inspect `GET /api/v1/admin/dlq` on the admin service.

### load-shedding

`load_shed_stage` >0 on the gateway means summed Aeron ingress-ring occupancy crossed a §2.7 watermark (80%/95% of `ipc.DefaultRingCapacity` × shard count, `cmd/gateway/main.go` Task 9.3.10 block). `load_shed_rejected_total` rising = clients are being shed (503 + `shed_stage`). Root cause is *downstream* — the engine is not draining the ring; correlate with [ipc-ring-pressure.md](./ipc-ring-pressure.md) and host CPU.

### engine-ipc

`engine_ipc_ring_depth`/`engine_ipc_last_seq` per shard. Seq stalled with depth >0 = consumer wedged or halted → [engine-halt-failover.md](./engine-halt-failover.md).

### telemetry-gap

`EngineIPCTelemetryAbsent` fires when the ring gauges render no samples — the `SetIPCRingDepth`/`SetIPCLastSeq` feeders are not yet wired (the shedder reads occupancy internally via `orderSubmitter.Channel(id).Occupancy()`). Wire the exporter to that source or to the admin Aeron monitor; until then ring pressure is only visible through `load_shed_stage`.

## Mitigation

1. Consumer backlog: scale/restart the wedged consumer; for poison messages triage the DLQ rather than letting redelivery loop.
2. Load shedding is *correct* fail-closed behavior — mitigation is removing downstream pressure, not raising watermarks.
3. Ring depth ≥80% is already covered by `IPCRingSaturated` (p2)/`IPCRingCritical` (p0); this pack's warn rule is the early ticket.

## Escalation

- `EngineIPCSeqStalled` is P1 (shard consumer wedged). Ack-pending and shed rules are P2; escalate to P1 if the same consumer keeps redelivering into the DLQ or shedding persists >30m.
