-- 043_legal_agreements.down.sql
BEGIN;

ALTER TABLE accounts DROP COLUMN IF EXISTS umr_in_scope;
DROP TABLE IF EXISTS legal_agreements;
DROP TYPE IF EXISTS legal_agreement_status_enum;
DROP TYPE IF EXISTS legal_agreement_type_enum;

COMMIT;
