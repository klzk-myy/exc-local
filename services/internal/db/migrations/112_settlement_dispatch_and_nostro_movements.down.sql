-- 112_settlement_dispatch_and_nostro_movements.down.sql

BEGIN;

DROP TABLE IF EXISTS nostro_movements;
DROP TYPE IF EXISTS nostro_movement_status_enum;
DROP TYPE IF EXISTS nostro_movement_direction_enum;

DROP INDEX IF EXISTS settlement_instructions_due_ix;
DROP INDEX IF EXISTS settlement_instructions_leg_ux;

ALTER TABLE settlement_instructions
    DROP COLUMN IF EXISTS message_format,
    DROP COLUMN IF EXISTS message_payload,
    DROP COLUMN IF EXISTS dispatched_at,
    DROP COLUMN IF EXISTS confirmation_ref,
    DROP COLUMN IF EXISTS updated_at;

COMMIT;
