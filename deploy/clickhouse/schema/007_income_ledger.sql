-- 007_income_ledger.sql — per-account income projection (spec §16.7,
-- Tasks 20.3.1 ETL + 20.3.12 query endpoint).
--
-- Source of truth is PostgreSQL `ledger_entries` (migration 102, wallet
-- ledger) joined to `journal_entries` (migration 036, GL) — PG remains the
-- book of record for disputes; this table is the read projection behind
-- GET /api/v1/account/income. The ETL batch-polled projection maps
-- ledger entry types onto the §16.7 income taxonomy:
--     FEE → COMMISSION · ROLLOVER → SWAP_ROLLOVER · ADJUSTMENT → NBP_ADJUSTMENT
-- every other entry_type passes through verbatim (future enum additions
-- like REBATE/DUST_CONVERT/FUNDING_FEE land unmapped-but-queryable).
--
-- `amount` is SIGNED from the account's perspective: ledger direction
-- DEBIT (value into the wallet) is positive, CREDIT negative.
--
-- ReplacingMergeTree(ver) on (account_id, currency, ledger_entry_id):
-- cursor replays re-insert the same ledger line with a higher ver and
-- collapse on merge. No TTL — finance retention ≥5–7y (§16.5/§19.12).

CREATE TABLE IF NOT EXISTS exchange_analytics.income_ledger
(
    posted_at        DateTime64(3, 'UTC'),
    account_id       UInt64,
    currency         FixedString(3),
    income_type      LowCardinality(String),
    entry_type       LowCardinality(String),  -- raw PG ledger_entry_type
    amount           Decimal(38, 8),          -- signed: +in / −out
    ledger_entry_id  UInt64,
    journal_entry_id UInt64,
    reference_id     UInt64,
    description      String,
    ver              UInt64
)
ENGINE = ReplacingMergeTree(ver)
PARTITION BY toYYYYMM(posted_at)
ORDER BY (account_id, currency, ledger_entry_id);
