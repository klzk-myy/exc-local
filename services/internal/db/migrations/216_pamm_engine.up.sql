-- 216_pamm_engine.up.sql
-- Phase-14 Task 14.3.8 — PAMM/MAM engine tables (spec §5.3 invariant 5 /
-- remediation #38 finding F6, §24 #195).
--
--   pamm_pools              master-sub relationship: manager account owns a
--                           dedicated pool sub-account (real accounts row so
--                           the §5.3 wallet/ledger machinery applies verbatim).
--   pamm_allocations        per-investor invested capital in a pool — the
--                           pro-rata weights for master-fill allocation.
--   pamm_subledger_entries  dedicated internal investment transaction log.
--                           txn_type ∈ PAMM_INVEST|PAMM_REDEEM|PAMM_FEE_PERF|
--                           PAMM_FEE_MGMT — these semantic types NEVER appear
--                           as DEPOSIT/WITHDRAWAL anywhere: the wallet mutation
--                           itself rides a GL journal with entry_type=TRANSFER
--                           (§5.3 invariant 4) while this table records the
--                           PAMM taxonomy. copy_follow_id is set (pool_id NULL)
--                           when the entry belongs to a Task 14.3.14
--                           profit-share accrual on a copy follow.
--   pamm_fill_allocations   deterministic pro-rata allocation of a pool's
--                           master fill across invested capital
--                           (UNIQUE (master_trade_id, allocation_id) → replay
--                           dedup).
--
-- Fiat-allowance segregation (F6): PAMM invest/redeem move wallet balances
-- only (available transfer between investor and pool accounts); they never
-- touch funding_transactions nor the daily fiat withdrawal counters
-- (risk_daily_usage / risk:daily_withdrawn) which only the Phase-11
-- withdrawal pipeline mutates. The trigger below makes the separation
-- structural: pool accounts cannot appear on fiat funding rails at all.
--
-- Chart of accounts: 2170_PAMM_POOL_LIABILITY_{CCY} (LIABILITY, seeded for
-- the §5.1 currency set) records the exchange's pooled-investment obligation
-- separately from free customer liability (2010). It lives in the 2000–2199
-- client-liability range so SegregationOf classifies pooled money as CLIENT
-- (chart.go numbering plan).

BEGIN;

CREATE TYPE pamm_pool_status_enum AS ENUM ('ACTIVE', 'SUSPENDED', 'CLOSED');
CREATE TYPE pamm_txn_type_enum    AS ENUM
    ('PAMM_INVEST', 'PAMM_REDEEM', 'PAMM_FEE_PERF', 'PAMM_FEE_MGMT');

CREATE TABLE pamm_pools (
    pool_id            BIGSERIAL PRIMARY KEY,
    manager_account_id BIGINT NOT NULL REFERENCES accounts (id),
    pool_account_id    BIGINT NOT NULL UNIQUE REFERENCES accounts (id),
    name               VARCHAR(128) NOT NULL,
    currency           VARCHAR(3)   NOT NULL,   -- pool denomination currency
    min_investment     DECIMAL(28,8) NOT NULL DEFAULT 0 CHECK (min_investment >= 0),
    status             pamm_pool_status_enum NOT NULL DEFAULT 'ACTIVE',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX pamm_pools_manager_ix ON pamm_pools (manager_account_id);

CREATE TABLE pamm_allocations (
    allocation_id       BIGSERIAL PRIMARY KEY,
    pool_id             BIGINT NOT NULL REFERENCES pamm_pools (pool_id),
    investor_account_id BIGINT NOT NULL REFERENCES accounts (id),
    currency            VARCHAR(3)    NOT NULL,
    invested            DECIMAL(28,8) NOT NULL DEFAULT 0 CHECK (invested >= 0),
    status              VARCHAR(16)   NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'CLOSED')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (pool_id, investor_account_id)      -- one allocation per investor
);
CREATE INDEX pamm_allocations_pool_ix ON pamm_allocations (pool_id, status);

CREATE TABLE pamm_subledger_entries (
    id               BIGSERIAL PRIMARY KEY,
    txn_type         pamm_txn_type_enum NOT NULL,
    pool_id          BIGINT REFERENCES pamm_pools (pool_id),
    copy_follow_id   BIGINT REFERENCES copy_follows (follow_id),
    account_id       BIGINT NOT NULL,            -- wallet the movement touched
    currency         VARCHAR(3) NOT NULL,
    direction        VARCHAR(6) NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount           DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    journal_entry_id BIGINT REFERENCES journal_entries (id),
    reference_id     BIGINT,                     -- allocation/accrual source id
    narrative        VARCHAR(255),
    idempotency_key  VARCHAR(128),               -- 'pamm:{kind}:{client-key}'
    posted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (pool_id IS NOT NULL OR copy_follow_id IS NOT NULL)
);
-- Idempotent replay: a client-keyed movement dedups; a replay carrying a
-- DIFFERENT amount/account under the same key is rejected in code.
CREATE UNIQUE INDEX pamm_subledger_idem_ux
    ON pamm_subledger_entries (idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX pamm_subledger_pool_ix   ON pamm_subledger_entries (pool_id, txn_type, id);
CREATE INDEX pamm_subledger_follow_ix ON pamm_subledger_entries (copy_follow_id, txn_type, id);
CREATE INDEX pamm_subledger_acct_ix   ON pamm_subledger_entries (account_id, id);

CREATE TABLE pamm_fill_allocations (
    id                  BIGSERIAL PRIMARY KEY,
    pool_id             BIGINT NOT NULL REFERENCES pamm_pools (pool_id),
    master_trade_id     BIGINT NOT NULL,
    allocation_id       BIGINT NOT NULL REFERENCES pamm_allocations (allocation_id),
    investor_account_id BIGINT NOT NULL,
    instrument_id       BIGINT NOT NULL,
    side                VARCHAR(4) NOT NULL CHECK (side IN ('BUY', 'SELL')),
    quantity            DECIMAL(28,8) NOT NULL CHECK (quantity >= 0),
    price               DECIMAL(28,8) NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (master_trade_id, allocation_id)      -- replay dedup
);
CREATE INDEX pamm_fill_allocations_pool_ix ON pamm_fill_allocations (pool_id, master_trade_id);

-- Structural guard (F6): pool accounts are internal investment sub-ledgers —
-- fiat funding rails (DEPOSIT/WITHDRAWAL funding_transactions) are rejected
-- outright. All pool money movement flows through balanced GL journals.
CREATE OR REPLACE FUNCTION pamm_pool_funding_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.type IN ('DEPOSIT', 'WITHDRAWAL') AND EXISTS (
        SELECT 1 FROM pamm_pools WHERE pool_account_id = NEW.account_id) THEN
        RAISE EXCEPTION 'PAMM_INVESTOR_LOCKED: pool account % cannot use fiat funding rails (internal sub-ledger only)',
            NEW.account_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER pamm_pool_funding_guard_trg
    BEFORE INSERT ON funding_transactions
    FOR EACH ROW EXECUTE FUNCTION pamm_pool_funding_guard();

-- Seed the pooled-investment liability accounts (client-segregated range).
INSERT INTO chart_of_accounts (account_code, account_name, account_type, currency)
SELECT '2170_PAMM_POOL_LIABILITY_' || c.ccy,
       'PAMM pooled investment liability (' || c.ccy || ')',
       'LIABILITY'::gl_account_type_enum,
       c.ccy
  FROM (VALUES
      ('USD'), ('EUR'), ('GBP'), ('JPY'), ('AUD'),
      ('CAD'), ('CHF'), ('NZD'), ('MXN')
  ) AS c (ccy);

COMMIT;
