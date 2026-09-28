# Unified Data Retention Policy

**Phase-09 Task 9.3.22 · spec §19.12 · §24 #212**
Controlled document — Compliance owns the matrix; DevOps owns the
enforcer (`internal/operations/retention`). Machine-readable source of
truth: `infrastructure/data-tiering/tiering_policy.yaml` (kept in lockstep
by `TestShippedPolicyCoversBuiltin`).

---

## 1. Retention matrix

| Data type | Retention | Archival mechanism | Regulatory basis | GDPR interaction |
|---|---|---|---|---|
| Order records (`orders` + partitions) | **5 years** | detach→warm schema→csv+zstd WORM S3 | MiFID II RTS 6 | Art. 17(3)(b) — legal obligation overrides erasure |
| Trades (`trades` partitions) | **5 years** | same | MiFID II RTS 6 | Art. 17(3)(b) |
| Order audit trail (`order_audit`) | **5 years** | same | MiFID II RTS 6 | Art. 17(3)(b) |
| Communications recordings (taping) | **5 years** | WORM object store | MiFID II Art. 16(7) | Art. 17(3)(b); erasure blocked while retain live |
| ClickHouse raw ticks (`tick_history`) | **90 days** | MergeTree TTL → aggregates only | spec §19.12 | no PII |
| OHLCV aggregates (12 persisted intervals) | **5 years** | SummingMergeTree TTL | spec §19.12 | no PII |
| Finance & house reports, GL (`journal_entries`, `ledger_lines`, `ledger_entries`) | **7 years** | detach→warm→WORM S3 | spec §19.12 | Art. 17(3)(b) |
| KYC documents | **account lifetime + 5 years** | lifecycle-bound: account-close + 5y via offboarding (Task 14.3.9) | AML/KYC | Art. 17 erasure honoured after close + floor; Art. 17(3)(b) blocks earlier |
| Audit hash chain (`audit_hash_chain`, `admin_audit_log`) | **7 years** | detach→warm→WORM S3 | spec §19.12 | tamper-evident — never row-purgeable |
| Surveillance signals | **5 years** | in-table; export on demand | spec §19.12 | Art. 17(3)(b) |
| Support tickets / complaints | **5 years** | in-table | MiFID II complaints record-keeping | PII — erasure reviewed against floor |
| Idempotency / dedup windows (`client_order_id_dedup`, `idempotency_keys`) | **7 days** | enforcer row purge | internal (24h semantic window + margin, migration 154) | operational key data |
| Webhook delivery log | **90 days** | enforcer row purge | operational | endpoint metadata only |
| WAL archives | **5 years** | WORM S3; Glacier transition at 90d | DR/evidence (spec §3.5, §18.1) | n/a — order-event journal |

## 2. Enforcement

- **Nightly:** `retention check` (dry-run) validates the matrix against
  live partition state, tier bookkeeping, ClickHouse TTLs (when a CH
  endpoint is wired) and cold-store evidence. Every check writes a
  `retention_audit_log` row; any `VIOLATION` finding emits
  `RETENTION_POLICY_VIOLATION` (P2 — spec §23, Compliance/DevOps) and the
  process exits 1.
- **Monthly:** `scripts/data_migration.sh` runs the lifecycle pass then
  `retention apply`, which performs corrective moves/purges (audited as
  `ACTION` rows).
- **WORM drill:** monthly `archiver verify` re-downloads sampled archives
  and re-verifies SHA-256/manifest/row-count (audit rows `worm_integrity`).

## 3. GDPR legal-hold carve-outs

`data_retention_holds` (migration 194) blocks erasure and every tier
transition — detach, drop, purge — for the covered scope. Holds are
partition-scoped or parent-scoped, carry `case_ref` + `reason`, and are
released only by an explicit Compliance Officer action
(`archiver hold release`). Art. 17(3)(b) overrides are recorded per data
type in the matrix above rather than asserted ad hoc; right-to-erasure
flows (Phase-14) consult this schedule.

## 4. Scope note

Data types bound to a *lifecycle* rather than a wall-clock window (KYC =
account close + 5y) are listed for completeness and enforced by the
offboarding workflow; the nightly enforcer logs them SKIPPED with the
owning flow named — the schedule still governs.
