-- 171_announcements.down.sql

BEGIN;

DROP TABLE IF EXISTS announcements;
DROP TYPE IF EXISTS announcement_status_enum;
DROP TYPE IF EXISTS announcement_category_enum;

COMMIT;
