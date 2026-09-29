-- 095_account_products_swapfree.up.sql
-- Phase-14 Tasks 14.3.13 (Account Product Profiles) + 14.3.15
-- (Swap-Free Verification Lifecycle) — spec §5.41, §12.8, §24 #369/#373.
--
--   1. account_product_profiles — admin-defined product profile:
--      pricing_plan is the single fee-model source consumed by the
--      Phase-03 Task 3.3.13 commission engine (PgProfileFeeModelSource);
--      instrument_scope is the allowlisted instrument-class set the
--      order gate enforces; subunit_divisor (1|100) is consumed by the
--      Task 3.3.21 minor-unit ledger convention (migration 096).
--   2. accounts.product_profile_id — NULL = default venue profile
--      (STANDARD), per spec §5.2. Assignment at onboarding resolves to
--      STANDARD through the same NULL→STANDARD read path; explicit
--      assignment goes through PUT /api/v1/admin/accounts/{id}/
--      product-profile with the open-exposure + zero-balance
--      (divisor change) preconditions.
--   3. accounts.swapfree_status — canonical column; migration 116
--      brought it forward additively (IF NOT EXISTS both ways).
--   4. swapfree_verifications — the request → attestation → Compliance
--      decision record; the mirrored accounts.swapfree_status drives
--      the Task 3.3.19/3.3.23 zero-Tom-Next rollover path.
--
-- Status vocabulary note: swapfree_verifications.status carries
-- PENDING|APPROVED|REJECTED|REVOKED — REJECTED extends the spec
-- §5.41.3 three-state list (PENDING|APPROVED|REVOKED) so a declined
-- request is a durable audit record rather than reusing REVOKED for a
-- row that was never approved. accounts.swapfree_status keeps the
-- canonical four-state set verbatim.

BEGIN;

CREATE TABLE account_product_profiles (
    profile_id        BIGSERIAL    PRIMARY KEY,
    code              VARCHAR(32)  NOT NULL UNIQUE,   -- STANDARD | CENT seeds; admin-defined
    pricing_plan      VARCHAR(24)  NOT NULL
        CHECK (pricing_plan IN ('SPREAD_MARKUP', 'RAW_SPREAD_COMMISSION')),
    instrument_scope  TEXT[]       NOT NULL
        CHECK (instrument_scope <@ ARRAY['SPOT','FORWARD','SWAP','NDF','OPTION']),
    subunit_divisor   INT          NOT NULL DEFAULT 1
        CHECK (subunit_divisor IN (1, 100)),
    min_deposit       DECIMAL(28,8) NOT NULL DEFAULT 0 CHECK (min_deposit >= 0),
    status            VARCHAR(8)   NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'RETIRED')),
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);

COMMENT ON TABLE account_product_profiles IS
    'Phase-14 Task 14.3.13 — product profiles (spec §5.41). '
    'pricing_plan is the single fee-model source for the Task 3.3.13 '
    'commission engine; instrument_scope gates order admission '
    '(PRODUCT_NOT_PERMITTED); subunit_divisor drives the Task 3.3.21 '
    'minor-unit ledger convention. RETIRED rows block new assignment '
    'but grandfather live holders.';

-- Seeds use explicit ids so accounts.product_profile_id has a stable
-- referent; the sequence is advanced past them.
INSERT INTO account_product_profiles
    (profile_id, code, pricing_plan, instrument_scope, subunit_divisor, min_deposit)
VALUES
    (1, 'STANDARD', 'SPREAD_MARKUP', '{SPOT,FORWARD,SWAP,NDF,OPTION}', 1, 0),
    (2, 'CENT',     'SPREAD_MARKUP', '{SPOT,FORWARD,SWAP,NDF,OPTION}', 100, 0)
ON CONFLICT (code) DO NOTHING;

SELECT setval('account_product_profiles_profile_id_seq',
              (SELECT max(profile_id) FROM account_product_profiles));

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS product_profile_id BIGINT
        REFERENCES account_product_profiles (profile_id);

-- Onboarding default: every new account lands on the STANDARD seed
-- (profile_id 1 — the INSERT above pins it) without a registration-path
-- change; NULL still resolves to STANDARD read-side for pre-095 rows.
ALTER TABLE accounts
    ALTER COLUMN product_profile_id SET DEFAULT 1;

-- Canonical ownership of swapfree_status lives here; migration 116's
-- bring-forward used the same VARCHAR+CHECK shape so this is a no-op
-- wherever 116 already ran.
ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS swapfree_status VARCHAR(16) NOT NULL DEFAULT 'STANDARD';
ALTER TABLE accounts
    DROP CONSTRAINT IF EXISTS chk_accounts_swapfree_status,
    ADD CONSTRAINT chk_accounts_swapfree_status
        CHECK (swapfree_status IN ('STANDARD', 'PENDING', 'VERIFIED', 'REVOKED'));

CREATE TABLE swapfree_verifications (
    id               BIGSERIAL    PRIMARY KEY,
    verification_ref VARCHAR(40)  NOT NULL UNIQUE,  -- "swv_<24hex>" public/audit id
    account_id       BIGINT       NOT NULL REFERENCES accounts (id),
    attestation_ref  VARCHAR(128) NOT NULL,          -- Phase-12 Task 12.3.4 document ref (kyc_documents)
    status           VARCHAR(8)   NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'REVOKED')),
    requested_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    decided_at       TIMESTAMPTZ,
    verifier         BIGINT,                         -- Compliance Officer users.id
    decision_note    TEXT,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- One live request per account: a second PENDING or a concurrent
-- APPROVED row can never coexist (the service maps the unique
-- violation to INVALID_REQUEST).
CREATE UNIQUE INDEX swapfree_verifications_live_ux
    ON swapfree_verifications (account_id)
    WHERE status IN ('PENDING', 'APPROVED');

CREATE INDEX swapfree_verifications_status_ix
    ON swapfree_verifications (status, requested_at);

-- Abuse-guard scan: VERIFIED→REVOKED transitions per trailing 12 months
-- read decided_at + status='REVOKED' for the account.
CREATE INDEX swapfree_verifications_revoked_ix
    ON swapfree_verifications (account_id, decided_at)
    WHERE status = 'REVOKED';

COMMENT ON TABLE swapfree_verifications IS
    'Phase-14 Task 14.3.15 — swap-free (Islamic) verification records '
    '(spec §12.8). attestation_ref resolves to a kyc_documents row owned '
    'by the account. Transitions mirror into accounts.swapfree_status: '
    'APPROVED→VERIFIED, REJECTED→STANDARD, REVOKED→REVOKED — '
    'prospective only, never retroactive; REVOKED resumes standard '
    'Tom-Next accrual with no back-billing. >2 REVOKED transitions per '
    '12 months auto-flag a compliance-hold review (Task 14.3.10).';

COMMIT;
