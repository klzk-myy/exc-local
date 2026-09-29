-- 038_orders_execution_params.down.sql
-- Phase-16 Task 16.3.10 — drop residual execution-parameter columns.

BEGIN;

ALTER TABLE orders
    DROP CONSTRAINT orders_peg_mode_check,
    DROP CONSTRAINT orders_fixing_benchmark_check;

ALTER TABLE orders
    DROP COLUMN peg_offset,
    DROP COLUMN peg_mode,
    DROP COLUMN peg_limit,
    DROP COLUMN algo_type,
    DROP COLUMN algo_params,
    DROP COLUMN fixing_benchmark,
    DROP COLUMN hidden,
    DROP COLUMN gslo;

COMMIT;
