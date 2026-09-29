-- 069_login_history.up.sql
-- Phase-12 Task 12.3.9: device management & login history.
--
-- Pinned column contract (spec §12.6 / phase doc):
--   {id, user_id, timestamp, ip, user_agent, device_fingerprint,
--    geo_city, geo_country, result, session_id}
--
-- result is the constrained vocabulary SUCCESS | FAILED | 2FA_FAILED |
-- LOCKED. geo_* are nullable — populated only when a GeoIP lookup is
-- configured; nothing fabricates them.
--
-- The pinned column name `timestamp` is kept verbatim (quoted — it is a
-- type keyword). The §8.8 keyset cursor over (created_at,id) maps onto
-- (timestamp,id) for this table: the cursor timestamp field carries this
-- column's value.

BEGIN;

CREATE TYPE login_result_enum AS ENUM ('SUCCESS', 'FAILED', '2FA_FAILED', 'LOCKED');

CREATE TABLE login_history (
    id                 BIGSERIAL PRIMARY KEY,
    user_id            BIGINT      NOT NULL REFERENCES users (id),
    "timestamp"        TIMESTAMPTZ NOT NULL DEFAULT now(),
    ip                 INET,
    user_agent         TEXT,
    device_fingerprint TEXT,
    geo_city           VARCHAR(128),
    geo_country        VARCHAR(2),                        -- ISO 3166-1 alpha-2
    result             login_result_enum NOT NULL,
    session_id         VARCHAR(128)
);

-- Login-activity read path: user-scoped, newest first, keyset-stable.
CREATE INDEX idx_login_history_user_ts
    ON login_history (user_id, "timestamp" DESC, id DESC);

COMMIT;
