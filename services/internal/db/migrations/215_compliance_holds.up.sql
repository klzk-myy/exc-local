-- Migration 215 — Compliance hold workflow records
-- (Phase-14 Task 14.3.10).
--
-- One row per compliance hold. PlaceHold is the stable seam Phase-21
-- sanctions/PEP callers use; trigger_source enumerates manual officer
-- placement now and machine triggers later. The SLA deadline is
-- stamped at placement (4h for high-confidence sanctions hits, 24h
-- otherwise) and a breach flag + alert feed the officer dashboard.

BEGIN;

CREATE TABLE compliance_holds (
    id             BIGSERIAL    PRIMARY KEY,
    hold_id        VARCHAR(40)  NOT NULL UNIQUE,  -- "chold_<24hex>" public/audit id
    account_id     BIGINT       NOT NULL REFERENCES accounts(id),
    trigger_source VARCHAR(24)  NOT NULL CHECK (trigger_source IN (
                        'MANUAL', 'SANCTIONS_HIT', 'PEP_MATCH', 'UNUSUAL_ACTIVITY')),
    reason         TEXT         NOT NULL,
    evidence_ref   TEXT,                          -- screening hit / document / case reference
    status         VARCHAR(24)  NOT NULL DEFAULT 'OPEN' CHECK (status IN (
                        'OPEN', 'RELEASED', 'ESCALATED_SAR', 'ESCALATED_CLOSURE')),
    sla_deadline   TIMESTAMPTZ  NOT NULL,
    sla_breached   BOOLEAN      NOT NULL DEFAULT false,
    placed_by      BIGINT       NOT NULL,          -- officer user id (machine triggers use owner id, source marks it)
    placed_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    resolved_at    TIMESTAMPTZ,
    resolved_by    BIGINT,
    resolution     TEXT,                           -- disposition note (release reason / escalation detail)
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_compliance_holds_account ON compliance_holds (account_id);
CREATE INDEX idx_compliance_holds_status  ON compliance_holds (status);
CREATE INDEX idx_compliance_holds_sla     ON compliance_holds (sla_deadline)
    WHERE status = 'OPEN';

COMMENT ON TABLE compliance_holds IS
    'Phase-14 Task 14.3.10 — compliance holds. OPEN rows keep the '
    'account FROZEN (orders cancelled on placement, withdrawals/new '
    'orders blocked via status, positions preserved). Dispositions: '
    'RELEASED (restore ACTIVE), ESCALATED_SAR (Phase-21 Task 21.3.3 '
    'files the actual SAR — this row records the escalation only), '
    'ESCALATED_CLOSURE (hands off to the Task 14.3.9 forced-closure '
    'dual-control request). SLA: 4h high-confidence sanctions, else 24h.';

COMMIT;
