-- 030_create_fix_sessions.up.sql
-- Phase-18 Task 18.3.1 — FIX session management (spec §5.20, §9.3,
-- §24 #54). QuickFIX-Go sequence state and the resend-replay archive.
--
-- fix_sessions   — spec §5.20 columns: per (BeginString:SenderCompID
--                  ->TargetCompID) pair, outgoing/incoming sequence
--                  numbers, lifecycle status, heartbeat timestamp.
--                  session_id is the canonical quickfixgo
--                  SessionID.String() form
--                  "FIX.4.4:<venueCompID>-><clientCompID>".
-- fix_messages   — persisted outbound FIX frames (seq_num → raw bytes)
--                  the QuickFIX MessageStore replays on ResendRequest
--                  with PossDupFlag(43)=Y / OrigSendingTime(122)
--                  (spec §9.3/§9.8). Not in §5.20's column list — the
--                  spec names only fix_sessions; the archive is the
--                  load-bearing half of "resend request: gap-fill with
--                  possible duplicate flag" and lives here because
--                  migration 030 owns the whole sequence-persistence
--                  surface.

BEGIN;

CREATE TYPE fix_session_status_enum AS ENUM
    ('ACTIVE', 'DISCONNECTED', 'LOGGED_OUT');

CREATE TABLE fix_sessions (
    id               BIGSERIAL PRIMARY KEY,
    session_id       VARCHAR(64)  NOT NULL UNIQUE,
    protocol_version VARCHAR(16)  NOT NULL DEFAULT 'FIX.4.4',
    sender_seq_num   BIGINT       NOT NULL DEFAULT 1,
    target_seq_num   BIGINT       NOT NULL DEFAULT 1,
    status           fix_session_status_enum NOT NULL DEFAULT 'DISCONNECTED',
    last_heartbeat_at TIMESTAMPTZ,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Resend replay archive (see header note). Rows are immutable; cleanup
-- is a retention job's business (Phase-09 Task 9.3.22 unified policy),
-- never inline deletes on the hot path.
CREATE TABLE fix_messages (
    session_id VARCHAR(64) NOT NULL
        REFERENCES fix_sessions (session_id) ON DELETE CASCADE,
    seq_num    BIGINT      NOT NULL,
    msg        BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, seq_num)
);

COMMIT;
