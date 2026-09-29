-- 052_fix_certification.up.sql
-- Phase-18 Task 18.3.11 — FIXS mutual TLS certificate identity and the
-- versioned production client-certification register (spec §9.7,
-- §24 #167). Requires 030_create_fix_sessions (sibling) and the 046
-- entitlement columns.
--
-- fix_certifications — one row per certification run: which participant
--   (SenderCompID), which client build, against which protocol
--   dictionary + venue schema version, in which environment, with which
--   certification-pack version, the verdict, a SHA-256 evidence hash of
--   the run transcript, and its validity window. A production
--   order-entry session may only open while an ACTIVE PASS row covers
--   the exact (participant, client_build, dictionary, schema,
--   environment) tuple — uncertified or stale-certified builds fail
--   closed (spec §9.7 item 4).
--
-- fix_sessions cert columns — the mTLS transport binding (tls.go):
--   cert_fingerprint          SHA-256 hex of the client certificate DER
--                             bound to this session (NULL = mTLS not
--                             enforced for the row — drop-copy or
--                             pre-production sessions only; order-entry
--                             sessions are provisioned with it).
--   cert_cn                   expected Subject CN / SAN token (audit).
--   cert_rollover_fingerprint second accepted fingerprint during a
--                             rotation window (dual-cert rollover).
--   cert_rotation_ends_at     rollover window end; after it only the
--                             primary fingerprint verifies.
--   cert_expires_at           cached notAfter of the bound certificate
--                             (operational alerting; the handshake
--                             still checks the presented cert itself).
--   environment               deployment label the binding is valid
--                             for ('production'|'staging'|'sandbox'|
--                             'development') — a staging cert can never
--                             open a production session.
--   client_build              last Logon'd client build label — joined
--                             against fix_certifications at gate time.

BEGIN;

CREATE TABLE fix_certifications (
    id                   BIGSERIAL PRIMARY KEY,
    participant_id       VARCHAR(64)  NOT NULL,   -- SenderCompID / firm id
    client_build         VARCHAR(64)  NOT NULL,
    dictionary_version   VARCHAR(32)  NOT NULL,   -- e.g. 'FIX.4.4' / 'FIX.5.0SP2'
    venue_schema_version VARCHAR(32)  NOT NULL,   -- venue message-set version
    environment          VARCHAR(16)  NOT NULL
        CHECK (environment IN ('production','staging','sandbox','development')),
    pack_version         VARCHAR(16)  NOT NULL,
    result               VARCHAR(8)   NOT NULL
        CHECK (result IN ('PASS','FAIL')),
    evidence_hash        CHAR(64)     NOT NULL,   -- sha256(transcript)
    certified_at         TIMESTAMPTZ  NOT NULL,
    expires_at           TIMESTAMPTZ  NOT NULL,
    status               VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE','REVOKED','EXPIRED','SUPERSEDED')),
    revoked_reason       VARCHAR(255),
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT fix_certifications_window_chk
        CHECK (expires_at > certified_at)
);

-- The production-enablement lookup: latest ACTIVE PASS per
-- (participant, build, dictionary, schema, env).
CREATE INDEX fix_certifications_lookup_ix
    ON fix_certifications (participant_id, client_build,
                           dictionary_version, venue_schema_version,
                           environment, status, expires_at DESC);

ALTER TABLE fix_sessions
    ADD COLUMN cert_fingerprint          CHAR(64),
    ADD COLUMN cert_cn                   VARCHAR(128),
    ADD COLUMN cert_rollover_fingerprint CHAR(64),
    ADD COLUMN cert_rotation_ends_at     TIMESTAMPTZ,
    ADD COLUMN cert_expires_at           TIMESTAMPTZ,
    ADD COLUMN environment               VARCHAR(16) NOT NULL DEFAULT 'production'
        CHECK (environment IN ('production','staging','sandbox','development')),
    ADD COLUMN client_build              VARCHAR(64);

-- Rollover without a window bound is a permanent second credential —
-- forbid it: a rollover fingerprint is only valid while its window is
-- set (the handshakes also re-check the window at verify time).
ALTER TABLE fix_sessions
    ADD CONSTRAINT fix_sessions_rollover_window_chk
        CHECK (cert_rollover_fingerprint IS NULL
               OR cert_rotation_ends_at IS NOT NULL);

-- Fingerprint lookups on the TLS handshake path.
CREATE INDEX fix_sessions_cert_fingerprint_ix
    ON fix_sessions (cert_fingerprint) WHERE cert_fingerprint IS NOT NULL;
CREATE INDEX fix_sessions_cert_rollover_ix
    ON fix_sessions (cert_rollover_fingerprint)
    WHERE cert_rollover_fingerprint IS NOT NULL;

COMMIT;
