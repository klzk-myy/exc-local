-- 093_balance_snapshots.up.sql
-- Phase-20 Task 20.3.13 — spec §16.7 / §24 #362: daily account snapshots.
--
-- One row per (account_id, currency, snapshot_date): the balances row image
-- at the 23:59 UTC daily build plus the account's open-position set in
-- positions_json. Rows are hash-chained PER ACCOUNT — prev_hash carries the
-- hash of the account's previous snapshot row ordered by
-- (snapshot_date, id), so a tampered/rewritten history breaks the chain the
-- same way audit_hash_chain (migration 009, Task 1.3.8) does:
--
--   hash      = SHA256("BS1|" account_id "|" currency "|" available "|"
--                      locked "|" positions_json_canonical "|"
--                      snapshot_date "|" created_at_unixmicro "|" prev_hash)
--   prev_hash = hash of the previous chain row; the first row anchors to the
--               shared genesis constant SHA-256("") (audit.GenesisPrevHash).
--
-- Chain discipline (fail-closed, enforced by the builder):
--   * rows for every account holding a balances row are written every day —
--     including zero/zero balances — so the chain covers every funded
--     account continuously;
--   * a date may only be rebuilt when it is the account's chain TIP (a
--     crashed partial build); inserting a date earlier than the tip is
--     refused — it would fork the chain;
--   * UNIQUE(account_id, currency, snapshot_date) makes a same-day rerun
--     collide loudly instead of silently duplicating.
--
-- positions_json: canonical JSON array (compact, fixed key order, decimal
-- values as strings, sorted by position_id) produced by the snapshot
-- builder; the verifier re-canonicalizes the stored jsonb before hashing,
-- so jsonb key-order/whitespace normalization cannot break the chain.
-- Retention: §19.12 finance schedule (5–7y) — the Phase-09 retention
-- enforcer archives expiring partitions; no TTL here.

BEGIN;

CREATE TABLE balance_snapshots (
    id             BIGSERIAL PRIMARY KEY,
    account_id     BIGINT        NOT NULL REFERENCES accounts (id),
    currency       VARCHAR(3)    NOT NULL,
    available      DECIMAL(28,8) NOT NULL,
    locked         DECIMAL(28,8) NOT NULL,
    positions_json JSONB         NOT NULL DEFAULT '[]'::jsonb,
    snapshot_date  DATE          NOT NULL,
    hash           CHAR(64)      NOT NULL,
    prev_hash      CHAR(64)      NOT NULL,
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now(),
    UNIQUE (account_id, currency, snapshot_date)
);

-- Chain-walk + history surface: per-account ordered by (date, id).
CREATE INDEX balance_snapshots_acct_chain_ix
    ON balance_snapshots (account_id, snapshot_date, id);

COMMENT ON TABLE balance_snapshots IS
    'Task 20.3.13 daily 23:59 UTC account snapshots; per-account SHA-256 hash chain (spec §16.7, §24 #362).';
COMMENT ON COLUMN balance_snapshots.prev_hash IS
    'SHA-256 of the account''s previous snapshot row hash; genesis = sha256("") (audit.GenesisPrevHash).';

COMMIT;
