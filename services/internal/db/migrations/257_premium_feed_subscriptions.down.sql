-- 257_premium_feed_subscriptions.down.sql

BEGIN;

DROP TABLE IF EXISTS premium_feed_subscriptions;
DROP TYPE IF EXISTS premium_feed_status_enum;

COMMIT;
