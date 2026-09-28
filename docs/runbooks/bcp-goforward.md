# Runbook: BCP go-forward modes — manual capture, withdrawal-only, frozen-reconcilable

**Phase-09 Task 9.3.27** · **Companion:** [../policies/business-continuity-plan.md](../policies/business-continuity-plan.md) · **Entry:** only by quorum decision via [bcp-standdown.md](./bcp-standdown.md) — the decision record names the mode. Each mode below has its own entry conditions, operating rules, and exit conditions.

Three modes are defined (§19.11.1.2). Only ONE is active at a time; mode changes require a fresh quorum decision.

## Mode GF-1 — Manual trade capture with dual control

**When:** matching engine cannot run but clients/counterparties must be able to trade (e.g., engine fleet dead, gateways + ledger healthy, oracle feeds live).

**Entry conditions:** PG + Redis healthy; oracle feeds fresh (<5s); ledger zero-sum verified (`exchange verify-audit`, `journal_sums`); quorum sign-off recorded.

**Operating rules:**

1. Venue mode `Maintenance` (no public order ingress). Trades are captured bilaterally by Ops under dual control — every manual fill recorded with two operator identities.
2. Records land as compensating journal entries via `audit-append`-equivalent GL posting (spec §5.21 GL, migrations 036/088) — **the manual-trade-capture workflow tooling is pending Phase-09 Task 9.3.27 implementation**; interim: controlled `INSERT` through the settlement GL path with `manual_capture` reference prefix, dual-control executed.
3. Every captured trade references an observable market price (oracle mid at capture time) + counterparty confirmation — no trade at an unverifiable price.
4. Hourly reconciliation: captured set vs counterparties; divergence → halt captures, return to stand-down review.
5. Client money never moves on manual entries until reconciled — settlements queue, do not dispatch.

**Exit:** engine fleet restored + ladder completes; all manual entries reconciled into the restored ledger; quorum declares exit. **Expected duration:** hours–days, reviewed every 4h.

## Mode GF-2 — Withdrawal-only servicing

**When:** trading cannot resume safely but client money must not be trapped (e.g., extended venue reconstruction; books verifiably correct but engine unavailable).

**Entry conditions:** ledger verified (`verify-audit` clean, `journal_sums` zero-sum); positions closed or immaterial; withdrawal pipeline (Phase-11 rails — banking-rails worker pending `services/cmd/banking_rails`) operable manually.

**Operating rules:**

1. Venue mode `Maintenance`; all trading endpoints reject `MAINTENANCE_MODE`.
2. Withdrawals processed under the standard control set — 15min confirm window, review tiers <$10K auto / $10K–$50K standard / >$50K PENDING_REVIEW+4h (canonical per spec/AGENTS) — executed manually by Finance Ops against `withdrawal_confirmations` (migration 008) + `funding_transactions` (migration 007).
3. Deposits suspended — inbound rails marked disabled; inbound wires quarantine at `banking-rails-worker` (pending Phase-11; interim: manual return with audit note).
4. No net-new obligations: no settlements initiated except completing in-flight T+1/T+2 value dates already instructed.
5. Daily proof: end-of-day `exchange merkle --run-daily` balance root vs prior day + per-account delta report to the quorum.

**Exit:** trading returns via resumption ladder, or the venue moves to wind-down governance (legal decision, outside this runbook). **Expected duration:** days–weeks; 48h review cadence.

## Mode GF-3 — Frozen-but-reconcilable state

**When:** nothing can safely move — but the books are provably correct and a restart is credible (e.g., awaiting a part/vendor, region rebuild, legal hold).

**Entry conditions:** full ledger + WAL evidence captured and verified (`recovery_digests` digests, `wal_audit` fingerprints, PG base backup `deploy/postgres/backup.sh` + WAL archive to S3, ClickHouse backup `deploy/clickhouse/backup.sh`); clients informed the venue is frozen with a stated review date.

**Operating rules:**

1. Venue mode `Maintenance`, all services at minimum footprint (cost + attack-surface reduction); monitoring stays live — `admin` service + Prometheus + `ops.alerts.monitoring` keep running.
2. Evidence sealed: WAL/ledger snapshots copied to the WORM archive (S3 Object Lock compliance mode per Task 9.3.17 — integrity verification via `deploy/crons/verify-worm-integrity.sh`; manifest SHA256 per Task 9.3.17 item 2).
3. Frozen-state invariant: no mutation jobs run — suspend the scheduled writers: `deploy/crons/pg-partition-archive.sh` + `verify-worm-integrity.sh` + `redis-failover-drill.sh` timers, the K8s CronJobs (`deploy/k8s/cronjobs/partition-archival-worker.yaml`, `tomnext-rollover.yaml`, `proof-of-reserves-builder.yaml`), `archiver`, `s3-market-data-exporter` — verify each is paused, not merely scheduled-to-fail.
4. Weekly integrity re-verification: `exchange verify-audit` + digest re-check — any drift in a *frozen* system is a forensic event (P0).
5. Client comms: weekly status update via the notification tree even with no change — silence is itself a compliance risk under DORA §19.5.

**Exit:** quorum decides resume (→ resumption ladder) or escalate to GF-1/GF-2, or wind-down. **Expected duration:** days–months; 7-day review cadence.

## Escalation (all modes)

- Every mode is P0-governed; the named decision-maker owns client and regulator comms per the BCP notification tree.
- A go-forward mode abandoned mid-execution returns to the stand-down decision framework — never to normal ops implicitly.
