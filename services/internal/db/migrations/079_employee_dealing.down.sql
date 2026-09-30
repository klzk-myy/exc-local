-- 079_employee_dealing.down.sql — Phase-21 Task 21.3.24.

BEGIN;

DROP TABLE IF EXISTS employee_trade_reviews;
DROP TRIGGER IF EXISTS pre_clearance_immutable_trg ON pre_clearance_requests;
DROP FUNCTION IF EXISTS p21_pre_clearance_immutable;
DROP TABLE IF EXISTS pre_clearance_requests;
DROP TABLE IF EXISTS restricted_lists;

ALTER TABLE accounts
    DROP COLUMN IF EXISTS employee_role,
    DROP COLUMN IF EXISTS employee_account;

COMMIT;
