-- 162_chargebacks — revert.
BEGIN;

DROP TABLE IF EXISTS chargeback_evidence;
DROP TABLE IF EXISTS chargebacks;
DROP TYPE IF EXISTS chargeback_status_enum;

COMMIT;
