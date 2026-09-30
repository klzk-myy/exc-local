-- 112_settlement_dispatch_and_nostro_movements.up.sql
-- Phase-03 Task 3.3.3 (spec §5.19, §6.3, §17.1, §24 #19/#20).
--
-- Three additions to the settlement-instruction pipeline:
--
--   1. Idempotent fill replay — one row per settlement leg, enforced by a
--      UNIQUE index on (trade_id, account_id, currency, direction). A spot
--      FX trade writes four legs (buyer RECEIVE base / PAY quote; seller
--      PAY base / RECEIVE quote); self-trades stay unique because the PAY
--      and RECEIVE legs differ in direction and the base/quote currencies
--      always differ.
--
--   2. Dispatch columns — the Phase-11 banking-rails seam: the
--      settlement-date job renders the SWIFT MT202 / ISO 20022 pacs.009
--      payload and records it (message_format + message_payload +
--      dispatched_at) alongside swift_message_id (the :20:/MsgId
--      transaction reference). The partial index backs the due-scan
--      (PENDING + not yet dispatched, by settlement_date).
--
--   3. nostro_movements — the nostro accounting INTENT ledger. On
--      confirmation the settlement service flips the instruction to
--      SETTLED and writes one movement row per leg (PAY leg → DEBIT the
--      nostro, RECEIVE leg → CREDIT, per spec §17.1). Phase-24 nostro
--      accounting (Task 24.3.1) consumes PENDING movements and applies
--      the nostro_accounts.balance mutation — this service records the
--      intent only.

BEGIN;

ALTER TABLE settlement_instructions
    ADD COLUMN IF NOT EXISTS message_format    VARCHAR(8),   -- 'MT202' | 'PACS009'
    ADD COLUMN IF NOT EXISTS message_payload   TEXT,         -- rendered SWIFT / ISO 20022 body
    ADD COLUMN IF NOT EXISTS dispatched_at     TIMESTAMPTZ,  -- when the payload was claimed for send
    ADD COLUMN IF NOT EXISTS confirmation_ref  VARCHAR(64),  -- correspondent confirmation reference
    ADD COLUMN IF NOT EXISTS updated_at        TIMESTAMPTZ NOT NULL DEFAULT now();

-- One row per settlement leg — ON CONFLICT DO NOTHING replay dedup.
CREATE UNIQUE INDEX settlement_instructions_leg_ux
    ON settlement_instructions (trade_id, account_id, currency, direction);

-- Due-scan for the settlement-date dispatch job.
CREATE INDEX settlement_instructions_due_ix
    ON settlement_instructions (settlement_date)
    WHERE status = 'PENDING' AND swift_message_id IS NULL;

CREATE TYPE nostro_movement_direction_enum AS ENUM ('DEBIT', 'CREDIT');
CREATE TYPE nostro_movement_status_enum    AS ENUM ('PENDING', 'POSTED', 'VOID');

CREATE TABLE nostro_movements (
    id                        BIGSERIAL PRIMARY KEY,
    settlement_instruction_id BIGINT NOT NULL REFERENCES settlement_instructions (id),
    nostro_account_id         BIGINT NOT NULL REFERENCES nostro_accounts (id),
    currency                  VARCHAR(3) NOT NULL,
    amount                    DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    direction                 nostro_movement_direction_enum NOT NULL,
    status                    nostro_movement_status_enum NOT NULL DEFAULT 'PENDING',
    confirmation_ref          VARCHAR(64),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at                 TIMESTAMPTZ
);

-- One movement per instruction leg (idempotent-confirm backstop) and a
-- work queue for the Phase-24 nostro poster.
CREATE UNIQUE INDEX nostro_movements_instruction_ux
    ON nostro_movements (settlement_instruction_id);
CREATE INDEX nostro_movements_pending_ix
    ON nostro_movements (created_at)
    WHERE status = 'PENDING';

-- statement_entries (057) declares reconciled_payment_id /
-- reconciled_movement_id without FK clauses because it is numbered ahead
-- of 108 (rail_payments) and this file; both constraints land here.
ALTER TABLE statement_entries
    ADD CONSTRAINT statement_entries_payment_fk
    FOREIGN KEY (reconciled_payment_id) REFERENCES rail_payments (id);
ALTER TABLE statement_entries
    ADD CONSTRAINT statement_entries_movement_fk
    FOREIGN KEY (reconciled_movement_id) REFERENCES nostro_movements (id);

COMMIT;
