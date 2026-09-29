-- 099_product_target_markets.up.sql
-- Phase-14 Task 14.3.16 — Retail Product Target-Market Governance
-- (spec §14.12, §24 #376; MiFID II product-governance axis).
--
-- One row per (profile, client_category): the positive target is the
-- instrument-class set the category may OPEN on the profile
-- (positive_classes ⊆ profile scope is enforced service-side);
-- negative_classes is the machine-readable bar list the order gate
-- rejects outright for that category, and negative_target the
-- narrative description MiFID II requires (e.g. "retail clients with
-- no loss capacity for leveraged derivatives"). review_due_at ≤
-- approval + 12 months is enforced service-side (CHECK cannot express
-- the interval bound on timestamptz); an overdue row puts the profile
-- in close-only for that category's opens until re-approved.
--
-- The RETAIL seed rows mirror §24 #132's conservative mapping: spot FX
-- stays in the retail positive target; leveraged classes
-- (FORWARD/SWAP/NDF) require the appropriateness PASS the 14.3.7 gate
-- separately enforces; OPTION is a retail negative-target class until
-- Phase-22's vanilla/binary subtype split exists.

BEGIN;

CREATE TABLE product_target_markets (
    id                    BIGSERIAL    PRIMARY KEY,
    profile_id            BIGINT       NOT NULL
        REFERENCES account_product_profiles (profile_id),
    client_category       VARCHAR(24)  NOT NULL
        CHECK (client_category IN
               ('RETAIL', 'PROFESSIONAL', 'ELIGIBLE_COUNTERPARTY')),
    knowledge_experience  VARCHAR(16)  NOT NULL DEFAULT 'BASIC'
        CHECK (knowledge_experience IN ('NONE', 'BASIC', 'INTERMEDIATE', 'ADVANCED')),
    risk_tolerance        VARCHAR(8)   NOT NULL DEFAULT 'MEDIUM'
        CHECK (risk_tolerance IN ('LOW', 'MEDIUM', 'HIGH')),
    positive_classes      TEXT[]       NOT NULL
        CHECK (positive_classes <@ ARRAY['SPOT','FORWARD','SWAP','NDF','OPTION']),
    negative_classes      TEXT[]       NOT NULL DEFAULT '{}'
        CHECK (negative_classes <@ ARRAY['SPOT','FORWARD','SWAP','NDF','OPTION']),
    negative_target       TEXT         NOT NULL DEFAULT '',
    distribution_strategy VARCHAR(16)  NOT NULL DEFAULT 'NON_ADVISED'
        CHECK (distribution_strategy IN ('ADVISED', 'NON_ADVISED')),
    status                VARCHAR(16)  NOT NULL DEFAULT 'APPROVED'
        CHECK (status IN ('APPROVED', 'REVIEW_OVERDUE', 'SUSPENDED')),
    review_due_at         TIMESTAMPTZ  NOT NULL,  -- ≤ last_reviewed_at + 12mo (service-enforced)
    last_reviewed_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_reviewed_by      BIGINT,
    alerted_at            TIMESTAMPTZ,            -- once-only overdue alert stamp
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (profile_id, client_category)
);

-- Order-entry gate: profile + category resolution; overdue scan.
CREATE INDEX product_target_markets_profile_ix
    ON product_target_markets (profile_id, client_category);
CREATE INDEX product_target_markets_review_ix
    ON product_target_markets (review_due_at)
    WHERE status = 'APPROVED';

COMMENT ON TABLE product_target_markets IS
    'Phase-14 Task 14.3.16 — MiFID II target market per product profile '
    '× client category (spec §14.12). RETAIL opens outside '
    'positive_classes or inside negative_classes reject '
    'PRODUCT_NOT_PERMITTED; status REVIEW_OVERDUE (or review_due_at '
    'elapsed) makes the profile close-only for that category until '
    're-approval; SUSPENDED blocks the category outright. All writes '
    'are Compliance-Officer gated and admin_audit_log recorded.';

-- Seeds: RETAIL keeps spot + leveraged classes under the dual gate,
-- OPTION negative-targeted (§24 #132 conservative mapping — vanilla /
-- binary subtype lands with Phase-22); PROFESSIONAL and ECP recorded
-- for completeness with the full class set.
INSERT INTO product_target_markets
    (profile_id, client_category, knowledge_experience, risk_tolerance,
     positive_classes, negative_classes, negative_target,
     distribution_strategy, review_due_at)
SELECT p.profile_id, 'RETAIL', 'BASIC', 'MEDIUM',
       '{SPOT,FORWARD,SWAP,NDF}', '{OPTION}',
       'retail clients without loss capacity for leveraged derivatives; '
       'options barred until the Phase-22 subtype split lands',
       'NON_ADVISED', now() + interval '12 months'
  FROM account_product_profiles p WHERE p.code IN ('STANDARD', 'CENT')
ON CONFLICT (profile_id, client_category) DO NOTHING;

INSERT INTO product_target_markets
    (profile_id, client_category, knowledge_experience, risk_tolerance,
     positive_classes, negative_classes, negative_target,
     distribution_strategy, review_due_at)
SELECT p.profile_id, 'PROFESSIONAL', 'ADVANCED', 'HIGH',
       '{SPOT,FORWARD,SWAP,NDF,OPTION}', '{}', '', 'NON_ADVISED',
       now() + interval '12 months'
  FROM account_product_profiles p WHERE p.code IN ('STANDARD', 'CENT')
ON CONFLICT (profile_id, client_category) DO NOTHING;

INSERT INTO product_target_markets
    (profile_id, client_category, knowledge_experience, risk_tolerance,
     positive_classes, negative_classes, negative_target,
     distribution_strategy, review_due_at)
SELECT p.profile_id, 'ELIGIBLE_COUNTERPARTY', 'ADVANCED', 'HIGH',
       '{SPOT,FORWARD,SWAP,NDF,OPTION}', '{}', '', 'NON_ADVISED',
       now() + interval '12 months'
  FROM account_product_profiles p WHERE p.code IN ('STANDARD', 'CENT')
ON CONFLICT (profile_id, client_category) DO NOTHING;

COMMIT;
