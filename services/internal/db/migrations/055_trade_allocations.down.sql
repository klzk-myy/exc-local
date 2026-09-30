-- 055_trade_allocations.down.sql — reverse Phase-24 Tasks 24.3.10/.15.

BEGIN;

DELETE FROM chart_of_accounts
 WHERE account_code LIKE '2090_BLOCK_ALLOCATION_CLEARING_%';

DROP TRIGGER IF EXISTS trade_allocation_events_no_update ON trade_allocation_events;
DROP FUNCTION IF EXISTS trade_allocation_events_immutable();
DROP TRIGGER IF EXISTS trade_allocations_conservation_chk ON trade_allocations;
DROP FUNCTION IF EXISTS trade_allocations_conservation_chk();
DROP TRIGGER IF EXISTS apg_account_capacity_chk ON average_price_group_accounts;
DROP FUNCTION IF EXISTS apg_account_capacity_chk();

DROP TABLE IF EXISTS trade_allocation_events;
DROP TABLE IF EXISTS trade_allocations;
DROP TABLE IF EXISTS average_price_group_fills;
DROP TABLE IF EXISTS average_price_group_accounts;
DROP TABLE IF EXISTS average_price_groups;

DROP TYPE IF EXISTS alloc_source_enum;
DROP TYPE IF EXISTS trade_alloc_kind_enum;
DROP TYPE IF EXISTS trade_alloc_status_enum;
DROP TYPE IF EXISTS avg_group_status_enum;
DROP TYPE IF EXISTS alloc_method_enum;
DROP TYPE IF EXISTS alloc_capacity_enum;

COMMIT;
