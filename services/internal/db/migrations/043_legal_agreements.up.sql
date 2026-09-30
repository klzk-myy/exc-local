-- 043_legal_agreements.up.sql
-- Phase-22 Task 22.3.11 (spec §5.25, §15.5, §24 #146): legal-agreement
-- registry gating NDF/FX-option order entry, plus the accounts.umr_in_scope
-- flag that routes the account onto the uncleared-margin (IM/UMR) regime.
--
-- Type/status domains follow spec §5.25 exactly. document_url is the S3/
-- document-store reference (spec column name, VARCHAR(512)). reviewed_by
-- records the Compliance Officer who last changed status (audit trail —
-- admin_audit_log carries the change history).
--
-- One OPEN agreement per (account, type, counterparty): PENDING or EXECUTED
-- rows are unique; terminal states (EXPIRED/TERMINATED) may repeat so the
-- historical record is preserved. counterparty '' = bilateral-with-venue.

BEGIN;

CREATE TYPE legal_agreement_type_enum AS ENUM
    ('ISDA', 'CSA', 'FMSB_GIVEUP', 'PB_AGREEMENT', 'DEA_ADDENDUM');
CREATE TYPE legal_agreement_status_enum AS ENUM
    ('PENDING', 'EXECUTED', 'EXPIRED', 'TERMINATED');

CREATE TABLE legal_agreements (
    id             BIGSERIAL PRIMARY KEY,
    account_id     BIGINT NOT NULL REFERENCES accounts (id),
    agreement_type legal_agreement_type_enum   NOT NULL,
    counterparty   VARCHAR(128),
    status         legal_agreement_status_enum NOT NULL DEFAULT 'PENDING',
    document_url   VARCHAR(512),
    executed_at    TIMESTAMPTZ,
    expires_at     TIMESTAMPTZ,
    reviewed_by    BIGINT,                      -- admin/compliance actor id
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (status <> 'EXECUTED' OR executed_at IS NOT NULL)
);

CREATE INDEX legal_agreements_account_ix
    ON legal_agreements (account_id, agreement_type, status);

CREATE UNIQUE INDEX legal_agreements_open_ux
    ON legal_agreements (account_id, agreement_type, COALESCE(counterparty, ''))
    WHERE status IN ('PENDING', 'EXECUTED');

ALTER TABLE accounts
    ADD COLUMN umr_in_scope BOOLEAN NOT NULL DEFAULT FALSE;

COMMIT;
