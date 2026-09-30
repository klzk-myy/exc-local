-- 080_regulatory_change.down.sql — drop the Task 21.3.25 regulatory
-- change register in FK-safe order.

BEGIN;

DROP TABLE IF EXISTS regulatory_change_correspondence;
DROP TABLE IF EXISTS regulatory_change_impacts;
DROP TABLE IF EXISTS regulatory_changes;

COMMIT;
