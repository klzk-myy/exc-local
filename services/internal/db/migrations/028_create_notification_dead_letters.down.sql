-- 028_create_notification_dead_letters.down.sql

BEGIN;

DROP TABLE IF EXISTS notification_dead_letters;
DROP TABLE IF EXISTS notification_deliveries;

COMMIT;
