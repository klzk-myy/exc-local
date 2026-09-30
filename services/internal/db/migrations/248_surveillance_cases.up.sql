-- 248_surveillance_cases.up.sql
-- Phase-21 Task 21.3.21 — Surveillance Case Management (spec §14.9.2).
--
-- surveillance_cases: the investigation spine — auto-opened from
-- Phase-17 surveillance_signals (source_ref 'signal:{id}' dedup) and
-- from compliance.MonitoringService findings (monitoring.CaseSink).
--   severity: URGENT (high-confidence / z≥3σ) → assign SLA 4h,
--             REVIEW                            → 24h.
--   status:   OPEN → ASSIGNED → INVESTIGATING →
--             CLOSED_FALSE_POSITIVE | ESCALATED_SAR | ESCALATED_STR |
--             ESCALATED_ACTION.
--
-- surveillance_case_evidence: immutable, hash-chained attachments —
-- notes, documents, communications recordings, STOR filings.
-- sha256 + attachment_ref let reviewers verify blobs stored in the
-- Phase-20 document backend without embedding opaque payloads.
-- The immutability trigger makes evidence append-only.

BEGIN;

CREATE TABLE surveillance_cases (
    id               BIGSERIAL    PRIMARY KEY,
    case_ref         VARCHAR(40)  NOT NULL UNIQUE,       -- "case_<26urlsafe>"
    source_ref       VARCHAR(160) NOT NULL UNIQUE,       -- 'signal:{id}' | 'monitoring:{rule}:{acct}:{bucket}'
    signal_id        BIGINT       REFERENCES surveillance_signals (id),
    account_id       BIGINT       REFERENCES accounts (id),
    account_hash     BIGINT       NOT NULL DEFAULT 0,
    signal_type      VARCHAR(32)  NOT NULL DEFAULT '',
    symbol           VARCHAR(32)  NOT NULL DEFAULT '',
    severity         VARCHAR(8)   NOT NULL CHECK (severity IN ('URGENT','REVIEW')),
    status           VARCHAR(24)  NOT NULL DEFAULT 'OPEN' CHECK (status IN (
                          'OPEN','ASSIGNED','INVESTIGATING',
                          'CLOSED_FALSE_POSITIVE','ESCALATED_SAR',
                          'ESCALATED_STR','ESCALATED_ACTION')),
    assigned_to      BIGINT,
    assigned_at      TIMESTAMPTZ,
    first_reviewed_at TIMESTAMPTZ,
    sla_deadline     TIMESTAMPTZ  NOT NULL,              -- assignment SLA (4h URGENT / 24h REVIEW)
    sla_breached     BOOLEAN      NOT NULL DEFAULT false,
    sla_note         TEXT         NOT NULL DEFAULT '',   -- SLA breach + disposition times recorded for the auditor report
    escalated_sar_id BIGINT       REFERENCES sar_reports (id),
    escalation_ref   TEXT         NOT NULL DEFAULT '',   -- STR filing ref / enforcement action id
    disposition_reason TEXT       NOT NULL DEFAULT '',
    evidence         JSONB        NOT NULL DEFAULT '{}', -- Phase-17 evidence blob copy + findings
    opened_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    closed_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_surveillance_cases_open
    ON surveillance_cases (severity, sla_deadline)
    WHERE status IN ('OPEN','ASSIGNED','INVESTIGATING');
CREATE INDEX idx_surveillance_cases_officer
    ON surveillance_cases (assigned_to, status);
CREATE INDEX idx_surveillance_cases_signal
    ON surveillance_cases (signal_id) WHERE signal_id IS NOT NULL;

COMMENT ON TABLE surveillance_cases IS
    'Phase-21 Task 21.3.21 — surveillance investigation spine. '
    'source_ref is the idempotency key; SLA breach flips sla_breached '
    'and fires a P2 alert to the AML Officer (SweepSLA). Dispositions '
    'are terminal; ESCALATE_SAR pre-populates a SAR draft from the case.';

CREATE TABLE surveillance_case_evidence (
    id             BIGSERIAL    PRIMARY KEY,
    case_id        BIGINT       NOT NULL REFERENCES surveillance_cases (id) ON DELETE CASCADE,
    kind           VARCHAR(16)  NOT NULL CHECK (kind IN (
                        'NOTE','ATTACHMENT','COMMS_RECORDING','DOCUMENT','STOR')),
    body           TEXT         NOT NULL DEFAULT '',     -- NOTE text / structured description
    attachment_ref VARCHAR(255) NOT NULL DEFAULT '',     -- document backend ref for binary payloads
    sha256         VARCHAR(64)  NOT NULL DEFAULT '',     -- payload digest — tamper check
    added_by       BIGINT       NOT NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_surveillance_case_evidence_case
    ON surveillance_case_evidence (case_id, id);

COMMENT ON TABLE surveillance_case_evidence IS
    'Phase-21 Task 21.3.21 — case workspace attachments. Append-only '
    '(immutability trigger): notes, comms-recording refs, external '
    'documents, STOR filings; every row is hash-chained by '
    'audit.AppendAuto at insert time. Retained ≥5y with the case.';

-- Case evidence is append-only (investigation integrity — spec §14.9.2).
CREATE OR REPLACE FUNCTION p21_case_evidence_immutable() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'surveillance_case_evidence is append-only';
END $$;

CREATE TRIGGER case_evidence_immutable_trg
    BEFORE UPDATE OR DELETE ON surveillance_case_evidence
    FOR EACH ROW EXECUTE FUNCTION p21_case_evidence_immutable();

COMMIT;
