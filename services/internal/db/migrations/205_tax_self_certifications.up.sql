-- 205_tax_self_certifications.up.sql
-- Phase-12 Task 12.3.13 — tax self-certification intake (spec §12.7).
--
-- Onboarding collects IRS self-certifications (W-8BEN / W-8BEN-E / W-9)
-- at KYC tier T1+ with TIN format validation (US TINs: SSN/EIN/ITIN;
-- non-US TINs pass through unvalidated — jurisdiction-specific format
-- rules are a Phase-21 concern). Phase-21 Task 21.3.22 CRS/FATCA
-- reporting reads this table directly — no joins or re-shaping.
--
-- Book of record: FIFO is the lot method for 1099-B reporting (Phase-05
-- Task 5.3.19 tax service). LIFO/HIFO/AVG_COST are planning projections,
-- never the filed record. IRC §871(m) withholding: N/A — spot FX only.

BEGIN;

CREATE TABLE tax_self_certifications (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT      NOT NULL REFERENCES accounts (id),
    form_type        VARCHAR(10) NOT NULL
                     CHECK (form_type IN ('W-8BEN','W-8BEN-E','W-9')),
    tin              VARCHAR(32),                       -- raw TIN as submitted
    tin_country      CHAR(2),                           -- ISO alpha-2 issuing country ('US' validated)
    tin_kind         VARCHAR(8),                        -- SSN|EIN|ITIN — set only when US-validated
    fields           JSONB       NOT NULL DEFAULT '{}'::jsonb, -- legal name, address, entity type, treaty claims
    status           VARCHAR(16) NOT NULL DEFAULT 'SUBMITTED'
                     CHECK (status IN ('SUBMITTED','VALIDATED','REJECTED')),
    tin_validated_at TIMESTAMPTZ,                       -- format check passed
    validated_at     TIMESTAMPTZ,                       -- record accepted (Phase-14/21 review seam)
    superseded_by    BIGINT REFERENCES tax_self_certifications (id), -- renewal chain
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX tax_self_certs_account_idx
    ON tax_self_certifications (account_id, form_type, id DESC);

COMMIT;
