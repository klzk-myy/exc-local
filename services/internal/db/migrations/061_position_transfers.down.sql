-- 061_position_transfers.down.sql
BEGIN;

DROP TABLE IF EXISTS position_transfers;
-- position_transfers' own enum only — transfer_status_enum belongs to
-- migration 161's transfers table and must survive.
DROP TYPE IF EXISTS position_transfer_status_enum;

COMMIT;
