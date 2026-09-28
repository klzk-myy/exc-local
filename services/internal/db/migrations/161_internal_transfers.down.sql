-- 161_internal_transfers — revert.
BEGIN;

DROP TABLE IF EXISTS transfers;
DROP TYPE IF EXISTS transfer_actor_enum;
DROP TYPE IF EXISTS transfer_status_enum;

COMMIT;
