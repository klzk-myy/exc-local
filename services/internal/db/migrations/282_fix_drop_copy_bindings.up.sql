-- 282_fix_drop_copy_bindings.up.sql
-- Phase-3 Task 3 (IMP-PLAN) — production wiring for the drop-copy and
-- mTLS seams that shipped as tested-but-unbound library code.
--
-- fix_session_bindings — the DropCopyBindingStore backing table: which
--   accounts a drop-copy session (fix_sessions.account_id IS NULL per
--   spec §5.20) may copy. A session with ZERO rows copies nothing —
--   the router fails closed rather than leak the tape.
--
-- fix_cert_revocations — the RevocationChecker backing table consulted
--   by tls.go VerifyPeer when a listener runs fix.mtls_required. A
--   presented certificate whose fingerprint appears here is rejected at
--   the TLS handshake, before any FIX byte is read (spec §2.7).

BEGIN;

CREATE TABLE fix_session_bindings (
    session_id  VARCHAR(64) NOT NULL REFERENCES fix_sessions (session_id)
        ON DELETE CASCADE,
    account_id  BIGINT      NOT NULL REFERENCES accounts (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, account_id)
);

CREATE INDEX fix_session_bindings_account_ix
    ON fix_session_bindings (account_id);

CREATE TABLE fix_cert_revocations (
    fingerprint CHAR(64)    PRIMARY KEY,               -- SHA-256 hex of the DER
    reason      VARCHAR(255) NOT NULL,
    revoked_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

COMMIT;
