-- 083_client_money_assurance.down.sql
BEGIN;

DROP TABLE IF EXISTS external_auditor_grants;
DROP TABLE IF EXISTS segregation_certifications;
DROP TABLE IF EXISTS client_money_evidence_packs;
DROP TABLE IF EXISTS client_money_audits;

COMMIT;
