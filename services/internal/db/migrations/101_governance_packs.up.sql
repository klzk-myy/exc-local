-- 101_governance_packs.up.sql
-- Phase-07 Tasks 7.3.13/7.3.14 — spec §5.43/§7.6, §24 #378/#379.
--
-- Governance pack records: CEO_DAILY executive roll-ups (06:00 UTC job),
-- BOARD_QUARTERLY and BOARD_ADHOC packs. The row IS the provable record
-- of "what the executive saw": content carries the rendered snapshot and
-- content_hash its SHA-256; prev_pack_hash chains packs of the same kind.
--
-- Spec §5.43 columns: pack_id, kind, period, content_hash,
-- source_versions, generated_at, released_by (NULL until dual-controlled
-- board release). Additive columns the task mechanics require:
--   status            GENERATED|RELEASED release lifecycle
--   content           the hash-pinned rendered snapshot (read-back surface)
--   prev_pack_hash    per-kind hash chain link (Task 7.3.14 "hash-chained
--                     content")
--   generated_by      'system' (scheduled job) or admin user id (on-demand)
--   release_*         maker/checker ids + timestamps (dual control)
--   deliveries        delivery receipts (dashboard/email); CEO packs with
--                     no executive binding stay QUEUED, never blocked
--
-- Immutability: a RELEASED row rejects UPDATE and DELETE via trigger —
-- application checks are advisory, the invariant lives in the schema.

BEGIN;

CREATE TABLE governance_packs (
    pack_id               BIGSERIAL   PRIMARY KEY,
    kind                  VARCHAR(16) NOT NULL
                          CHECK (kind IN ('CEO_DAILY','BOARD_QUARTERLY','BOARD_ADHOC')),
    period                VARCHAR(32) NOT NULL,            -- 'YYYY-MM-DD' | 'YYYYQn' | ad-hoc label
    status                VARCHAR(12) NOT NULL DEFAULT 'GENERATED'
                          CHECK (status IN ('GENERATED','RELEASED')),
    content               TEXT        NOT NULL             -- rendered snapshot (sections[]);
                          CHECK (content::jsonb IS NOT NULL), -- TEXT not JSONB: the hash pins the
                                                             -- exact rendered bytes — jsonb
                                                             -- re-serializes (whitespace/key order)
                                                             -- and would break hash verification
    content_hash          CHAR(64)    NOT NULL,            -- sha256 hex over canonical content
    prev_pack_hash        CHAR(64),                        -- content_hash of previous pack, same kind
    source_versions       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    generated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    generated_by          VARCHAR(64) NOT NULL DEFAULT 'system',
    release_initiated_by  BIGINT,                          -- maker (first approver)
    release_initiated_at  TIMESTAMPTZ,
    released_by           BIGINT,                          -- checker (distinct second approver)
    released_at           TIMESTAMPTZ,
    release_reason        TEXT,
    deliveries            JSONB       NOT NULL DEFAULT '[]'::jsonb,
    CONSTRAINT governance_pack_dual_release
        CHECK (released_by IS NULL OR released_by <> release_initiated_by)
);

-- At most one RELEASED pack per (kind, period); GENERATED history keeps
-- every rebuild (each hash-pinned) so intra-day regenerations are provable.
CREATE UNIQUE INDEX uq_governance_packs_released
    ON governance_packs (kind, period) WHERE status = 'RELEASED';
CREATE INDEX idx_governance_packs_kind_period
    ON governance_packs (kind, period, generated_at DESC);

CREATE OR REPLACE FUNCTION governance_packs_released_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'governance_packs pack_id=% is RELEASED and immutable', OLD.pack_id
        USING ERRCODE = 'raise_exception';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_governance_packs_immutable
    BEFORE UPDATE OR DELETE ON governance_packs
    FOR EACH ROW WHEN (OLD.status = 'RELEASED')
    EXECUTE FUNCTION governance_packs_released_immutable();

COMMIT;
