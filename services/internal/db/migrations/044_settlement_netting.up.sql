-- 044_settlement_netting
-- Phase-24 Task 24.3.9 — bilateral payment netting + standing settlement
-- instructions (spec §5.26, §17.7, §24 #155).
--
-- Field vocabulary follows spec §5.26 verbatim (remediation #35 ruled the
-- task text's ssi_id/beneficiary_bank/account_ref/swift_bic and
-- batch_id/gross_amount/net_amount names superseded):
--   standing_settlement_instructions: id, account_id, currency,
--       nostro_or_beneficiary_ref, bic, status, is_default
--   payment_netting_batches: id, counterparty_account_id, currency,
--       value_date, gross_obligation, net_obligation, status
--
-- Additive columns carry the Phase-11 bank_accounts verification link,
-- the dispatch-time SSI snapshot (SSI-change-mid-batch edge case) and the
-- reopened-cycle counter (trade-bust-after-netting edge case).

BEGIN;

CREATE TYPE ssi_status_enum AS ENUM ('ACTIVE', 'REVOKED');

CREATE TABLE standing_settlement_instructions (
    id                          BIGSERIAL PRIMARY KEY,
    account_id                  BIGINT       NOT NULL REFERENCES accounts (id),
    -- Verified Phase-11 beneficiary-registry row this SSI was verified
    -- against (bank_accounts.status = 'VERIFIED'). NULL only for the
    -- exchange's own nostro-side instructions.
    bank_account_id             BIGINT       REFERENCES bank_accounts (bank_account_id),
    currency                    VARCHAR(3)   NOT NULL,
    nostro_or_beneficiary_ref   VARCHAR(64)  NOT NULL,   -- IBAN / account ref of the SSI target
    bic                         VARCHAR(11),             -- receiving bank BIC
    beneficiary_bank            VARCHAR(128),            -- bank name snapshot (from bank_accounts)
    status                      ssi_status_enum NOT NULL DEFAULT 'ACTIVE',
    is_default                  BOOLEAN      NOT NULL DEFAULT FALSE,
    verified_at                 TIMESTAMPTZ,             -- set when registry verification passed
    created_at                  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    revoked_at                  TIMESTAMPTZ
);

-- One ACTIVE default SSI per (account, currency) — spec §17.7 "settlement
-- instructions default to the account's active SSI".
CREATE UNIQUE INDEX ssi_one_default_per_account_currency
    ON standing_settlement_instructions (account_id, currency)
    WHERE status = 'ACTIVE' AND is_default;

CREATE INDEX ssi_account_currency_ix
    ON standing_settlement_instructions (account_id, currency, status);

-- Netting batch lifecycle per spec §5.26: OPEN (accumulating) → NETTED
-- (obligations aggregated, single net payment computed) → DISPATCHED
-- (net payment released to the rail) → SETTLED | FAILED.
CREATE TYPE netting_batch_status_enum AS ENUM
    ('OPEN', 'NETTED', 'DISPATCHED', 'SETTLED', 'FAILED');

CREATE TABLE payment_netting_batches (
    id                        BIGSERIAL PRIMARY KEY,
    counterparty_account_id   BIGINT       NOT NULL REFERENCES accounts (id),
    currency                  VARCHAR(3)   NOT NULL,
    value_date                DATE         NOT NULL,
    gross_obligation          DECIMAL(28,8) NOT NULL DEFAULT 0,
    net_obligation            DECIMAL(28,8) NOT NULL DEFAULT 0,
    -- Net direction from the COUNTERPARTY's perspective: PAY = the
    -- counterparty pays the exchange nostro; RECEIVE = exchange pays out.
    direction                 VARCHAR(8)   CHECK (direction IN ('PAY', 'RECEIVE')),
    ssi_id                    BIGINT       REFERENCES standing_settlement_instructions (id),
    -- Dispatch-time SSI snapshot — a mid-batch SSI change never rewrites
    -- an already-dispatched payment (Task 24.3.9 edge case).
    ssi_snapshot              JSONB,
    rail                      VARCHAR(16),          -- rail the net payment rides
    status                    netting_batch_status_enum NOT NULL DEFAULT 'OPEN',
    reopen_count              INTEGER      NOT NULL DEFAULT 0, -- bust re-open cycles
    dispatch_ref              VARCHAR(64),          -- bank-side payment reference
    netted_at                 TIMESTAMPTZ,
    dispatched_at             TIMESTAMPTZ,
    settled_at                TIMESTAMPTZ,
    created_at                TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX netting_batches_scope_ix
    ON payment_netting_batches (counterparty_account_id, currency, value_date, status);

-- Line-level mapping: every constituent settlement obligation → batch.
-- Retained for break attribution and re-open accounting.
CREATE TABLE netting_batch_lines (
    id                         BIGSERIAL PRIMARY KEY,
    batch_id                   BIGINT NOT NULL REFERENCES payment_netting_batches (id),
    settlement_instruction_id  BIGINT NOT NULL REFERENCES settlement_instructions (id),
    trade_id                   BIGINT NOT NULL,
    direction                  settlement_direction_enum NOT NULL,
    amount                     DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    -- NETTED = inside the batch aggregate; RELEASED = pulled back out
    -- (trade bust after netting re-opens the batch).
    status                     VARCHAR(8) NOT NULL DEFAULT 'NETTED'
        CHECK (status IN ('NETTED', 'RELEASED')),
    released_reason            VARCHAR(32),
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A settlement instruction belongs to at most one live batch.
CREATE UNIQUE INDEX netting_lines_instruction_ux
    ON netting_batch_lines (settlement_instruction_id)
    WHERE status = 'NETTED';

CREATE INDEX netting_lines_batch_ix ON netting_batch_lines (batch_id);

COMMIT;
