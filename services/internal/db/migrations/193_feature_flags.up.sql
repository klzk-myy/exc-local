-- 193_feature_flags.up.sql
-- Phase-09 Task 9.3.7 — feature flags for canary deploys.
--
-- One row per flag. Evaluation contract (internal/flags):
--   enabled=false                                  → always off
--   account allowlist / tier allowlist hit         → on (targeted)
--   else staged rollout: stage_idx >= 0 selects the
--   current canary step out of `stages` (the 1%→10%→…→100%
--   ladder); stage_idx = -1 uses rollout_pct directly.
--   Percentage membership is deterministic:
--   fnv1a32(name ":" subject) % 100 < effective_pct.
--
-- `version` is the optimistic-concurrency / cache-invalidation counter —
-- the store bumps it on every write and mirrors it to Redis
-- `flags:version` so edge caches can cheaply detect staleness.

BEGIN;

CREATE TABLE feature_flags (
    name        VARCHAR(64) PRIMARY KEY
                CHECK (name ~ '^[a-z][a-z0-9_-]{1,63}$'),
    enabled     BOOLEAN     NOT NULL DEFAULT false,
    rollout_pct SMALLINT    NOT NULL DEFAULT 0
                CHECK (rollout_pct BETWEEN 0 AND 100),
    stages      SMALLINT[]  NOT NULL DEFAULT '{1,10,25,50,100}'
                CHECK (cardinality(stages) >= 1),
    stage_idx   SMALLINT    NOT NULL DEFAULT -1
                CHECK (stage_idx >= -1),
    tiers       TEXT[]      NOT NULL DEFAULT '{}',
    accounts    BIGINT[]    NOT NULL DEFAULT '{}',
    description TEXT        NOT NULL DEFAULT '',
    created_by  BIGINT      NOT NULL DEFAULT 0,
    updated_by  BIGINT      NOT NULL DEFAULT 0,
    version     BIGINT      NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Stage values 0..100 are enforced by the Go store — CHECK
    -- constraints cannot express set-membership validation.
    CONSTRAINT feature_flags_stage_in_ladder CHECK
        (stage_idx < cardinality(stages))
);

COMMENT ON TABLE feature_flags IS
    'Task 9.3.7 feature flags — canary ladder + account/tier targeting; Redis cache key flags:{name}';

COMMIT;
