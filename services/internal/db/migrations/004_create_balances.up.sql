-- 004_create_balances.up.sql
-- Spec §5.3 balances (derived cache).
-- total is a STORED generated column — structurally enforces the invariant
-- available + locked = total (the trigger/ledger enforcement lands with the
-- ledger_entries/journal_sums/wallets schema in migration 102).
-- All mutations: SERIALIZABLE + SELECT ... FOR UPDATE (spec §5.3/§5.40).

BEGIN;

CREATE TABLE balances (
    account_id BIGINT       NOT NULL,
    currency   VARCHAR(3)   NOT NULL,
    available  DECIMAL(28,8) NOT NULL DEFAULT 0,
    locked     DECIMAL(28,8) NOT NULL DEFAULT 0,
    total      DECIMAL(28,8) GENERATED ALWAYS AS (available + locked) STORED,
    version    BIGINT        NOT NULL DEFAULT 0,      -- optimistic locking version
    PRIMARY KEY (account_id, currency)
);

COMMIT;
