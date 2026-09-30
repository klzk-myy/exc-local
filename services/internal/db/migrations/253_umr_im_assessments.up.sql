-- 253_umr_im_assessments.up.sql
-- Phase-22 Task 22.3.11 (spec §15.5, §13.11, §24 #146): daily ISDA
-- SIMM-consistent initial-margin assessment for umr_in_scope accounts.
--
-- One row per (account, assessment_date): the recomputation is
-- idempotent by unique key — a same-day rerun upserts the amounts rather
-- than stacking rows. deficit_usd = max(0, required − posted) after the
-- MTA/threshold regime is applied; status tracks the call lifecycle
-- (CALL_ISSUED → SATISFIED, DISPUTED within the §15.7 dispute window,
-- ESCALATED when the window lapses unpaid).
--
-- Component columns keep the SIMM decomposition (delta/vega/curvature) for
-- audit; posted_im_usd is the haircut-adjusted segregated collateral the
-- collateral schedule (Phase-19 Task 19.3.8) reports for this account.

BEGIN;

CREATE TYPE umr_im_status_enum AS ENUM
    ('IN_TOLERANCE', 'CALL_ISSUED', 'DISPUTED', 'SATISFIED', 'ESCALATED');

CREATE TABLE umr_im_assessments (
    id                   BIGSERIAL PRIMARY KEY,
    account_id           BIGINT      NOT NULL REFERENCES accounts (id),
    assessment_date      DATE        NOT NULL,
    required_im_usd      DECIMAL(28,8) NOT NULL,
    delta_margin_usd     DECIMAL(28,8) NOT NULL,
    vega_margin_usd      DECIMAL(28,8) NOT NULL,
    curvature_margin_usd DECIMAL(28,8) NOT NULL,
    posted_im_usd        DECIMAL(28,8) NOT NULL DEFAULT 0,
    deficit_usd          DECIMAL(28,8) NOT NULL DEFAULT 0,
    mta_usd              DECIMAL(28,8) NOT NULL DEFAULT 0, -- min transfer amount applied
    threshold_usd        DECIMAL(28,8) NOT NULL DEFAULT 0, -- UMR exchange threshold
    status               umr_im_status_enum NOT NULL,
    call_issued_at       TIMESTAMPTZ,
    dispute_deadline     TIMESTAMPTZ,                      -- §15.7 dispute window
    satisfied_at         TIMESTAMPTZ,
    computed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, assessment_date)
);

CREATE INDEX umr_im_assessments_open_ix
    ON umr_im_assessments (account_id)
    WHERE status IN ('CALL_ISSUED', 'DISPUTED', 'ESCALATED');

COMMIT;
