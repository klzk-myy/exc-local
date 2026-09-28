# Runbook: `DLQEntriesAccumulating` — dead-letter queue growth

**Severity:** P3 (Slack informational, `#exchange-ops`, repeat 12h) · **Rule:** `nats_stream_messages{stream="ops-dlq"} > 100` for 15m · **Domain:** ops dead-letter queue (`ops-dlq` stream, subjects `ops-dlq.{stream}.{consumer}`, `services/internal/observability/dlq.go`).

## Symptom

The `ops-dlq` stream holds >100 dead letters. Each entry carries headers `X-DLQ-Stream`, `X-DLQ-Consumer`, `X-DLQ-Subject`, `X-DLQ-Deliveries`, `X-DLQ-Reason` and the original payload — messages that exhausted their consumer's delivery policy or were explicitly dead-lettered.

## Diagnosis

1. Enumerate: `natsctl dlq list` (flags `-stream`, `-consumer`, `-limit`); admin HTTP surface `GET /api/v1/admin/dlq` (registered Task 7.3.10).
2. Group by `X-DLQ-Stream`/`X-DLQ-Consumer` — one hot consumer indicates a poison class (schema change, bad payload), not random decay.
3. Sample payloads: `natsctl dlq get <seq>` — look for a shared cause: new event version, unknown enum, oversized frame, or a producer publishing to a subject no consumer understands.
4. Check the producing side timeline: a DLQ burst that starts exactly at a deploy is a schema-version incident, not a consumer bug.

## Mitigation

1. **Never mass-discard.** Each dead letter is a domain event the platform failed to consume once — discarding destroys the only copy (WorkQueue streams already removed the original).
2. Fix the cause first: consumer fix/schema rollout. Then replay: `natsctl dlq replay <seq>` republishes to the recorded `X-DLQ-Subject`; replay in small batches and watch the consumer ack.
3. Only after replay succeeds (or a payload is provably superseded): `natsctl dlq discard <seq>`; record the discard count + reason in the incident ticket — this is the audit trail that replaces the destroyed data.
4. If DLQ growth coexists with `NATSConsumerPendingHigh`, handle the consumer first ([nats-consumer-pending-high.md](./nats-consumer-pending-high.md)) — the DLQ is a symptom.

## Escalation

- P3: Slack triage within the business day. Escalate to P2 if growth is monotonic over 24h, if dead letters touch `trades`/`settlements`/`margin-events` (financial-path events), or if any discard decision is contested — two-operator review (dual control convention per spec §19.16 sensitive ops).
- Recurring poison class → file a producer-side schema bug; the DLQ contract lives in `services/internal/observability/dlq.go` + `services/cmd/natsctl`.
