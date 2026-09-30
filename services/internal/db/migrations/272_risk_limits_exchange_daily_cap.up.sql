-- 272_risk_limits_exchange_daily_cap.up.sql
-- Phase-11 Task 11.3.2 (remediation of the open AC row "Withdrawal caps
-- enforced (daily, hourly, exchange-wide)"): spec §24 criterion 79 /
-- T11-006 requires an exchange-wide daily withdrawal ceiling in addition
-- to the per-account caps.
--
-- The venue cap lives on risk_limits alongside the other withdrawal
-- columns so the existing LoadLimits/Resolve/Redis-publish machinery
-- carries it. It is honoured ONLY on the fully-global row
-- (account_id IS NULL AND tier IS NULL AND (symbol IS NULL OR
-- symbol = '*')) — the venue scope; a value on a scoped row is ignored
-- at resolution time. Amounts are summed in transaction-currency terms
-- (USD-par), matching the daily_withdraw_limit / max_withdraw_amount
-- seam semantics (spec §27: tier withdrawal caps are USD-par on
-- transaction currency).
--
-- NULL = unset → the venue ceiling is unlimited, consistent with every
-- other nullable risk_limits column; enforcement itself fails closed
-- (a usage-read error rejects the withdrawal).

BEGIN;

ALTER TABLE risk_limits
    ADD COLUMN exchange_daily_withdraw_limit DECIMAL(28,8);

COMMENT ON COLUMN risk_limits.exchange_daily_withdraw_limit IS
    'Exchange-wide daily withdrawal ceiling (UTC calendar day, summed '
    'across all accounts, transaction-currency USD-par). Honoured only '
    'on the global default row; NULL = unlimited. Phase-11 Task 11.3.2.';

COMMIT;
