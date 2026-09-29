-- 201_unfreeze_requests.down.sql — drop the Phase-12 unfreeze-request table.

BEGIN;

DROP TABLE IF EXISTS unfreeze_requests;

COMMIT;
