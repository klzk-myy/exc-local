-- 182_api_deprecations.up.sql
-- Phase-05 Task 5.3.20 — API deprecation policy store.
--
-- Implements the schema intent of the never-applied `026_create_api_deprecations`
-- note: announced_at → sunset_at is enforced at ≥ 6 months (CHECK) so no
-- row can encode a shorter notice than the published policy (spec §8.6,
-- §24 #91). The deprecation middleware (internal/deprecation) stamps
-- RFC 8594 Sunset + Deprecation headers on matching requests and returns
-- 410 ENDPOINT_GONE once now() >= sunset_at.
--
-- method NULL = the deprecation covers every method on the path.
-- match_prefix marks subtree rules (e.g. a retired /api/v1/legacy/*
-- namespace); exact otherwise.

BEGIN;

CREATE TABLE api_deprecations (
    id            BIGSERIAL PRIMARY KEY,
    method        VARCHAR(8),                        -- NULL = all methods
    path          TEXT        NOT NULL,              -- route path (or prefix when match_prefix)
    match_prefix  BOOLEAN     NOT NULL DEFAULT false,
    announced_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    sunset_at     TIMESTAMPTZ NOT NULL,
    replacement   TEXT,                              -- successor route, if any
    migration_url TEXT        NOT NULL DEFAULT '/developer/migration',
    notice        TEXT,
    created_by    BIGINT      NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT api_deprecations_six_month_notice CHECK
        (sunset_at >= announced_at + interval '6 months'),
    CONSTRAINT api_deprecations_path_absolute CHECK
        (path ~ '^/')
);

-- One live rule per (method-scope, path): the NULL method needs a
-- distinct unique index because NULLs never conflict.
CREATE UNIQUE INDEX ux_api_deprecations_exact
    ON api_deprecations (method, path) WHERE method IS NOT NULL;
CREATE UNIQUE INDEX ux_api_deprecations_anymethod
    ON api_deprecations (path) WHERE method IS NULL;

COMMIT;
