-- 019_create_settlement_instructions.up.sql
-- Spec §5.19 settlement_instructions.
-- NOTE: spec annotates trade_id as "FK → trades". trades is partitioned by
-- created_at with PK (id, created_at); id alone has no UNIQUE constraint, so
-- a declarative FK on trade_id is structurally impossible on a daily-
-- partitioned parent. trade_id is a plain BIGINT NOT NULL; referential
-- integrity is enforced by the settlement writer (Phase-24), same discipline
-- as the audit pipeline. Documented deviation per AGENTS.md change protocol.

BEGIN;

CREATE TYPE settlement_direction_enum AS ENUM ('PAY', 'RECEIVE');
CREATE TYPE settlement_status_enum    AS ENUM ('PENDING', 'SETTLED', 'FAILED', 'RECONCILED');

CREATE TABLE settlement_instructions (
    id                BIGSERIAL PRIMARY KEY,
    trade_id          BIGINT NOT NULL,                      -- logical ref → trades.id (see header note)
    account_id        BIGINT NOT NULL,
    currency          VARCHAR(3) NOT NULL,
    amount            DECIMAL(28,8) NOT NULL,
    direction         settlement_direction_enum NOT NULL,
    settlement_date   DATE,                                 -- T+1 or T+2
    nostro_account_id BIGINT REFERENCES nostro_accounts (id),
    status            settlement_status_enum NOT NULL DEFAULT 'PENDING',
    swift_message_id  VARCHAR(64),                          -- SWIFT MT202 / pacs.009 reference
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at        TIMESTAMPTZ
);

COMMIT;
