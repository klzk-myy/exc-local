-- 094_accounts_default_stp.up.sql
-- Spec §6.5 STP modes + Phase-02 Task 2.3.21 (account-default STP mode with
-- category gating). CHECK constraint mirrors the canonical mode set exactly
-- (CANCEL_NEWEST / CANCEL_OLDEST / CANCEL_BOTH / DECREMENT / NONE).

BEGIN;

ALTER TABLE accounts
    ADD COLUMN default_stp_mode VARCHAR(32) NOT NULL DEFAULT 'CANCEL_NEWEST'
        CHECK (default_stp_mode IN ('CANCEL_NEWEST', 'CANCEL_OLDEST',
                                    'CANCEL_BOTH', 'DECREMENT', 'NONE'));

COMMIT;
