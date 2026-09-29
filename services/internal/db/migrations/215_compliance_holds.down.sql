-- Migration 215 down — drop compliance_holds.
BEGIN;
DROP TABLE IF EXISTS compliance_holds;
COMMIT;
