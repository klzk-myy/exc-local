-- 216_pamm_engine.down.sql — reverse of 216 (Task 14.3.8).
-- Run BEFORE reverting 097: pamm_subledger_entries references copy_follows.

BEGIN;

DELETE FROM chart_of_accounts
 WHERE account_code LIKE '2170_PAMM_POOL_LIABILITY_%';

DROP TRIGGER IF EXISTS pamm_pool_funding_guard_trg ON funding_transactions;
DROP FUNCTION IF EXISTS pamm_pool_funding_guard();

DROP TABLE IF EXISTS pamm_fill_allocations;
DROP TABLE IF EXISTS pamm_subledger_entries;
DROP TABLE IF EXISTS pamm_allocations;
DROP TABLE IF EXISTS pamm_pools;

DROP TYPE IF EXISTS pamm_txn_type_enum;
DROP TYPE IF EXISTS pamm_pool_status_enum;

COMMIT;
