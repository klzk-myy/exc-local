-- 061_position_transfers.up.sql
-- Phase-19 Task 19.3.12 (spec §5.38, §13.9, §24 #190).
--
-- position_transfers is the audit register for off-book internal position
-- transfers between sub-accounts / same-legal-entity accounts executed at
-- official mark price with zero spread (spec §13.9). Every completed row
-- links the balanced double-entry GL journal via gl_journal_id
-- (journal_entries.id, migration 036).
--
-- Field vocabulary beyond the spec §5.38 "Key fields" row (that table is
-- a key-fields digest, not the column contract):
--
--   side            — position_side_enum (migration 014). Required because
--                     §13.9's hedged-vs-netting edge case makes the leg
--                     ambiguous without it: a HEDGING account can hold a
--                     LONG and a SHORT row on the same instrument
--                     (migration 233 relatched positions uniqueness to
--                     (account_id, instrument_id, side)).
--   reg_indicator   — §13.9 step 3 regulatory trade indicator
--                     ('POSITION_TRANSFER': MiFID II RTS 1/2 post-trade
--                     transparency exempt, reported in EMIR/CFTC lifecycle
--                     position-continuation reports).
--   failure_reason  — last abort cause for REJECTED/FAILED rows.
--   idempotency_key — client dedup key (UNIQUE where present), same
--                     convention as journal_entries.idempotency_key.
--   created_at / completed_at — the timestamps the spec row calls for.
--
-- transfer_status lifecycle: PENDING → COMPLETED | REJECTED | FAILED.
-- REJECTED = validation refusal (entity mismatch, insufficient position
-- or margin, missing mark); FAILED = committed-attempt machinery fault.

BEGIN;

-- Distinct type name: transfer_status_enum already belongs to migration
-- 161's cash-transfers table — and 061 runs BEFORE 161 in fresh-boot order,
-- so this table cannot depend on that enum existing yet.
CREATE TYPE position_transfer_status_enum AS ENUM ('PENDING', 'COMPLETED', 'REJECTED', 'FAILED');

CREATE TABLE position_transfers (
    id              BIGSERIAL PRIMARY KEY,
    from_account_id BIGINT NOT NULL REFERENCES accounts (id),
    to_account_id   BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id   BIGINT NOT NULL REFERENCES instruments (id),
    side            position_side_enum NOT NULL,
    quantity        DECIMAL(28,8) NOT NULL CHECK (quantity > 0),
    transfer_price  DECIMAL(20,8) NOT NULL,
    reason_code     VARCHAR(32) NOT NULL,
    reg_indicator   VARCHAR(32) NOT NULL DEFAULT 'POSITION_TRANSFER',
    gl_journal_id   BIGINT REFERENCES journal_entries (id),
    transfer_status position_transfer_status_enum NOT NULL DEFAULT 'PENDING',
    authorized_by   BIGINT NOT NULL,
    failure_reason  VARCHAR(255),
    idempotency_key VARCHAR(128),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

-- Idempotent replay: a retried submission resolves to the original row
-- (journal_entries_idem_ux convention, migration 036).
CREATE UNIQUE INDEX position_transfers_idem_ux
    ON position_transfers (idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE INDEX position_transfers_from_ix ON position_transfers (from_account_id, created_at);
CREATE INDEX position_transfers_to_ix   ON position_transfers (to_account_id, created_at);
CREATE INDEX position_transfers_instr_ix ON position_transfers (instrument_id, created_at);

COMMIT;
