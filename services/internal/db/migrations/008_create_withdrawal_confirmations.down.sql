-- 008_create_withdrawal_confirmations.down.sql
BEGIN;

DROP TABLE IF EXISTS withdrawal_confirmations CASCADE;
DROP TYPE IF EXISTS withdrawal_confirmation_status_enum;

COMMIT;
