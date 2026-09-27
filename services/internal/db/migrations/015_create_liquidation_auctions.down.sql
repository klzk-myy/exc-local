-- 015_create_liquidation_auctions.down.sql
BEGIN;

DROP TABLE IF EXISTS liquidation_auctions CASCADE;
DROP TYPE IF EXISTS auction_phase_enum;

COMMIT;
