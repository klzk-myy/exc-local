-- 102_ledger_wallet_shadow.up.sql
-- Task 3.3.6 — spec §5.3 wallet-shadow ledger tables.
--
-- Every balance mutation flows through DoubleEntryLedgerService::Post;
-- ledger_entries is the append-only record of each money movement and
-- journal_sums is the per-(account,currency) verification cache whose
-- GENERATED net_balance MUST equal balances.total inside the same
-- SERIALIZABLE transaction (the service asserts this — §5.3 invariant).
--
-- Fixture-equivalent contract mirrored by services/internal/settlement
-- integration tests; referenced as "migration 102" by migration 004 and
-- ledger_service.go comments.

CREATE TYPE ledger_direction_enum  AS ENUM ('DEBIT','CREDIT');
CREATE TYPE ledger_entry_type_enum AS ENUM
    ('DEPOSIT','WITHDRAWAL','TRADE_FILL','FEE','TRANSFER','SETTLEMENT','ROLLOVER','LIQUIDATION','ADJUSTMENT');

-- Append-only wallet ledger. direction DEBIT = value in to the wallet
-- (net_balance increases), CREDIT = value out — so
-- journal_sums.net_balance = total_debits − total_credits == balances.total.
CREATE TABLE ledger_entries (
    id               BIGSERIAL PRIMARY KEY,
    entry_type       ledger_entry_type_enum NOT NULL,
    reference_id     BIGINT,                        -- trade/transfer/journal source id
    account_id       BIGINT NOT NULL REFERENCES accounts(id),
    currency         VARCHAR(3) NOT NULL,
    direction        ledger_direction_enum NOT NULL,
    amount           DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    running_balance  DECIMAL(28,8) NOT NULL,
    description      VARCHAR(255),
    journal_entry_id BIGINT REFERENCES journal_entries(id),
    posted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_by        VARCHAR(64)
);

CREATE INDEX ledger_entries_acct_ix    ON ledger_entries (account_id, currency, id);
CREATE INDEX ledger_entries_ref_ix     ON ledger_entries (entry_type, reference_id);
CREATE INDEX ledger_entries_journal_ix ON ledger_entries (journal_entry_id);

-- Per-wallet verification cache; net_balance is GENERATED like
-- balances.total (migration 004) so the equality assertion is
-- engine-enforced arithmetic, not stored state.
CREATE TABLE journal_sums (
    account_id    BIGINT        NOT NULL REFERENCES accounts(id),
    currency      VARCHAR(3)    NOT NULL,
    total_debits  DECIMAL(28,8) NOT NULL DEFAULT 0,
    total_credits DECIMAL(28,8) NOT NULL DEFAULT 0,
    net_balance   DECIMAL(28,8) GENERATED ALWAYS AS (total_debits - total_credits) STORED,
    entry_count   BIGINT        NOT NULL DEFAULT 0,
    last_entry_id BIGINT REFERENCES ledger_entries(id),
    PRIMARY KEY (account_id, currency)
);
