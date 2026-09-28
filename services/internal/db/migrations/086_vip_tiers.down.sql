-- 086_vip_tiers.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_account_vip_history_account;
DROP TABLE IF EXISTS account_vip_history;
DROP TABLE IF EXISTS account_equity_snapshots;
DROP TABLE IF EXISTS vip_tier_schedule;

ALTER TABLE accounts DROP COLUMN IF EXISTS vip_tier;

COMMIT;
