-- 062_comms_recordings.up.sql
-- Phase-21 Task 21.3.20 — MiFID II Art. 16(7) / RTS 6 communications
-- recording ("taping") register + capture outbox (spec §14.12,
-- §19.12 compliance retention ≥5 years).
--
--   comms_recordings      — one row per recorded client-facing
--                           communication; the immutable register entry.
--                           Content bytes live in the WORM object store
--                           (ObjectLock COMPLIANCE mode); this row is the
--                           metadata + integrity anchor: sha256 of the
--                           stored object and a per-day hash chain
--                           (day_bucket partitions the chain so a
--                           tamper check walks one UTC day, not the
--                           whole table).
--
--   comms_capture_outbox  — transactional capture seam. DB triggers on
--                           the existing client-facing surfaces
--                           (notification_deliveries, support_tickets,
--                           ticket_notes) enqueue rows inside the same
--                           transaction as the source write, so a
--                           communication can never exist without its
--                           recording obligation. The recorder drains
--                           pending rows, writes the WORM object and
--                           inserts the register entry.
--
--   comms_recording_access— immutable retrieval/verification audit.
--                           Every Retrieval (dual-controlled per §14.12)
--                           appends a row; the table is append-only.
--
-- Retention: retention_until = ended_at + 5y minimum (MiFID II Art.
-- 16(7) five-year floor; competent-authority extension is modelled by
-- the service writing a later retention_until BEFORE first insert).
-- The row guard trigger refuses DELETE while retention_until is in the
-- future and refuses UPDATE outright — register rows are WORM like the
-- objects they describe. GDPR Art. 17(3)(b): recordings are exempt
-- from erasure (legal-obligation processing); the GDPR erasure path
-- (Task 21.3.7, migration 243) lists them under retained carve-outs.

BEGIN;

CREATE TABLE comms_recordings (
    recording_id     BIGSERIAL    PRIMARY KEY,
    account_id       BIGINT       REFERENCES accounts (id),
    -- nullable: user-level channels (notification_deliveries is keyed
    -- by user_id) resolve to the master account at drain time; a user
    -- with no account still tapes.
    user_id          BIGINT       REFERENCES users (id),
    channel          VARCHAR(24)  NOT NULL CHECK (channel IN (
        'EMAIL',            -- outbound email delivery
        'SMS',              -- outbound SMS delivery
        'PUSH',             -- outbound push delivery
        'WS',               -- websocket push (client-visible stream)
        'IN_APP_CHAT',      -- in-app chat message
        'SUPPORT_MESSAGE',  -- support-ticket correspondence (both directions)
        'OTHER')),          -- recorder-seam channels not yet enumerated
    direction        VARCHAR(8)   NOT NULL DEFAULT 'OUTBOUND'
                     CHECK (direction IN ('INBOUND', 'OUTBOUND', 'INTERNAL')),
    source           VARCHAR(48)  NOT NULL,   -- capturing surface: notification_deliveries | support_tickets | ticket_notes | api
    source_id        BIGINT       NOT NULL,   -- PK on the capturing surface
    started_at       TIMESTAMPTZ  NOT NULL,   -- communication start (UTC)
    ended_at         TIMESTAMPTZ  NOT NULL,   -- communication end (UTC; == started_at for atomic messages)
    content_ref      VARCHAR(512) NOT NULL,   -- WORM object-store key
    sha256           CHAR(64)     NOT NULL,   -- SHA-256 of the stored object bytes
    size_bytes       BIGINT       NOT NULL CHECK (size_bytes >= 0),
    storage_class    VARCHAR(16)  NOT NULL DEFAULT 'WORM',
    day_bucket       DATE         NOT NULL,   -- started_at::date (UTC) — chain partition
    prev_chain_hash  CHAR(64)     NOT NULL,   -- previous recording's chain_hash for the day
    chain_hash       CHAR(64)     NOT NULL,   -- SHA-256("COMMS1|" recording_id "|" account_id "|"
                                            --  channel "|" started_at "|" ended_at "|" content_ref "|"
                                            --  sha256 "|" retention_until "|" prev_chain_hash)
    retention_until  TIMESTAMPTZ  NOT NULL,   -- earliest legal deletion instant
    sealed           BOOLEAN      NOT NULL DEFAULT TRUE, -- KMS-wrapped object (SSE-KMS)
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (source, source_id)                -- idempotent capture — one register row per source row
);

-- Per-account evidence listing (surveillance-case attach, client
-- disclosure) and the per-day chain walk.
CREATE INDEX ix_comms_recordings_account
    ON comms_recordings (account_id, started_at DESC, recording_id DESC);
CREATE INDEX ix_comms_recordings_day
    ON comms_recordings (day_bucket, recording_id);
CREATE INDEX ix_comms_recordings_retention
    ON comms_recordings (retention_until);

-- Capture outbox — the trigger-written, recorder-drained seam.
CREATE TABLE comms_capture_outbox (
    id           BIGSERIAL    PRIMARY KEY,
    source       VARCHAR(48)  NOT NULL,
    source_id    BIGINT       NOT NULL,
    account_id   BIGINT,
    user_id      BIGINT,
    channel      VARCHAR(24)  NOT NULL,
    direction    VARCHAR(8)   NOT NULL DEFAULT 'OUTBOUND',
    happened_at  TIMESTAMPTZ  NOT NULL,
    payload      JSONB        NOT NULL,   -- canonical content envelope the recorder hashes+stores
    claimed_at   TIMESTAMPTZ,             -- recorder lease; NULL = pending
    recorded_id  BIGINT       REFERENCES comms_recordings (recording_id),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (source, source_id)
);
CREATE INDEX ix_comms_capture_outbox_pending
    ON comms_capture_outbox (id) WHERE recorded_id IS NULL;

-- Retrieval/verification audit — append-only (dual-control metadata:
-- accessed_by is the requesting officer, approved_by the second
-- control; both are mandatory for action='RETRIEVE', enforced by the
-- service before the row is written).
CREATE TABLE comms_recording_access (
    access_id     BIGSERIAL   PRIMARY KEY,
    recording_id  BIGINT      NOT NULL REFERENCES comms_recordings (recording_id),
    accessed_by   BIGINT      NOT NULL,
    approved_by   BIGINT,
    action        VARCHAR(16) NOT NULL CHECK (action IN ('RETRIEVE', 'VERIFY', 'LIST', 'ATTACH')),
    justification TEXT        NOT NULL,
    case_ref      VARCHAR(64),          -- surveillance-case linkage (Task 21.3.21)
    masked        BOOLEAN     NOT NULL DEFAULT FALSE, -- non-local auditor view (Task 21.3.18 seam)
    accessed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_comms_access_recording
    ON comms_recording_access (recording_id, accessed_at DESC);

-- WORM guard: register rows are immutable; deletion is allowed only
-- after retention_until has passed (post-expiry legal purge).
CREATE OR REPLACE FUNCTION comms_recordings_guard() RETURNS trigger AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.retention_until > now() THEN
            RAISE EXCEPTION 'comms_recordings % is under retention until % (MiFID II 5y floor)',
                OLD.recording_id, OLD.retention_until
                USING ERRCODE = 'raise_exception';
        END IF;
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'comms_recordings rows are immutable (WORM register)'
        USING ERRCODE = 'raise_exception';
END;
$guard$ LANGUAGE plpgsql;

CREATE TRIGGER trg_comms_recordings_guard
    BEFORE UPDATE OR DELETE ON comms_recordings
    FOR EACH ROW EXECUTE FUNCTION comms_recordings_guard();

CREATE OR REPLACE FUNCTION comms_recording_access_guard() RETURNS trigger AS $guard$
BEGIN
    RAISE EXCEPTION 'comms_recording_access is append-only'
        USING ERRCODE = 'raise_exception';
END;
$guard$ LANGUAGE plpgsql;

CREATE TRIGGER trg_comms_access_guard
    BEFORE UPDATE OR DELETE ON comms_recording_access
    FOR EACH ROW EXECUTE FUNCTION comms_recording_access_guard();

-- ---------------------------------------------------------------------------
-- Capture triggers — enqueue a recording obligation in the same tx as
-- the source write. account_id is resolved by the recorder (master
-- account for user-keyed sources).
-- ---------------------------------------------------------------------------

-- Outbound client notifications (email/sms/push/ws) — taped on enqueue;
-- the payload envelope records what the client was sent.
CREATE OR REPLACE FUNCTION comms_capture_delivery() RETURNS trigger AS $cap$
BEGIN
    INSERT INTO comms_capture_outbox
        (source, source_id, user_id, channel, direction, happened_at, payload)
    VALUES (
        'notification_deliveries', NEW.id, NEW.user_id,
        upper(NEW.channel), 'OUTBOUND', NEW.created_at,
        jsonb_build_object(
            'delivery_id', NEW.id,
            'event',       NEW.event,
            'channel',     NEW.channel,
            'payload',     NEW.payload))
    ON CONFLICT (source, source_id) DO NOTHING;
    RETURN NEW;
END;
$cap$ LANGUAGE plpgsql;

CREATE TRIGGER trg_comms_capture_delivery
    AFTER INSERT ON notification_deliveries
    FOR EACH ROW EXECUTE FUNCTION comms_capture_delivery();

-- Client support tickets — the client's opening message is INBOUND
-- correspondence (subject + body + origin channel).
CREATE OR REPLACE FUNCTION comms_capture_ticket() RETURNS trigger AS $cap$
BEGIN
    INSERT INTO comms_capture_outbox
        (source, source_id, account_id, channel, direction, happened_at, payload)
    VALUES (
        'support_tickets', NEW.id, NEW.account_id,
        'SUPPORT_MESSAGE', 'INBOUND', NEW.created_at,
        jsonb_build_object(
            'ticket_id',      NEW.id,
            'type',           NEW.type::text,
            'category',       NEW.category::text,
            'subject',        NEW.subject,
            'body',           NEW.body,
            'origin_channel', NEW.origin_channel))
    ON CONFLICT (source, source_id) DO NOTHING;
    RETURN NEW;
END;
$cap$ LANGUAGE plpgsql;

CREATE TRIGGER trg_comms_capture_ticket
    AFTER INSERT ON support_tickets
    FOR EACH ROW EXECUTE FUNCTION comms_capture_ticket();

-- Client-visible ticket notes — internal notes never surface to the
-- client and are NOT communications under Art. 16(7); client-visible
-- notes are taped (CLIENT-authored = INBOUND, ADMIN-authored =
-- OUTBOUND).
CREATE OR REPLACE FUNCTION comms_capture_ticket_note() RETURNS trigger AS $cap$
DECLARE
    acct BIGINT;
BEGIN
    IF NEW.internal THEN
        RETURN NEW;
    END IF;
    SELECT account_id INTO acct FROM support_tickets WHERE id = NEW.ticket_id;
    INSERT INTO comms_capture_outbox
        (source, source_id, account_id, channel, direction, happened_at, payload)
    VALUES (
        'ticket_notes', NEW.note_id, acct,
        'SUPPORT_MESSAGE',
        CASE WHEN NEW.author_kind = 'CLIENT' THEN 'INBOUND' ELSE 'OUTBOUND' END,
        NEW.created_at,
        jsonb_build_object(
            'note_id',     NEW.note_id,
            'ticket_id',   NEW.ticket_id,
            'author_kind', NEW.author_kind,
            'body',        NEW.body))
    ON CONFLICT (source, source_id) DO NOTHING;
    RETURN NEW;
END;
$cap$ LANGUAGE plpgsql;

CREATE TRIGGER trg_comms_capture_ticket_note
    AFTER INSERT ON ticket_notes
    FOR EACH ROW EXECUTE FUNCTION comms_capture_ticket_note();

COMMIT;
