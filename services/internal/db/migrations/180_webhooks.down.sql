-- 180_webhooks.down.sql

BEGIN;

DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_endpoints;

COMMIT;
