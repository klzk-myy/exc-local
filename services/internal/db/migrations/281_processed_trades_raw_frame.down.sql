-- 281_processed_trades_raw_frame.down.sql

BEGIN;

DROP INDEX IF EXISTS ix_processed_trades_backlog;

ALTER TABLE processed_trades
    DROP COLUMN raw_frame;

COMMIT;
