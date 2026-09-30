-- 035_create_swift_messages.up.sql
-- Phase-24 Task 24.3.4 — SWIFT message tracking (spec §17 backoffice;
-- §24 #13/#14/#15). Plan-reserved migration number.
--
-- swift_messages is the immutable audit journal of every SWIFT /
-- ISO 20022 message crossing the correspondent-bank boundary —
-- MT103 (customer credit transfer), MT202 (bank-to-bank), MT900/MT910
-- (debit/credit confirmations), pacs.009 (FI credit transfer) and any
-- further type the rails ingest; message_type stays an open VARCHAR so
-- no new rail message shape is ever silently dropped.
--
-- Immutability is enforced in the database, not just by convention: a
-- BEFORE UPDATE OR DELETE trigger refuses every mutation — the audit
-- trail is append-only (spec §24 criterion 15).

BEGIN;

CREATE TYPE swift_direction_enum AS ENUM ('IN', 'OUT');

CREATE TABLE swift_messages (
    id                 BIGSERIAL PRIMARY KEY,
    message_type       VARCHAR(16) NOT NULL,             -- MT103|MT202|MT900|MT910|pacs.009|...
    reference          VARCHAR(64) NOT NULL,             -- :20: sender ref / MsgId
    related_reference  VARCHAR(64),                      -- :21: related ref (confirmation → leg link)
    direction          swift_direction_enum NOT NULL,
    status             VARCHAR(24) NOT NULL DEFAULT 'RECEIVED', -- RECEIVED|SENT|ACKED|FAILED|PARSED
    nostro_account_id  BIGINT REFERENCES nostro_accounts (id),
    raw_payload        TEXT,                             -- verbatim wire body
    -- plan column "timestamp" renamed msg_timestamp: bare TIMESTAMP is a
    -- reserved type name in PostgreSQL and cannot head a column.
    msg_timestamp      TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Query surface: GET /api/v1/admin/swift-messages?from=&to=&type= —
-- keyset pages on (created_at, id), filters on type/direction/window.
CREATE INDEX swift_messages_page_ix ON swift_messages (created_at, id);
CREATE INDEX swift_messages_window_ix ON swift_messages (msg_timestamp, id);
CREATE INDEX swift_messages_type_ix ON swift_messages (message_type, created_at);
CREATE INDEX swift_messages_ref_ix ON swift_messages (reference);
CREATE INDEX swift_messages_related_ix ON swift_messages (related_reference)
    WHERE related_reference IS NOT NULL;

-- Append-only enforcement: no UPDATE, no DELETE, ever.
CREATE OR REPLACE FUNCTION swift_messages_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'swift_messages is an immutable audit journal (Phase-24 Task 24.3.4)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER swift_messages_no_update
    BEFORE UPDATE ON swift_messages
    FOR EACH ROW EXECUTE FUNCTION swift_messages_immutable();
CREATE TRIGGER swift_messages_no_delete
    BEFORE DELETE ON swift_messages
    FOR EACH ROW EXECUTE FUNCTION swift_messages_immutable();

COMMIT;
