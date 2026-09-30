-- 058_shard_margin_reservations.down.sql

BEGIN;

DROP TABLE IF EXISTS shard_margin_reservations;
DROP TYPE IF EXISTS shard_reservation_status_enum;

COMMIT;
