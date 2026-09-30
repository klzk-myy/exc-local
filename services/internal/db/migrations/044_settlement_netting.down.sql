-- 044_settlement_netting — down

BEGIN;

DROP TABLE IF EXISTS netting_batch_lines;
DROP TABLE IF EXISTS payment_netting_batches;
DROP TYPE IF EXISTS netting_batch_status_enum;
DROP TABLE IF EXISTS standing_settlement_instructions;
DROP TYPE IF EXISTS ssi_status_enum;

COMMIT;
