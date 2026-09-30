-- 243_gdpr_geo.down.sql — reverses Task 21.3.7.

BEGIN;

DROP TABLE IF EXISTS geo_jurisdiction_policies;
DROP TABLE IF EXISTS gdpr_requests;
DROP TABLE IF EXISTS account_consent_events;
DROP TABLE IF EXISTS account_consent_states;

COMMIT;
