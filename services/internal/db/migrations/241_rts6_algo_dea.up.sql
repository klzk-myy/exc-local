-- 241_rts6_algo_dea.up.sql
-- Phase-21 Task 21.3.12 — MiFID II RTS 6 algorithm certification, DEA
-- controls & order retention (spec §14.9.1, RTS 6 Art. 9–17).
--
-- algo_certifications: per-(account, algo) certification register —
--   Art. 9(2) requires tested deployment evidence + kill-functionality
--   test + capacity self-assessment BEFORE an algo submits orders.
--   The orders admission seam rejects unregistered algo_id flow with
--   ALGO_NOT_CERTIFIED (422). Expiry drives annual re-certification.
--
-- dea_session_controls: Direct Electronic Access session limits —
--   Art. 15: sponsored-access clients get hard pre-trade limits
--   (max_order_qty, msg/sec), sponsorship desk attribution and a
--   drop-copy feed for independent sponsor monitoring.
--
-- rts6_self_assessments: annual self-assessment & validation register
--   (Art. 9 — annual review; the sweep warns 60d before due_at).

BEGIN;

CREATE TABLE algo_certifications (
    id            BIGSERIAL    PRIMARY KEY,
    algo_id       VARCHAR(64)  NOT NULL,               -- client algo strategy identifier (from algo_params.algo_id)
    account_id    BIGINT       NOT NULL REFERENCES accounts (id),
    status        VARCHAR(12)  NOT NULL DEFAULT 'PENDING' CHECK (status IN (
                       'PENDING','CERTIFIED','SUSPENDED','EXPIRED','REVOKED')),
    test_evidence_ref VARCHAR(255) NOT NULL,           -- conformance-testnet evidence ref (mandatory)
    kill_button_tested BOOLEAN   NOT NULL DEFAULT false,
    capacity_assessment_ref VARCHAR(255) NOT NULL,     -- Art. 9 capacity self-assessment ref (mandatory)
    certified_by  BIGINT,                              -- officer who certified (Certify)
    certified_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,                         -- certification expiry → annual re-cert
    review_due_at TIMESTAMPTZ,                         -- next periodic review
    created_by    BIGINT       NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (account_id, algo_id)
);

CREATE INDEX idx_algo_certifications_algo ON algo_certifications (algo_id, status);
CREATE INDEX idx_algo_certifications_due  ON algo_certifications (expires_at)
    WHERE status = 'CERTIFIED';

COMMENT ON TABLE algo_certifications IS
    'Phase-21 Task 21.3.12 — RTS 6 Art. 9 algorithm certification '
    'register. Orders carrying algo_params.algo_id admit only while a '
    'CERTIFIED, unexpired row exists (orders.AlgoCertGate; '
    'ALGO_NOT_CERTIFIED 422 otherwise). Suspension is immediate '
    '(orders reject next admission), revocation is terminal.';

CREATE TABLE dea_session_controls (
    id             BIGSERIAL    PRIMARY KEY,
    session_id     VARCHAR(64)  NOT NULL UNIQUE,         -- FIX/SBE session id under DEA
    account_id     BIGINT       NOT NULL REFERENCES accounts (id),
    max_order_qty  DECIMAL(28,8) NOT NULL,
    max_msgs_per_sec INTEGER    NOT NULL CHECK (max_msgs_per_sec > 0),
    sponsoring_desk VARCHAR(64) NOT NULL,
    drop_copy_feed VARCHAR(64)  NOT NULL DEFAULT '',     -- sponsor's independent drop-copy session
    status         VARCHAR(12)  NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','SUSPENDED')),
    created_by     BIGINT       NOT NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

COMMENT ON TABLE dea_session_controls IS
    'Phase-21 Task 21.3.12 — RTS 6 Art. 15 DEA session controls. '
    'Session admission resolves limits via DEALimitsFor; breaching the '
    'session cap rejects the order through the normal gateway path; '
    'SUSPENDED denies all entry. Drop-copy feed reference binds the '
    'sponsor monitoring contract.';

CREATE TABLE rts6_self_assessments (
    id           BIGSERIAL    PRIMARY KEY,
    period_year  INTEGER      NOT NULL UNIQUE,
    document_ref VARCHAR(255) NOT NULL,                  -- assessment document/evidence ref
    status       VARCHAR(12)  NOT NULL DEFAULT 'DRAFT' CHECK (status IN (
                      'DRAFT','SUBMITTED','REVIEWED')),
    filed_by     BIGINT       NOT NULL,
    filed_at     TIMESTAMPTZ,
    reviewed_by  BIGINT,
    reviewed_at  TIMESTAMPTZ,
    review_note  TEXT         NOT NULL DEFAULT '',
    due_at       TIMESTAMPTZ   NOT NULL,                 -- annual deadline; sweep alerts 60d ahead
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

COMMENT ON TABLE rts6_self_assessments IS
    'Phase-21 Task 21.3.12 — RTS 6 annual self-assessment register. '
    'SweepAssessmentDue pages Compliance when a year approaches due_at '
    'unfiled (spec §14.9.1 audit evidence).';

COMMIT;
