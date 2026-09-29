-- 198_funding_fee_schedule.up.sql
-- Phase-11 Task 11.3.9 — funding fee schedule + currency conversion records.
--
-- funding_fee_tiers carries the per-(rail, currency, direction, account_tier)
-- fee schedule described by the task: flat_fee + percentage_bps bounded by
-- min_fee/max_fee, a free_tier_monthly_count allowance, and scheduling via
-- effective_date. Rows are VERSIONED — an update inserts a new row
-- (version+1, supersedes_id → the replaced row); withdrawn rows carry
-- retired_at and drop out of resolution. '*' is the wildcard for
-- currency/account_tier (a wildcard row applies when no exact row matches).
--
-- funding_fee_free_usage counts the month's consumed free movements per
-- (account, direction) — the free_tier_monthly_count enforcement ledger.
--
-- funding_currency_conversions persists every indicative deposit-conversion
-- quote: deposit currency ≠ account currency converts at the reference
-- mid-rate ± conversion_spread_bps (Phase-11 Task 11.3.9 item 4). Named
-- distinctly from currency_conversions (migration 110), which is the
-- realized-P&L sweep audit table with an incompatible shape.

BEGIN;

CREATE TABLE funding_fee_tiers (
    id                        BIGSERIAL PRIMARY KEY,
    rail                      bank_method_enum NOT NULL,            -- SWIFT|SEPA|FEDNOW|ACH|CHAPS|TARGET2|WIRE|INTERNAL
    currency                  VARCHAR(3)       NOT NULL,            -- ISO 4217 or '*' wildcard
    direction                 VARCHAR(12)      NOT NULL,            -- DEPOSIT | WITHDRAWAL
    account_tier              VARCHAR(12)      NOT NULL DEFAULT '*',-- '*' wildcard | T0 | T1 | T2
    flat_fee                  DECIMAL(20,8)    NOT NULL DEFAULT 0,
    percentage_bps            DECIMAL(10,4)    NOT NULL DEFAULT 0,
    min_fee                   DECIMAL(20,8)    NOT NULL DEFAULT 0,
    max_fee                   DECIMAL(20,8),                        -- NULL = uncapped
    free_tier_monthly_count   INT              NOT NULL DEFAULT 0,  -- free movements per UTC calendar month
    effective_date            TIMESTAMPTZ      NOT NULL,            -- scheduled start of this version
    version                   INT              NOT NULL DEFAULT 1,
    supersedes_id             BIGINT           REFERENCES funding_fee_tiers (id),
    retired_at                TIMESTAMPTZ,                          -- explicit withdrawal; NULL = resolvable
    retired_by                BIGINT,
    created_by                BIGINT           NOT NULL,
    created_at                TIMESTAMPTZ      NOT NULL DEFAULT now(),
    CONSTRAINT funding_fee_tiers_direction_chk CHECK (direction IN ('DEPOSIT','WITHDRAWAL')),
    CONSTRAINT funding_fee_tiers_tier_chk      CHECK (account_tier IN ('*','T0','T1','T2')),
    CONSTRAINT funding_fee_tiers_currency_chk  CHECK (
        currency ~ '^[A-Z]{3}$' OR currency = '*'),
    CONSTRAINT funding_fee_tiers_nonneg_chk    CHECK (
        flat_fee >= 0 AND percentage_bps >= 0 AND min_fee >= 0
        AND free_tier_monthly_count >= 0
        AND (max_fee IS NULL OR max_fee >= min_fee)),
    CONSTRAINT funding_fee_tiers_version_chk   CHECK (version >= 1),
    CONSTRAINT funding_fee_tiers_uq            UNIQUE (rail, currency, direction, account_tier, effective_date)
);

-- Fee resolution hot path: (rail, direction) + specificity-ordered
-- currency/tier, newest non-retired effective version first.
CREATE INDEX idx_funding_fee_tiers_resolve
    ON funding_fee_tiers (rail, direction, currency, account_tier, effective_date DESC)
    WHERE retired_at IS NULL;

CREATE TABLE funding_fee_free_usage (
    account_id    BIGINT      NOT NULL REFERENCES accounts (id),
    direction     VARCHAR(12) NOT NULL,
    period_month  DATE        NOT NULL,   -- first day of the UTC calendar month
    used_count    INT         NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, direction, period_month),
    CONSTRAINT funding_fee_free_usage_direction_chk CHECK (direction IN ('DEPOSIT','WITHDRAWAL')),
    CONSTRAINT funding_fee_free_usage_count_chk     CHECK (used_count >= 0)
);

CREATE TABLE funding_currency_conversions (
    id                     BIGSERIAL PRIMARY KEY,
    account_id             BIGINT         NOT NULL REFERENCES accounts (id),
    direction              VARCHAR(12)    NOT NULL DEFAULT 'DEPOSIT',
    from_currency          VARCHAR(3)     NOT NULL,
    to_currency            VARCHAR(3)     NOT NULL,
    amount_from            DECIMAL(28,8)  NOT NULL,
    mid_rate               DECIMAL(28,10) NOT NULL,  -- reference mid: to_currency per 1 from_currency
    spread_bps             DECIMAL(10,4)  NOT NULL,
    rate_applied           DECIMAL(28,10) NOT NULL,  -- mid × (1 − spread/10⁴)
    amount_to              DECIMAL(28,8)  NOT NULL,
    rate_source            VARCHAR(32)    NOT NULL,  -- oracle provenance enum / fx_cross_usd
    rate_valid_at          TIMESTAMPTZ    NOT NULL,  -- observation/read timestamp (staleness anchor)
    funding_transaction_id BIGINT         REFERENCES funding_transactions (id),
    created_at             TIMESTAMPTZ    NOT NULL DEFAULT now(),
    CONSTRAINT funding_currency_conversions_direction_chk CHECK (direction IN ('DEPOSIT','WITHDRAWAL')),
    CONSTRAINT funding_currency_conversions_amounts_chk   CHECK (
        amount_from > 0 AND mid_rate > 0 AND spread_bps >= 0
        AND rate_applied > 0 AND amount_to > 0),
    CONSTRAINT funding_currency_conversions_pair_chk      CHECK (from_currency <> to_currency)
);

CREATE INDEX idx_funding_currency_conversions_account
    ON funding_currency_conversions (account_id, created_at DESC, id DESC);

COMMIT;
