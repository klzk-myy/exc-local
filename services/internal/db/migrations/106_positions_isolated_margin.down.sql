-- 106_positions_isolated_margin.down.sql — revert Task 19.3.27 columns.

BEGIN;

ALTER TABLE positions
    DROP COLUMN IF EXISTS auto_margin_replenish;

ALTER TABLE positions
    DROP COLUMN IF EXISTS isolated_margin_allocated;

COMMIT;
