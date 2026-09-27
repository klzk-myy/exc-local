-- 009_create_audit_hash_chain.up.sql
-- Spec §5.8 audit_hash_chain — append-only WORM SHA-256 chain
-- (H_n = SHA256(H_{n-1} || payload_n)); daily Merkle root per §17.11 / Task 1.3.8.

BEGIN;

CREATE TABLE audit_hash_chain (
    id           BIGSERIAL PRIMARY KEY,
    sequence_num BIGINT NOT NULL UNIQUE,
    table_name   VARCHAR(64) NOT NULL,
    record_id    BIGINT,
    action       VARCHAR(16) NOT NULL,                      -- INSERT, UPDATE, DELETE
    payload_hash VARCHAR(64) NOT NULL,                      -- SHA-256 of row contents
    prev_hash    VARCHAR(64),                               -- hash of previous sequence
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
