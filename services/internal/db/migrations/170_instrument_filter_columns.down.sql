-- 170_instrument_filter_columns.down.sql

BEGIN;

ALTER TABLE instruments
    DROP COLUMN min_price,
    DROP COLUMN max_price,
    DROP COLUMN max_spread_pips,
    DROP COLUMN max_open_orders,
    DROP COLUMN max_algo_orders;

COMMIT;
