-- 037_create_prime_brokerage.up.sql
-- Spec §5.22 prime brokerage schema: prime_brokers, pb_credit_limits,
-- pb_giveup_trades — Phase-18 Task 18.3.6 (PB Drop Copy & Traiana
-- affirmation), Phase-19 Task 19.3.7 (NOP/DSL pre-trade checks),
-- Phase-24 Task 24.3.7 (give-up reconciliation & break management).
--
-- Deviations from the §5.22 column lists (additive only, recorded for §27):
--   * pb_giveup_trades.trade_id carries NO foreign-key constraint: trades is
--     pg_partman daily-partitioned with composite PK (id, created_at), so a
--     plain FK to trades(id) is not enforceable. The column retains the
--     semantic FK; uniqueness is enforced by (trade_id, prime_broker_id).
--   * pb_giveup_trades gains updated_at TIMESTAMPTZ + rejection_reason
--     VARCHAR(255) (affirmation reject narrative for the Phase-24 break
--     workflow).
--   * pb_credit_limits gains UNIQUE (prime_broker_id, client_account_id,
--     currency_pair) — one limit row per scope; currency_pair NULL is the
--     global scope (PostgreSQL NULLs distinct → enforced via COALESCE index).

BEGIN;

CREATE TYPE pb_status_enum     AS ENUM ('ACTIVE', 'SUSPENDED');
CREATE TYPE giveup_status_enum AS ENUM ('PENDING', 'AFFIRMED', 'REJECTED', 'DISPUTED', 'SETTLED');
-- DISPUTED (added to §5.22 enum 2026-09-27, remediation #35) is the
-- break-investigation state written by Phase-24 Task 24.3.7.

CREATE TABLE prime_brokers (
    id           BIGSERIAL PRIMARY KEY,
    pb_name      VARCHAR(128)   NOT NULL,           -- e.g. 'JPMorgan FXPB'
    bic_code     VARCHAR(11)    NOT NULL,           -- SWIFT BIC
    fix_comp_id  VARCHAR(64)    NOT NULL UNIQUE,    -- FIX TargetCompID for Drop Copy
    traiana_code VARCHAR(64),                        -- Traiana Harmony participant ID
    status       pb_status_enum NOT NULL DEFAULT 'ACTIVE',
    created_at   TIMESTAMPTZ    NOT NULL DEFAULT now()
);

CREATE TABLE pb_credit_limits (
    id                        BIGSERIAL PRIMARY KEY,
    prime_broker_id           BIGINT        NOT NULL REFERENCES prime_brokers (id),
    client_account_id         BIGINT        NOT NULL REFERENCES accounts (id),
    currency_pair             VARCHAR(16),          -- NULL = global across all pairs
    net_open_position_limit   DECIMAL(28,8),        -- NOP limit (USD equivalent)
    current_net_open_position DECIMAL(28,8) NOT NULL DEFAULT 0,
    daily_settled_limit       DECIMAL(28,8),        -- DSL limit (USD equivalent)
    current_daily_settled     DECIMAL(28,8) NOT NULL DEFAULT 0,
    updated_at                TIMESTAMPTZ   NOT NULL DEFAULT now()
);
-- One limit row per (pb, client, pair-or-global) scope; COALESCE makes the
-- NULL global row participate in uniqueness.
CREATE UNIQUE INDEX pb_credit_limits_scope_ux
    ON pb_credit_limits (prime_broker_id, client_account_id, COALESCE(currency_pair, ''));
CREATE INDEX pb_credit_limits_client_ix ON pb_credit_limits (client_account_id);

CREATE TABLE pb_giveup_trades (
    id                         BIGSERIAL PRIMARY KEY,
    trade_id                   BIGINT             NOT NULL, -- semantic FK -> trades.id (see header)
    prime_broker_id            BIGINT             NOT NULL REFERENCES prime_brokers (id),
    executing_broker_account_id BIGINT            NOT NULL REFERENCES accounts (id),
    client_account_id          BIGINT             NOT NULL REFERENCES accounts (id),
    giveup_status              giveup_status_enum NOT NULL DEFAULT 'PENDING',
    traiana_message_id         VARCHAR(64),                 -- external affirmation reference
    rejection_reason           VARCHAR(255),                -- PB reject narrative (additive)
    affirmed_at                TIMESTAMPTZ,
    created_at                 TIMESTAMPTZ        NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ        NOT NULL DEFAULT now(),
    UNIQUE (trade_id, prime_broker_id)            -- idempotent re-copy per PB
);
CREATE INDEX pb_giveup_trades_pending_ix ON pb_giveup_trades (created_at)
    WHERE giveup_status = 'PENDING';              -- affirmation-timeout sweep
CREATE INDEX pb_giveup_trades_traiana_ix ON pb_giveup_trades (traiana_message_id)
    WHERE traiana_message_id IS NOT NULL;
CREATE INDEX pb_giveup_trades_client_ix  ON pb_giveup_trades (client_account_id);

COMMIT;
