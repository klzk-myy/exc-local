-- 100_execution_policies.up.sql
-- Phase-21 Task 21.3.28 — Order Execution Policy Publication, Consent &
-- Annual Review (spec §5.42.2/§14.13; §24 #377; remediation #30).
--
--   execution_policies         — versioned venue execution policy:
--                                DRAFT → ACTIVE → SUPERSEDED. Exactly
--                                one ACTIVE row (partial unique index)
--                                — the public surface
--                                GET /api/v1/execution-policy serves
--                                it. review_due_at is the annual
--                                review deadline (≤12 months after
--                                effective_from); the overdue sweep
--                                raises a Compliance P1 and freezes
--                                upgrades while the ACTIVE version
--                                stays enforceable. material_change
--                                marks amendments that force
--                                re-consent. Backdated corrections
--                                always land as a NEW version — an
--                                ACTIVE row is never mutated
--                                (trigger).
--   execution_policy_consents  — spec §5.42 consent ledger:
--                                (account, policy version, consented_at)
--                                written alongside the generic
--                                account_consents gate row (migration
--                                239) in the same transaction.
--
-- Retention (§19.12): client-facing disclosures — no purge below the
-- 7-year compliance horizon.

BEGIN;

CREATE TABLE execution_policies (
    id              BIGSERIAL    PRIMARY KEY,
    version         VARCHAR(32)  NOT NULL UNIQUE,       -- e.g. 'v2026.1'
    body_ref        VARCHAR(255) NOT NULL,              -- content-store document ref
    status          VARCHAR(16)  NOT NULL DEFAULT 'DRAFT'
                    CHECK (status IN ('DRAFT','ACTIVE','SUPERSEDED')),
    effective_from  TIMESTAMPTZ,
    review_due_at   TIMESTAMPTZ,                        -- annual review deadline (≤12 months)
    material_change BOOLEAN      NOT NULL DEFAULT FALSE, -- amendment requires re-consent
    approver        BIGINT,                             -- CCO user id (Task 21.3.15)
    approved_at     TIMESTAMPTZ,
    evidence_pack   JSONB        NOT NULL DEFAULT '{}', -- annual review evidence snapshot
    review_notes    TEXT,
    reviewed_by     BIGINT,
    reviewed_at     TIMESTAMPTZ,
    created_by      BIGINT       NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK (status = 'DRAFT' OR effective_from IS NOT NULL),
    CHECK (status <> 'ACTIVE' OR review_due_at IS NOT NULL)
);

-- Exactly one publicly-served ACTIVE version at any time.
CREATE UNIQUE INDEX uq_execution_policies_active
    ON execution_policies ((status)) WHERE status = 'ACTIVE';
CREATE INDEX ix_execution_policies_review
    ON execution_policies (review_due_at) WHERE status = 'ACTIVE';

-- ACTIVE rows are immutable except for the review stamp and the
-- SUPERSEDED transition — backdated corrections always land as a new
-- DRAFT version (spec §14.13 edge case: never mutate ACTIVE).
CREATE OR REPLACE FUNCTION execution_policies_active_immutable() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'ACTIVE' AND NEW.status = 'ACTIVE' THEN
        IF NEW.version IS DISTINCT FROM OLD.version
           OR NEW.body_ref IS DISTINCT FROM OLD.body_ref
           OR NEW.effective_from IS DISTINCT FROM OLD.effective_from
           OR NEW.material_change IS DISTINCT FROM OLD.material_change THEN
            RAISE EXCEPTION 'execution_policies: ACTIVE policy fields are immutable — create a new version'
                USING ERRCODE = 'raise_exception';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' AND OLD.status = 'ACTIVE' THEN
        RAISE EXCEPTION 'execution_policies: the ACTIVE policy cannot be deleted'
            USING ERRCODE = 'raise_exception';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER trg_execution_policies_active
    BEFORE UPDATE OR DELETE ON execution_policies
    FOR EACH ROW EXECUTE FUNCTION execution_policies_active_immutable();

CREATE TABLE execution_policy_consents (
    id           BIGSERIAL    PRIMARY KEY,
    account_id   BIGINT       NOT NULL REFERENCES accounts (id),
    policy_id    BIGINT       NOT NULL REFERENCES execution_policies (id),
    version      VARCHAR(32)  NOT NULL,
    consented_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    consented_by BIGINT,
    ip           VARCHAR(45),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (account_id, policy_id)
);

CREATE INDEX ix_exec_policy_consents_account
    ON execution_policy_consents (account_id, consented_at DESC);

COMMIT;
