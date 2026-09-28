# Runbook: `NATSConsumerPendingHigh` — JetStream consumer stalled

**Severity:** P1 (page) · **Rule:** `nats_consumer_pending > 50000` for 2m; in-process `nats_consumer_pending` (`NATSConsumerPendingAlert = 50000`) → `ops.alerts.monitoring` · **Domain:** JetStream cold-path backbone — 7 canonical WorkQueue streams (`trades`, `settlements`, `compliance`, `analytics`, `funding`, `margin-events`, `surveillance`), R3, `FileStorage`, 7d `MaxAge`, 2min dedup (`services/internal/nats/streams.go`).

## Symptom

>50,000 messages pending on `{stream}/{consumer}` for >2m. A durable consumer is stalled or its owning service is down — downstream pipeline (settlement GL posting, surveillance, analytics ingest, funding) is behind real time.

## Diagnosis

1. Which consumer: `natsctl health` prints per-stream/consumer lag; the labels identify `stream`+`consumer`. Map consumer → owning service: `trades.*`/`settlements.*` → `settlement`; `compliance.*`/`surveillance.*` → `compliance`; `analytics.*` → analytics spooler (**pending — `services/cmd/analytics` not yet built; §19.13.1**); `margin-events.*` → risk/liquidation (pending Phase-19); `funding.*` → funding (pending Phase-11).
2. Owning service state: K8s pod health, restart loop, `/metrics` + `/healthz` (each `services/cmd/*` serves on its reserved port: settlement :8083, compliance :8084, admin :8085).
3. WorkQueuePolicy note: messages are removed once acked — pending growth is *unacked* work, not storage bloat. Check whether the consumer is receiving-but-not-acking (processing stall) vs not-receiving (dead/pull error).
4. DLQ check: if the consumer poisons on a message, entries may already be diverting — see [dlq-entries-accumulating.md](./dlq-entries-accumulating.md); `natsctl dlq list` on `ops-dlq`.

## Mitigation

1. Dead consumer → restart owning pod; durable consumer resumes from last ack (JetStream file storage, no loss).
2. Poison message → the DLQ path captures it after delivery-policy exhaustion; triage via `natsctl dlq list|get|replay|discard` and the admin endpoint `GET /api/v1/admin/dlq` (registered Phase-07 Task 7.3.10). Do not `discard` until the poison payload is captured in the incident record.
3. Consumer alive but behind: this is capacity, not failure — scale the owning service (HPA floor is 2 replicas; consumer-side concurrency per `services/internal/nats/consumer.go` durable config).
4. Settlement lag specifically: settlement ingest measured ~5,736 fills/s (perf report §6) — a burst beyond that drains at its own pace; pending >50k for >30min at sustained high ingress is expected to recover once ingress normalizes. Do not intervene unless pending is *monotonic* for 1h.

## Escalation

- P1: On-call SRE + owning Component Lead.
- Escalate to P0 if the stalled consumer is `settlement`/`compliance` AND backlog crosses a regulatory or value-date deadline (T+1/T+2 settlement cut — see DORA incident path [../ops/dora-incident-reporting.md](../ops/dora-incident-reporting.md)).
- Verify post-recovery: `nats_consumer_pending` → 0, and no `reconciliation_mismatches_total` increment (replayed events must not double-apply — dedup window 2min on `Nats-Msg-Id`).
