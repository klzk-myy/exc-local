-- 228_fixsbe_sessions.up.sql
-- Phase-18 Tasks 18.3.8 / 18.3.17 — SBE order-entry gateway sessions and
-- schema lifecycle registry (spec §9.5, §24 #284/#289).
--
-- fixsbe_sessions persists negotiated session state: the peer's Ed25519
-- session key fingerprint, the certificate SNI the session is bound to,
-- negotiated encodings (order entry inbound / reports outbound), schema
-- version, and drain state.
--
-- fixsbe_schema_registry is the schema lifecycle table
-- (ACTIVE → DEPRECATED ≥6 months → RETIRED), the same contract the
-- market-data schema registry (internal/sbe/schema.go) applies to
-- schema.yaml entries — this one is shared via Postgres so every
-- gateway replica negotiates from one truth.

BEGIN;

CREATE TABLE fixsbe_sessions (
    id                    BIGSERIAL PRIMARY KEY,
    session_key           VARCHAR(64)  NOT NULL UNIQUE, -- Ed25519 session key fingerprint
    account_id            BIGINT       REFERENCES accounts (id),
    comp_id               VARCHAR(64)  NOT NULL,
    sni                   VARCHAR(255) NOT NULL,
    order_encoding        VARCHAR(10)  NOT NULL
                          CHECK (order_encoding IN ('SBE','TAG_VALUE')),
    report_encoding       VARCHAR(10)  NOT NULL
                          CHECK (report_encoding IN ('SBE','TAG_VALUE')),
    sbe_schema_id         SMALLINT     NOT NULL DEFAULT 1,
    sbe_schema_version    SMALLINT     NOT NULL,
    state                 VARCHAR(12)  NOT NULL DEFAULT 'ACTIVE'
                          CHECK (state IN ('ACTIVE','DRAINING','CLOSED')),
    connected_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    disconnected_at       TIMESTAMPTZ,
    last_seen_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    messages_in           BIGINT       NOT NULL DEFAULT 0,
    messages_out          BIGINT       NOT NULL DEFAULT 0
);

CREATE INDEX fixsbe_sessions_account_idx ON fixsbe_sessions (account_id, state);

CREATE TABLE fixsbe_schema_registry (
    id             BIGSERIAL PRIMARY KEY,
    schema_id      SMALLINT     NOT NULL,
    version        SMALLINT     NOT NULL,
    state          VARCHAR(10)  NOT NULL DEFAULT 'ACTIVE'
                   CHECK (state IN ('ACTIVE','DEPRECATED','RETIRED')),
    deprecated_at  TIMESTAMPTZ,
    retired_at     TIMESTAMPTZ,
    notes          TEXT         NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (schema_id, version)
);

-- Seed schema 1 v1 (schema/order_entry.xml) as ACTIVE.
INSERT INTO fixsbe_schema_registry (schema_id, version, state, notes)
VALUES (1, 1, 'ACTIVE',
        'initial order-entry schema: NewOrder/Cancel/Replace/Reject/Report/News');

COMMIT;
