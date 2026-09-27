-- 017_create_kyc_documents.up.sql
-- Spec §5.17 kyc_documents.

BEGIN;

CREATE TYPE kyc_document_status_enum AS ENUM ('PENDING', 'APPROVED', 'REJECTED');

CREATE TABLE kyc_documents (
    id          BIGSERIAL PRIMARY KEY,
    account_id  BIGINT NOT NULL,
    type        VARCHAR(32) NOT NULL,                       -- passport, utility_bill, bank_statement, etc.
    file_url    VARCHAR(512) NOT NULL,                      -- S3 / encrypted storage
    status      kyc_document_status_enum NOT NULL DEFAULT 'PENDING',
    verified_at TIMESTAMPTZ,
    verified_by BIGINT,                                     -- admin user_id
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
