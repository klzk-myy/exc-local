-- 209_solvency_snapshots.up.sql
-- Phase-13 Task 13.3.7 — proof-of-reserves solvency snapshots (spec §24
-- #186, daily 22:00 UTC generation at the NY close / FX EOD boundary).
--
-- solvency_snapshots is the append-only published record: one row per
-- generation run carrying the Merkle root over ALL client liabilities,
-- the per-currency liability/asset totals, the nostro attestation ratios
-- and the detached signature over the canonical payload (production:
-- cold-storage GPG; see internal/reconciliation/signers.go).
--
-- solvency_proofs holds one salted leaf per (snapshot, account, currency)
-- plus the sibling path — the client verification surface for
-- GET /api/v1/solvency/proof. Rows carry sibling HASHES only — never peer
-- account ids or balances — so inclusion proofs leak nothing about other
-- clients (zero peer-data leak, task DoD).

BEGIN;

CREATE TABLE solvency_snapshots (
    id                 BIGSERIAL PRIMARY KEY,
    generated_at       TIMESTAMPTZ  NOT NULL,          -- run timestamp (UTC), distinct from created_at so replays are honest
    leaf_count         BIGINT       NOT NULL CHECK (leaf_count >= 0),
    merkle_root        BYTEA        NOT NULL,          -- 32-byte root digest
    liabilities        JSONB        NOT NULL,          -- {"USD":"123.45", ...} client-liability totals per currency
    nostro_assets      JSONB        NOT NULL,          -- {"USD":"...", ...} aggregate ACTIVE nostro per currency
    reserve_ratios     JSONB        NOT NULL,          -- {"USD":"1.0234", ...}; null entry when liabilities = 0
    solvent            BOOLEAN      NOT NULL,          -- every currency with liabilities has ratio >= 1
    signed_payload     BYTEA        NOT NULL,          -- canonical bytes the signature covers
    signature          TEXT         NOT NULL,          -- armored GPG detached sig, or DEV-HMAC-SHA256:<hex> (dev-only)
    signer_fingerprint VARCHAR(80)  NOT NULL,          -- GPG fingerprint or 'DEV-HMAC'
    signer_kind        VARCHAR(16)  NOT NULL
                       CHECK (signer_kind IN ('GPG', 'DEV-HMAC')),
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Snapshot rows are immutable audit evidence — no UPDATE path exists.
-- (Nothing prevents a superuser UPDATE, same as admin_audit_log; the
-- tamper-evidence contract is the signature + root, not row locks.)

CREATE TABLE solvency_proofs (
    id          BIGSERIAL PRIMARY KEY,
    snapshot_id BIGINT       NOT NULL REFERENCES solvency_snapshots (id) ON DELETE CASCADE,
    account_id  BIGINT       NOT NULL REFERENCES accounts (id),
    currency    VARCHAR(3)   NOT NULL,
    balance     DECIMAL(28,8) NOT NULL CHECK (balance >= 0), -- negative liabilities abort the run, never persisted
    salt        BYTEA        NOT NULL,                        -- per-leaf random salt (16 bytes)
    leaf_index  BIGINT       NOT NULL,
    leaf_hash   BYTEA        NOT NULL,
    path        JSONB        NOT NULL,                        -- [{"position":"left|right","hash":"<hex>"}] sibling digests only
    UNIQUE (snapshot_id, account_id, currency)
);

-- Client lookup: own-account proof against the latest snapshot.
CREATE INDEX solvency_proofs_account_idx
    ON solvency_proofs (account_id, currency, snapshot_id DESC);

COMMIT;
