-- 152_account_freeze_events.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_account_freeze_events_account;
DROP TABLE IF EXISTS account_freeze_events;
DROP TYPE IF EXISTS freeze_action_enum;

COMMIT;
