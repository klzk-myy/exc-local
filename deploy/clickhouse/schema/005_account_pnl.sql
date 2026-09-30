-- 005_account_pnl.sql — per-account P&L projection (spec §16.3/§16.4,
-- Task 20.3.4). Forward-declared here so the sibling writer
-- (services/internal/analytics/pnl.go) has its table; nothing in
-- Task 20.3.1 inserts into it.
--
-- CONTRACT (pnl.go): writers Upsert the FULL latest bucket values per
-- (account_id, instrument_id, day) — writes are replacements, never
-- deltas; a re-write of the same key with a higher `ver` collapses to
-- the newest row on merge. Reads use FINAL then sum.
--
-- §16.4 convention: `realized`/`unrealized`/`fees` are denominated in
-- the instrument's quote currency; the account base-currency conversion
-- is a read-side/projection concern resolved at report time against the
-- mark.
--
-- Finance retention ≥5–7y ⇒ no TTL (cold copies go to S3 per §19.12).

CREATE TABLE IF NOT EXISTS exchange_analytics.account_pnl
(
    account_id    Int64,
    instrument_id Int64,
    symbol        LowCardinality(String),
    day           Date,
    realized      Decimal(38, 8),
    unrealized    Decimal(38, 8),
    fees          Decimal(38, 8),
    ts            DateTime64(3, 'UTC'),
    ver           UInt64
)
ENGINE = ReplacingMergeTree(ver)
PARTITION BY toYYYYMM(day)
ORDER BY (account_id, instrument_id, day);
