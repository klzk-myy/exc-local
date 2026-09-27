-- 017_create_kyc_documents.down.sql
BEGIN;

DROP TABLE IF EXISTS kyc_documents CASCADE;
DROP TYPE IF EXISTS kyc_document_status_enum;

COMMIT;
