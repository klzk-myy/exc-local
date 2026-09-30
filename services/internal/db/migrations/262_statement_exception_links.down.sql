-- 262_statement_exception_links — down

BEGIN;

DROP INDEX IF EXISTS settlement_exceptions_statement_ix;
DROP INDEX IF EXISTS settlement_exceptions_entry_type_ux;

ALTER TABLE settlement_exceptions
    DROP COLUMN IF EXISTS statement_entry_id,
    DROP COLUMN IF EXISTS statement_id,
    DROP COLUMN IF EXISTS nostro_movement_id,
    DROP COLUMN IF EXISTS netting_batch_id,
    DROP COLUMN IF EXISTS cls_instruction_id,
    DROP COLUMN IF EXISTS suspense_mapping_id,
    DROP COLUMN IF EXISTS expected_amount,
    DROP COLUMN IF EXISTS actual_amount;

COMMIT;
