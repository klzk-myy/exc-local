-- 059_regulatory_submissions.down.sql — symmetric drop.

BEGIN;

DROP TABLE IF EXISTS regulatory_submission_log;
DROP TABLE IF EXISTS regulatory_submissions;

COMMIT;
