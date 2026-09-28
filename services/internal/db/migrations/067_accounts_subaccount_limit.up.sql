-- 067_accounts_subaccount_limit.up.sql
-- Phase-05 Task 5.3.11: tiered sub-account ceiling on master accounts.
-- Spec §5.2 documents `max_sub_accounts INT DEFAULT 20`; the column is
-- deliberately NULLABLE here so the *effective* default can vary by tier
-- without rewriting every account row: NULL = "use tier default"
-- (T0/T1 → 20 retail, T2 → 100 corporate); an explicit value is the
-- admin-set ceiling (up to 1,000 institutional/ECP/Prime Brokerage).
-- Resolution lives in services/internal/accounts (EffectiveSubAccountLimit).

BEGIN;

ALTER TABLE accounts
    ADD COLUMN max_sub_accounts INTEGER;

ALTER TABLE accounts
    ADD CONSTRAINT accounts_max_sub_accounts_range
    CHECK (max_sub_accounts IS NULL OR (max_sub_accounts >= 0 AND max_sub_accounts <= 1000));

-- Fast "children of master" scans (sub-account list, family aggregate,
-- freeze cascade). Partial: CLOSED sub-accounts are excluded from the
-- active-count check, but kept queryable for audit.
CREATE INDEX idx_accounts_parent ON accounts (parent_account_id)
    WHERE parent_account_id IS NOT NULL;

COMMIT;
