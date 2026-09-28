-- 110_multi_currency_pnl.up.sql
-- Phase-03 Task 3.3.9 (Multi-Currency P&L Conversion & Accounting).
--
-- 1. accounts.base_currency: spec §5.2 column (remediation #35) had no
--    assigned migration — added here with IF NOT EXISTS so a later owning
--    migration can still take canonical responsibility. DEFAULT 'USD' per
--    §13.1 base-currency convention.
-- 2. accounts.pnl_settlement_mode: realized-P&L cash destination per
--    §13.1 item 3 — QUOTE_CURRENCY books into the instrument's quote
--    currency cash line; SWEEP_TO_BASE auto-converts to base_currency at
--    the mark mid-rate.
-- 3. currency_conversions: immutable audit record of every FX conversion
--    executed by the settlement path (rate, rate path, GL journal link).
--    Required evidence for §24 #180 ("settled unambiguously in base
--    ledger") and reused by the dust-sweep path (Task 3.3.20).
-- 4. chart_of_accounts: seeds 4040_REALIZED_TRADING_PNL_{CCY} — the
--    realized trading gain/loss account Task 3.3.9 posts against (task
--    step 4 names it explicitly). Migration ordering guarantees 036 has
--    created the table; ON CONFLICT keeps the seed idempotent. The
--    canonical code builder belongs in internal/ledger/chart.go (GL
--    owner); the constant is duplicated in internal/balance until the GL
--    owner adds it.

BEGIN;

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS base_currency VARCHAR(3) NOT NULL DEFAULT 'USD';

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS pnl_settlement_mode VARCHAR(24) NOT NULL DEFAULT 'QUOTE_CURRENCY';

ALTER TABLE accounts
    DROP CONSTRAINT IF EXISTS chk_accounts_pnl_settlement_mode,
    ADD CONSTRAINT chk_accounts_pnl_settlement_mode
        CHECK (pnl_settlement_mode IN ('QUOTE_CURRENCY', 'SWEEP_TO_BASE'));

CREATE TABLE IF NOT EXISTS currency_conversions (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT         NOT NULL REFERENCES accounts (id),
    from_currency    VARCHAR(3)     NOT NULL,
    to_currency      VARCHAR(3)     NOT NULL,
    from_amount      DECIMAL(28,8)  NOT NULL CHECK (from_amount <> 0), -- signed: negative = loss leg
    to_amount        DECIMAL(28,8)  NOT NULL CHECK (to_amount <> 0),   -- same sign as from_amount
    rate             DECIMAL(28,12) NOT NULL CHECK (rate > 0),
    rate_path        VARCHAR(128)   NOT NULL,  -- e.g. 'GBP/USD' or 'AUD/USD,USD/JPY^-1' hop list
    reference_type   VARCHAR(32)    NOT NULL,  -- REALIZED_PNL_SETTLE | REALIZED_PNL_SWEEP | DUST_SWEEP | ...
    reference_id     BIGINT         NOT NULL,  -- trade_id / sweep id
    journal_entry_id BIGINT         REFERENCES journal_entries (id),   -- 036 ordering guaranteed
    created_at       TIMESTAMPTZ    NOT NULL DEFAULT now()
);

-- Idempotency: one conversion record per (reference_type, reference_id,
-- from_currency) — a retried close never double-sweeps.
CREATE UNIQUE INDEX IF NOT EXISTS uq_currency_conversions_ref
    ON currency_conversions (reference_type, reference_id, from_currency);

CREATE INDEX IF NOT EXISTS idx_currency_conversions_account
    ON currency_conversions (account_id, created_at);

-- Realized trading gain/loss CoA sub-accounts (house side of client P&L;
-- REVENUE class — debited on client profit, credited on client loss,
-- matching the SwapRolloverRevenue convention in ledger/swap_accrual.go).
INSERT INTO chart_of_accounts (account_code, account_name, account_type, currency)
SELECT '4040_REALIZED_TRADING_PNL_' || c, 'Realized trading P&L (' || c || ')', 'REVENUE', c
FROM (VALUES ('USD'),('EUR'),('GBP'),('JPY'),('AUD'),('CAD'),('CHF'),('NZD'),('MXN')) AS v(c)
ON CONFLICT (account_code) DO NOTHING;

COMMIT;
