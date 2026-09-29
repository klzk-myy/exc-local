-- 037_create_prime_brokerage.down.sql — drops spec §5.22 prime brokerage schema.

BEGIN;

DROP TABLE IF EXISTS pb_giveup_trades;
DROP TABLE IF EXISTS pb_credit_limits;
DROP TABLE IF EXISTS prime_brokers;
DROP TYPE IF EXISTS giveup_status_enum;
DROP TYPE IF EXISTS pb_status_enum;

COMMIT;
