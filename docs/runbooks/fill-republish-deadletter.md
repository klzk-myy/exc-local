# Runbook: `FillRepublishDeadLettered` — committed fill missing from JetStream

**Severity:** P2 (ticket, same-business-day) · **Rule:** `increase(settlement_fill_consumer{counter="repub_dropped"}[10m]) > 0` · **Domain:** spec §2.7 event-fan-out — the shm dev-topology republisher in `services/internal/settlement/balance_consumer.go` re-emits each committed fill to `trades.{symbol}` / `settlements.{symbol}` after `ProcessFills` succeeds.

## Symptom

`settlement_fill_consumer{shard, counter="repub_dropped"}` incremented. A fill was **settled in PostgreSQL** (balances/journals/positions all correct) but could **not** be re-emitted to JetStream — `nats.Subject` failed token validation on the resolved symbol. The consumer logs `fill republish failed — dead-lettered` with the trade ID and error, then continues processing. Downstream `trades.*`/`settlements.*` consumers (ClickHouse analytics, audit tape, market-data projections) are missing this fill and reconciliation *between the ledger and the streams* will diverge.

Deterministic failure means replay can never succeed — the fill is dead-lettered deliberately rather than wedging the consumer in a restart loop.

## Diagnosis

1. Pull the dead-lettered fill from the log line — the ERROR line carries `trade=<id> shard=<n> err=<subject error>`. The trade row itself is committed:
   `SELECT * FROM trades WHERE id = <trade_id>;`
2. Inspect the raw subject inputs — `instrument_symbol` on the fill and the `instruments.symbol` row it resolves to. `nats.Subject` rejects empty tokens, `.`, `*`, `>`, and whitespace — look for a symbol containing a character JetStream treats as a token separator (e.g. a symbol like `EUR/USD` containing `/` is fine — it is the segment chars `.`, `*`, `>`, space that break a subject).
3. Check whether the failure is structural (the symbol can never form a subject — data-quality defect on the instrument row) or a regression in the enrichment path (`rt.Symbol` unset because the instrument row was deleted after the fill committed).

## Mitigation

1. **The settlement state is correct — do NOT re-book the fill.** The gap is in the event streams, not the ledger.
2. Fix the underlying cause: if the instrument symbol itself is an invalid subject token, the instrument is unrepresentable on this topology — correct the symbol (maker-checker instrument maintenance workflow) and note that fills under the bad symbol will have dead-lettered since it was created.
3. Backfill the missing stream entries: re-emit the fill payload to `trades.{symbol}` and `settlements.{symbol}` with the dedup `Nats-Msg-Id` `s{shard}-{engine_seq}` — downstream consumers dedup on `trade_id`/`processed_trades`, so a manual re-publish is idempotent and safe. The payload is the raw fill frame bytes; reconstruct from the `trades` row if the frame is no longer in the shm ring.
4. If the failure class recurs (multiple fills dead-lettering), treat as a data regression on instrument enrichment — escalate to P1 and investigate the resolver path before more fills commit.

## Escalation

- P2 while isolated; escalate to P1 if `repub_dropped` grows across multiple trades or instruments — that means a systematic enrichment defect, not a single bad symbol.
- Reconciliation consequence: until backfilled, stream-side consumers are missing a committed fill; record the affected `trade_id`s in the incident ticket for the recon team's next divergence audit.
