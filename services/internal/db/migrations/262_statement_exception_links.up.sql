-- 262_statement_exception_links
-- Phase-24 Tasks 24.3.12 + 24.3.8 + 24.3.9 — statement/CLS/netting
-- linkage columns on the shared `settlement_exceptions` break queue
-- (created by 260_settlement_ops, sibling-owned).
--
-- The queue is the single break-investigation surface; these columns let
-- the statement parser (24.3.12), the CLS PvP lifecycle (24.3.8) and the
-- netting engine (24.3.9) attribute a break to its source row without a
-- second exceptions table.

BEGIN;

ALTER TABLE settlement_exceptions
    ADD COLUMN IF NOT EXISTS statement_entry_id BIGINT REFERENCES statement_entries (id),
    ADD COLUMN IF NOT EXISTS statement_id       BIGINT REFERENCES bank_statements (id),
    ADD COLUMN IF NOT EXISTS nostro_movement_id BIGINT REFERENCES nostro_movements (id),
    ADD COLUMN IF NOT EXISTS netting_batch_id   BIGINT REFERENCES payment_netting_batches (id),
    ADD COLUMN IF NOT EXISTS cls_instruction_id BIGINT REFERENCES cls_settlement_instructions (id),
    ADD COLUMN IF NOT EXISTS suspense_mapping_id BIGINT REFERENCES suspense_account_mappings (id),
    ADD COLUMN IF NOT EXISTS expected_amount    DECIMAL(28,8),
    ADD COLUMN IF NOT EXISTS actual_amount      DECIMAL(28,8);

-- Break attribution + dedup: one break per (entry, exception_type).
CREATE UNIQUE INDEX IF NOT EXISTS settlement_exceptions_entry_type_ux
    ON settlement_exceptions (statement_entry_id, exception_type)
    WHERE statement_entry_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS settlement_exceptions_statement_ix
    ON settlement_exceptions (statement_id)
    WHERE statement_id IS NOT NULL;

COMMIT;
