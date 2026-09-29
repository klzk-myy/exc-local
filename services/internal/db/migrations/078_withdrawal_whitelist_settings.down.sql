-- 078_withdrawal_whitelist_settings.down.sql

BEGIN;

DROP TABLE IF EXISTS withdrawal_whitelist_settings;
DROP TYPE IF EXISTS withdrawal_whitelist_mode_enum;

COMMIT;
