-- 196_api_deprecation_ops.up.sql
-- Phase-09 Task 9.3.6 — operationalize the deprecation lifecycle.
--
-- Migration 182 already carries the rule + 6-month CHECK; this adds the
-- lifecycle columns the sunset sweep and usage telemetry need:
--   status              ANNOUNCED → SUNSET (sweep flips rows whose
--                       sunset_at has passed so operators can tell an
--                       announced rule from an enforced one);
--   sunset_processed_at when the sweep first observed the transition
--                       (NULL while still announced);
--   api_deprecation_hits durable daily hit counters — the middleware's
--                       Redis-side telemetry (deprecation:hits:{id}:{yyyymmdd})
--                       is the fast path; the sweep compacts a durable
--                       rollup here so route-migration decisions survive
--                       Redis loss.

BEGIN;

ALTER TABLE api_deprecations
    ADD COLUMN status              VARCHAR(12) NOT NULL DEFAULT 'ANNOUNCED'
        CHECK (status IN ('ANNOUNCED', 'SUNSET')),
    ADD COLUMN sunset_processed_at TIMESTAMPTZ;

-- Rows already past sunset are enforced rules, not announced ones.
UPDATE api_deprecations
   SET status = 'SUNSET', sunset_processed_at = now()
 WHERE sunset_at <= now();

CREATE INDEX idx_api_deprecations_sunset_due
    ON api_deprecations (sunset_at)
    WHERE status = 'ANNOUNCED';

CREATE TABLE api_deprecation_hits (
    id         BIGSERIAL PRIMARY KEY,
    rule_id    BIGINT      NOT NULL REFERENCES api_deprecations (id) ON DELETE CASCADE,
    day        DATE        NOT NULL,
    hits       BIGINT      NOT NULL DEFAULT 0 CHECK (hits >= 0),
    gone_hits  BIGINT      NOT NULL DEFAULT 0 CHECK (gone_hits >= 0),
    UNIQUE (rule_id, day)
);

COMMENT ON TABLE api_deprecation_hits IS
    'Task 9.3.6 durable daily rollup of deprecated-route usage (Redis hash is the hot counter)';

COMMIT;
