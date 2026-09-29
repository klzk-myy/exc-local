-- 051_trade_busts.down.sql
-- NOTE: 'VOID' cannot be removed from settlement_status_enum once added —
-- PostgreSQL has no DROP VALUE for enum types (documented one-way change).
BEGIN;

DROP TABLE IF EXISTS trade_busts;
ALTER TABLE trades DROP COLUMN IF EXISTS status;
DROP TYPE IF EXISTS trade_status_enum;

COMMIT;
