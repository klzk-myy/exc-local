-- 244_data_residency.up.sql
-- Phase-21 Task 21.3.18 — jurisdictional data-residency enforcement &
-- cross-border administrative access controls (spec §14.13, §19.12;
-- deploy/storage/residency_policy.yml is the environment-tunable
-- sibling of this seed).
--
--   data_residency_policies    — jurisdiction → storage placement +
--                               KMS key map. jurisdiction_code is the
--                             logical residency tag ('EU', 'GB', 'CH',
--                             'SG', 'US', 'ROW'); applies_to lists the
--                             ISO country codes the tag covers so a
--                             client country resolves to exactly one
--                             policy. home_region is the regional
--                             PostgreSQL/ClickHouse/S3 placement; the
--                             KMS key is region-scoped. adequate +
--                             transfer_instrument model the GDPR Art.
--                             44+ cross-border transfer legality the
--                             replicator gate enforces.
--
--   data_residency_access_log — immutable audit of administrative
--                             access to resident data: every cross-
--                             border administrative query lands a row
--                             with the mandated justification; same-
--                             region reads may be logged too (the
--                             service decides), cross_border flags the
--                             regulated subset.
--
--   accounts.jurisdiction_code — the residency tag pinned at
--                             onboarding (from KYC jurisdiction /
--                             declared country); everything downstream
--                             (storage routing, cross-border checks,
--                             report partitioning) reads this column.

BEGIN;

CREATE TABLE data_residency_policies (
    jurisdiction_code    VARCHAR(8)  PRIMARY KEY,
    home_region          VARCHAR(32) NOT NULL,   -- region bucket: eu-central | uk | ch | ap-sg | us-east
    kms_key_id           VARCHAR(256) NOT NULL,  -- region-scoped KMS key for resident data
    s3_bucket            VARCHAR(128) NOT NULL,  -- regional object bucket
    pg_partition         VARCHAR(64) NOT NULL,   -- regional PG placement tag
    applies_to           JSONB       NOT NULL DEFAULT '[]', -- ISO country codes mapped to this jurisdiction
    adequate             BOOLEAN     NOT NULL DEFAULT FALSE, -- adequacy decision for transfers
    transfer_instrument  VARCHAR(32) NOT NULL DEFAULT 'NONE'
                         CHECK (transfer_instrument IN ('NONE', 'ADEQUACY', 'SCC', 'BCR')),
    description          TEXT        NOT NULL DEFAULT '',
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seed mirrors deploy/storage/residency_policy.yml. EU row covers the
-- EU/EEA member states; GB/CH/SG/US are single-country jurisdictions;
-- ROW is the fail-safe that keeps unknown countries OUT of EU storage
-- (fail-closed: no policy match → service refuses, never guesses).
INSERT INTO data_residency_policies
    (jurisdiction_code, home_region, kms_key_id, s3_bucket, pg_partition,
     applies_to, adequate, transfer_instrument, description) VALUES
    ('EU', 'eu-central',
     'arn:aws:kms:eu-central-1:000000000000:key/exc-eu-resident',
     'exc-eu-resident-data', 'pg_eu_central',
     '["AT","BE","BG","HR","CY","CZ","DK","EE","FI","FR","DE","GR","HU",
       "IE","IT","LV","LT","LU","MT","NL","PL","PT","RO","SK","SI","ES",
       "SE","IS","LI","NO"]'::jsonb,
     FALSE, 'NONE',
     'EU/EEA residents — data stays in EU region (GDPR Chapter V)'),
    ('GB', 'uk',
     'arn:aws:kms:eu-west-2:000000000000:key/exc-uk-resident',
     'exc-uk-resident-data', 'pg_uk',
     '["GB"]'::jsonb,
     TRUE, 'ADEQUACY',
     'UK residents — UK adequacy decision covers EU↔UK transfers'),
    ('CH', 'ch',
     'arn:aws:kms:eu-central-1:000000000000:key/exc-ch-resident',
     'exc-ch-resident-data', 'pg_ch',
     '["CH"]'::jsonb,
     TRUE, 'ADEQUACY',
     'Swiss residents — adequacy decision in force'),
    ('SG', 'ap-sg',
     'arn:aws:kms:ap-southeast-1:000000000000:key/exc-sg-resident',
     'exc-sg-resident-data', 'pg_ap_sg',
     '["SG"]'::jsonb,
     FALSE, 'SCC',
     'Singapore residents — SCCs required for EU-outbound transfer'),
    ('US', 'us-east',
     'arn:aws:kms:us-east-1:000000000000:key/exc-us-resident',
     'exc-us-resident-data', 'pg_us_east',
     '["US"]'::jsonb,
     FALSE, 'SCC',
     'US persons (institutional only per §27 R15) — SCC instrument'),
    ('ROW', 'eu-central',
     'arn:aws:kms:eu-central-1:000000000000:key/exc-eu-resident',
     'exc-eu-resident-data', 'pg_eu_central',
     '[]'::jsonb,
     FALSE, 'NONE',
     'rest-of-world default — EU region, no outbound transfer')
ON CONFLICT (jurisdiction_code) DO NOTHING;

CREATE TABLE data_residency_access_log (
    id                BIGSERIAL   PRIMARY KEY,
    admin_user_id     BIGINT      NOT NULL,
    admin_regions     JSONB       NOT NULL DEFAULT '[]', -- admin's granted regions at access time
    account_id        BIGINT,
    jurisdiction_code VARCHAR(8)  NOT NULL,
    home_region       VARCHAR(32) NOT NULL,
    action            VARCHAR(48) NOT NULL,  -- QUERY | EXPORT | PIN | REPLICATE | TRANSFER_CHECK
    cross_border      BOOLEAN     NOT NULL,
    justification     TEXT,                  -- mandatory when cross_border (service enforces)
    masked            BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_residency_access_admin
    ON data_residency_access_log (admin_user_id, created_at DESC);
CREATE INDEX ix_residency_access_account
    ON data_residency_access_log (account_id, created_at DESC);

-- Append-only — access evidence is never rewritten.
CREATE OR REPLACE FUNCTION data_residency_access_guard() RETURNS trigger AS $guard$
BEGIN
    RAISE EXCEPTION 'data_residency_access_log is append-only'
        USING ERRCODE = 'raise_exception';
END;
$guard$ LANGUAGE plpgsql;

CREATE TRIGGER trg_residency_access_guard
    BEFORE UPDATE OR DELETE ON data_residency_access_log
    FOR EACH ROW EXECUTE FUNCTION data_residency_access_guard();

-- Residency tag on the participant profile; onboarding pins it, the
-- resolver reads it before falling back to declared country.
ALTER TABLE accounts
    ADD COLUMN jurisdiction_code VARCHAR(8);
CREATE INDEX ix_accounts_jurisdiction
    ON accounts (jurisdiction_code) WHERE jurisdiction_code IS NOT NULL;

COMMIT;
