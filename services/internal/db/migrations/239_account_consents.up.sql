-- 239_account_consents.up.sql
-- Phase-21 Task 21.3.28 — generic account-level consent gate table
-- (spec §14.13 consent record; consumed by the Task 14.3.7
-- product-gating order-entry path — no new error code, the rejection
-- rides PRODUCT_NOT_PERMITTED with a policy reason).
--
--   account_consents — one row per (account, consent_type, doc_ref):
--                      the affirmative acknowledgement a client gives
--                      to a versioned document. consent_type is a
--                      bounded enum so the gate can distinguish
--                      EXECUTION_POLICY acknowledgements from future
--                      document families; doc_ref pins the version
--                      consented (re-consent under a material-change
--                      policy upgrade lands a NEW doc_ref row — the
--                      ledger is append-only, never mutated).
--
-- Retention (§19.12): consent records are legal-hold evidence — no
-- purge below the 7-year compliance horizon.

BEGIN;

CREATE TABLE account_consents (
    id           BIGSERIAL    PRIMARY KEY,
    account_id   BIGINT       NOT NULL REFERENCES accounts (id),
    consent_type VARCHAR(32)  NOT NULL CHECK (consent_type IN (
        'EXECUTION_POLICY',   -- venue order-execution policy (Task 21.3.28)
        'RISK_DISCLOSURE',    -- leveraged-FX risk disclosure
        'TERMS_OF_SERVICE',   -- venue terms
        'TARGET_MARKET_ACK')),-- retail target-market acknowledgement
    doc_ref      VARCHAR(64)  NOT NULL,                   -- document/policy version consented
    consented_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    consented_by BIGINT,                                -- client user id
    ip           VARCHAR(45),                           -- client IP evidence
    metadata     JSONB        NOT NULL DEFAULT '{}',    -- surface/locale/user-agent detail
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (account_id, consent_type, doc_ref)          -- one consent per document version
);

-- Gate probe: "does this account hold a consent for doc_ref X?" and the
-- compliance surface listing a document's consent coverage.
CREATE INDEX ix_account_consents_account
    ON account_consents (account_id, consent_type, consented_at DESC);
CREATE INDEX ix_account_consents_doc
    ON account_consents (consent_type, doc_ref);

COMMIT;
