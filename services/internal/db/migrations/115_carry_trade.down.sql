-- 115_carry_trade.down.sql
BEGIN;

DROP TABLE IF EXISTS carry_yield_totals;
DROP TABLE IF EXISTS carry_yield_records;
DROP TABLE IF EXISTS carry_trade_legs;
DROP TABLE IF EXISTS carry_trade_allocations;
DROP TYPE IF EXISTS carry_allocation_status_enum;

COMMIT;
