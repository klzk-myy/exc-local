-- 238_demo_accounts.down.sql
-- NOTE: 'DEMO' cannot be removed from account_type_enum once added —
-- PostgreSQL has no DROP VALUE for enum types (documented one-way
-- change; removing it requires an enum rebuild migration, deliberately
-- not attempted here). The down migration drops the demo auxiliary
-- column + sweep index; any residual DEMO account rows must be closed
-- or deleted first (the enum value itself stays valid).

BEGIN;

DROP INDEX IF EXISTS idx_accounts_demo_expiry;
ALTER TABLE accounts DROP COLUMN IF EXISTS demo_expires_at;

COMMIT;
