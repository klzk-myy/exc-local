-- 021_create_audit_merkle_roots.up.sql
-- Daily Proof-of-Reserves / audit Merkle roots (Task 1.3.8; spec §17.11).
-- One root per UTC day, computed at 00:10 UTC over audit_hash_chain.

BEGIN;

CREATE TABLE audit_merkle_roots (
    root_id     BIGSERIAL PRIMARY KEY,
    date        DATE NOT NULL,
    merkle_root VARCHAR(64) NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (date)
);

COMMIT;
