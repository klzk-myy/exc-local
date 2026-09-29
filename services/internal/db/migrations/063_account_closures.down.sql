-- Migration 063 down — drop account_closures.
BEGIN;
DROP TABLE IF EXISTS account_closures;
COMMIT;
