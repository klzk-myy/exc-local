-- 100_execution_policies.down.sql — drop the Task 21.3.28 execution
-- policy tables in FK-safe order.

BEGIN;

DROP TABLE IF EXISTS execution_policy_consents;
DROP TRIGGER IF EXISTS trg_execution_policies_active ON execution_policies;
DROP FUNCTION IF EXISTS execution_policies_active_immutable;
DROP TABLE IF EXISTS execution_policies;

COMMIT;
