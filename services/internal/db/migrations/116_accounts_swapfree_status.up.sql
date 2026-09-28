-- 116_accounts_swapfree_status.up.sql
-- Phase-03 Task 3.3.23 needs accounts.swapfree_status NOW (the 17:00 ET
-- rollover must distinguish VERIFIED swap-free accounts), but the column
-- is canonically owned by migration 095 (Phase-14 Task 14.3.15 swap-free
-- lifecycle). This is an additive bring-forward in the style of migration
-- 110's base_currency: IF NOT EXISTS guards let the later owning migration
-- take canonical responsibility without conflict.
--
-- Deliberately VARCHAR(16)+CHECK rather than the spec's ENUM — the enum
-- type `swapfree_status_enum` remains uncreated so migration 095 can still
-- CREATE TYPE it and ALTER COLUMN ... TYPE at will (a bare CREATE TYPE
-- here would collide with the owning migration later).

BEGIN;

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS swapfree_status VARCHAR(16) NOT NULL DEFAULT 'STANDARD';

ALTER TABLE accounts
    DROP CONSTRAINT IF EXISTS chk_accounts_swapfree_status,
    ADD CONSTRAINT chk_accounts_swapfree_status
        CHECK (swapfree_status IN ('STANDARD', 'PENDING', 'VERIFIED', 'REVOKED'));

COMMIT;
