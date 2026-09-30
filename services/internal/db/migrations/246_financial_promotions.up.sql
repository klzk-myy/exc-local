-- 246_financial_promotions.up.sql
-- Phase-21 Task 21.3.26 — financial-promotions approval register
-- (spec §14.14 / §23 PROMOTION_NOT_APPROVED; MiFID II fair-clear-not-
-- misleading + jurisdiction promotion rules).
--
--   financial_promotions         — HEAD row per promotion version. The
--                                  column set is the shape the Phase-20
--                                  marketing-ops report consumes
--                                  (promotion_id, channel, body_ref,
--                                  version, approval_status, approver,
--                                  created_at, approved_at,
--                                  approved_until). approval_status ∈
--                                  DRAFT|PENDING_REVIEW|APPROVED|
--                                  REJECTED|EXPIRED|WITHDRAWN. The
--                                  render gate serves only APPROVED and
--                                  unexpired; the 12-month approval
--                                  bound and the claims dual-control
--                                  rule are service-enforced
--                                  (timestamptz+interval is non-
--                                  immutable so a CHECK cannot encode
--                                  the 12-month ceiling).
--
--   financial_promotion_versions — immutable transition ledger: every
--                                  state change appends a row keyed by
--                                  (promotion_id, version, decision),
--                                  preserving the full audit history of
--                                  drafts, approvals, rejections,
--                                  expiries and withdrawals.
--
-- Consent interplay: outbound delivery additionally requires a GRANTED
-- MARKETING consent in account_consent_states (Task 21.3.7, migration
-- 243); that check lives in the delivery path, not this table.
--
-- Affiliate/influencer channels are deliberately absent (rulings
-- R4/R10 — referrals out of scope).

BEGIN;

CREATE TABLE financial_promotions (
    promotion_id    BIGSERIAL   PRIMARY KEY,
    slug            VARCHAR(128) NOT NULL,                 -- stable external key (per campaign)
    channel         VARCHAR(32)  NOT NULL CHECK (channel IN (
        'LANDING',   -- web landing page
        'AD',        -- paid advertisement
        'EMAIL',     -- outbound email marketing
        'PUSH',      -- push notification
        'SOCIAL',    -- social-media post
        'IN_APP')),  -- in-app banner/card
    body_ref        VARCHAR(512) NOT NULL,                 -- content object reference (versioned)
    title           VARCHAR(256) NOT NULL DEFAULT '',
    version         INTEGER      NOT NULL DEFAULT 1 CHECK (version > 0),
    approval_status VARCHAR(16)  NOT NULL DEFAULT 'DRAFT' CHECK (approval_status IN (
        'DRAFT', 'PENDING_REVIEW', 'APPROVED', 'REJECTED', 'EXPIRED', 'WITHDRAWN')),
    contains_claim  BOOLEAN      NOT NULL DEFAULT FALSE,   -- pricing/performance/return claim → dual control
    checklist       JSONB        NOT NULL DEFAULT '{}',    -- approval checklist evidence
    submitted_by    BIGINT       NOT NULL,
    approver        BIGINT,
    second_approver BIGINT,                                -- mandatory when contains_claim
    approved_at     TIMESTAMPTZ,
    approved_until  TIMESTAMPTZ,                           -- ≤ approved_at + 12 months (service-enforced)
    rejected_by     BIGINT,
    rejected_at     TIMESTAMPTZ,
    rejection_reason TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (slug, version)
);

CREATE INDEX ix_financial_promotions_status
    ON financial_promotions (approval_status, approved_until);
CREATE INDEX ix_financial_promotions_slug
    ON financial_promotions (slug, version DESC);

CREATE TABLE financial_promotion_versions (
    id            BIGSERIAL   PRIMARY KEY,
    promotion_id  BIGINT      NOT NULL REFERENCES financial_promotions (promotion_id),
    version       INTEGER     NOT NULL,
    channel       VARCHAR(32) NOT NULL,
    body_ref      VARCHAR(512) NOT NULL,
    title         VARCHAR(256) NOT NULL DEFAULT '',
    contains_claim BOOLEAN    NOT NULL DEFAULT FALSE,
    decision      VARCHAR(16) NOT NULL CHECK (decision IN (
        'CREATED', 'SUBMITTED', 'APPROVED', 'REJECTED', 'EXPIRED',
        'WITHDRAWN', 'REVISION')),
    actor         BIGINT      NOT NULL,
    checklist     JSONB       NOT NULL DEFAULT '{}',
    note          TEXT        NOT NULL DEFAULT '',
    decided_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (promotion_id, version, decision)
);

CREATE INDEX ix_fp_versions_promo
    ON financial_promotion_versions (promotion_id, version DESC, decided_at DESC);

-- Version ledger is append-only.
CREATE OR REPLACE FUNCTION financial_promotion_versions_guard() RETURNS trigger AS $guard$
BEGIN
    RAISE EXCEPTION 'financial_promotion_versions is append-only'
        USING ERRCODE = 'raise_exception';
END;
$guard$ LANGUAGE plpgsql;

CREATE TRIGGER trg_fp_versions_guard
    BEFORE UPDATE OR DELETE ON financial_promotion_versions
    FOR EACH ROW EXECUTE FUNCTION financial_promotion_versions_guard();

COMMIT;
