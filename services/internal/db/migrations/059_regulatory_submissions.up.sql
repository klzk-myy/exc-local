-- 059_regulatory_submissions.up.sql
-- Phase-21 Task 21.3.16 — MiFID II APA/ARM Submission Adapters
-- (spec §5.36, §14.5, §14.9 item 3; §24 #178).
--
--   regulatory_submissions      — transport-level submission ledger:
--                                 one row per outbound report pushed to a
--                                 vendor endpoint (APA/ARM/TR/SDR). This
--                                 is the audit-grade ledger spec §5.36
--                                 pins: regulation_type, destination,
--                                 batch, payload hash, ACK/NACK state,
--                                 error code, submitted/resolved stamps.
--                                 The artifact being transported lives
--                                 in regulatory_report_submissions
--                                 (migration 054) — report_submission_id
--                                 links transport to artifact.
--   regulatory_submission_log   — immutable wire log: every HTTP
--                                 request/response pair (body hashes only
--                                 — payloads stay in the artifact journal)
--                                 so the ACK/NACK audit trail survives
--                                 vendor endpoint loss.
--
-- Retry/repair columns implement §14.9 item 3 + §14.11 store-and-forward:
-- failed transports retry with bounded backoff (cap 2h post-recovery);
-- NACKs mark REJECTED and enter the Compliance Officer repair queue
-- (ack_status='REPAIRING' while a correction is in flight).
--
-- Retention: >= 5 years (MiFID II record-keeping). Task 9.3.22 enforcer
-- must not purge; archival-to-S3 only.

BEGIN;

CREATE TABLE regulatory_submissions (
    id                   BIGSERIAL PRIMARY KEY,
    report_submission_id BIGINT       REFERENCES regulatory_report_submissions (report_submission_id),
                                        -- artifact link (054); NULL only for
                                        -- vendor-side/system-level exchanges
    report_event_id      BIGINT       REFERENCES regulatory_report_events (event_id),
    trade_id             BIGINT,        -- logical ref → trades.id (partitioned; no FK)
    regulation_type      VARCHAR(24)  NOT NULL,   -- MIFID2_RTS22|MIFID2_RTS1|EMIR_REFIT|CFTC_P43|CFTC_P45
    destination_type     VARCHAR(8)   NOT NULL
                         CHECK (destination_type IN ('APA','ARM','TR','SDR')),
    destination_endpoint VARCHAR(256) NOT NULL,   -- env-configured vendor label
                                                 --   (EXC_APA_URL etc.; the label,
                                                 --   not credentials)
    batch_id             VARCHAR(64),               -- ARM T+1 batch grouping
    payload            JSONB          NOT NULL DEFAULT '{}'::jsonb,
    payload_xml        TEXT,                        -- ISO 20022 body when produced
    payload_hash       CHAR(64)       NOT NULL,     -- sha256 over the wire body
    schema_name        VARCHAR(64),                 -- auth.016.001.05 / RTS1-POST-TRADE …
    schema_version     VARCHAR(32),
    attempt            INTEGER        NOT NULL DEFAULT 1 CHECK (attempt >= 1),
    ack_status         VARCHAR(16)    NOT NULL DEFAULT 'PENDING'
                        CHECK (ack_status IN ('PENDING','SUBMITTED','ACK','NACK',
                                              'REPAIRING','RESOLVED','FAILED')),
    error_code         VARCHAR(64),                 -- vendor or validator code
    error_detail       VARCHAR(1024),
    external_ref       VARCHAR(128),                -- vendor receipt/report id
    repair_attempts    INTEGER        NOT NULL DEFAULT 0,
    next_attempt_at    TIMESTAMPTZ,                 -- store-and-forward backoff
    last_error_at      TIMESTAMPTZ,
    repaired_by        BIGINT,                      -- admin user id (repair queue)
    repaired_at        TIMESTAMPTZ,
    submitted_at       TIMESTAMPTZ,
    resolved_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ    NOT NULL DEFAULT now(),
    UNIQUE (report_submission_id, attempt)
);
CREATE INDEX regulatory_submissions_status_ix
    ON regulatory_submissions (ack_status, next_attempt_at);
CREATE INDEX regulatory_submissions_event_ix
    ON regulatory_submissions (report_event_id);
CREATE INDEX regulatory_submissions_trade_ix
    ON regulatory_submissions (trade_id) WHERE trade_id IS NOT NULL;
CREATE INDEX regulatory_submissions_batch_ix
    ON regulatory_submissions (batch_id) WHERE batch_id IS NOT NULL;
COMMENT ON TABLE regulatory_submissions IS
    'Task 21.3.16 APA/ARM/TR/SDR transport ledger (spec §5.36, §14.5): '
    'every outbound regulatory submission with payload hash, ACK/NACK '
    'state and repair state. NACK → ack_status=REJECTED → repair queue '
    'with resubmission-on-correction; transport outages hold rows PENDING '
    'under bounded backoff (<=2h post-recovery, §14.9/§14.11). RETENTION: '
    '>= 5 years.';

CREATE TABLE regulatory_submission_log (
    log_id         BIGSERIAL PRIMARY KEY,
    submission_id  BIGINT      NOT NULL
                   REFERENCES regulatory_submissions (id),
    direction      VARCHAR(4)  NOT NULL CHECK (direction IN ('OUT','IN')),
    http_status    INTEGER,
    body_hash      CHAR(64)    NOT NULL,      -- sha256 of the wire body
    body           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    endpoint       VARCHAR(256),
    at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX regulatory_submission_log_ix
    ON regulatory_submission_log (submission_id, at);
COMMENT ON TABLE regulatory_submission_log IS
    'Immutable wire log for Task 21.3.16 vendor exchanges (spec §14.5 '
    'tracking): request/response hashes + parsed response bodies per '
    'submission. Append-only — never UPDATE.';

COMMIT;
